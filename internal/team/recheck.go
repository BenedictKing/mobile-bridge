package team

import (
	"fmt"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// RecheckRun 由用户显式触发，只读取原派发回合，不发送消息或重跑任务。
// 已未知的任务不会再被 watcher 观察，因此允许在终态 Run 上核查迟到结果。
func (s *Service) RecheckRun(runID string) (*Run, error) {
	run := s.store.Run(runID)
	if run == nil {
		return nil, ErrNotFound
	}
	if run.Status != RunOutcomeUnknown && run.Status != RunWaitingInput {
		return nil, fmt.Errorf("%w: 只有结果未知或等待确认的 Run 可以重新核查", ErrRunNotWaiting)
	}
	team := s.store.Team(run.TeamID)
	if team == nil {
		return nil, ErrNotFound
	}
	var recovered []Transition
	sessions, err := s.DispatchSessions(run)
	if err != nil {
		return nil, err
	}
	for _, task := range run.Tasks {
		if task.Status != TaskOutcomeUnknown || task.SentText == "" || task.SubmissionID == "" {
			continue
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		sessionID := sessions[task.ID]
		if sessionID == "" {
			continue
		}
		_, session, err := s.resolver.Resolve(sessionID)
		if err != nil || session == nil {
			continue
		}
		turn := uniqueRecheckTurn(session.Snapshot(), task.SentText)
		if turn == nil || turnStatus(turn) != "completed" || cancelledTurn(turn) {
			continue
		}
		// Lead 动作协议需要原子执行动作，不能在核查路径仅补写 completed。
		if run.AutoPlan && task.MemberID == team.LeadID && task.ExecutionContext != ExecutionContextLeadRun {
			continue
		}
		result := ExtractResult(sessionID, turn, policyOf(team).MaxResultChars)
		recovered = append(recovered, Transition{TaskID: task.ID, Result: &result})
	}
	return s.commitRecheckedResults(run, recovered)
}

// 历史核查没有实时观察窗口；同文多个回合时不能凭“最后一条”猜属于哪个派发。
func uniqueRecheckTurn(snapshot *jsoncompat.Object, sentText string) *jsoncompat.Object {
	turn := matchTurn(snapshot, sentText)
	if turn == nil {
		return nil
	}
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return nil
	}
	matched := 0
	for _, value := range turns.Items {
		candidate, ok := value.(*jsoncompat.Object)
		if !ok {
			continue
		}
		single := jsoncompat.NewObject()
		single.Set("turns", &jsoncompat.Array{Items: []any{candidate}})
		if matchTurn(single, sentText) != nil {
			matched++
		}
	}
	if matched != 1 {
		return nil
	}
	return turn
}

// DispatchSessions 按 submission 与 attempt 从派发事实定位历史会话，不使用线程的后来绑定。
func (s *Service) DispatchSessions(run *Run) (map[string]string, error) {
	sessions := map[string]string{}
	for _, task := range run.Tasks {
		if task.DispatchSessionID != "" {
			sessions[task.ID] = task.DispatchSessionID
		}
	}
	events, err := s.journal.Replay()
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.RunID != run.ID || event.Type != EventTaskDispatching {
			continue
		}
		task := findTaskIn(run, payloadString(event.Payload, "taskId"))
		if task == nil || task.SubmissionID == "" || payloadString(event.Payload, "submissionId") != task.SubmissionID {
			continue
		}
		if attempt := payloadInt(event.Payload, "attempt"); attempt != 0 && task.Attempt != 0 && attempt != task.Attempt {
			continue
		}
		sessions[task.ID] = payloadString(event.Payload, "sessionId")
	}
	return sessions, nil
}

func (s *Service) commitRecheckedResults(observed *Run, recovered []Transition) (*Run, error) {
	state := s.stateFor(observed.ID)
	state.mu.Lock()
	defer state.mu.Unlock()
	if fresh := s.store.Run(observed.ID); fresh == nil {
		return nil, ErrNotFound
	} else if fresh.Version != observed.Version {
		return nil, ErrVersionConflict
	}
	err := s.commitRun(observed.ID, func(fresh *Run) ([]*Event, bool) {
		if fresh.Version != observed.Version {
			return nil, false
		}
		var events []*Event
		for _, result := range recovered {
			task := findTaskIn(fresh, result.TaskID)
			if task == nil || task.Status != TaskOutcomeUnknown {
				continue
			}
			// 唯一允许覆盖的终态是同一版本、同一派发的 outcome_unknown。
			task.Status = TaskRunning
			event, applied := mutateTaskTerminal(fresh, task.ID, TaskCompleted, result.Result.StopReason, "", "", result.Result, EventTaskCompleted, "user_recheck")
			if applied {
				task.Outcome = ""
				event.Payload["reconciled"] = true
				events = append(events, event)
			}
		}
		if len(events) == 0 {
			return nil, false
		}
		// 继续/验证仍由原有 Resume 流程完成，核查本身不能绕过验证闸门。
		fresh.Status = RunWaitingInput
		fresh.StopReason = "已从原会话核查完成结果；请继续运行，已停止的依赖分支可创建修订 Run"
		events = append(events, newEvent(fresh.ID, EventRunWaitingInput, "user_recheck", map[string]any{"stopReason": fresh.StopReason}))
		return events, true
	})
	return s.store.Run(observed.ID), err
}
