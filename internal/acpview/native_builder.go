package acpview

import (
	"fmt"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// NativeBuilder folds persisted user/assistant/tool events into the same turn
// shape used by live ACP updates. It has no transport or process side effects.
type NativeBuilder struct {
	view      *View
	agent     int
	awaiting  bool
	hasTurn   bool
	lastTitle string
}

func NewNativeBuilder(sessionID string) *NativeBuilder {
	return &NativeBuilder{view: NewView(sessionID, nil, nil, func() float64 { return 0 })}
}

func (b *NativeBuilder) User(text string) {
	if text == "" {
		return
	}
	if b.hasTurn {
		b.view.EndTurn("end_turn")
	}
	b.view.BeginTurn(text)
	b.hasTurn = true
	b.awaiting = true
}

func (b *NativeBuilder) Agent(text string) {
	if text == "" {
		return
	}
	b.agent++
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "agent_message_chunk")
	update.Set("messageId", fmt.Sprintf("native-agent-%d", b.agent))
	content := jsoncompat.NewObject()
	content.Set("type", "text")
	content.Set("text", text)
	update.Set("content", content)
	b.view.OnUpdate(update)
	b.hasTurn = true
	b.awaiting = false
}

func (b *NativeBuilder) Thought(text string) {
	if text == "" {
		return
	}
	b.agent++
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "agent_thought_chunk")
	update.Set("messageId", fmt.Sprintf("native-thought-%d", b.agent))
	content := jsoncompat.NewObject()
	content.Set("type", "text")
	content.Set("text", text)
	update.Set("content", content)
	b.view.OnUpdate(update)
	b.hasTurn = true
}

func (b *NativeBuilder) Tool(id, name string, input any) {
	if id == "" {
		return
	}
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call")
	update.Set("toolCallId", id)
	update.Set("title", name)
	update.Set("rawInput", input)
	update.Set("status", "in_progress")
	b.view.OnUpdate(update)
	b.hasTurn = true
	b.awaiting = true
}

func (b *NativeBuilder) ToolResult(id string, content any, failed bool) {
	if id == "" {
		return
	}
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call_update")
	update.Set("toolCallId", id)
	if failed {
		update.Set("status", "failed")
	} else {
		update.Set("status", "completed")
	}
	block := jsoncompat.NewObject()
	block.Set("type", "text")
	block.Set("text", Text(content))
	update.Set("content", &jsoncompat.Array{Items: []any{block}})
	b.view.OnUpdate(update)
}

func (b *NativeBuilder) SetModel(model string) {
	if model != "" {
		b.view.Model = model
	}
}

func (b *NativeBuilder) SetTitle(title string) {
	b.lastTitle = title
}

func (b *NativeBuilder) Title() string { return b.lastTitle }

func (b *NativeBuilder) HasTurns() bool { return b.hasTurn && len(b.view.Turns) > 0 }

func (b *NativeBuilder) End(reason string) {
	switch reason {
	case "failed", "blocked":
		if turn := b.view.currentTurn(); turn != nil {
			turn.Set("status", "failed")
		}
		b.view.Status = "idle"
	case "cancelled":
		b.view.EndTurn("cancelled")
	default:
		b.view.EndTurn("end_turn")
	}
	b.awaiting = false
}

func (b *NativeBuilder) Snapshot(stale bool) *NativeSnapshot {
	if !b.hasTurn || len(b.view.Turns) == 0 {
		return nil
	}
	if b.awaiting && !stale {
		b.view.Status = "active"
	} else if b.awaiting {
		b.view.EndTurn("cancelled")
	} else {
		b.view.EndTurn("end_turn")
	}
	return &NativeSnapshot{Title: b.lastTitle, Model: b.view.Model, Turns: b.view.Turns}
}
