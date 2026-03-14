//go:build !windows || !cgo

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

// Close no hace nada en el stub.
func (s *Service) Close() error {
	return nil
}
