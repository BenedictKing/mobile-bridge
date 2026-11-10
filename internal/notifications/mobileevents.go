// MobileEvents is the companion inbox feed (bridge/features/notifications/
// events.py port): a durable bounded event stream with one global cursor, a
// lightweight unread-pending layer whose lifetime is independent of the 200
// body entries, and per-provider read cursors. One feed serves every device;
// phones and push devices each keep their own derived cursor.
package notifications

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// MobileProviderKind is the provider vocabulary the companion apps know; any
// agent kind may still appear as a provider (it just keeps its own unread
// bucket and icon fallback).
var MobileProviders = []string{"codex", "claude", "deepseek", "zcode"}

const mobileEventLimit = 200

// MobileEvent is one inbox entry (events.py append's shape, persisted).
type MobileEvent struct {
	ID        string  `json:"id"`
	Sequence  int64   `json:"sequence"`
	ThreadID  string  `json:"threadId"`
	Host      string  `json:"host"`
	Trace     string  `json:"trace"`
	Kind      string  `json:"kind"` // attention | completion | request | state
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	CreatedAt float64 `json:"createdAt"`
	Phase     string  `json:"phase,omitempty"` // Live Activity state events only
}

// pendingMeta keeps unread counting independent of the bounded body history.
type pendingMeta struct {
	Sequence int64  `json:"sequence"`
	Host     string `json:"host"`
	ThreadID string `json:"threadId"`
	Kind     string `json:"kind"`
}

type mobileFeedState struct {
	StreamID string            `json:"streamId"`
	Cursor   int64             `json:"cursor"`
	Events   []*MobileEvent    `json:"events"`
	Pending  []pendingMeta     `json:"pending"`
	Seen     map[string]int64  `json:"seen"`
	Provider map[string]string `json:"provider,omitempty"` // threadId|host -> provider
}

// MobileSnapshot is read()'s response shape (HTTP layer copies it out).
type MobileSnapshot struct {
	StreamID string
	Cursor   int64
	Unread   map[string]int64
	Seen     map[string]int64
	Summary  []map[string]any
	Events   []*MobileEventView
}

// MobileEventView decorates a stored event with per-reader routing fields.
type MobileEventView struct {
	*MobileEvent
	Provider string `json:"provider"`
	ClientID string `json:"clientId"`
	Unread   bool   `json:"unread"`
	StreamID string `json:"streamId,omitempty"`
}

// MobileEvents is safe for concurrent use; Now is injectable for tests.
type MobileEvents struct {
	path string
	now  func() float64

	mu         sync.Mutex
	cond       *sync.Cond
	generation uint64
	closed     bool
	state      *mobileFeedState
}

// NewMobileEvents loads (or creates) <dataDir>/mobile-events.json.
func NewMobileEvents(dataDir string) *MobileEvents {
	feed := &MobileEvents{path: dataDir + string(os.PathSeparator) + "mobile-events.json",
		now: func() float64 { return float64(time.Now().UnixNano()) / 1e9 }, state: &mobileFeedState{
			StreamID: randomFeedID(), Cursor: 0, Events: []*MobileEvent{}, Seen: map[string]int64{}}}
	feed.cond = sync.NewCond(&feed.mu)
	feed.load()
	return feed
}

func randomFeedID() string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return hex.EncodeToString(buffer)
}

func (f *MobileEvents) load() {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var state mobileFeedState
	if err := json.Unmarshal(raw, &state); err != nil || state.StreamID == "" {
		return
	}
	if state.Events == nil {
		state.Events = []*MobileEvent{}
	}
	if state.Seen == nil {
		state.Seen = map[string]int64{}
	}
	// Rebuild the pending layer when an older file lacks it: unread counting
	// derives from events not yet covered by their client's read cursor.
	if state.Pending == nil {
		for _, event := range state.Events {
			if event.Sequence > state.Seen[providerOfState(&state, event)] {
				state.Pending = append(state.Pending, pendingMeta{Sequence: event.Sequence,
					Host: event.Host, ThreadID: event.ThreadID, Kind: event.Kind})
			}
		}
	}
	f.state = &state
}

func (f *MobileEvents) saveLocked() error {
	raw, err := json.MarshalIndent(f.state, "", "  ")
	if err != nil {
		return fmt.Errorf("手机通知编码失败: %w", err)
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("手机通知写入失败: %w", err)
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return fmt.Errorf("手机通知替换失败: %w", err)
	}
	return nil
}

// providerOfState maps an event to its provider: the recorded provider when
// the append site knew the agent, else the codex default (events.py's
// provider() with the desktop-instance vocabulary collapsed away).
func providerOfState(state *mobileFeedState, event *MobileEvent) string {
	if state.Provider != nil {
		if provider, ok := state.Provider[event.ThreadID+"|"+event.Host]; ok && provider != "" {
			return provider
		}
	}
	return "codex"
}

// ProviderOf is the lock-guarded form for callers outside the feed.
func (f *MobileEvents) ProviderOf(event *MobileEvent) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return providerOfState(f.state, event)
}

// clientOf mirrors events.py client(): the per-read-cursor bucket an event is
// counted under (same as the provider here; upstream separates desktop
// instances which this gateway does not have).
func (f *MobileEvents) clientOf(event *MobileEvent) string {
	return providerOfState(f.state, event)
}

// Version/Changed/Wait/Close implement the long-poll wakeup.
func (f *MobileEvents) Version() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generation
}

func (f *MobileEvents) changedLocked() {
	f.generation++
	f.cond.Broadcast()
}

// Wait blocks until the feed's generation moves past version, the feed closes
// or the timeout elapses.
func (f *MobileEvents) Wait(version uint64, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	f.mu.Lock()
	defer f.mu.Unlock()
	for !f.closed && f.generation == version {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		waitCond(f.cond, remaining)
	}
}

func waitCond(cond *sync.Cond, d time.Duration) {
	timer := time.AfterFunc(d, func() {
		cond.L.Lock()
		cond.Broadcast()
		cond.L.Unlock()
	})
	defer timer.Stop()
	cond.Wait()
}

// Close wakes every waiter; further waits return immediately.
func (f *MobileEvents) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	f.changedLocked()
}

func (f *MobileEvents) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// Append adds one event keyed by the delivery keys (sha256 of the sorted
// joined keys): a retry or a second channel delivering the same fact is
// idempotent. kind is attention | completion | request.
func (f *MobileEvents) Append(keys []string, threadID, host, title, body, kind, provider string) error {
	if len(keys) == 0 {
		return errors.New("通知投递键缺失")
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	identity := sha256DigestString(sorted...)
	trace := ""
	if len(sorted[0]) >= 64 {
		trace = sorted[0][len(sorted[0])-64:][:16]
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.state.Events {
		if event.ID == identity {
			return nil
		}
	}
	event := &MobileEvent{ID: identity, Sequence: f.state.Cursor + 1, ThreadID: threadID, Host: host,
		Trace: trace, Kind: kind, Title: title, Body: body, CreatedAt: f.now()}
	f.state.Cursor = event.Sequence
	f.state.Events = append(f.state.Events, event)
	if len(f.state.Events) > mobileEventLimit {
		f.state.Events = f.state.Events[len(f.state.Events)-mobileEventLimit:]
	}
	f.state.Pending = append(f.state.Pending, pendingMeta{Sequence: event.Sequence, Host: host,
		ThreadID: threadID, Kind: kind})
	if provider != "" {
		if f.state.Provider == nil {
			f.state.Provider = map[string]string{}
		}
		f.state.Provider[threadID+"|"+host] = provider
	}
	if err := f.saveLocked(); err != nil {
		return err
	}
	f.changedLocked()
	return nil
}

func sha256DigestString(values ...string) string {
	sum := sha256.Sum256([]byte(joinLines(values)))
	return hex.EncodeToString(sum[:])
}

func joinLines(values []string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += "\n"
		}
		out += value
	}
	return out
}

// Acknowledge advances a provider's read cursor (provider "all" fans out to
// every known provider). Stale streams and cursors beyond the feed are
// rejected so an older phone cannot rewind what another device read.
func (f *MobileEvents) Acknowledge(provider, stream string, through int64) error {
	if through < 0 || (provider != "all" && !f.knownProvider(provider)) {
		return errors.New("通知已读参数无效")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if stream != f.state.StreamID || through > f.state.Cursor {
		return errors.New("通知状态已变化，请刷新后重试")
	}
	seen := map[string]int64{}
	for key, value := range f.state.Seen {
		seen[key] = value
	}
	known := map[string]bool{}
	for _, name := range MobileProviders {
		known[name] = true
	}
	for _, event := range f.state.Pending {
		known[f.clientOf(&MobileEvent{ThreadID: event.ThreadID, Host: event.Host})] = true
	}
	for key := range seen {
		known[key] = true
	}
	var providers []string
	if provider == "all" {
		for key := range known {
			providers = append(providers, key)
		}
		sort.Strings(providers)
	} else if known[provider] {
		providers = []string{provider}
	}
	// all(): an empty provider set (or one already fully covered) is a no-op.
	covered := true
	for _, item := range providers {
		if through > seen[item] {
			covered = false
		}
	}
	if covered {
		return nil
	}
	for _, item := range providers {
		if through > seen[item] {
			seen[item] = through
		}
	}
	f.state.Seen = seen
	kept := f.state.Pending[:0]
	for _, event := range f.state.Pending {
		if event.Sequence > seen[f.clientOf(&MobileEvent{ThreadID: event.ThreadID, Host: event.Host})] {
			kept = append(kept, event)
		}
	}
	f.state.Pending = kept
	if err := f.saveLocked(); err != nil {
		return err
	}
	f.changedLocked()
	return nil
}

func (f *MobileEvents) knownProvider(provider string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.knownProviderLocked(provider)
}

func (f *MobileEvents) knownProviderLocked(provider string) bool {
	for _, name := range MobileProviders {
		if name == provider {
			return true
		}
	}
	for key := range f.state.Seen {
		if key == provider {
			return true
		}
	}
	for _, event := range f.state.Pending {
		if f.clientOf(&MobileEvent{ThreadID: event.ThreadID, Host: event.Host}) == provider {
			return true
		}
	}
	return false
}

// Checkpoint is the {streamId, cursor} pair embedded in list responses so any
// device can re-align without polling the feed first.
func (f *MobileEvents) Checkpoint() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return map[string]any{"streamId": f.state.StreamID, "cursor": f.state.Cursor}
}

// Read returns the snapshot for devices polling after `after`. allowed
// filters by per-thread notification policy; nil keeps everything. Even when
// `after` predates the bounded window (or the stream reset) the response still
// carries the most recent events so a reconnect can rebuild state.
func (f *MobileEvents) Read(after int64, allowed func(*MobileEvent) bool) *MobileSnapshot {
	if after < 0 {
		after = 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	unread := map[string]int64{}
	for _, name := range MobileProviders {
		unread[name] = 0
	}
	for _, event := range f.state.Pending {
		if allowed != nil && !allowed(&MobileEvent{ThreadID: event.ThreadID, Host: event.Host, Kind: event.Kind}) {
			continue
		}
		unread[f.clientOf(&MobileEvent{ThreadID: event.ThreadID, Host: event.Host})]++
	}
	summary := map[string]map[string]any{}
	events := []*MobileEventView{}
	for _, item := range f.state.Events {
		if allowed != nil && !allowed(item) {
			continue
		}
		provider := f.clientOf(item)
		view := &MobileEventView{MobileEvent: item, Provider: provider, ClientID: provider,
			Unread: item.Sequence > f.state.Seen[provider]}
		summary[provider] = map[string]any{"provider": provider, "kind": item.Kind, "sequence": item.Sequence}
		if item.Sequence > after {
			events = append(events, view)
		}
	}
	ordered := []map[string]any{}
	names := make([]string, 0, len(summary))
	for name := range summary {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ordered = append(ordered, summary[name])
	}
	seen := map[string]int64{}
	for key, value := range f.state.Seen {
		seen[key] = value
	}
	return &MobileSnapshot{StreamID: f.state.StreamID, Cursor: f.state.Cursor, Unread: unread,
		Seen: seen, Summary: ordered, Events: events}
}

// ReadJSON renders a snapshot in the HTTP response shape.
func MobileSnapshotJSON(snapshot *MobileSnapshot, enabled bool, clients []map[string]any) *orderedSnapshot {
	return &orderedSnapshot{snapshot: snapshot, Enabled: enabled, Clients: clients}
}

type orderedSnapshot struct {
	snapshot *MobileSnapshot
	Enabled  bool
	Clients  []map[string]any
}

// MarshalJSON keeps the upstream key order: streamId, cursor, unread, seen,
// summary, events, enabled, clients.
func (o *orderedSnapshot) MarshalJSON() ([]byte, error) {
	buffer := []byte("{")
	appendKey := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		keyJSON, _ := json.Marshal(key)
		buffer = append(buffer, append(append(keyJSON, ':'), encoded...)...)
		buffer = append(buffer, ',')
		return nil
	}
	if err := appendKey("streamId", o.snapshot.StreamID); err != nil {
		return nil, err
	}
	if err := appendKey("cursor", o.snapshot.Cursor); err != nil {
		return nil, err
	}
	if err := appendKey("unread", o.snapshot.Unread); err != nil {
		return nil, err
	}
	if err := appendKey("seen", o.snapshot.Seen); err != nil {
		return nil, err
	}
	if err := appendKey("summary", o.snapshot.Summary); err != nil {
		return nil, err
	}
	events := make([]*MobileEventView, 0, len(o.snapshot.Events))
	events = append(events, o.snapshot.Events...)
	if err := appendKey("events", events); err != nil {
		return nil, err
	}
	if err := appendKey("enabled", o.Enabled); err != nil {
		return nil, err
	}
	if err := appendKey("clients", o.Clients); err != nil {
		return nil, err
	}
	if len(buffer) > 1 {
		buffer = buffer[:len(buffer)-1]
	}
	return append(buffer, '}'), nil
}
