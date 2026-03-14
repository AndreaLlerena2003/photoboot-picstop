//go:build windows && cgo

package canon

/*
#include <windows.h>

static HRESULT codexCoInitializeSTA(void) {
    return CoInitializeEx(NULL, COINIT_APARTMENTTHREADED);
}

static void codexCoUninitialize(void) {
    CoUninitialize();
}
*/
import "C"

import (
	"fmt"
	"runtime"
)

// initPlatformThreading initializes COM STA on the current OS thread.
// It locks the OS thread and returns a function to uninitialize it.
func initPlatformThreading() (func(), error) {
	runtime.LockOSThread()
	hr := uint32(C.codexCoInitializeSTA())
	switch hr {
	case 0x00000000, 0x00000001: // S_OK / S_FALSE
		return func() {
			C.codexCoUninitialize()
			runtime.UnlockOSThread()
		}, nil
	case 0x80010106: // RPC_E_CHANGED_MODE
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CoInitializeEx(COINIT_APARTMENTTHREADED) failed: RPC_E_CHANGED_MODE (0x%08X)", hr)
	default:
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("CoInitializeEx(COINIT_APARTMENTTHREADED) failed: 0x%08X", hr)
	}
}

// withPlatformThreading executes fn in an OS thread with COM STA initialized.
func withPlatformThreading(fn func() error) error {
	uninit, err := initPlatformThreading()
	if err != nil {
		return err
	}
	defer uninit()
	return fn()
}
