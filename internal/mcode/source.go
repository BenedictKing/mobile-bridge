// Package mcode reads MiniMax Code's local-runtime SQLite store without
// starting the CLI or desktop runtime.
package mcode

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
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

const staleAfter = 15 * time.Minute

type Source struct {
	db         *sql.DB
	mu         sync.Mutex
	signatures map[string]string
	pending    map[string]bool
}

func DBPath(env func(string) string, home string) string {
	data := env("MINIMAX_DATA_DIR")
	if data == "" {
		data = filepath.Join(home, ".minimax-code")
	}
	return filepath.Join(data, "v2", "sqlite", "runtime-state.sqlite")
}

func OpenSource(path string) (*Source, error) {
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if info.IsDir() {
		return nil, errors.New("mcode session store is a directory")
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
	rows, err := s.db.Query(`SELECT session_id, title, workspace_dir, updated_at_ms
		FROM local_runtime_sessions
		WHERE parent_session_id IS NULL AND archived = 0 AND visibility = 'visible'
		AND session_kind NOT IN ('peek', 'channel', 'cron') ORDER BY updated_at_ms DESC`)
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
		if err := s.db.QueryRow(`SELECT updated_at_ms FROM local_runtime_sessions WHERE session_id = ?`, sessionID).Scan(&updated); err != nil {
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
	var updated, latest int64
	err := s.db.QueryRow(`SELECT s.updated_at_ms,
		COALESCE((SELECT MAX(id) FROM local_runtime_message_rows WHERE session_id = s.session_id), 0)
		FROM local_runtime_sessions s WHERE s.session_id = ?`, sessionID).Scan(&updated, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return jsoncompat.PyStr(updated) + "|" + jsoncompat.PyStr(latest), true, nil
}

func (s *Source) snapshot(sessionID string, stale bool) (*acpview.NativeSnapshot, error) {
	var title sql.NullString
	if err := s.db.QueryRow(`SELECT title FROM local_runtime_sessions WHERE session_id = ?`, sessionID).Scan(&title); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT data_json FROM local_runtime_message_rows
		WHERE session_id = ? ORDER BY created_at_ms, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	builder := acpview.NewNativeBuilder(sessionID)
	if title.Valid {
		builder.SetTitle(title.String)
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			continue
		}
		message, _ := value.(*jsoncompat.Object)
		if message == nil {
			continue
		}
		titleText := applyMessage(builder, message)
		if builder.HasTurns() && builder.Title() == "" {
			if candidate := textutil.TitleFromText(titleText); candidate != "" {
				builder.SetTitle(candidate)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return builder.Snapshot(stale), nil
}

func applyMessage(builder *acpview.NativeBuilder, message *jsoncompat.Object) string {
	role, _ := message.MustGet("role").(string)
	content := message.MustGet("content")
	if role == "tool" {
		id, _ := message.MustGet("toolCallId").(string)
		failed, _ := message.MustGet("isError").(bool)
		builder.ToolResult(id, content, failed)
		return ""
	}
	text := messageText(message)
	if role == "user" {
		builder.User(text)
		return text
	}
	if role != "assistant" {
		return ""
	}
	builder.Agent(text)
	for _, key := range []string{"toolCalls", "tool_calls"} {
		calls, _ := message.MustGet(key).(*jsoncompat.Array)
		if calls == nil {
			continue
		}
		for _, value := range calls.Items {
			call, _ := value.(*jsoncompat.Object)
			if call == nil {
				continue
			}
			id, _ := call.MustGet("id").(string)
			if id == "" {
				id, _ = call.MustGet("toolCallId").(string)
			}
			name, _ := call.MustGet("name").(string)
			input := call.MustGet("args")
			if input == nil {
				input = call.MustGet("input")
			}
			builder.Tool(id, name, input)
			if result := call.MustGet("result"); result != nil {
				builder.ToolResult(id, result, false)
			}
		}
	}
	if failed, _ := message.MustGet("isError").(bool); failed {
		builder.End("failed")
	}
	return ""
}

func messageText(message *jsoncompat.Object) string {
	for _, key := range []string{"editContent", "msg_content", "msgContent", "text"} {
		if text, _ := message.MustGet(key).(string); text != "" {
			return text
		}
	}
	return acpview.Text(message.MustGet("content"))
}

func nativeActive(snapshot *acpview.NativeSnapshot) bool {
	if snapshot == nil || len(snapshot.Turns) == 0 {
		return false
	}
	status, _ := snapshot.Turns[len(snapshot.Turns)-1].MustGet("status").(string)
	return status == "inProgress"
}
