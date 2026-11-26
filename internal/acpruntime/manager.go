// Package acpruntime owns long-lived ACP child processes and exposes their
// normalized acpview snapshots to the Go service layer.
package acpruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpadapter"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/command"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/planstore"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
	"github.com/google/uuid"
)

type Session struct {
	ID            string
	Cwd           string
	Process       *acp.Process
	View          *acpview.View
	Commands      *command.Ledger
	responseEpoch string // guarded by mu; native callbacks live for one handle

	// CreatedAtMs is immutable. Title follows session_info_update under mu after
	// filtering synthetic transcript protocol titles.
	Title       string
	CreatedAtMs int64

	mu              sync.Mutex
	promptRunning   bool
	promptDone      chan struct{}
	pending         map[string]*jsoncompat.Object
	pendingIDs      map[string]any
	pendingKinds    map[string]string
	responseUnknown map[string]bool
	respondToAgent  func(any, any) error
	changed         chan struct{}
	closed          bool
	active          bool
	generation      uint64
	sequence        int
	configOptions   *jsoncompat.Array
	modes           *jsoncompat.Object
	// modeOptionID mirrors Manager.ModeOptionID: non-empty routes SetMode to
	// that config option (dsh's collaboration_mode).
	modeOptionID string
	// prePlanMode is the permission preset (auto, acceptEdits, yolo…) a
	// modes-carrying agent was in when the phone switched it to plan; leaving
	// plan restores it instead of forcing the agent's manual default.
	prePlanMode string
	// pendingPermissionMode is an explicit phone choice made during plan. It
	// survives native mode changes until a normal send applies it successfully.
	pendingPermissionMode string
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
	autoApprove ApprovalHook
	// approvalPause overrides approvalRetryPause when positive (tests).
	approvalPause time.Duration
	// autoContinue runs asynchronously once a live turn or gateway-owned Kimi
	// notification chain settles. autoContinueRuns is the consecutive-resume
	// budget any human action re-arms.
	autoContinue           func(*Session)
	autoContinueRuns       int
	autoContinueTimer      *time.Timer
	autoContinueGeneration uint64
	// autoContinueDelay overrides the settlement debounce in tests.
	autoContinueDelay time.Duration
	kimiAutoChain     bool
	// switchResumeRuns is the account-pool switch-resume budget: a turn that
	// died on an exhausted quota may be resumed after a credential swap at
	// most MaxSwitchResume times in a row, re-armed by any human action.
	switchResumeRuns int
	// native loads the agent's own persisted conversation (ZCode's SQLite
	// store) for turns the desktop app drives, so the phone follows them
	// instead of showing only the ACP replay. nativeStop ends the poller.
	native         func(sessionID string) (*acpview.NativeSnapshot, error)
	nativeFresh    func(sessionID string) (*acpview.NativeSnapshot, error)
	nativeStop     chan struct{}
	nativeInterval time.Duration
	retryStatus    func(sessionID string) (*jsoncompat.Object, error)
	retrying       *jsoncompat.Object
	// nativeSettle runs when a native turn stops running, so queued phone
	// messages dispatch exactly like after a gateway-sent turn.
	nativeSettle func(*Session)
	// nativeLagSince debounces a settled ACP tip against a store that still
	// reports the same prompt running: the store may merely lag the final
	// response, but a turn that really keeps running reclaims the view after
	// nativeStoreLagGrace.
	nativeLagSince time.Time
	// readOnly marks a native-transcript view without a control handle. It may
	// be desktop-owned or simply not attached yet; mutations require an attach.
	readOnly bool
	// desktopOwned marks a read-only view the desktop app actively holds (the
	// wrapper refused session/load with "already owned", or the owner probe
	// saw its write lock). Attach leaves these alone — an upgrade would be
	// refused — while a plain history view upgrades on the first mutation.
	desktopOwned bool
	// nativeBootstrapped marks the first native fold of a handle. That fold
	// always applies so the native store (with its attachments and stable
	// ids) becomes the view; later folds keep the settled-tip dedupe so
	// ordinary gateway turns do not re-key the timeline.
	nativeBootstrapped bool
	// nativeTurns anchors reconciliation to the last applied store prefix.
	nativeTurns    int
	nativePending  []nativePendingPrompt
	nativeDeferred *acpview.NativeSnapshot
	nativeSyncMu   sync.Mutex
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

// ReadOnlySessionMessage refuses mutations before a native-transcript view has
// an attached control handle.
const ReadOnlySessionMessage = "当前为只读历史；请先重新连接会话，再发送、回应或修改设置"

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

// ApprovalVerdict is what the auto-approve hook returns for one permission
// request: a native Response to submit, or — when Jev assessed the card but
// did not approve it — the Assessment and RecommendOption the phone renders.
// Retry asks for another attempt while the card is still pending (a failed
// assessment with rungs left). Nil, or a verdict with neither, leaves the
// card untouched.
type ApprovalVerdict struct {
	Response        *jsoncompat.Object
	Assessment      *jsoncompat.Object
	RecommendOption string
	Retry           bool
}

// ApprovalHook evaluates one permission request; attempt counts the retries
// of the same card from zero.
type ApprovalHook func(sessionID, requestID, method string, params *jsoncompat.Object, attempt int) *ApprovalVerdict

// approvalRetryPause spaces retries so a fast failure (refused connection,
// 5xx) does not spin through the ladder in a burst.
const approvalRetryPause = 2 * time.Second

type Manager struct {
	Executable  string
	Command     []string
	HostSocket  string
	AutoApprove ApprovalHook
	Commands    *command.Ledger

	// AutoContinue, when set, is invoked (in its own goroutine) every time a
	// live turn settles — completion, failure or idle timeout — so the
	// gateway can judge whether unfinished work should resume. The hook must
	// re-check liveness itself; turns settle while the session stays open.
	AutoContinue func(*Session)
	// KeepAlive protects explicitly managed background work from idle eviction.
	KeepAlive func(sessionID string) bool

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
	// NativeFresh bypasses a reader's unchanged-snapshot cache for the final
	// automatic-continuation guard. Nil falls back to Native.
	NativeFresh func(sessionID string) (*acpview.NativeSnapshot, error)
	// NativeReset drops the store's per-session change cache so a freshly
	// attached handle always rebuilds once.
	NativeReset func(sessionID string)
	// NativeInterval is the poll cadence; zero means 800ms.
	NativeInterval time.Duration
	// NativeClose releases the native store when the manager shuts down.
	NativeClose func() error
	// NativeSettle runs when a native turn settles and dispatches queued work.
	// Automatic continuation is scheduled separately for gateway-owned Kimi
	// notification chains; desktop-driven turns are not ours to resume.
	NativeSettle func(*Session)
	// NativeMeta fills list-row facts the agent wrapper omits: DSH sessions
	// the desktop app owns carry no title in dsh-acp's session/list and a
	// creation-time updatedAt. It returns (title, last activity ms).
	NativeMeta func(sessionID string) (string, int64)
	// NativeTranscript loads a session's read-only conversation from the
	// agent's own store (DSH: the zstd transcript). OpenHistory uses it without
	// starting an agent; Open also falls back to it when load proves desktop ownership.
	NativeTranscript func(sessionID string) (*acpview.NativeSnapshot, error)
	// NativeList, when set, enumerates sessions from the agent's own on-disk
	// store as session/list-shaped rows. It supplements incomplete ACP pages
	// (kimi) and is the fallback for agents without session/list (workbuddy).
	// ACP rows win when the same sessionId appears in both sources.
	NativeList func() []*jsoncompat.Object
	// NativeListExclusive makes NativeList the complete list source, avoiding
	// process startup for agents whose native store covers every session.
	NativeListExclusive bool
	// OwnerProbe reports the current writer without starting an agent. known=false
	// is unsupported or inconclusive, so Open retains the load-based fallback.
	OwnerProbe func(sessionID string) (owned, known bool)
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
	// Plans keeps the latest plan revision chain available when views are rebuilt.
	// Nil disables history without changing the in-memory approval projection.
	Plans *planstore.Store
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
	// attaching single-flights the read-only→live upgrade per session id so
	// a burst of mutations cannot spawn parallel wrappers.
	attaching map[string]chan struct{}

	listMu   sync.Mutex
	listRows []*jsoncompat.Object
	listIDs  map[string]string
	listAt   time.Time
}

func New(executable string) *Manager {
	return &Manager{Executable: executable, sessions: map[string]*Session{}, opening: map[string]chan struct{}{}, attaching: map[string]chan struct{}{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
		InitializeTimeout: 10 * time.Second, ListTTL: 30 * time.Second}
}

func NewCommand(command []string) *Manager {
	return &Manager{Command: append([]string{}, command...), sessions: map[string]*Session{}, opening: map[string]chan struct{}{}, attaching: map[string]chan struct{}{}, now: func() float64 { return float64(time.Now().UnixMilli()) / 1000 },
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

// RefreshList expires the cached session/list read so the next List refreshes
// its configured native or ACP source (mirrors AcpAdapter.refresh_list).
func (m *Manager) RefreshList() {
	m.listMu.Lock()
	m.listAt = time.Time{}
	m.listMu.Unlock()
}

func (m *Manager) fetchListLocked(timeout time.Duration) ([]*jsoncompat.Object, error) {
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
	if m.NativeListExclusive && m.NativeList != nil {
		rows := m.NativeList()
		m.fillNativeMeta(rows)
		return cacheRows(rows), nil
	}
	process, err := m.start()
	if err != nil {
		return nil, err
	}
	defer process.Close()
	if _, err = process.Initialize(m.initializeTimeout()); err != nil {
		return nil, err
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
				// every refresh. Native rows carry the transcript mtime as
				// updatedAt — the startup preload refreshes exactly that — so
				// they must pass through the same NativeMeta fold as merged
				// rows, or every restart floats recent chats to list top.
				if m.NativeList != nil {
					rows := m.NativeList()
					m.fillNativeMeta(rows)
					return cacheRows(rows), nil
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
				// A protocol-only title (e.g. a Lead briefing echoed by session/list)
				// yields to a clean one from another source.
				if strings.TrimSpace(textutil.CleanTitle(current)) == "" && strings.TrimSpace(textutil.CleanTitle(title)) != "" {
					rowsByID[id].Set("title", title)
				}
				continue
			}
			seenSessions[id] = true
			output = append(output, row)
		}
	}
	m.fillNativeMeta(output)
	return cacheRows(output), nil
}

// fillNativeMeta folds the native store's honest title and last-interaction
// time into list rows, whatever source produced them (agent session/list,
// NativeList, or the -32601 native-only shortcut). Rows keep their own fields
// when the native store has nothing better (empty title, updated <= 0).
func (m *Manager) fillNativeMeta(rows []*jsoncompat.Object) {
	if m.NativeMeta == nil {
		return
	}
	for _, row := range rows {
		id, _ := row.MustGet("sessionId").(string)
		if id == "" {
			continue
		}
		title, updated := m.NativeMeta(id)
		if title != "" {
			if current, _ := row.MustGet("title").(string); strings.TrimSpace(textutil.CleanTitle(current)) == "" {
				row.Set("title", title)
			}
		}
		if updated > 0 {
			row.Set("updatedAt", jsoncompat.Num(int(updated)))
		}
	}
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
// Listed rows polluted by synthetic transcript titles recover the gateway's
// creation-time title without mutating the shared session/list cache.
func (m *Manager) MergePending(rows []*jsoncompat.Object) []*jsoncompat.Object {
	// rows may alias the cached session/list slice returned by List to many
	// concurrent callers; copy before changing or appending so concurrent
	// MergePending calls never write into the shared backing array.
	rows = append(rows[:0:0], rows...)
	listed := map[string]bool{}
	listedRows := map[string]int{}
	for index, row := range rows {
		if id, ok := row.MustGet("sessionId").(string); ok && id != "" {
			listed[id] = true
			listedRows[id] = index
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	restoreTitle := func(id, title string) {
		if title == "" {
			title = "未命名聊天"
		}
		rowID := id
		if !listed[rowID] {
			rowID = m.canonicalLocked(id)
		}
		index, exists := listedRows[rowID]
		if !exists {
			return
		}
		current, _ := rows[index].MustGet("title").(string)
		if textutil.CleanTitle(current) == current {
			return
		}
		replacement, _ := jsoncompat.Clone(rows[index]).(*jsoncompat.Object)
		replacement.Set("title", title)
		rows[index] = replacement
	}
	for id, session := range m.sessions {
		if session == nil || session.IsClosed() || session.CreatedAtMs == 0 {
			// Only bridge-created sessions count as pending; opened ones have no
			// creation timestamp and come from the list anyway.
			continue
		}
		session.mu.Lock()
		title := session.Title
		session.mu.Unlock()
		restoreTitle(id, title)
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
		title, _ := row.MustGet("title").(string)
		restoreTitle(id, title)
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
	if !live {
		if key := m.openKey(id); key != id {
			_, live = m.sessions[key]
		}
	}
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
	if m.Created == nil {
		return aliases
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
	if cwd, ok := m.listIDs[id]; ok && cwd != "" {
		m.listMu.Unlock()
		return cwd
	}
	canonical := m.canonicalUnlocked(id)
	cwd := m.listIDs[canonical]
	m.listMu.Unlock()
	if cwd != "" {
		return cwd
	}
	if canonical != "" {
		m.mu.Lock()
		session = m.sessions[canonical]
		m.mu.Unlock()
		if session != nil {
			return session.Cwd
		}
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

// openKey normalizes a requested session id onto the id the agent itself must
// be addressed by. Lazy agents (zcode) hand out a placeholder from
// session/new and materialize the backend sess_* id on the first prompt;
// after a gateway restart session/load with the placeholder is refused with a
// lineage error, so the placeholder and its canonical id must resolve to one
// handle keyed by the canonical id. Unknown or unmaterialized placeholders
// keep the requested id (they may still load, exactly as before).
func (m *Manager) openKey(id string) string {
	if canonical := m.canonicalUnlocked(id); canonical != "" && canonical != id {
		return canonical
	}
	return id
}

// liveAliasLocked returns a live handle registered under another id that
// resolves to key: Create keeps the placeholder key while its own wrapper
// still owns the session, and the lazy mapping only appears after the first
// prompt. Reusing that handle is what keeps concurrent opens of the two ids
// on one wrapper. Callers hold m.mu.
func (m *Manager) liveAliasLocked(key string) *Session {
	if m.Created == nil {
		return nil
	}
	for _, row := range m.Created.Rows(m.Kind) {
		other, _ := row.MustGet("sessionId").(string)
		if other == "" || other == key {
			continue
		}
		if m.canonicalLocked(other) != key {
			continue
		}
		if session := m.sessions[other]; session != nil && !session.IsClosed() {
			return session
		}
	}
	return nil
}

func (m *Manager) newView(id string, title, cwd any) *acpview.View {
	view := acpview.NewView(id, title, cwd, m.now)
	view.UsePlanStore(m.Plans, m.Kind)
	return view
}

func (m *Manager) openFresh(id, cwd string, timeout time.Duration) (*Session, error) {
	process, err := m.start()
	if err != nil {
		return nil, err
	}
	session := &Session{ID: id, Cwd: cwd, Process: process,
		View: m.newView(id, nil, cwd), active: true, mediaDir: m.MediaDir, usage: m.Usage, kind: m.Kind, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	session.autoApprove = m.AutoApprove
	session.autoContinue = m.AutoContinue
	session.Commands = m.Commands
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
	return m.open(id, cwd, timeout, false)
}

// OpenHistory opens only the persisted transcript. It never starts an agent
// or attempts session/load, even when no native snapshot exists.
func (m *Manager) OpenHistory(id, cwd string) (*Session, error) {
	return m.open(id, cwd, 0, true)
}

func (m *Manager) open(id, cwd string, timeout time.Duration, history bool) (*Session, error) {
	// One canonical key per underlying session: session/load, prompts and the
	// handle registry all address the backend id, whichever alias the caller
	// (team binding, list row, queue entry) holds.
	key := m.openKey(id)
	for {
		m.mu.Lock()
		if session := m.sessions[key]; session != nil && !session.IsClosed() {
			m.mu.Unlock()
			return session, nil
		}
		if session := m.liveAliasLocked(key); session != nil {
			m.mu.Unlock()
			return session, nil
		}
		if done, ok := m.opening[key]; ok {
			// Another caller is opening this id; wait and re-check instead of
			// opening a parallel handle. Through an Agent Host every open is a
			// new connection that re-registers the sessionId routing, so a
			// parallel open steals the survivor's update stream.
			m.mu.Unlock()
			<-done
			continue
		}
		done := make(chan struct{})
		m.opening[key] = done
		m.mu.Unlock()

		var fresh *Session
		var err error
		if history {
			fresh, err = m.openNativeHistory(key, cwd)
		} else {
			fresh, err = m.openControlled(key, cwd, timeout)
		}
		m.mu.Lock()
		delete(m.opening, key)
		close(done)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		m.generation++
		fresh.generation = m.generation
		m.sessions[key] = fresh
		m.mu.Unlock()
		return fresh, nil
	}
}

func (m *Manager) openControlled(id, cwd string, timeout time.Duration) (*Session, error) {
	if m.OwnerProbe != nil {
		if owned, known := m.OwnerProbe(id); known && owned {
			session, err := m.openNativeHistory(id, cwd)
			if session != nil {
				session.desktopOwned = true
			}
			return session, err
		}
	}
	fresh, err := m.openFresh(id, cwd, timeout)
	if err != nil {
		if native := m.openNative(id, cwd, err); native != nil {
			return native, nil
		}
	}
	return fresh, err
}

// Attach returns a controllable session for a mutation. A read opens as pure
// history (no agent process), so the first send/respond/settings on such a
// view upgrades it in place: the wrapper starts, session/load runs, and the
// live handle replaces the read-only one. A desktop-owned view is left
// untouched — the caller refuses the mutation. Concurrent attaches
// single-flight per id so a burst of writes spawns one wrapper.
func (m *Manager) Attach(id, cwd string, timeout time.Duration) (*Session, error) {
	// Upgrade decisions key on the canonical id too, or a placeholder attach
	// would miss the read-only view stored under it and skip the upgrade.
	key := m.openKey(id)
	m.mu.Lock()
	if done := m.attaching[key]; done != nil {
		m.mu.Unlock()
		<-done
		return m.Open(key, cwd, timeout)
	}
	current := m.sessions[key]
	if current == nil || current.IsClosed() || !current.ReadOnly() || current.DesktopOwned() {
		m.mu.Unlock()
		return m.Open(key, cwd, timeout)
	}
	done := make(chan struct{})
	if m.attaching == nil {
		m.attaching = map[string]chan struct{}{}
	}
	m.attaching[key] = done
	m.mu.Unlock()
	fresh, err := m.Reconcile(key, cwd, timeout)
	m.mu.Lock()
	delete(m.attaching, key)
	m.mu.Unlock()
	close(done)
	return fresh, err
}

// Reconcile creates and fully loads a new ACP handle before replacing the
// current one. Readers never observe a half-loaded session.
func (m *Manager) Reconcile(id, cwd string, timeout time.Duration) (*Session, error) {
	key := m.openKey(id)
	fresh, err := m.openControlled(key, cwd, timeout)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	old := m.sessions[key]
	if old != nil {
		old.mu.Lock()
		old.active = false
		old.mu.Unlock()
	}
	m.generation++
	fresh.generation = m.generation
	m.sessions[key] = fresh
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
		View: m.newView(id, title, cwd), active: true, mediaDir: m.MediaDir, usage: m.Usage, kind: m.Kind, pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{}), touched: time.Now(), lastActivity: time.Now()}
	session.interactionReady = true
	session.autoApprove = m.AutoApprove
	session.autoContinue = m.AutoContinue
	session.Commands = m.Commands
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
			// View 的会话 ID 决定快照 id 与计划历史的归属键，须随原生 ID 一起切换，
			// 否则历史记在占位 ID 下，按原生 ID 查询的手机端永远看不到。
			session.mu.Lock()
			session.View.SessionID = native
			session.mu.Unlock()
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

func (s *Session) AutoContinuePromptIfCurrent(turnID string, generation uint64, text string, idleTimeout time.Duration) error {
	return s.promptTurnIfCurrent(turnID, generation, text, idleTimeout, budgetAutoContinue)
}

// GoalPrompt draws its step budget from the persistent gateway Goal store.
func (s *Session) GoalPrompt(text string, idleTimeout time.Duration) error {
	return s.prompt(text, nil, idleTimeout, budgetGoal)
}

// ResumeAfterSwitchPrompt resumes a session whose turn died on an exhausted
// quota after the account pool swapped credentials. It draws on its own
// switch-resume budget (MaxSwitchResume) so the accidental-interruption
// budget stays intact for genuine stalls.
func (s *Session) ResumeAfterSwitchPrompt(text string, idleTimeout time.Duration) error {
	return s.prompt(text, nil, idleTimeout, budgetSwitchResume)
}

func (s *Session) ResumeAfterSwitchPromptIfCurrent(turnID string, generation uint64, text string, idleTimeout time.Duration) error {
	return s.promptTurnIfCurrent(turnID, generation, text, idleTimeout, budgetSwitchResume)
}

// AutoContinueGeneration identifies the settle evaluation currently allowed
// to submit. Human actions and newly observed native activity invalidate it.
func (s *Session) AutoContinueGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoContinueGeneration
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
	budgetGoal
)

func (s *Session) prompt(text string, extra []*jsoncompat.Object, idleTimeout time.Duration, budget promptBudget, files ...acpadapter.FileSpec) error {
	return s.promptTurn("", nil, text, extra, idleTimeout, budget, files...)
}

func (s *Session) promptTurnIfCurrent(expectedTurn string, generation uint64, text string, idleTimeout time.Duration, budget promptBudget) error {
	kimiNative := s.kind == "kimi" && s.native != nil
	if kimiNative {
		// Keep native reconciliation serialized until promptRunning is claimed and
		// the ACP request is submitted, so a poll cannot slip between validation
		// and the state transition.
		s.nativeSyncMu.Lock()
		if err := s.syncNativeLocked(true); err != nil {
			s.nativeSyncMu.Unlock()
			s.retryAutoContinue(generation, false)
			return fmt.Errorf("无法确认 Kimi 原生任务已空闲：%w", err)
		}
	}
	if kimiNative {
		defer s.nativeSyncMu.Unlock()
	}
	return s.promptTurn(expectedTurn, &generation, text, nil, idleTimeout, budget)
}

func (s *Session) promptTurn(expectedTurn string, expectedGeneration *uint64, text string, extra []*jsoncompat.Object, idleTimeout time.Duration, budget promptBudget, files ...acpadapter.FileSpec) error {
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
	if expectedTurn != "" {
		tip := s.View.TipTurn()
		if expectedGeneration != nil && *expectedGeneration != s.autoContinueGeneration ||
			s.View.Status == "active" || len(s.pending) > 0 || tip == nil || stringField(tip, "id") != expectedTurn {
			s.mu.Unlock()
			return errors.New("任务已变化，取消自动继续")
		}
	}
	s.cancelAutoContinueLocked()
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
	case budgetGoal:
		// Goal steps are counted in the durable Goal store.
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
	if submitErr == nil && s.kind == "kimi" && !warmup {
		s.mu.Lock()
		s.kimiAutoChain = true
		s.mu.Unlock()
	}
	if submitErr != nil {
		s.mu.Lock()
		s.promptRunning = false
		s.View.FailTurn(submitErr.Error())
		s.signalLocked()
		s.mu.Unlock()
		close(promptDone)
		// The view already shows the failed turn, but the caller must not
		// record a phantom acceptance: pre-write failures may requeue, while
		// interrupted writes stay unknown and are never auto-retried.
		return submitErr
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
		if s.kind == "kimi" {
			// Task notifications may have started outside the ACP turn. Fold
			// them before the settle hook tries to send queued work.
			s.syncNative()
		}
		if !warmup {
			s.scheduleAutoContinue()
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
	if meta == nil {
		return ""
	}
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
	category, statusCode := "", ""
	if data, _ := failure.MustGet("data").(*jsoncompat.Object); data != nil {
		category, _ = data.MustGet("category").(string)
		if code, ok := data.MustGet("statusCode").(json.Number); ok {
			statusCode = code.String()
		}
	}
	if category == "auth" || statusCode == "401" {
		return "CodeBuddy 登录已失效，请在 CLI 中执行 /login 后重试"
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

func (s *Session) cancelAutoContinueLocked() {
	s.autoContinueGeneration++
	if s.autoContinueTimer != nil {
		s.autoContinueTimer.Stop()
		s.autoContinueTimer = nil
	}
}

func (s *Session) scheduleAutoContinue() {
	if s.kind != "kimi" || s.native == nil {
		s.fireAutoContinue()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scheduleAutoContinueLocked()
}

func (s *Session) scheduleAutoContinueLocked() {
	s.cancelAutoContinueLocked()
	if !s.kimiAutoChain || s.closed || s.readOnly || s.promptRunning || s.View.Status == "active" || s.autoContinue == nil {
		return
	}
	delay := s.autoContinueSettleDelayLocked()
	generation := s.autoContinueGeneration
	s.autoContinueTimer = time.AfterFunc(delay, func() { s.fireScheduledAutoContinue(generation) })
}

func (s *Session) autoContinueSettleDelayLocked() time.Duration {
	if s.autoContinueDelay > 0 {
		return s.autoContinueDelay
	}
	delay := 3 * time.Second
	if pollDelay := 2 * s.nativeInterval; pollDelay > delay {
		delay = pollDelay
	}
	return delay
}

func (s *Session) retryAutoContinue(generation uint64, timerFired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.autoContinueGeneration {
		return
	}
	if timerFired {
		s.autoContinueTimer = nil
	}
	if s.autoContinueTimer == nil {
		s.scheduleAutoContinueLocked()
	}
}

func (s *Session) fireScheduledAutoContinue(generation uint64) {
	if err := s.syncNativeForAutoContinue(); err != nil {
		s.retryAutoContinue(generation, true)
		return
	}
	s.mu.Lock()
	if generation != s.autoContinueGeneration || s.autoContinueTimer == nil {
		s.mu.Unlock()
		return
	}
	s.autoContinueTimer = nil
	if !s.kimiAutoChain || s.closed || s.readOnly || s.promptRunning || s.View.Status == "active" {
		s.mu.Unlock()
		return
	}
	hook := s.autoContinue
	s.mu.Unlock()
	if hook != nil {
		go hook(s)
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
		if s.responseUnknown[requestID] {
			continue
		}
		queue = append(queue, responder{rawID: rawID, kind: s.pendingKinds[requestID]})
	}
	s.pending = map[string]*jsoncompat.Object{}
	s.pendingIDs = map[string]any{}
	s.pendingKinds = map[string]string{}
	s.responseUnknown = nil
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
	s.cancelAutoContinueLocked()
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
	_, err := s.setConfigOption(option, modeID)
	return err
}

func (s *Session) Cancel() error {
	return s.cancel(0)
}

// CancelAndWait stops the active turn and waits for its prompt to settle.
func (s *Session) CancelAndWait(timeout time.Duration) error {
	return s.cancel(timeout)
}

func (s *Session) cancel(timeout time.Duration) error {
	return s.cancelTurn("", timeout)
}

// CancelTurn binds a human Stop to its observed turn and waits for settlement.
func (s *Session) CancelTurn(expected string) error { return s.cancelTurn(expected, 10*time.Second) }

func (s *Session) cancelTurn(expected string, timeout time.Duration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("Agent 进程已退出；操作结果未知")
	}
	if s.readOnly {
		s.mu.Unlock()
		return errors.New(ReadOnlySessionMessage)
	}
	if expected != "" {
		turns, _ := s.View.Snapshot().MustGet("turns").(*jsoncompat.Array)
		current := ""
		if turns != nil && len(turns.Items) > 0 {
			if turn, ok := turns.Items[len(turns.Items)-1].(*jsoncompat.Object); ok {
				current, _ = turn.MustGet("id").(string)
			}
		}
		if current != expected {
			s.mu.Unlock()
			return errors.New("任务已切换，未停止新的任务；请刷新聊天")
		}
	}
	// Only a turn this gateway prompted owns the view. A desktop-driven turn
	// is followed from the store; marking it cancelled locally would leave
	// the phone showing 已停止 while the desktop keeps working.
	running := s.promptRunning
	promptDone := s.promptDone
	// Keep the prompt ownership until awaitPrompt settles the old request.
	// Otherwise a new prompt can start while its predecessor is still stopping.
	if err := s.Process.Notify("session/cancel", sessionParams(s.ID)); err != nil {
		s.mu.Unlock()
		return err
	}
	if running {
		s.View.EndTurn("cancelled")
	}
	s.signalLocked()
	s.mu.Unlock()
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

// Pending 是一次待处理聚合读取的结果：只克隆请求卡片，不复制整份时间线。
type Pending struct {
	Title    string
	Requests []*jsoncompat.Object
	Recency  int64
}

// PendingRequests 返回当前待处理请求的只读克隆，不启动 Agent、不改变会话状态。
func (s *Session) PendingRequests() Pending {
	s.mu.Lock()
	defer s.mu.Unlock()
	title, _ := s.View.Title.(string)
	pending := Pending{Title: title}
	if !s.lastInteraction.IsZero() {
		pending.Recency = s.lastInteraction.UnixMilli()
	}
	for _, request := range s.View.Requests {
		if cloned, ok := jsoncompat.Clone(request).(*jsoncompat.Object); ok {
			pending.Requests = append(pending.Requests, cloned)
		}
	}
	return pending
}

// RequestParams returns a clone of a pending card's projected params, or nil
// once the request is answered or gone. Callers use it to read server-side
// metadata (plan version/hash) instead of trusting values echoed by the phone.
func (s *Session) RequestParams(requestID string) *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, request := range s.View.Requests {
		if id, _ := request.MustGet("id").(string); id == requestID {
			params, _ := jsoncompat.Clone(request.MustGet("params")).(*jsoncompat.Object)
			return params
		}
	}
	return nil
}

func (s *Session) Respond(requestID string, response *jsoncompat.Object, commandIDs ...string) error {
	commandID := ""
	if len(commandIDs) > 0 {
		commandID = commandIDs[0]
	}
	scope := "reply:" + s.kind + ":" + s.ID + ":" + requestID
	if _, err, found := s.Commands.Replay(commandID, scope, response); found {
		return err
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
	rawID, exists := s.pendingIDs[requestID]
	params, ok := s.pending[requestID]
	if !ok || !exists {
		s.mu.Unlock()
		return ErrRequestExpired
	}
	if s.responseUnknown[requestID] {
		s.mu.Unlock()
		return &acp.Error{Message: "回应结果未知，请在原 Agent 会话核实；此请求不可再次提交", Kind: acp.KindWriteUnknown}
	}
	wire, invalid := respondWire(s.pendingKinds[requestID], params, response)
	if invalid != nil {
		// The reply is malformed but the request is still live: keep the card
		// answerable instead of consuming it (Python acp_adapter.respond).
		s.mu.Unlock()
		return invalid
	}
	respond := s.respondToAgent
	if respond == nil {
		respond = s.Process.Respond
	}
	if s.responseEpoch == "" {
		s.responseEpoch = uuid.NewString()
	}
	turnID := ""
	if turns, ok := s.View.Snapshot().MustGet("turns").(*jsoncompat.Array); ok && len(turns.Items) > 0 {
		if turn, ok := turns.Items[len(turns.Items)-1].(*jsoncompat.Object); ok {
			turnID, _ = turn.MustGet("id").(string)
		}
	}
	target := scope + ":" + s.responseEpoch + ":" + command.Hash(map[string]any{"kind": s.pendingKinds[requestID], "params": params, "rawId": rawID, "turnId": turnID})
	if commandID == "" {
		commandID = target
	}
	delivered := false
	_, replyErr := s.Commands.Run(command.Intent{ID: commandID, Scope: scope, Target: target, Payload: response}, func() (any, error) {
		err := respond(rawID, wire)
		// Only a newly delivered human response re-arms the budgets. Receipt
		// replays and Jev responses must not produce another human-action effect.
		if err == nil {
			delivered = true
		}
		if err == nil && len(commandIDs) > 0 {
			s.autoContinueRuns = 0
			s.switchResumeRuns = 0
			s.cancelAutoContinueLocked()
		}
		var acpErr *acp.Error
		if errors.As(err, &acpErr) && acpErr.Kind == acp.KindUnavailable {
			err = command.Rejected(err)
		}
		return map[string]any{}, err
	})
	if err := replyErr; err != nil {
		var acpErr *acp.Error
		if errors.Is(err, command.ErrUnknown) || (errors.As(err, &acpErr) && acpErr.Kind == acp.KindWriteUnknown) {
			if s.responseUnknown == nil {
				s.responseUnknown = map[string]bool{}
			}
			s.responseUnknown[requestID] = true
			s.View.MarkRequestOutcomeUnknown(requestID)
			s.signalLocked()
		}
		s.mu.Unlock()
		return err
	}
	relay := kimiPlanFeedback(s.kind, s.pendingKinds[requestID], params, response)
	delete(s.pending, requestID)
	delete(s.pendingIDs, requestID)
	delete(s.pendingKinds, requestID)
	delete(s.responseUnknown, requestID)
	s.View.RemovePermission(requestID)
	s.signalLocked()
	s.mu.Unlock()
	if relay != "" && delivered {
		return s.relayFeedback(relay)
	}
	return nil
}

// FeedbackRelayError 表示审批回应已送达，但随后转交修改意见失败。
type FeedbackRelayError struct{ Err error }

func (e *FeedbackRelayError) Error() string {
	return "修改意见未能转交给 Agent：" + e.Err.Error()
}
func (e *FeedbackRelayError) Unwrap() error { return e.Err }

// kimiPlanFeedback 返回需由网关代为转交的计划修改意见。Kimi acp-server 把
// plan_revise 映射为 {decision:rejected, selectedLabel:Revise}，从不读取
// outcome.text，模型只会收到 "User requested revisions"。
func kimiPlanFeedback(kind, requestKind string, params, response *jsoncompat.Object) string {
	if kind != "kimi" || requestKind != "permission" || response.MustGet("optionId") != "plan_revise" ||
		!acpview.KimiPlanPermission(params) {
		return ""
	}
	text, _ := response.MustGet("text").(string)
	return strings.TrimSpace(text)
}

// relayFeedback 停下 Agent 拿到"需修订"后自行继续的回合，再把意见作为下一条
// 用户消息发出，确保修订基于意见而不是盲改。仅在首次送达回应后调用，重放不会重复发送。
func (s *Session) relayFeedback(text string) error {
	err := s.CancelAndWait(10 * time.Second)
	if err == nil {
		err = s.Prompt(text, 3600*time.Second)
	}
	if err != nil {
		return &FeedbackRelayError{Err: err}
	}
	return nil
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

// SettingsPartialError 报告组合设置的部分成功：Applied 列出本次已生效的项
// （如 "模型"），Err 是后续项的失败原因。HTTP 以 409 透出整段消息——手机端
// 由此知道模型已切换、思考强度未生效，而不是一个笼统的失败。
type SettingsPartialError struct {
	Applied []string
	Err     error
}

func (e *SettingsPartialError) Error() string {
	return "已生效：" + strings.Join(e.Applied, "、") + "；" + e.Err.Error()
}

func (e *SettingsPartialError) Unwrap() error { return e.Err }

// UpdateSettings mirrors AcpAdapter.update_settings: validate model and
// effort against the advertised config options up front (a rejected value
// must not leave half the request applied), then apply each change with
// session/set_config_option. The model response is authoritative and may
// reshape the remaining options (mcode drops thought_level for models
// without reasoning), so the effort step re-reads the advertised options
// instead of the entry snapshot. confirmed reports whether every applied
// change carries agent evidence (an authoritative configOptions response);
// an optimistic local patch leaves it false and the phone shows 已接受，
// 等待同步 instead of 已生效.
func (m *Manager) UpdateSettings(session *Session, model, effort string, fastMode *bool) (confirmed bool, err error) {
	if session.ReadOnly() {
		return false, errors.New(ReadOnlySessionMessage)
	}
	if fastMode != nil {
		return false, errors.New("当前 Agent 不支持 Fast 模式")
	}
	options := session.ConfigOptions()
	modelOption := findCategory(options, "model")
	if modelOption == nil {
		return false, errors.New("当前 Agent 不支持切换模型")
	}
	thinking := findCategory(options, "thought_level")
	if thinking == nil {
		thinking = findCategory(options, "thinking")
	}
	hadThinking := thinking != nil
	// 前置校验：任何一项被拒都不发出 RPC（原先 effort 校验发生在 model
	// 已生效之后，形成未申报的部分成功）。
	if model != "" {
		if values := optionValues(modelOption); len(values) > 0 {
			if _, ok := values[model]; !ok {
				return false, errors.New("此 Agent 不支持所选模型")
			}
		}
	}
	mapped := ""
	if effort != "" {
		mapped = m.EffortMap[effort]
		if mapped == "" {
			return false, errors.New("此 Agent 不支持所选思考强度")
		}
		if thinking != nil {
			if values := optionValues(thinking); len(values) > 0 {
				if _, ok := values[mapped]; !ok {
					return false, errors.New("此 Agent 不支持所选思考强度")
				}
			}
		}
	}
	applied := []string{}
	partial := func(err error) error {
		if len(applied) == 0 {
			return err
		}
		return &SettingsPartialError{Applied: applied, Err: err}
	}
	confirmed = true
	if model != "" {
		if current, _ := modelOption.MustGet("currentValue").(string); current != model {
			replaced, applyErr := session.setConfigOption(modelOption, model)
			if applyErr != nil {
				return false, applyErr
			}
			confirmed = replaced
			applied = append(applied, "模型")
			// 模型响应是权威的，可能重塑其余选项：思考强度按切换后的
			// 新目录判断，不能沿用入口快照。
			options = session.ConfigOptions()
			thinking = findCategory(options, "thought_level")
			if thinking == nil {
				thinking = findCategory(options, "thinking")
			}
		}
	}
	if effort != "" && hadThinking {
		if thinking == nil {
			// 入口存在思考选项、模型切换后消失：明确申报部分成功，绝不
			// 静默吞掉用户显式选择的项。
			return false, partial(errors.New("切换后的模型不提供思考强度选项，已保持原值"))
		}
		if values := optionValues(thinking); len(values) > 0 {
			if _, ok := values[mapped]; !ok {
				return false, partial(errors.New("切换后的模型不支持所选思考强度"))
			}
		}
		if current, _ := thinking.MustGet("currentValue").(string); current != mapped {
			replaced, applyErr := session.setConfigOption(thinking, mapped)
			if applyErr != nil {
				return false, partial(applyErr)
			}
			if !replaced {
				confirmed = false
			}
		}
	}
	return confirmed, nil
}

// setConfigOption applies one advertised config option change through
// session/set_config_option; UpdateSettings and the mode fallback share it.
// The returned flag reports agent evidence: true only when the response
// carried a usable configOptions array, i.e. the new state is authoritative.
func (s *Session) setConfigOption(option *jsoncompat.Object, value string) (bool, error) {
	configID, _ := option.MustGet("id").(string)
	if configID == "" {
		return false, errors.New("当前 Agent 不支持切换模型")
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", s.ID)
	params.Set("configId", configID)
	params.Set("value", value)
	result, err := s.Process.Request("session/set_config_option", params, 30*time.Second)
	if err != nil {
		return false, err
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
	return replaced, nil
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

// Peek returns the live session for id without touching its idle clock.
func (m *Manager) Peek(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := m.openKey(id)
	session := m.sessions[key]
	if session == nil {
		// A newly materialized backend id can be the reverse alias of the live
		// placeholder handle, so reconnect must reuse it just like Open does.
		session = m.liveAliasLocked(key)
	}
	return session
}

// Active 返回已加载的会话句柄（按句柄去重）。聚合视图据此读取，不为未加载会话启动 Agent。
func (m *Manager) Active() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := make(map[*Session]bool, len(m.sessions))
	sessions := make([]*Session, 0, len(m.sessions))
	for _, session := range m.sessions {
		if session != nil && !seen[session] {
			seen[session] = true
			sessions = append(sessions, session)
		}
	}
	return sessions
}

// Get returns the live session for id (touching its idle clock), or nil.
func (m *Manager) Get(id string) *Session {
	session := m.Peek(id)
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
		if m.KeepAlive != nil && m.KeepAlive(session.ID) {
			continue
		}
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
	snapshot, _ := s.SnapshotWithSequence()
	return snapshot
}

// SnapshotWithSequence captures the view and its revision under the same lock.
func (s *Session) SnapshotWithSequence() (*jsoncompat.Object, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := s.View.Snapshot()
	if s.closed {
		snapshot.Set("connected", false)
	}
	if s.retrying != nil {
		snapshot.Set("retrying", jsoncompat.Clone(s.retrying))
	}
	return snapshot, s.sequence
}

// ReadOnly reports whether this view is a native transcript without a control
// handle, so prompts and settings require an explicit attach first.
func (s *Session) ReadOnly() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readOnly
}

// DesktopOwned reports whether the desktop app holds this session's write
// side: an attach would be refused, so mutations stay unavailable until the
// desktop side releases the session. A read-only view that is not
// desktop-owned is just an unattached history and can upgrade on demand.
func (s *Session) DesktopOwned() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desktopOwned
}

// Sequence returns the change counter used as the projection sequence: every
// view mutation (prompt, notification, request, config change) bumps it.
func (s *Session) Sequence() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sequence
}

// Revision subscribes without copying history. Subscribe before reading a
// snapshot so changes during the read cannot be lost by the subsequent wait.
func (s *Session) Revision() (int, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sequence, s.changed
}

// Wait blocks until the next view change or timeout. It picks up the change
// channel only when called, so a change landing between the caller's last
// read and this call is missed; callers that act on a read use WaitAfter.
func (s *Session) Wait(timeout time.Duration) {
	s.mu.Lock()
	changed := s.changed
	s.mu.Unlock()
	select {
	case <-changed:
	case <-time.After(timeout):
	}
}

// WaitAfter blocks until the change counter moves past sequence or timeout.
// Pass the Sequence read before the snapshot the caller acted on: a change
// that already happened returns at once instead of being lost. Every change
// bumps the counter before replacing the channel, under the same lock.
func (s *Session) WaitAfter(sequence int, timeout time.Duration) {
	s.mu.Lock()
	if s.sequence != sequence {
		s.mu.Unlock()
		return
	}
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
	session.nativeFresh = m.NativeFresh
	session.nativeSettle = m.NativeSettle
	session.retryStatus = m.RetryStatus
	m.startPoller(session)
}

// openNative builds a read-only session from the agent's own transcript when
// the live handle is refused because the desktop app owns the session. The
// view keeps following the transcript, so the phone sees the app's work.
func (m *Manager) openNative(id, cwd string, cause error) *Session {
	if !desktopOwned(cause) {
		return nil
	}
	session, err := m.openNativeHistory(id, cwd)
	if err != nil {
		return nil
	}
	session.desktopOwned = true
	return session
}

func (m *Manager) openNativeHistory(id, cwd string) (*Session, error) {
	if m.NativeTranscript == nil {
		return nil, errors.New("当前 Agent 不支持纯历史读取")
	}
	if m.NativeReset != nil {
		m.NativeReset(id)
	}
	snapshot, err := m.NativeTranscript(id)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, errors.New("未找到可读取的原生会话历史")
	}
	return m.newNativeSession(id, cwd, snapshot), nil
}

// StaleCreated reports a chat this gateway created through session/new that
// can never be opened again: nobody prompted it, so the agent wrote no
// transcript and refuses session/load, and the creating handle is gone (idle
// sweep or gateway restart). It reads the transcript, so callers use it to
// classify an open failure, not as a list filter. The title lets the caller
// recreate the chat under the same name.
func (m *Manager) StaleCreated(id string) (title string, stale bool) {
	if m == nil || m.NativeTranscript == nil || id == "" || m.openKey(id) != id {
		return "", false
	}
	record, created := m.Created.Lookup(m.Kind, id)
	if !created {
		return "", false
	}
	m.mu.Lock()
	live := m.sessions[id]
	m.mu.Unlock()
	if live != nil && !live.IsClosed() {
		return "", false
	}
	// A nil snapshot also means "unchanged since the last read"; reset the
	// cache so nil can only mean no transcript at all.
	if m.NativeReset != nil {
		m.NativeReset(id)
	}
	snapshot, err := m.NativeTranscript(id)
	return record.Title, err == nil && snapshot == nil
}

func (m *Manager) newNativeSession(id, cwd string, snapshot *acpview.NativeSnapshot) *Session {
	session := &Session{
		ID: id, Cwd: cwd, active: true, readOnly: true,
		View:    m.newView(id, nil, cwd),
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

// RefreshNative reconciles a view on demand. HTTP reads use this as a
// recovery path when the background poller missed a native notification.
func (s *Session) RefreshNative() {
	s.syncNative()
}

// syncNative folds one native snapshot into the view. The ACP view stays
// authoritative while a prompt this gateway sent is running; the store then
// reconciles desktop turns and Kimi notification chains.
func (s *Session) syncNative() {
	// The poller and ACP settle path may both read. Apply them in read order.
	s.nativeSyncMu.Lock()
	defer s.nativeSyncMu.Unlock()
	_ = s.syncNativeLocked(false)
}

func (s *Session) syncNativeForAutoContinue() error {
	s.nativeSyncMu.Lock()
	defer s.nativeSyncMu.Unlock()
	return s.syncNativeLocked(true)
}

func (s *Session) syncNativeLocked(requireFresh bool) error {
	loader := s.native
	if requireFresh && s.nativeFresh != nil {
		loader = s.nativeFresh
	}
	if loader == nil {
		if requireFresh {
			return errors.New("Kimi 原生会话读取器不可用")
		}
		return nil
	}
	s.mu.Lock()
	paused := s.closed || s.promptRunning
	s.mu.Unlock()
	if paused {
		return nil
	}
	snapshot, err := loader(s.ID)
	if err != nil {
		return err
	}
	if requireFresh && (snapshot == nil || len(snapshot.Turns) == 0) {
		return errors.New("Kimi 原生会话状态尚未可用")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if snapshot != nil {
		s.nativeDeferred = snapshot
	}
	if s.promptRunning || s.nativeDeferred == nil {
		s.mu.Unlock()
		return nil
	}
	snapshot = s.nativeDeferred
	wasActive := s.View.Status == "active"
	taskAhead := s.kimiTaskAhead(snapshot)
	changed := false
	if title := strings.TrimSpace(snapshot.Title); title != "" && s.View.Title != title {
		s.View.Title = title
		if s.Title == "" {
			s.Title = title
		}
		changed = true
	}
	if s.shouldApplyNative(snapshot) && s.View.ApplyNative(s.preserveKimiPrompts(snapshot)) {
		s.nativeBootstrapped = true
		s.nativeTurns = len(snapshot.Turns)
		s.nativeDeferred = nil
		changed = true
	} else if s.nativeLagSince.IsZero() {
		// Only a timed lag guard needs the consumed snapshot on later ticks.
		s.nativeDeferred = nil
	}
	if changed {
		s.signalViewLocked()
	}
	settled := (wasActive || taskAhead) && s.View.Status != "active"
	if s.kimiAutoChain && (changed || settled) {
		s.scheduleAutoContinueLocked()
	}
	hook := s.nativeSettle
	s.mu.Unlock()
	if settled && hook != nil {
		go hook(s)
	}
	return nil
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
	s.signalViewLocked()
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
	hasPrompt := snapshotHasUserText(snapshot, viewText)
	if s.kind == "kimi" && s.nativeTurns > 0 && len(s.View.Turns) > s.nativeTurns {
		// A repeated user text in old history does not acknowledge a new send.
		start := min(s.nativeTurns, len(snapshot.Turns))
		hasPrompt = snapshotHasUserText(&acpview.NativeSnapshot{Turns: snapshot.Turns[start:]}, viewText)
	}
	// The store is append-only: a snapshot that does not contain the prompt
	// the phone's view ends with is behind (the agent has not flushed it
	// yet) and must not roll the view back. A verified new Kimi task may
	// advance the view while unmaterialized user messages remain visible.
	if viewText != "" && !hasPrompt {
		if s.kimiTaskAhead(snapshot) {
			s.nativeLagSince = time.Time{}
			return true
		}
		s.nativeLagSince = time.Time{}
		return false
	}
	if s.kind == "kimi" && s.nativeTurns > 0 && len(snapshot.Turns) > s.nativeTurns {
		s.nativeLagSince = time.Time{}
		return true
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

func emptySettledPrompt(turn *jsoncompat.Object) bool {
	if turnStatusOf(turn) != "completed" {
		return false
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages == nil || len(messages.Items) != 1 {
		return false
	}
	message, _ := messages.Items[0].(*jsoncompat.Object)
	return message != nil && message.MustGet("kind") == "userMessage" && message.MustGet("role") == "user"
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
	s.kimiAutoChain = false
	s.cancelAutoContinueLocked()
	if s.nativeStop != nil {
		close(s.nativeStop)
		s.nativeStop = nil
	}
	process := s.Process
	// Explicit close also changes connected state. Wake readers just as an
	// unexpected process exit does, without changing conversation recency.
	if s.changed != nil {
		s.signalViewLocked()
	} else {
		s.sequence++
	}
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
	s.View.FailTurn("Agent 进程已退出；操作结果未知")
	if s.nativeStop != nil {
		close(s.nativeStop)
		s.nativeStop = nil
	}
	// The sweeper's idle eviction closes handles too; closing must not
	// restamp the list recency as if the session had just been used.
	s.signalViewLocked()
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
// The check is symmetric: a handle keyed by either id accepts updates that
// name the other.
func (s *Session) ownsUpdate(params *jsoncompat.Object) bool {
	sid, _ := params.MustGet("sessionId").(string)
	if sid == "" || sid == s.ID {
		return true
	}
	if s.canonical == nil {
		return false
	}
	return s.canonical(s.ID) == sid || s.canonical(sid) == s.ID
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
		title, _ := s.View.Title.(string)
		cleaned := textutil.CleanTitle(title)
		if cleaned != title && s.Title != "" && textutil.CleanTitle(s.Title) == s.Title {
			cleaned = s.Title
		}
		s.Title = cleaned
		s.View.Title = cleaned
	} else if updateKind == "current_mode_update" && s.modes != nil {
		previous, _ := s.modes.MustGet("currentModeId").(string)
		next, _ := params.MustGet("currentModeId").(string)
		s.notePlanEntryLocked(previous, next)
		s.modes.Set("currentModeId", params.MustGet("currentModeId"))
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
	delete(s.responseUnknown, requestID)
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
		go s.runAutoApprove(requestID, method, params)
	}
}

// runAutoApprove drives the Jev hook for one card. The card is already on the
// phone, so the user can answer at any time; a failed assessment is retried
// (the hook widens its deadline per attempt) only while the card stays
// pending, and whichever settles it first — a later verdict or the user —
// ends the loop.
func (s *Session) runAutoApprove(requestID, method string, params *jsoncompat.Object) {
	for attempt := 0; ; attempt++ {
		verdict := s.autoApprove(s.ID, requestID, method, params, attempt)
		if verdict == nil {
			return
		}
		if verdict.Response != nil && s.Respond(requestID, verdict.Response) == nil {
			return
		}
		s.annotateRequest(requestID, verdict)
		if !verdict.Retry {
			return
		}
		pause := approvalRetryPause
		if s.approvalPause > 0 {
			pause = s.approvalPause
		}
		time.Sleep(pause)
		if !s.requestPending(requestID) {
			return
		}
	}
}

func (s *Session) requestPending(requestID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, pending := s.pending[requestID]
	return pending && !s.closed
}

// annotateRequest pins a Jev verdict onto a still-pending card and pushes it
// to watchers. It re-presents state rather than recording an interaction, so
// the chat's list recency stays put.
func (s *Session) annotateRequest(requestID string, verdict *ApprovalVerdict) {
	if verdict.Assessment == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, pending := s.pending[requestID]; !pending {
		return
	}
	if s.View.AnnotateRequest(requestID, verdict.Assessment, verdict.RecommendOption) {
		s.signalViewLocked()
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

// signalViewLocked announces a view change without counting it as an
// interaction: the list recency stays put. Native-store folds (the first
// fold after a load, title syncs, retry state, closing the handle) re-present
// existing state — the gateway's own open (the startup preload, or opening a
// chat on the phone) must not restamp it, exactly like history replay.
func (s *Session) signalViewLocked() {
	s.sequence++
	s.lastActivity = time.Now()
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
	key := m.openKey(id)
	m.mu.Lock()
	session := m.sessions[key]
	if session == nil {
		session = m.liveAliasLocked(key)
	}
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
