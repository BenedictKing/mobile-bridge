package team

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

const goalPlanningInstruction = "\n[Goal workflow planning]\n先明确完整原目标的可验证验收条件，不把「开始实现」或一个 Agent 正常返回当作完成。把可独立验收的交付分成 workflows；公共接口/契约任务可以串行，独立 workflow 在真实依赖释放后并行，每个 workflow 自己完成实现和验证，不按工序横切接力。delegate 必须带 workflowId，并用 key/dependsOn 表达真实依赖，不为所有分支强加不必要的前后链。同一成员的不同 workflow 使用独立执行会话；同目录写入仍受独占锁保护。只有用户已登记的独立 workspaceId 可用于并行写，不自动创建 worktree、合并或提高预算。规划信息不足时先派发只读规划任务，后续评估会继续补齐工作。\nteam-actions 对象支持 plan 与 actions：plan={\"criteria\":[{\"id\":\"delivery\",\"text\":\"完整目标与验收标准\"}],\"workflows\":[{\"id\":\"core\",\"title\":\"可独立验收的交付\",\"workspaceId\":\"可选的已登记工作区\"}]}。没有独立工作区时省略 workspaceId，不填写占位符。首次实质派发必须带 plan；后续不得缩减或改写验收条件。actions 使用既有协议，每条 delegate 带对应 workflowId。\n"

const goalFeedbackInstruction = "你是目标评估与续做控制回合。逐项核对完整原目标、计划验收条件、实际产物与执行结果。任务 completed 仅表示回合结束；只有进度说明、空结果、降级产物或未验证声明不能作为完成证据。返回一个 team-actions 对象，包含 feedback 与 actions；可以在块外给用户可读说明。feedback={\"status\":\"continue/complete/blocked\",\"checks\":[{\"id\":\"条件ID\",\"state\":\"met/unmet/unknown\",\"evidenceTaskIds\":[\"已完成且有可读结果的任务ID\"],\"reason\":\"具体依据\"}],\"remaining\":[\"尚未完成的具体工作\"],\"blockers\":[\"权限、环境或外部资源阻塞\"]}。只有全部条件有证据满足、remaining/blockers 为空时才 complete，actions=[]。未完成且仍有可推进工作时 continue，并在 actions 中派发下一批独立 workflow；只补缺口，保留有效成果，不重复全部工作。某些外部步骤阻塞时继续其余可推进部分。只有无法再推进时 blocked，actions=[]，明确需要用户处理什么。不要只做最终汇总，不要缩小目标、提高预算或自行解除用户停止。"

func goalPlanningBriefing(run *Run, task *Task) string {
	if run == nil || run.Planning == nil || task == nil || task.WorkflowID != "" || task.ExecutionContext == ExecutionContextLeadRun || task.IssueClassificationOnly || (task.Reduction && !task.GoalFeedback) {
		return ""
	}
	definition := planningDefinition{Criteria: run.Planning.Criteria, Workflows: run.Planning.Workflows}
	raw, _ := json.Marshal(definition)
	briefing := goalPlanningInstruction + fmt.Sprintf("完整本轮目标：%s\n当前评估轮次 %d/%d；既有冻结规划：%s\n", run.Goal, run.Planning.Iteration, run.Planning.MaxIterations, raw)
	briefing += "聊天窗口的新输入由你自动归集：先按 team-issues 关联原事项，再结合已有 workflow 的标题与任务定位相关分支，使用其 workflowId 派发具体补充工作；只有独立交付才新建 workflow。用户补充条件可在 plan.criteria 末尾追加，必须完整保留已有条件。不要要求用户填写完成条件或反馈表。\n"
	if task.GoalFeedback {
		briefing += goalFeedbackInstruction + "\n[Execution facts]\n"
		var facts strings.Builder
		for _, executed := range run.Tasks {
			text := ResultDisplayText(executed.Result)
			fmt.Fprintf(&facts, "taskId=%s workflowId=%s status=%s resultPresent=%t · %s\n", executed.ID, executed.WorkflowID, executed.Status, text != "", truncateRunes(text, 240))
			if executed.ExecutionContext == ExecutionContextLeadControl && !executed.GoalFeedback {
				fmt.Fprintf(&facts, "控制输入：%s\n", truncateRunes(executed.Prompt, 1000))
			}
		}
		briefing += boundedContinuationText(facts.String(), 8000)
		if run.Verify != nil {
			verify, _ := json.Marshal(run.Verify)
			briefing += "\n[Verification facts]\n" + truncateRunes(string(verify), 1600) + "\n"
		}
	}
	return briefing
}

func goalPlanningSummary(state *GoalPlanningState) string {
	if state == nil {
		return ""
	}
	var builder strings.Builder
	labels := map[string]string{"complete": "目标条件已有完成证据", "continue": "目标未完成，继续补齐缺口", "blocked": "目标未完成，等待处理阻塞", "running": "正在执行", "evaluating": "正在评估目标", "planning": "正在规划"}
	fmt.Fprintf(&builder, "%s（评估 %d/%d）\n", labels[state.Status], state.Iteration, state.MaxIterations)
	for _, workflow := range state.Workflows {
		fmt.Fprintf(&builder, "- workflow %s：%s\n", workflow.ID, workflow.Title)
	}
	for _, criterion := range state.Criteria {
		label, reason := "未取得证据", ""
		for _, check := range state.Checks {
			if check.ID == criterion.ID {
				label = map[string]string{"met": "已有证据", "unmet": "未完成", "unknown": "未取得证据"}[check.State]
				reason = check.Reason
				if len(check.EvidenceTaskIDs) > 0 {
					reason += "（任务 " + strings.Join(check.EvidenceTaskIDs, "、") + "）"
				}
			}
		}
		fmt.Fprintf(&builder, "- %s：%s %s\n", criterion.Text, label, reason)
	}
	for _, remaining := range state.Remaining {
		fmt.Fprintf(&builder, "- 待完成：%s\n", remaining)
	}
	for _, blocker := range state.Blockers {
		fmt.Fprintf(&builder, "- 阻塞：%s\n", blocker)
	}
	return builder.String()
}

func (s *Service) ensureGoalFeedbackLocked(state *runState, run *Run) bool {
	if run.Planning == nil || len(run.Planning.Criteria) == 0 || run.Planning.Status == "complete" {
		return false
	}
	if run.Planning.Status == "blocked" {
		return s.parkGoalPlanningLocked(state, run, strings.Join(run.Planning.Blockers, "；"))
	}
	for _, task := range run.Tasks {
		if !terminalTask(task.Status) {
			return false
		}
		if task.Status != TaskCompleted {
			return false
		}
	}
	team := s.store.Team(run.TeamID)
	policy := policyOf(team)
	if !policy.AutoDispatch {
		return s.parkGoalPlanningLocked(state, run, "自动派发关闭，规划等待用户处理")
	}
	if len(run.Tasks) >= policy.MaxTasksPerRun {
		return s.parkGoalPlanningLocked(state, run, "任务数量已达上限，目标尚未验收完成")
	}
	if run.Planning.Iteration >= run.Planning.MaxIterations {
		return s.parkGoalPlanningLocked(state, run, "目标评估轮次已达上限，目标尚未完成")
	}
	iteration := run.Planning.Iteration + 1
	depends := make([]string, 0, len(run.Tasks))
	for _, task := range run.Tasks {
		depends = append(depends, task.ID)
	}
	task := Task{
		ID: fmt.Sprintf("lead-goal-check-%d", iteration), MemberID: team.LeadID,
		Prompt: goalFeedbackInstruction, Phase: "evaluate", GoalFeedback: true, Reduction: true,
		ExecutionContext: ExecutionContextLeadControl, WriteLock: TaskWriteLockNone,
		WorkspaceMode: WorkspaceShared, DependsOn: depends, Ordinal: len(run.Tasks), Status: TaskPlanned,
		InstructionsHead: InstructionsHead(goalFeedbackInstruction),
	}
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) || fresh.Status == RunPaused || fresh.Status == RunWaitingInput || findTaskIn(fresh, task.ID) != nil {
			return nil, false
		}
		fresh.Planning.Iteration, fresh.Planning.Status = iteration, "evaluating"
		fresh.Tasks = append(fresh.Tasks, task)
		payload := map[string]any{"taskId": task.ID, "memberId": task.MemberID, "phase": task.Phase, "executionContext": task.ExecutionContext, "instructionsHead": task.InstructionsHead, "reduction": true}
		addWorkflowTaskPayload(payload, &task)
		event := newEvent(fresh.ID, EventTaskPlanned, "planner", payload)
		event.IdempotencyKey = "team:" + fresh.TeamID + ":run:" + fresh.ID + ":goal-check:" + task.ID
		return []*Event{event, planningEvent(fresh, task.ID)}, true
	})
	if err != nil {
		log.Printf("team run %s 目标评估记账失败: %v", run.ID, err)
	}
	return true
}

// goalLoopRequiresAmend 表示当前自动规划 Run 已无法在原 Run 中继续追加
// 任务。聊天输入应创建带 parentRunId 的新修订 Run，保留旧 Run 的证据链。
func goalLoopRequiresAmend(run *Run, policy TeamPolicy) bool {
	if run == nil || run.Status != RunWaitingInput || run.Planning == nil || !strings.HasPrefix(run.StopReason, "goal_loop: ") {
		return false
	}
	return len(run.Tasks) >= policy.MaxTasksPerRun || run.Planning.Iteration >= run.Planning.MaxIterations
}

func (s *Service) parkGoalPlanningLocked(state *runState, run *Run, reason string) bool {
	if reason == "" {
		reason = "目标未完成，需要用户处理阻塞"
	}
	if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) || fresh.Status == RunPaused || fresh.Status == RunWaitingInput {
			return nil, false
		}
		fresh.Status, fresh.StopReason = RunWaitingInput, "goal_loop: "+reason
		event := newEvent(fresh.ID, EventRunWaitingInput, "planner", map[string]any{"stopReason": fresh.StopReason})
		return []*Event{event}, true
	}); err != nil {
		log.Printf("team run %s 目标停靠记账失败: %v", run.ID, err)
	}
	return true
}
