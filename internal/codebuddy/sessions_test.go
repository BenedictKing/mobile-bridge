package codebuddy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path string, content string) time.Time {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1792283000, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return mtime
}

func TestListSessionsReadsTranscripts(t *testing.T) {
	home := t.TempDir()
	mtime := writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-demo", "sess-1.jsonl"),
		`{"type":"session-meta","sessionId":"sess-1","timestamp":1792378459466}
{"type":"message","role":"user","content":[{"type":"input_text","text":"第一条消息"}],"cwd":"/work/demo"}
{"type":"ai-title","aiTitle":"实况标题","sessionId":"sess-1","cwd":"/work/demo"}
`)
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-other", "sess-2.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"没有 AI 标题时用首条消息"}],"cwd":"/work/other"}
`)
	// Empty in-progress transcripts and meta-only files carry no cwd and can
	// never be opened (session/load needs a cwd).
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-demo", "sess-empty.jsonl"), "")
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-demo", "sess-meta-only.jsonl"),
		`{"type":"session-meta","sessionId":"sess-meta-only","timestamp":1792378459466}
`)
	// Non-transcript entries are ignored.
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-demo", "notes.txt"), "{}")

	rows := ListSessions(home)
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		id, _ := row.MustGet("sessionId").(string)
		byID[id] = map[string]any{
			"title":     row.MustGet("title"),
			"cwd":       row.MustGet("cwd"),
			"updatedAt": row.MustGet("updatedAt"),
		}
	}
	first := byID["sess-1"]
	if first == nil || first["title"] != "实况标题" || first["cwd"] != "/work/demo" {
		t.Fatalf("sess-1 row = %v", first)
	}
	updated, ok := first["updatedAt"].(json.Number)
	if !ok {
		t.Fatalf("sess-1 updatedAt = %v", first["updatedAt"])
	}
	if ms, _ := updated.Int64(); ms != mtime.UnixMilli() {
		t.Fatalf("sess-1 updatedAt = %d, want %d", ms, mtime.UnixMilli())
	}
	second := byID["sess-2"]
	if second == nil || second["title"] != "没有 AI 标题时用首条消息" || second["cwd"] != "/work/other" {
		t.Fatalf("sess-2 row = %v", second)
	}
}

func TestListSessionsMissingStoreIsEmpty(t *testing.T) {
	if rows := ListSessions(t.TempDir()); len(rows) != 0 {
		t.Fatalf("rows = %v", rows)
	}
}

func TestListSessionsSkipsProtocolTitles(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(ProjectsDir(home), "Users-test-work-demo")
	writeTranscript(t, filepath.Join(dir, "sess-caveat.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"<system-reminder data-role=\"command-caveat\">Caveat: local command.</system-reminder>"}],"cwd":"/work/demo"}
{"type":"message","role":"user","content":[{"type":"input_text","text":"<command-name>/status</command-name>"}],"cwd":"/work/demo"}
{"type":"message","role":"user","content":[{"type":"input_text","text":"修复列表标题"}],"cwd":"/work/demo"}
`)
	writeTranscript(t, filepath.Join(dir, "sess-only-protocol.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"<local-command-stdout>ok</local-command-stdout>"}],"cwd":"/work/demo"}
`)
	writeTranscript(t, filepath.Join(dir, "sess-team.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"[Team context]\n来源成员：lead\n\n[Assigned task]\n只读检查交付语义"}],"cwd":"/work/demo"}
`)
	titles := map[string]any{}
	for _, row := range ListSessions(home) {
		id, _ := row.MustGet("sessionId").(string)
		titles[id] = row.MustGet("title")
	}
	if titles["sess-caveat"] != "修复列表标题" {
		t.Fatalf("caveat/command rows must be skipped: %v", titles["sess-caveat"])
	}
	if titles["sess-only-protocol"] != nil {
		t.Fatalf("protocol-only transcript must carry no title: %v", titles["sess-only-protocol"])
	}
	if titles["sess-team"] != "只读检查交付语义" {
		t.Fatalf("team envelope title must be the assigned task: %v", titles["sess-team"])
	}
}
