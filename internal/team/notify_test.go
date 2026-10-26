package team

// notify_test.go 是 Run 通知侧的单元测试（§12 阶段四工作项 3/5，§6.3）：
// 四类 run 状态事件→OnRunStatus 通知映射（完成/失败/取消/waiting_input，含
// 重启恢复路径）、通知文案组装（goal 截 60 rune、salvage 结果摘要）、stall
// 的可控 clock 判定（推进 20 分钟→事件+回调+Run 状态不变+单次去重+成功刷新
// 重置）、盲区声明用例（waiting_input 不产 stall；paused 尚未落地，见
// notify.go 的声明注释）。等待用 deadline 轮询与手动时钟，不用固定 sleep
// 判定正确性（§13.1）。

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/notifications"
)

// ---- 测试基件 ----

// notifyRecorder 收集 OnRunStatus 回调（回调在 Run 的 state.mu 下从 watcher/
// 循环 goroutine 同步调用，必须加锁）。
type notifyRecorder struct {
	mu    sync.Mutex
	calls [][2]string
}

func (r *notifyRecorder) onRunStatus(runID, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, [2]string{runID, kind})
}

func (r *notifyRecorder) count(runID, kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, call := range r.calls {
		if call[0] == runID && call[1] == kind {
			count++
		}
	}
	return count
}

func (r *notifyRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// manualClock 是可控时钟（Service.nowFn 注入缝）：stall 判定用它取时，测试
// 推进 20 分钟而不真睡。
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(by time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(by)
}

// stallAnchorOf 读取 run 的 stall 计时锚点（等待「任务完成已刷新锚点」的
// 正向屏障用）。
func stallAnchorOf(h *dispatchHarness, runID string) (time.Time, string) {
	state := h.service.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.lastSuccessAt, state.lastCompletedTaskID
}

// ---- 事件/状态 → 通知类别映射 ----

func TestRunStatusKindMapping(t *testing.T) {
	cases := []struct {
		event string
		kind  string
	}{
		{EventRunCompleted, notifications.RunNotifyCompleted},
		{EventRunFailed, notifications.RunNotifyFailed},
		{EventRunCancelled, notifications.RunNotifyCancelled},
		{EventRunStopped, notifications.RunNotifyStopped},
		{EventRunWaitingInput, notifications.RunNotifyWaitingInput},
		{EventTaskCompleted, ""}, {EventTaskStopped, ""}, {EventRunCreated, ""},
		{EventRunStalled, ""}, // stall 不走 run 状态事件映射（表外事件直发）。
	}
	for _, item := range cases {
		if got := runStatusKind(item.event); got != item.kind {
			t.Fatalf("事件 %s 应映射 %q，实际 %q", item.event, item.kind, got)
		}
	}
	statusCases := []struct {
		status string
		kind   string
	}{
		{RunCompleted, notifications.RunNotifyCompleted},
		{RunFailed, notifications.RunNotifyFailed},
		{RunCancelled, notifications.RunNotifyCancelled},
		{RunStopped, notifications.RunNotifyStopped},
		{RunWaitingInput, notifications.RunNotifyWaitingInput},
		{RunDispatching, ""}, {RunReducing, ""}, {RunCreated, ""},
		{RunOutcomeUnknown, ""}, // 无状态事件可挂（runStatusEvent 空串），不通知。
	}
	for _, item := range statusCases {
		if got := RunNotificationKindOfStatus(item.status); got != item.kind {
			t.Fatalf("状态 %s 应映射 %q，实际 %q", item.status, item.kind, got)
		}
	}
}

// ---- 通知文案组装 ----

func TestRunNotificationTextComposition(t *testing.T) {
	longResult := strings.Repeat("果", 260) // 260 rune，超 200 截断
	run := &Run{
		ID: "run-1", TeamID: "team-1", Goal: strings.Repeat("目", 70),
		Status: RunFailed, StopReason: "有任务失败",
		Tasks: []Task{
			{ID: "a", Status: TaskCompleted, Result: "第一项结果"},
			{ID: "b", Status: TaskCompleted, Result: "第二行1\n第二行2"},
			{ID: "c", Status: TaskCompleted, Result: longResult},
			{ID: "d", Status: TaskCompleted, Result: "第四项（超出前 3）"},
			{ID: "e", Status: TaskFailed, Error: "boom"},
			{ID: "f", Status: TaskRunning},
		},
	}
	// 标题：goal 截 60 rune。
	title, body := RunNotificationText(run, notifications.RunNotifyCompleted)
	if !strings.HasPrefix(title, "运行完成：") || len([]rune(title)) != len([]rune("运行完成："))+60 {
		t.Fatalf("标题应含 goal 前 60 rune: %q", title)
	}
	// 正文：任务计数 + 前 3 个已完成结果（换行压平、200 rune 截断加省略号）。
	if !strings.Contains(body, "任务 6 个：完成 4、失败 1、在途 1") {
		t.Fatalf("任务计数行不符: %q", body)
	}
	for _, want := range []string{"- a：第一项结果", "- b：第二行1 第二行2"} {
		if !strings.Contains(body, want) {
			t.Fatalf("正文应含 %q: %q", want, body)
		}
	}
	if !strings.Contains(body, "- c："+strings.Repeat("果", 200)+"…") {
		t.Fatalf("结果应截 200 rune 加省略号: %q", body[:min(len(body), 120)])
	}
	if strings.Contains(body, "第四项") {
		t.Fatalf("只携带前 3 个结果: %q", body)
	}
	// 失败通知同样带 salvage 结果（§6.5：失败不得吞掉已完成结果）。
	title, body = RunNotificationText(run, notifications.RunNotifyFailed)
	if !strings.HasPrefix(title, "运行失败：") || !strings.Contains(body, "有任务失败") || !strings.Contains(body, "- a：第一项结果") {
		t.Fatalf("失败通知应含原因与 salvage 结果: %q / %q", title, body)
	}
	// 取消：原因+计数，无结果摘要。
	_, body = RunNotificationText(run, notifications.RunNotifyCancelled)
	if !strings.Contains(body, "有任务失败") || strings.Contains(body, "- a：") {
		t.Fatalf("取消通知不带结果摘要: %q", body)
	}
	// waiting_input：原因+计数。
	_, body = RunNotificationText(run, notifications.RunNotifyWaitingInput)
	if !strings.Contains(body, "有任务失败") || !strings.Contains(body, "任务 6 个") {
		t.Fatalf("waiting_input 通知应含原因与计数: %q", body)
	}
	// stall：判定线表述（精确时长在 journal 事件 payload）。
	title, body = RunNotificationText(run, notifications.RunNotifyStall)
	if !strings.HasPrefix(title, "运行疑似停滞：") || !strings.Contains(body, "20 分钟") || !strings.Contains(body, "未停止") {
		t.Fatalf("stall 通知文案不符: %q / %q", title, body)
	}
	// 非 compliment：未知类别与 nil run。
	if title, body = RunNotificationText(run, "bogus"); title != "" || body != "" {
		t.Fatalf("未知类别应返回空文案: %q %q", title, body)
	}
	if title, body := RunNotificationText(nil, notifications.RunNotifyCompleted); title != "" || body != "" {
		t.Fatalf("nil run 应返回空文案: %q %q", title, body)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---- 四类 run 状态事件 → OnRunStatus 映射 ----

// TestOnRunStatusFiresForRunLifecycle 逐类驱动真实状态机：完成/失败/取消/
// waiting_input（含账号停靠路径）各恰好回调一次，且不夹带其他类别。
func TestOnRunStatusFiresForRunLifecycle(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		h := newDispatchHarness(t)
		recorder := &notifyRecorder{}
		h.service.OnRunStatus = recorder.onRunStatus
		run := h.createRun(t, ModeSingle, nil)
		submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
		h.completeSubmission("s-lead", submissionID, sentText, "分析完成。")
		waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
		waitFor(t, "完成通知", func() bool { return recorder.count(run.ID, notifications.RunNotifyCompleted) == 1 })
		if recorder.total() != 1 {
			t.Fatalf("应恰好一次回调，实际 %v", recorder.calls)
		}
	})
	t.Run("failed", func(t *testing.T) {
		h := newDispatchHarness(t)
		recorder := &notifyRecorder{}
		h.service.OnRunStatus = recorder.onRunStatus
		run := h.createRun(t, ModeSingle, nil)
		submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
		// 未知类别错误（非 quota/auth/rate_limited）：确定性 failed，无重试。
		h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
			turnFailedView(sentText, "工具执行异常")))
		h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
		waitFor(t, "run 失败", func() bool { return h.store.Run(run.ID).Status == RunFailed })
		waitFor(t, "失败通知", func() bool { return recorder.count(run.ID, notifications.RunNotifyFailed) == 1 })
		if recorder.total() != 1 {
			t.Fatalf("应恰好一次回调，实际 %v", recorder.calls)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		h := newDispatchHarness(t)
		recorder := &notifyRecorder{}
		h.service.OnRunStatus = recorder.onRunStatus
		run := h.createRun(t, ModeSingle, nil)
		h.waitForSubmission(t, "s-lead", "lead 任务")
		if _, err := h.service.Cancel(run.ID); err != nil {
			t.Fatalf("取消失败: %v", err)
		}
		waitFor(t, "取消通知", func() bool { return recorder.count(run.ID, notifications.RunNotifyCancelled) == 1 })
		if recorder.total() != 1 {
			t.Fatalf("应恰好一次回调，实际 %v", recorder.calls)
		}
	})
	t.Run("waiting_input（quota 停靠）", func(t *testing.T) {
		h := newDispatchHarness(t)
		recorder := &notifyRecorder{}
		h.service.OnRunStatus = recorder.onRunStatus
		run := h.createRun(t, ModePipeline, pipelinePlan())
		submissionID, sentText := h.waitForSubmission(t, "s-lead", "design")
		h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
			turnFailedView(sentText, "账户额度不足，请充值后重试")))
		h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
		waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
		waitFor(t, "waiting_input 通知", func() bool {
			return recorder.count(run.ID, notifications.RunNotifyWaitingInput) == 1
		})
		if recorder.total() != 1 {
			t.Fatalf("停靠应恰好一次回调，实际 %v", recorder.calls)
		}
	})
}

// TestOnRunStatusWaitingInputFromRecovery 重启恢复路径（recovery.go 的
// waiting_input 迁移经 commitRun 收口）同样触发挂点——停机期间需要用户处理
// 的 Run 重启后仍会通知。
func TestOnRunStatusWaitingInputFromRecovery(t *testing.T) {
	h := newDispatchHarness(t)
	recorder := &notifyRecorder{}
	h.service.OnRunStatus = recorder.onRunStatus
	run := h.seedInflightRun(t, "run-rec-notify", func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-lead", submissionID, ComposeTaskMessage("", "分析")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-lead", submissionID, SubStatusUnknown, "")
		h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead"))
	})
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	waitFor(t, "恢复期 waiting_input 通知", func() bool {
		return recorder.count(run.ID, notifications.RunNotifyWaitingInput) == 1
	})
	if recorder.total() != 1 {
		t.Fatalf("恢复路径应恰好一次回调，实际 %v", recorder.calls)
	}
}

// ---- stall（§6.3 只通知不停止）----

// TestStallNotifiesOncePerEpisodeAndResetsOnSuccess：手动时钟推进 20 分钟 →
// run_stalled 事件 + OnRunStatus(stall) 回调 + Run 状态/在途任务不变；同段
// 只通知一次（推进更久不再报）；任务完成后锚点刷新，新一段停滞可再报。
func TestStallNotifiesOncePerEpisodeAndResetsOnSuccess(t *testing.T) {
	h := newDispatchHarness(t)
	clock := &manualClock{now: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}
	h.service.nowFn = clock.Now
	recorder := &notifyRecorder{}
	h.service.OnRunStatus = recorder.onRunStatus
	// a 完成后 b 挂起（queued 永不派发）、b 完成后 c 挂起——两段停滞各可观察。
	plan := []PlanTaskInput{
		{ID: "a", MemberID: "lead", Prompt: "分析", WriteLock: TaskWriteLockNone},
		{ID: "b", MemberID: "dev", Prompt: "实现", DependsOn: []string{"a"}, WriteLock: TaskWriteLockNone},
		{ID: "c", MemberID: "rev", Prompt: "审查", DependsOn: []string{"b"}, WriteLock: TaskWriteLockNone},
	}
	run := h.createRun(t, ModePipeline, plan)
	subA, textA := h.waitForSubmission(t, "s-lead", "a 任务")
	h.completeSubmission("s-lead", subA, textA, "a 完成")
	waitFor(t, "a 完成", func() bool { return h.taskOf(run.ID, "a").Status == TaskCompleted })
	// 锚点刷新的正向屏障（避免「快照已落、锚点未刷」的竞态窗口）。
	waitFor(t, "a 锚点刷新", func() bool {
		anchor, taskID := stallAnchorOf(h, run.ID)
		return !anchor.IsZero() && taskID == "a"
	})
	subB, _ := h.waitForSubmission(t, "s-dev", "b 任务")
	// 第一段：推进 21 分钟 → stall（sinceMs 恰 21 分钟——时钟冻结时完成 a）。
	clock.Advance(21 * time.Minute)
	waitFor(t, "第一段 stall 通知", func() bool { return recorder.count(run.ID, notifications.RunNotifyStall) == 1 })
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunDispatching {
		t.Fatalf("stall 绝不改 run 状态（§6.3 只通知不停止），实际 %s", fresh.Status)
	}
	if task := h.taskOf(run.ID, "b"); task.Status != TaskDispatching {
		t.Fatalf("stall 不得取消在途任务，实际 %s", task.Status)
	}
	stalled := h.eventsOfType(run.ID, EventRunStalled)
	if len(stalled) != 1 {
		t.Fatalf("应恰好一条 run_stalled 事件，实际 %d", len(stalled))
	}
	if since, ok := stalled[0].Payload["sinceMs"].(json.Number); !ok || since.String() != "1260000" {
		t.Fatalf("sinceMs 应为 21 分钟（1260000ms），实际 %v", stalled[0].Payload["sinceMs"])
	}
	if stalled[0].Payload["lastCompletedTaskId"] != "a" {
		t.Fatalf("payload 应携带最近完成任务: %v", stalled[0].Payload)
	}
	// 同段去重：b 完成前（真实 pass 持续跑、时钟已过线更久）不第二次报。
	clock.Advance(5 * time.Minute)
	h.completeSubmission("s-dev", subB, h.queueEntryText(t, "s-dev", subB), "b 完成")
	waitFor(t, "b 完成", func() bool { return h.taskOf(run.ID, "b").Status == TaskCompleted })
	waitFor(t, "b 锚点刷新", func() bool {
		_, taskID := stallAnchorOf(h, run.ID)
		return taskID == "b"
	})
	if got := recorder.count(run.ID, notifications.RunNotifyStall); got != 1 {
		t.Fatalf("同段停滞只通知一次，实际 %d", got)
	}
	// 第二段：c 挂起，推进 21 分钟 → 新一段 stall（lastSuccessAt 刷新重置）。
	h.waitForSubmission(t, "s-rev", "c 任务")
	clock.Advance(21 * time.Minute)
	waitFor(t, "第二段 stall 通知", func() bool { return recorder.count(run.ID, notifications.RunNotifyStall) == 2 })
	if fresh := h.store.Run(run.ID); fresh.Status != RunDispatching {
		t.Fatalf("第二段 stall 后 run 仍不得改状态，实际 %s", fresh.Status)
	}
	if got := len(h.eventsOfType(run.ID, EventRunStalled)); got != 2 {
		t.Fatalf("两段停滞应恰好两条事件，实际 %d", got)
	}
	second := h.eventsOfType(run.ID, EventRunStalled)[1]
	if second.Payload["lastCompletedTaskId"] != "b" {
		t.Fatalf("第二段 payload 应携带 b: %v", second.Payload)
	}
}

// queueEntryText 取账本条目的发送文本（completeSubmission 需要 SentText 匹配
// turn）。
func (h *dispatchHarness) queueEntryText(t *testing.T, threadID, submissionID string) string {
	t.Helper()
	for _, entry := range h.queue.entriesFor(threadID) {
		if entry.submissionID == submissionID {
			return entry.text
		}
	}
	t.Fatalf("账本条目不存在: %s/%s", threadID, submissionID)
	return ""
}

// TestStallBlindSpotWaitingInput：waiting_input 不参与 stall 计时（有意设计，
// notify.go maybeStallLocked 的盲区声明）——停靠后时钟推进远超判定线、经
// 周期 reconciler 的 RefreshRun 路径，也不产生 stall 事件或回调。passAdvance
// 持 state.mu 串行：waiting_input 落账可观测时，任何在途 pass 已收口、后续
// pass 在检查点前早退，断言无竞态。paused 状态尚未落地（阶段四工作项 4），
// 落地后同理不计时——声明注释见 notify.go。
func TestStallBlindSpotWaitingInput(t *testing.T) {
	h := newDispatchHarness(t)
	clock := &manualClock{now: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}
	h.service.nowFn = clock.Now
	recorder := &notifyRecorder{}
	h.service.OnRunStatus = recorder.onRunStatus
	run := h.createRun(t, ModePipeline, pipelinePlan())
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "design")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "账户额度不足，请充值后重试")))
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	// 停靠后时钟推进 2 小时（远超 20 分钟判定线），并跑一轮 RefreshRun
	//（周期 reconciler 的同款路径）：不产生 stall。
	clock.Advance(2 * time.Hour)
	if err := h.service.RefreshRun(run.ID); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if got := recorder.count(run.ID, notifications.RunNotifyStall); got != 0 {
		t.Fatalf("waiting_input 驻留不产 stall 通知，实际 %d", got)
	}
	if events := h.eventsOfType(run.ID, EventRunStalled); len(events) != 0 {
		t.Fatalf("waiting_input 驻留不产 run_stalled 事件，实际 %d", len(events))
	}
	if got := recorder.count(run.ID, notifications.RunNotifyWaitingInput); got != 1 {
		t.Fatalf("停靠时刻应有且仅有一次 waiting_input 通知，实际 %d", got)
	}
}

// TestStallDisabledWhenStallAfterNonPositive：stallAfter≤0 关闭检查（注入缝
// 的关闭语义）。
func TestStallDisabledWhenStallAfterNonPositive(t *testing.T) {
	h := newDispatchHarness(t)
	clock := &manualClock{now: time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)}
	h.service.nowFn = clock.Now
	h.service.stallAfter = 0
	recorder := &notifyRecorder{}
	h.service.OnRunStatus = recorder.onRunStatus
	run := h.createRun(t, ModeSingle, nil)
	h.waitForSubmission(t, "s-lead", "lead 任务")
	clock.Advance(24 * time.Hour)
	waitFor(t, "run 保持 dispatching", func() bool {
		return h.store.Run(run.ID).Status == RunDispatching && h.taskOf(run.ID, "lead").Status == TaskDispatching
	})
	if got := recorder.count(run.ID, notifications.RunNotifyStall); got != 0 {
		t.Fatalf("stallAfter≤0 应关闭 stall 检查，实际 %d 次", got)
	}
	if events := h.eventsOfType(run.ID, EventRunStalled); len(events) != 0 {
		t.Fatalf("不应有 run_stalled 事件，实际 %d", len(events))
	}
}

// TestStallAnchorRebuildFromJournal：重启恢复的锚点重建——journal 最近
// task_completed 的 createdAt 有效则取之（cost-stats 为 newEvent 盖 createdAt
// 章后生效）；恒为零值（该工作项未合入的现状）回退当前时刻，绝不把历史
// 成功误报成 stall。
func TestStallAnchorRebuildFromJournal(t *testing.T) {
	h := newDispatchHarness(t)
	base := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	clock := &manualClock{now: base}
	h.service.nowFn = clock.Now
	// 有效 createdAt：取最近一条本 run 的 task_completed（倒序扫描）。
	state := h.service.stateFor("run-x")
	events := []*Event{
		{RunID: "run-x", Type: EventTaskCompleted, CreatedAt: base.Add(-2 * time.Hour),
			Payload: map[string]any{"taskId": "a"}},
		{RunID: "other", Type: EventTaskCompleted, CreatedAt: base.Add(-time.Minute),
			Payload: map[string]any{"taskId": "z"}},
		{RunID: "run-x", Type: EventTaskFailed, CreatedAt: base.Add(-time.Minute),
			Payload: map[string]any{"taskId": "b"}},
	}
	h.service.rebuildLastSuccessAt(state, events)
	if !state.lastSuccessAt.Equal(base.Add(-2*time.Hour)) || state.lastCompletedTaskID != "a" {
		t.Fatalf("锚点应取本 run 最近 task_completed 的 createdAt: %v %s",
			state.lastSuccessAt, state.lastCompletedTaskID)
	}
	// createdAt 恒为零值（现状）：回退当前时刻（可控 clock 断言）。
	fallback := h.service.stateFor("run-y")
	h.service.rebuildLastSuccessAt(fallback, []*Event{
		{RunID: "run-y", Type: EventTaskCompleted, CreatedAt: time.Time{},
			Payload: map[string]any{"taskId": "a"}},
	})
	if !fallback.lastSuccessAt.Equal(base) {
		t.Fatalf("无有效时间戳应回退当前时刻: %v", fallback.lastSuccessAt)
	}
	// 空 journal：同样回退。
	empty := h.service.stateFor("run-z")
	h.service.rebuildLastSuccessAt(empty, nil)
	if !empty.lastSuccessAt.Equal(base) {
		t.Fatalf("空 journal 应回退当前时刻: %v", empty.lastSuccessAt)
	}
}

// TestStallAnchorResetsOnResume：Resume 重启循环时锚点重置——停靠驻留时长
// 不计入「无成功」（resume 后立刻推进 19 分钟不报、21 分钟才报）。
func TestStallAnchorResetsOnResume(t *testing.T) {
	h := newDispatchHarness(t)
	base := time.Date(2026, 10, 24, 8, 0, 0, 0, time.UTC)
	clock := &manualClock{now: base}
	h.service.nowFn = clock.Now
	recorder := &notifyRecorder{}
	h.service.OnRunStatus = recorder.onRunStatus
	run := h.createRun(t, ModePipeline, pipelinePlan())
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "design")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "账户额度不足，请充值后重试")))
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	// 停靠 3 小时后 resume：锚点从 resume 时刻重起，不得立刻误报 stall。
	clock.Advance(3 * time.Hour)
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	waitFor(t, "重派入队", func() bool {
		entries := h.queue.entriesFor("s-lead")
		return len(entries) >= 2
	})
	clock.Advance(19 * time.Minute)
	waitFor(t, "run 回到执行态", func() bool {
		fresh := h.store.Run(run.ID)
		return fresh.Status == RunDispatching && h.taskOf(run.ID, "design").Status == TaskDispatching
	})
	if got := recorder.count(run.ID, notifications.RunNotifyStall); got != 0 {
		t.Fatalf("resume 后 19 分钟不得报 stall（锚点已重置），实际 %d", got)
	}
	clock.Advance(2 * time.Minute)
	waitFor(t, "resume 后到线报 stall", func() bool { return recorder.count(run.ID, notifications.RunNotifyStall) == 1 })
}
