package httpd

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
)

const remoteStateFixture = `{
  "codex-managed-remote-connections": [
    {"hostId": "ssh-dev", "displayName": "开发机", "source": "discovered", "alias": "devbox"},
    {"hostId": "ssh-idle", "displayName": "闲置", "alias": "idle"},
    {"hostId": "ssh-down", "alias": "down"}
  ],
  "remote-projects": [
    {"id": "p-app", "hostId": "ssh-dev", "remotePath": "/srv/app", "label": "应用"},
    {"id": "p-x", "hostId": "ssh-down", "remotePath": "/x"}
  ]
}`

const remoteRolloutFixture = `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"远端问题"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"远端回答"}]}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1"}}
`

var remoteRequestLiteral = regexp.MustCompile(`b64decode\("([A-Za-z0-9+/=]+)"\)`)

// fakeRemote answers helper requests per alias and counts contacts, standing
// in for ssh + the remote python helper.
type fakeRemote struct {
	mu    sync.Mutex
	calls map[string]int
}

func (f *fakeRemote) count(alias string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[alias]
}

func (f *fakeRemote) run(t *testing.T) store.RemoteRunner {
	return func(ctx context.Context, alias, script string) ([]byte, error) {
		f.mu.Lock()
		f.calls[alias]++
		f.mu.Unlock()
		switch alias {
		case "down":
			return nil, store.ErrRemoteUnavailable
		case "devbox":
		default:
			t.Errorf("host %q must not be contacted", alias)
			return nil, store.ErrRemoteUnavailable
		}
		match := remoteRequestLiteral.FindStringSubmatch(script)
		if match == nil {
			t.Fatalf("script carries no request")
		}
		raw, _ := base64.StdEncoding.DecodeString(match[1])
		value, _ := jsoncompat.Unmarshal(raw)
		request := value.(*jsoncompat.Object)
		switch request.MustGet("op") {
		case "schema":
			return []byte(`{"columns":["id","title","cwd","updated_at","originator","source"]}`), nil
		case "query":
			return []byte(`{"rows":[["remote-1","远端会话","/srv/app/api",30,"Codex Desktop","vscode"]]}`), nil
		case "rollout":
			if request.MustGet("thread_id") != "remote-1" {
				return []byte(`{"error":"找不到这个会话"}`), nil
			}
			reply := jsoncompat.NewObject()
			row := jsoncompat.NewObject()
			row.Set("id", "remote-1")
			row.Set("title", "远端会话")
			row.Set("cwd", "/srv/app/api")
			row.Set("originator", "Codex Desktop")
			row.Set("source", "vscode")
			reply.Set("row", row)
			reply.Set("content", remoteRolloutFixture)
			reply.Set("truncated", false)
			return jsoncompat.EncodeCompact(reply), nil
		}
		return []byte(`{"error":"未知的远端读取操作"}`), nil
	}
}

func newRemoteServer(t *testing.T) (*Server, string, string, *fakeRemote) {
	t.Helper()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".codex-global-state.json"), []byte(remoteStateFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	server.CodexHome = home
	remote := &fakeRemote{calls: map[string]int{}}
	server.remoteRun = remote.run(t)
	return server, token, csrf, remote
}

func decodeBody(t *testing.T, response *httptest.ResponseRecorder) *jsoncompat.Object {
	t.Helper()
	value, err := jsoncompat.Unmarshal(response.Body.Bytes())
	if err != nil {
		t.Fatalf("body is not JSON: %s", response.Body.String())
	}
	return value.(*jsoncompat.Object)
}

func TestRemoteHostsJoinListAndProjectsWithoutCrossingHosts(t *testing.T) {
	server, token, csrf, remote := newRemoteServer(t)
	server.PreloadRecentSessions()
	if remote.count("devbox")+remote.count("down") != 0 {
		t.Fatal("startup warm-up must not reach over SSH")
	}

	response := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	sessions := body.MustGet("sessions").(*jsoncompat.Array)
	if len(sessions.Items) != 1 {
		t.Fatalf("want one remote row: %s", response.Body.String())
	}
	row := sessions.Items[0].(*jsoncompat.Object)
	for key, want := range map[string]string{"id": "remote-1", "host": "ssh-dev", "hostLabel": "开发机",
		"agent": "codex", "groupKey": "ssh-dev|p-app", "projectKey": "ssh-dev|p-app", "projectName": "应用"} {
		if got := stringField(row, key); got != want {
			t.Fatalf("row %s=%q want %q: %s", key, got, want, response.Body.String())
		}
	}
	unavailable := body.MustGet("unavailableHosts").(*jsoncompat.Array)
	if len(unavailable.Items) != 1 || stringField(unavailable.Items[0].(*jsoncompat.Object), "host") != "ssh-down" ||
		stringField(unavailable.Items[0].(*jsoncompat.Object), "error") != store.ErrRemoteUnavailable.Error() {
		t.Fatalf("down host must be reported alone: %s", response.Body.String())
	}

	// A failed host is not retried on every list request.
	featureRequest(server, token, csrf, http.MethodGet, "/api/sessions", "")
	if remote.count("down") != 1 {
		t.Fatalf("failed host retried within backoff: %d", remote.count("down"))
	}
	// Agent filters that exclude codex never reach the hosts.
	before := remote.count("devbox")
	featureRequest(server, token, csrf, http.MethodGet, "/api/sessions?agent=kimi", "")
	if remote.count("devbox") != before {
		t.Fatal("non-codex list must not read SSH hosts")
	}

	projects := featureRequest(server, token, csrf, http.MethodGet, "/api/projects", "")
	if !strings.Contains(projects.Body.String(), `"key": "ssh-dev|p-app"`) ||
		!strings.Contains(projects.Body.String(), `"hostLabel": "开发机"`) {
		t.Fatalf("saved remote project missing: %s", projects.Body.String())
	}
	if cwd := (&handler{server: server}).resolveProjectCwd("ssh-dev|p-app"); cwd != "" {
		t.Fatalf("remote project resolved to local cwd %q", cwd)
	}
	create := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions",
		`{"id":"6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","title":"新聊天","project":"ssh-dev|p-app"}`)
	if create.Code != http.StatusConflict || !strings.Contains(create.Body.String(), "SSH 项目暂不支持") {
		t.Fatalf("remote create status=%d body=%s", create.Code, create.Body.String())
	}
}

func TestRemoteSessionRoutesReadHistoryAndRefuseWrites(t *testing.T) {
	server, token, csrf, _ := newRemoteServer(t)
	view := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/remote-1?host=ssh-dev", "")
	if view.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", view.Code, view.Body.String())
	}
	state := decodeBody(t, view)
	if state.MustGet("readOnly") != true || stringField(state, "readOnlyHint") != remoteReadOnlyHint ||
		stringField(state, "host") != "ssh-dev" || !strings.Contains(view.Body.String(), "远端回答") {
		t.Fatalf("remote view: %s", view.Body.String())
	}

	page := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/remote-1/timeline?host=ssh-dev&limit=20", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "远端回答") {
		t.Fatalf("timeline status=%d body=%s", page.Code, page.Body.String())
	}
	meta := decodeBody(t, page).MustGet("meta").(*jsoncompat.Object)
	if stringField(meta, "host") != "ssh-dev" || meta.MustGet("readOnly") != true {
		t.Fatalf("timeline meta must carry host and read-only: %s", page.Body.String())
	}
	changes := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/remote-1/changes?host=ssh-dev&after=-1", "")
	if changes.Code != http.StatusOK || !strings.Contains(changes.Body.String(), "远端回答") {
		t.Fatalf("changes status=%d body=%s", changes.Code, changes.Body.String())
	}
	// The remote projection is scoped by host: the local slot for the same id
	// stays untouched.
	if server.lookupProjection("remote-1") != nil {
		t.Fatal("remote read created a local projection slot")
	}

	for _, write := range []struct{ path, body string }{
		{"/api/sessions/remote-1/send?host=ssh-dev", `{"text":"hi"}`},
		{"/api/sessions/remote-1/rename?host=ssh-dev", `{"title":"x"}`},
		{"/api/sessions/remote-1/delete?host=ssh-dev", `{}`},
		{"/api/sessions/remote-1/goal/cancel?host=ssh-dev", `{}`},
	} {
		response := featureRequest(server, token, csrf, http.MethodPost, write.path, write.body)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), remoteReadOnlyHint) {
			t.Fatalf("%s status=%d body=%s", write.path, response.Code, response.Body.String())
		}
	}
	catalog := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/remote-1/catalog?host=ssh-dev", "")
	if catalog.Code != http.StatusOK || !strings.Contains(catalog.Body.String(), `"models": []`) {
		t.Fatalf("remote catalog must stay empty, never local: %d %s", catalog.Code, catalog.Body.String())
	}
	// Opening a chat always posts reconnect; a remote one answers read-only
	// instead of failing, so the phone shows no error toast.
	reconnect := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/remote-1/reconnect?host=ssh-dev", `{"activate":true}`)
	if reconnect.Code != http.StatusOK || decodeBody(t, reconnect).MustGet("readOnly") != true {
		t.Fatalf("remote reconnect status=%d body=%s", reconnect.Code, reconnect.Body.String())
	}
	missing := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/missing-1?host=ssh-dev", "")
	if missing.Code != http.StatusConflict || !strings.Contains(missing.Body.String(), "找不到这个会话") {
		t.Fatalf("missing thread status=%d body=%s", missing.Code, missing.Body.String())
	}
	for _, host := range []string{"ssh-unknown", "ssh-idle"} {
		response := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/remote-1?host="+host, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf("host %s status=%d", host, response.Code)
		}
	}
}

func TestRemoteSessionStreamSynchronizesThenOnlyHeartbeats(t *testing.T) {
	server, token, _, _ := newRemoteServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:8791/api/sessions/remote-1/events?host=ssh-dev&incremental=1&sync=1", nil).WithContext(ctx)
	request.Host = "127.0.0.1:8791"
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	body := response.Body.String()
	ready, changes, synced := strings.Index(body, "event: ready"), strings.Index(body, "event: reset"), strings.Index(body, "event: synchronized")
	if changes < 0 {
		changes = strings.Index(body, "event: changes")
	}
	if ready < 0 || changes < ready || synced < changes || !strings.Contains(body, "远端回答") {
		t.Fatalf("stream frames out of order:\n%s", body)
	}
}
