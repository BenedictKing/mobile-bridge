package team

import (
	"errors"
	"fmt"
	"strings"
)

// TaskSummary 是主控使用的有界任务投影；执行事实仍由 Run/Task journal 持有。
type TaskSummary struct {
	TaskID              string `json:"taskId"`
	RunID               string `json:"runId"`
	WorkItemID          string `json:"workItemId,omitempty"`
	MemberID            string `json:"memberId"`
	Status              string `json:"status"`
	RunStatus           string `json:"runStatus"`
	WaitingReason       string `json:"waitingReason,omitempty"`
	Attempt             int    `json:"attempt,omitempty"`
	LatestEventSequence uint64 `json:"latestEventSequence"`
}

// TaskNotification 是主控按游标消费的任务状态通知。
type TaskNotification struct {
	EventID    string `json:"eventId"`
	Sequence   uint64 `json:"sequence"`
	RunID      string `json:"runId"`
	TaskID     string `json:"taskId,omitempty"`
	WorkItemID string `json:"workItemId,omitempty"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

type taskViewFacts struct {
	runs      []*Run
	events    []*Event
	workItems map[string]string
	latest    map[string]uint64
	head      uint64
}

func taskEventKey(runID, taskID string) string { return runID + "\x00" + taskID }

func (s *Service) readTaskViewFacts(teamID string) (*taskViewFacts, error) {
	if s.store.Team(teamID) == nil {
		return nil, ErrNotFound
	}
	if s.journal == nil {
		return nil, errors.New("团队任务 journal 未装配")
	}
	events, err := s.journal.Replay()
	if err != nil {
		return nil, fmt.Errorf("团队任务 journal 读取失败: %w", err)
	}
	facts := &taskViewFacts{
		runs:      s.store.RunsByTeam(teamID),
		events:    events,
		workItems: map[string]string{},
		latest:    map[string]uint64{},
	}
	runTeams := make(map[string]string, len(facts.runs))
	for _, run := range facts.runs {
		runTeams[run.ID] = run.TeamID
	}
	for _, event := range events {
		if event == nil {
			continue
		}
		facts.head = event.Sequence
		if event.Type == EventRunCreated {
			if id := payloadString(event.Payload, "teamId"); id != "" {
				runTeams[event.RunID] = id
			}
		}
		if runTeams[event.RunID] != teamID {
			continue
		}
		if taskID := payloadString(event.Payload, "taskId"); taskID != "" {
			facts.latest[taskEventKey(event.RunID, taskID)] = event.Sequence
		}
	}
	if s.inputs != nil {
		for _, input := range s.inputs.ListAll(teamID) {
			for i := range input.WorkItems {
				item := &input.WorkItems[i]
				for _, taskID := range item.TaskIDs {
					facts.workItems[taskEventKey(item.RunID, taskID)] = item.ID
				}
			}
		}
	}
	return facts, nil
}

func taskSummary(run *Run, task *Task, facts *taskViewFacts) TaskSummary {
	key := taskEventKey(run.ID, task.ID)
	reason := strings.TrimSpace(task.StopReason)
	if reason == "" {
		reason = strings.TrimSpace(task.Error)
	}
	if reason == "" && run.Status == RunWaitingInput && !terminalTask(task.Status) {
		reason = strings.TrimSpace(run.StopReason)
	}
	if reason == "" && task.Status == TaskPlanned && len(task.DependsOn) > 0 {
		reason = "等待依赖任务到达终态"
	}
	return TaskSummary{
		TaskID: task.ID, RunID: run.ID, WorkItemID: facts.workItems[key],
		MemberID: task.MemberID, Status: task.Status, RunStatus: run.Status,
		WaitingReason: reason, Attempt: task.Attempt,
		LatestEventSequence: facts.latest[key],
	}
}

// ListTasks 返回 Team 的任务列表；inFlightOnly 时只保留非终态任务。
func (s *Service) ListTasks(teamID string, inFlightOnly bool) ([]TaskSummary, error) {
	facts, err := s.readTaskViewFacts(teamID)
	if err != nil {
		return nil, err
	}
	rows := []TaskSummary{}
	for _, run := range facts.runs {
		for i := range run.Tasks {
			task := &run.Tasks[i]
			if inFlightOnly && terminalTask(task.Status) {
				continue
			}
			rows = append(rows, taskSummary(run, task, facts))
		}
	}
	return rows, nil
}

// backgroundTaskBriefing 在下一次 Lead 决策前注入有界在途摘要；长任务由网关
// 持有，Lead 不需要把成员调用当成同步工具等待。
func (s *Service) backgroundTaskBriefing(run *Run, currentTaskID string) string {
	if run == nil {
		return ""
	}
	rows, err := s.ListTasks(run.TeamID, true)
	if err != nil {
		return ""
	}
	const maxRows = 20
	var builder strings.Builder
	count := 0
	for _, row := range rows {
		if row.RunID == run.ID && row.TaskID == currentTaskID {
			continue
		}
		if count == 0 {
			builder.WriteString("\n[Team background tasks]\n这些任务由网关后台执行；不要同步等待。用户可继续输入，更新默认 enqueue；改变正在写入的目标时先请求用户确认是否 interrupt。\n")
		}
		if count == maxRows {
			builder.WriteString("更多在途任务已通过 list 接口提供。\n")
			break
		}
		fmt.Fprintf(&builder, "- taskId=%s runId=%s workItemId=%s status=%s latestEventSequence=%d",
			row.TaskID, row.RunID, row.WorkItemID, row.Status, row.LatestEventSequence)
		if reason := strings.Join(strings.Fields(row.WaitingReason), " "); reason != "" {
			builder.WriteString(" waitingReason=" + truncateRunes(reason, 160))
		}
		builder.WriteString("\n")
		count++
	}
	return builder.String()
}

// CheckTask 返回一个明确属于 Team/Run 的任务投影。
func (s *Service) CheckTask(teamID, runID, taskID string) (*TaskSummary, error) {
	facts, err := s.readTaskViewFacts(teamID)
	if err != nil {
		return nil, err
	}
	for _, run := range facts.runs {
		if run.ID != runID {
			continue
		}
		if task := findTaskIn(run, taskID); task != nil {
			result := taskSummary(run, task, facts)
			return &result, nil
		}
	}
	return nil, ErrNotFound
}

func notificationStatus(event *Event) (string, bool) {
	switch event.Type {
	case EventTaskCompleted:
		return TaskCompleted, true
	case EventTaskFailed:
		return TaskFailed, true
	case EventTaskOutcomeUnknown:
		return TaskOutcomeUnknown, true
	case EventTaskStopped:
		status := payloadString(event.Payload, "status")
		if status == "" {
			status = TaskStopped
		}
		return status, true
	case EventQuestionOpened:
		if payloadString(event.Payload, "kind") == "approval" {
			return "waiting_approval", true
		}
		return "waiting_input", true
	case EventRunWaitingInput:
		return "waiting_input", true
	default:
		return "", false
	}
}

// TaskNotifications 从 journal 补读 after 之后的通知。cursor 已覆盖的事件无论
// 是否命中筛选都不会重复返回；相同事件 ID 的重复记录只投影一次。
func (s *Service) TaskNotifications(teamID string, after uint64, limit int) ([]TaskNotification, uint64, error) {
	if limit <= 0 {
		limit = 50
	} else if limit > 200 {
		limit = 200
	}
	facts, err := s.readTaskViewFacts(teamID)
	if err != nil {
		return nil, 0, err
	}
	runTeams := make(map[string]string, len(facts.runs))
	for _, run := range facts.runs {
		runTeams[run.ID] = run.TeamID
	}
	rows := []TaskNotification{}
	seen := map[string]bool{}
	cursor := after
	for _, event := range facts.events {
		if event == nil {
			continue
		}
		if event.Sequence > after {
			cursor = event.Sequence
		}
		if event.Type == EventRunCreated {
			if id := payloadString(event.Payload, "teamId"); id != "" {
				runTeams[event.RunID] = id
			}
		}
		if runTeams[event.RunID] != teamID {
			continue
		}
		status, ok := notificationStatus(event)
		if !ok || seen[event.ID] {
			continue
		}
		seen[event.ID] = true
		if event.Sequence <= after {
			continue
		}
		taskID := payloadString(event.Payload, "taskId")
		reason := payloadString(event.Payload, "reason")
		if reason == "" {
			reason = payloadString(event.Payload, "stopReason")
		}
		if reason == "" {
			reason = payloadString(event.Payload, "error")
		}
		rows = append(rows, TaskNotification{
			EventID: event.ID, Sequence: event.Sequence, RunID: event.RunID,
			TaskID: taskID, WorkItemID: facts.workItems[taskEventKey(event.RunID, taskID)],
			Type: event.Type, Status: status, Reason: reason,
		})
		if len(rows) == limit {
			cursor = event.Sequence
			break
		}
	}
	return rows, cursor, nil
}
