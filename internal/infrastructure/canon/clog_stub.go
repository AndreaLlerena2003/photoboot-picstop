//go:build !(windows || darwin) || !cgo

package canon

// clog_stub.go provides no-op implementations of the logging helpers and
// goroutine-role registry for platforms where clog.go is not compiled
// (i.e. non-Windows/Darwin or CGo-disabled builds).

// SetGoroutineRole is a no-op on stub platforms.
func SetGoroutineRole(_ string) {}

func cInfo(_ string, _ ...any)  {}
func cWarn(_ string, _ ...any)  {}
func cError(_ string, _ ...any) {}
func cDebug(_ string, _ ...any) {}
