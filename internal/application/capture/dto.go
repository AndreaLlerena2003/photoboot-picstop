package capture

// CapturePhotoResponse is the output DTO for the CapturePhoto use case.
type CapturePhotoResponse struct {
	OriginalPath string
	FilteredPath string // empty if no filter was active or filter failed
	Success      bool
}
