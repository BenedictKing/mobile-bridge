package store

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func historyTurnItems(t *testing.T, lines []string) *jsoncompat.Array {
	t.Helper()
	history := History("thread-id", sampleChangeMeta(), linesToRollout(lines), nil)
	turns, ok := history.MustGet("turns").(*jsoncompat.Array)
	if !ok || len(turns.Items) == 0 {
		t.Fatalf("no turns: %s", jsoncompat.EncodeCompact(history))
	}
	turn, _ := turns.Items[0].(*jsoncompat.Object)
	items, _ := turn.MustGet("items").(*jsoncompat.Array)
	return items
}

func sampleChangeMeta() *jsoncompat.Object {
	meta := jsoncompat.NewObject()
	meta.Set("name", "会话")
	meta.Set("cwd", "/work")
	meta.Set("model", "gpt-x")
	meta.Set("model_provider", "oai")
	return meta
}

func linesToRollout(lines []string) []byte {
	var raw []byte
	for _, line := range lines {
		raw = append(raw, line...)
		raw = append(raw, '\n')
	}
	return raw
}

// Real rollout shapes: FileChange keyed by path with unified_diff/content
// entries, CommandExecution with an argv array and snake_case results.
func TestHistoryParsesChangeItems(t *testing.T) {
	items := historyTurnItems(t, []string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"exec-1","command":["/bin/zsh","-lc","go test ./..."],"status":"completed","aggregated_output":"ok","exit_code":0}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","id":"exec-2","status":"completed","changes":{"/work/a.go":{"type":"update","unified_diff":"@@ -1 +1 @@\n-old\n+new","move_path":null},"/work/new.txt":{"type":"add","content":"全文"}}}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","summary":[]}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	})
	if len(items.Items) != 2 {
		t.Fatalf("items=%s", jsoncompat.EncodeCompact(items))
	}

	command, _ := items.Items[0].(*jsoncompat.Object)
	if command.MustGet("id") != "exec-1" || command.MustGet("type") != "commandExecution" {
		t.Fatalf("command row=%s", jsoncompat.EncodeCompact(command))
	}
	if command.MustGet("command") != "/bin/zsh -lc go test ./..." {
		t.Fatalf("command=%v", command.MustGet("command"))
	}
	if command.MustGet("aggregatedOutput") != "ok" {
		t.Fatalf("aggregatedOutput=%v", command.MustGet("aggregatedOutput"))
	}
	// exit_code keeps its numeric JSON type.
	if code, ok := command.MustGet("exitCode").(json.Number); !ok || code.String() != "0" {
		t.Fatalf("exitCode=%T %v", command.MustGet("exitCode"), command.MustGet("exitCode"))
	}

	change, _ := items.Items[1].(*jsoncompat.Object)
	if change.MustGet("id") != "exec-2" || change.MustGet("type") != "fileChange" {
		t.Fatalf("change row=%s", jsoncompat.EncodeCompact(change))
	}
	entries, _ := change.MustGet("changes").(*jsoncompat.Array)
	if len(entries.Items) != 2 {
		t.Fatalf("entries=%s", jsoncompat.EncodeCompact(entries))
	}
	update, _ := entries.Items[0].(*jsoncompat.Object)
	if update.MustGet("path") != "/work/a.go" {
		t.Fatalf("update=%s", jsoncompat.EncodeCompact(update))
	}
	if diff, _ := update.MustGet("diff").(string); !strings.Contains(diff, "+new") {
		t.Fatalf("diff=%q", diff)
	}
	added, _ := entries.Items[1].(*jsoncompat.Object)
	if added.MustGet("path") != "/work/new.txt" || added.MustGet("newText") != "全文" {
		t.Fatalf("add entry=%s", jsoncompat.EncodeCompact(added))
	}
	if _, exists := added.Get("diff"); exists {
		t.Fatal("add entry must not fake a diff")
	}
}

// Degenerate payloads: no id (positional fallback), a list-shaped changes
// field, a string command, and a command without exit_code — mirroring the
// older rollout layouts.
func TestHistoryChangeItemFallbacks(t *testing.T) {
	items := historyTurnItems(t, []string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"commandExecution","command":"ls"}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","changes":[{"path":"/work/b.go","diff":"d"}]}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"FileChange","changes":{"":{"type":"delete"}}}}}`,
	})
	if len(items.Items) != 2 {
		t.Fatalf("items=%s", jsoncompat.EncodeCompact(items))
	}
	command, _ := items.Items[0].(*jsoncompat.Object)
	if command.MustGet("id") != "0" || command.MustGet("command") != "ls" {
		t.Fatalf("command row=%s", jsoncompat.EncodeCompact(command))
	}
	if _, exists := command.Get("exitCode"); exists {
		t.Fatal("missing exit_code must stay absent")
	}
	change, _ := items.Items[1].(*jsoncompat.Object)
	if change.MustGet("id") != "1" {
		t.Fatalf("change row=%s", jsoncompat.EncodeCompact(change))
	}
	entries, _ := change.MustGet("changes").(*jsoncompat.Array)
	if len(entries.Items) != 1 {
		t.Fatalf("path-less entry must be dropped: %s", jsoncompat.EncodeCompact(entries))
	}
}
