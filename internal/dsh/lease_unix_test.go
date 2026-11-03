//go:build linux || darwin

package dsh

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWriteOwnerObservesKernelLock(t *testing.T) {
	root := t.TempDir()
	const id = "session-lock"
	dir := filepath.Join(root, "sessions", "--work--", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if owned, known := WriteOwner(root, id); !known || owned {
		t.Fatalf("unlocked probe = owned %v, known %v", owned, known)
	}

	held, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		held.Close()
		t.Fatal(err)
	}
	if owned, known := WriteOwner(root, id); !known || !owned {
		t.Fatalf("locked probe = owned %v, known %v", owned, known)
	}
	_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	_ = held.Close()
	if owned, known := WriteOwner(root, id); !known || owned {
		t.Fatalf("released probe = owned %v, known %v", owned, known)
	}
}

func TestWriteOwnerMissingLockIsFreeUnderLeaseProtocol(t *testing.T) {
	if owned, known := WriteOwner(t.TempDir(), "missing"); !known || owned {
		t.Fatalf("missing lock = owned %v, known %v", owned, known)
	}
}
