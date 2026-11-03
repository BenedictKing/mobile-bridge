package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestTranscriptReaderBuildsNativeTurnsAndList(t *testing.T) {
	root := t.TempDir()
	const id = "session-1"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"summary","summary":"历史标题"}
{"type":"user","cwd":"/work","message":{"role":"user","content":"第一问"}}
{"type":"assistant","message":{"role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"想一想"},{"type":"text","text":"先扫描"},{"type":"tool_use","id":"tu1","name":"list","input":{"path":"/w"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"3 files"}]}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"答完了"}]}}
{"type":"user","isSidechain":true,"message":{"role":"user","content":"子会话不能混入"}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	read := TranscriptReader(root)
	snapshot, err := read(id)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if snapshot.Title != "历史标题" || snapshot.Model != "claude-test" || len(snapshot.Turns) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	turn := snapshot.Turns[0]
	if status, _ := turn.MustGet("status").(string); status != "completed" {
		t.Fatalf("status=%q", status)
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 5 {
		t.Fatalf("messages=%d, want user/thought/text/tool/text", len(messages.Items))
	}
	if encoded := string(jsoncompat.EncodeCompact(turn)); !strings.Contains(encoded, "3 files") || strings.Contains(encoded, "子会话") {
		t.Fatalf("turn=%s", encoded)
	}
	if again, err := read(id); err != nil || again != nil {
		t.Fatalf("unchanged read=%v err=%v", again, err)
	}
	rows := ListSessions(root)
	if len(rows) != 1 || rows[0].MustGet("cwd") != "/work" || rows[0].MustGet("title") != "历史标题" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestTranscriptReaderSettlesInterruptedTail(t *testing.T) {
	root := t.TempDir()
	const id = "session-interrupted"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A turn is awaiting a tool result when the user interrupts and exits:
	// Claude Code appends the literal interrupt echo and nothing after it.
	content := `{"type":"user","cwd":"/work","message":{"role":"user","content":"继续"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"sleep 99"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	read := TranscriptReader(root)
	snapshot, err := read(id)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if len(snapshot.Turns) != 2 {
		t.Fatalf("turns=%d", len(snapshot.Turns))
	}
	for index, turn := range snapshot.Turns {
		if status, _ := turn.MustGet("status").(string); status != "interrupted" {
			t.Fatalf("turn %d status=%q, want interrupted", index, status)
		}
	}
	if nativeHistoryActive(snapshot) {
		t.Fatal("interrupted tail must not read as active")
	}
	marker := snapshot.Turns[1].MustGet("messages").(*jsoncompat.Array)
	if len(marker.Items) != 1 || marker.Items[0].(*jsoncompat.Object).MustGet("text") != "[Request interrupted by user]" {
		t.Fatalf("marker turn=%v", marker)
	}
}
