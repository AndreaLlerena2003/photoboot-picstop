package domain

// CaptureResult is a value object representing the outcome of a photo capture:
// the file path (if saved) and any error. No framework or infrastructure dependencies.
type CaptureResult struct {
	Path string
	Err  error
}
