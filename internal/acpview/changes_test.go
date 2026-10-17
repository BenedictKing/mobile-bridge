package acpview

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func changesOf(message *jsoncompat.Object) []*jsoncompat.Object {
	list, _ := message.MustGet("changes").(*jsoncompat.Array)
	items := []*jsoncompat.Object{}
	if list == nil {
		return items
	}
	for _, item := range list.Items {
		if change, ok := item.(*jsoncompat.Object); ok && change != nil {
			items = append(items, change)
		}
	}
	return items
}

// ACP diff content blocks fold onto the tool row as structured change
// entries; the raw block JSON must not leak into the flattened output text.
func TestToolCallDiffBlocks(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	view.BeginTurn("改文件")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"编辑","kind":"edit",
		  "rawInput":{"command":"edit"},
		  "content":[{"type":"diff","path":"/work/a.go","oldText":"old\n","newText":"new\n"},
		             {"type":"text","text":"done"}]}`))
	message := snapshotMessage(t, view, "toolCall")
	changes := changesOf(message)
	if len(changes) != 1 {
		t.Fatalf("changes=%v", changes)
	}
	if changes[0].MustGet("path") != "/work/a.go" ||
		changes[0].MustGet("oldText") != "old\n" || changes[0].MustGet("newText") != "new\n" {
		t.Fatalf("change=%v", changes[0])
	}
	if output, _ := message.MustGet("output").(string); strings.Contains(output, "oldText") {
		t.Fatalf("diff block leaked into output: %q", output)
	}
	if output, _ := message.MustGet("output").(string); output != "done" {
		t.Fatalf("output=%q", output)
	}
}

// tool_call_update frames refine the same file: one entry per path, latest
// text wins. A second file gets its own entry.
func TestToolCallUpdateDedupesByPath(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	view.BeginTurn("改文件")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"编辑","content":[
		  {"type":"diff","path":"/work/a.go","oldText":"v1","newText":"v2"}]}`))
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[
		  {"type":"diff","path":"/work/b.go","oldText":"","newText":"brand new"},
		  {"type":"diff","path":"/work/a.go","oldText":"v1","newText":"v3"}]}`))
	message := snapshotMessage(t, view, "toolCall")
	changes := changesOf(message)
	if len(changes) != 2 {
		t.Fatalf("changes=%d want 2: %v", len(changes), changes)
	}
	// First-seen position, latest text.
	if changes[0].MustGet("path") != "/work/a.go" || changes[0].MustGet("newText") != "v3" {
		t.Fatalf("first change=%v", changes[0])
	}
	if changes[1].MustGet("path") != "/work/b.go" {
		t.Fatalf("second change=%v", changes[1])
	}
}

// Edit-style rawInput (file_path/old_string/new_string) fills in when the
// content array carries no diff blocks — and never duplicates a content diff.
func TestToolCallRawInputFallback(t *testing.T) {
	view := NewView("sess-1", nil, "/work", func() float64 { return 0 })
	view.BeginTurn("编辑")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-1","title":"编辑",
		  "rawInput":{"file_path":"/work/b.js","old_string":"a","new_string":"b"},
		  "content":[{"type":"text","text":"ok"}]}`))
	message := snapshotMessage(t, view, "toolCall")
	changes := changesOf(message)
	if len(changes) != 1 || changes[0].MustGet("path") != "/work/b.js" ||
		changes[0].MustGet("oldText") != "a" || changes[0].MustGet("newText") != "b" {
		t.Fatalf("changes=%v", changes)
	}

	// Content diffs take priority: no duplicate entry for the same call.
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-2","title":"编辑",
		  "rawInput":{"file_path":"/work/c.py","old_string":"x","new_string":"y"},
		  "content":[{"type":"diff","path":"/work/c.py","oldText":"x","newText":"y"}]}`))
	message = snapshotMessage(t, view, "toolCall")
	if changes = changesOf(message); len(changes) != 1 {
		t.Fatalf("content+rawInput changes=%v", changes)
	}

	// Non-edit rawInput contributes nothing.
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"tool_call","toolCallId":"call-3","title":"读文件",
		  "rawInput":{"file_path":"/work/d.py"},"content":[]}`))
	message = snapshotMessage(t, view, "toolCall")
	if changes = changesOf(message); len(changes) != 0 {
		t.Fatalf("plain read changes=%v", changes)
	}
}
