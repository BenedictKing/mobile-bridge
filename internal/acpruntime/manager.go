// Package acpruntime owns long-lived ACP child processes and exposes their
// normalized acpview snapshots to the Go service layer.
package acpruntime

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpadapter"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
)

type Session struct {
	ID      string
	Cwd     string
	Process *acp.Process
	View    *acpview.View

	// CreatedAtMs is immutable. Title follows session_info_update under mu.
	Title       string
	CreatedAtMs int64

	mu            sync.Mutex
	promptRunning bool
	promptDone    chan struct{}
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
	// modeOptionID mirrors Manager.ModeOptionID: non-empty routes SetMode to
	// that config option (dsh's collaboration_mode).
	modeOptionID string
	// lastActivity moves on every agent signal (streamed updates, reverse
	// requests); the prompt idle timeout measures against it.
	lastActivity time.Time
	// lastInteraction is the list recency. It stays unset while session/load
	// replays history so reopening a chat does not make it look recently used.
	lastInteraction  time.Time
	interactionReady bool
	// touched moves on gateway-side reads (HTTP/SSE polling); the idle
	// sweeper evicts sessions nobody has read for a while.
	touched time.Time
	// activitySlice is the prompt wait slice (Python ACTIVITY_SLICE); the
	// default matches, tests shrink it.
	activitySlice        time.Duration
	lastSubagentRevision int64
	// mediaDir (from Manager.MediaDir) receives decoded inline image blocks.
	mediaDir string
	// usage / kind (from Manager.Usage / Kind) persist the last context usage
	// an agent reported so a reopened view can restore it.
	usage *UsageStore
	kind  string
	// canonical (from Manager.Canonical) maps a lazy placeholder id to the
	// materialized backend id; ownsUpdate consults it so zcode's first-prompt
	// stream — which may already carry the backend id — is not dropped.
	canonical   func(id string) string
	autoApprove func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object
	// autoContinue runs asynchronously once a live turn settles; the gateway
	// installs it to resume unfinished work. autoContinueRuns is the
	// consecutive-resume budget any human action re-arms.
	autoContinue     func(*Session)
	autoContinueRuns int
	// switchResumeRuns is the account-pool switch-resume budget: a turn that
	// died on an exhausted quota may be resumed after a credential swap at
	// most MaxSwitchResume times in a row, re-armed by any human action.
	switchResumeRuns int
	// native loads the agent's own persisted conversation (ZCode's SQLite
	// store) for turns the desktop app drives, so the phone follows them
	// instead of showing only the ACP replay. nativeStop ends the poller.
	native         func(sessionID string) (*acpview.NativeSnapshot, error)
	nativeStop     chan struct{}
	nativeInterval time.Duration
	retryStatus    func(sessionID string) (*jsoncompat.Object, error)
	retrying       *jsoncompat.Object
	// nativeSettle runs when a desktop-driven turn stops running, so queued
	// phone messages dispatch exactly like after a gateway-sent turn.
	nativeSettle func(*Session)
	// nativeLagSince debounces a settled ACP tip against a store that still
	// reports the same prompt running: the store may merely lag the final
	// response, but a turn that really keeps running reclaims the view after
	// nativeStoreLagGrace.
	nativeLagSince time.Time
	// readOnly marks a view built from the agent's own transcript because the
	// desktop app owns the write handle. Reads work; mutations are refused.
	readOnly bool
	// nativeBootstrapped marks the first native fold of a handle. That fold
	// always applies so the native store (with its attachments and stable
	// ids) becomes the view; later folds keep the settled-tip dedupe so
	// ordinary gateway turns do not re-key the timeline.
	nativeBootstrapped bool
}

// MaxAutoContinue bounds consecutive automatic "继续" sends without an
// intervening human message, answer or steer.
const MaxAutoContinue = 5

// MaxSwitchResume bounds consecutive post-switch resume sends per session:
// enough to survive a switch cascade, small enough to stop a loop where every
// account is already exhausted.
const MaxSwitchResume = 3

// nativeStoreLagGrace bounds how long a settled ACP tip may hold off a store
// copy that still reports the same prompt running before the store wins.
const nativeStoreLagGrace = 5 * time.Second

// ReadOnlySessionMessage refuses mutations on a session the desktop app owns:
// the phone can read the native transcript but cannot prompt, steer, answer
// approvals or change settings while the app holds the write handle.
const ReadOnlySessionMessage = "此会话正在桌面 App 中运行，手机端只读；可先对消息「从这里分支」再在手机继续"

// ErrRequestExpired is the sentinel behind Respond's "请求已过期，请刷新聊天"
// (the pending request is gone: answered elsewhere, flushed by the idle sweeper
// or the turn ended). Exported so orchestration layers (team questions, §5.4)
// can errors.Is it to withdraw their registration; the HTTP text is unchanged.
var ErrRequestExpired = errors.New("请求已过期，请刷新聊天")

// Subagents is the on-disk child-agent source decorating an agent's views
// (Python's native_subagents stores). Nil disables decoration.
type Subagents interface {
	// Activity returns the flattened rows plus the roster revision in one
	// read; implementations cache by signature.
	Activity(parentID string) subagents.Activity
}

type Manager struct {
	Executable  string
	Command     []string
	HostSocket  string
	AutoApprove func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object

	// AutoContinue, when set, is invoked (in its own goroutine) every time a
	// live turn settles — completion, failure or idle timeout — so the
	// gateway can judge whether unfinished work should resume. The hook must
	// re-check liveness itself; turns settle while the session stays open.
	AutoContinue func(*Session)

	// Label, Capabilities and EffortMap mirror the Python adapter profile
	// (bridge/agents/*.py): list rows, views and the settings validator all
	// read them. InitializeTimeout is the handshake budget — agents launched
	// through npx need the long 180s first-run download window.
	Label             string
	Capabilities      []string
	EffortMap         map[string]string
	InitializeTimeout time.Duration

	// SteerMethod names the agent's steering RPC ("_session/steering" for
	// claude/dsh, "mcode/session/steer" for mcode); empty means the agent cannot
	// steer and the send route rejects the mode. ModeMap translates the
	// bridge's plan/default work modes onto the agent's own mode ids
	// (zcode: default→yolo).
	SteerMethod string
	ModeMap     map[string]string

	// ModeOptionID names the dialect whose collaboration surface is the
	// config option with this id instead of the modes object (dsh:
	// collaboration_mode — its modes object and "mode" option are permission
	// presets, and it implements neither session/set_mode nor a mode
	// category). Sessions stamp it at creation so SetMode and the view fold
	// onto the right surface.
	ModeOptionID string

	// Subagents decorates this agent's views with on-disk child agents.
	Subagents Subagents

	// Native, when set, loads the agent's own persisted conversation (ZCode's
	// SQLite store). The ACP wrapper only streams turns it runs itself; a turn
	// started in the desktop app must be followed from this store or the phone
	// sees it minutes late. Nil disables native following.
	Native func(sessionID string) (*acpview.NativeSnapshot, error)
	// NativeReset drops the store's per-session change cache so a freshly
	// attached handle always rebuilds once.
	NativeReset func(sessionID string)
	// NativeInterval is the poll cadence; zero means 800ms.
	NativeInterval time.Duration
	// NativeClose releases the native store when the manager shuts down.
	NativeClose func() error
	// NativeSettle runs when a desktop-driven native turn settles. The
	// gateway chains the queue dispatcher here; auto-continue deliberately
	// stays off — a turn the user drives on the desktop is not ours to resume.
	NativeSettle func(*Session)
	// NativeMeta fills list-row facts the agent wrapper omits: DSH sessions
	// the desktop app owns carry no title in dsh-acp's session/list and a
	// creation-time updatedAt. It returns (title, last activity ms).
	NativeMeta func(sessionID string) (string, int64)
	// NativeTranscript loads a session's read-only conversation from the
	// agent's own store. When the desktop app owns the live write handle and
	// session/load is refused, the gateway falls back to this view instead of
	// failing the open (DSH: the zstd transcript).
	NativeTranscript func(sessionID string) (*acpview.NativeSnapshot, error)
	// NativeList, when set, enumerates sessions from the agent's own on-disk
	// store as session/list-shaped rows. It supplements incomplete ACP pages
	// (kimi) and is the fallback for agents without session/list (workbuddy).
	// ACP rows win when the same sessionId appears in both sources.
	NativeList func() []*jsoncompat.Object
	// RetryStatus supplements agents whose ACP stream omits provider retries.
	RetryStatus func(sessionID string) (*jsoncompat.Object, error)

	// Canonical resolves a session/new placeholder to the backend session id
	// once the agent materializes it under a different id (zcode's lazy
	// sessions). List coverage checks consult it; nil disables resolution.
	Canonical func(id string) string

	// LazySessions marks agents that only materialize a session in their own
	// store after the first prompt (see acpagents.Profile.LazySessions). A
	// session created on someone's behalf — a team member slot nobody has
	// messaged yet — is unloadable after a restart, so callers that create
	// one without sending anything right away call Session.Warmup.
	LazySessions bool

	// ListTTL bounds how long one session/list read is trusted, mirroring
	// Python's LIST_CACHE_SECONDS (30s).
	ListTTL time.Duration

	// MediaDir persists decoded inline image blocks (ACP image content with
	// base64 data) as per-session files so the phone can preview them; empty
	// disables persistence and the view drops such blocks like before.
	MediaDir string

	// Usage remembers the last context usage an agent reported per session.
	// Agents only report usage inside a live turn and views are rebuilt on
	// every open, so the ledger is what keeps the phone's gauge populated
	// across a gateway restart or the idle sweep. Nil disables the feature.
	Usage *UsageStore
	// Created remembers the sessions this manager created through session/new
	// (id → cwd/title). Agents only list a session after its first turn, so
	// without the ledger a created-but-unused chat would vanish from the list
	// and its team member would read as unavailable. Nil disables the feature.
	Created *CreatedStore
	// Kind namespaces the per-agent ledgers (Usage, Created) and the discovery
	// probes: session ids are unique per agent, and the same id string may
	// belong to different agents.
	Kind string

	mu         sync.Mutex
	sessions   map[string]*Session
	generation uint64
	now        func() float64
	// opening single-flights concurrent Open calls per session id: every
	// caller used to openFresh on its own, which through a shared Agent Host
	// connection-per-open steals the session's update routing from the
	// surviving session (the host routes by sessionId to the last
	// registrant), leaving the opened chat permanently empty.
	opening map[string]chan struct{}

	listMu   sync.Mutex
	listRows []*jsoncompat.Object
	listIDs  map[string]string
	listAt   time.Time
}

func New(executable string) *Manager {
	return &Manager{Executable: executable, sessions: map[string]*Session{}, opening: map[string]chan struct{}{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
		InitializeTimeout: 10 * time.Second, ListTTL: 30 * time.Second}
}

func NewCommand(command []string) *Manager {
	return &Manager{Command: append([]string{}, command...), sessions: map[string]*Session{}, opening: map[string]chan struct{}{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
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
	if m.HostSocket != "" {
		return acp.StartSocket(m.HostSocket)
	}
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
	// cacheRows stamps the list cache and builds the id→cwd index every list
	// source (ACP or native) shares.
	cacheRows := func(rows []*jsoncompat.Object) []*jsoncompat.Object {
		if rows == nil {
			rows = []*jsoncompat.Object{}
		}
		ids := map[string]string{}
		for _, row := range rows {
			if id, ok := row.MustGet("sessionId").(string); ok && id != "" {
				cwd, _ := row.MustGet("cwd").(string)
				ids[id] = cwd
			}
		}
		m.listRows, m.listIDs, m.listAt = rows, ids, time.Now()
		return rows
	}
	output := []*jsoncompat.Object{}
	seenSessions := map[string]bool{}
	seenCursors := map[string]bool{}
	params := jsoncompat.NewObject()
	for page := 0; ; page++ {
		if page >= 100 {
			return nil, errors.New("session/list 分页超过 100 页")
		}
		value, requestErr := process.Request("session/list", params, timeout)
		if requestErr != nil {
			var acpErr *acp.Error
			if errors.As(requestErr, &acpErr) && methodNotFound(acpErr) {
				// session/list is optional in ACP; agents without it (older
				// claude-agent-acp, workbuddy's codebuddy --acp) answer -32601.
				// A wired native store fills the gap; otherwise cache an empty
				// list instead of failing so the phone list does not nag on
				// every refresh.
				if m.NativeList != nil {
					return cacheRows(m.NativeList()), nil
				}
				return cacheRows(nil), nil
			}
			return nil, requestErr
		}
		result, _ := value.(*jsoncompat.Object)
		if result == nil {
			if page == 0 {
				return cacheRows(nil), nil
			}
			break
		}
		rows, _ := result.MustGet("sessions").(*jsoncompat.Array)
		if rows != nil {
			for _, item := range rows.Items {
				row, ok := item.(*jsoncompat.Object)
				if !ok {
					continue
				}
				id, _ := row.MustGet("sessionId").(string)
				if id != "" && seenSessions[id] {
					continue
				}
				seenSessions[id] = true
				output = append(output, row)
			}
		}
		cursor, _ := result.MustGet("nextCursor").(string)
		if cursor == "" || seenCursors[cursor] {
			break
		}
		seenCursors[cursor] = true
		params.Set("cursor", cursor)
	}
	if m.NativeList != nil {
		rowsByID := map[string]*jsoncompat.Object{}
		for _, row := range output {
			id, _ := row.MustGet("sessionId").(string)
			rowsByID[id] = row
		}
		for _, row := range m.NativeList() {
			if row == nil {
				continue
			}
			id, _ := row.MustGet("sessionId").(string)
			if id == "" {
				continue
			}
			if seenSessions[id] {
				current, _ := rowsByID[id].MustGet("title").(string)
				title, _ := row.MustGet("title").(string)
				if strings.TrimSpace(current) == "" && strings.TrimSpace(title) != "" {
					rowsByID[id].Set("title", title)
				}
				continue
			}
			seenSessions[id] = true
			output = append(output, row)
		}
	}
	if m.NativeMeta != nil {
		for _, row := range output {
			id, _ := row.MustGet("sessionId").(string)
			if id == "" {
				continue
			}
			title, updated := m.NativeMeta(id)
			if title != "" {
				if current, _ := row.MustGet("title").(string); strings.TrimSpace(current) == "" {
					row.Set("title", title)
				}
			}
			if updated > 0 {
				row.Set("updatedAt", jsoncompat.Num(int(updated)))
			}
		}
	}
	return cacheRows(output), nil
}

// methodNotFound reports whether the agent answered a request with the
// JSON-RPC -32601 code, i.e. it does not implement the method at all.
func methodNotFound(err *acp.Error) bool {
	switch code := err.Code.(type) {
	case json.Number:
		return code.String() == "-32601"
	case float64:
		return code == -32601
	}
	return false
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
	// rows may alias the cached session/list slice returned by List to many
	// concurrent callers; copy before appending so concurrent MergePending
	// calls never write into the shared backing array.
	rows = append(rows[:0:0], rows...)
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, session := range m.sessions {
		if session == nil || session.IsClosed() || session.CreatedAtMs == 0 {
			// Only bridge-created sessions count as pending; opened ones have no
			// creation timestamp and come from the list anyway.
			continue
		}
		session.mu.Lock()
		title := session.Title
		session.mu.Unlock()
		if title == "" {
			title = "未命名聊天"
		}
		if listed[id] {
			continue
		}
		if canonical := m.canonicalLocked(id); canonical != "" && listed[canonical] {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", id)
		row.Set("title", title)
		row.Set("cwd", session.Cwd)
		row.Set("updatedAt", jsoncompat.Num(int(session.CreatedAtMs)))
		rows = append(rows, row)
		listed[id] = true
	}
	// Sessions created earlier keep their list row through an idle sweep, a
	// reopen (open has no creation timestamp) or a restart.
	for _, row := range m.Created.Rows(m.Kind) {
		id, _ := row.MustGet("sessionId").(string)
		if id == "" || listed[id] {
			continue
		}
		if canonical := m.canonicalLocked(id); canonical != "" && listed[canonical] {
			continue
		}
		listed[id] = true
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
	// A chat this gateway created but the agent does not list yet (no first
	// turn) is still ours: the created ledger carries its cwd.
	if _, created := m.Created.Lookup(m.Kind, id); created {
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

// Aliases returns the other ids the same chat is listed under: a lazy
// session/new placeholder and the backend id it materialized into on the
// first turn. Deleting a chat must hide every alias, or the row comes back
// under whichever id the agent's own list uses.
func (m *Manager) Aliases(id string) []string {
	if id == "" {
		return nil
	}
	aliases := []string{}
	if canonical := m.canonicalLocked(id); canonical != "" && canonical != id {
		aliases = append(aliases, canonical)
	}
	for _, row := range m.Created.Rows(m.Kind) {
		other, _ := row.MustGet("sessionId").(string)
		if other == "" || other == id {
			continue
		}
		if m.canonicalLocked(other) == id {
			aliases = append(aliases, other)
		}
	}
	return aliases
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
	if record, created := m.Created.Lookup(m.Kind, id); created && record.Cwd != "" {
		return record.Cwd
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
		View: acpview.NewView(id, nil, cwd, m.now), active: true, mediaDir: m.MediaDir, usage: m.Usage, kind: m.Kind, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	session.autoApprove = m.AutoApprove
	session.autoContinue = m.AutoContinue
	session.modeOptionID = m.ModeOptionID
	session.canonical = m.Canonical
	session.View.ModeOption = m.ModeOptionID
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
	session.mu.Lock()
	session.interactionReady = true
	session.mu.Unlock()
	if m.NativeMeta != nil {
		if title, _ := m.NativeMeta(id); title != "" {
			session.Title = title
			session.View.Title = title
		}
	}
	m.seedUsage(session)
	m.startNative(session)
	return session, nil
}

func (m *Manager) Open(id, cwd string, timeout time.Duration) (*Session, error) {
	for {
		m.mu.Lock()
		if session := m.sessions[id]; session != nil && !session.IsClosed() {
			m.mu.Unlock()
			return session, nil
		}
		if done, ok := m.opening[id]; ok {
			// Another caller is opening this id; wait and re-check instead of
			// opening a parallel handle. Through an Agent Host every open is a
			// new connection that re-registers the sessionId routing, so a
			// parallel open steals the survivor's update stream.
			m.mu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		m.opening[id] = done
		m.mu.Unlock()

		fresh, err := m.openFresh(id, cwd, timeout)
		if err != nil {
			if native := m.openNative(id, cwd, err); native != nil {
				fresh = native
				err = nil
			}
		}
		m.mu.Lock()
		delete(m.opening, id)
		close(done)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.generation++
		fresh.generation = m.generation
		m.sessions[id] = fresh
		m.mu.Unlock()
		return fresh, nil
	}
}

// Reconcile creates and fully loads a new ACP handle before replacing the
// current one. Readers never observe a half-loaded session.
func (m *Manager) Reconcile(id, cwd string, timeout time.Duration) (*Session, error) {
	fresh, err := m.openFresh(id, cwd, timeout)
	if err != nil {
		if native := m.openNative(id, cwd, err); native != nil {
			fresh = native
		} else {
			return nil, err
		}
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
		View: acpview.NewView(id, title, cwd, m.now), active: true, mediaDir: m.MediaDir, usage: m.Usage, kind: m.Kind, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	session.interactionReady = true
	session.autoApprove = m.AutoApprove
	session.autoContinue = m.AutoContinue
	session.modeOptionID = m.ModeOptionID
	session.canonical = m.Canonical
	session.View.ModeOption = m.ModeOptionID
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
	// The agent only lists this session after its first turn; remember it so
	// the chat stays in the phone list and resolvable until then.
	m.Created.Remember(m.Kind, session.ID, cwd, title, createdAt)
	m.startNative(session)
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

func (s *Session) PromptWithBlocks(text string, extra []*jsoncompat.Object, idleTimeout time.Duration, files ...acpadapter.FileSpec) error {
	return s.prompt(text, extra, idleTimeout, budgetHuman, files...)
}

// AutoContinuePrompt sends the automatic resume message: the same prompt
// path, but it consumes instead of re-arming the consecutive auto-continue
// budget and never resets what a human action armed.
func (s *Session) AutoContinuePrompt(text string, idleTimeout time.Duration) error {
	return s.prompt(text, nil, idleTimeout, budgetAutoContinue)
}

// ResumeAfterSwitchPrompt resumes a session whose turn died on an exhausted
// quota after the account pool swapped credentials. It draws on its own
// switch-resume budget (MaxSwitchResume) so the accidental-interruption
// budget stays intact for genuine stalls.
func (s *Session) ResumeAfterSwitchPrompt(text string, idleTimeout time.Duration) error {
	return s.prompt(text, nil, idleTimeout, budgetSwitchResume)
}

// WarmupPrompt is the throwaway first turn sent to a freshly created session
// of a lazy agent. It asks for the shortest possible answer and forbids work
// so the turn costs one tiny exchange and leaves the workspace untouched.
const WarmupPrompt = "（会话初始化）这是网关自动发送的预热消息，用于确认会话可用。" +
	"请只回复「待命」，不要调用任何工具，不要读写或修改任何文件，也不要追问。"

// Warmup runs the first turn of a session nobody is going to prompt right
// away (a team member slot created by the gateway). Lazy agents only
// materialize a session on its first prompt; without one the session cannot
// be reloaded after an idle sweep or a restart, and the member shows as
// unavailable. The turn is a system action: it neither re-arms the human
// budgets nor triggers auto-continue when it settles.
func (s *Session) Warmup(idleTimeout time.Duration) error {
	return s.prompt(WarmupPrompt, nil, idleTimeout, budgetWarmup)
}

// promptBudget selects which consecutive-send budget a prompt draws on.
type promptBudget int

const (
	budgetHuman promptBudget = iota
	budgetAutoContinue
	budgetSwitchResume
	budgetWarmup
)

func (s *Session) prompt(text string, extra []*jsoncompat.Object, idleTimeout time.Duration, budget promptBudget, files ...acpadapter.FileSpec) error {
	attachments := make([]*jsoncompat.Object, 0, len(files))
	for _, file := range files {
		attachment := jsoncompat.NewObject()
		attachment.Set("type", "file")
		if file.Image {
			attachment.Set("type", "localImage")
		}
		attachment.Set("path", file.Path)
		attachment.Set("name", file.Name)
		attachments = append(attachments, attachment)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	if s.promptRunning {
		s.mu.Unlock()
		return errors.New("当前已有任务正在运行")
	}
	warmup := budget == budgetWarmup
	switch budget {
	case budgetAutoContinue:
		if s.autoContinueRuns >= MaxAutoContinue {
			s.mu.Unlock()
			return errors.New("连续自动继续已达上限")
		}
		s.autoContinueRuns++
	case budgetSwitchResume:
		if s.switchResumeRuns >= MaxSwitchResume {
			s.mu.Unlock()
			return errors.New("切换后自动继续已达上限")
		}
		s.switchResumeRuns++
	case budgetWarmup:
		// A warm-up is neither a human message nor a resume: leave both
		// budgets exactly as they are.
	default:
		// Any human message re-arms both budgets.
		s.autoContinueRuns = 0
		s.switchResumeRuns = 0
	}
	s.promptRunning = true
	promptDone := make(chan struct{})
	s.promptDone = promptDone
	s.retrying = nil
	s.View.BeginTurn(text, attachments...)
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
		close(promptDone)
		return nil
	}
	go s.awaitPrompt(ident, channel, idleTimeout, slice, promptDone, warmup)
	return nil
}

// awaitPrompt mirrors _wait_prompt: wait in fixed slices (Python
// ACTIVITY_SLICE), staying alive while the agent streams updates or holds a
// pending approval. Idle past the limit cancels the agent-side turn, answers
// pending reverse requests as cancelled, discards the orphaned answer and
// fails the turn — otherwise every later prompt is rejected with "another
// turn is already in progress".
func (s *Session) awaitPrompt(ident string, channel <-chan acp.Delivery, idleTimeout, slice time.Duration, promptDone chan struct{}, warmup bool) {
	if slice <= 0 {
		slice = 30 * time.Second
	}
	if idleTimeout <= 0 {
		idleTimeout = 3600 * time.Second
	}
	// Every exit below is a settled turn (answered, errored or timed out):
	// give the auto-continue hook its edge after the final state lands.
	// A warm-up is a synthetic turn nobody asked to resume, so it never
	// reaches the hook.
	defer func() {
		if !warmup {
			s.fireAutoContinue()
		}
	}()
	defer close(promptDone)
	for {
		select {
		case outcome := <-channel:
			s.mu.Lock()
			s.promptRunning = false
			if outcome.Err != nil {
				s.View.FailTurn(outcome.Err.Error())
			} else {
				stopReason := "end_turn"
				var result *jsoncompat.Object
				if object, ok := outcome.Value.(*jsoncompat.Object); ok {
					result = object
					if value, ok := object.MustGet("stopReason").(string); ok && value != "" {
						stopReason = value
					}
				}
				if stopReason == "refusal" {
					if failure := refusalFailure(result); failure != "" {
						s.View.FailTurn(failure)
					} else {
						s.View.EndTurn(stopReason)
					}
				} else {
					s.View.EndTurn(stopReason)
				}
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

func refusalFailure(result *jsoncompat.Object) string {
	if result == nil {
		return ""
	}
	meta, _ := result.MustGet("_meta").(*jsoncompat.Object)
	raw, _ := meta.MustGet("codebuddy.ai/errorMessage").(string)
	if raw == "" {
		return ""
	}
	decoded, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		return raw
	}
	failure, _ := decoded.(*jsoncompat.Object)
	if failure == nil {
		return raw
	}
	data, _ := failure.MustGet("data").(*jsoncompat.Object)
	category, _ := data.MustGet("category").(string)
	statusCode, _ := data.MustGet("statusCode").(json.Number)
	if category == "auth" || statusCode.String() == "401" {
		return "WorkBuddy 登录已失效，请在 CLI 中执行 /login 后重试"
	}
	if message, _ := failure.MustGet("message").(string); message != "" {
		return message
	}
	return raw
}

// fireAutoContinue launches the installed settle hook off the turn's
// goroutine so a slow Jev assessment never blocks the reader loops.
func (s *Session) fireAutoContinue() {
	if s.autoContinue != nil {
		go s.autoContinue(s)
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
// agent's steering RPC. claude/dsh answer outcome "injected" and mcode answers
// mode "steered"; anything else means the turn ended in the race window and
// the caller should fall back to a normal prompt.
func (s *Session) Steer(method, text string) (injected bool, err error) {
	s.mu.Lock()
	if s.closed || (s.Process != nil && !s.active) {
		s.mu.Unlock()
		return false, errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return false, errors.New(ReadOnlySessionMessage)
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
	injected = false
	if object, ok := result.(*jsoncompat.Object); ok {
		outcome, _ := object.MustGet("outcome").(string)
		mode, _ := object.MustGet("mode").(string)
		injected = outcome == "injected" || method == "mcode/session/steer" && mode == "steered"
	}
	if injected {
		// A human steered the running turn; re-arm the auto-continue budget.
		s.NoteHumanAction()
	}
	return injected, nil
}

// Request sends one generic agent RPC on the session process — the custom
// extension methods (mcode's mcode/session/goal/*) that live outside the ACP
// core. It mirrors SetMode/Steer's request style and deliberately never
// touches the prompt lock: control RPCs are legal in any turn state (Python
// calls process.request straight from the adapter). Returns the response
// object, nil when the agent answered with a non-object result.
func (s *Session) Request(method string, params *jsoncompat.Object, timeout time.Duration) (*jsoncompat.Object, error) {
	s.mu.Lock()
	closed, readOnly := s.closed, s.readOnly
	s.mu.Unlock()
	if closed {
		return nil, errors.New("Agent 进程已退出；操作结果未知")
	}
	if readOnly {
		return nil, errors.New(ReadOnlySessionMessage)
	}
	result, err := s.Process.Request(method, params, timeout)
	if err != nil {
		return nil, err
	}
	object, _ := result.(*jsoncompat.Object)
	return object, nil
}

// NoteHumanAction re-arms the consecutive auto-continue budget. The HTTP
// layer calls it for human approval answers and form replies; Jev's own
// automatic responses deliberately do not (they are not human intent).
func (s *Session) NoteHumanAction() {
	s.mu.Lock()
	s.autoContinueRuns = 0
	s.switchResumeRuns = 0
	s.mu.Unlock()
}

// SetMode switches the session's work mode (AcpAdapter._set_mode port):
// the requested id must be advertised, and a no-op when already current.
// Agents that skip the legacy modes object (opencode advertises modes as a
// "mode" config option) fall back to session/set_config_option. Dialects
// with ModeOptionID (dsh) always route to that named config option — their
// modes object is a permission-preset surface session/set_mode would flip
// (DshAdapter._set_mode).
func (s *Session) SetMode(modeID string) error {
	// Snapshot the advertised mode facts under the lock: SetMode's own tail
	// (and the reader goroutine) rewrite the modes object in place, so the
	// validation below must not read it unlocked.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	modes := s.modes
	configOptions := s.configOptions
	modeOptionID := s.modeOptionID
	available := []string{}
	current := ""
	if modes != nil {
		if advertised, _ := modes.MustGet("availableModes").(*jsoncompat.Array); advertised != nil {
			for _, item := range advertised.Items {
				if mode, ok := item.(*jsoncompat.Object); ok {
					if id, _ := mode.MustGet("id").(string); id != "" {
						available = append(available, id)
					}
				}
			}
		}
		current, _ = modes.MustGet("currentModeId").(string)
	}
	s.mu.Unlock()
	if modeOptionID != "" {
		// dsh advertises collaboration_mode only while its runtime carries a
		// plan command; absence keeps the original refusal.
		option := findOption(configOptions, modeOptionID)
		if option == nil {
			return errors.New("当前 Agent 不支持此工作模式")
		}
		return s.setModeOption(option, modeID)
	}
	if modes == nil {
		if option := findCategory(configOptions, "mode"); option != nil {
			return s.setModeOption(option, modeID)
		}
		return errors.New("当前 Agent 不支持此工作模式")
	}
	found := false
	for _, id := range available {
		if id == modeID {
			found = true
			break
		}
	}
	if !found {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	if current == modeID {
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

// setModeOption switches modes through a config option: the generic "mode"
// category fallback for agents without the legacy modes object, and the
// dialect's ModeOptionID surface (dsh collaboration_mode). The requested id
// must be advertised.
func (s *Session) setModeOption(option *jsoncompat.Object, modeID string) error {
	values := optionValues(option)
	if len(values) > 0 && !values[modeID] {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	if configID, _ := option.MustGet("id").(string); configID == "" {
		return errors.New("当前 Agent 不支持此工作模式")
	}
	if current, _ := option.MustGet("currentValue").(string); current == modeID {
		return nil
	}
	return s.setConfigOption(option, modeID)
}

func (s *Session) Cancel() error {
	return s.cancel(0)
}

// CancelAndWait stops the active turn and waits for its prompt to settle.
func (s *Session) CancelAndWait(timeout time.Duration) error {
	return s.cancel(timeout)
}

func (s *Session) cancel(timeout time.Duration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	// Only a turn this gateway prompted owns the view. A desktop-driven turn
	// is followed from the store; marking it cancelled locally would leave
	// the phone showing 已停止 while the desktop keeps working.
	running := s.promptRunning
	promptDone := s.promptDone
	s.promptRunning = false
	if running {
		s.View.EndTurn("cancelled")
	}
	s.signalLocked()
	s.mu.Unlock()
	if err := s.Process.Notify("session/cancel", sessionParams(s.ID)); err != nil {
		return err
	}
	if timeout > 0 && running && promptDone != nil {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-promptDone:
		case <-timer.C:
			return errors.New("上一任务仍在停止，请稍后重试")
		}
	}
	return nil
}

func (s *Session) Respond(requestID string, response *jsoncompat.Object) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	rawID, exists := s.pendingIDs[requestID]
	params, ok := s.pending[requestID]
	if !ok || !exists {
		s.mu.Unlock()
		return ErrRequestExpired
	}
	wire, invalid := respondWire(s.pendingKinds[requestID], params, response)
	if invalid != nil {
		// The reply is malformed but the request is still live: keep the card
		// answerable instead of consuming it (Python acp_adapter.respond).
		s.mu.Unlock()
		return invalid
	}
	delete(s.pending, requestID)
	delete(s.pendingIDs, requestID)
	delete(s.pendingKinds, requestID)
	s.View.RemovePermission(requestID)
	s.signalLocked()
	s.mu.Unlock()
	return s.Process.Respond(rawID, wire)
}

// respondWire normalizes a phone-side reply into the wire shape the reverse
// request expects. The permission card answers {optionId,text}/{cancel} while
// ACP session/request_permission results are nested {outcome:{outcome,
// optionId,text}} — spec agents (kimi at least) read only the nested form and
// treat anything else as rejected/dismissed. Elicitation input replies keep
// their {action,content} shape verbatim.
func respondWire(kind string, params, response *jsoncompat.Object) (*jsoncompat.Object, error) {
	if kind == "input" {
		for _, key := range response.Keys() {
			if key != "action" && key != "content" {
				return nil, errors.New("请求回应格式不正确")
			}
		}
		switch action, _ := response.MustGet("action").(string); action {
		case "accept", "decline", "cancel":
			return response, nil
		}
		return nil, errors.New("请求回应格式不正确")
	}
	for _, key := range response.Keys() {
		if key != "optionId" && key != "cancel" && key != "text" {
			return nil, errors.New("请求回应格式不正确")
		}
	}
	text, hasText := response.MustGet("text").(string)
	if _, exists := response.Get("text"); exists && !hasText {
		return nil, errors.New("请求回应格式不正确")
	}
	if cancel, _ := response.MustGet("cancel").(bool); cancel {
		outcome := jsoncompat.NewObject()
		outcome.Set("outcome", "cancelled")
		result := jsoncompat.NewObject()
		result.Set("outcome", outcome)
		return result, nil
	}
	optionID, _ := response.MustGet("optionId").(string)
	if optionID == "" || !validOptionID(params, optionID) {
		return nil, errors.New("请选择有效的选项")
	}
	outcome := jsoncompat.NewObject()
	outcome.Set("outcome", "selected")
	outcome.Set("optionId", optionID)
	if text != "" {
		outcome.Set("text", text)
	}
	result := jsoncompat.NewObject()
	result.Set("outcome", outcome)
	return result, nil
}

func validOptionID(params *jsoncompat.Object, optionID string) bool {
	options, _ := params.MustGet("options").(*jsoncompat.Array)
	if options == nil {
		return false
	}
	for _, value := range options.Items {
		option, _ := value.(*jsoncompat.Object)
		if option == nil {
			continue
		}
		if id, _ := option.MustGet("optionId").(string); id == optionID {
			return true
		}
	}
	return false
}

// UpdateSettings mirrors AcpAdapter.update_settings: validate model and
// effort against the advertised config options, then apply each change with
// session/set_config_option. fastMode is rejected like Python.
func (m *Manager) UpdateSettings(session *Session, model, effort string, fastMode *bool) error {
	if session.ReadOnly() {
		return errors.New(ReadOnlySessionMessage)
	}
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
	return session.setConfigOption(option, value)
}

// setConfigOption applies one advertised config option change through
// session/set_config_option; UpdateSettings and the mode fallback share it.
func (s *Session) setConfigOption(option *jsoncompat.Object, value string) error {
	configID, _ := option.MustGet("id").(string)
	if configID == "" {
		return errors.New("当前 Agent 不支持切换模型")
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("configId", configID)
	params.Set("value", value)
	result, err := s.Process.Request("session/set_config_option", params, 30*time.Second)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The response is authoritative: mcode drops thought_level for models
	// without reasoning support and that must propagate. Without a usable
	// configOptions array, patch the local copy like Python does.
	replaced := false
	if object, ok := result.(*jsoncompat.Object); ok {
		if updated, ok := object.MustGet("configOptions").(*jsoncompat.Array); ok && len(updated.Items) > 0 {
			s.configOptions = updated
			replaced = true
		}
	}
	if !replaced && s.configOptions != nil {
		for _, item := range s.configOptions.Items {
			if candidate, ok := item.(*jsoncompat.Object); ok {
				if id, _ := candidate.MustGet("id").(string); id == configID {
					candidate.Set("currentValue", value)
				}
			}
		}
	}
	s.View.ApplyConfig(s.configOptions, s.modes)
	s.signalLocked()
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

// CommandSnapshot returns a detached copy of the session's advertised slash
// commands. The view's command list is reassigned by session/update
// notifications on the reader goroutine, so catalog reads must not walk the
// live slice; the command objects themselves are never mutated in place.
func (s *Session) CommandSnapshot() []*jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	commands := make([]*jsoncompat.Object, len(s.View.Commands))
	copy(commands, s.View.Commands)
	return commands
}

// CommandSkills maps the session's advertised slash commands to picker rows
// (AcpAdapter._command_skills port). The advertised list is read through a
// locked snapshot: the notification reader goroutine reassigns it while
// catalog requests walk it.
func (m *Manager) CommandSkills(session *Session) []*jsoncompat.Object {
	rows := []*jsoncompat.Object{}
	for _, command := range session.CommandSnapshot() {
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
	if options == nil {
		// Agents that advertise neither modes nor config options leave the
		// session without any mode surface; callers treat nil as "no such
		// category" instead of dereferencing it (a nil here used to take the
		// whole gateway down from the queue worker).
		return nil
	}
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

// findOption locates the advertised config option whose id is exactly id
// (dsh's collaboration_mode carries no category).
func findOption(options *jsoncompat.Array, id string) *jsoncompat.Object {
	if options == nil {
		return nil
	}
	for _, item := range options.Items {
		option, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if optionID, _ := option.MustGet("id").(string); optionID == id {
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
	if m.NativeClose != nil {
		_ = m.NativeClose()
	}
}

// Snapshot returns the session's current view.
func (s *Session) Snapshot() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.View.Snapshot()
	if s.retrying != nil {
		snapshot.Set("retrying", jsoncompat.Clone(s.retrying))
	}
	return snapshot
}

// ReadOnly reports whether this view is a native transcript of a session the
// desktop app owns, so prompts and settings must be refused.
func (s *Session) ReadOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOnly
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

// seedUsage restores the last recorded context usage into a freshly built
// view. A session/load replay that already delivered a live usage_update keeps
// its own (authoritative) value; the ledger only fills the blank gauge a
// restart, an idle sweep or a transcript-backed open would otherwise leave.
func (m *Manager) seedUsage(session *Session) {
	if m == nil || m.Usage == nil || session == nil {
		return
	}
	session.mu.Lock()
	id := session.ID
	session.mu.Unlock()
	usage := m.Usage.Get(m.Kind, id)
	if usage == nil {
		return
	}
	session.mu.Lock()
	if session.View != nil && session.View.Usage == nil {
		session.View.Usage = usage
	}
	session.mu.Unlock()
}

// recordUsage persists the usage an agent just reported. Called outside the
// session lock: the ledger write must not stall the update stream.
func (s *Session) recordUsage(sessionID string, used, size any) {
	if s == nil || s.usage == nil {
		return
	}
	s.usage.Record(s.kind, sessionID, used, size)
}

// startNative wires optional persisted-store and retry-state pollers. They run
// for the session's whole life and share one timer.
func (m *Manager) startNative(session *Session) {
	if m == nil || (m.Native == nil && m.RetryStatus == nil) || session == nil {
		return
	}
	session.native = m.Native
	session.nativeSettle = m.NativeSettle
	session.retryStatus = m.RetryStatus
	m.startPoller(session)
}

// openNative builds a read-only session from the agent's own transcript when
// the live handle is refused because the desktop app owns the session. The
// view keeps following the transcript, so the phone sees the app's work.
func (m *Manager) openNative(id, cwd string, cause error) *Session {
	if m.NativeTranscript == nil || !desktopOwned(cause) {
		return nil
	}
	snapshot, err := m.NativeTranscript(id)
	if err != nil || snapshot == nil {
		return nil
	}
	session := &Session{
		ID: id, Cwd: cwd, active: true, readOnly: true,
		View:    acpview.NewView(id, nil, cwd, m.now),
		changed: make(chan struct{}),
		usage:   m.Usage,
		kind:    m.Kind,
		pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{},
		touched: time.Now(), lastActivity: time.Now(), interactionReady: true,
		native: m.NativeTranscript,
	}
	if snapshot.Title != "" {
		session.Title = snapshot.Title
	}
	session.View.ApplyNative(snapshot)
	m.seedUsage(session)
	m.startPoller(session)
	return session
}

func (m *Manager) startPoller(session *Session) {
	session.nativeInterval = m.NativeInterval
	if session.nativeInterval <= 0 {
		session.nativeInterval = 800 * time.Millisecond
	}
	if m.NativeReset != nil && !session.readOnly {
		m.NativeReset(session.ID)
	}
	stop := make(chan struct{})
	session.nativeStop = stop
	// First fold before the view is exposed so the phone's first paint is the
	// native conversation (title and settled tip included), not a replay.
	session.syncNative()
	session.syncRetry()
	go session.followNative(stop)
}

// desktopOwned reports the wrapper's refusal to load a session the desktop
// app is actively writing.
func desktopOwned(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already owned")
}

func (s *Session) followNative(stop <-chan struct{}) {
	timer := time.NewTimer(s.nativeInterval)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
		}
		s.syncNative()
		s.syncRetry()
		timer.Reset(s.nativeInterval)
	}
}

// syncNative folds one native snapshot into the view. The ACP view stays
// authoritative while a prompt this gateway sent is running; the store is
// only consulted for turns the desktop drives.
func (s *Session) syncNative() {
	loader := s.native
	if loader == nil {
		return
	}
	s.mu.Lock()
	paused := s.closed || s.promptRunning
	s.mu.Unlock()
	if paused {
		return
	}
	snapshot, err := loader(s.ID)
	if err != nil || snapshot == nil {
		return
	}
	s.mu.Lock()
	if s.closed || s.promptRunning {
		s.mu.Unlock()
		return
	}
	wasActive := s.View.Status == "active"
	changed := false
	if title := strings.TrimSpace(snapshot.Title); title != "" && s.View.Title != title {
		s.View.Title = title
		if s.Title == "" {
			s.Title = title
		}
		changed = true
	}
	if s.shouldApplyNative(snapshot) && s.View.ApplyNative(snapshot) {
		s.nativeBootstrapped = true
		changed = true
	}
	if changed {
		s.signalLocked()
	}
	settled := wasActive && s.View.Status != "active"
	hook := s.nativeSettle
	s.mu.Unlock()
	if settled && hook != nil {
		go hook(s)
	}
}

func (s *Session) syncRetry() {
	loader := s.retryStatus
	if loader == nil {
		return
	}
	s.mu.Lock()
	skip := s.closed || (s.View.Status != "active" && s.retrying == nil)
	s.mu.Unlock()
	if skip {
		return
	}
	next, err := loader(s.ID)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	same := (s.retrying == nil && next == nil) || (s.retrying != nil && next != nil &&
		string(jsoncompat.EncodeCompact(s.retrying)) == string(jsoncompat.EncodeCompact(next)))
	if same {
		return
	}
	s.retrying = next
	s.signalLocked()
}

// shouldApplyNative keeps the ACP-built tip when both sides describe the
// same settled prompt (re-keying it would reset every timeline cursor), and
// when the ACP tip is settled while the store still reports that prompt as
// running (the store lags the agent's final response). A new prompt, a
// running tip, or a snapshot ahead of a settled ACP turn applies.
func (s *Session) shouldApplyNative(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	candidate := snapshot.Turns[len(snapshot.Turns)-1]
	if candidate == nil {
		return false
	}
	if s.readOnly || !s.nativeBootstrapped {
		// The transcript is the only source for a read-only view, and the
		// first native fold replaces the ACP replay so native attachments and
		// ids land exactly once.
		return true
	}
	tip := s.View.TipTurn()
	if tip == nil {
		s.nativeLagSince = time.Time{}
		return true
	}
	viewText, nativeText := firstUserText(tip), firstUserText(candidate)
	// The store is append-only: a snapshot that does not contain the prompt
	// the phone's view ends with is behind (the agent has not flushed it
	// yet) and must never roll the view back.
	if viewText != "" && !snapshotHasUserText(snapshot, viewText) {
		return false
	}
	if viewText == "" || viewText != nativeText {
		s.nativeLagSince = time.Time{}
		return true
	}
	if turnStatusOf(tip) == "inProgress" {
		s.nativeLagSince = time.Time{}
		return true
	}
	if turnStatusOf(candidate) == "inProgress" {
		// Same prompt, settled ACP copy, running store copy: usually the
		// store has not flushed the final response yet. A turn the ACP side
		// wrongly settled (a cancel aimed at a desktop turn) reclaims the
		// view once the mismatch persists.
		if s.nativeLagSince.IsZero() {
			s.nativeLagSince = time.Now()
		}
		if time.Since(s.nativeLagSince) < nativeStoreLagGrace {
			return false
		}
		s.nativeLagSince = time.Time{}
		return true
	}
	s.nativeLagSince = time.Time{}
	return false
}

func snapshotHasUserText(snapshot *acpview.NativeSnapshot, text string) bool {
	if snapshot == nil {
		return false
	}
	for _, turn := range snapshot.Turns {
		if turn == nil {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, item := range messages.Items {
			message, _ := item.(*jsoncompat.Object)
			if message == nil {
				continue
			}
			if role, _ := message.MustGet("role").(string); role != "user" {
				continue
			}
			if candidate, _ := message.MustGet("text").(string); candidate == text {
				return true
			}
		}
	}
	return false
}

func turnStatusOf(turn *jsoncompat.Object) string {
	if turn == nil {
		return ""
	}
	status, _ := turn.MustGet("status").(string)
	return status
}

func firstUserText(turn *jsoncompat.Object) string {
	if turn == nil {
		return ""
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages == nil {
		return ""
	}
	for _, item := range messages.Items {
		message, _ := item.(*jsoncompat.Object)
		if message == nil {
			continue
		}
		if role, _ := message.MustGet("role").(string); role == "user" {
			if text, _ := message.MustGet("text").(string); text != "" {
				return text
			}
		}
	}
	return ""
}

func (s *Session) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.active = false
	if s.nativeStop != nil {
		close(s.nativeStop)
		s.nativeStop = nil
	}
	process := s.Process
	s.mu.Unlock()
	if process != nil {
		process.Close()
	}
}

func (s *Session) markProcessClosed() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.active = false
	if s.nativeStop != nil {
		close(s.nativeStop)
		s.nativeStop = nil
	}
	s.signalLocked()
	s.mu.Unlock()
}

func (s *Session) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// ownsUpdate reports whether a session-scoped message (params.sessionId)
// belongs to this session. A shared agent host fans child notifications out
// to every gateway connection, so each session must drop traffic addressed
// to a sibling; zcode's first-prompt stream may already carry the
// materialized backend id, which Canonical maps back to the placeholder.
func (s *Session) ownsUpdate(params *jsoncompat.Object) bool {
	sid, _ := params.MustGet("sessionId").(string)
	if sid == "" || sid == s.ID {
		return true
	}
	return s.canonical != nil && s.canonical(s.ID) == sid
}

func (s *Session) onNotification(message *jsoncompat.Object) {
	method, _ := message.MustGet("method").(string)
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		return
	}
	if method == "$/cancel_request" {
		requestID := jsoncompat.PyStr(params.MustGet("id"))
		s.mu.Lock()
		if _, exists := s.pendingIDs[requestID]; exists {
			delete(s.pending, requestID)
			delete(s.pendingIDs, requestID)
			delete(s.pendingKinds, requestID)
			s.View.RemovePermission(requestID)
			s.signalLocked()
		}
		s.mu.Unlock()
		return
	}
	if method != "session/update" {
		return
	}
	if !s.ownsUpdate(params) {
		return
	}
	if update, ok := params.MustGet("update").(*jsoncompat.Object); ok {
		params = update
	}
	// Inline image blocks are decoded to disk before the view folds the
	// update: snapshots stay small and the phone gets a previewable path.
	s.prepareMedia(params)
	var usageUsed, usageSize any
	var usageID string
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.View.OnUpdate(params)
	updateKind, _ := params.MustGet("sessionUpdate").(string)
	if updateKind == "session_info_update" {
		s.Title, _ = s.View.Title.(string)
	} else if updateKind == "usage_update" {
		// The agent reports the context it is working against; remember it so
		// reopening this chat (restart, idle sweep, reconnect) keeps the count.
		usageUsed, usageSize, usageID = params.MustGet("used"), params.MustGet("size"), s.ID
	}
	s.signalLocked()
	s.mu.Unlock()
	if updateKind == "usage_update" {
		s.recordUsage(usageID, usageUsed, usageSize)
	}
}

func (s *Session) onRequest(message *jsoncompat.Object, respond func(any)) {
	method, _ := message.MustGet("method").(string)
	requestID := jsoncompat.PyStr(message.MustGet("id"))
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		params = jsoncompat.NewObject()
	}
	if !s.ownsUpdate(params) {
		// A reverse request addressed to a sibling session (misrouted by the
		// shared host) must not surface in this chat; answer null so the
		// agent is not left waiting.
		respond(nil)
		return
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
	if s.autoApprove != nil && (method == "session/request_permission" || method == "session/requestPermission") {
		go func() {
			if response := s.autoApprove(s.ID, requestID, method, params); response != nil {
				_ = s.Respond(requestID, response)
			}
		}()
	}
}

func (s *Session) signalLocked() {
	s.sequence++
	s.lastActivity = time.Now()
	if s.interactionReady {
		s.lastInteraction = s.lastActivity
	}
	s.touched = time.Now()
	close(s.changed)
	s.changed = make(chan struct{})
}

// LastActivity returns the latest post-load interaction in epoch milliseconds.
func (s *Session) LastActivity() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastInteraction.IsZero() {
		return 0
	}
	return s.lastInteraction.UnixMilli()
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
// refreshed subagent rows. Returns the current revision. The refresh rides
// the view-read path (acpViewState calls it on every read), so it must not
// advance lastInteraction: opening an unread chat would otherwise stamp the
// list recency with "now".
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

// Drop closes one live session handle and forgets it (session delete/hide);
// the agent's own stored session is not touched. A later Open re-reads it.
func (m *Manager) Drop(id string) {
	m.mu.Lock()
	session := m.sessions[id]
	if session != nil {
		delete(m.sessions, session.ID)
	}
	m.mu.Unlock()
	// A deleted chat must not come back with an old gauge or list row if its
	// id is reused (lazy agents may have recorded it under the placeholder id
	// too).
	m.Usage.Forget(m.Kind, id)
	m.Created.Forget(m.Kind, id)
	if session != nil {
		m.Usage.Forget(m.Kind, session.ID)
		m.Created.Forget(m.Kind, session.ID)
		session.Close()
	}
}
