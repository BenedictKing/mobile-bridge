package acpruntime

// Context-usage ledger. ACP agents only report context usage inside a live
// turn (`usage_update`), and every view is rebuilt from scratch when a chat is
// opened again — after a gateway restart, an idle sweep, a reconnect or a
// desktop-owned transcript. Without a ledger the phone's gauge drops to "—"
// until the next turn finishes, so the last reported value is remembered per
// agent+session and folded back into the view on open.

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// usageLedgerVersion guards the on-disk shape: a future change bumps it and
// older files are ignored instead of misread.
const usageLedgerVersion = 1

// usageLedgerLimit caps remembered sessions so the file cannot grow without
// bound; the least recently reported entries are dropped first.
const usageLedgerLimit = 512

type usageRecord struct {
	Used      json.Number `json:"used"`
	Size      json.Number `json:"size"`
	UpdatedAt int64       `json:"updatedAt"`
}

type usageLedger struct {
	Version  int                    `json:"version"`
	Sessions map[string]usageRecord `json:"sessions"`
}

// UsageStore remembers the last context usage an agent reported per session.
// It is safe for concurrent use; a nil store is a no-op so callers need no
// guards when the gateway runs without a data dir.
type UsageStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]usageRecord
}

// LoadUsage reads <dataDir>/context-usage.json. A missing, unreadable or
// schema-mismatched file starts an empty store: the gauge is decoration and
// must never keep the gateway from booting.
func LoadUsage(path string) *UsageStore {
	store := &UsageStore{path: path, entries: map[string]usageRecord{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var decoded usageLedger
	if json.Unmarshal(raw, &decoded) != nil || decoded.Version != usageLedgerVersion {
		return store
	}
	for key, record := range decoded.Sessions {
		if key == "" || record.Size == "" || record.Used == "" {
			continue
		}
		store.entries[key] = record
	}
	return store
}

func usageKey(agent, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if agent == "" {
		return sessionID
	}
	return agent + ":" + sessionID
}

// Get returns the remembered usage as a view-shaped object ({used,size}) or
// nil. The value is marked stale: the agent's live context may have moved on
// (compaction, desktop turns), so the phone labels it as a replay instead of
// presenting it as the current context.
func (u *UsageStore) Get(agent, sessionID string) *jsoncompat.Object {
	if u == nil {
		return nil
	}
	key := usageKey(agent, sessionID)
	if key == "" {
		return nil
	}
	u.mu.Lock()
	record, ok := u.entries[key]
	u.mu.Unlock()
	if !ok {
		return nil
	}
	usage := jsoncompat.NewObject()
	usage.Set("used", record.Used)
	usage.Set("size", record.Size)
	usage.Set("stale", true)
	usage.Set("updatedAt", json.Number(strconv.FormatInt(record.UpdatedAt, 10)))
	return usage
}

// Record stores the usage an agent just reported. Values that are not usable
// numbers (missing size, negative or malformed counts) are ignored so a
// partial notification never overwrites a good record.
func (u *UsageStore) Record(agent, sessionID string, used, size any) {
	if u == nil {
		return
	}
	key := usageKey(agent, sessionID)
	if key == "" {
		return
	}
	usedNumber, usedOK := usageNumber(used)
	sizeNumber, sizeOK := usageNumber(size)
	if !usedOK || !sizeOK {
		return
	}
	if sizeFloat, err := sizeNumber.Float64(); err != nil || sizeFloat <= 0 {
		return
	}
	record := usageRecord{Used: usedNumber, Size: sizeNumber, UpdatedAt: time.Now().UnixMilli()}
	u.mu.Lock()
	defer u.mu.Unlock()
	if previous, exists := u.entries[key]; exists &&
		previous.Used == record.Used && previous.Size == record.Size {
		return
	}
	u.entries[key] = record
	u.pruneLocked()
	u.saveLocked()
}

// Forget drops a session's record (a deleted chat must not come back with an
// old gauge if its id is ever reused).
func (u *UsageStore) Forget(agent, sessionID string) {
	if u == nil {
		return
	}
	key := usageKey(agent, sessionID)
	if key == "" {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if _, exists := u.entries[key]; !exists {
		return
	}
	delete(u.entries, key)
	u.saveLocked()
}

func (u *UsageStore) pruneLocked() {
	overflow := len(u.entries) - usageLedgerLimit
	if overflow <= 0 {
		return
	}
	keys := make([]string, 0, len(u.entries))
	for key := range u.entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return u.entries[keys[i]].UpdatedAt < u.entries[keys[j]].UpdatedAt
	})
	for _, key := range keys[:overflow] {
		delete(u.entries, key)
	}
}

// saveLocked rewrites the ledger atomically (tmp + rename) like the other
// gateway ledgers, so a crash mid-write cannot truncate the old records.
func (u *UsageStore) saveLocked() {
	if u.path == "" {
		return
	}
	raw, err := json.MarshalIndent(usageLedger{Version: usageLedgerVersion, Sessions: u.entries}, "", "  ")
	if err != nil {
		return
	}
	tmp := u.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, u.path)
	}
}

// usageNumber normalizes the numbers agents put on the wire (json.Number
// through jsoncompat, float64 through encoding/json, ints from tests) into a
// lexeme the view and the ledger both keep verbatim.
func usageNumber(value any) (json.Number, bool) {
	switch typed := value.(type) {
	case json.Number:
		text := typed.String()
		if text == "" {
			return "", false
		}
		parsed, err := typed.Float64()
		if err != nil || parsed < 0 {
			return "", false
		}
		return typed, true
	case float64:
		if typed < 0 {
			return "", false
		}
		return json.Number(strconv.FormatFloat(typed, 'f', -1, 64)), true
	case int:
		if typed < 0 {
			return "", false
		}
		return jsoncompat.Num(typed), true
	case int64:
		if typed < 0 {
			return "", false
		}
		return json.Number(strconv.FormatInt(typed, 10)), true
	case string:
		if typed == "" {
			return "", false
		}
		number := json.Number(typed)
		parsed, err := number.Float64()
		if err != nil || parsed < 0 {
			return "", false
		}
		return number, true
	}
	return "", false
}
