// Inline image block persistence: ACP agents may stream image content as
// base64 data. Decoding them to per-session files keeps view snapshots small
// (the alternative is cloning megabytes on every SSE poll) and turns each
// block into a local path the timeline attachment annotation can serve.
package acpruntime

import (
	"github.com/BenedictKing/mobile-bridge/internal/agentmedia"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// prepareMedia rewrites image blocks with inline data in one session/update
// payload into {type: image, path} references. Failures degrade to dropping
// the data field — the block then behaves like the pre-media era (no
// preview, no base64 dumped into tool output text).
func (s *Session) prepareMedia(update *jsoncompat.Object) {
	if update == nil {
		return
	}
	kind, _ := update.MustGet("sessionUpdate").(string)
	switch kind {
	case "user_message_chunk", "agent_message_chunk", "agent_thought_chunk", "tool_call", "tool_call_update":
		agentmedia.PersistImages(update.MustGet("content"), s.mediaDir, s.ID)
	}
}
