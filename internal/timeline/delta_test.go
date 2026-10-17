package timeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// deltaView builds a one-turn view whose assistant messages carry the given
// texts; extra fields shape individual scenarios.
func deltaView(sequence int, texts ...string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-1")
	turn.Set("status", "inProgress")
	messages := &jsoncompat.Array{}
	for _, text := range texts {
		message := jsoncompat.NewObject()
		message.Set("id", "m-"+strings.TrimSpace(text))
		message.Set("kind", "agentMessage")
		message.Set("role", "assistant")
		message.Set("text", text)
		messages.Items = append(messages.Items, message)
	}
	turn.Set("messages", messages)
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("status", "active")
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})
	return view
}

func updateTextAt(view *jsoncompat.Object, index int, text string) *jsoncompat.Object {
	next := jsoncompat.Clone(view).(*jsoncompat.Object)
	message := next.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object).
		MustGet("messages").(*jsoncompat.Array).Items[index].(*jsoncompat.Object)
	message.Set("text", text)
	return next
}

// firstKey returns the projection's first row key.
func firstKey(t *testing.T, instance *Timeline) string {
	t.Helper()
	page := instance.Page(20, "")
	row, _ := page.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	key, _ := row.MustGet("key").(string)
	return key
}

func TestDeltasPureAppend(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	instance.Update(updateTextAt(deltaView(2, "你好"), 0, "你好，世界"))

	delta, ok := instance.Deltas("1")
	if !ok {
		t.Fatal("pure append must produce a delta")
	}
	if got := delta.MustGet("epoch"); got != "epoch-1" {
		t.Fatalf("epoch=%v", got)
	}
	if got, _ := delta.MustGet("sequence").(json.Number); got.String() != "2" {
		t.Fatalf("sequence=%v", got)
	}
	rows := delta.MustGet("rows").(*jsoncompat.Array)
	if len(rows.Items) != 1 {
		t.Fatalf("rows=%d", len(rows.Items))
	}
	row := rows.Items[0].(*jsoncompat.Object)
	if row.MustGet("key") != firstKey(t, instance) {
		t.Fatalf("key=%v", row.MustGet("key"))
	}
	previous := instance.Page(20, "") // current generation row for versions
	_ = previous
	if row.MustGet("prevVersion") == row.MustGet("version") {
		t.Fatal("version must move with the append")
	}
	if row.MustGet("append") != "，世界" {
		t.Fatalf("append=%v", row.MustGet("append"))
	}
	if _, hasMeta := delta.Get("meta"); !hasMeta {
		t.Fatal("delta payload must carry meta")
	}
}

func TestDeltasRejectsReplacement(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	instance.Update(updateTextAt(deltaView(2, "你好"), 0, "再见"))
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("rewritten text is not an append")
	}
}

func TestDeltasRejectsOversizedAppend(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	instance.Update(updateTextAt(deltaView(2, "你好"), 0, "你好"+strings.Repeat("长", deltaMaxAppend)))
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("append beyond deltaMaxAppend must fall back")
	}
}

func TestDeltasRejectsPreviewTruncation(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, strings.Repeat("字", TextPreview-1)))
	instance.Update(updateTextAt(deltaView(2, ""), 0, strings.Repeat("字", TextPreview+4)))
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("an append crossing the preview budget must fall back")
	}
}

func TestDeltasRejectsFieldChange(t *testing.T) {
	base := deltaView(1, "你好")
	instance := New(func() string { return "epoch-1" })
	instance.Update(base)
	next := updateTextAt(deltaView(2, "你好"), 0, "你好，世界")
	turn := next.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	turn.Set("status", "completed")
	instance.Update(next)
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("a turnStatus move rides the changes frame, not a delta")
	}
}

func TestDeltasRejectsStaleAfter(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	instance.Update(updateTextAt(deltaView(2, "你好"), 0, "你好，世界"))
	instance.Update(updateTextAt(deltaView(3, "你好"), 0, "你好，世界，再来"))
	// Only the immediately previous generation carries prevText.
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("deltas only anchor on the previous generation")
	}
	if _, ok := instance.Deltas(""); ok {
		t.Fatal("empty after must fall back")
	}
	if _, ok := instance.Deltas("3"); ok {
		t.Fatal("current sequence is not a delta anchor")
	}
	delta, ok := instance.Deltas("2")
	if !ok {
		t.Fatal("the previous generation anchors a delta")
	}
	if got := delta.MustGet("rows").(*jsoncompat.Array).Items[0].(*jsoncompat.Object).
		MustGet("append"); got != "，再来" {
		t.Fatalf("append=%v", got)
	}
}

func TestDeltasRejectsNewRow(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	instance.Update(deltaView(2, "你好", "第二条"))
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("a new row has no previous text to append onto")
	}
}

func TestDeltasVacuousWhenOnlyMetaMoves(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "你好"))
	next := deltaView(2, "你好")
	next.Set("status", "idle")
	instance.Update(next)
	delta, ok := instance.Deltas("1")
	if !ok {
		t.Fatal("a meta-only bump stays representable")
	}
	if rows := delta.MustGet("rows").(*jsoncompat.Array); len(rows.Items) != 0 {
		t.Fatalf("rows=%d", len(rows.Items))
	}
	if got, _ := delta.MustGet("meta").(*jsoncompat.Object).MustGet("status").(string); got != "idle" {
		t.Fatalf("meta status=%v", got)
	}
}

func TestDeltasRejectsOrderMove(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	instance.Update(deltaView(1, "旧的一句", "你好"))
	next := deltaView(2, "旧的一句", "你好，世界")
	// Prepend one turn: every order shifts, so nothing may ship as a delta.
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-0")
	turn.Set("status", "completed")
	message := jsoncompat.NewObject()
	message.Set("id", "m-earlier")
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", "更早的一句")
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	turns := next.MustGet("turns").(*jsoncompat.Array)
	turns.Items = append([]any{turn}, turns.Items...)
	instance.Update(next)
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("shifted orders must fall back to changes")
	}
}

func TestDeltasRejectsActivityRows(t *testing.T) {
	instance := New(func() string { return "epoch-1" })
	activityTurn := jsoncompat.NewObject()
	activityTurn.Set("id", "turn-a")
	activityTurn.Set("status", "inProgress")
	activity := jsoncompat.NewObject()
	activity.Set("id", "tool-1")
	activity.Set("kind", "toolCall")
	activity.Set("role", "activity")
	activity.Set("text", "运行中")
	activityTurn.Set("messages", &jsoncompat.Array{Items: []any{activity}})
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(1))
	view.Set("turns", &jsoncompat.Array{Items: []any{activityTurn}})
	instance.Update(view)
	grown := jsoncompat.Clone(view).(*jsoncompat.Object)
	activity = grown.MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object).
		MustGet("messages").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
	activity.Set("text", "运行中，完成")
	grown.Set("sequence", jsoncompat.Num(2))
	instance.Update(grown)
	if _, ok := instance.Deltas("1"); ok {
		t.Fatal("activity rows preview at 180 and always count as truncated")
	}
}

// TestIncrementalMatchesColdBuild pins the byte contract of the reuse fast
// path: a timeline that reached a view through several Updates must produce
// byte-identical Page/Changes payloads to one that built the same view
// directly (the same property the golden scenarios enforce).
func TestIncrementalMatchesColdBuild(t *testing.T) {
	step := deltaView(1, "第一句", "你好")
	grown := updateTextAt(deltaView(2, "第一句", "你好"), 1, "你好，世界")
	rewritten := updateTextAt(deltaView(3, "第一句", "你好，世界"), 1, "换了内容")
	expanded := deltaView(4, "第一句", "换了内容", "新增行")
	trimmed := deltaView(5, "换了内容", "新增行")

	incremental := New(func() string { return "epoch-1" })
	cold := New(func() string { return "epoch-1" })
	for _, view := range []*jsoncompat.Object{step, grown, rewritten, expanded, trimmed} {
		incremental.Update(view)
		cold.Update(jsoncompat.Clone(view).(*jsoncompat.Object))
		if string(jsoncompat.EncodeCompact(incremental.Page(20, ""))) !=
			string(jsoncompat.EncodeCompact(cold.Page(20, ""))) {
			t.Fatalf("page bytes diverged at sequence %v", view.MustGet("sequence"))
		}
		if string(jsoncompat.EncodeCompact(incremental.Changes("1", "epoch-1", ""))) !=
			string(jsoncompat.EncodeCompact(cold.Changes("1", "epoch-1", ""))) {
			t.Fatalf("changes bytes diverged at sequence %v", view.MustGet("sequence"))
		}
	}
}
