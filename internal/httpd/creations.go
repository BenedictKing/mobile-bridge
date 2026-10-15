package httpd

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// creationEntry is one idempotent create request (Python service.Bridge's
// creations ledger, persisted across restarts). Sent marks a migration whose
// transcript injection already dispatched, so a retried migrate request
// never re-sends it.
type creationEntry struct {
	Agent    string  `json:"agent,omitempty"`
	Project  string  `json:"project"`
	Title    string  `json:"title"`
	Host     string  `json:"host"`
	At       float64 `json:"at"`
	ID       string  `json:"id,omitempty"`
	Sent     bool    `json:"sent,omitempty"`
	State    string  `json:"state,omitempty"`
	Warning  string  `json:"warning,omitempty"`
	Attempts int     `json:"attempts,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// creations deduplicates create retries by the phone's request id: the same
// id with the same content returns the created thread; a crash between
// begin and complete leaves an id-less entry that refuses to double-create.
type creations struct {
	mu      sync.Mutex
	path    string
	entries map[string]*creationEntry
}

// LoadCreations reads <dataDir>/creations.json; a missing or unreadable file
// starts an empty ledger.
func LoadCreations(path string) *creations {
	ledger := &creations{path: path, entries: map[string]*creationEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	var decoded map[string]*creationEntry
	if json.Unmarshal(raw, &decoded) == nil {
		ledger.entries = decoded
	}
	return ledger
}

func (c *creations) lookup(requestID string) *creationEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[requestID]
}

func (c *creations) begin(requestID string, entry *creationEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[requestID] = entry
	c.saveLocked()
}

func (c *creations) complete(requestID, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[requestID]; entry != nil {
		entry.ID = id
		c.saveLocked()
	}
}

// markSent records that a migration's transcript injection dispatched, so a
// retry of the same request reports "sent" without prompting again.
func (c *creations) markSent(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[requestID]; entry != nil {
		entry.Sent = true
		entry.State = "sent"
		entry.Warning = ""
		c.saveLocked()
	}
}

func (c *creations) markWaiting(requestID, warning string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[requestID]; entry != nil {
		entry.State = "waiting_for_target"
		entry.Warning = warning
		entry.Attempts++
		c.saveLocked()
	}
}

func (c *creations) markOpened(requestID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[requestID]; entry != nil {
		entry.State = "opened"
		entry.Warning = "请等待桌面 App 接管此聊天"
		c.saveLocked()
	}
}

func (c *creations) setMessage(requestID, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[requestID]; entry != nil {
		entry.Message = message
		c.saveLocked()
	}
}

func (c *creations) pendingForTarget(targetID string) (string, *creationEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for requestID, entry := range c.entries {
		if entry.ID == targetID && !entry.Sent && entry.Message != "" {
			copy := *entry
			return requestID, &copy
		}
	}
	return "", nil
}

func (c *creations) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return
	}
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

func (c *creations) now() float64 { return float64(time.Now().Unix()) }
