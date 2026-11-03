//go:build windows

package dsh

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const waitTimeout = 0x00000102

var (
	createSemaphoreW = windows.NewLazyDLL("kernel32.dll").NewProc("CreateSemaphoreW")
	releaseSemaphore = windows.NewLazyDLL("kernel32.dll").NewProc("ReleaseSemaphore")
)

func createSemaphore(name *uint16, initialCount int32) windows.Handle {
	result, _, _ := createSemaphoreW.Call(0, uintptr(initialCount), 1, uintptr(unsafe.Pointer(name)))
	return windows.Handle(result)
}

func release(handle windows.Handle) {
	_, _, _ = releaseSemaphore.Call(uintptr(handle), 1, 0)
}

func probeWriteOwner(paths []string) (owned, known bool) {
	for _, path := range paths {
		owned, known := probeWriteSemaphore(path)
		if !known || owned {
			return owned, known
		}
	}
	return false, true
}

func probeWriteSemaphore(path string) (owned, known bool) {
	name, err := windows.UTF16PtrFromString(lockName(path))
	if err != nil {
		return false, false
	}
	handle := createSemaphore(name, 1)
	if handle == 0 {
		return false, false
	}
	defer windows.CloseHandle(handle)
	wait, err := windows.WaitForSingleObject(handle, 0)
	if wait == windows.WAIT_OBJECT_0 {
		release(handle)
		return false, true
	}
	if wait == waitTimeout {
		return true, true
	}
	return false, false
}

func lockName(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	digest := sha256.Sum256([]byte(strings.ToLower(absolute)))
	return `Local\dsh-session-lock-` + hex.EncodeToString(digest[:])
}
