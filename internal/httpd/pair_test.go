package httpd

import (
	"database/sql"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/pairing"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func TestPairExchangeCreatesAuthenticatedSession(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Hosts["example.com"] = true
	server.Origins["https://example.com"] = true
	server.Pairing = pairing.New([]string{"https://example.com"})
	grant, err := server.Pairing.Control(jsonObject(t, `{"action":"create","url":"https://example.com"}`), float64(time.Now().Unix()), func() string {
		return strings.Repeat("t", 43)
	}, func() string { return "grant-1" })
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "http://example.com/api/pair", strings.NewReader(`{"token":"`+strings.Repeat("t", 43)+`"}`))
	request.Host = "example.com"
	request.Header.Set("Origin", "https://example.com")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if response.Header().Get("Set-Cookie") == "" || !strings.Contains(response.Body.String(), `"csrf"`) {
		t.Fatalf("missing session response: headers=%v body=%s", response.Header(), response.Body.String())
	}
	if grant.MustGet("state") != "active" {
		t.Fatal("control result unexpectedly changed")
	}
}

func TestHistoricalTimelineUsesGoStore(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(home, "sessions", "rollout.jsonl")
	if err := os.WriteFile(rollout, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"text","text":"hello"}]}}
{"type":"event_msg","payload":{"type":"task_complete"}}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT,
		updated_at INTEGER, archived INTEGER, originator TEXT, source TEXT, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO threads VALUES ('desktop-1','History','/work',1,0,'Codex Desktop','desktop',?)`, rollout); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, _, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Store = store.NewSessionStore(home)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/sessions/desktop-1/timeline", nil)
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "hello") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestUploadAndPreviewRoutes(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Uploads = uploads.New(t.TempDir())
	id := uuid.NewString()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/sessions/thread-1/uploads?id="+id+"&name=photo.png", strings.NewReader("\x89PNG\r\n\x1a\nbody"))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status=%d body=%s", response.Code, response.Body.String())
	}
	preview := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/sessions/thread-1/uploads/"+id+"/preview", nil)
	preview.Host = "127.0.0.1:8790"
	preview.Header.Set("Origin", "http://127.0.0.1:8790")
	preview.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response = httptest.NewRecorder()
	server.ServeHTTP(response, preview)
	if response.Code != http.StatusOK || !strings.HasPrefix(response.Body.String(), "\x89PNG") {
		t.Fatalf("preview status=%d body=%q", response.Code, response.Body.String())
	}
}

type fsys struct{}

func (fsys) Open(string) (fs.File, error) { return nil, fs.ErrNotExist }

func jsonObject(t *testing.T, raw string) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(*jsoncompat.Object)
}
