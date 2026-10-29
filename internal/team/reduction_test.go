package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reductionRunFixture() *Run {
	run := execRunFixture()
	run.Tasks = append(run.Tasks, Task{
		ID: "dev-1", MemberID: "dev", Status: TaskCompleted, Ordinal: 1,
		Result: "接口与回归测试已完成", ResultDetail: &TaskResult{Result: "接口与回归测试已完成"},
	})
	return run
}

func TestAutoPlanReductionIsMandatoryAndScoped(t *testing.T) {
	h := newActionsExecHarness(t)
	run := reductionRunFixture()
	state := h.saveExecRun(t, run)
	state.mu.Lock()
	created := h.service.ensureAutoPlanReductionLocked(state, run)
	state.mu.Unlock()
	if !created {
		t.Fatal("Worker 完成后应补 Lead Reduce")
	}
	fresh := h.store.Run(run.ID)
	reduction := findTaskIn(fresh, "lead-auto-reduce")
	if reduction == nil || !reduction.Reduction || reduction.MemberID != "lead" || reduction.WriteLock != TaskWriteLockNone {
		t.Fatalf("Reduce 任务契约不正确: %+v", reduction)
	}
	if len(reduction.DependsOn) != 2 {
		t.Fatalf("Reduce 应依赖 Lead 与 Worker 结果: %+v", reduction.DependsOn)
	}
	context := BuildTaskContext(fresh, reduction, h.team.Policy)
	if !strings.Contains(context.Text, "接口与回归测试已完成") {
		t.Fatalf("Reduce 未注入 Worker 结果: %s", context.Text)
	}
	state.mu.Lock()
	again := h.service.ensureAutoPlanReductionLocked(state, fresh)
	state.mu.Unlock()
	if again || len(h.store.Run(run.ID).Tasks) != 3 {
		t.Fatal("已有 Reduce 后不得重复创建")
	}
}

func TestAutoPlanReductionSkipsLeadOnlyAndFailure(t *testing.T) {
	h := newActionsExecHarness(t)
	leadOnly := execRunFixture()
	state := h.saveExecRun(t, leadOnly)
	state.mu.Lock()
	created := h.service.ensureAutoPlanReductionLocked(state, leadOnly)
	state.mu.Unlock()
	if created {
		t.Fatal("纯 Lead 单任务不需要强制 Reduce")
	}

	failed := reductionRunFixture()
	failed.ID = "run-failed-worker"
	failed.Tasks[1].Status = TaskFailed
	failedState := h.saveExecRun(t, failed)
	failedState.mu.Lock()
	created = h.service.ensureAutoPlanReductionLocked(failedState, failed)
	failedState.mu.Unlock()
	if created {
		t.Fatal("Worker 失败不得补成功收尾")
	}
}

func TestAutoPlanReductionTaskLimitFailsDeterministically(t *testing.T) {
	h := newActionsExecHarness(t)
	h.team.Policy.MaxTasksPerRun = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := reductionRunFixture()
	state := h.saveExecRun(t, run)
	state.mu.Lock()
	created := h.service.ensureAutoPlanReductionLocked(state, run)
	state.mu.Unlock()
	if !created {
		t.Fatal("任务满额也应落确定性收尾结果")
	}
	reduction := findTaskIn(h.store.Run(run.ID), "lead-auto-reduce")
	if reduction == nil || reduction.Status != TaskFailed || !strings.Contains(reduction.Error, "任务数已达上限") {
		t.Fatalf("任务满额应明确失败: %+v", reduction)
	}
}

func TestAutoPlanReductionSurvivesJournalRebuild(t *testing.T) {
	h := newActionsExecHarness(t)
	run := reductionRunFixture()
	state := h.saveExecRun(t, run)
	if err := h.journal.Append(newEvent(run.ID, EventRunCreated, "user", map[string]any{
		"teamId": run.TeamID, "goal": run.Goal, "mode": run.Mode, "autoPlan": true, "taskCount": 2,
	})); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	h.service.ensureAutoPlanReductionLocked(state, run)
	state.mu.Unlock()
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	teamsPath, runsPath := filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json")
	seedTeam := h.team.Clone()
	seedTeam.Version = 0
	if err := LoadStore(teamsPath, runsPath).SaveTeam(seedTeam); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := LoadStore(teamsPath, runsPath)
	fresh.RebuildRunsFromJournal(events)
	reduction := findTaskIn(fresh.Run(run.ID), "lead-auto-reduce")
	if reduction == nil || !reduction.Reduction || reduction.Prompt != autoReductionPrompt || len(reduction.DependsOn) != 2 {
		t.Fatalf("Journal 重建丢失 Reduce 契约: %+v", reduction)
	}
}
