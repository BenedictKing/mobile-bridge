package team

// review_order_test.go 是阶段 0 的审查顺序与结论回归（docs/
// team-demand-queue-plan.md §8 阶段 0）：2026-11-07 的真实 Run 暴露
// 「实现 17:19 才完成、审查 17:15 已派发」与「unknown 被摘要成通过」两类
// 交付风险。这里锁住三道机械防线：
//   - autoPlan 动作不得同批混排执行与审查、实现未完成不接审查动作（解析期
//     + 执行期同口径；显式 Plan 走 ParsePlan 的 DAG 依赖不受影响）；
//   - 审查任务 completed 但无裁决标记（unknown）时，编排器同批追加一次
//     revfix 修复任务，第二次仍 unknown 不再派单；
//   - 汇总（Reduction）任务的派发文本注入结构化交付事实，unknown 只能表述
//     为「未取得裁决」，没有审查任务必须写明未安排。

import (
	"strings"
	"testing"
)

// reviewOrderRunFixture 构造审查顺序用 Run：lead 已完成（动作来源）、impl
// 处于指定状态、rev 的审查任务已按计划存在。
func reviewOrderRunFixture(implStatus string) *Run {
	return &Run{
		ID: "run-1", TeamID: "team-1", Status: RunDispatching, Round: 1, AutoPlan: true,
		Tasks: []Task{
			{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0},
			{ID: "impl", MemberID: "dev", Status: implStatus, Ordinal: 1},
		},
	}
}

func reviewDelegateEntry(member string, review string) string {
	return `{"type":"delegate","sourceTaskId":"lead","round":1,"member":"` + member + `","task":"审查最终改动"` + review + `}`
}

// TestValidateActionsMixedBatchNeedsDependencyCoverage 同批混排执行与审查：
// 未用 dependsOn 覆盖全部执行任务时整体拒绝（审查抢跑的根因形态——二者都
// 只依赖根 Lead 任务）；全覆盖时放行（阶段 2 起，docs/
// team-demand-queue-plan.md §8）。
func TestValidateActionsMixedBatchNeedsDependencyCoverage(t *testing.T) {
	team := testTeam()
	run := reviewOrderRunFixture(TaskCompleted)
	// 未声明依赖的混排：拒绝。
	inner := actionJSON(
		reviewDelegateEntry("rev", `,"review":true`),
		delegateEntry("dev", "实现删除逻辑"),
	)
	payload, violations := decodeActionPayload(inner)
	if payload == nil {
		t.Fatalf("内文必须可解码，实际 %v", violations)
	}
	actions, violations := ValidateActions(payload, team, run, actionsPolicyFixture())
	if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), "未用 dependsOn 覆盖执行任务") {
		t.Fatalf("未覆盖的混排批次应被拒绝，实际 violations=%v actions=%v", violations, actions)
	}
	// 覆盖全部执行任务的混排：放行（一条审查依赖两条实现）。
	covered := actionJSON(
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"实现 Web 删除","key":"web"}`,
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"实现桌面删除","key":"desktop"}`,
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"rev","task":"审查两端最终改动","review":true,"writeLock":"none","dependsOn":["web","desktop"]}`,
	)
	payload, violations = decodeActionPayload(covered)
	if payload == nil {
		t.Fatalf("内文必须可解码，实际 %v", violations)
	}
	actions, violations = ValidateActions(payload, team, run, actionsPolicyFixture())
	if len(violations) != 0 {
		t.Fatalf("全覆盖混排应放行，实际 %v", violations)
	}
	if len(actions) != 3 {
		t.Fatalf("应产出三条动作: %+v", actions)
	}
}

// TestValidateActionsRejectsReviewWhileExecutionUnfinished 实现任务未终态时
// 不接受新的审查动作（回归 2026-11-07：实现 17:19 才完成，审查绝不在 17:15
// 派发）。Lead 后续任务在途不拦（编排载体），审查动作自身也不计入执行。
func TestValidateActionsRejectsReviewWhileExecutionUnfinished(t *testing.T) {
	team := testTeam()
	for _, status := range []string{TaskPlanned, TaskReady, TaskDispatching, TaskRunning} {
		run := reviewOrderRunFixture(status)
		run.Tasks = append(run.Tasks, Task{ID: "lead-d1-0", MemberID: "lead", Status: TaskPlanned, Ordinal: 2})
		payload, violations := decodeActionPayload(actionJSON(reviewDelegateEntry("rev", `,"review":true`)))
		if payload == nil {
			t.Fatalf("内文必须可解码，实际 %v", violations)
		}
		_, violations = ValidateActions(payload, team, run, actionsPolicyFixture())
		if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), "未用 dependsOn 覆盖执行任务") {
			t.Fatalf("实现 %s 时未覆盖审查动作应被拒绝，实际 %v", status, violations)
		}
	}
}

// TestValidateActionsAcceptsReviewAfterImplementationDone 实现全部终态后，
// 单独成批的审查动作通过（含失败终态的实现——失败分支不再阻塞审查）。
func TestValidateActionsAcceptsReviewAfterImplementationDone(t *testing.T) {
	team := testTeam()
	for _, status := range []string{TaskCompleted, TaskFailed, TaskStopped} {
		run := reviewOrderRunFixture(status)
		payload, violations := decodeActionPayload(actionJSON(reviewDelegateEntry("rev", `,"review":true`)))
		if payload == nil {
			t.Fatalf("内文必须可解码，实际 %v", violations)
		}
		actions, violations := ValidateActions(payload, team, run, actionsPolicyFixture())
		if len(violations) != 0 {
			t.Fatalf("实现 %s 后单独审查动作应通过，实际 %v", status, violations)
		}
		if len(actions) != 1 || actions[0].Review == nil || !*actions[0].Review {
			t.Fatalf("应产出一条审查动作: %+v", actions)
		}
	}
}

// TestParsePlanExplicitReviewDependencyUnaffected 显式 Plan 的 DAG 依赖不受
// 阶段 0 保守屏障影响：implement 未完成时 review 仍可按 dependsOn 计划。
func TestParsePlanExplicitReviewDependencyUnaffected(t *testing.T) {
	plan, err := ParsePlan(testTeam(), "实现并审查", ModePipeline, "", pipelinePlan())
	if err != nil {
		t.Fatalf("显式计划不应受审查顺序屏障影响: %v", err)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("三条任务应全部保留: %+v", plan.Tasks)
	}
}

// TestValidateExecutableBlocksReviewWhileExecutionRunning 执行期复判：解析
// 通过的审查动作在执行时若实现任务仍在途，必须拒绝（崩溃窗口/策略变更兜底）。
func TestValidateExecutableBlocksReviewWhileExecutionRunning(t *testing.T) {
	team := testTeam()
	run := reviewOrderRunFixture(TaskRunning)
	action := &Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "rev", Task: "审查最终改动", Review: reviewFlag(true),
		ActionKey: ActionKey("team-1", "run-1", "lead", 1, 0),
	}
	ok, reason := ValidateExecutable(run, team, team.Policy, action, &actionPrefetch{sessionLive: map[string]bool{"rev": true}}, nil)
	if ok || !strings.Contains(reason, "未被审查动作依赖覆盖") {
		t.Fatalf("实现运行中未覆盖审查动作应被拒绝，实际 ok=%v reason=%q", ok, reason)
	}
	// 显式依赖覆盖在途实现：放行。
	if ok, reason := ValidateExecutableWithDeps(run, team, team.Policy, &Action{
		Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1,
		Member: "rev", Task: "审查最终改动", Review: reviewFlag(true), DependsOn: []string{"impl"},
		ActionKey: ActionKey("team-1", "run-1", "lead", 1, 1),
	}, &actionPrefetch{sessionLive: map[string]bool{"rev": true}}, nil, []string{"impl"}); !ok {
		t.Fatalf("覆盖在途实现的审查动作应可执行，实际 %q", reason)
	}
	run.Tasks[1].Status = TaskCompleted
	if ok, reason := ValidateExecutable(run, team, team.Policy, action, &actionPrefetch{sessionLive: map[string]bool{"rev": true}}, nil); !ok {
		t.Fatalf("实现完成后审查动作应可执行，实际 %q", reason)
	}
}

// ---- 审查格式修复（revfix）----

// reviewRepairRunFixture 构造修复用 Run：审查任务在途，即将以 unknown 裁决
// 结算。
func reviewRepairRunFixture() *Run {
	return &Run{
		ID: "run-rev", TeamID: "team-1", Goal: "实现并审查", Mode: ModePipeline,
		Status: RunDispatching, Round: 1,
		Tasks: []Task{
			{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0},
			{ID: "impl", MemberID: "dev", Status: TaskCompleted, Ordinal: 1, Result: "实现完成"},
			{ID: "review", MemberID: "rev", Status: TaskRunning, Ordinal: 2, WriteLock: TaskWriteLockNone},
		},
	}
}

// TestBuildReviewRepairTaskBounds 修复任务的构造边界：只补一次、修复任务不
// 再派生、非审查任务不补、ID 超长不补。
func TestBuildReviewRepairTaskBounds(t *testing.T) {
	team := testTeam()
	run := reviewRepairRunFixture()
	repair, ok := buildReviewRepairTask(run, team, "review")
	if !ok {
		t.Fatal("unknown 裁决的审查任务应构造修复任务")
	}
	if repair.ID != "revfix-review-1" || repair.MemberID != "rev" || repair.WriteLock != TaskWriteLockNone {
		t.Fatalf("修复任务形态不符: %+v", repair)
	}
	if len(repair.DependsOn) != 1 || repair.DependsOn[0] != "review" {
		t.Fatalf("修复任务应依赖原审查任务: %+v", repair.DependsOn)
	}
	if repair.Review == nil || !*repair.Review {
		t.Fatalf("修复任务应保持审查语义: %+v", repair.Review)
	}
	// 一次后到上限：追加已有修复任务再构造 → false。
	run.Tasks = append(run.Tasks, repair)
	if _, ok := buildReviewRepairTask(run, team, "review"); ok {
		t.Fatal("修复只允许一次，第二次不得再构造")
	}
	// 修复任务本身不再派生修复（防 revfix-revfix-* 无限链）。
	if _, ok := buildReviewRepairTask(run, team, repair.ID); ok {
		t.Fatal("修复任务不得再派生修复")
	}
	// 非审查任务不补。
	if _, ok := buildReviewRepairTask(run, team, "impl"); ok {
		t.Fatal("非审查任务不得构造修复任务")
	}
	// ID 超长（64 上限）不补：确定性收场为 unknown。
	long := &Run{
		ID: "run-long", TeamID: "team-1", Status: RunDispatching,
		Tasks: []Task{{ID: strings.Repeat("r", 60), MemberID: "rev", Status: TaskRunning, Ordinal: 0}},
	}
	if _, ok := buildReviewRepairTask(long, team, long.Tasks[0].ID); ok {
		t.Fatal("派生 ID 超长时不得构造修复任务")
	}
}

// TestCompleteTaskPlansBoundedReviewRepair 集成面：审查任务以 unknown 结算
// 时，修复任务与 completed 事实同批落账（事件带幂等键与 reviewRepairFor）；
// 修复任务再以 unknown 结算不再派生第三轮。
func TestCompleteTaskPlansBoundedReviewRepair(t *testing.T) {
	h := newActionsExecHarness(t)
	h.team.Policy.AutoDispatch = false // 不启动执行循环，手工结算
	run := reviewRepairRunFixture()
	run.TeamID = h.team.ID
	state := h.saveExecRun(t, run)

	result := &TaskResult{SessionID: "s-rev", Result: "我看过改动，没什么问题。"}
	if !h.service.completeTask(state, run.ID, "review", result) {
		t.Fatal("审查任务应落 completed")
	}
	fresh := h.store.Run(run.ID)
	review := findTaskIn(fresh, "review")
	if review.Status != TaskCompleted || review.ResultDetail.ReviewVerdict != "" {
		t.Fatalf("无标记回复应落 completed 且裁决为空（unknown）: %+v", review)
	}
	repair := findTaskIn(fresh, "revfix-review-1")
	if repair == nil {
		t.Fatal("unknown 裁决应同批追加 revfix-review-1 修复任务")
	}
	if repair.Prompt != reviewRepairPrompt || repair.Status != TaskPlanned {
		t.Fatalf("修复任务形态不符: %+v", repair)
	}
	var plannedEvent *Event
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	for _, event := range events {
		if event == nil || event.RunID != run.ID || event.Type != EventTaskPlanned {
			continue
		}
		if payloadString(event.Payload, "taskId") == "revfix-review-1" {
			plannedEvent = event
		}
	}
	if plannedEvent == nil {
		t.Fatal("修复任务应有 task_planned 事件")
	}
	if payloadString(plannedEvent.Payload, "reviewRepairFor") != "review" ||
		payloadString(plannedEvent.Payload, "prompt") != reviewRepairPrompt ||
		payloadString(plannedEvent.Payload, "writeLock") != TaskWriteLockNone {
		t.Fatalf("修复事件 payload 应可重建完整任务: %+v", plannedEvent.Payload)
	}
	if plannedEvent.IdempotencyKey == "" {
		t.Fatal("修复事件必须携带幂等键")
	}

	// 修复任务再以 unknown 结算：不再派生（上限 1），裁决保持 unknown。
	second := &TaskResult{SessionID: "s-rev", Result: "还是没有输出标记。"}
	if !h.service.completeTask(state, run.ID, "revfix-review-1", second) {
		t.Fatal("修复任务应落 completed")
	}
	fresh = h.store.Run(run.ID)
	if findTaskIn(fresh, "revfix-revfix-review-1-1") != nil {
		t.Fatal("修复任务不得再派生修复")
	}
	if findTaskIn(fresh, "revfix-review-2") != nil {
		t.Fatal("修复只允许一次")
	}
}

// TestCompleteTaskPassedVerdictPlansNoRepair 显式裁决（passed/failed）不触发
// 修复回路。
func TestCompleteTaskPassedVerdictPlansNoRepair(t *testing.T) {
	h := newActionsExecHarness(t)
	run := reviewRepairRunFixture()
	run.TeamID = h.team.ID
	state := h.saveExecRun(t, run)
	result := &TaskResult{SessionID: "s-rev", Result: "没问题", ReviewVerdict: ReviewVerdictPassed}
	if !h.service.completeTask(state, run.ID, "review", result) {
		t.Fatal("审查任务应落 completed")
	}
	if findTaskIn(h.store.Run(run.ID), "revfix-review-1") != nil {
		t.Fatal("有显式裁决时不得创建修复任务")
	}
}

// ---- 汇总任务的交付事实简报 ----

// TestBuildDeliveryBriefingMatchesMachineVerdicts 简报逐条对应机器裁决：
// unknown→未取得裁决、passed→已通过、无审查→未安排独立审查。
func TestBuildDeliveryBriefingMatchesMachineVerdicts(t *testing.T) {
	team := testTeam()
	run := reviewRepairRunFixture()
	text := BuildDeliveryBriefing(team, run)
	if !strings.Contains(text, "机器裁决=未取得裁决") {
		t.Fatalf("在途审查应报未取得裁决:\n%s", text)
	}
	run.Tasks[2].Status = TaskCompleted
	run.Tasks[2].ResultDetail = &TaskResult{Result: "通过", ReviewVerdict: ReviewVerdictPassed}
	if text = BuildDeliveryBriefing(team, run); !strings.Contains(text, "机器裁决=已通过") {
		t.Fatalf("显式 passed 应报已通过:\n%s", text)
	}
	run.Tasks[2].ResultDetail = &TaskResult{Result: "不通过", ReviewVerdict: ReviewVerdictFailed}
	if text = BuildDeliveryBriefing(team, run); !strings.Contains(text, "机器裁决=未通过") {
		t.Fatalf("显式 failed 应报未通过:\n%s", text)
	}
	noReview := &Run{ID: "run-2", TeamID: "team-1", Status: RunDispatching,
		Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0}}}
	if text = BuildDeliveryBriefing(team, noReview); !strings.Contains(text, "未安排独立审查") {
		t.Fatalf("无审查任务应明示未安排:\n%s", text)
	}
}

// TestReductionDispatchCarriesDeliveryFacts 派发文本组合：Reduction 任务的
// 简报带交付事实，普通任务不带。
func TestReductionDispatchCarriesDeliveryFacts(t *testing.T) {
	team := testTeam()
	run := reviewRepairRunFixture()
	run.Tasks[2].Status = TaskCompleted
	run.Tasks[2].ResultDetail = &TaskResult{Result: "看过了", ReviewVerdict: ""} // unknown
	reduction := Task{ID: "lead-auto-reduce", MemberID: "lead", Prompt: autoReductionPrompt,
		Reduction: true, Status: TaskPlanned, WriteLock: TaskWriteLockNone}
	composed := composeTaskDispatchMessageWithBriefing(team, run, &reduction, team.Policy, "")
	if !strings.Contains(composed.text, "[Team delivery]") || !strings.Contains(composed.text, "未取得裁决") {
		t.Fatalf("汇总派发文本应含结构化交付事实:\n%s", composed.text)
	}
	normal := Task{ID: "impl2", MemberID: "dev", Prompt: "实现", Status: TaskPlanned}
	if composed := composeTaskDispatchMessageWithBriefing(team, run, &normal, team.Policy, ""); strings.Contains(composed.text, "[Team delivery]") {
		t.Fatalf("非汇总任务不得注入交付事实:\n%s", composed.text)
	}
}

// TestLeadBriefingTeachesReviewBatching Lead 简报必须告知审查单独成批规则，
// 否则 Lead 无从知道新校验（修复回路只会空转）。
func TestLeadBriefingTeachesReviewBatching(t *testing.T) {
	team := testTeam()
	run := &Run{ID: "run-b", TeamID: team.ID, AutoPlan: true, Status: RunDispatching}
	task := &Task{ID: "lead", MemberID: "lead", Status: TaskPlanned}
	briefing := BuildLeadBriefing(team, run, task, team.Policy)
	if !strings.Contains(briefing, "审查动作必须单独成批") {
		t.Fatalf("Lead 简报应包含审查成批规则:\n%s", briefing)
	}
}

// TestValidateActionsReviewerDefaultSemanticsOrder 未声明 review 的评审成员
// 委派按审查兼容（IsReviewTask 缺省口径），同样受顺序屏障约束。
func TestValidateActionsReviewerDefaultSemanticsOrder(t *testing.T) {
	team := testTeam()
	run := reviewOrderRunFixture(TaskRunning)
	payload, violations := decodeActionPayload(actionJSON(reviewDelegateEntry("rev", "")))
	if payload == nil {
		t.Fatalf("内文必须可解码，实际 %v", violations)
	}
	_, violations = ValidateActions(payload, team, run, actionsPolicyFixture())
	if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), "未用 dependsOn 覆盖执行任务") {
		t.Fatalf("缺省审查语义也应被顺序屏障拦截，实际 %v", violations)
	}
}

// TestRunHasUnfinishedExecutionExcludesOrchestration Lead 后续任务、汇总任务
// 与既有审查任务不算未完成执行——审查可与编排并行。
func TestRunHasUnfinishedExecutionExcludesOrchestration(t *testing.T) {
	team := testTeam()
	run := reviewOrderRunFixture(TaskCompleted)
	run.Tasks = append(run.Tasks,
		Task{ID: "lead-d1-0", MemberID: "lead", Status: TaskDispatching, Ordinal: 2},
		Task{ID: "actfix-lead-1", MemberID: "lead", Status: TaskPlanned, Ordinal: 3},
		Task{ID: "lead-auto-reduce", MemberID: "lead", Reduction: true, Status: TaskPlanned, Ordinal: 4},
		Task{ID: "review", MemberID: "rev", Status: TaskRunning, Ordinal: 5, WriteLock: TaskWriteLockNone},
	)
	if runHasUnfinishedExecution(team, run) {
		t.Fatal("编排与审查任务不算未完成执行")
	}
	run.Tasks[1].Status = TaskDispatching
	if !runHasUnfinishedExecution(team, run) {
		t.Fatal("实现任务在途必须算未完成执行")
	}
}
