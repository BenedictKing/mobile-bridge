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
	"unicode/utf8"

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
	createdSessions := filepath.Join(dir, "sessions.jsonl")
	script := `import json,sys
CREATES = "` + creates + `"
PROMPTS = "` + prompts + `"
CREATED_SESSIONS = "` + createdSessions + `"
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
def created_sessions():
 rows = []
 try:
  rows = [json.loads(line) for line in open(CREATED_SESSIONS) if line.strip()]
 except OSError:
  pass
 return rows
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":SESSIONS+created_sessions()}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new":
  params=m.get("params",{}); session_id="%s-%d" % (PREFIX, bump(CREATES))
  with open(CREATED_SESSIONS,"a") as f: f.write(json.dumps({"sessionId":session_id,"title":params.get("title",""),"cwd":params.get("cwd",""),"updatedAt":1})+"\n")
  result={"sessionId":session_id,"modes":{"currentModeId":"build"},"configOptions":[]}
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
	server.Creations = mustLoadCreations(t, filepath.Join(t.TempDir(), "creations.json"))

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

	tooLong := request(http.MethodPost, "/api/sessions/source/migrate",
		`{"agent":"kimi","scope":"main","id":"0aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","instructions":"`+strings.Repeat("指", 8001)+`"}`)
	if tooLong.Code != http.StatusBadRequest || !strings.Contains(tooLong.Body.String(), "后续指示最多 8000 字") {
		t.Fatalf("long instructions status=%d body=%s", tooLong.Code, tooLong.Body.String())
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

	// prepare creates a target session in the source cwd. That session then makes
	// the cwd appear in session/list, changing its displayed project key; confirm
	// must still claim the same persisted migration rather than report a false
	// content mismatch.
	prepare := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"kimi","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN","phase":"prepare"}`)
	if prepare.Code != http.StatusOK || !strings.Contains(prepare.Body.String(), `"status": "prepared"`) ||
		!strings.Contains(prepare.Body.String(), `"projectCreated": true`) {
		t.Fatalf("migrate prepare status=%d body=%s", prepare.Code, prepare.Body.String())
	}
	first := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"kimi","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN","instructions":"继续完成剩余测试"}`)
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
		!strings.Contains(migrated.Body.String(), "从 ZCode 迁移的会话") ||
		!strings.Contains(migrated.Body.String(), "【用户后续指示】") ||
		!strings.Contains(migrated.Body.String(), "继续完成剩余测试") {
		t.Fatalf("migrated view missing transcript or instructions: %s", migrated.Body.String())
	}

	// A retried request replays the recorded outcome: no second create, no
	// second prompt.
	retry := request(http.MethodPost, "/api/sessions/zcode-0/migrate",
		`{"agent":"kimi","scope":"main","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN","instructions":"继续完成剩余测试"}`)
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
		turn(message("activity", "搜索参数与结果", [2]string{"kind", "toolCall"}, [2]string{"title", "WebSearch"})),
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
	// Routine read and generic tool activity drop from the main scope; the
	// multi-line command activity collapses to its first line with the title prefix.
	if roles != "user,assistant,activity,error,user,assistant," {
		t.Fatalf("main roles=%s", roles)
	}
	if !strings.Contains(texts, "执行命令：重要操作的第一行") || !strings.Contains(texts, "出错了") {
		t.Fatalf("main texts=%s", texts)
	}
	if strings.Contains(texts, "WebSearch") || strings.Contains(texts, "搜索参数与结果") {
		t.Fatalf("main 不应保留普通工具调用: %s", texts)
	}

	latest, _ := migrationTranscript(view, "latest")
	if len(latest) != 2 || latest[0].text != "第二个问题" || latest[1].text != "第二个回答" {
		t.Fatalf("latest entries=%+v", latest)
	}

	full, _ := migrationTranscript(view, "full")
	if len(full) != 8 {
		t.Fatalf("full entries=%+v", full)
	}
}

func TestMigrationTranscriptKeepsLongAssistantSummary(t *testing.T) {
	for _, scope := range []string{"main", "full", "latest"} {
		t.Run(scope, func(t *testing.T) {
			view := transcriptView()
			turns, _ := view.MustGet("turns").(*jsoncompat.Array)
			turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
			messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
			assistant, _ := messages.Items[len(messages.Items)-1].(*jsoncompat.Object)
			summary := "最终总结：" + strings.Repeat("关键结论与完整上下文。", 2500)
			assistant.Set("text", summary)

			entries, _ := migrationTranscript(view, scope)
			if len(entries) == 0 || entries[len(entries)-1].text != summary {
				t.Fatalf("%s 应完整保留助手长总结，实际末尾长度=%d", scope, len(entries[len(entries)-1].text))
			}
		})
	}
}

func TestMigrationTranscriptTailBudget(t *testing.T) {
	view := transcriptView()
	original := migrationBudgets["main"]
	migrationBudgets["main"] = [3]int{2, 100000, 6000}
	defer func() { migrationBudgets["main"] = original }()
	entries, truncated := migrationTranscript(view, "main")
	if len(entries) != 2 || !truncated || entries[0].text != "第一个问题" || entries[1].text != "第二个回答" {
		t.Fatalf("budgeted entries=%+v truncated=%v", entries, truncated)
	}
}

func TestMigrationMainPreservesTaskAndThoughtAroundImageAttachment(t *testing.T) {
	view := jsoncompat.NewObject()
	view.Set("title", "长对话")
	turn := jsoncompat.NewObject()
	messages := &jsoncompat.Array{}
	add := func(role, text string, fields ...[2]string) {
		message := jsoncompat.NewObject()
		message.Set("role", role)
		message.Set("text", text)
		for _, field := range fields {
			message.Set(field[0], field[1])
		}
		messages.Items = append(messages.Items, message)
	}
	add("user", "/effort xhigh")
	add("user", "请修复团队画布连线并设计旧 Issue 删除功能")
	image := "iVBORw0KGgo" + strings.Repeat("A", 476300)
	add("user", image)
	thought := "用户要修复画布布局，并设计旧 Issue 删除。\n关键约束：先检查现有数据关系，再实现并验证删除语义。"
	add("activity", thought, [2]string{"title", "思考摘要"}, [2]string{"kind", "agentThought"})
	for index := 0; index < 45; index++ {
		add("activity", fmt.Sprintf("go test ./internal/team/... 第 %d 轮", index), [2]string{"title", "执行命令"})
	}
	add("user", "继续完成最后验证")
	add("assistant", "最终结论：验证通过，变更已完成。")
	turn.Set("messages", messages)
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})

	entries, truncated := migrationTranscript(view, "main")
	joined := ""
	for _, entry := range entries {
		joined += entry.text + "\n"
	}
	if !truncated || !strings.Contains(joined, "请修复团队画布连线并设计旧 Issue 删除功能") ||
		!strings.Contains(joined, "关键约束：先检查现有数据关系") ||
		!strings.Contains(joined, "继续完成最后验证") || !strings.Contains(joined, "验证通过") {
		t.Fatalf("main transcript lost task, thought, or recent exchange: %s", joined)
	}
	if strings.Contains(joined, "iVBORw0KGgo") || !strings.Contains(joined, "图片附件未迁移") {
		t.Fatalf("image payload should be replaced by a marker: %q", joined)
	}
	if len(entries) > migrationBudgets["main"][0] {
		t.Fatalf("main transcript exceeds message budget: %d", len(entries))
	}
}

func TestMigrationLatestKeepsQuestionBeforeEncodedImage(t *testing.T) {
	view := transcriptView()
	turns := view.MustGet("turns").(*jsoncompat.Array)
	last := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	messages := last.MustGet("messages").(*jsoncompat.Array)
	image := jsoncompat.NewObject()
	image.Set("role", "user")
	image.Set("text", "iVBORw0KGgo"+strings.Repeat("A", 5000))
	messages.Items = append(messages.Items[:1], append([]any{image}, messages.Items[1:]...)...)
	entries, _ := migrationTranscript(view, "latest")
	if len(entries) != 3 || entries[0].text != "第二个问题" || entries[2].text != "第二个回答" ||
		!strings.Contains(entries[1].text, "图片附件未迁移") {
		t.Fatalf("latest must retain the question preceding its image: %+v", entries)
	}
}

func TestMigrationLatestSkipsClaudeLocalCommandRecords(t *testing.T) {
	view := transcriptView()
	turns := view.MustGet("turns").(*jsoncompat.Array)
	last := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	messages := last.MustGet("messages").(*jsoncompat.Array)
	for _, text := range []string{"<command-name>/model</command-name>", "<local-command-stdout>Set model to Opus</local-command-stdout>"} {
		message := jsoncompat.NewObject()
		message.Set("role", "user")
		message.Set("text", text)
		messages.Items = append(messages.Items, message)
	}
	for _, scope := range []string{"main", "full", "latest"} {
		entries, _ := migrationTranscript(view, scope)
		if len(entries) < 2 || entries[len(entries)-2].text != "第二个问题" || entries[len(entries)-1].text != "第二个回答" {
			t.Fatalf("%s should end at the latest model exchange, not local CLI records: %+v", scope, entries)
		}
	}
}

func TestMigrationKeepsLatestExchangeWithManyThoughts(t *testing.T) {
	for _, scope := range []string{"main", "full"} {
		t.Run(scope, func(t *testing.T) {
			view := transcriptView()
			turns := view.MustGet("turns").(*jsoncompat.Array)
			turn := turns.Items[0].(*jsoncompat.Object)
			messages := turn.MustGet("messages").(*jsoncompat.Array)
			for index := 0; index < 80; index++ {
				thought := jsoncompat.NewObject()
				thought.Set("role", "activity")
				thought.Set("kind", "agentThought")
				thought.Set("text", strings.Repeat("分析细节与约束。", 200)+fmt.Sprint(index))
				messages.Items = append(messages.Items, thought)
			}
			entries, truncated := migrationTranscript(view, scope)
			joined := ""
			for _, entry := range entries {
				joined += entry.text + "\n"
			}
			if !truncated || !strings.Contains(joined, "第一个问题") || !strings.Contains(joined, "第二个问题") ||
				!strings.Contains(joined, "第二个回答") || !strings.Contains(joined, "分析细节与约束") {
				t.Fatalf("%s lost dialogue to reasoning: %s", scope, joined)
			}
		})
	}
}

func TestMigrationMainFiltersNativeInspectionWithoutDroppingBuild(t *testing.T) {
	for _, test := range []struct {
		command string
		noise   bool
	}{
		{"sed -n '1,80p' internal/team/reducer.go", true},
		{"go test ./internal/team/...", false},
		{"sed -n '1,80p' file; go test ./...", false},
		{"cat source > destination", false},
	} {
		message := jsoncompat.NewObject()
		message.Set("kind", "subagent")
		message.Set("title", "读取事件重放 reducer")
		message.Set("text", test.command)
		if got := isMigrationNoise(message); got != test.noise {
			t.Fatalf("command %q noise=%v, want %v", test.command, got, test.noise)
		}
	}
}

func TestMigrationMessageKeepsUnicodeAndInstructionsWithinWireLimit(t *testing.T) {
	for _, scope := range []string{"main", "full", "latest"} {
		t.Run(scope, func(t *testing.T) {
			view := transcriptView()
			turns := view.MustGet("turns").(*jsoncompat.Array)
			turn := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
			messages := turn.MustGet("messages").(*jsoncompat.Array)
			messages.Items[0].(*jsoncompat.Object).Set("text", "LATEST_QUESTION："+strings.Repeat("原始任务约束。", 7000))
			messages.Items[1].(*jsoncompat.Object).Set("text", "FINAL_ANSWER："+strings.Repeat("分析结论。", 10000)+"END_OF_RESULT")
			instructions := strings.Repeat("后续指示。", 1500)
			text := migrationMessage("Claude", view, "zh-CN", scope, instructions)
			if len(text) > 100000 || !utf8.ValidString(text) || !strings.HasSuffix(text, instructions) {
				t.Fatalf("%s invalid wire message: bytes=%d validUTF8=%v", scope, len(text), utf8.ValidString(text))
			}
			for _, needle := range []string{"LATEST_QUESTION", "FINAL_ANSWER", "END_OF_RESULT", "内容已截断"} {
				if !strings.Contains(text, needle) {
					t.Fatalf("%s message lost %q", scope, needle)
				}
			}
		})
	}
}

func TestMigrationMessageLocales(t *testing.T) {
	view := transcriptView()
	zh := migrationMessage("ZCode", view, "zh-CN", "main", "")
	if !strings.HasPrefix(zh, "【从 ZCode 迁移的会话】\n标题：标题\n目录：/tmp/project") ||
		!strings.Contains(zh, "用户：第一个问题") || !strings.Contains(zh, "助手：第一个回答") ||
		!strings.HasSuffix(zh, "以上是从 ZCode 迁移过来的会话转录，请在此基础上继续。") {
		t.Fatalf("zh message=%q", zh)
	}
	zh = migrationMessage("ZCode", view, "zh-CN", "main", "  继续补完测试  ")
	if !strings.HasSuffix(zh, "【用户后续指示】\n继续补完测试") {
		t.Fatalf("zh instructions=%q", zh)
	}
	en := migrationMessage("ZCode", view, "en_US", "latest", "")
	if !strings.HasPrefix(en, "[Latest exchange from ZCode]\nTitle: 标题\nDirectory: /tmp/project") ||
		!strings.Contains(en, "User: 第二个问题") ||
		!strings.HasSuffix(en, "The exchange above is the latest question and answer from ZCode. Please continue from this context.") {
		t.Fatalf("en message=%q", en)
	}
	en = migrationMessage("ZCode", view, "en_US", "latest", "Run the remaining tests")
	if !strings.HasSuffix(en, "[User instructions for what to do next]\nRun the remaining tests") {
		t.Fatalf("en instructions=%q", en)
	}
}
