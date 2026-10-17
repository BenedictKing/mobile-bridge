package httpd

// Session deletion (post-backlog port; the legacy Python bridge never had
// it). Codex threads delete natively through the short-lived app-server's
// thread/delete; ACP threads have no protocol delete, so the gateway keeps a
// durable hide list instead — the rollout/agent-side record is never touched.
// A codex delete that fails (older runtime without the method) degrades to
// the same hide list so the control never dead-ends.

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type hiddenSessions struct {
	mu    sync.Mutex
	path  string
	ids   map[string]bool
	order []string
}

// LoadHiddenSessions reads <dataDir>/hidden-sessions.json.
func LoadHiddenSessions(path string) *hiddenSessions {
	ledger := &hiddenSessions{path: path, ids: map[string]bool{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	var decoded []string
	if json.Unmarshal(raw, &decoded) == nil {
		for _, id := range decoded {
			if id == "" || ledger.ids[id] {
				continue
			}
			ledger.ids[id] = true
			ledger.order = append(ledger.order, id)
		}
	}
	return ledger
}

func (h *hiddenSessions) Hidden(id string) bool {
	if h == nil || id == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ids[id]
}

func (h *hiddenSessions) hide(id string) {
	if id == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ids[id] {
		return
	}
	h.ids[id] = true
	h.order = append(h.order, id)
	h.saveLocked()
}

func (h *hiddenSessions) saveLocked() {
	if h.path == "" {
		return
	}
	raw, err := json.MarshalIndent(h.order, "", "  ")
	if err != nil {
		return
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, h.path)
	}
}

// hiddenSession is the Server-level nil-safe check used by every list and
// projection read path.
func (s *Server) hiddenSession(id string) bool {
	return s.Hidden.Hidden(id)
}

// handleSessionDelete serves POST /api/sessions/<id>/delete. Ownership first:
// an ACP thread hides (and drops its live handle); a codex thread deletes
// through the app-server runtime, falling back to hiding. Pending queue
// entries of the thread are cancelled either way so the maintain loop never
// dispatches into a removed chat.
func (h *handler) handleSessionDelete(threadID string) {
	result := jsoncompat.NewObject()
	result.Set("id", threadID)
	result.Set("host", "local")

	// Already hidden (a retried request, or a previous codex delete that
	// degraded): idempotent success.
	if h.server.hiddenSession(threadID) {
		result.Set("mode", "hidden")
		h.finishSessionDelete(threadID, result)
		return
	}
	resolution := h.server.resolveACP(threadID)
	if resolution.manager != nil {
		// ACP: gateway-side removal only. Dropping the live handle ends this
		// gateway's child process; the agent's own stored session stays.
		resolution.manager.Drop(threadID)
		h.server.Hidden.hide(threadID)
		result.Set("mode", "hidden")
		h.finishSessionDelete(threadID, result)
		return
	}
	if h.server.Codex == nil {
		h.fail(http.StatusConflict, errorBody("当前网关没有可删除的会话来源"))
		return
	}
	if err := h.server.Codex.DeleteThread(threadID); err != nil {
		// Older runtimes lack thread/delete: degrade to hiding instead of
		// dead-ending the control.
		h.server.Hidden.hide(threadID)
		result.Set("mode", "hidden")
		result.Set("warning", err.Error())
		h.finishSessionDelete(threadID, result)
		return
	}
	result.Set("mode", "deleted")
	h.finishSessionDelete(threadID, result)
}

// finishSessionDrop cancels the thread's queued submissions, drops the
// retained timeline projection and bumps the discovery revision so every
// open chat list reloads without the deleted row.
func (h *handler) finishSessionDelete(threadID string, result *jsoncompat.Object) {
	if h.server.Queue != nil {
		h.server.Queue.forgetThread(threadID)
	}
	h.server.dropProjection(threadID)
	if discovery := h.server.discovery; discovery != nil {
		go discovery.refresh()
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
