package httpd

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	_ "modernc.org/sqlite"
)

func discoveryFixture(t *testing.T) (*Server, *sql.DB) {
	t.Helper()
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY, name TEXT, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT,
		rollout_path TEXT
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO threads
		(id,name,title,cwd,updated_at,archived,originator,source,rollout_path)
		VALUES ('desktop-1','First','First','/work',20,0,'Codex Desktop','desktop','/work/rollout.jsonl')`); err != nil {
		t.Fatal(err)
	}
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Store = store.NewSessionStore(home)
	t.Cleanup(func() { _ = db.Close() })
	return server, db
}

func TestListDiscoveryRevisionFolding(t *testing.T) {
	server, db := discoveryFixture(t)
	discovery := newListDiscovery(server)

	discovery.refresh()
	if revision := discovery.Revision(); revision != 1 {
		t.Fatalf("baseline revision=%d", revision)
	}

	// recency 变化不触发列表重载：由 activity 轮询单独下发。
	if _, err := db.Exec(`UPDATE threads SET updated_at = 30 WHERE id = 'desktop-1'`); err != nil {
		t.Fatal(err)
	}
	discovery.refresh()
	if revision := discovery.Revision(); revision != 1 {
		t.Fatalf("recency-only bump revision=%d", revision)
	}

	// 网页之外新开的会话使版本自增。
	if _, err := db.Exec(`INSERT INTO threads
		VALUES ('cli-1','CLI','CLI','/work',40,0,'codex-tui','cli','/work/cli.jsonl')`); err != nil {
		t.Fatal(err)
	}
	discovery.refresh()
	if revision := discovery.Revision(); revision != 2 {
		t.Fatalf("new session revision=%d", revision)
	}

	// 重命名同样使版本自增。
	if _, err := db.Exec(`UPDATE threads SET title = '改名' WHERE id = 'cli-1'`); err != nil {
		t.Fatal(err)
	}
	discovery.refresh()
	if revision := discovery.Revision(); revision != 3 {
		t.Fatalf("rename revision=%d", revision)
	}
}

func TestListDiscoveryPausesWhenUnwatched(t *testing.T) {
	server, db := discoveryFixture(t)
	discovery := newListDiscovery(server)
	defer discovery.close()
	discovery.interval = 10 * time.Millisecond
	discovery.idleAfter = time.Second
	discovery.watchedAt = time.Now().Add(-time.Hour)

	go discovery.run()
	if _, err := db.Exec(`INSERT INTO threads
		VALUES ('cli-1','CLI','CLI','/work',40,0,'codex-tui','cli','/work/cli.jsonl')`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if revision := discovery.Revision(); revision != 0 {
		t.Fatalf("unwatched revision=%d", revision)
	}

	discovery.touch()
	time.Sleep(100 * time.Millisecond)
	if revision := discovery.Revision(); revision == 0 {
		t.Fatal("watched discovery never refreshed")
	}
}

func TestActivityResponseCarriesListRevision(t *testing.T) {
	server, _ := discoveryFixture(t)
	server.StartDiscovery()
	defer server.StopDiscovery()
	token, session, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/activity", strings.NewReader(`{"hosts":{"local":["desktop-1"]}}`))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"listRevision"`) {
		t.Fatalf("activity status=%d body=%s", response.Code, response.Body.String())
	}
}
