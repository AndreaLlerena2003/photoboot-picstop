package http

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"photoboot-picstop/internal/application/capture"
)

// Server is the HTTP presentation server: routes and controllers only.
type Server struct {
	srv *http.Server
}

// NewServer builds the server with capture route and controller (dependencies injected).
func NewServer(addr string, captureUseCase *capture.CapturePhotoUseCase, defaultCaptureTimeout time.Duration) *Server {
	ctrl := NewCaptureController(captureUseCase, defaultCaptureTimeout)
	mux := http.NewServeMux()
	mux.HandleFunc("/capture", ctrl.ServeHTTP)
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

	go func() {
		log.Printf("camera endpoint listening on http://localhost%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server listen error: %v", err)
		}
	}()

	sig := <-stop
	signal.Stop(stop)
	log.Printf("shutdown signal received: %s", sig)

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
