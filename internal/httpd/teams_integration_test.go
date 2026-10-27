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
// 两者都经 queueMu 串行化。等待断言全部用 deadline 轮询（120ms 节拍、20s
// 超时——race 开销下 8s 实测出现过间歇超时，统一提至 20s 留余量；§13.1
// 不用固定 sleep 判正确性）；watcher 等待片经 team.Service.SetWaitSlice
// 缩到毫秒级（runner.go 的导出测试钩子）。缺 python3 时沿用 t.Skip 惯例。
// 阶段二组（TestTeamIntegration2*，§12 阶段二验收 / §13.2 条目 2/7/8/11/12）
// 在本文件后半段，复用同一套 fixture，另装配 Transcripts/Workspaces 端口。
// 阶段四组（TestTeamIntegration4*，§12 阶段四验收三条 + §13.2 对应行）同样
// 在后半段（定时触发/重启恢复/结果未知 reconciler/手机恢复/pause/stall/
// 预算/通知/stats/graph），另装配 TeamJournal、Scheduler 与伪 ntfy 通知渠道。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// teamAgent 是 queueAgent 的多会话扩展：session/new 每次发递增 sessionId
// （km-team-N，计数落盘跨进程共享——每个会话是独立子进程）；session/list 从
// sessions.txt 读回（供“重启后”的新 Manager 冷探测归属与 cwd）；session/load
// 回显请求的 sessionId（broken-sess 除外，报错模拟会话打不开→派发 requeue 爬
// 梯）。session/prompt 把 sessionId+NUL+text 追加进 prompts.log（换行转义成
// \n 保住一行一记录），随后按标记分派（阶段二增量见 teamAgentMarkers）：
//   - text 含 FAIL → 回 JSON-RPC error（awaitPrompt→FailTurn，turn.status=
//     failed 且 messages 附 kind==error 条目，manager.go:810-811）模拟任务失败；
//   - text 含 HOLDLONG → 后台线程 hold 20s，session/cancel 立即以
//     stopReason=cancelled 收尾（运行中取消窗口）；
//   - text 含 hold → sleep 1.5s（并发/写锁互斥窗口）；
//   - text 含 HOLDUNTIL:key → 等待 {dir}/release-{key}-{sessionId}.txt 出现
//     或 session/cancel（每 50ms 轮询、20s 上限；测试用落文件精确放行单个
//     turn，替代不可控的时长 sleep）；
//   - 阶段三增量（teams_phase3_integration_test.go，分支先于 FAIL 族判定）：
//     修复提示词（RepairPrompt 特征「动作块第 N 次解析失败」）→ 回
//     actionsrepair-N.txt（缺文件退 actionsrepair-default.txt，再缺回普通
//     文本）；ASKQ4/ASKQ → 发 4/1 张 question:true 反向提问卡（title=
//     AskUserQuestion、问题文本在 toolCall.content、选项在 options——
//     acpview AddPermission 据此标记 question 并映射 {id,label}），等全部
//     回答（JSON-RPC response 按 ask-* id 路由、答案落 answers.log）或
//     session/cancel；REPLYACTIONS:NAME → 回 actions-NAME.txt 全文
//     （<team-actions> 剧本，合法/非法由文件内容决定）；
//   - 其余立即 end_turn。完成前先发一条 assistant 文本块“已完成：<全文>”
//     （ExtractResult 据此拿到有界结果）。
func teamAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,re,sys,threading,time
DIR = "` + dir + `"
LOCK = threading.Lock()
STATE = {"event": threading.Event(), "cancelled": False}
CWD = {"dir": "."}
ASKEV = {}
ASKANS = {}
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
def prompt_count(sid, text):
 # 同一 (会话, 全文) 已到达次数（含本次——record 先于判定落盘）：一次性
 # 失败标记（RATELIMIT/QUOTAFAIL）据此只在首次到达时失败。
 needle = sid + "\x00" + text.replace("\n", "\\n")
 n = 0
 try:
  with open(os.path.join(DIR, "prompts.log")) as fh:
   for line in fh:
    if line.rstrip("\n") == needle:
     n += 1
 except FileNotFoundError:
  pass
 return n
def reply(payload):
 print(json.dumps(payload, ensure_ascii = False), flush = True)
def read_script(name):
 try:
  with open(os.path.join(DIR, name)) as fh:
   return fh.read()
 except FileNotFoundError:
  return None
def ask_card(sid, key, question):
 # 注册并发一张 question:true 反向提问卡（不等待——多张卡须同帧在途供
 # 「同 Task 未决上限」用例观察）。
 ev = threading.Event()
 with LOCK:
  ASKEV[key] = ev
 reply({"jsonrpc": "2.0", "id": key, "method": "session/request_permission",
        "params": {"sessionId": sid,
                   "options": [{"optionId": "opt-a", "name": "继续", "kind": "allow_once"},
                               {"optionId": "opt-b", "name": "跳过", "kind": "allow_once"}],
                   "toolCall": {"title": "AskUserQuestion", "content": question}}})
def ask_wait(key):
 # 等单张卡回答或 session/cancel；答案落 answers.log，返回 turn 是否被取消。
 ev = ASKEV.get(key)
 if ev is None:
  return False
 cancelled = False
 for _ in range(400):
  with LOCK:
   cancelled = STATE["cancelled"]
  if cancelled or ev.wait(0.05):
   break
 with LOCK:
  answer = ASKANS.pop(key, None)
  ASKEV.pop(key, None)
 if answer is not None:
  record("answers.log", key + "\x00" + json.dumps(answer, ensure_ascii = False))
 return cancelled
def handle_prompt(m, ident):
 blocks = (m.get("params") or {}).get("prompt") or []
 text = next((b.get("text", "") for b in blocks if isinstance(b, dict)), "")
 sid = (m.get("params") or {}).get("sessionId") or "?"
 record("prompts.log", sid + "\x00" + text.replace("\n", "\\n"))
 # 阶段三分支先于 FAIL 族：修复提示词里引用的违规模板可能含 FAIL 等字样。
 repair = re.search(r"动作块第 (\d+) 次解析失败", text)
 script = None if repair else re.search(r"REPLYACTIONS:([A-Za-z0-9_-]+)", text)
 if repair:
  body = read_script("actionsrepair-%s.txt" % repair.group(1))
  if body is None:
   body = read_script("actionsrepair-default.txt")
  if body is None:
   body = "已完成：没有可执行的动作。"
  reply({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": body}}}})
  reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "end_turn"}})
  return
 if "ASKQ4" in text or "ASKQ" in text:
  count = 4 if "ASKQ4" in text else 1
  keys = ["ask-%d" % k for k in range(1, count + 1)]
  for k, key in enumerate(keys, 1):
   ask_card(sid, key, "部署环境选择 PH3-ASKQ-%d：请选择处理方式" % k)
  for key in keys:
   if ask_wait(key):
    reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "cancelled"}})
    return
  reply({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "已完成：" + text}}}})
  reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "end_turn"}})
  return
 if script:
  body = read_script("actions-%s.txt" % script.group(1))
  if body is None:
   body = "已完成：动作剧本文件缺失。"
  reply({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": body}}}})
  reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "end_turn"}})
  return
 # 阶段四增量（本文件后半段）：QUOTAGATE:key → 先等 {dir}/release-{key}-
 # {sessionId}.txt（HOLDUNTIL 同款精确放行），放行后回 quota 错误（余额/
 # 充值措辞触发 aimd.go quotaRE）——制造「在途任务延迟停靠」的可控窗口
 # （pause 期间停靠升级 waiting_input 的用例）；一次性标记，二次到达跳过
 # 本分支落入普通完成（resume 重飞即通过）。
 if "QUOTAGATE" in text and prompt_count(sid, text) < 2:
  with LOCK:
   STATE["cancelled"] = False
  match = re.search(r"QUOTAGATE:([A-Za-z0-9_-]+)", text)
  gate = os.path.join(DIR, "release-%s-%s.txt" % (match.group(1) if match else "x", sid))
  for _ in range(400):
   with LOCK:
    if STATE["cancelled"]:
     break
   if os.path.exists(gate):
    break
   time.sleep(0.05)
  with LOCK:
   cancelled = STATE["cancelled"]
  if cancelled:
   reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": "cancelled"}})
  else:
   reply({"jsonrpc": "2.0", "id": ident, "error": {"code": -32000, "message": "请求失败：账户余额不足，请充值后再试"}})
  return
 # 特定失败标记先于通用 FAIL（QUOTAFAIL 内含子串 FAIL：一次性标记二次到达
 # 跳过自身分支后，也不得落入通用 FAIL——否则重试 attempt 永远失败）。
 if "RATELIMIT" in text and prompt_count(sid, text) < 2:
  reply({"jsonrpc": "2.0", "id": ident, "error": {"code": -32000, "message": "429 Too Many Requests：请求过于频繁，请稍后重试（Retry-After: 2）"}})
  return
 if "QUOTAFAIL" in text and prompt_count(sid, text) < 2:
  reply({"jsonrpc": "2.0", "id": ident, "error": {"code": -32000, "message": "请求失败：账户余额不足，请充值后再试"}})
  return
 if "FAIL" in text and "QUOTAFAIL" not in text:
  reply({"jsonrpc": "2.0", "id": ident, "error": {"code": -32000, "message": "任务执行失败：FAIL 标记"}})
  return
 if "WRITEFILE" in text:
  # 写入会话工作目录下已跟踪的 notes.txt（worktree diff 数据面的改动载体；
  # 新建未跟踪文件不会出现在 git diff 里）。
  try:
   with open(os.path.join(CWD["dir"], "notes.txt"), "w") as fh:
    fh.write("agent 产出：" + text + "\n")
  except Exception:
   pass
 reply({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "已完成：" + text}}}})
 stop = "end_turn"
 if "HOLDLONG" in text:
  with LOCK:
   STATE["event"].clear(); STATE["cancelled"] = False
  STATE["event"].wait(20)
  stop = "cancelled" if STATE["cancelled"] else "end_turn"
 elif "HOLDUNTIL" in text:
  with LOCK:
   STATE["cancelled"] = False  # 取消状态按 turn 作用：上个 turn 的 session/cancel 不得传染本 turn
  # 闸门键只从 [Assigned task] 尾段提取：注入的 [Team context] 可能携带上游
  # 结果里的同名标记（rsplit 取最后一个信封头）；尾段无标记说明本任务不设
  # 闸门（阶段四图用例的下游任务上下文含上游 HOLDUNTIL 结果文本），直接
  # 放行，绝不落到 release-x 的 20s 死等。
  tail = text.rsplit("[Assigned task]", 1)[-1]
  match = re.search(r"HOLDUNTIL:([A-Za-z0-9_-]+)", tail)
  if match:
   gate = os.path.join(DIR, "release-%s-%s.txt" % (match.group(1), sid))
   for _ in range(400):
    with LOCK:
     if STATE["cancelled"]:
      break
    if os.path.exists(gate):
     break
    time.sleep(0.05)
  stop = "cancelled" if STATE["cancelled"] else "end_turn"
 elif "hold" in text:
  time.sleep(1.5)
 reply({"jsonrpc": "2.0", "id": ident, "result": {"stopReason": stop}})
for line in sys.stdin:
 m = json.loads(line); method = m.get("method"); ident = m.get("id")
 if method == "initialize":
  result = {"protocolVersion": 1, "agentCapabilities": {}, "agentInfo": {"name": "fake"}, "authMethods": []}
 elif method == "session/new":
  sid = next_session()
  cwd = (m.get("params") or {}).get("cwd") or "."
  CWD["dir"] = cwd
  record("sessions.txt", sid + "\x00" + cwd)
  result = {"sessionId": sid, "modes": {"currentModeId": "default", "availableModes": [{"id": "default"}]}, "configOptions": []}
 elif method == "session/load":
  sid = (m.get("params") or {}).get("sessionId") or ""
  if sid == "broken-sess":
   reply({"jsonrpc": "2.0", "id": ident, "error": {"code": -32001, "message": "会话无法恢复：broken-sess"}})
   continue
  cwd = (m.get("params") or {}).get("cwd") or "."
  CWD["dir"] = cwd
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
 elif method is None and isinstance(ident, str) and ident.startswith("ask-"):
  # 反向提问卡的回答（JSON-RPC response）：唤醒等待线程，答案由 ask_wait 落盘。
  with LOCK:
   ASKANS[ident] = m.get("result")
   if ident in ASKEV:
    ASKEV[ident].set()
  continue
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
// teamIntegrationWait 给全包运行时的 ACP 子进程调度留出余量；单测单独运行
// 通常远快于此值，但完整 httpd 包会同时启动大量伪 Agent。
const teamIntegrationWait = 45 * time.Second

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
	// team-runs.json 整文件损坏时按 journal 重建（main.go:921-930 的同构启动
	// 序列；损坏原件已由 LoadStore 改名留存）。阶段三恢复面用例据此覆盖
	// pendingQuestions/pendingActions/缓存完成事实的重建。
	if store.RunsLedgerCorrupt() {
		if events, replayErr := journal.Replay(); replayErr == nil {
			store.RebuildRunsFromJournal(events)
		}
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
	// TeamJournal 供 /stats 端点全量重放 journal（main.go:928-930 的同构
	// 装配：与 TeamStore 同源；缺它 stats 端点按 409 拒绝）。
	server.TeamJournal = journal
	server.TeamFeeds = NewTeamFeedRegistry(journal, store)
	// changes 长轮询的测试等待上限（生产默认 12s，见 teams.go）。
	server.TeamChangesWait = 20 * time.Millisecond
	server.Teams = team.NewService(store, journal, queueAdapter, resolver)
	server.Teams.SetWaitSlice(2 * time.Millisecond)
	server.Teams.OnEvent = server.TeamFeeds.Ingest
	// 阶段二端口（main.go:935-940 的同构装配）：转发转录源 + worktree 注册表
	// （注册表与 Service.Workspaces 共用同一份 team-workspaces.json）。
	server.Teams.Transcripts = NewTeamTranscriptSource(server)
	teamWorkspaces := NewTeamWorkspaceRegistry(filepath.Join(dataDir, "team-workspaces.json"))
	server.TeamWorkspaceRegistry = teamWorkspaces
	server.Teams.Workspaces = teamWorkspaces
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
	return e.createMemberSessionIn(t, requestedID, e.dataDir, title)
}

// createMemberSessionIn 建立开在指定 cwd 的成员会话（worktree 用例：会话
// 必须已开在 worktree 中，§6.4 规则 4 的派发前置校验据此判定）。
func (e *teamIntegrationEnv) createMemberSessionIn(t *testing.T, requestedID, cwd, title string) string {
	t.Helper()
	session, err := e.manager.Create(requestedID, cwd, title, 30*time.Second)
	if err != nil {
		t.Fatalf("成员会话创建失败: %v", err)
	}
	return session.ID
}

// awaitCond 是不做派发巡检的 deadline 轮询（drive 会推进账本派发；等待
// 调度循环自身完成的副作用——如 first_success 取消兄弟分支——时不能混入
// 派发推动，否则时序证据被污染）。120ms 节拍，与 drive 同款超时纪律。
func awaitCond(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(120 * time.Millisecond)
	}
	return check()
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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

	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	interrupted := env.drive(t, teamIntegrationWait, func() bool {
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
	deadline := time.Now().Add(20 * time.Second)
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

	if !env.drive(t, teamIntegrationWait, func() bool {
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
		if !env.drive(t, teamIntegrationWait, func() bool {
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
		if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		workspace, _ := detail["workspace"].(map[string]any)
		return workspace["writeLockHeldBy"] == "w1" && teamTaskOf(t, detail, "w2")["status"] == "planned"
	}) {
		t.Fatal("w1 应独占写锁且 w2 保持 planned")
	}
	// w1 落定后 w2 接棒持锁（hold 拉宽观察窗口），最终 Run 完成、锁清零。
	sawW2Lock := false
	if !env.drive(t, teamIntegrationWait, func() bool {
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

	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	if !env.drive(t, teamIntegrationWait, func() bool {
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
	deadline := time.Now().Add(20 * time.Second)
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
	case <-time.After(20 * time.Second):
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	// 驱动派发：hold 拉宽窗口，task_running 与 task_completed 事件相继入投影。
	if !env.drive(t, teamIntegrationWait, func() bool {
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
		case <-time.After(20 * time.Second):
			t.Fatal("SSE 未在时限内发出携带 task_completed 的帧")
		}
	}
	// 收流结束：先断开订阅再关服务器（Close 会等待 handler 退出）。
	events.Body.Close()
	<-readerDone
	api.Close()
}

// ---- 阶段二组（TestTeamIntegration2*）：§12 阶段二验收 / §13.2 条目 2/7/8/11/12 ----
//
// 覆盖 broadcast 并行与分支失败 salvage、first_success 早停（下游不被级联
// 误杀）、成员显式转发、worktree 基线与 diff、验证闸门退出码裁决、AIMD 降容
// 与错误策略（quota 停靠/重派）、幂等与重启收养、事件流行。选择器约定：
// 阶段一组 `-run 'TestTeamIntegration[^2]'`，阶段二组 `-run TestTeamIntegration2`
// （TestTeamIntegration 前缀会同时命中两组，-race 下分组执行避免时限压力叠加）。

// inFlightTask 报告任务是否处于在途状态（并行窗口断言用）。
func inFlightTask(task map[string]any) bool {
	switch task["status"] {
	case "dispatching", "running":
		return true
	}
	return false
}

// journalEvents 读回 journal 全量事件（事件顺序/计数断言用）。
func (e *teamIntegrationEnv) journalEvents(t *testing.T) []*team.Event {
	t.Helper()
	events, err := e.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	return events
}

// teamEventSeq 返回首个（类型, taskId）匹配事件的序号（-1=不存在；taskID 空
// 串匹配任意 run 级事件）。
func teamEventSeq(events []*team.Event, eventType, taskID string) int64 {
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		if taskID == "" {
			return int64(event.Sequence)
		}
		if id, _ := event.Payload["taskId"].(string); id == taskID {
			return int64(event.Sequence)
		}
	}
	return -1
}

// teamEventCount 统计某类型事件条数。
func teamEventCount(events []*team.Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

// teamEventPayload 返回首条该类型事件的 payload（nil=不存在）。
func teamEventPayload(events []*team.Event, eventType string) map[string]any {
	for _, event := range events {
		if event.Type == eventType {
			return event.Payload
		}
	}
	return nil
}

// releaseAgentTurn 落 HOLDUNTIL 闸门文件，放行指定会话上 key 闸门的在途
// turn（替代固定 sleep：测试用文件精确控制单个 turn 的完成时机）。
func releaseAgentTurn(t *testing.T, dir, key, sessionID string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "release-"+key+"-"+sessionID+".txt"),
		[]byte("go\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// teamSessionTurnStatus 在会话快照里按 user 消息全文找到 turn 并返回其状态
// （matchTurn 的测试侧镜像；找不到返回空串）。
func teamSessionTurnStatus(session *acpruntime.Session, userText string) string {
	if session == nil {
		return ""
	}
	turns, _ := session.Snapshot().MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return ""
	}
	matched := ""
	for _, item := range turns.Items {
		turn, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, entry := range messages.Items {
			message, isObject := entry.(*jsoncompat.Object)
			if !isObject {
				continue
			}
			if role, _ := message.MustGet("role").(string); role != "user" {
				continue
			}
			if text, _ := message.MustGet("text").(string); text == userText {
				matched, _ = turn.MustGet("status").(string)
			}
		}
	}
	return matched
}

// newTeamWorktreeFixture 预备 git 仓库 + linked worktree（含已跟踪文件
// notes.txt——WRITEFILE 标记的改动载体，diff 数据面只呈现已跟踪改动），返回
// worktree 路径与其 HEAD 基线提交。
func newTeamWorktreeFixture(t *testing.T) (string, string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	run := func(dir string, args ...string) string {
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		var out strings.Builder
		cmd.Stdout = &out
		cmd.Stderr = &out
		if runErr := cmd.Run(); runErr != nil {
			t.Fatalf("git %v 失败: %v\n%s", args, runErr, out.String())
		}
		return out.String()
	}
	run(root, "init", "-q", "repo")
	run(repo, "config", "user.email", "team@test.local")
	run(repo, "config", "user.name", "team test")
	if err := os.WriteFile(filepath.Join(repo, "notes.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-q", "-m", "base")
	worktree := filepath.Join(root, "wt")
	run(repo, "worktree", "add", "-q", worktree, "-b", "wt-branch")
	return worktree, strings.TrimSpace(run(worktree, "rev-parse", "HEAD"))
}

// registerTeamWorkspace 注册 worktree 到 /api/teams/workspaces 注册表。
func registerTeamWorkspace(t *testing.T, env *teamIntegrationEnv, id, path string) {
	t.Helper()
	response := env.request(http.MethodPost, "/api/teams/workspaces",
		fmt.Sprintf(`{"id":%q,"path":%q}`, id, path))
	if response.Code != http.StatusOK {
		t.Fatalf("工作区注册失败: %d %s", response.Code, response.Body.String())
	}
}

// writeVerifyMakefile 在 dir 写出以 exitCode 收口的 Makefile check 目标（闸门
// 命令 make check 的 Dir 是 Lead 会话 cwd）。注意：本机 GNU Make 3.81 把失败
// recipe 映射为 make 退出码 2（实测 @exit 7 → shell $? = 2，.POSIX 亦然），
// 退出码断言按机器实测的 2 记——「退出码 7」无法经 make 的状态码透出。
func writeVerifyMakefile(t *testing.T, dir string, exitCode int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "Makefile"),
		[]byte(fmt.Sprintf("check:\n\t@exit %d\n", exitCode)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// 用例 2-1：§13.2 条目 2 + §12 阶段二验收——broadcast 双会话并行。

// TestTeamIntegration2BroadcastParallel：broadcast 条目展开为两成员任务
// （grp-lead/grp-dev），两会话并行（hold 拉宽窗口内同处 in-flight）、提示词
// 同文完整落盘（每条整行、无 interleave）、结果按来源各自携带
// sessionId/turnId、collect=all 下全 completed 后 Run 终局 completed。
func TestTeamIntegration2BroadcastParallel(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "并行会话一")
	sessionB := env.createMemberSession(t, "dev-sess", "并行会话二")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"dev","sessionId":"`+sessionB+`","role":"member"}]`)
	const prompt = "并行竞速任务 BCAST-PARALLEL hold 请两路同时处理"
	runID := env.createRun(t, teamID, `{"goal":"双会话并行","mode":"broadcast","plan":[
		{"id":"grp","memberIds":["lead","dev"],"prompt":"`+prompt+`","writeLock":"none"}]}`)

	// 并行窗口：两分支同时处于在途（两成员、写锁 none、并发上限 3——互不
	// 排斥的分支应同时在飞）。
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return inFlightTask(teamTaskOf(t, detail, "grp-lead")) &&
			inFlightTask(teamTaskOf(t, detail, "grp-dev"))
	}) {
		t.Fatal("两分支未在时限内并行在途")
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	detail := env.runDetail(t, teamID, runID)
	branches := []struct{ id, session string }{{"grp-lead", sessionA}, {"grp-dev", sessionB}}
	for _, branch := range branches {
		card := teamTaskOf(t, detail, branch.id)
		if card["status"] != "completed" {
			t.Fatalf("任务 %s 应 completed: %v", branch.id, card["status"])
		}
		if group, _ := card["broadcastGroup"].(string); group != "grp" {
			t.Fatalf("任务 %s 应携带广播组 grp: %v", branch.id, card["broadcastGroup"])
		}
		result := taskResultOf(t, card)
		if result["sessionId"] != branch.session {
			t.Fatalf("任务 %s 的结果应归属本成员会话: %v ≠ %s", branch.id, result["sessionId"], branch.session)
		}
		turnID, _ := result["turnId"].(string)
		if turnID == "" {
			t.Fatalf("任务 %s 的结果缺 turnId: %v", branch.id, result)
		}
	}
	// 提示词恰好两条、落在不同成员会话、文本逐字节相同（每条完整一行，
	// 无 interleave），且无上下文信封时全文就是任务本身。
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 2 {
		t.Fatalf("应恰好两条分支提示词: %d 条", len(records))
	}
	if records[0].session == records[1].session ||
		(records[0].session != sessionA && records[0].session != sessionB) {
		t.Fatalf("两条提示词应落在不同成员会话: %v", records)
	}
	if records[0].text != records[1].text || records[0].text != prompt {
		t.Fatalf("广播分支提示词应同文且完整无交错: %q vs %q", records[0].text, records[1].text)
	}
}

// 用例 2-2：§12 阶段二验收 / §13.2 条目 12——分支失败 salvage。

// TestTeamIntegration2BroadcastBranchFailureSalvage：broadcast 分支之一失效
// （broken-sess 爬梯 failed），另一分支完成——Run 折 failed 但 salvage 保住
// 完成分支结果；下游合并任务不被失败分支级联误杀（组内全终态且 ≥1
// completed 即释放），其上下文只含完成成员。
func TestTeamIntegration2BroadcastBranchFailureSalvage(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
		[]byte("broken-sess\x00"+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := newTeamIntegrationEnv(t, dataDir)
	if _, err := env.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	sessionB := env.createMemberSession(t, "dev-sess", "健全分支会话")
	sessionC := env.createMemberSession(t, "rev-sess", "合并分支会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"dev","sessionId":"`+sessionB+
		`","role":"lead"},{"id":"dead","sessionId":"broken-sess","role":"member"},`+
		`{"id":"rev","sessionId":"`+sessionC+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"分支失败挽救","mode":"broadcast","plan":[
		{"id":"grp","memberIds":["dev","dead"],"prompt":"广播分支任务 SALV2-BRANCH","writeLock":"none"},
		{"id":"merge","memberIds":["dev","rev"],"prompt":"合并任务 SALV2-MERGE 汇总上游结果","writeLock":"none","dependsOn":["grp"]}]}`)

	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "failed"
	}) {
		t.Fatal("含失败分支的 Run 未在时限内折 failed")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "grp-dead"); task["status"] != "failed" {
		t.Fatalf("失效分支应 failed: %v", task["status"])
	}
	salvaged := teamTaskOf(t, detail, "grp-dev")
	if salvaged["status"] != "completed" || salvaged["result"] == nil {
		t.Fatalf("失败 Run 里完成分支的结果仍应可见（salvage）: %v", salvaged)
	}
	if result := taskResultOf(t, salvaged); !strings.Contains(result["result"].(string), "SALV2-BRANCH") {
		t.Fatalf("完成分支结果内容异常: %v", result)
	}
	// 下游合并不被失败分支级联误杀：组内全终态且 ≥1 completed 即释放，两个
	// 合并任务都完成且上下文只含完成成员。
	for _, mergeID := range []string{"merge-dev", "merge-rev"} {
		if task := teamTaskOf(t, detail, mergeID); task["status"] != "completed" {
			t.Fatalf("合并任务 %s 不应被级联终止: %v", mergeID, task["status"])
		}
	}
	records := readTeamPrompts(t, dataDir)
	mergeAt := promptIndex(records, "SALV2-MERGE")
	if mergeAt < 0 {
		t.Fatal("合并任务提示词应已派发")
	}
	if text := records[mergeAt].text; !strings.Contains(text, "[Team context]") ||
		!strings.Contains(text, "来源任务：grp-dev") || strings.Contains(text, "grp-dead") {
		t.Fatalf("合并任务应只携带完成分支的上下文: %q", text)
	}
}

// 用例 2-3：§6.5 first_success 早停——下游不被级联误杀（评审缺失项）。

// TestTeamIntegration2FirstSuccessEarlyStop：组内首个 completed 即满足——
// 落败分支取消（superseded_by_first_success）、其 turn 被真取消（interrupted）、
// 账本不重排队；事件顺序 task_completed(胜者) 先于 task_stopped(败者)；下游
// 合并任务不被 superseded 取消级联误杀，组释放后正常派发并只收到胜者的
// [Team context]；quota 停靠驱动的 waiting_input 经 resume 后同语义重放。
func TestTeamIntegration2FirstSuccessEarlyStop(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "a-sess", "竞速会话甲")
	sessionB := env.createMemberSession(t, "b-sess", "竞速会话乙")
	teamID := env.createTeamWithMembers(t, `[{"id":"a","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"b","sessionId":"`+sessionB+`","role":"member"}]`)
	const grpPrompt = "竞速任务 FIRST-GRP HOLDUNTIL:k1"
	runID := env.createRun(t, teamID, `{"goal":"首达即停","mode":"broadcast","collectPolicy":"first_success","plan":[
		{"id":"grp","memberIds":["a","b"],"prompt":"`+grpPrompt+`","writeLock":"none"},
		{"id":"merge","memberIds":["a","b"],"prompt":"合并任务 FIRST-MERGE QUOTAFAIL HOLDUNTIL:k2","writeLock":"none","dependsOn":["grp"]}]}`)

	// 两分支并行在途后只放行 b：b completed → a 被 superseded 取消。
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return inFlightTask(teamTaskOf(t, detail, "grp-a")) && inFlightTask(teamTaskOf(t, detail, "grp-b"))
	}) {
		t.Fatal("竞速分支未在时限内并行在途")
	}
	releaseAgentTurn(t, env.dataDir, "k1", sessionB)
	if !awaitCond(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return teamTaskOf(t, detail, "grp-b")["status"] == "completed" &&
			teamTaskOf(t, detail, "grp-a")["status"] == "cancelled"
	}) {
		t.Fatal("首个成功未在时限内取消落败分支")
	}
	detail := env.runDetail(t, teamID, runID)
	loser := teamTaskOf(t, detail, "grp-a")
	if reason, _ := loser["stopReason"].(string); reason != team.StopReasonSupersededByFirstSuccess {
		t.Fatalf("落败分支的停止原因应是 superseded: %v", loser["stopReason"])
	}
	// 事件顺序：task_completed(grp-b) 先于 task_stopped(grp-a)。
	events := env.journalEvents(t)
	completedSeq := teamEventSeq(events, team.EventTaskCompleted, "grp-b")
	stoppedSeq := teamEventSeq(events, team.EventTaskStopped, "grp-a")
	if completedSeq < 0 || stoppedSeq < 0 || completedSeq > stoppedSeq {
		t.Fatalf("事件顺序应为 task_completed(grp-b)→task_stopped(grp-a): completed=%d stopped=%d",
			completedSeq, stoppedSeq)
	}
	// 落败分支的 turn 被真取消（interrupted）且账本条目不被重排队。
	if status := teamSessionTurnStatus(env.manager.Get(sessionA), grpPrompt); status != "interrupted" {
		t.Fatalf("落败分支的 turn 应被取消为 interrupted: %q", status)
	}
	loserSubmission, _ := loser["submissionId"].(string)
	entry := env.server.Queue.get(sessionA, loserSubmission)
	if entry == nil || entry.Status != SubAccepted || entry.QueueAttempts != 0 {
		t.Fatalf("落败分支的账本条目应保持 accepted 且不爬梯: %+v", entry)
	}

	// 下游合并：首次派发被 quota 文本停靠（任务 stopped+outcome=quota、Run
	// waiting_input——首个停靠即暂停 Run，另一个在途任务由其 watcher 继续收
	// 尾停靠）；停靠期间 grp 的终态保持不变（收集策略幂等重推导）。
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("quota 停靠未在时限内把 Run 停进 waiting_input")
	}
	if !awaitCond(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return teamTaskOf(t, detail, "merge-a")["status"] == "stopped" &&
			teamTaskOf(t, detail, "merge-b")["status"] == "stopped"
	}) {
		t.Fatal("两个合并任务未在时限内全部 quota 停靠")
	}
	detail = env.runDetail(t, teamID, runID)
	for _, mergeID := range []string{"merge-a", "merge-b"} {
		task := teamTaskOf(t, detail, mergeID)
		if task["status"] != "stopped" || task["outcome"] != team.OutcomeQuota {
			t.Fatalf("合并任务 %s 应 quota 停靠: %v", mergeID, task)
		}
	}
	if task := teamTaskOf(t, detail, "grp-a"); task["status"] != "cancelled" {
		t.Fatalf("停靠期间 superseded 分支应保持 cancelled: %v", task["status"])
	}

	// resume：quota 任务重臂（attempt+1 新 submissionID），一次性 quota 通过后
	// 进入 HOLDUNTIL；只放行 merge-a → merge-b 被 superseded → Run completed。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return inFlightTask(teamTaskOf(t, detail, "merge-a")) && inFlightTask(teamTaskOf(t, detail, "merge-b"))
	}) {
		t.Fatal("重臂的合并任务未在时限内并行在途")
	}
	releaseAgentTurn(t, env.dataDir, "k2", sessionA)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	detail = env.runDetail(t, teamID, runID)
	winner := teamTaskOf(t, detail, "merge-a")
	if winner["status"] != "completed" || winner["result"] == nil {
		t.Fatalf("merge-a 应完成: %v", winner)
	}
	if attempt, _ := winner["attempt"].(float64); attempt != 2 {
		t.Fatalf("quota 重臂后的合并任务 attempt 应为 2: %v", winner["attempt"])
	}
	if task := teamTaskOf(t, detail, "merge-b"); task["status"] != "cancelled" {
		t.Fatalf("merge-b 应被 superseded 取消: %v", task["status"])
	}
	// 合并任务上下文只含胜出上游（grp-b），绝无 superseded 的 grp-a。
	if mergeAt := promptIndex(readTeamPrompts(t, env.dataDir), "FIRST-MERGE"); mergeAt < 0 {
		t.Fatal("合并任务提示词应已派发")
	} else if text := readTeamPrompts(t, env.dataDir)[mergeAt].text; !strings.Contains(text, "来源任务：grp-b") ||
		strings.Contains(text, "来源任务：grp-a") {
		t.Fatalf("合并上下文应只含胜出分支: %q", text)
	}
}

// 用例 2-4：§7 成员间显式转发（任务源 + 会话源）。

// TestTeamIntegration2Forward：任务结果经 [Team context] 信封（来源成员/
// 来源任务/时间行）注入目标成员队列；同 id 重放 duplicate 且零副作用；
// sourceSessionId+scope 走有界转录（团队 MaxContextChars=400 预算截断，事件
// truncated 标记）；信封不带 migrate 头与「继续」尾行。Run 停靠前（t2 长跑
// 在途）执行转发——终态 Run 拒绝转发是另一条独立语义。
func TestTeamIntegration2Forward(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "转发来源会话")
	sessionC := env.createMemberSession(t, "c-sess", "转发目标会话")
	response := env.request(http.MethodPost, "/api/teams", `{"name":"转发团队","policy":{"maxContextChars":400},
		"members":[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"},
		{"id":"c","sessionId":"`+sessionC+`","role":"member"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	teamID := created.ID
	// t1 的长结果（>400 rune）让转发正文必然触发预算截断；t2 长跑拉住 Run
	// 使转发发生在非终态窗口。
	longMaterial := strings.Repeat("转发素材段落。", 60)
	runID := env.createRun(t, teamID, `{"goal":"显式转发","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"产出转发素材 FWD-SRC `+longMaterial+`","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"长跑占位任务 FWD-HOLD HOLDLONG","writeLock":"none","dependsOn":["t1"]}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "t2")["status"] == "running"
	}) {
		t.Fatal("Run 未在时限内推进到长跑任务")
	}

	requestID := uuid.NewString()
	forwardBody := `{"id":"` + requestID + `","sourceTaskId":"t1","targetMemberId":"c","note":"请按转发素材处理 FWD-NOTE"}`
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/forward", forwardBody)
	if response.Code != http.StatusOK {
		t.Fatalf("转发失败: %d %s", response.Code, response.Body.String())
	}
	var forwarded struct {
		SubmissionID string `json:"submissionId"`
		Duplicate    bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.SubmissionID == "" || forwarded.Duplicate {
		t.Fatalf("首次转发应返回新 submissionId: %+v", forwarded)
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return promptIndex(readTeamPrompts(t, env.dataDir), "FWD-NOTE") >= 0
	}) {
		t.Fatal("转发消息未在时限内到达目标成员")
	}
	records := readTeamPrompts(t, env.dataDir)
	forwardAt := promptIndex(records, "FWD-NOTE")
	if records[forwardAt].session != sessionC {
		t.Fatalf("转发应落在目标成员会话: %v", records[forwardAt].session)
	}
	text := records[forwardAt].text
	for _, needle := range []string{"[Team context]", "来源成员：lead", "来源任务：t1",
		"时间：20", "[Assigned task]", "FWD-SRC", "FWD-NOTE"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("转发信封应含 %q: %q", needle, text)
		}
	}
	for _, banned := range []string{"【来自", "请在此基础上继续", "[Migration"} {
		if strings.Contains(text, banned) {
			t.Fatalf("转发信封不应携带 migrate 头/尾行 %q: %q", banned, text)
		}
	}
	// 长结果被 MaxContextChars 预算截断：truncated 事实随 task_forwarded 落账。
	if payload := teamEventPayload(env.journalEvents(t), team.EventTaskForwarded); payload == nil ||
		payload["truncated"] != true {
		t.Fatalf("task_forwarded 应标记 truncated: %v", payload)
	}

	// 同 id 重放：duplicate=true，提示词零增长。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/forward", forwardBody)
	if err := json.Unmarshal(response.Body.Bytes(), &forwarded); err != nil || response.Code != http.StatusOK {
		t.Fatalf("同 id 重放应 duplicate:true: %d %s", response.Code, response.Body.String())
	}
	if !forwarded.Duplicate || forwarded.SubmissionID == "" {
		t.Fatalf("同 id 重放应命中幂等: %+v", forwarded)
	}
	env.server.dispatchQueuedPass()
	if got := len(readTeamPrompts(t, env.dataDir)); got != 3 {
		t.Fatalf("重放不得产生第二条转发（t1+t2+转发共 3 条）: %d", got)
	}

	// 会话源 + scope：来源会话（sessionA）的转录按 main 裁剪。先把团队预算
	// 放宽（PATCH policy）——400 预算会把转录截在第一行内，看不到角色行结构；
	// 有界性已由上方 forward#1 的 truncated 事实覆盖。t1 的问答随 t1 完成早已
	// 落定（t2 在跑是 t1 完成的硬前提），无时序依赖。
	teamView := env.request(http.MethodGet, "/api/teams/"+teamID, "")
	var teamRow struct {
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(teamView.Body.Bytes(), &teamRow); err != nil {
		t.Fatal(err)
	}
	if patched := env.request(http.MethodPatch, "/api/teams/"+teamID,
		fmt.Sprintf(`{"version":%d,"policy":{"maxContextChars":4000}}`, teamRow.Version)); patched.Code != http.StatusOK {
		t.Fatalf("放宽策略失败: %d %s", patched.Code, patched.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/forward",
		`{"id":"`+uuid.NewString()+`","sourceSessionId":"`+sessionA+
			`","scope":"main","targetMemberId":"c","note":"请参考转录处理 FWD-TRANS"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("会话源转发失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return promptIndex(readTeamPrompts(t, env.dataDir), "FWD-TRANS") >= 0
	}) {
		t.Fatal("会话源转发未在时限内到达目标成员")
	}
	records = readTeamPrompts(t, env.dataDir)
	transcriptAt := promptIndex(records, "FWD-TRANS")
	if records[transcriptAt].session != sessionC {
		t.Fatalf("会话源转发应落在目标成员会话: %v", records[transcriptAt].session)
	}
	transcript := records[transcriptAt].text
	for _, needle := range []string{"来源会话：" + sessionA, "用户：", "助手：", "已完成：", "FWD-SRC"} {
		if !strings.Contains(transcript, needle) {
			t.Fatalf("会话源转发应携带角色化转录（含 %q）: %q", needle, transcript)
		}
	}
	for _, banned := range []string{"【来自", "请在此基础上继续", "[Migration"} {
		if strings.Contains(transcript, banned) {
			t.Fatalf("会话源转发不应携带 migrate 头/尾行 %q: %q", banned, transcript)
		}
	}
	// 收尾：取消 Run 结束长跑（取消语义已在阶段一组覆盖，此处只做清理）。
	if cancel := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); cancel.Code != http.StatusOK {
		t.Fatalf("清理取消失败: %d %s", cancel.Code, cancel.Body.String())
	}
}

// 用例 2-5：§6.4 + §16 默认决策 6——worktree 基线、diff 与取消不删用户改动。

// TestTeamIntegration2Worktree：注册 worktree → isolated 任务首派记录
// baseRevision、成员会话开在 worktree、Agent 改已跟踪文件后 GET diff 返回
// unified 文本与双截断标记字段；取消任务后 worktree 目录与用户改动仍在
// （§13.2 条目 7），脏区显式清理被拒（409 + workspace_cleanup_failed）；
// 会话 cwd 不在 worktree 的 isolated 派发确定性失败；重启重载后基线仍在。
func TestTeamIntegration2Worktree(t *testing.T) {
	worktree, baseRevision := newTeamWorktreeFixture(t)
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionWT := env.createMemberSessionIn(t, "wt-sess", worktree, "工作区会话")
	sessionShared := env.createMemberSession(t, "dev2-sess", "共享目录会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"dev","sessionId":"`+sessionWT+
		`","role":"lead"},{"id":"dev2","sessionId":"`+sessionShared+`","role":"member"}]`)
	registerTeamWorkspace(t, env, "wt-alpha", worktree)
	// 同 id 同路径重放幂等。
	response := env.request(http.MethodPost, "/api/teams/workspaces",
		fmt.Sprintf(`{"id":"wt-alpha","path":%q}`, worktree))
	var replayedWorkspace struct {
		ID        string `json:"id"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &replayedWorkspace); err != nil ||
		response.Code != http.StatusOK || !replayedWorkspace.Duplicate || replayedWorkspace.ID != "wt-alpha" {
		t.Fatalf("工作区重放应 duplicate:true: %d %s", response.Code, response.Body.String())
	}

	// 基线与 diff。
	runID := env.createRun(t, teamID, `{"goal":"worktree 基线","mode":"pipeline","plan":[
		{"id":"iso","memberId":"dev","prompt":"写入工作区产出 WT-WRITE-1 WRITEFILE","writeLock":"exclusive","workspaceMode":"isolated_worktree","workspaceId":"wt-alpha"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("isolated 任务未在时限内完成")
	}
	detail := env.runDetail(t, teamID, runID)
	iso := teamTaskOf(t, detail, "iso")
	if revision, _ := iso["baseRevision"].(string); revision != baseRevision {
		t.Fatalf("任务基线应等于首派时 HEAD: %v ≠ %s", iso["baseRevision"], baseRevision)
	}
	if id, _ := iso["workspaceId"].(string); id != "wt-alpha" {
		t.Fatalf("任务应携带 workspaceId: %v", iso["workspaceId"])
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/tasks/iso/diff", "")
	if response.Code != http.StatusOK {
		t.Fatalf("diff 读取失败: %d %s", response.Code, response.Body.String())
	}
	if strings.Count(response.Body.String(), "truncated") < 2 {
		t.Fatalf("diff 响应应携带顶层与文件级两层截断标记: %s", response.Body.String())
	}
	var diff struct {
		WorkspaceID  string `json:"workspaceId"`
		BaseRevision string `json:"baseRevision"`
		Truncated    bool   `json:"truncated"`
		Files        []struct {
			Path      string `json:"path"`
			Diff      string `json:"diff"`
			Added     int    `json:"added"`
			Removed   int    `json:"removed"`
			Truncated bool   `json:"truncated"`
		} `json:"files"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &diff); err != nil {
		t.Fatal(err)
	}
	if diff.BaseRevision != baseRevision || diff.WorkspaceID != "wt-alpha" || diff.Truncated {
		t.Fatalf("diff 应携带基线/工作区且整包未截断: %+v", diff)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "notes.txt" ||
		!strings.Contains(diff.Files[0].Diff, "+++ b/notes.txt") ||
		!strings.Contains(diff.Files[0].Diff, "WT-WRITE-1") || diff.Files[0].Added == 0 {
		t.Fatalf("diff 应含 notes.txt 的 unified 改动: %+v", diff.Files)
	}

	// 取消不删用户改动。
	run2 := env.createRun(t, teamID, `{"goal":"worktree 取消","mode":"pipeline","plan":[
		{"id":"iso2","memberId":"dev","prompt":"长跑写入 WT-CANCEL-2 WRITEFILE HOLDLONG","writeLock":"exclusive","workspaceMode":"isolated_worktree","workspaceId":"wt-alpha"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, run2), "iso2")["status"] == "running"
	}) {
		t.Fatal("iso2 未在时限内进入 running")
	}
	if cancel := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+run2+"/cancel", `{}`); cancel.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", cancel.Code, cancel.Body.String())
	}
	if content, err := os.ReadFile(filepath.Join(worktree, "notes.txt")); err != nil ||
		!strings.Contains(string(content), "WT-CANCEL-2") {
		t.Fatalf("取消后用户改动应仍在: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
		t.Fatalf("取消后 worktree 目录应仍在: %v", err)
	}
	list := env.request(http.MethodGet, "/api/teams/workspaces", "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), worktree) {
		t.Fatalf("注册表应仍列出工作区: %d %s", list.Code, list.Body.String())
	}
	cleanup := env.request(http.MethodPost,
		"/api/teams/"+teamID+"/runs/"+run2+"/tasks/iso2/workspace/cleanup", `{}`)
	if cleanup.Code != http.StatusConflict {
		t.Fatalf("脏 worktree 的显式清理应 409: %d %s", cleanup.Code, cleanup.Body.String())
	}
	if count := teamEventCount(env.journalEvents(t), team.EventWorkspaceCleanupFailed); count != 1 {
		t.Fatalf("清理失败应恰好落一次 workspace_cleanup_failed: %d", count)
	}
	if _, err := os.Stat(filepath.Join(worktree, "notes.txt")); err != nil {
		t.Fatalf("清理失败后用户改动应仍在: %v", err)
	}

	// cwd 不匹配：会话不在 worktree 的成员接 isolated 任务 → 派发确定性拒绝。
	run3 := env.createRun(t, teamID, `{"goal":"cwd 校验","mode":"pipeline","plan":[
		{"id":"iso3","memberId":"dev2","prompt":"错位任务 WT-MISMATCH","writeLock":"exclusive","workspaceMode":"isolated_worktree","workspaceId":"wt-alpha"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, run3)["status"] == "failed"
	}) {
		t.Fatal("cwd 不匹配的 isolated 任务未在时限内确定性失败")
	}
	iso3 := teamTaskOf(t, env.runDetail(t, teamID, run3), "iso3")
	if text, _ := iso3["error"].(string); !strings.Contains(text, "请把会话开在 worktree 中") {
		t.Fatalf("失败原因应是 cwd 不一致: %q", text)
	}
	if promptIndex(readTeamPrompts(t, dataDir), "WT-MISMATCH") >= 0 {
		t.Fatal("错位任务不应到达任何成员会话")
	}

	// 持久化往返：同 dataDir 重启重载后 baseRevision/workspaceId 仍在。
	reopen := reopenTeamIntegrationEnv(t, dataDir)
	reloaded := teamTaskOf(t, reopen.runDetail(t, teamID, runID), "iso")
	if revision, _ := reloaded["baseRevision"].(string); revision != baseRevision {
		t.Fatalf("重启后基线应仍在: %v", reloaded["baseRevision"])
	}
	if id, _ := reloaded["workspaceId"].(string); id != "wt-alpha" {
		t.Fatalf("重启后 workspaceId 应仍在: %v", reloaded["workspaceId"])
	}
}

// 用例 2-6：§6.4.1 验证闸门——退出码裁决，不采信 Agent 口头声明。

// TestTeamIntegration2VerifyGateExitCode：Agent 口头汇报完成（turn 正常
// completed）但 make check 非零 → Run 停 waiting_input、stopReason
// verify_failed、run_verify 事件带退出码、detail.verify 齐；闸门进行中无重复
// 派发；修 Makefile 后 resume → 闸门重跑（第二次 run_verify）→ completed。
// 退出码按本机 make 实测记 2（recipe @exit 7 被 GNU Make 3.81 映射为 2，见
// writeVerifyMakefile 注释）。
func TestTeamIntegration2VerifyGateExitCode(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip(err)
	}
	const failingCode = 2
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "闸门会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	writeVerifyMakefile(t, dataDir, 7)
	runID := env.createRun(t, teamID, `{"goal":"闸门验收","mode":"pipeline","verifyCommand":["make","check"],"plan":[
		{"id":"t1","memberId":"lead","prompt":"闸门任务 VERIFY-GATE 请汇报完成","writeLock":"none"}]}`)

	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("闸门非零未在时限内把 Run 停进 waiting_input")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "completed" {
		t.Fatalf("Agent 已口头完成任务（对照：闸门仍不采信）: %v", task["status"])
	}
	if reason, _ := detail["stopReason"].(string); !strings.HasPrefix(reason, team.StopReasonVerifyFailed) {
		t.Fatalf("停靠原因应为 verify_failed: %q", reason)
	}
	verify, _ := detail["verify"].(map[string]any)
	if verify == nil || verify["exitCode"] != float64(failingCode) {
		t.Fatalf("闸门退出码应为 %d: %v", failingCode, verify)
	}
	if head, _ := verify["outputHead"].(string); !strings.Contains(head, "Error 7") {
		t.Fatalf("闸门输出应含 recipe 的 Error 7 证据: %q", head)
	}
	events := env.journalEvents(t)
	if count := teamEventCount(events, team.EventRunVerify); count != 1 {
		t.Fatalf("闸门应恰好执行一次: %d", count)
	}
	if payload := teamEventPayload(events, team.EventRunVerify); fmt.Sprint(payload["exitCode"]) != fmt.Sprint(failingCode) {
		t.Fatalf("run_verify 事件退出码异常: %v", payload)
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 1 {
		t.Fatalf("闸门期不应有重复派发: %d 条", got)
	}

	// 修复后 resume：闸门重跑（复验事实再次落 run_verify）→ completed。
	writeVerifyMakefile(t, dataDir, 0)
	if resume := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); resume.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", resume.Code, resume.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("修复后 resume 复验未在时限内完成")
	}
	detail = env.runDetail(t, teamID, runID)
	if verify, _ = detail["verify"].(map[string]any); verify == nil || verify["exitCode"] != float64(0) {
		t.Fatalf("复验后闸门退出码应为 0: %v", verify)
	}
	events = env.journalEvents(t)
	if count := teamEventCount(events, team.EventRunVerify); count != 2 {
		t.Fatalf("resume 应重跑闸门（共两次 run_verify）: %d", count)
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 1 {
		t.Fatalf("复验期同样不应有重复派发: %d 条", got)
	}
}

// 用例 2-7：§6.3 AIMD 限流降容与错误策略（§13.2 条目 11）。

// TestTeamIntegration2AIMDErrorPolicy：429 文本 → provider_throttled 一次、
// 桶 window 5→4 且冷却内同桶新任务不被派发；attempt=2 换新 submissionID 重派
// 完成；quota 文本 → 任务 stopped(outcome=quota)+Run waiting_input、桶不变
// （共享桶不冻结——另一 Team 同 kind 任务照常完成）；resume 后 attempt+1 重派。
// poolhooks 不在测试环境装配：quota 停靠与账号池自动切换的任务级交互属生产
// 时序，归 VERIFICATION.md 人工记录，不在此伪造。
func TestTeamIntegration2AIMDErrorPolicy(t *testing.T) {
	env := newTeamIntegrationEnv(t, t.TempDir())
	sessionA := env.createMemberSession(t, "lead-sess", "限流会话")
	sessionB := env.createMemberSession(t, "dev-sess", "冷却会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"dev","sessionId":"`+sessionB+`","role":"member"}]`)

	// 限流：一次性 429 → 桶减窗 + 冷却（Retry-After: 2s）。
	runRL := env.createRun(t, teamID, `{"goal":"限流","mode":"pipeline","plan":[
		{"id":"t-rl","memberId":"lead","prompt":"限流任务 AIMD-RL RATELIMIT","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamEventCount(env.journalEvents(t), team.EventProviderThrottled) == 1
	}) {
		t.Fatal("限流未在时限内压桶（provider_throttled）")
	}
	var firstSubmission string
	for _, event := range env.journalEvents(t) {
		if event.Type != team.EventTaskDispatching {
			continue
		}
		if id, _ := event.Payload["taskId"].(string); id == "t-rl" {
			firstSubmission, _ = event.Payload["submissionId"].(string)
			break // 首条派发事件即 attempt 1。
		}
	}
	if firstSubmission == "" {
		t.Fatal("未取到首次派发的 submissionId")
	}

	// 冷却窗口（约 2s）：新 Run 的同桶任务不被派发，桶快照 window=4 且带
	// cooldownUntil。观察窗口是稳定性检查（阶段一组 700ms 同款），非 sleep 判定。
	runCD := env.createRun(t, teamID, `{"goal":"冷却","mode":"pipeline","plan":[
		{"id":"t-cd","memberId":"dev","prompt":"冷却窗口任务 AIMD-COOL","writeLock":"none"}]}`)
	env.drive(t, 700*time.Millisecond, func() bool { return false })
	records := readTeamPrompts(t, env.dataDir)
	if len(records) != 1 || promptIndex(records, "AIMD-COOL") >= 0 {
		t.Fatalf("冷却内同桶新任务不应派发: %v", records)
	}
	if task := teamTaskOf(t, env.runDetail(t, teamID, runCD), "t-cd"); task["status"] != "planned" {
		t.Fatalf("冷却内同桶任务应保持 planned: %v", task["status"])
	}
	snapshot := env.request(http.MethodGet, "/api/teams/"+teamID+"/aimd", "")
	if snapshot.Code != http.StatusOK {
		t.Fatalf("aimd 快照读取失败: %d %s", snapshot.Code, snapshot.Body.String())
	}
	var aimd struct {
		Buckets []struct {
			Bucket        string `json:"bucket"`
			Window        int    `json:"window"`
			Ceiling       int    `json:"ceiling"`
			CooldownUntil string `json:"cooldownUntil"`
		} `json:"buckets"`
	}
	if err := json.Unmarshal(snapshot.Body.Bytes(), &aimd); err != nil {
		t.Fatal(err)
	}
	cooldownBucket := -1
	for i := range aimd.Buckets {
		if aimd.Buckets[i].Bucket == "kimi:" {
			cooldownBucket = i
		}
	}
	if cooldownBucket < 0 || aimd.Buckets[cooldownBucket].Window != 4 ||
		aimd.Buckets[cooldownBucket].Ceiling != 5 || aimd.Buckets[cooldownBucket].CooldownUntil == "" {
		t.Fatalf("冷却期桶快照异常（window 应 5→4 且带冷却截止）: %+v", aimd.Buckets)
	}

	// 冷却结束：限流任务 attempt=2 重派（新 submissionID）完成，同桶新任务
	// 照常派发完成；epoch 只减一次（provider_throttled 恒为 1）。
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runRL)["status"] == "completed" &&
			env.runDetail(t, teamID, runCD)["status"] == "completed"
	}) {
		t.Fatal("冷却结束后限流重派与同桶任务未在时限内完成")
	}
	task := teamTaskOf(t, env.runDetail(t, teamID, runRL), "t-rl")
	if attempt, _ := task["attempt"].(float64); attempt != 2 {
		t.Fatalf("限流任务应重派为 attempt=2: %v", task["attempt"])
	}
	if submission, _ := task["submissionId"].(string); submission == "" || submission == firstSubmission {
		t.Fatalf("重派应换新 submissionID: %q → %q", firstSubmission, submission)
	}
	if count := teamEventCount(env.journalEvents(t), team.EventProviderThrottled); count != 1 {
		t.Fatalf("provider_throttled 应恰好一次（epoch 内只减一次）: %d", count)
	}

	// quota：任务停靠 + Run 暂停，桶不变（无减窗、无冷却）。
	runQuota := env.createRun(t, teamID, `{"goal":"quota 停靠","mode":"pipeline","plan":[
		{"id":"t-quota","memberId":"lead","prompt":"计费任务 AIMD-QUOTA QUOTAFAIL","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runQuota)["status"] == "waiting_input"
	}) {
		t.Fatal("quota 错误未在时限内停靠 Run")
	}
	parked := teamTaskOf(t, env.runDetail(t, teamID, runQuota), "t-quota")
	if parked["status"] != "stopped" || parked["outcome"] != team.OutcomeQuota {
		t.Fatalf("quota 任务应停靠: %v", parked)
	}
	snapshot = env.request(http.MethodGet, "/api/teams/"+teamID+"/aimd", "")
	aimd = struct {
		Buckets []struct {
			Bucket        string `json:"bucket"`
			Window        int    `json:"window"`
			Ceiling       int    `json:"ceiling"`
			CooldownUntil string `json:"cooldownUntil"`
		} `json:"buckets"`
	}{}
	if err := json.Unmarshal(snapshot.Body.Bytes(), &aimd); err != nil {
		t.Fatal(err)
	}
	var quotaBucket *struct {
		Bucket        string `json:"bucket"`
		Window        int    `json:"window"`
		Ceiling       int    `json:"ceiling"`
		CooldownUntil string `json:"cooldownUntil"`
	}
	for i := range aimd.Buckets {
		if aimd.Buckets[i].Bucket == "kimi:" {
			quotaBucket = &aimd.Buckets[i]
		}
	}
	if quotaBucket == nil || quotaBucket.Window != 4 {
		t.Fatalf("quota 停靠不应冻结共享桶（window 应保持 4）: %+v", aimd.Buckets)
	}
	// cooldownUntil 字段过期后不清零（Admit 只比对时刻），此处断言的是「quota
	// 没有产生新冷却」：限流残留的冷却此刻必已过期（runRL 的重派在冷却结束后
	// 才发生）。
	if quotaBucket.CooldownUntil != "" {
		until, parseErr := time.Parse(time.RFC3339, quotaBucket.CooldownUntil)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if time.Now().Before(until) {
			t.Fatalf("quota 停靠不应产生新的冷却: %+v", quotaBucket)
		}
	}

	// 另一 Team 同 kind 桶不受影响：新团队的同桶任务照常派发完成。
	sessionRev := env.createMemberSession(t, "rev-sess", "另一团队会话")
	team2 := env.createTeamWithMembers(t, `[{"id":"rev","sessionId":"`+sessionRev+`","role":"lead"}]`)
	runOther := env.createRun(t, team2, `{"goal":"共享桶不冻结","mode":"pipeline","plan":[
		{"id":"t-other","memberId":"rev","prompt":"另一团队任务 AIMD-OTHER","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, team2, runOther)["status"] == "completed"
	}) {
		t.Fatal("quota 停靠不应冻结共享桶（另一 Team 的同桶任务应完成）")
	}

	// resume：quota 任务重臂 attempt+1、换新 submissionID，一次性 quota 通过完成。
	quotaSubmission, _ := parked["submissionId"].(string)
	if resume := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runQuota+"/resume", `{}`); resume.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", resume.Code, resume.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runQuota)["status"] == "completed"
	}) {
		t.Fatal("resume 后 quota 任务未在时限内重派完成")
	}
	task = teamTaskOf(t, env.runDetail(t, teamID, runQuota), "t-quota")
	if attempt, _ := task["attempt"].(float64); attempt != 2 {
		t.Fatalf("quota 重臂应 attempt=2: %v", task["attempt"])
	}
	if submission, _ := task["submissionId"].(string); submission == "" || submission == quotaSubmission {
		t.Fatalf("quota 重派应换新 submissionID: %q → %q", quotaSubmission, submission)
	}

	// 连续 4 次成功（t-rl 重派、t-cd、t-other、t-quota 重派）后窗口增长回 5
	//（§6.3 规则 b 的端到端回归：先探回 lastGood）。
	snapshot = env.request(http.MethodGet, "/api/teams/"+teamID+"/aimd", "")
	aimd = struct {
		Buckets []struct {
			Bucket        string `json:"bucket"`
			Window        int    `json:"window"`
			Ceiling       int    `json:"ceiling"`
			CooldownUntil string `json:"cooldownUntil"`
		} `json:"buckets"`
	}{}
	if err := json.Unmarshal(snapshot.Body.Bytes(), &aimd); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range aimd.Buckets {
		if bucket.Bucket == "kimi:" && bucket.Window != 5 {
			t.Fatalf("连续 4 次成功后窗口应增长回 5: %+v", bucket)
		}
	}
}

// 用例 2-8：§15 可靠性 2 + 评审遗留③④——幂等创建与重启收养（阶段二版）。

// teamPhase2RestartFixture 是 writeTeamPhase2RestartState 的产物索引。
type teamPhase2RestartFixture struct {
	teamB string // rb 所属团队（上下文预算已收窄）
	teamC string // rc/rv 所属团队
	runB  string // sentText 在途收养
	runC  string // broadcast+first_success 在途重启
	runV  string // verify_failed 停靠中的 Run
}

// writeTeamPhase2RestartState 手工预置三条 Run 的重启前账本组合（阶段一
// writeTeamRestartState 同款确定性手法）：
//
//   - rb：t1 已完成（长结果），t2 在途且 SentText 按大上下文预算（12000）
//     合成落盘，团队策略却收窄到 160——收养若按当前策略重算 sentText 必然
//     与账本存量文本不一致（matchTurn 漂移 → 误落 outcome_unknown 或永不
//     收口，遗留④的回归防线）；
//   - rc：broadcast + first_success 在途——grp-b 已完成（带 FileSummary），
//     grp-a 在途（账本 queued、SentText 落盘），merge 双成员 planned：重启
//     后收集策略应幂等重推导（取消 grp-a、撤账本条目）并释放下游合并；
//   - rv：verify_failed 停靠中（verifyCommand + Verify 快照）——验证闸门
//     字段跨重启重载。
func writeTeamPhase2RestartState(t *testing.T, dataDir string) *teamPhase2RestartFixture {
	t.Helper()
	now := time.Now()
	fixture := &teamPhase2RestartFixture{
		teamB: "tteam-p2r-b", teamC: "tteam-p2r-c",
		runB: "trun-p2r-b", runC: "trun-p2r-c", runV: "trun-p2r-v",
	}
	const sessionLead, sessionPeer, sessionRace = "km-team-1", "km-team-2", "km-team-3"

	// rb：t1 长结果 + t2 在途（SentText 按大预算合成）。
	policyFull := team.DefaultPolicy()
	policyNarrow := team.DefaultPolicy()
	policyNarrow.MaxContextChars = 160
	upstream := "已完成：P2R 上游结果。" + strings.Repeat("细节段落。", 80)
	runB := &team.Run{ID: fixture.runB, TeamID: fixture.teamB, Goal: "sentText 收养",
		Mode: team.ModePipeline, Status: team.RunDispatching, Round: 1, PlanHash: "hand-p2r-b",
		CreatedAt: now, UpdatedAt: now}
	runB.Tasks = []team.Task{
		{ID: "t1", MemberID: "lead", Prompt: "上游任务 P2R-T1", Attempt: 1, Ordinal: 0,
			Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone,
			Result: upstream,
			ResultDetail: &team.TaskResult{SessionID: sessionLead, TurnID: "turn-p2r-1",
				Result: upstream, ResultChars: len([]rune(upstream))}},
		{ID: "t2", MemberID: "lead", Prompt: "下游任务 P2R-T2 等待收养",
			DependsOn: []string{"t1"}, Attempt: 1, Ordinal: 1,
			Status: team.TaskDispatching, WriteLock: team.TaskWriteLockNone},
	}
	context := team.BuildTaskContext(runB, &runB.Tasks[1], policyFull)
	if context.Text == "" {
		t.Fatal("夹具异常：上游结果未进入上下文")
	}
	sentText := team.ComposeTaskMessage(context.Text, runB.Tasks[1].Prompt)
	recomputed := team.ComposeTaskMessage(
		team.BuildTaskContext(runB, &runB.Tasks[1], policyNarrow).Text, runB.Tasks[1].Prompt)
	if sentText == recomputed {
		t.Fatal("夹具异常：收窄预算下重算文本应与存量 SentText 不同")
	}
	runB.Tasks[1].SentText = sentText
	runB.Tasks[1].InputHash = team.InputHashFor(&runB.Tasks[1], team.PolicyDigest(policyFull), context.Text)
	runB.Tasks[1].SubmissionID = team.TaskSubmissionID(fixture.teamB, fixture.runB, "t2", 1)

	// rc：broadcast + first_success 在途。
	const mergePrompt = "合并任务 P2R-MERGE HOLDUNTIL:k2"
	runC := &team.Run{ID: fixture.runC, TeamID: fixture.teamC, Goal: "广播重启",
		Mode: team.ModeBroadcast, CollectPolicy: team.RunCollectFirstSuccess,
		Status: team.RunDispatching, Round: 1, PlanHash: "hand-p2r-c",
		CreatedAt: now, UpdatedAt: now}
	peerResult := "已完成：P2R-GRB 广播重启胜出结果"
	grpA := team.Task{ID: "grp-a", MemberID: "a", Prompt: "竞速分支甲 P2R-GRP-A", Attempt: 1,
		Ordinal: 0, Status: team.TaskDispatching, BroadcastGroup: "grp", WriteLock: team.TaskWriteLockNone}
	grpA.SentText = grpA.Prompt
	grpA.SubmissionID = team.TaskSubmissionID(fixture.teamC, fixture.runC, "grp-a", 1)
	grpA.InputHash = "hand-hash-grpa"
	runC.Tasks = []team.Task{
		grpA,
		{ID: "grp-b", MemberID: "b", Prompt: "竞速分支乙 P2R-GRP-B", Ordinal: 1,
			Status: team.TaskCompleted, BroadcastGroup: "grp", WriteLock: team.TaskWriteLockNone,
			Result: peerResult,
			ResultDetail: &team.TaskResult{SessionID: sessionPeer, TurnID: "turn-p2r-2",
				Result: peerResult, ResultChars: len([]rune(peerResult)), FileSummary: "rt-file.txt"}},
		{ID: "merge-a", MemberID: "a", Prompt: mergePrompt,
			DependsOn: []string{"grp-a", "grp-b"}, Ordinal: 2, Status: team.TaskPlanned,
			BroadcastGroup: "merge", WriteLock: team.TaskWriteLockNone},
		{ID: "merge-b", MemberID: "b", Prompt: mergePrompt,
			DependsOn: []string{"grp-a", "grp-b"}, Ordinal: 3, Status: team.TaskPlanned,
			BroadcastGroup: "merge", WriteLock: team.TaskWriteLockNone},
	}

	// rv：verify_failed 停靠中。
	runV := &team.Run{ID: fixture.runV, TeamID: fixture.teamC, Goal: "闸门停靠",
		Mode: team.ModeSingle, Status: team.RunWaitingInput, Round: 1, PlanHash: "hand-p2r-v",
		StopReason:    "verify_failed（退出码 7）",
		VerifyCommand: []string{"make", "check"},
		Verify:        &team.VerifyResult{ExitCode: 7, OutputHead: "make: *** [check] Error 7", FinishedAt: now},
		CreatedAt:     now, UpdatedAt: now}
	vResult := "已完成：P2R-V"
	runV.Tasks = []team.Task{
		{ID: "t1", MemberID: "a", Prompt: "闸门任务 P2R-V", Attempt: 1, Ordinal: 0,
			Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone, Result: vResult,
			ResultDetail: &team.TaskResult{SessionID: sessionLead, TurnID: "turn-p2r-3",
				Result: vResult, ResultChars: len([]rune(vResult))}},
	}

	// 团队：rb 团队策略收窄；rc/rv 共用默认策略团队。
	teamBRow := &team.Team{ID: fixture.teamB, Name: "sentText 团队",
		Members: []team.TeamMember{{ID: "lead", SessionID: sessionLead, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
		LeadID:  "lead", Policy: policyNarrow, CreatedAt: now, UpdatedAt: now}
	teamCRow := &team.Team{ID: fixture.teamC, Name: "广播团队",
		Members: []team.TeamMember{
			{ID: "a", SessionID: sessionRace, AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
			{ID: "b", SessionID: sessionPeer, AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
		},
		LeadID: "a", Policy: team.DefaultPolicy(), CreatedAt: now, UpdatedAt: now}
	teamRaw, err := json.Marshal(map[string]*team.Team{fixture.teamB: teamBRow, fixture.teamC: teamCRow})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), teamRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	runsRaw, err := json.Marshal(map[string]*team.Run{fixture.runB: runB, fixture.runC: runC, fixture.runV: runV})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), runsRaw, 0o600); err != nil {
		t.Fatal(err)
	}

	// journal：序号跨 Run 全局单调（1 起）。
	events := []*team.Event{
		{ID: uuid.NewString(), RunID: fixture.runB, Type: team.EventRunCreated, Source: "user",
			Payload:  map[string]any{"teamId": fixture.teamB, "goal": runB.Goal, "mode": team.ModePipeline, "planHash": "hand-p2r-b", "taskCount": 2},
			Sequence: 1, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runB, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "t1", "memberId": "lead"}, Sequence: 2, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runB, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "t2", "memberId": "lead"}, Sequence: 3, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runB, Type: team.EventTaskCompleted, Source: "watcher",
			Payload:  map[string]any{"taskId": "t1", "sessionId": sessionLead, "turnId": "turn-p2r-1", "result": upstream, "resultChars": len([]rune(upstream)), "truncated": false},
			Sequence: 4, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runB, Type: team.EventTaskDispatching, Source: "runner",
			Payload:  map[string]any{"taskId": "t2", "sessionId": sessionLead, "submissionId": runB.Tasks[1].SubmissionID, "inputHash": runB.Tasks[1].InputHash, "attempt": 1},
			Sequence: 5, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventRunCreated, Source: "user",
			Payload:  map[string]any{"teamId": fixture.teamC, "goal": runC.Goal, "mode": team.ModeBroadcast, "collectPolicy": team.RunCollectFirstSuccess, "planHash": "hand-p2r-c", "taskCount": 4},
			Sequence: 6, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "grp-a", "memberId": "a"}, Sequence: 7, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "grp-b", "memberId": "b"}, Sequence: 8, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "merge-a", "memberId": "a"}, Sequence: 9, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "merge-b", "memberId": "b"}, Sequence: 10, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskDispatching, Source: "runner",
			Payload:  map[string]any{"taskId": "grp-a", "sessionId": sessionRace, "submissionId": grpA.SubmissionID, "inputHash": grpA.InputHash, "attempt": 1},
			Sequence: 11, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runC, Type: team.EventTaskCompleted, Source: "watcher",
			Payload:  map[string]any{"taskId": "grp-b", "sessionId": sessionPeer, "turnId": "turn-p2r-2", "result": peerResult, "resultChars": len([]rune(peerResult)), "truncated": false, "fileSummary": "rt-file.txt"},
			Sequence: 12, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runV, Type: team.EventRunCreated, Source: "user",
			Payload:  map[string]any{"teamId": fixture.teamC, "goal": runV.Goal, "mode": team.ModeSingle, "planHash": "hand-p2r-v", "taskCount": 1, "verifyCommand": []string{"make", "check"}},
			Sequence: 13, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runV, Type: team.EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "t1", "memberId": "a"}, Sequence: 14, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runV, Type: team.EventTaskCompleted, Source: "watcher",
			Payload:  map[string]any{"taskId": "t1", "sessionId": sessionRace, "turnId": "turn-p2r-3", "result": vResult, "resultChars": len([]rune(vResult)), "truncated": false},
			Sequence: 15, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runV, Type: team.EventRunVerify, Source: "runner",
			Payload:  map[string]any{"exitCode": 7, "durationMs": 12, "outputHead": "make: *** [check] Error 7", "truncated": false},
			Sequence: 16, CreatedAt: now},
		{ID: uuid.NewString(), RunID: fixture.runV, Type: team.EventRunWaitingInput, Source: "runner",
			Payload: map[string]any{"reason": "verify_failed（退出码 7）"}, Sequence: 17, CreatedAt: now},
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

	// 账本：rb t2 与 rc grp-a 的在途条目（queued——重启后交既有 worker 派发；
	// grp-a 的条目会被收集策略撤销）。
	ledger := map[string]*submissionEntry{
		sessionLead + ":" + runB.Tasks[1].SubmissionID: {
			ID: runB.Tasks[1].SubmissionID, ThreadID: sessionLead, Host: "local",
			Text: sentText, Mode: "queue", Status: SubQueued,
			CreatedAt: float64(time.Now().UnixMilli()) / 1000},
		sessionRace + ":" + grpA.SubmissionID: {
			ID: grpA.SubmissionID, ThreadID: sessionRace, Host: "local",
			Text: grpA.Prompt, Mode: "queue", Status: SubQueued,
			CreatedAt: float64(time.Now().UnixMilli()) / 1000},
	}
	ledgerRaw, marshalErr := json.Marshal(ledger)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "submissions.json"), ledgerRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	// 会话登记：重启后的新 Manager 冷 List 探测归属与 cwd 用。
	if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
		[]byte(sessionLead+"\x00"+dataDir+"\n"+sessionPeer+"\x00"+dataDir+"\n"+
			sessionRace+"\x00"+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestTeamIntegration2IdempotencyAndRestart：同 id 同内容 POST 两次 → 一个
// Run（duplicate:true）、异内容 400；带 sentText 的在途任务重启后收养用存量
// 文本匹配 turn（策略已收窄不错认）；broadcast 在途重启 → collect 策略跨重启
// 保持、first_success 兄弟取消幂等重推导；含 mode/collectPolicy/verify/
// sentText/broadcastGroup/fileSummary 的 Run 重启重载后字段仍在。
func TestTeamIntegration2IdempotencyAndRestart(t *testing.T) {
	t.Run("同 id 创建 Run 幂等", func(t *testing.T) {
		env := newTeamIntegrationEnv(t, t.TempDir())
		sessionA := env.createMemberSession(t, "lead-sess", "幂等会话")
		teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
		requestID := uuid.NewString()
		body := `{"id":"` + requestID + `","goal":"幂等创建","mode":"pipeline","plan":[
			{"id":"t1","memberId":"lead","prompt":"幂等任务 IDEM2 只建一个","writeLock":"none"}]}`
		first := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", body)
		if first.Code != http.StatusOK {
			t.Fatalf("创建失败: %d %s", first.Code, first.Body.String())
		}
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, teamID, requestID)["status"] == "completed"
		}) {
			t.Fatal("Run 未在时限内完成")
		}
		baseline := len(readTeamPrompts(t, env.dataDir))
		second := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", body)
		var replayed struct {
			ID        string `json:"id"`
			Duplicate bool   `json:"duplicate"`
		}
		if err := json.Unmarshal(second.Body.Bytes(), &replayed); err != nil || second.Code != http.StatusOK {
			t.Fatalf("同 id 重放应返回已建 Run + duplicate:true: %d %s", second.Code, second.Body.String())
		}
		if !replayed.Duplicate || replayed.ID != requestID {
			t.Fatalf("同 id 重放应命中幂等: %+v", replayed)
		}
		var runList struct {
			Runs []map[string]any `json:"runs"`
		}
		list := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs", "")
		if err := json.Unmarshal(list.Body.Bytes(), &runList); err != nil {
			t.Fatal(err)
		}
		if len(runList.Runs) != 1 || runList.Runs[0]["id"] != requestID {
			t.Fatalf("网络重试不应产生第二个 Run: %s", list.Body.String())
		}
		env.server.dispatchQueuedPass()
		if got := len(readTeamPrompts(t, env.dataDir)); got != baseline {
			t.Fatalf("重放不得重派任务: %d ≠ %d", got, baseline)
		}
		mutated := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
			`{"id":"`+requestID+`","goal":"幂等创建","mode":"pipeline","plan":[
			{"id":"t1","memberId":"lead","prompt":"不同内容 IDEM2-OTHER","writeLock":"none"}]}`)
		if mutated.Code != http.StatusBadRequest {
			t.Fatalf("同 id 异内容应 400: %d %s", mutated.Code, mutated.Body.String())
		}
	})

	t.Run("sentText 收养与广播策略跨重启", func(t *testing.T) {
		dataDir := t.TempDir()
		fixture := writeTeamPhase2RestartState(t, dataDir)
		env := reopenTeamIntegrationEnv(t, dataDir)
		// 先等调度循环自身完成收集策略重推导（grp-a 被 superseded 取消、其
		// 账本条目撤队）——此间不做派发巡检，防把 grp-a 的存量条目推给 Agent。
		if !awaitCond(t, teamIntegrationWait, func() bool {
			return teamTaskOf(t, env.runDetail(t, fixture.teamC, fixture.runC), "grp-a")["status"] == "cancelled"
		}) {
			t.Fatal("重启后 first_success 未在时限内幂等重推导（取消在途落败分支）")
		}
		detail := env.runDetail(t, fixture.teamC, fixture.runC)
		loser := teamTaskOf(t, detail, "grp-a")
		if reason, _ := loser["stopReason"].(string); reason != team.StopReasonSupersededByFirstSuccess {
			t.Fatalf("取消原因应为 superseded: %v", loser["stopReason"])
		}
		// rb 的在途任务收养：驱动派发后按存量 SentText 匹配 turn 完成入账
		//（策略已收窄——若按当前策略重算必然漂移，任务永不收口）。
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, fixture.teamB, fixture.runB)["status"] == "completed"
		}) {
			t.Fatal("sentText 收养未在时限内按存量文本完成")
		}
		detailB := env.runDetail(t, fixture.teamB, fixture.runB)
		if task := teamTaskOf(t, detailB, "t2"); task["status"] != "completed" || task["result"] == nil {
			t.Fatalf("在途任务应按存量 SentText 完成入账（而非 outcome_unknown）: %v", task)
		}
		// 下游合并组释放：merge 双任务派发并 HOLDUNTIL，只放行 merge-a。
		if !env.drive(t, teamIntegrationWait, func() bool {
			detail := env.runDetail(t, fixture.teamC, fixture.runC)
			return inFlightTask(teamTaskOf(t, detail, "merge-a")) && inFlightTask(teamTaskOf(t, detail, "merge-b"))
		}) {
			t.Fatal("合并任务未在时限内派发")
		}
		records := readTeamPrompts(t, dataDir)
		mergeAt := promptIndex(records, "P2R-MERGE")
		if mergeAt < 0 {
			t.Fatal("合并任务提示词应已派发")
		}
		if text := records[mergeAt].text; !strings.Contains(text, "来源任务：grp-b") ||
			!strings.Contains(text, "变更文件：rt-file.txt") {
			t.Fatalf("合并上下文应只含胜出分支（含 FileSummary 摘要行）: %q", text)
		}
		if promptIndex(records, "P2R-GRP-A") >= 0 {
			t.Fatal("被 superseded 的在途分支不得再派发")
		}
		releaseAgentTurn(t, dataDir, "k2", "km-team-3")
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, fixture.teamC, fixture.runC)["status"] == "completed"
		}) {
			t.Fatal("重启后的广播 Run 未在时限内完成")
		}
		detail = env.runDetail(t, fixture.teamC, fixture.runC)
		if task := teamTaskOf(t, detail, "merge-b"); task["status"] != "cancelled" {
			t.Fatalf("合并组落败分支应被 superseded: %v", task["status"])
		}
		// 持久化往返（评审验收）：mode/collectPolicy/verify 走 API 投影，
		// sentText/broadcastGroup/fileSummary 不进 API 投影，从 store 直读。
		if reloaded := env.store.Run(fixture.runC); reloaded == nil ||
			reloaded.Mode != team.ModeBroadcast ||
			reloaded.CollectPolicy != team.RunCollectFirstSuccess {
			t.Fatalf("mode/collectPolicy 应跨重启重载: %+v", reloaded)
		} else {
			for _, task := range reloaded.Tasks {
				if task.BroadcastGroup == "" {
					t.Fatalf("广播组字段应跨重启重载: %+v", task)
				}
			}
			for _, task := range reloaded.Tasks {
				if task.ID == "grp-b" &&
					(task.ResultDetail == nil || task.ResultDetail.FileSummary != "rt-file.txt") {
					t.Fatalf("fileSummary 应跨重启重载: %+v", task.ResultDetail)
				}
			}
		}
		if reloaded := env.store.Run(fixture.runB); reloaded == nil {
			t.Fatal("runB 应可重载")
		} else {
			sentKept := false
			for _, task := range reloaded.Tasks {
				if task.ID == "t2" && task.SentText != "" {
					sentKept = true
				}
			}
			if !sentKept {
				t.Fatal("sentText 应跨重启重载（遗留④的持久化往返）")
			}
		}
		verifyDetail := env.runDetail(t, fixture.teamC, fixture.runV)
		if code, _ := verifyDetail["verify"].(map[string]any); code == nil || code["exitCode"] != float64(7) {
			t.Fatalf("verify 快照应跨重启重载: %v", verifyDetail["verify"])
		}
		if command, _ := verifyDetail["verifyCommand"].([]any); len(command) != 2 ||
			command[0] != "make" || command[1] != "check" {
			t.Fatalf("verifyCommand 应跨重启重载: %v", verifyDetail["verifyCommand"])
		}
	})
}

// 用例 2-9：§10.3 事件流——阶段二事件行与 meta。

// TestTeamIntegration2EventStreamPhase2Rows：一条同时踩中 worktree（
// workspace_bound 行）、验证闸门（run_verify 行）与转发（task_forwarded 行）
// 的 Run——changes reset 页三行齐、meta 含 mode 与 verify 摘要；修复闸门后
// resume，增量帧携带第二次 run_verify（exitCode=0）与 run_completed。
func TestTeamIntegration2EventStreamPhase2Rows(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip(err)
	}
	worktree, _ := newTeamWorktreeFixture(t)
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionC := env.createMemberSession(t, "c-sess", "闸门负责人") // lead：Makefile 落在其 cwd
	sessionWT := env.createMemberSessionIn(t, "wt-sess", worktree, "工作区会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"c","sessionId":"`+sessionC+
		`","role":"lead"},{"id":"wt","sessionId":"`+sessionWT+`","role":"member"}]`)
	registerTeamWorkspace(t, env, "wt-gate", worktree)
	writeVerifyMakefile(t, dataDir, 7)
	runID := env.createRun(t, teamID, `{"goal":"事件流阶段二","mode":"pipeline","verifyCommand":["make","check"],"plan":[
		{"id":"iso","memberId":"wt","prompt":"事件流工作区任务 GATE-WT WRITEFILE","writeLock":"exclusive","workspaceMode":"isolated_worktree","workspaceId":"wt-gate"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("闸门非零未在时限内停靠")
	}
	// Run 停靠（waiting_input 非终态）时转发仍可用：task_forwarded 行入投影。
	forward := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/forward",
		`{"id":"`+uuid.NewString()+`","sourceTaskId":"iso","targetMemberId":"c","note":"事件流转发 GATE-FWD"}`)
	if forward.Code != http.StatusOK {
		t.Fatalf("停靠期转发失败: %d %s", forward.Code, forward.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return promptIndex(readTeamPrompts(t, dataDir), "GATE-FWD") >= 0
	}) {
		t.Fatal("转发消息未在时限内到达")
	}

	// reset 整页：阶段二事件行齐，meta 含 mode 与 verify 摘要。
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
	}
	page := map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	rowTypes := map[string]bool{}
	rows, _ := page["rows"].([]any)
	for _, row := range rows {
		if object, ok := row.(map[string]any); ok {
			rowTypes[object["type"].(string)] = true
		}
	}
	for _, want := range []string{team.EventWorkspaceBound, team.EventRunVerify, team.EventTaskForwarded} {
		if !rowTypes[want] {
			t.Fatalf("changes 行缺 %s（现有行型 %v）", want, rowTypes)
		}
	}
	meta, _ := page["meta"].(map[string]any)
	if meta == nil || meta["mode"] != "pipeline" {
		t.Fatalf("meta 应含 mode: %v", meta)
	}
	verify, _ := meta["verify"].(map[string]any)
	if verify == nil || verify["exitCode"] != float64(2) || verify["verifying"] != false {
		t.Fatalf("meta.verify 应含停靠退出码: %v", verify)
	}
	sequence, _ := page["sequence"].(float64)
	epoch, _ := page["epoch"].(string)

	// 修复闸门 → resume：增量帧携带第二次 run_verify（exitCode=0）与
	// run_completed，meta 状态翻 completed。
	writeVerifyMakefile(t, dataDir, 0)
	if resume := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); resume.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", resume.Code, resume.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("修复后 resume 复验未在时限内完成")
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+
		"/changes?after="+strconv.FormatInt(int64(sequence), 10)+"&epoch="+epoch, "")
	if response.Code != http.StatusOK {
		t.Fatalf("增量读取应 200: %d %s", response.Code, response.Body.String())
	}
	page = map[string]any{}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	sawVerifyPass, sawCompleted := false, false
	rows, _ = page["rows"].([]any)
	for _, row := range rows {
		object, ok := row.(map[string]any)
		if !ok {
			continue
		}
		switch object["type"] {
		case team.EventRunVerify:
			if payload, isMap := object["payload"].(map[string]any); isMap &&
				fmt.Sprint(payload["exitCode"]) == "0" {
				sawVerifyPass = true
			}
		case team.EventRunCompleted:
			sawCompleted = true
		}
	}
	if !sawVerifyPass || !sawCompleted {
		t.Fatalf("增量帧应携带复验 run_verify(0) 与 run_completed: verify=%v completed=%v", sawVerifyPass, sawCompleted)
	}
	if meta, _ = page["meta"].(map[string]any); meta == nil || meta["status"] != "completed" {
		t.Fatalf("增量帧 meta 应翻 completed: %v", meta)
	}
}

// ---- 阶段四组（TestTeamIntegration4*）：§12 阶段四验收 / §13.2 对应行 ----
//
// 覆盖计划阶段四验收三条与配套工作项的端到端：定时触发全链路与崩溃窗口
// 幂等（fixture 预置 team-schedules.json 绕开 PUT 的 intervalSecs≥300 下限
// ——评审缺失项③定案，API 层下限由 teams_test 的校验用例钉住）、定时 Run
// 重启恢复、结果未知经 reconciler 周期/重启/refresh 三面绝不重发、
// waiting_input 手机恢复（通知落账+resume 续跑）、pause/resume 全语义
// （收尾/冻结/重启保持/journal 重建/停靠升级）、Run 通知四类载荷与水位
// 补发退避、stall 只通知不停止（SetStallAfter 缩线）、预算停靠与重启
// ceiling 不漂移、stats 数字与 coverage=none、阶段图形状。选择器约定：
// -run 'TestTeamIntegration4'（与阶段一/二组分开跑，-race 下避免时限压力
// 叠加）；等待全部 deadline 轮询（120ms 节拍/20s 超时），不用固定 sleep
// 判正确性（§13.1）。

// writeTeamScheduleFixture 直写 team-schedules.json（集成测试旁路：fixture
// 不经 ValidateSchedule，intervalSecs 可小于生产下限 300——MinScheduleInterval
// Secs 只挡 HTTP 配置面）。mutate 允许覆盖 NextDue/PendingTriggerID 等
// 簿记字段构造崩溃窗口形态。
func writeTeamScheduleFixture(t *testing.T, dataDir, teamID string, mutate func(schedule *team.TeamSchedule)) {
	t.Helper()
	schedule := &team.TeamSchedule{TeamID: teamID, Enabled: true, IntervalSecs: 600}
	if mutate != nil {
		mutate(schedule)
	}
	raw, err := json.Marshal(map[string]*team.TeamSchedule{teamID: schedule})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "team-schedules.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// attachScheduler 照 main.go:966-978 装配定时触发器（ScheduleStore 与
// /schedule 端点共用同一实例；CreateRun 恒传新 runID、autoPlan=false、
// scheduledFrom=triggerID）。tick 经导出字段缩到秒级（env
// GATEWAY_TEAM_SCHEDULE_TICK_SECONDS 是进程级全局、测试包内互串，不取）；
// 返回的触发器已 Start——Start 首轮 sweep 即含 PendingTriggerID 回扫。
func (e *teamIntegrationEnv) attachScheduler(t *testing.T) *team.Scheduler {
	t.Helper()
	schedules := team.LoadScheduleStore(filepath.Join(e.dataDir, "team-schedules.json"))
	e.server.TeamSchedules = schedules
	scheduler := team.NewScheduler(schedules, e.store, e.journal,
		func(scheduledTeam *team.Team, goal, mode, collect string, verifyCommand []string, plan []team.PlanTaskInput, scheduledFrom string) (*team.Run, error) {
			return e.server.Teams.CreateRun(scheduledTeam, "", goal, mode, collect, verifyCommand, plan, false, scheduledFrom)
		})
	scheduler.Tick = time.Second
	t.Cleanup(scheduler.Stop)
	scheduler.Start()
	return scheduler
}

// teamScheduleView 读取 GET /api/teams/:id/schedule 的配置投影。
func (e *teamIntegrationEnv) teamScheduleView(t *testing.T, teamID string) map[string]any {
	t.Helper()
	response := e.request(http.MethodGet, "/api/teams/"+teamID+"/schedule", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 schedule 失败: %d %s", response.Code, response.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// teamRunsList 返回团队的 Run 概要列表。
func (e *teamIntegrationEnv) teamRunsList(t *testing.T, teamID string) []map[string]any {
	t.Helper()
	response := e.request(http.MethodGet, "/api/teams/"+teamID+"/runs", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 Run 列表失败: %d %s", response.Code, response.Body.String())
	}
	var list struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	return list.Runs
}

// teamRunStats 读取 GET /stats 的统计投影。
func (e *teamIntegrationEnv) teamRunStats(t *testing.T, teamID, runID string) map[string]any {
	t.Helper()
	response := e.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/stats", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 stats 失败: %d %s", response.Code, response.Body.String())
	}
	var stats map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	return stats
}

// teamRunGraph 读取 GET /graph 的阶段图投影。
func (e *teamIntegrationEnv) teamRunGraph(t *testing.T, teamID, runID string) map[string]any {
	t.Helper()
	response := e.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/graph", "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 graph 失败: %d %s", response.Code, response.Body.String())
	}
	var graph map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	return graph
}

// ---- 伪 ntfy 渠道与 Run 通知装配（main.go:938-962 的同构测试面）----

// fakeNtfyPost 是 ntfy 渠道的一条发送载荷（publish.go publishNtfy 的落点）。
type fakeNtfyPost struct {
	Topic   string `json:"topic"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Click   string `json:"click"`
}

// fakeNtfy 是可关停的伪 ntfy 服务：捕获全部 POST 载荷；failing 置真时回
// 500 模拟服务不可达（杀 server / 复活由测试翻转，无需重建服务器——
// URL 不变才能跨「重启」复用送达痕迹）。
type fakeNtfy struct {
	url string

	mu      sync.Mutex
	posts   []fakeNtfyPost
	failing bool
}

func startFakeNtfy(t *testing.T) *fakeNtfy {
	t.Helper()
	fake := &fakeNtfy{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var post fakeNtfyPost
		_ = json.Unmarshal(raw, &post)
		fake.mu.Lock()
		fake.posts = append(fake.posts, post)
		failing := fake.failing
		fake.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	fake.url = server.URL
	return fake
}

func (f *fakeNtfy) setFailing(failing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failing = failing
}

func (f *fakeNtfy) all() []fakeNtfyPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeNtfyPost(nil), f.posts...)
}

// findByTitle 返回标题包含 want 的全部载荷（按 Run goal 区分通知归属）。
func (f *fakeNtfy) findByTitle(want string) []fakeNtfyPost {
	var matched []fakeNtfyPost
	for _, post := range f.all() {
		if strings.Contains(post.Title, want) {
			matched = append(matched, post)
		}
	}
	return matched
}

// writeNotificationsSettings 预置 {dataDir}/notifications.json：启用 ntfy
// 渠道指向伪服务、clickBase 固定值（click deep link 断言载体）。
func writeNotificationsSettings(t *testing.T, dataDir, serverURL string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"enabled": true, "server": serverURL, "topic": "bridgetest", "token": "",
		"barkEnabled": false, "barkServer": "https://api.day.app", "barkKey": "",
		"clickBase": "https://bridge.example", "includeTitle": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "notifications.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// notifyKindLog 是 OnRunStatus 回调的测试侧观察面（回调在 Run 的 state.mu
// 下同步执行，只做记录；commitRun 先落快照后回调，故等待以本日志为准而
// 非轮询 run 状态——状态可见可能早于回调）。
type notifyKindLog struct {
	mu    sync.Mutex
	kinds []string
}

func (l *notifyKindLog) record(runID, kind string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.kinds = append(l.kinds, runID+"/"+kind)
}

func (l *notifyKindLog) has(runID, kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, entry := range l.kinds {
		if entry == runID+"/"+kind {
			return true
		}
	}
	return false
}

func (l *notifyKindLog) count(runID, kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, entry := range l.kinds {
		if entry == runID+"/"+kind {
			count++
		}
	}
	return count
}

// attachRunNotifications 照 main.go:941-944 + recordTeamRunNotification 装配
// OnRunStatus→账本（标题正文经 team.RunNotificationText 组装）；发送与退避
// 由测试驱动 ledger.Reconcile（不 StartWorker，时间轴完全可控）。
func (e *teamIntegrationEnv) attachRunNotifications(t *testing.T, ledger *notifications.RunNotificationLedger) *notifyKindLog {
	t.Helper()
	log := &notifyKindLog{}
	e.server.Teams.OnRunStatus = func(runID, kind string) {
		log.record(runID, kind)
		run := e.store.Run(runID)
		if run == nil {
			return
		}
		title, body := team.RunNotificationText(run, kind)
		if title == "" {
			return
		}
		if err := ledger.Record(runID, run.TeamID, kind, title, body, time.Now()); err != nil {
			t.Logf("Run 通知记账失败: %v", err)
		}
	}
	return log
}

// teamBackfillCandidates 是 main.go backfillTeamRunNotifications 的测试侧
// 镜像：把 store 快照里的可通知 Run 投影成补发候选（水位窗口裁决归
// ledger.Backfill）。
func teamBackfillCandidates(e *teamIntegrationEnv) []notifications.RunBackfillCandidate {
	candidates := []notifications.RunBackfillCandidate{}
	for _, run := range e.store.Runs() {
		kind := team.RunNotificationKindOfStatus(run.Status)
		if kind == "" {
			continue
		}
		title, body := team.RunNotificationText(run, kind)
		if title == "" {
			continue
		}
		candidates = append(candidates, notifications.RunBackfillCandidate{
			RunID: run.ID, TeamID: run.TeamID, Kind: kind,
			UpdatedAt: run.UpdatedAt, Title: title, Body: body,
		})
	}
	return candidates
}

// ---- 用例 4-1：§12 阶段四验收行 1 前置——定时触发全链路 ----

// TestTeamIntegration4ScheduledTriggerEndToEnd：fixture 预置 NextDue=过去
// 时刻的 schedule → 装配触发器即在首轮 sweep 到期 → 自动建 Run 跑完。
// 断言溯源双载体一致（RunDetail.scheduledFrom == run_created
// payload.scheduledBy）、账面收尾（schedule.lastRunId、pendingTriggerId
// 清空）、稳定性窗口内不二发（intervalSecs=600，NextDue 已推进）。
func TestTeamIntegration4ScheduledTriggerEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "定时会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"}]`)
	writeTeamScheduleFixture(t, dataDir, teamID, func(schedule *team.TeamSchedule) {
		schedule.Goal = "定时验收目标 SCHED-E2E"
		schedule.Plan = []team.PlanTaskInput{
			{ID: "st1", MemberID: "lead", Prompt: "定时任务 SCHED-E2E 执行", WriteLock: team.TaskWriteLockNone},
		}
		schedule.NextDue = time.Now().Add(-time.Minute)
	})
	env.attachScheduler(t)

	// 首轮 sweep 即到期：deadline 轮询等 Run 出现（不经 HTTP 创建）。
	if !awaitCond(t, teamIntegrationWait, func() bool {
		return len(env.teamRunsList(t, teamID)) == 1
	}) {
		t.Fatal("定时触发未在时限内创建 Run")
	}
	runID, _ := env.teamRunsList(t, teamID)[0]["id"].(string)
	drive := env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	})
	if !drive {
		t.Fatal("定时 Run 未在时限内完成")
	}
	// 溯源双载体：RunDetail.scheduledFrom 与 run_created payload.scheduledBy
	// 同值且非空（触发幂等键）。
	detail := env.runDetail(t, teamID, runID)
	scheduledFrom, _ := detail["scheduledFrom"].(string)
	if scheduledFrom == "" {
		t.Fatalf("定时 Run 应携带 scheduledFrom: %v", detail)
	}
	payload := teamEventPayload(env.journalEvents(t), team.EventRunCreated)
	if payload == nil || payload["scheduledBy"] != scheduledFrom {
		t.Fatalf("run_created payload.scheduledBy 应与 scheduledFrom 一致: payload=%v scheduledFrom=%s", payload, scheduledFrom)
	}
	// 账面收尾：lastRunId 指向本 Run、pending 标记清除、NextDue 已推进。
	scheduleView := env.teamScheduleView(t, teamID)
	if scheduleView["lastRunId"] != runID {
		t.Fatalf("schedule.lastRunId 应指向触发的 Run: %v", scheduleView)
	}
	if pending, _ := scheduleView["pendingTriggerId"].(string); pending != "" {
		t.Fatalf("触发完成后 pendingTriggerId 应清空: %v", scheduleView)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 1 ||
		!strings.Contains(records[0].text, "SCHED-E2E") || records[0].session != sessionA {
		t.Fatalf("定时任务应恰好派发一次到成员会话: %v", records)
	}
	// 稳定性窗口：几个 sweep 周期内不二发（NextDue 已推进到下个周期）。
	env.drive(t, 1300*time.Millisecond, func() bool { return false })
	if runs := env.teamRunsList(t, teamID); len(runs) != 1 {
		t.Fatalf("同一窗口内不得重复触发: %d 条 Run", len(runs))
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 1 {
		t.Fatalf("稳定性窗口内不得二次派发: %d 条", got)
	}
}

// TestTeamIntegration4ScheduledTriggerCrashWindows：journal-first 触发协议的
// 两个崩溃窗口（schedule.go 的回扫对账）：
//   - (a) journal 已有 scheduledBy 事件 + team-runs.json 损坏（快照丢失）→
//     重启回扫按 journal 事实收尾：清标记、记账 lastRunId、绝不重触发（快照
//     按 journal 重建后由恢复路径收账）；
//   - (b) 标记已落、journal 无该事件（标记与创建之间的崩溃）→ provably 未
//     触发 → 以同一 triggerID 重执行恰好一次。
func TestTeamIntegration4ScheduledTriggerCrashWindows(t *testing.T) {
	const crashTeam = "tteam-sched-crash"
	const crashRun = "trun-sched-crash"
	const crashSession = "km-team-1"
	writeCrashTeamFixture := func(t *testing.T, dataDir string) {
		t.Helper()
		now := time.Now()
		teamRow := &team.Team{ID: crashTeam, Name: "触发崩溃团队",
			Members: []team.TeamMember{{ID: "lead", SessionID: crashSession, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
			LeadID:  "lead", Policy: team.DefaultPolicy(), CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(map[string]*team.Team{crashTeam: teamRow})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "sessions.txt"),
			[]byte(crashSession+"\x00"+dataDir+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("journal 已有 scheduledBy 快照丢失不重触发", func(t *testing.T) {
		dataDir := t.TempDir()
		writeCrashTeamFixture(t, dataDir)
		triggerID := uuid.NewString()
		submissionID := team.TaskSubmissionID(crashTeam, crashRun, "st1", 1)
		// journal：触发已生效（run_created.scheduledBy）+ 任务已记账派发。
		events := []*team.Event{
			{ID: uuid.NewString(), RunID: crashRun, Type: team.EventRunCreated, Source: "scheduler",
				Payload:  map[string]any{"teamId": crashTeam, "goal": "崩溃窗口目标", "mode": team.ModePipeline, "planHash": "hand-sched", "taskCount": 1, "scheduledBy": triggerID},
				Sequence: 1, CreatedAt: time.Now()},
			{ID: uuid.NewString(), RunID: crashRun, Type: team.EventTaskPlanned, Source: "user",
				Payload: map[string]any{"taskId": "st1", "memberId": "lead"}, Sequence: 2, CreatedAt: time.Now()},
			{ID: uuid.NewString(), RunID: crashRun, Type: team.EventTaskDispatching, Source: "runner",
				Payload: map[string]any{"taskId": "st1", "sessionId": crashSession, "submissionId": submissionID, "attempt": 1}, Sequence: 3, CreatedAt: time.Now()},
		}
		var journalLines strings.Builder
		for _, event := range events {
			line, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			journalLines.Write(line)
			journalLines.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dataDir, "team-journal.jsonl"), []byte(journalLines.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		// 快照损坏（写一半崩溃）→ 启动按 journal 重建。
		if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), []byte(`{"trun-sched-crash": {bro`), 0o600); err != nil {
			t.Fatal(err)
		}
		writeTeamScheduleFixture(t, dataDir, crashTeam, func(schedule *team.TeamSchedule) {
			schedule.Goal = "崩溃窗口目标"
			schedule.Plan = []team.PlanTaskInput{
				{ID: "st1", MemberID: "lead", Prompt: "崩溃窗口任务 SCHED-CRASH-A 执行", WriteLock: team.TaskWriteLockNone},
			}
			schedule.PendingTriggerID = triggerID
			schedule.NextDue = time.Now().Add(time.Hour)
		})
		env := reopenTeamIntegrationEnv(t, dataDir)
		// 冷 List 预热成员会话归属（生产由 discovery 周期执行；不预热则
		// 重建后的派发解析不到会话）。
		if _, err := env.manager.List(15 * time.Second); err != nil {
			t.Fatalf("session/list 预热失败: %v", err)
		}
		env.attachScheduler(t) // 首轮 sweep 即回扫 pending
		// 回扫裁决：标记清除、lastRunId 指向 journal 里那条 Run（而非新建）。
		if !awaitCond(t, teamIntegrationWait, func() bool {
			view := env.teamScheduleView(t, crashTeam)
			pending, _ := view["pendingTriggerId"].(string)
			return pending == "" && view["lastRunId"] == crashRun
		}) {
			t.Fatalf("回扫未按 journal 事实收尾: %v", env.teamScheduleView(t, crashTeam))
		}
		if runs := env.teamRunsList(t, crashTeam); len(runs) != 1 || runs[0]["id"] != crashRun {
			t.Fatalf("快照丢失不得触发第二条 Run: %v", runs)
		}
		// 快照丢失的边界（现状钉住）：journal 只携带有界 instructionsHead
		//（§10.3），任务全文随快照丢失——重建后回 ready 的任务重派发时入队
		// 校验拒绝空文本、Run 确定性 failed。触发协议的关键断言是上一段：
		// journal 事件在，回扫就绝不重触发（哪怕 Run 自身已不可恢复）。
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, crashTeam, crashRun)["status"] == "failed"
		}) {
			t.Fatalf("重建的定时 Run 未在时限内确定性收口: %+v", env.runDetail(t, crashTeam, crashRun))
		}
		detail := env.runDetail(t, crashTeam, crashRun)
		if reason, _ := detail["stopReason"].(string); !strings.Contains(reason, "入队失败") {
			t.Fatalf("快照丢失的任务重派发应确定性失败（空文本入队被拒）: %v", reason)
		}
		records := readTeamPrompts(t, dataDir)
		if len(records) != 0 {
			t.Fatalf("空文本任务不得到达 Agent: %v", records)
		}
		if scheduledFrom, _ := detail["scheduledFrom"].(string); scheduledFrom != triggerID {
			t.Fatalf("重建后的 scheduledFrom 应保持 triggerID: %q", scheduledFrom)
		}
	})
	t.Run("标记已落 journal 无事件则重执行一次", func(t *testing.T) {
		dataDir := t.TempDir()
		writeCrashTeamFixture(t, dataDir)
		triggerID := uuid.NewString()
		writeTeamScheduleFixture(t, dataDir, crashTeam, func(schedule *team.TeamSchedule) {
			schedule.Goal = "崩溃重执行目标"
			schedule.Plan = []team.PlanTaskInput{
				{ID: "st1", MemberID: "lead", Prompt: "崩溃重执行任务 SCHED-CRASH-B 执行", WriteLock: team.TaskWriteLockNone},
			}
			schedule.PendingTriggerID = triggerID
			schedule.NextDue = time.Now().Add(time.Hour)
		})
		env := reopenTeamIntegrationEnv(t, dataDir)
		env.attachScheduler(t)
		if !awaitCond(t, teamIntegrationWait, func() bool {
			return len(env.teamRunsList(t, crashTeam)) == 1
		}) {
			t.Fatal("provably 未触发的标记应在回扫后重执行")
		}
		runID, _ := env.teamRunsList(t, crashTeam)[0]["id"].(string)
		// 重执行沿用同一 triggerID（崩溃重试间幂等）。
		if scheduledFrom, _ := env.runDetail(t, crashTeam, runID)["scheduledFrom"].(string); scheduledFrom != triggerID {
			t.Fatalf("重执行应以同一 triggerID 打标: %q", scheduledFrom)
		}
		payload := teamEventPayload(env.journalEvents(t), team.EventRunCreated)
		if payload == nil || payload["scheduledBy"] != triggerID {
			t.Fatalf("重执行的 run_created 应带 scheduledBy: %v", payload)
		}
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, crashTeam, runID)["status"] == "completed"
		}) {
			t.Fatal("重执行的 Run 未在时限内完成")
		}
		if runs := env.teamRunsList(t, crashTeam); len(runs) != 1 {
			t.Fatalf("重执行恰好一次: %d 条 Run", len(runs))
		}
		if records := readTeamPrompts(t, dataDir); len(records) != 1 {
			t.Fatalf("应恰好派发一次: %v", records)
		}
	})
}

// ---- 用例 4-2：§12 阶段四验收行 1——定时 Run 重启后恢复可确认任务 ----

// TestTeamIntegration4ScheduledRunRestartRecovery：定时触发后在途（任务已
// 记账、账本 queued、提示词尚未到达 Agent——真实崩溃瞬间）→ 同 dataDir
// 重装配 + RecoverAll → 账本 queued 保持、重启后恰好派发一次完成、
// scheduledFrom 与触发溯源跨重启不丢、不重复触发。
func TestTeamIntegration4ScheduledRunRestartRecovery(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "定时重启会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"}]`)
	writeTeamScheduleFixture(t, dataDir, teamID, func(schedule *team.TeamSchedule) {
		schedule.Goal = "定时重启目标 SCHED-RESTART"
		schedule.Plan = []team.PlanTaskInput{
			{ID: "st1", MemberID: "lead", Prompt: "定时重启任务 SCHED-RESTART HOLDUNTIL:sr", WriteLock: team.TaskWriteLockNone},
		}
		schedule.NextDue = time.Now().Add(-time.Minute)
	})
	scheduler := env.attachScheduler(t)
	if !awaitCond(t, teamIntegrationWait, func() bool {
		return len(env.teamRunsList(t, teamID)) == 1
	}) {
		t.Fatal("定时触发未在时限内创建 Run")
	}
	runID, _ := env.teamRunsList(t, teamID)[0]["id"].(string)
	// 在途窗口：loopPass 已把 st1 记账派发（submissionId 可见、账本 queued），
	// 不做派发巡检——提示词未出发即「崩溃」。
	var submissionID string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && submissionID == "" {
		task := teamTaskOf(t, env.runDetail(t, teamID, runID), "st1")
		if id, _ := task["submissionId"].(string); id != "" {
			submissionID = id
		} else {
			time.Sleep(120 * time.Millisecond)
		}
	}
	if submissionID == "" {
		t.Fatal("任务未在时限内完成派发记账")
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("崩溃前提示词不应已到达: %v", records)
	}
	// 重启：先停旧触发器（避免双触发器并存的账本版本串扰），再同 dataDir
	// 重装配 + RecoverAll（main.go:921-945 的启动顺序），重挂触发器。
	scheduler.Stop()
	env2 := reopenTeamIntegrationEnv(t, dataDir)
	if _, err := env2.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	env2.attachScheduler(t)
	// 恢复后：账本 queued 保持并恰好派发一次（HOLDUNTIL 精确放行收账）。
	if !env2.drive(t, 20*time.Second, func() bool {
		return promptIndex(readTeamPrompts(t, dataDir), "SCHED-RESTART") >= 0
	}) {
		t.Fatal("重启后任务未在时限内派发")
	}
	releaseAgentTurn(t, dataDir, "sr", sessionA)
	if !env2.drive(t, 20*time.Second, func() bool {
		return env2.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("重启后的定时 Run 未在时限内完成")
	}
	records := readTeamPrompts(t, dataDir)
	if len(records) != 1 || !strings.Contains(records[0].text, "SCHED-RESTART") {
		t.Fatalf("重启恢复零重复：应恰好一条提示词: %v", records)
	}
	entry := env2.server.Queue.get(sessionA, submissionID)
	if entry == nil || entry.Status != SubAccepted {
		t.Fatalf("恢复派发的条目应 accepted: %+v", entry)
	}
	// 溯源跨重启：scheduledFrom 仍在、触发器不重复触发。
	if scheduledFrom, _ := env2.runDetail(t, teamID, runID)["scheduledFrom"].(string); scheduledFrom == "" {
		t.Fatal("重启后 scheduledFrom 不应丢失")
	}
	if runs := env2.teamRunsList(t, teamID); len(runs) != 1 {
		t.Fatalf("重启不得重复触发: %d 条 Run", len(runs))
	}
	if view := env2.teamScheduleView(t, teamID); view["lastRunId"] != runID {
		t.Fatalf("schedule.lastRunId 应保持: %v", view)
	}
}

// ---- 用例 4-3：§12 阶段四验收行 2——结果未知经 reconciler 三面绝不重发 ----

// TestTeamIntegration4UnknownOutcomeReconcilerNeverResends：派发结果未知
// （账本 unknown）→ run 停 waiting_input 后，周期 reconciler、手动 refresh
// 端点、重启 RecoverAll 三面都只对账：run 状态永不变（waiting_input 驻留）、
// 任务不转 ready、提示词零增长；用户 Resume 才把未决 unknown 落 stopped
// （「不再重发」）。
func TestTeamIntegration4UnknownOutcomeReconcilerNeverResends(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "对账会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"结果未知对账","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"等待对账的任务 UNKNOWN-R4","writeLock":"none"}]}`)

	// 等派发记账后手动落 unknown（生产 dispatchSubmission 的真实中间态）。
	var submissionID string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && submissionID == "" {
		task := teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")
		if id, _ := task["submissionId"].(string); id != "" {
			submissionID = id
		} else {
			time.Sleep(120 * time.Millisecond)
		}
	}
	if submissionID == "" {
		t.Fatal("任务未在时限内完成派发记账")
	}
	env.server.Queue.setStatus(sessionA, submissionID, SubUnknown)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("结果未知应把 Run 停进 waiting_input")
	}

	// 面①：周期 reconciler 循环跑过多个 tick，状态纹丝不动。
	env.server.Teams.StartReconciler(150 * time.Millisecond)
	t.Cleanup(env.server.Teams.StopReconciler)
	since := time.Now()
	if !awaitCond(t, 5*time.Second, func() bool {
		return time.Since(since) > 600*time.Millisecond
	}) {
		t.Fatal("等待 reconciler 周期失败")
	}
	// 面②：手动 refresh 端点（同一 Service.RefreshRun 入口）。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/refresh", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("refresh 应 200: %d %s", response.Code, response.Body.String())
	}
	detail := env.runDetail(t, teamID, runID)
	if detail["status"] != "waiting_input" || teamTaskOf(t, detail, "t1")["status"] != "outcome_unknown" {
		t.Fatalf("reconciler/refresh 只对账不改状态: %v", detail["status"])
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("对账绝不重发: %v", records)
	}
	env.server.Teams.StopReconciler()

	// 面③：重启 + RecoverAll + 派发巡检（巡检节拍空转两秒——稳定性窗口，
	// 返回值按惯例忽略，用例 3 的取消后巡检同款）。
	env2 := reopenTeamIntegrationEnv(t, dataDir)
	env2.server.Teams.StartReconciler(150 * time.Millisecond)
	env2.drive(t, 2*time.Second, func() bool { return false })
	response = env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/refresh", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重启后 refresh 应 200: %d %s", response.Code, response.Body.String())
	}
	detail = env2.runDetail(t, teamID, runID)
	if detail["status"] != "waiting_input" || teamTaskOf(t, detail, "t1")["status"] != "outcome_unknown" {
		t.Fatalf("重启后对账不得改 run 状态/转 ready: %v", detail)
	}
	entry := env2.server.Queue.get(sessionA, submissionID)
	if entry == nil || entry.Status != SubUnknown {
		t.Fatalf("账本应保持 unknown: %+v", entry)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("重启后对账绝不重发: %v", records)
	}
	env2.server.Teams.StopReconciler()

	// 用户裁决：Resume 把未决 unknown 落 stopped（不再重发），fold stopped。
	response = env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	detail = env2.runDetail(t, teamID, runID)
	if detail["status"] != "stopped" {
		t.Fatalf("resume 后 Run 应 stopped: %v", detail["status"])
	}
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "stopped" {
		t.Fatalf("未决 unknown 应转 stopped: %v", task["status"])
	} else if reason, _ := task["stopReason"].(string); !strings.Contains(reason, "不再重发") {
		t.Fatalf("stopReason 应说明不再重发: %v", reason)
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 0 {
		t.Fatalf("全程零重发: %v", records)
	}
}

// ---- 用例 4-4：§12 阶段四验收行 3——waiting_input 手机可恢复 ----

// TestTeamIntegration4MobileRecoveryViaNotification：quota 停靠把 Run 停进
// waiting_input → 通知落账（标题「需要你的输入」+ click deep link 指向
// #run=teamId/runId）→ 用户在手机端 Resume → 停靠任务重飞（attempt+1、
// 一次性 quota 通过）→ Run 完成并收到完成通知。AskUserQuestion 的 decision
// 流程不停靠 Run（questions.go:225「不强制 waiting_input」，阶段三
// TestTeamPhase3EscalateQuestionLifecycle 已覆盖手机回答），验收行的
// 「需要用户输入」形态即 waiting_input 停靠。
func TestTeamIntegration4MobileRecoveryViaNotification(t *testing.T) {
	dataDir := t.TempDir()
	fake := startFakeNtfy(t)
	writeNotificationsSettings(t, dataDir, fake.url)
	env := newTeamIntegrationEnv(t, dataDir)
	ledger := notifications.NewRunNotificationLedger(dataDir)
	kindLog := env.attachRunNotifications(t, ledger)
	sessionA := env.createMemberSession(t, "lead-sess", "手机恢复会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"手机恢复目标 MOBILE-RECOVER","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"手机恢复任务 MOBILE-RECOVER QUOTAFAIL 首航停靠","writeLock":"none"}]}`)

	// 停靠：quota 文本 → 任务 stopped(outcome=quota) + run waiting_input +
	// waiting_input 通知落账。
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
	}) {
		t.Fatal("quota 停靠未在时限内发生")
	}
	if !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runID, notifications.RunNotifyWaitingInput)
	}) {
		t.Fatal("waiting_input 通知未落账")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "stopped" || task["outcome"] != team.OutcomeQuota {
		t.Fatalf("任务应 quota 停靠: %v", task)
	}
	// 手机侧收到通知：fake ntfy 载荷含「需要你的输入」与 click deep link。
	if err := ledger.Reconcile(time.Now(), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("通知补发失败: %v", err)
	}
	waiting := fake.findByTitle("需要你的输入")
	if len(waiting) != 1 {
		t.Fatalf("waiting_input 通知应恰好一条: %+v", fake.all())
	}
	if want := "https://bridge.example/#run=" + teamID + "/" + runID; waiting[0].Click != want {
		t.Fatalf("waiting_input 通知 click 应指向 Run 页: %q ≠ %q", waiting[0].Click, want)
	}
	if !strings.Contains(waiting[0].Title, "MOBILE-RECOVER") {
		t.Fatalf("通知标题应含目标: %+v", waiting[0])
	}

	// 手机端恢复：Resume → 重飞 attempt 2（QUOTAFAIL 一次性）→ 完成。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("resume 后 Run 未在时限内完成")
	}
	detail = env.runDetail(t, teamID, runID)
	if attempt, _ := teamTaskOf(t, detail, "t1")["attempt"].(float64); attempt != 2 {
		t.Fatalf("quota 重飞后 attempt 应为 2: %v", attempt)
	}
	if !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runID, notifications.RunNotifyCompleted)
	}) {
		t.Fatal("完成通知未落账")
	}
	if err := ledger.Reconcile(time.Now(), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("通知补发失败: %v", err)
	}
	completed := fake.findByTitle("运行完成")
	if len(completed) != 1 {
		t.Fatalf("完成通知应恰好一条: %+v", fake.all())
	}
	if want := "https://bridge.example/#run=" + teamID + "/" + runID; completed[0].Click != want {
		t.Fatalf("完成通知 click 异常: %q", completed[0].Click)
	}
	if !strings.Contains(completed[0].Message, "MOBILE-RECOVER") {
		t.Fatalf("完成通知正文应含任务结果摘要: %+v", completed[0])
	}
	if records := readTeamPrompts(t, dataDir); len(records) != 2 {
		t.Fatalf("停靠+重飞应恰好两次派发: %d 条", len(records))
	}
}

// ---- 用例 4-5：§12 阶段四工作项 4——pause/resume 全语义 ----

// TestTeamIntegration4PauseResumeLifecycle：pause 掐断新派发（剩余任务零
// 提示词）、在途任务自然收尾（HOLDUNTIL 精确放行，等价 HOLDLONG 收尾的可
// 控版）、收尾后 Run 保持 paused 不折叠、run 超时 deadline 冻结（跨线不
// 超时）、重启保持 paused（快照路径）、resume 续跑恰好派剩余任务、重复
// pause 409。
func TestTeamIntegration4PauseResumeLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "暂停会话")
	teamID := env.createTeamWithPolicy(t, `{"runTimeoutSecs":4}`,
		`[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runStarted := time.Now()
	runID := env.createRun(t, teamID, `{"goal":"暂停恢复 PAUSE-LIFE","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"暂停在途任务 PAUSE-HOLD HOLDUNTIL:pl","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"暂停剩余任务 PAUSE-TAIL 执行","dependsOn":["t1"],"writeLock":"none"}]}`)

	// t1 在途（闸门未放行）时暂停。
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["status"] == "running"
	}) {
		t.Fatal("t1 未在时限内进入 running")
	}
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("pause 失败: %d %s", response.Code, response.Body.String())
	}
	if status := env.runDetail(t, teamID, runID)["status"]; status != "paused" {
		t.Fatalf("Run 应 paused: %v", status)
	}
	// 重复 pause 409（failTeamError 映射哨兵）。
	if again := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); again.Code != http.StatusConflict {
		t.Fatalf("重复 pause 应 409: %d %s", again.Code, again.Body.String())
	}

	// 暂停期间在途收尾：放行 t1 → completed，Run 仍 paused 不折叠。
	releaseAgentTurn(t, dataDir, "pl", sessionA)
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return detail["status"] == "paused" && teamTaskOf(t, detail, "t1")["status"] == "completed"
	}) {
		t.Fatal("在途任务未在暂停期间自然收尾（或 Run 被误折叠）")
	}
	// 暂停期间零新派发：t2 的提示词绝不出现。
	if records := readTeamPrompts(t, dataDir); len(records) != 1 || promptIndex(records, "PAUSE-TAIL") >= 0 {
		t.Fatalf("暂停后不得派发剩余任务: %v", records)
	}

	// run 超时冻结：跨过 runTimeoutSecs 判定线（自 Run 创建起 >4.5s，其中
	// 大半是 paused 驻留）仍 paused——deadline 只由活循环检查，软停不烧时钟。
	if !awaitCond(t, 10*time.Second, func() bool {
		return time.Since(runStarted) > 4500*time.Millisecond
	}) {
		t.Fatal("等待超时判定线失败")
	}
	detail := env.runDetail(t, teamID, runID)
	if detail["status"] != "paused" {
		t.Fatalf("paused 驻留期间 run 超时不得触发: %v（stopReason=%v）", detail["status"], detail["stopReason"])
	}
	if events := env.journalEvents(t); teamEventCount(events, team.EventRunStopped) > 0 {
		t.Fatal("暂停期间不得落 run_stopped（超时冻结）")
	}

	// 重启保持 paused：同 dataDir 重装配 + RecoverAll + 派发巡检，t2 仍零派发。
	env2 := reopenTeamIntegrationEnv(t, dataDir)
	if status := env2.runDetail(t, teamID, runID)["status"]; status != "paused" {
		t.Fatalf("重启后应保持 paused: %v", status)
	}
	env2.drive(t, 1300*time.Millisecond, func() bool { return false })
	if records := readTeamPrompts(t, dataDir); len(records) != 1 {
		t.Fatalf("重启后的 paused Run 不得派发: %v", records)
	}

	// resume 续跑恰好派剩余：t2 派发并完成，t1 零重发（提示词总数 2）。
	response = env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env2.drive(t, 20*time.Second, func() bool {
		return env2.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("resume 后 Run 未在时限内完成")
	}
	records := readTeamPrompts(t, dataDir)
	if len(records) != 2 || promptIndex(records, "PAUSE-TAIL") != 1 {
		t.Fatalf("续跑应恰好派剩余任务: %v", records)
	}
	if t1 := teamTaskOf(t, env2.runDetail(t, teamID, runID), "t1"); t1["status"] != "completed" || t1["attempt"] != float64(1) {
		t.Fatalf("已确认任务零重发（attempt 保持 1）: %v", t1)
	}
}

// TestTeamIntegration4PauseCorruptRebuildStaysPaused：team-runs.json 损坏后
// 按 journal 重建回 paused（run_paused 事件是唯一可恢复事实，store.go 的
// applyJournalEventToRun）；重建后的 Run 等 resume，不自动推进。
func TestTeamIntegration4PauseCorruptRebuildStaysPaused(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "重建暂停会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"重建暂停 PAUSE-REBUILD","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"重建暂停在途任务 PAUSE-RB HOLDUNTIL:pr","writeLock":"none"},
		{"id":"t2","memberId":"lead","prompt":"重建暂停剩余任务 PAUSE-RB-TAIL","dependsOn":["t1"],"writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["status"] == "running"
	}) {
		t.Fatal("t1 未在时限内进入 running")
	}
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); response.Code != http.StatusOK {
		t.Fatalf("pause 失败: %d %s", response.Code, response.Body.String())
	}
	releaseAgentTurn(t, dataDir, "pr", sessionA)
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return detail["status"] == "paused" && teamTaskOf(t, detail, "t1")["status"] == "completed"
	}) {
		t.Fatal("在途任务未收尾")
	}
	// 快照损坏 → 重启按 journal 重建（冷 List 预热：重建任务的 writeLock
	// 随快照丢失、空值按独占处理，cwd 解析依赖 list 缓存）。
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), []byte(`{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	env2 := reopenTeamIntegrationEnv(t, dataDir)
	if _, err := env2.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	detail := env2.runDetail(t, teamID, runID)
	if status := detail["status"]; status != "paused" {
		t.Fatalf("journal 重建后应回 paused: %v", status)
	}
	if task := teamTaskOf(t, detail, "t1"); task["status"] != "completed" {
		t.Fatalf("重建应保留已确认任务事实: %v", task["status"])
	}
	env2.drive(t, 1300*time.Millisecond, func() bool { return false })
	if records := readTeamPrompts(t, dataDir); len(records) != 1 {
		t.Fatalf("重建后的 paused Run 不得派发: %v", records)
	}
	// 续跑语义的边界（现状钉住）：journal 只携带有界 instructionsHead，
	// t2 全文随快照丢失——resume 会推进 Run 但剩余任务以空文本入队被拒、
	// 确定性 failed。重建后「保持 paused 等用户处置」正是 run_paused 事件
	// journal-first 的价值；快照完好的续跑链路由上一用例覆盖。
	if response := env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env2.drive(t, 20*time.Second, func() bool {
		return env2.runDetail(t, teamID, runID)["status"] == "failed"
	}) {
		t.Fatalf("重建后的 resume 未在时限内确定性收口: %+v", env2.runDetail(t, teamID, runID))
	}
	if reason, _ := teamTaskOf(t, env2.runDetail(t, teamID, runID), "t2")["error"].(string); !strings.Contains(reason, "入队失败") {
		t.Fatalf("快照丢失的剩余任务应确定性失败: %v", reason)
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 1 {
		t.Fatalf("已确认任务零重发、空文本任务不得到达 Agent: %d", got)
	}
}

// TestTeamIntegration4PauseUpgradesToWaitingInput：暂停期间在途任务的后续
// 停靠把 Run 升级 waiting_input（语义严格强于 paused，runner.go:550-556）：
//   - unknown：暂停中账本转 unknown → watcher 落 outcome_unknown + 升级；
//     Resume 走 PlanResume——unknown 落 stopped「不再重发」，绝不重飞。
//   - quota：QUOTAGATE 在途停靠（闸门文件精确触发）→ 升级 waiting_input；
//     Resume 重飞 attempt+1 通过并完成。
func TestTeamIntegration4PauseUpgradesToWaitingInput(t *testing.T) {
	t.Run("暂停期间 unknown 升级且不再重发", func(t *testing.T) {
		dataDir := t.TempDir()
		env := newTeamIntegrationEnv(t, dataDir)
		sessionA := env.createMemberSession(t, "lead-sess", "升级会话")
		teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
		runID := env.createRun(t, teamID, `{"goal":"暂停升级 PAUSE-UNK","mode":"pipeline","plan":[
			{"id":"t1","memberId":"lead","prompt":"暂停升级任务 PAUSE-UNK 等待对账","writeLock":"none"}]}`)
		// 派发记账但不巡检（提示词未出发——账本 queued）；watcher 的
		// unknown 判定要求「会话空闲且无本任务 turn」，真实在途 turn 不会
		// 被误判为未知（去抖复核的正确性面），故本形态停在记账窗口。
		var submissionID string
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && submissionID == "" {
			if id, _ := teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["submissionId"].(string); id != "" {
				submissionID = id
			} else {
				time.Sleep(120 * time.Millisecond)
			}
		}
		if submissionID == "" {
			t.Fatal("任务未在时限内完成派发记账")
		}
		if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); response.Code != http.StatusOK {
			t.Fatalf("pause 失败: %d %s", response.Code, response.Body.String())
		}
		// 暂停中「派发结果未知」：账本转 unknown → watcher 升级。
		env.server.Queue.setStatus(sessionA, submissionID, SubUnknown)
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
		}) {
			t.Fatal("暂停期间结果未知应把 Run 升级 waiting_input")
		}
		detail := env.runDetail(t, teamID, runID)
		if task := teamTaskOf(t, detail, "t1"); task["status"] != "outcome_unknown" {
			t.Fatalf("任务应 outcome_unknown: %v", task["status"])
		}
		response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
		if response.Code != http.StatusOK {
			t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
		}
		detail = env.runDetail(t, teamID, runID)
		if detail["status"] != "stopped" {
			t.Fatalf("升级后的 resume 应按 waiting_input 语义收口 stopped: %v", detail["status"])
		}
		if reason, _ := teamTaskOf(t, detail, "t1")["stopReason"].(string); !strings.Contains(reason, "不再重发") {
			t.Fatalf("升级后 resume 绝不重飞: %v", reason)
		}
		if got := len(readTeamPrompts(t, dataDir)); got != 0 {
			t.Fatalf("结果未知绝不重发（提示词零到达）: %d", got)
		}
	})
	t.Run("暂停期间 quota 停靠升级可重飞", func(t *testing.T) {
		dataDir := t.TempDir()
		env := newTeamIntegrationEnv(t, dataDir)
		sessionA := env.createMemberSession(t, "lead-sess", "配额升级会话")
		teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
		runID := env.createRun(t, teamID, `{"goal":"暂停配额升级 PAUSE-QUOTA","mode":"pipeline","plan":[
			{"id":"t1","memberId":"lead","prompt":"暂停配额任务 PAUSE-QUOTA QUOTAGATE:pq","writeLock":"none"}]}`)
		// QUOTAGATE 首航：闸门放行前 turn 在途。
		if !env.drive(t, teamIntegrationWait, func() bool {
			return teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["status"] == "running"
		}) {
			t.Fatal("任务未在时限内进入 running")
		}
		if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); response.Code != http.StatusOK {
			t.Fatalf("pause 失败: %d %s", response.Code, response.Body.String())
		}
		// 放行闸门：在途 turn 以 quota 错误收尾 → watcher 停靠 → 从 paused
		// 升级 waiting_input。
		releaseAgentTurn(t, dataDir, "pq", sessionA)
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "waiting_input"
		}) {
			t.Fatal("暂停期间 quota 停靠应把 Run 升级 waiting_input")
		}
		detail := env.runDetail(t, teamID, runID)
		if task := teamTaskOf(t, detail, "t1"); task["status"] != "stopped" || task["outcome"] != team.OutcomeQuota {
			t.Fatalf("任务应 quota 停靠: %v", task)
		}
		// Resume：停靠重飞 attempt+1（QUOTAGATE 一次性）→ 完成。
		response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
		if response.Code != http.StatusOK {
			t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
		}
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "completed"
		}) {
			t.Fatal("升级后的 resume 未在时限内完成")
		}
		if attempt, _ := teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["attempt"].(float64); attempt != 2 {
			t.Fatalf("quota 停靠重飞 attempt 应为 2: %v", attempt)
		}
		if got := len(readTeamPrompts(t, dataDir)); got != 2 {
			t.Fatalf("停靠+重飞应恰好两次派发: %d", got)
		}
	})
}

// ---- 用例 4-6：§12 阶段四工作项 3/5——通知四类载荷 + 水位补发与退避 ----

// TestTeamIntegration4RunNotificationPayloads：完成/失败/waiting_input 三类
// 经真实 OnRunStatus 落账并送达伪 ntfy——标题前缀、click deep link
// （#run=teamId/runId）、失败通知 salvage 前 3 个已完成任务结果（§6.5：不
// 因后续失败吞掉，第 4 个已完成任务被 3 条上限截去）。stall 类见
// TestTeamIntegration4StallNotifiesWithoutStopping（同渠道同 click 组装）。
func TestTeamIntegration4RunNotificationPayloads(t *testing.T) {
	dataDir := t.TempDir()
	fake := startFakeNtfy(t)
	writeNotificationsSettings(t, dataDir, fake.url)
	env := newTeamIntegrationEnv(t, dataDir)
	ledger := notifications.NewRunNotificationLedger(dataDir)
	kindLog := env.attachRunNotifications(t, ledger)
	sessionA := env.createMemberSession(t, "lead-sess", "通知会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	clickOf := func(runID string) string {
		return "https://bridge.example/#run=" + teamID + "/" + runID
	}

	// Run 1：单任务完成。
	doneRun := env.createRun(t, teamID, `{"goal":"通知完成目标 NOTIF-OK","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"通知完成任务 NOTIF-OK","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, doneRun)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	// Run 2：四个完成 + 一个失败（salvage 上限载体）。
	failedRun := env.createRun(t, teamID, `{"goal":"通知失败目标 NOTIF-FAIL","mode":"pipeline","plan":[
		{"id":"c1","memberId":"lead","prompt":"失败挽救一 NOTIF-C1","writeLock":"none"},
		{"id":"c2","memberId":"lead","prompt":"失败挽救二 NOTIF-C2","dependsOn":["c1"],"writeLock":"none"},
		{"id":"c3","memberId":"lead","prompt":"失败挽救三 NOTIF-C3","dependsOn":["c2"],"writeLock":"none"},
		{"id":"c4","memberId":"lead","prompt":"失败挽救四 NOTIF-C4","dependsOn":["c3"],"writeLock":"none"},
		{"id":"f5","memberId":"lead","prompt":"失败触发 NOTIF-F5 FAIL 注入","dependsOn":["c4"],"writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, failedRun)["status"] == "failed"
	}) {
		t.Fatal("Run 未在时限内失败")
	}
	// Run 3：quota 停靠 waiting_input。
	parkedRun := env.createRun(t, teamID, `{"goal":"通知停靠目标 NOTIF-PARK","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"通知停靠任务 NOTIF-PARK QUOTAFAIL","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, parkedRun)["status"] == "waiting_input"
	}) {
		t.Fatal("Run 未在时限内停靠")
	}
	for _, want := range []struct {
		runID, kind string
	}{
		{doneRun, notifications.RunNotifyCompleted},
		{failedRun, notifications.RunNotifyFailed},
		{parkedRun, notifications.RunNotifyWaitingInput},
	} {
		if !awaitCond(t, teamIntegrationWait, func() bool {
			return kindLog.has(want.runID, want.kind)
		}) {
			t.Fatalf("Run %s 的 %s 通知未落账", want.runID, want.kind)
		}
	}
	// 一次性 Reconcile 全部送达（任一渠道成功即送达）。
	if err := ledger.Reconcile(time.Now(), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("通知补发失败: %v", err)
	}
	assertNotification := func(prefix, runID string) fakeNtfyPost {
		t.Helper()
		posts := fake.findByTitle(prefix)
		if len(posts) != 1 {
			t.Fatalf("%s 通知应恰好一条: %+v", prefix, fake.all())
		}
		if posts[0].Click != clickOf(runID) {
			t.Fatalf("%s 通知 click 异常: %q", prefix, posts[0].Click)
		}
		return posts[0]
	}
	donePost := assertNotification("运行完成", doneRun)
	if !strings.Contains(donePost.Message, "NOTIF-OK") || !strings.Contains(donePost.Message, "完成 1") {
		t.Fatalf("完成通知正文应含结果摘要与计数: %+v", donePost)
	}
	failedPost := assertNotification("运行失败", failedRun)
	for _, want := range []string{"NOTIF-C1", "NOTIF-C2", "NOTIF-C3"} {
		if !strings.Contains(failedPost.Message, want) {
			t.Fatalf("失败通知应 salvage 前 3 个完成结果（缺 %s）: %q", want, failedPost.Message)
		}
	}
	if strings.Contains(failedPost.Message, "NOTIF-C4") {
		t.Fatalf("失败通知结果摘要应截在前 3 个: %q", failedPost.Message)
	}
	if !strings.Contains(failedPost.Message, "失败 1") {
		t.Fatalf("失败通知正文应含任务计数: %q", failedPost.Message)
	}
	assertNotification("需要你的输入", parkedRun)
}

// TestTeamIntegration4RunNotificationBackfillAndBackoff：发送失败按指数退避
// 顺延（退避窗内 Reconcile 不再尝试——不轰炸）、server 复活后到点补发；
// 优雅关停水位 + 送达痕迹双闸：重启后补发扫描只补水位窗口内的未送达 Run，
// 已送达的绝不重发、窗口外的老 Run 绝不轰炸。
func TestTeamIntegration4RunNotificationBackfillAndBackoff(t *testing.T) {
	dataDir := t.TempDir()
	fake := startFakeNtfy(t)
	writeNotificationsSettings(t, dataDir, fake.url)
	env := newTeamIntegrationEnv(t, dataDir)
	ledger := notifications.NewRunNotificationLedger(dataDir)
	kindLog := env.attachRunNotifications(t, ledger)
	sessionA := env.createMemberSession(t, "lead-sess", "补发会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)

	// Run A：完成即送达（对照面——此后永不重发）。
	runA := env.createRun(t, teamID, `{"goal":"补发对照 BACKFILL-A","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"补发对照任务 BACKFILL-A","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runA)["status"] == "completed"
	}) || !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runA, notifications.RunNotifyCompleted)
	}) {
		t.Fatal("Run A 未完成或通知未落账")
	}
	// Run A：完成即送达（对照面——此后永不重发）。第一次尝试失败（杀
	// server），退避到点（now=first+11s）后复活送达。
	first := time.Now()
	fake.setFailing(true)
	if err := ledger.Reconcile(first, notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("补发失败: %v", err)
	}
	fake.setFailing(false)
	if err := ledger.Reconcile(first.Add(11*time.Second), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("补发失败: %v", err)
	}
	if got := len(fake.findByTitle("BACKFILL-A")); got != 2 {
		t.Fatalf("Run A 应恰好一次失败尝试+一次送达: %d", got)
	}
	if !ledger.Delivered(runA, notifications.RunNotifyCompleted) {
		t.Fatal("Run A 应已送达")
	}

	// Run B：server 挂掉期间完成 → 失败退避。
	fake.setFailing(true)
	runB := env.createRun(t, teamID, `{"goal":"补发退避 BACKFILL-B","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"补发退避任务 BACKFILL-B","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runB)["status"] == "completed"
	}) || !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runB, notifications.RunNotifyCompleted)
	}) {
		t.Fatal("Run B 未完成或通知未落账")
	}
	base := first.Add(20 * time.Second)
	if err := ledger.Reconcile(base, notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("补发失败: %v", err)
	}
	if got := len(fake.findByTitle("BACKFILL-B")); got != 1 {
		t.Fatalf("失败也应有一次尝试: %d", got)
	}
	if ledger.Delivered(runB, notifications.RunNotifyCompleted) {
		t.Fatal("失败的发送不得标记送达")
	}
	// 退避窗内（attempts=1 → next=+10s）反复 Reconcile 不再尝试。
	if err := ledger.Reconcile(base.Add(5*time.Second), notifications.NewPublisher(nil)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reconcile(base.Add(9*time.Second), notifications.NewPublisher(nil)); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.findByTitle("BACKFILL-B")); got != 1 {
		t.Fatalf("退避窗内不得轰炸: %d 次尝试", got)
	}

	// Run C：关停前落账、未送达——水位窗口内的补发候选。
	runC := env.createRun(t, teamID, `{"goal":"补发窗口 BACKFILL-C","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"补发窗口任务 BACKFILL-C","writeLock":"none"}]}`)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runC)["status"] == "completed"
	}) || !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runC, notifications.RunNotifyCompleted)
	}) {
		t.Fatal("Run C 未完成或通知未落账")
	}
	// 优雅关停：水位盖到关停时刻（B/C 的 UpdatedAt 都在水位附近，落在
	// watermark−5min 窗口内；水位窗口的排除算术由 runledger 单测
	// TestRunLedgerWatermarkBackfill 钉住，此处验端到端接线）。
	shutdown := base.Add(30 * time.Second)
	if err := ledger.SetWatermark(shutdown); err != nil {
		t.Fatalf("水位落账失败: %v", err)
	}

	// 「重启」：同 dataDir 重建账本（文件加载——退避与送达痕迹随文件存活），
	// 水位扫描补记未送达的窗口内 Run（B 幂等重记不重置退避、C 补记后立即
	// 到期），已送达的 A 绝不重发。
	restarted := notifications.NewRunNotificationLedger(dataDir)
	candidates := teamBackfillCandidates(env)
	restarted.Backfill(shutdown.Add(time.Second), notifications.RunBackfillMargin, candidates)
	fake.setFailing(false)
	aBefore := len(fake.findByTitle("BACKFILL-A"))
	if err := restarted.Reconcile(base.Add(41*time.Second), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("重启补发失败: %v", err)
	}
	if got := len(fake.findByTitle("BACKFILL-A")); got != aBefore {
		t.Fatalf("已送达的 Run 重启后绝不重发: %d → %d", aBefore, got)
	}
	if got := len(fake.findByTitle("BACKFILL-B")); got != 2 {
		t.Fatalf("Run B 应在退避到点后恰好补发一次（共 2 次尝试）: %d", got)
	}
	if got := len(fake.findByTitle("BACKFILL-C")); got != 1 {
		t.Fatalf("Run C 应经水位扫描恰好补发一次: %d", got)
	}
	if !restarted.Delivered(runB, notifications.RunNotifyCompleted) ||
		!restarted.Delivered(runC, notifications.RunNotifyCompleted) {
		t.Fatal("补发后应留送达痕迹")
	}
	if restarted.Delivered(runA, notifications.RunNotifyFailed) {
		t.Fatal("Run A 只有 completed 痕迹（键按类别隔离）")
	}
}

// ---- 用例 4-7：§6.3 stall——20 分钟无成功只通知不停止 ----

// TestTeamIntegration4StallNotifiesWithoutStopping：SetStallAfter 把判定线
// 缩到 300ms（notify.go maybeStallLocked 取真实时钟，推进由 deadline 轮询
// 兜住——不用固定 sleep 判正确性）：在途任务挂闸门期间跨线 → run_stalled
// 事件恰好一条（payload.sinceMs≥判定线）+ stall 通知落账送达（标题「运行
// 疑似停滞」+ click deep link）+ Run 状态纹丝不动（不停、不取消）；放行后
// 任务完成、Run 正常终局——成功即恢复，不再报第二条。
func TestTeamIntegration4StallNotifiesWithoutStopping(t *testing.T) {
	dataDir := t.TempDir()
	fake := startFakeNtfy(t)
	writeNotificationsSettings(t, dataDir, fake.url)
	env := newTeamIntegrationEnv(t, dataDir)
	env.server.Teams.SetStallAfter(300 * time.Millisecond)
	ledger := notifications.NewRunNotificationLedger(dataDir)
	kindLog := env.attachRunNotifications(t, ledger)
	sessionA := env.createMemberSession(t, "lead-sess", "停滞会话")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"停滞观测目标 STALL-WATCH","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"停滞观测任务 STALL-WATCH HOLDUNTIL:sw","writeLock":"none"}]}`)

	// 在途（闸门未放行）→ 跨过判定线 → run_stalled 落账恰好一条。
	if !env.drive(t, teamIntegrationWait, func() bool {
		return teamTaskOf(t, env.runDetail(t, teamID, runID), "t1")["status"] == "running"
	}) {
		t.Fatal("任务未在时限内进入 running")
	}
	if !awaitCond(t, teamIntegrationWait, func() bool {
		return kindLog.has(runID, notifications.RunNotifyStall) &&
			teamEventCount(env.journalEvents(t), team.EventRunStalled) == 1
	}) {
		t.Fatal("停滞未在时限内触发 run_stalled（恰好一条）")
	}
	// §6.3 只通知不停止：Run 状态不变、任务仍在途。
	detail := env.runDetail(t, teamID, runID)
	if detail["status"] != "dispatching" {
		t.Fatalf("stall 绝不改 Run 状态: %v（stopReason=%v）", detail["status"], detail["stopReason"])
	}
	if task := teamTaskOf(t, detail, "t1"); !inFlightTask(task) {
		t.Fatalf("stall 绝不取消任务: %v", task["status"])
	}
	var stalledPayload map[string]any
	for _, event := range env.journalEvents(t) {
		if event.Type == team.EventRunStalled {
			stalledPayload = event.Payload
		}
	}
	if sinceMs := eventPayloadInt(stalledPayload, "sinceMs"); sinceMs < 300 {
		t.Fatalf("run_stalled payload.sinceMs 应≥判定线 300: %v", stalledPayload)
	}
	// stall 通知送达伪 ntfy（click deep link 与其他类别同一组装）。
	if err := ledger.Reconcile(time.Now(), notifications.NewPublisher(nil)); err != nil {
		t.Fatalf("通知补发失败: %v", err)
	}
	stallPosts := fake.findByTitle("运行疑似停滞")
	if len(stallPosts) != 1 {
		t.Fatalf("stall 通知应恰好一条: %+v", fake.all())
	}
	if want := "https://bridge.example/#run=" + teamID + "/" + runID; stallPosts[0].Click != want {
		t.Fatalf("stall 通知 click 异常: %q", stallPosts[0].Click)
	}

	// 放行 → 任务完成 → Run 正常终局（成功即恢复；不再报第二条 stall）。
	releaseAgentTurn(t, dataDir, "sw", sessionA)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("停滞后的 Run 未在时限内正常完成")
	}
	if got := teamEventCount(env.journalEvents(t), team.EventRunStalled); got != 1 {
		t.Fatalf("成功恢复后不得再报 stall: %d 条", got)
	}
	if got := kindLog.count(runID, notifications.RunNotifyStall); got != 1 {
		t.Fatalf("stall 通知应恰好一次: %d", got)
	}
}

// ---- 用例 4-8：§9 预算暂停与恢复、重启重放 ceiling 不漂移 ----

// TestTeamIntegration4BudgetParkResumeAndRestart：BudgetUnits=2 的五任务
// 流水线——第 2 次派发后试图派第 3 个时停靠 waiting_input
// （stopReason=budget_exhausted）；Resume 追加预算（run_budget_resumed，
// ceiling=已派发+units）续跑到下一阈值再次停靠；team-runs.json 损坏后按
// journal 重建，ceiling 与停靠语义不漂移（store.go 回填 run_budget_resumed
// 的 ceiling）；再次 Resume 后跑完，stats 的预算计量与 journal 一致。
func TestTeamIntegration4BudgetParkResumeAndRestart(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "预算会话")
	teamID := env.createTeamWithPolicy(t, `{"budgetUnits":2}`,
		`[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
	runID := env.createRun(t, teamID, `{"goal":"预算闸门 BUDGET-FLOW","mode":"pipeline","plan":[
		{"id":"a","memberId":"lead","prompt":"预算任务一 BUDGET-A","writeLock":"none"},
		{"id":"b","memberId":"lead","prompt":"预算任务二 BUDGET-B","dependsOn":["a"],"writeLock":"none"},
		{"id":"c","memberId":"lead","prompt":"预算任务三 BUDGET-C","dependsOn":["b"],"writeLock":"none"},
		{"id":"d","memberId":"lead","prompt":"预算任务四 BUDGET-D","dependsOn":["c"],"writeLock":"none"},
		{"id":"e","memberId":"lead","prompt":"预算任务五 BUDGET-E","dependsOn":["d"],"writeLock":"none"}]}`)

	// 第一次停靠：a/b 派发记账后，试图派 c 时预算到线（在途的 b 由 watcher
	// 自然收尾——停靠不 fold，等它落 completed 再断言）。
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		reason, _ := detail["stopReason"].(string)
		return detail["status"] == "waiting_input" &&
			strings.Contains(reason, team.StopReasonBudgetExhausted) &&
			teamTaskOf(t, detail, "b")["status"] == "completed"
	}) {
		t.Fatal("第 2 次派发后未在时限内预算停靠（或 b 未收尾）")
	}
	detail := env.runDetail(t, teamID, runID)
	if task := teamTaskOf(t, detail, "c"); task["status"] != "planned" {
		t.Fatalf("预算停靠时未派发任务保持 planned: %v", task["status"])
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 2 {
		t.Fatalf("停靠前应恰好派发 2 次: %d", got)
	}

	// 第一次 Resume：ceiling=2+2=4 → c/d 续跑 → 到下一阈值再次停靠。
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		reason, _ := detail["stopReason"].(string)
		return detail["status"] == "waiting_input" &&
			strings.Contains(reason, team.StopReasonBudgetExhausted) &&
			teamTaskOf(t, detail, "d")["status"] == "completed"
	}) {
		t.Fatal("续跑未在时限内到下一预算阈值停靠")
	}
	events := env.journalEvents(t)
	if got := teamEventCount(events, team.EventRunBudgetResumed); got != 1 {
		t.Fatalf("应恰好一条 run_budget_resumed: %d", got)
	}
	if payload := teamEventPayload(events, team.EventRunBudgetResumed); payload == nil ||
		eventPayloadInt(payload, "ceiling") != 4 || eventPayloadInt(payload, "units") != 2 ||
		eventPayloadInt(payload, "used") != 2 {
		t.Fatalf("run_budget_resumed payload 异常: %v", payload)
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 4 {
		t.Fatalf("第二阈值停靠前应恰好派发 4 次: %d", got)
	}

	// 快照损坏 → journal 重建：ceiling 回填 4、停靠语义还原，重启不漂移
	//（冷 List 预热：重建任务的 writeLock 随快照丢失、空值按独占处理，
	// resume 后剩余任务的 cwd 解析依赖 list 缓存）。
	if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), []byte(`{"broken`), 0o600); err != nil {
		t.Fatal(err)
	}
	env2 := reopenTeamIntegrationEnv(t, dataDir)
	if _, err := env2.manager.List(15 * time.Second); err != nil {
		t.Fatalf("session/list 预热失败: %v", err)
	}
	detail = env2.runDetail(t, teamID, runID)
	if detail["status"] != "waiting_input" ||
		!strings.Contains(detail["stopReason"].(string), team.StopReasonBudgetExhausted) {
		t.Fatalf("重建后应保持预算停靠: %v %v", detail["status"], detail["stopReason"])
	}
	stats := env2.teamRunStats(t, teamID, runID)
	budget, _ := stats["budget"].(map[string]any)
	if budget == nil || budget["ceiling"] != float64(4) || budget["used"] != float64(4) ||
		budget["exhausted"] != true {
		t.Fatalf("重建后预算计量不漂移: %v", budget)
	}
	env2.drive(t, 1300*time.Millisecond, func() bool { return false })
	if got := len(readTeamPrompts(t, dataDir)); got != 4 {
		t.Fatalf("停靠中的 Run 重启后不得派发: %d", got)
	}

	// 第二次 Resume（重建后）：ceiling=4+2=6，e 派发。续跑语义的边界
	// （现状钉住，与定时触发崩溃窗口同源）：journal 只携带有界
	// instructionsHead，从未派发的 e 全文随快照丢失——重派发以空文本入队
	// 被拒、确定性 failed；追加与计量事实仍完整落 journal（这正是双载体的
	// 意义）。快照完好的续跑链路见暂停恢复用例（TestTeamIntegration4
	// PauseResumeLifecycle）。
	if response := env2.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); response.Code != http.StatusOK {
		t.Fatalf("重建后 resume 失败: %d %s", response.Code, response.Body.String())
	}
	if !env2.drive(t, 20*time.Second, func() bool {
		return env2.runDetail(t, teamID, runID)["status"] == "failed"
	}) {
		t.Fatalf("重建后的 resume 未在时限内确定性收口: %+v", env2.runDetail(t, teamID, runID))
	}
	if errText, _ := teamTaskOf(t, env2.runDetail(t, teamID, runID), "e")["error"].(string); !strings.Contains(errText, "入队失败") {
		t.Fatalf("快照丢失的剩余任务应确定性失败: %v", errText)
	}
	events = env2.journalEvents(t)
	if got := teamEventCount(events, team.EventRunBudgetResumed); got != 2 {
		t.Fatalf("应有两条 run_budget_resumed: %d", got)
	}
	lastBudget := map[string]int{}
	for _, event := range events {
		if event.Type == team.EventRunBudgetResumed {
			lastBudget = map[string]int{
				"ceiling": eventPayloadInt(event.Payload, "ceiling"),
				"units":   eventPayloadInt(event.Payload, "units"),
				"used":    eventPayloadInt(event.Payload, "used"),
			}
		}
	}
	if lastBudget["ceiling"] != 6 || lastBudget["used"] != 4 {
		t.Fatalf("第二次追加应 ceiling=6 used=4: %v", lastBudget)
	}
	// e 的派发记账已入 journal（used=5<6，未再停靠）。
	stats = env2.teamRunStats(t, teamID, runID)
	budget, _ = stats["budget"].(map[string]any)
	if budget == nil || budget["units"] != float64(2) || budget["used"] != float64(5) ||
		budget["ceiling"] != float64(6) || budget["exhausted"] != false {
		t.Fatalf("终局预算计量异常: %v", budget)
	}
	if got := len(readTeamPrompts(t, dataDir)); got != 4 {
		t.Fatalf("已派发任务零重发、空文本任务不得到达 Agent: %d", got)
	}
}

// ---- 用例 4-9：§12 阶段四工作项 5——成本与耗时统计 ----

// TestTeamIntegration4RunStats：真实跑完的 Run 统计数字断言（coverage=full、
// runElapsedMs>0、dispatchCount、任务条目、tokens 恒 null 明示不支持）；
// 手工预置零时间戳 journal 的历史 Run（升级前事件 createdAt 恒零值）以
// coverage=none 诚实降级——耗时为 0 绝不编造，派发计数仍按事件可得。
func TestTeamIntegration4RunStats(t *testing.T) {
	t.Run("真实 Run 统计", func(t *testing.T) {
		dataDir := t.TempDir()
		env := newTeamIntegrationEnv(t, dataDir)
		sessionA := env.createMemberSession(t, "lead-sess", "统计会话")
		teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+`","role":"lead"}]`)
		runID := env.createRun(t, teamID, `{"goal":"统计目标 STATS-FLOW","mode":"pipeline","plan":[
			{"id":"t1","memberId":"lead","prompt":"统计任务一 STATS-ONE","writeLock":"none"},
			{"id":"t2","memberId":"lead","prompt":"统计任务二 STATS-TWO","dependsOn":["t1"],"writeLock":"none"}]}`)
		if !env.drive(t, teamIntegrationWait, func() bool {
			return env.runDetail(t, teamID, runID)["status"] == "completed"
		}) {
			t.Fatal("Run 未在时限内完成")
		}
		stats := env.teamRunStats(t, teamID, runID)
		if stats["coverage"] != team.StatsCoverageFull {
			t.Fatalf("新事件全部盖章应 coverage=full: %v", stats["coverage"])
		}
		if elapsed, _ := stats["runElapsedMs"].(float64); elapsed <= 0 {
			t.Fatalf("runElapsedMs 应为正: %v", stats["runElapsedMs"])
		}
		if stats["dispatchCount"] != float64(2) {
			t.Fatalf("dispatchCount 应为 2: %v", stats["dispatchCount"])
		}
		if stats["tokens"] != nil {
			t.Fatalf("tokens 恒 null（明示不支持）: %v", stats["tokens"])
		}
		tasks, _ := stats["tasks"].([]any)
		if len(tasks) != 2 {
			t.Fatalf("应有两条任务统计: %v", tasks)
		}
		for _, row := range tasks {
			stat, _ := row.(map[string]any)
			if stat["attempt"] != float64(1) {
				t.Fatalf("任务条目 attempt 应为 1: %v", stat)
			}
			if _, ok := stat["elapsedMs"]; !ok {
				t.Fatalf("任务条目应带 elapsedMs: %v", stat)
			}
		}
		budget, _ := stats["budget"].(map[string]any)
		if budget == nil || budget["units"] != float64(0) || budget["used"] != float64(2) {
			t.Fatalf("默认策略只统计不暂停: %v", budget)
		}
	})
	t.Run("零时间戳历史 Run 降级为无数据", func(t *testing.T) {
		dataDir := t.TempDir()
		now := time.Now()
		const statsTeam, statsRun, statsSession = "tteam-stats-old", "trun-stats-old", "km-team-1"
		teamRow := &team.Team{ID: statsTeam, Name: "历史统计团队",
			Members: []team.TeamMember{{ID: "lead", SessionID: statsSession, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
			LeadID:  "lead", Policy: team.DefaultPolicy(), CreatedAt: now, UpdatedAt: now}
		teamRaw, err := json.Marshal(map[string]*team.Team{statsTeam: teamRow})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "teams.json"), teamRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		run := &team.Run{ID: statsRun, TeamID: statsTeam, Goal: "历史统计目标", Mode: team.ModePipeline,
			Status: team.RunCompleted, Round: 1, PlanHash: "hand-stats", CreatedAt: now, UpdatedAt: now}
		result := "已完成：历史统计结果 STATS-OLD"
		run.Tasks = []team.Task{{
			ID: "t1", MemberID: "lead", Prompt: "历史统计任务 STATS-OLD", Attempt: 1, Ordinal: 0,
			Status: team.TaskCompleted, WriteLock: team.TaskWriteLockNone, Result: result,
			ResultDetail: &team.TaskResult{SessionID: statsSession, TurnID: "turn-stats-1",
				Result: result, ResultChars: len([]rune(result))},
		}}
		runRaw, err := json.Marshal(map[string]*team.Run{statsRun: run})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "team-runs.json"), runRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		// journal：全部事件 createdAt 零值（升级前的历史形态）。
		events := []*team.Event{
			{ID: uuid.NewString(), RunID: statsRun, Type: team.EventRunCreated, Source: "user",
				Payload: map[string]any{"teamId": statsTeam, "goal": run.Goal, "mode": team.ModePipeline, "planHash": "hand-stats", "taskCount": 1}, Sequence: 1},
			{ID: uuid.NewString(), RunID: statsRun, Type: team.EventTaskPlanned, Source: "user",
				Payload: map[string]any{"taskId": "t1", "memberId": "lead"}, Sequence: 2},
			{ID: uuid.NewString(), RunID: statsRun, Type: team.EventTaskDispatching, Source: "runner",
				Payload: map[string]any{"taskId": "t1", "sessionId": statsSession, "submissionId": team.TaskSubmissionID(statsTeam, statsRun, "t1", 1), "attempt": 1}, Sequence: 3},
			{ID: uuid.NewString(), RunID: statsRun, Type: team.EventTaskCompleted, Source: "watcher",
				Payload: map[string]any{"taskId": "t1", "sessionId": statsSession, "turnId": "turn-stats-1", "result": result}, Sequence: 4},
			{ID: uuid.NewString(), RunID: statsRun, Type: team.EventRunCompleted, Source: "runner",
				Payload: map[string]any{"taskCounts": map[string]any{"completed": 1}}, Sequence: 5},
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
		env := reopenTeamIntegrationEnv(t, dataDir)
		stats := env.teamRunStats(t, statsTeam, statsRun)
		if stats["coverage"] != team.StatsCoverageNone {
			t.Fatalf("零时间戳历史 Run 应 coverage=none: %v", stats["coverage"])
		}
		if elapsed, _ := stats["runElapsedMs"].(float64); elapsed != 0 {
			t.Fatalf("无数据不得编造耗时: %v", stats["runElapsedMs"])
		}
		if stats["dispatchCount"] != float64(1) {
			t.Fatalf("派发计数按事件可得: %v", stats["dispatchCount"])
		}
		tasks, _ := stats["tasks"].([]any)
		if len(tasks) != 1 {
			t.Fatalf("应有一条任务条目: %v", tasks)
		}
		stat, _ := tasks[0].(map[string]any)
		if stat["elapsedMs"] != float64(0) {
			t.Fatalf("任务耗时应降级为 0: %v", stat)
		}
	})
}

// ---- 用例 4-10：§11.3 完整阶段图 ----

// TestTeamIntegration4RunGraph：三节点 broadcast 链的聚合形状——每条
// memberIds 条目聚合单节点（memberIds/agentKinds 去重、runCount 恒 1、
// 注意力序归并状态）、依赖映射到节点 ID、Kahn 分层行序、结果摘要就位。
// 纯 memberId 的普通节点形状由 graph 单测覆盖（planner 限制：broadcast
// 模式逐条目 memberIds≥2，单 Run 无法混排普通条目）。运行中窗口断言组内
// 一支完成一支在途 → 聚合节点取在途（注意力序）。
func TestTeamIntegration4RunGraph(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	sessionA := env.createMemberSession(t, "lead-sess", "图会话一")
	sessionB := env.createMemberSession(t, "dev-sess", "图会话二")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+sessionA+
		`","role":"lead"},{"id":"dev","sessionId":"`+sessionB+`","role":"member"}]`)
	runID := env.createRun(t, teamID, `{"goal":"阶段图目标 GRAPH-FLOW","mode":"broadcast","plan":[
		{"id":"prep","memberIds":["lead","dev"],"prompt":"阶段图准备 GRAPH-PREP","writeLock":"none"},
		{"id":"grp","memberIds":["lead","dev"],"prompt":"阶段图广播 GRAPH-GRP HOLDUNTIL:gg","writeLock":"none","dependsOn":["prep"]},
		{"id":"fin","memberIds":["lead","dev"],"prompt":"阶段图收口 GRAPH-FIN","writeLock":"none","dependsOn":["grp"]}]}`)

	// 中间态：广播组一支放行完成、另一支仍持闸门——聚合节点状态按注意力
	// 序取在途（dispatching/running 优先于 completed）。
	if !env.drive(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return inFlightTask(teamTaskOf(t, detail, "grp-lead")) &&
			inFlightTask(teamTaskOf(t, detail, "grp-dev"))
	}) {
		t.Fatal("广播分支未在时限内并行在途")
	}
	releaseAgentTurn(t, dataDir, "gg", sessionA)
	if !awaitCond(t, teamIntegrationWait, func() bool {
		detail := env.runDetail(t, teamID, runID)
		return teamTaskOf(t, detail, "grp-lead")["status"] == "completed" &&
			inFlightTask(teamTaskOf(t, detail, "grp-dev"))
	}) {
		t.Fatal("广播分支未在时限内分出先后")
	}
	graph := env.teamRunGraph(t, teamID, runID)
	nodes, _ := graph["nodes"].([]any)
	nodeOf := func(id string) map[string]any {
		t.Helper()
		for _, row := range nodes {
			node, _ := row.(map[string]any)
			if node != nil && node["id"] == id {
				return node
			}
		}
		t.Fatalf("阶段图缺节点 %s: %v", id, nodes)
		return nil
	}
	if len(nodes) != 3 {
		t.Fatalf("broadcast 组应聚合为单节点（共 3 节点）: %v", nodes)
	}
	grp := nodeOf("grp")
	if members, _ := grp["memberIds"].([]any); len(members) != 2 {
		t.Fatalf("聚合节点应带双成员: %v", grp)
	}
	if kinds, _ := grp["agentKinds"].([]any); len(kinds) == 0 {
		t.Fatalf("聚合节点应带成员类型徽标: %v", grp)
	} else if fmt.Sprint(kinds) != fmt.Sprint([]any{"kimi"}) {
		t.Fatalf("成员类型应解析为 kimi: %v", kinds)
	}
	if grp["runCount"] != float64(1) {
		t.Fatalf("广播组是一次条目的多路执行 runCount=1: %v", grp["runCount"])
	}
	if grp["status"] != "running" && grp["status"] != "dispatching" {
		t.Fatalf("聚合状态按注意力序应取在途分支: %v", grp["status"])
	}
	if deps, _ := grp["dependsOn"].([]any); len(deps) != 1 || deps[0] != "prep" {
		t.Fatalf("聚合节点依赖应映射到节点 ID: %v", grp["dependsOn"])
	}

	// 终态：跑完 → 三节点 completed、分层行序 prep→grp→fin、结果摘要就位。
	releaseAgentTurn(t, dataDir, "gg", sessionB)
	if !env.drive(t, teamIntegrationWait, func() bool {
		return env.runDetail(t, teamID, runID)["status"] == "completed"
	}) {
		t.Fatal("Run 未在时限内完成")
	}
	graph = env.teamRunGraph(t, teamID, runID)
	nodes, _ = graph["nodes"].([]any)
	if len(nodes) != 3 {
		t.Fatalf("终态阶段图仍应 3 节点: %v", nodes)
	}
	for _, id := range []string{"prep", "grp", "fin"} {
		if nodeOf(id)["status"] != "completed" {
			t.Fatalf("节点 %s 应 completed: %v", id, nodeOf(id))
		}
	}
	if head, _ := nodeOf("prep")["resultHead"].(string); !strings.Contains(head, "GRAPH-PREP") {
		t.Fatalf("节点应带结果摘要: %v", nodeOf("prep"))
	}
	layers, _ := graph["layers"].([]any)
	if len(layers) != 3 {
		t.Fatalf("三段依赖应分三层: %v", layers)
	}
	for depth, want := range []string{"prep", "grp", "fin"} {
		layer, _ := layers[depth].([]any)
		if len(layer) != 1 || layer[0] != want {
			t.Fatalf("第 %d 层应只有 %s: %v", depth, want, layers)
		}
	}
	if deps, _ := nodeOf("fin")["dependsOn"].([]any); len(deps) != 1 || deps[0] != "grp" {
		t.Fatalf("下游依赖应指向聚合节点: %v", nodeOf("fin")["dependsOn"])
	}
}
