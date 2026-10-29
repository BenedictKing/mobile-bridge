package team

import (
	"log"
	"strconv"
	"strings"
)

const autoReductionPrompt = "请汇总本次团队执行成果，形成面向用户的最终交付报告：先给结论，再列出目标完成情况、文件改动、验证结果和遗留问题。不要创建新任务或输出 team-actions。"

// ensureAutoPlanReductionLocked 在 autoPlan 成功终局前强制补一次 Lead 收尾。
// 返回 true 表示本轮已补任务或记账失败需要重试，调用方不得按旧快照 fold。
func (s *Service) ensureAutoPlanReductionLocked(state *runState, run *Run) bool {
	team := s.store.Team(run.TeamID)
	if !run.AutoPlan || team == nil || team.LeadID == "" || hasReductionTask(run.Tasks) {
		return false
	}
	tasks := make([]Task, 0, len(run.Tasks))
	workerCompleted := false
	for _, task := range run.Tasks {
		if task.Reduction {
			continue
		}
		tasks = append(tasks, task)
		if task.MemberID != team.LeadID && task.Status == TaskCompleted {
			workerCompleted = true
		}
	}
	if !workerCompleted {
		return false
	}
	if status, _ := FoldRunStatus(tasks, run.Mode, run.CollectPolicy); status != RunCompleted {
		return false
	}

	policy := policyOf(team)
	task := buildAutoReductionTask(run, team)
	events := []*Event{newEvent(run.ID, EventTaskPlanned, "reducer", map[string]any{
		"taskId": task.ID, "memberId": task.MemberID, "instructionsHead": task.InstructionsHead,
		"prompt": task.Prompt, "dependsOn": task.DependsOn, "reduction": true,
	})}
	if len(run.Tasks) >= policy.MaxTasksPerRun {
		task.Status = TaskFailed
		task.Error = "任务数已达上限，无法创建最终收尾任务"
		events = append(events, newEvent(run.ID, EventTaskFailed, "reducer", map[string]any{
			"taskId": task.ID, "error": task.Error,
		}))
	}
	taskID := task.ID
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) || hasReductionTask(fresh.Tasks) {
			return nil, false
		}
		fresh.Tasks = append(fresh.Tasks, task)
		for _, event := range events {
			event.IdempotencyKey = "team:" + fresh.TeamID + ":run:" + fresh.ID + ":reduction:" + taskID + ":" + event.Type
		}
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 最终收尾任务记账失败，下轮重试: %v", run.ID, err)
	}
	return true
}

func isAutoReductionTask(task *Task) bool {
	return task != nil && task.Reduction && strings.HasPrefix(task.ID, "lead-auto-reduce")
}

func hasReductionTask(tasks []Task) bool {
	for _, task := range tasks {
		if task.Reduction {
			return true
		}
	}
	return false
}

func buildAutoReductionTask(run *Run, team *Team) Task {
	depends := make([]string, 0, len(run.Tasks))
	ordinal := 0
	for _, task := range run.Tasks {
		if task.Status == TaskCompleted {
			depends = append(depends, task.ID)
		}
		if task.Ordinal > ordinal {
			ordinal = task.Ordinal
		}
	}
	id := "lead-auto-reduce"
	for index := 2; findTaskIn(run, id) != nil; index++ {
		id = "lead-auto-reduce-" + strconv.Itoa(index)
	}
	task := Task{
		ID: id, MemberID: team.LeadID, Prompt: autoReductionPrompt, Ordinal: ordinal + 1,
		DependsOn: depends, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
		Status: TaskPlanned, InstructionsHead: InstructionsHead(autoReductionPrompt), Reduction: true,
	}
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	return task
}
