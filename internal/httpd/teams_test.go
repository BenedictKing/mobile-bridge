package httpd

// teams_test.go 覆盖 /api/teams* 的阶段一契约（docs/team-orchestration-plan.md
// §13.1 httpd 行）：认证/CSRF、校验 400、幂等创建、版本冲突 409、删除拦截、
// 运行中取消与重复取消、interrupted turn 不落 completed、resume 的不再重发、
// changes 游标降级与未知参数容忍、SSE ready→changes 帧序列；以及阶段二增量
// 端点：成员间转发（三态校验/幂等/终态 409）、任务工作区 diff（双截断标记）
// 与清理、AIMD 快照、工作区注册/列表、创建请求的 workspaceId/收集策略校验
// 与 changes meta 新键。QueueAdapter 与 SessionResolver 用 Go 级 fake；取消/
// 中断路径的 turn 由 python 伪 Agent（teamsHoldAgent，hold 到 session/cancel
// 才结束）驱动。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// teamsHoldAgent 起一个可中断的伪 Agent：session/prompt 在后台线程 hold
// （默认 20s），收到 session/cancel 立即以 stopReason=cancelled 收尾；提示词
// 落 prompts.txt 供断言。single 模式（不支持 session/list）——成员归属靠
// manager.Create 后的 live 句柄判定，正好覆盖 teamMemberKind 的免探测路径。
// 提示词含 "ASK" 时（阶段三 decision 用例）：先发一条 session/request_
// permission 反向提问卡（id=q1、question=true、两个选项），等手机端回答
// （结果落 answers.txt）后再走 hold/cancel 收尾——模拟成员把提问桥接在
// permission 上的形态（questions.go 的登记口径）。
func teamsHoldAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys,threading
DIR = "` + dir + `"
LOCK = threading.Lock()
STATE = {"event": threading.Event(), "cancelled": False, "ask": threading.Event(), "answer": None}
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
def handle_prompt(m, ident):
 blocks=(m.get("params") or {}).get("prompt") or []
 text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
 record("prompts.txt", text)
 if "ASK" in text:
  print(json.dumps({"jsonrpc":"2.0","id":"q1","method":"session/request_permission","params":{"sessionPermissionsRequested":[],"question":True,"text":"选择部署环境","options":[{"optionId":"prod","name":"生产","kind":"allow_once"},{"optionId":"stag","name":"预发","kind":"allow_once"}]}}),flush=True)
  STATE["ask"].wait(20)
  record("answers.txt", json.dumps(STATE["answer"]))
 STATE["event"].wait(20)
 stop="cancelled" if STATE["cancelled"] else "end_turn"
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":{"stopReason":stop}}),flush=True)
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  result={"sessionId":"km-team","modes":{"currentModeId":"default","availableModes":[{"id":"default"}]},"configOptions":[]}
 elif method == "session/prompt":
  with LOCK:
   STATE["event"].clear(); STATE["cancelled"]=False
  threading.Thread(target=handle_prompt,args=(m,ident),daemon=True).start()
  continue
 elif method == "session/cancel":
  with LOCK:
   STATE["cancelled"]=True; STATE["event"].set(); STATE["ask"].set()
  result={}
 elif method is None and ident == "q1":
  with LOCK:
   STATE["answer"]=m.get("result"); STATE["ask"].set()
  continue
 else:
  result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// fakeTeamQueueEntry 复刻 submissions 账本条目的 team 侧最小视图。
type fakeTeamQueueEntry struct {
	text   string
	status string
}

// fakeTeamQueue 是 Go 级 QueueAdapter：默认状态可编程（queued=观察等待、
// accepted=进入会话观察、unknown=结果未知），Enqueue 计数供 resume 断言不重发。
type fakeTeamQueue struct {
	mu            sync.Mutex
	entries       map[string]fakeTeamQueueEntry
	defaultStatus string
	enqueueCalls  int
}

func newFakeTeamQueue(status string) *fakeTeamQueue {
	return &fakeTeamQueue{entries: map[string]fakeTeamQueueEntry{}, defaultStatus: status}
}

func (q *fakeTeamQueue) Enqueue(threadID, submissionID, text string) (string, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.enqueueCalls++
	key := threadID + ":" + submissionID
	if prior, ok := q.entries[key]; ok {
		if prior.text != text {
			return "", false, errors.New("同一消息标识不能用于不同内容")
		}
		return prior.status, true, nil
	}
	q.entries[key] = fakeTeamQueueEntry{text: text, status: q.defaultStatus}
	return q.defaultStatus, false, nil
}

func (q *fakeTeamQueue) Submission(threadID, submissionID string) (team.TeamSubmission, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry, ok := q.entries[threadID+":"+submissionID]
	if !ok {
		return team.TeamSubmission{}, false
	}
	return team.TeamSubmission{Status: entry.status}, true
}

func (q *fakeTeamQueue) CancelSubmission(threadID, submissionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := threadID + ":" + submissionID
	entry, ok := q.entries[key]
	if !ok || entry.status != team.SubStatusQueued {
		return errors.New("此消息已离开队列，请查看会话")
	}
	entry.status = team.SubStatusCancelled
	q.entries[key] = entry
	return nil
}

func (q *fakeTeamQueue) enqueueCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.enqueueCalls
}

// fakeTeamResolver 恒返回预置句柄（成员会话）；Cwd 走 manager.CwdFor。
type fakeTeamResolver struct {
	mu      sync.Mutex
	manager *acpruntime.Manager
	session *acpruntime.Session
}

func (r *fakeTeamResolver) Resolve(threadID string) (*acpruntime.Manager, *acpruntime.Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manager, r.session, nil
}

func (r *fakeTeamResolver) Cwd(threadID string) string {
	if r.manager == nil {
		return ""
	}
	return r.manager.CwdFor(threadID)
}

// teamTestEnv 装配带 team 服务的测试网关（端口 8791，featureRequest 惯例）。
type teamTestEnv struct {
	server    *Server
	store     team.TeamStore
	journal   *team.Journal
	token     string
	csrf      string
	queue     *fakeTeamQueue
	resolver  *fakeTeamResolver
	manager   *acpruntime.Manager
	session   *acpruntime.Session
	sessionID string
	dataDir   string
}

// newTeamTestEnv 装配测试网关。withAgent 时挂一个伪 Agent 并创建成员会话；
// queueStatus 控制 fake 账本的默认入账状态。
func newTeamTestEnv(t *testing.T, queueStatus string, withAgent bool) *teamTestEnv {
	t.Helper()
	dataDir := t.TempDir()
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
	env := &teamTestEnv{server: server, store: store, journal: journal, token: token, csrf: csrf, dataDir: dataDir}
	if withAgent {
		manager := acpruntime.NewCommand(teamsHoldAgent(t, dataDir))
		t.Cleanup(func() { manager.Close() })
		acpSession, createErr := manager.Create("sess-team-1", dataDir, "成员会话", 30*time.Second)
		if createErr != nil {
			t.Fatalf("伪 Agent 会话创建失败: %v", createErr)
		}
		server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
		env.manager = manager
		env.session = acpSession
		env.sessionID = acpSession.ID
		env.resolver = &fakeTeamResolver{manager: manager, session: acpSession}
	} else {
		env.resolver = &fakeTeamResolver{}
	}
	env.queue = newFakeTeamQueue(queueStatus)
	server.TeamStore = store
	// 阶段四端点装配（照 main.go：journal 供 /stats 全量重放，schedule 账本
	// 供 GET/PUT /schedule 读写）。
	server.TeamJournal = journal
	server.TeamSchedules = team.LoadScheduleStore(filepath.Join(dataDir, "team-schedules.json"))
	server.TeamFeeds = NewTeamFeedRegistry(journal, store)
	// changes 端点已是长轮询（teams.go）：测试默认把等待上限压到毫秒级，
	// 「游标最新」的用例不至于各挂 12s；长轮询行为本身由
	// TestTeamsRunChangesLongPoll 显式调大等待上限验证。
	server.TeamChangesWait = 20 * time.Millisecond
	server.Teams = team.NewService(store, journal, env.queue, env.resolver)
	server.Teams.OnEvent = server.TeamFeeds.Ingest
	return env
}

func (e *teamTestEnv) request(method, target, body string) *httptest.ResponseRecorder {
	return featureRequest(e.server, e.token, e.csrf, method, target, body)
}

// createTeam 经 HTTP 创建带单个 lead 成员的团队，返回 (teamID, version)。
func (e *teamTestEnv) createTeam(t *testing.T) (string, uint64) {
	t.Helper()
	payload := `{"name":"测试团队","members":[{"id":"lead","sessionId":"` + e.sessionID +
		`","role":"lead","name":"负责人"}],"leadId":"lead"}`
	response := e.request(http.MethodPost, "/api/teams", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("创建团队失败: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID      string `json:"id"`
		Version uint64 `json:"version"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	return created.ID, created.Version
}

func (e *teamTestEnv) createRun(t *testing.T, teamID, body string) string {
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

// runDetail 读取并解码 RunDetail。
func (e *teamTestEnv) runDetail(t *testing.T, teamID, runID string) map[string]any {
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

// waitForRunStatus 轮询 Run 详情直到进入期望状态之一，返回该详情。
func (e *teamTestEnv) waitForRunStatus(t *testing.T, teamID, runID string, statuses ...string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		response := e.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID, "")
		if response.Code == http.StatusOK {
			var run map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &run); err == nil {
				status, _ := run["status"].(string)
				for _, want := range statuses {
					if status == want {
						return run
					}
				}
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	t.Fatalf("Run 未在时限内进入状态 %v", statuses)
	return nil
}

func teamTaskOf(t *testing.T, run map[string]any, taskID string) map[string]any {
	t.Helper()
	tasks, _ := run["tasks"].([]any)
	for _, item := range tasks {
		task, _ := item.(map[string]any)
		if task["id"] == taskID {
			return task
		}
	}
	t.Fatalf("任务不存在: %s", taskID)
	return nil
}

func TestTeamsRouteRequiresAuth(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	// 未登录：GET 列表 401。
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8791/api/teams", nil)
	request.Host = "127.0.0.1:8791"
	request.Header.Set("Origin", "http://127.0.0.1:8791")
	response := httptest.NewRecorder()
	env.server.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应 401: %d %s", response.Code, response.Body.String())
	}
	// 登录但写请求缺 CSRF：403。
	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8791/api/teams", strings.NewReader(`{"name":"x"}`))
	request.Host = "127.0.0.1:8791"
	request.Header.Set("Origin", "http://127.0.0.1:8791")
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: env.token})
	response = httptest.NewRecorder()
	env.server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("缺 CSRF 应 403: %d %s", response.Code, response.Body.String())
	}
	// 未装配团队服务的网关：认证通过后 409 提示升级。
	bare := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8791, "instance")
	token, session, _ := bare.Auth.NewSession("127.0.0.1", "", false)
	csrf, _ := session.MustGet("csrf").(string)
	response = featureRequest(bare, token, csrf, http.MethodGet, "/api/teams", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "请启动支持团队编排的新版网关") {
		t.Fatalf("未装配服务应 409: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamsValidation(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)

	// 坏 member id。
	response := env.request(http.MethodPost, "/api/teams",
		`{"name":"坏成员","members":[{"id":"lead!","sessionId":"`+env.sessionID+`"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "成员 ID 无效") {
		t.Fatalf("坏成员 ID 应 400: %d %s", response.Code, response.Body.String())
	}
	// 非 ACP 会话（解析不到归属）。
	response = env.request(http.MethodPost, "/api/teams",
		`{"name":"非 ACP","members":[{"id":"lead","sessionId":"codex-thread-001"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "阶段一 Team 成员仅支持 ACP Agent 会话") {
		t.Fatalf("非 ACP 会话应 400: %d %s", response.Code, response.Body.String())
	}
	// 策略取值校验：isolated_worktree / autoApplyChanges / 非法整数 /
	// maxResultChars=0（0 会被 ExtractResult 当作不截断，结果 payload 失去
	// 上界，§10.3/§15.3）/ maxTaskRetries 超上限 3（§6.3 受限重试）。
	for _, payload := range []string{
		`{"name":"p1","policy":{"workspaceMode":"isolated_worktree"}}`,
		`{"name":"p2","policy":{"autoApplyChanges":true}}`,
		`{"name":"p3","policy":{"maxParallelTasks":0}}`,
		`{"name":"p4","policy":{"maxResultChars":0}}`,
		`{"name":"p5","policy":{"maxTaskRetries":4}}`,
	} {
		response = env.request(http.MethodPost, "/api/teams", payload)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("非法策略应 400: %s -> %d %s", payload, response.Code, response.Body.String())
		}
	}
	teamID, _ := env.createTeam(t)
	// 列表与详情都带 available 标记（live 会话可达）。
	response = env.request(http.MethodGet, "/api/teams", "")
	var list struct {
		Teams []struct {
			Members []struct {
				Available bool   `json:"available"`
				AgentKind string `json:"agentKind"`
			} `json:"members"`
		} `json:"teams"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Teams) != 1 || len(list.Teams[0].Members) != 1 ||
		!list.Teams[0].Members[0].Available || list.Teams[0].Members[0].AgentKind != "kimi" {
		t.Fatalf("团队列表投影异常: %s", response.Body.String())
	}

	// broadcast 模式（阶段二已支持）：条目必须用 memberIds 指定 ≥2 个成员，
	// 误用 memberId 仍 400。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"并行","mode":"broadcast","plan":[{"id":"a","memberId":"lead","prompt":"x"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "不使用 memberId") {
		t.Fatalf("broadcast 误用 memberId 应 400: %d %s", response.Code, response.Body.String())
	}
	// broadcast 的 memberIds 解析与校验（阶段二）。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"并行","mode":"broadcast","collectPolicy":"first_success","plan":[{"id":"a","memberIds":["lead"],"prompt":"x"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "至少需要 2 个互异成员") {
		t.Fatalf("broadcast memberIds 单成员应 400: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"并行","mode":"broadcast","plan":[{"id":"a","memberIds":["lead",7],"prompt":"x"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "成员列表必须全是成员 ID") {
		t.Fatalf("broadcast memberIds 非法元素应 400: %d %s", response.Code, response.Body.String())
	}
	// first_success 仅 broadcast 允许。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"串行","mode":"pipeline","collectPolicy":"first_success","plan":[{"id":"a","memberId":"lead","prompt":"x"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "first_success 收集策略仅并行分派模式支持") {
		t.Fatalf("pipeline + first_success 应 400: %d %s", response.Code, response.Body.String())
	}
	// 循环依赖：报错含环路径。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", `{"goal":"环形","mode":"pipeline","plan":[
		{"id":"a","memberId":"lead","prompt":"甲","dependsOn":["b"]},
		{"id":"b","memberId":"lead","prompt":"乙","dependsOn":["a"]}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "循环依赖") ||
		!strings.Contains(response.Body.String(), "a → b → a") {
		t.Fatalf("循环依赖应 400 且含环路径: %d %s", response.Code, response.Body.String())
	}
	// 超任务数：默认上限 12 条。
	plan := make([]string, 0, 13)
	for i := 0; i < 13; i++ {
		plan = append(plan, fmt.Sprintf(`{"id":"t%d","memberId":"lead","prompt":"第 %d 条"}`, i, i))
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"超限","mode":"pipeline","plan":[`+strings.Join(plan, ",")+`]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "任务数超过上限") {
		t.Fatalf("超任务数应 400: %d %s", response.Code, response.Body.String())
	}
	// prompt 超余量上限：默认字节口径 100000−12000×3−512=63488（评审遗留②：
	// 创建期与派发期字节校验同源）。22000 个 CJK 字（66000 字节）rune 在
	// 87800 限内、字节超限——多字节内容必须创建即拒。
	long := strings.Repeat("长", 22000)
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"超长","mode":"pipeline","plan":[{"id":"t1","memberId":"lead","prompt":"`+long+`"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "63488") {
		t.Fatalf("prompt 超限应 400 且含字节上限: %d %.200s", response.Code, response.Body.String())
	}
	// goal 自身上限（10000 字）。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"`+strings.Repeat("标", 10001)+`","mode":"single"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "1–10000 字的运行目标") {
		t.Fatalf("goal 超限应 400: %d", response.Code)
	}

	// maxTaskRetries 合法区间（0–3）应接受并回显（0=关闭受限重试）；
	// 放在列表断言之后：前文按「列表恰一个团队」断言投影。
	for _, value := range []string{"0", "3"} {
		response = env.request(http.MethodPost, "/api/teams",
			`{"name":"重试`+value+`","members":[{"id":"lead","sessionId":"`+env.sessionID+`"}],"policy":{"maxTaskRetries":`+value+`}}`)
		var created struct {
			Policy struct {
				MaxTaskRetries int `json:"maxTaskRetries"`
			} `json:"policy"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || response.Code != http.StatusOK {
			t.Fatalf("maxTaskRetries=%s 应接受: %d %s（%v）", value, response.Code, response.Body.String(), err)
		}
		if want, _ := strconv.Atoi(value); created.Policy.MaxTaskRetries != want {
			t.Fatalf("maxTaskRetries=%s 应回显，实际 %d", value, created.Policy.MaxTaskRetries)
		}
	}
}

func TestTeamsCreateIdempotency(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	const requestID = "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	payload := `{"id":"` + requestID + `","name":"幂等团队","members":[]}`
	response := env.request(http.MethodPost, "/api/teams", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("创建失败: %d %s", response.Code, response.Body.String())
	}
	var first struct {
		ID        string `json:"id"`
		Version   uint64 `json:"version"`
		Duplicate bool   `json:"duplicate"`
	}
	json.Unmarshal(response.Body.Bytes(), &first)
	if first.ID != requestID || first.Version != 1 || first.Duplicate {
		t.Fatalf("首次创建: %+v", first)
	}
	// 同 id 同内容重放：返回已建对象 + duplicate:true，版本不动。
	response = env.request(http.MethodPost, "/api/teams", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("重放失败: %d %s", response.Code, response.Body.String())
	}
	var replay struct {
		Version   uint64 `json:"version"`
		Duplicate bool   `json:"duplicate"`
	}
	json.Unmarshal(response.Body.Bytes(), &replay)
	if !replay.Duplicate || replay.Version != 1 {
		t.Fatalf("幂等重放: %+v", replay)
	}
	// 同 id 异内容：400。
	response = env.request(http.MethodPost, "/api/teams",
		`{"id":"`+requestID+`","name":"另一个名字","members":[]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("异内容应 400: %d %s", response.Code, response.Body.String())
	}
	// 坏请求 id。
	response = env.request(http.MethodPost, "/api/teams", `{"id":"not-a-uuid","name":"x","members":[]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "创建请求标识无效") {
		t.Fatalf("坏请求 id 应 400: %d %s", response.Code, response.Body.String())
	}
}

// seedRunTeam 直落一支带 lead 成员的团队（绕过 HTTP 成员校验的 ACP 探测，
// Run 创建链路只依赖 ParsePlan 的成员存在性检查）。注册收尾清理：取消该
// 团队全部 Run，让调度循环先于 t.TempDir 的目录清理退出（否则循环写盘撞
// 上目录删除，测试收尾报 directory not empty）。
func (e *teamTestEnv) seedRunTeam(t *testing.T) string {
	t.Helper()
	teamRow := &team.Team{
		ID: "team-run-idem", Name: "幂等 Run 团队", LeadID: "lead",
		Policy:  team.DefaultPolicy(),
		Members: []team.TeamMember{{ID: "lead", SessionID: "sess-team-1", AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
	}
	if err := e.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, run := range e.store.RunsByTeam(teamRow.ID) {
			_, _ = e.server.Teams.Cancel(run.ID)
		}
	})
	return teamRow.ID
}

// TestTeamsRunCreateIdempotency Run 创建的幂等键（评审遗留③）：同 id 同内容
// 重放返回已建 Run + duplicate:true（网络重试不产生重复 Run）；异内容
// （goal 或 verifyCommand 变化）400；无 id 走原行为。
func TestTeamsRunCreateIdempotency(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	const requestID = "3bbbbbbb-cccc-4ddd-8eee-ffffffffffff"
	payload := `{"id":"` + requestID + `","goal":"幂等运行","mode":"single","verifyCommand":["make","check"]}`
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("创建失败: %d %s", response.Code, response.Body.String())
	}
	var first struct {
		ID            string   `json:"id"`
		Duplicate     bool     `json:"duplicate"`
		VerifyCommand []string `json:"verifyCommand"`
	}
	json.Unmarshal(response.Body.Bytes(), &first)
	if first.ID != requestID || first.Duplicate {
		t.Fatalf("首次创建应回显请求 id 且无 duplicate 标记: %+v", first)
	}
	if len(first.VerifyCommand) != 2 || first.VerifyCommand[0] != "make" || first.VerifyCommand[1] != "check" {
		t.Fatalf("verifyCommand 应回显: %v", first.VerifyCommand)
	}
	// 同 id 同内容重放：已建 Run + duplicate:true，不重复创建。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", payload)
	if response.Code != http.StatusOK {
		t.Fatalf("重放失败: %d %s", response.Code, response.Body.String())
	}
	var replay struct {
		ID        string `json:"id"`
		Duplicate bool   `json:"duplicate"`
	}
	json.Unmarshal(response.Body.Bytes(), &replay)
	if !replay.Duplicate || replay.ID != requestID {
		t.Fatalf("幂等重放: %+v", replay)
	}
	if runs := env.store.RunsByTeam(teamID); len(runs) != 1 {
		t.Fatalf("重放不得创建重复 Run，实际 %d 条", len(runs))
	}
	// 同 id 异内容（goal 变化）→ 400。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"id":"`+requestID+`","goal":"另一个目标","mode":"single"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("异内容应 400: %d %s", response.Code, response.Body.String())
	}
	// 同 id 异 verifyCommand → 400。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"id":"`+requestID+`","goal":"幂等运行","mode":"single","verifyCommand":["pytest"]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("异 verifyCommand 应 400: %d %s", response.Code, response.Body.String())
	}
	// 坏请求 id。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"id":"not-a-uuid","goal":"目标","mode":"single"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "创建请求标识无效") {
		t.Fatalf("坏请求 id 应 400: %d %s", response.Code, response.Body.String())
	}
	// 无 id 创建不受影响（服务端生成 uuid）。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", `{"goal":"普通运行","mode":"single"}`)
	var plain struct {
		ID string `json:"id"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &plain) != nil || plain.ID == "" || plain.ID == requestID {
		t.Fatalf("无 id 创建应生成新 Run: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsRunCreateVerifyCommandValidation 验证命令的创建期校验（§6.4.1/
// §16.8：固定 argv 白名单，禁任意 shell 字符串）映射 400。
func TestTeamsRunCreateVerifyCommandValidation(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	cases := []struct {
		payload string
		wantErr string
	}{
		{`{"goal":"x","mode":"single","verifyCommand":"make check"}`, "必须是字符串数组"},
		{`{"goal":"x","mode":"single","verifyCommand":["make",7]}`, "必须是字符串数组"},
		{`{"goal":"x","mode":"single","verifyCommand":["sh","-c","make check"]}`, "不在允许清单内"},
		{`{"goal":"x","mode":"single","verifyCommand":["bash","-c","true"]}`, "不在允许清单内"},
		{`{"goal":"x","mode":"single","verifyCommand":["make","a|b"]}`, "不支持 shell 语法"},
		{`{"goal":"x","mode":"single","verifyCommand":["make","a","b","c","d","e","f","g","h"]}`, "最多 8 段"},
	}
	for _, testCase := range cases {
		response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", testCase.payload)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("%s: 应 400 且含 %q: %d %s", testCase.payload, testCase.wantErr, response.Code, response.Body.String())
		}
	}
	// 空数组视为未声明闸门：正常创建。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"x","mode":"single","verifyCommand":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("空 verifyCommand 应视为未声明: %d %s", response.Code, response.Body.String())
	}
}

// failingTeamStore 把指定写路径变成确定性 IO 失败（评审遗留①的注入面：
// 磁盘满/权限等账本故障在单测里以错误注入模拟，读路径原样透传）。
type failingTeamStore struct {
	team.TeamStore
	failSaveTeam bool
	failSaveRun  bool
}

func (s *failingTeamStore) SaveTeam(row *team.Team) error {
	if s.failSaveTeam {
		return errors.New("模拟磁盘已满")
	}
	return s.TeamStore.SaveTeam(row)
}

func (s *failingTeamStore) SaveRun(run *team.Run) error {
	if s.failSaveRun {
		return errors.New("模拟磁盘已满")
	}
	return s.TeamStore.SaveRun(run)
}

// TestTeamsStoreIOErrorMaps500 账本 IO 故障映射 500『服务内部错误』（评审
// 遗留①：曾被统一映射 409，误导用户以为重试可解）；版本哨兵仍走 409。
func TestTeamsStoreIOErrorMaps500(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	version := env.store.Team(teamID).Version
	broken := &failingTeamStore{TeamStore: env.store, failSaveTeam: true, failSaveRun: true}
	env.server.TeamStore = broken
	env.server.Teams = team.NewService(broken, env.journal, env.queue, env.resolver)
	env.server.Teams.OnEvent = env.server.TeamFeeds.Ingest

	// PATCH 团队（SaveTeam IO 失败）→ 500 固定文案，真实原因只进日志。
	response := env.request(http.MethodPatch, "/api/teams/"+teamID,
		fmt.Sprintf(`{"name":"改名","version":%d}`, version))
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "服务内部错误") {
		t.Fatalf("账本 IO 应 500: %d %s", response.Code, response.Body.String())
	}
	// 创建 Run（CreateRun→SaveRun IO 失败）→ 500。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", `{"goal":"目标","mode":"single"}`)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "服务内部错误") {
		t.Fatalf("Run 创建的账本 IO 应 500: %d %s", response.Code, response.Body.String())
	}
	// 版本哨兵不受影响：仍 409『版本冲突』。
	response = env.request(http.MethodPatch, "/api/teams/"+teamID, `{"name":"冲突","version":999}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "版本冲突") {
		t.Fatalf("版本冲突应保持 409: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamsVersionConflicts(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	response := env.request(http.MethodPost, "/api/teams", `{"name":"版本团队","members":[]}`)
	var team struct {
		ID      string `json:"id"`
		Version uint64 `json:"version"`
	}
	json.Unmarshal(response.Body.Bytes(), &team)

	// 缺版本号 / 非整数 / 版本不符。
	for _, payload := range []string{
		`{"name":"新名字"}`,
		`{"name":"新名字","version":"x"}`,
		`{"name":"新名字","version":99}`,
	} {
		response = env.request(http.MethodPatch, "/api/teams/"+team.ID, payload)
		if response.Code != http.StatusBadRequest && response.Code != http.StatusConflict {
			t.Fatalf("版本校验应 400/409: %s -> %d %s", payload, response.Code, response.Body.String())
		}
	}
	// 正确版本的重命名。
	response = env.request(http.MethodPatch, "/api/teams/"+team.ID,
		fmt.Sprintf(`{"name":"改名了","version":%d}`, team.Version))
	if response.Code != http.StatusOK {
		t.Fatalf("PATCH 应 200: %d %s", response.Code, response.Body.String())
	}
	var updated struct {
		Version uint64 `json:"version"`
		Name    string `json:"name"`
	}
	json.Unmarshal(response.Body.Bytes(), &updated)
	if updated.Name != "改名了" || updated.Version != team.Version+1 {
		t.Fatalf("PATCH 结果: %+v", updated)
	}
	// 成员路由的版本冲突与 404。
	response = env.request(http.MethodPatch, "/api/teams/"+team.ID+"/members/lead", `{"version":1,"role":"reviewer"}`)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "成员不存在") {
		t.Fatalf("未知成员应 404: %d %s", response.Code, response.Body.String())
	}
	// DELETE team：无版本参数时按当前版本删除。
	response = env.request(http.MethodDelete, "/api/teams/"+team.ID, `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE 应 200: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+team.ID, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("删除后应 404: %d", response.Code)
	}
}

func TestTeamsDeleteBlockedByActiveRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true) // queued：watcher 挂在队列观察，Run 停在 dispatching
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"删除拦截","mode":"single"}`)
	env.waitForRunStatus(t, teamID, runID, "dispatching")

	// 有非终态 Run：DELETE 409。
	response := env.request(http.MethodDelete, "/api/teams/"+teamID, `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "未结束的 Run") {
		t.Fatalf("活动 Run 应拦截删除: %d %s", response.Code, response.Body.String())
	}
	// 取消 Run 后可删。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodDelete, "/api/teams/"+teamID, `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("取消后删除应 200: %d %s", response.Code, response.Body.String())
	}
	if env.store.Team(teamID) != nil {
		t.Fatal("团队应已删除")
	}
}

func TestTeamsCancelRunningRun(t *testing.T) {
	env := newTeamTestEnv(t, "accepted", true)
	teamID, _ := env.createTeam(t)
	const taskPrompt = "hold 运行中取消"
	// 先在成员会话里制造本任务的运行中 turn（sentText=任务本体：无依赖时
	// 不带 [Team context] 信封），再创建 Run——fake 账本直接报 accepted，
	// watcher 进入会话观察并匹配到该 turn。
	if err := env.session.PromptWithBlocks(taskPrompt, nil, 60*time.Second); err != nil {
		t.Fatalf("制造运行中 turn 失败: %v", err)
	}
	runID := env.createRun(t, teamID, `{"goal":"运行中取消","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"`+taskPrompt+`","writeLock":"exclusive"}]}`)
	// 任务进入 running（watcher 匹配到 turn）并持有写锁投影。
	deadline := time.Now().Add(8 * time.Second)
	running := false
	for time.Now().Before(deadline) && !running {
		detail := env.runDetail(t, teamID, runID)
		task := teamTaskOf(t, detail, "t1")
		if task["status"] == "running" {
			running = true
			workspace, _ := detail["workspace"].(map[string]any)
			if workspace["writeLockHeldBy"] != "t1" {
				t.Fatalf("运行中写锁持有者应为 t1: %v", workspace)
			}
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	if !running {
		t.Fatal("任务未在时限内进入 running")
	}
	// POST cancel：任务与 Run 均 cancelled，重复取消 409。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	var cancelled map[string]any
	json.Unmarshal(response.Body.Bytes(), &cancelled)
	if cancelled["status"] != "cancelled" {
		t.Fatalf("Run 应 cancelled: %s", response.Body.String())
	}
	task := teamTaskOf(t, cancelled, "t1")
	if task["status"] != "cancelled" {
		t.Fatalf("任务应 cancelled: %v", task["status"])
	}
	if workspace, _ := cancelled["workspace"].(map[string]any); workspace["writeLockHeldBy"] != nil {
		t.Fatalf("取消后不应有写锁持有者: %v", workspace)
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "运行已结束") {
		t.Fatalf("重复取消应 409: %d %s", response.Code, response.Body.String())
	}
	// 伪 Agent 侧：turn 被真正取消（interrupted），提示词已落盘。
	for time.Now().Before(deadline) {
		snapshot := env.session.Snapshot()
		turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
		if turns != nil && len(turns.Items) > 0 {
			turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
			if status, _ := turn.MustGet("status").(string); status == "interrupted" {
				break
			}
		}
		time.Sleep(120 * time.Millisecond)
	}
	raw, readErr := os.ReadFile(filepath.Join(env.dataDir, "prompts.txt"))
	if readErr != nil || !strings.Contains(string(raw), taskPrompt) {
		t.Fatalf("伪 Agent 应收到提示词: err=%v", readErr)
	}

	// 取消必须释放项目写锁（曾为高危缺陷：锁残留会让同 cwd 的后续
	// exclusive 任务永远停在 planned，且投影显示无持有者掩盖死锁）。取消后
	// 同成员的 exclusive 任务必须能派发并持锁（fake 账本 accepted → 任务
	// dispatching；阶段二起派发经团队派发池异步执行，run 落 dispatching 与
	// 任务实际派发之间有窗口，按 deadline 轮询投影而非立即断言）。
	secondRun := env.createRun(t, teamID, `{"goal":"取消后重建","mode":"pipeline","plan":[
		{"id":"t2","memberId":"lead","prompt":"hold 取消后的写任务","writeLock":"exclusive"}]}`)
	env.waitForRunStatus(t, teamID, secondRun, "dispatching")
	held := false
	for time.Now().Before(deadline) && !held {
		detail := env.runDetail(t, teamID, secondRun)
		if workspace, _ := detail["workspace"].(map[string]any); workspace["writeLockHeldBy"] == "t2" {
			held = true
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	if !held {
		detail := env.runDetail(t, teamID, secondRun)
		t.Fatalf("新任务的写锁持有者应为 t2: %v", detail["workspace"])
	}
}

func TestTeamsWatcherInterruptedTurnNotCompleted(t *testing.T) {
	env := newTeamTestEnv(t, "accepted", true)
	teamID, _ := env.createTeam(t)
	const taskPrompt = "hold 观察中断"
	if err := env.session.PromptWithBlocks(taskPrompt, nil, 60*time.Second); err != nil {
		t.Fatalf("制造运行中 turn 失败: %v", err)
	}
	runID := env.createRun(t, teamID, `{"goal":"`+taskPrompt+`","mode":"single"}`)
	env.waitForRunStatus(t, teamID, runID, "dispatching")
	// 用户在原会话手动停止（与 run 取消无关的中断）：watcher 应把任务归
	// stopped（绝不记 completed），Run fold 为 stopped。
	if err := env.session.Cancel(); err != nil {
		t.Fatalf("中断 turn 失败: %v", err)
	}
	final := env.waitForRunStatus(t, teamID, runID, "stopped")
	task := teamTaskOf(t, final, "lead")
	if task["status"] != "stopped" {
		t.Fatalf("中断任务应 stopped 而非 %v", task["status"])
	}
	if task["result"] != nil {
		t.Fatalf("中断任务不应携带结果: %v", task["result"])
	}
}

func TestTeamsResumeWaitingInput(t *testing.T) {
	env := newTeamTestEnv(t, "unknown", true) // 账本 unknown + 会话空闲 → 结果未知暂停
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"结果未知暂停","mode":"single"}`)
	waiting := env.waitForRunStatus(t, teamID, runID, "waiting_input")
	task := teamTaskOf(t, waiting, "lead")
	if task["status"] != "outcome_unknown" {
		t.Fatalf("任务应 outcome_unknown: %v", task["status"])
	}
	// resume：未决 unknown 转 stopped（不重发），无可派发分支 → fold stopped。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("resume 失败: %d %s", response.Code, response.Body.String())
	}
	var resumed map[string]any
	json.Unmarshal(response.Body.Bytes(), &resumed)
	if resumed["status"] != "stopped" {
		t.Fatalf("resume 后 Run 应 stopped: %s", response.Body.String())
	}
	task = teamTaskOf(t, resumed, "lead")
	if task["status"] != "stopped" {
		t.Fatalf("未决 unknown 应转 stopped: %v", task["status"])
	}
	if reason, _ := task["stopReason"].(string); !strings.Contains(reason, "不再重发") {
		t.Fatalf("stopReason 应说明不再重发: %v", task["stopReason"])
	}
	if count := env.queue.enqueueCount(); count != 1 {
		t.Fatalf("resume 不得重发任务：enqueue 次数=%d", count)
	}
	// 非 waiting_input 的 resume：409。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "等待用户处理") {
		t.Fatalf("非暂停 resume 应 409: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamsRunChangesCursor(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"游标","mode":"single"}`)
	// 等 run_created/task_planned/task_dispatching 三条事件入投影。
	deadline := time.Now().Add(8 * time.Second)
	var page map[string]any
	for time.Now().Before(deadline) {
		response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
		if response.Code != http.StatusOK {
			t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
		}
		page = map[string]any{}
		json.Unmarshal(response.Body.Bytes(), &page)
		if rows, _ := page["rows"].([]any); len(rows) >= 3 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	if rows, _ := page["rows"].([]any); len(rows) < 3 {
		t.Fatalf("事件不足: %s", page)
	}
	epoch, _ := page["epoch"].(string)
	sequence, _ := page["sequence"].(float64)
	if epoch == "" || sequence < 3 {
		t.Fatalf("epoch/sequence 异常: %v %v", epoch, sequence)
	}
	if isReset, _ := page["reset"].(bool); !isReset {
		t.Fatalf("无游标首轮应 reset 整页: %s", page)
	}
	// 增量读取：带 start= 与未知参数不得报错（transport.js 恒带 start:''）。
	response := env.request(http.MethodGet,
		fmt.Sprintf("/api/teams/%s/runs/%s/changes?after=%d&epoch=%s&start=&foo=bar", teamID, runID, int(sequence), epoch), "")
	if response.Code != http.StatusOK {
		t.Fatalf("增量读取应 200: %d %s", response.Code, response.Body.String())
	}
	page = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &page)
	if rows, _ := page["rows"].([]any); len(rows) != 0 {
		t.Fatalf("最新游标的增量应为空: %s", page)
	}
	if _, hasReset := page["reset"]; hasReset {
		t.Fatalf("最新游标不应 reset: %s", page)
	}
	// meta 注入 Run 状态摘要。
	meta, _ := page["meta"].(map[string]any)
	if meta == nil || meta["status"] == nil || meta["workspace"] == nil {
		t.Fatalf("meta 应含 status/workspace: %v", meta)
	}
	// 游标滑出/未知 after：reset 整页。
	response = env.request(http.MethodGet,
		fmt.Sprintf("/api/teams/%s/runs/%s/changes?after=%d&epoch=%s", teamID, runID, int(sequence)+1000, epoch), "")
	if response.Code != http.StatusOK {
		t.Fatalf("未知游标读取应 200: %d", response.Code)
	}
	page = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &page)
	if isReset, _ := page["reset"].(bool); !isReset {
		t.Fatalf("未知游标应 reset: %s", page)
	}
}

// TestTeamsRunChangesLongPoll：changes 是长轮询（sessions.go
// sessionRouteChanges 同款）——游标最新时请求挂到「有新事件或等待上限」，
// 不再立即返回空页（否则轮询传输下每个打开的 Run 页都是稳定 5 QPS）；
// 挂起中的请求被事件投递（EventFeed.Notify）提前唤醒。
func TestTeamsRunChangesLongPoll(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	env.server.TeamChangesWait = 600 * time.Millisecond
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"长轮询","mode":"single"}`)
	// 取当前游标（等 run_created/task_planned/task_dispatching 三条事件）。
	deadline := time.Now().Add(8 * time.Second)
	var page map[string]any
	for time.Now().Before(deadline) {
		response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
		if response.Code != http.StatusOK {
			t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
		}
		page = map[string]any{}
		json.Unmarshal(response.Body.Bytes(), &page)
		if rows, _ := page["rows"].([]any); len(rows) >= 3 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	epoch, _ := page["epoch"].(string)
	sequence, _ := page["sequence"].(float64)
	if epoch == "" || sequence < 3 {
		t.Fatalf("游标异常: %v", page)
	}
	changesURL := fmt.Sprintf("/api/teams/%s/runs/%s/changes?after=%d&epoch=%s",
		teamID, runID, int(sequence), epoch)

	// ① 无变更：挂到等待上限才返回，且为空增量。
	start := time.Now()
	response := env.request(http.MethodGet, changesURL, "")
	if response.Code != http.StatusOK {
		t.Fatalf("无变更读取应 200: %d %s", response.Code, response.Body.String())
	}
	page = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &page)
	if rows, _ := page["rows"].([]any); len(rows) != 0 {
		t.Fatalf("无变更的等待到点应返回空增量: %v", page)
	}
	if elapsed := time.Since(start); elapsed < 500*time.Millisecond {
		t.Fatalf("游标最新时应挂到等待上限（600ms），实际 %v 即返回", elapsed)
	}

	// ② 挂起中的请求被新事件提前唤醒：先让请求入园，再取消 Run（追加
	// task_stopped/run_cancelled 事件 → EventFeed.Add → Notify）。
	type pollResult struct {
		code int
		body string
	}
	done := make(chan pollResult, 1)
	go func() {
		parked := env.request(http.MethodGet, changesURL, "")
		done <- pollResult{parked.Code, parked.Body.String()}
	}()
	// 园内确认：等一小段确保请求已挂起（即便竞态下请求晚于事件到达，
	// 入口判据也会立即带增量返回，断言不受影响）。
	time.Sleep(100 * time.Millisecond)
	start = time.Now()
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	select {
	case got := <-done:
		// 取消按序落 task_stopped → run_cancelled 两条事件；挂起的请求在
		// 首条新事件到达即被唤醒返回（客户端随后自会续读下一条增量）。
		if got.code != http.StatusOK || !strings.Contains(got.body, "task_stopped") {
			t.Fatalf("挂起的请求应被变更唤醒并携带增量: %d %s", got.code, got.body)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("事件到达后应立即唤醒，实际等了 %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("长轮询未在时限内被变更唤醒")
	}
}

func TestTeamsRunEventsSSE(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
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

	teamResponse := authorized(http.MethodPost, "/api/teams",
		`{"name":"SSE 团队","members":[{"id":"lead","sessionId":"`+env.sessionID+`","role":"lead"}],"leadId":"lead"}`)
	body, _ := io.ReadAll(teamResponse.Body)
	teamResponse.Body.Close()
	var teamRow struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &teamRow)
	runResponse := authorized(http.MethodPost, "/api/teams/"+teamRow.ID+"/runs", `{"goal":"SSE","mode":"single"}`)
	body, _ = io.ReadAll(runResponse.Body)
	runResponse.Body.Close()
	var runRow struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &runRow)

	events := authorized(http.MethodGet,
		"/api/teams/"+teamRow.ID+"/runs/"+runRow.ID+"/events?incremental=1", "")
	if events.StatusCode != http.StatusOK {
		t.Fatalf("events 应 200: %d", events.StatusCode)
	}

	type sseFrame struct {
		event string
		data  string
		id    string
	}
	frames := make(chan sseFrame, 16)
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
	readyTimeout := time.After(8 * time.Second)
	var readyFrame sseFrame
	select {
	case readyFrame = <-frames:
	case <-readyTimeout:
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	if readyFrame.event != "ready" || !strings.Contains(readyFrame.data, "epoch") {
		t.Fatalf("ready 帧异常: %+v", readyFrame)
	}
	// 触发变更（取消 Run）→ changes/reset 帧携带事件与 epoch.sequence 帧 id。
	cancel := authorized(http.MethodPost,
		"/api/teams/"+teamRow.ID+"/runs/"+runRow.ID+"/cancel", "{}")
	io.ReadAll(cancel.Body)
	cancel.Body.Close()

	changeTimeout := time.After(8 * time.Second)
	sawEvent := ""
	for sawEvent == "" {
		select {
		case frame := <-frames:
			if frame.event == "changes" || frame.event == "reset" {
				if strings.Contains(frame.data, "run_cancelled") {
					sawEvent = frame.event
					if !strings.Contains(frame.id, ".") {
						t.Fatalf("帧 id 应为 epoch.sequence: %q", frame.id)
					}
				}
			}
		case <-changeTimeout:
			t.Fatal("SSE 未在时限内发出携带 run_cancelled 的帧")
		}
	}
	// 收流结束：先断开订阅再关服务器（Close 会等待 handler 退出）。
	events.Body.Close()
	<-readerDone
	api.Close()
}

// TestTeamsRunEventsSSEStaleSinceReplaysBacklog：重连携带落后的 since 游标
// （手机锁屏/断网期间事件已落定）时，头部与 since 之间的积压必须立即冲给
// 客户端——变更判定基准是客户端游标而非订阅时刻的服务端头部；对已终态、
// 此后不再有新事件的 Run，这次积压冲刷是唯一出口（否则 Run 页永远显示旧
// 状态、断线 chip 滞留，transport 的 wake 重订阅带同一 stale since 无法自愈）。
func TestTeamsRunEventsSSEStaleSinceReplaysBacklog(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
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

	teamResponse := authorized(http.MethodPost, "/api/teams",
		`{"name":"SSE 积压团队","members":[{"id":"lead","sessionId":"`+env.sessionID+`","role":"lead"}],"leadId":"lead"}`)
	body, _ := io.ReadAll(teamResponse.Body)
	teamResponse.Body.Close()
	var teamRow struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &teamRow)
	runResponse := authorized(http.MethodPost, "/api/teams/"+teamRow.ID+"/runs", `{"goal":"SSE backlog","mode":"single"}`)
	body, _ = io.ReadAll(runResponse.Body)
	runResponse.Body.Close()
	var runRow struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &runRow)

	// 首次连接：从 ready 帧取当前 epoch（sequence 落后到 0 模拟陈旧游标）。
	first := authorized(http.MethodGet,
		"/api/teams/"+teamRow.ID+"/runs/"+runRow.ID+"/events?incremental=1", "")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("events 应 200: %d", first.StatusCode)
	}
	type sseFrame struct {
		event string
		data  string
		id    string
	}
	frames := make(chan sseFrame, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(first.Body)
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
	var readyFrame sseFrame
	select {
	case readyFrame = <-frames:
	case <-time.After(8 * time.Second):
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	first.Body.Close()
	<-readerDone
	var ready struct {
		Epoch    string `json:"epoch"`
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal([]byte(readyFrame.data), &ready); err != nil || ready.Epoch == "" || ready.Sequence == 0 {
		t.Fatalf("ready 帧应携带 epoch/sequence: %q (%v)", readyFrame.data, err)
	}

	// 重连携带 since=epoch.0（落后于头部）：积压必须立即冲刷——没有任何新
	// 事件会到来（等一条 12s 心跳周期之外的帧），唯一出口是订阅即发增量。
	stale := authorized(http.MethodGet,
		"/api/teams/"+teamRow.ID+"/runs/"+runRow.ID+"/events?incremental=1&since="+
			url.QueryEscape(ready.Epoch+".0"), "")
	defer stale.Body.Close()
	staleFrames := make(chan sseFrame, 16)
	staleDone := make(chan struct{})
	go func() {
		defer close(staleDone)
		reader := bufio.NewReader(stale.Body)
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
					staleFrames <- sseFrame{event: event, data: data, id: id}
				}
				event, data, id = "", "", ""
			}
		}
	}()
	// ready 帧总是先行（携带当前 epoch/头部序号），跳过它再等积压帧。
	deadline := time.After(3 * time.Second)
	for {
		var frame sseFrame
		select {
		case frame = <-staleFrames:
		case <-deadline:
			// 3s < 12s 心跳周期：等不到帧说明积压被吞（旧实现只在「订阅时刻
			// 之后的新事件」才下发 changes）。
			t.Fatal("陈旧 since 重连未在时限内收到积压帧")
		}
		if frame.event == "ready" {
			continue
		}
		if frame.event != "changes" && frame.event != "reset" {
			t.Fatalf("陈旧游标重连的第一帧应是 changes/reset: %+v", frame)
		}
		if !strings.Contains(frame.data, "run_created") {
			t.Fatalf("积压帧应携带创建以来的事件行: %s", frame.data)
		}
		if !strings.Contains(frame.id, ".") || frame.id == ready.Epoch+".0" {
			t.Fatalf("帧 id 应推进到当前头部游标: %q", frame.id)
		}
		break
	}
	stale.Body.Close()
	<-staleDone
	api.Close()
}

// ---- 阶段二增量端点（forward/diff/cleanup/aimd/workspaces/meta）----

// stubTranscriptSource 是 TranscriptSource 的测试桩：任意会话返回固定正文。
type stubTranscriptSource struct{}

func (stubTranscriptSource) BoundedTranscript(sessionID, scope string) (string, bool, error) {
	return "成员会话的转录内容", false, nil
}

// seedForwardTeam 直落一支两成员团队（lead 结果源 + helper 转发目标，另带
// 一个停用成员）并登记收尾清理。
func (e *teamTestEnv) seedForwardTeam(t *testing.T) string {
	t.Helper()
	teamRow := &team.Team{
		ID: "team-forward", Name: "转发团队", LeadID: "lead", Policy: team.DefaultPolicy(),
		Members: []team.TeamMember{
			{ID: "lead", SessionID: "sess-team-1", AgentKind: "kimi", Role: team.RoleLead, Enabled: true},
			{ID: "helper", SessionID: "sess-team-2", AgentKind: "kimi", Role: team.RoleMember, Enabled: true},
			{ID: "sleepy", SessionID: "sess-team-3", AgentKind: "kimi", Role: team.RoleMember, Enabled: false},
		},
	}
	if err := e.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, run := range e.store.RunsByTeam(teamRow.ID) {
			_, _ = e.server.Teams.Cancel(run.ID)
		}
	})
	return teamRow.ID
}

// forceRunUpdate 在测试里直改 Run 快照（乐观版本与调度循环并发写冲突时重读
// 重试；收尾清理由各测试自行登记）。
func (e *teamTestEnv) forceRunUpdate(t *testing.T, runID string, mutate func(*team.Run)) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run := e.store.Run(runID)
		if run == nil {
			t.Fatalf("run 不存在: %s", runID)
		}
		mutate(run)
		if err := e.store.SaveRun(run); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run 更新在时限内未成功: %s", runID)
}

// gitRun 在 dir 下执行 git 子命令并返回去空白 stdout（fixture 辅助）。
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v 失败: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// TestTeamsRunForward 成员间转发的三态校验、幂等与终态拒绝（§7）。
func TestTeamsRunForward(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedForwardTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"转发","mode":"pipeline","plan":[
		{"id":"t1","memberId":"lead","prompt":"甲"},{"id":"t2","memberId":"helper","prompt":"乙"}]}`)
	// t1 标记 completed（转发来源要求已完成的任务）；t2 保持 planned——
	// exclusive 任务无 cwd 不会被派发，Run 停在非终态。
	env.forceRunUpdate(t, runID, func(run *team.Run) {
		for i := range run.Tasks {
			if run.Tasks[i].ID == "t1" {
				run.Tasks[i].Status = team.TaskCompleted
				run.Tasks[i].ResultDetail = &team.TaskResult{
					SessionID: "sess-team-1", TurnID: "turn-1",
					Result: "完成的结果内容", ResultChars: len([]rune("完成的结果内容")),
				}
			}
		}
	})
	target := "/api/teams/" + teamID + "/runs/" + runID + "/forward"
	const requestID = "4ccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"

	// 会话源未装配 TranscriptSource → 409（先于 scope 校验）。
	response := env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","sourceSessionId":"sess-team-1","scope":"main","targetMemberId":"helper"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "网关未装配会话转录提取") {
		t.Fatalf("未装配转录源应 409: %d %s", response.Code, response.Body.String())
	}
	env.server.Teams.Transcripts = stubTranscriptSource{}

	// 参数校验族（400）。
	for _, testCase := range []struct{ payload, wantErr string }{
		{`{"sourceTaskId":"t1","targetMemberId":"helper"}`, "缺少转发请求标识"},
		{`{"id":"not-a-uuid","sourceTaskId":"t1","targetMemberId":"helper"}`, "转发请求标识无效"},
		{`{"id":"` + requestID + `","targetMemberId":"helper"}`, "二选一"},
		{`{"id":"` + requestID + `","sourceTaskId":"t1","sourceSessionId":"sess-team-1","targetMemberId":"helper"}`, "二选一"},
		{`{"id":"` + requestID + `","sourceTaskId":"t1","scope":"main","targetMemberId":"helper"}`, "任务来源不支持 scope"},
		{`{"id":"` + requestID + `","sourceTaskId":"nope","targetMemberId":"helper"}`, "来源任务不存在"},
		{`{"id":"` + requestID + `","sourceTaskId":"t2","targetMemberId":"helper"}`, "来源任务尚未完成"},
		{`{"id":"` + requestID + `","sourceTaskId":"t1","targetMemberId":"ghost"}`, "目标成员不存在"},
		{`{"id":"` + requestID + `","sourceTaskId":"t1","targetMemberId":"sleepy"}`, "目标成员已停用"},
		{`{"id":"` + requestID + `","sourceSessionId":"sess-team-1","scope":"bogus","targetMemberId":"helper"}`, "未知的转录范围"},
	} {
		response = env.request(http.MethodPost, target, testCase.payload)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("%s: 应 400 且含 %q: %d %s", testCase.payload, testCase.wantErr,
				response.Code, response.Body.String())
		}
	}

	// 任务源转发：200 {submissionId, duplicate:false}，目标成员会话入队。
	response = env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","sourceTaskId":"t1","targetMemberId":"helper","note":"请接手"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("转发应 200: %d %s", response.Code, response.Body.String())
	}
	var forwarded struct {
		SubmissionID string `json:"submissionId"`
		Duplicate    bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded.SubmissionID == "" || forwarded.Duplicate {
		t.Fatalf("首次转发: %+v", forwarded)
	}
	if submission, ok := env.queue.Submission("sess-team-2", forwarded.SubmissionID); !ok || submission.Status != team.SubStatusQueued {
		t.Fatalf("目标成员会话应入队: %+v ok=%v", submission, ok)
	}
	env.queue.mu.Lock()
	forwardText := env.queue.entries["sess-team-2:"+forwarded.SubmissionID].text
	env.queue.mu.Unlock()
	if !strings.Contains(forwardText, "[Team context]") || !strings.Contains(forwardText, "完成的结果内容") ||
		!strings.Contains(forwardText, "请接手") {
		t.Fatalf("转发信封应含来源结果与指派语:\n%s", forwardText)
	}
	enqueueCount := env.queue.enqueueCount()

	// 同 id 重放：duplicate:true，零副作用（不再入队）。
	response = env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","sourceTaskId":"t1","targetMemberId":"helper","note":"请接手"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重放应 200: %d %s", response.Code, response.Body.String())
	}
	json.Unmarshal(response.Body.Bytes(), &forwarded)
	if !forwarded.Duplicate {
		t.Fatalf("重放应 duplicate: %+v", forwarded)
	}
	if count := env.queue.enqueueCount(); count != enqueueCount {
		t.Fatalf("重放不得重复入队：enqueue 次数 %d → %d", enqueueCount, count)
	}
	// 同 id 异内容：400。
	response = env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","sourceTaskId":"t2","targetMemberId":"helper"}`)
	// t2 仍未完成：先撞「尚未完成」预检——换用异目标成员构造异内容。
	response = env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","sourceTaskId":"t1","targetMemberId":"lead"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一转发请求不能用于不同内容") {
		t.Fatalf("同 id 异内容应 400: %d %s", response.Code, response.Body.String())
	}

	// 会话源转发（装配桩后）：scope 裁剪经 TranscriptSource。
	const sessionRequestID = "5ddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb"
	response = env.request(http.MethodPost, target,
		`{"id":"`+sessionRequestID+`","sourceSessionId":"sess-team-1","scope":"latest","targetMemberId":"helper"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("会话源转发应 200: %d %s", response.Code, response.Body.String())
	}
	json.Unmarshal(response.Body.Bytes(), &forwarded)
	if forwarded.SubmissionID == "" || forwarded.Duplicate {
		t.Fatalf("会话源转发: %+v", forwarded)
	}

	// Run 终态后拒绝转发：409。
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodPost, target,
		`{"id":"6eeeeeee-ffff-4aaa-8bbb-cccccccccccc","sourceTaskId":"t1","targetMemberId":"helper"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "运行已结束，不能转发") {
		t.Fatalf("终态转发应 409: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsTaskDiffProjection 任务工作区 diff 投影（§6.4 规则 6）：顶层与
// 文件级双截断标记、基线回显、404/409。
func TestTeamsTaskDiffProjection(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-diff")
	registryPath := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registryPath, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	env := newTeamTestEnv(t, "queued", false)
	env.server.TeamWorkspaceRegistry = NewTeamWorkspaceRegistry(registryPath)
	env.server.Teams.Workspaces = env.server.TeamWorkspaceRegistry
	teamID := env.seedRunTeam(t)
	// isolated 任务派发需要成员会话 cwd==worktree（fake 解析器无 cwd）→ 任务
	// 确定性 failed；diff 数据面只依赖快照的 workspaceId/BaseRevision。
	runID := env.createRun(t, teamID, `{"goal":"diff","mode":"pipeline","plan":[
		{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"wt-1"}]}`)
	env.waitForRunStatus(t, teamID, runID, "failed")
	head := gitRun(t, worktree, "rev-parse", "HEAD")
	env.forceRunUpdate(t, runID, func(run *team.Run) {
		for i := range run.Tasks {
			if run.Tasks[i].ID == "iso" {
				run.Tasks[i].BaseRevision = head
			}
		}
	})
	diffURL := "/api/teams/" + teamID + "/runs/" + runID + "/tasks/iso/diff"

	// 小改动：完整投影、无截断。
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("base\n追加行\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	response := env.request(http.MethodGet, diffURL, "")
	if response.Code != http.StatusOK {
		t.Fatalf("diff 应 200: %d %s", response.Code, response.Body.String())
	}
	var page struct {
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
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.WorkspaceID != "wt-1" || page.BaseRevision != head || page.Truncated {
		t.Fatalf("diff 头部异常: %+v", page)
	}
	if len(page.Files) != 1 || page.Files[0].Path != "a.txt" || page.Files[0].Added != 1 ||
		page.Files[0].Truncated || !strings.Contains(page.Files[0].Diff, "追加行") {
		t.Fatalf("diff 文件异常: %+v", page.Files)
	}

	// 单文件超 20000 字节：文件级截断，顶层不截断。
	big := strings.Repeat("x", 21000) + "\n"
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	response = env.request(http.MethodGet, diffURL, "")
	json.Unmarshal(response.Body.Bytes(), &page)
	if response.Code != http.StatusOK || !page.Files[0].Truncated || page.Truncated {
		t.Fatalf("单文件超限应只置文件级标记: code=%d truncated=%v files=%d",
			response.Code, page.Truncated, len(page.Files))
	}

	// 整包超 50 文件：顶层截断，只携带前 50 个文件。
	for i := 0; i < 51; i++ {
		name := fmt.Sprintf("f%02d.txt", i)
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		stageIntentToMakeNewFileVisible(t, worktree, name)
	}
	response = env.request(http.MethodGet, diffURL, "")
	json.Unmarshal(response.Body.Bytes(), &page)
	if response.Code != http.StatusOK || !page.Truncated || len(page.Files) != 50 {
		t.Fatalf("整包超限应置顶层标记且只带 50 个文件: code=%d truncated=%v files=%d",
			response.Code, page.Truncated, len(page.Files))
	}

	// 共享任务无基线 → 409；未知任务 → 404；未知 Run → 404。
	sharedRun := env.createRun(t, teamID, `{"goal":"共享","mode":"pipeline","plan":[
		{"id":"shared","memberId":"lead","prompt":"p"}]}`)
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+sharedRun+"/tasks/shared/diff", "")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "没有工作区基线") {
		t.Fatalf("共享任务 diff 应 409: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/tasks/ghost/diff", "")
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "任务不存在") {
		t.Fatalf("未知任务应 404: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/run-no-such-1/diff", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("未知 Run 应 404: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsTaskWorkspaceCleanup 工作区清理（§6.4 红线：用户显式触发；脏区
// 拒绝并落 workspace_cleanup_failed）。
func TestTeamsTaskWorkspaceCleanup(t *testing.T) {
	_, clean := newGitWorktreeFixture(t, "wt-clean")
	_, dirty := newGitWorktreeFixture(t, "wt-dirty")
	registryPath := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registryPath, map[string]teamWorkspaceEntry{
		"wt-clean": {Path: clean}, "wt-dirty": {Path: dirty},
	})
	env := newTeamTestEnv(t, "queued", false)
	env.server.TeamWorkspaceRegistry = NewTeamWorkspaceRegistry(registryPath)
	env.server.Teams.Workspaces = env.server.TeamWorkspaceRegistry
	teamID := env.seedRunTeam(t)

	// 干净 worktree：200 {ok:true}，目录被 git 删除。
	cleanRun := env.createRun(t, teamID, `{"goal":"清理","mode":"pipeline","plan":[
		{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"wt-clean"}]}`)
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+cleanRun+"/tasks/iso/workspace/cleanup", `{}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
		t.Fatalf("干净清理应 200: %d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(clean); !os.IsNotExist(err) {
		t.Fatalf("worktree 目录应被删除: %v", err)
	}

	// 脏 worktree：409 且落 workspace_cleanup_failed 事件。
	dirtyRun := env.createRun(t, teamID, `{"goal":"脏区","mode":"pipeline","plan":[
		{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"wt-dirty"}]}`)
	if err := os.WriteFile(filepath.Join(dirty, "a.txt"), []byte("dirty\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+dirtyRun+"/tasks/iso/workspace/cleanup", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "失败") {
		t.Fatalf("脏区清理应 409: %d %s", response.Code, response.Body.String())
	}
	events, err := env.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type == team.EventWorkspaceCleanupFailed && event.RunID == dirtyRun {
			found = true
		}
	}
	if !found {
		t.Fatal("清理失败应落 workspace_cleanup_failed 事件")
	}

	// 非 isolated 任务 → 409；未装配端口 → 409。
	sharedRun := env.createRun(t, teamID, `{"goal":"共享","mode":"pipeline","plan":[
		{"id":"shared","memberId":"lead","prompt":"p"}]}`)
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+sharedRun+"/tasks/shared/workspace/cleanup", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "没有引用独立工作区") {
		t.Fatalf("共享任务清理应 409: %d %s", response.Code, response.Body.String())
	}
	env.server.Teams.Workspaces = nil
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+dirtyRun+"/tasks/iso/workspace/cleanup", `{}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "网关未装配工作区注册表") {
		t.Fatalf("未装配清理应 409: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsAIMDSnapshotEndpoint AIMD 快照端点（§6.3）：空表是空数组、团队
// 不存在 404。
func TestTeamsAIMDSnapshotEndpoint(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/aimd", "")
	if response.Code != http.StatusOK {
		t.Fatalf("aimd 应 200: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"buckets": []`) {
		t.Fatalf("无桶时 buckets 应为空数组: %s", response.Body.String())
	}
	var page struct {
		TeamID  string           `json:"teamId"`
		Buckets []map[string]any `json:"buckets"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.TeamID != teamID || len(page.Buckets) != 0 {
		t.Fatalf("aimd 投影异常: %s (%v)", response.Body.String(), err)
	}
	response = env.request(http.MethodGet, "/api/teams/NoSuchTeamID1/aimd", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("未知团队应 404: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsRunCreateWorkspaceValidation 创建 Run 的 workspaceId 校验族与
// 未装配降级（§6.4）：isolated 必带 workspaceId（planner 400）、shared 不允许
// 携带、未装配 Workspaces 时 isolated 创建降级 409、装配后透传进任务快照。
func TestTeamsRunCreateWorkspaceValidation(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	cases := []struct {
		payload string
		code    int
		wantErr string
	}{
		{`{"goal":"缺键","mode":"pipeline","plan":[
			{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree"}]}`,
			http.StatusBadRequest, "必须提供 workspaceId"},
		{`{"goal":"坏键","mode":"pipeline","plan":[
			{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"坏 id!"}]}`,
			http.StatusBadRequest, "workspaceId"},
		{`{"goal":"共享带键","mode":"pipeline","plan":[
			{"id":"shared","memberId":"lead","prompt":"p","workspaceMode":"shared_workspace","workspaceId":"wt-1"}]}`,
			http.StatusBadRequest, "不能指定 workspaceId"},
		{`{"goal":"未装配","mode":"pipeline","plan":[
			{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"wt-1"}]}`,
			http.StatusConflict, "网关未装配工作区注册表"},
	}
	for _, testCase := range cases {
		response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", testCase.payload)
		if response.Code != testCase.code || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("应 %d 且含 %q: %d %s", testCase.code, testCase.wantErr,
				response.Code, response.Body.String())
		}
	}
	// 装配后同请求 200，workspaceId 透传进任务快照。
	env.server.TeamWorkspaceRegistry = NewTeamWorkspaceRegistry(filepath.Join(t.TempDir(), "team-workspaces.json"))
	env.server.Teams.Workspaces = env.server.TeamWorkspaceRegistry
	runID := env.createRun(t, teamID, `{"goal":"装配后","mode":"pipeline","plan":[
		{"id":"iso","memberId":"lead","prompt":"p","workspaceMode":"isolated_worktree","workspaceId":"wt-1"}]}`)
	detail := env.runDetail(t, teamID, runID)
	task := teamTaskOf(t, detail, "iso")
	if task["workspaceMode"] != "isolated_worktree" || task["workspaceId"] != "wt-1" {
		t.Fatalf("workspaceId 应透传进任务快照: %v", task)
	}
}

// TestTeamsRunMetaPhase2Keys changes meta 与 Run 列表的阶段二新键（§6.4.1/
// §6.5）：mode/collectPolicy/verify 摘要；verifying 的两个稳态用投影函数直验。
func TestTeamsRunMetaPhase2Keys(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID,
		`{"goal":"新键","mode":"pipeline","collectPolicy":"all","verifyCommand":["make","check"],`+
			`"plan":[{"id":"t1","memberId":"lead","prompt":"p"}]}`)
	// changes 帧 meta：mode/collectPolicy 落键，verify 摘要存在且闸门未决
	//（任务无 cwd 不会推进到 RunReducing，verifying 恒 false、无 exitCode）。
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Meta struct {
			Mode    string `json:"mode"`
			Collect string `json:"collectPolicy"`
			Verify  *struct {
				ExitCode  *int `json:"exitCode"`
				Verifying bool `json:"verifying"`
			} `json:"verify"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Meta.Mode != "pipeline" || page.Meta.Collect != "all" {
		t.Fatalf("meta 应带 mode/collectPolicy: %s", response.Body.String())
	}
	if page.Meta.Verify == nil || page.Meta.Verify.Verifying || page.Meta.Verify.ExitCode != nil {
		t.Fatalf("闸门未决的 verify 摘要应 {verifying:false} 且无 exitCode: %s", response.Body.String())
	}
	// Run 列表行的 mode。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs", "")
	var list struct {
		Runs []struct {
			Mode string `json:"mode"`
		} `json:"runs"`
	}
	json.Unmarshal(response.Body.Bytes(), &list)
	if len(list.Runs) != 1 || list.Runs[0].Mode != "pipeline" {
		t.Fatalf("Run 列表应带 mode: %s", response.Body.String())
	}

	// verifying 的两个稳态（RunReducing 未决 → true；结果已落 → exitCode）。
	reducing := teamRunMeta(&team.Run{
		VerifyCommand: []string{"make", "check"}, Status: team.RunReducing,
	})
	verify, _ := reducing.MustGet("verify").(*jsoncompat.Object)
	if verify == nil {
		t.Fatalf("reducing 应带 verify 摘要: %v", reducing)
	}
	if flag, _ := verify.MustGet("verifying").(bool); !flag {
		t.Fatalf("RunReducing 未决应 verifying=true: %v", verify)
	}
	settled := teamRunMeta(&team.Run{
		VerifyCommand: []string{"make", "check"}, Status: team.RunWaitingInput,
		Verify: &team.VerifyResult{ExitCode: 2},
	})
	verify, _ = settled.MustGet("verify").(*jsoncompat.Object)
	if code, _ := verify.MustGet("exitCode").(json.Number); code.String() != "2" {
		t.Fatalf("结果已落应带 exitCode: %v", verify)
	}
	if flag, _ := verify.MustGet("verifying").(bool); flag {
		t.Fatalf("结果已落应 verifying=false: %v", verify)
	}
	// 无闸门的 Run 不带 verify 键。
	if plain := teamRunMeta(&team.Run{Mode: team.ModeSingle}); plain.Len() > 0 {
		if _, has := plain.Get("verify"); has {
			t.Fatalf("无闸门不应带 verify: %v", plain)
		}
	}
}

// TestTeamsWorkspacesRegisterList 工作区注册表端点（§6.4 + §16 默认决策 6）：
// 未装配 409、worktree 探测校验（非 git/主检出 400）、幂等注册与列表。
func TestTeamsWorkspacesRegisterList(t *testing.T) {
	main, worktree := newGitWorktreeFixture(t, "wt-reg")
	_, other := newGitWorktreeFixture(t, "wt-other")
	env := newTeamTestEnv(t, "queued", false)

	// 未装配（默认 nil）→ 409。
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		body := ""
		if method == http.MethodPost {
			body = `{"id":"wt-1","path":"/tmp/nowhere"}`
		}
		response := env.request(method, "/api/teams/workspaces", body)
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "网关未装配工作区注册表") {
			t.Fatalf("%s 未装配应 409: %d %s", method, response.Code, response.Body.String())
		}
	}
	registryPath := filepath.Join(t.TempDir(), "team-workspaces.json")
	env.server.TeamWorkspaceRegistry = NewTeamWorkspaceRegistry(registryPath)

	// 首次列表：空数组。
	response := env.request(http.MethodGet, "/api/teams/workspaces", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"workspaces": []`) {
		t.Fatalf("空列表: %d %s", response.Code, response.Body.String())
	}

	// 校验族（400）。
	for _, testCase := range []struct {
		payload string
		wantErr string
	}{
		{`{"id":"坏 id!","path":"` + worktree + `"}`, "工作区标识无效"},
		{`{"id":"wt-1","path":"relative/path"}`, "绝对路径"},
		{`{"id":"wt-1","path":"` + t.TempDir() + `"}`, "不是 git 仓库"},
		{`{"id":"wt-1","path":"` + main + `"}`, "主检出不能注册"},
	} {
		response = env.request(http.MethodPost, "/api/teams/workspaces", testCase.payload)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("%s: 应 400 且含 %q: %d %s", testCase.payload, testCase.wantErr,
				response.Code, response.Body.String())
		}
	}

	// 注册成功：回显 id/path/createdAt/createdBy（登录会话 IP）。
	response = env.request(http.MethodPost, "/api/teams/workspaces",
		`{"id":"wt-1","path":"`+worktree+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("注册应 200: %d %s", response.Code, response.Body.String())
	}
	var registered struct {
		ID        string `json:"id"`
		Path      string `json:"path"`
		CreatedAt string `json:"createdAt"`
		CreatedBy string `json:"createdBy"`
		Duplicate bool   `json:"duplicate"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.ID != "wt-1" || registered.Path != worktree || registered.CreatedAt == "" ||
		registered.CreatedBy == "" || registered.Duplicate {
		t.Fatalf("注册回显异常: %+v", registered)
	}
	// 同 id 同路径重放：duplicate:true。
	response = env.request(http.MethodPost, "/api/teams/workspaces",
		`{"id":"wt-1","path":"`+worktree+`"}`)
	json.Unmarshal(response.Body.Bytes(), &registered)
	if response.Code != http.StatusOK || !registered.Duplicate {
		t.Fatalf("同 id 同路径重放应 duplicate: %d %s", response.Code, response.Body.String())
	}
	// 同 id 异路径：400。
	response = env.request(http.MethodPost, "/api/teams/workspaces",
		`{"id":"wt-1","path":"`+other+`"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一工作区标识不能用于不同路径") {
		t.Fatalf("同 id 异路径应 400: %d %s", response.Code, response.Body.String())
	}
	// 列表带回条目。
	response = env.request(http.MethodGet, "/api/teams/workspaces", "")
	var list struct {
		Workspaces []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || len(list.Workspaces) != 1 ||
		list.Workspaces[0].ID != "wt-1" || list.Workspaces[0].Path != worktree {
		t.Fatalf("列表异常: %s (%v)", response.Body.String(), err)
	}
}

// ---- 阶段三增量端点（amend/decision/questions/autoPlan/meta 计数/事件流）----

// seedAmendTeam 直落一支两任务团队成员（lead 担纲两个任务槽，均无 cwd 不
// 派发——forceRunUpdate 前后 Run 快照稳定）并登记收尾清理。
func (e *teamTestEnv) seedAmendTeam(t *testing.T) string {
	t.Helper()
	teamRow := &team.Team{
		ID: "team-amend", Name: "修订团队", LeadID: "lead", Policy: team.DefaultPolicy(),
		Members: []team.TeamMember{{ID: "lead", SessionID: "sess-team-1", AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
	}
	if err := e.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, run := range e.store.RunsByTeam(teamRow.ID) {
			_, _ = e.server.Teams.Cancel(run.ID)
		}
	})
	return teamRow.ID
}

// amendPlanJSON 是父/修订共用的计划体（t1 无依赖、t2 依赖 t1）。
const amendPlanJSON = `"plan":[{"id":"t1","memberId":"lead","prompt":"分析需求"},` +
	`{"id":"t2","memberId":"lead","prompt":"按分析实现","dependsOn":["t1"]}]`

// TestTeamsRunAmend 修订 Run 的三态校验、缓存导入、幂等与父 Run 不可变
// （§10.2 amend 段、§6.4.1 多轮缓存）。
func TestTeamsRunAmend(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedAmendTeam(t)
	parentID := env.createRun(t, teamID, `{"goal":"原始目标","mode":"pipeline",`+amendPlanJSON+`}`)
	// createRun 返回后，Run 自己的调度循环会异步完成首派推进（created →
	// dispatching 落一次 SaveRun）。先等这次推进落定再取父 Run 基线，否则
	// 版本基线与断言窗口内的循环写入相撞（全包负载下偶发 before=2 after=3）。
	quiesce := time.Now().Add(5 * time.Second)
	for {
		parent := env.store.Run(parentID)
		if parent != nil && parent.Status != "created" {
			break
		}
		if time.Now().After(quiesce) {
			t.Fatal("父 Run 未在时限内离开 created 状态")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 父 Run 的 t1 落 completed 带结果引用（amend 缓存导入的最小凭据）。
	env.forceRunUpdate(t, parentID, func(run *team.Run) {
		for i := range run.Tasks {
			if run.Tasks[i].ID == "t1" {
				run.Tasks[i].Status = team.TaskCompleted
				run.Tasks[i].ResultDetail = &team.TaskResult{
					SessionID: "sess-team-1", TurnID: "turn-9",
					Result: "分析完成的结果", ResultChars: len([]rune("分析完成的结果")),
				}
			}
		}
	})
	parentBefore := env.store.Run(parentID)
	parentEventsBefore := countRunEvents(t, env, parentID)
	target := "/api/teams/" + teamID + "/runs/" + parentID + "/amend"
	const requestID = "5ddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb"
	amendBody := `{"id":"` + requestID + `","goal":"修订目标","mode":"pipeline",` + amendPlanJSON + `}`

	// 参数校验族（400）。
	for _, testCase := range []struct{ payload, wantErr string }{
		{`{"id":"not-a-uuid","goal":"修订目标","mode":"pipeline",` + amendPlanJSON + `}`, "创建请求标识无效"},
		{`{"id":"` + requestID + `","mode":"pipeline",` + amendPlanJSON + `}`, "请输入 1–10000 字的运行目标"},
		{`{"id":"` + requestID + `","goal":"修订目标","mode":"pipeline","plan":[{"id":"t1","memberId":"ghost","prompt":"甲"}]}`, "引用了不存在的成员"},
		{`{"id":"` + requestID + `","goal":"修订目标","mode":"pipeline",` + amendPlanJSON + `,"verifyCommand":"make check"}`, "必须是字符串数组"},
		{`{"id":"` + parentID + `","goal":"修订目标","mode":"pipeline",` + amendPlanJSON + `}`, "修订 Run 不能复用父 Run 的标识"},
	} {
		response := env.request(http.MethodPost, target, testCase.payload)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("%s: 应 400 且含 %q: %d %s", testCase.payload, testCase.wantErr,
				response.Code, response.Body.String())
		}
	}
	// 父 Run 不存在：404（路由层 loadRun）。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/run-missing-404/amend", amendBody)
	if response.Code != http.StatusNotFound {
		t.Fatalf("父 Run 不存在应 404: %d %s", response.Code, response.Body.String())
	}
	// isolated_worktree 未装配注册表：409 降级（handleRunCreate 同款；先过
	// planner 的 workspaceId 必填校验）。
	response = env.request(http.MethodPost, target,
		`{"goal":"修订目标","mode":"pipeline","plan":[{"id":"t1","memberId":"lead","prompt":"甲","workspaceMode":"isolated_worktree","workspaceId":"wt-1"}]}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "网关未装配工作区注册表") {
		t.Fatalf("isolated 未装配应 409: %d %s", response.Code, response.Body.String())
	}

	// 成功：新 Run + parentRunId + cacheImports（t1 命中，t2 因父任务未完成按全新执行）。
	response = env.request(http.MethodPost, target, amendBody)
	if response.Code != http.StatusOK {
		t.Fatalf("amend 应 200: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID           string `json:"id"`
		ParentRunID  string `json:"parentRunId"`
		Duplicate    bool   `json:"duplicate"`
		CacheImports []struct {
			TaskID       string `json:"taskId"`
			ParentTaskID string `json:"parentTaskId"`
			InputHash    string `json:"inputHash"`
			SessionID    string `json:"sessionId"`
			TurnID       string `json:"turnId"`
			ResultChars  int    `json:"resultChars"`
			Truncated    bool   `json:"truncated"`
		} `json:"cacheImports"`
		Tasks []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			CachedFrom string `json:"cachedFrom"`
			Result     *struct {
				SessionID string `json:"sessionId"`
				TurnID    string `json:"turnId"`
				Result    string `json:"result"`
			} `json:"result"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == parentID || created.ID == "" || created.ParentRunID != parentID || created.Duplicate {
		t.Fatalf("amend 应创建新 Run 并携带 parentRunId: %+v", created)
	}
	if len(created.CacheImports) != 1 || created.CacheImports[0].TaskID != "t1" ||
		created.CacheImports[0].ParentTaskID != "t1" || created.CacheImports[0].InputHash == "" ||
		created.CacheImports[0].SessionID != "sess-team-1" || created.CacheImports[0].TurnID != "turn-9" ||
		created.CacheImports[0].ResultChars != len([]rune("分析完成的结果")) {
		t.Fatalf("cacheImports 形状异常: %+v", created.CacheImports)
	}
	var t1, t2 struct {
		Status     string `json:"status"`
		CachedFrom string `json:"cachedFrom"`
		Result     *struct {
			SessionID string `json:"sessionId"`
			TurnID    string `json:"turnId"`
			Result    string `json:"result"`
		} `json:"result"`
	}
	detail := env.runDetail(t, teamID, created.ID)
	if err := json.Unmarshal(mustTaskJSON(t, detail, "t1"), &t1); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(mustTaskJSON(t, detail, "t2"), &t2); err != nil {
		t.Fatal(err)
	}
	if t1.Status != team.TaskCompleted || t1.CachedFrom != "t1" ||
		t1.Result == nil || t1.Result.SessionID != "sess-team-1" || t1.Result.TurnID != "turn-9" {
		t.Fatalf("缓存命中任务应 completed+cachedFrom+结果引用: %+v", t1)
	}
	if t2.Status == team.TaskCompleted || t2.CachedFrom != "" {
		t.Fatalf("父任务未完成的 t2 应按全新任务执行: %+v", t2)
	}
	// 父 Run 不可变：版本与 journal 行数零增，新事件不落在父 Run。
	parentAfter := env.store.Run(parentID)
	if parentAfter.Version != parentBefore.Version || !parentAfter.UpdatedAt.Equal(parentBefore.UpdatedAt) {
		t.Fatalf("父 Run 版本/时间不得因 amend 改变: before=%d after=%d",
			parentBefore.Version, parentAfter.Version)
	}
	if count := countRunEvents(t, env, parentID); count != parentEventsBefore {
		t.Fatalf("父 Run 的 journal 事件应零增: before=%d after=%d", parentEventsBefore, count)
	}
	// 新 Run 的事件流（Ingest 无类型白名单）：task_cache_hit 携带溯源字段。
	feed := env.server.TeamFeeds.Feed(created.ID)
	sawCacheHit := false
	for _, event := range feed.Events() {
		if event.Type == team.EventTaskCacheHit {
			sawCacheHit = true
			if event.Payload["parentRunId"] != parentID || event.Payload["parentTaskId"] != "t1" {
				t.Fatalf("task_cache_hit 载荷应带父 Run/父任务: %+v", event.Payload)
			}
		}
	}
	if !sawCacheHit {
		t.Fatal("新 Run 事件流应包含 task_cache_hit")
	}
	// 幂等重放：同 id 同内容 → duplicate:true，不重复建 Run。
	response = env.request(http.MethodPost, target, amendBody)
	if response.Code != http.StatusOK {
		t.Fatalf("重放应 200: %d %s", response.Code, response.Body.String())
	}
	var replay struct {
		ID        string `json:"id"`
		Duplicate bool   `json:"duplicate"`
	}
	json.Unmarshal(response.Body.Bytes(), &replay)
	if !replay.Duplicate || replay.ID != created.ID {
		t.Fatalf("幂等重放: %+v", replay)
	}
	if runs := env.store.RunsByTeam(teamID); len(runs) != 2 {
		t.Fatalf("重放不得创建重复 Run，实际 %d 条", len(runs))
	}
	// 同 id 异内容（goal 变化）→ 400。
	response = env.request(http.MethodPost, target,
		`{"id":"`+requestID+`","goal":"另一个修订目标","mode":"pipeline",`+amendPlanJSON+`}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一创建请求不能用于不同内容") {
		t.Fatalf("异内容应 400: %d %s", response.Code, response.Body.String())
	}
	// inputHash 不一致（t1 prompt 修订）：显式拒绝复用，按全新任务执行。
	response = env.request(http.MethodPost, target,
		`{"id":"6eeeeeee-ffff-4aaa-8bbb-cccccccccccc","goal":"修订目标","mode":"pipeline",`+
			`"plan":[{"id":"t1","memberId":"lead","prompt":"重新分析"},{"id":"t2","memberId":"lead","prompt":"按分析实现","dependsOn":["t1"]}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("修订 prompt 的 amend 应 200: %d %s", response.Code, response.Body.String())
	}
	var revised struct {
		CacheImports []map[string]any `json:"cacheImports"`
		Tasks        []struct {
			ID         string `json:"id"`
			Status     string `json:"status"`
			CachedFrom string `json:"cachedFrom"`
		} `json:"tasks"`
	}
	json.Unmarshal(response.Body.Bytes(), &revised)
	if len(revised.CacheImports) != 0 {
		t.Fatalf("inputHash 不一致不得导入缓存: %+v", revised.CacheImports)
	}
	for _, task := range revised.Tasks {
		if task.CachedFrom != "" || task.Status == team.TaskCompleted {
			t.Fatalf("缓存拒绝后应按全新任务执行: %+v", task)
		}
	}
}

// countRunEvents 统计 journal 里属于 runID 的事件数（父 Run 不可变断言用）。
func countRunEvents(t *testing.T, env *teamTestEnv, runID string) int {
	t.Helper()
	events, err := env.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.RunID == runID {
			count++
		}
	}
	return count
}

// mustTaskJSON 从 RunDetail 的 tasks 数组剥出指定任务的原始 JSON。
func mustTaskJSON(t *testing.T, run map[string]any, taskID string) json.RawMessage {
	t.Helper()
	task := teamTaskOf(t, run, taskID)
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// waitForSessionRequest 轮询会话快照直到出现指定 id 的未决请求（伪 Agent
// 的提问卡经 session/request_permission 反向请求登记）。
func waitForSessionRequest(t *testing.T, session *acpruntime.Session, requestID string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := session.Snapshot()
		if requests, _ := snapshot.MustGet("requests").(*jsoncompat.Array); requests != nil {
			for _, item := range requests.Items {
				if entry, ok := item.(*jsoncompat.Object); ok {
					if id, _ := entry.MustGet("id").(string); id == requestID {
						return
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("会话未在时限内出现未决请求 %s", requestID)
}

// waitFileContains 轮询直到 dataDir/name 的内容包含 needle（伪 Agent 的
// prompts/answers 落盘断言）。
func waitFileContains(t *testing.T, dataDir, name, needle string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(filepath.Join(dataDir, name)); err == nil &&
			strings.Contains(string(data), needle) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s 未在时限内包含 %q", name, needle)
}

// TestTeamsRunDecision 成员提问 decision 的错误三分类、成功回答、幂等与
// 会话侧过期撤回（§5.4）。提问卡由伪 Agent 经 session/request_permission
// 反向请求登记（prompt 含 ASK），Run 快照的问题条目按生产形态注入
// （Version=登记时 Run 快照版本）。
func TestTeamsRunDecision(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamRow := &team.Team{
		ID: "team-decision", Name: "提问团队", LeadID: "lead", Policy: team.DefaultPolicy(),
		Members: []team.TeamMember{{ID: "lead", SessionID: env.sessionID, AgentKind: "kimi", Role: team.RoleLead, Enabled: true}},
	}
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamRow.ID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	runID := env.createRun(t, teamRow.ID, `{"goal":"回答问题","mode":"pipeline",`+amendPlanJSON+`}`)
	// 伪 Agent 的提问卡：q1 真实登记在会话；q2 指向不存在的请求（过期路径）。
	if err := env.session.Prompt("ASK", 30*time.Second); err != nil {
		t.Fatalf("发起提问回合失败: %v", err)
	}
	waitForSessionRequest(t, env.session, "q1")
	env.forceRunUpdate(t, runID, func(run *team.Run) {
		version := run.Version
		run.PendingQuestions = append(run.PendingQuestions,
			team.PendingQuestion{
				QuestionID: "t1:q1", TaskID: "t1", Version: version, Kind: team.QuestionKindQuestion,
				Summary: "选择部署环境",
				Options: []team.QuestionOption{{ID: "prod", Label: "生产"}, {ID: "stag", Label: "预发"}},
				Status:  team.QuestionStatusOpen,
			},
			team.PendingQuestion{
				QuestionID: "t1:q2", TaskID: "t1", Version: version, Kind: team.QuestionKindInput,
				Summary: "从未登记的输入卡", Status: team.QuestionStatusOpen,
			})
	})
	detail := env.runDetail(t, teamRow.ID, runID)
	questions := detail["pendingQuestions"].([]any)
	if len(questions) != 2 {
		t.Fatalf("问题登记异常: %s", responseText(t, detail))
	}
	registeredVersion := uint64(questions[0].(map[string]any)["version"].(float64))
	target := "/api/teams/" + teamRow.ID + "/runs/" + runID + "/decision"
	const answerID = "7abcdef1-1234-4abc-8def-aaaaaaaaaaaa"
	const secondID = "7abcdef2-1234-4abc-8def-aaaaaaaaaaaa"
	const staleID = "7abcdef3-1234-4abc-8def-aaaaaaaaaaaa"

	// 格式校验族（400）。
	for _, testCase := range []struct{ payload, wantErr string }{
		{`{"questionId":"t1:q1","version":1,"response":{"optionId":"prod"}}`, "缺少决策标识"},
		{`{"decisionId":"not-a-uuid","questionId":"t1:q1","version":1,"response":{"optionId":"prod"}}`, "决策标识无效"},
		{`{"decisionId":"` + answerID + `","version":1,"response":{"optionId":"prod"}}`, "缺少问题标识"},
		{`{"decisionId":"` + answerID + `","questionId":"t1:q1","response":{"optionId":"prod"}}`, "缺少版本号"},
		{`{"decisionId":"` + answerID + `","questionId":"t1:q1","version":"x","response":{"optionId":"prod"}}`, "版本号必须是整数"},
		{`{"decisionId":"` + answerID + `","questionId":"t1:q1","version":1}`, "回应必须是对象"},
		{`{"decisionId":"` + answerID + `","questionId":"t1:q1","version":1,"response":"prod"}`, "回应必须是对象"},
	} {
		response := env.request(http.MethodPost, target, testCase.payload)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), testCase.wantErr) {
			t.Fatalf("%s: 应 400 且含 %q: %d %s", testCase.payload, testCase.wantErr,
				response.Code, response.Body.String())
		}
	}
	// 问题不存在：404。
	response := env.request(http.MethodPost, target,
		`{"decisionId":"`+secondID+`","questionId":"t1:ghost","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"optionId":"prod"}}`)
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "问题不存在") {
		t.Fatalf("问题不存在应 404: %d %s", response.Code, response.Body.String())
	}
	// 版本不匹配：409（过期版本拒绝并要求刷新）。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+secondID+`","questionId":"t1:q1","version":`+strconv.FormatUint(registeredVersion+7, 10)+`,"response":{"optionId":"prod"}}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "已过期") {
		t.Fatalf("版本不匹配应 409: %d %s", response.Code, response.Body.String())
	}
	// 选项无效：400（请求未被消费，问题仍 open 可重答）。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+answerID+`","questionId":"t1:q1","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"optionId":"nope"}}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "请选择有效的选项") {
		t.Fatalf("选项无效应 400: %d %s", response.Code, response.Body.String())
	}
	if questionStatus(t, env, teamRow.ID, runID, "t1:q1") != team.QuestionStatusOpen {
		t.Fatal("选项无效后问题应保持 open")
	}
	// 会话侧过期（q2 无此请求卡）：409 且登记撤回（reason=stale）。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+staleID+`","questionId":"t1:q2","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"action":"cancel"}}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "请求已过期") {
		t.Fatalf("会话侧过期应 409: %d %s", response.Code, response.Body.String())
	}
	if questionStatus(t, env, teamRow.ID, runID, "t1:q2") != team.QuestionStatusWithdrawn {
		t.Fatal("过期问题应被撤回")
	}
	// 有效回答：200 {ok:true}，答案原样送达成员会话。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+answerID+`","questionId":"t1:q1","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"optionId":"prod"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("有效回答应 200: %d %s", response.Code, response.Body.String())
	}
	var answered struct {
		OK        bool `json:"ok"`
		Duplicate bool `json:"duplicate"`
	}
	json.Unmarshal(response.Body.Bytes(), &answered)
	if !answered.OK || answered.Duplicate {
		t.Fatalf("首次回答: %+v", answered)
	}
	waitFileContains(t, env.dataDir, "answers.txt", "prod")
	if questionStatus(t, env, teamRow.ID, runID, "t1:q1") != team.QuestionStatusAnswered {
		t.Fatal("回答后问题应置 answered")
	}
	// 同 decisionID 重放：duplicate:true，不重复投递。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+answerID+`","questionId":"t1:q1","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"optionId":"prod"}}`)
	if response.Code != http.StatusOK {
		t.Fatalf("重放应 200: %d %s", response.Code, response.Body.String())
	}
	json.Unmarshal(response.Body.Bytes(), &answered)
	if !answered.OK || !answered.Duplicate {
		t.Fatalf("重放应 duplicate: %+v", answered)
	}
	if data, _ := os.ReadFile(filepath.Join(env.dataDir, "answers.txt")); strings.Count(string(data), "\n") != 1 {
		t.Fatalf("重放不得重复投递答案: %q", string(data))
	}
	// 已回答后再次提交（新 decisionID）：409（答案只允许消费一次）。
	response = env.request(http.MethodPost, target,
		`{"decisionId":"`+secondID+`","questionId":"t1:q1","version":`+strconv.FormatUint(registeredVersion, 10)+`,"response":{"optionId":"stag"}}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "问题已回答或已撤回") {
		t.Fatalf("二次回答应 409: %d %s", response.Code, response.Body.String())
	}
}

// questionStatus 读取 RunDetail 里指定问题的当前状态。
func questionStatus(t *testing.T, env *teamTestEnv, teamID, runID, questionID string) string {
	t.Helper()
	detail := env.runDetail(t, teamID, runID)
	questions, _ := detail["pendingQuestions"].([]any)
	for _, item := range questions {
		question, _ := item.(map[string]any)
		if question["questionId"] == questionID {
			status, _ := question["status"].(string)
			return status
		}
	}
	return ""
}

// responseText 把 detail 序列化成可读文本（断言失败信息用）。
func responseText(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestTeamsRunQuestionsProjection questions 端点与 pendingQuestions 投影
// （§5.4）：快照直出、空表空数组、404、meta 只带计数不带文本。
func TestTeamsRunQuestionsProjection(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"提问投影","mode":"single"}`)
	env.forceRunUpdate(t, runID, func(run *team.Run) {
		run.PendingQuestions = append(run.PendingQuestions,
			team.PendingQuestion{QuestionID: "t1:qa", TaskID: run.Tasks[0].ID, Version: run.Version,
				Kind: team.QuestionKindQuestion, Summary: "开放的问题甲",
				Options: []team.QuestionOption{{ID: "yes", Label: "是"}, {ID: "no", Label: "否"}},
				Status:  team.QuestionStatusOpen},
			team.PendingQuestion{QuestionID: "t1:qb", TaskID: run.Tasks[0].ID, Version: run.Version,
				Kind: team.QuestionKindQuestion, Summary: "已回答的问题乙", Status: team.QuestionStatusAnswered},
			team.PendingQuestion{QuestionID: "t1:qc", TaskID: run.Tasks[0].ID, Version: run.Version,
				Kind: team.QuestionKindInput, Summary: "已撤回的问题丙", Status: team.QuestionStatusWithdrawn})
	})
	// questions 端点：快照直出（三条全量，含各状态）。
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/questions", "")
	if response.Code != http.StatusOK {
		t.Fatalf("questions 应 200: %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Questions []struct {
			QuestionID string `json:"questionId"`
			TaskID     string `json:"taskId"`
			Version    uint64 `json:"version"`
			Kind       string `json:"kind"`
			Summary    string `json:"summary"`
			Options    []struct {
				ID    string `json:"id"`
				Label string `json:"label"`
			} `json:"options"`
			Status string `json:"status"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || len(page.Questions) != 3 {
		t.Fatalf("questions 投影异常: %s (%v)", response.Body.String(), err)
	}
	if page.Questions[0].QuestionID != "t1:qa" || page.Questions[0].Summary != "开放的问题甲" ||
		page.Questions[0].Status != team.QuestionStatusOpen ||
		len(page.Questions[0].Options) != 2 || page.Questions[0].Options[0].ID != "yes" {
		t.Fatalf("问题条目字段异常: %+v", page.Questions[0])
	}
	// RunDetail 顶层直出 pendingQuestions（权威展示源）。
	detail := env.runDetail(t, teamID, runID)
	if questions, _ := detail["pendingQuestions"].([]any); len(questions) != 3 {
		t.Fatalf("detail 应带 pendingQuestions: %s", responseText(t, detail))
	}
	// meta 只带 open 计数，绝不含问题文本（整页重渲染契约）。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	var changes struct {
		Meta map[string]any `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	if count, ok := changes.Meta["pendingQuestions"].(float64); !ok || count != 1 {
		t.Fatalf("meta 应带 pendingQuestions=1（仅 open）: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "开放的问题甲") {
		t.Fatalf("meta/rows 外发不得携带问题文本: %s", response.Body.String())
	}
	// 无问题的 Run：空数组（不是 null/缺键）。
	otherRun := env.createRun(t, teamID, `{"goal":"无问题","mode":"single"}`)
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+otherRun+"/questions", "")
	var empty struct {
		Questions []map[string]any `json:"questions"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &empty) != nil || empty.Questions == nil || len(empty.Questions) != 0 {
		t.Fatalf("无问题应回空数组: %d %s", response.Code, response.Body.String())
	}
	// Run 不存在：404。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/run-missing-404/questions", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("Run 不存在应 404: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsRunCreateAutoPlan autoPlan 透传（§5.1/§10.2：plan 为空才允许 Lead
// 产生 team-actions）：无 plan 键或空数组 → autoPlan=true；显式计划 → 不带。
func TestTeamsRunCreateAutoPlan(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	// 无 plan：autoPlan=true。
	detail := env.runDetail(t, teamID, env.createRun(t, teamID, `{"goal":"自动规划","mode":"single"}`))
	if autoPlan, _ := detail["autoPlan"].(bool); !autoPlan {
		t.Fatalf("空 plan 的 single Run 应 autoPlan=true: %s", responseText(t, detail))
	}
	// plan 空数组：同样 autoPlan=true。
	detail = env.runDetail(t, teamID, env.createRun(t, teamID, `{"goal":"空数组","mode":"single","plan":[]}`))
	if autoPlan, _ := detail["autoPlan"].(bool); !autoPlan {
		t.Fatalf("空数组的 single Run 应 autoPlan=true: %s", responseText(t, detail))
	}
	// 显式计划：不带 autoPlan 键（false）。
	detail = env.runDetail(t, teamID, env.createRun(t, teamID,
		`{"goal":"显式计划","mode":"pipeline","plan":[{"id":"t1","memberId":"lead","prompt":"甲"}]}`))
	if _, exists := detail["autoPlan"]; exists {
		t.Fatalf("显式计划不得带 autoPlan: %s", responseText(t, detail))
	}
	// pipeline 空计划：创建即拒（planner 校验），不落 autoPlan 语义。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs", `{"goal":"坏计划","mode":"pipeline"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("pipeline 空计划应 400: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsRunAmendAutoPlan amend 的 autoPlan 推导与创建端点同形（§5.1/
// §10.2，阶段三复核确认的静默分歧修复）：无 plan 键的 single 修订 →
// autoPlan=true（「让 Lead 再规划一轮」拿得到 team-actions 解析）；显式
// 计划的修订 → 不带 autoPlan 键。
func TestTeamsRunAmendAutoPlan(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	parentID := env.createRun(t, teamID, `{"goal":"原始目标","mode":"single"}`)

	// 无 plan 键的 single 修订：autoPlan=true（与父 Run 及同形 POST /runs 一致）。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+parentID+"/amend",
		`{"goal":"让 Lead 再规划一轮","mode":"single"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("空 plan 修订应 200: %d %s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("amend 响应异常: %v %s", err, response.Body.String())
	}
	detail := env.runDetail(t, teamID, created.ID)
	if autoPlan, _ := detail["autoPlan"].(bool); !autoPlan {
		t.Fatalf("空 plan 修订应 autoPlan=true: %s", responseText(t, detail))
	}

	// 显式计划的修订：不带 autoPlan 键（false）。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+parentID+"/amend",
		`{"goal":"显式计划修订","mode":"pipeline","plan":[{"id":"t1","memberId":"lead","prompt":"甲"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("显式计划修订应 200: %d %s", response.Code, response.Body.String())
	}
	created.ID = ""
	json.Unmarshal(response.Body.Bytes(), &created)
	detail = env.runDetail(t, teamID, created.ID)
	if _, exists := detail["autoPlan"]; exists {
		t.Fatalf("显式计划修订不得带 autoPlan: %s", responseText(t, detail))
	}
}

// emitTeamEvent 走生产同款装配（journal.Append 编号 → TeamFeeds.Ingest 并入）
// 把一条事件送进 run 的事件流——Service.OnEvent 的等价物，用于证实新事件
// 类型无白名单直通。
func (e *teamTestEnv) emitTeamEvent(t *testing.T, event *team.Event) {
	t.Helper()
	if err := e.journal.Append(event); err != nil {
		t.Fatal(err)
	}
	e.server.TeamFeeds.Ingest(event)
}

// phase3SampleEvents 构造阶段三七类新事件（载荷形态镜像 runner.go/amend.go/
// questions.go 的生产构造：有界、字段同名）。
func phase3SampleEvents(runID string) []*team.Event {
	return []*team.Event{
		{ID: "ev-invalid", RunID: runID, Type: team.EventInvalidActionBlock, Source: "watcher",
			Payload: map[string]any{
				"taskId": "t1", "attempt": jsoncompat.Num(1), "final": false,
				"round": jsoncompat.Num(1), "violations": []any{"标记 team-actions 未成对出现"},
			}},
		{ID: "ev-proposed", RunID: runID, Type: team.EventActionProposed, Source: "watcher",
			Payload: map[string]any{
				"taskId": "t1", "index": jsoncompat.Num(0),
				"actionKey": "team:team-x:run:" + runID + ":action:t1:1:0",
				"type":      "delegate", "round": jsoncompat.Num(1), "sourceTaskId": "t1",
				"member": "lead", "taskHead": "检查新增 API 的错误处理",
			}},
		{ID: "ev-rejected", RunID: runID, Type: team.EventActionRejected, Source: "runner",
			Payload: map[string]any{
				"taskId": "t1", "type": "delegate",
				"actionKey": "team:team-x:run:" + runID + ":action:t1:1:0",
				"reason":    "目标成员不存在", "index": jsoncompat.Num(0),
			}},
		{ID: "ev-qopen", RunID: runID, Type: team.EventQuestionOpened, Source: "watcher",
			Payload: map[string]any{
				"questionId": "t1:r1", "taskId": "t1", "version": jsoncompat.Num(3),
				"kind": "question", "summary": "选择部署环境",
			}},
		{ID: "ev-qresolved", RunID: runID, Type: team.EventQuestionResolved, Source: "user",
			Payload: map[string]any{
				"questionId": "t1:r1", "taskId": "t1", "answerHead": `{"optionId":"prod"}`,
			}},
		{ID: "ev-qwithdrawn", RunID: runID, Type: team.EventQuestionWithdrawn, Source: "user",
			Payload: map[string]any{
				"questionId": "t1:r1", "taskId": "t1", "reason": "task_terminal",
			}},
		{ID: "ev-cachehit", RunID: runID, Type: team.EventTaskCacheHit, Source: "user",
			Payload: map[string]any{
				"taskId": "t1", "parentRunId": "run-parent", "parentTaskId": "t1",
				"inputHash": "abc123", "sessionId": "sess-team-1", "turnId": "turn-9",
				"result": "分析完成的结果", "resultChars": jsoncompat.Num(7), "truncated": false,
			}},
	}
}

// TestTeamsPhase3EventStreamChanges 七类新事件经 changes 增量下发（Ingest
// 无事件类型白名单）且 payload 有界（§10.3）。
func TestTeamsPhase3EventStreamChanges(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"事件流","mode":"single"}`)
	// 游标锚：reset 页取当前 epoch/sequence。
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	var anchor struct {
		Epoch    string `json:"epoch"`
		Sequence uint64 `json:"sequence"`
		Reset    bool   `json:"reset"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &anchor); err != nil || anchor.Epoch == "" {
		t.Fatalf("changes 锚页异常: %s (%v)", response.Body.String(), err)
	}
	for _, event := range phase3SampleEvents(runID) {
		env.emitTeamEvent(t, event)
	}
	// 增量页：七行按序、类型齐全、载荷字段保持、有界。
	response = env.request(http.MethodGet,
		"/api/teams/"+teamID+"/runs/"+runID+"/changes?after="+
			url.QueryEscape(strconv.FormatUint(anchor.Sequence, 10))+"&epoch="+url.QueryEscape(anchor.Epoch), "")
	if response.Code != http.StatusOK {
		t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Rows []struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		} `json:"rows"`
		Meta struct {
			PendingQuestions float64 `json:"pendingQuestions"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		team.EventInvalidActionBlock, team.EventActionProposed, team.EventActionRejected,
		team.EventQuestionOpened, team.EventQuestionResolved, team.EventQuestionWithdrawn,
		team.EventTaskCacheHit,
	}
	if len(page.Rows) != len(wantTypes) {
		t.Fatalf("增量页应携带全部七类新事件: %s", response.Body.String())
	}
	for i, want := range wantTypes {
		if page.Rows[i].Type != want {
			t.Fatalf("第 %d 行应是 %s，实际 %s", i, want, page.Rows[i].Type)
		}
		if len(page.Rows[i].Payload) == 0 {
			t.Fatalf("%s 行应带 payload: %s", want, response.Body.String())
		}
	}
	if key, _ := page.Rows[1].Payload["actionKey"].(string); !strings.Contains(key, "action:t1:1:0") {
		t.Fatalf("action_proposed 载荷应保持 actionKey: %+v", page.Rows[1].Payload)
	}
	if reason, _ := page.Rows[2].Payload["reason"].(string); reason != "目标成员不存在" {
		t.Fatalf("action_rejected 载荷应保持拒绝原因: %+v", page.Rows[2].Payload)
	}
	// 有界（§10.3）：单行（含载荷）不超过 8KB——上游构造已截断（240 rune 摘要
	// /200 rune answerHead/20 条 violation），投影层不得再放大。
	for _, row := range page.Rows {
		if encoded := responseText(t, row); len(encoded) > 8192 {
			t.Fatalf("%s 行超出有界上限: %d bytes", row.Type, len(encoded))
		}
	}
	if page.Meta.PendingQuestions != 0 {
		t.Fatalf("无问题 Run 的 meta 计数应为 0: %+v", page.Meta)
	}
}

// TestTeamsPhase3EventStreamSSE 新事件经 SSE 下发：订阅期间产生的
// question_opened 必须以 changes 帧到达（Ingest→Notify→帧）。
func TestTeamsPhase3EventStreamSSE(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	api := httptest.NewServer(env.server)
	hostPort := api.URL[len("http://"):]
	env.server.Hosts[hostPort] = true
	env.server.Origins["http://"+hostPort] = true
	env.server.LocalHosts[hostPort] = true
	defer api.Close()

	teamResponse := featureRequest(env.server, env.token, env.csrf, http.MethodPost, "/api/teams",
		`{"name":"SSE 阶段三","members":[{"id":"lead","sessionId":"`+env.sessionID+`","role":"lead"}],"leadId":"lead"}`)
	var teamRow struct {
		ID string `json:"id"`
	}
	json.Unmarshal(teamResponse.Body.Bytes(), &teamRow)
	runID := env.createRun(t, teamRow.ID, `{"goal":"SSE 新事件","mode":"single"}`)

	events := authorizedSSEGet(t, env, api.URL, hostPort,
		"/api/teams/"+teamRow.ID+"/runs/"+runID+"/events?incremental=1")
	defer events.Body.Close()
	frames := make(chan string, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(events.Body)
		event, data := "", ""
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
			case line == "\n":
				if event != "" {
					frames <- event + "\n" + data
				}
				event, data = "", ""
			}
		}
	}()
	// ready 帧先行。
	select {
	case frame := <-frames:
		if !strings.HasPrefix(frame, "ready\n") {
			t.Fatalf("首帧应 ready: %q", frame)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	// 订阅期间落一条新事件 → changes 帧携带它。
	env.emitTeamEvent(t, &team.Event{
		ID: "ev-sse", RunID: runID, Type: team.EventQuestionOpened, Source: "watcher",
		Payload: map[string]any{
			"questionId": "t1:r1", "taskId": "t1", "version": jsoncompat.Num(2),
			"kind": "question", "summary": "SSE 下发的问题",
		},
	})
	deadline := time.After(8 * time.Second)
	for {
		select {
		case frame := <-frames:
			if strings.HasPrefix(frame, "changes\n") && strings.Contains(frame, "question_opened") &&
				strings.Contains(frame, "SSE 下发的问题") {
				events.Body.Close()
				<-readerDone
				return
			}
		case <-deadline:
			t.Fatal("SSE 未在时限内发出携带 question_opened 的 changes 帧")
		}
	}
}

// authorizedSSEGet 以带认证的 GET 打开 SSE 流（events 端点需要真实 HTTP 服务器）。
func authorizedSSEGet(t *testing.T, env *teamTestEnv, baseURL, hostPort, target string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Host = hostPort
	request.Header.Set("Origin", "http://"+hostPort)
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: env.token})
	response, callErr := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if callErr != nil {
		t.Fatal(callErr)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE 应 200: %d", response.StatusCode)
	}
	return response
}

// TestTeamsRunPendingActionsProjection 动作预览投影（§11/§5.2）：
// detail.pendingActions 是 Lead 动作与拒绝原因的唯一展示源（run 级事件在
// 前端 applyEvent 早退不留 payload），三态与字段全量直出。
func TestTeamsRunPendingActionsProjection(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"动作预览","mode":"single"}`)
	env.forceRunUpdate(t, runID, func(run *team.Run) {
		run.PendingActions = append(run.PendingActions,
			team.Action{Type: "delegate", SourceTaskID: "t1", Round: 1, Member: "lead",
				Task: "检查错误处理", ActionKey: "team:team-run-idem:run:" + runID + ":action:t1:1:0",
				Status: team.ActionStatusProposed},
			team.Action{Type: "request_context", SourceTaskID: "t1", Round: 1, Member: "helper",
				FromTaskID: "t0", ActionKey: "team:team-run-idem:run:" + runID + ":action:t1:1:1",
				Status: team.ActionStatusExecuted},
			team.Action{Type: "summarize", SourceTaskID: "t1", Round: 2,
				ActionKey: "team:team-run-idem:run:" + runID + ":action:t1:2:0",
				Status:    team.ActionStatusRejected, Reason: "目标成员不存在"})
	})
	detail := env.runDetail(t, teamID, runID)
	actions, _ := detail["pendingActions"].([]any)
	if len(actions) != 3 {
		t.Fatalf("detail 应带 pendingActions 三态: %s", responseText(t, detail))
	}
	first, _ := actions[0].(map[string]any)
	if first["status"] != team.ActionStatusProposed || first["actionKey"] == "" ||
		first["member"] != "lead" || first["round"].(float64) != 1 {
		t.Fatalf("proposed 动作字段异常: %s", responseText(t, first))
	}
	second, _ := actions[1].(map[string]any)
	if second["status"] != team.ActionStatusExecuted || second["fromTaskId"] != "t0" {
		t.Fatalf("executed 动作字段异常: %s", responseText(t, second))
	}
	third, _ := actions[2].(map[string]any)
	if third["status"] != team.ActionStatusRejected || third["reason"] != "目标成员不存在" {
		t.Fatalf("rejected 动作应带拒绝原因: %s", responseText(t, third))
	}
	// 无动作的 Run 不带 pendingActions 键（omitempty 语义）。
	plain := env.runDetail(t, teamID, env.createRun(t, teamID, `{"goal":"无动作","mode":"single"}`))
	if _, exists := plain["pendingActions"]; exists {
		t.Fatalf("无动作 Run 不应带 pendingActions: %s", responseText(t, plain))
	}
}

// ---- 阶段四增量端点（§9/§11/§12 工作项 1/2/4/5/6）----

// seedStage4Run 直落一支指定状态的 fixture Run（绕过 HTTP 创建链路的调度
// 循环——纯投影/统计端点测试不依赖执行流）；收尾照 seedRunTeam 先取消。
func (e *teamTestEnv) seedStage4Run(t *testing.T, teamID, runID string, mutate func(*team.Run)) *team.Run {
	t.Helper()
	run := &team.Run{ID: runID, TeamID: teamID, Goal: "阶段四 fixture", Mode: team.ModePipeline,
		Status: team.RunCompleted, PlanHash: "ph-" + runID,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if mutate != nil {
		mutate(run)
	}
	if err := e.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = e.server.Teams.Cancel(runID) })
	return run
}

// TestTeamsScheduleConfigValidation PUT /schedule 的校验族（中文 400）：
// 触发规格二选一、间隔生产下限（300s 硬护栏，无缩限钩子——集成测试用
// fixture 预置绕开）、cron 解析错带实际表达式值、goal 上限、计划成员
// 合法性（复用 Run 创建件）与验证命令白名单。
func TestTeamsScheduleConfigValidation(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	put := func(body string) *httptest.ResponseRecorder {
		return env.request(http.MethodPut, "/api/teams/"+teamID+"/schedule", body)
	}
	cases := []struct {
		name    string
		body    string
		message string
	}{
		{"无触发规格", `{"goal":"目标"}`, "需指定间隔或 cron 之一"},
		{"双触发规格", `{"goal":"目标","intervalSecs":600,"cron":"0 * * * *"}`, "二选一"},
		{"间隔低于下限", `{"goal":"目标","intervalSecs":299}`, "定时间隔至少 300 秒"},
		{"间隔非整数", `{"goal":"目标","intervalSecs":1.5}`, "定时间隔必须是整数"},
		{"cron 缺字段", `{"goal":"目标","cron":"* * * *"}`, "5 个字段"},
		{"cron 取值越界", `{"goal":"目标","cron":"61 * * * *"}`, "61 * * * *"},
		{"goal 为空", `{"goal":"  ","intervalSecs":600}`, "运行目标"},
		{"计划成员不存在", `{"goal":"目标","intervalSecs":600,"plan":[{"id":"a","memberId":"ghost","prompt":"x"}]}`, "ghost"},
		{"验证命令未过白名单", `{"goal":"目标","intervalSecs":600,"verifyCommand":["rm -rf /"]}`, "验证命令"},
		{"requestId 非 UUID", `{"requestId":"not-uuid","goal":"目标","intervalSecs":600}`, "保存请求标识无效"},
		{"enabled 非布尔", `{"goal":"目标","intervalSecs":600,"enabled":"yes"}`, "布尔值"},
	}
	for _, testCase := range cases {
		response := put(testCase.body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400: %d %s", testCase.name, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), testCase.message) {
			t.Fatalf("%s 报错应含 %q: %s", testCase.name, testCase.message, response.Body.String())
		}
	}
	// 团队不存在 404。
	response := env.request(http.MethodPut, "/api/teams/team-not-exist-x/schedule", `{"goal":"x","intervalSecs":600}`)
	if response.Code != http.StatusNotFound {
		t.Fatalf("未知团队应 404: %d %s", response.Code, response.Body.String())
	}
	// 未装配账本（旧网关形态）409。
	schedules := env.server.TeamSchedules
	env.server.TeamSchedules = nil
	if response = put(`{"goal":"x","intervalSecs":600}`); response.Code != http.StatusConflict {
		t.Fatalf("未装配应 409: %d %s", response.Code, response.Body.String())
	}
	env.server.TeamSchedules = schedules
}

// TestTeamsSchedulePutGetAndIdempotency PUT/GET /schedule 的写入、簿记保留
// 与 requestId 幂等：触发规格未变时 nextDue/lastRun/pendingTriggerID 原样
// 保留（journal-first 触发协议的标记绝不被配置写清掉），规格变更清 nextDue
// 让调度器按新规格重锚；同 requestId 同内容重放 duplicate:true 且不推版本、
// 异内容 400；GET 无配置 404；响应含 pendingTriggerId（触发协议可观测）。
func TestTeamsSchedulePutGetAndIdempotency(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	base := "/api/teams/" + teamID + "/schedule"
	// GET 无配置：404（前端契约——未配置态）。
	if response := env.request(http.MethodGet, base, ""); response.Code != http.StatusNotFound {
		t.Fatalf("未配置 GET 应 404: %d %s", response.Code, response.Body.String())
	}
	// 首写：间隔模式。
	body := `{"requestId":"11111111-1111-4111-8111-111111111111","goal":"每十分钟巡检","intervalSecs":600,"enabled":true}`
	response := env.request(http.MethodPut, base, body)
	if response.Code != http.StatusOK {
		t.Fatalf("PUT 应 200: %d %s", response.Code, response.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["goal"] != "每十分钟巡检" || saved["intervalSecs"].(float64) != 600 || saved["enabled"] != true {
		t.Fatalf("保存响应异常: %s", response.Body.String())
	}
	version := saved["version"].(float64)
	if _, exists := saved["cron"]; exists {
		t.Fatalf("间隔模式不应带 cron: %s", response.Body.String())
	}
	// 回读一致。
	response = env.request(http.MethodGet, base, "")
	if response.Code != http.StatusOK {
		t.Fatalf("GET 应 200: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "每十分钟巡检") {
		t.Fatalf("GET 应回读配置: %s", response.Body.String())
	}
	// 幂等重放：同 requestId 同内容 → duplicate:true、版本不推。
	response = env.request(http.MethodPut, base, body)
	if response.Code != http.StatusOK {
		t.Fatalf("重放应 200: %d %s", response.Code, response.Body.String())
	}
	saved = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &saved)
	if saved["duplicate"] != true {
		t.Fatalf("重放应 duplicate:true: %s", response.Body.String())
	}
	if saved["version"].(float64) != version {
		t.Fatalf("重放不应推版本: %v → %v", version, saved["version"])
	}
	// 同 requestId 异内容：400。
	response = env.request(http.MethodPut, base, `{"requestId":"11111111-1111-4111-8111-111111111111","goal":"改了目标","intervalSecs":600}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "不同内容") {
		t.Fatalf("异内容重放应 400: %d %s", response.Code, response.Body.String())
	}
	// 簿记保留：fixture 预置 nextDue/lastRun/pendingTriggerID 后改 goal（规格
	// 不变）——簿记原样带回且响应可观测 pendingTriggerId。
	due := time.Now().Add(time.Hour).UTC()
	seed := env.server.TeamSchedules.Schedule(teamID)
	seed.NextDue = due
	seed.LastRunID = "run-last-0001"
	seed.LastRunAt = time.Now().UTC()
	seed.PendingTriggerID = "trig-pending-0001"
	if err := env.server.TeamSchedules.SaveSchedule(seed); err != nil {
		t.Fatal(err)
	}
	response = env.request(http.MethodPut, base, `{"requestId":"22222222-2222-4222-8222-222222222222","goal":"目标升级","intervalSecs":600}`)
	if response.Code != http.StatusOK {
		t.Fatalf("改 goal PUT 应 200: %d %s", response.Code, response.Body.String())
	}
	saved = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &saved)
	if saved["pendingTriggerId"] != "trig-pending-0001" {
		t.Fatalf("进行中触发标记应随响应可观测: %s", responseText(t, saved))
	}
	if saved["lastRunId"] != "run-last-0001" {
		t.Fatalf("lastRunId 簿记应保留: %s", responseText(t, saved))
	}
	if next, _ := saved["nextDue"].(string); next == "" {
		t.Fatalf("规格未变时 nextDue 应保留: %s", responseText(t, saved))
	} else if parsed, parseErr := time.Parse(time.RFC3339Nano, next); parseErr != nil || !parsed.Equal(due) {
		t.Fatalf("规格未变时 nextDue 应原样保留 %v: %s", due, next)
	}
	// 触发规格变更（间隔→cron）：nextDue 清零（调度器按新规格重锚），触发
	// 标记仍在。
	response = env.request(http.MethodPut, base, `{"requestId":"33333333-3333-4333-8333-333333333333","goal":"目标升级","cron":"15 3 * * *"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("切 cron PUT 应 200: %d %s", response.Code, response.Body.String())
	}
	saved = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &saved)
	if _, exists := saved["nextDue"]; exists {
		t.Fatalf("规格变更应清 nextDue 待调度器重锚: %s", responseText(t, saved))
	}
	if saved["pendingTriggerId"] != "trig-pending-0001" {
		t.Fatalf("触发标记不因配置写丢失: %s", responseText(t, saved))
	}
	// 账本落盘核对：换实例重读同一文件（fixture 绕开下限走的就是这条路径）。
	reloaded := team.LoadScheduleStore(filepath.Join(env.dataDir, "team-schedules.json"))
	if stored := reloaded.Schedule(teamID); stored == nil || stored.Cron != "15 3 * * *" || stored.PendingTriggerID != "trig-pending-0001" {
		t.Fatalf("账本落盘与内存不一致: %+v", stored)
	}
}

// TestTeamsPauseRunRoute pause 路由三态（§12 阶段四工作项 4）：进行中 Run
// 200→RunDetail 带 paused 状态与 stopReason；重复暂停 409；paused 经同一
// resume 端点续跑（零新路由）；取消后（终态）pause 409；run_paused 事件
// 经 OnEvent→Ingest 进 changes 流。
func TestTeamsPauseRunRoute(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"暂停巡检","mode":"single"}`)
	env.waitForRunStatus(t, teamID, runID, "dispatching")
	// 进行中暂停：200 + RunDetail。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("pause 应 200: %d %s", response.Code, response.Body.String())
	}
	var paused map[string]any
	paused = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &paused)
	if paused["status"] != team.RunPaused {
		t.Fatalf("pause 后状态应为 paused: %s", response.Body.String())
	}
	if reason, _ := paused["stopReason"].(string); !strings.Contains(reason, "暂停") {
		t.Fatalf("stopReason 应说明用户暂停: %v", paused["stopReason"])
	}
	// 重复暂停：409。
	if response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); response.Code != http.StatusConflict {
		t.Fatalf("重复暂停应 409: %d %s", response.Code, response.Body.String())
	}
	// run_paused 事件进 changes（OnEvent→Ingest 无白名单）。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	if !strings.Contains(response.Body.String(), team.EventRunPaused) {
		t.Fatalf("changes 应含 run_paused: %s", response.Body.String())
	}
	// paused → 同一 resume 端点续跑（前端契约：同一端点、零新路由）。
	if response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/resume", `{}`); response.Code != http.StatusOK {
		t.Fatalf("paused 续跑应 200: %d %s", response.Code, response.Body.String())
	}
	paused = map[string]any{}
	json.Unmarshal(response.Body.Bytes(), &paused)
	if paused["status"] != team.RunDispatching {
		t.Fatalf("续跑后应回执行态: %s", response.Body.String())
	}
	// 终态 Run：取消后 pause 409。
	env.waitForRunStatus(t, teamID, runID, "dispatching")
	if response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`); response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	if response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/pause", `{}`); response.Code != http.StatusConflict {
		t.Fatalf("终态 pause 应 409: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamsRefreshRunRoute refresh 端点（§12 阶段四工作项 2 手动入口）：
// waiting_input Run 的账本事实从 unknown 翻转为 failed 后刷新——reconciled:
// true、任务事实单向纠正、run 状态绝不变更（recovery-refresh 铁律）；二次
// 刷新 reconciled:false（幂等、版本不再前进）；终态 Run 刷新 200 no-op。
func TestTeamsRefreshRunRoute(t *testing.T) {
	env := newTeamTestEnv(t, "unknown", true)
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"刷新对账","mode":"single"}`)
	waiting := env.waitForRunStatus(t, teamID, runID, "waiting_input")
	if task := teamTaskOf(t, waiting, "lead"); task["status"] != "outcome_unknown" {
		t.Fatalf("任务应 outcome_unknown: %v", task["status"])
	}
	// 账本事实翻转：unknown → failed（探到确定事实才纠正）。
	env.queue.mu.Lock()
	for key, entry := range env.queue.entries {
		entry.status = "failed"
		env.queue.entries[key] = entry
	}
	env.queue.mu.Unlock()
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/refresh", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("refresh 应 200: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Run        map[string]any `json:"run"`
		Reconciled bool           `json:"reconciled"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !result.Reconciled {
		t.Fatalf("探到确定事实的刷新应 reconciled:true: %s", response.Body.String())
	}
	if result.Run["status"] != "waiting_input" {
		t.Fatalf("刷新绝不改 run 状态: %s", response.Body.String())
	}
	if task := teamTaskOf(t, result.Run, "lead"); task["status"] != "failed" {
		t.Fatalf("unknown 应单向纠正为 failed: %s", response.Body.String())
	}
	// 二次刷新：无新事实，reconciled:false、版本不推。
	before := env.runDetail(t, teamID, runID)["version"].(float64)
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/refresh", `{}`)
	json.Unmarshal(response.Body.Bytes(), &result)
	if result.Reconciled {
		t.Fatalf("无新事实的刷新应 reconciled:false: %s", response.Body.String())
	}
	if after := env.runDetail(t, teamID, runID)["version"].(float64); after != before {
		t.Fatalf("幂等刷新不应推版本: %v → %v", before, after)
	}
	// 终态 Run：no-op 200。
	env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	env.waitForRunStatus(t, teamID, runID, "cancelled")
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/refresh", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("终态 refresh 应 200: %d %s", response.Code, response.Body.String())
	}
	json.Unmarshal(response.Body.Bytes(), &result)
	if result.Reconciled || result.Run["status"] != "cancelled" {
		t.Fatalf("终态刷新应 no-op: %s", response.Body.String())
	}
	// 重发铁律：全程只有首派那一次入账。
	if count := env.queue.enqueueCount(); count != 1 {
		t.Fatalf("刷新绝不重发任务：enqueue 次数=%d", count)
	}
}

// TestTeamsRunStatsEndpoint /stats 形状（§12 阶段四工作项 5）：运行中 Run
// 直出 ComputeRunStats（coverage full、tasks 数组、budget 快照、tokens 恒
// null）；旧 journal（事件无时间戳）的 fixture Run coverage=none 且 tasks
// 为空数组（无派发事实）——绝不 null、绝不编数字。
func TestTeamsRunStatsEndpoint(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"统计巡检","mode":"single"}`)
	// 等 task_dispatching 落账（run 级状态在创建即可达 dispatching，先于循环
	// 首拍——按任务状态轮询，不用固定 sleep，§13.1）。
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if task := teamTaskOf(t, env.runDetail(t, teamID, runID), "lead"); task["status"] == "dispatching" {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/stats", "")
	if response.Code != http.StatusOK {
		t.Fatalf("stats 应 200: %d %s", response.Code, response.Body.String())
	}
	var stats struct {
		RunElapsedMs  float64 `json:"runElapsedMs"`
		DispatchCount float64 `json:"dispatchCount"`
		Tokens        any     `json:"tokens"`
		Tasks         []any   `json:"tasks"`
		Coverage      string  `json:"coverage"`
		Budget        struct {
			Units     float64 `json:"units"`
			Used      float64 `json:"used"`
			Ceiling   float64 `json:"ceiling"`
			Exhausted bool    `json:"exhausted"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Coverage != team.StatsCoverageFull {
		t.Fatalf("运行期事件已盖章，coverage 应 full: %s", response.Body.String())
	}
	if stats.RunElapsedMs < 0 || stats.DispatchCount < 1 || len(stats.Tasks) < 1 {
		t.Fatalf("统计形状异常: %s", response.Body.String())
	}
	if stats.Tokens != nil {
		t.Fatalf("tokens 恒 null（按任务归因暂不支持）: %s", response.Body.String())
	}
	if stats.Budget.Units != 0 || stats.Budget.Used < 1 || stats.Budget.Exhausted {
		t.Fatalf("默认策略只统计不暂停: %s", response.Body.String())
	}
	// 旧 journal fixture：run_created 事件零时间戳、无派发事实——coverage
	// none + tasks 空数组（非 null）。
	fixtureID := "run-stats-legacy1"
	env.seedStage4Run(t, teamID, fixtureID, nil)
	env.emitTeamEvent(t, &team.Event{ID: "ev-legacy", RunID: fixtureID, Type: team.EventRunCreated,
		Source: "user", Payload: map[string]any{"goal": "旧 Run"}})
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+fixtureID+"/stats", "")
	if response.Code != http.StatusOK {
		t.Fatalf("历史 Run stats 应 200: %d %s", response.Code, response.Body.String())
	}
	var legacy struct {
		Tasks    []any  `json:"tasks"`
		Coverage string `json:"coverage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Coverage != team.StatsCoverageNone {
		t.Fatalf("零时间戳 journal 应明示无数据: %s", response.Body.String())
	}
	if legacy.Tasks == nil || len(legacy.Tasks) != 0 {
		t.Fatalf("无派发事实的 tasks 应为空数组（非 null）: %s", response.Body.String())
	}
	// 404：未知 Run。
	if response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/run-not-exist1/stats", ""); response.Code != http.StatusNotFound {
		t.Fatalf("未知 Run stats 应 404: %d", response.Code)
	}
}

// TestTeamsRunGraphEndpoint /graph 端到端（§11.3）：fixture Run（broadcast
// 组 + autoPlan 派生链 + 依赖）经 HTTP 输出 nodes/layers，节点字段按
// apiContract（id/label/status/memberIds/agentKinds/dependsOn/runCount/
// resultHead/truncated——agentKinds 必带，web 成员徽标依赖）。
func TestTeamsRunGraphEndpoint(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	// 成员表补两成员供 agentKinds 解析（seedRunTeam 只有 lead）。
	teamRow := env.store.Team(teamID)
	teamRow.Members = append(teamRow.Members,
		team.TeamMember{ID: "dev", SessionID: "sess-team-2", AgentKind: "zcode", Role: team.RoleMember, Enabled: true},
		team.TeamMember{ID: "rev", SessionID: "sess-team-3", AgentKind: "claude", Role: team.RoleReviewer, Enabled: true})
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	runID := "run-graph-fixture1"
	env.seedStage4Run(t, teamID, runID, func(run *team.Run) {
		run.Mode = team.ModeBroadcast
		run.Tasks = []team.Task{
			{ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: "第一轮规划"},
			{ID: "lead-d1-0", MemberID: "dev", Status: team.TaskCompleted, DependsOn: []string{"lead"}, Result: "委派完成"},
			{ID: "lead-s2-0", MemberID: "rev", Status: team.TaskRunning, DependsOn: []string{"lead-d1-0"}},
			{ID: "scan-lead", MemberID: "lead", Status: team.TaskCompleted, BroadcastGroup: "scan", DependsOn: []string{"lead-d1-0"}},
			{ID: "scan-dev", MemberID: "dev", Status: team.TaskFailed, BroadcastGroup: "scan", DependsOn: []string{"lead-d1-0"}},
		}
	})
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/graph", "")
	if response.Code != http.StatusOK {
		t.Fatalf("graph 应 200: %d %s", response.Code, response.Body.String())
	}
	var graph struct {
		Nodes []struct {
			ID         string   `json:"id"`
			Label      string   `json:"label"`
			Status     string   `json:"status"`
			MemberIDs  []string `json:"memberIds"`
			AgentKinds []string `json:"agentKinds"`
			DependsOn  []string `json:"dependsOn"`
			RunCount   float64  `json:"runCount"`
			ResultHead string   `json:"resultHead"`
			Truncated  bool     `json:"truncated"`
		} `json:"nodes"`
		Layers [][]string `json:"layers"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &graph); err != nil {
		t.Fatal(err)
	}
	nodeOf := func(id string) (struct {
		ID         string   `json:"id"`
		Label      string   `json:"label"`
		Status     string   `json:"status"`
		MemberIDs  []string `json:"memberIds"`
		AgentKinds []string `json:"agentKinds"`
		DependsOn  []string `json:"dependsOn"`
		RunCount   float64  `json:"runCount"`
		ResultHead string   `json:"resultHead"`
		Truncated  bool     `json:"truncated"`
	}, bool) {
		for _, node := range graph.Nodes {
			if node.ID == id {
				return node, true
			}
		}
		return graph.Nodes[0], false
	}
	if len(graph.Nodes) != 2 {
		t.Fatalf("应聚合成两节点（派生链归并根任务、broadcast 组聚合单节点）: %s", response.Body.String())
	}
	chain, ok := nodeOf("lead")
	if !ok || chain.RunCount != 3 || chain.Status != team.TaskRunning {
		t.Fatalf("派生链应聚合 runCount=3 且状态取最新: %s", response.Body.String())
	}
	if len(chain.MemberIDs) != 3 || len(chain.AgentKinds) != 3 || chain.AgentKinds[0] != "kimi" {
		t.Fatalf("链节点成员/类型应跨实例聚合: %s", responseText(t, chain))
	}
	if chain.ResultHead != "委派完成" {
		t.Fatalf("链结果取最近一次带结果的执行: %s", responseText(t, chain))
	}
	group, ok := nodeOf("scan")
	if !ok || len(group.MemberIDs) != 2 || group.RunCount != 1 || group.Status != team.TaskFailed {
		t.Fatalf("broadcast 组应聚合单节点: %s", response.Body.String())
	}
	if len(group.DependsOn) != 1 || group.DependsOn[0] != "lead" {
		t.Fatalf("组依赖应映射到派生链根节点: %s", responseText(t, group))
	}
	if len(graph.Layers) < 2 || graph.Layers[0][0] != "lead" {
		t.Fatalf("Kahn 分层应 lead 在首层: %+v", graph.Layers)
	}
	// 404：未知 Run / 越权团队。
	if response = env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/run-not-exist1/graph", ""); response.Code != http.StatusNotFound {
		t.Fatalf("未知 Run graph 应 404: %d", response.Code)
	}
}

// TestTeamsRunDetailInstructionsHead 契约确认（§12 阶段四工作项 6）：
// tasks[].instructionsHead 已随 RunDetail 全字段直出下发（teamRunDetail 剔除
// resultDetail/sentText 之外的账本字段原样保留），240 rune 截断由
// team.InstructionsHead 在创建期落账——本测试钉住契约防回归，非新增投影。
func TestTeamsRunDetailInstructionsHead(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	longPrompt := strings.Repeat("析", 300) + "尾部"
	runID := env.createRun(t, teamID, `{"goal":"摘要契约","mode":"pipeline","plan":[`+
		`{"id":"design","memberId":"lead","prompt":"`+longPrompt+`"}]}`)
	detail := env.runDetail(t, teamID, runID)
	task := teamTaskOf(t, detail, "design")
	head, _ := task["instructionsHead"].(string)
	if got := len([]rune(head)); got != 240 {
		t.Fatalf("instructionsHead 应截 240 rune: %d", got)
	}
	if !strings.HasPrefix(longPrompt, head) {
		t.Fatalf("instructionsHead 应是 Prompt 前缀: %s", head)
	}
	// 派发全文不外发（遗留④口径不变）。
	if _, exists := task["sentText"]; exists {
		t.Fatalf("sentText 不应进 detail: %s", responseText(t, task))
	}
}

// TestTeamsRunDetailStage4Fields RunDetail 的阶段四新字段（scheduledFrom/
// budgetCeiling/paused 状态值域）经 Run 全字段 MarshalJSON 自动带出——
// 投影零手写、测试断言防回归。
func TestTeamsRunDetailStage4Fields(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := "run-detail-stage41"
	env.seedStage4Run(t, teamID, runID, func(run *team.Run) {
		run.Status = team.RunPaused
		run.StopReason = "用户暂停运行"
		run.ScheduledFrom = "trig-stage4-0001"
		run.BudgetCeiling = 7
	})
	detail := env.runDetail(t, teamID, runID)
	if detail["status"] != team.RunPaused {
		t.Fatalf("paused 状态值域应直出: %s", responseText(t, detail))
	}
	if detail["scheduledFrom"] != "trig-stage4-0001" {
		t.Fatalf("scheduledFrom 应直出: %s", responseText(t, detail))
	}
	if detail["budgetCeiling"].(float64) != 7 {
		t.Fatalf("budgetCeiling 应直出: %s", responseText(t, detail))
	}
	// 普通用户 Run：scheduledFrom/budgetCeiling 零值不外发（omitempty）。
	plainID := env.createRun(t, teamID, `{"goal":"普通 Run","mode":"single"}`)
	plain := env.runDetail(t, teamID, plainID)
	if _, exists := plain["scheduledFrom"]; exists {
		t.Fatalf("非定时 Run 不应带 scheduledFrom: %s", responseText(t, plain))
	}
	if _, exists := plain["budgetCeiling"]; exists {
		t.Fatalf("未追加预算的 Run 不应带 budgetCeiling: %s", responseText(t, plain))
	}
}

// TestTeamsRunMetaStage4NoNewKeys meta 键集回归（§12 阶段四：stats/graph/
// schedule 不进 meta；阶段四只扩 status 值域，不新增键——meta 逐键变更触发
// 前端整页重渲染契约）。对照 phase2/3 键集逐键断言相等。
func TestTeamsRunMetaStage4NoNewKeys(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"meta 键集","mode":"single"}`)
	metaKeys := func(run map[string]any) []string {
		response := env.request(http.MethodGet,
			"/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
		var page struct {
			Meta map[string]any `json:"meta"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || page.Meta == nil {
			t.Fatalf("changes meta 异常: %s (%v)", response.Body.String(), err)
		}
		keys := make([]string, 0, len(page.Meta))
		for key := range page.Meta {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return keys
	}
	// 进行中 Run：phase2/3 键集（mode 直出 single；无 verifyCommand 无 verify）。
	want := []string{"collectPolicy", "mode", "pendingQuestions", "runId", "status", "stopReason", "taskCounts", "teamId", "workspace"}
	env.waitForRunStatus(t, teamID, runID, "dispatching")
	if got := metaKeys(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("meta 键集漂移: got %v want %v", got, want)
	}
	// paused Run：仍是同一键集（paused 只是 status 的值域扩展）。
	if _, err := env.server.Teams.Pause(runID); err != nil {
		t.Fatal(err)
	}
	if got := metaKeys(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("paused Run 的 meta 键集漂移: got %v want %v", got, want)
	}
}

// TestTeamsStage4EventStreamChanges 阶段四新事件（run_paused/run_stalled/
// run_budget_resumed）经 OnEvent→TeamFeedRegistry.Ingest 自动入流（无白
// 名单），照七类新事件先例断言 changes 增量可见且 payload 有界。
func TestTeamsStage4EventStreamChanges(t *testing.T) {
	env := newTeamTestEnv(t, "queued", false)
	teamID := env.seedRunTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"新事件","mode":"single"}`)
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID+"/changes?after=-1", "")
	var anchor struct {
		Epoch    string `json:"epoch"`
		Sequence uint64 `json:"sequence"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &anchor); err != nil || anchor.Epoch == "" {
		t.Fatalf("changes 锚页异常: %s (%v)", response.Body.String(), err)
	}
	for _, event := range []*team.Event{
		{ID: "ev-p4a", RunID: runID, Type: team.EventRunPaused, Source: "user", Payload: map[string]any{
			"reason": "用户暂停运行", "sequence": jsoncompat.Num(42)}},
		{ID: "ev-p4b", RunID: runID, Type: team.EventRunStalled, Source: "runner", Payload: map[string]any{
			"sinceMs": jsoncompat.Num(1200000), "lastTaskId": "lead"}},
		{ID: "ev-p4c", RunID: runID, Type: team.EventRunBudgetResumed, Source: "user", Payload: map[string]any{
			"ceiling": jsoncompat.Num(12), "units": jsoncompat.Num(5), "used": jsoncompat.Num(7)}},
	} {
		env.emitTeamEvent(t, event)
	}
	response = env.request(http.MethodGet,
		"/api/teams/"+teamID+"/runs/"+runID+"/changes?after="+
			url.QueryEscape(strconv.FormatUint(anchor.Sequence, 10))+"&epoch="+url.QueryEscape(anchor.Epoch), "")
	if response.Code != http.StatusOK {
		t.Fatalf("changes 应 200: %d %s", response.Code, response.Body.String())
	}
	var page struct {
		Rows []struct {
			Type    string         `json:"type"`
			Payload map[string]any `json:"payload"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{team.EventRunPaused, team.EventRunStalled, team.EventRunBudgetResumed}
	if len(page.Rows) != len(wantTypes) {
		t.Fatalf("增量页应携带全部三类新事件: %s", response.Body.String())
	}
	for i, want := range wantTypes {
		if page.Rows[i].Type != want {
			t.Fatalf("第 %d 行应是 %s，实际 %s", i, want, page.Rows[i].Type)
		}
		if len(page.Rows[i].Payload) == 0 {
			t.Fatalf("%s 行应带 payload: %s", want, response.Body.String())
		}
		if encoded := responseText(t, page.Rows[i]); len(encoded) > 8192 {
			t.Fatalf("%s 行超出有界上限: %d bytes", want, len(encoded))
		}
	}
}

// TestTeamsStage4EventStreamSSE 阶段四新事件经 SSE 下发：订阅期间落一条
// run_stalled → changes 帧携带（Ingest→Notify→帧）。
func TestTeamsStage4EventStreamSSE(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	api := httptest.NewServer(env.server)
	hostPort := api.URL[len("http://"):]
	env.server.Hosts[hostPort] = true
	env.server.Origins["http://"+hostPort] = true
	env.server.LocalHosts[hostPort] = true
	defer api.Close()

	teamID, _ := env.createTeam(t)
	runID := env.createRun(t, teamID, `{"goal":"SSE 阶段四","mode":"single"}`)
	events := authorizedSSEGet(t, env, api.URL, hostPort,
		"/api/teams/"+teamID+"/runs/"+runID+"/events?incremental=1")
	defer events.Body.Close()
	frames := make(chan string, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(events.Body)
		event, data := "", ""
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
			case line == "\n":
				if event != "" {
					frames <- event + "\n" + data
				}
				event, data = "", ""
			}
		}
	}()
	select {
	case frame := <-frames:
		if !strings.HasPrefix(frame, "ready\n") {
			t.Fatalf("首帧应 ready: %q", frame)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("SSE 未在时限内发出 ready 帧")
	}
	env.emitTeamEvent(t, &team.Event{
		ID: "ev-sse4", RunID: runID, Type: team.EventRunStalled, Source: "runner",
		Payload: map[string]any{"sinceMs": jsoncompat.Num(1200000), "lastTaskId": "lead"},
	})
	deadline := time.After(8 * time.Second)
	for {
		select {
		case frame := <-frames:
			if strings.HasPrefix(frame, "changes\n") && strings.Contains(frame, team.EventRunStalled) {
				events.Body.Close()
				<-readerDone
				return
			}
		case <-deadline:
			t.Fatal("SSE 未在时限内发出携带 run_stalled 的 changes 帧")
		}
	}
}
