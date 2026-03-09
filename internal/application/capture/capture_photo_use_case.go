package capture

import "context"

// CapturePhotoUseCase is the application use case: capture a single photo.
// It depends only on ICameraCapturePort (injected); no infrastructure or presentation details.
type CapturePhotoUseCase struct {
	camera ICameraCapturePort
}

// NewCapturePhotoUseCase constructs the use case with the given port (dependency injection).
func NewCapturePhotoUseCase(camera ICameraCapturePort) *CapturePhotoUseCase {
	return &CapturePhotoUseCase{camera: camera}
}

// Execute runs the use case: delegates to the camera port and maps the result to a response DTO.
// Business rule: one capture at a time; preempt and job waiting are handled by the port implementation.
func (u *CapturePhotoUseCase) Execute(ctx context.Context, req CapturePhotoRequest) (CapturePhotoResponse, error) {
	path, err := u.camera.Capture(ctx)
	if err != nil {
		return CapturePhotoResponse{Success: false}, err
	}
	return CapturePhotoResponse{Path: path, Success: true}, nil
}
