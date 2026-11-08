package team

import (
	"errors"
	"sync"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/google/uuid"
)

func taskUpdateRunFixture(targetStatus string) *Run {
	return &Run{ID: "run-update", TeamID: "team-1", Goal: "原任务", Mode: ModeSingle,
		Status: RunDispatching, Round: 1, Tasks: []Task{{
			ID: "target", MemberID: "dev", Prompt: "原正文", Status: targetStatus,
			WriteLock: TaskWriteLockNone,
		}}}
}

func saveTaskUpdateRun(t *testing.T, h *actionsExecHarness, run *Run) {
	t.Helper()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.service.Cancel(run.ID) })
}

func TestTaskUpdateEnqueueDependsOnOriginalAndReplays(t *testing.T) {
	h := newActionsExecHarness(t)
	run := taskUpdateRunFixture(TaskRunning)
	saveTaskUpdateRun(t, h, run)
	opID := uuid.NewString()
	result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "enqueue", "补充验收", run.Version)
	if err != nil || result.Status != TaskUpdateAccepted || result.Task.Status != TaskPlanned ||
		result.Task.UpdateOf != "target" || result.Task.UpdateStrategy != "enqueue" ||
		len(result.Task.DependsOn) != 1 || result.Task.DependsOn[0] != "target" {
		t.Fatalf("enqueue 受理异常: %+v err=%v", result, err)
	}
	if findTaskIn(h.store.Run(run.ID), "target").Status != TaskRunning {
		t.Fatal("enqueue 不得停止原任务")
	}
	replay, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "enqueue", "补充验收", 999)
	if err != nil || !replay.Duplicate || replay.Task.ID != result.Task.ID {
		t.Fatalf("更新重放应绕过旧版本并幂等: %+v err=%v", replay, err)
	}
	if _, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "enqueue", "不同正文", run.Version); !errors.Is(err, ErrTaskUpdateConflict) {
		t.Fatalf("同操作异内容应冲突: %v", err)
	}
	if _, err := h.service.UpdateTask(h.team.ID, run.ID, "target", uuid.NewString(), "enqueue", "另一更新", 999); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("新操作应校验期望版本: %v", err)
	}
	if got := len(h.store.Run(run.ID).Tasks); got != 2 {
		t.Fatalf("重放不得重复建 Task: %d", got)
	}

	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := &Run{ID: run.ID, TeamID: h.team.ID}
	for _, event := range events {
		applyJournalEventToRun(rebuilt, event)
	}
	task := findTaskUpdate(rebuilt, opID)
	if task == nil || task.Prompt != "补充验收" || task.UpdateStatus != TaskUpdateAccepted ||
		len(task.DependsOn) != 1 || task.DependsOn[0] != "target" {
		t.Fatalf("journal 重建丢失更新协议: %+v", task)
	}
}

func TestTaskUpdateInterruptQueuedStopsBeforeSuccessor(t *testing.T) {
	h := newActionsExecHarness(t)
	run := taskUpdateRunFixture(TaskRunning)
	run.Tasks[0].SubmissionID = uuid.NewString()
	saveTaskUpdateRun(t, h, run)
	if _, _, err := h.queue.Enqueue("s-dev", run.Tasks[0].SubmissionID, "原任务"); err != nil {
		t.Fatal(err)
	}
	h.service.interruptAgentTurn = func(session *acpruntime.Session, sentText string) error {
		t.Fatal("队列任务已确认撤销，不应再取消会话")
		return nil
	}
	result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", uuid.NewString(), "interrupt", "改变实现方向", run.Version)
	if err != nil || result.Status != TaskUpdateAccepted || result.Task.UpdateStatus != TaskUpdateAccepted {
		t.Fatalf("确认撤销后应接受后继任务: %+v err=%v", result, err)
	}
	sub, _ := h.queue.Submission("s-dev", run.Tasks[0].SubmissionID)
	if sub.Status != SubStatusCancelled || findTaskIn(h.store.Run(run.ID), "target").Status != TaskStopped {
		t.Fatalf("原任务与队列都应停止: sub=%+v run=%+v", sub, h.store.Run(run.ID))
	}
}

func TestTaskUpdateInterruptUnknownNeverDispatchesSuccessor(t *testing.T) {
	h := newActionsExecHarness(t)
	run := taskUpdateRunFixture(TaskRunning)
	run.Tasks[0].SubmissionID = uuid.NewString()
	saveTaskUpdateRun(t, h, run)
	if _, _, err := h.queue.Enqueue("s-dev", run.Tasks[0].SubmissionID, "原任务"); err != nil {
		t.Fatal(err)
	}
	h.queue.setStatus("s-dev", run.Tasks[0].SubmissionID, SubStatusUnknown, "未知")
	opID := uuid.NewString()
	result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "interrupt", "不要自动重发", run.Version)
	if err != nil || result.Status != TaskUpdateOutcomeUnknown || result.Task.Status != TaskOutcomeUnknown {
		t.Fatalf("停止未知应显式返回 outcome_unknown: %+v err=%v", result, err)
	}
	fresh := h.store.Run(run.ID)
	if findTaskIn(fresh, "target").Status != TaskRunning || fresh.Status != RunWaitingInput {
		t.Fatalf("未知不得停止事实或继续运行: %+v", fresh)
	}
	replay, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "interrupt", "不要自动重发", 999)
	if err != nil || !replay.Duplicate || replay.Status != TaskUpdateOutcomeUnknown || len(h.store.Run(run.ID).Tasks) != 2 {
		t.Fatalf("未知重放不得重发: %+v err=%v", replay, err)
	}
}

func TestTaskUpdateInterruptAdapterFailureAndConfirmedStop(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		stopErr    error
		wantStatus string
	}{
		{"停止超时", errors.New("timeout"), TaskUpdateOutcomeUnknown},
		{"适配器确认停止", nil, TaskUpdateAccepted},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newActionsExecHarness(t)
			run := taskUpdateRunFixture(TaskRunning)
			run.Tasks[0].SubmissionID = uuid.NewString()
			saveTaskUpdateRun(t, h, run)
			if _, _, err := h.queue.Enqueue("s-dev", run.Tasks[0].SubmissionID, "原任务"); err != nil {
				t.Fatal(err)
			}
			h.queue.setStatus("s-dev", run.Tasks[0].SubmissionID, SubStatusAccepted, "")
			h.service.interruptAgentTurn = func(session *acpruntime.Session, sentText string) error { return testCase.stopErr }
			result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", uuid.NewString(), "interrupt", "更新任务", run.Version)
			if err != nil || result.Status != testCase.wantStatus {
				t.Fatalf("停止结果异常: %+v err=%v", result, err)
			}
			if testCase.stopErr != nil && (h.store.Run(run.ID).Status != RunWaitingInput ||
				findTaskIn(h.store.Run(run.ID), "target").Status != TaskRunning) {
				t.Fatalf("失败/超时不得派发后继: %+v", h.store.Run(run.ID))
			}
			if testCase.stopErr == nil && findTaskIn(h.store.Run(run.ID), "target").Status != TaskStopped {
				t.Fatalf("确认停止后原任务应停止: %+v", h.store.Run(run.ID))
			}
		})
	}
}

func TestTaskUpdatePendingIntentAfterRestartParksWithoutResend(t *testing.T) {
	h := newActionsExecHarness(t)
	run := taskUpdateRunFixture(TaskRunning)
	opID := uuid.NewString()
	run.Tasks = append(run.Tasks, Task{ID: "update-" + opID, MemberID: "dev", Prompt: "更新",
		Status: TaskUpdatePending, DependsOn: []string{"target"}, UpdateID: opID,
		UpdateOf: "target", UpdateStrategy: "interrupt", UpdateStatus: TaskUpdateInterruptPending})
	saveTaskUpdateRun(t, h, run)
	result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "interrupt", "更新", run.Version)
	if err != nil || result.Status != TaskUpdateOutcomeUnknown || h.store.Run(run.ID).Status != RunWaitingInput {
		t.Fatalf("崩溃遗留中断意图只能停靠为未知: %+v err=%v", result, err)
	}
}

func TestTaskUpdateConcurrentReplayIsIdempotent(t *testing.T) {
	h := newActionsExecHarness(t)
	run := taskUpdateRunFixture(TaskRunning)
	saveTaskUpdateRun(t, h, run)
	opID := uuid.NewString()
	const callers = 8
	var wg sync.WaitGroup
	results := make(chan *TaskUpdateResult, callers)
	errs := make(chan error, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := h.service.UpdateTask(h.team.ID, run.ID, "target", opID, "enqueue", "并发补充", run.Version)
			if err != nil {
				errs <- err
			} else {
				results <- result
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("并发同操作不应报错: %v", err)
	}
	accepted := 0
	for result := range results {
		if !result.Duplicate {
			accepted++
		}
	}
	if accepted != 1 || len(h.store.Run(run.ID).Tasks) != 2 {
		t.Fatalf("并发重放应只有一次落成: accepted=%d tasks=%d", accepted, len(h.store.Run(run.ID).Tasks))
	}
}
