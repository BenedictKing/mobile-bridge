package acpview

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func nativeTestTurn(id, status, text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", status)
	message := jsoncompat.NewObject()
	message.Set("id", "u-"+id)
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", text)
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	turn.Set("actionable", false)
	return turn
}

func TestApplyNativeReplacesTurnsAndTitle(t *testing.T) {
	view := NewView("sess-1", nil, "/w", func() float64 { return 0 })
	view.BeginTurn("旧问题")
	if view.Status != "active" {
		t.Fatalf("begin status = %q", view.Status)
	}
	snapshot := &NativeSnapshot{Title: "真实标题", Turns: []*jsoncompat.Object{
		nativeTestTurn("t1", "completed", "第一问"),
		nativeTestTurn("t2", "inProgress", "第二问"),
	}}
	if !view.ApplyNative(snapshot) {
		t.Fatal("apply returned false")
	}
	if view.Title != "真实标题" {
		t.Fatalf("title = %v", view.Title)
	}
	if len(view.Turns) != 2 {
		t.Fatalf("turns = %d", len(view.Turns))
	}
	if view.Status != "active" {
		t.Fatalf("in-progress tip status = %q, want active", view.Status)
	}
	snapshot.Turns[1] = nativeTestTurn("t2", "completed", "第二问")
	if !view.ApplyNative(snapshot) || view.Status != "idle" {
		t.Fatalf("settled tip status = %q, want idle", view.Status)
	}
	if tip := view.TipTurn(); tip == nil || tip.MustGet("id") != "t2" {
		t.Fatalf("tip = %v", tip)
	}
}

func TestApplyNativeEmptyIsNoop(t *testing.T) {
	view := NewView("sess-1", nil, "/w", func() float64 { return 0 })
	view.BeginTurn("保留")
	if view.ApplyNative(&NativeSnapshot{Title: "别的"}) {
		t.Fatal("empty snapshot applied")
	}
	if view.Title != nil || view.Status != "active" {
		t.Fatalf("view mutated: title=%v status=%q", view.Title, view.Status)
	}
}
