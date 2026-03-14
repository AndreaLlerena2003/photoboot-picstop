package capture

import "context"

// ICameraCapturePort is the output port (repository-like abstraction) for triggering
// a photo capture. Implemented by the infrastructure layer (e.g. Canon EDSDK).
// Application layer depends on this interface; infrastructure implements it (DIP).
type ICameraCapturePort interface {
	// Capture triggers a photo and waits until the file is downloaded to the host.
	// Returns the local file path or a domain error (e.g. ErrCaptureInProgress, ErrShutterCommandTimeout).
	Capture(ctx context.Context) (path string, err error)
}

// IPreviewPort is the output port for EVF (Electronic Viewfinder) live preview.
// Implemented by the infrastructure layer. Application depends on this interface (DIP).
type IPreviewPort interface {
	// StartPreview begins EVF streaming and returns a channel of JPEG frames.
	// Frames are pushed until ctx is cancelled or StopPreview is called, at which
	// point the channel is closed. Returns ErrPreviewUnavailable if EVF cannot start.
	StartPreview(ctx context.Context) (<-chan []byte, error)

	// StopPreview stops EVF mode and closes the frame channel. Idempotent.
	StopPreview()
}
