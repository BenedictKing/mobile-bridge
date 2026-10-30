package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFailureTriageCreatesLeadTaskBeforeFold(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture()
	run.Tasks = append(run.Tasks, Task{
		ID: "dev-task", MemberID: "dev", Prompt: "实现接口", Error: "Session not found",
		Status: TaskFailed, Ordinal: 1,
	})
	state := h.saveExecRun(t, run)
	if !h.service.passFold(state) {
		t.Fatal("补分诊任务后调度循环应继续")
	}
	fresh := h.store.Run(run.ID)
	if fresh.Status == RunFailed {
		t.Fatal("尚有分诊机会时不得直接 fold 为 failed")
	}
	triage := findTaskIn(fresh, "lead-triage-1")
	if triage == nil || triage.MemberID != "lead" || triage.TriageFor != "dev-task" || triage.WriteLock != TaskWriteLockNone {
		t.Fatalf("分诊任务契约不正确: %+v", triage)
	}
	for _, want := range []string{"dev-task", "dev", "Session not found", "id=rev", "delegate"} {
		if !strings.Contains(triage.Prompt, want) {
			t.Fatalf("分诊提示缺少 %q:\n%s", want, triage.Prompt)
		}
	}
	if strings.Contains(triage.Prompt, "id=ghost") {
		t.Fatal("停用成员不得进入候选列表")
	}
	h.service.passFold(state)
	if countFailureTriages(h.store.Run(run.ID)) != 1 {
		t.Fatal("分诊任务不得重复创建")
	}
}

func TestFailureTriageRespectsGuards(t *testing.T) {
	h := newActionsExecHarness(t)
	ensure := func(run *Run) bool {
		state := h.saveExecRun(t, run)
		state.mu.Lock()
		defer state.mu.Unlock()
		return h.service.ensureFailureTriageLocked(state, run)
	}
	leadFailure := execRunFixture()
	leadFailure.Tasks[0].Status = TaskFailed
	if ensure(leadFailure) {
		t.Fatal("Lead 自身失败不得继续分诊")
	}

	manual := execRunFixture()
	manual.ID = "run-manual"
	manual.Tasks = append(manual.Tasks, Task{ID: "dev-task", MemberID: "dev", Status: TaskFailed})
	h.team.Policy.AutoDispatch = false
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	if ensure(manual) {
		t.Fatal("autoDispatch=false 时不得自动改派")
	}
	h.team.Policy.AutoDispatch = true
	h.team.Members = h.team.Members[1:2]
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	noCandidate := execRunFixture()
	noCandidate.ID = "run-no-candidate"
	noCandidate.Tasks = append(noCandidate.Tasks, Task{ID: "dev-task", MemberID: "dev", Status: TaskFailed})
	if ensure(noCandidate) {
		t.Fatal("没有其他启用成员时不得分诊")
	}

	h.team.Members = testTeam().Members
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	exhausted := execRunFixture()
	exhausted.ID = "run-exhausted"
	exhausted.Tasks = append(exhausted.Tasks,
		Task{ID: "dev-task", MemberID: "dev", Status: TaskFailed},
		Task{ID: "lead-triage-1", MemberID: "lead", Status: TaskCompleted, TriageFor: "dev-task"},
		Task{ID: "lead-triage-2", MemberID: "lead", Status: TaskCompleted, TriageFor: "dev-task"})
	if ensure(exhausted) {
		t.Fatal("两轮分诊耗尽后必须允许 run_failed")
	}
}

func TestRetriageRearmsOriginalTaskAndDependencyBranch(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture()
	run.Tasks = append(run.Tasks,
		Task{ID: "dev-task", MemberID: "dev", Prompt: "旧说明", Status: TaskFailed,
			Attempt: 1, Ordinal: 1, DependsOn: []string{"lead"}, Error: "broken"},
		Task{ID: "review", MemberID: "rev", Status: TaskStopped, Ordinal: 2,
			DependsOn: []string{"dev-task"}, StopReason: "依赖分支已终止，任务不再执行"},
		Task{ID: "lead-triage-1", MemberID: "lead", Status: TaskCompleted, Ordinal: 3, TriageFor: "dev-task"})
	action := Action{Type: ActionTypeDelegate, SourceTaskID: "lead-triage-1", Round: 2,
		Member: "rev", Task: "换成员重做", ActionKey: ActionKey(run.TeamID, run.ID, "lead-triage-1", 2, 0), Status: ActionStatusProposed}
	run.PendingActions = []Action{action}
	state := h.saveExecRun(t, run)
	h.service.executePlannedAction(state, h.team, h.team.Policy, &action, nil)

	fresh := h.store.Run(run.ID)
	if len(fresh.Tasks) != len(run.Tasks) {
		t.Fatal("改派必须复用原任务，不得追加替代节点")
	}
	original := findTaskIn(fresh, "dev-task")
	if original.Status != TaskReady || original.MemberID != "rev" || original.Prompt != "换成员重做" ||
		original.Attempt != 2 || original.Error != "" || len(original.DependsOn) != 1 || original.DependsOn[0] != "lead" {
		t.Fatalf("原任务改派状态不正确: %+v", original)
	}
	if downstream := findTaskIn(fresh, "review"); downstream.Status != TaskReady || downstream.StopReason != "" {
		t.Fatalf("依赖分支应恢复: %+v", downstream)
	}
	if got := len(h.eventsOf(t, EventTaskRetryScheduled)); got != 2 {
		t.Fatalf("原任务与下游应各有重臂事实，实际 %d", got)
	}
	if facts := h.service.actionFacts(run.ID); facts.settled[action.ActionKey] != actionVerdictExecuted {
		t.Fatal("重臂事件必须证明改派动作已执行，防止崩溃后重复改派")
	}
	original.Status = TaskCompleted
	findTaskIn(fresh, "review").Status = TaskCompleted
	if status, _ := FoldRunStatus(fresh.Tasks, fresh.Mode, fresh.CollectPolicy); status != RunCompleted {
		t.Fatalf("改派成功后应能正常完成，实际 %s", status)
	}
}

func TestRetriageJournalRestoresAssignmentAfterCrash(t *testing.T) {
	run := execRunFixture()
	run.Tasks = append(run.Tasks,
		Task{ID: "dev-task", MemberID: "dev", Prompt: "旧说明", Status: TaskFailed, Attempt: 1},
		Task{ID: "review", MemberID: "rev", Status: TaskStopped,
			DependsOn: []string{"dev-task"}, StopReason: "依赖分支已终止，任务不再执行"},
		Task{ID: "lead-triage-1", MemberID: "lead", Status: TaskCompleted, TriageFor: "dev-task"})
	action := Action{Type: ActionTypeDelegate, SourceTaskID: "lead-triage-1", Round: 2,
		Member: "rev", Task: "换成员重做", ActionKey: ActionKey(run.TeamID, run.ID, "lead-triage-1", 2, 0)}
	broken := run.Clone()
	replay := run.Clone()
	events := applyRetriage(run, findTaskIn(run, "dev-task"), &action)

	transitions := ReconcileRun(broken, events, newFakeProbe())
	for _, taskID := range []string{"dev-task", "review"} {
		transition, ok := findTransition(transitions, TransitionTaskStatus, taskID)
		if !ok || transition.Status != TaskReady || transition.Retry == nil {
			t.Fatalf("崩溃恢复缺少 %s 的重臂迁移: %+v", taskID, transitions)
		}
		applyTransitions(broken, []Transition{transition}, "test")
	}
	for _, taskID := range []string{"dev-task", "review"} {
		if task := findTaskIn(broken, taskID); task.Status != TaskReady {
			t.Fatalf("恢复快照后 %s 应为 ready: %+v", taskID, task)
		}
	}
	if task := findTaskIn(broken, "dev-task"); task.MemberID != "rev" || task.Prompt != "换成员重做" || task.Attempt != 2 {
		t.Fatalf("崩溃恢复丢失改派内容: %+v", task)
	}
	for _, event := range events {
		applyJournalEventToRun(replay, event)
	}
	if task := findTaskIn(replay, "dev-task"); task.Status != TaskReady || task.MemberID != "rev" || task.Prompt != "换成员重做" {
		t.Fatalf("journal 重建丢失改派内容: %+v", task)
	}
	if task := findTaskIn(replay, "review"); task.Status != TaskReady || task.StopReason != "" {
		t.Fatalf("journal 重建丢失下游恢复: %+v", task)
	}
}

func TestLeadFollowUpDoesNotDependOnFailedBranches(t *testing.T) {
	run := execRunFixture()
	run.Tasks = append(run.Tasks,
		Task{ID: "failed", MemberID: "dev", Status: TaskFailed},
		Task{ID: "stopped", MemberID: "dev", Status: TaskStopped},
		Task{ID: "pending", MemberID: "dev", Status: TaskPlanned})
	action := Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "lead", Task: "收尾", ActionKey: ActionKey(run.TeamID, run.ID, "lead", 1, 0)}
	task := buildActionTask(run, testTeam(), &action)
	if strings.Join(task.DependsOn, ",") != "lead,pending" {
		t.Fatalf("Lead 后续任务只应等待可执行分支: %+v", task.DependsOn)
	}
}

func TestTriageMetadataPersists(t *testing.T) {
	original := Task{ID: "lead-triage-1", MemberID: "lead", TriageFor: "dev-task"}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Task
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TriageFor != "dev-task" {
		t.Fatalf("triageFor 未持久化: %+v", decoded)
	}
}

func TestFailureTriageDoesNotRetriageAuthBlockedRun(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture()
	run.Status = RunWaitingInput
	run.Tasks = append(run.Tasks, Task{
		ID: "dev-task", MemberID: "dev", Status: TaskStopped,
		Outcome: OutcomeAuth, StopReason: "认证失效，请重新登录",
	})
	state := h.saveExecRun(t, run)
	h.service.passFold(state)
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunWaitingInput || countFailureTriages(fresh) != 0 {
		t.Fatalf("认证停靠必须等待用户，不得自动改派: %+v", fresh)
	}
}
