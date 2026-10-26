package kimi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"encoding/json"
)

func writeSessionState(t *testing.T, root, workspace, session, relative, content string) string {
	t.Helper()
	path := filepath.Join(root, "sessions", workspace, session, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestListSessionsReadsNativeIndex(t *testing.T) {
	root := t.TempDir()
	writeSessionState(t, root, "wd-new", "session-new", "state.json", `{"title":"新会话","cwd":"/work/new","updatedAt":2000,"archived":false}`)
	writeSessionState(t, root, "wd-old", "session-old", "session-meta/state.json", `{"title":"旧会话","custom":{"cwd":"/work/old"},"updatedAt":"1970-01-01T00:00:01Z","archived":false}`)
	writeSessionState(t, root, "wd-archived", "session-archived", "state.json", `{"cwd":"/work/archived","archived":true}`)
	writeSessionState(t, root, "wd-missing-cwd", "session-missing-cwd", "state.json", `{"title":"无法加载","updatedAt":3000}`)
	writeSessionState(t, root, "wd-corrupt", "session-corrupt", "state.json", `{not-json`)
	writeSessionState(t, root, ".index-cache", "session-cache", "state.json", `{"cwd":"/work/cache"}`)

	rows := ListSessions(root)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	if rows[0].MustGet("sessionId") != "session-new" || rows[1].MustGet("sessionId") != "session-old" {
		t.Fatalf("rows not sorted newest first: %v", rows)
	}
	if rows[0].MustGet("title") != "新会话" || rows[1].MustGet("cwd") != "/work/old" {
		t.Fatalf("native fields missing: %v", rows)
	}
	updated, _ := rows[1].MustGet("updatedAt").(json.Number)
	if updated.String() != "1000" {
		t.Fatalf("ISO updatedAt = %v", updated)
	}
}

func TestListSessionsUsesStateMtimeFallback(t *testing.T) {
	root := t.TempDir()
	path := writeSessionState(t, root, "wd", "session-mtime", "state.json", `{"workDir":"/work/mtime"}`)
	mtime := time.UnixMilli(123456)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	rows := ListSessions(root)
	if len(rows) != 1 || rows[0].MustGet("updatedAt").(json.Number).String() != "123456" {
		t.Fatalf("mtime fallback = %v", rows)
	}
}

func TestListSessionsMissingStore(t *testing.T) {
	if rows := ListSessions(t.TempDir()); rows != nil {
		t.Fatalf("missing store = %v", rows)
	}
}
