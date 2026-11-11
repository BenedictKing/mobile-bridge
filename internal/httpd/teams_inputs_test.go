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

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

// postTeamInput 是 POST /inputs 的快捷封装（词汇与主控聊天发送一致：
// text/goal 别名 + attachments，id 必填 UUID）。
func (e *teamTestEnv) postTeamInput(teamID, body string) *httptest.ResponseRecorder {
	target := "/api/teams/" + teamID + "/inputs"
	return e.request(http.MethodPost, target, body)
}

func TestTeamInputProjectsPublicImageMetadata(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	env.server.Uploads = uploads.New(env.dataDir)
	const id = "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, err := env.server.Uploads.Put(env.sessionID, id, "photo.png", []byte("\x89PNG\r\n\x1a\nimage")); err != nil {
		t.Fatal(err)
	}
	h := &handler{server: env.server}
	row := h.teamInputProject(&team.TeamInput{ID: "input", TeamID: teamID, AttachmentIDs: []string{id}})
	text := string(jsoncompat.EncodeCompact(row))
	if !strings.Contains(text, `"image":"image/png"`) || !strings.Contains(text, `"sessionId":"`+env.sessionID+`"`) {
		t.Fatalf("附件应包含 MIME 与会话: %s", text)
	}
	if strings.Contains(text, "localPath") || strings.Contains(text, env.dataDir) {
		t.Fatalf("不得泄漏本地附件路径: %s", text)
	}
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

	// 纯附件：默认目标文案 + 附件追加到当前活跃 Run 的新 Lead 任务。
	const imageOnlyID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	response := env.postTeamInput(teamID, fmt.Sprintf(`{"id":%q,"attachments":[{"id":%q}]}`, imageOnlyID, uploadID))
	if response.Code != http.StatusAccepted {
		t.Fatalf("纯附件输入应 202: %d %s", response.Code, response.Body.String())
	}
	run := env.store.Run("00000000-0000-4000-8000-000000000004")
	if run == nil || len(run.Tasks) != 2 || run.Tasks[1].ID != conversationTaskID(imageOnlyID) ||
		run.Tasks[1].Prompt == "" || len(run.Tasks[1].Attachments) != 1 {
		t.Fatalf("纯附件输入应追加默认目标与附件: %+v", run)
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

func TestIssueRoutingProjectionReportsFailedLead(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID,
		"cccccccc-bbbb-4ccc-8ddd-eeeeeeeeee03", "新团队输入", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	teamRow := env.store.Team(teamID)
	run := &team.Run{
		ID: input.ID, TeamID: teamID, Goal: input.Text, Mode: team.ModeSingle,
		AutoPlan: true, Status: "failed", Tasks: []team.Task{{
			ID: "lead", MemberID: teamRow.LeadID, Status: team.TaskFailed, Error: "provider unavailable",
		}},
	}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	project := (&handler{server: env.server}).teamInputProject(input)
	if got := project.MustGet("associationStatus"); got != team.IssueAssociationFailed {
		t.Fatalf("failed Lead should stop the pending Issue badge: %v", got)
	}
	if got := project.MustGet("associationReason"); got != "provider unavailable" {
		t.Fatalf("failure reason should remain visible: %v", got)
	}
}

func TestTeamIssueListProjectsPersistedInputLinks(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID,
		"dddddddd-bbbb-4ccc-8ddd-eeeeeeeeee04", "導航", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	proposal := &team.IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: []team.IssueProposalSegment{{
		Action: team.IssueSegmentCreate, NewIssueTitle: "團隊導航", SourceStart: 0, SourceEnd: 2,
	}}}
	if _, _, err := env.server.TeamInputs.ApplyIssueProposal(teamID, "issue-proposal-http", proposal); err != nil {
		t.Fatal(err)
	}
	const runID = "run-issue-evidence"
	if err := env.server.TeamInputs.MarkRoutingIntent(teamID, input.ID, "new_task", runID, "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.server.TeamInputs.MarkRouted(teamID, input.ID, "new_task", runID, "lead"); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: runID, TeamID: teamID, Goal: "实现团队导航", Status: team.RunCompleted, Tasks: []team.Task{{
		ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: "导航已完成，并通过只读验证。", Ordinal: 1,
	}}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/issues", "")
	if response.Code != http.StatusOK {
		t.Fatalf("Issue list failed: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Issues []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
			Inputs []struct {
				InputID    string `json:"inputId"`
				SourceText string `json:"sourceText"`
			} `json:"inputs"`
			LastOwner map[string]any `json:"lastOwner"`
			Runs      []struct {
				Delivery map[string]any `json:"delivery"`
			} `json:"runs"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || result.Issues[0].Title != "團隊導航" ||
		result.Issues[0].Status != "waiting" || len(result.Issues[0].Inputs) != 1 ||
		result.Issues[0].Inputs[0].InputID != input.ID || result.Issues[0].Inputs[0].SourceText != "導航" {
		t.Fatalf("Issue projection lost its persisted source link: %+v", result.Issues)
	}
	lead := env.store.Team(teamID).Members[0]
	leadName := lead.Name
	if leadName == "" {
		leadName = lead.ID
	}
	if result.Issues[0].LastOwner["name"] != leadName || len(result.Issues[0].Runs) != 1 {
		t.Fatalf("Issue list should keep the final executor while remaining compact: %+v", result.Issues[0])
	}
	if _, hasEvidenceText := result.Issues[0].Runs[0].Delivery["resultSummary"]; hasEvidenceText {
		t.Fatalf("Issue list should not include long delivery text: %+v", result.Issues[0].Runs[0])
	}
	issueID := env.server.TeamInputs.ListIssues(teamID)[0].ID
	detail := env.request(http.MethodGet, "/api/teams/"+teamID+"/issues/"+issueID, "")
	if detail.Code != http.StatusOK {
		t.Fatalf("Issue detail failed: %d %s", detail.Code, detail.Body.String())
	}
	var detailed struct {
		Runs []struct {
			Delivery struct {
				ResultSummary struct {
					Text string `json:"text"`
				} `json:"resultSummary"`
			} `json:"delivery"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &detailed); err != nil {
		t.Fatal(err)
	}
	if len(detailed.Runs) != 1 || !strings.Contains(detailed.Runs[0].Delivery.ResultSummary.Text, "只读验证") {
		t.Fatalf("Issue detail should include the bounded delivery summary: %+v", detailed)
	}
	deleted := env.request(http.MethodDelete, "/api/teams/"+teamID+"/issues/"+issueID, `{}`)
	if deleted.Code != http.StatusOK {
		t.Fatalf("Issue delete failed: %d %s", deleted.Code, deleted.Body.String())
	}
	var deleteResult struct {
		DeletedInputs int `json:"deletedInputs"`
	}
	if err := json.Unmarshal(deleted.Body.Bytes(), &deleteResult); err != nil || deleteResult.DeletedInputs != 1 {
		t.Fatalf("Issue delete result = %s, err=%v", deleted.Body.String(), err)
	}
	if env.server.TeamInputs.GetIssue(teamID, issueID) != nil || env.server.TeamInputs.Get(teamID, input.ID) != nil {
		t.Fatal("deleted Issue or its source input remains in the ledger")
	}
	if env.store.Run(runID) == nil {
		t.Fatal("deleting an Issue must retain its Run execution record")
	}
	missing := env.request(http.MethodGet, "/api/teams/"+teamID+"/issues/"+issueID, "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("deleted Issue detail status = %d", missing.Code)
	}
}

func TestTeamIssueDeleteRejectsActiveRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID,
		"dddddddd-bbbb-4ccc-8ddd-eeeeeeeeee05", "進行中的事項", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	proposal := &team.IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: []team.IssueProposalSegment{{
		Action: team.IssueSegmentCreate, NewIssueTitle: "進行中的事項", SourceStart: 0, SourceEnd: 6,
	}}}
	if _, _, err := env.server.TeamInputs.ApplyIssueProposal(teamID, "issue-proposal-active", proposal); err != nil {
		t.Fatal(err)
	}
	const runID = "run-issue-active"
	if err := env.server.TeamInputs.MarkRoutingIntent(teamID, input.ID, "new_task", runID, "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.server.TeamInputs.MarkRouted(teamID, input.ID, "new_task", runID, "lead"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveRun(&team.Run{ID: runID, TeamID: teamID, Status: team.RunWaitingInput}); err != nil {
		t.Fatal(err)
	}
	issueID := env.server.TeamInputs.ListIssues(teamID)[0].ID
	response := env.request(http.MethodDelete, "/api/teams/"+teamID+"/issues/"+issueID, `{}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("active Issue delete status = %d, body=%s", response.Code, response.Body.String())
	}
	if env.server.TeamInputs.GetIssue(teamID, issueID) == nil || env.server.TeamInputs.Get(teamID, input.ID) == nil {
		t.Fatal("active Issue or its input was deleted")
	}
	// 与界面相同：明确取消等待中的 Run，再删除事项；执行记录保留。
	cancelled := env.request(http.MethodPost, "/api/teams/"+teamID+"/runs/"+runID+"/cancel", `{}`)
	if cancelled.Code != http.StatusOK {
		t.Fatalf("cancel parked Run: %d %s", cancelled.Code, cancelled.Body.String())
	}
	deleted := env.request(http.MethodDelete, "/api/teams/"+teamID+"/issues/"+issueID, `{}`)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete after cancellation: %d %s", deleted.Code, deleted.Body.String())
	}
	if env.server.TeamInputs.GetIssue(teamID, issueID) != nil || env.server.TeamInputs.Get(teamID, input.ID) != nil {
		t.Fatal("cancelled Issue or input still visible")
	}
	if run := env.store.Run(runID); run == nil || run.Status != team.RunCancelled {
		t.Fatalf("cancelled Run history must remain: %+v", run)
	}
}

func TestTeamIssueDeleteRejectsSharedActiveRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID,
		"dddddddd-bbbb-4ccc-8ddd-eeeeeeeeee06", "甲乙", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = env.server.TeamInputs.ApplyIssueProposal(teamID, "issue-proposal-shared", &team.IssueProposal{
		InputID: input.ID, Revision: input.Revision, Segments: []team.IssueProposalSegment{
			{Action: team.IssueSegmentCreate, NewIssueTitle: "甲事项", SourceStart: 0, SourceEnd: 1},
			{Action: team.IssueSegmentCreate, NewIssueTitle: "乙事项", SourceStart: 1, SourceEnd: 2},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run-issue-shared"
	if err := env.server.TeamInputs.MarkRoutingIntent(teamID, input.ID, "new_task", runID, "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err := env.server.TeamInputs.MarkRouted(teamID, input.ID, "new_task", runID, "lead"); err != nil {
		t.Fatal(err)
	}
	if err := env.store.SaveRun(&team.Run{ID: runID, TeamID: teamID, Status: team.RunWaitingInput}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = env.server.Teams.Cancel(runID) })
	issues := env.server.TeamInputs.ListIssues(teamID)
	if len(issues) != 2 {
		t.Fatalf("shared input should create two Issues: %+v", issues)
	}
	response := env.request(http.MethodDelete, "/api/teams/"+teamID+"/issues/"+issues[0].ID, `{}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("shared active Issue delete status = %d, body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Error        string `json:"error"`
		SharedIssues []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"sharedIssues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.SharedIssues) != 1 || body.SharedIssues[0].ID != issues[1].ID || body.SharedIssues[0].Title != issues[1].Title {
		t.Fatalf("shared Issue conflict should identify the other Issue: %s", response.Body.String())
	}
	if len(env.server.TeamInputs.ListIssues(teamID)) != 2 || env.server.TeamInputs.Get(teamID, input.ID) == nil {
		t.Fatal("shared active Issue delete must not change the ledger")
	}
}

func TestTeamInputFollowUpResumesWaitingRun(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const firstID = "1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postTeamInput(teamID, `{"id":"`+firstID+`","text":"先实现"}`); response.Code != http.StatusAccepted {
		t.Fatalf("首条输入失败: %d %s", response.Code, response.Body.String())
	}
	original := env.store.Run(firstID)
	original.Status = team.RunWaitingInput
	original.StopReason = "等待用户处理"
	original.Tasks[0].Status = team.TaskOutcomeUnknown
	if err := env.store.SaveRun(original); err != nil {
		t.Fatal(err)
	}
	// 较新的终态 Run 不得抢占仍活跃的旧 Run（复现现场的消息路由）。
	const newerID = "5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	newer, err := env.server.Teams.CreateConversationRun(env.store.Team(teamID), newerID, "较新的已完成对话", nil)
	if err != nil {
		t.Fatal(err)
	}
	newer.Status = team.RunCompleted
	newer.Tasks[0].Status = team.TaskCompleted
	if err := env.store.SaveRun(newer); err != nil {
		t.Fatal(err)
	}

	const secondID = "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	body := `{"id":"` + secondID + `","text":"继续评审并汇总"}`
	response := env.postTeamInput(teamID, body)
	if response.Code != http.StatusAccepted {
		t.Fatalf("后续输入应复用原 Run: %d %s", response.Code, response.Body.String())
	}
	var accepted map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &accepted); err != nil {
		t.Fatal(err)
	}
	if accepted["inputId"] != secondID || accepted["runId"] != firstID {
		t.Fatalf("后续输入的 inputId/runId 不符: %v", accepted)
	}
	run := env.store.Run(firstID)
	if len(env.store.RunsByTeam(teamID)) != 2 || len(run.Tasks) != 2 {
		t.Fatalf("后续输入不得新建 Run: %+v", run)
	}
	if run.Status != team.RunDispatching || run.StopReason != "" || run.Tasks[0].Status != team.TaskOutcomeUnknown {
		t.Fatalf("后续输入应恢复执行且保留旧任务: %+v", run)
	}
	if task := run.Tasks[1]; task.ID != conversationTaskID(secondID) || task.Prompt != "继续评审并汇总" || task.MemberID != "lead" {
		t.Fatalf("后续 Lead 任务不符: %+v", task)
	}

	response = env.postTeamInput(teamID, body)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate": true`) {
		t.Fatalf("后续输入重放应幂等: %d %s", response.Code, response.Body.String())
	}
	if got := len(env.store.Run(firstID).Tasks); got != 2 {
		t.Fatalf("重放不得重复追加任务: %d", got)
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/inputs/"+secondID, "")
	var projection map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	if projection["inputId"] != secondID || projection["runId"] != firstID ||
		projection["taskId"] != conversationTaskID(secondID) || projection["goal"] != "继续评审并汇总" {
		t.Fatalf("后续输入投影不符: %v", projection)
	}
}

func TestTeamInputAfterTerminalRunCreatesConversationAmend(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	t.Cleanup(func() {
		for _, run := range env.store.RunsByTeam(teamID) {
			_, _ = env.server.Teams.Cancel(run.ID)
		}
	})
	const firstID = "3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if response := env.postTeamInput(teamID, `{"id":"`+firstID+`","text":"继续"}`); response.Code != http.StatusAccepted {
		t.Fatalf("首条输入失败: %d %s", response.Code, response.Body.String())
	}
	original := env.store.Run(firstID)
	original.Status = team.RunCompleted
	original.Tasks[0].Status = team.TaskCompleted
	if err := env.store.SaveRun(original); err != nil {
		t.Fatal(err)
	}

	const secondID = "4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	response := env.postTeamInput(teamID, `{"id":"`+secondID+`","text":"继续"}`)
	if response.Code != http.StatusAccepted {
		t.Fatalf("终态后的输入应创建修订: %d %s", response.Code, response.Body.String())
	}
	amend := env.store.Run(secondID)
	if amend == nil || amend.ParentRunID != firstID || len(amend.Tasks) != 1 || amend.Tasks[0].CachedFrom != "" {
		t.Fatalf("终态后的输入应创建全新 Lead 修订任务: %+v", amend)
	}
	if len(env.store.RunsByTeam(teamID)) != 2 || env.store.Run(firstID).Status != team.RunCompleted {
		t.Fatalf("修订不得重开父 Run: %+v", env.store.RunsByTeam(teamID))
	}
}

func TestTeamInputsShareLedgerWithLeadConversation(t *testing.T) {
	// 两个入口同一账本（§10「Lead Chat 继续兼容」）：主控会话创建的 Run
	// 与后续任务都能被 GET /inputs 回查，反之亦然。
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
