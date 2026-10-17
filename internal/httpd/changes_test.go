package httpd

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

func parseObject(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		t.Fatalf("not an object: %T", value)
	}
	return object
}

func viewMessage(view *jsoncompat.Object, id string) *jsoncompat.Object {
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	for _, turnValue := range turns.Items {
		turn, _ := turnValue.(*jsoncompat.Object)
		if turn == nil {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		for _, messageValue := range messages.Items {
			message, _ := messageValue.(*jsoncompat.Object)
			if message == nil {
				continue
			}
			if messageID, _ := message.MustGet("id").(string); messageID == id {
				return message
			}
		}
	}
	return nil
}

// The annotation mirrors raw codex items onto the normalized view: diff text
// stays structured on the fileChange message, exitCode survives on the
// commandExecution message, and normalization's folded text is untouched.
func TestAnnotateViewChanges(t *testing.T) {
	state := parseObject(t, `{
	 "id": "thread-1", "cwd": "/work",
	 "turns": [{
	  "turnId": "t1", "status": "completed", "items": [
	   {"id": "u1", "type": "userMessage", "content": [{"type": "text", "text": "跑测试"}]},
	   {"id": "i2", "type": "commandExecution", "command": "go test ./...", "aggregatedOutput": "ok", "exitCode": 0},
	   {"id": "i3", "type": "fileChange", "changes": [
	    {"path": "/work/a.go", "diff": "--- a.go\n+++ b.go\n@@ -1 +1 @@\n-old\n+new"},
	    {"path": "/work/b.js", "diff": "short"}]},
	   {"id": "i4", "type": "fileChange", "changes": [{"path": "", "diff": "no path"}]},
	   {"id": "i5", "type": "commandExecution", "command": "bad", "exitCode": 2}
	  ]
	 }]
	}`)
	view := model.NormalizeState(state, true)
	annotateViewChanges(view, model.OrderedTurns(state))

	changed := viewMessage(view, "i3")
	if changed == nil {
		t.Fatal("fileChange message missing")
	}
	if text, _ := changed.MustGet("text").(string); !strings.Contains(text, "+new") {
		t.Fatalf("normalized text must stay folded: %q", text)
	}
	changes, _ := changed.MustGet("changes").(*jsoncompat.Array)
	if changes == nil || len(changes.Items) != 2 {
		t.Fatalf("changes=%v", changes)
	}
	first, _ := changes.Items[0].(*jsoncompat.Object)
	if first.MustGet("path") != "/work/a.go" {
		t.Fatalf("first change=%v", first)
	}
	if diff, _ := first.MustGet("diff").(string); !strings.Contains(diff, "+new") {
		t.Fatalf("diff=%q", diff)
	}

	command := viewMessage(view, "i2")
	if command == nil {
		t.Fatal("commandExecution message missing")
	}
	if exitCode := command.MustGet("exitCode"); exitCode == nil {
		t.Fatal("exitCode 0 must be preserved on the view")
	}

	failed := viewMessage(view, "i5")
	if failed == nil {
		t.Fatal("failed command message missing")
	}
	if value := failed.MustGet("exitCode"); value == nil {
		t.Fatal("non-zero exitCode missing")
	}

	// Path-less change lists annotate nothing.
	empty := viewMessage(view, "i4")
	if empty != nil {
		if changes, _ := empty.MustGet("changes").(*jsoncompat.Array); changes != nil {
			t.Fatalf("path-less change annotated: %v", changes)
		}
	}
}

// Bounds: at most 20 entries per item and 16000 chars per diff, with a
// truncated marker on clipped diffs.
func TestAnnotateViewChangesBounds(t *testing.T) {
	items := ""
	for index := 0; index < 25; index++ {
		items += `{"path": "/work/f` + strings.Repeat("x", index) + `.go", "diff": "d"}`
		if index < 24 {
			items += ","
		}
	}
	long := strings.Repeat("行", 17000)
	state := parseObject(t, `{
	 "id": "thread-1", "cwd": "/work",
	 "turns": [{"turnId": "t1", "status": "completed", "items": [
	  {"id": "many", "type": "fileChange", "changes": [`+items+`]},
	  {"id": "long", "type": "fileChange", "changes": [{"path": "/work/big.txt", "diff": "`+long+`"}]}
	 ]}]
	}`)
	view := model.NormalizeState(state, true)
	annotateViewChanges(view, model.OrderedTurns(state))

	many := viewMessage(view, "many")
	changes, _ := many.MustGet("changes").(*jsoncompat.Array)
	if len(changes.Items) != changesMaxFiles {
		t.Fatalf("changes=%d want %d", len(changes.Items), changesMaxFiles)
	}
	longMessage := viewMessage(view, "long")
	longChanges, _ := longMessage.MustGet("changes").(*jsoncompat.Array)
	entry, _ := longChanges.Items[0].(*jsoncompat.Object)
	if diff, _ := entry.MustGet("diff").(string); len([]rune(diff)) != changesMaxChars {
		t.Fatalf("diff length=%d want %d", len([]rune(diff)), changesMaxChars)
	}
	if entry.MustGet("truncated") != true {
		t.Fatal("truncated marker missing")
	}

	// The raw state (shared with the follower snapshot) stays untouched.
	rawItems, _ := state.MustGet("turns").(*jsoncompat.Array)
	turn, _ := rawItems.Items[0].(*jsoncompat.Object)
	itemList, _ := turn.MustGet("items").(*jsoncompat.Array)
	first, _ := itemList.Items[1].(*jsoncompat.Object)
	if _, exists := first.Get("changes"); !exists {
		t.Fatal("raw item lost its changes field")
	}
}

// buildHistoryView runs the same annotation; today's saved rollouts carry no
// structured items, so it must be a no-op that still yields rows.
func TestBuildHistoryViewAnnotationNoop(t *testing.T) {
	history := parseObject(t, `{
	 "id": "thread-1", "title": "标题", "cwd": "/work",
	 "turnsPagination": {"hasLoadedOldest": true},
	 "turns": [{"turnId": "history", "status": "completed", "items": [
	  {"id": "0", "type": "agentMessage", "text": "完成"}
	 ]}]
	}`)
	view := buildHistoryView(history)
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 {
		t.Fatalf("turns=%d", len(turns.Items))
	}
	turn, _ := turns.Items[0].(*jsoncompat.Object)
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	for _, messageValue := range messages.Items {
		message, _ := messageValue.(*jsoncompat.Object)
		if message == nil {
			continue
		}
		if _, exists := message.Get("changes"); exists {
			t.Fatalf("unexpected changes on %v", message)
		}
	}
}
