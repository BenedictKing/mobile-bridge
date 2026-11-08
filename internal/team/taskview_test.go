package team

import (
	"errors"
	"strings"
	"testing"
)

func saveTaskViewRun(t *testing.T, h *actionsExecHarness, runID, taskID, status string) {
	t.Helper()
	runStatus := RunDispatching
	if terminalTask(status) {
		runStatus = RunCompleted
	}
	if err := h.store.SaveRun(&Run{ID: runID, TeamID: h.team.ID, Goal: "任务投影", Mode: ModeSingle,
		Status: runStatus, Round: 1, Tasks: []Task{{ID: taskID, MemberID: "dev", Status: status}}}); err != nil {
		t.Fatal(err)
	}
}

func appendTaskViewEvent(t *testing.T, h *actionsExecHarness, eventType, runID, taskID string, payload map[string]any) *Event {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	if taskID != "" {
		payload["taskId"] = taskID
	}
	event := newEvent(runID, eventType, "test", payload)
	if err := h.journal.Append(event); err != nil {
		t.Fatal(err)
	}
	return event
}

func TestTaskViewListCheckAndWorkItem(t *testing.T) {
	h := newRouterHarness(t)
	saveTaskViewRun(t, h, "run-live", "task-live", TaskRunning)
	saveTaskViewRun(t, h, "run-done", "task-done", TaskCompleted)
	if _, _, err := h.service.inputs.Accept(h.team.ID, "input-view", "需求", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	if err := h.service.inputs.MarkRoutingIntent(h.team.ID, "input-view", "follow_up", "run-live", "task-live", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.service.inputs.MarkRouted(h.team.ID, "input-view", "follow_up", "run-live", "task-live"); err != nil {
		t.Fatal(err)
	}
	event := appendTaskViewEvent(t, h, EventTaskRunning, "run-live", "task-live", nil)
	rows, err := h.service.ListTasks(h.team.ID, true)
	if err != nil || len(rows) != 1 {
		t.Fatalf("在途列表异常: %+v err=%v", rows, err)
	}
	if rows[0].TaskID != "task-live" || rows[0].RunID != "run-live" || rows[0].WorkItemID != "wi-input-view" ||
		rows[0].LatestEventSequence != event.Sequence {
		t.Fatalf("任务投影缺少血缘或事件序号: %+v", rows[0])
	}
	briefing := h.service.backgroundTaskBriefing(&Run{ID: "run-lead", TeamID: h.team.ID}, "lead")
	if !strings.Contains(briefing, "taskId=task-live") || !strings.Contains(briefing, "workItemId=wi-input-view") ||
		!strings.Contains(briefing, "latestEventSequence=") {
		t.Fatalf("Lead 应取得有界后台任务摘要: %q", briefing)
	}
	all, err := h.service.ListTasks(h.team.ID, false)
	if err != nil || len(all) != 2 {
		t.Fatalf("全量列表异常: %+v err=%v", all, err)
	}
	checked, err := h.service.CheckTask(h.team.ID, "run-live", "task-live")
	if err != nil || checked.Status != TaskRunning {
		t.Fatalf("check 异常: %+v err=%v", checked, err)
	}
	if _, err := h.service.CheckTask(h.team.ID, "run-done", "task-live"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("任务不得跨 Run 串读: %v", err)
	}
}

func TestTaskNotificationsCursorDeduplicatesAndReplays(t *testing.T) {
	h := newRouterHarness(t)
	saveTaskViewRun(t, h, "run-events", "task-events", TaskRunning)
	completed := appendTaskViewEvent(t, h, EventTaskCompleted, "run-events", "task-events", nil)
	approval := appendTaskViewEvent(t, h, EventQuestionOpened, "run-events", "task-events", map[string]any{"kind": "approval"})
	duplicate := *completed
	if err := h.journal.Append(&duplicate); err != nil {
		t.Fatal(err)
	}
	waiting := appendTaskViewEvent(t, h, EventRunWaitingInput, "run-events", "", map[string]any{"reason": "等待用户"})

	rows, cursor, err := h.service.TaskNotifications(h.team.ID, 0, 2)
	if err != nil || len(rows) != 2 || rows[0].EventID != completed.ID || rows[1].EventID != approval.ID {
		t.Fatalf("第一页通知异常: %+v cursor=%d err=%v", rows, cursor, err)
	}
	if cursor != approval.Sequence || rows[1].Status != "waiting_approval" {
		t.Fatalf("审批通知与游标异常: %+v cursor=%d", rows, cursor)
	}
	rows, cursor, err = h.service.TaskNotifications(h.team.ID, cursor, 50)
	if err != nil || len(rows) != 1 || rows[0].EventID != waiting.ID || cursor != waiting.Sequence {
		t.Fatalf("第二页应跳过重复事件并补读等待输入: %+v cursor=%d err=%v", rows, cursor, err)
	}
	rows, cursor, err = h.service.TaskNotifications(h.team.ID, cursor, 50)
	if err != nil || len(rows) != 0 {
		t.Fatalf("相同游标不得重复投递: %+v cursor=%d err=%v", rows, cursor, err)
	}

	reopened, err := OpenJournal(h.journal.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	h.service.journal = reopened
	rows, cursor, err = h.service.TaskNotifications(h.team.ID, completed.Sequence, 50)
	if err != nil || len(rows) != 2 || rows[0].EventID != approval.ID || rows[1].EventID != waiting.ID {
		t.Fatalf("重启后应按游标补读且按事件 ID 去重: %+v cursor=%d err=%v", rows, cursor, err)
	}
}

func TestTaskNotificationsFailureAndUnknownRemainVisible(t *testing.T) {
	h := newRouterHarness(t)
	saveTaskViewRun(t, h, "run-outcomes", "task-outcomes", TaskRunning)
	appendTaskViewEvent(t, h, EventTaskFailed, "run-outcomes", "task-outcomes", map[string]any{"error": "失败原因"})
	appendTaskViewEvent(t, h, EventTaskOutcomeUnknown, "run-outcomes", "task-outcomes", nil)
	appendTaskViewEvent(t, h, EventTaskStopped, "run-outcomes", "task-outcomes", map[string]any{"status": TaskCancelled})
	rows, _, err := h.service.TaskNotifications(h.team.ID, 0, 50)
	if err != nil || len(rows) != 3 || rows[0].Status != TaskFailed || rows[0].Reason != "失败原因" ||
		rows[1].Status != TaskOutcomeUnknown || rows[2].Status != TaskCancelled {
		t.Fatalf("失败、未知与取消通知必须保留: %+v err=%v", rows, err)
	}
}
