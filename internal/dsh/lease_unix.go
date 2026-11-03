//go:build linux || darwin

package dsh

import (
	"errors"
	"os"
	"syscall"
)

func probeWriteOwner(paths []string) (owned, known bool) {
	for _, path := range paths {
		owned, known := probeWriteLock(path)
		if !known || owned {
			return owned, known
		}
	}
	return false, true
}

func probeWriteLock(path string) (owned, known bool) {
	for attempt := 0; attempt < 3; attempt++ {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, true
		}
		if err != nil {
			return false, false
		}
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return true, true
		}
		if err != nil {
			_ = file.Close()
			return false, false
		}
		held, heldErr := file.Stat()
		current, currentErr := os.Stat(path)
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		if heldErr != nil || currentErr != nil {
			return false, false
		}
		if os.SameFile(held, current) {
			return false, true
		}
	}
	return false, false
}
