// Package service is the Go port of bridge/service.py, starting with
// LiveSession's view-merge slice: saved-history prefix stitching over live
// normalized snapshots plus the connection-status fields. Parity is enforced
// against conformance/service goldens.
package service

import (
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// LiveSession mirrors the LiveSession fields this slice touches.
type LiveSession struct {
	ID         string
	State      any // *jsoncompat.Object or nil
	SavedView  *jsoncompat.Object
	Sequence   int
	Connected  bool
	Connecting bool
	Activating bool
	Error      any
	Timeline   *timeline.Timeline // the projection this session owns
}

func (s *LiveSession) Changed() { s.Sequence++ }

// SetHistory mirrors LiveSession.set_history: normalize the saved snapshot
// and adopt it as state unless a connected live state already exists.
func (s *LiveSession) SetHistory(state *jsoncompat.Object) {
	s.SavedView = model.NormalizeState(state, false)
	if s.State == nil || !s.Connected {
		s.State = state
	}
	s.Changed()
}

func objectOf(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

// View mirrors LiveSession.view: normalize the live state, stitch the saved
// prefix over incomplete tail snapshots, and append the status fields.
func (s *LiveSession) View() *jsoncompat.Object {
	state := objectOf(s.State)
	if state == nil {
		state = jsoncompat.NewObject()
		state.Set("id", s.ID)
	}
	result := model.NormalizeState(state, s.Connected)
	if s.SavedView != nil && !jsoncompat.Truthy(result.MustGet("historyComplete")) {
		savedTurns, _ := s.SavedView.MustGet("turns").(*jsoncompat.Array)
		liveTurns, _ := result.MustGet("turns").(*jsoncompat.Array)
		liveCount := 0
		if liveTurns != nil {
			liveCount = len(liveTurns.Items)
		}
		if liveCount == 0 {
			// An empty incomplete snapshot never clears saved history.
			result.Set("turns", savedTurns)
			result.Set("historyComplete", s.SavedView.MustGet("historyComplete"))
		} else {
			firstID := objectOf(liveTurns.Items[0]).MustGet("id")
			for index, turnValue := range savedTurns.Items {
				if turn := objectOf(turnValue); turn != nil &&
					valueEqual(turn.MustGet("id"), firstID) {
					merged := &jsoncompat.Array{}
					merged.Items = append(merged.Items, savedTurns.Items[:index]...)
					merged.Items = append(merged.Items, liveTurns.Items...)
					result.Set("turns", merged)
					result.Set("historyComplete", s.SavedView.MustGet("historyComplete"))
					break
				}
			}
			// An unmatched live head means a fresh history: keep it as-is.
		}
	}
	savedMore := s.SavedView != nil &&
		!jsoncompat.Truthy(result.MustGet("historyComplete")) &&
		!jsoncompat.Truthy(s.SavedView.MustGet("historyComplete"))
	result.Set("savedHistoryMore", savedMore)
	result.Set("sequence", jsoncompat.Num(s.Sequence))
	result.Set("connectionError", s.Error)
	result.Set("connecting", s.Connecting)
	result.Set("activating", s.Activating)
	result.Set("loadingHistory", s.State == nil)
	return result
}

func valueEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	return string(jsoncompat.EncodeCompact(a)) == string(jsoncompat.EncodeCompact(b))
}
