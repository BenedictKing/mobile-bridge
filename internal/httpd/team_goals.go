package httpd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func goalObject(value any) any {
	raw, _ := json.Marshal(value)
	parsed, _ := jsoncompat.Unmarshal(raw)
	return parsed
}

func (h *handler) issueGoalScope(issue *team.Issue) team.GoalScope {
	var texts, identities []string
	for _, link := range issue.Links {
		input := h.server.TeamInputs.Get(issue.TeamID, link.InputID)
		if input == nil || input.Revision != link.Revision {
			continue
		}
		texts = append(texts, link.SourceText)
		raw, _ := json.Marshal(link)
		identities = append(identities, string(raw))
	}
	scope := team.NewGoalScope(issue.ID, strings.Join(texts, "\n\n"))
	scope.Hash = team.NewGoalScope(issue.ID, strings.Join(identities, "\n")).Hash
	return scope
}

func (h *handler) runGoalScopes(run *team.Run) []team.GoalScope {
	var scopes []team.GoalScope
	var unscoped []string
	represented := false
	if h.server.TeamInputs != nil {
		for _, input := range h.server.TeamInputs.ListAll(run.TeamID) {
			linked := input.TargetRunID == run.ID
			for _, id := range input.RunIDs {
				linked = linked || id == run.ID
			}
			if !linked {
				continue
			}
			represented = represented || input.Text == run.Goal
			if input.AssociationStatus != team.IssueAssociationLinked && input.AssociationStatus != team.IssueAssociationIgnored {
				unscoped = append(unscoped, input.Text)
			}
		}
		for _, issue := range h.server.TeamInputs.ListIssues(run.TeamID) {
			linked := false
			for _, link := range issue.Links {
				input := h.server.TeamInputs.Get(run.TeamID, link.InputID)
				if input == nil || input.Revision != link.Revision {
					continue
				}
				for _, id := range input.RunIDs {
					if id == run.ID {
						linked = true
						break
					}
				}
				if input.TargetRunID == run.ID {
					linked = true
				}
			}
			if linked {
				scopes = append(scopes, h.issueGoalScope(issue))
			}
		}
	}
	if len(scopes) > 0 && !represented {
		unscoped = append([]string{run.Goal}, unscoped...)
	}
	if len(scopes) > 0 && len(unscoped) > 0 {
		scopes = append(scopes, team.NewGoalScope("run", strings.Join(unscoped, "\n\n")))
	}
	if len(scopes) == 0 {
		scopes = append(scopes, team.RunGoalScope(run))
	}
	return scopes
}

func (h *handler) runGoalOutcomes(run *team.Run) []team.GoalOutcome {
	var outcomes []team.GoalOutcome
	for _, scope := range h.runGoalScopes(run) {
		outcomes = append(outcomes, h.goalOutcome(run, scope))
	}
	return outcomes
}

func (h *handler) goalOutcome(run *team.Run, scope team.GoalScope) team.GoalOutcome {
	if h.server.TeamInputs != nil && scope.ID != "run" {
		if issue := h.server.TeamInputs.GetIssue(run.TeamID, scope.ID); issue != nil {
			record := h.server.TeamInputs.IssueGoal(run.TeamID, scope.ID)
			if h.server.Teams != nil {
				return h.server.Teams.IssueGoalOutcome(run, scope, record)
			}
			return team.EvaluateIssueGoal(issue, run, h.server.TeamInputs.ListAll(run.TeamID), h.server.TeamStore.Team(run.TeamID), scope, record)
		}
	}
	if h.server.Teams != nil {
		return h.server.Teams.GoalOutcome(run, scope)
	}
	return team.EvaluateGoal(run, h.server.TeamStore.Team(run.TeamID), scope)
}

func aggregateGoals(outcomes []team.GoalOutcome) team.GoalOutcome {
	if len(outcomes) == 0 {
		return team.GoalOutcome{SchemaVersion: 1, Status: "unassessed", Label: "尚未评估", Reason: "尚无可评估的目标。"}
	}
	if len(outcomes) == 1 {
		return outcomes[0]
	}
	out := team.GoalOutcome{SchemaVersion: 1, Status: "satisfied", Reason: "全部原始目标已确认满足。"}
	out.Closed = true
	met := 0
	priority := map[string]int{"satisfied": 0, "partial": 1, "unsatisfied": 2, "pending_confirmation": 3, "unassessed": 4, "pending_verification": 5, "cancelled": 6}
	for _, goal := range outcomes {
		out.Closed = out.Closed && goal.Closed
		if goal.Status == "satisfied" {
			met++
		}
		if priority[goal.Status] > priority[out.Status] {
			out.Status, out.Reason = goal.Status, goal.Reason
		}
	}
	if met > 0 && met < len(outcomes) && out.Status == "unsatisfied" {
		out.Status = "partial"
	}
	out.Label = team.GoalOutcomeLabels[out.Status]
	if out.Closed {
		out.Label = "已关闭"
		out.Reason = "已结束目标追踪；不代表目标已达成。"
	}
	return out
}

func goalNextAction(outcome team.GoalOutcome) *jsoncompat.Object {
	action := jsoncompat.NewObject()
	kind, label := "review_goal", "查看结果并确认是否满足要求"
	if outcome.Status == "satisfied" {
		kind, label = "view_result", "查看已确认的结果"
	}
	if outcome.Status == "pending_verification" {
		kind, label = "verify_goal", "查看并补齐必要的验证证据"
	}
	if outcome.Status == "partial" || outcome.Status == "unsatisfied" {
		kind, label = "revise", "查看未满足项并创建修订 Run"
	}
	if outcome.Closed {
		kind, label = "view_result", "目标追踪已关闭，可查看已有结果"
	}
	action.Set("kind", kind)
	action.Set("label", label)
	action.Set("reason", outcome.Reason)
	return action
}

func (h *handler) decorateRunGoals(run *team.Run, object *jsoncompat.Object, detail bool) {
	outcomes := h.runGoalOutcomes(run)
	outcome := aggregateGoals(outcomes)
	issueIDs := &jsoncompat.Array{}
	seen := map[string]bool{}
	for _, goal := range outcomes {
		if goal.Scope.ID != "run" && !seen[goal.Scope.ID] {
			seen[goal.Scope.ID] = true
			issueIDs.Items = append(issueIDs.Items, goal.Scope.ID)
		}
	}
	object.Set("issueIds", issueIDs)
	object.Set("outcome", goalObject(map[string]any{"schemaVersion": 1, "status": outcome.Status, "label": outcome.Label, "reason": outcome.Reason, "closed": outcome.Closed}))
	if detail {
		object.Set("goalOutcomes", goalObject(outcomes))
	}
	if run.Status == team.RunCompleted {
		object.Set("nextAction", goalNextAction(outcome))
	}
}

func (h *handler) handleRunGoalReview(teamID, runID string) {
	run := h.loadRun(teamID, runID)
	if run == nil {
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	var request team.GoalReviewRequest
	if err := json.Unmarshal(jsoncompat.EncodeCompact(body), &request); err != nil {
		h.fail(http.StatusBadRequest, errorBody("目标评估格式无效"))
		return
	}
	scopeID := stringField(body, "scopeId")
	var scope *team.GoalScope
	for _, candidate := range h.runGoalScopes(run) {
		if candidate.ID == scopeID {
			copy := candidate
			scope = &copy
			break
		}
	}
	if scope == nil {
		h.fail(http.StatusConflict, errorBody("目标范围已变化，请刷新后重试"))
		return
	}
	fresh, err := h.server.Teams.ReviewGoal(runID, *scope, request)
	if err != nil {
		if errors.Is(err, team.ErrGoalReviewInvalid) {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
		} else {
			h.failTeamError(err)
		}
		return
	}
	h.output(http.StatusOK, h.teamRunDetail(fresh), contentTypeJSON, "")
}
