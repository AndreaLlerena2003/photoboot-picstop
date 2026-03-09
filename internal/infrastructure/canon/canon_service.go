//go:build windows && cgo

package canon

/*
#cgo windows CFLAGS: -I${SRCDIR}/../../../edsdk
#cgo windows LDFLAGS: -L${SRCDIR}/../../../edsdk -l:EDSDK.lib -lole32
#include <windows.h>
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

static HRESULT codexCoInitializeSTA(void) {
    return CoInitializeEx(NULL, COINIT_APARTMENTTHREADED);
}

static void codexCoUninitialize(void) {
    CoUninitialize();
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
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"photoboot-picstop/internal/domain"
)

// activeService guarda el Service actual para que los callbacks C puedan despachar eventos.
var activeService atomic.Pointer[Service]

// Service implementa el puerto de cámara (CameraPort) usando Canon EDSDK en Windows.
type Service struct {
	mu        sync.Mutex
	closeOnce sync.Once
	closeErr  error

	camera C.EdsCameraRef

	outputDir              string
	pending                *captureRequest
	closed                 bool
	jobBusy                atomic.Uint32
	suppressTransfersUntil time.Time

	stopEventPump chan struct{}
	eventPumpDone chan struct{}
	jobStateCh    chan struct{}
}

type captureRequest struct {
	done chan captureResult
}

type captureResult struct {
	path string
	err  error
}

// NewService inicializa el SDK, descubre la primera cámara, abre sesión, registra handlers y configura SaveTo=Camera.
func NewService(outputDir string) (_ *Service, retErr error) {
	absDir, err := filepath.Abs(outputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve capture dir: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("create capture dir: %w", err)
	}

	uninitCOM, err := initCOMForCurrentThread()
	if err != nil {
		return nil, err
	}
	defer uninitCOM()

	log.Printf("canon: calling EdsInitializeSDK")
	if err := edsCheck("EdsInitializeSDK", C.EdsInitializeSDK()); err != nil {
		return nil, err
	}
	sdkInitialized := true
	defer func() {
		if retErr != nil && sdkInitialized {
			_ = C.EdsTerminateSDK()
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
		camera:        camera,
		outputDir:     absDir,
		stopEventPump: make(chan struct{}),
		eventPumpDone: make(chan struct{}),
		jobStateCh:    make(chan struct{}, 1),
	}
	activeService.Store(svc)
	defer func() {
		if retErr != nil {
			activeService.CompareAndSwap(svc, nil)
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

	log.Printf("canon: configuring SD capture defaults")
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

// Capture dispara una foto, espera a que la cámara guarde en SD y devuelve la ruta o error (dominio).
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

	// ── Preempt any in-flight capture ──────────────────────────────────────
	var preempted *captureRequest
	var preemptWindow time.Duration

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
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
		go func() {
			_ = withCOMForEDSDK(func() error {
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
		return captureResult{}, context.DeadlineExceeded
	}

	// ── Fire shutter ────────────────────────────────────────────────────────
	if err := sendTakePictureWithTimeout(camera, commandTimeout); err != nil {
		s.clearPending(req)
		return captureResult{}, err
	}

	// ── FIX: with SaveTo=Camera, DirItemRequestTransfer never fires.
	// We wait for JobStatusChanged → jobBusy==0, which means the camera
	// finished writing to SD. Then we notify the pending request ourselves.
	// ────────────────────────────────────────────────────────────────────────
	saveTimeout := cameraSaveTimeout()
	if !s.waitForTransferJobsIdle(saveTimeout) {
		log.Printf("canon: warning: camera job did not complete in %s, still signaling success", saveTimeout)
	}

	// Notify success — the photo is on the SD card.
	s.mu.Lock()
	if s.pending == req {
		s.pending = nil
		notifyCaptureResult(req, captureResult{path: "saved_to_sd_card", err: nil})
	}
	s.mu.Unlock()

	select {
	case result := <-req.done:
		return result, nil
	case <-ctx.Done():
		s.clearPending(req)
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
func sendTakePictureWithTimeout(camera C.EdsCameraRef, timeout time.Duration) error {
	if timeout <= 0 {
		return fmt.Errorf("%w after %s", domain.ErrShutterCommandTimeout, timeout)
	}

	resultCh := make(chan error, 1)
	go func() {
		resultCh <- withCOMForEDSDK(func() error {
			return sendTakePicture(camera)
		})
	}()

	select {
	case err := <-resultCh:
		return err
	case <-time.After(timeout):
		// Best-effort reset so subsequent captures can proceed.
		go func() {
			_ = withCOMForEDSDK(func() error {
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
// Event handlers
// ─────────────────────────────────────────────

// onObjectEvent handles camera object events.
// With SaveTo=Camera, DirItemRequestTransfer is NOT expected — the camera
// writes directly to SD. We cancel any accidental transfer requests and
// release the ref. The capture completion is signaled via onStateEvent
// (JobStatusChanged → jobBusy==0).
func (s *Service) onObjectEvent(event C.EdsObjectEvent, ref C.EdsBaseRef) C.EdsError {
	if ref != nil {
		if event == C.kEdsObjectEvent_DirItemRequestTransfer ||
			event == C.kEdsObjectEvent_DirItemRequestTransferDT {

			if s.isTransferSuppressed() {
				log.Printf("canon: suppressing stale transfer request")
			} else {
				log.Printf("canon: unexpected transfer request with SaveTo=Camera; cancelling")
			}
			_ = C.EdsDownloadCancel(C.EdsDirectoryItemRef(ref))
		}
	}
	releaseRef(ref)
	return C.EDS_ERR_OK
}

// onStateEvent handles camera state changes.
// JobStatusChanged with inParameter==0 means the camera finished its job
// (i.e. finished writing to SD card). We use this to unblock capture().
func (s *Service) onStateEvent(event C.EdsStateEvent, inParameter C.EdsUInt32) C.EdsError {
	if event != C.kEdsStateEvent_JobStatusChanged {
		return C.EDS_ERR_OK
	}

	if inParameter == 0 {
		s.jobBusy.Store(0)
		log.Printf("canon: camera job finished (SD write complete)")
	} else {
		s.jobBusy.Store(1)
		log.Printf("canon: camera job started")
	}

	select {
	case s.jobStateCh <- struct{}{}:
	default:
	}
	return C.EDS_ERR_OK
}

// ─────────────────────────────────────────────
// Camera configuration
// ─────────────────────────────────────────────

// configureCamera pone SaveTo=Camera (SD) y opcionalmente no-flash según env.
func configureCamera(camera C.EdsCameraRef) error {
	// Always save to SD card — no host transfer needed.
	log.Printf("canon: setting SaveTo=Camera (SD card)")
	saveTo := C.EdsUInt32(C.kEdsSaveTo_Camera)
	if err := edsCheck("EdsSetPropertyData(SaveTo)", C.EdsSetPropertyData(
		C.EdsBaseRef(camera),
		C.kEdsPropID_SaveTo,
		0,
		C.EdsUInt32(unsafe.Sizeof(saveTo)),
		unsafe.Pointer(&saveTo),
	)); err != nil {
		log.Printf("canon: warning: SaveTo=Camera not applied: %v", err)
	} else {
		log.Printf("canon: SaveTo=Camera configured")
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

		var errs []error
		if err := withCOMForEDSDK(func() error {
			closeErr := edsCheck("EdsCloseSession", C.EdsCloseSession(camera))
			releaseRef(C.EdsBaseRef(camera))
			terminateErr := edsCheck("EdsTerminateSDK", C.EdsTerminateSDK())
			return errors.Join(closeErr, terminateErr)
		}); err != nil {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			s.closeErr = errors.Join(errs...)
		}
	})
	return s.closeErr
}

// startEventPump ejecuta EdsGetEvent en un ticker para que los callbacks EDSDK se disparen.
func (s *Service) startEventPump() {
	go func() {
		defer close(s.eventPumpDone)

		uninitCOM, err := initCOMForCurrentThread()
		if err != nil {
			log.Printf("canon: event pump COM initialization failed: %v", err)
			return
		}
		defer uninitCOM()

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

func (s *Service) takePending() *captureRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	req := s.pending
	s.pending = nil
	return req
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

// ─────────────────────────────────────────────
// COM / OS thread helpers
// ─────────────────────────────────────────────

// withCOMForEDSDK ejecuta fn en un hilo con COM inicializado (requerido por EDSDK).
func withCOMForEDSDK(fn func() error) error {
	uninitCOM, err := initCOMForCurrentThread()
	if err != nil {
		return err
	}
	defer uninitCOM()
	return fn()
}

// initCOMForCurrentThread llama a CoInitializeEx(COINIT_APARTMENTTHREADED) en el hilo actual; devuelve un uninit.
func initCOMForCurrentThread() (func(), error) {
	runtime.LockOSThread()
	hr := uint32(C.codexCoInitializeSTA())
	switch hr {
	case 0x00000000, 0x00000001: // S_OK / S_FALSE
		return func() {
			C.codexCoUninitialize()
			runtime.UnlockOSThread()
		}, nil
	case 0x80010106: // RPC_E_CHANGED_MODE
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CoInitializeEx(COINIT_APARTMENTTHREADED) failed: RPC_E_CHANGED_MODE (0x%08X)", hr)
	default:
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CoInitializeEx(COINIT_APARTMENTTHREADED) failed: 0x%08X", hr)
	}
}

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
