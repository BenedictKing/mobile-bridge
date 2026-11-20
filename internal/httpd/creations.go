package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// creationEntry is one idempotent create request (Python service.Bridge's
// creations ledger, persisted across restarts). Sent marks a migration whose
// transcript injection already dispatched, so a retried migrate request
// never re-sends it.
type creationEntry struct {
	Agent          string  `json:"agent,omitempty"`
	Project        string  `json:"project"`
	ProjectCwd     string  `json:"projectCwd,omitempty"`
	ProjectCreated bool    `json:"projectCreated,omitempty"`
	Title          string  `json:"title"`
	Host           string  `json:"host"`
	At             float64 `json:"at"`
	ID             string  `json:"id,omitempty"`
	Sent           bool    `json:"sent,omitempty"`
	State          string  `json:"state,omitempty"`
	Warning        string  `json:"warning,omitempty"`
	Attempts       int     `json:"attempts,omitempty"`
	Message        string  `json:"message,omitempty"`
	// Model/Effort are the target settings chosen in the migrate dialog or
	// resolved from Codex defaults; they are applied on every send path.
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	Execution string `json:"execution,omitempty"`
}

// creations deduplicates create retries by the phone's request id: the same
// id with the same content returns the created thread; a crash between
// begin and complete leaves an id-less entry that refuses to double-create.
type creations struct {
	mu      sync.Mutex
	path    string
	entries map[string]*creationEntry
}

// LoadCreations refuses unreadable history so a prior create cannot disappear.
func LoadCreations(path string) (*creations, error) {
	ledger := &creations{path: path, entries: map[string]*creationEntry{}}
	if path == "" {
		return ledger, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return nil, fmt.Errorf("创建账本读取失败: %w", err)
	}
	var decoded map[string]*creationEntry
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("创建账本解析失败: %w", err)
	}
	if decoded == nil {
		return nil, errors.New("创建账本顶层必须是对象")
	}
	for key, entry := range decoded {
		if entry == nil {
			return nil, fmt.Errorf("创建账本条目 %s 无效", key)
		}
	}
	ledger.entries = decoded
	return ledger, nil
}

func (c *creations) lookup(requestID string) *creationEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[requestID]
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

// begin atomically claims a request id or returns its existing entry.
func (c *creations) begin(requestID string, entry *creationEntry) (*creationEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.entries[requestID]; existing != nil {
		copy := *existing
		return &copy, false, nil
	}
	copy := *entry
	c.entries[requestID] = &copy
	if err := c.saveLocked(); err != nil {
		delete(c.entries, requestID)
		return nil, false, err
	}
	return &copy, true, nil
}

func (c *creations) complete(requestID, id string) error {
	return c.update(requestID, func(entry *creationEntry) { entry.ID = id })
}

// release removes an id-less claim after the target definitively failed to
// create. Callers must retain unknown-result claims to prevent double creation.
func (c *creations) release(requestID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[requestID]
	if entry == nil || entry.ID != "" {
		return nil
	}
	delete(c.entries, requestID)
	if err := c.saveLocked(); err != nil {
		c.entries[requestID] = entry
		return err
	}
	return nil
}

// markSent records that a migration's transcript injection dispatched, so a
// retry of the same request reports "sent" without prompting again.
func (c *creations) markSent(requestID string) error {
	return c.update(requestID, func(entry *creationEntry) {
		entry.Sent = true
		entry.State = "sent"
		entry.Warning = ""
	})
}

// beginDispatch durably claims the external send; a retry cannot send twice.
func (c *creations) beginDispatch(requestID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[requestID]
	if entry == nil || entry.Sent || entry.State == "dispatching" {
		return false, nil
	}
	prior := entry.State
	entry.State = "dispatching"
	if err := c.saveLocked(); err != nil {
		entry.State = prior
		return false, err
	}
	return true, nil
}

// markPrepared records that the target session exists but the transcript has
// not been sent: the phone is choosing model/effort from its real catalog.
func (c *creations) markPrepared(requestID string) error {
	return c.update(requestID, func(entry *creationEntry) {
		if !entry.Sent && entry.State != "dispatching" {
			entry.State = "prepared"
		}
	})
}

// setSettings remembers the chosen model/effort so a deferred send (Codex
// waiting for the desktop App) still applies them.
func (c *creations) setSettings(requestID, model, effort string) error {
	return c.update(requestID, func(entry *creationEntry) {
		entry.Model, entry.Effort = model, effort
	})
}

// forgetPrepared removes a prepared, never-sent entry and reports the target
// session id it had created. Anything else is left untouched.
func (c *creations) forgetPrepared(requestID, agent string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[requestID]
	if entry == nil || entry.Sent || entry.State != "prepared" || entry.Agent != agent {
		return "", false, nil
	}
	delete(c.entries, requestID)
	if err := c.saveLocked(); err != nil {
		c.entries[requestID] = entry
		return "", false, err
	}
	return entry.ID, true, nil
}

func (c *creations) markWaiting(requestID, warning string) error {
	return c.update(requestID, func(entry *creationEntry) {
		if entry.State == "dispatching" {
			return
		}
		entry.State = "waiting_for_target"
		entry.Warning = warning
		entry.Attempts++
	})
}

func (c *creations) markOpened(requestID string) error {
	return c.update(requestID, func(entry *creationEntry) {
		if entry.State == "dispatching" {
			return
		}
		entry.State = "opened"
		entry.Warning = "请等待桌面 App 接管此聊天"
	})
}

func (c *creations) setMessage(requestID, message string) error {
	return c.update(requestID, func(entry *creationEntry) { entry.Message = message })
}

func (c *creations) update(requestID string, change func(*creationEntry)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[requestID]
	if entry == nil {
		return fmt.Errorf("创建账本中不存在请求 %s", requestID)
	}
	prior := *entry
	change(entry)
	if err := c.saveLocked(); err != nil {
		*entry = prior
		return err
	}
	return nil
}

func (c *creations) pendingForTarget(targetID string) (string, *creationEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for requestID, entry := range c.entries {
		if entry.ID == targetID && !entry.Sent && entry.State != "dispatching" && entry.Message != "" {
			copy := *entry
			return requestID, &copy
		}
	}
	return "", nil
}

// migrationForTarget includes dispatching entries: an uncertain send must remain
// visible after reload even though it is deliberately excluded from retries.
func (c *creations) migrationForTarget(targetID string) *creationEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.entries {
		if entry.ID == targetID && !entry.Sent && entry.Message != "" {
			copy := *entry
			return &copy
		}
	}
	return nil
}

func (c *creations) setExecution(requestID, execution string) error {
	return c.update(requestID, func(entry *creationEntry) { entry.Execution = execution })
}

func (c *creations) cliTargets() []*creationEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := []*creationEntry{}
	for _, entry := range c.entries {
		if entry.Execution == "cli" && entry.ID != "" {
			copy := *entry
			result = append(result, &copy)
		}
	}
	return result
}

func (c *creations) saveLocked() error {
	if c.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return fmt.Errorf("创建账本编码失败: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("创建账本写入失败: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("创建账本替换失败: %w", err)
	}
	return nil
}

func (c *creations) now() float64 { return float64(time.Now().Unix()) }
