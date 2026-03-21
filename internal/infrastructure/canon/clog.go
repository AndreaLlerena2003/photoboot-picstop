//go:build (windows || darwin) && cgo

package canon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Goroutine identity registry
// ─────────────────────────────────────────────────────────────────────────────

// goroutineRoles maps goroutine IDs to human-readable role names.
// Call SetGoroutineRole at the top of each goroutine so every subsequent
// log line from that goroutine shows its role.
var goroutineRoles sync.Map // map[int64]string

// SetGoroutineRole registers a human-readable name for the calling goroutine.
func SetGoroutineRole(role string) {
	goroutineRoles.Store(currentGID(), role)
}

// currentGID returns the current goroutine ID by parsing the runtime stack trace.
func currentGID() int64 {
	var buf [128]byte
	n := runtime.Stack(buf[:], false)
	// Stack trace starts with "goroutine 42 [running]:\n..."
	s := strings.TrimPrefix(string(buf[:n]), "goroutine ")
	if i := strings.IndexByte(s, ' '); i > 0 {
		id, _ := strconv.ParseInt(s[:i], 10, 64)
		return id
	}
	return 0
}

// gidRole returns a fixed-width tag like "g42:pump" or "g7:?" for the current goroutine.
func gidRole() string {
	id := currentGID()
	role := "?"
	if v, ok := goroutineRoles.Load(id); ok {
		role = v.(string)
	}
	return fmt.Sprintf("g%d:%s", id, role)
}

// ─────────────────────────────────────────────────────────────────────────────
// Custom slog handler
// ─────────────────────────────────────────────────────────────────────────────

// canonHandler is a slog.Handler that prepends a timestamp, level, and goroutine
// identity tag to every log line.
//
// Output format:
//
//	15:04:05.000 INFO  [g42:pump         ]  message text key=value
type canonHandler struct {
	mu  sync.Mutex
	out io.Writer
}

func newCanonHandler(w io.Writer) *canonHandler {
	return &canonHandler{out: w}
}

func (h *canonHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *canonHandler) Handle(_ context.Context, r slog.Record) error {
	// goroutineTag is captured here, on the same goroutine that called slog.Info/etc.
	tag := gidRole()

	var buf bytes.Buffer

	ts := r.Time.Format("15:04:05.000")

	var level string
	switch {
	case r.Level >= slog.LevelError:
		level = "ERROR"
	case r.Level >= slog.LevelWarn:
		level = "WARN "
	case r.Level >= slog.LevelInfo:
		level = "INFO "
	default:
		level = "DEBUG"
	}

	// %-18s gives a fixed-width column so messages align across goroutines.
	fmt.Fprintf(&buf, "%s %s [%-18s]  %s", ts, level, tag, r.Message)

	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&buf, "  %s=%v", a.Key, a.Value)
		return true
	})
	buf.WriteByte('\n')

	h.mu.Lock()
	_, err := h.out.Write(buf.Bytes())
	h.mu.Unlock()
	return err
}

func (h *canonHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *canonHandler) WithGroup(name string) slog.Handler       { return h }

// ─────────────────────────────────────────────────────────────────────────────
// Package-level logger
// ─────────────────────────────────────────────────────────────────────────────

// clog is the structured logger for the canon package.
// Use cInfo/cWarn/cError/cDebug in all canon code instead of log.Printf.
var clog = slog.New(newCanonHandler(os.Stderr))

func cInfo(format string, args ...any) {
	clog.Log(context.Background(), slog.LevelInfo, fmt.Sprintf(format, args...))
}

func cWarn(format string, args ...any) {
	clog.Log(context.Background(), slog.LevelWarn, fmt.Sprintf(format, args...))
}

func cError(format string, args ...any) {
	clog.Log(context.Background(), slog.LevelError, fmt.Sprintf(format, args...))
}

func cDebug(format string, args ...any) {
	clog.Log(context.Background(), slog.LevelDebug, fmt.Sprintf(format, args...))
}

// cAttr logs an INFO message with additional structured key=value attributes.
func cAttr(msg string, attrs ...slog.Attr) {
	r := slog.NewRecord(time.Now(), slog.LevelInfo, msg, 0)
	r.AddAttrs(attrs...)
	_ = clog.Handler().Handle(context.Background(), r)
}
