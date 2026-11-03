package opencode

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func openFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode-next.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, title TEXT, directory TEXT, time_updated INTEGER, time_archived INTEGER)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER, data TEXT)`,
		`CREATE TABLE session_part (message_id TEXT, session_id TEXT, ordinal INTEGER, data TEXT)`,
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
		`INSERT INTO session_v2 VALUES ('ses-1', NULL, '原生标题', '/work', 1000, NULL)`,
		`INSERT INTO session_message VALUES ('msg-u', 'ses-1', 'user', 1, '{"text":"第一问"}')`,
		`INSERT INTO session_message VALUES ('msg-a', 'ses-1', 'assistant', 2, '{"model":{"id":"model-x"},"content":[],"finish":"stop"}')`,
		`INSERT INTO session_part VALUES ('msg-a', 'ses-1', 0, '{"type":"reasoning","text":"想一想"}')`,
		`INSERT INTO session_part VALUES ('msg-a', 'ses-1', 1, '{"type":"text","text":"答完了"}')`,
		`INSERT INTO session_part VALUES ('msg-a', 'ses-1', 2, '{"type":"tool","id":"call-1","name":"read","state":{"status":"completed","input":{"file":"a"},"content":[{"type":"text","text":"内容 A"}]}}')`,
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
	if snapshot.Title != "原生标题" || snapshot.Model != "model-x" || len(snapshot.Turns) != 1 {
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
