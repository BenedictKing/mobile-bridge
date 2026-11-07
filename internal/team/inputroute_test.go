package team

// inputroute_test.go 覆盖输入路由器（docs/team-demand-queue-plan.md §4/§8
// 阶段 2）：无歧义输入自动派发（new_task/amend/follow_up）、多活跃 Run 停
// 靠 waiting_input、意图-确认顺序契约（routing 中间态）、崩溃恢复重泵对账、
// 优先输入先路由、工作项链接。

import (
	"path/filepath"
	"strings"
	"testing"
)

// newRouterHarness 构造带输入账本的服务基座（复用 actions 执行基座——不启
// 动循环，手工泵路由）。
func newRouterHarness(t *testing.T) *actionsExecHarness {
	t.Helper()
	h := newActionsExecHarness(t)
	dir := t.TempDir()
	inputJournal, err := OpenJournal(filepath.Join(dir, "team-inputs.jsonl"))
	if err != nil {
		t.Fatalf("输入 journal 打开失败: %v", err)
	}
	t.Cleanup(func() { inputJournal.Close() })
	h.service.SetInputs(LoadInputLedger(filepath.Join(dir, "team-inputs.json"), inputJournal))
	return h
}

func (h *actionsExecHarness) acceptInput(t *testing.T, id, text string) {
	t.Helper()
	if _, _, err := h.service.inputs.Accept(h.team.ID, id, text, nil, "digest", 64); err != nil {
		t.Fatalf("输入受理失败: %v", err)
	}
}

// TestRouterCreatesInitialRunForFirstInput 首条输入 → new_task（runId=inputID
// 的 autoPlan Run），输入 routed + 工作项链接。
func TestRouterCreatesInitialRunForFirstInput(t *testing.T) {
	h := newRouterHarness(t)
	h.acceptInput(t, "in-first", "给登录页补错误态")
	if routed := h.service.RoutePendingInputs(h.team.ID); routed != 1 {
		t.Fatalf("应确认 1 条路由，实际 %d", routed)
	}
	input := h.service.inputs.Get(h.team.ID, "in-first")
	if input.QueueStatus != InputRouted || input.RouteStatus != "new_task" {
		t.Fatalf("输入应 routed/new_task: %+v", input)
	}
	run := h.store.Run("in-first")
	if run == nil || !run.AutoPlan || len(run.Tasks) != 1 || run.Tasks[0].MemberID != h.team.LeadID {
		t.Fatalf("应创建初始 autoPlan Run: %+v", run)
	}
	if len(input.RunIDs) != 1 || input.RunIDs[0] != "in-first" {
		t.Fatalf("RunIDs 应记录路由产物: %+v", input.RunIDs)
	}
	if len(input.WorkItems) != 1 || input.WorkItems[0].RunID != "in-first" ||
		input.WorkItems[0].TaskIDs[0] != "lead" || input.WorkItems[0].SourceRevision != 1 {
		t.Fatalf("工作项应链接 lead 任务: %+v", input.WorkItems)
	}
}

// TestRouterAppendsWithActiveRunAndAmendsAfterTerminal 第二条输入追加为活跃
// Run 的 Lead 任务（follow_up）；Run 终态后新输入走 amend（parentRunId 血缘）。
func TestRouterAppendsWithActiveRunAndAmendsAfterTerminal(t *testing.T) {
	h := newRouterHarness(t)
	h.acceptInput(t, "in-1", "第一条")
	h.service.RoutePendingInputs(h.team.ID)
	h.acceptInput(t, "in-2", "补充要求")
	if routed := h.service.RoutePendingInputs(h.team.ID); routed != 1 {
		t.Fatalf("第二条应路由，实际 %d", routed)
	}
	followUp := h.service.inputs.Get(h.team.ID, "in-2")
	if followUp.RouteStatus != "follow_up" || followUp.TargetTaskID != ConversationTaskID("in-2") {
		t.Fatalf("追加路由不符: %+v", followUp)
	}
	run := h.store.Run("in-1")
	if task := findTaskIn(run, ConversationTaskID("in-2")); task == nil || task.MemberID != h.team.LeadID {
		t.Fatalf("活跃 Run 应追加 Lead 任务: %+v", run.Tasks)
	}
	// 终态后：新输入 amend。
	_, _ = h.service.Cancel(run.ID)
	h.acceptInput(t, "in-3", "修订要求")
	h.service.RoutePendingInputs(h.team.ID)
	amend := h.service.inputs.Get(h.team.ID, "in-3")
	if amend.RouteStatus != "amend" {
		t.Fatalf("终态后应走 amend: %+v", amend)
	}
	if amendRun := h.store.Run("in-3"); amendRun == nil || amendRun.ParentRunID != "in-1" {
		t.Fatalf("amend Run 应保留血缘: %+v", amendRun)
	}
}

// TestRouterParksOnMultipleActiveRuns 多个活跃 Run → waiting_input（原因可
// 见），收敛后重泵恢复。
func TestRouterParksOnMultipleActiveRuns(t *testing.T) {
	h := newRouterHarness(t)
	// 手工落两个活跃 Run（绕过会话化路径：直写快照）。
	for _, id := range []string{"run-a", "run-b"} {
		if err := h.store.SaveRun(&Run{ID: id, TeamID: h.team.ID, Goal: "g", Mode: ModeSingle,
			AutoPlan: true, Status: RunDispatching, Round: 1,
			Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskRunning, Ordinal: 0}}}); err != nil {
			t.Fatal(err)
		}
	}
	h.acceptInput(t, "in-x", "下一步做什么")
	h.service.RoutePendingInputs(h.team.ID)
	input := h.service.inputs.Get(h.team.ID, "in-x")
	if input.QueueStatus != InputWaitingInput || !strings.Contains(input.WaitingReason, "多个未结束的 Run") {
		t.Fatalf("应停靠并说明原因: %+v", input)
	}
	if h.store.Run("in-x") != nil {
		t.Fatal("停靠输入不得创建 Run")
	}
	// 收敛（取消其一）后重泵：恢复路由到剩余活跃 Run。
	if _, err := h.service.Cancel("run-b"); err != nil {
		t.Fatal(err)
	}
	if routed := h.service.RoutePendingInputs(h.team.ID); routed != 1 {
		t.Fatalf("收敛后应恢复路由，实际 %d", routed)
	}
	input = h.service.inputs.Get(h.team.ID, "in-x")
	if input.QueueStatus != InputRouted || input.RouteStatus != "follow_up" || input.WaitingReason != "" {
		t.Fatalf("恢复路由后应清除停靠: %+v", input)
	}
}

// TestRouterPriorityOrdersRouting 「立即发送」的输入先路由（接受序在后）。
func TestRouterPriorityOrdersRouting(t *testing.T) {
	h := newRouterHarness(t)
	h.acceptInput(t, "in-normal", "普通输入")
	h.acceptInput(t, "in-urgent", "紧急输入")
	if _, _, err := h.service.inputs.Prioritize(h.team.ID, "in-urgent", "op-prio", 1); err != nil {
		t.Fatal(err)
	}
	h.service.RoutePendingInputs(h.team.ID)
	urgent := h.service.inputs.Get(h.team.ID, "in-urgent")
	if urgent.QueueStatus != InputRouted || urgent.RunIDs[0] != "in-urgent" {
		t.Fatalf("优先输入应先路由为初始 Run: %+v", urgent)
	}
	// 普通输入随后追加到活跃 Run。
	normal := h.service.inputs.Get(h.team.ID, "in-normal")
	if normal.QueueStatus != InputRouted || normal.RouteStatus != "follow_up" {
		t.Fatalf("普通输入按序追加: %+v", normal)
	}
}

// TestRouterResumesRoutingAfterRestart 崩溃窗口（intent 已落、Run 未建成）
// 重启后重泵补齐：runID 确定性使对账幂等，输入终达 routed。
func TestRouterResumesRoutingAfterRestart(t *testing.T) {
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "team-inputs.jsonl")
	inputJournal, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := LoadInputLedger(filepath.Join(dir, "team-inputs.json"), inputJournal)
	h := newActionsExecHarness(t)
	h.service.SetInputs(ledger)
	h.acceptInput(t, "in-crash", "崩溃前接受的输入")
	// 只落意图（模拟在 intent 与创建之间的崩溃）。
	if err := ledger.MarkRoutingIntent(h.team.ID, "in-crash", "new_task", "in-crash", "lead"); err != nil {
		t.Fatal(err)
	}
	inputJournal.Close()
	// 重启：新账本 + 新服务（同一 store/journal dataDir 语义）。
	reopened, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	h2 := newActionsExecHarness(t)
	h2.service.SetInputs(LoadInputLedger(filepath.Join(dir, "team-inputs.json"), reopened))
	// 保存团队到第二个 store（不同 harness 各自内存 store）——直接把 run
	// 建在 h2 的 store 上。
	if err := h2.store.SaveTeam(h2.team); err != nil {
		t.Fatal(err)
	}
	if routed := h2.service.RoutePendingInputs(h2.team.ID); routed != 1 {
		t.Fatalf("重泵应补齐崩溃窗口，实际 %d", routed)
	}
	input := h2.service.inputs.Get(h2.team.ID, "in-crash")
	if input == nil || input.QueueStatus != InputRouted {
		t.Fatalf("崩溃窗口应补齐为 routed: %+v", input)
	}
	if run := h2.store.Run("in-crash"); run == nil || !run.AutoPlan {
		t.Fatalf("对账应建成初始 Run: %+v", run)
	}
}

// TestRouterHonorsRevisions 路由使用输入的当前修订正文（编辑后再路由）。
func TestRouterHonorsRevisions(t *testing.T) {
	h := newRouterHarness(t)
	h.acceptInput(t, "in-rev", "初版")
	if _, _, err := h.service.inputs.Revise(h.team.ID, "in-rev", "op-r1", "修订后的正文", nil, 1); err != nil {
		t.Fatal(err)
	}
	h.service.RoutePendingInputs(h.team.ID)
	run := h.store.Run("in-rev")
	if run == nil || run.Goal != "修订后的正文" {
		t.Fatalf("路由应使用当前修订: %+v", run)
	}
	input := h.service.inputs.Get(h.team.ID, "in-rev")
	if input.WorkItems[0].SourceRevision != 2 {
		t.Fatalf("工作项应记录来源修订: %+v", input.WorkItems)
	}
}
