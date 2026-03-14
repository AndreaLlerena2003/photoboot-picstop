//go:build (windows || darwin) && cgo

package canon

/*
#cgo windows CFLAGS: -I${SRCDIR}/../../../edsdk
#cgo windows LDFLAGS: -L${SRCDIR}/../../../edsdk -l:EDSDK.lib -lole32
#cgo darwin CFLAGS: -I${SRCDIR}/../../../edsdk
#include <stdbool.h>

#ifndef _MSC_VER
#define EDSDK_FAKE_MSC_VER
#define _MSC_VER 1900
#endif
#include "EDSDK.h"
#ifdef EDSDK_FAKE_MSC_VER
#undef _MSC_VER
#undef EDSDK_FAKE_MSC_VER
#endif

extern EdsError goObjectEventHandler(EdsObjectEvent inEvent, EdsBaseRef inRef, EdsVoid* inContext);
extern EdsError goCameraStateEventHandler(EdsStateEvent inEvent, EdsUInt32 inParameter, EdsVoid* inContext);

// getStreamData extracts the data pointer and byte length from a memory stream.
static EdsError getStreamData(EdsStreamRef stream, void** outPtr, EdsUInt64* outLen) {
    EdsError err = EdsGetPointer(stream, (EdsVoid**)outPtr);
    if (err != EDS_ERR_OK) return err;
    return EdsGetLength(stream, outLen);
}

// createFileStreamW wraps EdsCreateFileStream.
static EdsError createFileStreamW(const char* path, EdsFileCreateDisposition disp,
                                   EdsAccess access, EdsStreamRef* outStream) {
    return EdsCreateFileStream((const EdsChar*)path, disp, access, outStream);
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"photoboot-picstop/internal/domain"
)

// activeService guarda el Service actual para que los callbacks C puedan despachar eventos.
var activeService atomic.Pointer[Service]

// sdkMu / sdkRefs guard EdsInitializeSDK / EdsTerminateSDK so the SDK is initialised
// exactly once across concurrent NewService calls and torn down only when the last
// service is closed — enabling restart-without-process-restart.
var (
	sdkMu   sync.Mutex
	sdkRefs int
)

// acquireSDK increments the SDK reference count and, on the first call, initialises EDSDK.
func acquireSDK() error {
	sdkMu.Lock()
	defer sdkMu.Unlock()
	if sdkRefs == 0 {
		if err := edsCheck("EdsInitializeSDK", C.EdsInitializeSDK()); err != nil {
			return err
		}
	}
	sdkRefs++
	return nil
}

// releaseSDK decrements the SDK reference count and terminates EDSDK when it reaches zero.
func releaseSDK() {
	sdkMu.Lock()
	defer sdkMu.Unlock()
	sdkRefs--
	if sdkRefs == 0 {
		_ = C.EdsTerminateSDK()
	}
}

// Service implementa el puerto de cámara (CameraPort) usando Canon EDSDK en Windows.
type Service struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error

	camera C.EdsCameraRef

	outputDir              string
	pending                *captureRequest
	closed                 bool
	cameraDisconnected     bool // set by onStateEvent(kEdsStateEvent_Shutdown); skips EdsCloseSession in Close()
	jobBusy                atomic.Uint32
	suppressTransfersUntil time.Time

	// disconnectedCh is closed (exactly once) when kEdsStateEvent_Shutdown fires.
	// ReconnectingService waits on this to know when to start a reconnect attempt.
	disconnectedCh   chan struct{}
	disconnectedOnce sync.Once

	stopEventPump chan struct{}
	eventPumpDone chan struct{}
	jobStateCh    chan struct{}

	// EVF (Electronic Viewfinder / live preview) state — all guarded by mu.
	evfActive             atomic.Bool        // true while evfLoop goroutine is running
	evfSuspendedByCapture atomic.Bool        // true while EVF is suspended for a capture cycle
	evfStop               chan struct{}      // closed to signal evfLoop to stop; nil when EVF off
	evfDone               chan struct{}      // closed when evfLoop exits; nil when EVF off
	evfFrameCh            chan []byte        // current frame sink; nil when no preview client
	evfCtx                context.Context    // lifetime of the current preview client
	evfCancel             context.CancelFunc // cancels evfCtx

	bgWg sync.WaitGroup
}

type captureRequest struct {
	done chan captureResult
}

type captureResult struct {
	path string
	err  error
}

// NewService inicializa el SDK, descubre la primera cámara, abre sesión, registra handlers y configura SaveTo=Host.
func NewService(outputDir string) (_ *Service, retErr error) {
	absDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve capture dir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create capture dir: %w", err)
	}

	uninitThreading, err := initPlatformThreading()
	if err != nil {
		return nil, err
	}
	defer uninitThreading()

	log.Printf("canon: calling EdsInitializeSDK")
	if err := acquireSDK(); err != nil {
		return nil, err
	}
	sdkInitialized := true
	defer func() {
		if retErr != nil && sdkInitialized {
			releaseSDK()
		}
	}()

	discoveryTimeout := cameraDiscoveryTimeout()
	log.Printf("canon: discovering camera (timeout=%s)", discoveryTimeout)
	cameraBase, err := waitForFirstCamera(discoveryTimeout)
	if err != nil {
		return nil, err
	}
	camera := C.EdsCameraRef(cameraBase)
	cameraOwned := true
	defer func() {
		if retErr != nil && cameraOwned {
			releaseRef(cameraBase)
		}
	}()

	handler := (C.EdsObjectEventHandler)(C.goObjectEventHandler)
	log.Printf("canon: registering object event handler")
	if err := edsCheck("EdsSetObjectEventHandler", C.EdsSetObjectEventHandler(camera, C.kEdsObjectEvent_All, handler, nil)); err != nil {
		return nil, err
	}
	stateHandler := (C.EdsStateEventHandler)(C.goCameraStateEventHandler)
	log.Printf("canon: registering state event handler")
	if err := edsCheck("EdsSetCameraStateEventHandler", C.EdsSetCameraStateEventHandler(camera, C.kEdsStateEvent_All, stateHandler, nil)); err != nil {
		return nil, err
	}

	log.Printf("canon: opening camera session")
	if err := openSessionWithRetry(camera); err != nil {
		return nil, err
	}
	sessionOpen := true
	defer func() {
		if retErr != nil && sessionOpen {
			_ = C.EdsCloseSession(camera)
		}
	}()

	svc := &Service{
		camera:         camera,
		outputDir:      absDir,
		stopEventPump:  make(chan struct{}),
		eventPumpDone:  make(chan struct{}),
		jobStateCh:     make(chan struct{}, 1),
		disconnectedCh: make(chan struct{}),
	}
	activeService.Store(svc)
	defer func() {
		if retErr != nil {
			activeService.CompareAndSwap(svc, nil)
		}
	}()

	log.Printf("canon: configuring host capture defaults")
	if err := configureCamera(camera); err != nil {
		return nil, err
	}

	log.Printf("canon: starting event pump")
	svc.startEventPump()

	sessionOpen = false
	cameraOwned = false
	sdkInitialized = false
	return svc, nil
}

// ─────────────────────────────────────────────
// Capture
// ─────────────────────────────────────────────

// Capture dispara una foto, espera a que el archivo sea descargado al host y devuelve la ruta local.
func (s *Service) Capture(ctx context.Context) (string, error) {
	result, err := s.capture(ctx)
	if err != nil {
		return "", err
	}
	return result.path, result.err
}

func (s *Service) capture(ctx context.Context) (captureResult, error) {
	req := &captureRequest{
		done: make(chan captureResult, 1),
	}

	// ── Suspend EVF before doing anything with the camera ──────────────────
	// evfSuspendedByCapture tracks whether WE are the goroutine that suspended
	// EVF, so that maybeResumeEVF knows who should restart it.
	if s.evfActive.Load() && !s.evfSuspendedByCapture.Swap(true) {
		// We set evfSuspendedByCapture false→true: we own the suspend.
		s.suspendEVF()
	}

	// ── Preempt any in-flight capture ──────────────────────────────────────
	var preempted *captureRequest
	var preemptWindow time.Duration

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.maybeResumeEVF()
		return captureResult{}, errors.New("service is closed")
	}
	if s.pending != nil {
		preempted = s.pending
		s.pending = nil
		preemptWindow = capturePreemptWindow()
		if preemptWindow > 0 {
			s.suppressTransfersUntil = time.Now().Add(preemptWindow)
		}
		log.Printf("canon: preempting in-progress capture with a newer request")
	}
	s.pending = req
	camera := s.camera
	s.mu.Unlock()

	if preempted != nil {
		notifyCaptureResult(preempted, captureResult{err: domain.ErrCaptureSuperseded})
		s.bgWg.Add(1)
		go func() {
			defer s.bgWg.Done()
			_ = withPlatformThreading(func() error {
				resetShutterState(camera)
				return nil
			})
		}()
		if preemptWindow > 0 {
			time.Sleep(preemptWindow)
		}
	}

	// ── Wait for previous job to finish ────────────────────────────────────
	drainTimeout := jobDrainTimeout()
	if !s.waitForTransferJobsIdle(drainTimeout) {
		log.Printf("canon: warning: transfer jobs still pending before capture after %s; continuing", drainTimeout)
	}

	// ── Guard: check context deadline ──────────────────────────────────────
	commandTimeout := effectiveShutterTimeout(ctx, shutterCommandTimeout())
	if commandTimeout <= 0 {
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, context.DeadlineExceeded
	}

	// ── Fire shutter ────────────────────────────────────────────────────────
	if err := sendTakePictureWithTimeout(camera, commandTimeout, &s.bgWg); err != nil {
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, err
	}

	// ── Wait for onObjectEvent to deliver the downloaded file ──────────────
	// DirItemRequestTransfer → onObjectEvent → EdsDownload → notifyCaptureResult.
	transferDeadline := hostTransferTimeout()
	select {
	case result := <-req.done:
		s.maybeResumeEVF()
		return result, nil
	case <-time.After(transferDeadline):
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, fmt.Errorf("host transfer timed out after %s", transferDeadline)
	case <-ctx.Done():
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, ctx.Err()
	}
}

// ─────────────────────────────────────────────
// Shutter commands
// ─────────────────────────────────────────────

// sendTakePicture envía TakePicture; si falla por AF usa disparo NonAF.
func sendTakePicture(camera C.EdsCameraRef) error {
	// Try standard TakePicture first (works when AF is not required).
	takePictureCode := sendCameraCommandWithRetry(
		camera,
		C.kEdsCameraCommand_TakePicture,
		C.EdsInt32(0),
		6,
		180*time.Millisecond,
	)
	if takePictureCode == C.EDS_ERR_OK {
		return nil
	}

	// AF failed — fall back to NonAF shutter.
	if takePictureCode != C.EDS_ERR_TAKE_PICTURE_AF_NG {
		return edsCheck("EdsSendCommand(TakePicture)", takePictureCode)
	}

	log.Printf("canon: AF not confirmed on TakePicture; retrying with NonAF shutter")
	nonAFPressCode, nonAFReleaseCode := pressAndReleaseShutter(
		camera,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_Completely_NonAF),
	)
	if nonAFPressCode == C.EDS_ERR_OK {
		if nonAFReleaseCode != C.EDS_ERR_OK {
			return edsCheck("EdsSendCommand(ShutterButton_OFF)", nonAFReleaseCode)
		}
		return nil
	}

	afErr := edsCheck("EdsSendCommand(TakePicture)", takePictureCode)
	nonAFErr := edsCheck("EdsSendCommand(ShutterButton_Completely_NonAF)", nonAFPressCode)
	if nonAFReleaseCode != C.EDS_ERR_OK {
		return fmt.Errorf("%w; non-AF fallback failed: %v; release failed: %v",
			afErr, nonAFErr, edsCheck("EdsSendCommand(ShutterButton_OFF)", nonAFReleaseCode))
	}
	return fmt.Errorf("%w; non-AF fallback failed: %v", afErr, nonAFErr)
}

// sendTakePictureWithTimeout ejecuta sendTakePicture con timeout; al superarlo hace reset del shutter.
func sendTakePictureWithTimeout(camera C.EdsCameraRef, timeout time.Duration, wg *sync.WaitGroup) error {
	if timeout <= 0 {
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}

	resultCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resultCh <- withPlatformThreading(func() error {
			return sendTakePicture(camera)
		})
	}()

	select {
	case err := <-resultCh:
		return err
	case <-time.After(timeout):
		// Best-effort reset so subsequent captures can proceed.
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = withPlatformThreading(func() error {
				_ = sendCameraCommandWithRetry(
					camera,
					C.kEdsCameraCommand_PressShutterButton,
					C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF),
					3,
					120*time.Millisecond,
				)
				return nil
			})
		}()
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}
}

func pressAndReleaseShutter(camera C.EdsCameraRef, pressParam C.EdsInt32) (C.EdsError, C.EdsError) {
	pressCode := sendCameraCommandWithRetry(
		camera,
		C.kEdsCameraCommand_PressShutterButton,
		pressParam,
		10,
		220*time.Millisecond,
	)
	releaseCode := sendCameraCommandWithRetry(
		camera,
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF),
		8,
		120*time.Millisecond,
	)
	return pressCode, releaseCode
}

// resetShutterState envía ShutterButton_OFF para dejar el shutter en estado conocido.
func resetShutterState(camera C.EdsCameraRef) {
	_ = sendCameraCommandWithRetry(
		camera,
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF),
		3,
		120*time.Millisecond,
	)
}

func sendCameraCommandWithRetry(
	camera C.EdsCameraRef,
	command C.EdsCameraCommand,
	param C.EdsInt32,
	attempts int,
	delay time.Duration,
) C.EdsError {
	if attempts < 1 {
		attempts = 1
	}
	var code C.EdsError
	for i := 0; i < attempts; i++ {
		code = C.EdsSendCommand(camera, command, param)
		if code == C.EDS_ERR_OK {
			return code
		}
		if !isRetryableCommandErr(code) {
			return code
		}
		_ = C.EdsGetEvent()
		time.Sleep(delay)
	}
	return code
}

// ─────────────────────────────────────────────
// Live preview (EVF)
// ─────────────────────────────────────────────

// StartPreview begins EVF streaming. Frames (JPEG bytes) are pushed to the returned channel
// until ctx is cancelled or StopPreview is called, at which point the channel is closed.
func (s *Service) StartPreview(ctx context.Context) (<-chan []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, domain.ErrPreviewUnavailable
	}
	// Block EVF start while a capture+download cycle is in progress (s.pending is set
	// for the entire window from shutter-fire through EdsDownloadComplete). Starting
	// the EVF goroutine now would put a second OS thread into EDSDK while the event
	// pump thread is mid-download — concurrent EDSDK calls from two threads.
	if s.pending != nil {
		return nil, domain.ErrPreviewUnavailable
	}
	if s.evfActive.Load() {
		// Already running: return the live channel so the new client gets frames.
		return s.evfFrameCh, nil
	}

	evfCtx, evfCancel := context.WithCancel(ctx)
	frameCh := make(chan []byte, 2)
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})

	s.evfCtx = evfCtx
	s.evfCancel = evfCancel
	s.evfFrameCh = frameCh
	s.evfStop = stopCh
	s.evfDone = doneCh
	s.evfActive.Store(true)

	s.bgWg.Add(1)
	go s.evfLoop(evfCtx, frameCh, stopCh, doneCh)

	return frameCh, nil
}

// StopPreview stops EVF mode and closes the frame channel. Idempotent.
func (s *Service) StopPreview() {
	s.mu.Lock()
	stop := s.evfStop
	done := s.evfDone
	if stop != nil {
		select {
		case <-stop: // already closed
		default:
			close(stop)
		}
		s.evfStop = nil
	}
	s.mu.Unlock()

	if done != nil {
		<-done
	}
}

// suspendEVF stops the EVF goroutine without closing the frame channel,
// so preview clients remain subscribed and frames resume after resumeEVF.
// Safe to call concurrently; closing the stop channel is guarded by mu.
func (s *Service) suspendEVF() {
	s.mu.Lock()
	stop := s.evfStop
	done := s.evfDone
	if stop != nil {
		select {
		case <-stop: // already closed
		default:
			close(stop)
		}
		s.evfStop = nil
	}
	s.mu.Unlock()

	if done != nil {
		<-done // wait outside lock to avoid deadlock
	}
}

// resumeEVF starts a new EVF goroutine reusing the existing frame channel.
// Must only be called after suspendEVF has returned.
func (s *Service) resumeEVF() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.evfFrameCh == nil {
		return
	}
	if s.evfActive.Load() {
		return // already running (e.g. another goroutine beat us here)
	}

	// If the client's context was cancelled while we were capturing, don't restart.
	if s.evfCtx != nil {
		select {
		case <-s.evfCtx.Done():
			// Client disconnected during capture — clean up preview state.
			s.evfFrameCh = nil
			s.evfCtx = nil
			s.evfCancel = nil
			s.evfStop = nil
			s.evfDone = nil
			return
		default:
		}
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	s.evfStop = stopCh
	s.evfDone = doneCh
	s.evfActive.Store(true)

	s.bgWg.Add(1)
	go s.evfLoop(s.evfCtx, s.evfFrameCh, stopCh, doneCh)
}

// maybeResumeEVF resumes EVF after a capture cycle, but only if no other
// capture request is pending. Called at the end of every capture() path.
func (s *Service) maybeResumeEVF() {
	if !s.evfSuspendedByCapture.Load() {
		return
	}
	s.mu.Lock()
	hasPending := s.pending != nil
	s.mu.Unlock()

	if hasPending {
		// Another capture is queued — it will resume EVF when it finishes.
		return
	}
	if s.evfSuspendedByCapture.CompareAndSwap(true, false) {
		s.resumeEVF()
	}
}

// evfLoop is the dedicated goroutine for EVF frame grabbing.
// It locks an OS thread with a COM STA context for the entire loop lifetime.
// Exits when stop is closed (suspend for capture) or ctx is cancelled (client disconnected).
// On ctx cancellation it closes frameCh to notify the HTTP handler.
func (s *Service) evfLoop(ctx context.Context, frameCh chan<- []byte, stop <-chan struct{}, done chan<- struct{}) {
	defer s.bgWg.Done()
	defer close(done)
	defer s.evfActive.Store(false)

	uninitThreading, err := initPlatformThreading()
	if err != nil {
		log.Printf("canon: EVF loop threading init failed: %v", err)
		close(frameCh)
		s.mu.Lock()
		s.evfFrameCh = nil
		s.evfCtx = nil
		s.evfCancel = nil
		s.mu.Unlock()
		return
	}
	defer uninitThreading()

	camera := s.camera

	if err := startEVFMode(camera); err != nil {
		log.Printf("canon: failed to start EVF mode: %v", err)
		close(frameCh)
		s.mu.Lock()
		s.evfFrameCh = nil
		s.evfCtx = nil
		s.evfCancel = nil
		s.mu.Unlock()
		return
	}
	log.Printf("canon: EVF mode started")

	ticker := time.NewTicker(evfFrameInterval())
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			// Suspend for capture: stop EVF mode on camera but keep frameCh open.
			stopEVFMode(camera)
			log.Printf("canon: EVF suspended for capture")
			return

		case <-ctx.Done():
			// Client disconnected: stop EVF and close the channel.
			stopEVFMode(camera)
			log.Printf("canon: EVF stopped (client disconnected)")
			close(frameCh)
			s.mu.Lock()
			s.evfFrameCh = nil
			s.evfCtx = nil
			s.evfCancel = nil
			s.evfStop = nil
			s.evfDone = nil
			s.evfSuspendedByCapture.Store(false)
			s.mu.Unlock()
			return

		case <-ticker.C:
			frame, err := grabEVFFrame(camera)
			if err != nil {
				if isRetryableEVFErr(err) {
					continue // camera not ready yet; skip this frame
				}
				log.Printf("canon: EVF frame error: %v", err)
				continue
			}
			// Non-blocking send: drop frame if HTTP handler is slow.
			select {
			case frameCh <- frame:
			default:
			}
		}
	}
}

// startEVFMode enables EVF (live view) output to PC on the camera.
func startEVFMode(camera C.EdsCameraRef) error {
	// Some cameras require Evf_Mode=1 first; use setOptionalUInt32Property so
	// cameras that don't support it still proceed.
	evfMode := C.EdsUInt32(1)
	_ = C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_Evf_Mode, 0,
		C.EdsUInt32(unsafe.Sizeof(evfMode)),
		unsafe.Pointer(&evfMode),
	)

	// Read current output device and OR in the PC flag.
	var device C.EdsUInt32
	if err := edsCheck("EdsGetPropertyData(Evf_OutputDevice)",
		C.EdsGetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_Evf_OutputDevice, 0,
			C.EdsUInt32(unsafe.Sizeof(device)),
			unsafe.Pointer(&device),
		)); err != nil {
		return err
	}
	device |= C.kEdsEvfOutputDevice_PC
	return edsCheck("EdsSetPropertyData(Evf_OutputDevice)",
		C.EdsSetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_Evf_OutputDevice, 0,
			C.EdsUInt32(unsafe.Sizeof(device)),
			unsafe.Pointer(&device),
		))
}

// stopEVFMode removes the PC flag from Evf_OutputDevice, ending live view to PC.
func stopEVFMode(camera C.EdsCameraRef) {
	var device C.EdsUInt32
	_ = C.EdsGetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_Evf_OutputDevice, 0,
		C.EdsUInt32(unsafe.Sizeof(device)),
		unsafe.Pointer(&device),
	)
	device &^= C.kEdsEvfOutputDevice_PC
	_ = C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_Evf_OutputDevice, 0,
		C.EdsUInt32(unsafe.Sizeof(device)),
		unsafe.Pointer(&device),
	)
}

// grabEVFFrame downloads one EVF frame and returns its JPEG bytes.
func grabEVFFrame(camera C.EdsCameraRef) ([]byte, error) {
	var stream C.EdsStreamRef
	if err := edsCheck("EdsCreateMemoryStream",
		C.EdsCreateMemoryStream(0, &stream)); err != nil {
		return nil, err
	}
	defer releaseRef(C.EdsBaseRef(stream))

	var evfImage C.EdsEvfImageRef
	if err := edsCheck("EdsCreateEvfImageRef",
		C.EdsCreateEvfImageRef(stream, &evfImage)); err != nil {
		return nil, err
	}
	defer releaseRef(C.EdsBaseRef(evfImage))

	if err := edsCheck("EdsDownloadEvfImage",
		C.EdsDownloadEvfImage(camera, evfImage)); err != nil {
		return nil, err
	}

	var ptr unsafe.Pointer
	var length C.EdsUInt64
	if err := edsCheck("getStreamData",
		C.getStreamData(stream, &ptr, &length)); err != nil {
		return nil, err
	}

	if length == 0 || ptr == nil {
		return nil, fmt.Errorf("empty EVF frame")
	}

	// Copy bytes before stream is released.
	frame := make([]byte, int(length))
	copy(frame, (*[1 << 28]byte)(ptr)[:length:length])
	return frame, nil
}

func isRetryableEVFErr(err error) bool {
	if err == nil {
		return false
	}
	// Check if the underlying EDSDK error is a "not ready" / "busy" code.
	msg := err.Error()
	return containsAny(msg,
		"EDS_ERR_OBJECT_NOTREADY",
		"EDS_ERR_DEVICE_BUSY",
		"EDS_ERR_PTP_DEVICE_BUSY",
	)
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// ─────────────────────────────────────────────
// Host file download
// ─────────────────────────────────────────────

// downloadDirectoryItem downloads the camera file referenced by dirItem to the host
// output directory and returns the local file path. It owns and releases dirItem.
// Must be called from a goroutine with a COM STA context (e.g. the event pump).
func (s *Service) downloadDirectoryItem(dirItem C.EdsDirectoryItemRef) (string, error) {
	var info C.EdsDirectoryItemInfo
	if err := edsCheck("EdsGetDirectoryItemInfo",
		C.EdsGetDirectoryItemInfo(dirItem, &info)); err != nil {
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("get dir item info: %w", err)
	}

	fileName := C.GoString(&info.szFileName[0])
	destPath := filepath.Join(s.outputDir, fileName)

	cPath := C.CString(destPath)
	defer C.free(unsafe.Pointer(cPath))

	var stream C.EdsStreamRef
	if err := edsCheck("EdsCreateFileStream",
		C.createFileStreamW(
			cPath,
			C.kEdsFileCreateDisposition_CreateAlways,
			C.kEdsAccess_ReadWrite,
			&stream,
		)); err != nil {
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("create file stream: %w", err)
	}
	defer releaseRef(C.EdsBaseRef(stream))

	if err := edsCheck("EdsDownload",
		C.EdsDownload(dirItem, C.EdsUInt64(info.size), stream)); err != nil {
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("download: %w", err)
	}

	if err := edsCheck("EdsDownloadComplete",
		C.EdsDownloadComplete(dirItem)); err != nil {
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("download complete: %w", err)
	}

	releaseRef(C.EdsBaseRef(dirItem))
	log.Printf("canon: downloaded image to %s", destPath)
	return destPath, nil
}

// ─────────────────────────────────────────────
// Event handlers
// ─────────────────────────────────────────────

// onObjectEvent handles camera object events.
// With SaveTo=Host, DirItemRequestTransfer fires when the photo is ready to pull.
// This callback runs on the event pump goroutine, which already holds a COM STA
// thread — exactly what EDSDK requires. We call EdsDownload directly here, as
// shown in the EDSDK sample code (Section 6.3).
func (s *Service) onObjectEvent(event C.EdsObjectEvent, ref C.EdsBaseRef) C.EdsError {
	if ref == nil {
		return C.EDS_ERR_OK
	}

	if event == C.kEdsObjectEvent_DirItemRequestTransfer ||
		event == C.kEdsObjectEvent_DirItemRequestTransferDT {

		dirItem := C.EdsDirectoryItemRef(ref)
		if s.isTransferSuppressed() {
			log.Printf("canon: suppressing stale transfer request")
			_ = C.EdsDownloadCancel(dirItem)
			releaseRef(ref)
			return C.EDS_ERR_OK
		}

		// Download directly on this goroutine — no extra thread needed.
		path, err := s.downloadDirectoryItem(dirItem)

		s.mu.Lock()
		req := s.pending
		if req != nil {
			s.pending = nil
		}
		s.mu.Unlock()

		if req != nil {
			notifyCaptureResult(req, captureResult{path: path, err: err})
		} else if err == nil {
			log.Printf("canon: download complete but no pending request (path=%s)", path)
		}
		return C.EDS_ERR_OK
	}

	releaseRef(ref)
	return C.EDS_ERR_OK
}

// onStateEvent handles camera state changes.
// JobStatusChanged with inParameter==0 means the camera finished its current job.
// Shutdown means the camera was physically disconnected.
func (s *Service) onStateEvent(event C.EdsStateEvent, inParameter C.EdsUInt32) C.EdsError {
	switch event {
	case C.kEdsStateEvent_Shutdown:
		log.Printf("canon: camera shutdown event received (disconnection)")

		s.mu.Lock()
		s.closed = true
		s.cameraDisconnected = true
		pending := s.pending
		s.pending = nil
		// Cancel EVF context so evfLoop takes the ctx.Done() path, which closes
		// frameCh and signals HTTP preview clients to stop cleanly.
		evfCancel := s.evfCancel
		s.mu.Unlock()

		if evfCancel != nil {
			evfCancel()
		}
		if pending != nil {
			notifyCaptureResult(pending, captureResult{err: domain.ErrCameraDisconnected})
		}
		activeService.CompareAndSwap(s, nil)
		// Signal ReconnectingService (or any other waiter) that reconnect can begin.
		s.disconnectedOnce.Do(func() { close(s.disconnectedCh) })

	case C.kEdsStateEvent_JobStatusChanged:
		if inParameter == 0 {
			s.jobBusy.Store(0)
			log.Printf("canon: camera job finished")
		} else {
			s.jobBusy.Store(1)
			log.Printf("canon: camera job started")
		}
		select {
		case s.jobStateCh <- struct{}{}:
		default:
		}
	}
	return C.EDS_ERR_OK
}

// ─────────────────────────────────────────────
// Camera configuration
// ─────────────────────────────────────────────

// configureCamera sets SaveTo=Host (so we receive DirItemRequestTransfer) and
// optionally disables flash. EdsSetCapacity is required when SaveTo=Host.
func configureCamera(camera C.EdsCameraRef) error {
	log.Printf("canon: setting SaveTo=Host")
	saveTo := C.EdsUInt32(C.kEdsSaveTo_Host)
	if err := edsCheck("EdsSetPropertyData(SaveTo)", C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveTo)),
		unsafe.Pointer(&saveTo),
	)); err != nil {
		log.Printf("canon: warning: SaveTo=Host not applied: %v", err)
	} else {
		log.Printf("canon: SaveTo=Host configured")
	}

	// EdsSetCapacity is required for SaveTo=Host or the camera may report busy.
	cap := C.EdsCapacity{
		numberOfFreeClusters: C.EdsInt32(0x7FFFFFFF),
		bytesPerSector:       C.EdsInt32(0x1000),
		reset:                C.EdsBool(1),
	}
	if err := edsCheck("EdsSetCapacity", C.EdsSetCapacity(camera, cap)); err != nil {
		log.Printf("canon: warning: EdsSetCapacity failed: %v", err)
	}

	if !flashConfigEnabled() {
		log.Printf("canon: no-flash config skipped (set CANON_CONFIGURE_NO_FLASH=1 to enable)")
		return nil
	}
	log.Printf("canon: applying no-flash config")
	return configureNoFlash(camera)
}

// configureNoFlash aplica FlashOn=0 si la cámara lo soporta.
func configureNoFlash(camera C.EdsCameraRef) error {
	return setOptionalUInt32Property(camera, "FlashOn=0", C.kEdsPropID_FlashOn, C.EdsUInt32(0))
}

// ─────────────────────────────────────────────
// Service lifecycle
// ─────────────────────────────────────────────

// Close detiene el event pump, notifica captura pendiente como cancelada y cierra sesión/SDK.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		activeService.CompareAndSwap(s, nil)

		// Stop EVF first so the camera gets the stop command before the session closes.
		s.StopPreview()

		close(s.stopEventPump)
		<-s.eventPumpDone

		s.mu.Lock()
		s.closed = true
		pending := s.pending
		s.pending = nil
		camera := s.camera
		s.mu.Unlock()

		if pending != nil {
			notifyCaptureResult(pending, captureResult{err: errors.New("capture canceled: service closed")})
		}

		s.bgWg.Wait()

		s.mu.Lock()
		disconnected := s.cameraDisconnected
		s.mu.Unlock()

		var errs []error
		if err := withPlatformThreading(func() error {
			var closeErr error
			if !disconnected {
				// Camera still connected: close the session gracefully.
				closeErr = edsCheck("EdsCloseSession", C.EdsCloseSession(camera))
				releaseRef(C.EdsBaseRef(camera))
			}
			// Decrement SDK ref count; terminates EDSDK when it reaches zero.
			releaseSDK()
			return closeErr
		}); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			s.closeErr = errors.Join(errs...)
		}
	})
	return s.closeErr
}

// Disconnected returns a channel that is closed when the camera fires a shutdown event
// (USB unplugged). Callers can select on it to detect disconnection without polling.
func (s *Service) Disconnected() <-chan struct{} {
	return s.disconnectedCh
}

// startEventPump ejecuta EdsGetEvent en un ticker para que los callbacks EDSDK se disparen.
func (s *Service) startEventPump() {
	go func() {
		defer close(s.eventPumpDone)

		uninitThreading, err := initPlatformThreading()
		if err != nil {
			log.Printf("canon: event pump threading initialization failed: %v", err)
			return
		}
		defer uninitThreading()

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-s.stopEventPump:
				return
			case <-ticker.C:
				_ = C.EdsGetEvent()
			}
		}
	}()
}

// ─────────────────────────────────────────────
// Camera discovery
// ─────────────────────────────────────────────

// waitForFirstCamera hace polling de la lista de cámaras hasta obtener una o superar timeout.
func waitForFirstCamera(timeout time.Duration) (C.EdsBaseRef, error) {
	deadline := time.Now().Add(timeout)
	attempt := 0
	var lastCount uint32

	for {
		attempt++
		cameraRef, count, err := firstCameraRefAndCount()
		if err != nil {
			return nil, err
		}
		lastCount = count
		if cameraRef != nil {
			if attempt > 1 {
				log.Printf("canon: camera discovered after %d attempts", attempt)
			}
			return cameraRef, nil
		}

		if attempt == 1 || attempt%5 == 0 {
			log.Printf("canon: waiting for camera (attempt=%d count=%d)", attempt, count)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"no Canon camera found after %s (count=%d). Check USB mode, disable camera Wi-Fi, and close EOS Utility/other camera apps",
				timeout, lastCount,
			)
		}

		_ = C.EdsGetEvent()
		time.Sleep(300 * time.Millisecond)
	}
}

// firstCameraRefAndCount devuelve la primera cámara de la lista y el total de cámaras.
func firstCameraRefAndCount() (C.EdsBaseRef, uint32, error) {
	var cameraList C.EdsCameraListRef
	if err := edsCheck("EdsGetCameraList", C.EdsGetCameraList(&cameraList)); err != nil {
		return nil, 0, err
	}
	defer releaseRef(C.EdsBaseRef(cameraList))

	var count C.EdsUInt32
	if err := edsCheck("EdsGetChildCount", C.EdsGetChildCount(C.EdsBaseRef(cameraList), &count)); err != nil {
		return nil, 0, err
	}
	if count == 0 {
		return nil, 0, nil
	}

	var cameraBase C.EdsBaseRef
	if err := edsCheck("EdsGetChildAtIndex", C.EdsGetChildAtIndex(C.EdsBaseRef(cameraList), 0, &cameraBase)); err != nil {
		return nil, 0, err
	}
	return cameraBase, uint32(count), nil
}

// openSessionWithRetry abre sesión EDSDK con reintentos ante DEVICE_BUSY / OBJECT_NOTREADY.
func openSessionWithRetry(camera C.EdsCameraRef) error {
	const attempts = 8
	var last C.EdsError
	for i := 0; i < attempts; i++ {
		last = C.EdsOpenSession(camera)
		if last == C.EDS_ERR_OK {
			return nil
		}
		if last == C.EDS_ERR_DEVICE_BUSY ||
			last == C.EDS_ERR_OBJECT_NOTREADY ||
			last == C.EDS_ERR_COMM_PORT_IS_IN_USE {
			time.Sleep(450 * time.Millisecond)
			continue
		}
		return edsCheck("EdsOpenSession", last)
	}
	return edsCheck("EdsOpenSession", last)
}

// ─────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────

func (s *Service) clearPending(expected *captureRequest) {
	s.mu.Lock()
	if s.pending == expected {
		s.pending = nil
	}
	s.mu.Unlock()
}

func (s *Service) waitForTransferJobsIdle(timeout time.Duration) bool {
	if timeout <= 0 {
		return true
	}
	if s.jobBusy.Load() == 0 {
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for s.jobBusy.Load() != 0 {
		select {
		case <-timer.C:
			return s.jobBusy.Load() == 0
		case <-s.jobStateCh:
		}
	}
	return true
}

func (s *Service) isTransferSuppressed() bool {
	s.mu.Lock()
	until := s.suppressTransfersUntil
	s.mu.Unlock()
	if until.IsZero() {
		return false
	}
	return time.Now().Before(until)
}

func notifyCaptureResult(req *captureRequest, result captureResult) {
	if req == nil {
		return
	}
	select {
	case req.done <- result:
	default:
	}
}

func effectiveShutterTimeout(ctx context.Context, configured time.Duration) time.Duration {
	if configured <= 0 {
		return configured
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return configured
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return remaining
	}
	if remaining < configured {
		return remaining
	}
	return configured
}

// ─────────────────────────────────────────────
// Property helpers
// ─────────────────────────────────────────────

func setOptionalUInt32Property(camera C.EdsCameraRef, label string, propID C.EdsPropertyID, value C.EdsUInt32) error {
	var last C.EdsError
	for attempt := 0; attempt < 5; attempt++ {
		code := C.EdsSetPropertyData(
			C.EdsBaseRef(camera),
			propID,
			0,
			C.EdsUInt32(unsafe.Sizeof(value)),
			unsafe.Pointer(&value),
		)
		last = code
		if code == C.EDS_ERR_OK {
			return nil
		}
		if isRetryablePropertyErr(code) {
			_ = C.EdsGetEvent()
			time.Sleep(120 * time.Millisecond)
			continue
		}
		if isIgnorablePropertyErr(code) {
			return nil
		}
		return edsCheck("EdsSetPropertyData("+label+")", code)
	}
	return edsCheck("EdsSetPropertyData("+label+")", last)
}

// ─────────────────────────────────────────────
// Error classification
// ─────────────────────────────────────────────

func isRetryableCommandErr(code C.EdsError) bool {
	switch uint32(code) {
	case uint32(C.EDS_ERR_DEVICE_BUSY),
		uint32(C.EDS_ERR_PTP_DEVICE_BUSY),
		uint32(C.EDS_ERR_OBJECT_NOTREADY):
		return true
	default:
		return false
	}
}

func isRetryablePropertyErr(code C.EdsError) bool {
	switch uint32(code) {
	case uint32(C.EDS_ERR_DEVICE_BUSY),
		uint32(C.EDS_ERR_PTP_DEVICE_BUSY),
		uint32(C.EDS_ERR_OBJECT_NOTREADY),
		uint32(C.EDS_ERR_PROPERTIES_NOT_LOADED):
		return true
	default:
		return false
	}
}

func isIgnorablePropertyErr(code C.EdsError) bool {
	switch uint32(code) {
	case uint32(C.EDS_ERR_NOT_SUPPORTED),
		uint32(C.EDS_ERR_PROPERTIES_UNAVAILABLE),
		uint32(C.EDS_ERR_PROPERTIES_MISMATCH),
		uint32(C.EDS_ERR_INVALID_PARAMETER),
		uint32(C.EDS_ERR_DEVICE_INVALID_PARAMETER),
		uint32(C.EDS_ERR_DEVICEPROP_NOT_SUPPORTED),
		uint32(C.EDS_ERR_INVALID_DEVICEPROP_FORMAT),
		uint32(C.EDS_ERR_INVALID_DEVICEPROP_VALUE):
		return true
	default:
		return false
	}
}

// ─────────────────────────────────────────────
// EDS error helpers
// ─────────────────────────────────────────────

// edsCheck convierte un código EDS en error Go con nombre y hint si aplica.
func edsCheck(op string, code C.EdsError) error {
	if code == C.EDS_ERR_OK {
		return nil
	}
	hint := edsErrHint(code)
	if hint == "" {
		return fmt.Errorf("%s failed: %s (0x%08X)", op, edsErrName(code), uint32(code))
	}
	return fmt.Errorf("%s failed: %s (0x%08X): %s", op, edsErrName(code), uint32(code), hint)
}

func edsErrName(code C.EdsError) string {
	switch uint32(code) {
	case uint32(C.EDS_ERR_DEVICE_NOT_FOUND):
		return "EDS_ERR_DEVICE_NOT_FOUND"
	case uint32(C.EDS_ERR_DEVICE_BUSY):
		return "EDS_ERR_DEVICE_BUSY"
	case uint32(C.EDS_ERR_COMM_PORT_IS_IN_USE):
		return "EDS_ERR_COMM_PORT_IS_IN_USE"
	case uint32(C.EDS_ERR_COMM_DISCONNECTED):
		return "EDS_ERR_COMM_DISCONNECTED"
	case uint32(C.EDS_ERR_SESSION_NOT_OPEN):
		return "EDS_ERR_SESSION_NOT_OPEN"
	case uint32(C.EDS_ERR_OBJECT_NOTREADY):
		return "EDS_ERR_OBJECT_NOTREADY"
	case uint32(C.EDS_ERR_TAKE_PICTURE_AF_NG):
		return "EDS_ERR_TAKE_PICTURE_AF_NG"
	default:
		return "EDS_ERR_UNKNOWN"
	}
}

func edsErrHint(code C.EdsError) string {
	switch uint32(code) {
	case uint32(C.EDS_ERR_COMM_PORT_IS_IN_USE):
		return "another app is connected to the camera; close EOS Utility/Canon apps/webcam tools and reconnect USB"
	case uint32(C.EDS_ERR_DEVICE_BUSY), uint32(C.EDS_ERR_OBJECT_NOTREADY):
		return "camera is busy; wait a few seconds and retry"
	case uint32(C.EDS_ERR_TAKE_PICTURE_AF_NG):
		return "focus was not confirmed; switch to manual focus or improve focus/lighting"
	default:
		return ""
	}
}

// withPlatformThreading is moved to threading_*.go
// initPlatformThreading is moved to threading_*.go

// releaseRef libera una referencia EDS con EdsRelease.
func releaseRef(ref C.EdsBaseRef) {
	if ref != nil {
		_ = C.EdsRelease(ref)
	}
}

// ─────────────────────────────────────────────
// CGo exports
// ─────────────────────────────────────────────

//export goObjectEventHandler
func goObjectEventHandler(inEvent C.EdsObjectEvent, inRef C.EdsBaseRef, _ unsafe.Pointer) C.EdsError {
	svc := activeService.Load()
	if svc == nil {
		releaseRef(inRef)
		return C.EDS_ERR_OK
	}
	return svc.onObjectEvent(inEvent, inRef)
}

//export goCameraStateEventHandler
func goCameraStateEventHandler(inEvent C.EdsStateEvent, inParameter C.EdsUInt32, _ unsafe.Pointer) C.EdsError {
	svc := activeService.Load()
	if svc == nil {
		return C.EDS_ERR_OK
	}
	return svc.onStateEvent(inEvent, inParameter)
}
