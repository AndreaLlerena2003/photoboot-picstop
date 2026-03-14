package http

import (
	"context"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"net/textproto"

	"photoboot-picstop/internal/application/preview"
)

const mjpegBoundary = "mjpegframe"

// PreviewController handles GET /preview: streams MJPEG live view to the client.
type PreviewController struct {
	useCase *preview.StreamPreviewUseCase
}

// NewPreviewController creates a controller backed by the given preview use case.
func NewPreviewController(uc *preview.StreamPreviewUseCase) *PreviewController {
	return &PreviewController{useCase: uc}
}

// ServeHTTP streams MJPEG frames until the client disconnects or preview stops.
// Compatible with browser <img src="/preview"> tags (multipart/x-mixed-replace).
func (c *PreviewController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed; use GET /preview"}`, http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	frames, err := c.useCase.Execute(ctx)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "multipart/x-mixed-replace;boundary="+mjpegBoundary)
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "close")

	mw := multipart.NewWriter(w)
	if err := mw.SetBoundary(mjpegBoundary); err != nil {
		log.Printf("preview: failed to set multipart boundary: %v", err)
		return
	}

	flusher, canFlush := w.(http.Flusher)

	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-frames:
			if !ok {
				// Channel closed: preview stopped (client disconnect or StopPreview).
				return
			}
			hdr := make(textproto.MIMEHeader)
			hdr.Set("Content-Type", "image/jpeg")
			hdr.Set("Content-Length", fmt.Sprintf("%d", len(frame)))

			pw, err := mw.CreatePart(hdr)
			if err != nil {
				log.Printf("preview: multipart part error: %v", err)
				return
			}
			if _, err := pw.Write(frame); err != nil {
				log.Printf("preview: frame write error: %v", err)
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
	}
}
