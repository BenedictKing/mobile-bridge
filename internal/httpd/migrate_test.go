package httpd

import (
	"fmt"
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

// migrationFakeAgent is a stateless python ACP agent with per-file counters:
// session/new mints "<prefix>-<n>" ids, session/prompt bumps its own counter
// so tests can assert idempotent retries never re-create or re-send.
func migrationFakeAgent(t *testing.T, sessions, prefix string) (argv []string, creates, prompts string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	creates, prompts = filepath.Join(dir, "creates.txt"), filepath.Join(dir, "prompts.txt")
	script := `import json,sys
CREATES = "` + creates + `"
PROMPTS = "` + prompts + `"
SESSIONS = ` + sessions + `
PREFIX = "` + prefix + `"
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
 elif method == "session/list": result={"sessions":SESSIONS}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new": result={"sessionId":"%s-%d" % (PREFIX, bump(CREATES)),"modes":{"currentModeId":"build"},"configOptions":[]}
 elif method == "session/prompt": bump(PROMPTS); result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}, creates, prompts
}

func counterValue(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &value); err != nil {
		t.Fatalf("bad counter %s: %q", path, raw)
	}
	return value
}

func TestMigrateAcrossAgents(t *testing.T) {
	cwd := t.TempDir()
	zcodeArgv, zcodeCreates, zcodePrompts := migrationFakeAgent(t,
		`[{"sessionId":"sess-old","title":"旧会话","cwd":`+jsonText(cwd)+`,"updatedAt":1}]`, "zcode")
	kimiArgv, kimiCreates, kimiPrompts := migrationFakeAgent(t, `[]`, "kimi")
	zcode := acpruntime.NewCommand(zcodeArgv)
	zcode.Label = "ZCode"
	kimi := acpruntime.NewCommand(kimiArgv)
	kimi.Label = "Kimi"
	defer zcode.Close()
	defer kimi.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": zcode, "kimi": kimi}
	server.Creations = LoadCreations(filepath.Join(t.TempDir(), "creations.json"))

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		reader := strings.NewReader(body)
		httptestRequest := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, reader)
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

	// A source chat on zcode with one user turn.
	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"源聊天","id":"1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), `"zcode-0"`) {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	if send := request(http.MethodPost, "/api/sessions/zcode-0/send", `{"text":"你好世界"}`); send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	// Regression for the missing migrate entry: live ACP timelines must keep
	// the agent roster in meta so the control has its target list.
	timeline := request(http.MethodGet, "/api/sessions/zcode-0/timeline?limit=20", "")
	if timeline.Code != http.StatusOK {
		t.Fatalf("timeline status=%d body=%s", timeline.Code, timeline.Body.String())
	}
	for _, needle := range []string{`"agents"`, `"kind": "zcode"`, `"kind": "kimi"`, `"kind": "codex"`, `"agent": "zcode"`} {
		if !strings.Contains(timeline.Body.String(), needle) {
			t.Fatalf("timeline meta missing %s: %s", needle, timeline.Body.String())
		}
	}

	// Migrating to kimi: no listed kimi project covers the cwd, so the
	// directory itself becomes the project (projectCreated) and the
	// transcript lands as the first prompt.
	first := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"kimi","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN"}`)
	if first.Code != http.StatusOK {
		t.Fatalf("migrate status=%d body=%s", first.Code, first.Body.String())
	}
	body := first.Body.String()
	for _, needle := range []string{`"kimi-0"`, `"agent": "kimi"`, `"status": "sent"`, `"sent": true`,
		`"projectCreated": true`, `"project": "kimi:`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("migrate body missing %s: %s", needle, body)
		}
	}
	time.Sleep(150 * time.Millisecond)
	migrated := request(http.MethodGet, "/api/sessions/kimi-0", "")
	if !strings.Contains(migrated.Body.String(), "你好世界") ||
		!strings.Contains(migrated.Body.String(), "从 ZCode 迁移的会话") {
		t.Fatalf("migrated view missing transcript: %s", migrated.Body.String())
	}

	// A retried request replays the recorded outcome: no second create, no
	// second prompt.
	retry := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"kimi","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN"}`)
	if retry.Code != http.StatusOK || !strings.Contains(retry.Body.String(), `"kimi-0"`) ||
		!strings.Contains(retry.Body.String(), `"sent": true`) {
		t.Fatalf("migrate retry status=%d body=%s", retry.Code, retry.Body.String())
	}
	if counterValue(t, kimiCreates) != 1 || counterValue(t, kimiPrompts) != 1 {
		t.Fatalf("retry duplicated work: creates=%d prompts=%d",
			counterValue(t, kimiCreates), counterValue(t, kimiPrompts))
	}

	// The same request id cannot migrate different content.
	conflict := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"zcode","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if conflict.Code != http.StatusBadRequest ||
		!strings.Contains(conflict.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("conflict status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// Migrating to zcode itself: the listed session's cwd matches, so the
	// shared project key is reused (no auto-create).
	self := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"zcode","scope":"latest","id":"3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN"}`)
	if self.Code != http.StatusOK ||
		!strings.Contains(self.Body.String(), `"projectCreated": false`) ||
		!strings.Contains(self.Body.String(), `"agent": "zcode"`) {
		t.Fatalf("self migrate status=%d body=%s", self.Code, self.Body.String())
	}

	for _, bad := range []struct {
		body   string
		status int
		needle string
	}{
		{`{"agent":"nope","id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`, 400, "未知的 Agent 类型"},
		{`{"agent":"kimi","scope":"wat","id":"5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`, 400, "未知的迁移范围"},
		{`{"agent":"kimi","id":"not-a-uuid"}`, 400, "迁移请求标识无效"},
		{`{"agent":"codex","id":"6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`, 400, "未知的 Agent 类型"},
	} {
		response := request(http.MethodPost, "/api/sessions/zcode-0/migrate", bad.body)
		if response.Code != bad.status || !strings.Contains(response.Body.String(), bad.needle) {
			t.Fatalf("negative case %s: status=%d body=%s", bad.body, response.Code, response.Body.String())
		}
	}

	// The source zcode chat saw exactly one create (the source) plus one
	// self-migrate create, and two prompts (the seed send + self migrate).
	if creates, prompts := counterValue(t, zcodeCreates), counterValue(t, zcodePrompts); creates != 2 || prompts != 2 {
		t.Fatalf("zcode counters creates=%d prompts=%d", creates, prompts)
	}
}

func transcriptView() *jsoncompat.Object {
	message := func(role, text string, extra ...[2]string) *jsoncompat.Object {
		item := jsoncompat.NewObject()
		item.Set("role", role)
		item.Set("text", text)
		for _, pair := range extra {
			item.Set(pair[0], pair[1])
		}
		return item
	}
	turn := func(messages ...*jsoncompat.Object) *jsoncompat.Object {
		turn := jsoncompat.NewObject()
		array := &jsoncompat.Array{}
		for _, item := range messages {
			array.Items = append(array.Items, item)
		}
		turn.Set("messages", array)
		return turn
	}
	view := jsoncompat.NewObject()
	view.Set("title", "标题")
	view.Set("cwd", "/tmp/project")
	turns := &jsoncompat.Array{}
	turns.Items = append(turns.Items,
		turn(message("user", "第一个问题"), message("assistant", "第一个回答")),
		turn(message("activity", "详细的活动输出文本", [2]string{"title", "read"})),
		turn(message("activity", "重要操作的第一行\n更多细节", [2]string{"title", "执行命令"})),
		turn(message("error", "出错了")),
		turn(message("user", "第二个问题"), message("assistant", "第二个回答")))
	view.Set("turns", turns)
	return view
}

func TestMigrationTranscriptScopes(t *testing.T) {
	view := transcriptView()
	mainEntries, truncated := migrationTranscript(view, "main")
	if truncated {
		t.Fatalf("main unexpectedly truncated")
	}
	roles, texts := "", ""
	for _, entry := range mainEntries {
		roles += entry.role + ","
		texts += entry.text + "|"
	}
	// Routine read activity drops from the main scope; the multi-line
	// command activity collapses to its first line with the title prefix.
	if roles != "user,assistant,activity,error,user,assistant," {
		t.Fatalf("main roles=%s", roles)
	}
	if !strings.Contains(texts, "执行命令：重要操作的第一行") || !strings.Contains(texts, "出错了") {
		t.Fatalf("main texts=%s", texts)
	}

	latest, _ := migrationTranscript(view, "latest")
	if len(latest) != 2 || latest[0].text != "第二个问题" || latest[1].text != "第二个回答" {
		t.Fatalf("latest entries=%+v", latest)
	}

	full, _ := migrationTranscript(view, "full")
	if len(full) != 7 {
		t.Fatalf("full entries=%+v", full)
	}
}

func TestMigrationTranscriptTailBudget(t *testing.T) {
	view := transcriptView()
	original := migrationBudgets["main"]
	migrationBudgets["main"] = [3]int{2, 100000, 6000}
	defer func() { migrationBudgets["main"] = original }()
	entries, truncated := migrationTranscript(view, "main")
	if len(entries) != 2 || !truncated || entries[0].text != "第二个问题" {
		t.Fatalf("budgeted entries=%+v truncated=%v", entries, truncated)
	}
}

func TestMigrationMessageLocales(t *testing.T) {
	view := transcriptView()
	zh := migrationMessage("ZCode", view, "zh-CN", "main")
	if !strings.HasPrefix(zh, "【从 ZCode 迁移的会话】\n标题：标题\n目录：/tmp/project") ||
		!strings.Contains(zh, "用户：第一个问题") || !strings.Contains(zh, "助手：第一个回答") ||
		!strings.HasSuffix(zh, "以上是从 ZCode 迁移过来的会话转录，请在此基础上继续。") {
		t.Fatalf("zh message=%q", zh)
	}
	en := migrationMessage("ZCode", view, "en_US", "latest")
	if !strings.HasPrefix(en, "[Latest exchange from ZCode]\nTitle: 标题\nDirectory: /tmp/project") ||
		!strings.Contains(en, "User: 第二个问题") ||
		!strings.HasSuffix(en, "The exchange above is the latest question and answer from ZCode. Please continue from this context.") {
		t.Fatalf("en message=%q", en)
	}
}
