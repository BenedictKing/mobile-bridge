package httpd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// fakeACPAgent is a stateless python ACP agent: one listed session, lazy
// session/new placeholder ids, echoing loads and prompts.
func fakeACPAgent(t *testing.T, cwd string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[{"sessionId":"sess-old","title":"旧会话","cwd":"` + cwd + `","updatedAt":1}]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new": result={"sessionId":"uuid-new","modes":{"currentModeId":"build"},"configOptions":[]}
 elif method == "session/prompt": result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func TestACPAgentEndToEndRouting(t *testing.T) {
	cwd := t.TempDir()
	manager := acpruntime.NewCommand(fakeACPAgent(t, cwd))
	manager.Label = "ZCode"
	manager.EffortMap = map[string]string{"low": "low", "high": "high"}
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		request := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, reader)
		request.Host = "127.0.0.1:8790"
		request.Header.Set("Origin", "http://127.0.0.1:8790")
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CSRF-Token", csrf)
		}
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	// Creating a chat returns the agent's native (placeholder) id and the
	// owning kind; no desktop handoff is involved.
	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"新聊天","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	if !strings.Contains(create.Body.String(), `"id": "uuid-new"`) ||
		!strings.Contains(create.Body.String(), `"agent": "zcode"`) ||
		!strings.Contains(create.Body.String(), `"host": "local"`) {
		t.Fatalf("create body=%s", create.Body.String())
	}

	// The session list merges the agent's own row and the pending
	// placeholder, both routed without any agent query parameter.
	list := request(http.MethodGet, "/api/sessions", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"sess-old"`) || !strings.Contains(list.Body.String(), `"uuid-new"`) {
		t.Fatalf("list body=%s", list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"新聊天"`) || !strings.Contains(list.Body.String(), `"agent": "zcode"`) {
		t.Fatalf("list body missing pending row fields: %s", list.Body.String())
	}

	// The projects payload advertises the agent roster (codex + zcode).
	projects := request(http.MethodGet, "/api/projects", "")
	if projects.Code != http.StatusOK ||
		!strings.Contains(projects.Body.String(), `"kind": "zcode"`) ||
		!strings.Contains(projects.Body.String(), `"label": "ZCode"`) ||
		!strings.Contains(projects.Body.String(), `"kind": "codex"`) {
		t.Fatalf("projects status=%d body=%s", projects.Code, projects.Body.String())
	}

	// Opening the created chat (no agent param) routes by ownership and
	// returns the ACP view with the agent kind.
	view := request(http.MethodGet, "/api/sessions/uuid-new", "")
	if view.Code != http.StatusOK {
		t.Fatalf("view status=%d body=%s", view.Code, view.Body.String())
	}
	for _, needle := range []string{`"agent": "zcode"`, `"title": "新聊天"`, `"capabilities": []`, `"host": "local"`} {
		if !strings.Contains(view.Body.String(), needle) {
			t.Fatalf("view body missing %s: %s", needle, view.Body.String())
		}
	}

	// Sending a message reflects into the live view and the timeline through
	// the shared projection.
	send := request(http.MethodPost, "/api/sessions/uuid-new/send", `{"text":"你好"}`)
	if send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	after := request(http.MethodGet, "/api/sessions/uuid-new", "")
	if !strings.Contains(after.Body.String(), "你好") {
		t.Fatalf("view after send=%s", after.Body.String())
	}
	timeline := request(http.MethodGet, "/api/sessions/uuid-new/timeline?limit=20", "")
	if timeline.Code != http.StatusOK || !strings.Contains(timeline.Body.String(), "你好") {
		t.Fatalf("timeline status=%d body=%s", timeline.Code, timeline.Body.String())
	}
	if !strings.Contains(timeline.Body.String(), `"agent": "zcode"`) {
		t.Fatalf("timeline meta missing agent kind: %s", timeline.Body.String())
	}

	// An unknown agent parameter is rejected like Python's registry.
	unknown := request(http.MethodGet, "/api/sessions/uuid-new?agent=nope", "")
	if unknown.Code != http.StatusBadRequest || !strings.Contains(unknown.Body.String(), "未知的 Agent 类型") {
		t.Fatalf("unknown agent status=%d body=%s", unknown.Code, unknown.Body.String())
	}
}

func jsonText(value string) string {
	return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"`
}

func TestACPCreateIsIdempotentPerRequestID(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	cwd := t.TempDir()
	counter := filepath.Join(t.TempDir(), "creates.txt")
	script := `import json,sys,os
COUNTER = "` + counter + `"
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new":
  n = 0
  try:
   n = int(open(COUNTER).read() or "0")
  except OSError:
   pass
  open(COUNTER, "w").write(str(n + 1))
  result={"sessionId":"uuid-new-%d" % n}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	server.Creations = LoadCreations(filepath.Join(t.TempDir(), "creations.json"))

	create := func(body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/sessions", strings.NewReader(body))
		request.Host = "127.0.0.1:8790"
		request.Header.Set("Origin", "http://127.0.0.1:8790")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", csrf)
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}

	payload := `{"agent":"zcode","cwd":` + jsonText(cwd) + `,"title":"幂等测试","id":"11111111-2222-4333-8444-555555555555"}`
	first := create(payload)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "uuid-new-0") {
		t.Fatalf("first create status=%d body=%s", first.Code, first.Body.String())
	}
	// A retry with the same request id returns the same chat and never
	// reaches the agent again.
	retry := create(payload)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), "uuid-new-0") {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	raw, readErr := os.ReadFile(counter)
	if readErr != nil || strings.TrimSpace(string(raw)) != "1" {
		t.Fatalf("agent created %q times (err=%v)", strings.TrimSpace(string(raw)), readErr)
	}
	// The same id with different content is rejected.
	conflict := create(`{"agent":"zcode","cwd":` + jsonText(cwd) + `,"title":"另一个标题","id":"11111111-2222-4333-8444-555555555555"}`)
	if conflict.Code != http.StatusBadRequest || !strings.Contains(conflict.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
	// A malformed request id never reaches the ledger.
	bad := create(`{"agent":"zcode","cwd":` + jsonText(cwd) + `,"title":"x","id":"not-a-uuid"}`)
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "创建请求标识无效") {
		t.Fatalf("bad id status=%d body=%s", bad.Code, bad.Body.String())
	}
}

func TestSplitACPOwnedRoutesIdsByOwnership(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[{"sessionId":"sess-owned","title":"x","cwd":"/w","updatedAt":1}]}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	// Warm the list cache so Knows resolves without the cold probe.
	if _, err := manager.List(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	owned, remaining := splitACPOwned(map[string]*acpruntime.Manager{"zcode": manager},
		[]string{"sess-owned", "codex-uuid-1"})
	if len(owned) != 1 || owned[0] != "sess-owned" {
		t.Fatalf("owned=%v", owned)
	}
	if len(remaining) != 1 || remaining[0] != "codex-uuid-1" {
		t.Fatalf("remaining=%v", remaining)
	}
}

func TestSessionListPaginatesMergedRows(t *testing.T) {
	cwd := t.TempDir()
	manager := acpruntime.NewCommand(fakeACPAgent(t, cwd))
	manager.Label = "ZCode"
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	request := func(target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+target, nil)
		request.Host = "127.0.0.1:8790"
		request.Header.Set("Origin", "http://127.0.0.1:8790")
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	// One listed row plus one bridge-created pending row gives the merged
	// list two entries to paginate.
	create := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/sessions",
		strings.NewReader(`{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"分页夹具","id":"5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`))
	create.Host = "127.0.0.1:8790"
	create.Header.Set("Origin", "http://127.0.0.1:8790")
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set("X-CSRF-Token", csrf)
	create.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	createResponse := httptest.NewRecorder()
	server.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}

	full := request("/api/sessions?agent=zcode")
	if full.Code != http.StatusOK {
		t.Fatalf("status=%d", full.Code)
	}
	page := request("/api/sessions?agent=zcode&offset=1")
	if page.Code != http.StatusOK {
		t.Fatalf("status=%d", page.Code)
	}
	count := func(body string) int {
		value, err := jsoncompat.Unmarshal([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := value.(*jsoncompat.Object).MustGet("sessions").(*jsoncompat.Array)
		return len(rows.Items)
	}
	total := count(full.Body.String())
	if total < 2 {
		t.Fatalf("fixture rows=%d", total)
	}
	if paged := count(page.Body.String()); paged != total-1 {
		t.Fatalf("offset page=%d total=%d", paged, total)
	}
	if beyond := count(request("/api/sessions?agent=zcode&offset=99").Body.String()); beyond != 0 {
		t.Fatalf("offset beyond end=%d", beyond)
	}
	_ = csrf
}
