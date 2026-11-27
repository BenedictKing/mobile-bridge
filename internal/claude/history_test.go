package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestTranscriptReaderSettlesLocalCommandOutputTail(t *testing.T) {
	root := t.TempDir()
	const id = "session-local-command"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A native /compact ends with its stdout echo; a later harness write (an
	// orphaned background-task notice on resume) keeps the file fresh.
	content := `{"type":"user","cwd":"/work","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-message>compact</command-message>\n<command-args></command-args>"}}
{"type":"user","message":{"role":"user","content":"<local-command-stdout>Compacted PostCompact [callback] completed successfully</local-command-stdout>"}}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := TranscriptReader(root)(id)
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if nativeHistoryActive(snapshot) {
		t.Fatal("local command output must not read as an active turn")
	}
	tip := snapshot.Turns[len(snapshot.Turns)-1]
	if status, _ := tip.MustGet("status").(string); status != "completed" {
		t.Fatalf("tip status=%q, want completed", status)
	}
}

func TestTranscriptReaderProjectsBackgroundTaskLifecycle(t *testing.T) {
	root := t.TempDir()
	const id = "session-background-task"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{
		{"type": "user", "uuid": "u1", "parentUuid": "root", "cwd": "/work", "message": map[string]any{"role": "user", "content": "跑完整门禁"}},
		{"type": "assistant", "uuid": "a1", "parentUuid": "u1", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "bash-use-1", "name": "Bash", "input": map[string]any{"command": "GOFLAGS=-p=2 make check"}}}}},
		{"type": "user", "uuid": "u2", "parentUuid": "a1", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "bash-use-1", "content": []any{map[string]any{"type": "text", "text": "Command running in background with ID: task-123"}}}}}},
		{"type": "user", "uuid": "u3", "parentUuid": "u2", "origin": map[string]any{"kind": "task-notification", "producer": "session-task", "runId": "run-1"}, "message": map[string]any{"role": "user", "content": "<task-notification>\n<task-id>task-123</task-id>\n<tool-use-id>bash-use-1</tool-use-id>\n<output-file>/private/tmp/private-output</output-file>\n<status>completed</status>\n<summary>Background command completed (exit code 0)</summary>\n</task-notification>"}},
	}
	lines := make([]string, 0, len(events))
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(strings.Join(lines[:3], "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	running, _, err := readHistory(path, false, "")
	if err != nil || running == nil {
		t.Fatalf("running snapshot=%v err=%v", running, err)
	}
	tool := running.Turns[0].MustGet("messages").(*jsoncompat.Array).Items[1].(*jsoncompat.Object)
	if tool.MustGet("backgroundTask") != true || tool.MustGet("status") != "inProgress" {
		t.Fatalf("background launch not visible: %s", jsoncompat.EncodeCompact(tool))
	}
	stale, _, err := readHistory(path, true, "")
	if err != nil || stale == nil {
		t.Fatalf("stale snapshot=%v err=%v", stale, err)
	}
	staleTool := stale.Turns[0].MustGet("messages").(*jsoncompat.Array).Items[1].(*jsoncompat.Object)
	if staleTool.MustGet("status") != "outcome_unknown" {
		t.Fatalf("missing completion must become unknown: %s", jsoncompat.EncodeCompact(staleTool))
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := readHistory(path, false, "")
	if err != nil || snapshot == nil || len(snapshot.Turns) != 1 {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	messages := snapshot.Turns[0].MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 2 {
		t.Fatalf("want original prompt and updated Bash row: %s", jsoncompat.EncodeCompact(messages))
	}
	status := messages.Items[1].(*jsoncompat.Object)
	if status.MustGet("role") != "activity" || status.MustGet("backgroundTask") != true || status.MustGet("status") != "completed" {
		t.Fatalf("background task completed projection=%s", jsoncompat.EncodeCompact(status))
	}
	text, _ := status.MustGet("backgroundTaskSummary").(string)
	if !strings.Contains(text, "exit code 0") || strings.Contains(text, "/private/tmp") {
		t.Fatalf("unsafe or incomplete summary %q", text)
	}
	projected := string(jsoncompat.EncodeCompact(snapshot.Turns[0]))
	if strings.Contains(projected, "<task-notification>") || strings.Contains(projected, "task-123") {
		t.Fatalf("raw notification leaked into conversation: %s", projected)
	}
}

func TestTranscriptReaderSettlesStaleTailAfterSixMinutes(t *testing.T) {
	root := t.TempDir()
	const id = "session-stale"
	path := filepath.Join(root, "project", id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","cwd":"/work","message":{"role":"user","content":"继续实现"}}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-6*time.Minute - time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	read := TranscriptReader(root)
	active, err := read(id)
	if err != nil || active == nil || !nativeHistoryActive(active) {
		t.Fatalf("first transcript read should retain the active tail: snapshot=%v err=%v", active, err)
	}
	settled, err := read(id)
	if err != nil || settled == nil || nativeHistoryActive(settled) {
		t.Fatalf("unchanged transcript older than six minutes should settle: snapshot=%v err=%v", settled, err)
	}
	if status, _ := settled.Turns[len(settled.Turns)-1].MustGet("status").(string); status != "interrupted" {
		t.Fatalf("stale tail status=%q, want interrupted", status)
	}
}
