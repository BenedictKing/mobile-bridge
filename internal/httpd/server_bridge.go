package httpd

import (
	"fmt"
	"strconv"
	"time"

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
	epoch, sequence := "go-"+threadID, 0
	if s.Codex != nil {
		if liveEpoch, liveSequence, ok := s.refreshLiveProjection(threadID); ok {
			epoch, sequence = liveEpoch, liveSequence
		}
	}
	session := &ScriptedSession{Epoch: epoch, Sequence: sequence}
	if s.Codex != nil {
		server := s
		session.Live = func() (string, int, bool) { return server.refreshLiveProjection(threadID) }
	}
	return session
}

// refreshLiveProjection mirrors liveTimeline's read path for the SSE loop:
// attach the follower, fold the latest snapshot into the shared per-thread
// projection, and report the projection's epoch/sequence.
func (s *Server) refreshLiveProjection(threadID string) (string, int, bool) {
	if s.Codex == nil {
		return "", 0, false
	}
	_, _ = s.Codex.Watch(threadID, 1500*time.Millisecond)
	state, sequence, connected, connectionError := s.Codex.Snapshot(threadID)
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	projection := s.historyTL[threadID]
	if state == nil {
		if projection == nil {
			return "", 0, false
		}
		current := projection.State()
		parsed, _ := strconv.Atoi(current.Sequence)
		return current.Epoch, parsed, true
	}
	if projection == nil {
		projection = timeline.New(nil)
		s.historyTL[threadID] = projection
	}
	view := model.NormalizeState(state, connected)
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("connectionError", connectionError)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	projection.Update(view)
	current := projection.State()
	parsed, _ := strconv.Atoi(current.Sequence)
	return current.Epoch, parsed, true
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
