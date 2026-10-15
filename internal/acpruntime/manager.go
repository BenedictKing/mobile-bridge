// Package acpruntime owns long-lived ACP child processes and exposes their
// normalized acpview snapshots to the Go service layer.
package acpruntime

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
)

type Session struct {
	ID      string
	Cwd     string
	Process *acp.Process
	View    *acpview.View

	// Title and CreatedAtMs are set at creation time and never mutated, so
	// list merging may read them without the session lock.
	Title       string
	CreatedAtMs int64

	mu            sync.Mutex
	promptRunning bool
	pending       map[string]*jsoncompat.Object
	pendingIDs    map[string]any
	pendingKinds  map[string]string
	changed       chan struct{}
	closed        bool
	active        bool
	generation    uint64
	sequence      int
	configOptions *jsoncompat.Array
	modes         *jsoncompat.Object
	// lastActivity moves on every agent signal (streamed updates, reverse
	// requests); the prompt idle timeout measures against it.
	lastActivity time.Time
	// touched moves on gateway-side reads (HTTP/SSE polling); the idle
	// sweeper evicts sessions nobody has read for a while.
	touched time.Time
	// activitySlice is the prompt wait slice (Python ACTIVITY_SLICE); the
	// default matches, tests shrink it.
	activitySlice        time.Duration
	lastSubagentRevision int64
}

// Subagents is the on-disk child-agent source decorating an agent's views
// (Python's native_subagents stores). Nil disables decoration.
type Subagents interface {
	// Activity returns the flattened rows plus the roster revision in one
	// read; implementations cache by signature.
	Activity(parentID string) subagents.Activity
}

type Manager struct {
	Executable string
	Command    []string

	// Label, Capabilities and EffortMap mirror the Python adapter profile
	// (bridge/agents/*.py): list rows, views and the settings validator all
	// read them. InitializeTimeout is the handshake budget — agents launched
	// through npx need the long 180s first-run download window.
	Label             string
	Capabilities      []string
	EffortMap         map[string]string
	InitializeTimeout time.Duration

	// SteerMethod names the agent's steering RPC ("_session/steering" for
	// dsh, "mcode/session/steer" for mcode); empty means the agent cannot
	// steer and the send route rejects the mode. ModeMap translates the
	// bridge's plan/default work modes onto the agent's own mode ids
	// (zcode: default→yolo).
	SteerMethod string
	ModeMap     map[string]string

	// Subagents decorates this agent's views with on-disk child agents.
	Subagents Subagents

	// Canonical resolves a session/new placeholder to the backend session id
	// once the agent materializes it under a different id (zcode's lazy
	// sessions). List coverage checks consult it; nil disables resolution.
	Canonical func(id string) string

	// ListTTL bounds how long one session/list read is trusted, mirroring
	// Python's LIST_CACHE_SECONDS (30s).
	ListTTL time.Duration

	mu         sync.Mutex
	sessions   map[string]*Session
	generation uint64
	now        func() float64

	listMu   sync.Mutex
	listRows []*jsoncompat.Object
	listIDs  map[string]string
	listAt   time.Time
}

func New(executable string) *Manager {
	return &Manager{Executable: executable, sessions: map[string]*Session{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
		InitializeTimeout: 10 * time.Second, ListTTL: 30 * time.Second}
}

func NewCommand(command []string) *Manager {
	return &Manager{Command: append([]string{}, command...), sessions: map[string]*Session{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
		InitializeTimeout: 10 * time.Second, ListTTL: 30 * time.Second}
}

func (m *Manager) initializeTimeout() time.Duration {
	if m.InitializeTimeout > 0 {
		return m.InitializeTimeout
	}
	return 10 * time.Second
}

func (m *Manager) listTTL() time.Duration {
	if m.ListTTL > 0 {
		return m.ListTTL
	}
	return 30 * time.Second
}

func (m *Manager) start() (*acp.Process, error) {
	if len(m.Command) > 0 {
		return acp.Start(m.Command)
	}
	if m.Executable == "" {
		return nil, errors.New("未找到 Agent 命令，请先安装")
	}
	return acp.Start([]string{m.Executable, "acp"})
}

// List returns the agent's own session/list rows, cached for ListTTL so the
// phone's list refreshes do not respawn the agent on every request. The
// handshake uses InitializeTimeout; the session/list request uses timeout.
func (m *Manager) List(timeout time.Duration) ([]*jsoncompat.Object, error) {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if time.Since(m.listAt) < m.listTTL() && m.listIDs != nil {
		return m.listRows, nil
	}
	rows, err := m.fetchListLocked(timeout)
	if err != nil {
		// A failed refresh keeps serving the previous snapshot like Python's
		// cache-on-error behavior (_fetch_rows raises, list keeps the cache).
		if m.listIDs != nil {
			return m.listRows, nil
		}
		return nil, err
	}
	return rows, nil
}

// RefreshList expires the cached session/list read so the next List goes to
// a freshly spawned agent process (mirrors AcpAdapter.refresh_list for the
// periodic discovery cycle).
func (m *Manager) RefreshList() {
	m.listMu.Lock()
	m.listAt = time.Time{}
	m.listMu.Unlock()
}

func (m *Manager) fetchListLocked(timeout time.Duration) ([]*jsoncompat.Object, error) {
	process, err := m.start()
	if err != nil {
		return nil, err
	}
	defer process.Close()
	if _, err = process.Initialize(m.initializeTimeout()); err != nil {
		return nil, err
	}
	value, err := process.Request("session/list", jsoncompat.NewObject(), timeout)
	if err != nil {
		return nil, err
	}
	result, _ := value.(*jsoncompat.Object)
	if result == nil {
		m.listRows, m.listIDs, m.listAt = []*jsoncompat.Object{}, map[string]string{}, time.Now()
		return m.listRows, nil
	}
	rows, _ := result.MustGet("sessions").(*jsoncompat.Array)
	output := []*jsoncompat.Object{}
	ids := map[string]string{}
	for _, item := range rows.Items {
		if row, ok := item.(*jsoncompat.Object); ok {
			output = append(output, row)
			if id, ok := row.MustGet("sessionId").(string); ok && id != "" {
				cwd, _ := row.MustGet("cwd").(string)
				ids[id] = cwd
			}
		}
	}
	m.listRows, m.listIDs, m.listAt = output, ids, time.Now()
	return output, nil
}

// MergePending appends rows for sessions created through this manager that
// the agent's own session/list does not cover yet (zcode's lazy placeholder
// before the first prompt, claude before the first persisted transcript).
// A pending id is dropped once it — or its canonical backend id — is listed.
func (m *Manager) MergePending(rows []*jsoncompat.Object) []*jsoncompat.Object {
	listed := map[string]bool{}
	for _, row := range rows {
		if id, ok := row.MustGet("sessionId").(string); ok && id != "" {
			listed[id] = true
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, session := range m.sessions {
		if session == nil || session.IsClosed() || session.Title == "" {
			// Only bridge-created sessions count as pending (Python's
			// _pending_created); opened ones come from the list anyway.
			continue
		}
		if listed[id] {
			continue
		}
		if canonical := m.canonicalLocked(id); canonical != "" && listed[canonical] {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", id)
		row.Set("title", session.Title)
		row.Set("cwd", session.Cwd)
		row.Set("updatedAt", jsoncompat.Num(int(session.CreatedAtMs)))
		rows = append(rows, row)
	}
	return rows
}

func (m *Manager) canonicalLocked(id string) string {
	if m.Canonical == nil || id == "" {
		return ""
	}
	// sess_-prefixed ids are already backend sessions, not placeholders.
	if len(id) >= 5 && id[:5] == "sess_" {
		return ""
	}
	return m.Canonical(id)
}

// Knows reports whether the id belongs to this manager: a live session, a
// cached session/list row, or a placeholder whose canonical id is listed.
// It never spawns; Pair with List for a cold-cache probe.
func (m *Manager) Knows(id string) bool {
	if id == "" {
		return false
	}
	m.mu.Lock()
	_, live := m.sessions[id]
	m.mu.Unlock()
	if live {
		return true
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if m.listIDs == nil {
		return false
	}
	if _, listed := m.listIDs[id]; listed {
		return true
	}
	canonical := m.canonicalUnlocked(id)
	if canonical == "" {
		return false
	}
	_, listed := m.listIDs[canonical]
	return listed
}

// HasListCache reports whether at least one session/list read succeeded.
func (m *Manager) HasListCache() bool {
	m.listMu.Lock()
	defer m.listMu.Unlock()
	return m.listIDs != nil
}

// CwdFor returns the working directory the cached list rows carry for id, or
// the live session's cwd; "" when unknown.
func (m *Manager) CwdFor(id string) string {
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session != nil {
		return session.Cwd
	}
	m.listMu.Lock()
	defer m.listMu.Unlock()
	if cwd, ok := m.listIDs[id]; ok && cwd != "" {
		return cwd
	}
	canonical := m.canonicalUnlocked(id)
	if canonical != "" {
		return m.listIDs[canonical]
	}
	return ""
}

func (m *Manager) canonicalUnlocked(id string) string {
	if m.Canonical == nil || id == "" {
		return ""
	}
	if len(id) >= 5 && id[:5] == "sess_" {
		return ""
	}
	return m.Canonical(id)
}

func (m *Manager) openFresh(id, cwd string, timeout time.Duration) (*Session, error) {
	process, err := m.start()
	if err != nil {
		return nil, err
	}
	session := &Session{ID: id, Cwd: cwd, Process: process,
		View: acpview.NewView(id, nil, cwd, m.now), active: true, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	process.OnNotification = func(message *jsoncompat.Object) { session.onNotification(message) }
	process.OnRequest = func(message *jsoncompat.Object, respond func(any)) { session.onRequest(message, respond) }
	process.OnClose = session.markProcessClosed
	value, err := process.Initialize(timeout)
	if err != nil {
		session.Close()
		return nil, err
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", id)
	params.Set("cwd", cwd)
	params.Set("mcpServers", &jsoncompat.Array{})
	result, err := process.Request("session/load", params, timeout)
	if err != nil {
		session.Close()
		return nil, err
	}
	session.applySessionResult(value, result)
	return session, nil
}

func (m *Manager) Open(id, cwd string, timeout time.Duration) (*Session, error) {
	m.mu.Lock()
	if session := m.sessions[id]; session != nil && !session.IsClosed() {
		m.mu.Unlock()
		return session, nil
	}
	m.mu.Unlock()
	fresh, err := m.openFresh(id, cwd, timeout)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if session := m.sessions[id]; session != nil && !session.IsClosed() {
		m.mu.Unlock()
		fresh.Close()
		return session, nil
	}
	m.generation++
	fresh.generation = m.generation
	m.sessions[id] = fresh
	m.mu.Unlock()
	return fresh, nil
}

// Reconcile creates and fully loads a new ACP handle before replacing the
// current one. Readers never observe a half-loaded session.
func (m *Manager) Reconcile(id, cwd string, timeout time.Duration) (*Session, error) {
	fresh, err := m.openFresh(id, cwd, timeout)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	old := m.sessions[id]
	if old != nil {
		old.mu.Lock()
		old.active = false
		old.mu.Unlock()
	}
	m.generation++
	fresh.generation = m.generation
	m.sessions[id] = fresh
	m.mu.Unlock()
	if old != nil {
		old.Close()
	}
	return fresh, nil
}

// Create is the lifecycle entry for a new ACP conversation. Providers that
// support session/load use Open for existing ids; creation keeps the same
// process ownership and sends session/new. The returned session's ID is the
// agent's native id — for zcode a lazy placeholder the agent only
// materializes on the first prompt, which is exactly why the creating
// process must stay alive as the session handle.
func (m *Manager) Create(id, cwd, title string, timeout time.Duration) (*Session, error) {
	process, err := m.start()
	if err != nil {
		return nil, err
	}
	createdAt := int64(m.now() * 1000)
	session := &Session{ID: id, Cwd: cwd, Title: title, CreatedAtMs: createdAt, Process: process,
		View: acpview.NewView(id, title, cwd, m.now), active: true, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	process.OnNotification = func(message *jsoncompat.Object) { session.onNotification(message) }
	process.OnRequest = func(message *jsoncompat.Object, respond func(any)) { session.onRequest(message, respond) }
	process.OnClose = session.markProcessClosed
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	initializeResult, err := process.Initialize(timeout)
	if err != nil {
		session.Close()
		return nil, err
	}
	params := jsoncompat.NewObject()
	params.Set("cwd", cwd)
	params.Set("mcpServers", &jsoncompat.Array{})
	value, err := process.Request("session/new", params, timeout)
	if err != nil {
		session.Close()
		return nil, err
	}
	if result, ok := value.(*jsoncompat.Object); ok {
		if native, ok := result.MustGet("sessionId").(string); ok && native != "" && native != id {
			session.ID = native
			m.mu.Lock()
			delete(m.sessions, id)
			m.sessions[native] = session
			m.mu.Unlock()
		}
	}
	session.applySessionResult(initializeResult, value)
	session.mu.Lock()
	session.signalLocked()
	session.mu.Unlock()
	return session, nil
}

// applySessionResult folds the advertised configOptions/modes (from either
// initialize or session/new/session/load responses) into the view so the
// model catalog and settings validator see the agent's own options.
func (s *Session) applySessionResult(initialize, sessionResult any) {
	for _, value := range []any{initialize, sessionResult} {
		result, ok := value.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if options, ok := result.MustGet("configOptions").(*jsoncompat.Array); ok && len(options.Items) > 0 {
			s.mu.Lock()
			s.configOptions = options
			s.mu.Unlock()
		}
		if modes, ok := result.MustGet("modes").(*jsoncompat.Object); ok {
			s.mu.Lock()
			s.modes = modes
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.View.ApplyConfig(s.configOptions, s.modes)
}

// ConfigOptions returns a copy of the advertised config options.
func (s *Session) ConfigOptions() *jsoncompat.Array {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.configOptions == nil {
		return &jsoncompat.Array{}
	}
	clone, _ := jsoncompat.Clone(s.configOptions).(*jsoncompat.Array)
	if clone == nil {
		return &jsoncompat.Array{}
	}
	return clone
}

func (s *Session) Prompt(text string, idleTimeout time.Duration) error {
	return s.PromptWithBlocks(text, nil, idleTimeout)
}

func (s *Session) PromptWithBlocks(text string, extra []*jsoncompat.Object, idleTimeout time.Duration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.promptRunning {
		s.mu.Unlock()
		return errors.New("当前已有任务正在运行")
	}
	s.promptRunning = true
	s.View.BeginTurn(text)
	s.signalLocked()
	slice := s.activitySlice
	s.mu.Unlock()
	block := jsoncompat.NewObject()
	block.Set("type", "text")
	block.Set("text", text)
	items := []any{block}
	for _, extraBlock := range extra {
		if extraBlock != nil {
			items = append(items, extraBlock)
		}
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("prompt", &jsoncompat.Array{Items: items})
	ident, channel, submitErr := s.Process.Submit("session/prompt", params)
	if submitErr != nil {
		s.mu.Lock()
		s.promptRunning = false
		s.View.FailTurn(submitErr.Error())
		s.signalLocked()
		s.mu.Unlock()
		return nil
	}
	go s.awaitPrompt(ident, channel, idleTimeout, slice)
	return nil
}

// awaitPrompt mirrors _wait_prompt: wait in fixed slices (Python
// ACTIVITY_SLICE), staying alive while the agent streams updates or holds a
// pending approval. Idle past the limit cancels the agent-side turn, answers
// pending reverse requests as cancelled, discards the orphaned answer and
// fails the turn — otherwise every later prompt is rejected with "another
// turn is already in progress".
func (s *Session) awaitPrompt(ident string, channel <-chan acp.Delivery, idleTimeout, slice time.Duration) {
	if slice <= 0 {
		slice = 30 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = 3600 * time.Second
	}
	for {
		select {
		case outcome := <-channel:
			s.mu.Lock()
			s.promptRunning = false
			if outcome.Err != nil {
				s.View.FailTurn(outcome.Err.Error())
			} else {
				stopReason := "end_turn"
				if object, ok := outcome.Value.(*jsoncompat.Object); ok {
					if value, ok := object.MustGet("stopReason").(string); ok && value != "" {
						stopReason = value
					}
				}
				s.View.EndTurn(stopReason)
			}
			s.signalLocked()
			s.mu.Unlock()
			return
		case <-time.After(slice):
			s.mu.Lock()
			pending := len(s.pending) > 0
			last := s.lastActivity
			s.mu.Unlock()
			if pending || time.Since(last) < idleTimeout {
				continue
			}
			_ = s.Process.Notify("session/cancel", sessionParams(s.ID))
			s.flushResponders()
			s.Process.Discard(ident)
			s.mu.Lock()
			s.promptRunning = false
			s.View.FailTurn("Agent 响应超时")
			s.signalLocked()
			s.mu.Unlock()
			return
		}
	}
}

// flushResponders mirrors _flush_responders: answer every pending reverse
// request as cancelled (input requests with an action, permissions with the
// nested outcome object).
func (s *Session) flushResponders() {
	type responder struct {
		rawID any
		kind  string
	}
	s.mu.Lock()
	queue := []responder{}
	for requestID, rawID := range s.pendingIDs {
		queue = append(queue, responder{rawID: rawID, kind: s.pendingKinds[requestID]})
	}
	s.pending = map[string]*jsoncompat.Object{}
	s.pendingIDs = map[string]any{}
	s.pendingKinds = map[string]string{}
	s.View.Requests = nil
	s.signalLocked()
	s.mu.Unlock()
	for _, item := range queue {
		result := jsoncompat.NewObject()
		if item.kind == "input" {
			result.Set("action", "cancel")
		} else {
			outcome := jsoncompat.NewObject()
			outcome.Set("outcome", "cancelled")
			result.Set("outcome", outcome)
		}
		_ = s.Process.Respond(item.rawID, result)
	}
}

// Steer injects a supplementary prompt into the running turn through the
// agent's steering RPC (dsh/mcode dispatch_turn ports). dsh answers
// outcome "injected"; anything else means the turn ended in the race
// window and the caller should fall back to a normal prompt.
func (s *Session) Steer(method, text string) (injected bool, err error) {
	s.mu.Lock()
	if s.closed || (s.Process != nil && !s.active) {
		s.mu.Unlock()
		return false, errors.New("Agent 进程已退出；操作结果未知")
	}
	s.mu.Unlock()
	block := jsoncompat.NewObject()
	block.Set("type", "text")
	block.Set("text", text)
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	if method == "mcode/session/steer" {
		params.Set("text", text)
	} else {
		params.Set("prompt", &jsoncompat.Array{Items: []any{block}})
		meta := jsoncompat.NewObject()
		steering := jsoncompat.NewObject()
		steering.Set("idleBehavior", "promptRequired")
		meta.Set("steering", steering)
		params.Set("_meta", meta)
	}
	result, err := s.Process.Request(method, params, 30*time.Second)
	if err != nil {
		return false, err
	}
	outcome := ""
	if object, ok := result.(*jsoncompat.Object); ok {
		outcome, _ = object.MustGet("outcome").(string)
	}
	return outcome == "injected", nil
}

// SetMode switches the session's work mode (AcpAdapter._set_mode port):
// the requested id must be advertised, and a no-op when already current.
func (s *Session) SetMode(modeID string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	modes := s.modes
	s.mu.Unlock()
	if modes == nil {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	available, _ := modes.MustGet("availableModes").(*jsoncompat.Array)
	found := false
	if available != nil {
		for _, item := range available.Items {
			if mode, ok := item.(*jsoncompat.Object); ok {
				if id, _ := mode.MustGet("id").(string); id == modeID {
					found = true
					break
				}
			}
		}
	}
	if !found {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	if current, _ := modes.MustGet("currentModeId").(string); current == modeID {
		return nil
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("modeId", modeID)
	if _, err := s.Process.Request("session/set_mode", params, 30*time.Second); err != nil {
		return err
	}
	s.mu.Lock()
	s.modes.Set("currentModeId", modeID)
	s.View.SetMode(modeID)
	s.signalLocked()
	s.mu.Unlock()
	return nil
}

func (s *Session) Cancel() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	s.promptRunning = false
	s.View.EndTurn("cancelled")
	s.signalLocked()
	s.mu.Unlock()
	return s.Process.Notify("session/cancel", sessionParams(s.ID))
}

func (s *Session) Respond(requestID string, response *jsoncompat.Object) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	rawID, exists := s.pendingIDs[requestID]
	if _, ok := s.pending[requestID]; !ok || !exists {
		s.mu.Unlock()
		return errors.New("请求已过期，请刷新聊天")
	}
	delete(s.pending, requestID)
	delete(s.pendingIDs, requestID)
	delete(s.pendingKinds, requestID)
	s.View.RemovePermission(requestID)
	s.signalLocked()
	s.mu.Unlock()
	return s.Process.Respond(rawID, response)
}

// UpdateSettings mirrors AcpAdapter.update_settings: validate model and
// effort against the advertised config options, then apply each change with
// session/set_config_option. fastMode is rejected like Python.
func (m *Manager) UpdateSettings(session *Session, model, effort string, fastMode *bool) error {
	if fastMode != nil {
		return errors.New("当前 Agent 不支持 Fast 模式")
	}
	options := session.ConfigOptions()
	modelOption := findCategory(options, "model")
	if modelOption == nil {
		return errors.New("当前 Agent 不支持切换模型")
	}
	if model != "" {
		values := optionValues(modelOption)
		if len(values) > 0 {
			if _, ok := values[model]; !ok {
				return errors.New("此 Agent 不支持所选模型")
			}
		}
		if current, _ := modelOption.MustGet("currentValue").(string); current != model {
			if err := m.setConfig(session, modelOption, model); err != nil {
				return err
			}
		}
	}
	if effort != "" {
		thinking := findCategory(options, "thought_level")
		if thinking == nil {
			thinking = findCategory(options, "thinking")
		}
		if thinking != nil {
			mapped := m.EffortMap[effort]
			if mapped == "" {
				return errors.New("此 Agent 不支持所选思考强度")
			}
			values := optionValues(thinking)
			if len(values) > 0 {
				if _, ok := values[mapped]; !ok {
					return errors.New("此 Agent 不支持所选思考强度")
				}
			}
			if current, _ := thinking.MustGet("currentValue").(string); current != mapped {
				if err := m.setConfig(session, thinking, mapped); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (m *Manager) setConfig(session *Session, option *jsoncompat.Object, value string) error {
	configID, _ := option.MustGet("id").(string)
	if configID == "" {
		return errors.New("当前 Agent 不支持切换模型")
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", session.ID)
	params.Set("configId", configID)
	params.Set("value", value)
	result, err := session.Process.Request("session/set_config_option", params, 30*time.Second)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	// The response is authoritative: mcode drops thought_level for models
	// without reasoning support and that must propagate. Without a usable
	// configOptions array, patch the local copy like Python does.
	replaced := false
	if object, ok := result.(*jsoncompat.Object); ok {
		if updated, ok := object.MustGet("configOptions").(*jsoncompat.Array); ok && len(updated.Items) > 0 {
			session.configOptions = updated
			replaced = true
		}
	}
	if !replaced && session.configOptions != nil {
		for _, item := range session.configOptions.Items {
			if candidate, ok := item.(*jsoncompat.Object); ok {
				if id, _ := candidate.MustGet("id").(string); id == configID {
					candidate.Set("currentValue", value)
				}
			}
		}
	}
	session.View.ApplyConfig(session.configOptions, session.modes)
	session.signalLocked()
	return nil
}

// Catalog maps the session's advertised config options onto the phone's
// model catalog: one entry per model option, efforts folded through the
// agent's EFFORT_MAP (the exact agent value wins so kimi's low/high/max do
// not expand into the eight bridge levels). Mirrors AcpAdapter.catalog.
// commandSkillBuiltins mirrors kimi's fixed built-in slash commands: every
// other advertised command is an activatable skill.
var commandSkillBuiltins = map[string]bool{"compact": true, "status": true, "usage": true,
	"mcp": true, "tasks": true, "help": true}

// CommandSkills maps the session's advertised slash commands to picker rows
// (AcpAdapter._command_skills port).
func (m *Manager) CommandSkills(session *Session) []*jsoncompat.Object {
	rows := []*jsoncompat.Object{}
	for _, command := range session.View.Commands {
		name, _ := command.MustGet("name").(string)
		if name == "" || commandSkillBuiltins[name] {
			continue
		}
		display := name
		if strings.HasPrefix(name, "skill:") {
			display = name[len("skill:"):]
		}
		row := jsoncompat.NewObject()
		row.Set("id", name)
		row.Set("name", display)
		row.Set("displayName", display)
		row.Set("description", stringField(command, "description"))
		rows = append(rows, row)
	}
	return rows
}

// ResolveCommandSkill validates the picker's skill selection for one prompt:
// ACP agents accept a single leading slash command.
func (m *Manager) ResolveCommandSkill(session *Session, ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) > 1 {
		return "", errors.New("该 Agent 一次只能调用一个 Skill")
	}
	for _, row := range m.CommandSkills(session) {
		if id, _ := row.MustGet("id").(string); id == ids[0] {
			name, _ := row.MustGet("name").(string)
			return name, nil
		}
	}
	return "", errors.New("Skill 不可用，请刷新列表")
}

func (m *Manager) Catalog(session *Session, kindParam, query string, ids []string) *jsoncompat.Object {
	if kindParam == "skills" {
		return m.skillCatalog(session, query, ids)
	}
	options := session.ConfigOptions()
	modelOption := findCategory(options, "model")
	thinking := findCategory(options, "thought_level")
	if thinking == nil {
		thinking = findCategory(options, "thinking")
	}
	efforts := []any{}
	if thinking != nil {
		if list, ok := thinking.MustGet("options").(*jsoncompat.Array); ok {
			for _, item := range list.Items {
				entry, ok := item.(*jsoncompat.Object)
				if !ok {
					continue
				}
				value, _ := entry.MustGet("value").(string)
				if value == "" {
					continue
				}
				name := ""
				if _, isLevel := m.EffortMap[value]; isLevel {
					name = value
				} else {
					for level, mapped := range m.EffortMap {
						if mapped == value {
							name = level
							break
						}
					}
				}
				if name == "" {
					continue
				}
				duplicate := false
				for _, existing := range efforts {
					if existing == name {
						duplicate = true
						break
					}
				}
				if !duplicate {
					efforts = append(efforts, name)
				}
			}
		}
	}
	models := &jsoncompat.Array{}
	if modelOption != nil {
		if list, ok := modelOption.MustGet("options").(*jsoncompat.Array); ok {
			for _, item := range list.Items {
				entry, ok := item.(*jsoncompat.Object)
				if !ok {
					continue
				}
				value, _ := entry.MustGet("value").(string)
				if value == "" {
					continue
				}
				name, _ := entry.MustGet("name").(string)
				if name == "" {
					name = value
				}
				model := jsoncompat.NewObject()
				model.Set("id", value)
				model.Set("name", name)
				model.Set("efforts", &jsoncompat.Array{Items: efforts})
				models.Items = append(models.Items, model)
			}
		}
	}
	snapshot := session.Snapshot()
	result := jsoncompat.NewObject()
	result.Set("kind", kindParam)
	result.Set("models", models)
	result.Set("skills", &jsoncompat.Array{})
	result.Set("selectedSkills", &jsoncompat.Array{})
	result.Set("total", jsoncompat.Num(len(models.Items)))
	result.Set("currentModel", snapshot.MustGet("model"))
	result.Set("currentEffort", snapshot.MustGet("effort"))
	return result
}

func stringField(object *jsoncompat.Object, key string) string {
	text, _ := object.MustGet(key).(string)
	return text
}

func findCategory(options *jsoncompat.Array, category string) *jsoncompat.Object {
	for _, item := range options.Items {
		option, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		slot, _ := option.MustGet("category").(string)
		if slot == "" {
			slot, _ = option.MustGet("id").(string)
		}
		if slot == category {
			return option
		}
	}
	return nil
}

func optionValues(option *jsoncompat.Object) map[string]bool {
	values := map[string]bool{}
	if list, ok := option.MustGet("options").(*jsoncompat.Array); ok {
		for _, item := range list.Items {
			if entry, ok := item.(*jsoncompat.Object); ok {
				if value, ok := entry.MustGet("value").(string); ok && value != "" {
					values[value] = true
				}
			}
		}
	}
	return values
}

// Get returns the live session for id (touching its idle clock), or nil.
func (m *Manager) Get(id string) *Session {
	m.mu.Lock()
	session := m.sessions[id]
	m.mu.Unlock()
	if session != nil {
		session.Touch()
	}
	return session
}

// SweepIdle closes live sessions nobody has read or signaled for longer
// than idle (mirroring the Python maintain loop's 300s eviction). A running
// turn is cancelled first so the agent side is not left wedged. Returns the
// number of closed sessions.
func (m *Manager) SweepIdle(idle time.Duration) int {
	m.mu.Lock()
	candidates := []*Session{}
	for _, session := range m.sessions {
		candidates = append(candidates, session)
	}
	m.mu.Unlock()
	closed := 0
	for _, session := range candidates {
		if session.IdleFor() < idle {
			continue
		}
		if session.PromptRunning() {
			_ = session.Cancel()
			session.flushResponders()
		}
		session.Close()
		m.mu.Lock()
		if m.sessions[session.ID] == session {
			delete(m.sessions, session.ID)
		}
		m.mu.Unlock()
		closed++
	}
	return closed
}

// PromptRunning reports whether a turn is in flight.
func (s *Session) PromptRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.promptRunning
}

// Close tears down every live session process owned by this manager.
func (m *Manager) Close() {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

// Snapshot returns the session's current view.
func (s *Session) Snapshot() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.View.Snapshot()
}

// Sequence returns the change counter used as the projection sequence: every
// view mutation (prompt, notification, request, config change) bumps it.
func (s *Session) Sequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sequence
}

func (s *Session) Wait(timeout time.Duration) {
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	select {
	case <-changed:
	case <-time.After(timeout):
	}
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.active = false
	process := s.Process
	s.mu.Unlock()
	process.Close()
}

func (s *Session) markProcessClosed() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.active = false
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Session) onNotification(message *jsoncompat.Object) {
	method, _ := message.MustGet("method").(string)
	if method != "session/update" {
		return
	}
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		return
	}
	if update, ok := params.MustGet("update").(*jsoncompat.Object); ok {
		params = update
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.View.OnUpdate(params)
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) onRequest(message *jsoncompat.Object, respond func(any)) {
	method, _ := message.MustGet("method").(string)
	requestID := jsoncompat.PyStr(message.MustGet("id"))
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		params = jsoncompat.NewObject()
	}
	s.mu.Lock()
	if s.closed || (s.Process != nil && !s.active) {
		s.mu.Unlock()
		respond(nil)
		return
	}
	if s.pending == nil {
		s.pending = map[string]*jsoncompat.Object{}
	}
	if s.pendingIDs == nil {
		s.pendingIDs = map[string]any{}
	}
	if s.pendingKinds == nil {
		s.pendingKinds = map[string]string{}
	}
	s.pending[requestID] = params
	s.pendingIDs[requestID] = message.MustGet("id")
	switch method {
	case "session/request_permission", "session/requestPermission":
		s.pendingKinds[requestID] = "permission"
		s.View.AddPermission(requestID, params)
	case "elicitation/create":
		s.pendingKinds[requestID] = "input"
		s.View.AddInput(requestID, params)
	default:
		delete(s.pending, requestID)
		delete(s.pendingIDs, requestID)
		s.mu.Unlock()
		respond(nil)
		return
	}
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) signalLocked() {
	s.sequence++
	s.lastActivity = time.Now()
	s.touched = time.Now()
	close(s.changed)
	s.changed = make(chan struct{})
}

// Touch marks gateway-side interest: HTTP reads and SSE polls call it so the
// idle sweeper keeps sessions somebody is watching.
func (s *Session) Touch() {
	s.mu.Lock()
	s.touched = time.Now()
	s.mu.Unlock()
}

// IdleFor reports how long since the last gateway-side read or agent signal.
func (s *Session) IdleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.touched)
}

// NoteSubagents records the latest child-agent roster revision; a change
// bumps the change counter so downstream projections rebuild with the
// refreshed subagent rows. Returns the current revision.
func (s *Session) NoteSubagents(revision int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision != s.lastSubagentRevision {
		s.lastSubagentRevision = revision
		s.sequence++
		s.lastActivity = time.Now()
		close(s.changed)
		s.changed = make(chan struct{})
	}
}

func sessionParams(id string) *jsoncompat.Object {
	params := jsoncompat.NewObject()
	params.Set("sessionId", id)
	return params
}

// skillCatalog serves kind=skills: the session's advertised command skills
// (AcpAdapter.catalog's skills branch — name/description substring filter,
// page slice, selected echo).
func (m *Manager) skillCatalog(session *Session, rawQuery string, wanted []string) *jsoncompat.Object {
	rows := m.CommandSkills(session)
	filtered := rows
	if query := strings.TrimSpace(rawQuery); query != "" {
		needle := strings.ToLower(query)
		filtered = nil
		for _, row := range rows {
			hay := strings.ToLower(stringField(row, "name") + " " +
				stringField(row, "displayName") + " " + stringField(row, "description"))
			if strings.Contains(hay, needle) {
				filtered = append(filtered, row)
			}
		}
	}
	result := jsoncompat.NewObject()
	result.Set("kind", "skills")
	result.Set("models", &jsoncompat.Array{})
	skills := &jsoncompat.Array{}
	for _, row := range filtered {
		skills.Items = append(skills.Items, row)
	}
	result.Set("skills", skills)
	selected := &jsoncompat.Array{}
	for _, row := range rows {
		if id := stringField(row, "id"); id != "" {
			for _, id := range wanted {
				if id == stringField(row, "id") {
					selected.Items = append(selected.Items, row)
					break
				}
			}
		}
	}
	result.Set("selectedSkills", selected)
	result.Set("total", jsoncompat.Num(len(filtered)))
	result.Set("currentModel", nil)
	result.Set("currentEffort", nil)
	return result
}
