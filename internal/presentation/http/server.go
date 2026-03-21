package http

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"photoboot-picstop/internal/application/capture"
	"photoboot-picstop/internal/application/preview"
)

// Server is the HTTP presentation server: routes and controllers only.
type Server struct {
	srv *http.Server
}

// NewServer builds the server with capture and preview routes (dependencies injected).
func NewServer(
	addr string,
	captureUseCase *capture.CapturePhotoUseCase,
	previewUseCase *preview.StreamPreviewUseCase,
	defaultCaptureTimeout time.Duration,
	captureDir string,
) *Server {
	captureCtrl := NewCaptureController(captureUseCase, defaultCaptureTimeout)
	previewCtrl := NewPreviewController(previewUseCase)

	mux := http.NewServeMux()
	mux.HandleFunc("/capture", captureCtrl.ServeHTTP)
	mux.HandleFunc("/preview", previewCtrl.ServeHTTP)

	// Serve captured photos so the browser can load them for strip generation.
	mux.Handle("/captures/", http.StripPrefix("/captures/", http.FileServer(http.Dir(captureDir))))

	// Serve the web UI at the root.
	mux.Handle("/", http.FileServer(http.Dir("web")))

	return &Server{
		srv: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// Start runs the server and blocks until shutdown signal (SIGINT/SIGTERM). Graceful shutdown with 5s timeout.
func (s *Server) Start() error {
	srv := s.srv
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		log.Printf("camera endpoint listening on http://localhost%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case sig := <-stop:
		signal.Stop(stop)
		log.Printf("shutdown signal received: %s", sig)
	case err := <-errCh:
		return fmt.Errorf("server failed to start: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			log.Printf("server shutdown timed out after 5s; forcing close")
			_ = srv.Close()
			return nil
		}
		return err
	}
	return nil
}
