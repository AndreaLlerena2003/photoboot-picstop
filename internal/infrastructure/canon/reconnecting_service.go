package canon

import (
	"context"
	"sync"
	"time"

	"photoboot-picstop/internal/domain"
)

// ReconnectingService wraps a Service and automatically reconnects when the camera
// is unplugged and replugged, so the application stays alive and resumes serving
// captures and live preview with no restart required.
//
// While disconnected, Capture and StartPreview return ErrCameraDisconnected immediately.
// Once the camera is detected again, a fresh Service is created and swapped in atomically —
// the next request succeeds transparently.
type ReconnectingService struct {
	outputDir string

	mu    sync.RWMutex
	inner *Service // nil while disconnected/reconnecting

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewReconnectingService creates a Service, connects to the camera, and starts the
// automatic reconnect loop. Returns an error only if the initial connection fails.
func NewReconnectingService(outputDir string) (*ReconnectingService, error) {
	inner, err := NewService(outputDir)
	if err != nil {
		return nil, err
	}
	rs := &ReconnectingService{
		outputDir: outputDir,
		inner:     inner,
		stopCh:    make(chan struct{}),
	}
	rs.wg.Add(1)
	go rs.reconnectLoop()
	return rs, nil
}

// ─────────────────────────────────────────────
// Port implementations — delegate to inner service
// ─────────────────────────────────────────────

func (rs *ReconnectingService) Capture(ctx context.Context) (string, error) {
	rs.mu.RLock()
	inner := rs.inner
	rs.mu.RUnlock()
	if inner == nil {
		return "", domain.ErrCameraDisconnected
	}
	return inner.Capture(ctx)
}

func (rs *ReconnectingService) StartPreview(ctx context.Context) (<-chan []byte, error) {
	rs.mu.RLock()
	inner := rs.inner
	rs.mu.RUnlock()
	if inner == nil {
		return nil, domain.ErrCameraDisconnected
	}
	return inner.StartPreview(ctx)
}

func (rs *ReconnectingService) StopPreview() {
	rs.mu.RLock()
	inner := rs.inner
	rs.mu.RUnlock()
	if inner != nil {
		inner.StopPreview()
	}
}

// Close stops the reconnect loop and closes the current inner service.
func (rs *ReconnectingService) Close() error {
	close(rs.stopCh)
	rs.wg.Wait()

	rs.mu.Lock()
	inner := rs.inner
	rs.inner = nil
	rs.mu.Unlock()

	if inner != nil {
		return inner.Close()
	}
	return nil
}

// ─────────────────────────────────────────────
// Reconnect loop
// ─────────────────────────────────────────────

func (rs *ReconnectingService) reconnectLoop() {
	SetGoroutineRole("reconnect")
	defer rs.wg.Done()

	for {
		// Grab the current live service and wait for it to disconnect.
		rs.mu.RLock()
		current := rs.inner
		rs.mu.RUnlock()

		if current != nil {
			select {
			case <-rs.stopCh:
				return
			case <-current.Disconnected():
			}

			cInfo("[reconnect] camera disconnected; will attempt reconnect")

			// Nil out inner so callers get ErrCameraDisconnected while we reconnect.
			rs.mu.Lock()
			rs.inner = nil
			rs.mu.Unlock()

			// Close the dead service (idempotent — shutdown event already ran most teardown).
			_ = current.Close()
		}

		// Reconnect with exponential backoff.
		delay := reconnectInitialDelay()
		max := reconnectMaxDelay()

		for {
			select {
			case <-rs.stopCh:
				return
			case <-time.After(delay):
			}

			cInfo("[reconnect] attempting camera reconnect...")
			svc, err := NewService(rs.outputDir)
			if err != nil {
				cWarn("[reconnect] reconnect failed: %v; retrying in %s", err, delay)
				if delay < max {
					delay = delay * 2
					if delay > max {
						delay = max
					}
				}
				continue
			}

			rs.mu.Lock()
			rs.inner = svc
			rs.mu.Unlock()

			cInfo("[reconnect] camera reconnected successfully")
			break
		}
	}
}
