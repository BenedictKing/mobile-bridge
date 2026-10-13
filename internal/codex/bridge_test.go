package codex

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestBridgeAppliesSnapshotAndPatches(t *testing.T) {
	bridge := New(t.TempDir(), "")
	state := jsoncompat.NewObject()
	state.Set("id", "thread-1")
	state.Set("title", "first")
	snapshot := jsoncompat.NewObject()
	snapshot.Set("type", "snapshot")
	snapshot.Set("revision", jsoncompat.Num(1))
	snapshot.Set("conversationState", state)
	bridge.handleEvent(event("thread-1", snapshot, "owner"))
	view, ok := bridge.View("thread-1")
	if !ok || view.MustGet("title") != "first" {
		t.Fatalf("snapshot was not applied: %v", view)
	}

	patch := jsoncompat.NewObject()
	patch.Set("type", "patches")
	patch.Set("baseRevision", jsoncompat.Num(1))
	patch.Set("revision", jsoncompat.Num(2))
	operation := jsoncompat.NewObject()
	operation.Set("op", "replace")
	operation.Set("path", &jsoncompat.Array{Items: []any{"title"}})
	operation.Set("value", "second")
	patch.Set("patches", &jsoncompat.Array{Items: []any{operation}})
	bridge.handleEvent(event("thread-1", patch, "owner"))
	view, ok = bridge.View("thread-1")
	if !ok || view.MustGet("title") != "second" {
		t.Fatalf("patch was not applied: %v", view)
	}
}

func event(id string, change *jsoncompat.Object, owner string) *jsoncompat.Object {
	params := jsoncompat.NewObject()
	params.Set("hostId", "local")
	params.Set("conversationId", id)
	params.Set("change", change)
	message := jsoncompat.NewObject()
	message.Set("method", "thread-stream-state-changed")
	message.Set("version", jsoncompat.Num(11))
	message.Set("sourceClientId", owner)
	message.Set("params", params)
	return message
}
