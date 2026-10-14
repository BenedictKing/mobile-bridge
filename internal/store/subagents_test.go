package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
	_ "modernc.org/sqlite"
)

func writeRollout(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func codexSubagentFixture(t *testing.T) *SessionStore {
	t.Helper()
	home := t.TempDir()
	if err := writeRollout(filepath.Join(home, "sessions", "parent-rollout.jsonl"), `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"跑起来"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`); err != nil {
		t.Fatal(err)
	}
	if err := writeRollout(filepath.Join(home, "sessions", "child-rollout.jsonl"), `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-c"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"子代理完成"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statements := []string{
		`CREATE TABLE threads (id TEXT PRIMARY KEY, name TEXT, title TEXT, cwd TEXT,
			created_at INTEGER, created_at_ms INTEGER, updated_at INTEGER, updated_at_ms INTEGER,
			archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`,
		`CREATE TABLE thread_spawn_edges (parent_thread_id TEXT, child_thread_id TEXT, status TEXT)`,
		`INSERT INTO threads VALUES ('parent-1','主会话','主会话','/work',1,1000,9,9000,0,'Codex Desktop','desktop','` + filepath.Join(home, "sessions", "parent-rollout.jsonl") + `')`,
		`INSERT INTO threads VALUES ('child-1','检索','检索','/work',2,2000,8,8000,0,'codex_cli_rs','{"thread_source":"subagent"}','` + filepath.Join(home, "sessions", "child-rollout.jsonl") + `')`,
		`INSERT INTO thread_spawn_edges VALUES ('parent-1','child-1','completed')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return NewSessionStore(home)
}

func TestChildThreadsAndSubagentHistory(t *testing.T) {
	store := codexSubagentFixture(t)
	rows, err := store.ChildThreads("parent-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("children=%v err=%v", rows, err)
	}
	if rows[0].MustGet("id") != "child-1" || rows[0].MustGet("spawn_status") != "completed" {
		t.Fatalf("row=%s", jsoncompat.EncodeCompact(rows[0]))
	}
	// The child is not desktop-owned: Get rejects it, getAny/history do not.
	if _, err := store.Get("child-1"); err == nil {
		t.Fatal("whitelist accepted a subagent row")
	}
	history, err := store.HistoryForAny("child-1", nil)
	if err != nil {
		t.Fatalf("history err=%v", err)
	}
	if !jsoncompat.Truthy(history.MustGet("turns")) {
		t.Fatalf("history=%s", jsoncompat.EncodeCompact(history))
	}
}

func TestSubagentStoreFlattensIntoActivity(t *testing.T) {
	store := codexSubagentFixture(t)
	adapter := store.Subagents()
	children := adapter.Children("parent-1")
	if len(children) != 1 || children[0].ID != "child-1" || children[0].SpawnStatus != "completed" {
		t.Fatalf("children=%+v", children)
	}
	activity := subagents.Flatten(adapter, "parent-1")
	if activity.Revision != 8000 || len(activity.Rows) != 1 {
		t.Fatalf("activity=%+v", activity)
	}
	row := activity.Rows[0]
	if row.SubagentID != "child-1" || row.Status != "completed" {
		t.Fatalf("row=%+v", row)
	}
	// The cached roster serves the second read without a new query hit
	// being observable; the important part is stability.
	if again := adapter.Children("parent-1"); len(again) != 1 || again[0].ID != "child-1" {
		t.Fatalf("cached children=%+v", again)
	}
}
