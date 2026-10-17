// Per-thread projection slots: one mutex per thread timeline, so concurrent
// chat screens, SSE loops and message actions serialize only on their own
// thread (the previous single Server.historyMu serialized every thread
// behind one lock). The map itself is guarded by projectionsMu; slots are
// created on first fold and live for the process (bounded by the thread
// count a gateway actually serves).
package httpd

import (
	"strconv"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// projectionSlot serializes one thread's projection: Update/SetMeta and the
// Page/Changes/Detail/Deltas reads that must observe one consistent
// generation.
type projectionSlot struct {
	mu       sync.Mutex
	timeline *timeline.Timeline
}

// projectionSlotFor returns the thread's slot, creating it when missing
// (write paths that are about to fold a view).
func (s *Server) projectionSlotFor(threadID string) *projectionSlot {
	s.projectionsMu.Lock()
	defer s.projectionsMu.Unlock()
	slot := s.projections[threadID]
	if slot == nil {
		slot = &projectionSlot{timeline: timeline.New(nil)}
		s.projections[threadID] = slot
	}
	return slot
}

// lookupProjection returns the thread's existing slot without creating one,
// so read paths never materialize projections for unknown threads.
func (s *Server) lookupProjection(threadID string) *projectionSlot {
	s.projectionsMu.Lock()
	defer s.projectionsMu.Unlock()
	return s.projections[threadID]
}

// dropProjection forgets a thread's projection (session deleted or hidden);
// a later fold starts a fresh epoch, so stale cursors reset cleanly.
func (s *Server) dropProjection(threadID string) {
	s.projectionsMu.Lock()
	defer s.projectionsMu.Unlock()
	delete(s.projections, threadID)
}

// foldProjection updates the shared per-thread timeline projection under the
// thread's own lock and reports its epoch/sequence.
func (s *Server) foldProjection(threadID string, view *jsoncompat.Object) (string, int, bool) {
	slot := s.projectionSlotFor(threadID)
	slot.mu.Lock()
	defer slot.mu.Unlock()
	slot.timeline.Update(view)
	current := slot.timeline.State()
	parsed, _ := strconv.Atoi(current.Sequence)
	return current.Epoch, parsed, true
}

// projectionState reports the existing projection without folding a new view
// (used when an ACP thread is owned but temporarily unopenable).
func (s *Server) projectionState(threadID string) (string, int, bool) {
	slot := s.lookupProjection(threadID)
	if slot == nil {
		return "", 0, false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	current := slot.timeline.State()
	parsed, _ := strconv.Atoi(current.Sequence)
	return current.Epoch, parsed, true
}
