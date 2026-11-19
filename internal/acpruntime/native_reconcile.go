package acpruntime

import (
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type nativePendingPrompt struct {
	message *jsoncompat.Object
	after   int
}

// kimiTaskAhead requires a new task after the store prefix we already applied.
// Empty ACP replies do not acknowledge queued user prompts in Kimi's store.
func (s *Session) kimiTaskAhead(snapshot *acpview.NativeSnapshot) bool {
	if s.kind != "kimi" || s.nativeTurns == 0 || snapshot == nil ||
		len(snapshot.Turns) <= s.nativeTurns || len(snapshot.TurnOrigins) != len(snapshot.Turns) ||
		snapshot.TurnOrigins[len(snapshot.TurnOrigins)-1] != "task" || len(s.View.Turns) <= s.nativeTurns {
		return false
	}
	for _, turn := range s.View.Turns[s.nativeTurns:] {
		if !emptySettledPrompt(turn) {
			return false
		}
	}
	return true
}

// preserveKimiPrompts keeps accepted user messages visible until a later native
// user turn acknowledges them. They ride inside the native tip so its actual
// lifecycle, rather than an empty ACP reply, controls the page's busy state.
func (s *Session) preserveKimiPrompts(snapshot *acpview.NativeSnapshot) *acpview.NativeSnapshot {
	if s.kind != "kimi" {
		return snapshot
	}
	for _, turn := range s.View.Turns[min(s.nativeTurns, len(s.View.Turns)):] {
		if !emptySettledPrompt(turn) {
			continue
		}
		messages := turn.MustGet("messages").(*jsoncompat.Array)
		message := jsoncompat.Clone(messages.Items[0]).(*jsoncompat.Object)
		message.Set("id", "native-pending-"+stringField(message, "id"))
		s.nativePending = append(s.nativePending, nativePendingPrompt{message: message, after: s.nativeTurns})
	}
	next := 0
	pending := s.nativePending[:0]
	for _, prompt := range s.nativePending {
		prompt.after = max(prompt.after, next)
		acknowledged := false
		for index := prompt.after; index < len(snapshot.Turns); index++ {
			if index >= len(snapshot.TurnOrigins) || snapshot.TurnOrigins[index] != "user" ||
				!nativePromptMatches(snapshot.Turns[index], prompt.message) {
				continue
			}
			next = index + 1
			acknowledged = true
			break
		}
		if !acknowledged {
			pending = append(pending, prompt)
		}
	}
	s.nativePending = pending
	if len(pending) == 0 {
		return snapshot
	}
	merged := *snapshot
	merged.Turns = append([]*jsoncompat.Object(nil), snapshot.Turns...)
	last := len(merged.Turns) - 1
	tip := jsoncompat.Clone(merged.Turns[last]).(*jsoncompat.Object)
	messages, _ := tip.MustGet("messages").(*jsoncompat.Array)
	if messages == nil {
		messages = &jsoncompat.Array{}
		tip.Set("messages", messages)
	}
	for _, prompt := range pending {
		messages.Items = append(messages.Items, jsoncompat.Clone(prompt.message))
	}
	merged.Turns[last] = tip
	return &merged
}

func nativePromptMatches(turn, prompt *jsoncompat.Object) bool {
	if turn == nil {
		return false
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages == nil {
		return false
	}
	for _, item := range messages.Items {
		message, _ := item.(*jsoncompat.Object)
		if message != nil && message.MustGet("role") == "user" && message.MustGet("text") == prompt.MustGet("text") &&
			string(jsoncompat.EncodeCompact(message.MustGet("attachments"))) == string(jsoncompat.EncodeCompact(prompt.MustGet("attachments"))) {
			return true
		}
	}
	return false
}
