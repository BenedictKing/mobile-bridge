package acpruntime

import (
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPromptRefusalSettlesAsInterrupted(t *testing.T) {
	session := &Session{
		ID: "sess-refusal", View: acpview.NewView("sess-refusal", nil, "/work", func() float64 { return 1 }),
		changed: make(chan struct{}), promptRunning: true,
	}
	session.View.BeginTurn("执行这个请求")
	result := jsoncompat.NewObject()
	result.Set("stopReason", "refusal")
	delivery := make(chan acp.Delivery, 1)
	delivery <- acp.Delivery{Value: result}
	promptDone := make(chan struct{})
	session.promptDone = promptDone

	session.awaitPrompt("prompt-1", delivery, time.Hour, time.Hour, promptDone, true)

	turns, _ := session.Snapshot().MustGet("turns").(*jsoncompat.Array)
	if turns == nil || len(turns.Items) != 1 {
		t.Fatalf("turns=%v", turns)
	}
	turn, _ := turns.Items[0].(*jsoncompat.Object)
	if status, _ := turn.MustGet("status").(string); status != "interrupted" {
		t.Fatalf("refusal status=%q, want interrupted", status)
	}
	if reason, _ := turn.MustGet("stopReason").(string); reason != "refusal" {
		t.Fatalf("stopReason=%q, want refusal", reason)
	}
	if session.promptRunning {
		t.Fatal("refusal must settle promptRunning")
	}
	select {
	case <-promptDone:
	default:
		t.Fatal("refusal must close promptDone")
	}
}

func TestRefusalFailureWithoutDataUsesMessage(t *testing.T) {
	meta := jsoncompat.NewObject()
	meta.Set("codebuddy.ai/errorMessage", `{"message":"Agent 拒绝执行"}`)
	result := jsoncompat.NewObject()
	result.Set("_meta", meta)
	if failure := refusalFailure(result); failure != "Agent 拒绝执行" {
		t.Fatalf("failure=%q, want metadata message", failure)
	}
}
