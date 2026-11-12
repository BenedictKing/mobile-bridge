package team

import (
	"fmt"
	"strings"
)

const (
	TaskPhasePlan      = "plan"
	TaskPhaseImplement = "implement"
)

func NormalizeTaskPhase(value string) string {
	return truncateRunes(strings.TrimSpace(value), 64)
}

func phasePlanGateViolation(policy TeamPolicy, task *Task, tasks map[string]*Task) string {
	if task == nil || policy.AllowImplementationWithoutPlan ||
		task.Phase != TaskPhaseImplement || task.WriteLock == TaskWriteLockNone {
		return ""
	}
	for _, dependency := range task.DependsOn {
		if planned := tasks[dependency]; planned != nil && planned.Phase == TaskPhasePlan {
			return ""
		}
	}
	return fmt.Sprintf("实现任务 %s 必须依赖一个 phase=plan 的计划任务", task.ID)
}

func validateActionPlanGates(team *Team, run *Run, actions []Action, batchKeys map[string]string, policy TeamPolicy) []string {
	tasks := map[string]*Task{}
	for i := range run.Tasks {
		tasks[run.Tasks[i].ID] = &run.Tasks[i]
	}
	for i := range actions {
		task := buildActionTaskWithKeys(run, team, &actions[i], batchKeys)
		tasks[task.ID] = &task
	}
	var violations []string
	for i := range actions {
		task := buildActionTaskWithKeys(run, team, &actions[i], batchKeys)
		if violation := phasePlanGateViolation(policy, &task, tasks); violation != "" {
			violations = append(violations, violation)
		}
	}
	return violations
}

func validatePlanPhaseGate(team *Team, tasks []Task) error {
	byID := make(map[string]*Task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	for i := range tasks {
		if violation := phasePlanGateViolation(team.Policy, &tasks[i], byID); violation != "" {
			return fmt.Errorf("%s", violation)
		}
	}
	return nil
}
