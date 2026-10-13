package store

import (
	"database/sql"
	"path/filepath"
	"testing"

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
	if len(rows) != 1 || rows[0].MustGet("id") != "desktop-1" {
		t.Fatalf("unexpected rows: %v", rows)
	}
}
