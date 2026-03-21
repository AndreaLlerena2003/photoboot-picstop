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
	pumpTaskCh    chan pumpTask // commands to run on the event pump OS thread

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

// pumpTask is a function dispatched to the event pump goroutine for execution
// on its OS thread. All EdsSendCommand calls must go through the pump to avoid
// deadlocking with EdsGetEvent, which holds the same internal EDSDK lock.
type pumpTask struct {
	fn     func() C.EdsError
	result chan C.EdsError
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
		pumpTaskCh:     make(chan pumpTask, 1),
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
	captureStart := time.Now()
	log.Printf("canon: [capture] START — evfActive=%v evfSuspendedByCapture=%v jobBusy=%d",
		s.evfActive.Load(), s.evfSuspendedByCapture.Load(), s.jobBusy.Load())

	req := &captureRequest{
		done: make(chan captureResult, 1),
	}

	// ── Suspend EVF before doing anything with the camera ──────────────────
	// evfSuspendedByCapture tracks whether WE are the goroutine that suspended
	// EVF, so that maybeResumeEVF knows who should restart it.
	if s.evfActive.Load() && !s.evfSuspendedByCapture.Swap(true) {
		// We set evfSuspendedByCapture false→true: we own the suspend.
		log.Printf("canon: [capture] suspending EVF before capture")
		s.suspendEVF()
		log.Printf("canon: [capture] EVF suspended (+%s)", time.Since(captureStart))
	} else {
		log.Printf("canon: [capture] EVF not active or already suspended, skipping suspend")
	}

	// ── Preempt any in-flight capture ──────────────────────────────────────
	var preempted *captureRequest
	var preemptWindow time.Duration

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		log.Printf("canon: [capture] ABORT — service is closed")
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
		log.Printf("canon: [capture] preempting in-progress capture; suppressTransfersUntil=+%s", preemptWindow)
	} else {
		log.Printf("canon: [capture] no pending capture to preempt")
	}
	s.pending = req
	log.Printf("canon: [capture] registered as pending request (+%s)", time.Since(captureStart))
	s.mu.Unlock()

	if preempted != nil {
		notifyCaptureResult(preempted, captureResult{err: domain.ErrCaptureSuperseded})
		s.bgWg.Add(1)
		go func() {
			defer s.bgWg.Done()
			s.resetShutterStateOnPump()
		}()
		if preemptWindow > 0 {
			log.Printf("canon: [capture] sleeping for preempt window %s", preemptWindow)
			time.Sleep(preemptWindow)
		}
	}

	// ── Wait for previous job to finish ────────────────────────────────────
	drainTimeout := jobDrainTimeout()
	log.Printf("canon: [capture] waiting for transfer jobs idle (jobBusy=%d timeout=%s) (+%s)",
		s.jobBusy.Load(), drainTimeout, time.Since(captureStart))
	idled := s.waitForTransferJobsIdle(drainTimeout)
	log.Printf("canon: [capture] transfer jobs idle=%v jobBusy=%d (+%s)",
		idled, s.jobBusy.Load(), time.Since(captureStart))
	if !idled {
		log.Printf("canon: [capture] WARNING: transfer jobs still busy after %s; proceeding anyway", drainTimeout)
	}

	// ── Guard: check context deadline ──────────────────────────────────────
	commandTimeout := effectiveShutterTimeout(ctx, shutterCommandTimeout())
	log.Printf("canon: [capture] effective shutter command timeout=%s (+%s)", commandTimeout, time.Since(captureStart))
	if commandTimeout <= 0 {
		log.Printf("canon: [capture] ABORT — context deadline already exceeded")
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, context.DeadlineExceeded
	}

	// ── Fire shutter ────────────────────────────────────────────────────────
	log.Printf("canon: [capture] firing shutter (+%s)", time.Since(captureStart))
	if err := s.sendTakePictureWithTimeout(commandTimeout); err != nil {
		log.Printf("canon: [capture] shutter FAILED: %v (+%s)", err, time.Since(captureStart))
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, err
	}
	log.Printf("canon: [capture] shutter command SUCCESS — now waiting for DirItemRequestTransfer event (+%s)", time.Since(captureStart))

	// ── Wait for onObjectEvent to deliver the downloaded file ──────────────
	// DirItemRequestTransfer → onObjectEvent → EdsDownload → notifyCaptureResult.
	transferDeadline := hostTransferTimeout()
	log.Printf("canon: [capture] host transfer timeout=%s", transferDeadline)
	select {
	case result := <-req.done:
		log.Printf("canon: [capture] COMPLETE — path=%q err=%v elapsed=%s",
			result.path, result.err, time.Since(captureStart))
		s.maybeResumeEVF()
		return result, nil
	case <-time.After(transferDeadline):
		log.Printf("canon: [capture] TIMEOUT waiting for DirItemRequestTransfer after %s — shutter fired but no transfer event received; check SaveTo mode and SD card", transferDeadline)
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, fmt.Errorf("host transfer timed out after %s", transferDeadline)
	case <-ctx.Done():
		log.Printf("canon: [capture] context cancelled while waiting for transfer: %v (+%s)", ctx.Err(), time.Since(captureStart))
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, ctx.Err()
	}
}

// ─────────────────────────────────────────────
// Shutter commands
// ─────────────────────────────────────────────

// sendTakePicture dispatches TakePicture (and NonAF fallback) to the event pump
// goroutine. Each EdsSendCommand is run on the pump's OS thread to avoid the
// EDSDK internal lock deadlock that occurs when EdsSendCommand and EdsGetEvent
// run concurrently on separate threads.
func (s *Service) sendTakePicture() error {
	log.Printf("canon: [shutter] trying EdsSendCommand(TakePicture) via pump (up to 6 attempts)")
	takePictureCode := s.sendCameraCommandOnPump(
		C.kEdsCameraCommand_TakePicture,
		C.EdsInt32(0),
		6,
		180*time.Millisecond,
	)
	if takePictureCode == C.EDS_ERR_OK {
		log.Printf("canon: [shutter] TakePicture accepted by camera (EDS_ERR_OK)")
		return nil
	}

	log.Printf("canon: [shutter] TakePicture returned: %s (0x%08X)", edsErrName(takePictureCode), uint32(takePictureCode))

	// AF failed — fall back to NonAF shutter.
	if takePictureCode != C.EDS_ERR_TAKE_PICTURE_AF_NG {
		return edsCheck("EdsSendCommand(TakePicture)", takePictureCode)
	}

	log.Printf("canon: [shutter] AF not confirmed; retrying with NonAF shutter (ShutterButton_Completely_NonAF)")
	nonAFPressCode, nonAFReleaseCode := s.pressAndReleaseShutter(
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_Completely_NonAF),
	)
	log.Printf("canon: [shutter] NonAF press=%s (0x%08X) release=%s (0x%08X)",
		edsErrName(nonAFPressCode), uint32(nonAFPressCode),
		edsErrName(nonAFReleaseCode), uint32(nonAFReleaseCode))
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

// sendTakePictureWithTimeout fires the shutter with a timeout guard.
// The actual EdsSendCommand calls run on the event pump goroutine via execOnPump.
func (s *Service) sendTakePictureWithTimeout(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}

	log.Printf("canon: [shutter] sendTakePictureWithTimeout timeout=%s", timeout)
	resultCh := make(chan error, 1)
	s.bgWg.Add(1)
	go func() {
		defer s.bgWg.Done()
		log.Printf("canon: [shutter] goroutine started: dispatching TakePicture to pump thread")
		resultCh <- s.sendTakePicture()
	}()

	select {
	case err := <-resultCh:
		if err == nil {
			log.Printf("canon: [shutter] shutter command returned OK")
		} else {
			log.Printf("canon: [shutter] shutter command returned error: %v", err)
		}
		return err
	case <-time.After(timeout):
		log.Printf("canon: [shutter] TIMEOUT after %s — shutter goroutine still running; sending ShutterButton_OFF to reset", timeout)
		// Best-effort reset on pump thread.
		s.bgWg.Add(1)
		go func() {
			defer s.bgWg.Done()
			s.resetShutterStateOnPump()
		}()
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}
}

func (s *Service) pressAndReleaseShutter(pressParam C.EdsInt32) (C.EdsError, C.EdsError) {
	pressCode := s.sendCameraCommandOnPump(
		C.kEdsCameraCommand_PressShutterButton,
		pressParam,
		10,
		220*time.Millisecond,
	)
	releaseCode := s.sendCameraCommandOnPump(
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF),
		8,
		120*time.Millisecond,
	)
	return pressCode, releaseCode
}

// resetShutterStateOnPump sends ShutterButton_OFF via the pump thread to leave
// the shutter in a known state.
func (s *Service) resetShutterStateOnPump() {
	_ = s.sendCameraCommandOnPump(
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF),
		3,
		120*time.Millisecond,
	)
}

// sendCameraCommandOnPump dispatches each EdsSendCommand attempt to the event pump
// goroutine. Between retries the caller goroutine sleeps, allowing the pump to
// continue calling EdsGetEvent normally.
func (s *Service) sendCameraCommandOnPump(
	command C.EdsCameraCommand,
	param C.EdsInt32,
	attempts int,
	delay time.Duration,
) C.EdsError {
	if attempts < 1 {
		attempts = 1
	}
	cmdName := edsCameraCommandName(command, param)
	camera := s.camera
	log.Printf("canon: [cmd] dispatching EdsSendCommand(%s) to pump — up to %d attempt(s)", cmdName, attempts)
	var code C.EdsError
	for i := 0; i < attempts; i++ {
		code = s.execOnPump(func() C.EdsError {
			return C.EdsSendCommand(camera, command, param)
		})
		log.Printf("canon: [cmd] EdsSendCommand(%s) attempt %d/%d → %s (0x%08X)",
			cmdName, i+1, attempts, edsErrName(code), uint32(code))
		if code == C.EDS_ERR_OK {
			return code
		}
		if !isRetryableCommandErr(code) {
			log.Printf("canon: [cmd] non-retryable error on %s, stopping retries", cmdName)
			return code
		}
		// Sleep in caller goroutine — pump keeps calling EdsGetEvent between retries.
		time.Sleep(delay)
	}
	log.Printf("canon: [cmd] all %d attempt(s) exhausted for %s", attempts, cmdName)
	return code
}

// edsCameraCommandName returns a human-readable name for an EDSDK camera command + param.
func edsCameraCommandName(command C.EdsCameraCommand, param C.EdsInt32) string {
	switch uint32(command) {
	case uint32(C.kEdsCameraCommand_TakePicture):
		return "TakePicture"
	case uint32(C.kEdsCameraCommand_PressShutterButton):
		switch uint32(param) {
		case uint32(C.kEdsCameraCommand_ShutterButton_OFF):
			return "PressShutterButton(OFF)"
		case uint32(C.kEdsCameraCommand_ShutterButton_Completely_NonAF):
			return "PressShutterButton(Completely_NonAF)"
		case uint32(C.kEdsCameraCommand_ShutterButton_Completely):
			return "PressShutterButton(Completely)"
		case uint32(C.kEdsCameraCommand_ShutterButton_Halfway):
			return "PressShutterButton(Halfway)"
		default:
			return fmt.Sprintf("PressShutterButton(0x%08X)", uint32(param))
		}
	default:
		return fmt.Sprintf("Command(0x%08X,0x%08X)", uint32(command), uint32(param))
	}
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
	log.Printf("canon: [download] DirItemInfo: filename=%q size=%d isFolder=%v",
		fileName, uint64(info.size), info.isFolder != 0)
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
	log.Printf("canon: [event] onObjectEvent: event=%s (0x%08X) ref=%v",
		edsObjectEventName(event), uint32(event), ref != nil)

	if ref == nil {
		log.Printf("canon: [event] ref is nil, ignoring event")
		return C.EDS_ERR_OK
	}

	if event == C.kEdsObjectEvent_DirItemRequestTransfer ||
		event == C.kEdsObjectEvent_DirItemRequestTransferDT {

		dirItem := C.EdsDirectoryItemRef(ref)
		suppressed := s.isTransferSuppressed()
		log.Printf("canon: [event] DirItemRequestTransfer received — isTransferSuppressed=%v", suppressed)

		if suppressed {
			log.Printf("canon: [event] suppressing stale transfer request (suppressTransfersUntil window active)")
			_ = C.EdsDownloadCancel(dirItem)
			releaseRef(ref)
			return C.EDS_ERR_OK
		}

		s.mu.Lock()
		hasPending := s.pending != nil
		s.mu.Unlock()
		log.Printf("canon: [event] hasPendingCapture=%v — starting download", hasPending)

		// Download directly on this goroutine — no extra thread needed.
		path, err := s.downloadDirectoryItem(dirItem)
		log.Printf("canon: [event] download finished: path=%q err=%v", path, err)

		s.mu.Lock()
		req := s.pending
		if req != nil {
			s.pending = nil
		}
		s.mu.Unlock()

		if req != nil {
			log.Printf("canon: [event] notifying pending capture request with result")
			notifyCaptureResult(req, captureResult{path: path, err: err})
		} else if err == nil {
			log.Printf("canon: [event] WARNING: download complete but NO pending request to notify (path=%s) — capture() may have already timed out", path)
		}
		return C.EDS_ERR_OK
	}

	log.Printf("canon: [event] unhandled object event — releasing ref")
	releaseRef(ref)
	return C.EDS_ERR_OK
}

// edsObjectEventName maps an EdsObjectEvent to a human-readable name.
func edsObjectEventName(event C.EdsObjectEvent) string {
	switch uint32(event) {
	case uint32(C.kEdsObjectEvent_All):
		return "All"
	case uint32(C.kEdsObjectEvent_DirItemCancelTransferDT):
		return "DirItemCancelTransferDT"
	case uint32(C.kEdsObjectEvent_DirItemContentChanged):
		return "DirItemContentChanged"
	case uint32(C.kEdsObjectEvent_DirItemCreated):
		return "DirItemCreated"
	case uint32(C.kEdsObjectEvent_DirItemInfoChanged):
		return "DirItemInfoChanged"
	case uint32(C.kEdsObjectEvent_DirItemRemoved):
		return "DirItemRemoved"
	case uint32(C.kEdsObjectEvent_DirItemRequestTransfer):
		return "DirItemRequestTransfer"
	case uint32(C.kEdsObjectEvent_DirItemRequestTransferDT):
		return "DirItemRequestTransferDT"
	case uint32(C.kEdsObjectEvent_FolderUpdateItems):
		return "FolderUpdateItems"
	case uint32(C.kEdsObjectEvent_VolumeAdded):
		return "VolumeAdded"
	case uint32(C.kEdsObjectEvent_VolumeInfoChanged):
		return "VolumeInfoChanged"
	case uint32(C.kEdsObjectEvent_VolumeRemoved):
		return "VolumeRemoved"
	case uint32(C.kEdsObjectEvent_VolumeUpdateItems):
		return "VolumeUpdateItems"
	default:
		return "Unknown"
	}
}

// onStateEvent handles camera state changes.
// JobStatusChanged with inParameter==0 means the camera finished its current job.
// Shutdown means the camera was physically disconnected.
func (s *Service) onStateEvent(event C.EdsStateEvent, inParameter C.EdsUInt32) C.EdsError {
	log.Printf("canon: [event] onStateEvent: event=%s (0x%08X) param=0x%08X",
		edsStateEventName(event), uint32(event), uint32(inParameter))

	switch event {
	case C.kEdsStateEvent_Shutdown:
		log.Printf("canon: [event] camera SHUTDOWN — physical disconnection detected")

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
			log.Printf("canon: [event] notifying pending capture of disconnection")
			notifyCaptureResult(pending, captureResult{err: domain.ErrCameraDisconnected})
		}
		activeService.CompareAndSwap(s, nil)
		// Signal ReconnectingService (or any other waiter) that reconnect can begin.
		s.disconnectedOnce.Do(func() { close(s.disconnectedCh) })

	case C.kEdsStateEvent_JobStatusChanged:
		if inParameter == 0 {
			s.jobBusy.Store(0)
			log.Printf("canon: [event] JobStatusChanged → job IDLE (param=0)")
		} else {
			s.jobBusy.Store(1)
			log.Printf("canon: [event] JobStatusChanged → job BUSY (param=%d)", inParameter)
		}
		select {
		case s.jobStateCh <- struct{}{}:
		default:
		}

	default:
		log.Printf("canon: [event] unhandled state event: %s (0x%08X) param=0x%08X",
			edsStateEventName(event), uint32(event), uint32(inParameter))
	}
	return C.EDS_ERR_OK
}

// edsStateEventName maps an EdsStateEvent to a human-readable name.
func edsStateEventName(event C.EdsStateEvent) string {
	switch uint32(event) {
	case uint32(C.kEdsStateEvent_All):
		return "All"
	case uint32(C.kEdsStateEvent_Shutdown):
		return "Shutdown"
	case uint32(C.kEdsStateEvent_JobStatusChanged):
		return "JobStatusChanged"
	case uint32(C.kEdsStateEvent_WillSoonShutDown):
		return "WillSoonShutDown"
	case uint32(C.kEdsStateEvent_ShutDownTimerUpdate):
		return "ShutDownTimerUpdate"
	case uint32(C.kEdsStateEvent_CaptureError):
		return "CaptureError"
	case uint32(C.kEdsStateEvent_InternalError):
		return "InternalError"
	case uint32(C.kEdsStateEvent_AfResult):
		return "AfResult"
	case uint32(C.kEdsStateEvent_BulbExposureTime):
		return "BulbExposureTime"
	default:
		return "Unknown"
	}
}

// ─────────────────────────────────────────────
// Camera configuration
// ─────────────────────────────────────────────

// configureCamera sets SaveTo=Host (so we receive DirItemRequestTransfer) and
// optionally disables flash. EdsSetCapacity is required when SaveTo=Host.
func configureCamera(camera C.EdsCameraRef) error {
	// Read current SaveTo before changing it.
	var saveToBeforeSet C.EdsUInt32
	readErr := C.EdsGetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveToBeforeSet)),
		unsafe.Pointer(&saveToBeforeSet),
	)
	if readErr == C.EDS_ERR_OK {
		log.Printf("canon: [config] SaveTo current value BEFORE set: %s (0x%08X)",
			saveToName(saveToBeforeSet), uint32(saveToBeforeSet))
	} else {
		log.Printf("canon: [config] could not read current SaveTo: %s (0x%08X)", edsErrName(readErr), uint32(readErr))
	}

	log.Printf("canon: [config] setting SaveTo=Host (kEdsSaveTo_Host=0x%08X)", uint32(C.kEdsSaveTo_Host))
	saveTo := C.EdsUInt32(C.kEdsSaveTo_Host)
	if err := edsCheck("EdsSetPropertyData(SaveTo)", C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveTo)),
		unsafe.Pointer(&saveTo),
	)); err != nil {
		log.Printf("canon: [config] WARNING: SaveTo=Host not applied: %v", err)
	} else {
		// Read back to confirm it was applied.
		var saveToAfterSet C.EdsUInt32
		readBackErr := C.EdsGetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_SaveTo,
			0,
			C.EdsUInt32(unsafe.Sizeof(saveToAfterSet)),
			unsafe.Pointer(&saveToAfterSet),
		)
		if readBackErr == C.EDS_ERR_OK {
			log.Printf("canon: [config] SaveTo AFTER set: %s (0x%08X) — expected Host (0x%08X)",
				saveToName(saveToAfterSet), uint32(saveToAfterSet), uint32(C.kEdsSaveTo_Host))
			if saveToAfterSet != C.EdsUInt32(C.kEdsSaveTo_Host) {
				log.Printf("canon: [config] WARNING: SaveTo readback mismatch! Camera may save to SD card instead of host — DirItemRequestTransfer will NOT fire")
			}
		} else {
			log.Printf("canon: [config] SaveTo=Host set returned OK but readback failed: %s (0x%08X)", edsErrName(readBackErr), uint32(readBackErr))
		}
	}

	// EdsSetCapacity is required for SaveTo=Host or the camera may report busy.
	log.Printf("canon: [config] setting EdsSetCapacity (required for SaveTo=Host)")
	cap := C.EdsCapacity{
		numberOfFreeClusters: C.EdsInt32(0x7FFFFFFF),
		bytesPerSector:       C.EdsInt32(0x1000),
		reset:                C.EdsBool(1),
	}
	if err := edsCheck("EdsSetCapacity", C.EdsSetCapacity(camera, cap)); err != nil {
		log.Printf("canon: [config] WARNING: EdsSetCapacity failed: %v — host transfer may fail", err)
	} else {
		log.Printf("canon: [config] EdsSetCapacity OK")
	}

	if !flashConfigEnabled() {
		log.Printf("canon: [config] no-flash config skipped (set CANON_CONFIGURE_NO_FLASH=1 to enable)")
		return nil
	}
	log.Printf("canon: [config] applying no-flash config")
	return configureNoFlash(camera)
}

// saveToName returns a human-readable label for an EdsUInt32 SaveTo value.
func saveToName(v C.EdsUInt32) string {
	switch uint32(v) {
	case uint32(C.kEdsSaveTo_Camera):
		return "Camera"
	case uint32(C.kEdsSaveTo_Host):
		return "Host"
	case uint32(C.kEdsSaveTo_Both):
		return "Both"
	default:
		return "Unknown"
	}
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

// startEventPump runs EdsGetEvent on a dedicated OS thread (COM STA) so that
// EDSDK callbacks fire correctly. It also executes pumpTask commands on the
// same thread — this is critical: EdsSendCommand and EdsGetEvent must share a
// single OS thread to avoid internal EDSDK lock deadlocks.
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

			case task := <-s.pumpTaskCh:
				// Run the EDSDK command on this OS thread, then send the result.
				// While this runs, EdsGetEvent is NOT called — that's intentional:
				// EdsSendCommand holds the EDSDK internal lock so EdsGetEvent would
				// block anyway. After the command returns, the ticker resumes pumping.
				log.Printf("canon: [pump] executing task on pump thread")
				code := task.fn()
				log.Printf("canon: [pump] task complete: %s (0x%08X)", edsErrName(code), uint32(code))
				task.result <- code

			case <-ticker.C:
				_ = C.EdsGetEvent()
			}
		}
	}()
}

// execOnPump dispatches fn to the event pump goroutine and waits for the result.
// Use this for all EdsSendCommand calls to guarantee single-threaded EDSDK access.
func (s *Service) execOnPump(fn func() C.EdsError) C.EdsError {
	task := pumpTask{fn: fn, result: make(chan C.EdsError, 1)}
	select {
	case s.pumpTaskCh <- task:
		return <-task.result
	case <-s.stopEventPump:
		log.Printf("canon: [pump] execOnPump: pump stopped, returning internal error")
		return C.EDS_ERR_INTERNAL_ERROR
	}
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
	case uint32(C.EDS_ERR_OK):
		return "EDS_ERR_OK"
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
	case uint32(C.EDS_ERR_INTERNAL_ERROR):
		return "EDS_ERR_INTERNAL_ERROR"
	case uint32(C.EDS_ERR_MEM_ALLOC_FAILED):
		return "EDS_ERR_MEM_ALLOC_FAILED"
	case uint32(C.EDS_ERR_INVALID_HANDLE):
		return "EDS_ERR_INVALID_HANDLE"
	case uint32(C.EDS_ERR_INVALID_PARAMETER):
		return "EDS_ERR_INVALID_PARAMETER"
	case uint32(C.EDS_ERR_NOT_SUPPORTED):
		return "EDS_ERR_NOT_SUPPORTED"
	default:
		return fmt.Sprintf("EDS_ERR_0x%08X", uint32(code))
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
