package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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

	req := c.buildRequest(r)
	timeout := c.effectiveTimeout(req)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	resp, err := c.useCase.Execute(ctx, req)
	if err != nil {
		writeJSON(w, domainErrToHTTPStatus(err), CapturePhotoViewModel{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, CapturePhotoViewModel{Success: resp.Success, Path: resp.Path})
}

// buildRequest maps HTTP query to CapturePhotoRequest (presentation -> application DTO).
func (c *CaptureController) buildRequest(r *http.Request) capture.CapturePhotoRequest {
	req := capture.CapturePhotoRequest{}
	if raw := r.URL.Query().Get("timeout_ms"); raw != "" {
		if ms, err := strconv.Atoi(raw); err == nil && ms >= 250 && ms <= 300000 {
			req.TimeoutMs = ms
		}
	}
	return req
}

func (c *CaptureController) effectiveTimeout(req capture.CapturePhotoRequest) time.Duration {
	if req.TimeoutMs > 0 {
		return time.Duration(req.TimeoutMs) * time.Millisecond
	}
	return c.defaultCaptureTimeout
}

// CapturePhotoViewModel is the JSON view model for capture responses (presentation layer).
type CapturePhotoViewModel struct {
	Success bool   `json:"success,omitempty"`
	Path    string `json:"path,omitempty"`
	Error   string `json:"error,omitempty"`
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
