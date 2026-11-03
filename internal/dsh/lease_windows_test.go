//go:build windows

package dsh

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWriteOwnerObservesNamedSemaphore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.lock")
	name, err := windows.UTF16PtrFromString(lockName(path))
	if err != nil {
		t.Fatal(err)
	}
	held := createSemaphore(name, 0)
	if held == 0 {
		t.Fatal("CreateSemaphoreW failed")
	}
	if owned, known := probeWriteOwner([]string{path}); !known || !owned {
		t.Fatalf("held semaphore = owned %v, known %v", owned, known)
	}
	if err := windows.CloseHandle(held); err != nil {
		t.Fatal(err)
	}
	if owned, known := probeWriteOwner([]string{path}); !known || owned {
		t.Fatalf("released semaphore = owned %v, known %v", owned, known)
	}
}
