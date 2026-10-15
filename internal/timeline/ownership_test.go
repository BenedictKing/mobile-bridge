package timeline

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestResponsesOwnTheirJSONTree(t *testing.T) {
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(1))
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-1")
	turn.Set("status", "completed")
	message := jsoncompat.NewObject()
	message.Set("id", "message-1")
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", "original")
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})

	projection := New(func() string { return "epoch-1" })
	projection.Update(view)

	page := projection.Page(20, "")
	pageRow := page.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	pageRow.Set("text", "mutated response")
	page.MustGet("meta").(*jsoncompat.Object).Set("host", "mutated response")

	viewRow := view.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object).
		MustGet("messages").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	viewRow.Set("text", "mutated input")

	nextPage := projection.Page(20, "")
	nextRow := nextPage.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	if got := nextRow.MustGet("text"); got != "original" {
		t.Fatalf("response mutation leaked into projection: %v", got)
	}
	if _, exists := nextPage.MustGet("meta").(*jsoncompat.Object).Get("host"); exists {
		t.Fatal("response metadata mutation leaked into projection")
	}

	if got := projection.MetaSnapshot().MustGet("host"); got != nil {
		t.Fatalf("unexpected host metadata: %v", got)
	}
}

func TestMetaSnapshotCannotMutateProjection(t *testing.T) {
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(1))
	view.Set("goal", jsoncompat.NewObject())
	projection := New(func() string { return "epoch-1" })
	projection.Update(view)

	meta := projection.MetaSnapshot()
	meta.Set("goal", "changed")
	meta.Set("newField", true)

	stored := projection.MetaSnapshot()
	if _, exists := stored.Get("newField"); exists {
		t.Fatal("metadata snapshot mutation leaked into projection")
	}
	if _, ok := stored.MustGet("goal").(*jsoncompat.Object); !ok {
		t.Fatalf("stored goal was changed through metadata snapshot: %T", stored.MustGet("goal"))
	}
}
