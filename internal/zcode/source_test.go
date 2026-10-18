package zcode

import (
	"database/sql"
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const testSession = "sess-test"

func openFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli", "db", "db.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	return db, path
}

func jsonText(value int64) string { return strconv.FormatInt(value, 10) }

func insertRow(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	now := time.Now().UnixMilli()
	insertRow(t, db, `INSERT INTO session VALUES (?, ?, ?, ?)`, testSession, "真实标题", now-120000, now-1000)
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m1", testSession, now-1000, now-1000, `{"role":"user","time":{"created":`+jsonText(now-1000)+`},"anchor":{"turnId":"t1"}}`, 1)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p1", "m1", testSession, now-1000, now-1000, `{"type":"text","text":"第一问"}`, 0)
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m2", testSession, now-900, now-500, `{"role":"assistant","time":{"created":`+jsonText(now-900)+`,"completed":`+jsonText(now-500)+`},"anchor":{"turnId":"t1"},"finish":"stop"}`, 2)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p2", "m2", testSession, now-900, now-500, `{"type":"reasoning","text":"想了想"}`, 0)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p3", "m2", testSession, now-900, now-500, `{"type":"text","text":"答一"}`, 1)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p4", "m2", testSession, now-900, now-500,
		`{"type":"tool","callID":"c1","tool":"Bash","state":{"status":"completed","title":"Bash","input":{"command":"echo hi"},"output":"hi\n"}}`, 2)
	// A model-only reminder must never become a phone bubble.
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m3", testSession, now-400, now-400,
		`{"role":"user","time":{"created":3000},"anchor":{"turnId":"t2"},"synthetic":true,"semantics":{"uiVisibility":"hidden"}}`, 3)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p5", "m3", testSession, now-400, now-400, `{"type":"text","text":"隐藏提醒"}`, 0)
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m4", testSession, now-300, now-300, `{"role":"user","time":{"created":`+jsonText(now-300)+`},"anchor":{"turnId":"t2"}}`, 4)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p6", "m4", testSession, now-300, now-300, `{"type":"text","text":"第二问"}`, 0)
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m5", testSession, now-200, now-200, `{"role":"assistant","time":{"created":`+jsonText(now-200)+`},"anchor":{"turnId":"t2"}}`, 5)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p7", "m5", testSession, now-200, now-200,
		`{"type":"tool","callID":"c2","tool":"Read","state":{"status":"running","title":"Read","input":{"file_path":"/tmp/x"}}}`, 1)
	insertRow(t, db, `INSERT INTO turn_usage VALUES (?, ?, ?, ?, ?, ?)`, testSession, "t1", "completed", 0, now-500, nil)
	insertRow(t, db, `INSERT INTO todo VALUES (?, ?, ?, ?, ?, ?, ?)`, testSession, "写测试", "completed", 1, 0, now-1000, now-1000)
	insertRow(t, db, `INSERT INTO todo VALUES (?, ?, ?, ?, ?, ?, ?)`, testSession, "跑测试", "in_progress", 1, 1, now-1000, now-1000)
	return now
}

func TestSourceFollowsPersistedTurns(t *testing.T) {
	db, path := openFixture(t)
	now := seed(t, db)
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := source.Load(testSession)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot == nil {
		t.Fatal("first load returned nil")
	}
	if snapshot.Title != "真实标题" {
		t.Fatalf("title = %q", snapshot.Title)
	}
	if title, updated := source.Meta(testSession); title != "真实标题" || updated != now-1000 {
		t.Fatalf("meta = %q/%d", title, updated)
	}
	if len(snapshot.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(snapshot.Turns))
	}
	first := snapshot.Turns[0]
	if status, _ := first.MustGet("status").(string); status != "completed" {
		t.Fatalf("first status = %q", status)
	}
	if got := turnMessages(t, first); len(got) != 4 {
		t.Fatalf("first messages = %d, want 4 (user/thought/answer/tool)", len(got))
	}
	tool := turnMessages(t, first)[3]
	for key, want := range map[string]string{
		"kind": "toolCall", "role": "activity", "title": "Bash",
		"text": "echo hi", "output": "hi\n", "status": "completed",
	} {
		if value, _ := tool.MustGet(key).(string); value != want {
			t.Fatalf("tool %s = %q, want %q", key, value, want)
		}
	}
	second := snapshot.Turns[1]
	if status, _ := second.MustGet("status").(string); status != "inProgress" {
		t.Fatalf("second status = %q, want inProgress", status)
	}
	messages := turnMessages(t, second)
	if len(messages) != 3 { // hidden reminder skipped: user + tool + plan
		t.Fatalf("second messages = %d, want 3", len(messages))
	}
	if text, _ := messages[0].MustGet("text").(string); text != "第二问" {
		t.Fatalf("second user text = %q", text)
	}
	if status, _ := messages[1].MustGet("status").(string); status != "inProgress" {
		t.Fatalf("running tool status = %q", status)
	}
	plan := messages[2]
	if kind, _ := plan.MustGet("kind").(string); kind != "plan" {
		t.Fatalf("tail kind = %q, want plan", kind)
	}
	entries, _ := plan.MustGet("entries").(*jsoncompat.Array)
	if entries == nil || len(entries.Items) != 2 {
		t.Fatalf("plan entries = %v", entries)
	}

	// Nothing moved: the poller must not rebuild.
	if again, err := source.Load(testSession); err != nil || again != nil {
		t.Fatalf("unchanged load = %v, %v", again, err)
	}

	// A new step changes the signature and must reach the snapshot.
	insertRow(t, db, `INSERT INTO message VALUES (?, ?, ?, ?, ?, ?)`,
		"m6", testSession, 3700, 3700, `{"role":"assistant","time":{"created":3700,"completed":3800},"anchor":{"turnId":"t2"},"finish":"stop"}`, 6)
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p8", "m6", testSession, 3700, 3800, `{"type":"text","text":"第二答"}`, 0)
	insertRow(t, db, `UPDATE session SET time_updated = ? WHERE id = ?`, now, testSession)
	updated, err := source.Load(testSession)
	if err != nil {
		t.Fatal(err)
	}
	if updated == nil {
		t.Fatal("changed load returned nil")
	}
	last := turnMessages(t, updated.Turns[len(updated.Turns)-1])
	if text, _ := last[len(last)-2].MustGet("text").(string); text != "第二答" {
		t.Fatalf("appended answer = %q", text)
	}

	// Reset forces a rebuild even without a store change (fresh handle).
	if again, _ := source.Load(testSession); again != nil {
		t.Fatal("expected a cached no-op")
	}
	source.Reset(testSession)
	if rebuilt, err := source.Load(testSession); err != nil || rebuilt == nil {
		t.Fatalf("reset load = %v, %v", rebuilt, err)
	}
}

func TestSourceUnknownSessionIsNil(t *testing.T) {
	db, path := openFixture(t)
	seed(t, db)
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if snapshot, err := source.Load("sess-missing"); err != nil || snapshot != nil {
		t.Fatalf("unknown session = %v, %v", snapshot, err)
	}
}

func TestSourceUserAttachments(t *testing.T) {
	db, path := openFixture(t)
	seed(t, db)
	existing := filepath.Join(t.TempDir(), "pasted.txt")
	if err := os.WriteFile(existing, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p-file", "m1", testSession, 1000, 1000,
		`{"type":"file","mime":"text/plain","filename":"pasted.txt","source":{"type":"file","path":`+jsonQuote(existing)+`}}`, 1)
	artifactDir := filepath.Join(filepath.Dir(filepath.Dir(path)), "artifacts", testSession)
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	if err := os.WriteFile(filepath.Join(artifactDir, "attachment-1-tool-result-abc.txt"),
		[]byte("data:image/png;base64,"+encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	insertRow(t, db, `INSERT INTO part VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"p-image", "m1", testSession, 1000, 1000,
		`{"type":"file","mime":"image/png","filename":"shot.png","source":{"type":"file","path":"/does/not/exist"},"url":"zcode-artifact://`+testSession+`/tool-result-abc"}`, 2)
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	source.SetMediaDir(t.TempDir())
	snapshot, err := source.Load(testSession)
	if err != nil || snapshot == nil {
		t.Fatalf("load = %v/%v", snapshot, err)
	}
	messages := turnMessages(t, snapshot.Turns[0])
	attachments, _ := messages[0].MustGet("attachments").(*jsoncompat.Array)
	if attachments == nil || len(attachments.Items) != 2 {
		t.Fatalf("attachments = %v", attachments)
	}
	first, _ := attachments.Items[0].(*jsoncompat.Object)
	if kind, _ := first.MustGet("type").(string); kind != "file" {
		t.Fatalf("first attachment = %v", first)
	}
	second, _ := attachments.Items[1].(*jsoncompat.Object)
	imagePath, _ := second.MustGet("path").(string)
	if kind, _ := second.MustGet("type").(string); kind != "localImage" || imagePath == "" {
		t.Fatalf("second attachment = %v", second)
	}
	if data, err := os.ReadFile(imagePath); err != nil || string(data) != "png-bytes" {
		t.Fatalf("decoded artifact = %q/%v", data, err)
	}
}

func jsonQuote(value string) string {
	return string(jsoncompat.EncodePythonDefault(value))
}

func TestSourceStaleOpenTurn(t *testing.T) {
	db, path := openFixture(t)
	now := seed(t, db)
	source, err := OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	first, err := source.Load(testSession)
	if err != nil || first == nil {
		t.Fatalf("first = %v/%v", first, err)
	}
	if status, _ := first.Turns[len(first.Turns)-1].MustGet("status").(string); status != "inProgress" {
		t.Fatalf("fresh status = %q", status)
	}
	source.now = func() time.Time { return time.UnixMilli(now).Add(16 * time.Minute) }
	stale, err := source.Load(testSession)
	if err != nil || stale == nil {
		t.Fatalf("stale = %v/%v", stale, err)
	}
	if status, _ := stale.Turns[len(stale.Turns)-1].MustGet("status").(string); status != "interrupted" {
		t.Fatalf("stale status = %q", status)
	}
	if again, _ := source.Load(testSession); again != nil {
		t.Fatal("stale rebuild repeated")
	}
}

func turnMessages(t *testing.T, turn *jsoncompat.Object) []*jsoncompat.Object {
	t.Helper()
	list, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if list == nil {
		t.Fatal("turn has no messages")
	}
	messages := make([]*jsoncompat.Object, 0, len(list.Items))
	for _, item := range list.Items {
		message, _ := item.(*jsoncompat.Object)
		if message != nil {
			messages = append(messages, message)
		}
	}
	return messages
}
