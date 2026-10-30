package kimi

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"encoding/json"
	"strings"
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

func TestListSessionsFallsBackToFirstUserMessage(t *testing.T) {
	root := t.TempDir()
	writeSessionState(t, root, "wd", "session-fallback", "state.json", `{"cwd":"/work/fallback"}`)
	writeSessionState(t, root, "wd", "session-fallback", "agents/main/wire.jsonl", `
{"type":"turn.prompt","input":"继续","origin":{"kind":"system_trigger"}}
{"type":"turn.prompt","input":[{"type":"text","text":"<system-reminder>内部</system-reminder>\n修复  Kimi 标题"}],"origin":{"kind":"user"}}
{"type":"context.append_message","message":{"role":"user","content":"不应使用后面的消息","origin":{"kind":"user"}}}
`)
	writeSessionState(t, root, "wd", "session-official", "state.json", `{"title":"官方标题","cwd":"/work/official"}`)
	writeSessionState(t, root, "wd", "session-official", "agents/main/wire.jsonl", `{not-json`)
	writeSessionState(t, root, "wd", "session-empty", "state.json", `{"cwd":"/work/empty"}`)

	rows := ListSessions(root)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	byID := map[string]string{}
	for _, row := range rows {
		id, _ := row.MustGet("sessionId").(string)
		title, _ := row.MustGet("title").(string)
		byID[id] = title
	}
	if byID["session-fallback"] != "修复 Kimi 标题" || byID["session-official"] != "官方标题" || byID["session-empty"] != "" {
		t.Fatalf("titles = %v", byID)
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

func TestRetrySourceTracksLatestRetry(t *testing.T) {
	root := t.TempDir()
	retry := `{"type":"turn.step.retrying","turnId":7,"nextAttempt":8,"maxAttempts":10,"delayMs":1234,"errorName":"APIProviderRateLimitError","errorMessage":"429 overloaded","statusCode":429,"time":123456}`
	wire := strings.Repeat("x", maxRetryWireBytes) + "\n" +
		`{"type":"llm.request"}` + "\n" + retry + "\n"
	path := writeSessionState(t, root, "wd", "session-retry", "agents/main/wire.jsonl", wire)
	source := NewRetrySource(root)

	status, err := source.Status("session-retry")
	if err != nil {
		t.Fatal(err)
	}
	if status == nil || status.MustGet("nextAttempt").(json.Number).String() != "8" ||
		status.MustGet("maxAttempts").(json.Number).String() != "10" ||
		status.MustGet("statusCode").(json.Number).String() != "429" ||
		status.MustGet("errorMessage") != "429 overloaded" {
		t.Fatalf("retry status = %v", status)
	}

	wire += `{"type":"context.append_loop_event","event":{"type":"content.part"}}` + "\n"
	if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = source.Status("session-retry")
	if err != nil || status == nil {
		t.Fatalf("ordinary activity cleared retry: %v %v", status, err)
	}

	for _, event := range []string{`{"type":"llm.request"}`, `{"type":"turn.ended"}`, `{"type":"turn.prompt"}`, `{"type":"turn.cancel"}`} {
		wire = retry + "\n" + event + "\n"
		if err := os.WriteFile(path, []byte(wire), 0o600); err != nil {
			t.Fatal(err)
		}
		status, err = source.Status("session-retry")
		if err != nil || status != nil {
			t.Fatalf("%s did not clear retry: %v %v", event, status, err)
		}
	}
}

func TestMetaReaderUsesLastWireEvent(t *testing.T) {
	root := t.TempDir()
	writeSessionState(t, root, "wd", "session-live", "agents/main/wire.jsonl",
		"{\"type\":\"turn.prompt\",\"input\":\"调研\",\"time\":1000}\n"+
			"{\"type\":\"message.delta\",\"time\":2000}\n")
	// state.json sits far newer: a session/load (startup preload or chat
	// open) just rewrote its updatedAt.
	writeSessionState(t, root, "wd", "session-live", "state.json",
		`{"title":"调研","cwd":"/work","updatedAt":9999999999999}`)
	if _, updated := MetaReader(root)("session-live"); updated != 2000 {
		t.Fatalf("updated=%d want 2000 (last wire event, not the load-time state.json)", updated)
	}
}

func TestMetaReaderAcceptsISOWireTime(t *testing.T) {
	root := t.TempDir()
	writeSessionState(t, root, "wd", "session-iso", "agents/main/wire.jsonl",
		"{\"type\":\"turn.prompt\",\"time\":1000}\n"+
			"{\"type\":\"message.delta\",\"time\":\"2026-10-23T11:22:15.384Z\"}\n")
	want := time.Date(2026, 10, 23, 11, 22, 15, 384000000, time.UTC).UnixMilli()
	if _, updated := MetaReader(root)("session-iso"); updated != want {
		t.Fatalf("updated=%d want %d (ISO string wire time)", updated, want)
	}
}

func TestMetaReaderUnknownSessionOrMissingWire(t *testing.T) {
	root := t.TempDir()
	writeSessionState(t, root, "wd", "session-state-only", "state.json", `{"cwd":"/work","updatedAt":123}`)
	meta := MetaReader(root)
	if _, updated := meta("session-state-only"); updated != 0 {
		t.Fatalf("updated=%d want 0 when only state.json exists", updated)
	}
	if _, updated := meta("session-missing"); updated != 0 {
		t.Fatalf("updated=%d want 0 for a missing session", updated)
	}
	if _, updated := meta("../escape"); updated != 0 {
		t.Fatalf("updated=%d want 0 for a path-like id", updated)
	}
}

// The tail window can open mid-event (a huge base64 line) or entirely inside
// a stretch of untimestamped records; the scan must skip the cut line and
// grow past the noise instead of reporting nothing.
func TestMetaReaderSkipsCutLineAndGrowsPastUntimestampedTail(t *testing.T) {
	root := t.TempDir()
	var body strings.Builder
	body.WriteString("{\"type\":\"turn.prompt\",\"input\":\"x\",\"time\":1000}\n")
	for body.Len() < wireTailFirstScan+16*1024 {
		body.WriteString("{\"type\":\"context.append_loop_event\",\"event\":{\"type\":\"content.part\",\"blob\":\"")
		body.WriteString(strings.Repeat("y", 512))
		body.WriteString("\"}}\n")
	}
	writeSessionState(t, root, "wd", "session-noise", "agents/main/wire.jsonl", body.String())
	if _, updated := MetaReader(root)("session-noise"); updated != 1000 {
		t.Fatalf("updated=%d want 1000 (scan must grow past the untimestamped tail)", updated)
	}
}

// A relocated project can leave the same session in two workspaces; the
// newest transcript carries the current timeline.
func TestMetaReaderPicksNewestCopy(t *testing.T) {
	root := t.TempDir()
	old := writeSessionState(t, root, "wd-old", "session-copy", "agents/main/wire.jsonl",
		"{\"type\":\"message.delta\",\"time\":1000}\n")
	writeSessionState(t, root, "wd-new", "session-copy", "agents/main/wire.jsonl",
		"{\"type\":\"message.delta\",\"time\":3000}\n")
	newer := filepath.Join(root, "sessions", "wd-new", "session-copy", "agents", "main", "wire.jsonl")
	now := time.Now()
	if err := os.Chtimes(old, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, now, now); err != nil {
		t.Fatal(err)
	}
	if _, updated := MetaReader(root)("session-copy"); updated != 3000 {
		t.Fatalf("updated=%d want 3000 (newest copy wins)", updated)
	}
}
