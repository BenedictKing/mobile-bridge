package httpd

import (
	"strconv"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
)

// Guarded by projectionSlot.mu. The view is immutable after publication and is
// only used by timeline readers; control routes still get private snapshots.
type liveProjectionCache struct {
	source         any
	sourceSequence int
	view           *jsoncompat.Object
	metadata       *jsoncompat.Object
	metadataKey    string
	activity       subagents.Activity
	checkedAt      time.Time
}

// Unknown/unavailable reads must not retain an empty projection per requested
// id. A follower can finish attaching asynchronously and supply a slot later.
func (s *Server) liveProjectionSlot(threadID string) *projectionSlot {
	if slot := s.lookupProjection(threadID); slot != nil {
		return slot
	}
	if s.hiddenSession(threadID) {
		return nil
	}
	if resolution := s.resolveACPHistory(threadID); resolution.manager != nil {
		if resolution.session == nil {
			return nil
		}
	} else {
		if s.Codex == nil {
			return nil
		}
		s.Codex.EnsureWatching(threadID)
		if _, _, ready := s.Codex.Revision(threadID); !ready {
			return nil
		}
	}
	return s.projectionSlotFor(threadID)
}

func (s *Server) liveProjectionChanges(threadID string) <-chan struct{} {
	slot := s.lookupProjection(threadID)
	if slot == nil {
		return nil
	}
	slot.mu.Lock()
	source := slot.live.source
	slot.mu.Unlock()
	if session, ok := source.(*acpruntime.Session); ok {
		_, changed := session.Revision()
		return changed
	}
	if s.Codex != nil {
		_, changed, _ := s.Codex.Revision(threadID)
		return changed
	}
	return nil
}

// refreshProjectionLocked checks cheap source revisions before copying history.
// Disk-backed controls are checked at most once a second during idle SSE waits;
// explicit timeline reads check them immediately. All subscribers share a slot.
func (s *Server) refreshProjectionLocked(threadID string, slot *projectionSlot, forceMetadata bool) (*jsoncompat.Object, string, bool) {
	if s.hiddenSession(threadID) {
		return nil, "", false
	}
	resolution := s.resolveACPHistory(threadID)
	cache := &slot.live
	kind := resolution.kind
	var source any
	var sequence int
	if resolution.manager != nil {
		if resolution.session == nil {
			return nil, kind, false
		}
		source = resolution.session
		sequence, _ = resolution.session.Revision()
	} else if s.Codex != nil {
		source = s.Codex
		s.Codex.EnsureWatching(threadID)
		var ready bool
		sequence, _, ready = s.Codex.Revision(threadID)
		if !ready {
			cache.source = source
			return nil, "", false
		}
	} else {
		return nil, "", false
	}
	if cache.source != source {
		*cache = liveProjectionCache{source: source}
	}
	metadataChanged := false
	if cache.metadata == nil || forceMetadata || time.Since(cache.checkedAt) >= time.Second {
		metadata := jsoncompat.NewObject()
		var activity subagents.Activity
		if resolution.manager != nil {
			if resolution.manager.Subagents != nil {
				activity = resolution.manager.Subagents.Activity(resolution.session.ID)
				resolution.session.NoteSubagents(activity.Revision)
				sequence, _ = resolution.session.Revision()
			}
			metadata = s.acpViewMetadata(kind, resolution.manager, resolution.session)
		} else {
			metadata.Set("host", "local")
			metadata.Set("hostLabel", "此电脑")
			metadata.Set("agent", "codex")
			metadata.Set("capabilities", s.codexCapabilitiesFor(threadID))
			metadata.Set("executionOwner", "desktop")
			if s.Codex.IsCLI(threadID) {
				metadata.Set("executionOwner", "cli")
			}
			metadata.Set("connecting", false)
			metadata.Set("activating", false)
			metadata.Set("loadingHistory", false)
			s.applyGoalOverlay(threadID, metadata)
			if s.Store != nil {
				s.subagentOnce.Do(func() { s.codexSubagents = subagents.NewFlattener(s.Store.Subagents()) })
				activity = s.codexSubagents.Activity(threadID)
			}
		}
		metadata.Set("subagentRevision", jsoncompat.Num(int(activity.Revision)))
		if s.Queue != nil {
			metadata.Set("submissions", s.queueMetaExcludingActivations(threadID))
		}
		if s.Forks != nil {
			metadata.Set("forkedFrom", s.forkedFromMeta(threadID))
		}
		metadata.Set("migration", s.migrationMeta(threadID))
		key := string(jsoncompat.EncodeCompact(metadata))
		metadataChanged = key != cache.metadataKey
		cache.metadata, cache.metadataKey, cache.activity = metadata, key, activity
		cache.checkedAt = time.Now()
	}
	if cache.view != nil && cache.sourceSequence == sequence && !metadataChanged &&
		slot.timeline.State().Sequence == jsoncompat.PyStr(cache.view.MustGet("sequence")) {
		return cache.view, kind, true
	}
	var view *jsoncompat.Object
	if resolution.session != nil {
		view, sequence = resolution.session.SnapshotWithSequence()
		annotateACPForkability(view)
	} else {
		state, revision, connected, connectionError := s.Codex.Snapshot(threadID)
		if state == nil {
			return nil, "", false
		}
		sequence = revision
		view = model.NormalizeState(state, connected)
		annotateViewChanges(view, model.OrderedTurns(state))
		view.Set("connectionError", connectionError)
	}
	subagents.Inject(view, cache.activity)
	for _, key := range cache.metadata.Keys() {
		view.Set(key, jsoncompat.Clone(cache.metadata.MustGet(key)))
	}
	// Independent metadata changes need a projection revision too. Never assign
	// an old source sequence to a newer snapshot or move a client's cursor back.
	projected := sequence
	if previous := slot.timeline.State().Sequence; previous != "" {
		last, _ := strconv.Atoi(previous)
		if projected <= last {
			projected = last + 1
		}
	}
	view.Set("sequence", jsoncompat.Num(projected))
	view.Set("activeTurnId", activeViewTurn(view))
	slot.timeline.Update(view)
	cache.view, cache.sourceSequence = view, sequence
	return view, kind, true
}
