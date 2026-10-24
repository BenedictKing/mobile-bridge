package httpd

// teams_integration_test.go 是 §12 阶段一验收级集成测试（docs/
// team-orchestration-plan.md §12 验收清单、§13.2 条目 1/4/5/6/8/9/12）：全部
// 走生产链路——team.Service 经 teams_adapter 的真实 QueueAdapter /
// SessionResolver 对接 submissions 账本与 acpruntime.Manager，伪 Agent 是
// python3 子进程（queueAgent 模板的多会话扩展 teamAgent）。排队 submission 的
// 自动派发在生产只发生在 StartQueueWorker 的 3s ticker 与 settle 钩子；测试不
// 起 ticker（其 goroutine 永不退出，会与 t.TempDir 清理竞争写盘），而用双驱动：
// ① 照 main.go:866-881 挂 manager.AutoContinue → QueueSettleACP；② 轮询循环体内
// 同步调 server.dispatchQueuedPass()（queue.go:599-603，即 ticker 循环体），
// 两者都经 queueMu 串行化。等待断言全部用 deadline 轮询（120ms 节拍、8s 超时，
// §13.1 不用固定 sleep 判正确性）；watcher 等待片经 team.Service.SetWaitSlice
// 缩到毫秒级（runner.go 的导出测试钩子）。缺 python3 时沿用 t.Skip 惯例。

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// teamAgent 是 queueAgent 的多会话扩展：session/new 每次发递增 sessionId
// （km-team-N，计数落盘跨进程共享——每个会话是独立子进程）；session/list 从
// sessions.txt 读回（供“重启后”的新 Manager 冷探测归属与 cwd）；session/load
// 回显请求的 sessionId（broken-sess 除外，报错模拟会话打不开→派发 requeue 爬
// 梯）。session/prompt 把 sessionId+NUL+text 追加进 prompts.log（换行转义成
// \n 保住一行一记录），随后：
//   - text 含 FAIL → 回 JSON-RPC error（awaitPrompt→FailTurn，turn.status=
//     failed 且 messages 附 kind==error 条目，manager.go:810-811）模拟任务失败；
//   - text 含 HOLDLONG → 后台线程 hold 20s，session/cancel 立即以
//     stopReason=cancelled 收尾（运行中取消窗口）；
//   - text 含 hold → sleep 1.5s（并发/写锁互斥窗口）；
//   - 其余立即 end_turn。完成前先发一条 assistant 文本块“已完成：<全文>”
//     （ExtractResult 据此拿到有界结果）。
func teamAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys,threading,time
DIR = "` + dir + `"
LOCK = threading.Lock()
STATE = {"event": threading.Event(), "cancelled": False}
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
def next_session():
 path = os.path.join(DIR, "counter.txt")
 try:
  n = int(open(path).read().strip() or "0")
 except Exception:
  n = 0
 n += 1
 open(path, "w").write(str(n))
 return "km-team-%d" % n
def known_sessions():
 rows = []
 try:
  with open(os.path.join(DIR, "sessions.txt")) as fh:
   for line in fh:
    line = line.rstrip("\n")
    if not line:
     continue
    sid, _, cwd = line.partition("\x00")
    rows.append({"sessionId": sid, "cwd": cwd, "title": sid})
 except FileNotFoundError:
  pass
 return rows
def handle_prompt(m, ident):
 blocks = (m.get("params") or {}).get("prompt") or []
 text = next((b.get("text", "") for b in blocks if isinstance(b, dict)), "")
 sid = (m.get("params") or {}).get("sessionId") or "?"
 record("prompts.log", sid + "\x00" + text.replace("\n", "\\n"))
 if "FAIL" in text:
  print(json.dumps({"jsonrpc": "2.0", "id": ident, "error": {"code": -32000, "message": "任务执行失败：FAIL 标记"}}), flush = True)
  return
 print(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "已完成：" + text}}}}), flush = True)
 stop = "end_turn"
 if "HOLDLONG" in text:
  with LOCK:
   STATE["event"].clear(); STATE["cancelled"] = False
  STATE["event"].wait(20)
  stop = "cancelled" if STATE["cancelled"] else "end_turn"
 elif "hold" in text:
  time.sleep(1.5)
 print(json.dumps({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": stop}}), flush = True)
for line in sys.stdin:
 m = json.loads(line); method = m.get("method"); ident = m.get("id")
 if method == "initialize":
  result = {"protocolVersion": 1, "agentCapabilities": {}, "agentInfo": {"name": "fake"}, "authMethods": []}
 elif method == "session/new":
  sid = next_session()
  cwd = (m.get("params") or {}).get("cwd") or "."
  record("sessions.txt", sid + "\x00" + cwd)
  result = {"sessionId": sid, "modes": {"currentModeId": "default", "availableModes": [{"id": "default"}]}, "configOptions": []}
 elif method == "session/load":
  sid = (m.get("params") or {}).get("sessionId") or ""
  if sid == "broken-sess":
   print(json.dumps({"jsonrpc": "2.0", "id": ident, "error": {"code": -32001, "message": "会话无法恢复：broken-sess"}}), flush = True)
   continue
  cwd = (m.get("params") or {}).get("cwd") or "."
  record("sessions.txt", sid + "\x00" + cwd)
  result = {"sessionId": sid, "modes": {"currentModeId": "default", "availableModes": [{"id": "default"}]}, "configOptions": []}
 elif method == "session/list":
  result = {"sessions": known_sessions()}
 elif method == "session/prompt":
  threading.Thread(target = handle_prompt, args = (m, ident), daemon = True).start()
  continue
 elif method == "session/cancel":
  with LOCK:
   STATE["cancelled"] = True; STATE["event"].set()
  result = {}
 else:
  result = {}
 print(json.dumps({"jsonrpc": "2.0", "id": ident, "result": result}), flush = True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// teamIntegrationEnv 装配带真实 submissions 账本与 team 服务的测试网关
// （main.go:904-930 的同构装配：LoadStore/OpenJournal/LoadSubmissions 后直赋
// 字段；SetWaitSlice 必须在任何 Run 启动前调用）。
type teamIntegrationEnv struct {
	server   *Server
	store    team.TeamStore
	journal  *team.Journal
	manager  *acpruntime.Manager
	queueAdp team.QueueAdapter
	token    string
	csrf     string
	dataDir  string
}

// newTeamIntegrationEnv 在 dataDir 上装配网关；重启恢复用例对同一 dataDir 再次
// 调用即可复用账本（文件按既有内容加载，journal 以追加句柄打开）。
func newTeamIntegrationEnv(t *testing.T, dataDir string) *teamIntegrationEnv {
	t.Helper()
	store := team.LoadStore(filepath.Join(dataDir, "teams.json"), filepath.Join(dataDir, "team-runs.json"))
	journal, err := team.OpenJournal(filepath.Join(dataDir, "team-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8791, "instance")
	server.Queue = LoadSubmissions(filepath.Join(dataDir, "submissions.json"))
	manager := acpruntime.NewCommand(teamAgent(t, dataDir))
	manager.Label = "Kimi"
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	// 派发驱动①：照 main.go:866-881 挂 settle 钩子（turn 落定立即派发该会话
	// 的下一条排队消息）。Cleanup 先置 nil 防断言后的后台活动（queue_test.go:
	// 233 惯例），再关进程。
	manager.AutoContinue = func(session *acpruntime.Session) {
		server.QueueSettleACP("kimi", session)
	}
	t.Cleanup(func() { manager.AutoContinue = nil })
	t.Cleanup(func() { manager.Close() })
	t.Cleanup(func() { journal.Close() })
	queueAdapter, resolver := NewTeamAdapters(server)
	server.TeamStore = store
	server.TeamFeeds = NewTeamFeedRegistry(journal, store)
	// changes 长轮询的测试等待上限（生产默认 12s，见 teams.go）。
	server.TeamChangesWait = 20 * time.Millisecond
	server.Teams = team.NewService(store, journal, queueAdapter, resolver)
	server.Teams.SetWaitSlice(2 * time.Millisecond)
	server.Teams.OnEvent = server.TeamFeeds.Ingest
	return &teamIntegrationEnv{server: server, store: store, journal: journal,
		manager: manager, queueAdp: queueAdapter, token: token, csrf: csrf, dataDir: dataDir}
}

// reopenTeamIntegrationEnv 模拟网关重启：同一 dataDir 重新装配 + 投影 prime +
// RecoverAll（main.go:921-930 的启动顺序）。
func reopenTeamIntegrationEnv(t *testing.T, dataDir string) *teamIntegrationEnv {
	t.Helper()
	env := newTeamIntegrationEnv(t, dataDir)
	if err := env.server.TeamFeeds.PrimeFromJournal(); err != nil {
		t.Fatalf("PrimeFromJournal: %v", err)
	}
	if err := env.server.Teams.RecoverAll(); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}
	return env
}

func (e *teamIntegrationEnv) request(method, target, body string) *httptest.ResponseRecorder {
	return featureRequest(e.server, e.token, e.csrf, method, target, body)
}

// createMemberSession 经 manager.Create 拉起独立伪 Agent 进程并建立成员会话，
// 返回 Agent 侧原生 sessionId（km-team-N）。
func (e *teamIntegrationEnv) createMemberSession(t *testing.T, requestedID, title string) string {
	t.Helper()
	session, err := e.manager.Create(requestedID, e.dataDir, title, 30*time.Second)
	if err != nil {
		t.Fatalf("成员会话创建失败: %v", err)
	}
	return session.ID
}

// createTeamWithMembers 创建团队（leadId 缺省落在首个成员上，teams.go:177-179）。
func (e *teamIntegrationEnv) createTeamWithMembers(t *testing.T, membersJSON string) string {
	t.Helper()
	body := `{"name":"集成团队","members":` + membersJSON + `}`
	response := e.request(http.MethodPost, "/api/teams", body)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func (e *teamIntegrationEnv) createRun(t *testing.T, teamID, body string) string {
	t.Helper()
	response := e.request(http.MethodPost, "/api/teams/"+teamID+"/runs", body)
	if response.Code != http.StatusOK {
		t.Fatalf("创建 Run 失败: %d %s", response.Code, response.Body.String())
	}
	var run struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func (e *teamIntegrationEnv) runDetail(t *testing.T, teamID, runID string) map[string]any {
	t.Helper()
	response := e.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 Run 失败: %d %s", response.Code, response.Body.String())
	}
	var run map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	return run
}

// drive 是派发驱动②：每个节拍同步执行一次派发巡检（即 StartQueueWorker 的
// ticker 循环体），随后评估 check；check 为 true 或超时返回 false。节拍 120ms，
// requeue 爬梯 10 次（queue.go:158-171）约 1.2s 完成。
func (e *teamIntegrationEnv) drive(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		e.server.dispatchQueuedPass()
		if check() {
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	e.server.dispatchQueuedPass()
	return check()
}

// teamPromptRecord 是 prompts.log 的一行：sessionId\x00text（text 内换行已转义）。
type teamPromptRecord struct {
	session string
	text    string
}

func readTeamPrompts(t *testing.T, dir string) []teamPromptRecord {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "prompts.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var records []teamPromptRecord
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		session, text, _ := strings.Cut(line, "\x00")
		records = append(records, teamPromptRecord{session: session, text: text})
	}
	return records
}

// promptIndex 返回首个包含 marker 的记录下标（-1=不存在）。
func promptIndex(records []teamPromptRecord, marker string) int {
	for i, record := range records {
		if strings.Contains(record.text, marker) {
			return i
		}
	}
	return -1
}

// taskResultOf 取任务卡上的 result 投影（§6.5：sessionId/turnId/result/
// resultChars/truncated）。
func taskResultOf(t *testing.T, task map[string]any) map[string]any {
	t.Helper()
	result, ok := task["result"].(map[string]any)
	if !ok {
		t.Fatalf("任务 %v 缺少 result 投影: %v", task["id"], task)
	}
	return result
}

// ---- 用例 1：§12 验收全链路——双会话 pipeline 按依赖执行、每结果可查 ----

// TestTeamIntegrationPipelineAcceptance 建团队绑两个伪会话（lead=A、dev=B），
// design→implement→review 流水线：断言 prompts.log 严格按依赖顺序落盘、design
// 完成结果在 implement 运行期间即可见（§6.5 结果即时入账）、终态三任务全带
// result 引用、原会话 timeline 能看到同样内容（验收“每个结果可在 Team 和原会话
// 查看”）。
func TestTeamIntegrationPipelineAcceptance(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "设计会话")
	sessionB := env.createMemberSession(t, "dev-sess", "开发会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead","name":"负责人"},{"id":"dev","sessionId":"`+sessionB+
		`","role":"member","name":"开发"}]`)
	runID := env.createRun(t, teamID, `{"goal":"三阶段验收","mode":"pipeline","plan":[
		{"id":"design","memberId":"lead","prompt":"请给出 DESIGN 阶段的设计方案","writeLock":"none"},
		{"id":"implement","memberId":"dev","prompt":"请完成 IMPLEMENT hold 阶段的实现","dependsOn":["design"],"writeLock":"exclusive"},
		{"id":"review","memberId":"dev","prompt":"请审查 REVIEW 阶段的产出","dependsOn":["implement"],"writeLock":"none"}]}`)

	// 中间态：design 已完成且结果入账，而 implement 仍在途（hold 拉宽窗口）。
	sawDesignResultEarly := false
	if !env.drive(t, 8*time.Second, func() bool {
		detail := env.runDetail(t, teamID, runID)
		design := teamTaskOf(t, detail, "design")
		implement := teamTaskOf(t, detail, "implement")
		if design["status"] != "completed" || design["result"] == nil {
			return false
		}
		switch implement["status"] {
		case "dispatching", "running":
			result := taskResultOf(t, design)
			text, _ := result["result"].(string)
			if result["sessionId"] == sessionA && result["turnId"] != "" && strings.Contains(text, "DESIGN") {
				sawDesignResultEarly = true
			}
			return true
		case "completed":
			return true // 窗口错过也不误报，靠下方终态断言兜底
		}
		return false
	}) {
		t.Fatal("pipeline 未在时限内推进到 design 完成")
	}
	if !sawDesignResultEarly {
		t.Fatal("design 的结果应在 implement 运行期间即可见（§6.5 结果即时入账）")
	}

	// 终态：三任务全 completed，全带 result 引用。
	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	detail := env.runDetail(t, teamID, runID)
	for _, taskID := range []string{"design", "implement", "review"} {
		task := teamTaskOf(t, detail, taskID)
		if task["status"] != "completed" {
			t.Fatalf("任务 %s 应 completed: %v", taskID, task["status"])
		}
		result := taskResultOf(t, task)
		if result["turnId"] == "" {
			t.Fatalf("任务 %s 的 result 缺 turnId: %v", taskID, result)
		}
		if chars, _ := result["resultChars"].(float64); chars <= 0 {
			t.Fatalf("任务 %s 的 resultChars 应为正: %v", taskID, result)
		}
	}
	if result := taskResultOf(t, teamTaskOf(t, detail, "implement")); result["sessionId"] != sessionB {
		t.Fatalf("implement 结果应归属会话 B: %v", result["sessionId"])
	}

	// prompts.log 严格按 design→implement→review 顺序，且各落在本成员会话。
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 3 {
		t.Fatalf("应恰好派发 3 条任务消息: %d 条", len(records))
	}
	designAt, implementAt, reviewAt :=
		promptIndex(records, "DESIGN"), promptIndex(records, "IMPLEMENT"), promptIndex(records, "REVIEW")
	if designAt < 0 || implementAt < 0 || reviewAt < 0 {
		t.Fatalf("三阶段提示词都应到达伪 Agent: design=%d implement=%d review=%d", designAt, implementAt, reviewAt)
	}
	if !(designAt < implementAt && implementAt < reviewAt) {
		t.Fatalf("派发顺序应严格为 design→implement→review: %d %d %d", designAt, implementAt, reviewAt)
	}
	if records[designAt].session != sessionA || records[implementAt].session != sessionB ||
		records[reviewAt].session != sessionB {
		t.Fatalf("任务应派发到绑定成员的会话: A=%s B=%s 记录=%v", sessionA, sessionB, records)
	}
	// 依赖任务的提示词带 [Team context] 信封与上游结果（§7.1）。
	if !strings.Contains(records[implementAt].text, "[Team context]") ||
		!strings.Contains(records[implementAt].text, "[Assigned task]") ||
		!strings.Contains(records[implementAt].text, "DESIGN") {
		t.Fatalf("implement 应携带 design 的结果上下文: %q", records[implementAt].text)
	}

	// 验收“每个结果可在原会话查看”：成员会话 timeline 能看到任务往返内容。
	timelineA := env.request(http.MethodGet, "/api/sessions/"+sessionA+"/timeline?limit=50", "")
	if timelineA.Code != http.StatusOK || !strings.Contains(timelineA.Body.String(), "DESIGN") ||
		!strings.Contains(timelineA.Body.String(), "已完成：") {
		t.Fatalf("会话 A timeline 应含 design 内容与结果: %d %s", timelineA.Code, timelineA.Body.String())
	}
	timelineB := env.request(http.MethodGet, "/api/sessions/"+sessionB+"/timeline?limit=50", "")
	if timelineB.Code != http.StatusOK || !strings.Contains(timelineB.Body.String(), "IMPLEMENT") ||
		!strings.Contains(timelineB.Body.String(), "REVIEW") {
		t.Fatalf("会话 B timeline 应含 implement/review 内容: %d %s", timelineB.Code, timelineB.Body.String())
	}
}

// ---- 用例 2：§13.2 条目 12——Run 失败 salvage ----

// TestTeamIntegrationFailureSalvage：implement 提示词触发 FAIL（JSON-RPC error
// → turn failed）→ Run 归 failed；design 已完成结果（resultDetail）仍在 Run
// 详情可见（§6.5 salvage：不因后续失败删除已确认结果）；review 级联停止且
// 不再派发。
func TestTeamIntegrationFailureSalvage(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "设计会话")
	sessionB := env.createMemberSession(t, "dev-sess", "开发会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"dev","sessionId":"`+sessionB+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"失败挽救","mode":"pipeline","plan":[
		{"id":"design","memberId":"lead","prompt":"salvage 设计阶段 SALVAGE-DESIGN","writeLock":"none"},
		{"id":"implement","memberId":"dev","prompt":"执行 SALVAGE-IMPL 并注入 FAIL 触发失败","dependsOn":["design"],"writeLock":"exclusive"},
		{"id":"review","memberId":"dev","prompt":"审查 SALVAGE-REVIEW","dependsOn":["implement"],"writeLock":"none"}]}`)

	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "failed"
	}) {
		t.Fatal("Run 未在时限内失败")
	}
	detail := env.runDetail(t, teamID, runID)
	design := teamTaskOf(t, detail, "design")
	if design["status"] != "completed" || design["result"] == nil {
		t.Fatalf("失败的 Run 里 design 的已完成结果仍应可见（salvage）: %v", design)
	}
	if result := taskResultOf(t, design); !strings.Contains(result["result"].(string), "SALVAGE-DESIGN") {
		t.Fatalf("design 结果内容异常: %v", result)
	}
	implement := teamTaskOf(t, detail, "implement")
	if implement["status"] != "failed" {
		t.Fatalf("implement 应 failed: %v", implement["status"])
	}
	if text, _ := implement["error"].(string); !strings.Contains(text, "任务执行失败") {
		t.Fatalf("implement 失败原因应取自 turn 的 kind==error 文案: %q", text)
	}
	review := teamTaskOf(t, detail, "review")
	if review["status"] != "stopped" {
		t.Fatalf("依赖失败分支的 review 应级联 stopped: %v", review["status"])
	}
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 2 || promptIndex(records, "SALVAGE-REVIEW") >= 0 {
		t.Fatalf("失败后不应再派发 review: %v", records)
	}
}

// ---- 用例 3：§12 验收——运行中取消 ----

// TestTeamIntegrationCancelMidRun：implement 提示词含 HOLDLONG（turn 进行中，
// 20s 或 session/cancel 才收尾）→ POST cancel 后任务归 cancelled（绝不
// completed——turn interrupted/stopReason=cancelled 的误分类防线，view.go:
// 169-178）、Run 归 cancelled、伪 Agent 侧 turn 真被取消、重复 cancel 409、
// 之后账本不被重排队、无任何再派发。
func TestTeamIntegrationCancelMidRun(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "设计会话")
	sessionB := env.createMemberSession(t, "dev-sess", "开发会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"dev","sessionId":"`+sessionB+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"运行中取消","mode":"pipeline","plan":[
		{"id":"design","memberId":"lead","prompt":"设计 CANCEL-DESIGN","writeLock":"none"},
		{"id":"implement","memberId":"dev","prompt":"长跑 CANCEL-IMPL HOLDLONG 任务","dependsOn":["design"],"writeLock":"exclusive"}]}`)

	// 等 implement 真正进入 running（watcher 已匹配到进行中的 turn）。
	if !env.drive(t, 8*time.Second, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "implement")["status"] == "running"
	}) {
		t.Fatal("implement 未在时限内进入 running")
	}
	implement := teamTaskOf(t, env.runDetail(t, teamID, runID), "implement")
	submissionID, _ := implement["submissionId"].(string)
	if submissionID == "" {
		t.Fatalf("running 任务应带 submissionId: %v", implement)
	}

	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	cancelled := env.runDetail(t, teamID, runID)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("Run 应 cancelled: %v", cancelled["status"])
	}
	implement = teamTaskOf(t, cancelled, "implement")
	if implement["status"] != "cancelled" {
		t.Fatalf("运行中任务应 cancelled 而非 %v", implement["status"])
	}
	if design := teamTaskOf(t, cancelled, "design"); design["status"] != "completed" || design["result"] == nil {
		t.Fatalf("取消前已完成的 design 结果应保留（salvage）: %v", design)
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "运行已结束") {
		t.Fatalf("重复取消应 409: %d %s", response.Code, response.Body.String())
	}

	// 伪 Agent 侧 turn 被真正取消：Session.Cancel 立即 EndTurn("cancelled")
	// （manager.go:1062-1083），轮询快照直到最后一个 turn 落 interrupted。
	interrupted := env.drive(t, 8*time.Second, func() bool {
		session := env.manager.Get(sessionB)
		if session == nil {
			return false
		}
		turns, _ := session.Snapshot().MustGet("turns").(*jsoncompat.Array)
		if turns == nil || len(turns.Items) == 0 {
			return false
		}
		turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
		if turn == nil {
			return false
		}
		status, _ := turn.MustGet("status").(string)
		return status == "interrupted"
	})
	if !interrupted {
		t.Fatal("伪 Agent 侧 turn 应被取消为 interrupted")
	}

	// 取消后不再有任何派发：账本条目不被重排队（unknown 才会爬梯），提示词
	// 不再增长——用若干个巡检节拍确认稳定。
	baseline := len(readTeamPrompts(t, env.dataDir))
	env.drive(t, 2*time.Second, func() bool { return false })
	if got := len(readTeamPrompts(t, env.dataDir)); got != baseline {
		t.Fatalf("取消后不应再派发: 基线 %d 现在 %d", baseline, got)
	}
	entry := env.server.Queue.get(sessionB, submissionID)
	if entry == nil || entry.Status != SubAccepted || entry.QueueAttempts != 0 {
		t.Fatalf("已派发条目应保持 accepted 且不爬梯: %+v", entry)
	}
}

// ---- 用例 4：§13.2 条目 4——结果未知只对账不重发、waiting_input 与 resume ----

// TestTeamIntegrationUnknownOutcomeWaitingInputAndResume：构造在途任务账本
// entry=unknown 的真实中间态（dispatchQueuedThread 派发前先 setStatus(unknown)，
// queue.go:542；此处手动落同款状态：条目 unknown、会话空闲、无本任务 turn）→
// watcher 归任务 outcome_unknown、run 停 waiting_input、无第二次入账 → POST
// resume → 任务转 stopped（含“不再重发”）、run fold 为 stopped → 非
// waiting_input 的 resume 409。全程提示词零派发（对账绝不重发）。
func TestTeamIntegrationUnknownOutcomeWaitingInputAndResume(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "对账会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"结果未知对账","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"等待对账的任务 UNKNOWN-CHECK","writeLock":"none"}]}`)

	// 等 loopPass 把 t1 记账到 dispatching（submissionId 可见）——此阶段不做
	// 派发巡检，条目停在 queued。
	var submissionID string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && submissionID == "" {
		task := teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")
		if id, _ := task["submissionId"].(string); id != "" {
			submissionID = id
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	if submissionID == "" {
		t.Fatal("任务未在时限内完成派发记账")
	}
	entry := env.server.Queue.get(sessionA, submissionID)
	if entry == nil || entry.Status != SubQueued {
		t.Fatalf("入账条目应 queued: %+v", entry)
	}
	// 手动落“派发结果未知”状态（生产里 dispatchSubmission 返回 outcomeUnknown
	// 时即此形态，queue.go:551）。
	env.server.Queue.setStatus(sessionA, submissionID, SubUnknown)

	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("结果未知应把 Run 停进 waiting_input")
	}
	waiting := env.runDetail(t, teamID, runID)
	task := teamTaskOf(t, waiting, "t1")
	if task["status"] != "outcome_unknown" {
		t.Fatalf("任务应 outcome_unknown: %v", task["status"])
	}
	if records := readTeamPrompts(t, env.dataDir); len(records) != 0 {
		t.Fatalf("结果未知只对账不重发，提示词应为空: %v", records)
	}
	if entries := env.server.Queue.threadEntries(sessionA); len(entries) != 1 {
		t.Fatalf("不应有第二次入账: %+v", entries)
	}

	// resume：未决 unknown → stopped（不再重发），无可派发分支 → fold stopped。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	resumed := env.runDetail(t, teamID, runID)
	if resumed["status"] != "stopped" {
		t.Fatalf("resume 后 Run 应 stopped: %s", response.Body.String())
	}
	task = teamTaskOf(t, resumed, "t1")
	if task["status"] != "stopped" {
		t.Fatalf("未决 unknown 应转 stopped: %v", task["status"])
	}
	if reason, _ := task["stopReason"].(string); !strings.Contains(reason, "不再重发") {
		t.Fatalf("stopReason 应说明不再重发: %v", task["stopReason"])
	}
	if records := readTeamPrompts(t, env.dataDir); len(records) != 0 {
		t.Fatalf("resume 不得重发任务: %v", records)
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "等待用户处理") {
		t.Fatalf("非暂停态 resume 应 409: %d %s", response.Code, response.Body.String())
	}
}

// ---- 用例 5：§12 验收——网关重启后不重复发送任务 ----

// teamRestartState 是手工预置的重启前状态（确定性优先：直接写 teams.json /
// team-runs.json / team-journal.jsonl / submissions.json 的组合，模拟崩溃瞬间）。
type teamRestartState struct {
	teamID       string
	runID        string
	sessionID    string
	submissionID string
}

// writeTeamRestartState 落一组“任务已记账派发（journal 有 task_dispatching）、
// 快照 dispatching”的账本组合；ledgerStatus 空串表示 submissions.json 不写该
// 条目（journal 先记账、enqueue 未落盘的崩溃窗口）。
func writeTeamRestartState(t *testing.T, dataDir, ledgerStatus string) *teamRestartState {
	t.Helper()
	now := time.Now()
	state := &teamRestartState{teamID: "tteam-restart-01", runID: "trun-restart-01", sessionID: "km-team-1"}
	policy := team.DefaultPolicy()
	task := &team.Task{ID: "t1", MemberID: "lead", Prompt: "重启对账任务 RESTART-CHECK",
		Attempt: 1, Ordinal: 1, Status: team.TaskDispatching, WriteLock: team.TaskWriteLockNone}
	task.InputHash = team.InputHashFor(task, team.PolicyDigest(policy), "")
	state.submissionID = team.TaskSubmissionID(state.teamID, state.runID, "t1", 1)

	teamRow := &team.Team{ID: state.teamID, Name: "重启团队",
		Members: []team.TeamMember{{ID: "lead", SessionID: state.sessionID, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
		LeadID:  "lead", Policy: policy, CreatedAt: now, UpdatedAt: now}
	teamRaw, err := json.Marshal(map[string]*team.Team{state.teamID: teamRow})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), teamRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: state.runID, TeamID: state.teamID, Goal: "重启对账",
		Status: team.RunDispatching, Round: 1, PlanHash: "hand-hash",
		Tasks: []team.Task{*task}, CreatedAt: now, UpdatedAt: now}
	runRaw, err := json.Marshal(map[string]*team.Run{state.runID: run})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), runRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	events := []*team.Event{
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventRunCreated, Source: "user",
			Payload:  map[string]any{"mode": "pipeline", "planHash": "hand-hash", "taskCount": 1},
			Sequence: 1, CreatedAt: now},
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventTaskPlanned, Source: "user",
			Payload:  map[string]any{"taskId": "t1", "memberId": "lead"},
			Sequence: 2, CreatedAt: now},
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventTaskDispatching, Source: "runner",
			Payload:  map[string]any{"taskId": "t1", "sessionId": state.sessionID, "submissionId": state.submissionID, "inputHash": task.InputHash, "attempt": 1},
			Sequence: 3, CreatedAt: now},
	}
	var journal strings.Builder
	for _, event := range events {
		line, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		journal.Write(line)
		journal.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-journal.jsonl"), []byte(journal.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if ledgerStatus != "" {
		entry := &submissionEntry{ID: state.submissionID, ThreadID: state.sessionID, Host: "local",
			Text: task.Prompt, Mode: "queue", Status: ledgerStatus,
			CreatedAt: float64(time.Now().UnixMilli()) / 1000}
		ledgerRaw, marshalErr := json.Marshal(map[string]*submissionEntry{
			state.sessionID + ":" + state.submissionID: entry})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "submissions.json"), ledgerRaw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 伪 Agent 的会话登记：重启后的新 Manager 经冷 List 探测按此归属并取 cwd。
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
		[]byte(state.sessionID+"\x00"+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestTeamIntegrationRestartRecoveryNoResend：同 dataDir 重新装配（OpenStore +
// NewService + PrimeFromJournal + RecoverAll + 新 Server）后：
//   - (a) 账本 entry=unknown → 任务归 outcome_unknown、run 停 waiting_input、
//     账本保持 unknown、提示词零增长（恢复只对账，绝不重发）；
//   - (b) journal 已记账但账本无条目（崩溃窗口）→ provably 未发送 → 任务回
//     ready 并恰好重新派发一次。
func TestTeamIntegrationRestartRecoveryNoResend(t *testing.T) {
	t.Run("unknown 账本恢复只对账不重发", func(t *testing.T) {
		dataDir := t.TempDir()
		state := writeTeamRestartState(t, dataDir, SubUnknown)
		env := reopenTeamIntegrationEnv(t, dataDir)
		if !env.drive(t, 8*time.Second, func() bool {
			return env.runDetail(t, state.teamID, state.runID)["status"] == "waiting_input"
		}) {
			t.Fatal("恢复后 Run 应停 waiting_input")
		}
		detail := env.runDetail(t, state.teamID, state.runID)
		if task := teamTaskOf(t, detail, "t1"); task["status"] != "outcome_unknown" {
			t.Fatalf("任务应 outcome_unknown: %v", task["status"])
		}
		entry := env.server.Queue.get(state.sessionID, state.submissionID)
		if entry == nil || entry.Status != SubUnknown {
			t.Fatalf("账本条目应保持 unknown（只对账不重发）: %+v", entry)
		}
		if records := readTeamPrompts(t, dataDir); len(records) != 0 {
			t.Fatalf("恢复不得重发任务: %v", records)
		}
	})
	t.Run("崩溃窗口回 ready 且只重发一次", func(t *testing.T) {
		dataDir := t.TempDir()
		state := writeTeamRestartState(t, dataDir, "")
		env := reopenTeamIntegrationEnv(t, dataDir)
		if !env.drive(t, 8*time.Second, func() bool {
			return env.runDetail(t, state.teamID, state.runID)["status"] == "completed"
		}) {
			t.Fatal("回 ready 的任务应重新派发并完成")
		}
		records := readTeamPrompts(t, dataDir)
		if len(records) != 1 || records[0].session != state.sessionID ||
			!strings.Contains(records[0].text, "RESTART-CHECK") {
			t.Fatalf("应恰好重新派发一次: %v", records)
		}
		entry := env.server.Queue.get(state.sessionID, state.submissionID)
		if entry == nil || entry.Status != SubAccepted {
			t.Fatalf("重派条目应 accepted: %+v", entry)
		}
		detail := env.runDetail(t, state.teamID, state.runID)
		if task := teamTaskOf(t, detail, "t1"); task["status"] != "completed" || task["result"] == nil {
			t.Fatalf("任务应完成并带结果: %v", task)
		}
	})
}

// writeTeamExclusiveRestartState 预置「同 cwd 双 exclusive 写任务、其一在途」
// 的重启前账本组合（teams.json / team-runs.json / team-journal.jsonl /
// submissions.json / sessions.txt）：w1 已记账派发（dispatching、账本
// queued），w2 planned；成员 lead/dev 绑定不同会话、同 cwd——跨成员的写
// 互斥只可能来自项目写锁（MaxParallelTasks=3、无依赖）。
func writeTeamExclusiveRestartState(t *testing.T, dataDir string) *teamRestartState {
	t.Helper()
	now := time.Now()
	state := &teamRestartState{teamID: "tteam-lock-01", runID: "trun-lock-01"}
	const sessionLead, sessionDev = "km-team-1", "km-team-2"
	policy := team.DefaultPolicy()
	w1 := &team.Task{ID: "w1", MemberID: "lead", Prompt: "重启写锁独占 RESTART-LOCK-ONE hold",
		Attempt: 1, Ordinal: 0, Status: team.TaskDispatching, WriteLock: team.TaskWriteLockExclusive}
	w1.InputHash = team.InputHashFor(w1, team.PolicyDigest(policy), "")
	w2 := &team.Task{ID: "w2", MemberID: "dev", Prompt: "重启写锁接棒 RESTART-LOCK-TWO",
		Ordinal: 1, Status: team.TaskPlanned, WriteLock: team.TaskWriteLockExclusive}
	state.submissionID = team.TaskSubmissionID(state.teamID, state.runID, "w1", 1)
	w1.SubmissionID = state.submissionID
	state.sessionID = sessionLead

	teamRow := &team.Team{ID: state.teamID, Name: "重启写锁团队",
		Members: []team.TeamMember{
			{ID: "lead", SessionID: sessionLead, AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
			{ID: "dev", SessionID: sessionDev, AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
		},
		LeadID: "lead", Policy: policy, CreatedAt: now, UpdatedAt: now}
	teamRaw, err := json.Marshal(map[string]*team.Team{state.teamID: teamRow})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), teamRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: state.runID, TeamID: state.teamID, Goal: "重启写锁", Status: team.RunDispatching,
		Round: 1, PlanHash: "hand-lock", Tasks: []team.Task{*w1, *w2}, CreatedAt: now, UpdatedAt: now}
	runRaw, err := json.Marshal(map[string]*team.Run{state.runID: run})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), runRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	events := []*team.Event{
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventRunCreated, Source: "user",
			Payload:  map[string]any{"teamId": state.teamID, "mode": "pipeline", "planHash": "hand-lock", "taskCount": 2},
			Sequence: 1, CreatedAt: now},
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventTaskPlanned, Source: "user",
			Payload:  map[string]any{"taskId": "w1", "memberId": "lead"},
			Sequence: 2, CreatedAt: now},
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventTaskPlanned, Source: "user",
			Payload:  map[string]any{"taskId": "w2", "memberId": "dev"},
			Sequence: 3, CreatedAt: now},
		{ID: uuid.NewString(), RunID: state.runID, Type: team.EventTaskDispatching, Source: "runner",
			Payload:  map[string]any{"taskId": "w1", "sessionId": sessionLead, "submissionId": state.submissionID, "inputHash": w1.InputHash, "attempt": 1},
			Sequence: 4, CreatedAt: now},
	}
	var journalLines strings.Builder
	for _, event := range events {
		line, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		journalLines.Write(line)
		journalLines.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-journal.jsonl"), []byte(journalLines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := &submissionEntry{ID: state.submissionID, ThreadID: sessionLead, Host: "local",
		Text: w1.Prompt, Mode: "queue", Status: SubQueued,
		CreatedAt: float64(time.Now().UnixMilli()) / 1000}
	ledgerRaw, marshalErr := json.Marshal(map[string]*submissionEntry{
		sessionLead + ":" + state.submissionID: entry})
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "submissions.json"), ledgerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 两个成员会话同 cwd（写互斥的载体）；供重启后的新 Manager 冷 List 归属。
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
		[]byte(sessionLead+"\x00"+dataDir+"\n"+sessionDev+"\x00"+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return state
}

// TestTeamIntegrationRestartAdoptsWriteLock：重启恢复收养在途 exclusive 任务
// 时必须重取 cwd 写锁（LockRegistry 纯内存、进程重启即空）。断言用提示词
// 落盘做时序证据：w1 的 turn hold 期间（提示词已落盘、1.5s 窗口），w2 的
// 提示词不得出现（不重取锁则恢复后的首个调度 pass 就会把 w2 并发派出）；
// w1 落定后 w2 接棒，最终严格串行完成。
func TestTeamIntegrationRestartAdoptsWriteLock(t *testing.T) {
	dataDir := t.TempDir()
	state := writeTeamExclusiveRestartState(t, dataDir)
	env := newTeamIntegrationEnv(t, dataDir)
	// 冷 List 预热成员会话归属与 cwd（生产由 discovery 周期执行；Cwd 绝不
	// 主动冷探测，不预热则 exclusive 任务的 cwd 解析不到）。
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	if err := env.server.TeamFeeds.PrimeFromJournal(); err != nil {
		t.Fatalf("PrimeFromJournal: %v", err)
	}
	if err := env.server.Teams.RecoverAll(); err != nil {
		t.Fatalf("RecoverAll: %v", err)
	}

	// w1 的提示词到达伪 Agent（turn 随即 hold 1.5s）。
	if !env.drive(t, 8*time.Second, func() bool {
		return promptIndex(readTeamPrompts(t, dataDir), "RESTART-LOCK-ONE") >= 0
	}) {
		t.Fatal("w1 未在时限内派发到成员会话")
	}
	// hold 窗口内巡检若干拍：w2 的提示词不得出现（写锁互斥跨重启存活）。
	env.drive(t, 700*time.Millisecond, func() bool { return false })
	records := readTeamPrompts(t, dataDir)
	if len(records) != 1 || promptIndex(records, "RESTART-LOCK-TWO") >= 0 {
		t.Fatalf("w1 在途期间 w2 不得并发派发（写锁应跨重启重建）: %v", records)
	}

	// w1 落定后 w2 接棒，最终 Run 完成且提示词严格串行。
	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, state.teamID, state.runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	records = readTeamPrompts(t, dataDir)
	if len(records) != 2 {
		t.Fatalf("应恰好两条写任务提示词: %d 条", len(records))
	}
	oneAt, twoAt := promptIndex(records, "RESTART-LOCK-ONE"), promptIndex(records, "RESTART-LOCK-TWO")
	if oneAt != 0 || twoAt != 1 {
		t.Fatalf("写任务应严格串行（w1 完整先于 w2）: ONE@%d TWO@%d", oneAt, twoAt)
	}
	final := env.runDetail(t, state.teamID, state.runID)
	if workspace, _ := final["workspace"].(map[string]any); workspace["writeLockHeldBy"] != nil {
		t.Fatalf("完成后不应残留写锁: %v", workspace)
	}
}

// ---- 用例 6：§13.2 条目 6——共享工作区写锁互斥 ----

// TestTeamIntegrationWriteLockMutualExclusion：两个成员（不同会话、同 cwd）、
// 两条无依赖 exclusive 写任务（MaxParallelTasks=3 本可并行）→ 首任务 hold 期间
// 次任务必须仍在 planned；workspace.writeLockHeldBy 先后指向 w1、w2；提示词
// 串行落盘（首任务完整先于次任务，无 interleave）。
func TestTeamIntegrationWriteLockMutualExclusion(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "dev1-sess", "写者一")
	sessionB := env.createMemberSession(t, "dev2-sess", "写者二")
	teamID := env.createTeamWithMembers(t, `[{"id":"dev1","sessionId":"`+sessionA+
		`","role":"member"},{"id":"dev2","sessionId":"`+sessionB+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"写锁互斥","mode":"pipeline","plan":[
		{"id":"w1","memberId":"dev1","prompt":"首任务 hold 独占写入 ONE","writeLock":"exclusive"},
		{"id":"w2","memberId":"dev2","prompt":"次任务 hold 独占写入 TWO","writeLock":"exclusive"}]}`)

	// w1 持锁期间 w2 必须仍在 planned——互斥完全来自写锁而非并发上限。
	if !env.drive(t, 8*time.Second, func() bool {
		detail := env.runDetail(t, teamID, runID)
		workspace, _ := detail["workspace"].(map[string]any)
		return workspace["writeLockHeldBy"] == "w1" && teamTaskOf(t, detail, "w2")["status"] == "planned"
	}) {
		t.Fatal("w1 应独占写锁且 w2 保持 planned")
	}
	// w1 落定后 w2 接棒持锁（hold 拉宽观察窗口），最终 Run 完成、锁清零。
	sawW2Lock := false
	if !env.drive(t, 8*time.Second, func() bool {
		detail := env.runDetail(t, teamID, runID)
		workspace, _ := detail["workspace"].(map[string]any)
		if workspace["writeLockHeldBy"] == "w2" {
			sawW2Lock = true
		}
		return detail["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	if !sawW2Lock {
		t.Fatal("w2 应先后持有写锁")
	}
	final := env.runDetail(t, teamID, runID)
	if workspace, _ := final["workspace"].(map[string]any); workspace["writeLockHeldBy"] != nil {
		t.Fatalf("完成后不应残留写锁: %v", workspace)
	}
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 2 {
		t.Fatalf("应恰好两条写任务提示词: %d", len(records))
	}
	oneAt, twoAt := promptIndex(records, "ONE"), promptIndex(records, "TWO")
	if oneAt != 0 || twoAt != 1 {
		t.Fatalf("写任务应严格串行（首任务完整先于次任务）: ONE@%d TWO@%d", oneAt, twoAt)
	}
}

// ---- 用例 7：§13.2 条目 5——成员失效不静默改派 ----

// TestTeamIntegrationDeadMemberRequeueLadder：成员绑定的会话 Manager.Open 打不
// 开（伪 Agent 对 broken-sess 的 session/load 报错）→ 派发每次 outcomeRequeue
// 爬一格（每巡检节拍一格，10 次约 1.2s）→ 账本 SubFailed → 任务 failed、Run
// failed；成员记录仍在、run 详情可见错误，不静默改派；提示词零到达。
func TestTeamIntegrationDeadMemberRequeueLadder(t *testing.T) {
	dataDir := t.TempDir()
	// 伪 Agent 的会话登记：broken-sess 归属 kimi 且带 cwd（冷 List 探测命中），
	// 但 session/load 报错——归属明确、进程打不开的“成员失效”形态。
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
		[]byte("broken-sess\x00"+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := newTeamIntegrationEnv(t, dataDir)
	// 预热 list 缓存（生产由 discovery 周期执行，绑定校验与 cwd 解析靠它）。
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	teamID := env.createTeamWithMembers(t, `[{"id":"dev","sessionId":"broken-sess","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"成员失效","mode":"pipeline","plan":[
		{"id":"t1","memberId":"dev","prompt":"坏成员任务 BROKEN-CHECK","writeLock":"none"}]}`)

	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "failed"
	}) {
		t.Fatal("成员失效未在时限内爬梯至 failed")
	}
	detail := env.runDetail(t, teamID, runID)
	task := teamTaskOf(t, detail, "t1")
	if task["status"] != "failed" {
		t.Fatalf("任务应 failed: %v", task["status"])
	}
	if text, _ := task["error"].(string); !strings.Contains(text, "队列派发失败") {
		t.Fatalf("失败原因应来自账本爬梯: %q", text)
	}
	entry := env.server.Queue.get("broken-sess", task["submissionId"].(string))
	if entry == nil || entry.Status != SubFailed || entry.QueueAttempts != maxQueueAttempts {
		t.Fatalf("账本应爬满梯次后 failed: %+v", entry)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("失效会话不应收到任何提示词: %v", records)
	}
	// 成员记录仍在（不静默改派）。
	response := env.request(http.MethodGet, "/api/teams/"+teamID, "")
	var teamView struct {
		Members []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
		} `json:"members"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &teamView); err != nil {
		t.Fatal(err)
	}
	if len(teamView.Members) != 1 || teamView.Members[0].ID != "dev" ||
		teamView.Members[0].SessionID != "broken-sess" {
		t.Fatalf("失效成员应保留原绑定: %s", response.Body.String())
	}
}

// ---- 用例 8：§15 可靠性 2——重复提交幂等 ----

// TestTeamIntegrationIdempotentRedispatch：submission 级幂等（生产 QueueAdapter
// 重放同 id 同内容 → duplicate=true 零副作用、提示词不增长）；已终态 Run 的
// 恢复是 no-op。现状记录：run 创建接口未定义请求幂等键（§10.2 无 id 字段），
// 重复 POST 会创建第二个 Run——两条 Run 各自恰好派发一次，submission 级去重
// 仍然成立（与指令“同 id 重复 POST run → 一个 Run”的差距在结果中说明）。
func TestTeamIntegrationIdempotentRedispatch(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "幂等会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	const prompt = "幂等任务 IDEM 只发一次"
	runBody := `{"goal":"幂等","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"` + prompt + `","writeLock":"none"}]}`
	runID := env.createRun(t, teamID, runBody)
	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	detail := env.runDetail(t, teamID, runID)
	submissionID, _ := teamTaskOf(t, detail, "t1")["submissionId"].(string)
	if submissionID == "" {
		t.Fatal("完成任务应带 submissionId")
	}
	if got := len(readTeamPrompts(t, env.dataDir)); got != 1 {
		t.Fatalf("应恰好派发一次: %d", got)
	}

	// 生产 QueueAdapter 重放同 id 同内容：duplicate=true，零副作用。
	status, duplicate, err := env.queueAdp.Enqueue(sessionA, submissionID, prompt)
	if err != nil || !duplicate || status != SubAccepted {
		t.Fatalf("重放应命中账本幂等: status=%s duplicate=%v err=%v", status, duplicate, err)
	}
	env.server.dispatchQueuedPass()
	if got := len(readTeamPrompts(t, env.dataDir)); got != 1 {
		t.Fatalf("重放不得产生第二个 turn: %d", got)
	}

	// 已终态 Run 的恢复是 no-op（不重派、不改状态）。
	if err := env.server.Teams.RecoverRun(runID); err != nil {
		t.Fatalf("终态恢复应无错: %v", err)
	}
	env.server.dispatchQueuedPass()
	if got := len(readTeamPrompts(t, env.dataDir)); got != 1 {
		t.Fatalf("终态恢复不得重发: %d", got)
	}

	// 现状记录：run 创建无幂等键，重复请求创建第二个 Run（见函数注释）。
	secondID := env.createRun(t, teamID, runBody)
	if secondID == runID {
		t.Fatal("当前接口重复 POST 会创建新 Run，不应复用旧 id")
	}
	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, secondID)["status"] == "completed"
	}) {
		t.Fatal("第二个 Run 未在时限内完成")
	}
	if got := len(readTeamPrompts(t, env.dataDir)); got != 2 {
		t.Fatalf("两条 Run 应各自恰好派发一次（共 2 次）: %d", got)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs", "")
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 2 {
		t.Fatalf("应有两条 Run 记录: %s", response.Body.String())
	}
}

// ---- 用例 9：§13.2 条目 9——事件流 changes/reset 与 SSE ----

// TestTeamIntegrationEventStreamChangesAndSSE：真实 Run 的事件投影——
// changes after=-1 全量（reset 整页）、游标增量为空、超界游标 reset、带 start=
// 与多余参数不报错（web/transport.js:104 恒发 start，回归保护）；SSE 走真服务
// （httptest.NewServer + hostPort 注册 + 后台收流）：ready 帧后，任务运行/完成
// 事件以 changes/reset 帧到达且帧 id 为 epoch.sequence。
func TestTeamIntegrationEventStreamChangesAndSSE(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "事件流会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"事件流","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"事件流任务 EVSTREAM hold","writeLock":"none"}]}`)

	// 先做游标断言（此时尚未驱动派发，事件集稳定在创建期三事件）。
	var page map[string]any
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
		if response.Code != http.StatusOK {
			t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
		}
		page = map[string]any{}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if rows, _ := page["rows"].([]any); len(rows) >= 3 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	if rows, _ := page["rows"].([]any); len(rows) < 3 {
		t.Fatalf("创建期事件不足: %v", page)
	}
	if reset, _ := page["reset"].(bool); !reset {
		t.Fatalf("无 epoch 首读应 reset 整页: %v", page)
	}
	epoch, _ := page["epoch"].(string)
	sequence, _ := page["sequence"].(float64)
	if epoch == "" || sequence < 3 {
		t.Fatalf("epoch/sequence 异常: %v %v", epoch, sequence)
	}
	// 增量：带 start= 与未知参数（transport.js 恒发 start）不报错且为空。
	response := env.request(http.MethodGet,
		"/api/teams/"+teamID+"/runs/"+runID+"/changes?after="+strconv.FormatInt(int64(sequence), 10)+"&epoch="+epoch+"&start=&foo=bar", "")
	if response.Code != http.StatusOK {
		t.Fatalf("增量读取应 200: %d %s", response.Code, response.Body.String())
	}
	page = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &page)
	if rows, _ := page["rows"].([]any); len(rows) != 0 {
		t.Fatalf("最新游标的增量应为空: %v", page)
	}
	if _, hasReset := page["reset"]; hasReset {
		t.Fatalf("最新游标不应 reset: %v", page)
	}
	if meta, _ := page["meta"].(map[string]any); meta == nil || meta["status"] == nil {
		t.Fatalf("meta 应含 Run 状态摘要: %v", meta)
	}
	// 超界游标：reset 整页。
	response = env.request(http.MethodGet,
		"/api/teams/"+teamID+"/runs/"+runID+"/changes?after="+strconv.FormatInt(int64(sequence)+1000, 10)+"&epoch="+epoch, "")
	if response.Code != http.StatusOK {
		t.Fatalf("超界游标读取应 200: %d", response.Code)
	}
	page = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &page)
	if reset, _ := page["reset"].(bool); !reset {
		t.Fatalf("超界游标应 reset: %v", page)
	}

	// SSE：真服务 + 注册 hostPort（projections_test.go:185-188 惯例）。
	api := httptest.NewServer(env.server)
	hostPort := api.URL[len("http://"):]
	env.server.Hosts[hostPort] = true
	env.server.Origins["http://"+hostPort] = true
	env.server.LocalHosts[hostPort] = true
	authorized := func(method, target, body string) *http.Response {
		request, err := http.NewRequest(method, api.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = hostPort
		request.Header.Set("Origin", "http://"+hostPort)
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CSRF-Token", env.csrf)
		}
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: env.token})
		response, callErr := (&http.Client{Timeout: 15 * time.Second}).Do(request)
		if callErr != nil {
			t.Fatal(callErr)
		}
		return response
	}

	events := authorized(http.MethodGet,
		"/api/teams/"+teamID+"/runs/"+runID+"/events?incremental=1", "")
	if events.StatusCode != http.StatusOK {
		t.Fatalf("events 应 200: %d", events.StatusCode)
	}
	type sseFrame struct {
		event string
		data  string
		id    string
	}
	frames := make(chan sseFrame, 32)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(events.Body)
		event, data, id := "", "", ""
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimSuffix(strings.TrimPrefix(line, "event: "), "\n")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimSuffix(strings.TrimPrefix(line, "data: "), "\n")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimSuffix(strings.TrimPrefix(line, "id: "), "\n")
			case line == "\n":
				if event != "" {
					frames <- sseFrame{event: event, data: data, id: id}
				}
				event, data, id = "", "", ""
			}
		}
	}()

	// ready 帧先到（含 epoch/sequence）。
	select {
	case frame := <-frames:
		if frame.event != "ready" || !strings.Contains(frame.data, "epoch") {
			t.Fatalf("ready 帧异常: %+v", frame)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	// 驱动派发：hold 拉宽窗口，task_running 与 task_completed 事件相继入投影。
	if !env.drive(t, 8*time.Second, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	sawCompleted := false
	for !sawCompleted {
		select {
		case frame := <-frames:
			if frame.event == "changes" || frame.event == "reset" {
				if strings.Contains(frame.data, "task_completed") {
					sawCompleted = true
					if !strings.Contains(frame.id, ".") {
						t.Fatalf("帧 id 应为 epoch.sequence: %q", frame.id)
					}
				}
			}
		case <-time.After(8 * time.Second):
			t.Fatal("SSE 未在时限内发出携带 task_completed 的帧")
		}
	}
	// 收流结束：先断开订阅再关服务器（Close 会等待 handler 退出）。
	events.Body.Close()
	<-readerDone
	api.Close()
}
