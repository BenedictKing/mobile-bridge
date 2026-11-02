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

func TestMetaReaderUsesLastMessageTime(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(ProjectsDir(home), "Users-test-work-demo")
	// Session with multiple events: session-meta from loads, then real messages
	mtime := writeTranscript(t, filepath.Join(dir, "sess-1.jsonl"),
		`{"type":"session-meta","sessionId":"sess-1","timestamp":1792378459466}
{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}],"cwd":"/work/demo","timestamp":1792378460000}
{"type":"message","role":"assistant","content":[{"type":"text","text":"reply"}],"timestamp":1792378461000}
{"type":"session-meta","sessionId":"sess-1","timestamp":1792378500000}
{"type":"session-meta","sessionId":"sess-1","timestamp":1792378550000}
`)
	// Session with only session-meta (load events, no real interaction)
	writeTranscript(t, filepath.Join(dir, "sess-2.jsonl"),
		`{"type":"session-meta","sessionId":"sess-2","timestamp":1792378600000}
{"type":"session-meta","sessionId":"sess-2","timestamp":1792378650000}
`)
	// Session with tool events
	writeTranscript(t, filepath.Join(dir, "sess-3.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"run test"}],"timestamp":1792378700000}
{"type":"tool-use","timestamp":1792378710000}
{"type":"tool-result","timestamp":1792378720000}
{"type":"session-meta","sessionId":"sess-3","timestamp":1792378800000}
`)

	resolve := MetaReader(home)

	// sess-1: should use last message time, not session-meta
	_, ms1 := resolve("sess-1")
	if ms1 != 1792378461000 {
		t.Errorf("sess-1 time = %d, want 1792378461000 (last message, not session-meta)", ms1)
	}

	// sess-2: only session-meta (load events, no real interaction) — their
	// timestamps are load time, so the resolver yields nothing and the row
	// keeps its own fields instead of floating to the preload time.
	_, ms2 := resolve("sess-2")
	if ms2 != 0 {
		t.Errorf("sess-2 time = %d, want 0 (session-meta is not interaction)", ms2)
	}

	// sess-3: should use last tool-result time
	_, ms3 := resolve("sess-3")
	if ms3 != 1792378720000 {
		t.Errorf("sess-3 time = %d, want 1792378720000 (last tool event)", ms3)
	}

	// Missing session
	_, ms4 := resolve("missing")
	if ms4 != 0 {
		t.Errorf("missing session time = %d, want 0", ms4)
	}

	// Verify mtime is NOT used (mtime is much earlier than any timestamp)
	if ms1 == mtime.UnixMilli() {
		t.Errorf("MetaReader incorrectly used mtime instead of transcript timestamp")
	}
}

func TestMetaReaderHandlesEmptyAndInvalidTranscripts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(ProjectsDir(home), "Users-test-work-demo")

	// Empty transcript
	writeTranscript(t, filepath.Join(dir, "sess-empty.jsonl"), "")
	// Invalid JSON
	writeTranscript(t, filepath.Join(dir, "sess-bad.jsonl"), "{not json\n")
	// No timestamp fields
	writeTranscript(t, filepath.Join(dir, "sess-no-ts.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"no timestamp"}]}
`)

	resolve := MetaReader(home)

	_, ms1 := resolve("sess-empty")
	if ms1 != 0 {
		t.Errorf("empty transcript time = %d, want 0", ms1)
	}

	_, ms2 := resolve("sess-bad")
	if ms2 != 0 {
		t.Errorf("invalid transcript time = %d, want 0", ms2)
	}

	_, ms3 := resolve("sess-no-ts")
	if ms3 != 0 {
		t.Errorf("no-timestamp transcript time = %d, want 0", ms3)
	}
}

func TestMetaReaderHandlesDuplicateSessions(t *testing.T) {
	home := t.TempDir()
	// Same session ID in different projects (relocated project scenario)
	dir1 := filepath.Join(ProjectsDir(home), "Users-test-work-old")
	dir2 := filepath.Join(ProjectsDir(home), "Users-test-work-new")

	// Older file with older mtime
	oldMtime := time.Unix(1792283000, 0)
	oldPath := filepath.Join(dir1, "sess-dup.jsonl")
	if err := os.MkdirAll(filepath.Dir(oldPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte(`{"type":"message","timestamp":1792378400000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldPath, oldMtime, oldMtime); err != nil {
		t.Fatal(err)
	}

	// Newer file with newer mtime
	time.Sleep(2 * time.Millisecond) // Ensure different mtime
	newMtime := time.Unix(1792283100, 0)
	newPath := filepath.Join(dir2, "sess-dup.jsonl")
	if err := os.MkdirAll(filepath.Dir(newPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(`{"type":"message","timestamp":1792378500000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newPath, newMtime, newMtime); err != nil {
		t.Fatal(err)
	}

	resolve := MetaReader(home)

	// Should use the newer file's timestamp
	_, ms := resolve("sess-dup")
	if ms != 1792378500000 {
		t.Errorf("duplicate session time = %d, want 1792378500000 (newer file)", ms)
	}
}

func TestMetaReaderIgnoresParasiticMetaOnlyCopy(t *testing.T) {
	home := t.TempDir()
	// A real conversation lives in its own project slug...
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-work-codehub", "sess-live.jsonl"),
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"跑一下 ga fetch"}],"cwd":"/work/codehub","timestamp":1793340000000}
{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"timestamp":1793340600000}
`)
	// ...while every gateway restart's preload (session/load answered under a
	// foreign cwd) appends meta-only records to a parasitic copy in that other
	// project's slug. Its mtime is the newest of the two.
	parasitePath := filepath.Join(ProjectsDir(home), "Users-test-work-bridge", "sess-live.jsonl")
	writeTranscript(t, parasitePath,
		`{"type":"session-meta","sessionId":"sess-live","timestamp":1793599741032}
{"type":"session-meta","sessionId":"sess-live","timestamp":1793599741032}
`)
	time.Sleep(2 * time.Millisecond) // Ensure the parasite's mtime is strictly newer
	if err := os.Chtimes(parasitePath, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}

	_, ms := MetaReader(home)("sess-live")
	if ms != 1793340600000 {
		t.Fatalf("time = %d, want 1793340600000 (real copy's last message, not the parasite's load stamps)", ms)
	}
}
