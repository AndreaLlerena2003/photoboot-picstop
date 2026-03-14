//go:build darwin && cgo

package canon

/*
#cgo darwin LDFLAGS: -F${SRCDIR}/../../../libs -framework EDSDK
*/
import "C"

import (
	"runtime"
)

// initPlatformThreading initializes the platform-specific threading context.
// On macOS, it currently locks the OS thread as a placeholder for CFRunLoop integration.
func initPlatformThreading() (func(), error) {
	runtime.LockOSThread()
	return func() {
		runtime.UnlockOSThread()
	}, nil
}

// withPlatformThreading executes fn in an OS thread with the required platform context.
func withPlatformThreading(fn func() error) error {
	uninit, err := initPlatformThreading()
	if err != nil {
		return err
	}
	defer uninit()
	return fn()
}
