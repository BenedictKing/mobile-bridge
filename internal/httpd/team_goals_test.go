package httpd

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

func TestRunGoalReviewScopesAndIssueResolution(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	const runID = "goal-scope-run"
	input, _, err := env.server.TeamInputs.AcceptWithIssueRouting(teamID, uuid.NewString(), "调查甲；调查乙", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = env.server.TeamInputs.ApplyIssueProposal(teamID, "goals-proposal", &team.IssueProposal{InputID: input.ID, Revision: 1, Segments: []team.IssueProposalSegment{
		{Action: team.IssueSegmentCreate, NewIssueTitle: "甲", SourceStart: 0, SourceEnd: 4},
		{Action: team.IssueSegmentCreate, NewIssueTitle: "乙", SourceStart: 4, SourceEnd: 7},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.server.TeamInputs.MarkRoutingIntent(teamID, input.ID, "new_task", runID, "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err = env.server.TeamInputs.MarkRouted(teamID, input.ID, "new_task", runID, "lead"); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{ID: runID, TeamID: teamID, Goal: input.Text, Status: team.RunCompleted, CreatedAt: time.Now(), Tasks: []team.Task{
		{ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: "调查结果甲乙均已给出。", Reduction: true},
	}}
	if err = env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	issues := env.server.TeamInputs.ListIssues(teamID)
	oldInput, _, err := env.server.TeamInputs.ImportHistoricalInput(teamID, uuid.NewString(), "历史甲", "old-goal-run", "lead", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = env.server.TeamInputs.ApplyIssueProposal(teamID, "old-goal-link", &team.IssueProposal{InputID: oldInput.ID, Revision: 1, Segments: []team.IssueProposalSegment{{Action: team.IssueSegmentLink, IssueID: issues[0].ID, SourceStart: 0, SourceEnd: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = env.store.SaveRun(&team.Run{ID: "old-goal-run", TeamID: teamID, Status: team.RunFailed, CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	base := "/api/teams/" + teamID + "/runs/" + runID
	get := func() struct {
		Version      uint64             `json:"version"`
		GoalOutcomes []team.GoalOutcome `json:"goalOutcomes"`
		Outcome      team.GoalOutcome   `json:"outcome"`
	} {
		response := env.request(http.MethodGet, base, "")
		var result struct {
			Version      uint64             `json:"version"`
			GoalOutcomes []team.GoalOutcome `json:"goalOutcomes"`
			Outcome      team.GoalOutcome   `json:"outcome"`
		}
		if response.Code != http.StatusOK {
			t.Fatalf("get: %d %s", response.Code, response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	detail := get()
	if detail.Outcome.Status != "pending_confirmation" || len(detail.GoalOutcomes) != 2 {
		t.Fatalf("Task 全结束不能解决事项: %+v", detail)
	}
	goal := detail.GoalOutcomes[0]
	post := func(body map[string]any, intended int) {
		raw, _ := json.Marshal(body)
		response := env.request(http.MethodPost, base+"/goal-review", string(raw))
		if response.Code != intended {
			t.Fatalf("post want %d: %d %s", intended, response.Code, response.Body.String())
		}
	}
	request := map[string]any{"id": uuid.NewString(), "version": detail.Version, "goalRevision": goal.Contract.Revision, "scopeId": goal.Scope.ID, "scopeHash": goal.Scope.Hash, "evidenceHash": goal.EvidenceHash, "action": "define",
		"contract": map[string]any{"deliverableKind": "investigation", "criteria": goal.Contract.Criteria, "verify": map[string]any{"requirement": "not_required", "reason": "本次仅调查"}, "review": map[string]any{"requirement": "not_required", "reason": "用户核对依据"}}}
	post(request, http.StatusOK)
	detail = get()
	goal = detail.GoalOutcomes[0]
	confirm := map[string]any{"id": uuid.NewString(), "version": detail.Version, "goalRevision": goal.Contract.Revision, "scopeId": goal.Scope.ID, "scopeHash": goal.Scope.Hash, "evidenceHash": goal.EvidenceHash, "action": "assess", "states": map[string]any{"original": "met"}, "reason": "确认甲的调查已回答原问题"}
	post(confirm, http.StatusOK)
	post(confirm, http.StatusOK)
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/issues", "")
	var projection struct {
		Issues []struct {
			ID      string           `json:"id"`
			Status  string           `json:"status"`
			Outcome team.GoalOutcome `json:"outcome"`
		} `json:"issues"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &projection); err != nil {
		t.Fatal(err)
	}
	for _, issue := range projection.Issues {
		if issue.ID == goal.Scope.ID && (issue.Status != "resolved" || issue.Outcome.Status != "satisfied") {
			t.Fatalf("已确认事项应解决: %+v", issue)
		}
		if issue.ID != goal.Scope.ID && (issue.Status != "waiting" || issue.Outcome.Status == "satisfied") {
			t.Fatalf("另一个事项不能被一起关闭: %+v", issue)
		}
	}
	if get().Outcome.Status == "satisfied" {
		t.Fatal("一个事项确认不代表全部达成")
	}
	confirm["id"] = uuid.NewString()
	post(confirm, http.StatusConflict)
	if env.queue.enqueueCount() != 0 {
		t.Fatal("确认不得派发任务")
	}
}
