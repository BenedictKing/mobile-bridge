package team

// stats_test.go 是成本与耗时统计的单元测试（§12 阶段四工作项 5、§9 预算，
// §13.1）：ComputeRunStats 纯函数表驱动（同 attempt 配对、多 attempt 取高、
// 在途、覆盖度 full/partial/none、broadcast 组、重复派发去重）、预算谓词；
// 预算闸门的集成行为（达阈值 waiting_input 且不再选新任务、Resume 追加
// ceiling 生效至下一阈值、重启重放回填不漂移、budget=0 只统计）。等待用
// deadline 轮询，不用固定 sleep 判定正确性（§13.1）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ---- 纯函数：ComputeRunStats ----

// statsEvent 是测试用事件构造（时间戳显式、payload 数字用 json.Number——
// 与 journal 解码后的形态一致，payloadInt 只认 json.Number）。
func statsEvent(runID, eventType string, at time.Time, payload map[string]any) *Event {
	return &Event{ID: eventType + "-" + at.Format("150405.000000000"),
		RunID: runID, Type: eventType, Source: "runner", Payload: payload, CreatedAt: at}
}

var statsBase = time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)

func TestComputeRunStatsFullCoverage(t *testing.T) {
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, map[string]any{"goal": "g"}),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(10*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventTaskRunning, statsBase.Add(20*time.Second),
			map[string]any{"taskId": "a"}),
		statsEvent("r1", EventTaskCompleted, statsBase.Add(70*time.Second),
			map[string]any{"taskId": "a"}), // completed 不携带 attempt：归属最近派发
		statsEvent("r1", EventRunCompleted, statsBase.Add(90*time.Second), nil),
		statsEvent("other", EventTaskDispatching, statsBase.Add(30*time.Second),
			map[string]any{"taskId": "x", "attempt": jsoncompat.Num(1)}), // 他 run：过滤
	}
	run := &Run{ID: "r1", Status: RunCompleted, Tasks: []Task{{ID: "a"}, {ID: "never-dispatched"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), statsBase.Add(time.Minute))
	if stats.Coverage != StatsCoverageFull {
		t.Fatalf("覆盖度应为 full，得到 %s", stats.Coverage)
	}
	if stats.RunElapsedMs != 90_000 {
		t.Fatalf("Run 耗时应为 90000ms，得到 %d", stats.RunElapsedMs)
	}
	if stats.DispatchCount != 1 {
		t.Fatalf("派发计数应为 1（他 run 事件不计），得到 %d", stats.DispatchCount)
	}
	if len(stats.Tasks) != 1 || stats.Tasks[0].TaskID != "a" {
		t.Fatalf("任务条目应只有已派发的 a：%+v", stats.Tasks)
	}
	if stats.Tasks[0].Attempt != 1 || stats.Tasks[0].ElapsedMs != 60_000 {
		t.Fatalf("a 的耗时应为 attempt1 派发→终态 60000ms，得到 %+v", stats.Tasks[0])
	}
	if stats.Tokens != nil {
		t.Fatalf("Tokens 必须恒 nil（token 明示不支持），得到 %+v", stats.Tokens)
	}
}

func TestComputeRunStatsMultiAttemptTakesHighest(t *testing.T) {
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, nil),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(10*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventTaskFailed, statsBase.Add(40*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1), "outcome": "transient"}),
		statsEvent("r1", EventTaskRetryScheduled, statsBase.Add(41*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(2)}),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(60*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(2)}),
		statsEvent("r1", EventTaskCompleted, statsBase.Add(90*time.Second),
			map[string]any{"taskId": "a"}),
	}
	run := &Run{ID: "r1", Status: RunCompleted, Tasks: []Task{{ID: "a"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), statsBase.Add(2*time.Minute))
	if stats.DispatchCount != 2 {
		t.Fatalf("两次派发应计 2，得到 %d", stats.DispatchCount)
	}
	if len(stats.Tasks) != 1 {
		t.Fatalf("任务条目数异常: %+v", stats.Tasks)
	}
	entry := stats.Tasks[0]
	if entry.Attempt != 2 {
		t.Fatalf("attempt 应取最高 2（照 recovery 单调先例），得到 %d", entry.Attempt)
	}
	if entry.ElapsedMs != 30_000 {
		t.Fatalf("耗时应为 attempt2 派发→completed 30000ms，得到 %d", entry.ElapsedMs)
	}
}

func TestComputeRunStatsInFlightTakesNow(t *testing.T) {
	now := statsBase.Add(5 * time.Minute)
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, nil),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(time.Minute),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
	}
	run := &Run{ID: "r1", Status: RunDispatching, Tasks: []Task{{ID: "a"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), now)
	if stats.Coverage != StatsCoverageFull {
		t.Fatalf("在途任务（派发盖章、终态未落）仍是 full，得到 %s", stats.Coverage)
	}
	if stats.Tasks[0].ElapsedMs != 4*60_000 {
		t.Fatalf("在途耗时应取 now-派发 240000ms，得到 %d", stats.Tasks[0].ElapsedMs)
	}
	if stats.RunElapsedMs != 5*60_000 {
		t.Fatalf("未终局 Run 耗时应取 now-created 300000ms，得到 %d", stats.RunElapsedMs)
	}
}

func TestComputeRunStatsLegacyCoverageNone(t *testing.T) {
	zero := time.Time{}
	events := []*Event{
		statsEvent("r1", EventRunCreated, zero, nil),
		statsEvent("r1", EventTaskDispatching, zero,
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventTaskCompleted, zero, map[string]any{"taskId": "a"}),
		statsEvent("r1", EventRunCompleted, zero, nil),
	}
	run := &Run{ID: "r1", Status: RunCompleted, Tasks: []Task{{ID: "a"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), statsBase)
	if stats.Coverage != StatsCoverageNone {
		t.Fatalf("旧 journal（时间戳全零）应为 none，得到 %s", stats.Coverage)
	}
	if stats.RunElapsedMs != 0 || stats.Tasks[0].ElapsedMs != 0 {
		t.Fatalf("无时间戳不得编造耗时: %+v", stats)
	}
	if stats.DispatchCount != 1 {
		t.Fatalf("派发计数不依赖时间戳，应为 1，得到 %d", stats.DispatchCount)
	}
}

func TestComputeRunStatsUpgradeWindowPartial(t *testing.T) {
	zero := time.Time{}
	events := []*Event{
		statsEvent("r1", EventRunCreated, zero, nil), // 升级前创建：零值
		statsEvent("r1", EventTaskDispatching, statsBase.Add(time.Minute),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}), // 升级后派发：已盖章
		statsEvent("r1", EventTaskCompleted, statsBase.Add(2*time.Minute),
			map[string]any{"taskId": "a"}),
	}
	run := &Run{ID: "r1", Status: RunCompleted, Tasks: []Task{{ID: "a"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), statsBase.Add(3*time.Minute))
	if stats.Coverage != StatsCoveragePartial {
		t.Fatalf("升级窗口应为 partial，得到 %s", stats.Coverage)
	}
	if stats.RunElapsedMs != 0 {
		t.Fatalf("created 零值时 Run 耗时不可计，得到 %d", stats.RunElapsedMs)
	}
	if stats.Tasks[0].ElapsedMs != 60_000 {
		t.Fatalf("任务侧锚点齐全应可计 60000ms，得到 %d", stats.Tasks[0].ElapsedMs)
	}
}

func TestComputeRunStatsBroadcastAndDedupe(t *testing.T) {
	// broadcast 组：两个成员任务各自派发、各自计时；重复的派发事件
	//（commitRun 版本冲突重试的 last-wins 重复落账）只计一次。
	dup := statsEvent("r1", EventTaskDispatching, statsBase.Add(10*time.Second),
		map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)})
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, nil),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(10*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		dup,
		statsEvent("r1", EventTaskCompleted, statsBase.Add(30*time.Second),
			map[string]any{"taskId": "a"}),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(15*time.Second),
			map[string]any{"taskId": "b", "attempt": jsoncompat.Num(1)}),
	}
	run := &Run{ID: "r1", Status: RunDispatching,
		Tasks: []Task{{ID: "a", BroadcastGroup: "g1"}, {ID: "b", BroadcastGroup: "g1"}}}
	stats := ComputeRunStats(events, run, DefaultPolicy(), statsBase.Add(time.Minute))
	if stats.DispatchCount != 2 {
		t.Fatalf("重复派发应去重后计 2，得到 %d", stats.DispatchCount)
	}
	if len(stats.Tasks) != 2 {
		t.Fatalf("broadcast 两成员各一条: %+v", stats.Tasks)
	}
	if stats.Tasks[0].TaskID != "a" || stats.Tasks[0].ElapsedMs != 20_000 {
		t.Fatalf("a 耗时应为 20000ms: %+v", stats.Tasks[0])
	}
	if stats.Tasks[1].TaskID != "b" || stats.Tasks[1].ElapsedMs != 45_000 {
		t.Fatalf("b 在途应取 now-派发 45000ms: %+v", stats.Tasks[1])
	}
	if stats.Coverage != StatsCoverageFull {
		t.Fatalf("重复事件不得把 full 误降 partial，得到 %s", stats.Coverage)
	}
}

func TestComputeRunStatsBudgetFields(t *testing.T) {
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, nil),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
	}
	policy := DefaultPolicy()
	policy.BudgetUnits = 3
	run := &Run{ID: "r1", Status: RunWaitingInput, BudgetCeiling: 5, Tasks: []Task{{ID: "a"}}}
	stats := ComputeRunStats(events, run, policy, statsBase.Add(time.Minute))
	want := BudgetStat{Units: 3, Used: 1, Ceiling: 5, Exhausted: false}
	if stats.Budget != want {
		t.Fatalf("预算字段应为 %+v，得到 %+v", want, stats.Budget)
	}
	// 用尽判定与 runner 闸门同一谓词：used≥max(ceiling,units) 即真
	//（used=1：预算调到 1、从未追加上限即到线）。
	policy.BudgetUnits = 1
	run.BudgetCeiling = 0
	stats = ComputeRunStats(events, run, policy, statsBase.Add(time.Minute))
	if !stats.Budget.Exhausted {
		t.Fatal("used=1 ≥ max(0,1) 应判耗尽")
	}
}

func TestBudgetExhaustedPredicate(t *testing.T) {
	cases := []struct {
		units, ceiling, used int
		want                 bool
	}{
		{0, 0, 100, false},  // units=0 只统计（§4.2 默认）
		{-1, 0, 100, false}, // 非法策略等同关闭
		{5, 0, 4, false},    // 未达策略阈值
		{5, 0, 5, true},     // 恰达阈值
		{5, 0, 6, true},     // 超线（动作派生任务的少量过冲）
		{3, 8, 7, false},    // 追加过的上限优先
		{3, 8, 8, true},     // 达追加上限
		{8, 3, 8, true},     // 策略调高后不低于策略值
		{8, 3, 5, false},    // max(3,8)=8 未达
	}
	for _, item := range cases {
		if got := budgetExhausted(item.units, item.ceiling, item.used); got != item.want {
			t.Fatalf("budgetExhausted(%d,%d,%d) 应为 %v，得到 %v",
				item.units, item.ceiling, item.used, item.want, got)
		}
	}
}

// ---- 集成：预算闸门（dispatchHarness 基座）----

// budgetHarness 建预算=1、两任务两成员的并行计划（同批 ReadyTasks 会同时
// 放行两个——预算余量封顶必须只选第一个）。
func budgetPlan() []PlanTaskInput {
	return []PlanTaskInput{
		{ID: "first", MemberID: "lead", Prompt: "第一项分析", WriteLock: TaskWriteLockNone},
		{ID: "second", MemberID: "dev", Prompt: "第二项实现", WriteLock: TaskWriteLockNone},
	}
}

func newBudgetHarness(t *testing.T) *dispatchHarness {
	t.Helper()
	h := newDispatchHarness(t)
	h.team.Policy.BudgetUnits = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatalf("团队策略保存失败: %v", err)
	}
	return h
}

// TestBudgetParksRunAndStopsNewDispatch 达阈值 → run 落 waiting_input、
// 未派发任务保持 planned 不落终态、在途任务自然收尾后不 fold。
func TestBudgetParksRunAndStopsNewDispatch(t *testing.T) {
	h := newBudgetHarness(t)
	run := h.createRun(t, ModePipeline, budgetPlan())
	sub, text := h.waitForSubmission(t, "s-lead", "first 派发")
	waitFor(t, "预算 1 已用尽，run 应停靠 waiting_input", func() bool {
		return h.store.Run(run.ID).Status == RunWaitingInput
	})
	fresh := h.store.Run(run.ID)
	if !strings.HasPrefix(fresh.StopReason, StopReasonBudgetExhausted) {
		t.Fatalf("停靠原因应带 %s 前缀，得到 %q", StopReasonBudgetExhausted, fresh.StopReason)
	}
	if task := h.taskOf(run.ID, "second"); task.Status != TaskPlanned {
		t.Fatalf("预算停靠是 run 级：second 应保持 planned，得到 %s", task.Status)
	}
	// 事件 payload {units,used,stopReason}（stopReason 供 journal 重建还原）。
	parks := h.eventsOfType(run.ID, EventRunWaitingInput)
	if len(parks) != 1 {
		t.Fatalf("应恰好一条预算停靠事件，得到 %d", len(parks))
	}
	if got := payloadInt(parks[0].Payload, "units"); got != 1 {
		t.Fatalf("payload.units 应为 1，得到 %d", got)
	}
	if got := payloadInt(parks[0].Payload, "used"); got != 1 {
		t.Fatalf("payload.used 应为 1（按派发计数），得到 %d", got)
	}
	if reason := payloadString(parks[0].Payload, "stopReason"); !strings.HasPrefix(reason, StopReasonBudgetExhausted) {
		t.Fatalf("payload.stopReason 应为停靠说明，得到 %q", reason)
	}
	// 在途任务自然收尾：first 落 completed，run 保持驻留、second 仍不派发。
	h.completeSubmission("s-lead", sub, text, "分析完成")
	waitFor(t, "在途任务收尾", func() bool {
		return h.taskOf(run.ID, "first").Status == TaskCompleted
	})
	if h.store.Run(run.ID).Status != RunWaitingInput {
		t.Fatalf("全终态快照 + waiting_input 不得 fold，得到 %s", h.store.Run(run.ID).Status)
	}
	if task := h.taskOf(run.ID, "second"); task.Status != TaskPlanned {
		t.Fatalf("停靠期间 second 不得被派发，得到 %s", task.Status)
	}
	if _, ok := h.queue.firstEntry("s-dev"); ok {
		t.Fatal("预算耗尽后不得再向 second 的会话入队")
	}
}

// TestBudgetResumeRaisesCeilingToNextThreshold Resume 追加预算：落
// run_budget_resumed（ceiling=已派发数+BudgetUnits）+快照双写，新任务照常
// 派发直至下一阈值再次停靠；全终态后的 Resume 走 PlanResume 折叠终局。
func TestBudgetResumeRaisesCeilingToNextThreshold(t *testing.T) {
	h := newBudgetHarness(t)
	run := h.createRun(t, ModePipeline, budgetPlan())
	sub1, text1 := h.waitForSubmission(t, "s-lead", "first 派发")
	waitFor(t, "首阈值停靠", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })

	resumed, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("预算停靠后 Resume 应可达: %v", err)
	}
	if resumed.Status != RunDispatching {
		t.Fatalf("Resume 后应回执行态，得到 %s", resumed.Status)
	}
	if resumed.BudgetCeiling != 2 {
		t.Fatalf("追加上限应为 已派发1+预算1=2，得到 %d", resumed.BudgetCeiling)
	}
	if resumed.StopReason != "" {
		t.Fatalf("续跑应清停靠原因，得到 %q", resumed.StopReason)
	}
	// journal-first：run_budget_resumed 已落账（ceiling/units/used）。
	resumes := h.eventsOfType(run.ID, EventRunBudgetResumed)
	if len(resumes) != 1 || payloadInt(resumes[0].Payload, "ceiling") != 2 {
		t.Fatalf("应有一条 ceiling=2 的 run_budget_resumed: %+v", resumes)
	}
	// 新上限内照常派发：second 入队（first 仍在途占 lead）。
	sub2, text2 := h.waitForSubmission(t, "s-dev", "second 派发")
	// 第二阈值（max(ceiling2,units1)=2）再次停靠。
	waitFor(t, "第二阈值停靠", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	if fresh := h.store.Run(run.ID); fresh.BudgetCeiling != 2 || !strings.HasPrefix(fresh.StopReason, StopReasonBudgetExhausted) {
		t.Fatalf("第二阈值停靠应保持 ceiling=2 与预算前缀: %+v", fresh)
	}
	// 全终态后 Resume：预算分支照常追加（ceiling=2+1=3），PlanResume 折叠终局。
	h.completeSubmission("s-lead", sub1, text1, "分析完成")
	h.completeSubmission("s-dev", sub2, text2, "实现完成")
	waitFor(t, "两任务收尾", func() bool {
		return h.taskOf(run.ID, "first").Status == TaskCompleted && h.taskOf(run.ID, "second").Status == TaskCompleted
	})
	final, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("全终态 Resume 应折叠终局: %v", err)
	}
	if final.Status != RunCompleted {
		t.Fatalf("全终态续跑应 fold completed，得到 %s", final.Status)
	}
	resumes = h.eventsOfType(run.ID, EventRunBudgetResumed)
	if len(resumes) != 2 || payloadInt(resumes[1].Payload, "ceiling") != 3 {
		t.Fatalf("第二次追加应为 ceiling=3: %+v", resumes)
	}
}

// TestBudgetRestartReplayKeepsCeiling 重启（RecoverRun 重放 journal）后：
// 已派发数按 journal 重算、ceiling 从快照+事件双载体还原，再次 Resume 的
// 上限推导与运行期一致（不漂移、不丢失）。
func TestBudgetRestartReplayKeepsCeiling(t *testing.T) {
	h := newBudgetHarness(t)
	run := h.createRun(t, ModePipeline, budgetPlan())
	sub1, text1 := h.waitForSubmission(t, "s-lead", "first 派发")
	waitFor(t, "首阈值停靠", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("首次 Resume: %v", err)
	}
	sub2, text2 := h.waitForSubmission(t, "s-dev", "second 派发")
	waitFor(t, "第二阈值停靠", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	// 两任务收尾至全终态：重启对账零迁移驻留（recovery 先例），不引入
	// 会话探活的额外变量。
	h.completeSubmission("s-lead", sub1, text1, "分析完成")
	h.completeSubmission("s-dev", sub2, text2, "实现完成")
	waitFor(t, "全终态", func() bool {
		return h.taskOf(run.ID, "first").Status == TaskCompleted && h.taskOf(run.ID, "second").Status == TaskCompleted
	})

	// 重启对账：dispatchCount 按 journal 重放重建（与运行期增量同口径）。
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("重启对账失败: %v", err)
	}
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	used := state.dispatchCount
	state.mu.Unlock()
	if used != 2 {
		t.Fatalf("重启后已派发数应按 journal 重算为 2，得到 %d", used)
	}
	if fresh := h.store.Run(run.ID); fresh.BudgetCeiling != 2 || fresh.Status != RunWaitingInput {
		t.Fatalf("重启后 ceiling 应保持 2、驻留 waiting_input: %+v", fresh)
	}
	// 重启后的 Resume 与运行期同推导：ceiling=2+1=3（journal 事实为准）。
	final, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("重启后 Resume: %v", err)
	}
	if final.BudgetCeiling != 3 {
		t.Fatalf("重启后追加上限应为 3（与运行期一致，不漂移），得到 %d", final.BudgetCeiling)
	}
	if final.Status != RunCompleted {
		t.Fatalf("全终态续跑应折叠 completed，得到 %s", final.Status)
	}
}

// TestBudgetZeroOnlyCounts budget=0（§4.2 默认）只统计不暂停：全任务正常
// 派发收尾折叠，ComputeRunStats 给出完整计数。
func TestBudgetZeroOnlyCounts(t *testing.T) {
	h := newDispatchHarness(t) // DefaultPolicy：BudgetUnits=0
	run := h.createRun(t, ModePipeline, budgetPlan())
	sub1, text1 := h.waitForSubmission(t, "s-lead", "first 派发")
	sub2, text2 := h.waitForSubmission(t, "s-dev", "second 派发")
	h.completeSubmission("s-lead", sub1, text1, "分析完成")
	h.completeSubmission("s-dev", sub2, text2, "实现完成")
	waitFor(t, "全终态折叠", func() bool { return h.store.Run(run.ID).Status == RunCompleted })

	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	fresh := h.store.Run(run.ID)
	stats := ComputeRunStats(events, fresh, h.team.Policy, time.Now())
	if stats.DispatchCount != 2 {
		t.Fatalf("派发计数应为 2，得到 %d", stats.DispatchCount)
	}
	if stats.Budget.Units != 0 || stats.Budget.Exhausted || stats.Budget.Used != 2 {
		t.Fatalf("budget=0 只统计: %+v", stats.Budget)
	}
	if stats.Coverage != StatsCoverageFull {
		t.Fatalf("新事件已全部盖章，覆盖度应为 full，得到 %s", stats.Coverage)
	}
	if stats.RunElapsedMs <= 0 {
		t.Fatalf("Run 耗时应可计，得到 %d", stats.RunElapsedMs)
	}
	for _, entry := range stats.Tasks {
		if entry.ElapsedMs <= 0 {
			t.Fatalf("任务 %s 耗时应可计: %+v", entry.TaskID, entry)
		}
	}
	if stats.Tokens != nil {
		t.Fatalf("Tokens 必须恒 nil: %+v", stats.Tokens)
	}
}

// TestBudgetRebuildFromJournalRestoresCeiling team-runs.json 损坏后的
// journal 重建（§8.1）：run_waiting_input(payload.stopReason) 还原停靠语义、
// run_budget_resumed 回填 BudgetCeiling（单调取高），不依赖快照。
func TestBudgetRebuildFromJournalRestoresCeiling(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(sampleTeam("t1")); err != nil {
		t.Fatal(err)
	}
	// 两次追加（ceiling 2 → 3，单调）：重放取高防漂移。
	events := []*Event{
		{ID: "e1", RunID: "rb", Type: EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": "t1", "goal": "预算重建", "mode": "pipeline"}, CreatedAt: statsBase},
		{ID: "e2", RunID: "rb", Type: EventTaskDispatching, Source: "runner",
			Payload: map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}, CreatedAt: statsBase.Add(time.Second)},
		{ID: "e3", RunID: "rb", Type: EventRunWaitingInput, Source: "runner",
			Payload: map[string]any{"units": jsoncompat.Num(1), "used": jsoncompat.Num(1),
				"stopReason": budgetStopReason(1, 1)}, CreatedAt: statsBase.Add(2 * time.Second)},
		{ID: "e4", RunID: "rb", Type: EventRunBudgetResumed, Source: "user",
			Payload: map[string]any{"ceiling": jsoncompat.Num(2), "units": jsoncompat.Num(1), "used": jsoncompat.Num(1)}, CreatedAt: statsBase.Add(3 * time.Second)},
		{ID: "e5", RunID: "rb", Type: EventTaskDispatching, Source: "runner",
			Payload: map[string]any{"taskId": "b", "attempt": jsoncompat.Num(1)}, CreatedAt: statsBase.Add(4 * time.Second)},
		{ID: "e6", RunID: "rb", Type: EventRunWaitingInput, Source: "runner",
			Payload: map[string]any{"units": jsoncompat.Num(1), "used": jsoncompat.Num(2),
				"stopReason": budgetStopReason(2, 2)}, CreatedAt: statsBase.Add(5 * time.Second)},
		{ID: "e7", RunID: "rb", Type: EventRunBudgetResumed, Source: "user",
			Payload: map[string]any{"ceiling": jsoncompat.Num(3), "units": jsoncompat.Num(1), "used": jsoncompat.Num(2)}, CreatedAt: statsBase.Add(6 * time.Second)},
	}
	if err := os.WriteFile(runsPath, []byte("不是 JSON{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if !store.RunsLedgerCorrupt() {
		t.Fatal("前置失败：账本应标记损坏")
	}
	store.RebuildRunsFromJournal(events)
	run := store.Run("rb")
	if run == nil {
		t.Fatal("重建后 Run 应存在")
	}
	if run.Status != RunWaitingInput {
		t.Fatalf("重建应还原 waiting_input 停靠，得到 %s", run.Status)
	}
	if !strings.HasPrefix(run.StopReason, StopReasonBudgetExhausted) {
		t.Fatalf("重建应按 payload.stopReason 还原预算停靠，得到 %q", run.StopReason)
	}
	if run.BudgetCeiling != 3 {
		t.Fatalf("重建应回填单调最高的 ceiling=3，得到 %d", run.BudgetCeiling)
	}
	// 重建结果落盘可重开（budgetCeiling 键持久化）。
	reopened := LoadStore(teamsPath, runsPath)
	if got := reopened.Run("rb"); got == nil || got.BudgetCeiling != 3 {
		t.Fatalf("重开后 ceiling 应持久化为 3: %+v", got)
	}
	// 快照 JSON 里 budgetCeiling 键存在（四处同步的序列化面；账本为
	// Python 风格缩进，冒号后带空格）。
	if raw, err := os.ReadFile(runsPath); err != nil || !strings.Contains(string(raw), `"budgetCeiling": 3`) {
		t.Fatalf("team-runs.json 应含 budgetCeiling:3: %v %s", err, raw)
	}
}

func TestComputeRunStatsWaitingInputExcludesIdle(t *testing.T) {
	events := []*Event{
		statsEvent("r1", EventRunCreated, statsBase, nil),
		statsEvent("r1", EventTaskDispatching, statsBase.Add(time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventRunWaitingInput, statsBase.Add(60*time.Second), nil),
		statsEvent("r1", EventTaskFailed, statsBase.Add(90*time.Second),
			map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
	}
	now := statsBase.Add(11 * time.Hour)
	for _, status := range []string{RunWaitingInput, RunPaused} {
		run := &Run{ID: "r1", Status: status, Tasks: []Task{{ID: "a", Status: TaskFailed}}}
		if stats := ComputeRunStats(events, run, DefaultPolicy(), now); stats.RunElapsedMs != 90_000 {
			t.Fatalf("%s 且无在途任务时耗时应止于最后一条事件 90000ms，得到 %d", status, stats.RunElapsedMs)
		}
	}
	// 停靠期间仍有任务在跑：耗时继续按 now 增长。
	run := &Run{ID: "r1", Status: RunWaitingInput, Tasks: []Task{{ID: "a", Status: TaskRunning}}}
	if stats := ComputeRunStats(events, run, DefaultPolicy(), now); stats.RunElapsedMs != (11 * time.Hour).Milliseconds() {
		t.Fatalf("有在途任务时耗时应取 now，得到 %d", stats.RunElapsedMs)
	}
}

func TestComputeRunStatsResumedRunExcludesParkedIdle(t *testing.T) {
	// 跑 5 分钟停靠，2 小时后续跑（Resume 不写事件，以下一次派发为界），再跑 1 分钟完成：
	// 续跑后与终局后都只计 6 分钟，不随 resume 跳回墙钟。
	at := func(d time.Duration) time.Time { return statsBase.Add(d) }
	events := []*Event{
		statsEvent("r1", EventRunCreated, at(0), nil),
		statsEvent("r1", EventTaskDispatching, at(time.Second), map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventTaskStopped, at(5*time.Minute), map[string]any{"taskId": "a", "attempt": jsoncompat.Num(1)}),
		statsEvent("r1", EventRunWaitingInput, at(5*time.Minute), nil),
		statsEvent("r1", EventTaskDispatching, at(2*time.Hour+5*time.Minute), map[string]any{"taskId": "a", "attempt": jsoncompat.Num(2)}),
	}
	running := &Run{ID: "r1", Status: RunDispatching, Tasks: []Task{{ID: "a", Status: TaskRunning}}}
	if stats := ComputeRunStats(events, running, DefaultPolicy(), at(2*time.Hour+5*time.Minute+30*time.Second)); stats.RunElapsedMs != (5*time.Minute + 30*time.Second).Milliseconds() {
		t.Fatalf("续跑后应扣除停靠空闲，得到 %d", stats.RunElapsedMs)
	}
	events = append(events,
		statsEvent("r1", EventTaskCompleted, at(2*time.Hour+6*time.Minute), map[string]any{"taskId": "a", "attempt": jsoncompat.Num(2)}),
		statsEvent("r1", EventRunCompleted, at(2*time.Hour+6*time.Minute), nil))
	done := &Run{ID: "r1", Status: RunCompleted, Tasks: []Task{{ID: "a", Status: TaskCompleted}}}
	if stats := ComputeRunStats(events, done, DefaultPolicy(), at(5*time.Hour)); stats.RunElapsedMs != (6 * time.Minute).Milliseconds() {
		t.Fatalf("终局后同样扣除停靠空闲，得到 %d", stats.RunElapsedMs)
	}
	// 停靠中被取消：停靠区间同样不算执行时间。
	cancelled := append(events[:5:5], statsEvent("r1", EventRunCancelled, at(3*time.Hour), nil))
	if stats := ComputeRunStats(cancelled[:4], &Run{ID: "r1", Status: RunWaitingInput, Tasks: []Task{{ID: "a", Status: TaskStopped}}}, DefaultPolicy(), at(3*time.Hour)); stats.RunElapsedMs != (5 * time.Minute).Milliseconds() {
		t.Fatalf("停靠中耗时止于最后事件，得到 %d", stats.RunElapsedMs)
	}
}
