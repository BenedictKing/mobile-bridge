package team

// planner 的单元测试（§13.1 team/planner 行）：合法 pipeline、循环拒绝（报错
// 含环路径）、未知成员、超 maxTasksPerRun、broadcast 拒绝、single 空计划合成
// Lead 任务、planHash 稳定性、prompt 上限含上下文余量（100000−12000−200）。

import (
	"strings"
	"testing"
)

func testTeam() *Team {
	team := &Team{
		ID:     "team-1",
		Name:   "测试团队",
		LeadID: "lead",
		Policy: DefaultPolicy(),
		Members: []TeamMember{
			{ID: "lead", SessionID: "s-lead", AgentKind: "kimi", Role: RoleLead, Enabled: true},
			{ID: "dev", SessionID: "s-dev", AgentKind: "codex", Role: RoleMember, Enabled: true},
			{ID: "rev", SessionID: "s-rev", AgentKind: "zcode", Role: RoleReviewer, Enabled: true},
			{ID: "ghost", SessionID: "s-ghost", AgentKind: "codex", Role: RoleMember, Enabled: false},
		},
	}
	return team
}

func pipelinePlan() []PlanTaskInput {
	return []PlanTaskInput{
		{ID: "design", MemberID: "lead", Prompt: "分析需求并拆分任务", WriteLock: TaskWriteLockNone},
		{ID: "implement", MemberID: "dev", Prompt: "实现接口", DependsOn: []string{"design"}, WriteLock: TaskWriteLockExclusive},
		{ID: "review", MemberID: "rev", Prompt: "检查实现并补测试", DependsOn: []string{"implement"}, WriteLock: TaskWriteLockNone},
	}
}

func TestParsePlanValidPipeline(t *testing.T) {
	plan, err := ParsePlan(testTeam(), "实现用户头像上传接口", ModePipeline, pipelinePlan())
	if err != nil {
		t.Fatalf("合法 pipeline 不应报错: %v", err)
	}
	if plan.Mode != ModePipeline || plan.Goal != "实现用户头像上传接口" {
		t.Fatalf("模式/目标未保留: %+v", plan)
	}
	if len(plan.Tasks) != 3 {
		t.Fatalf("任务数应为 3，实际 %d", len(plan.Tasks))
	}
	wantOrder := []string{"design", "implement", "review"}
	for i, want := range wantOrder {
		if plan.Tasks[i].ID != want {
			t.Fatalf("拓扑序第 %d 位应为 %s，实际 %s", i, want, plan.Tasks[i].ID)
		}
		if plan.Tasks[i].Ordinal != i {
			t.Fatalf("任务 %s Ordinal 应为 %d，实际 %d", want, i, plan.Tasks[i].Ordinal)
		}
		if plan.Tasks[i].Status != TaskPlanned {
			t.Fatalf("任务 %s 初始状态应为 planned，实际 %s", want, plan.Tasks[i].Status)
		}
		if plan.Tasks[i].InputHash == "" {
			t.Fatalf("任务 %s 缺少创建期 InputHash", want)
		}
	}
	if plan.Tasks[2].InstructionsHead != "检查实现并补测试" {
		t.Fatalf("InstructionsHead 应为 Prompt 本身（短于 240 字），实际 %q", plan.Tasks[2].InstructionsHead)
	}
	if plan.PlanHash == "" || len(plan.PlanHash) != 64 {
		t.Fatalf("planHash 应为 sha256 hex，实际 %q", plan.PlanHash)
	}
	// 空写锁归一为 exclusive（§6.4 默认写策略）。
	if plan.Tasks[0].WriteLock != TaskWriteLockNone || plan.Tasks[1].WriteLock != TaskWriteLockExclusive {
		t.Fatalf("写锁归一错误: %q / %q", plan.Tasks[0].WriteLock, plan.Tasks[1].WriteLock)
	}
	// 空工作区模式归一为团队策略（默认 shared_workspace）。
	for _, task := range plan.Tasks {
		if task.WorkspaceMode != WorkspaceShared {
			t.Fatalf("任务 %s 工作区模式应归一为 shared_workspace，实际 %q", task.ID, task.WorkspaceMode)
		}
	}
}

func TestParsePlanRejectsDependencyCycle(t *testing.T) {
	cycle := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "a", DependsOn: []string{"c"}},
		{ID: "b", MemberID: "dev", Prompt: "b", DependsOn: []string{"a"}},
		{ID: "c", MemberID: "dev", Prompt: "c", DependsOn: []string{"b"}},
	}
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, cycle)
	if err == nil {
		t.Fatal("循环依赖应被拒绝")
	}
	if !strings.Contains(err.Error(), "计划存在循环依赖") {
		t.Fatalf("报错应说明循环依赖，实际 %q", err.Error())
	}
	if !strings.Contains(err.Error(), " → ") {
		t.Fatalf("报错应含环路径（a → b → c → a 形式），实际 %q", err.Error())
	}
	// 自依赖也是环。
	self := []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", DependsOn: []string{"a"}}}
	_, err = ParsePlan(testTeam(), "目标", ModePipeline, self)
	if err == nil || !strings.Contains(err.Error(), "计划存在循环依赖：a → a") {
		t.Fatalf("自依赖应报『计划存在循环依赖：a → a』，实际 %v", err)
	}
}

func TestParsePlanRejectsUnknownMember(t *testing.T) {
	plan := []PlanTaskInput{{ID: "a", MemberID: "nobody", Prompt: "干活"}}
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, plan)
	if err == nil || !strings.Contains(err.Error(), "不存在的成员：nobody") {
		t.Fatalf("未知成员应被拒绝，实际 %v", err)
	}
	disabled := []PlanTaskInput{{ID: "a", MemberID: "ghost", Prompt: "干活"}}
	_, err = ParsePlan(testTeam(), "目标", ModePipeline, disabled)
	if err == nil || !strings.Contains(err.Error(), "已停用的成员：ghost") {
		t.Fatalf("停用成员应被拒绝，实际 %v", err)
	}
}

func TestParsePlanRejectsTooManyTasks(t *testing.T) {
	team := testTeam()
	team.Policy.MaxTasksPerRun = 2
	plan := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "a"},
		{ID: "b", MemberID: "dev", Prompt: "b"},
		{ID: "c", MemberID: "dev", Prompt: "c"},
	}
	_, err := ParsePlan(team, "目标", ModePipeline, plan)
	if err == nil || !strings.Contains(err.Error(), "任务数超过上限：最多 2 条") {
		t.Fatalf("超任务上限应被拒绝，实际 %v", err)
	}
}

func TestParsePlanRejectsBroadcast(t *testing.T) {
	_, err := ParsePlan(testTeam(), "目标", ModeBroadcast, pipelinePlan())
	if err == nil || err.Error() != "并行分派在后续阶段提供" {
		t.Fatalf("broadcast 应报『并行分派在后续阶段提供』，实际 %v", err)
	}
	_, err = ParsePlan(testTeam(), "目标", "unknown", pipelinePlan())
	if err == nil || !strings.Contains(err.Error(), "未知运行模式") {
		t.Fatalf("未知模式应被拒绝，实际 %v", err)
	}
}

func TestParsePlanSingleSynthesizesLeadTask(t *testing.T) {
	plan, err := ParsePlan(testTeam(), "总结当前进度", ModeSingle, nil)
	if err != nil {
		t.Fatalf("single 空计划应合成 Lead 任务: %v", err)
	}
	if len(plan.Tasks) != 1 {
		t.Fatalf("应合成 1 条任务，实际 %d", len(plan.Tasks))
	}
	task := plan.Tasks[0]
	if task.ID != "lead" || task.MemberID != "lead" || task.Prompt != "总结当前进度" {
		t.Fatalf("合成任务字段错误: %+v", task)
	}
	if task.Status != TaskPlanned || task.WriteLock != TaskWriteLockExclusive {
		t.Fatalf("合成任务状态/写锁错误: %s/%s", task.Status, task.WriteLock)
	}
	// 恰一条显式任务也接受。
	one := []PlanTaskInput{{ID: "design", MemberID: "dev", Prompt: "干活"}}
	plan, err = ParsePlan(testTeam(), "目标", ModeSingle, one)
	if err != nil {
		t.Fatalf("single 恰一条应接受: %v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "design" {
		t.Fatalf("应原样保留单条任务，实际 %+v", plan.Tasks)
	}
	// 多于一条拒绝；缺 Lead 拒绝；Lead 停用拒绝。
	_, err = ParsePlan(testTeam(), "目标", ModeSingle, pipelinePlan())
	if err == nil || !strings.Contains(err.Error(), "单任务模式最多一条任务") {
		t.Fatalf("single 多条应拒绝，实际 %v", err)
	}
	noLead := testTeam()
	noLead.LeadID = ""
	if _, err = ParsePlan(noLead, "目标", ModeSingle, nil); err == nil ||
		!strings.Contains(err.Error(), "需要先为团队指定 Lead 成员") {
		t.Fatalf("缺 Lead 应拒绝，实际 %v", err)
	}
	noLead.LeadID = "ghost"
	if _, err = ParsePlan(noLead, "目标", ModeSingle, nil); err == nil ||
		!strings.Contains(err.Error(), "Lead 成员已停用") {
		t.Fatalf("Lead 停用应拒绝，实际 %v", err)
	}
	// pipeline 空计划拒绝。
	if _, err = ParsePlan(testTeam(), "目标", ModePipeline, nil); err == nil ||
		!strings.Contains(err.Error(), "流水线模式需要显式计划") {
		t.Fatalf("pipeline 空计划应拒绝，实际 %v", err)
	}
}

func TestParsePlanPlanHashStable(t *testing.T) {
	team := testTeam()
	first, err := ParsePlan(team, "目标", ModePipeline, pipelinePlan())
	if err != nil {
		t.Fatalf("第一次解析失败: %v", err)
	}
	second, err := ParsePlan(team, "目标", ModePipeline, pipelinePlan())
	if err != nil {
		t.Fatalf("第二次解析失败: %v", err)
	}
	if first.PlanHash != second.PlanHash {
		t.Fatalf("同一计划两次解析 planHash 应一致: %s != %s", first.PlanHash, second.PlanHash)
	}
	// 输入顺序打乱（仍为同一 DAG）：拓扑序恢复后哈希不变。
	reversed := []PlanTaskInput{
		{ID: "review", MemberID: "rev", Prompt: "检查实现并补测试", DependsOn: []string{"implement"}, WriteLock: TaskWriteLockNone},
		{ID: "implement", MemberID: "dev", Prompt: "实现接口", DependsOn: []string{"design"}, WriteLock: TaskWriteLockExclusive},
		{ID: "design", MemberID: "lead", Prompt: "分析需求并拆分任务", WriteLock: TaskWriteLockNone},
	}
	shuffled, err := ParsePlan(team, "目标", ModePipeline, reversed)
	if err != nil {
		t.Fatalf("乱序 DAG 解析失败: %v", err)
	}
	if shuffled.PlanHash != first.PlanHash {
		t.Fatalf("同一 DAG 乱序输入 planHash 应一致: %s != %s", shuffled.PlanHash, first.PlanHash)
	}
	// 内容变化（prompt 不同）哈希必须变化。
	changed := pipelinePlan()
	changed[1].Prompt = "实现接口与数据校验"
	diverted, err := ParsePlan(team, "目标", ModePipeline, changed)
	if err != nil {
		t.Fatalf("变体解析失败: %v", err)
	}
	if diverted.PlanHash == first.PlanHash {
		t.Fatal("prompt 变化后 planHash 不应相同")
	}
}

func TestParsePlanPromptLimitIncludesContextEnvelope(t *testing.T) {
	if PromptLimit(DefaultPolicy().MaxContextChars) != 87800 {
		t.Fatalf("默认有效上限应为 87800 字，实际 %d", PromptLimit(DefaultPolicy().MaxContextChars))
	}
	team := testTeam() // MaxContextChars=12000 → limit 87800。
	atLimit := strings.Repeat("字", 87800)
	plan := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: atLimit},
		{ID: "b", MemberID: "rev", Prompt: "接续", DependsOn: []string{"a"}},
	}
	if parsed, err := ParsePlan(team, "目标", ModePipeline, plan); err != nil {
		t.Fatalf("恰好 87800 字应通过: %v", err)
	} else if len(parsed.Tasks) != 2 {
		t.Fatalf("任务数错误: %d", len(parsed.Tasks))
	}
	over := strings.Repeat("字", 87801)
	_, err := ParsePlan(team, "目标", ModePipeline, []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: over}})
	if err == nil {
		t.Fatal("超过有效上限应被拒绝")
	}
	if !strings.Contains(err.Error(), "当前上限 87800 字") {
		t.Fatalf("报错应给出有效上限 87800 字，实际 %q", err.Error())
	}
	// 目标即 prompt 的 single 空计划同样受限。
	_, err = ParsePlan(team, strings.Repeat("字", 87801), ModeSingle, nil)
	if err == nil || !strings.Contains(err.Error(), "运行目标过长") {
		t.Fatalf("single 超长目标应拒绝，实际 %v", err)
	}
	// 余量随策略变化：MaxContextChars=0 时上限 99800。
	wide := testTeam()
	wide.Policy.MaxContextChars = 0
	if _, err := ParsePlan(wide, "目标", ModePipeline,
		[]PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: strings.Repeat("字", 99800)}}); err != nil {
		t.Fatalf("MaxContextChars=0 时 99800 字应通过: %v", err)
	}
}

func TestParsePlanFieldValidation(t *testing.T) {
	cases := []struct {
		name    string
		input   []PlanTaskInput
		wantErr string
	}{
		{"非法任务 ID", []PlanTaskInput{{ID: "任务一", MemberID: "dev", Prompt: "a"}}, "任务 ID 无效"},
		{"重复任务 ID", []PlanTaskInput{
			{ID: "a", MemberID: "dev", Prompt: "a"},
			{ID: "a", MemberID: "rev", Prompt: "b"},
		}, "任务 ID 重复"},
		{"空 prompt", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "   "}}, "内容不能为空"},
		{"未知依赖", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", DependsOn: []string{"missing"}}}, "依赖了不存在的任务"},
		{"非法写锁", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WriteLock: "sometimes"}}, "写锁取值无效"},
		{"独立工作区", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: WorkspaceIsolatedWorktree}}, "独立工作区在后续阶段提供"},
		{"非法工作区", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: "cloud"}}, "工作区模式无效"},
	}
	for _, testCase := range cases {
		_, err := ParsePlan(testTeam(), "目标", ModePipeline, testCase.input)
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: 期望报错含 %q，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
	if _, err := ParsePlan(testTeam(), "  ", ModePipeline, pipelinePlan()); err == nil ||
		!strings.Contains(err.Error(), "请填写运行目标") {
		t.Fatalf("空目标应被拒绝，实际 %v", err)
	}
	// dependsOn 去重归一。
	dedup := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "a"},
		{ID: "b", MemberID: "rev", Prompt: "b", DependsOn: []string{"a", "a"}},
	}
	plan, err := ParsePlan(testTeam(), "目标", ModePipeline, dedup)
	if err != nil {
		t.Fatalf("重复依赖应去重而非报错: %v", err)
	}
	if got := plan.Tasks[1].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("依赖应去重为 [a]，实际 %v", got)
	}
}

func TestInputHashForCoversDeterministicFields(t *testing.T) {
	team := testTeam()
	plan, err := ParsePlan(team, "目标", ModePipeline, pipelinePlan())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	digest := PolicyDigest(team.Policy)
	if digest != PolicyDigest(DefaultPolicy()) {
		t.Fatal("相同策略摘要应一致")
	}
	task := &plan.Tasks[1]
	base := InputHashFor(task, digest, "")
	if base != InputHashFor(task, digest, "") {
		t.Fatal("同输入哈希应稳定")
	}
	if base == InputHashFor(task, "other-digest", "") {
		t.Fatal("策略摘要变化应改变哈希")
	}
	if base == InputHashFor(task, digest, "上游结果上下文") {
		t.Fatal("派发期上下文应改变哈希（planner 空上下文 vs dispatcher 真实上下文）")
	}
	changed := *task
	changed.Prompt = "另一个任务"
	if base == InputHashFor(&changed, digest, "") {
		t.Fatal("prompt 变化应改变哈希")
	}
}
