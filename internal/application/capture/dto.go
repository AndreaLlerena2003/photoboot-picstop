package capture

// CapturePhotoResponse is the output DTO for the CapturePhoto use case.
type CapturePhotoResponse struct {
	Path    string
	Success bool
}
