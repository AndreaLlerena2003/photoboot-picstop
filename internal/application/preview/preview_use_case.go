package preview

import (
	"context"

	"photoboot-picstop/internal/application/capture"
	"photoboot-picstop/internal/application/filter"
)

// StreamPreviewUseCase streams EVF (live view) JPEG frames from the camera.
type StreamPreviewUseCase struct {
	port   capture.IPreviewPort
	filter filter.IFilterPort // nil means passthrough
}

// NewStreamPreviewUseCase creates a use case backed by the given preview port.
// f may be nil (disables filtering on preview frames).
func NewStreamPreviewUseCase(port capture.IPreviewPort, f filter.IFilterPort) *StreamPreviewUseCase {
	return &StreamPreviewUseCase{port: port, filter: f}
}

// Execute starts EVF streaming and returns a channel of JPEG frames.
// If a filter port is configured, frames are passed through the active filter
// (read atomically per frame — filter changes take effect on the next frame).
// The channel is closed when ctx is cancelled or StopPreview is called.
func (u *StreamPreviewUseCase) Execute(ctx context.Context) (<-chan []byte, error) {
	rawCh, err := u.port.StartPreview(ctx)
	if err != nil {
		return nil, err
	}
	if u.filter == nil {
		return rawCh, nil
	}
	// Pass "" so the engine reads ActiveFilter() per frame, enabling live filter switching.
	return u.filter.WrapPreviewChannel(ctx, rawCh, ""), nil
}
