package canon

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// cameraDiscoveryTimeout devuelve el timeout para descubrir la primera cámara (env: CANON_DISCOVERY_TIMEOUT_MS).
func cameraDiscoveryTimeout() time.Duration {
	const defaultTimeout = 20 * time.Second
	raw := strings.TrimSpace(os.Getenv("CANON_DISCOVERY_TIMEOUT_MS"))
	if raw == "" {
		return defaultTimeout
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 1000 || ms > 300000 {
		return defaultTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

// jobDrainTimeout devuelve cuánto esperar a que los jobs de transferencia terminen antes de captura (env: CANON_JOB_DRAIN_TIMEOUT_MS).
func jobDrainTimeout() time.Duration {
	const fallback = 4 * time.Second
	raw := strings.TrimSpace(os.Getenv("CANON_JOB_DRAIN_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 250 || ms > 300000 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// shutterCommandTimeout devuelve el timeout del comando de disparo (env: CANON_SHUTTER_COMMAND_TIMEOUT_MS).
func shutterCommandTimeout() time.Duration {
	const fallback = 45 * time.Second
	raw := strings.TrimSpace(os.Getenv("CANON_SHUTTER_COMMAND_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 1000 || ms > 120000 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// capturePreemptWindow devuelve la ventana de tiempo tras preempt antes de permitir nueva captura (env: CANON_CAPTURE_PREEMPT_MS).
func capturePreemptWindow() time.Duration {
	const fallback = 1200 * time.Millisecond
	raw := strings.TrimSpace(os.Getenv("CANON_CAPTURE_PREEMPT_MS"))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 || ms > 10000 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// cameraSaveTimeout devuelve cuánto esperar a que la cámara termine de guardar en SD (env: CANON_SAVE_TIMEOUT_MS).
func cameraSaveTimeout() time.Duration {
	const fallback = 8 * time.Second
	raw := strings.TrimSpace(os.Getenv("CANON_SAVE_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 500 || ms > 60000 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

// flashConfigEnabled indica si se debe aplicar la configuración de no-flash (env: CANON_CONFIGURE_NO_FLASH).
func flashConfigEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("CANON_CONFIGURE_NO_FLASH")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}
