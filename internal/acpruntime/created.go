package acpruntime

// Created-session ledger. Agents only publish a session after its first turn:
// codebuddy writes the transcript's cwd line with the first message and zcode
// materializes its backend sess_* id on the first prompt. A chat the gateway
// created through session/new therefore has no row in session/list until it is
// used — once its handle is idle-swept or the gateway restarts, the chat would
// drop out of the phone list, its team member would read as unavailable and
// dispatch could not resolve the session at all. Remembering (agent, id, cwd,
// title, createdAt) keeps such chats first-class until the agent lists them on
// its own.

import (
	"encoding/json"
	"os"
	"sort"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// createdLedgerVersion guards the on-disk shape; a future change bumps it and
// older files are ignored instead of misread.
const createdLedgerVersion = 1

// createdLedgerLimit caps remembered sessions; the oldest creations are
// dropped first (an evicted entry only loses the pre-first-turn guarantee).
const createdLedgerLimit = 512

type createdRecord struct {
	Agent     string `json:"agent"`
	ID        string `json:"id"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
}

type createdLedger struct {
	Version  int                      `json:"version"`
	Sessions map[string]createdRecord `json:"sessions"`
}

// CreatedStore remembers the sessions this gateway created. Safe for
// concurrent use; a nil store is a no-op so callers need no guards.
type CreatedStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]createdRecord
}

// LoadCreated reads <dataDir>/created-sessions.json. A missing, unreadable or
// schema-mismatched file starts an empty store.
func LoadCreated(path string) *CreatedStore {
	store := &CreatedStore{path: path, entries: map[string]createdRecord{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var decoded createdLedger
	if json.Unmarshal(raw, &decoded) != nil || decoded.Version != createdLedgerVersion {
		return store
	}
	for key, record := range decoded.Sessions {
		if key == "" || record.ID == "" {
			continue
		}
		store.entries[key] = record
	}
	return store
}

func createdKey(agent, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if agent == "" {
		return sessionID
	}
	return agent + ":" + sessionID
}

// Remember records one session/new result under the agent that owns it. The
// cwd is what makes the session openable later: resolveACP needs it to spawn
// the agent for a chat the agent's own list does not know yet.
func (c *CreatedStore) Remember(agent, sessionID, cwd, title string, createdAtMs int64) {
	if c == nil {
		return
	}
	key := createdKey(agent, sessionID)
	if key == "" {
		return
	}
	record := createdRecord{Agent: agent, ID: sessionID, Cwd: cwd, Title: title, CreatedAt: createdAtMs}
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous, exists := c.entries[key]; exists {
		// The title can be renamed before the first turn; cwd and creation
		// time never change.
		if previous.Title == record.Title && previous.Cwd == record.Cwd {
			return
		}
		record.CreatedAt = previous.CreatedAt
		if record.Cwd == "" {
			record.Cwd = previous.Cwd
		}
	}
	c.entries[key] = record
	c.pruneLocked()
	c.saveLocked()
}

// Lookup returns the recorded session for an agent, if any.
func (c *CreatedStore) Lookup(agent, sessionID string) (createdRecord, bool) {
	if c == nil {
		return createdRecord{}, false
	}
	key := createdKey(agent, sessionID)
	if key == "" {
		return createdRecord{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.entries[key]
	return record, ok
}

// Forget drops a deleted session so it cannot resurface in the list.
func (c *CreatedStore) Forget(agent, sessionID string) {
	if c == nil {
		return
	}
	key := createdKey(agent, sessionID)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; !exists {
		return
	}
	delete(c.entries, key)
	c.saveLocked()
}

// Rows renders the agent's remembered sessions as session/list rows so the
// phone's list keeps showing a chat the agent does not list yet.
func (c *CreatedStore) Rows(agent string) []*jsoncompat.Object {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	records := make([]createdRecord, 0, len(c.entries))
	for _, record := range c.entries {
		if record.Agent == agent {
			records = append(records, record)
		}
	}
	c.mu.Unlock()
	rows := make([]*jsoncompat.Object, 0, len(records))
	for _, record := range records {
		title := record.Title
		if title == "" {
			title = "未命名聊天"
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", record.ID)
		row.Set("title", title)
		row.Set("cwd", record.Cwd)
		row.Set("updatedAt", jsoncompat.Num(int(record.CreatedAt)))
		rows = append(rows, row)
	}
	return rows
}

func (c *CreatedStore) pruneLocked() {
	overflow := len(c.entries) - createdLedgerLimit
	if overflow <= 0 {
		return
	}
	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return c.entries[keys[i]].CreatedAt < c.entries[keys[j]].CreatedAt
	})
	for _, key := range keys[:overflow] {
		delete(c.entries, key)
	}
}

func (c *CreatedStore) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.MarshalIndent(createdLedger{Version: createdLedgerVersion, Sessions: c.entries}, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}
