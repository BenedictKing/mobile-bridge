package kimi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestTranscriptReaderBuildsTurnsFromMainWire(t *testing.T) {
	home := t.TempDir()
	const id = "sess-history"
	dir := filepath.Join(home, "sessions", "workspace", id)
	wire := filepath.Join(dir, "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wire), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"title":"原生标题","cwd":"/work"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"turn.prompt","input":[{"type":"text","text":"第一问"}],"origin":{"kind":"user"}}
{"type":"turn.prompt","input":"内部继续","origin":{"kind":"system_trigger"}}
{"type":"context.append_message","message":{"role":"assistant","content":[{"type":"think","think":"想一想"},{"type":"text","text":"先扫描"}],"toolCalls":[{"id":"c1","name":"read","args":{"file":"a"}}]}}
{"type":"context.append_loop_event","event":{"type":"tool.result","toolCallId":"c1","result":"内容 A"}}
{"type":"context.append_message","message":{"role":"assistant","content":[{"type":"text","text":"答完了"}]}}
{"type":"turn.ended","reason":"completed"}
`
	if err := os.WriteFile(wire, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	read := TranscriptReader(home)
	snapshot, err := read(id)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if snapshot.Title != "原生标题" || len(snapshot.Turns) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	turn := snapshot.Turns[0]
	if status, _ := turn.MustGet("status").(string); status != "completed" {
		t.Fatalf("status=%q", status)
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 5 {
		t.Fatalf("messages=%d", len(messages.Items))
	}
	encoded := string(jsoncompat.EncodeCompact(turn))
	if !strings.Contains(encoded, "内容 A") || strings.Contains(encoded, "内部继续") {
		t.Fatalf("turn=%s", encoded)
	}
	if again, err := read(id); err != nil || again != nil {
		t.Fatalf("unchanged read=%v err=%v", again, err)
	}
}
