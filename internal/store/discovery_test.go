package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	_ "modernc.org/sqlite"
)

func TestSessionStoreListReadsNewestDesktopRows(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY, name TEXT, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT,
		rollout_path TEXT
	)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO threads
		(id,name,title,cwd,updated_at,archived,originator,source,rollout_path)
		VALUES ('desktop-1','First',NULL,'/work',20,0,'Codex Desktop','desktop','/work/rollout.jsonl'),
		       ('cli-1','CLI',NULL,'/work',25,0,'codex-tui','cli','/work/cli.jsonl'),
		       ('legacy-cli-1','LegacyCLI',NULL,'/work',24,0,NULL,'cli','/work/legacy.jsonl'),
		       ('other-1','Other',NULL,'/other',30,0,'other','desktop','/other.jsonl'),
		       ('archived-1','Old',NULL,'/old',40,1,'Codex Desktop','desktop','/old.jsonl')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	rows, err := NewSessionStore(home).List("", 100, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[0].MustGet("id") != "cli-1" ||
		rows[1].MustGet("id") != "legacy-cli-1" || rows[2].MustGet("id") != "desktop-1" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}

func TestSessionStoreHistoryGuardsAndParsesRollout(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "sessions", "rollout.jsonl")
	raw := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"hello"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`
	if err := os.WriteFile(rollout, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO threads VALUES ('desktop-1','History','/work',1,0,'Codex Desktop','desktop',?)`, rollout)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	history, err := NewSessionStore(home).HistoryFor("desktop-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	turns, ok := history.MustGet("turns").(*jsoncompat.Array)
	if !ok || len(turns.Items) != 1 {
		t.Fatalf("unexpected history: %s", jsoncompat.EncodeCompact(history))
	}
}
