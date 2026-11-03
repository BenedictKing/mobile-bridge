package codebuddy

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestTranscriptReaderBuildsTurnsAndIgnoresMetaOnlyCopy(t *testing.T) {
	home := t.TempDir()
	const id = "sess-history"
	path := filepath.Join(ProjectsDir(home), "Users-test-work", id+".jsonl")
	writeTranscript(t, path, `{"type":"session-meta","sessionId":"sess-history"}
{"type":"message","role":"user","content":[{"type":"input_text","text":"第一问"}],"cwd":"/work"}
{"type":"message","role":"assistant","content":[{"type":"text","text":"先扫描"},{"type":"tool_use","id":"tu1","name":"list","input":{"path":"/w"}}]}
{"type":"message","role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"3 files"}]}]}
{"type":"message","role":"assistant","content":[{"type":"text","text":"答完了"}]}
{"type":"ai-title","aiTitle":"原生标题"}
`)
	writeTranscript(t, filepath.Join(ProjectsDir(home), "Users-test-meta-only", id+".jsonl"),
		`{"type":"session-meta","sessionId":"sess-history","cwd":"/other"}`)

	read := TranscriptReader(home)
	snapshot, err := read(id)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if snapshot.Title != "原生标题" || len(snapshot.Turns) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	turn := snapshot.Turns[0]
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 4 {
		t.Fatalf("messages=%d", len(messages.Items))
	}
	encoded := string(jsoncompat.EncodeCompact(turn))
	if !strings.Contains(encoded, "3 files") || !strings.Contains(encoded, "答完了") {
		t.Fatalf("turn=%s", encoded)
	}
	if again, err := read(id); err != nil || again != nil {
		t.Fatalf("unchanged read=%v err=%v", again, err)
	}
}
