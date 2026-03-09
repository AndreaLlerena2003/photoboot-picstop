package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config agrupa toda la configuración leída desde variables de entorno.
type Config struct {
	// CaptureDir es el directorio donde se guardan las fotos (env: CAPTURE_DIR).
	CaptureDir string
	// Port es el puerto HTTP del servidor (env: PORT).
	Port string
	// CaptureTimeoutMs es el timeout por defecto de captura en ms (env: CAPTURE_TIMEOUT_MS).
	CaptureTimeoutMs int
}

// Load lee la configuración desde el entorno y devuelve Config con valores por defecto donde aplique.
func Load() Config {
	return Config{
		CaptureDir:       envOrDefault("CAPTURE_DIR", "captures"),
		Port:             envOrDefault("PORT", "8080"),
		CaptureTimeoutMs: envIntOrDefault("CAPTURE_TIMEOUT_MS", 60_000, 250, 300_000),
	}
}

// DefaultCaptureTimeout devuelve el timeout de captura como Duration (para handlers).
func (c Config) DefaultCaptureTimeout() time.Duration {
	return time.Duration(c.CaptureTimeoutMs) * time.Millisecond
}

// envOrDefault devuelve el valor de key o fallback si está vacío.
func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// envIntOrDefault parsea key como int; si falla o está fuera de [min,max] devuelve fallback.
func envIntOrDefault(key string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return fallback
	}
	return n
}
