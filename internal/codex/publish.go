// Publish feeds one desktop conversation snapshot into the follower state as
// if it had arrived on the IPC event stream: the deterministic concurrency
// tests (and any future replay tooling) use it to drive a Bridge without a
// live desktop App connection. Snapshots follow the same version-11
// thread-stream-state-changed shape handleEvent validates.
package codex

import (
	"encoding/json"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func (b *Bridge) Publish(id string, state *jsoncompat.Object) {
	revision := 0
	b.mu.Lock()
	if session := b.sessions[id]; session != nil {
		if number, ok := session.Revision.(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				revision = int(parsed)
			}
		}
	}
	b.mu.Unlock()
	change := jsoncompat.NewObject()
	change.Set("type", "snapshot")
	change.Set("revision", jsoncompat.Num(revision+1))
	change.Set("conversationState", state)
	params := jsoncompat.NewObject()
	params.Set("hostId", b.Host)
	params.Set("conversationId", id)
	params.Set("change", change)
	message := jsoncompat.NewObject()
	message.Set("method", "thread-stream-state-changed")
	message.Set("version", jsoncompat.Num(11))
	message.Set("sourceClientId", "publish")
	message.Set("params", params)
	b.handleEvent(message)
}
