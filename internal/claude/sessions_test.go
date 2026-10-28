package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, path, body string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestMetaReaderUsesLastTimestampedRecord(t *testing.T) {
	root := t.TempDir()
	id := "3e085a48-5482-488a-82b9-1788f9399814"
	body := `{"type":"user","timestamp":"2026-10-23T11:20:01.000Z","message":{"role":"user"}}
{"type":"assistant","timestamp":"2026-10-23T11:22:15.384Z"}
{"type":"summary","summary":"好了吗","leafUuid":"x"}
{"type":"cost-state"}
{"type":"cost-state"}
`
	// The mtime sits far beyond any record: a resume appended the trailing
	// cost-state lines and the file clock moved with them.
	writeTranscript(t, filepath.Join(root, "-Users-demo-app", id+".jsonl"),
		body, time.Now().Add(time.Hour))
	meta := MetaReader(root)
	_, updated := meta(id)
	want := time.Date(2026, 10, 23, 11, 22, 15, 384000000, time.UTC).UnixMilli()
	if updated != want {
		t.Fatalf("updated=%d want %d (last timestamped record, not the mtime)", updated, want)
	}
}

func TestMetaReaderUnknownSession(t *testing.T) {
	meta := MetaReader(t.TempDir())
	if _, updated := meta("00000000-0000-0000-0000-000000000000"); updated != 0 {
		t.Fatalf("updated=%d want 0 for a missing transcript", updated)
	}
	if _, updated := meta("../escape"); updated != 0 {
		t.Fatalf("updated=%d want 0 for a path-like id", updated)
	}
}

func TestMetaReaderTranscriptWithoutTimestamps(t *testing.T) {
	root := t.TempDir()
	id := "11111111-1111-1111-1111-111111111111"
	writeTranscript(t, filepath.Join(root, "-Users-demo-app", id+".jsonl"),
		"{\"type\":\"cost-state\"}\n{\"type\":\"cost-state\"}\n", time.Now())
	if _, updated := MetaReader(root)(id); updated != 0 {
		t.Fatalf("updated=%d want 0 when no record carries a timestamp", updated)
	}
}

// A resume can append more metadata than the first tail window holds; the
// scan must grow past it instead of falling back to the mtime.
func TestMetaReaderGrowsPastLargeMetadataTail(t *testing.T) {
	root := t.TempDir()
	id := "22222222-2222-2222-2222-222222222222"
	var body strings.Builder
	body.WriteString("{\"type\":\"assistant\",\"timestamp\":\"2026-10-24T10:53:48.409Z\"}\n")
	for len(body.String()) < tailFirstScan+16*1024 {
		body.WriteString("{\"type\":\"cost-state\",\"usage\":{\"lines\":123456789012345678901234567890}}\n")
	}
	writeTranscript(t, filepath.Join(root, "-Users-demo-app", id+".jsonl"),
		body.String(), time.Now().Add(time.Hour))
	want := time.Date(2026, 10, 24, 10, 53, 48, 409000000, time.UTC).UnixMilli()
	if _, updated := MetaReader(root)(id); updated != want {
		t.Fatalf("updated=%d want %d (scan window must grow past the metadata tail)", updated, want)
	}
}

// A session copied or continued elsewhere leaves transcripts in several
// project directories; the newest file carries the current timeline.
func TestMetaReaderPicksNewestCopy(t *testing.T) {
	root := t.TempDir()
	id := "33333333-3333-3333-3333-333333333333"
	old := "old-project"
	newer := "new-project"
	writeTranscript(t, filepath.Join(root, old, id+".jsonl"),
		"{\"type\":\"assistant\",\"timestamp\":\"2026-10-20T08:00:00.000Z\"}\n",
		time.Now().Add(-48*time.Hour))
	writeTranscript(t, filepath.Join(root, newer, id+".jsonl"),
		"{\"type\":\"assistant\",\"timestamp\":\"2026-10-24T09:00:00.000Z\"}\n",
		time.Now())
	want := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC).UnixMilli()
	if _, updated := MetaReader(root)(id); updated != want {
		t.Fatalf("updated=%d want %d (newest copy wins)", updated, want)
	}
}

func TestProjectsRoot(t *testing.T) {
	env := func(key string) string {
		if key == "CLAUDE_CONFIG_DIR" {
			return "/custom/claude"
		}
		return ""
	}
	if got := ProjectsRoot(env, "/home/demo"); got != "/custom/claude/projects" {
		t.Fatalf("ProjectsRoot=%q want the config-dir override", got)
	}
	if got := ProjectsRoot(func(string) string { return "" }, "/home/demo"); got != "/home/demo/.claude/projects" {
		t.Fatalf("ProjectsRoot=%q want the home default", got)
	}
}
