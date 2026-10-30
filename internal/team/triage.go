package team

import (
	"fmt"
	"log"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const maxFailureTriages = 2

// ensureFailureTriageLocked 在确定性失败终局前补一次 Lead 分诊。
// 返回 true 表示已补任务或记账需重试，调用方不得按旧快照 fold。
func (s *Service) ensureFailureTriageLocked(state *runState, run *Run) bool {
	team := s.store.Team(run.TeamID)
	if !run.AutoPlan || team == nil || team.LeadID == "" || !policyOf(team).AutoDispatch {
		return false
	}
	failed := failureTriageCandidate(run, team)
	if failed == nil || countFailureTriages(run) >= maxFailureTriages {
		return false
	}
	if len(run.Tasks) >= policyOf(team).MaxTasksPerRun || len(triageMembers(team, failed.MemberID)) == 0 {
		return false
	}
	task := buildFailureTriageTask(run, team, failed)
	taskID := task.ID
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) || findTaskIn(fresh, taskID) != nil {
			return nil, false
		}
		fresh.Tasks = append(fresh.Tasks, task)
		event := newEvent(fresh.ID, EventTaskPlanned, "triage", map[string]any{
			"taskId": task.ID, "memberId": task.MemberID, "instructionsHead": task.InstructionsHead,
			"prompt": task.Prompt, "triageFor": task.TriageFor,
		})
		event.IdempotencyKey = "team:" + fresh.TeamID + ":run:" + fresh.ID + ":triage:" + taskID
		return []*Event{event}, true
	})
	if err != nil {
		log.Printf("team run %s 失败分诊任务记账失败，下轮重试: %v", run.ID, err)
	}
	return true
}

func failureTriageCandidate(run *Run, team *Team) *Task {
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if task.Status == TaskFailed && task.MemberID != team.LeadID && task.TriageFor == "" {
			return findTaskIn(run, task.ID)
		}
	}
	return nil
}

func countFailureTriages(run *Run) int {
	count := 0
	for _, task := range run.Tasks {
		if task.TriageFor != "" {
			count++
		}
	}
	return count
}

func buildFailureTriageTask(run *Run, team *Team, failed *Task) Task {
	ordinal := 0
	for _, task := range run.Tasks {
		if task.Ordinal > ordinal {
			ordinal = task.Ordinal
		}
	}
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "团队成员任务失败，请进行第 %d 次失败分诊（最多 %d 次）。\n", countFailureTriages(run)+1, maxFailureTriages)
	fmt.Fprintf(&prompt, "原任务 ID：%s\n失败成员：%s\n错误摘要：%s\n原任务说明：%s\n",
		failed.ID, failed.MemberID, truncateRunes(failed.Error, 1000), truncateRunes(failed.Prompt, 1000))
	prompt.WriteString("当前可接手成员：\n")
	for _, member := range triageMembers(team, failed.MemberID) {
		name := strings.Join(strings.Fields(member.Name), " ")
		if name == "" {
			name = member.ID
		}
		fmt.Fprintf(&prompt, "- id=%s · name=%s · agent=%s\n", member.ID, truncateRunes(name, 120), member.AgentKind)
	}
	fmt.Fprintf(&prompt, "请只输出一个 delegate 动作：sourceTaskId 使用本分诊任务 ID %q，member 从上述成员中选择，task 写明接手后要重做的内容。", "lead-triage-"+fmt.Sprint(countFailureTriages(run)+1))
	prompt.WriteString("不要输出 summarize，不要把失败成员当作唯一候选；改派会保留原任务 ID 与依赖关系。")
	text := prompt.String()
	task := Task{
		ID: fmt.Sprintf("lead-triage-%d", countFailureTriages(run)+1), MemberID: team.LeadID,
		Prompt: text, Ordinal: ordinal + 1, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
		Status: TaskPlanned, InstructionsHead: InstructionsHead(text), TriageFor: failed.ID,
	}
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	return task
}

func triageMembers(team *Team, failedMemberID string) []TeamMember {
	members := []TeamMember{}
	for _, member := range team.Members {
		if member.Enabled && member.ID != failedMemberID {
			members = append(members, member)
		}
	}
	return members
}

// failureRetriageTarget 识别分诊 Lead 发出的改派动作及其原失败任务。
func failureRetriageTarget(run *Run, action *Action) (*Task, bool) {
	if action == nil || action.Type != ActionTypeDelegate {
		return nil, false
	}
	source := findTaskIn(run, action.SourceTaskID)
	if source == nil || source.TriageFor == "" {
		return nil, false
	}
	return findTaskIn(run, source.TriageFor), true
}

func applyRetriage(run *Run, target *Task, action *Action) []*Event {
	applyTaskRetryAssignment(target, newRetriageAssignment(target, action))
	events := []*Event{retriageActionEvent(target, action, run, true)}
	recovered := map[string]bool{target.ID: true}
	for _, ordered := range sortTasksByOrdinal(run.Tasks) {
		if ordered.Status != TaskStopped || ordered.StopReason != "依赖分支已终止，任务不再执行" {
			continue
		}
		blockedByBranch := false
		for _, dependency := range ordered.DependsOn {
			blockedByBranch = blockedByBranch || recovered[dependency]
		}
		if !blockedByBranch {
			continue
		}
		task := findTaskIn(run, ordered.ID)
		next := task.Attempt + 1
		if next < firstAttempt {
			next = firstAttempt
		}
		applyTaskRetryAssignment(task, taskRetryAssignment{attempt: next})
		recovered[task.ID] = true
		events = append(events, retriageActionEvent(task, action, run, false))
	}
	return events
}

func retriageActionEvent(task *Task, action *Action, run *Run, reassigned bool) *Event {
	payload := map[string]any{
		"taskId": task.ID, "attempt": jsoncompat.Num(task.Attempt), "outcome": "lead_triage",
		"round": jsoncompat.Num(action.Round), "actionKey": action.ActionKey,
	}
	if reassigned {
		payload["memberId"] = task.MemberID
		payload["prompt"] = task.Prompt
		payload["instructionsHead"] = task.InstructionsHead
		payload["writeLock"] = task.WriteLock
	}
	event := newEvent(run.ID, EventTaskRetryScheduled, "triage", payload)
	event.IdempotencyKey = actionExecutionKey(run.TeamID, run.ID, action.ActionKey)
	if !reassigned {
		event.IdempotencyKey += ":" + task.ID
	}
	return event
}

type taskRetryAssignment struct {
	attempt          int
	memberID         string
	prompt           string
	instructionsHead string
	writeLock        string
}

func newRetriageAssignment(task *Task, action *Action) taskRetryAssignment {
	next := task.Attempt + 1
	if next < firstAttempt {
		next = firstAttempt
	}
	writeLock := action.WriteLock
	if writeLock != TaskWriteLockExclusive && writeLock != TaskWriteLockNone {
		writeLock = task.WriteLock
	}
	prompt := ActionTaskPrompt(action)
	return taskRetryAssignment{
		attempt: next, memberID: action.Member, prompt: prompt,
		instructionsHead: InstructionsHead(prompt), writeLock: writeLock,
	}
}

func retryAssignmentFromEvent(event *Event) taskRetryAssignment {
	return taskRetryAssignment{
		attempt: payloadInt(event.Payload, "attempt"), memberID: payloadString(event.Payload, "memberId"),
		prompt: payloadString(event.Payload, "prompt"), instructionsHead: payloadString(event.Payload, "instructionsHead"),
		writeLock: payloadString(event.Payload, "writeLock"),
	}
}

func applyTaskRetryAssignment(task *Task, assignment taskRetryAssignment) {
	task.Status = TaskReady
	task.Attempt = assignment.attempt
	task.SubmissionID = ""
	task.InputHash = ""
	task.Result = ""
	task.ResultDetail = nil
	task.CachedFrom = ""
	task.Error = ""
	task.Outcome = ""
	task.StopReason = ""
	task.SentText = ""
	if assignment.memberID != "" {
		task.MemberID = assignment.memberID
		task.Prompt = assignment.prompt
		task.InstructionsHead = assignment.instructionsHead
		task.WriteLock = assignment.writeLock
	}
}

func taskRetryAssignmentApplied(task *Task, assignment taskRetryAssignment) bool {
	return task.Status == TaskReady && task.Attempt == assignment.attempt &&
		(assignment.memberID == "" || task.MemberID == assignment.memberID && task.Prompt == assignment.prompt)
}
