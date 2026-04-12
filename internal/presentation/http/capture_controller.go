package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"photoboot-picstop/internal/application/capture"
	"photoboot-picstop/internal/domain"
)

// CaptureController handles HTTP for photo capture. No business logic; only maps HTTP to use case and response.
type CaptureController struct {
	useCase               *capture.CapturePhotoUseCase
	defaultCaptureTimeout time.Duration
}

// NewCaptureController builds the controller with injected use case and default timeout.
func NewCaptureController(useCase *capture.CapturePhotoUseCase, defaultCaptureTimeout time.Duration) *CaptureController {
	return &CaptureController{
		useCase:               useCase,
		defaultCaptureTimeout: defaultCaptureTimeout,
	}
}

// ServeHTTP handles POST /capture: parses request into DTO, calls use case, maps response/errors to HTTP (ViewModel).
func (c *CaptureController) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, CapturePhotoViewModel{Error: "use POST /capture"})
		return
	}

	timeout, ok := c.parseTimeout(r)
	if !ok {
		writeJSON(w, http.StatusBadRequest, CapturePhotoViewModel{Error: "timeout_ms must be between 250 and 300000"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	resp, err := c.useCase.Execute(ctx)
	if err != nil {
		writeJSON(w, domainErrToHTTPStatus(err), CapturePhotoViewModel{Error: err.Error()})
		return
	}

	vm := CapturePhotoViewModel{
		Success:     resp.Success,
		OriginalURL: "/captures/" + filepath.Base(resp.OriginalPath),
	}
	if resp.FilteredPath != "" {
		vm.FilteredURL = "/captures/" + filepath.Base(resp.FilteredPath)
	}
	writeJSON(w, http.StatusOK, vm)
}

// parseTimeout parses the optional timeout_ms query parameter.
// Returns (timeout, true) on success, or (0, false) if the parameter is present but invalid.
func (c *CaptureController) parseTimeout(r *http.Request) (time.Duration, bool) {
	raw := r.URL.Query().Get("timeout_ms")
	if raw == "" {
		return c.defaultCaptureTimeout, true
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 250 || ms > 300000 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// CapturePhotoViewModel is the JSON view model for capture responses (presentation layer).
type CapturePhotoViewModel struct {
	Success     bool   `json:"success,omitempty"`
	OriginalURL string `json:"original_url,omitempty"` // web-accessible URL for the original photo
	FilteredURL string `json:"filtered_url,omitempty"` // web-accessible URL for the filtered photo (if any)
	Error       string `json:"error,omitempty"`
}

// domainErrToHTTPStatus maps domain/context errors to HTTP status codes (presentation concern).
func domainErrToHTTPStatus(err error) int {
	switch {
	case errors.Is(err, domain.ErrCaptureInProgress), errors.Is(err, domain.ErrCaptureSuperseded):
		return http.StatusConflict
	case errors.Is(err, domain.ErrShutterCommandTimeout):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.Is(err, context.Canceled):
		return http.StatusRequestTimeout
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
