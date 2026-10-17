package httpd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// acpForkAgent is a python ACP fixture for the synthesized branch routes:
// session/new mints unique "fk-<n>" ids (counted), session/prompt logs the
// prompt text (NUL-separated) and streams one agent message chunk so the
// turn completes with a forkable assistant row.
func acpForkAgent(t *testing.T) (argv []string, creates, prompts string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	logs := t.TempDir()
	creates, prompts = filepath.Join(logs, "creates.txt"), filepath.Join(logs, "prompts.log")
	script := `import json,sys,os
CREATES = "` + creates + `"
PROMPTS = "` + prompts + `"
def bump(path):
 n = 0
 try:
  n = int(open(path).read() or "0")
 except OSError:
  pass
 open(path, "w").write(str(n + 1))
 return n
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new": result={"sessionId":"fk-%02d" % bump(CREATES)}
 elif method == "session/prompt":
  blocks=(m.get("params") or {}).get("prompt") or []
  text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
  with open(PROMPTS, "a") as fh:
   fh.write(text)
   fh.write("\n\x00\n")
  n = open(PROMPTS).read().count("\x00")
  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"回答%d" % n}}}}),flush=True)
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}, creates, prompts
}

// acpForkHarness wires one fake-agent manager into a server with the forks
// ledger and an authenticated request helper.
type acpForkHarness struct {
	server  *Server
	cwd     string
	creates string
	prompts string
	request func(method, target, body string) *httptest.ResponseRecorder
}

func newACPForkHarness(t *testing.T) *acpForkHarness {
	t.Helper()
	cwd := t.TempDir()
	argv, creates, prompts := acpForkAgent(t)
	manager := acpruntime.NewCommand(argv)
	manager.Label = "ZCode"
	manager.Capabilities = []string{"fork"}
	t.Cleanup(manager.Close)

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	server.Forks = LoadForks(filepath.Join(t.TempDir(), "forks.json"))

	h := &acpForkHarness{server: server, cwd: cwd, creates: creates, prompts: prompts}
	h.request = func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
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
	return h
}

// nextUUID mints a fresh request id: the message-action ledger is a package
// -level map shared across the whole test binary, so identities must never
// repeat across harnesses.
var forkIdentity int64

func (h *acpForkHarness) nextUUID() string {
	n := atomic.AddInt64(&forkIdentity, 1)
	return fmt.Sprintf("6aaaaaaa-bbbb-4ccc-8ddd-%012d", n)
}

// createAndTalk creates one chat and exchanges the given turns on it. The
// fake agent answers each prompt with 回答<n>, so completed turns carry a
// forkable assistant row.
func (h *acpForkHarness) createAndTalk(t *testing.T, title string, questions ...string) string {
	t.Helper()
	create := h.request(http.MethodPost, "/api/sessions",
		`{"agent":"zcode","cwd":`+jsonText(h.cwd)+`,"title":`+jsonText(title)+`,"id":"`+h.nextUUID()+`"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	value, err := jsoncompat.Unmarshal([]byte(create.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	id := stringField(value.(*jsoncompat.Object), "id")
	for _, question := range questions {
		response := h.request(http.MethodPost, "/api/sessions/"+id+"/send", `{"text":`+jsonText(question)+`}`)
		if response.Code != http.StatusOK {
			t.Fatalf("send %q status=%d body=%s", question, response.Code, response.Body.String())
		}
		time.Sleep(150 * time.Millisecond)
	}
	return id
}

// timelineRow returns the first projection row matching want, with the
// request cursor stashed under "cursor".
func (h *acpForkHarness) timelineRow(t *testing.T, id string, want func(*jsoncompat.Object) bool) *jsoncompat.Object {
	t.Helper()
	rows, _ := h.timelineRows(t, id)
	for _, row := range rows {
		if want(row) {
			return row
		}
	}
	t.Fatalf("no matching timeline row for %s", id)
	return nil
}

func (h *acpForkHarness) timelineRows(t *testing.T, id string) ([]*jsoncompat.Object, string) {
	t.Helper()
	response := h.request(http.MethodGet, "/api/sessions/"+id+"/timeline?limit=100", "")
	if response.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", response.Code, response.Body.String())
	}
	value, err := jsoncompat.Unmarshal([]byte(response.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	page := value.(*jsoncompat.Object)
	epoch, _ := page.MustGet("epoch").(string)
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	out := []*jsoncompat.Object{}
	for _, item := range rows.Items {
		if row, ok := item.(*jsoncompat.Object); ok {
			row.Set("cursor", epoch+"."+stringField(row, "key"))
			out = append(out, row)
		}
	}
	return out, epoch
}

func (h *acpForkHarness) recordedPrompts(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(h.prompts)
	if err != nil {
		t.Fatalf("prompts log missing: %v", err)
	}
	parts := strings.Split(string(raw), "\n\x00\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

func (h *acpForkHarness) createCount(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(h.creates)
	if err != nil {
		return 0
	}
	count := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &count); err != nil {
		t.Fatalf("bad create counter %q", string(raw))
	}
	return count
}

func assistantRow(row *jsoncompat.Object) bool {
	role, _ := row.MustGet("role").(string)
	forkable, _ := row.MustGet("forkable").(bool)
	return role == "assistant" && forkable
}

func userRowWithTurn(turnID string) func(*jsoncompat.Object) bool {
	return func(row *jsoncompat.Object) bool {
		role, _ := row.MustGet("role").(string)
		editable, _ := row.MustGet("editable").(bool)
		rowTurn, _ := row.MustGet("turnId").(string)
		return role == "user" && editable && rowTurn == turnID
	}
}

func forkBody(t *testing.T, action, id string, row *jsoncompat.Object, text string) string {
	t.Helper()
	body := `{"action":` + jsonText(action) + `,"id":"` + id + `","key":` +
		jsonText(stringField(row, "cursor")) + `,"version":` + jsonText(stringField(row, "version"))
	if action != "fork" {
		body += `,"text":` + jsonText(text)
	}
	return body + `,"uiLocale":"zh-CN"}`
}

func TestACPMessageActionForkSynthesizesBranch(t *testing.T) {
	h := newACPForkHarness(t)
	source := h.createAndTalk(t, "源会话", "问甲", "问乙")
	if source != "fk-00" {
		t.Fatalf("source id=%q", source)
	}
	// Settled ACP turns fold into forkable assistant rows (the capability
	// annotation), and the view advertises the fork capability again.
	view := h.request(http.MethodGet, "/api/sessions/"+source, "")
	if view.Code != http.StatusOK || !strings.Contains(view.Body.String(), `"capabilities": ["fork"]`) {
		t.Fatalf("view capabilities=%s", view.Body.String())
	}
	rows, _ := h.timelineRows(t, source)
	var row *jsoncompat.Object
	for _, candidate := range rows {
		if assistantRow(candidate) {
			row = candidate // the last forkable assistant row (turn-2)
		}
	}
	if row == nil {
		t.Fatalf("no forkable assistant row for %s", source)
	}
	turnID := stringField(row, "turnId")
	if turnID == "" {
		t.Fatalf("assistant row missing turnId: %s", string(jsoncompat.EncodePythonDefault(row)))
	}

	body := forkBody(t, "fork", h.nextUUID(), row, "")
	fork := h.request(http.MethodPost, "/api/sessions/"+source+"/message-action", body)
	if fork.Code != http.StatusOK {
		t.Fatalf("fork status=%d body=%s", fork.Code, fork.Body.String())
	}
	value, err := jsoncompat.Unmarshal([]byte(fork.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	result := value.(*jsoncompat.Object)
	if got := stringField(result, "status"); got != "created" {
		t.Fatalf("fork status=%q body=%s", got, fork.Body.String())
	}
	child := stringField(result, "id")
	if child != "fk-01" {
		t.Fatalf("fork child=%q body=%s", child, fork.Body.String())
	}
	if host := stringField(result, "host"); host != "local" {
		t.Fatalf("fork host=%q", host)
	}
	if draft := result.MustGet("draft"); draft != nil {
		t.Fatalf("fork draft=%v", draft)
	}
	origin := objectOf(result.MustGet("source"))
	if origin == nil || stringField(origin, "id") != source || stringField(origin, "host") != "local" ||
		stringField(origin, "title") != "源会话" || stringField(origin, "turnId") != turnID {
		t.Fatalf("fork source=%s", string(jsoncompat.EncodePythonDefault(origin)))
	}

	// The child exists on the same manager and its first prompt carries the
	// full two-turn transcript plus the branch framing — not the convention
	// hint reserved for user-initiated sends.
	var prompts []string
	for deadline := time.Now().Add(2 * time.Second); ; {
		prompts = h.recordedPrompts(t)
		if len(prompts) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(prompts) != 3 {
		t.Fatalf("prompt count=%d", len(prompts))
	}
	transcript := prompts[2]
	for _, needle := range []string{"【从 ZCode 分支的会话】", "标题：源会话", "用户：问甲",
		"助手：回答1", "用户：问乙", "助手：回答2", "分支出来的会话转录，请在此基础上继续"} {
		if !strings.Contains(transcript, needle) {
			t.Fatalf("transcript missing %q: %s", needle, transcript)
		}
	}
	if strings.Contains(transcript, "产物") {
		t.Fatalf("transcript leaked the result-visibility convention: %s", transcript)
	}
	if count := h.createCount(t); count != 2 {
		t.Fatalf("agent created %d sessions, want 2", count)
	}
	if recorded, ok := h.server.Forks.Origin(child); !ok || recorded.ID != source || recorded.Title != "源会话" {
		t.Fatalf("forks ledger=%+v", recorded)
	}

	// The child timeline links back to its source through meta.forkedFrom.
	childPage := h.request(http.MethodGet, "/api/sessions/"+child+"/timeline?limit=10", "")
	if !strings.Contains(childPage.Body.String(),
		`"forkedFrom": {"id": "fk-00", "host": "local", "title": "源会话"}`) {
		t.Fatalf("child timeline meta=%s", childPage.Body.String())
	}

	// A retried POST with the same identity returns the recorded result and
	// never mints a second branch.
	retry := h.request(http.MethodPost, "/api/sessions/"+source+"/message-action", body)
	if retry.Code != http.StatusOK || retry.Body.String() != fork.Body.String() {
		t.Fatalf("retry status=%d body=%s (first %s)", retry.Code, retry.Body.String(), fork.Body.String())
	}
	if count := h.createCount(t); count != 2 {
		t.Fatalf("retry created %d sessions, want 2", count)
	}
	// The same operation id with different content is rejected.
	conflict := h.request(http.MethodPost, "/api/sessions/"+source+"/message-action",
		strings.Replace(body, jsonText(stringField(row, "cursor")), `"epoch.k-1"`, 1))
	if conflict.Code != http.StatusBadRequest ||
		!strings.Contains(conflict.Body.String(), "同一操作标识不能用于不同内容") {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}
}

func TestACPMessageActionEditForkCarriesEditedQuestion(t *testing.T) {
	h := newACPForkHarness(t)
	source := h.createAndTalk(t, "编辑分支", "问甲", "问乙", "问丙")

	// edit-fork targets the middle turn's user row (editable, not latest).
	rows, _ := h.timelineRows(t, source)
	userRows := []*jsoncompat.Object{}
	for _, row := range rows {
		if userRowWithTurn(stringField(row, "turnId"))(row) {
			userRows = append(userRows, row)
		}
	}
	if len(userRows) != 3 {
		t.Fatalf("user rows=%d", len(userRows))
	}
	edited := "问乙（改后的问题）"
	body := forkBody(t, "edit-fork", h.nextUUID(), userRows[1], edited)
	editFork := h.request(http.MethodPost, "/api/sessions/"+source+"/message-action", body)
	if editFork.Code != http.StatusOK {
		t.Fatalf("edit-fork status=%d body=%s", editFork.Code, editFork.Body.String())
	}
	value, err := jsoncompat.Unmarshal([]byte(editFork.Body.String()))
	if err != nil {
		t.Fatal(err)
	}
	result := value.(*jsoncompat.Object)
	if status := stringField(result, "status"); status != "created" || result.MustGet("draft") != nil {
		t.Fatalf("edit-fork result=%s", editFork.Body.String())
	}
	if child := stringField(result, "id"); child != "fk-01" {
		t.Fatalf("edit-fork child=%q", child)
	}
	var prompts []string
	for deadline := time.Now().Add(2 * time.Second); ; {
		prompts = h.recordedPrompts(t)
		if len(prompts) >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(prompts) != 4 {
		t.Fatalf("prompt count=%d", len(prompts))
	}
	branch := prompts[3]
	// The transcript stops before the chosen turn; the edited text rides as
	// the branch's new user question.
	for _, needle := range []string{"用户：问甲", "助手：回答1", edited} {
		if !strings.Contains(branch, needle) {
			t.Fatalf("branch message missing %q: %s", needle, branch)
		}
	}
	for _, banned := range []string{"用户：问乙", "回答2", "问丙", "回答3"} {
		if strings.Contains(branch, banned) {
			t.Fatalf("branch message leaked %q: %s", banned, branch)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(branch), edited) {
		t.Fatalf("edited question is not the trailing message: %s", branch)
	}
	if count := h.createCount(t); count != 2 {
		t.Fatalf("agent created %d sessions, want 2", count)
	}
}

func TestACPMessageActionRejectsUnsupportedAndStaleRows(t *testing.T) {
	h := newACPForkHarness(t)
	source := h.createAndTalk(t, "校验", "问甲", "问乙")
	assistant := h.timelineRow(t, source, assistantRow)
	user := h.timelineRow(t, source, userRowWithTurn("turn-1"))

	post := func(payload string) *httptest.ResponseRecorder {
		return h.request(http.MethodPost, "/api/sessions/"+source+"/message-action", payload)
	}
	// In-place edit has no ACP counterpart; the error points at edit-fork.
	edit := post(forkBody(t, "edit", h.nextUUID(), user, "改"))
	if edit.Code != http.StatusBadRequest ||
		!strings.Contains(edit.Body.String(), "当前 Agent 不支持编辑并重新发送，请使用编辑并新建分支") {
		t.Fatalf("edit status=%d body=%s", edit.Code, edit.Body.String())
	}
	// fork needs an assistant row; edit-fork needs a user row.
	wrongFlag := post(forkBody(t, "fork", h.nextUUID(), user, ""))
	if wrongFlag.Code != http.StatusBadRequest || !strings.Contains(wrongFlag.Body.String(), "此消息不支持该操作") {
		t.Fatalf("fork-on-user status=%d body=%s", wrongFlag.Code, wrongFlag.Body.String())
	}
	wrongFlag = post(forkBody(t, "edit-fork", h.nextUUID(), assistant, "改"))
	if wrongFlag.Code != http.StatusBadRequest || !strings.Contains(wrongFlag.Body.String(), "此消息不支持该操作") {
		t.Fatalf("edit-fork-on-assistant status=%d body=%s", wrongFlag.Code, wrongFlag.Body.String())
	}
	// A stale version is refused.
	stale := `{"action":"fork","id":"` + h.nextUUID() + `","key":` + jsonText(stringField(assistant, "cursor")) +
		`,"version":"stale","uiLocale":"zh-CN"}`
	response := post(stale)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "消息内容已变化，请重新打开编辑") {
		t.Fatalf("stale status=%d body=%s", response.Code, response.Body.String())
	}
	// Unknown actions and malformed ids never reach the agent.
	response = post(`{"action":"nope","id":"` + h.nextUUID() + `","key":"x","version":"y"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "消息操作无效") {
		t.Fatalf("invalid action status=%d body=%s", response.Code, response.Body.String())
	}
	response = post(`{"action":"fork","id":"not-a-uuid","key":"x","version":"y"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "消息操作无效") {
		t.Fatalf("bad id status=%d body=%s", response.Code, response.Body.String())
	}
	// edit-fork without text is rejected like the codex branch.
	response = post(forkBody(t, "edit-fork", h.nextUUID(), user, "  "))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "请输入 1–100000 字的消息") {
		t.Fatalf("empty text status=%d body=%s", response.Code, response.Body.String())
	}
	if count := h.createCount(t); count != 1 {
		t.Fatalf("rejected actions created %d sessions, want 1", count)
	}
}
