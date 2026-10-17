// Result-visibility convention plumbing for the HTTP layer: the persisted
// switch lives in internal/conventions; this file turns it into prompt
// blocks so the send path (and any queue drain) can share one injection
// point.
package httpd

import (
	"github.com/BenedictKing/mobile-bridge/internal/conventions"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// conventionExtras prepends the artifact-path convention text block to a
// turn's extra prompt blocks when the switch is on (nil wiring = off). The
// caller keeps the user's raw text as the leading prompt block, so the
// timeline's user row (BeginTurn(text)) never shows the convention. Queue
// drains that bypass the send handler should route through here too so
// queued turns follow the same rule.
func (s *Server) conventionExtras(extra []*jsoncompat.Object) []*jsoncompat.Object {
	if s.Conventions == nil || !s.Conventions.ArtifactPaths() {
		return extra
	}
	block := jsoncompat.NewObject()
	block.Set("type", "text")
	block.Set("text", conventions.ArtifactPathHint)
	return append([]*jsoncompat.Object{block}, extra...)
}
