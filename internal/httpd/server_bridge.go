package httpd

import (
	"fmt"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// Server itself is the production local bridge. The old scripted bridge is
// retained only by isolated conformance fixtures; normal gateway requests
// must read the configured Codex/store state.
func (s *Server) SessionsList() any {
	result := jsoncompat.NewObject()
	result.Set("sessions", &jsoncompat.Array{})
	result.Set("unavailableHosts", &jsoncompat.Array{})
	return result
}

func (s *Server) AccountsPublic() any {
	result := jsoncompat.NewObject()
	result.Set("accounts", &jsoncompat.Array{})
	result.Set("activeId", nil)
	state := jsoncompat.NewObject()
	state.Set("phase", "idle")
	result.Set("switch", state)
	result.Set("canSwitch", true)
	return result
}

func (s *Server) ForHost(host string) (SessionBridge, error) {
	if host != "" && host != "local" {
		return nil, fmt.Errorf("KeyError: %s", host)
	}
	return s, nil
}

func (s *Server) Session(threadID string) *ScriptedSession {
	sequence := 0
	if s.Codex != nil {
		_, sequence, _, _ = s.Codex.Snapshot(threadID)
	}
	return &ScriptedSession{Epoch: "go-" + threadID, Sequence: sequence}
}

func (s *Server) View(threadID string) any {
	if s.Codex != nil {
		if state, ok := s.liveViewForBridge(threadID); ok {
			return state
		}
	}
	if s.Store != nil {
		if history, err := s.Store.HistoryFor(threadID, nil); err == nil {
			state := jsoncompat.NewObject()
			for _, key := range []string{"id", "title", "cwd", "latestModel", "modelProvider", "turns"} {
				if value, exists := history.Get(key); exists {
					state.Set(key, value)
				}
			}
			if value, exists := history.Get("turnsPagination"); exists {
				state.Set("turnsPagination", value)
			}
			return model.NormalizeState(state, false)
		}
	}
	return jsoncompat.NewObject()
}

func (s *Server) TimelineRead(threadID, mode string) any {
	view, _ := s.View(threadID).(*jsoncompat.Object)
	if view == nil {
		view = jsoncompat.NewObject()
	}
	projection := timeline.New(nil)
	projection.Update(view)
	switch mode {
	case "detail":
		result, err := projection.Detail("", 0, nil)
		if err == nil {
			return result
		}
	case "changes":
		return projection.Changes("-1", "", "")
	default:
		return projection.Page(20, "")
	}
	return projection.Page(20, "")
}

func (s *Server) Closed() <-chan struct{} {
	return nil
}

func (s *Server) liveViewForBridge(threadID string) (any, bool) {
	state, _, connected, connectionError := s.Codex.Snapshot(threadID)
	if state == nil {
		return nil, false
	}
	view := model.NormalizeState(state, connected)
	view.Set("connectionError", connectionError)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	return view, true
}
