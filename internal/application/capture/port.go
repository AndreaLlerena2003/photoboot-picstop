package capture

import "context"

// ICameraCapturePort is the output port (repository-like abstraction) for triggering
// a photo capture. Implemented by the infrastructure layer (e.g. Canon EDSDK).
// Application layer depends on this interface; infrastructure implements it (DIP).
type ICameraCapturePort interface {
	// Capture triggers a photo and waits until the camera finishes (e.g. saved to SD).
	// Returns the file path or a domain error (e.g. ErrCaptureInProgress, ErrShutterCommandTimeout).
	Capture(ctx context.Context) (path string, err error)
}
