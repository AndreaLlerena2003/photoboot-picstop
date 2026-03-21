//go:build (windows || darwin) && cgo

package canon

import "C"
import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"photoboot-picstop/internal/domain"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

/*
#cgo windows CFLAGS: -I${SRCDIR}/../../../edsdk
#cgo windows LDFLAGS: -L${SRCDIR}/../../../edsdk -l:EDSDK.lib -lole32 -luser32
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

// pumpWindowsMessages drains the calling thread's Windows message queue.
// This is required on COM STA threads: camera USB ACKs are posted as Windows
// messages to the thread that sent the EDSDK command. Without pumping, those
// ACKs queue up and EdsSendCommand / EdsSendStatusCommand block indefinitely.
#ifdef _WIN32
static void pumpWindowsMessages(void) {
    MSG msg;
    while (PeekMessageW(&msg, NULL, 0, 0, PM_REMOVE)) {
        if (msg.message == WM_QUIT) { PostQuitMessage(0); break; }
        TranslateMessage(&msg);
        DispatchMessageW(&msg);
    }
}
#else
static void pumpWindowsMessages(void) {
	// empty on non windows
}
#endif
*/
import "C"

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

	if err := acquireSDK(); err != nil {
		return nil, err
	}
	cInfo("[init] SDK initialized")
	sdkInitialized := true
	defer func() {
		if retErr != nil && sdkInitialized {
			releaseSDK()
		}
	}()

	discoveryTimeout := cameraDiscoveryTimeout()
	cInfo("[init] discovering camera (timeout=%s)", discoveryTimeout)
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
	if err := edsCheck("EdsSetObjectEventHandler", C.EdsSetObjectEventHandler(camera, C.kEdsObjectEvent_All, handler, nil)); err != nil {
		return nil, err
	}
	cInfo("[init] object event handler registered")
	stateHandler := (C.EdsStateEventHandler)(C.goCameraStateEventHandler)
	if err := edsCheck("EdsSetCameraStateEventHandler", C.EdsSetCameraStateEventHandler(camera, C.kEdsStateEvent_All, stateHandler, nil)); err != nil {
		return nil, err
	}
	cInfo("[init] state event handler registered")

	if err := openSessionWithRetry(camera); err != nil {
		return nil, err
	}
	cInfo("[init] camera session opened")
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

	if err := configureCamera(camera); err != nil {
		return nil, err
	}
	cInfo("[init] camera configured")

	cInfo("[init] starting event pump")
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
	SetGoroutineRole("capture")
	captureStart := time.Now()
	cInfo("[capture] START — evfActive=%v evfSuspendedByCapture=%v jobBusy=%d",
		s.evfActive.Load(), s.evfSuspendedByCapture.Load(), s.jobBusy.Load())

	req := &captureRequest{
		done: make(chan captureResult, 1),
	}

	// ── Suspend EVF before doing anything with the camera ──────────────────
	// evfSuspendedByCapture tracks whether WE are the goroutine that suspended
	// EVF, so that maybeResumeEVF knows who should restart it.
	if s.evfActive.Load() && !s.evfSuspendedByCapture.Swap(true) {
		// We set evfSuspendedByCapture false→true: we own the suspend.
		cInfo("[capture] suspending EVF before capture")
		s.suspendEVF()
		cInfo("[capture] EVF suspended (+%s)", time.Since(captureStart))
	} else {
		cDebug("[capture] EVF not active or already suspended, skipping suspend")
	}

	// ── Preempt any in-flight capture ──────────────────────────────────────
	var preempted *captureRequest
	var preemptWindow time.Duration

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cWarn("[capture] ABORT — service is closed")
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
		cInfo("[capture] preempting in-progress capture; suppressTransfersUntil=+%s", preemptWindow)
	} else {
		cDebug("[capture] no pending capture to preempt")
	}
	s.pending = req
	cDebug("[capture] registered as pending request (+%s)", time.Since(captureStart))
	s.mu.Unlock()

	if preempted != nil {
		notifyCaptureResult(preempted, captureResult{err: domain.ErrCaptureSuperseded})
		s.bgWg.Add(1)
		go func() {
			SetGoroutineRole("preempt")
			defer s.bgWg.Done()
			_ = withPlatformThreading(func() error {
				s.resetShutterState()
				return nil
			})
		}()
		if preemptWindow > 0 {
			cInfo("[capture] sleeping for preempt window %s", preemptWindow)
			time.Sleep(preemptWindow)
		}
	}

	// ── Wait for previous job to finish ────────────────────────────────────
	drainTimeout := jobDrainTimeout()
	cDebug("[capture] waiting for transfer jobs idle (jobBusy=%d timeout=%s) (+%s)",
		s.jobBusy.Load(), drainTimeout, time.Since(captureStart))
	idled := s.waitForTransferJobsIdle(drainTimeout)
	cDebug("[capture] transfer jobs idle=%v jobBusy=%d (+%s)",
		idled, s.jobBusy.Load(), time.Since(captureStart))
	if !idled {
		cWarn("[capture] transfer jobs still busy after %s; proceeding anyway", drainTimeout)
	}

	// ── Guard: check context deadline ──────────────────────────────────────
	commandTimeout := effectiveShutterTimeout(ctx, shutterCommandTimeout())
	cDebug("[capture] effective shutter command timeout=%s (+%s)", commandTimeout, time.Since(captureStart))
	if commandTimeout <= 0 {
		cWarn("[capture] ABORT — context deadline already exceeded")
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, context.DeadlineExceeded
	}

	// ── Fire shutter ────────────────────────────────────────────────────────
	cInfo("[capture] firing shutter (+%s)", time.Since(captureStart))
	if err := s.sendTakePictureWithTimeout(commandTimeout); err != nil {
		cError("[capture] shutter FAILED: %v (+%s)", err, time.Since(captureStart))
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, err
	}
	cInfo("[capture] shutter command SUCCESS — now waiting for DirItemRequestTransfer event (+%s)", time.Since(captureStart))

	// ── Wait for onObjectEvent to deliver the downloaded file ──────────────
	// DirItemRequestTransfer → onObjectEvent → EdsDownload → notifyCaptureResult.
	transferDeadline := hostTransferTimeout()
	cDebug("[capture] host transfer timeout=%s", transferDeadline)
	select {
	case result := <-req.done:
		cInfo("[capture] COMPLETE — path=%q err=%v elapsed=%s",
			result.path, result.err, time.Since(captureStart))
		s.maybeResumeEVF()
		return result, nil
	case <-time.After(transferDeadline):
		cError("[capture] TIMEOUT waiting for DirItemRequestTransfer after %s — shutter fired but no transfer event received; check SaveTo mode and SD card", transferDeadline)
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, fmt.Errorf("host transfer timed out after %s", transferDeadline)
	case <-ctx.Done():
		cWarn("[capture] context cancelled while waiting for transfer: %v (+%s)", ctx.Err(), time.Since(captureStart))
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, ctx.Err()
	}
}

// ─────────────────────────────────────────────
// Shutter commands
// ─────────────────────────────────────────────

// sendTakePicture acquires the camera UI lock (EDSDK §2.7), fires the shutter
// via PressShutterButton, then releases the lock unconditionally.
//
// THREADING: Must be called from within a withPlatformThreading context (a
// dedicated COM STA OS thread, separate from the event pump). The pump goroutine
// runs EdsGetEvent concurrently, which delivers the camera's USB ACKs so that
// PressShutterButton can return. pumpWindowsMessages() is called between each
// command to drain this thread's Windows message queue for the same reason.
//
// Shutter strategy: ShutterButton_Completely (AF) first; NonAF fallback on failure.
func (s *Service) sendTakePicture() error {
	uiLocked := s.uiLock()
	defer s.uiUnlock(uiLocked)

	cInfo("[shutter] pressing ShutterButton_Completely (uiLocked=%v)", uiLocked)
	pressCode, releaseCode := s.pressAndReleaseShutter(
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_Completely),
	)
	cDebug("[shutter] ShutterButton_Completely press=%s (0x%08X) release=%s (0x%08X)",
		edsErrName(pressCode), uint32(pressCode),
		edsErrName(releaseCode), uint32(releaseCode))

	if pressCode == C.EDS_ERR_OK {
		if releaseCode != C.EDS_ERR_OK {
			return edsCheck("EdsSendCommand(ShutterButton_OFF after Completely)", releaseCode)
		}
		cInfo("[shutter] ShutterButton_Completely succeeded — camera firing, waiting for transfer event")
		return nil
	}

	cWarn("[shutter] ShutterButton_Completely failed (%s); retrying with NonAF", edsErrName(pressCode))
	nonAFPressCode, nonAFReleaseCode := s.pressAndReleaseShutter(
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_Completely_NonAF),
	)
	cDebug("[shutter] ShutterButton_NonAF press=%s (0x%08X) release=%s (0x%08X)",
		edsErrName(nonAFPressCode), uint32(nonAFPressCode),
		edsErrName(nonAFReleaseCode), uint32(nonAFReleaseCode))

	if nonAFPressCode == C.EDS_ERR_OK {
		if nonAFReleaseCode != C.EDS_ERR_OK {
			return edsCheck("EdsSendCommand(ShutterButton_OFF after NonAF)", nonAFReleaseCode)
		}
		cInfo("[shutter] ShutterButton_NonAF succeeded — camera firing, waiting for transfer event")
		return nil
	}

	pressErr := edsCheck("EdsSendCommand(ShutterButton_Completely)", pressCode)
	nonAFErr := edsCheck("EdsSendCommand(ShutterButton_Completely_NonAF)", nonAFPressCode)
	return fmt.Errorf("%w; NonAF also failed: %v", pressErr, nonAFErr)
}

// uiLock sends UILock directly on the current STA thread (EDSDK §2.7).
// Must be called from within a withPlatformThreading context.
func (s *Service) uiLock() bool {
	cDebug("[shutter] sending UILock")
	code := C.EdsSendStatusCommand(s.camera, C.kEdsCameraStatusCommand_UILock, 0)
	cDebug("[shutter] UILock → %s (0x%08X)", edsErrName(code), uint32(code))
	if code == C.EDS_ERR_OK {
		cInfo("[shutter] UI lock acquired")
		return true
	}
	cWarn("[shutter] UILock failed (%s 0x%08X) — proceeding without lock", edsErrName(code), uint32(code))
	return false
}

// uiUnlock sends UIUnLock directly on the current STA thread.
// Must be called from within a withPlatformThreading context. Always called via defer.
func (s *Service) uiUnlock(locked bool) {
	if !locked {
		cDebug("[shutter] UIUnlock skipped (lock was not held)")
		return
	}
	code := C.EdsSendStatusCommand(s.camera, C.kEdsCameraStatusCommand_UIUnLock, 0)
	cDebug("[shutter] UIUnLock → %s (0x%08X)", edsErrName(code), uint32(code))
	if code == C.EDS_ERR_OK {
		cInfo("[shutter] UI lock released")
	} else {
		cWarn("[shutter] UIUnLock failed (%s 0x%08X)", edsErrName(code), uint32(code))
	}
}

// sendTakePictureWithTimeout fires the shutter on a dedicated STA OS thread
// (separate from the event pump) with a timeout guard.
//
// Why a separate thread: PressShutterButton sends a USB command and waits for
// the camera's ACK. That ACK arrives via EdsGetEvent on the pump thread. Both
// threads must run concurrently — routing commands through the pump blocks
// EdsGetEvent and deadlocks the whole flow.
func (s *Service) sendTakePictureWithTimeout(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}

	cInfo("[shutter] sendTakePictureWithTimeout timeout=%s", timeout)
	resultCh := make(chan error, 1)
	s.bgWg.Add(1)
	go func() {
		SetGoroutineRole("shutter")
		defer s.bgWg.Done()
		cDebug("[shutter] goroutine started: initializing dedicated STA thread")
		resultCh <- withPlatformThreading(func() error {
			return s.sendTakePicture()
		})
	}()

	select {
	case err := <-resultCh:
		if err == nil {
			cInfo("[shutter] shutter commands returned OK")
		} else {
			cError("[shutter] shutter commands returned error: %v", err)
		}
		return err
	case <-time.After(timeout):
		cError("[shutter] TIMEOUT after %s — shutter goroutine still blocked; spawning reset goroutine", timeout)
		s.bgWg.Add(1)
		go func() {
			SetGoroutineRole("reset")
			defer s.bgWg.Done()
			_ = withPlatformThreading(func() error {
				s.resetShutterState()
				return nil
			})
		}()
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}
}

// pressAndReleaseShutter sends press+release on the current STA OS thread.
// pumpWindowsMessages is called between press and release to drain this thread's
// Windows message queue so COM STA can deliver the camera's ACK for the press.
func (s *Service) pressAndReleaseShutter(pressParam C.EdsInt32) (C.EdsError, C.EdsError) {
	pressCode := sendCameraCommandDirect(s.camera,
		C.kEdsCameraCommand_PressShutterButton, pressParam, 10, 220*time.Millisecond)
	C.pumpWindowsMessages()
	releaseCode := sendCameraCommandDirect(s.camera,
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF), 8, 120*time.Millisecond)
	return pressCode, releaseCode
}

// resetShutterState sends ShutterButton_OFF to leave the shutter in a known state.
// Must be called from within a withPlatformThreading context.
func (s *Service) resetShutterState() {
	_ = sendCameraCommandDirect(s.camera,
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF), 3, 120*time.Millisecond)
}

// sendCameraCommandDirect calls EdsSendCommand on the current OS thread with retries.
// Must be called from within a withPlatformThreading context (dedicated COM STA thread).
// Pumps Windows messages and calls EdsGetEvent between retries so camera ACKs
// that arrive as Windows messages can be processed before the next attempt.
func sendCameraCommandDirect(
	camera C.EdsCameraRef,
	command C.EdsCameraCommand,
	param C.EdsInt32,
	attempts int,
	delay time.Duration,
) C.EdsError {
	if attempts < 1 {
		attempts = 1
	}
	cmdName := edsCameraCommandName(command, param)
	cDebug("[cmd] EdsSendCommand(%s) — up to %d attempt(s)", cmdName, attempts)
	var code C.EdsError
	for i := 0; i < attempts; i++ {
		code = C.EdsSendCommand(camera, command, param)
		cDebug("[cmd] EdsSendCommand(%s) attempt %d/%d → %s (0x%08X)",
			cmdName, i+1, attempts, edsErrName(code), uint32(code))
		if code == C.EDS_ERR_OK {
			return code
		}
		if !isRetryableCommandErr(code) {
			cWarn("[cmd] non-retryable error on %s, stopping retries", cmdName)
			return code
		}
		C.pumpWindowsMessages()
		_ = C.EdsGetEvent()
		time.Sleep(delay)
	}
	cWarn("[cmd] all %d attempt(s) exhausted for %s", attempts, cmdName)
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
			return "PressShutterButton(Completely_AF)"
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
	SetGoroutineRole("evf")
	defer s.bgWg.Done()
	defer close(done)
	defer s.evfActive.Store(false)

	uninitThreading, err := initPlatformThreading()
	if err != nil {
		cError("[evf] threading init failed: %v", err)
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
		cError("[evf] failed to start EVF mode: %v", err)
		close(frameCh)
		s.mu.Lock()
		s.evfFrameCh = nil
		s.evfCtx = nil
		s.evfCancel = nil
		s.mu.Unlock()
		return
	}
	cInfo("[evf] EVF mode started")

	ticker := time.NewTicker(evfFrameInterval())
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			// Suspend for capture: stop EVF mode on camera but keep frameCh open.
			stopEVFMode(camera)
			cInfo("[evf] EVF suspended for capture")
			return

		case <-ctx.Done():
			// Client disconnected: stop EVF and close the channel.
			stopEVFMode(camera)
			cInfo("[evf] EVF stopped (client disconnected)")
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
				cWarn("[evf] frame error: %v", err)
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
	cInfo("[download] DirItemInfo: filename=%q size=%d isFolder=%v",
		fileName, uint64(info.size), info.isFolder != 0)
	destPath := filepath.Join(s.outputDir, fileName)

	cPath := C.CString(destPath)
	defer C.free(unsafe.Pointer(cPath))

	var stream C.EdsStreamRef
	cDebug("[download] creating file stream at %s", destPath)
	if err := edsCheck("EdsCreateFileStream",
		C.createFileStreamW(
			cPath,
			C.kEdsFileCreateDisposition_CreateAlways,
			C.kEdsAccess_ReadWrite,
			&stream,
		)); err != nil {
		cError("[download] EdsCreateFileStream failed: %v", err)
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("create file stream: %w", err)
	}
	defer releaseRef(C.EdsBaseRef(stream))

	cDebug("[download] calling EdsDownload (size=%d bytes)", uint64(info.size))
	if err := edsCheck("EdsDownload",
		C.EdsDownload(dirItem, C.EdsUInt64(info.size), stream)); err != nil {
		cError("[download] EdsDownload failed: %v", err)
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("download: %w", err)
	}
	cDebug("[download] EdsDownload OK")

	cDebug("[download] calling EdsDownloadComplete")
	if err := edsCheck("EdsDownloadComplete",
		C.EdsDownloadComplete(dirItem)); err != nil {
		cError("[download] EdsDownloadComplete failed: %v", err)
		releaseRef(C.EdsBaseRef(dirItem))
		return "", fmt.Errorf("download complete: %w", err)
	}
	cInfo("[download] EdsDownloadComplete OK — file saved to %s", destPath)

	releaseRef(C.EdsBaseRef(dirItem))
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
	cDebug("[event] onObjectEvent: event=%s (0x%08X) ref=%v",
		edsObjectEventName(event), uint32(event), ref != nil)

	if ref == nil {
		cWarn("[event] ref is nil, ignoring event")
		return C.EDS_ERR_OK
	}

	if event == C.kEdsObjectEvent_DirItemRequestTransfer ||
		event == C.kEdsObjectEvent_DirItemRequestTransferDT {

		dirItem := C.EdsDirectoryItemRef(ref)
		suppressed := s.isTransferSuppressed()
		cInfo("[event] DirItemRequestTransfer received — isTransferSuppressed=%v", suppressed)

		if suppressed {
			cWarn("[event] suppressing stale transfer request (suppressTransfersUntil window active)")
			_ = C.EdsDownloadCancel(dirItem)
			releaseRef(ref)
			return C.EDS_ERR_OK
		}

		s.mu.Lock()
		hasPending := s.pending != nil
		s.mu.Unlock()
		cDebug("[event] hasPendingCapture=%v — starting download", hasPending)

		// Download directly on this goroutine — no extra thread needed.
		path, err := s.downloadDirectoryItem(dirItem)
		cDebug("[event] download finished: path=%q err=%v", path, err)

		s.mu.Lock()
		req := s.pending
		if req != nil {
			s.pending = nil
		}
		s.mu.Unlock()

		if req != nil {
			cDebug("[event] notifying pending capture request with result")
			notifyCaptureResult(req, captureResult{path: path, err: err})
		} else if err == nil {
			cWarn("[event] download complete but NO pending request to notify (path=%s) — capture() may have already timed out", path)
		}
		return C.EDS_ERR_OK
	}

	cDebug("[event] unhandled object event — releasing ref")
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
	cDebug("[event] onStateEvent: event=%s (0x%08X) param=0x%08X",
		edsStateEventName(event), uint32(event), uint32(inParameter))

	switch event {
	case C.kEdsStateEvent_Shutdown:
		cInfo("[event] camera SHUTDOWN — physical disconnection detected")

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
			cInfo("[event] notifying pending capture of disconnection")
			notifyCaptureResult(pending, captureResult{err: domain.ErrCameraDisconnected})
		}
		activeService.CompareAndSwap(s, nil)
		// Signal ReconnectingService (or any other waiter) that reconnect can begin.
		s.disconnectedOnce.Do(func() { close(s.disconnectedCh) })

	case C.kEdsStateEvent_JobStatusChanged:
		if inParameter == 0 {
			s.jobBusy.Store(0)
			cInfo("[event] JobStatusChanged → job IDLE (param=0)")
		} else {
			s.jobBusy.Store(1)
			cInfo("[event] JobStatusChanged → job BUSY (param=%d)", inParameter)
		}
		select {
		case s.jobStateCh <- struct{}{}:
		default:
		}

	default:
		cDebug("[event] unhandled state event: %s (0x%08X) param=0x%08X",
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
		cInfo("[config] SaveTo current value BEFORE set: %s (0x%08X)",
			saveToName(saveToBeforeSet), uint32(saveToBeforeSet))
	} else {
		cWarn("[config] could not read current SaveTo: %s (0x%08X)", edsErrName(readErr), uint32(readErr))
	}

	cInfo("[config] setting SaveTo=Host (kEdsSaveTo_Host=0x%08X)", uint32(C.kEdsSaveTo_Host))
	saveTo := C.EdsUInt32(C.kEdsSaveTo_Host)
	if err := edsCheck("EdsSetPropertyData(SaveTo)", C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveTo)),
		unsafe.Pointer(&saveTo),
	)); err != nil {
		cWarn("[config] SaveTo=Host not applied: %v", err)
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
			cInfo("[config] SaveTo AFTER set: %s (0x%08X) — expected Host (0x%08X)",
				saveToName(saveToAfterSet), uint32(saveToAfterSet), uint32(C.kEdsSaveTo_Host))
			if saveToAfterSet != C.EdsUInt32(C.kEdsSaveTo_Host) {
				cWarn("[config] SaveTo readback mismatch! Camera may save to SD card instead of host — DirItemRequestTransfer will NOT fire")
			}
		} else {
			cWarn("[config] SaveTo=Host set returned OK but readback failed: %s (0x%08X)", edsErrName(readBackErr), uint32(readBackErr))
		}
	}

	// EdsSetCapacity is required for SaveTo=Host or the camera may report busy.
	cInfo("[config] setting EdsSetCapacity (required for SaveTo=Host)")
	cap := C.EdsCapacity{
		numberOfFreeClusters: C.EdsInt32(0x7FFFFFFF),
		bytesPerSector:       C.EdsInt32(0x1000),
		reset:                C.EdsBool(1),
	}
	if err := edsCheck("EdsSetCapacity", C.EdsSetCapacity(camera, cap)); err != nil {
		cWarn("[config] EdsSetCapacity failed: %v — host transfer may fail", err)
	} else {
		cInfo("[config] EdsSetCapacity OK")
	}

	if !flashConfigEnabled() {
		cDebug("[config] no-flash config skipped (set CANON_CONFIGURE_NO_FLASH=1 to enable)")
		return nil
	}
	cInfo("[config] applying no-flash config")
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
		cInfo("[close] Service.Close called")
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
		cInfo("[close] Service.Close complete err=%v", s.closeErr)
	})
	return s.closeErr
}

// Disconnected returns a channel that is closed when the camera fires a shutdown event
// (USB unplugged). Callers can select on it to detect disconnection without polling.
func (s *Service) Disconnected() <-chan struct{} {
	return s.disconnectedCh
}

// startEventPump runs EdsGetEvent on a dedicated OS thread (COM STA) so that
// EDSDK callbacks fire correctly. The shutter goroutine runs on its own STA
// thread via withPlatformThreading, so this goroutine only pumps events.
func (s *Service) startEventPump() {
	go func() {
		SetGoroutineRole("pump")
		defer close(s.eventPumpDone)

		uninitThreading, err := initPlatformThreading()
		if err != nil {
			cError("[pump] threading initialization failed: %v", err)
			return
		}
		defer uninitThreading()

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()

		var pumpCalls uint64

		for {
			select {
			case <-s.stopEventPump:
				return

			case <-ticker.C:
				pumpCalls++
				result := C.EdsGetEvent()
				if pumpCalls%500 == 0 {
					cDebug("[pump] heartbeat: %d EdsGetEvent calls", pumpCalls)
				}
				if result != C.EDS_ERR_OK {
					cWarn("[pump] EdsGetEvent returned %s (0x%08X)", edsErrName(result), uint32(result))
				}
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
				cInfo("[init] camera discovered after %d attempts", attempt)
			}
			return cameraRef, nil
		}

		if attempt == 1 || attempt%5 == 0 {
			cInfo("[init] waiting for camera (attempt=%d count=%d)", attempt, count)
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
		cDebug("[session] EdsOpenSession attempt %d/%d → %s (0x%08X)", i+1, attempts, edsErrName(last), uint32(last))
		if last == C.EDS_ERR_OK {
			return nil
		}
		if last == C.EDS_ERR_DEVICE_BUSY ||
			last == C.EDS_ERR_OBJECT_NOTREADY ||
			last == C.EDS_ERR_COMM_PORT_IS_IN_USE {
			cDebug("[session] retrying EdsOpenSession in 450ms (err=%s)", edsErrName(last))
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
	cDebug("[drain] jobBusy=%d — waiting for job to become idle (timeout=%s)", s.jobBusy.Load(), timeout)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for s.jobBusy.Load() != 0 {
		select {
		case <-timer.C:
			return s.jobBusy.Load() == 0
		case <-s.jobStateCh:
			cDebug("[drain] jobStateCh signal received, jobBusy=%d", s.jobBusy.Load())
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
		cDebug("[prop] EdsSetPropertyData(%s) attempt %d/5 → %s (0x%08X)", label, attempt+1, edsErrName(code), uint32(code))
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
		cWarn("[event] goObjectEventHandler: activeService is nil, dropping event 0x%08X", uint32(inEvent))
		releaseRef(inRef)
		return C.EDS_ERR_OK
	}
	return svc.onObjectEvent(inEvent, inRef)
}

//export goCameraStateEventHandler
func goCameraStateEventHandler(inEvent C.EdsStateEvent, inParameter C.EdsUInt32, _ unsafe.Pointer) C.EdsError {
	svc := activeService.Load()
	if svc == nil {
		cWarn("[event] goCameraStateEventHandler: activeService is nil, dropping event 0x%08X", uint32(inEvent))
		return C.EDS_ERR_OK
	}
	return svc.onStateEvent(inEvent, inParameter)
}
