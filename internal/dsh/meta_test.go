package dsh

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMetaReaderReadsTitleAndTranscriptActivity(t *testing.T) {
	root := t.TempDir()
	sessionID := "session-3ff1021e-2c32-494e-b7fb-d749376de850"
	writeFile(t, filepath.Join(root, "storages", "session_projcache", "sessions", sessionID+".json"),
		`{"version":7,"record":{"identity":{"createdAt":1792281074656,"cwd":"/work"},"rows":{"title":{"ver":1,"seq":1369,"val":"实况差别未体现"}}}}`)
	transcript := filepath.Join(root, "sessions", "--work--", sessionID, "session.v4.jsonl.zstd")
	writeFile(t, transcript, "zstd-bytes")
	if err := os.Chtimes(transcript, timeAt(t, 1792283000), timeAt(t, 1792283000)); err != nil {
		t.Fatal(err)
	}
	// A lock file must not win the activity scan.
	writeFile(t, filepath.Join(root, "sessions", "--work--", sessionID, "session.lock"), "")
	if err := os.Chtimes(filepath.Join(root, "sessions", "--work--", sessionID, "session.lock"),
		timeAt(t, 1792289000), timeAt(t, 1792289000)); err != nil {
		t.Fatal(err)
	}

	title, updated := MetaReader(root)(sessionID)
	if title != "实况差别未体现" {
		t.Fatalf("title = %q", title)
	}
	if updated != 1792283000000 {
		t.Fatalf("updated = %d", updated)
	}
}

func TestMetaReaderFallsBackToFirstUserMessage(t *testing.T) {
	root := t.TempDir()
	sessionID := "abc"
	writeFile(t, filepath.Join(root, "storages", "session_projcache", "sessions", sessionID+".json"),
		`{"record":{"rows":{"titleInput":{"val":{"first":{"text":"这个实况差别没体现出来"}}}}}}`)
	title, updated := MetaReader(root)(sessionID)
	if title != "这个实况差别没体现出来" || updated != 0 {
		t.Fatalf("title=%q updated=%d", title, updated)
	}
}

func TestMetaReaderRejectsUnsafeIdentifiers(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"", "../escape", "a/b", `a\b`} {
		if title, updated := MetaReader(root)(id); title != "" || updated != 0 {
			t.Fatalf("id %q resolved to %q/%d", id, title, updated)
		}
	}
}

func TestMetaReaderMissingFilesAreEmpty(t *testing.T) {
	if title, updated := MetaReader(t.TempDir())("missing"); title != "" || updated != 0 {
		t.Fatalf("missing = %q/%d", title, updated)
	}
}

func timeAt(t *testing.T, seconds int64) time.Time {
	t.Helper()
	return time.Unix(seconds, 0)
}
