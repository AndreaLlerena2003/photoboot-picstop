//go:build (windows || darwin) && cgo

package canon

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
// On the SDK thread (COM STA), calling this between EDSDK command retries helps
// drain any queued Windows messages so the next attempt starts clean.
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
static void pumpWindowsMessages(void) {}
#endif
*/
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

// activeService holds the current Service so CGo callbacks can dispatch events.
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
		if err := edsCheck("EdsTerminateSDK", C.EdsTerminateSDK()); err != nil {
			cWarn("[sdk] EdsTerminateSDK failed: %v", err)
		}
	}
}

// Service implements the camera port using Canon EDSDK on Windows.
//
// Threading model: a single dedicated OS thread (the "SDK thread") owns all EDSDK
// state for this Service's lifetime. Per EDSDK §2.8, callbacks fire on the thread
// that established the session. All EdsSendCommand calls must also come from this
// thread so their USB ACKs arrive in its Windows message queue (COM STA) and are
// processed by EdsSendCommand or EdsGetEvent on the same thread.
// Other goroutines communicate with the SDK thread via sdkCh.
type Service struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error

	camera C.EdsCameraRef

	outputDir              string
	pending                *captureRequest
	closed                 bool
	cameraDisconnected     bool // set by onStateEvent(kEdsStateEvent_Shutdown)
	jobBusy                atomic.Uint32
	suppressTransfersUntil time.Time

	// disconnectedCh is closed (exactly once) when kEdsStateEvent_Shutdown fires.
	disconnectedCh   chan struct{}
	disconnectedOnce sync.Once

	// sdkCh delivers tasks to the SDK thread. Buffer of 4 prevents deadlock when
	// the preempt goroutine and the capture goroutine both post near-simultaneously.
	sdkCh         chan func()
	stopEventPump chan struct{}
	eventPumpDone chan struct{}
	jobStateCh    chan struct{}

	// EVF (Electronic Viewfinder / live preview) state — all guarded by mu.
	evfActive             atomic.Bool
	evfSuspendedByCapture atomic.Bool
	evfStop               chan struct{}
	evfDone               chan struct{}
	evfFrameCh            chan []byte
	evfCtx                context.Context
	evfCancel             context.CancelFunc

	bgWg sync.WaitGroup
}

type captureRequest struct {
	done chan captureResult
}

type captureResult struct {
	path string
	err  error
}

// NewService creates a Service, spawns the SDK thread, waits for camera init, and returns.
func NewService(outputDir string) (_ *Service, retErr error) {
	absDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve capture dir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create capture dir: %w", err)
	}

	svc := &Service{
		outputDir:      absDir,
		sdkCh:          make(chan func(), 4),
		stopEventPump:  make(chan struct{}),
		eventPumpDone:  make(chan struct{}),
		jobStateCh:     make(chan struct{}, 1),
		disconnectedCh: make(chan struct{}),
	}

	initDone := make(chan error, 1)
	go svc.runSDKThread(initDone)

	if err := <-initDone; err != nil {
		return nil, err
	}
	return svc, nil
}

// ─────────────────────────────────────────────
// SDK thread
// ─────────────────────────────────────────────

// runSDKThread is the single goroutine that owns all EDSDK state. It:
//  1. Locks itself to one OS thread and initialises COM STA (via initPlatformThreading).
//  2. Initialises the SDK, discovers the camera, opens the session, configures it.
//  3. Signals initDone so NewService can return.
//  4. Enters the event loop: EdsGetEvent every 20 ms, plus tasks from sdkCh.
//  5. On stopEventPump, closes the session, releases the SDK ref, and returns.
func (s *Service) runSDKThread(initDone chan<- error) {
	SetGoroutineRole("sdk")
	defer close(s.eventPumpDone)

	// Lock this goroutine to its OS thread and init COM STA for the whole lifetime.
	uninitThreading, err := initPlatformThreading()
	if err != nil {
		initDone <- fmt.Errorf("platform threading init: %w", err)
		return
	}
	defer uninitThreading()

	// ── SDK + camera init ──────────────────────────────────────────────────
	if err := acquireSDK(); err != nil {
		initDone <- err
		return
	}
	cInfo("[init] SDK initialized")

	discoveryTimeout := cameraDiscoveryTimeout()
	cInfo("[init] discovering camera (timeout=%s)", discoveryTimeout)
	cameraBase, err := waitForFirstCamera(discoveryTimeout)
	if err != nil {
		releaseSDK()
		initDone <- err
		return
	}
	camera := C.EdsCameraRef(cameraBase)

	handler := (C.EdsObjectEventHandler)(C.goObjectEventHandler)
	if err := edsCheck("EdsSetObjectEventHandler",
		C.EdsSetObjectEventHandler(camera, C.kEdsObjectEvent_All, handler, nil)); err != nil {
		releaseRef(cameraBase)
		releaseSDK()
		initDone <- err
		return
	}
	cInfo("[init] object event handler registered")

	stateHandler := (C.EdsStateEventHandler)(C.goCameraStateEventHandler)
	if err := edsCheck("EdsSetCameraStateEventHandler",
		C.EdsSetCameraStateEventHandler(camera, C.kEdsStateEvent_All, stateHandler, nil)); err != nil {
		releaseRef(cameraBase)
		releaseSDK()
		initDone <- err
		return
	}
	cInfo("[init] state event handler registered")

	if err := openSessionWithRetry(camera); err != nil {
		releaseRef(cameraBase)
		releaseSDK()
		initDone <- err
		return
	}
	cInfo("[init] camera session opened")

	s.camera = camera
	activeService.Store(s)

	if err := configureCamera(camera); err != nil {
		cWarn("[init] camera configuration warning (non-fatal): %v", err)
	}
	cInfo("[init] camera configured; entering SDK event loop")

	// Signal NewService that init succeeded.
	initDone <- nil

	// ── Event loop ─────────────────────────────────────────────────────────
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var loopCalls uint64

	for {
		select {
		case <-s.stopEventPump:
			cInfo("[sdk] stop signal — closing session and releasing SDK")
			s.mu.Lock()
			disconnected := s.cameraDisconnected
			s.mu.Unlock()
			if !disconnected {
				if err := edsCheck("EdsCloseSession", C.EdsCloseSession(camera)); err != nil {
					cWarn("[sdk] EdsCloseSession error: %v", err)
				}
			}
			releaseRef(C.EdsBaseRef(camera))
			releaseSDK()
			return

		case task := <-s.sdkCh:
			task()

		case <-ticker.C:
			loopCalls++
			result := C.EdsGetEvent()
			if loopCalls%500 == 0 {
				cDebug("[sdk] heartbeat: %d EdsGetEvent calls", loopCalls)
			}
			if result != C.EDS_ERR_OK {
				cWarn("[sdk] EdsGetEvent returned %s (0x%08X)", edsErrName(result), uint32(result))
			}
		}
	}
}

// postSDKTask posts fn to the SDK thread, waits for it to run, and returns its error.
// Returns an error immediately if the service is being stopped.
func (s *Service) postSDKTask(fn func() error) error {
	resultCh := make(chan error, 1)
	select {
	case s.sdkCh <- func() { resultCh <- fn() }:
	case <-s.stopEventPump:
		return errors.New("service closed")
	}
	select {
	case err := <-resultCh:
		return err
	case <-s.stopEventPump:
		return errors.New("service closed")
	}
}

// ─────────────────────────────────────────────
// Capture
// ─────────────────────────────────────────────

// Capture fires a photo and waits for the camera to finish saving it to the SD card.
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
	if s.evfActive.Load() && !s.evfSuspendedByCapture.Swap(true) {
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
			done := make(chan struct{}, 1)
			select {
			case s.sdkCh <- func() { s.resetShutterState(); done <- struct{}{} }:
				<-done
			case <-s.stopEventPump:
			}
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
	cInfo("[capture] shutter command SUCCESS — waiting for DirItemCreated+download (+%s)", time.Since(captureStart))

	// ── Wait for download to complete ──────────────────────────────────────
	// onObjectEvent(DirItemCreated) downloads the file, deletes it from SD,
	// then calls notifyCaptureResult on req.
	saveTimeout := hostTransferTimeout()
	cDebug("[capture] download timeout=%s", saveTimeout)
	select {
	case result := <-req.done:
		cInfo("[capture] COMPLETE — err=%v elapsed=%s", result.err, time.Since(captureStart))
		s.maybeResumeEVF()
		return result, nil
	case <-time.After(saveTimeout):
		cError("[capture] TIMEOUT waiting for DirItemCreated after %s — camera may not have fired or SD card missing", saveTimeout)
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, fmt.Errorf("capture timed out after %s (no file received from camera)", saveTimeout)
	case <-ctx.Done():
		cWarn("[capture] context cancelled while waiting for job: %v (+%s)", ctx.Err(), time.Since(captureStart))
		s.clearPending(req)
		s.maybeResumeEVF()
		return captureResult{}, ctx.Err()
	}
}

// ─────────────────────────────────────────────
// Shutter commands
// ─────────────────────────────────────────────

// sendTakePictureWithTimeout posts the shutter command to the SDK thread and
// waits for the result with a timeout guard.
//
// THREADING: EdsSendCommand must run on the SDK thread (same thread as EdsGetEvent
// and EdsOpenSession). Posting via sdkCh guarantees this.
func (s *Service) sendTakePictureWithTimeout(timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}

	cInfo("[shutter] sendTakePictureWithTimeout timeout=%s", timeout)
	resultCh := make(chan error, 1)

	task := func() {
		resultCh <- s.sendTakePicture()
	}

	// Post task to SDK thread.
	select {
	case s.sdkCh <- task:
	case <-time.After(timeout):
		cError("[shutter] TIMEOUT posting task to SDK thread after %s", timeout)
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	case <-s.stopEventPump:
		return errors.New("service closed")
	}

	// Wait for SDK thread to complete the shutter sequence.
	select {
	case err := <-resultCh:
		if err == nil {
			cInfo("[shutter] shutter commands returned OK")
		} else {
			cError("[shutter] shutter commands returned error: %v", err)
		}
		return err
	case <-time.After(timeout):
		cError("[shutter] TIMEOUT after %s — camera not responding; spawning reset", timeout)
		s.bgWg.Add(1)
		go func() {
			SetGoroutineRole("reset")
			defer s.bgWg.Done()
			done := make(chan struct{}, 1)
			select {
			case s.sdkCh <- func() { s.resetShutterState(); done <- struct{}{} }:
				<-done
			case <-s.stopEventPump:
			}
		}()
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}
}

// sendTakePicture fires the shutter via PressShutterButton.
// Must be called from the SDK thread (via sdkCh).
//
// Strategy: ShutterButton_Completely (AF) first; NonAF fallback on failure.
func (s *Service) sendTakePicture() error {
	cInfo("[shutter] pressing ShutterButton_Completely")
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
		cInfo("[shutter] ShutterButton_Completely succeeded — camera firing")
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
		cInfo("[shutter] ShutterButton_NonAF succeeded — camera firing")
		return nil
	}

	pressErr := edsCheck("EdsSendCommand(ShutterButton_Completely)", pressCode)
	nonAFErr := edsCheck("EdsSendCommand(ShutterButton_Completely_NonAF)", nonAFPressCode)
	return fmt.Errorf("%w; NonAF also failed: %v", pressErr, nonAFErr)
}

// pressAndReleaseShutter sends press then release on the SDK thread.
// pumpWindowsMessages is called between press and release to drain any queued
// Windows messages before the next EDSDK call.
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
// Must be called from the SDK thread.
func (s *Service) resetShutterState() {
	_ = sendCameraCommandDirect(s.camera,
		C.kEdsCameraCommand_PressShutterButton,
		C.EdsInt32(C.kEdsCameraCommand_ShutterButton_OFF), 3, 120*time.Millisecond)
}

// sendCameraCommandDirect calls EdsSendCommand on the current OS thread with retries.
// Must be called from the SDK thread.
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
	if s.pending != nil {
		return nil, domain.ErrPreviewUnavailable
	}
	if s.evfActive.Load() {
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
		case <-stop:
		default:
			close(stop)
		}
		s.evfStop = nil
	}
	s.mu.Unlock()

	if done != nil {
		<-done
	}

	// evfLoop may have exited via the <-stop (suspend) path, which does NOT close
	// frameCh. Close it here so any caller ranging over the channel unblocks.
	// If evfLoop exited via ctx.Done() it already closed frameCh and set evfFrameCh
	// to nil, so the nil-check below prevents a double-close.
	s.mu.Lock()
	ch := s.evfFrameCh
	cancel := s.evfCancel
	if ch != nil {
		s.evfFrameCh = nil
		s.evfCtx = nil
		s.evfCancel = nil
		s.evfDone = nil
		s.evfSuspendedByCapture.Store(false)
	}
	s.mu.Unlock()

	if ch != nil {
		if cancel != nil {
			cancel() // release context resources
		}
		close(ch)
		cInfo("[evf] frame channel closed by StopPreview")
	}
}

// suspendEVF stops the EVF goroutine without closing the frame channel.
func (s *Service) suspendEVF() {
	s.mu.Lock()
	stop := s.evfStop
	done := s.evfDone
	if stop != nil {
		select {
		case <-stop:
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

// resumeEVF starts a new EVF goroutine reusing the existing frame channel.
func (s *Service) resumeEVF() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed || s.evfFrameCh == nil {
		return
	}
	if s.evfActive.Load() {
		return
	}

	if s.evfCtx != nil {
		select {
		case <-s.evfCtx.Done():
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

// maybeResumeEVF resumes EVF after a capture cycle if no other capture is pending.
func (s *Service) maybeResumeEVF() {
	if !s.evfSuspendedByCapture.Load() {
		return
	}
	s.mu.Lock()
	hasPending := s.pending != nil
	s.mu.Unlock()

	if hasPending {
		return
	}
	if s.evfSuspendedByCapture.CompareAndSwap(true, false) {
		s.resumeEVF()
	}
}

// evfLoop is the dedicated goroutine for EVF frame grabbing.
// All EDSDK calls are routed through postSDKTask so they execute on the SDK thread.
func (s *Service) evfLoop(ctx context.Context, frameCh chan<- []byte, stop <-chan struct{}, done chan<- struct{}) {
	SetGoroutineRole("evf")
	defer s.bgWg.Done()
	defer close(done)
	defer s.evfActive.Store(false)

	camera := s.camera

	if err := s.postSDKTask(func() error { return startEVFMode(camera) }); err != nil {
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
			if err := s.postSDKTask(func() error { stopEVFMode(camera); return nil }); err != nil {
				cWarn("[evf] stopEVFMode failed during suspend: %v", err)
			}
			cInfo("[evf] EVF suspended for capture")
			return

		case <-ctx.Done():
			_ = s.postSDKTask(func() error { stopEVFMode(camera); return nil })
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
			var frame []byte
			err := s.postSDKTask(func() error {
				var e error
				frame, e = grabEVFFrame(camera)
				return e
			})
			if err != nil {
				if isRetryableEVFErr(err) {
					continue
				}
				cWarn("[evf] frame error: %v", err)
				continue
			}
			select {
			case frameCh <- frame:
			default:
			}
		}
	}
}

// startEVFMode enables EVF (live view) output to PC on the camera.
func startEVFMode(camera C.EdsCameraRef) error {
	evfMode := C.EdsUInt32(1)
	if err := edsCheck("EdsSetPropertyData(Evf_Mode)",
		C.EdsSetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_Evf_Mode, 0,
			C.EdsUInt32(unsafe.Sizeof(evfMode)),
			unsafe.Pointer(&evfMode),
		)); err != nil {
		// Non-fatal: some cameras have EVF always enabled and reject this property.
		cWarn("[evf] EdsSetPropertyData(Evf_Mode=1) failed (non-fatal): %v", err)
	}

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
	if err := edsCheck("EdsGetPropertyData(Evf_OutputDevice)",
		C.EdsGetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_Evf_OutputDevice, 0,
			C.EdsUInt32(unsafe.Sizeof(device)),
			unsafe.Pointer(&device),
		)); err != nil {
		cWarn("[evf] stopEVFMode: EdsGetPropertyData(Evf_OutputDevice) failed: %v — EVF may remain active on camera", err)
		return
	}
	device &^= C.kEdsEvfOutputDevice_PC
	if err := edsCheck("EdsSetPropertyData(Evf_OutputDevice)",
		C.EdsSetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_Evf_OutputDevice, 0,
			C.EdsUInt32(unsafe.Sizeof(device)),
			unsafe.Pointer(&device),
		)); err != nil {
		cWarn("[evf] stopEVFMode: EdsSetPropertyData(Evf_OutputDevice) failed: %v — EVF may remain active on camera", err)
	}
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

	frame := make([]byte, int(length))
	copy(frame, (*[1 << 28]byte)(ptr)[:length:length])
	return frame, nil
}

func isRetryableEVFErr(err error) bool {
	if err == nil {
		return false
	}
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
// CGo callback entry points
// ─────────────────────────────────────────────

// goObjectEventHandler is the C-callable entry point registered with EdsSetObjectEventHandler.
// It is called by EDSDK on the SDK thread (via EdsGetEvent) and dispatches to the active service.
//
//export goObjectEventHandler
func goObjectEventHandler(inEvent C.EdsObjectEvent, inRef C.EdsBaseRef, _ unsafe.Pointer) C.EdsError {
	svc := activeService.Load()
	if svc == nil {
		cWarn("[event] goObjectEventHandler: activeService is nil, dropping event 0x%08X", uint32(inEvent))
		if inRef != nil {
			_ = C.EdsRelease(inRef)
		}
		return C.EDS_ERR_OK
	}
	return svc.onObjectEvent(inEvent, inRef)
}

// goCameraStateEventHandler is the C-callable entry point registered with EdsSetCameraStateEventHandler.
// It is called by EDSDK on the SDK thread (via EdsGetEvent) and dispatches to the active service.
//
//export goCameraStateEventHandler
func goCameraStateEventHandler(inEvent C.EdsStateEvent, inParameter C.EdsUInt32, _ unsafe.Pointer) C.EdsError {
	svc := activeService.Load()
	if svc == nil {
		cWarn("[event] goCameraStateEventHandler: activeService is nil, dropping event 0x%08X", uint32(inEvent))
		return C.EDS_ERR_OK
	}
	return svc.onStateEvent(inEvent, inParameter)
}

// ─────────────────────────────────────────────
// Event handlers
// ─────────────────────────────────────────────

// onObjectEvent handles camera object events (called on the SDK thread via EdsGetEvent).
func (s *Service) onObjectEvent(event C.EdsObjectEvent, ref C.EdsBaseRef) C.EdsError {
	cDebug("[event] onObjectEvent: event=%s (0x%08X) ref=%v",
		edsObjectEventName(event), uint32(event), ref != nil)

	if ref == nil {
		cWarn("[event] ref is nil, ignoring event")
		return C.EDS_ERR_OK
	}

	if event == C.kEdsObjectEvent_DirItemCreated {
		// SaveTo=Camera: camera wrote the photo to the SD card.
		// Download it to the host captures folder, then delete it from the SD card.
		dirItem := C.EdsDirectoryItemRef(ref)

		suppressed := s.isTransferSuppressed()
		cInfo("[event] DirItemCreated — isTransferSuppressed=%v", suppressed)
		if suppressed {
			cWarn("[event] suppressing stale DirItemCreated (preempt window active)")
			releaseRef(ref)
			return C.EDS_ERR_OK
		}

		path, err := s.downloadAndDeleteDirectoryItem(dirItem)
		cDebug("[event] downloadAndDelete finished: path=%q err=%v", path, err)

		s.mu.Lock()
		req := s.pending
		if req != nil {
			s.pending = nil
		}
		s.mu.Unlock()

		if req != nil {
			cDebug("[event] notifying pending capture with download result")
			notifyCaptureResult(req, captureResult{path: path, err: err})
		} else if err == nil {
			cWarn("[event] download complete but no pending capture to notify (path=%s) — capture() may have timed out", path)
		}
		return C.EDS_ERR_OK
	}

	if event == C.kEdsObjectEvent_DirItemRequestTransfer ||
		event == C.kEdsObjectEvent_DirItemRequestTransferDT {
		// Should not fire with SaveTo=Camera. Cancel so the camera doesn't stall.
		cWarn("[event] unexpected DirItemRequestTransfer with SaveTo=Camera — cancelling")
		_ = C.EdsDownloadCancel(C.EdsDirectoryItemRef(ref))
		releaseRef(ref)
		return C.EDS_ERR_OK
	}

	cDebug("[event] unhandled object event — releasing ref")
	releaseRef(ref)
	return C.EDS_ERR_OK
}

// downloadAndDeleteDirectoryItem downloads the file at dirItem to the host captures
// folder, then deletes it from the camera's SD card. Owns and releases dirItem.
// Must be called from the SDK thread.
func (s *Service) downloadAndDeleteDirectoryItem(dirItem C.EdsDirectoryItemRef) (string, error) {
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
		cError("[download] EdsDownload failed for %q: %v — cancelling transfer and removing partial file", destPath, err)
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		if removeErr := os.Remove(destPath); removeErr != nil && !os.IsNotExist(removeErr) {
			cWarn("[download] failed to remove partial file %q: %v", destPath, removeErr)
		}
		return "", fmt.Errorf("download %q: %w", fileName, err)
	}
	cDebug("[download] EdsDownload OK")

	cDebug("[download] calling EdsDownloadComplete")
	if err := edsCheck("EdsDownloadComplete",
		C.EdsDownloadComplete(dirItem)); err != nil {
		cError("[download] EdsDownloadComplete failed for %q: %v — cancelling transfer and removing partial file", destPath, err)
		_ = C.EdsDownloadCancel(dirItem)
		releaseRef(C.EdsBaseRef(dirItem))
		if removeErr := os.Remove(destPath); removeErr != nil && !os.IsNotExist(removeErr) {
			cWarn("[download] failed to remove partial file %q: %v", destPath, removeErr)
		}
		return "", fmt.Errorf("download complete %q: %w", fileName, err)
	}
	cInfo("[download] EdsDownloadComplete OK — file saved to %s", destPath)

	cDebug("[download] deleting file from SD card")
	if err := edsCheck("EdsDeleteDirectoryItem",
		C.EdsDeleteDirectoryItem(dirItem)); err != nil {
		cWarn("[download] EdsDeleteDirectoryItem failed (non-fatal): %v", err)
	} else {
		cInfo("[download] file deleted from SD card")
	}

	releaseRef(C.EdsBaseRef(dirItem))
	return destPath, nil
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

// onStateEvent handles camera state changes (called on the SDK thread via EdsGetEvent).
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

// configureCamera sets SaveTo=Camera (photo written to SD card) and optionally
// disables flash. With SaveTo=Camera, no EdsSetCapacity is needed and no
// DirItemRequestTransfer event fires — completion is signalled by JobStatusChanged.
func configureCamera(camera C.EdsCameraRef) error {
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

	cInfo("[config] setting SaveTo=Camera (kEdsSaveTo_Camera=0x%08X)", uint32(C.kEdsSaveTo_Camera))
	saveTo := C.EdsUInt32(C.kEdsSaveTo_Camera)
	if err := edsCheck("EdsSetPropertyData(SaveTo)", C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveTo)),
		unsafe.Pointer(&saveTo),
	)); err != nil {
		cWarn("[config] SaveTo=Camera not applied: %v", err)
	} else {
		var saveToAfterSet C.EdsUInt32
		readBackErr := C.EdsGetPropertyData(
			C.EdsBaseRef(camera),
			C.kEdsPropID_SaveTo,
			0,
			C.EdsUInt32(unsafe.Sizeof(saveToAfterSet)),
			unsafe.Pointer(&saveToAfterSet),
		)
		if readBackErr == C.EDS_ERR_OK {
			cInfo("[config] SaveTo AFTER set: %s (0x%08X) — expected Camera (0x%08X)",
				saveToName(saveToAfterSet), uint32(saveToAfterSet), uint32(C.kEdsSaveTo_Camera))
			if saveToAfterSet != C.EdsUInt32(C.kEdsSaveTo_Camera) {
				cWarn("[config] SaveTo readback mismatch! Camera may not save to SD card")
			}
		} else {
			cWarn("[config] SaveTo readback failed: %s (0x%08X)", edsErrName(readBackErr), uint32(readBackErr))
		}
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

// configureNoFlash applies FlashOn=0 if the camera supports it.
func configureNoFlash(camera C.EdsCameraRef) error {
	return setOptionalUInt32Property(camera, "FlashOn=0", C.kEdsPropID_FlashOn, C.EdsUInt32(0))
}

// ─────────────────────────────────────────────
// Service lifecycle
// ─────────────────────────────────────────────

// Close stops the SDK thread (which closes the session and releases the SDK) and
// waits for all background goroutines to finish.
func (s *Service) Close() error {
	s.closeOnce.Do(func() {
		cInfo("[close] Service.Close called")
		activeService.CompareAndSwap(s, nil)

		// Stop EVF so the camera gets the stop command before the session closes.
		s.StopPreview()

		// Signal the SDK thread to close the session and release the SDK.
		close(s.stopEventPump)
		<-s.eventPumpDone

		s.mu.Lock()
		s.closed = true
		pending := s.pending
		s.pending = nil
		s.mu.Unlock()

		if pending != nil {
			notifyCaptureResult(pending, captureResult{err: errors.New("capture canceled: service closed")})
		}

		s.bgWg.Wait()
		cInfo("[close] Service.Close complete")
	})
	return s.closeErr
}

// Disconnected returns a channel that is closed when the camera fires a shutdown event.
func (s *Service) Disconnected() <-chan struct{} {
	return s.disconnectedCh
}

// ─────────────────────────────────────────────
// Camera discovery
// ─────────────────────────────────────────────

// waitForFirstCamera polls the camera list until a camera appears or timeout elapses.
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

// firstCameraRefAndCount returns the first camera from the list and the total count.
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

// openSessionWithRetry opens the EDSDK session with retries on DEVICE_BUSY / OBJECT_NOTREADY.
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
	case uint32(C.EDS_ERR_OBJECT_NOTREADY):
		return "EDS_ERR_OBJECT_NOTREADY"
	case uint32(C.EDS_ERR_PROPERTIES_NOT_LOADED):
		return "EDS_ERR_PROPERTIES_NOT_LOADED"
	case uint32(C.EDS_ERR_NOT_SUPPORTED):
		return "EDS_ERR_NOT_SUPPORTED"
	case uint32(C.EDS_ERR_PROPERTIES_UNAVAILABLE):
		return "EDS_ERR_PROPERTIES_UNAVAILABLE"
	case uint32(C.EDS_ERR_PROPERTIES_MISMATCH):
		return "EDS_ERR_PROPERTIES_MISMATCH"
	case uint32(C.EDS_ERR_INVALID_PARAMETER):
		return "EDS_ERR_INVALID_PARAMETER"
	case uint32(C.EDS_ERR_DEVICE_INVALID_PARAMETER):
		return "EDS_ERR_DEVICE_INVALID_PARAMETER"
	case uint32(C.EDS_ERR_DEVICEPROP_NOT_SUPPORTED):
		return "EDS_ERR_DEVICEPROP_NOT_SUPPORTED"
	case uint32(C.EDS_ERR_INVALID_DEVICEPROP_FORMAT):
		return "EDS_ERR_INVALID_DEVICEPROP_FORMAT"
	case uint32(C.EDS_ERR_INVALID_DEVICEPROP_VALUE):
		return "EDS_ERR_INVALID_DEVICEPROP_VALUE"
	case uint32(C.EDS_ERR_PTP_DEVICE_BUSY):
		return "EDS_ERR_PTP_DEVICE_BUSY"
	case uint32(C.EDS_ERR_INTERNAL_ERROR):
		return "EDS_ERR_INTERNAL_ERROR"
	default:
		return fmt.Sprintf("EDS_ERR_0x%08X", uint32(code))
	}
}

func edsErrHint(code C.EdsError) string {
	switch uint32(code) {
	case uint32(C.EDS_ERR_COMM_PORT_IS_IN_USE):
		return "close EOS Utility or other Canon apps"
	case uint32(C.EDS_ERR_DEVICE_BUSY):
		return "camera is busy; retry shortly"
	case uint32(C.EDS_ERR_COMM_DISCONNECTED):
		return "USB disconnected"
	default:
		return ""
	}
}

// releaseRef wraps EdsRelease for any EdsBaseRef.
func releaseRef(ref C.EdsBaseRef) {
	if ref != nil {
		_ = C.EdsRelease(ref)
	}
}
