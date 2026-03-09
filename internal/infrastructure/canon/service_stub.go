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

// Close no hace nada en el stub.
func (s *Service) Close() error {
	return nil
}
