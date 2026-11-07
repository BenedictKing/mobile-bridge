package httpd

// team_inputs_queue_test.go 覆盖持久输入队列的 HTTP 面（docs/
// team-demand-queue-plan.md §3.2/§8 阶段 1）：queueVersion:2 分流的 202 契
// 约（先持久化再返回、不给 runId）、同 ID 幂等与异内容 409、容量 429、
// 编辑/优先/取消的操作幂等与版本冲突、重启后可查询（对同一 dataDir 重建
// 环境），以及旧入口契约不变。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// postQueuedTeamInput 是 POST /inputs（queueVersion:2）的快捷封装。
func (e *teamTestEnv) postQueuedTeamInput(teamID, body string) *httptest.ResponseRecorder {
	return e.request(http.MethodPost, "/api/teams/"+teamID+"/inputs", body)
}

func TestTeamInputsQueueVersionAcceptsAndReplays(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	const inputID = "1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := fmt.Sprintf(`{"id":%q,"text":"给登录页补一套错误态设计","queueVersion":"2"}`, inputID)
	response := env.postQueuedTeamInput(teamID, body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("排队受理应 202: %d %s", response.Code, response.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["status"] != "queued" || accepted["queueStatus"] != "queued" ||
		accepted["inputId"] != inputID || accepted["teamId"] != teamID {
		t.Fatalf("202 契约不符: %v", accepted)
	}
	if runIDs, ok := accepted["runIds"].([]any); !ok || len(runIDs) != 0 {
		t.Fatalf("排队受理不给 runId（runIds 恒空数组）: %v", accepted)
	}
	if _, has := accepted["runId"]; has {
		t.Fatalf("排队受理不得携带同步契约的 runId: %v", accepted)
	}
	// 先持久化：不创建任何 Run（路由是阶段 2）。
	if runs := env.store.RunsByTeam(teamID); len(runs) != 0 {
		t.Fatalf("排队输入不得立即创建 Run: %d", len(runs))
	}
	// 同 ID 同内容重放：202 + duplicate，状态原样。
	response = env.postQueuedTeamInput(teamID, body)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("同内容重放应 202+duplicate: %d %s", response.Code, response.Body.String())
	}
	// 同 ID 异内容：409。
	response = env.postQueuedTeamInput(teamID, `{"id":"`+inputID+`","text":"换一个目标","queueVersion":"2"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "同一消息标识") {
		t.Fatalf("异内容重放应 409: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamInputsQueueLifecycleOperations(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	const inputID = "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postQueuedTeamInput(teamID, `{"id":"`+inputID+`","text":"初版需求","queueVersion":"2"}`); response.Code != http.StatusAccepted {
		t.Fatalf("受理失败: %d %s", response.Code, response.Body.String())
	}

	// 列表投影：队列概况 + 接受序。
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs", "")
	if response.Code != http.StatusOK {
		t.Fatalf("列表失败: %d %s", response.Code, response.Body.String())
	}
	var listed struct {
		PendingCount     int `json:"pendingCount"`
		MaxPendingInputs int `json:"maxPendingInputs"`
		QueueVersion     int `json:"queueVersion"`
		Inputs           []struct {
			InputID     string `json:"inputId"`
			QueueStatus string `json:"queueStatus"`
			Revision    int    `json:"revision"`
			Version     int    `json:"version"`
			Revisions   []struct {
				Revision   int `json:"revision"`
				ReplacedBy int `json:"replacedBy"`
			} `json:"revisions"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.QueueVersion != 2 || listed.PendingCount != 1 || listed.MaxPendingInputs != team.DefaultMaxPendingInputs || len(listed.Inputs) != 1 {
		t.Fatalf("列表概况不符: %+v", listed)
	}

	// 单条回查：queueVersion:2 家族投影。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"queueVersion": 2`) {
		t.Fatalf("单条回查应命中账本投影: %d %s", response.Code, response.Body.String())
	}

	// 编辑：缺版本 400、版本不符 409、正确版本产生 revision 2。
	revise := func(body string) *httptest.ResponseRecorder {
		return patchTeamQueueInput(t, env, teamID, inputID, body)
	}
	if response := revise(`{"id":"11111111-1111-4111-8111-111111111111","text":"修订后的需求"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("缺期望版本应 400: %d %s", response.Code, response.Body.String())
	}
	if response := revise(`{"id":"11111111-1111-4111-8111-111111111111","text":"修订后的需求","version":99}`); response.Code != http.StatusConflict {
		t.Fatalf("版本冲突应 409: %d %s", response.Code, response.Body.String())
	}
	if response := revise(`{"id":"11111111-1111-4111-8111-111111111111","text":"修订后的需求","version":1}`); response.Code != http.StatusOK {
		t.Fatalf("编辑失败: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "")
	var revised struct {
		Revision  int    `json:"revision"`
		Version   int    `json:"version"`
		Text      string `json:"text"`
		Revisions []struct {
			Revision   int    `json:"revision"`
			ReplacedBy int    `json:"replacedBy"`
			Text       string `json:"text"`
		} `json:"revisions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &revised); err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.Version != 2 || revised.Text != "修订后的需求" {
		t.Fatalf("修订后状态不符: %+v", revised)
	}
	if len(revised.Revisions) != 2 || revised.Revisions[0].ReplacedBy != 2 || revised.Revisions[0].Text != "初版需求" {
		t.Fatalf("修订历史应保留旧版本: %+v", revised.Revisions)
	}
	// 同操作重放：幂等 duplicate，不产生第三版。
	if response := revise(`{"id":"11111111-1111-4111-8111-111111111111","text":"修订后的需求","version":1}`); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("操作重放应幂等: %d %s", response.Code, response.Body.String())
	}

	// 立即发送：只标记优先。
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs/"+inputID+"/prioritize",
		`{"id":"22222222-2222-4222-8222-222222222222","version":2}`); response.Code != http.StatusOK {
		t.Fatalf("优先失败: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "").Body.String(), `"priority": true`) {
		t.Fatal("优先标记应落账")
	}

	// 取消：之后编辑/优先都被拒（409 排队操作不可用）。
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs/"+inputID+"/cancel",
		`{"id":"33333333-3333-4333-8333-333333333333","version":3}`); response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	if response := revise(`{"id":"44444444-4444-4444-8444-444444444444","text":"取消后编辑","version":4}`); response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "排队操作不可用") {
		t.Fatalf("取消后编辑应 409: %d %s", response.Code, response.Body.String())
	}
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs/"+inputID+"/prioritize",
		`{"id":"55555555-5555-4555-8555-555555555555","version":4}`); response.Code != http.StatusConflict {
		t.Fatalf("取消后优先应 409: %d %s", response.Code, response.Body.String())
	}
	// 取消释放容量：pendingCount 归零。
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs", "")
	if !strings.Contains(response.Body.String(), `"pendingCount": 0`) {
		t.Fatalf("取消后待处理应归零: %s", response.Body.String())
	}
}

// patchTeamQueueInput 是 PATCH /inputs/:id 的快捷封装。
func patchTeamQueueInput(t *testing.T, env *teamTestEnv, teamID, inputID, body string) *httptest.ResponseRecorder {
	t.Helper()
	t.Helper()
	return env.request(http.MethodPatch, "/api/teams/"+teamID+"/inputs/"+inputID, body)
}

func TestTeamInputsQueueCapacityBackpressure(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, version := env.createTeam(t)
	// 压低容量：maxPendingInputs=2。
	patch := env.request(http.MethodPatch, "/api/teams/"+teamID,
		fmt.Sprintf(`{"policy":{"maxPendingInputs":2},"version":%d}`, version))
	if patch.Code != http.StatusOK {
		t.Fatalf("策略调整失败: %d %s", patch.Code, patch.Body.String())
	}
	accept := func(id string) *httptest.ResponseRecorder {
		return env.postQueuedTeamInput(teamID, fmt.Sprintf(`{"id":%q,"text":"需求 %s","queueVersion":"2"}`, id, id))
	}
	if response := accept("3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); response.Code != http.StatusAccepted {
		t.Fatalf("第一条受理失败: %d %s", response.Code, response.Body.String())
	}
	if response := accept("4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); response.Code != http.StatusAccepted {
		t.Fatalf("第二条受理失败: %d %s", response.Code, response.Body.String())
	}
	response := accept("5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "上限 2") {
		t.Fatalf("容量满应 429 并说明容量: %d %s", response.Code, response.Body.String())
	}
	// 取消一条后恢复准入。
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs/3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/cancel",
		`{"id":"66666666-6666-4666-8666-666666666666","version":1}`); response.Code != http.StatusOK {
		t.Fatalf("取消失败: %d %s", response.Code, response.Body.String())
	}
	if response := accept("5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); response.Code != http.StatusAccepted {
		t.Fatalf("取消后应恢复准入: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamInputsQueueSessionSendGate(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	// teamQueueVersion:2 → 持久队列（202 queued、无 runId、零 Run）。
	const queuedID = "6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sendBody := fmt.Sprintf(`{"id":%q,"teamId":%q,"text":"走队列的会话消息","mode":"send","workMode":"default","teamQueueVersion":"2"}`, queuedID, teamID)
	response := env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/send?host=local", sendBody)
	if response.Code != http.StatusAccepted {
		t.Fatalf("会话入口排队受理应 202: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"runId"`) || !strings.Contains(response.Body.String(), `"queueStatus": "queued"`) {
		t.Fatalf("排队受理不给 runId: %s", response.Body.String())
	}
	if runs := env.store.RunsByTeam(teamID); len(runs) != 0 {
		t.Fatalf("排队消息不得立即创建 Run: %d", len(runs))
	}
	// 不带开关的旧路径：立即创建 Run（旧客户端契约不变）。
	const legacyID = "7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	legacyBody := fmt.Sprintf(`{"id":%q,"teamId":%q,"text":"走旧路径的消息","mode":"send","workMode":"default"}`, legacyID, teamID)
	response = env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/send?host=local", legacyBody)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"runId": "`+legacyID+`"`) {
		t.Fatalf("旧路径应保持同步 runId 契约: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamInputsQueueSurvivesGatewayRestart(t *testing.T) {
	dataDir := t.TempDir()
	env := newTeamIntegrationEnv(t, dataDir)
	lead := env.createMemberSession(t, "lead-sess", "主控")
	teamID := env.createTeamWithMembers(t, `[{"id":"lead","sessionId":"`+lead+`","role":"lead"}]`)
	const inputID = "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := fmt.Sprintf(`{"id":%q,"text":"重启前接受的输入","queueVersion":"2"}`, inputID)
	if response := env.request(http.MethodPost, "/api/teams/"+teamID+"/inputs", body); response.Code != http.StatusAccepted {
		t.Fatalf("受理失败: %d %s", response.Code, response.Body.String())
	}
	// 重启（同一 dataDir 重建环境）：输入仍可查询、可继续（修订/取消）。
	env = newTeamIntegrationEnv(t, dataDir)
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"queueStatus": "queued"`) {
		t.Fatalf("重启后输入应仍可查询: %d %s", response.Code, response.Body.String())
	}
	if response := env.request(http.MethodPatch, "/api/teams/"+teamID+"/inputs/"+inputID,
		`{"id":"77777777-7777-4777-8777-777777777777","text":"重启后的修订","version":1}`); response.Code != http.StatusOK {
		t.Fatalf("重启后应可继续修订: %d %s", response.Code, response.Body.String())
	}
}

// TestTeamInputsQueueAutoRoutesWithFlag 灰度开关开启时，受理后自动泵路由：
// 无歧义输入落成初始 Run（new_task），输入投影变 routed 并携带 runIds 与
// 工作项；停靠场景（多活跃 Run）原因可见。
func TestTeamInputsQueueAutoRoutesWithFlag(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	env.server.TeamQueueAutoRoute = true
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const inputID = "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postQueuedTeamInput(teamID, `{"id":"`+inputID+`","text":"自动路由的需求","queueVersion":"2"}`); response.Code != http.StatusAccepted {
		t.Fatalf("受理失败: %d %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	routedBody := ""
	for time.Now().Before(deadline) {
		routedBody = env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "").Body.String()
		if strings.Contains(routedBody, `"queueStatus": "routed"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(routedBody, `"queueStatus": "routed"`) {
		t.Fatalf("灰度开启时输入应自动路由: %s", routedBody)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+inputID, "")
	var routed struct {
		RouteStatus string   `json:"routeStatus"`
		RunIDs      []string `json:"runIds"`
		WorkItems   []struct {
			ID      string   `json:"id"`
			RunID   string   `json:"runId"`
			TaskIDs []string `json:"taskIds"`
		} `json:"workItems"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &routed); err != nil {
		t.Fatal(err)
	}
	if routed.RouteStatus != "new_task" || len(routed.RunIDs) != 1 || routed.RunIDs[0] != inputID {
		t.Fatalf("路由投影不符: %+v", routed)
	}
	if len(routed.WorkItems) != 1 || routed.WorkItems[0].RunID != inputID || len(routed.WorkItems[0].TaskIDs) != 1 {
		t.Fatalf("工作项应链接 lead 任务: %+v", routed.WorkItems)
	}
	if run := env.store.Run(inputID); run == nil || !run.AutoPlan {
		t.Fatalf("应创建初始 autoPlan Run: %+v", run)
	}
}
