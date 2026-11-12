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
