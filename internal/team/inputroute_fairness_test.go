package team

// inputroute_fairness_test.go 覆盖阶段 3（docs/team-demand-queue-plan.md §5）：
// 跨 Run 成员 lease（同一成员不双开）、活跃 Run 准入上限（超额 Run 等待
// 在飞收尾）、路由公平（3:1 配额 + 老化提升）与 Team 队列滚动派发预算。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTeamMemberLeaseAcrossRuns 同一成员跨 Run 最多一个在途任务：run-b 的
// 同成员任务派发必须晚于 run-a 在途任务的 completed 事实（事件序断言）。
func TestTeamMemberLeaseAcrossRuns(t *testing.T) {
	h := newDispatchHarness(t)
	runA := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "实现 A", WriteLock: TaskWriteLockNone},
	})
	runB := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "b", MemberID: "dev", Prompt: "实现 B", WriteLock: TaskWriteLockNone},
	})

	// 两个 Run 的循环竞速，先到者占住成员（a 或 b 都可能先派）。lease 的
	// 判据：首个任务在途期间 s-dev 恰有一条入账；后派任务的 task_dispatching
	// 晚于先派任务的 task_completed（journal 事件序，确定性）。
	waitFor(t, "首个任务入队", func() bool { return len(h.queue.entriesFor("s-dev")) >= 1 })
	entries := h.queue.entriesFor("s-dev")
	if len(entries) != 1 {
		t.Fatalf("成员 lease 被绕过：两个任务同时入队: %+v", entries)
	}
	first := entries[0]
	h.completeSubmission("s-dev", first.submissionID, first.text, "先派任务完成。")
	waitFor(t, "后派任务入队", func() bool { return len(h.queue.entriesFor("s-dev")) >= 2 })

	var firstCompletedAt, secondDispatchingAt uint64
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	firstDispatched, secondDispatched := "", ""
	for _, event := range events {
		taskID := payloadString(event.Payload, "taskId")
		switch {
		case event.Type == EventTaskDispatching && (event.RunID == runA.ID || event.RunID == runB.ID):
			if firstDispatched == "" {
				firstDispatched = taskID
			} else if taskID != firstDispatched && secondDispatched == "" {
				secondDispatched = taskID
				secondDispatchingAt = event.Sequence
			}
		case event.Type == EventTaskCompleted && taskID == firstDispatched:
			firstCompletedAt = event.Sequence
		}
	}
	if firstDispatched == "" || secondDispatched == "" || firstCompletedAt == 0 ||
		secondDispatchingAt < firstCompletedAt {
		t.Fatalf("跨 Run 成员 lease 违约：first=%s completed seq=%d，second=%s dispatching seq=%d",
			firstDispatched, firstCompletedAt, secondDispatched, secondDispatchingAt)
	}
}

// TestActiveRunCapDefersDispatch 活跃 Run 准入（MaxActiveRuns=1）：run-b 的
// 任务在 run-a 有在途任务期间保持 planned，run-a 收尾后才派发（不同成员
// 也不得越过准入上限）。
func TestActiveRunCapDefersDispatch(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.MaxActiveRuns = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "实现 A", WriteLock: TaskWriteLockNone},
	})
	// b 派给 lead（非评审成员）：派给 rev 会注入审查协议，无标记回复触发
	// revfix 修复轮，Run 保持活跃——那是审查回路的正确行为，不属于本用例。
	h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "b", MemberID: "lead", Prompt: "实现 B", WriteLock: TaskWriteLockNone},
	})
	// 竞速对称：先激活的 Run（a 或 b 都可能）占住唯一活跃槽，另一 Run 的
	// 任务须等先激活任务 completed 事实之后才派发（事件序断言）。
	waitFor(t, "首个任务入队", func() bool {
		return len(h.queue.entriesFor("s-dev"))+len(h.queue.entriesFor("s-lead")) >= 1
	})
	firstThread, firstEntry := "s-dev", fakeQueueEntry{}
	if entries := h.queue.entriesFor("s-dev"); len(entries) > 0 {
		firstEntry = entries[0]
	} else {
		firstThread = "s-lead"
		firstEntry = h.queue.entriesFor("s-lead")[0]
	}
	otherThread := "s-lead"
	if firstThread == "s-lead" {
		otherThread = "s-dev"
	}
	if entries := h.queue.entriesFor(otherThread); len(entries) != 0 {
		t.Fatalf("活动 Run 上限被绕过：第二个 Run 提前入队: %+v", entries)
	}
	h.completeSubmission(firstThread, firstEntry.submissionID, firstEntry.text, "先激活任务完成。")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(h.queue.entriesFor(otherThread)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.queue.entriesFor(otherThread)) == 0 {
		for _, run := range h.store.Runs() {
			for i := range run.Tasks {
				task := run.Tasks[i]
				t.Logf("DEBUG run=%s status=%s task=%s taskStatus=%s", run.ID, run.Status, task.ID, task.Status)
			}
		}
		t.Fatalf("第二个 Run 的任务未派发（first=%s firstTask=%s）", firstThread, firstEntry.text)
	}
	var firstCompletedAt, secondDispatchingAt uint64
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		taskID := payloadString(event.Payload, "taskId")
		if event.Type == EventTaskCompleted && (taskID == "a" || taskID == "b") &&
			((firstThread == "s-dev" && taskID == "a") || (firstThread == "s-lead" && taskID == "b")) {
			firstCompletedAt = event.Sequence
		}
		if event.Type == EventTaskDispatching && ((otherThread == "s-dev" && taskID == "a") || (otherThread == "s-lead" && taskID == "b")) {
			secondDispatchingAt = event.Sequence
		}
	}
	if firstCompletedAt == 0 || secondDispatchingAt == 0 || secondDispatchingAt < firstCompletedAt {
		t.Fatalf("活动 Run 准入违约：first completed seq=%d，second dispatching seq=%d", firstCompletedAt, secondDispatchingAt)
	}
}

// TestRouteOrderQuotaAfterThreePriority 连续 3 条优先输入 routed 后，最早的
// 普通输入强制插队（3:1 配额）。
func TestRouteOrderQuotaAfterThreePriority(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "team-inputs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() })
	ledger := LoadInputLedger(filepath.Join(dir, "team-inputs.json"), journal)
	now := time.Now()
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("prio-%d", i)
		if _, _, err := ledger.Accept("team-1", id, "紧急", nil, "", 8); err != nil {
			t.Fatal(err)
		}
		if _, _, err := ledger.Prioritize("team-1", id, "op-"+id, 1); err != nil {
			t.Fatal(err)
		}
		if err := ledger.MarkRoutingIntent("team-1", id, "new_task", "r"+id, "lead", ""); err != nil {
			t.Fatal(err)
		}
		if err := ledger.MarkRouted("team-1", id, "new_task", "r"+id, "lead"); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := ledger.Accept("team-1", "normal-1", "普通需求", nil, "", 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Accept("team-1", "prio-next", "又一条紧急", nil, "", 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Prioritize("team-1", "prio-next", "op-next", 1); err != nil {
		t.Fatal(err)
	}
	order := ledger.RouteOrderAt("team-1", now)
	if len(order) != 2 || order[0].ID != "normal-1" || order[1].ID != "prio-next" {
		t.Fatalf("连续 3 条优先后，最早的普通输入应插队: %v", idsOf(order))
	}
}

// TestRouteOrderAgingPromotesNormal 普通输入等待超过提升线即视同优先（手落
// 快照回拨 AcceptedAt——计时字段不经 API 暴露）。
func TestRouteOrderAgingPromotesNormal(t *testing.T) {
	dir := t.TempDir()
	snapshot := filepath.Join(dir, "team-inputs.json")
	aged := time.Now().Add(-routerAgingAfter - time.Minute).UTC().Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{
  "aged": {"id":"aged","teamId":"team-1","sequence":1,"revision":1,"text":"老输入","acceptedAt":%[1]q,"queueStatus":"queued","version":1,"updatedAt":%[1]q},
  "fresh": {"id":"fresh","teamId":"team-1","sequence":2,"revision":1,"text":"新输入","acceptedAt":%[2]q,"queueStatus":"queued","version":1,"updatedAt":%[2]q}
}`, aged, fresh)
	if err := os.WriteFile(snapshot, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ledger := LoadInputLedger(snapshot, nil)
	order := ledger.RouteOrderAt("team-1", time.Now())
	if len(order) != 2 || order[0].ID != "aged" || order[1].ID != "fresh" {
		t.Fatalf("老化输入应先于未老化普通输入: %v", idsOf(order))
	}
}

func idsOf(inputs []*TeamInput) []string {
	ids := make([]string, 0, len(inputs))
	for _, input := range inputs {
		ids = append(ids, input.ID)
	}
	return ids
}

// TestQueueDispatchBudgetParksRouting 队列滚动派发预算：journal 里的
// task_dispatching 达到上限后，新输入停靠 waiting_input（原因含上限与重置
// 提示）；调高预算后重泵恢复。
func TestQueueDispatchBudgetParksRouting(t *testing.T) {
	h := newRouterHarness(t)
	h.team.Policy.QueueDispatchBudget = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	h.acceptInput(t, "in-budget-1", "预算内的第一条")
	h.service.RoutePendingInputs(h.team.ID)
	if h.service.inputs.Get(h.team.ID, "in-budget-1").QueueStatus != InputRouted {
		t.Fatal("首条应在预算内路由")
	}
	// 模拟一次已发生的派发事实（lead 任务已派出）。
	run := h.store.Run("in-budget-1")
	event := newEvent(run.ID, EventTaskDispatching, "runner", map[string]any{
		"taskId": "lead", "submissionId": "sub-1", "threadId": "th-1", "inputHash": "h", "attempt": 1,
	})
	if err := h.journal.Append(event); err != nil {
		t.Fatal(err)
	}
	h.acceptInput(t, "in-budget-2", "预算外的第二条")
	h.service.RoutePendingInputs(h.team.ID)
	input := h.service.inputs.Get(h.team.ID, "in-budget-2")
	if input.QueueStatus != InputWaitingInput || !strings.Contains(input.WaitingReason, "队列预算已用完") {
		t.Fatalf("超预算应停靠并说明原因: %+v", input)
	}
	if !strings.Contains(input.WaitingReason, "queueDispatchBudget") {
		t.Fatalf("停靠原因应给出调整入口: %s", input.WaitingReason)
	}
	// 调高预算后重泵恢复。
	h.team.Policy.QueueDispatchBudget = 4
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	if routed := h.service.RoutePendingInputs(h.team.ID); routed != 1 {
		t.Fatalf("调高预算后应恢复路由，实际 %d", routed)
	}
	if h.service.inputs.Get(h.team.ID, "in-budget-2").QueueStatus != InputRouted {
		t.Fatal("恢复后应 routed")
	}
}
