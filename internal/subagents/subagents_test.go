package subagents

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestZcodeStoreRosterAndHistory(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "v2", "acp-lazy-sessions.json"),
		`{"ph-1": {"cwd": "/w", "zcodeSid": "sess-back-1"}}`)
	agentsDir := filepath.Join(root, "cli", "agents", "sess-back-1")
	write(t, filepath.Join(agentsDir, "agent_a", "metadata.json"), `{
  "childSessionId": "child-1", "description": "检索代码", "profileId": "Explore",
  "status": "completed", "updatedAt": 1791900000000, "prompt": "找出所有引用"
}`)
	write(t, filepath.Join(agentsDir, "agent_a", "transcript.jsonl"), `
{"type":"turn_started","payload":{"input":"找出所有引用"}}
{"type":"tool_call_scheduled","payload":{"toolCallId":"t1","toolName":"grep","input":{"q":"foo"}}}
{"type":"model_complete","payload":{"content":"找到 3 处"}}
{"type":"turn_complete","payload":{"resultType":"success","response":"共 3 处引用"}}
`)
	store := NewZcodeStore(root)
	// An unmapped placeholder id yields nothing.
	if children := store.Children("unmapped-placeholder"); len(children) != 0 {
		t.Fatalf("unmapped children=%v", children)
	}
	children := store.Children("ph-1")
	if len(children) != 1 || children[0].ID != "child-1" || children[0].AgentRole != "Explore" {
		t.Fatalf("children=%+v", children)
	}
	history := store.History("child-1")
	turns, _ := history.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 {
		t.Fatalf("history=%s", jsoncompat.EncodeCompact(history))
	}
	turn := turns.Items[0].(*jsoncompat.Object)
	messages := turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 4 {
		t.Fatalf("messages=%s", jsoncompat.EncodeCompact(turn))
	}
	if kind := messages.Items[1].(*jsoncompat.Object).MustGet("kind"); kind != "toolCall" {
		t.Fatalf("second message kind=%v", kind)
	}
	// Flatten + Inject round out the pipeline.
	activity := Flatten(store, "ph-1")
	if activity.Revision != 1791900000000 || len(activity.Rows) != 1 {
		t.Fatalf("activity=%+v", activity)
	}
	row := activity.Rows[0]
	if row.SubagentID != "child-1" || row.SubagentType != "Explore" || row.Status != "completed" {
		t.Fatalf("row=%+v", row)
	}
	if row.Text == "" || !contains(row.Text, "[Agent]") || !contains(row.Text, "共 3 处引用") {
		t.Fatalf("row text=%q", row.Text)
	}
	view := jsoncompat.NewObject()
	view.Set("turns", &jsoncompat.Array{})
	Inject(view, activity)
	if view.MustGet("subagentRevision") != jsoncompat.Num(1791900000000) {
		t.Fatalf("revision=%v", view.MustGet("subagentRevision"))
	}
	first := view.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	if len(first.MustGet("messages").(*jsoncompat.Array).Items) != 1 {
		t.Fatal("subagent row not injected")
	}
	// A second inject with the same activity does not duplicate the row.
	Inject(view, activity)
	if len(first.MustGet("messages").(*jsoncompat.Array).Items) != 1 {
		t.Fatal("subagent row duplicated")
	}
}

func TestClaudeStoreRosterAndHistory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "slug-1", "parent-9", "subagents")
	write(t, filepath.Join(dir, "agent-alpha-1.jsonl"), `
{"type":"user","timestamp":"2026-10-14T01:00:00Z","message":{"role":"user","content":"扫描目录"},"agentId":"alpha"}
{"type":"assistant","timestamp":"2026-10-14T01:00:05Z","message":{"role":"assistant","content":[{"type":"text","text":"已扫描"},{"type":"tool_use","id":"tu1","name":"list","input":{"path":"/w"}}]}}
{"type":"user","timestamp":"2026-10-14T01:00:06Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":[{"type":"text","text":"3 files"}]}]}}
`)
	store := NewClaudeStore(root)
	children := store.Children("parent-9")
	if len(children) != 1 || children[0].Name != "alpha" || children[0].AgentRole != "alpha" {
		t.Fatalf("children=%+v", children)
	}
	history := store.History(children[0].ID)
	compact := string(jsoncompat.EncodeCompact(history))
	if !contains(compact, "已扫描") || !contains(compact, "3 files") {
		t.Fatalf("history=%s", compact)
	}
	// The tool result lands on the tool_use item.
	turns := history.MustGet("turns").(*jsoncompat.Array)
	messages := turns.Items[0].(*jsoncompat.Object).MustGet("messages").(*jsoncompat.Array)
	found := false
	for _, item := range messages.Items {
		message := item.(*jsoncompat.Object)
		if message.MustGet("kind") == "toolCall" {
			found = contains(jsoncompat.PyStr(message.MustGet("output")), "3 files")
		}
	}
	if !found {
		t.Fatalf("tool output missing: %s", compact)
	}
}

func TestKimiStoreRosterAndHistory(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "session_index.jsonl"),
		`{"sessionId":"k-parent","sessionDir":"`+filepath.Join(root, "s1")+`"}`+"\n")
	write(t, filepath.Join(root, "s1", "state.json"), `{
  "updatedAt": 1791900000000, "lastPrompt": "整理结果",
  "agents": {"agent-0": {"type": "sub"}, "main": {"type": "main"}}
}`)
	write(t, filepath.Join(root, "s1", "agents", "main", "wire.jsonl"), `
{"type":"task.started","info":{"kind":"agent","agentId":"agent-0","subagentType":"Explore"}}
{"type":"task.terminated","info":{"kind":"agent","agentId":"agent-0","status":"completed"}}
`)
	write(t, filepath.Join(root, "s1", "agents", "agent-0", "wire.jsonl"), `
{"type":"turn.prompt","input":"整理结果"}
{"type":"context.append_loop_event","event":{"type":"tool.call","toolCallId":"c1","name":"read","args":{"file":"a"}}}
{"type":"context.append_loop_event","event":{"type":"tool.result","toolCallId":"c1","result":"内容 A"}}
{"type":"turn.ended","reason":"completed"}
`)
	store := NewKimiStore(root)
	children := store.Children("k-parent")
	if len(children) != 1 {
		t.Fatalf("children=%+v", children)
	}
	child := children[0]
	if child.ID != "k-parent/agent-0" || child.AgentRole != "Explore" || child.SpawnStatus != "completed" {
		t.Fatalf("child=%+v", child)
	}
	history := store.History(child.ID)
	compact := string(jsoncompat.EncodeCompact(history))
	if !contains(compact, "整理结果") || !contains(compact, "内容 A") {
		t.Fatalf("history=%s", compact)
	}
}

func TestFlattenerCachesByRevision(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "v2", "acp-lazy-sessions.json"),
		`{"p": {"zcodeSid": "sess-x"}}`)
	write(t, filepath.Join(root, "cli", "agents", "sess-x", "agent_a", "metadata.json"),
		`{"childSessionId":"c1","description":"d","updatedAt":5,"prompt":"q"}`)
	write(t, filepath.Join(root, "cli", "agents", "sess-x", "agent_a", "transcript.jsonl"),
		`{"type":"turn_complete","payload":{"resultType":"success","response":"done"}}`)
	flattener := NewFlattener(NewZcodeStore(root))
	first := flattener.Activity("p")
	second := flattener.Activity("p")
	if first.Revision != 5 || len(first.Rows) != 1 || second.Rows[0].ID != first.Rows[0].ID {
		t.Fatalf("activities=%+v %+v", first, second)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
