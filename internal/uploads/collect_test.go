package uploads

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

func TestCollectRemovesOnlyExpiredUnprotectedUploads(t *testing.T) {
	manager := New(t.TempDir())
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("x"), 32)...)
	put := func(thread string) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := manager.Put(thread, id, "photo.png", png); err != nil {
			t.Fatal(err)
		}
		return id
	}
	age := func(thread, id string) {
		t.Helper()
		meta := manager.metaPath(thread, id)
		raw, err := os.ReadFile(meta)
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsoncompat.Unmarshal(raw)
		row, _ := value.(*jsoncompat.Object)
		if err != nil || row == nil {
			t.Fatalf("metadata: %v", err)
		}
		row.Set("createdAt", time.Now().Add(-30*24*time.Hour).Unix())
		if err := os.WriteFile(meta, append(jsoncompat.EncodePythonIndent2(row), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	expired, protected := put("thread-1"), put("thread-1")
	age("thread-1", expired)
	age("thread-1", protected)
	fresh := put("thread-2")
	damaged := put("thread-2")
	if err := os.WriteFile(manager.metaPath("thread-2", damaged), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	orphan := put("thread-3")
	age("thread-3", orphan)
	stray := filepath.Join(manager.Root, "thread-1", "notes.txt")
	if err := os.WriteFile(stray, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed := manager.Collect(DefaultRetention, func(thread, id string) bool {
		return thread == "thread-1" && id == protected
	}, time.Now())
	if removed != 2 {
		t.Fatalf("removed = %d, want 2 (expired and emptied-thread upload)", removed)
	}
	for id, want := range map[string]bool{expired: false, protected: true} {
		_, err := manager.Get("thread-1", id)
		_, statErr := os.Stat(filepath.Join(manager.Root, "thread-1", id))
		if (err == nil) != want || (statErr == nil) != want {
			t.Fatalf("upload %s: available=%v directory=%v, want %v", id, err == nil, statErr == nil, want)
		}
	}
	// A fresh upload still serves; an unreadable row stays on disk untouched
	// (Get cannot describe it, so only its files are checked).
	if _, err := manager.Get("thread-2", fresh); err != nil {
		t.Fatalf("fresh upload must survive: %v", err)
	}
	for _, path := range []string{manager.metaPath("thread-2", damaged), manager.folder("thread-2", damaged)} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("upload with unreadable metadata must stay on disk: %v", err)
		}
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("files that are not uploads must stay: %v", err)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, "thread-3")); !os.IsNotExist(err) {
		t.Fatalf("emptied thread directory should be removed, stat err = %v", err)
	}
}
