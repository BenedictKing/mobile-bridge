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
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

// featureAgent is a python ACP agent advertising command skills after the
// handshake, recording prompts, steering and mode switches to files.
func featureAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	record := func(name, value string) {
		file, err := os.OpenFile(filepath.Join(dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString(value + "\n")
			_ = file.Close()
		}
	}
	_ = record
	script := `import json,os,sys
DIR = "` + dir + `"
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
NOTICE={"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"skill:translate","description":"翻译文本"},{"name":"compact","description":"内置"}]}}}
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  print(json.dumps(NOTICE),flush=True)
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new": result={"sessionId":"km-feature","modes":{"currentModeId":"default","availableModes":[{"id":"default"},{"id":"plan"}]},"configOptions":[]}
 elif method == "session/prompt":
  blocks=(m.get("params") or {}).get("prompt") or []
  text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
  record("prompts.txt", text)
  result={"stopReason":"end_turn"}
 elif method == "_session/steering":
  blocks=(m.get("params") or {}).get("prompt") or []
  text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
  record("steers.txt", text)
  result={"outcome":"injected"}
 elif method == "session/set_mode":
  record("modes.txt", (m.get("params") or {}).get("modeId",""))
  result={}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func recorded(t *testing.T, dir, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(raw)
}

func TestACPCommandSkillsSteerAndWorkMode(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(featureAgent(t, dir))
	manager.Label = "Kimi"
	manager.Capabilities = []string{"skills", "steer"}
	manager.SteerMethod = "_session/steering"
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8790"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8790")
		if body != "" {
			httptestRequest.Header.Set("Content-Type", "application/json")
			httptestRequest.Header.Set("X-CSRF-Token", csrf)
		}
		httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}

	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"技能会话","id":"7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), "km-feature") {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}

	// The advertised skill (minus builtins) is selectable, paged and echoes
	// back the selection.
	catalog := request(http.MethodGet, "/api/sessions/km-feature/catalog?kind=skills&q=trans&offset=0&limit=50", "")
	if catalog.Code != http.StatusOK ||
		!strings.Contains(catalog.Body.String(), `"name": "translate"`) ||
		strings.Contains(catalog.Body.String(), "compact") {
		t.Fatalf("skills catalog=%s", catalog.Body.String())
	}
	if !strings.Contains(catalog.Body.String(), `"total": 1`) {
		t.Fatalf("skills total missing: %s", catalog.Body.String())
	}

	// Sending with a skill prefixes the ACP slash command.
	send := request(http.MethodPost, "/api/sessions/km-feature/send",
		`{"text":"你好","skills":["skill:translate"]}`)
	if send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	time.Sleep(120 * time.Millisecond)
	if recorded(t, dir, "prompts.txt") != "/translate 你好\n" {
		t.Fatalf("prompt recorded=%q", recorded(t, dir, "prompts.txt"))
	}

	// Steer routes to the agent's steering RPC.
	steer := request(http.MethodPost, "/api/sessions/km-feature/send",
		`{"text":"补充一下","mode":"steer"}`)
	if steer.Code != http.StatusOK ||
		!strings.Contains(steer.Body.String(), `"steered": true`) {
		t.Fatalf("steer status=%d body=%s", steer.Code, steer.Body.String())
	}
	time.Sleep(120 * time.Millisecond)
	if recorded(t, dir, "steers.txt") != "补充一下\n" {
		t.Fatalf("steer recorded=%q", recorded(t, dir, "steers.txt"))
	}

	// Plan mode switches the session mode before prompting.
	plan := request(http.MethodPost, "/api/sessions/km-feature/send",
		`{"text":"做计划","workMode":"plan"}`)
	if plan.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", plan.Code, plan.Body.String())
	}
	time.Sleep(120 * time.Millisecond)
	if recorded(t, dir, "modes.txt") != "plan\n" {
		t.Fatalf("mode recorded=%q", recorded(t, dir, "modes.txt"))
	}

	// Goal mode is refused for ACP agents.
	goal := request(http.MethodPost, "/api/sessions/km-feature/send",
		`{"text":"目标","workMode":"goal"}`)
	if goal.Code != http.StatusBadRequest ||
		!strings.Contains(goal.Body.String(), "当前 Agent 不支持目标模式") {
		t.Fatalf("goal status=%d body=%s", goal.Code, goal.Body.String())
	}

	// Timeline meta carries the agent's capabilities (the caps clobber that
	// used to hide the phone controls is gone).
	meta := request(http.MethodGet, "/api/sessions/km-feature/timeline?limit=5", "")
	if !strings.Contains(meta.Body.String(), `"capabilities": ["skills", "steer"]`) {
		t.Fatalf("timeline caps missing: %s", meta.Body.String())
	}
}

func TestCodexCapabilitiesExcludeUnported(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	caps := server.codexCapabilities()
	body := string(jsoncompat.EncodePythonDefault(caps))
	for _, needle := range []string{"steer", "skills", "rename", "editMessage", "fastMode",
		"fullHistory", "serviceTier", "accounts", "notificationScan", "fork"} {
		if !strings.Contains(body, needle) {
			t.Fatalf("caps missing %s: %s", needle, body)
		}
	}
	// The native goal send chain now rides the command ledger + sidecar RPC
	// with activation turns; fork branches work through the app-server's
	// thread/fork. Both are advertised for codex only.
	if !strings.Contains(body, "goals") {
		t.Fatalf("goal cap missing now that the send chain is ported: %s", body)
	}
}

func TestValidateMessageEdit(t *testing.T) {
	view := jsoncompat.NewObject()
	view.Set("id", "t")
	view.Set("sequence", jsoncompat.Num(1))
	turn := func(id string, messages ...[2]string) *jsoncompat.Object {
		object := jsoncompat.NewObject()
		object.Set("id", id)
		array := &jsoncompat.Array{}
		for _, pair := range messages {
			message := jsoncompat.NewObject()
			message.Set("role", pair[0])
			message.Set("text", pair[1])
			if pair[0] == "user" {
				message.Set("kind", "userMessage")
			}
			array.Items = append(array.Items, message)
		}
		object.Set("messages", array)
		return object
	}
	turns := &jsoncompat.Array{}
	turns.Items = append(turns.Items,
		turn("turn-1", [2]string{"user", "第一问"}),
		turn("turn-2", [2]string{"user", "第二问"}, [2]string{"assistant", "回答"}))
	view.Set("turns", turns)
	projection := timeline.New(nil)
	projection.Update(view)
	page := projection.Page(20, "")
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	first, _ := rows.Items[0].(*jsoncompat.Object)
	second, _ := rows.Items[1].(*jsoncompat.Object) // the latest user turn's row
	firstKey := projection.Cursor(stringField(first, "key"))
	firstVersion, _ := first.MustGet("version").(string)
	secondKey := projection.Cursor(stringField(second, "key"))

	if _, message := validateMessageEdit(projection, firstVersion, "nope"); message != "消息位置已变化，请刷新后重试" {
		t.Fatalf("missing row message=%q", message)
	}
	if _, message := validateMessageEdit(projection, "wrong", firstKey); message != "消息内容已变化，请重新打开编辑" {
		t.Fatalf("version message=%q", message)
	}
	if _, message := validateMessageEdit(projection, firstVersion, firstKey); message != "只有最近一条消息可原地编辑，请使用编辑并新建分支" {
		t.Fatalf("latest message=%q", message)
	}
	secondVersion, _ := second.MustGet("version").(string)
	row, message := validateMessageEdit(projection, secondVersion, secondKey)
	if message != "" || row == nil {
		t.Fatalf("latest edit rejected: %q", message)
	}
}

func TestCodexRenameRoute(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `#!/usr/bin/env python3
import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize": result={"ok":True}
 elif method=="thread/name/set": result={"ok":True}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	executable := filepath.Join(t.TempDir(), "app-server")
	if err := os.WriteFile(executable, []byte(strings.Replace(script, "#!/usr/bin/env python3", "#!"+python, 1)), 0o700); err != nil {
		t.Fatal(err)
	}
	bridge := codex.New(t.TempDir(), "unused-endpoint")
	bridge.Executable = executable

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Codex = bridge

	request := func(target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8790"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8790")
		httptestRequest.Header.Set("Content-Type", "application/json")
		httptestRequest.Header.Set("X-CSRF-Token", csrf)
		httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}
	threadID := "5f0a1111-2222-4333-8444-555566667777"
	renamed := request("/api/sessions/"+threadID+"/rename", `{"title":"新名字"}`)
	if renamed.Code != http.StatusOK ||
		!strings.Contains(renamed.Body.String(), `"title": "新名字"`) ||
		!strings.Contains(renamed.Body.String(), `"id": "`+threadID+`"`) {
		t.Fatalf("rename status=%d body=%s", renamed.Code, renamed.Body.String())
	}
	long := strings.Repeat("长", 121)
	if response := request("/api/sessions/"+threadID+"/rename", `{"title":"`+long+`"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("long title status=%d", response.Code)
	}
	if response := request("/api/sessions/"+threadID+"/rename", `{"title":"坏\n名字"}`); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "不能包含换行") {
		t.Fatalf("newline title status=%d body=%s", response.Code, response.Body.String())
	}
}
