package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type activityEntry struct {
	Token      string `json:"token"`
	TurnStatus string `json:"turnStatus"`
	Pending    bool   `json:"pending"`
}

type activityStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]*activityEntry
}

// LoadActivity reads <dataDir>/session-activity.json.
func LoadActivity(path string) (*activityStore, error) {
	store := &activityStore{path: path, entries: map[string]*activityEntry{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("会话状态读取失败: %w", err)
	}
	if err := json.Unmarshal(raw, &store.entries); err != nil {
		return nil, fmt.Errorf("会话状态解析失败: %w", err)
	}
	if store.entries == nil {
		return nil, errors.New("会话状态顶层必须是对象")
	}
	for key, entry := range store.entries {
		if entry == nil || entry.Token == "" {
			return nil, fmt.Errorf("会话状态条目 %s 无效", key)
		}
	}
	return store, nil
}

func activityKey(host, id string) string { return host + "|" + id }

func terminalActivityToken(row *jsoncompat.Object) string {
	turnID, _ := row.MustGet("turnId").(string)
	status, _ := row.MustGet("turnStatus").(string)
	if turnID == "" || (status != "completed" && status != "failed" && status != "interrupted") {
		return ""
	}
	return turnID + ":" + status
}

// observe records terminal turns before decorating the activity response. A
// token is retained after reading so polling the same turn cannot revive it.
func (s *Server) observeActivity(rows *jsoncompat.Array, visible string) error {
	store := s.Activity
	if store == nil {
		return nil
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	next := make(map[string]*activityEntry, len(store.entries)+len(rows.Items))
	for key, entry := range store.entries {
		next[key] = entry
	}
	changed := false
	for _, item := range rows.Items {
		row, _ := item.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		host, _ := row.MustGet("host").(string)
		id, _ := row.MustGet("id").(string)
		key := activityKey(host, id)
		entry := next[key]
		connected, _ := row.MustGet("connected").(bool)
		if connected {
			status, _ := row.MustGet("turnStatus").(string)
			if token := terminalActivityToken(row); token != "" {
				if entry == nil || entry.Token != token {
					entry = &activityEntry{Token: token, TurnStatus: status, Pending: key != visible}
					next[key] = entry
					changed = true
				} else if key == visible && entry.Pending {
					copy := *entry
					copy.Pending = false
					entry = &copy
					next[key] = entry
					changed = true
				}
			}
		}
		if entry != nil {
			row.Set("pending", entry.Pending)
			if row.MustGet("turnStatus") == nil || row.MustGet("turnStatus") == "" {
				row.Set("turnStatus", entry.TurnStatus)
			}
		}
	}
	if !changed {
		return nil
	}
	if err := store.saveLocked(next); err != nil {
		return err
	}
	store.entries = next
	return nil
}

func (s *Server) markActivityRead(host, id string) error {
	store := s.Activity
	if store == nil {
		return nil
	}
	key := activityKey(host, id)
	store.mu.Lock()
	defer store.mu.Unlock()
	entry := store.entries[key]
	if entry == nil || !entry.Pending {
		return nil
	}
	next := make(map[string]*activityEntry, len(store.entries))
	for existingKey, existing := range store.entries {
		next[existingKey] = existing
	}
	copy := *entry
	copy.Pending = false
	next[key] = &copy
	if err := store.saveLocked(next); err != nil {
		return err
	}
	store.entries = next
	return nil
}

func (s *activityStore) saveLocked(entries map[string]*activityEntry) error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("会话状态编码失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("会话状态写入失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("会话状态替换失败: %w", err)
	}
	return nil
}
