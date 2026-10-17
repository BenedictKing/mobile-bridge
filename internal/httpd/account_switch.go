// Switch-side glue on the Server: the idle gate handed to Manager.Switch so a
// desktop restart cannot interrupt live tasks or uncertain submissions
// (Python Accounts.idle/blockers, evaluated from the Go gateway's own live
// projections and the shared submissions ledger).
package httpd

import (
	"errors"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

// switchBusyStatus mirrors blockers()' running set: threadRuntimeStatus types
// that mean the desktop is mid-turn.
var switchBusyStatus = map[string]bool{
	"active": true, "running": true, "waiting": true, "busy": true,
}

// accountIdleGate returns the Switch closure: one blocker fails the request
// with the first few titles (Python idle's message shape).
func (s *Server) accountIdleGate() func() error {
	return func() error {
		blockers := s.accountBlockers()
		if len(blockers) == 0 {
			return nil
		}
		names := make([]string, 0, 3)
		for _, row := range blockers {
			if len(names) == 3 {
				break
			}
			names = append(names, row.Title)
		}
		return errors.New("请先结束任务或处理待发送消息：" + strings.Join(names, "、"))
	}
}

// accountBlockers mirrors blockers() with the gateway's local state: followed
// codex conversations that are connected and busy or awaiting approval, plus
// queued/unknown submissions across every agent.
func (s *Server) accountBlockers() []account.Blocker {
	var rows []account.Blocker
	if s.Codex != nil {
		s.projectionsMu.Lock()
		ids := make([]string, 0, len(s.projections))
		for id := range s.projections {
			ids = append(ids, id)
		}
		s.projectionsMu.Unlock()
		for _, id := range ids {
			state, _, connected, _ := s.Codex.Snapshot(id)
			if state == nil || !connected {
				continue
			}
			view := model.NormalizeState(state, true)
			title := pyStrOr(view.MustGet("title"), id)
			if requests, _ := view.MustGet("requests").(*jsoncompat.Array); len(orArrayItems(requests)) > 0 {
				rows = append(rows, account.Blocker{ID: id, Host: "local", Title: title, Reason: "approval"})
				continue
			}
			if status, _ := view.MustGet("status").(string); switchBusyStatus[status] {
				rows = append(rows, account.Blocker{ID: id, Host: "local", Title: title, Reason: "running"})
			}
		}
	}
	if s.Queue != nil {
		rows = append(rows, s.queueBlockers()...)
	}
	return rows
}

// queueBlockers reads the submissions ledger (same package, lock respected):
// queued/unknown entries are evidence of work that must not be stranded.
func (s *Server) queueBlockers() []account.Blocker {
	s.Queue.mu.Lock()
	defer s.Queue.mu.Unlock()
	rows := []account.Blocker{}
	for _, key := range s.Queue.order {
		entry := s.Queue.entries[key]
		if entry == nil || (entry.Status != SubQueued && entry.Status != SubUnknown) {
			continue
		}
		title := entry.Text
		if title == "" {
			title = entry.ThreadID
		}
		rows = append(rows, account.Blocker{ID: entry.ThreadID, Host: entry.Host,
			Title: truncateSubText(title, 40), Reason: entry.Status})
	}
	return rows
}

func pyStrOr(value any, fallback string) string {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fallback
}

func orArrayItems(array *jsoncompat.Array) []any {
	if array == nil {
		return nil
	}
	return array.Items
}
