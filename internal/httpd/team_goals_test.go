package httpd

import (
	"encoding/json"
	"net/http"
	"strings"
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
	criteria := append([]team.GoalCriterion(nil), goal.Contract.Criteria...)
	criteria = append(criteria, team.GoalCriterion{ID: "extra", Text: "保留追加条件", State: "unknown"})
	request := map[string]any{"id": uuid.NewString(), "version": detail.Version, "goalRevision": goal.Contract.Revision, "scopeId": goal.Scope.ID, "scopeHash": goal.Scope.Hash, "evidenceHash": goal.EvidenceHash, "action": "define",
		"contract": map[string]any{"deliverableKind": "investigation", "criteria": criteria, "verify": map[string]any{"requirement": "not_required", "reason": "本次仅调查"}, "review": map[string]any{"requirement": "not_required", "reason": "用户核对依据"}}}
	post(request, http.StatusOK)
	detail = get()
	goal = detail.GoalOutcomes[0]
	confirm := map[string]any{"id": uuid.NewString(), "version": detail.Version, "goalRevision": goal.Contract.Revision, "scopeId": goal.Scope.ID, "scopeHash": goal.Scope.Hash, "evidenceHash": goal.EvidenceHash, "action": "assess", "states": map[string]any{"original": "met", "extra": "met"}, "reason": "确认甲的调查已回答原问题"}
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

	routeIssueRun := func(runID, text, status string, createdAt time.Time, result string) {
		t.Helper()
		followUp, _, acceptErr := env.server.TeamInputs.AcceptWithIssueRouting(teamID, uuid.NewString(), text, nil, "policy", 64, true)
		if acceptErr != nil {
			t.Fatal(acceptErr)
		}
		_, _, acceptErr = env.server.TeamInputs.ApplyIssueProposal(teamID, "link-"+runID, &team.IssueProposal{InputID: followUp.ID, Revision: 1,
			Segments: []team.IssueProposalSegment{{Action: team.IssueSegmentLink, IssueID: goal.Scope.ID, SourceStart: 0, SourceEnd: len([]rune(text))}}})
		if acceptErr != nil {
			t.Fatal(acceptErr)
		}
		if acceptErr = env.server.TeamInputs.MarkRoutingIntent(teamID, followUp.ID, "follow_up", runID, "lead", ""); acceptErr != nil {
			t.Fatal(acceptErr)
		}
		if acceptErr = env.server.TeamInputs.MarkRouted(teamID, followUp.ID, "follow_up", runID, "lead"); acceptErr != nil {
			t.Fatal(acceptErr)
		}
		run := &team.Run{ID: runID, TeamID: teamID, Goal: text, Status: status, CreatedAt: createdAt}
		if result != "" {
			run.Tasks = []team.Task{{ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: result}}
		}
		if acceptErr = env.store.SaveRun(run); acceptErr != nil {
			t.Fatal(acceptErr)
		}
	}
	const nextRunID = "goal-scope-next-run"
	routeIssueRun(nextRunID, "甲后续", team.RunCompleted, time.Now().Add(time.Minute), "新的执行结果")
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/issues/"+goal.Scope.ID, "")
	var latest struct {
		Status      string           `json:"status"`
		ReviewRunID string           `json:"reviewRunId"`
		Outcome     team.GoalOutcome `json:"outcome"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &latest); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || latest.ReviewRunID != nextRunID || latest.Status != "resolved" || latest.Outcome.Status != "satisfied" {
		t.Fatalf("新 Run 关联后应保留事项确认: %d %+v", response.Code, latest)
	}
	if len(latest.Outcome.Contract.Criteria) != 2 || latest.Outcome.Contract.Criteria[1].Text != "保留追加条件" ||
		!strings.Contains(latest.Outcome.Contract.Criteria[0].Text, "甲后续") {
		t.Fatalf("跨 Run 应保留追加条件并迁移当前范围: %+v", latest.Outcome.Contract.Criteria)
	}

	const activeRunID = "goal-scope-active-run"
	routeIssueRun(activeRunID, "甲活跃后续", team.RunWaitingInput, time.Now().Add(2*time.Minute), "")
	activeBase := "/api/teams/" + teamID + "/runs/" + activeRunID
	response = env.request(http.MethodGet, activeBase, "")
	var active struct {
		Version      uint64             `json:"version"`
		GoalOutcomes []team.GoalOutcome `json:"goalOutcomes"`
	}
	if err = json.Unmarshal(response.Body.Bytes(), &active); err != nil {
		t.Fatal(err)
	}
	var activeGoal *team.GoalOutcome
	for i := range active.GoalOutcomes {
		if active.GoalOutcomes[i].Scope.ID == goal.Scope.ID {
			activeGoal = &active.GoalOutcomes[i]
		}
	}
	if activeGoal == nil || activeGoal.Status != "satisfied" {
		t.Fatalf("活跃 Run 不应清除事项确认: %+v", active.GoalOutcomes)
	}
	closeBody, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "version": active.Version,
		"goalRevision": activeGoal.Contract.Revision, "scopeId": activeGoal.Scope.ID, "scopeHash": activeGoal.Scope.Hash,
		"evidenceHash": activeGoal.EvidenceHash, "action": "close", "reason": "关闭事项追踪，执行继续"})
	response = env.request(http.MethodPost, activeBase+"/goal-review", string(closeBody))
	if response.Code != http.StatusOK || env.store.Run(activeRunID).Status != team.RunWaitingInput {
		t.Fatalf("活跃事项应可独立关闭且不取消 Run: %d %s", response.Code, response.Body.String())
	}
	response = env.request(http.MethodGet, "/api/teams/"+teamID+"/issues/"+goal.Scope.ID, "")
	if err = json.Unmarshal(response.Body.Bytes(), &latest); err != nil {
		t.Fatal(err)
	}
	if latest.Status != "closed" || !latest.Outcome.Closed {
		t.Fatalf("关闭应独立于活跃 Run 状态: %+v", latest)
	}
	confirm["id"] = uuid.NewString()
	post(confirm, http.StatusConflict)
	if env.queue.enqueueCount() != 0 {
		t.Fatal("确认不得派发任务")
	}
}

func TestRunGoalFeedbackNeedsNoSeparateContractDefinition(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	run := &team.Run{ID: "direct-feedback", TeamID: teamID, Goal: "解释 Git 回撤机制", Status: team.RunCompleted,
		Tasks: []team.Task{{ID: "answer", Status: team.TaskCompleted, Result: "已说明已提交和未提交文件的回撤方式。"}}}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	base := "/api/teams/" + teamID + "/runs/" + run.ID
	response := env.request(http.MethodGet, base, "")
	var detail struct {
		Version      uint64             `json:"version"`
		GoalOutcomes []team.GoalOutcome `json:"goalOutcomes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || len(detail.GoalOutcomes) != 1 {
		t.Fatalf("detail: %s %v", response.Body.String(), err)
	}
	goal := detail.GoalOutcomes[0]
	if !goal.CanConfirm || goal.Contract.DeliverableKind != "result" || goal.ExecutionStatus != team.RunCompleted {
		t.Fatalf("默认应允许查看结果后直接确认: %+v", goal)
	}
	payload, _ := json.Marshal(map[string]any{"id": uuid.NewString(), "action": "assess", "version": detail.Version,
		"goalRevision": goal.Contract.Revision, "scopeId": goal.Scope.ID, "scopeHash": goal.Scope.Hash, "evidenceHash": goal.EvidenceHash,
		"states": map[string]string{"original": "met"}})
	response = env.request(http.MethodPost, base+"/goal-review", string(payload))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status": "satisfied"`) {
		t.Fatalf("无需先填表或输入说明: %d %s", response.Code, response.Body.String())
	}
	fresh := env.store.Run(run.ID)
	fresh.Status = team.RunCancelled
	if err := env.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	response = env.request(http.MethodGet, base, "")
	if !strings.Contains(response.Body.String(), `"executionStatus": "cancelled"`) || !strings.Contains(response.Body.String(), `"status": "cancelled"`) {
		t.Fatalf("取消执行不应仍提示尚未评估: %s", response.Body.String())
	}
}
