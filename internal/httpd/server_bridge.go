package httpd

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
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
	if liveEpoch, liveSequence, ok := s.refreshLiveProjection(threadID); ok {
		epoch, sequence = liveEpoch, liveSequence
	}
	session := &ScriptedSession{Epoch: epoch, Sequence: sequence}
	server := s
	session.Live = func() (string, int, bool) { return server.refreshLiveProjection(threadID) }
	return session
}

// acpAgentSet returns the configured per-kind ACP managers, folding the
// generic GATEWAY_ACP_BIN manager in as "acp" when no kinds were wired.
func (s *Server) acpAgentSet() map[string]*acpruntime.Manager {
	if len(s.ACPAgents) > 0 {
		return s.ACPAgents
	}
	if s.ACP != nil {
		return map[string]*acpruntime.Manager{"acp": s.ACP}
	}
	return nil
}

// acpResolution describes one thread's ACP ownership: manager != nil means
// the id belongs to this ACP agent (session nil when it cannot be opened).
type acpResolution struct {
	kind    string
	manager *acpruntime.Manager
	session *acpruntime.Session
	err     error
}

// resolveACP mirrors Python's Registry.bridge_for: a live session wins, then
// the cached session/list rows (including lazy-placeholder canonical ids),
// then one cold list probe for managers that have never been listed. It never
// spawns for codex ids beyond that single probe per manager.
func (s *Server) resolveACP(threadID string) acpResolution {
	agents := s.acpAgentSet()
	if len(agents) == 0 || threadID == "" {
		return acpResolution{}
	}
	// A deleted (hidden) session loses ownership: reads fall through and the
	// agent process is never respawned for it.
	if s.Hidden.Hidden(threadID) {
		return acpResolution{}
	}
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil {
			continue
		}
		if session := manager.Get(threadID); session != nil {
			return acpResolution{kind: kind, manager: manager, session: session}
		}
	}
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil || !manager.Knows(threadID) {
			continue
		}
		return s.openACP(kind, manager, threadID)
	}
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil || manager.HasListCache() {
			continue
		}
		if _, err := manager.List(15 * time.Second); err != nil {
			continue
		}
		if manager.Knows(threadID) {
			return s.openACP(kind, manager, threadID)
		}
	}
	return acpResolution{}
}

func (s *Server) openACP(kind string, manager *acpruntime.Manager, threadID string) acpResolution {
	cwd := manager.CwdFor(threadID)
	if cwd == "" {
		return acpResolution{kind: kind, manager: manager,
			err: errors.New("找不到这个会话的工作目录，请稍后刷新列表")}
	}
	session, err := manager.Open(threadID, cwd, manager.InitializeTimeout)
	if err != nil {
		return acpResolution{kind: kind, manager: manager, err: err}
	}
	return acpResolution{kind: kind, manager: manager, session: session}
}

// acpViewState shapes one ACP snapshot into the chat-screen view contract:
// the snapshot plus host/agent/capabilities and the liveness flags the web
// app renders (mirrors service.Bridge.view's ACP annotations). On-disk
// child agents decorate the view when the agent has a subagent store; a
// roster change bumps the session sequence so projections rebuild.
func (s *Server) acpViewState(kind string, manager *acpruntime.Manager, session *acpruntime.Session) *jsoncompat.Object {
	var activity subagents.Activity
	if manager.Subagents != nil {
		activity = manager.Subagents.Activity(session.ID)
		session.NoteSubagents(activity.Revision)
	}
	view := session.Snapshot()
	// Settled turns become actionable so the shared projection derives
	// editable/forkable rows (the phone's branch controls) — see acp_fork.go.
	annotateACPForkability(view)
	view.Set("sequence", jsoncompat.Num(session.Sequence()))
	if manager.Subagents != nil {
		subagents.Inject(view, activity)
	}
	view.Set("host", "local")
	view.Set("hostLabel", "此电脑")
	view.Set("agent", kind)
	capabilities := &jsoncompat.Array{}
	for _, capability := range manager.Capabilities {
		// The ACP goal send path is not ported; hide the control rather
		// than offer a goal mode that silently degrades to a plain prompt.
		// Branching IS offered: acp_fork.go synthesizes it on the gateway.
		if capability == "goals" {
			continue
		}
		capabilities.Items = append(capabilities.Items, capability)
	}
	view.Set("capabilities", capabilities)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	return view
}

// codexCapabilities mirrors the CodexAdapter capability set minus the
// surfaces the Go gateway has not ported yet (the native goal send path);
// each entry re-enables a phone control that reads it from the timeline
// meta. Fork branches work through the app-server's thread/fork.
func (s *Server) codexCapabilities() *jsoncompat.Array {
	items := []string{"accounts", "editMessage", "fastMode", "fork", "fullHistory",
		"goals", "notificationScan", "rename", "serviceTier", "skills", "steer"}
	array := &jsoncompat.Array{}
	for _, item := range items {
		array.Items = append(array.Items, item)
	}
	return array
}

// agentRoster mirrors Registry.kinds(): every enabled agent including codex,
// sorted, as {kind,label} descriptors for /api/projects and timeline meta.
func (s *Server) agentRoster() *jsoncompat.Array {
	entries := []struct{ kind, label string }{}
	agents := s.acpAgentSet()
	if len(agents) == 0 {
		return &jsoncompat.Array{}
	}
	for kind, manager := range agents {
		if manager == nil {
			continue
		}
		label := manager.Label
		if label == "" {
			label = kind
		}
		entries = append(entries, struct{ kind, label string }{kind, label})
	}
	entries = append(entries, struct{ kind, label string }{"codex", "Codex"})
	sort.Slice(entries, func(i, j int) bool { return entries[i].kind < entries[j].kind })
	roster := &jsoncompat.Array{}
	for _, entry := range entries {
		item := jsoncompat.NewObject()
		item.Set("kind", entry.kind)
		item.Set("label", entry.label)
		roster.Items = append(roster.Items, item)
	}
	return roster
}

// refreshLiveProjection mirrors liveTimeline's read path for the SSE loop:
// attach the follower, fold the latest snapshot into the shared per-thread
// projection, and report the projection's epoch/sequence. ACP threads fold
// their acpview snapshot through the same shared projection.
func (s *Server) refreshLiveProjection(threadID string) (string, int, bool) {
	if resolution := s.resolveACP(threadID); resolution.manager != nil {
		if resolution.session == nil {
			return s.projectionState(threadID)
		}
		view := s.acpViewState(resolution.kind, resolution.manager, resolution.session)
		return s.foldProjection(threadID, view)
	}
	if s.Codex == nil {
		return "", 0, false
	}
	_, _ = s.Codex.Watch(threadID, 1500*time.Millisecond)
	state, sequence, connected, connectionError := s.Codex.Snapshot(threadID)
	if state == nil {
		return s.projectionState(threadID)
	}
	view := model.NormalizeState(state, connected)
	// Must match liveView's annotation exactly: both fold the same shared
	// per-thread projection, and divergent rows would flap row digests.
	annotateViewChanges(view, model.OrderedTurns(state))
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("connectionError", connectionError)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	view.Set("host", "local")
	view.Set("hostLabel", "此电脑")
	view.Set("agent", "codex")
	view.Set("capabilities", s.codexCapabilities())
	s.codexSubagentView(threadID, view)
	return s.foldProjection(threadID, view)
}

// codexSubagentView decorates a codex view with on-disk child agents
// (thread_spawn_edges via the store), mirroring service.Bridge._add_subagents.
// No sequence bump: the codex IPC sequence stays authoritative, rows land on
// the next natural rebuild — same as Python.
func (s *Server) codexSubagentView(threadID string, view *jsoncompat.Object) {
	if s.Store == nil {
		return
	}
	s.subagentOnce.Do(func() {
		s.codexSubagents = subagents.NewFlattener(s.Store.Subagents())
	})
	subagents.Inject(view, s.codexSubagents.Activity(threadID))
}

func (s *Server) View(threadID string) any {
	if s.Hidden.Hidden(threadID) {
		return jsoncompat.NewObject()
	}
	if resolution := s.resolveACP(threadID); resolution.manager != nil && resolution.session != nil {
		return s.acpViewState(resolution.kind, resolution.manager, resolution.session)
	}
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
			view := model.NormalizeState(state, false)
			view.Set("host", "local")
			view.Set("hostLabel", "此电脑")
			view.Set("agent", "codex")
			view.Set("capabilities", s.codexCapabilities())
			s.codexSubagentView(threadID, view)
			return view
		}
	}
	return jsoncompat.NewObject()
}

func (s *Server) TimelineRead(threadID, mode string) any {
	if resolution := s.resolveACP(threadID); resolution.manager != nil && resolution.session != nil {
		view := s.acpViewState(resolution.kind, resolution.manager, resolution.session)
		projection := timeline.New(nil)
		projection.Update(view)
		projection.SetMeta("host", "local")
		projection.SetMeta("agent", resolution.kind)
		projection.SetMeta("hostLabel", "此电脑")
		projection.SetMeta("capabilities", &jsoncompat.Array{})
		switch mode {
		case "detail":
			if result, err := projection.Detail("", 0, nil); err == nil {
				return result
			}
		case "changes":
			return projection.Changes("-1", "", "")
		default:
			return projection.Page(20, "")
		}
		return projection.Page(20, "")
	}
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
	view.Set("host", "local")
	view.Set("hostLabel", "此电脑")
	view.Set("agent", "codex")
	view.Set("capabilities", s.codexCapabilities())
	return view, true
}
