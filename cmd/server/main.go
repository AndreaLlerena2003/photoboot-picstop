// Package main is the composition root: wires layers via dependency injection (no logic).
package main

import (
	"log"
	"os"

	"photoboot-picstop/config"
	"photoboot-picstop/internal/application/capture"
	"photoboot-picstop/internal/application/preview"
	"photoboot-picstop/internal/infrastructure/canon"
	httppkg "photoboot-picstop/internal/presentation/http"
)

func main() {
	cfg := config.Load()
	log.Printf("initializing Canon service (capture_dir=%s)", cfg.CaptureDir)

	// Infrastructure: self-reconnecting camera service (implements ICameraCapturePort + IPreviewPort).
	// Automatically discovers and reconnects to the camera when replugged — no restart needed.
	camera, err := canon.NewReconnectingService(cfg.CaptureDir)
	if err != nil {
		log.Fatalf("failed to initialize Canon service: %v", err)
	}
	log.Printf("Canon service initialized")
	defer func() {
		if err := camera.Close(); err != nil {
			log.Printf("close Canon service: %v", err)
		}
	}()

	// Application: use cases with ports injected (DIP)
	captureUseCase := capture.NewCapturePhotoUseCase(camera)
	previewUseCase := preview.NewStreamPreviewUseCase(camera)

	// Presentation: server with use cases injected
	srv := httppkg.NewServer(":"+cfg.Port, captureUseCase, previewUseCase, cfg.DefaultCaptureTimeout(), cfg.CaptureDir)
	if err := srv.Start(); err != nil {
		// Do NOT use log.Fatalf here — it calls os.Exit which skips all defers,
		// including camera.Close(), leaving the EDSDK session and camera ref leaked.
		log.Printf("server error: %v", err)
		if closeErr := camera.Close(); closeErr != nil {
			log.Printf("camera close during error exit: %v", closeErr)
		}
		os.Exit(1)
	}
}
