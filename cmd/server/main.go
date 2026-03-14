// Package main is the composition root: wires layers via dependency injection (no logic).
package main

import (
	"log"

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
	srv := httppkg.NewServer(":"+cfg.Port, captureUseCase, previewUseCase, cfg.DefaultCaptureTimeout())
	if err := srv.Start(); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
