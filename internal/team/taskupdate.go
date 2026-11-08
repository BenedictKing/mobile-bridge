package team

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/google/uuid"
)

const (
	TaskUpdatePending = "update_pending"

	TaskUpdateAccepted         = "accepted"
	TaskUpdateInterruptPending = "interrupt_pending"
	TaskUpdateOutcomeUnknown   = "outcome_unknown"
)

var ErrTaskUpdateConflict = errors.New("任务更新与当前状态冲突")

// TaskUpdateResult 区分更新已受理、重放和停止结果未知；accepted 不代表成员
// 已完成，交付与完成仍由任务的 dispatching/completed 事实表达。
type TaskUpdateResult struct {
	Status    string `json:"status"`
	Duplicate bool   `json:"duplicate,omitempty"`
	Task      *Task  `json:"task"`
}

func findTaskUpdate(run *Run, updateID string) *Task {
	if run == nil || updateID == "" {
		return nil
	}
	for i := range run.Tasks {
		if run.Tasks[i].UpdateID == updateID {
			return &run.Tasks[i]
		}
	}
	return nil
}

func sameTaskUpdate(task *Task, targetTaskID, strategy, text string) bool {
	return task != nil && task.UpdateOf == targetTaskID && task.UpdateStrategy == strategy && task.Prompt == text
}

func taskUpdateEventPayload(task *Task) map[string]any {
	return map[string]any{
		"taskId": task.ID, "memberId": task.MemberID,
		"instructionsHead": task.InstructionsHead, "prompt": task.Prompt,
		"dependsOn": append([]string(nil), task.DependsOn...),
		"writeLock": task.WriteLock, "updateId": task.UpdateID,
		"updateOf": task.UpdateOf, "updateStrategy": task.UpdateStrategy,
		"updateStatus": task.UpdateStatus,
	}
}

// UpdateTask 在既有 Run DAG 中受理 enqueue/interrupt 更新。操作 UUID 同时是
// 审计与跨重启幂等身份；expectedVersion 防止基于过期任务视图更新。
func (s *Service) UpdateTask(teamID, runID, targetTaskID, opID, strategy, text string, expectedVersion uint64) (*TaskUpdateResult, error) {
	if _, err := uuid.Parse(opID); err != nil {
		return nil, errors.New("更新操作标识无效")
	}
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > 10000 {
		return nil, errors.New("请输入 1–10000 字的更新内容")
	}
	if strategy != "enqueue" && strategy != "interrupt" {
		return nil, errors.New("任务更新策略必须是 enqueue 或 interrupt")
	}
	team := s.store.Team(teamID)
	if team == nil {
		return nil, ErrNotFound
	}
	if existing := findTaskUpdate(s.store.Run(runID), opID); existing != nil {
		if !sameTaskUpdate(existing, targetTaskID, strategy, text) {
			return nil, fmt.Errorf("%w: 同一更新操作不能用于不同内容", ErrTaskUpdateConflict)
		}
		if existing.UpdateStatus == TaskUpdateInterruptPending {
			if err := s.parkUnknownTaskUpdate(runID, existing.ID, "中断在结果确认前重启，未自动重发"); err != nil {
				return nil, err
			}
			existing = findTaskUpdate(s.store.Run(runID), opID)
		}
		status := existing.UpdateStatus
		return &TaskUpdateResult{Status: status, Duplicate: true, Task: existing.Clone()}, nil
	}

	state := s.stateFor(runID)
	state.mu.Lock()
	var update *Task
	var duplicate bool
	var decisionErr error
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if fresh.TeamID != teamID {
			decisionErr = ErrNotFound
			return nil, false
		}
		if existing := findTaskUpdate(fresh, opID); existing != nil {
			if !sameTaskUpdate(existing, targetTaskID, strategy, text) {
				decisionErr = fmt.Errorf("%w: 同一更新操作不能用于不同内容", ErrTaskUpdateConflict)
				return nil, false
			}
			update = existing.Clone()
			duplicate = true
			return nil, false
		}
		if fresh.Version != expectedVersion {
			decisionErr = ErrVersionConflict
			return nil, false
		}
		if terminalRun(fresh.Status) {
			decisionErr = fmt.Errorf("%w: 当前状态 %s", ErrRunFinished, fresh.Status)
			return nil, false
		}
		target := findTaskIn(fresh, targetTaskID)
		if target == nil {
			decisionErr = fmt.Errorf("%w: 目标任务不存在", ErrNotFound)
			return nil, false
		}
		if limit := policyOf(team).MaxTasksPerRun; limit > 0 && len(fresh.Tasks) >= limit {
			decisionErr = fmt.Errorf("%w: 当前 Run 已达 %d 个任务上限", ErrTaskUpdateConflict, limit)
			return nil, false
		}
		update = &Task{
			ID: "update-" + opID, MemberID: target.MemberID, Prompt: text,
			Ordinal: len(fresh.Tasks), DependsOn: []string{targetTaskID},
			WriteLock: target.WriteLock, WorkspaceMode: target.WorkspaceMode,
			WorkspaceID: target.WorkspaceID, BaseRevision: target.BaseRevision,
			InstructionsHead: InstructionsHead(text), UpdateID: opID,
			UpdateOf: targetTaskID, UpdateStrategy: strategy,
		}
		eventType := EventTaskPlanned
		update.Status = TaskPlanned
		update.UpdateStatus = TaskUpdateAccepted
		if strategy == "interrupt" {
			eventType = EventTaskUpdateIntent
			update.Status = TaskUpdatePending
			update.UpdateStatus = TaskUpdateInterruptPending
		}
		fresh.Tasks = append(fresh.Tasks, *update)
		event := newEvent(runID, eventType, "user", taskUpdateEventPayload(update))
		event.IdempotencyKey = "team:" + teamID + ":run:" + runID + ":task-update:" + opID
		return []*Event{event}, true
	})
	state.mu.Unlock()
	if decisionErr != nil {
		return nil, decisionErr
	}
	if err != nil {
		return nil, err
	}
	if duplicate {
		return &TaskUpdateResult{Status: update.UpdateStatus, Duplicate: true, Task: update}, nil
	}
	if strategy == "enqueue" {
		s.startLoop(state)
		return &TaskUpdateResult{Status: TaskUpdateAccepted, Task: update.Clone()}, nil
	}
	if err := s.confirmTaskInterrupt(team, runID, targetTaskID); err != nil {
		if parkErr := s.parkUnknownTaskUpdate(runID, update.ID, err.Error()); parkErr != nil {
			return nil, parkErr
		}
		return &TaskUpdateResult{Status: TaskUpdateOutcomeUnknown, Task: findTaskIn(s.store.Run(runID), update.ID).Clone()}, nil
	}
	if err := s.activateInterruptedTaskUpdate(state, team, runID, update.ID, targetTaskID); err != nil {
		return nil, err
	}
	return &TaskUpdateResult{Status: TaskUpdateAccepted, Task: findTaskIn(s.store.Run(runID), update.ID).Clone()}, nil
}

func (s *Service) confirmTaskInterrupt(team *Team, runID, targetTaskID string) error {
	run := s.store.Run(runID)
	target := findTaskIn(run, targetTaskID)
	if target == nil {
		return errors.New("目标任务不存在，停止结果未知")
	}
	if terminalTask(target.Status) || target.SubmissionID == "" {
		return nil
	}
	member := memberOf(team, target.MemberID)
	if member == nil {
		return errors.New("目标成员不存在，停止结果未知")
	}
	if sub, ok := s.queue.Submission(member.SessionID, target.SubmissionID); ok {
		switch sub.Status {
		case SubStatusCancelled, SubStatusFailed, SubStatusIgnored:
			return nil
		case SubStatusUnknown:
			return errors.New("原任务派发结果未知，不能确认停止")
		case SubStatusQueued:
			if err := s.queue.CancelSubmission(member.SessionID, target.SubmissionID); err == nil {
				return nil
			}
		}
	} else {
		return errors.New("无法读取原任务派发记录，停止结果未知")
	}
	_, session, err := s.resolver.Resolve(member.SessionID)
	if err != nil || session == nil {
		return errors.New("无法连接目标成员会话，停止结果未知")
	}
	return s.interruptAgentTurn(session, s.sentTextFor(run, target, policyOf(team)))
}

func (s *Service) interruptTurnAndConfirm(session *acpruntime.Session, sentText string) error {
	turn := matchTurn(session.Snapshot(), sentText)
	if turn == nil {
		return errors.New("未找到原任务回合，停止结果未知")
	}
	if turnStatus(turn) != "inProgress" {
		return nil
	}
	if err := session.CancelAndWait(10 * time.Second); err != nil {
		return fmt.Errorf("停止原任务未确认: %w", err)
	}
	turn = matchTurn(session.Snapshot(), sentText)
	if turn == nil || turnStatus(turn) == "inProgress" {
		return errors.New("停止请求后原任务仍在运行，结果未知")
	}
	return nil
}

func (s *Service) parkUnknownTaskUpdate(runID, updateID, reason string) error {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	return s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		update := findTaskIn(fresh, updateID)
		if update == nil || update.UpdateStatus != TaskUpdateInterruptPending {
			return nil, false
		}
		update.Status = TaskOutcomeUnknown
		update.UpdateStatus = TaskUpdateOutcomeUnknown
		update.StopReason = reason
		fresh.Status = RunWaitingInput
		fresh.StopReason = reason
		return []*Event{
			newEvent(runID, EventTaskStopped, "gateway", map[string]any{
				"taskId": updateID, "status": TaskOutcomeUnknown,
				"updateStatus": TaskUpdateOutcomeUnknown, "stopReason": reason,
			}),
			newEvent(runID, EventRunWaitingInput, "gateway", map[string]any{"reason": reason}),
		}, true
	})
}

func (s *Service) activateInterruptedTaskUpdate(state *runState, team *Team, runID, updateID, targetTaskID string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	var decisionErr error
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		update := findTaskIn(fresh, updateID)
		target := findTaskIn(fresh, targetTaskID)
		if update == nil || target == nil || update.UpdateStatus != TaskUpdateInterruptPending {
			decisionErr = errors.New("中断意图已变化，请查询任务状态")
			return nil, false
		}
		events := []*Event{}
		if !terminalTask(target.Status) {
			target.Status = TaskStopped
			target.StopReason = "用户确认中断并更新任务"
			events = append(events, newEvent(runID, EventTaskStopped, "user", map[string]any{
				"taskId": target.ID, "status": TaskStopped, "stopReason": target.StopReason,
			}))
			events = append(events, withdrawTaskQuestions(fresh, target.ID, questionWithdrawTaskTerminal, "user")...)
		}
		update.Status = TaskPlanned
		update.UpdateStatus = TaskUpdateAccepted
		update.StopReason = ""
		if fresh.Status == RunWaitingInput || fresh.Status == RunPaused {
			fresh.Status = RunDispatching
			fresh.StopReason = ""
		}
		events = append(events, newEvent(runID, EventTaskPlanned, "gateway", taskUpdateEventPayload(update)))
		return events, true
	})
	if decisionErr != nil {
		return decisionErr
	}
	if err == nil {
		s.startLoopLocked(state)
	}
	return err
}
