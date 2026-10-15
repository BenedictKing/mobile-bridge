package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

var ErrUnavailable = errors.New("暂时无法读取 Codex 会话数据库，请稍后重试或重新打开电脑 Codex。")

// SessionStore provides read-only access to Codex's state_*.sqlite index.
// It never creates or writes the database.
type SessionStore struct {
	Home string
}

func NewSessionStore(home string) *SessionStore { return &SessionStore{Home: home} }

func (s *SessionStore) databasePath() (string, error) {
	entries, err := os.ReadDir(s.Home)
	if err != nil {
		return "", ErrUnavailable
	}
	var paths []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "state_") && strings.HasSuffix(entry.Name(), ".sqlite") {
			paths = append(paths, filepath.Join(s.Home, entry.Name()))
		}
	}
	if len(paths) == 0 {
		return "", errors.New("暂时找不到 Codex 会话数据库，请打开电脑 Codex 后重试。")
	}
	sort.Slice(paths, func(i, j int) bool {
		left, leftErr := os.Stat(paths[i])
		right, rightErr := os.Stat(paths[j])
		if leftErr != nil || rightErr != nil {
			return paths[i] > paths[j]
		}
		return left.ModTime().After(right.ModTime())
	})
	return paths[0], nil
}

func (s *SessionStore) open() (*sql.DB, error) {
	path, err := s.databasePath()
	if err != nil {
		return nil, err
	}
	// The URI keeps the live desktop database read-only. WAL snapshots are
	// handled by SQLite itself when sidecars are present.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return nil, ErrUnavailable
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, ErrUnavailable
	}
	return db, nil
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, ErrUnavailable
	}
	defer rows.Close()
	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, ErrUnavailable
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, ErrUnavailable
	}
	return columns, nil
}

func scanObjects(rows *sql.Rows, fields []string) ([]*jsoncompat.Object, error) {
	defer rows.Close()
	result := []*jsoncompat.Object{}
	for rows.Next() {
		values := make([]any, len(fields))
		pointers := make([]any, len(fields))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, ErrUnavailable
		}
		object := jsoncompat.NewObject()
		for i, field := range fields {
			object.Set(field, sqlValue(values[i]))
		}
		result = append(result, object)
	}
	if err := rows.Err(); err != nil {
		return nil, ErrUnavailable
	}
	return result, nil
}

func sqlValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	case int64:
		return json.Number(fmt.Sprintf("%d", typed))
	case float64:
		return json.Number(fmt.Sprintf("%g", typed))
	default:
		return value
	}
}

// List returns desktop-owned, non-subagent thread metadata in recency order.
func (s *SessionStore) List(query string, limit, offset int, archived bool) ([]*jsoncompat.Object, error) {
	if limit < 0 || offset < 0 {
		return nil, errors.New("分页参数无效")
	}
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	columns, err := tableColumns(db, "threads")
	if err != nil {
		return nil, err
	}
	available := []string{"id", "name", "title", "cwd", "updated_at", "updated_at_ms", "recency_at", "recency_at_ms", "model_provider", "model", "originator", "source", "archived", "is_pinned"}
	fields := []string{}
	for _, field := range available {
		if columns[field] {
			fields = append(fields, field)
		}
	}
	if !columns["id"] {
		return nil, ErrUnavailable
	}
	where := []string{}
	args := []any{}
	if columns["archived"] {
		where = append(where, "archived = ?")
		args = append(args, boolInt(archived))
	}
	if columns["originator"] {
		owner := "originator IN (" + userFacingOriginators + ")"
		if columns["source"] {
			owner += " OR (originator IS NULL AND source IN (" + userFacingSources + "))"
		}
		where = append(where, "("+owner+")")
	}
	if columns["thread_source"] {
		where = append(where, "COALESCE(thread_source, '') != 'subagent'")
	}
	if columns["source"] {
		where = append(where, "COALESCE(source, '') NOT LIKE '%\"subagent\"%'")
	}
	if query != "" {
		search := []string{}
		for _, field := range []string{"name", "title", "cwd"} {
			if columns[field] {
				search = append(search, field+" LIKE ?")
				args = append(args, "%"+query+"%")
			}
		}
		if len(search) > 0 {
			where = append(where, "("+strings.Join(search, " OR ")+")")
		}
	}
	clocks := []string{}
	for _, field := range []string{"updated_at_ms", "updated_at", "recency_at_ms", "recency_at"} {
		if columns[field] {
			if strings.HasSuffix(field, "_ms") {
				clocks = append(clocks, field)
			} else {
				clocks = append(clocks, field+" * 1000")
			}
		}
	}
	recency := "0"
	if len(clocks) == 1 {
		recency = clocks[0]
	} else if len(clocks) > 1 {
		recency = "COALESCE(" + strings.Join(clocks, ",") + ")"
	}
	statement := "SELECT " + strings.Join(fields, ",") + " FROM threads"
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY " + recency + " DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := db.Query(statement, args...)
	if err != nil {
		return nil, ErrUnavailable
	}
	return scanObjects(rows, fields)
}

// Get returns one user-facing thread row, rejecting subagent rows that are
// not user-facing chats.
func (s *SessionStore) Get(threadID string) (*jsoncompat.Object, error) {
	object, err := s.getAny(threadID)
	if err != nil {
		return nil, err
	}
	originator, _ := object.MustGet("originator").(string)
	source, _ := object.MustGet("source").(string)
	if !userFacingThread(originator, source) {
		return nil, errors.New("不是用户会话")
	}
	if sourceText, ok := object.MustGet("source").(string); ok && strings.Contains(sourceText, `"subagent"`) {
		return nil, errors.New("不是用户会话")
	}
	if threadSource, ok := object.MustGet("thread_source").(string); ok && threadSource == "subagent" {
		return nil, errors.New("不是用户会话")
	}
	return object, nil
}

// userFacingOriginators 是会话列表与单会话读取共用的来源白名单：桌面 App、
// 网关自身（含 Go 版）以及 CLI（tui/exec/旧版 codex_cli_rs）。
const userFacingOriginators = "'Codex Desktop', 'codex_work_desktop', 'codex_mobile_bridge', 'mobile_bridge_go', 'codex_cli_rs', 'codex-tui', 'codex_exec'"

// userFacingSources 是 originator 为空（旧记录）时放行的 source 取值。
const userFacingSources = "'vscode', 'cli', 'exec'"

// userFacingThread 与 List 的 owner 条件保持一致。
func userFacingThread(originator, source string) bool {
	switch originator {
	case "Codex Desktop", "codex_work_desktop", "codex_mobile_bridge",
		"mobile_bridge_go", "codex_cli_rs", "codex-tui", "codex_exec":
		return true
	}
	return originator == "" && (source == "vscode" || source == "cli" || source == "exec")
}

// getAny mirrors Python's get_any: one thread row without the desktop-owner
// whitelist — subagent children of a parent thread are readable histories.
func (s *SessionStore) getAny(threadID string) (*jsoncompat.Object, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT * FROM threads WHERE id = ?", threadID)
	if err != nil {
		return nil, ErrUnavailable
	}
	fields, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, ErrUnavailable
	}
	objects, err := scanObjects(rows, fields)
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, errors.New("找不到这个桌面会话")
	}
	return objects[0], nil
}

// ChildThreads returns the spawned-child thread rows for one parent via the
// thread_spawn_edges table (missing table → no children, like Python).
func (s *SessionStore) ChildThreads(parentID string) ([]*jsoncompat.Object, error) {
	db, err := s.open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	tables, err := db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		return nil, ErrUnavailable
	}
	hasEdges := false
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err == nil && name == "thread_spawn_edges" {
			hasEdges = true
		}
	}
	_ = tables.Close()
	if !hasEdges {
		return []*jsoncompat.Object{}, nil
	}
	rows, err := db.Query("SELECT c.*, e.status AS spawn_status FROM thread_spawn_edges e " +
		"JOIN threads c ON c.id = e.child_thread_id WHERE e.parent_thread_id = ? " +
		"ORDER BY c.created_at_ms, c.created_at, c.id", parentID)
	if err != nil {
		return nil, ErrUnavailable
	}
	fields, err := rows.Columns()
	if err != nil {
		_ = rows.Close()
		return nil, ErrUnavailable
	}
	return scanObjects(rows, fields)
}

// HistoryFor loads and parses a rollout belonging to the configured Codex
// home. The path guard prevents a database row from escaping the home folder.
func (s *SessionStore) HistoryFor(threadID string, turnLimit *int) (*jsoncompat.Object, error) {
	return s.historyFor(threadID, turnLimit, false)
}

// HistoryForAny is HistoryFor without the desktop-owner whitelist, for
// spawned-child threads (Python's allow_subagent history path).
func (s *SessionStore) HistoryForAny(threadID string, turnLimit *int) (*jsoncompat.Object, error) {
	return s.historyFor(threadID, turnLimit, true)
}

func (s *SessionStore) historyFor(threadID string, turnLimit *int, allowSubagent bool) (*jsoncompat.Object, error) {
	var meta *jsoncompat.Object
	var err error
	if allowSubagent {
		meta, err = s.getAny(threadID)
	} else {
		meta, err = s.Get(threadID)
	}
	if err != nil {
		return nil, err
	}
	pathText, _ := meta.MustGet("rollout_path").(string)
	if pathText == "" {
		return nil, errors.New("找不到这个会话的历史记录")
	}
	path, err := filepath.Abs(pathText)
	if err != nil {
		return nil, errors.New("会话记录路径无效")
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, errors.New("找不到这个会话的历史记录")
	}
	home, err := filepath.Abs(s.Home)
	if err != nil {
		return nil, ErrUnavailable
	}
	allowed := false
	for _, rootName := range []string{"sessions", "archived_sessions"} {
		root, _ := filepath.EvalSymlinks(filepath.Join(home, rootName))
		if root != "" && withinPath(root, path) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, errors.New("会话记录路径不在 Codex 数据目录中")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("找不到这个会话的历史记录")
	}
	return History(threadID, meta, raw, turnLimit), nil
}

func withinPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
