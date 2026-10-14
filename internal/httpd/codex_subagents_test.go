package httpd

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	_ "modernc.org/sqlite"
)

func TestHistoricalTimelineIncludesCodexSubagents(t *testing.T) {
	home := t.TempDir()
	sessions := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	parentRollout := filepath.Join(sessions, "parent.jsonl")
	childRollout := filepath.Join(sessions, "child.jsonl")
	if err := os.WriteFile(parentRollout, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"主会话消息"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childRollout, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-c"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"子代理结论"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE threads (id TEXT PRIMARY KEY, name TEXT, title TEXT, cwd TEXT,
			created_at INTEGER, created_at_ms INTEGER, updated_at INTEGER, updated_at_ms INTEGER,
			archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`,
		`CREATE TABLE thread_spawn_edges (parent_thread_id TEXT, child_thread_id TEXT, status TEXT)`,
		`INSERT INTO threads VALUES ('parent-h','历史会话','历史会话','/work',1,1000,9,9000,0,'Codex Desktop','desktop','` + parentRollout + `')`,
		`INSERT INTO threads VALUES ('child-h','检索','检索','/work',2,2000,8,8000,0,'codex_cli_rs','{}','` + childRollout + `')`,
		`INSERT INTO thread_spawn_edges VALUES ('parent-h','child-h','completed')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, _, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Store = store.NewSessionStore(home)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/sessions/parent-h/timeline?limit=20", nil)
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, needle := range []string{"主会话消息", `"subagentRevision": 8000`, "子 Agent", "子代理结论"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("timeline missing %s: %s", needle, body)
		}
	}
}
