package domain

import "errors"

var (
	// ErrCaptureInProgress indica que ya hay una captura en curso.
	ErrCaptureInProgress = errors.New("there is already a capture in progress")
	// ErrCaptureSuperseded indica que la captura fue reemplazada por una solicitud más reciente.
	ErrCaptureSuperseded = errors.New("capture superseded by a newer request")
	// ErrShutterCommandTimeout indica que el comando de disparo superó el tiempo límite.
	ErrShutterCommandTimeout = errors.New("shutter command timeout")
	// ErrUnsupported indica que la cámara Canon EDSDK no está disponible (p. ej. no Windows o cgo deshabilitado).
	ErrUnsupported = errors.New("Canon EDSDK requires Windows with cgo enabled")
	// ErrPreviewUnavailable indica que el live preview no está disponible (cámara ocupada o no soportado).
	ErrPreviewUnavailable = errors.New("live preview unavailable")
	// ErrCameraDisconnected indica que la cámara fue desconectada físicamente durante la operación.
	ErrCameraDisconnected = errors.New("camera was disconnected")
)
