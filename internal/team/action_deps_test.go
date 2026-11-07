package team

// action_deps_test.go 覆盖同批次 key/dependsOn 依赖（docs/
// team-demand-queue-plan.md §4，阶段 2）：批级校验（key 唯一/不与既有任务
// 同名/引用可解析/无环）、派生任务的依赖映射与「显式依赖优先于旧的等全部
// 在途缺省」、同批拓扑执行顺序，以及 passActions 直驱的「实现 + 依赖审查」
// 全链路（审查的 dependsOn 指向两条实现，派生任务不抢跑）。

import (
	"strings"
	"testing"
)

// batchDepsRunFixture 构造 key/dependsOn 用 Run：lead 已完成（动作来源）。
func batchDepsRunFixture() *Run {
	return &Run{
		ID: "run-deps", TeamID: "team-1", Status: RunDispatching, Round: 1, AutoPlan: true,
		Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0}},
	}
}

func TestValidateActionBatchKeys(t *testing.T) {
	team := testTeam()
	policy := actionsPolicyFixture()
	delegate := func(extra string) string {
		return `{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"做事"` + extra + `}`
	}
	cases := []struct {
		name       string
		inner      string
		wantVioSub string
	}{
		{name: "合法同批依赖", inner: actionJSON(
			delegate(`,"key":"web"`),
			`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"rev","task":"审查","review":true,"writeLock":"none","dependsOn":["web"]}`,
		), wantVioSub: ""},
		{name: "依赖既有任务 ID", inner: actionJSON(
			delegate(`,"dependsOn":["lead"]`),
		), wantVioSub: ""},
		{name: "key 重复", inner: actionJSON(delegate(`,"key":"a"`), delegate(`,"key":"a"`)), wantVioSub: "key 重复"},
		{name: "key 与既有任务同名", inner: actionJSON(delegate(`,"key":"lead"`)), wantVioSub: "与本 Run 既有任务 ID 同名"},
		{name: "依赖无法解析", inner: actionJSON(delegate(`,"dependsOn":["nobody"]`)), wantVioSub: "无法解析"},
		{name: "同批环", inner: actionJSON(
			delegate(`,"key":"a","dependsOn":["b"]`),
			delegate(`,"key":"b","dependsOn":["a"]`),
		), wantVioSub: "循环"},
		{name: "自依赖", inner: actionJSON(delegate(`,"key":"a","dependsOn":["a"]`)), wantVioSub: "循环"},
		{name: "request_context 不接受 key", inner: actionJSON(
			`{"type":"request_context","sourceTaskId":"lead","round":1,"member":"rev","task":"取回","fromTaskId":"lead","key":"q"}`,
		), wantVioSub: "不支持 key 字段"},
		{name: "summarize 不接受 dependsOn", inner: actionJSON(
			`{"type":"summarize","sourceTaskId":"lead","round":2,"dependsOn":["lead"]}`,
		), wantVioSub: "不支持 dependsOn 字段"},
		{name: "key 形态非法", inner: actionJSON(delegate(`,"key":"坏 key!"`)), wantVioSub: "key 无效"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := batchDepsRunFixture()
			payload, violations := decodeActionPayload(testCase.inner)
			if payload == nil {
				t.Fatalf("内文必须可解码，实际 %v", violations)
			}
			_, violations = ValidateActions(payload, team, run, policy)
			if testCase.wantVioSub == "" {
				if len(violations) != 0 {
					t.Fatalf("应校验通过，实际 %v", violations)
				}
				return
			}
			if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), testCase.wantVioSub) {
				t.Fatalf("期望 violation 含 %q，实际 %v", testCase.wantVioSub, violations)
			}
		})
	}
}

func TestBuildActionTaskWithKeysMapsDependencies(t *testing.T) {
	team := testTeam()
	run := batchDepsRunFixture()
	implA := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev",
		Task: "实现 Web", Key: "web", ActionKey: ActionKey("team-1", "run-deps", "lead", 1, 0)}
	implB := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev",
		Task: "实现桌面", Key: "desktop", ActionKey: ActionKey("team-1", "run-deps", "lead", 1, 1)}
	review := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "rev",
		Task: "审查两端", Review: reviewFlag(true), WriteLock: TaskWriteLockNone,
		DependsOn: []string{"web", "desktop", "lead"},
		ActionKey: ActionKey("team-1", "run-deps", "lead", 1, 2)}
	byKey := map[string]string{
		"web":     ActionDerivedTaskID(implA),
		"desktop": ActionDerivedTaskID(implB),
	}
	taskA := buildActionTaskWithKeys(run, team, implA, byKey)
	if len(taskA.DependsOn) != 1 || taskA.DependsOn[0] != "lead" {
		t.Fatalf("无显式依赖的实现保留来源任务依赖: %+v", taskA.DependsOn)
	}
	run.Tasks = append(run.Tasks, taskA)
	taskB := buildActionTaskWithKeys(run, team, implB, byKey)
	run.Tasks = append(run.Tasks, taskB)
	reviewTask := buildActionTaskWithKeys(run, team, review, byKey)
	if len(reviewTask.DependsOn) != 3 {
		t.Fatalf("审查依赖应含两条实现 + 既有任务: %+v", reviewTask.DependsOn)
	}
	for _, want := range []string{ActionDerivedTaskID(implA), ActionDerivedTaskID(implB), "lead"} {
		if !containsString(reviewTask.DependsOn, want) {
			t.Fatalf("审查依赖缺少 %s: %+v", want, reviewTask.DependsOn)
		}
	}
	if reviewTask.WriteLock != TaskWriteLockNone || reviewTask.Review == nil || !*reviewTask.Review {
		t.Fatalf("审查任务形态不符: %+v", reviewTask)
	}
	// 显式依赖存在时不再叠加「等全部在途」缺省。
	leadFollowUp := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "lead",
		Task: "后续规划", DependsOn: []string{"web"}, ActionKey: ActionKey("team-1", "run-deps", "lead", 1, 3)}
	followUpTask := buildActionTaskWithKeys(run, team, leadFollowUp, byKey)
	if len(followUpTask.DependsOn) != 1 || followUpTask.DependsOn[0] != ActionDerivedTaskID(implA) {
		t.Fatalf("显式依赖优先于等全部在途缺省: %+v", followUpTask.DependsOn)
	}
}

func TestSortActionsByBatchDependencies(t *testing.T) {
	build := func(key string, deps ...string) Action {
		return Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev",
			Task: "做 " + key, Key: key, DependsOn: deps,
			ActionKey: ActionKey("team-1", "run-deps", "lead", 1, len(deps))}
	}
	// 审查列在首位但依赖两条实现：拓扑排序后实现先执行。
	actions := []Action{
		build("review", "web", "desktop"),
		build("web"),
		build("desktop", "web"),
	}
	byKey := map[string]string{}
	for i := range actions {
		byKey[actions[i].Key] = ActionDerivedTaskID(&actions[i])
	}
	sortActionsByBatchDependencies(actions, byKey)
	order := make([]string, 0, len(actions))
	for i := range actions {
		order = append(order, actions[i].Key)
	}
	// web 必须先于 desktop（依赖），desktop 先于 review；web 与 review 的相对
	// 位置按稳定序（web 在 review 之后访问先入栈……拓扑保证约束即可）。
	position := map[string]int{}
	for index, key := range order {
		position[key] = index
	}
	if !(position["web"] < position["desktop"] && position["desktop"] < position["review"]) {
		t.Fatalf("拓扑顺序不符: %v", order)
	}
}

// TestPassActionsExecutesBatchWithDependencies 全链路：一个动作块携带两条
// 实现 + 一条依赖两者的审查；passActions 按拓扑执行，派生任务的 DependsOn
// 指向实现任务——审查绝不在实现完成前派发（阶段 2 验收用例）。
func TestPassActionsExecutesBatchWithDependencies(t *testing.T) {
	h := newActionsExecHarness(t)
	run := batchDepsRunFixture()
	run.TeamID = h.team.ID
	run.PendingActions = []Action{
		{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev",
			Task: "修改 Web 删除逻辑", Key: "web", WriteLock: TaskWriteLockExclusive,
			ActionKey: ActionKey(h.team.ID, run.ID, "lead", 1, 0), Status: ActionStatusProposed},
		{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev",
			Task: "修改桌面删除逻辑", Key: "desktop", WriteLock: TaskWriteLockExclusive,
			ActionKey: ActionKey(h.team.ID, run.ID, "lead", 1, 1), Status: ActionStatusProposed},
		{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "rev",
			Task: "审查两端最终改动", Review: reviewFlag(true), WriteLock: TaskWriteLockNone,
			DependsOn: []string{"web", "desktop"},
			ActionKey: ActionKey(h.team.ID, run.ID, "lead", 1, 2), Status: ActionStatusProposed},
	}
	state := h.saveExecRun(t, run)
	h.service.passActions(state)

	fresh := h.store.Run(run.ID)
	web := findTaskIn(fresh, "lead-d1-0")
	desktop := findTaskIn(fresh, "lead-d1-1")
	review := findTaskIn(fresh, "lead-d1-2")
	if web == nil || desktop == nil || review == nil {
		t.Fatalf("三条派生任务都应创建: %+v", fresh.Tasks)
	}
	if len(review.DependsOn) != 2 || !containsString(review.DependsOn, web.ID) || !containsString(review.DependsOn, desktop.ID) {
		t.Fatalf("审查必须依赖两条实现: %+v", review.DependsOn)
	}
	if review.WriteLock != TaskWriteLockNone {
		t.Fatalf("审查任务只读: %+v", review.WriteLock)
	}
	// 依赖在途：审查保持 planned（调度器按 DAG 就绪，不抢跑）。
	if review.Status != TaskPlanned {
		t.Fatalf("实现未完成时审查不得派发，实际 %s", review.Status)
	}
}

// TestPassActionsMixedBatchWithoutCoverageRejected 未覆盖的混排批次整批拒绝
// （动作不执行、快照无派生任务），拒绝原因落快照供修复回路回送。
func TestPassActionsMixedBatchWithoutCoverageRejected(t *testing.T) {
	h := newActionsExecHarness(t)
	// 直接走解析层：ValidateActions 拒绝（执行层不会见到动作）。
	run := batchDepsRunFixture()
	inner := actionJSON(
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"实现"}`,
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"rev","task":"审查","review":true,"writeLock":"none"}`,
	)
	payload, violations := decodeActionPayload(inner)
	if payload == nil {
		t.Fatalf("内文必须可解码，实际 %v", violations)
	}
	actions, violations := ParseLeadActions("<team-actions>"+inner+"</team-actions>", h.team, run, h.team.Policy)
	if len(actions) != 0 || len(violations) == 0 {
		t.Fatalf("未覆盖混排应整体拒绝: actions=%v violations=%v", actions, violations)
	}
	if !strings.Contains(strings.Join(violations, "；"), "未用 dependsOn 覆盖执行任务") {
		t.Fatalf("拒绝原因不符: %v", violations)
	}
}
