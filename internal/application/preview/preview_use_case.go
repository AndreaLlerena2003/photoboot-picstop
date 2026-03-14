package preview

import (
	"context"

	"photoboot-picstop/internal/application/capture"
)

// StreamPreviewUseCase streams EVF (live view) JPEG frames from the camera.
type StreamPreviewUseCase struct {
	port capture.IPreviewPort
}

// NewStreamPreviewUseCase creates a use case backed by the given preview port.
func NewStreamPreviewUseCase(port capture.IPreviewPort) *StreamPreviewUseCase {
	return &StreamPreviewUseCase{port: port}
}

// Execute starts EVF streaming and returns a channel of JPEG frames.
// The channel is closed when ctx is cancelled or StopPreview is called.
func (u *StreamPreviewUseCase) Execute(ctx context.Context) (<-chan []byte, error) {
	return u.port.StartPreview(ctx)
}
