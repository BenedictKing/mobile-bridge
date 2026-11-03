// Package opencode reads OpenCode's current SQLite session store without
// starting the CLI or its server.
package opencode

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const staleAfter = 15 * time.Minute

type Source struct {
	db         *sql.DB
	mu         sync.Mutex
	signatures map[string]string
	pending    map[string]bool
}

func DBPath(env func(string) string, home string) string {
	if explicit := env("OPENCODE_DB"); explicit != "" && explicit != ":memory:" {
		return explicit
	}
	if data := env("XDG_DATA_HOME"); data != "" {
		return filepath.Join(data, "opencode", "opencode-next.db")
	}
	return filepath.Join(home, ".local", "share", "opencode", "opencode-next.db")
}

func OpenSource(path string) (*Source, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if info.IsDir() {
		return nil, errors.New("opencode session store is a directory")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(2000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Source{db: db, signatures: map[string]string{}, pending: map[string]bool{}}, nil
}

func (s *Source) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Source) Reset(sessionID string) {
	s.mu.Lock()
	delete(s.signatures, sessionID)
	delete(s.pending, sessionID)
	s.mu.Unlock()
}

func (s *Source) List() []*jsoncompat.Object {
	if s == nil || s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT id, title, directory, time_updated FROM session_v2
		WHERE parent_id IS NULL AND time_archived IS NULL ORDER BY time_updated DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := []*jsoncompat.Object{}
	for rows.Next() {
		var id, cwd string
		var title sql.NullString
		var updated int64
		if err := rows.Scan(&id, &title, &cwd, &updated); err != nil {
			return nil
		}
		if id == "" || cwd == "" {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", id)
		if title.Valid && strings.TrimSpace(title.String) != "" {
			row.Set("title", strings.TrimSpace(title.String))
		}
		row.Set("cwd", cwd)
		row.Set("updatedAt", jsoncompat.Num(int(updated)))
		result = append(result, row)
	}
	return result
}

func (s *Source) Load(sessionID string) (*acpview.NativeSnapshot, error) {
	if s == nil || s.db == nil || sessionID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	signature, found, err := s.signature(sessionID)
	if err != nil || !found {
		return nil, err
	}
	stale := false
	if previous := s.signatures[sessionID]; previous == signature {
		if !s.pending[sessionID] {
			return nil, nil
		}
		var updated int64
		if err := s.db.QueryRow(`SELECT time_updated FROM session_v2 WHERE id = ?`, sessionID).Scan(&updated); err != nil {
			return nil, err
		}
		if time.Since(time.UnixMilli(updated)) < staleAfter {
			return nil, nil
		}
		stale = true
	}
	snapshot, err := s.snapshot(sessionID, stale)
	if err != nil || snapshot == nil {
		return nil, err
	}
	s.signatures[sessionID] = signature
	s.pending[sessionID] = nativeActive(snapshot)
	return snapshot, nil
}

func (s *Source) signature(sessionID string) (string, bool, error) {
	var updated, messages, parts int64
	err := s.db.QueryRow(`SELECT s.time_updated,
		COALESCE((SELECT MAX(seq) FROM session_message WHERE session_id = s.id), 0),
		COALESCE((SELECT MAX(time_updated) FROM session_part WHERE session_id = s.id), 0)
		FROM session_v2 s WHERE s.id = ?`, sessionID).Scan(&updated, &messages, &parts)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return jsoncompat.PyStr(updated) + "|" + jsoncompat.PyStr(messages) + "|" + jsoncompat.PyStr(parts), true, nil
}

type message struct {
	id      string
	kind    string
	data    *jsoncompat.Object
	content []*jsoncompat.Object
}

func (s *Source) snapshot(sessionID string, stale bool) (*acpview.NativeSnapshot, error) {
	var title string
	if err := s.db.QueryRow(`SELECT title FROM session_v2 WHERE id = ?`, sessionID).Scan(&title); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT id, type, data FROM session_message WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages := []*message{}
	for rows.Next() {
		item := &message{}
		var raw []byte
		if err := rows.Scan(&item.id, &item.kind, &raw); err != nil {
			return nil, err
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		item.data, _ = value.(*jsoncompat.Object)
		messages = append(messages, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	partRows, err := s.db.Query(`SELECT message_id, ordinal, data FROM session_part
		WHERE session_id = ? ORDER BY message_id, ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer partRows.Close()
	byID := map[string]*message{}
	for _, item := range messages {
		byID[item.id] = item
	}
	for partRows.Next() {
		var id string
		var ordinal int
		var raw []byte
		if err := partRows.Scan(&id, &ordinal, &raw); err != nil {
			return nil, err
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		part, _ := value.(*jsoncompat.Object)
		if item := byID[id]; item != nil && part != nil {
			item.content = append(item.content, part)
		}
	}
	if err := partRows.Err(); err != nil {
		return nil, err
	}
	builder := acpview.NewNativeBuilder(sessionID)
	builder.SetTitle(title)
	for _, item := range messages {
		if item.data == nil {
			continue
		}
		switch item.kind {
		case "user":
			text, _ := item.data.MustGet("text").(string)
			builder.User(text)
		case "assistant":
			if model, _ := item.data.MustGet("model").(*jsoncompat.Object); model != nil {
				if id, _ := model.MustGet("id").(string); id != "" {
					builder.SetModel(id)
				}
			}
			content := item.content
			if len(content) == 0 {
				if list, ok := item.data.MustGet("content").(*jsoncompat.Array); ok {
					for _, value := range list.Items {
						if part, ok := value.(*jsoncompat.Object); ok {
							content = append(content, part)
						}
					}
				}
			}
			for _, part := range content {
				applyPart(builder, part)
			}
			if finish, _ := item.data.MustGet("finish").(string); finish != "" {
				builder.End("end_turn")
			}
			if item.data.MustGet("error") != nil {
				builder.End("failed")
			}
		}
	}
	return builder.Snapshot(stale), nil
}

func applyPart(builder *acpview.NativeBuilder, part *jsoncompat.Object) {
	switch part.MustGet("type") {
	case "text":
		text, _ := part.MustGet("text").(string)
		builder.Agent(text)
	case "reasoning":
		text, _ := part.MustGet("text").(string)
		builder.Thought(text)
	case "tool":
		id, _ := part.MustGet("id").(string)
		name, _ := part.MustGet("name").(string)
		state, _ := part.MustGet("state").(*jsoncompat.Object)
		if state == nil {
			return
		}
		builder.Tool(id, name, state.MustGet("input"))
		status, _ := state.MustGet("status").(string)
		if status == "completed" || status == "error" {
			builder.ToolResult(id, state.MustGet("content"), status == "error")
		}
	}
}

func nativeActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}
