package httpd

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

type runListView struct {
	ID                 string   `json:"id"`
	AutoPlan           bool     `json:"autoPlan"`
	ScheduledFrom      string   `json:"scheduledFrom"`
	ClassificationOnly bool     `json:"classificationOnly"`
	IssueIDs           []string `json:"issueIds"`
}

func TestRunExecutionViewIncludesClassificationAndMetadata(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID,
		"dddddddd-bbbb-4ccc-8ddd-eeeeeeeeee08", "关联事项", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = env.server.TeamInputs.ApplyIssueProposal(teamID, "execution-view-proposal", &team.IssueProposal{InputID: input.ID, Revision: 1,
		Segments: []team.IssueProposalSegment{{Action: team.IssueSegmentCreate, NewIssueTitle: "关联事项", SourceStart: 0, SourceEnd: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.server.TeamInputs.MarkRoutingIntent(teamID, input.ID, "new_task", "linked-run", "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err = env.server.TeamInputs.MarkRouted(teamID, input.ID, "new_task", "linked-run", "lead"); err != nil {
		t.Fatal(err)
	}
	issueID := env.server.TeamInputs.ListIssues(teamID)[0].ID
	runs := []*team.Run{
		{ID: "linked-run", TeamID: teamID, Goal: input.Text, AutoPlan: true, Mode: team.ModeSingle, Status: team.RunCompleted,
			Tasks: []team.Task{{ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: "完成"}}},
		{ID: "explicit-run", TeamID: teamID, Goal: "显式计划", Status: team.RunCompleted},
		{ID: "scheduled-run", TeamID: teamID, Goal: "定时", ScheduledFrom: "trigger-1", Status: team.RunCompleted},
		{ID: "classification-run", TeamID: teamID, Goal: "归类", AutoPlan: true, Mode: team.ModeSingle, Status: team.RunCompleted,
			Tasks: []team.Task{{ID: "lead", MemberID: "lead", IssueClassificationOnly: true, Status: team.TaskCompleted}}},
	}
	for _, run := range runs {
		if err := env.store.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/teams/" + teamID + "/runs"
	response := env.request(http.MethodGet, base, "")
	var defaultPayload struct {
		Runs []runListView `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &defaultPayload); err != nil {
		t.Fatal(err)
	}
	defaultRows := defaultPayload.Runs
	if response.Code != http.StatusOK || len(defaultRows) != 3 {
		t.Fatalf("默认 Run 列表应继续排除归类 Run: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, base+"?includeClassification=1", "")
	var payload struct {
		Runs []runListView `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	rows := payload.Runs
	if len(rows) != 4 {
		t.Fatalf("执行记录应包含全部 Run: %s", response.Body.String())
	}
	byID := map[string]runListView{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if !byID["classification-run"].ClassificationOnly || byID["scheduled-run"].ScheduledFrom != "trigger-1" ||
		byID["explicit-run"].AutoPlan || len(byID["linked-run"].IssueIDs) != 1 || byID["linked-run"].IssueIDs[0] != issueID {
		t.Fatalf("执行记录元数据不符: %+v", rows)
	}
	for _, id := range []string{"classification-run", "scheduled-run", "explicit-run"} {
		if len(byID[id].IssueIDs) != 0 {
			t.Fatalf("无 Issue 的执行记录应明确为空关联: %s", id)
		}
	}
	response = env.request(http.MethodGet, base+"?filter=explicit-plan", "")
	var explicitPayload struct {
		Runs []runListView `json:"runs"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &explicitPayload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(explicitPayload.Runs) != 1 || explicitPayload.Runs[0].ID != "explicit-run" {
		t.Fatalf("显式计划筛选不应混入自动或定时 Run: %d %s", response.Code, response.Body.String())
	}
	if unknown := env.request(http.MethodGet, base+"?filter=unknown", ""); unknown.Code != http.StatusBadRequest {
		t.Fatalf("未知 Run 筛选应返回 400: %d %s", unknown.Code, unknown.Body.String())
	}
}

func TestScheduledIssueProjectionAndExplicitPlanFilter(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	if err := env.server.TeamSchedules.SaveSchedule(&team.TeamSchedule{
		TeamID: teamID, Enabled: true, IntervalSecs: 600, Goal: "每日巡检",
	}); err != nil {
		t.Fatal(err)
	}
	baseTime := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	runs := []*team.Run{
		{ID: "scheduled-old", TeamID: teamID, Goal: "每日巡检", ScheduledFrom: "trigger-old", Status: team.RunCompleted,
			CreatedAt: baseTime, UpdatedAt: baseTime, Tasks: []team.Task{{ID: "check", MemberID: "lead", Status: team.TaskCompleted}}},
		{ID: "scheduled-new", TeamID: teamID, Goal: "每日巡检", ScheduledFrom: "trigger-new", Status: team.RunDispatching,
			CreatedAt: baseTime.Add(time.Hour), UpdatedAt: baseTime.Add(time.Hour), Tasks: []team.Task{{ID: "check", MemberID: "lead", Status: team.TaskRunning}}},
		{ID: "explicit-only", TeamID: teamID, Goal: "手动计划", Status: team.RunCompleted,
			CreatedAt: baseTime.Add(2 * time.Hour), UpdatedAt: baseTime.Add(2 * time.Hour)},
	}
	for _, run := range runs {
		if err := env.store.SaveRun(run); err != nil {
			t.Fatal(err)
		}
	}
	base := "/api/teams/" + teamID
	response := env.request(http.MethodGet, base+"/issues", "")
	var payload struct {
		Issues []struct {
			ID              string          `json:"id"`
			Title           string          `json:"title"`
			Status          string          `json:"status"`
			ExecutionStatus string          `json:"executionStatus"`
			Scheduled       bool            `json:"scheduled"`
			Historical      bool            `json:"historical"`
			RunIDs          []string        `json:"runIds"`
			Runs            []runListView   `json:"runs"`
			Outcome         json.RawMessage `json:"outcome"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(payload.Issues) != 1 {
		t.Fatalf("定时配置应生成一个伪 Issue: %d %s", response.Code, response.Body.String())
	}
	issue := payload.Issues[0]
	issueID := scheduledTeamIssueID(teamID)
	if issue.ID != issueID || issue.Title != "每日巡检" || !issue.Scheduled || issue.Historical ||
		issue.Status != "in_progress" || issue.ExecutionStatus != team.RunDispatching || len(issue.RunIDs) != 2 ||
		len(issue.Runs) != 2 || issue.Runs[0].ID != "scheduled-new" || len(issue.Outcome) != 0 {
		t.Fatalf("定时伪 Issue 投影不符: %+v", issue)
	}
	if len(env.server.TeamInputs.ListIssues(teamID)) != 0 {
		t.Fatal("定时伪 Issue 不应写入输入账本")
	}
	for _, run := range issue.Runs {
		if len(run.IssueIDs) != 1 || run.IssueIDs[0] != issueID {
			t.Fatalf("定时 Run 应反向关联伪 Issue: %+v", run)
		}
	}
	if detail := env.request(http.MethodGet, base+"/issues/"+issueID, ""); detail.Code != http.StatusOK {
		t.Fatalf("定时伪 Issue 详情应可读: %d %s", detail.Code, detail.Body.String())
	}
	if deleted := env.request(http.MethodDelete, base+"/issues/"+issueID, "{}"); deleted.Code != http.StatusConflict {
		t.Fatalf("定时伪 Issue 不应走输入账本删除: %d %s", deleted.Code, deleted.Body.String())
	}
}

func TestHistoricalIssueProjection(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	input, _, err := env.server.TeamInputs.ImportHistoricalInput(teamID, "history-view", "历史事项", "history-run", "lead", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = env.server.TeamInputs.ApplyIssueProposal(teamID, "history-view-proposal", &team.IssueProposal{InputID: input.ID, Revision: 1,
		Segments: []team.IssueProposalSegment{{Action: team.IssueSegmentCreate, NewIssueTitle: "历史事项", SourceStart: 0, SourceEnd: 4}}})
	if err != nil {
		t.Fatal(err)
	}
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/issues", "")
	var result struct {
		Issues []struct {
			Title      string `json:"title"`
			Historical bool   `json:"historical"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Issues) != 1 || !result.Issues[0].Historical {
		t.Fatalf("历史事项应带分组标识: %s", response.Body.String())
	}
}
