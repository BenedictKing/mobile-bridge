package team

import "fmt"

// CreateConversationAmendRun 在旧 Run 已终态后延续团队对话。新消息始终形成
// 新的 Lead 任务，不按相同文本复用旧结果；parentRunId 只负责保留修订血缘。
func (s *Service) CreateConversationAmendRun(team *Team, parentRunID, runID, goal string, attachments []string) (*Run, error) {
	if team == nil {
		return nil, fmt.Errorf("team 不能为 nil")
	}
	if parentRunID == "" || parentRunID == runID {
		return nil, fmt.Errorf("团队对话修订必须指定另一个父 Run")
	}
	parent := s.store.Run(parentRunID)
	if parent == nil || parent.TeamID != team.ID {
		return nil, fmt.Errorf("%w: run=%s", ErrNotFound, parentRunID)
	}
	if !terminalRun(parent.Status) && !goalLoopRequiresAmend(parent, team.Policy) {
		return nil, fmt.Errorf("%w: 当前 Run 尚未结束，后续消息应追加到原 Run", ErrRunNotWaiting)
	}
	return s.createRun(team, runID, goal, ModeSingle, "", nil, nil, true, "", parentRunID, attachments)
}

// FollowUpConversationRun 把一条团队消息追加为当前 autoPlan Run 的任务。
// 默认进入 Lead 控制线程；sourceTaskID 非空时表示显式追问某个干活任务，
// 新任务复用其成员与执行上下文。waiting_input/paused 由用户的新消息显式恢复；
// 旧任务及其结果保持原样。
func (s *Service) FollowUpConversationRun(team *Team, runID, taskID, goal string, attachments []string, sourceTaskIDs ...string) (*Run, bool, error) {
	sourceTaskID := ""
	if len(sourceTaskIDs) > 0 {
		sourceTaskID = sourceTaskIDs[0]
	}
	if team == nil {
		return nil, false, fmt.Errorf("team 不能为 nil")
	}
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()

	duplicate := false
	var decisionErr error
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			decisionErr = fmt.Errorf("%w: 当前状态 %s", ErrRunFinished, fresh.Status)
			return nil, false
		}
		if fresh.Status == RunReducing {
			decisionErr = fmt.Errorf("%w: 当前 Run 正在验证，请稍后继续", ErrRunNotWaiting)
			return nil, false
		}
		if sourceTaskID == "" && (!fresh.AutoPlan || fresh.Mode != ModeSingle) {
			decisionErr = fmt.Errorf("%w: 当前显式计划 Run 请在其详情页处理", ErrRunNotWaiting)
			return nil, false
		}
		memberID := team.LeadID
		executionContext := ExecutionContextLeadControl
		var dependsOn []string
		if sourceTaskID != "" {
			source := findTaskIn(fresh, sourceTaskID)
			if source == nil {
				decisionErr = fmt.Errorf("%w: 追问来源任务 %s", ErrNotFound, sourceTaskID)
				return nil, false
			}
			memberID = source.MemberID
			executionContext = source.ExecutionContext
			if source.Status == TaskPlanned || source.Status == TaskReady || source.Status == TaskDispatching || source.Status == TaskRunning {
				dependsOn = []string{sourceTaskID}
			}
		}
		if existing := findTaskIn(fresh, taskID); existing != nil {
			if existing.MemberID == memberID && existing.Prompt == goal && sameStringSlices(existing.Attachments, attachments) {
				duplicate = true
				return nil, false
			}
			decisionErr = ErrRunContentMismatch
			return nil, false
		}
		if limit := policyOf(team).MaxTasksPerRun; limit > 0 && len(fresh.Tasks) >= limit {
			decisionErr = fmt.Errorf("%w: 当前 Run 已达 %d 个任务的上限，请先完成或取消后再继续", ErrRunNotWaiting, limit)
			return nil, false
		}
		task := Task{
			ID: taskID, MemberID: memberID, Prompt: goal, Attachments: append([]string(nil), attachments...),
			Ordinal: len(fresh.Tasks), Status: TaskPlanned, InstructionsHead: InstructionsHead(goal),
			ExecutionContext: executionContext, DependsOn: dependsOn,
		}
		if sourceTaskID != "" {
			source := findTaskIn(fresh, sourceTaskID)
			task.WorkflowID, task.ExecutionThreadID = source.WorkflowID, source.ExecutionThreadID
			task.WorkspaceID, task.WorkspaceMode, task.WriteLock = source.WorkspaceID, source.WorkspaceMode, source.WriteLock
		}
		task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
		fresh.Tasks = append(fresh.Tasks, task)
		if fresh.Status == RunWaitingInput || fresh.Status == RunPaused {
			fresh.Status = RunDispatching
			fresh.StopReason = ""
		}
		if fresh.Planning != nil {
			fresh.Planning.Status = "running"
			fresh.Planning.Blockers = nil
			fresh.StopReason = ""
		}
		payload := map[string]any{
			"taskId": task.ID, "memberId": task.MemberID, "instructionsHead": task.InstructionsHead,
			"executionContext": task.ExecutionContext,
		}
		if len(task.Attachments) > 0 {
			payload["attachments"] = append([]string(nil), task.Attachments...)
		}
		if sourceTaskID != "" {
			payload["sourceTaskId"] = sourceTaskID
			payload["prompt"] = task.Prompt
			payload["dependsOn"] = append([]string(nil), task.DependsOn...)
		}
		addWorkflowTaskPayload(payload, &task)
		events := []*Event{newEvent(runID, EventTaskPlanned, "user", payload)}
		if fresh.Planning != nil {
			events = append(events, planningEvent(fresh, "input-"+taskID))
		}
		return events, true
	})
	if decisionErr != nil {
		return nil, false, decisionErr
	}
	if err != nil {
		return nil, false, err
	}
	if duplicate {
		return s.store.Run(runID), true, nil
	}
	s.resetStallClockLocked(state)
	s.startLoopLocked(state)
	return s.store.Run(runID), false, nil
}
