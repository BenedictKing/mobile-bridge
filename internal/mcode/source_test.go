package mcode

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func openFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "runtime-state.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE local_runtime_sessions (session_id TEXT PRIMARY KEY, title TEXT, workspace_dir TEXT, updated_at_ms INTEGER, parent_session_id TEXT, archived INTEGER, visibility TEXT, session_kind TEXT)`,
		`CREATE TABLE local_runtime_message_rows (id INTEGER PRIMARY KEY, session_id TEXT, msg_id TEXT, role TEXT, created_at_ms INTEGER, data_json TEXT)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return db, path
}

func TestSourceReadsNativeSession(t *testing.T) {
	db, path := openFixture(t)
	for _, statement := range []string{
		`INSERT INTO local_runtime_sessions VALUES ('ses-1', '原生标题', '/work', 1000, NULL, 0, 'visible', 'conversation')`,
		`INSERT INTO local_runtime_message_rows VALUES (1, 'ses-1', 'u1', 'user', 1, '{"role":"user","content":"第一问"}')`,
		`INSERT INTO local_runtime_message_rows VALUES (2, 'ses-1', 'a1', 'assistant', 2, '{"role":"assistant","content":"先扫描","toolCalls":[{"id":"c1","name":"read","args":{"file":"a"},"result":"内容 A"}]}')`,
		`INSERT INTO local_runtime_message_rows VALUES (3, 'ses-1', 'a2', 'assistant', 3, '{"role":"assistant","content":"答完了"}')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	rows := source.List()
	if len(rows) != 1 || rows[0].MustGet("cwd") != "/work" {
		t.Fatalf("rows=%v", rows)
	}
	snapshot, err := source.Load("ses-1")
	if err != nil || snapshot == nil {
		t.Fatalf("snapshot=%v err=%v", snapshot, err)
	}
	if snapshot.Title != "原生标题" || len(snapshot.Turns) != 1 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	turn := snapshot.Turns[0]
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if len(messages.Items) != 4 {
		t.Fatalf("messages=%d", len(messages.Items))
	}
	encoded := string(jsoncompat.EncodeCompact(turn))
	if !strings.Contains(encoded, "内容 A") || !strings.Contains(encoded, "答完了") {
		t.Fatalf("turn=%s", encoded)
	}
	if again, err := source.Load("ses-1"); err != nil || again != nil {
		t.Fatalf("unchanged load=%v err=%v", again, err)
	}
}
