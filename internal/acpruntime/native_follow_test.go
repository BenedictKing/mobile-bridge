package acpruntime

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"

	_ "modernc.org/sqlite"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/zcode"
)

// TestNativeFollowerEndToEnd folds real SQLite rows through the follower and
// asserts the view mirrors a desktop-driven turn from start to settlement,
// including the queue-settle hook.
func TestNativeFollowerEndToEnd(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, title TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT, sequence INTEGER)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT, time_created INTEGER, time_updated INTEGER, data TEXT, sequence INTEGER)`,
		`CREATE TABLE turn_usage (session_id TEXT, turn_id TEXT, status TEXT, cancelled_by_user INTEGER, completed_at INTEGER, error_type TEXT)`,
		`CREATE TABLE todo (session_id TEXT, content TEXT, status TEXT, priority INTEGER, position INTEGER, time_created INTEGER, time_updated INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := "sess-native"
	exec(`INSERT INTO session VALUES (?, ?, ?, ?)`, sessionID, "桌面标题", 1000, 2000)
	exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m1", sessionID, 1000, 1000, `{"role":"user","time":{"created":1000},"anchor":{"turnId":"t1"}}`, 1)
	exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p1", "m1", sessionID, 1000, 1000, `{"type":"text","text":"桌面问题"}`, 0)
	exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m2", sessionID, 1001, 1500, `{"role":"assistant","time":{"created":1001,"completed":1500},"anchor":{"turnId":"t1"},"finish":"tool-calls"}`, 2)
	exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p2", "m2", sessionID, 1001, 1500, `{"type":"reasoning","text":"先看看"}`, 0)
	exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p3", "m2", sessionID, 1001, 1500,
		`{"type":"tool","callID":"c1","tool":"Bash","state":{"status":"running","title":"Bash","input":{"command":"ls"}}}`, 1)

	source, err := zcode.OpenSource(filepath.Join(filepath.Dir(dbPathOf(t, db)), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	settled := make(chan struct{}, 1)
	session := &Session{
		ID:           sessionID,
		View:         acpview.NewView(sessionID, nil, "/w", func() float64 { return 0 }),
		changed:      make(chan struct{}),
		native:       source.Load,
		nativeSettle: func(*Session) { settled <- struct{}{} },
	}
	session.syncNative()

	if session.View.Title != "桌面标题" {
		t.Fatalf("title = %v", session.View.Title)
	}
	if session.View.Status != "active" {
		t.Fatalf("status = %q, want active", session.View.Status)
	}
	tip := session.View.TipTurn()
	if tip == nil {
		t.Fatal("no native tip")
	}
	if text := firstUserText(tip); text != "桌面问题" {
		t.Fatalf("tip prompt = %q", text)
	}
	before := len(tip.MustGet("messages").(*jsoncompat.Array).Items)

	// A tool-calls step lands: the view follows and the turn stays running.
	exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m3", sessionID, 2000, 2500, `{"role":"assistant","time":{"created":2000,"completed":2500},"anchor":{"turnId":"t1"},"finish":"tool-calls"}`, 3)
	exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p4", "m3", sessionID, 2000, 2500, `{"type":"text","text":"继续"}`, 0)
	exec(`UPDATE session SET time_updated = 2500 WHERE id = ?`, sessionID)
	session.syncNative()
	if after := len(session.View.TipTurn().MustGet("messages").(*jsoncompat.Array).Items); after != before+1 {
		t.Fatalf("tip rows = %d, want %d", after, before+1)
	}
	if session.View.Status != "active" {
		t.Fatalf("tool-calls status = %q, want active", session.View.Status)
	}
	select {
	case <-settled:
		t.Fatal("settle fired on a tool-calls continuation")
	default:
	}

	// Final step: status returns to idle and queued work is released.
	exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m4", sessionID, 3000, 3500, `{"role":"assistant","time":{"created":3000,"completed":3500},"anchor":{"turnId":"t1"},"finish":"stop"}`, 4)
	exec(`INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p5", "m4", sessionID, 3000, 3500, `{"type":"text","text":"桌面答复"}`, 0)
	exec(`INSERT INTO turn_usage VALUES (?, ?, ?, ?, ?, ?)`, sessionID, "t1", "completed", 0, 3500, nil)
	exec(`UPDATE session SET time_updated = 3600 WHERE id = ?`, sessionID)
	session.syncNative()
	if session.View.Status != "idle" {
		t.Fatalf("status = %q, want idle", session.View.Status)
	}
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settle hook did not fire")
	}
}

func dbPathOf(t *testing.T, db *sql.DB) string {
	t.Helper()
	var path string
	if err := db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path
}
