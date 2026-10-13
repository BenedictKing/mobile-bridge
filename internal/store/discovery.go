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
		owner := "originator IN ('Codex Desktop', 'codex_work_desktop', 'codex_mobile_bridge')"
		if columns["source"] {
			owner += " OR (originator IS NULL AND source = 'vscode')"
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

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
