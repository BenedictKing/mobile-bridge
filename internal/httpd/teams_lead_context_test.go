package httpd

// teams_lead_context_test.go 覆盖主控上下文管理端点（teams_lead_context.go）：
// /new 只换绑 Lead（幂等、不动其他成员、不建 Run）、rebind 的校验与幂等、
// 版本/预期会话/非终态 Run/排队消息各防线的 409。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// leadContextEnv 在集成环境上补 Creations 账本（newTeamIntegrationEnv 不装配
// 迁移面），并建好「主控 + 单成员」团队。
func leadContextEnv(t *testing.T) (*teamIntegrationEnv, string, string, string) {
	t.Helper()
	env := newTeamIntegrationEnv(t, t.TempDir())
	env.server.Creations = mustLoadCreations(t, filepath.Join(env.dataDir, "creations.json"))
	lead := env.createMemberSession(t, "req-lead", "主控会话")
	member := env.createMemberSession(t, "req-member", "成员会话")
	members := `[{"id":"lead-1","sessionId":"` + lead + `","role":"lead"},` +
		`{"id":"member-1","sessionId":"` + member + `","role":"member"}]`
	teamID := env.createTeamWithMembers(t, members)
	return env, teamID, lead, member
}

func (e *teamIntegrationEnv) leadContext(t *testing.T, teamID, body string) *leadContextResult {
	t.Helper()
	response := e.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context", body)
	if response.Code != http.StatusOK {
		t.Fatalf("lead-context %s → %d %s", body, response.Code, response.Body.String())
	}
	var result leadContextResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

type leadContextResult struct {
	Action    string `json:"action"`
	SessionID string `json:"sessionId"`
	Team      struct {
		ID      string `json:"id"`
		Version uint64 `json:"version"`
		LeadID  string `json:"leadId"`
		Members []struct {
			ID        string `json:"id"`
			SessionID string `json:"sessionId"`
		} `json:"members"`
	} `json:"team"`
}

func (r *leadContextResult) memberSession(memberID string) string {
	for _, member := range r.Team.Members {
		if member.ID == memberID {
			return member.SessionID
		}
	}
	return ""
}

// /new 换绑主控：其他成员不动、版本自增、不创建 Run；同请求 id 重放收敛到
// 同一新会话（不被自增后的版本门挡住）。
func TestLeadContextNewRebindsLeadOnly(t *testing.T) {
	env, teamID, lead, member := leadContextEnv(t)
	teamRow := env.server.TeamStore.Team(teamID)
	body := `{"id":"` + uuid.NewString() + `","action":"new","version":` +
		jsonUint(teamRow.Version) + `,"expectedSessionId":"` + lead + `"}`

	result := env.leadContext(t, teamID, body)
	if result.SessionID == "" || result.SessionID == lead {
		t.Fatalf("new lead session = %q", result.SessionID)
	}
	if got := result.memberSession("lead-1"); got != result.SessionID {
		t.Fatalf("lead member still bound to %q", got)
	}
	if got := result.memberSession("member-1"); got != member {
		t.Fatalf("other member was rebound: %q", got)
	}
	if result.Team.Version != teamRow.Version+1 {
		t.Fatalf("version = %d, want %d", result.Team.Version, teamRow.Version+1)
	}
	if runs := env.server.TeamStore.RunsByTeam(teamID); len(runs) > 0 {
		t.Fatalf("/new created runs: %d", len(runs))
	}

	// 响应丢失后的重试：版本号还是旧的，仍应收敛到同一个新会话。
	replay := env.leadContext(t, teamID, body)
	if replay.SessionID != result.SessionID {
		t.Fatalf("replay created a second session: %q vs %q", replay.SessionID, result.SessionID)
	}
	if replay.Team.Version != result.Team.Version {
		t.Fatalf("replay bumped version again: %d", replay.Team.Version)
	}

	// 原主控会话保留：不被 Drop，账本仍认得它，历史 Run 仍指向它。
	if !env.manager.Knows(lead) {
		t.Fatal("original lead session was dropped from the ledger")
	}
}

// 版本门、预期会话门、非终态 Run、排队消息的 409 分支。
func TestLeadContextGuards(t *testing.T) {
	env, teamID, lead, _ := leadContextEnv(t)
	teamRow := env.server.TeamStore.Team(teamID)

	post := func(body string) (*leadContextResult, int) {
		response := env.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context", body)
		var result leadContextResult
		_ = json.Unmarshal(response.Body.Bytes(), &result)
		return &result, response.Code
	}
	id := uuid.NewString()
	if _, code := post(`{"id":"` + id + `","action":"new","version":` +
		jsonUint(teamRow.Version+5) + `,"expectedSessionId":"` + lead + `"}`); code != http.StatusConflict {
		t.Fatalf("stale version → %d", code)
	}
	if _, code := post(`{"id":"` + id + `","action":"new","version":` +
		jsonUint(teamRow.Version) + `,"expectedSessionId":"not-the-lead"}`); code != http.StatusConflict {
		t.Fatalf("wrong expected session → %d", code)
	}
	if _, code := post(`{"id":"` + id + `","action":"dance","version":` +
		jsonUint(teamRow.Version) + `,"expectedSessionId":"` + lead + `"}`); code != http.StatusBadRequest {
		t.Fatalf("unknown action → %d", code)
	}
	// 非终态 Run：静止校验必须拒绝。
	run := &team.Run{ID: uuid.NewString(), TeamID: teamID, Goal: "进行中", Status: team.RunPlanning}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if _, code := post(`{"id":"` + id + `","action":"new","version":` +
		jsonUint(teamRow.Version) + `,"expectedSessionId":"` + lead + `"}`); code != http.StatusConflict {
		t.Fatalf("active run → %d", code)
	}
	stored := env.store.Run(run.ID)
	if stored == nil {
		t.Fatal("planning run vanished from the store")
	}
	if err := env.store.DeleteRun(stored.ID, stored.Version); err != nil {
		t.Fatal(err)
	}
	// 主控上的排队消息同样拒绝。
	if _, _, err := env.server.Queue.enqueue(lead, uuid.NewString(), "排队中", "queue", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context",
		`{"id":"`+id+`","action":"new","version":`+jsonUint(teamRow.Version)+`,"expectedSessionId":"`+lead+`"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "排队") {
		t.Fatalf("queued entry → %d %s", response.Code, response.Body.String())
	}
}

func TestLeadContextNewAfterCancellingParkedRun(t *testing.T) {
	env, teamID, lead, _ := leadContextEnv(t)
	teamRow := env.store.Team(teamID)
	run := &team.Run{ID: uuid.NewString(), TeamID: teamID, Goal: "停靠的任务", Status: team.RunWaitingInput,
		Tasks: []team.Task{{ID: "pending", MemberID: "lead-1", Status: team.TaskPlanned}}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	stored := env.store.Run(run.ID)
	path := "/api/teams/" + teamID + "/runs/" + run.ID + "/cancel-parked"
	newBody := `{"id":"` + uuid.NewString() + `","action":"new","version":` +
		jsonUint(teamRow.Version) + `,"expectedSessionId":"` + lead + `"}`
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context", newBody); response.Code != http.StatusConflict {
		t.Fatalf("parked run must block /new before cancellation: %d %s", response.Code, response.Body.String())
	}
	if response := env.request(http.MethodPost, path, `{"version":`+jsonUint(stored.Version+1)+`}`); response.Code != http.StatusConflict {
		t.Fatalf("stale version must not cancel parked run: %d %s", response.Code, response.Body.String())
	}
	if fresh := env.store.Run(run.ID); fresh.Status != team.RunWaitingInput {
		t.Fatalf("stale cancellation changed run: %s", fresh.Status)
	}
	if response := env.request(http.MethodPost, path, `{"version":`+jsonUint(stored.Version)+`}`); response.Code != http.StatusOK {
		t.Fatalf("cancel parked run: %d %s", response.Code, response.Body.String())
	}
	if fresh := env.store.Run(run.ID); fresh.Status != team.RunCancelled || fresh.Tasks[0].Status != team.TaskCancelled {
		t.Fatalf("parked run not cancelled: %+v", fresh)
	}
	result := env.leadContext(t, teamID, newBody)
	if result.SessionID == lead {
		t.Fatal("/new did not switch lead after parked run was cancelled")
	}
	running := &team.Run{ID: uuid.NewString(), TeamID: teamID, Goal: "执行中的任务", Status: team.RunDispatching}
	if err := env.store.SaveRun(running); err != nil {
		t.Fatal(err)
	}
	running = env.store.Run(running.ID)
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+running.ID+"/cancel-parked",
		`{"version":`+jsonUint(running.Version)+`}`)
	if response.Code != http.StatusConflict || env.store.Run(running.ID).Status != team.RunDispatching {
		t.Fatalf("running run must not be conditionally cancelled: %d %s", response.Code, response.Body.String())
	}
}

// rebind（网关转录压缩后的换绑）：合法目标换绑、重复 rebind 幂等、其他成员
// 占用的会话与未知会话拒绝。
func TestLeadContextRebindValidates(t *testing.T) {
	env, teamID, lead, member := leadContextEnv(t)
	teamRow := env.server.TeamStore.Team(teamID)
	compacted := env.createMemberSession(t, "req-compacted", "压缩后的主控")

	body := func(sessionID, expected string, version uint64) string {
		return `{"id":"` + uuid.NewString() + `","action":"rebind","sessionId":"` + sessionID +
			`","version":` + jsonUint(version) + `,"expectedSessionId":"` + expected + `"}`
	}
	result := env.leadContext(t, teamID, body(compacted, lead, teamRow.Version))
	if result.SessionID != compacted || result.memberSession("lead-1") != compacted {
		t.Fatalf("rebind did not land: %+v", result.Team.Members)
	}
	// 同一目标重复 rebind：幂等返回，不再自增版本。
	again := env.leadContext(t, teamID, body(compacted, compacted, result.Team.Version))
	if again.SessionID != compacted || again.Team.Version != result.Team.Version {
		t.Fatalf("idempotent rebind changed state: %+v", again)
	}
	// 其他成员占用的会话不能成为主控。
	response := env.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context",
		body(member, compacted, again.Team.Version))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "已被成员") {
		t.Fatalf("member-held session → %d %s", response.Code, response.Body.String())
	}
	// 未知会话拒绝。
	response = env.request(http.MethodPost, "/api/teams/"+teamID+"/lead-context",
		body("6ccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee", compacted, again.Team.Version))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown session → %d %s", response.Code, response.Body.String())
	}
}

func jsonUint(value uint64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// settingsAgent 是带模型/思考强度选项的第二伪 Agent：session/set_config_option
// 记录并回带新目录，专用于验证 /new 与 rebind 的设置继承。
func settingsAgent(t *testing.T, dir string) []string {
	t.Helper()
	script := `import json,os,sys,uuid
DIR = "` + dir + `"
STATE = {"model": "m-default", "thought_level": "low"}
def record(name, value):
    with open(os.path.join(DIR, name), "a") as fh:
        fh.write(value + "\n")
def options():
    return [
        {"id": "model", "type": "category", "currentValue": STATE["model"],
         "values": [{"value": "m-default"}, {"value": "m-pro"}]},
        {"id": "thought_level", "type": "category", "currentValue": STATE["thought_level"],
         "values": [{"value": "low"}, {"value": "high"}]},
    ]
for line in sys.stdin:
    m=json.loads(line); method=m.get("method"); ident=m.get("id")
    if method == "initialize":
        result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
    elif method == "session/new":
        result={"sessionId":"set-"+uuid.uuid4().hex[:10],"modes":{"currentModeId":"default","availableModes":[{"id":"default"}]},"configOptions":options()}
    elif method == "session/load":
        result={"sessionId":(m.get("params") or {}).get("sessionId"),"modes":{"currentModeId":"default","availableModes":[{"id":"default"}]},"configOptions":options()}
    elif method == "session/set_config_option":
        params=m.get("params") or {}
        cid=params.get("configId",""); value=params.get("value","")
        record("sets.txt", (params.get("sessionId") or "?")+" "+cid+"="+value)
        if cid in STATE:
            STATE[cid]=value
        result={"configOptions":options()}
    elif method == "session/prompt":
        result={"stopReason":"end_turn"}
    else:
        result={}
    print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"/usr/bin/env", "python3", "-u", path}
}

// /new 换绑时新主控继承旧主控的模型与推理强度（网关侧 effort 词经 EffortMap
// 反查），且继承失败绝不阻断换绑。
func TestLeadContextNewInheritsSettings(t *testing.T) {
	env, _, _, _ := leadContextEnv(t)
	agentDir := t.TempDir()
	manager := acpruntime.NewCommand(settingsAgent(t, agentDir))
	manager.Label = "Settings"
	manager.EffortMap = map[string]string{"low": "low", "high": "high"}
	env.server.ACPAgents["kimi2"] = manager
	t.Cleanup(func() { manager.Close() })

	lead, err := manager.Create("req-settings-lead", env.dataDir, "设置主控", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings(lead, "m-pro", "high", nil); err != nil {
		t.Fatalf("预设旧主控设置: %v", err)
	}
	teamID := env.createTeamWithMembers(t, `[{"id":"lead-2","sessionId":"`+lead.ID+`","role":"lead"}]`)
	teamRow := env.server.TeamStore.Team(teamID)

	result := env.leadContext(t, teamID, `{"id":"`+uuid.NewString()+`","action":"new","version":`+
		jsonUint(teamRow.Version)+`,"expectedSessionId":"`+lead.ID+`"}`)
	sets := recordedFile(agentDir, "sets.txt")
	if !strings.Contains(sets, result.SessionID+" model=m-pro") ||
		!strings.Contains(sets, result.SessionID+" thought_level=high") {
		t.Fatalf("新主控未继承设置（sets=%q）", sets)
	}
}

// 普通会话 /new：POST /api/sessions 携带 inheritFrom，新会话继承源会话的
// 模型与推理强度（teams_lead_context.go 的继承助手被创建端点复用）。
func TestSessionsCreateInheritsSettings(t *testing.T) {
	agentDir := t.TempDir()
	manager := acpruntime.NewCommand(settingsAgent(t, agentDir))
	manager.Label = "Settings"
	manager.EffortMap = map[string]string{"low": "low", "high": "high"}
	defer manager.Close()

	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi2": manager}
	server.Creations = mustLoadCreations(t, filepath.Join(t.TempDir(), "creations.json"))

	source, err := manager.Create("req-plain-lead", t.TempDir(), "源会话", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.UpdateSettings(source, "m-pro", "high", nil); err != nil {
		t.Fatalf("预设源会话设置: %v", err)
	}
	body := `{"id":"` + uuid.NewString() + `","agent":"kimi2","cwd":"` + t.TempDir() + `","title":"/new 新会话","inheritFrom":"` + source.ID + `"}`
	response := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", body)
	if response.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || created.ID == "" || created.ID == source.ID {
		t.Fatalf("created=%q err=%v", created.ID, err)
	}
	sets := recordedFile(agentDir, "sets.txt")
	if !strings.Contains(sets, created.ID+" model=m-pro") ||
		!strings.Contains(sets, created.ID+" thought_level=high") {
		t.Fatalf("/new 会话未继承设置（sets=%q）", sets)
	}
}
