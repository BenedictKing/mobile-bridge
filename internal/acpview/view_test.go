package acpview

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func cardParams(title string, rawInput *jsoncompat.Object) *jsoncompat.Object {
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", title)
	toolCall.Set("rawInput", rawInput)
	params := jsoncompat.NewObject()
	params.Set("toolCall", toolCall)
	return params
}

func TestPermissionCardExtractsSubject(t *testing.T) {
	rawInput := jsoncompat.NewObject()
	rawInput.Set("command", "cargo test")
	title, command, question := PermissionCard(cardParams("Bash", rawInput))
	if title != "Bash" || command != "cargo test" || question {
		t.Fatalf("subject = %q %q %v", title, command, question)
	}

	// kimi bridges AskUserQuestion onto request_permission: a questions array
	// in rawInput flags the card even under a plain tool title.
	choice := jsoncompat.NewObject()
	choice.Set("question", "选 A 还是 B？")
	choice.Set("options", &jsoncompat.Array{Items: []any{optionLabel("A"), optionLabel("B")}})
	questionInput := jsoncompat.NewObject()
	questionInput.Set("questions", &jsoncompat.Array{Items: []any{choice}})
	_, _, question = PermissionCard(cardParams("Bash", questionInput))
	if !question {
		t.Fatal("questions rawInput should flag the card as a question")
	}

	if title, command, question = PermissionCard(jsoncompat.NewObject()); title != "权限请求" || command != "" || question {
		t.Fatalf("empty params subject = %q %q %v", title, command, question)
	}
	if title, command, question = PermissionCard(nil); title != "权限请求" || command != "" || question {
		t.Fatalf("nil params subject = %q %q %v", title, command, question)
	}
}

// 预算停靠（max_tokens / max_turns / max_turn_requests）的回答是截断的：
// 徽标必须落 interrupted，绝不能显示为已完成（audit：stopReason 映射偏宽）。
func TestEndTurnClassifiesBudgetStopsAsInterrupted(t *testing.T) {
	for _, reason := range []string{"max_tokens", "max_turns", "max_turn_requests"} {
		view := NewView("sess-1", "标题", "/work", func() float64 { return 0 })
		view.BeginTurn("跑完测试")
		view.EndTurn(reason)
		turn, _ := view.Snapshot().MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
		if status, _ := turn.MustGet("status").(string); status != "interrupted" {
			t.Fatalf("stopReason=%s status=%q，应为 interrupted", reason, status)
		}
		if recorded, _ := turn.MustGet("stopReason").(string); recorded != reason {
			t.Fatalf("stopReason 应原样保留，got=%q", recorded)
		}
	}
	// 正常收尾与取消保持原口径。
	for reason, want := range map[string]string{"end_turn": "completed", "cancelled": "interrupted"} {
		view := NewView("sess-1", "标题", "/work", func() float64 { return 0 })
		view.BeginTurn("跑完测试")
		view.EndTurn(reason)
		turn, _ := view.Snapshot().MustGet("turns").(*jsoncompat.Array).Items[0].(*jsoncompat.Object)
		if status, _ := turn.MustGet("status").(string); status != want {
			t.Fatalf("stopReason=%s status=%q，应为 %s", reason, status, want)
		}
	}
}

// kimi injects <system-reminder> prompts and replays them as
// user_message_chunk; stripped to empty they must not leave blank user
// bubbles, but they still close a trailing agent turn.
func TestUserReminderChunkLeavesNoBlankRow(t *testing.T) {
	view := NewView("sess-1", "标题", "/work", func() float64 { return 0 })
	view.BeginTurn("第一轮")
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"text","text":"回答"}}`))
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"<system-reminder>内部提醒</system-reminder>"}}`))
	snapshot := view.Snapshot()
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 {
		t.Fatalf("reminder chunk must not open a new turn: %d turns", len(turns.Items))
	}
	turn, _ := turns.Items[0].(*jsoncompat.Object)
	if status, _ := turn.MustGet("status").(string); status != "completed" {
		t.Fatalf("reminder chunk must close the agent turn, status=%q", status)
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	for _, item := range messages.Items {
		message, _ := item.(*jsoncompat.Object)
		if text, _ := message.MustGet("text").(string); text == "" {
			t.Fatalf("blank row survived: %+v", message)
		}
	}

	// The real user message after the reminder still lands in a fresh turn.
	view.OnUpdate(notification(t,
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"继续处理"}}`))
	snapshot = view.Snapshot()
	turns, _ = snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 2 {
		t.Fatalf("real user chunk should open a new turn: %d turns", len(turns.Items))
	}
	turn, _ = turns.Items[1].(*jsoncompat.Object)
	messages, _ = turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 1 {
		t.Fatalf("new turn should hold exactly the user row: %d", len(messages.Items))
	}
	message, _ := messages.Items[0].(*jsoncompat.Object)
	if text, _ := message.MustGet("text").(string); text != "继续处理" {
		t.Fatalf("user text = %q", text)
	}
}
