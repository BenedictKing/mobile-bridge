package httpd

// teams_test.go 覆盖 /api/teams* 的阶段一契约（docs/team-orchestration-plan.md
// §13.1 httpd 行）：认证/CSRF、校验 400、幂等创建、版本冲突 409、删除拦截、
// 运行中取消与重复取消、interrupted turn 不落 completed、resume 的不再重发、
// changes 游标降级与未知参数容忍、SSE ready→changes 帧序列。QueueAdapter 与
// SessionResolver 用 Go 级 fake；取消/中断路径的 turn 由 python 伪 Agent
//（teamsHoldAgent，hold 到 session/cancel 才结束）驱动。

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
func teamsHoldAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys,threading
DIR = "` + dir + `"
LOCK = threading.Lock()
STATE = {"event": threading.Event(), "cancelled": False}
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
def handle_prompt(m, ident):
 blocks=(m.get("params") or {}).get("prompt") or []
 text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
 record("prompts.txt", text)
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
   STATE["cancelled"]=True; STATE["event"].set()
  result={}
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
	env := &teamTestEnv{server: server, store: store, token: token, csrf: csrf, dataDir: dataDir}
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
	// 上界，§10.3/§15.3）。
	for _, payload := range []string{
		`{"name":"p1","policy":{"workspaceMode":"isolated_worktree"}}`,
		`{"name":"p2","policy":{"autoApplyChanges":true}}`,
		`{"name":"p3","policy":{"maxParallelTasks":0}}`,
		`{"name":"p4","policy":{"maxResultChars":0}}`,
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

	// broadcast 模式拒绝。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"并行","mode":"broadcast","plan":[{"id":"a","memberId":"lead","prompt":"x"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "并行分派在后续阶段提供") {
		t.Fatalf("broadcast 应 400: %d %s", response.Code, response.Body.String())
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
	// prompt 超余量上限：默认 100000−12000−200=87800，报错给出有效上限。
	long := strings.Repeat("长", 88000)
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"超长","mode":"pipeline","plan":[{"id":"t1","memberId":"lead","prompt":"`+long+`"}]}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "87800") {
		t.Fatalf("prompt 超限应 400 且含余量上限: %d %.200s", response.Code, response.Body.String())
	}
	// goal 自身上限（10000 字）。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/runs",
		`{"goal":"`+strings.Repeat("标", 10001)+`","mode":"single"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "1–10000 字的运行目标") {
		t.Fatalf("goal 超限应 400: %d", response.Code)
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
	// 同成员的 exclusive 任务必须能立即派发（fake 账本 accepted → 任务停在
	// dispatching 即为派发成功的可观察证据）。
	secondRun := env.createRun(t, teamID, `{"goal":"取消后重建","mode":"pipeline","plan":[
		{"id":"t2","memberId":"lead","prompt":"hold 取消后的写任务","writeLock":"exclusive"}]}`)
	env.waitForRunStatus(t, teamID, secondRun, "dispatching")
	detail := env.runDetail(t, teamID, secondRun)
	if workspace, _ := detail["workspace"].(map[string]any); workspace["writeLockHeldBy"] != "t2" {
		t.Fatalf("新任务的写锁持有者应为 t2: %v", workspace)
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
