package team

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const EventGoalPlanning = "goal_planning"

type PlanningCriterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type PlanningWorkflow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	WorkspaceID string `json:"workspaceId,omitempty"`
}

type PlanningCheck struct {
	ID              string   `json:"id"`
	State           string   `json:"state"`
	EvidenceTaskIDs []string `json:"evidenceTaskIds,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

type GoalPlanningState struct {
	Version       int                 `json:"version"`
	Criteria      []PlanningCriterion `json:"criteria,omitempty"`
	Workflows     []PlanningWorkflow  `json:"workflows,omitempty"`
	Iteration     int                 `json:"iteration"`
	MaxIterations int                 `json:"maxIterations"`
	Status        string              `json:"status"`
	Checks        []PlanningCheck     `json:"checks,omitempty"`
	Remaining     []string            `json:"remaining,omitempty"`
	Blockers      []string            `json:"blockers,omitempty"`
	NoProgress    int                 `json:"noProgress"`
	LastMet       int                 `json:"lastMet"`
}

type planningDefinition struct {
	Criteria  []PlanningCriterion `json:"criteria"`
	Workflows []PlanningWorkflow  `json:"workflows"`
}

type planningFeedback struct {
	Status    string          `json:"status"`
	Checks    []PlanningCheck `json:"checks"`
	Remaining []string        `json:"remaining"`
	Blockers  []string        `json:"blockers"`
}

func cloneGoalPlanning(state *GoalPlanningState) *GoalPlanningState {
	if state == nil {
		return nil
	}
	raw, _ := json.Marshal(state)
	var copy GoalPlanningState
	_ = json.Unmarshal(raw, &copy)
	return &copy
}

func planningEvent(run *Run, taskID string) *Event {
	raw, _ := json.Marshal(run.Planning)
	value, _ := jsoncompat.Unmarshal(raw)
	event := newEvent(run.ID, EventGoalPlanning, "lead", map[string]any{"planning": value, "runStatus": run.Status, "stopReason": run.StopReason})
	event.IdempotencyKey = "team:" + run.TeamID + ":run:" + run.ID + ":planning:" + taskID
	return event
}

func decodePlanningValue(value any, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(jsoncompat.EncodeCompact(value))))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// 规划元数据与动作一起校验；失败时不提交新的验收口径或执行动作。
func parsePlanningReply(fullText string, team *Team, run *Run, source *Task, policy TeamPolicy) ([]Action, *GoalPlanningState, []string) {
	inner, violations := ExtractActionBlock(fullText)
	if len(violations) > 0 {
		return nil, nil, violations
	}
	if inner == "" {
		if source != nil && source.GoalFeedback {
			return nil, nil, []string{"目标评估必须输出带 feedback 的 team-actions 对象"}
		}
		return nil, cloneGoalPlanning(run.Planning), nil
	}
	payload, violations := decodeActionPayload(inner)
	if len(violations) > 0 {
		return nil, nil, violations
	}
	for _, key := range payload.Keys() {
		if key != "actions" && key != "plan" && key != "feedback" {
			return nil, nil, []string{"规划对象包含未识别字段：" + key}
		}
	}
	state := cloneGoalPlanning(run.Planning)
	if definition, present := payload.Get("plan"); present {
		var plan planningDefinition
		if err := decodePlanningValue(definition, &plan); err != nil {
			return nil, nil, []string{"plan 不是有效规划：" + err.Error()}
		}
		if err := applyPlanningDefinition(state, plan, policy); err != nil {
			return nil, nil, []string{err.Error()}
		}
	}
	actionPayload := jsoncompat.NewObject()
	actionPayload.Set("actions", payload.MustGet("actions"))
	effective := run.Clone()
	effective.Planning = state
	actions, violations := ValidateActions(actionPayload, team, effective, policy)
	if len(violations) > 0 {
		return nil, nil, violations
	}
	if len(actions) > 0 && len(state.Criteria) == 0 {
		return nil, nil, []string{"首次实质派发必须先在 plan 中明确完整目标的 criteria 和独立 workflows"}
	}
	for _, action := range actions {
		if action.Type != ActionTypeDelegate {
			continue
		}
		workflow := planningWorkflow(state, action.WorkflowID)
		if workflow == nil {
			return nil, nil, []string{"delegate 必须引用 plan.workflows 中已定义的 workflowId"}
		}
		if action.WorkspaceID != "" && action.WorkspaceID != workflow.WorkspaceID {
			return nil, nil, []string{"动作不能改变 workflow 已冻结的 workspaceId"}
		}
		if target, retriage := failureRetriageTarget(run, &action); retriage && target != nil && target.WorkflowID != action.WorkflowID {
			return nil, nil, []string{"失败改派必须保留原任务的 workflowId"}
		}
	}
	feedbackValue, feedbackPresent := payload.Get("feedback")
	if source != nil && source.GoalFeedback {
		if !feedbackPresent {
			return nil, nil, []string{"目标评估缺少 feedback，不能用普通汇总代替验收和续做决定"}
		}
		var feedback planningFeedback
		if err := decodePlanningValue(feedbackValue, &feedback); err != nil {
			return nil, nil, []string{"feedback 不是有效评估：" + err.Error()}
		}
		if err := applyPlanningFeedback(state, run, feedback, actions); err != nil {
			return nil, nil, []string{err.Error()}
		}
		if feedback.Status == "complete" {
			verify, review := goalCheckEvidence(run, team, DefaultGoalContract(GoalScope{}))
			if review.Requirement == "required" && review.Status != "passed" || run.Verify != nil && verify.Status != "passed" {
				return nil, nil, []string{"必要检查未有效通过，不能 complete；请 continue 派发修复或 blocked 说明真实阻塞"}
			}
		}
	} else if feedbackPresent {
		return nil, nil, []string{"只有网关派发的目标评估回合可以提交 feedback"}
	} else if len(actions) > 0 {
		state.Status = "running"
	}
	return actions, state, nil
}

func planningWorkflow(state *GoalPlanningState, id string) *PlanningWorkflow {
	if state != nil {
		for i := range state.Workflows {
			if state.Workflows[i].ID == id {
				return &state.Workflows[i]
			}
		}
	}
	return nil
}

func applyPlanningDefinition(state *GoalPlanningState, plan planningDefinition, policy TeamPolicy) error {
	if len(plan.Criteria) == 0 || len(plan.Criteria) > 32 || len(plan.Workflows) == 0 || len(plan.Workflows) > min(policy.MaxTasksPerRun, 32) {
		return fmt.Errorf("plan 必须包含 1–32 个验收条件和有限个独立 workflow")
	}
	ids := map[string]bool{}
	for _, criterion := range plan.Criteria {
		if !ValidMemberID(criterion.ID) || strings.TrimSpace(criterion.Text) == "" || len([]rune(criterion.Text)) > 1000 || ids[criterion.ID] {
			return fmt.Errorf("验收条件必须有唯一有效 id 和 1–1000 字 text")
		}
		ids[criterion.ID] = true
	}
	if len(state.Criteria) != 0 {
		if len(state.Criteria) > len(plan.Criteria) {
			return fmt.Errorf("不能删除已有验收条件；聊天补充需求可以追加条件")
		}
		for i := range state.Criteria {
			if state.Criteria[i] != plan.Criteria[i] {
				return fmt.Errorf("续做不能改写已冻结的验收条件")
			}
		}
	}
	ids = map[string]bool{}
	for _, workflow := range plan.Workflows {
		if !ValidMemberID(workflow.ID) || strings.TrimSpace(workflow.Title) == "" || len([]rune(workflow.Title)) > 200 || ids[workflow.ID] {
			return fmt.Errorf("workflow 必须有唯一有效 id 和简短 title")
		}
		ids[workflow.ID] = true
		if workflow.WorkspaceID != "" && !ValidMemberID(workflow.WorkspaceID) {
			return fmt.Errorf("workflow workspaceId 必须是有效的已登记工作区 ID")
		}
		if existing := planningWorkflow(state, workflow.ID); existing != nil {
			if *existing != workflow {
				return fmt.Errorf("不能改写既有 workflow 的名称或工作区")
			}
		} else {
			state.Workflows = append(state.Workflows, workflow)
		}
	}
	if len(state.Workflows) > min(policy.MaxTasksPerRun, 32) {
		return fmt.Errorf("workflow 数量超过 Run 任务上限")
	}
	state.Criteria = append([]PlanningCriterion(nil), plan.Criteria...)
	raw, _ := json.Marshal(planningDefinition{Criteria: state.Criteria, Workflows: state.Workflows})
	if len([]rune(string(raw))) > 6000 {
		return fmt.Errorf("规划定义超过 6000 字，请保留完整范围并精简条件表述")
	}
	return nil
}

func applyPlanningFeedback(state *GoalPlanningState, run *Run, feedback planningFeedback, actions []Action) error {
	raw, _ := json.Marshal(feedback)
	if len([]rune(string(raw))) > 6000 {
		return fmt.Errorf("评估说明超过 6000 字，请精简检查依据、缺口与阻塞")
	}
	if len(feedback.Checks) != len(state.Criteria) || len(state.Criteria) == 0 || len(feedback.Remaining) > 32 || len(feedback.Blockers) > 32 {
		return fmt.Errorf("feedback 必须逐项覆盖原始验收条件，缺口和阻塞列表必须有界")
	}
	seen, met := map[string]bool{}, 0
	for _, check := range feedback.Checks {
		known := false
		for _, criterion := range state.Criteria {
			known = known || criterion.ID == check.ID
		}
		if !known || seen[check.ID] || (check.State != "met" && check.State != "unmet" && check.State != "unknown") || len([]rune(check.Reason)) > 1000 {
			return fmt.Errorf("feedback 条件 id/state 无效或重复")
		}
		seen[check.ID] = true
		if len(check.EvidenceTaskIDs) > 32 {
			return fmt.Errorf("验收证据任务数量超过上限")
		}
		for _, id := range check.EvidenceTaskIDs {
			task := findTaskIn(run, id)
			if task == nil || task.Status != TaskCompleted || task.GoalFeedback || task.ExecutionContext == ExecutionContextLeadControl || ResultDisplayText(task.Result) == "" {
				return fmt.Errorf("验收证据任务 %s 不存在、未完成或没有执行结果", id)
			}
		}
		if check.State == "met" {
			met++
			if len(check.EvidenceTaskIDs) == 0 || len(check.EvidenceTaskIDs) > 32 {
				return fmt.Errorf("已满足条件必须引用实际完成任务的可读证据")
			}
		}
	}
	for _, text := range append(append([]string{}, feedback.Remaining...), feedback.Blockers...) {
		if strings.TrimSpace(text) == "" || len([]rune(text)) > 1000 {
			return fmt.Errorf("缺口和阻塞必须是 1–1000 字的具体说明")
		}
	}
	switch feedback.Status {
	case "complete":
		if met != len(state.Criteria) || len(feedback.Remaining)+len(feedback.Blockers)+len(actions) != 0 {
			return fmt.Errorf("目标未全部有证据满足或仍有缺口/动作，不能声明 complete")
		}
	case "continue":
		hasDelegate := false
		for _, action := range actions {
			hasDelegate = hasDelegate || action.Type == ActionTypeDelegate
		}
		if len(feedback.Remaining) == 0 || !hasDelegate {
			return fmt.Errorf("continue 必须明确剩余工作并派发至少一个实际 delegate")
		}
	case "blocked":
		if len(feedback.Blockers) == 0 || len(actions) > 0 {
			return fmt.Errorf("blocked 必须说明具体阻塞，尚有可执行动作时应 continue")
		}
	default:
		return fmt.Errorf("feedback.status 仅支持 complete/continue/blocked")
	}
	previousEvidence := map[string]bool{}
	for _, check := range state.Checks {
		for _, id := range check.EvidenceTaskIDs {
			previousEvidence[id] = true
		}
	}
	newEvidence := false
	for _, check := range feedback.Checks {
		for _, id := range check.EvidenceTaskIDs {
			newEvidence = newEvidence || !previousEvidence[id]
		}
	}
	if feedback.Status == "continue" && len(state.Checks) > 0 && met <= state.LastMet && !newEvidence {
		state.NoProgress++
	} else {
		state.NoProgress = 0
	}
	state.Status, state.LastMet = feedback.Status, met
	state.Checks = append([]PlanningCheck(nil), feedback.Checks...)
	state.Remaining = append([]string(nil), feedback.Remaining...)
	state.Blockers = append([]string(nil), feedback.Blockers...)
	return nil
}
