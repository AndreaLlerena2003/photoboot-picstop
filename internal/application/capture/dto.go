package capture

// CapturePhotoRequest is the input DTO for the CapturePhoto use case.
// TimeoutMs is optional (0 = use default); validated by the application or presentation layer.
type CapturePhotoRequest struct {
	TimeoutMs int
}

// CapturePhotoResponse is the output DTO for the CapturePhoto use case.
type CapturePhotoResponse struct {
	Path    string
	Success bool
}
