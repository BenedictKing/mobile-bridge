package httpd

// teams_inputs_test.go 覆盖 TeamInput 长期入口（docs/team-overall-design.md
// §10 阶段二）：POST /api/teams/:teamId/inputs 的 202 契约、inputId 幂等重放、
// 与主控会话入口共用同一 Run 账本，以及 GET /inputs/:inputId 的路由投影。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

// postTeamInput 是 POST /inputs 的快捷封装（词汇与主控聊天发送一致：
// text/goal 别名 + attachments，id 必填 UUID）。
func (e *teamTestEnv) postTeamInput(teamID, body string) *httptest.ResponseRecorder {
	target := "/api/teams/" + teamID + "/inputs"
	return e.request(http.MethodPost, target, body)
}

func TestTeamInputsCreateAcceptedAndIdempotent(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const inputID = "7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := fmt.Sprintf(`{"id":%q,"text":"给登录页补一套错误态设计"}`, inputID)
	response := env.postTeamInput(teamID, body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("TeamInput 应 202: %d %s", response.Code, response.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["inputId"] != inputID || accepted["runId"] != inputID ||
		accepted["teamId"] != teamID || accepted["routeStatus"] != TeamInputRouteNewTask || accepted["status"] != "accepted" {
		t.Fatalf("202 契约不符: %v", accepted)
	}
	if _, has := accepted["duplicate"]; has {
		t.Fatalf("首次创建不应带 duplicate: %v", accepted)
	}
	run := env.store.Run(inputID)
	if run == nil || !run.AutoPlan || run.Mode != team.ModeSingle || len(run.Tasks) != 1 {
		t.Fatalf("应创建 single+autoPlan Run: %+v", run)
	}
	if run.Tasks[0].MemberID != "lead" {
		t.Fatalf("首个任务应派给 Lead: %+v", run.Tasks[0])
	}

	// 同 id 同内容重放：返回原结果（duplicate），不重复创建。
	response = env.postTeamInput(teamID, body)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("同 id 重放应 202+duplicate: %d %s", response.Code, response.Body.String())
	}
	if got := len(env.store.RunsByTeam(teamID)); got != 1 {
		t.Fatalf("重放不得重复创建 Run: %d", got)
	}
	// 同 id 异内容：拒绝。
	response = env.postTeamInput(teamID, `{"id":"`+inputID+`","text":"换一个目标"}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "同一消息标识") {
		t.Fatalf("异内容重放应 400: %d %s", response.Code, response.Body.String())
	}
}

func TestTeamInputsValidationAndAttachments(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	env.server.Uploads = uploads.New(env.dataDir)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const uploadID = "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := env.server.Uploads.Put(env.sessionID, uploadID, "error-state.png", []byte("png")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body string
		want int
		text string
	}{
		{"空目标无附件", `{"id":"00000000-0000-4000-8000-000000000001"}`, http.StatusBadRequest, "1–10000"},
		{"非法 id", `{"id":"not-uuid","text":"目标"}`, http.StatusBadRequest, "消息标识无效"},
		{"缺 id", `{"text":"目标"}`, http.StatusBadRequest, "消息标识无效"},
		{"未知附件", `{"id":"00000000-0000-4000-8000-000000000002","text":"看图","attachments":[{"id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}]}`, http.StatusBadRequest, ""},
		{"queue 模式拒绝", `{"id":"00000000-0000-4000-8000-000000000003","text":"目标","mode":"queue"}`, http.StatusBadRequest, "普通发送方式"},
		{"goal 别名", `{"id":"00000000-0000-4000-8000-000000000004","goal":"用 goal 字段"}`, http.StatusAccepted, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := env.postTeamInput(teamID, testCase.body)
			if response.Code != testCase.want {
				t.Fatalf("期望 %d 实际 %d: %s", testCase.want, response.Code, response.Body.String())
			}
			if testCase.text != "" && !strings.Contains(response.Body.String(), testCase.text) {
				t.Fatalf("错误文案应含 %q: %s", testCase.text, response.Body.String())
			}
		})
	}
	if run := env.store.Run("00000000-0000-4000-8000-000000000004"); run == nil || run.Goal != "用 goal 字段" {
		t.Fatalf("goal 别名应生效: %+v", run)
	}

	// 纯附件：默认目标文案 + 附件随首个 Lead 任务（与主控聊天同语义）。
	const imageOnlyID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	response := env.postTeamInput(teamID, fmt.Sprintf(`{"id":%q,"attachments":[{"id":%q}]}`, imageOnlyID, uploadID))
	if response.Code != http.StatusAccepted {
		t.Fatalf("纯附件输入应 202: %d %s", response.Code, response.Body.String())
	}
	run := env.store.Run(imageOnlyID)
	if run == nil || strings.TrimSpace(run.Goal) == "" || len(run.Tasks) != 1 || len(run.Tasks[0].Attachments) != 1 {
		t.Fatalf("纯附件输入应落默认目标与附件: %+v", run)
	}
}

func TestTeamInputGetProjection(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const inputID = "baaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postTeamInput(teamID, `{"id":"`+inputID+`","text":"梳理部署脚本"}`); response.Code != http.StatusAccepted {
		t.Fatalf("创建失败: %d %s", response.Code, response.Body.String())
	}
	target := "/api/teams/" + teamID + "/inputs/" + inputID
	response := env.request(http.MethodGet, target, "")
	if response.Code != http.StatusOK {
		t.Fatalf("回查失败: %d %s", response.Code, response.Body.String())
	}
	var projection map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if projection["inputId"] != inputID || projection["runId"] != inputID ||
		projection["routeStatus"] != TeamInputRouteNewTask || projection["goal"] != "梳理部署脚本" {
		t.Fatalf("回查投影不符: %v", projection)
	}
	if status, _ := projection["runStatus"].(string); status == "" {
		t.Fatalf("投影应带 runStatus: %v", projection)
	}
	if _, has := projection["taskId"]; !has {
		t.Fatalf("投影应带首个任务: %v", projection)
	}

	// 未知输入 / 显式计划 Run（非 input 产出）/ 跨团队均 404。
	if response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/caaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", ""); response.Code != http.StatusNotFound {
		t.Fatalf("未知输入应 404: %d", response.Code)
	}
	explicitID := "daaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := env.server.Teams.CreateRun(env.store.Team(teamID), explicitID, "显式计划目标", team.ModePipeline, "", nil,
		[]team.PlanTaskInput{{ID: "step", MemberID: "lead", Prompt: "做一步"}}, false, ""); err != nil {
		t.Fatal(err)
	}
	if response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+explicitID, ""); response.Code != http.StatusNotFound {
		t.Fatalf("显式计划 Run 不是输入: %d", response.Code)
	}
}

func TestTeamInputsShareLedgerWithLeadConversation(t *testing.T) {
	// 两个入口同一账本（§10「Lead Chat 继续兼容」）：主控会话发送创建的
	// Run 能被 GET /inputs 回查，反之亦然——inputId 与 runID 是同一身份。
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const viaSession = "eaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sendBody := fmt.Sprintf(`{"id":%q,"teamId":%q,"text":"从会话入口来","mode":"send","workMode":"default"}`, viaSession, teamID)
	if response := env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/send?host=local", sendBody); response.Code != http.StatusOK {
		t.Fatalf("主控会话发送失败: %d %s", response.Code, response.Body.String())
	}
	// 非法发送方式在会话入口仍被拒（共享核心不吞发送侧词汇校验）。
	if response := env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/send?host=local",
		fmt.Sprintf(`{"id":%q,"teamId":%q,"text":"坏模式","mode":"bogus"}`, "00000000-0000-4000-8000-000000000009", teamID)); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "未知发送方式") {
		t.Fatalf("非法发送方式应 400: %d %s", response.Code, response.Body.String())
	}
	if response := env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+viaSession, ""); response.Code != http.StatusOK {
		t.Fatalf("会话入口产物应可按输入回查: %d %s", response.Code, response.Body.String())
	}
	const viaInputs = "faaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postTeamInput(teamID, `{"id":"`+viaInputs+`","text":"从输入入口来"}`); response.Code != http.StatusAccepted {
		t.Fatalf("输入入口失败: %d %s", response.Code, response.Body.String())
	}
	// 输入入口的 Run 被会话入口按同 id 重放 → duplicate（同一账本身份）。
	if response := env.request(http.MethodPost, "/api/sessions/"+env.sessionID+"/send?host=local",
		fmt.Sprintf(`{"id":%q,"teamId":%q,"text":"从输入入口来","mode":"send","workMode":"default"}`, viaInputs, teamID)); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("跨入口重放应命中同一 Run: %d %s", response.Code, response.Body.String())
	}
}
