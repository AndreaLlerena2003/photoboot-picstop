package capture

import (
	"context"
	"log"

	"photoboot-picstop/internal/application/filter"
)

// CapturePhotoUseCase is the application use case: capture a single photo.
// It depends only on ICameraCapturePort and (optionally) filter.IFilterPort; no infrastructure details.
type CapturePhotoUseCase struct {
	camera ICameraCapturePort
	filter filter.IFilterPort // nil means passthrough
}

// NewCapturePhotoUseCase constructs the use case. filter may be nil (disables filtering).
func NewCapturePhotoUseCase(camera ICameraCapturePort, f filter.IFilterPort) *CapturePhotoUseCase {
	return &CapturePhotoUseCase{camera: camera, filter: f}
}

// Execute runs the use case: triggers the camera, then optionally applies the active filter.
// Filter failure is non-fatal — the original path is always returned.
func (u *CapturePhotoUseCase) Execute(ctx context.Context) (CapturePhotoResponse, error) {
	path, err := u.camera.Capture(ctx)
	if err != nil {
		return CapturePhotoResponse{Success: false}, err
	}

	resp := CapturePhotoResponse{OriginalPath: path, Success: true}

	if u.filter != nil {
		if name := u.filter.ActiveFilter(); name != "" {
			filteredPath, ferr := u.filter.ApplyToFile(ctx, path, name)
			if ferr != nil {
				log.Printf("capture: filter apply error (non-fatal): %v", ferr)
			} else {
				resp.FilteredPath = filteredPath
			}
		}
	}

	return resp, nil
}
