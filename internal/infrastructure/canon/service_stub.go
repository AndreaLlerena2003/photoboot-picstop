//go:build !(windows || darwin) || !cgo

package canon

import (
	"context"

	"photoboot-picstop/internal/domain"
)

// Service es un stub cuando EDSDK no está disponible (no Windows o sin cgo).
type Service struct{}

// NewService devuelve nil, domain.ErrUnsupported en plataformas no soportadas.
func NewService(_ string) (*Service, error) {
	return nil, domain.ErrUnsupported
}

// Capture devuelve "", domain.ErrUnsupported en el stub.
func (s *Service) Capture(_ context.Context) (string, error) {
	return "", domain.ErrUnsupported
}

// StartPreview devuelve ErrPreviewUnavailable en el stub.
func (s *Service) StartPreview(_ context.Context) (<-chan []byte, error) {
	return nil, domain.ErrPreviewUnavailable
}

// StopPreview no hace nada en el stub.
func (s *Service) StopPreview() {}

// Disconnected devuelve un canal que nunca se cierra en el stub (plataforma no soportada).
func (s *Service) Disconnected() <-chan struct{} {
	return make(chan struct{}) // never closed; stub never connects
}

// Close no hace nada en el stub.
func (s *Service) Close() error {
	return nil
}
