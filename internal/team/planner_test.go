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
	plan, err := ParsePlan(testTeam(), "实现用户头像上传接口", ModePipeline, "", pipelinePlan())
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
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, "", cycle)
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
	_, err = ParsePlan(testTeam(), "目标", ModePipeline, "", self)
	if err == nil || !strings.Contains(err.Error(), "计划存在循环依赖：a → a") {
		t.Fatalf("自依赖应报『计划存在循环依赖：a → a』，实际 %v", err)
	}
}

func TestParsePlanRejectsUnknownMember(t *testing.T) {
	plan := []PlanTaskInput{{ID: "a", MemberID: "nobody", Prompt: "干活"}}
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, "", plan)
	if err == nil || !strings.Contains(err.Error(), "不存在的成员：nobody") {
		t.Fatalf("未知成员应被拒绝，实际 %v", err)
	}
	disabled := []PlanTaskInput{{ID: "a", MemberID: "ghost", Prompt: "干活"}}
	_, err = ParsePlan(testTeam(), "目标", ModePipeline, "", disabled)
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
	_, err := ParsePlan(team, "目标", ModePipeline, "", plan)
	if err == nil || !strings.Contains(err.Error(), "任务数超过上限：最多 2 条") {
		t.Fatalf("超任务上限应被拒绝，实际 %v", err)
	}
}

func TestParsePlanRejectsUnknownMode(t *testing.T) {
	_, err := ParsePlan(testTeam(), "目标", "unknown", "", pipelinePlan())
	if err == nil || !strings.Contains(err.Error(), "未知运行模式") {
		t.Fatalf("未知模式应被拒绝，实际 %v", err)
	}
}

func TestParsePlanSingleSynthesizesLeadTask(t *testing.T) {
	plan, err := ParsePlan(testTeam(), "总结当前进度", ModeSingle, "", nil)
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
	plan, err = ParsePlan(testTeam(), "目标", ModeSingle, "", one)
	if err != nil {
		t.Fatalf("single 恰一条应接受: %v", err)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].ID != "design" {
		t.Fatalf("应原样保留单条任务，实际 %+v", plan.Tasks)
	}
	// 多于一条拒绝；缺 Lead 拒绝；Lead 停用拒绝。
	_, err = ParsePlan(testTeam(), "目标", ModeSingle, "", pipelinePlan())
	if err == nil || !strings.Contains(err.Error(), "单任务模式最多一条任务") {
		t.Fatalf("single 多条应拒绝，实际 %v", err)
	}
	noLead := testTeam()
	noLead.LeadID = ""
	if _, err = ParsePlan(noLead, "目标", ModeSingle, "", nil); err == nil ||
		!strings.Contains(err.Error(), "需要先为团队指定 Lead 成员") {
		t.Fatalf("缺 Lead 应拒绝，实际 %v", err)
	}
	noLead.LeadID = "ghost"
	if _, err = ParsePlan(noLead, "目标", ModeSingle, "", nil); err == nil ||
		!strings.Contains(err.Error(), "Lead 成员已停用") {
		t.Fatalf("Lead 停用应拒绝，实际 %v", err)
	}
	// pipeline 空计划拒绝。
	if _, err = ParsePlan(testTeam(), "目标", ModePipeline, "", nil); err == nil ||
		!strings.Contains(err.Error(), "流水线模式需要显式计划") {
		t.Fatalf("pipeline 空计划应拒绝，实际 %v", err)
	}
}

func TestParsePlanPlanHashStable(t *testing.T) {
	team := testTeam()
	first, err := ParsePlan(team, "目标", ModePipeline, "", pipelinePlan())
	if err != nil {
		t.Fatalf("第一次解析失败: %v", err)
	}
	second, err := ParsePlan(team, "目标", ModePipeline, "", pipelinePlan())
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
	shuffled, err := ParsePlan(team, "目标", ModePipeline, "", reversed)
	if err != nil {
		t.Fatalf("乱序 DAG 解析失败: %v", err)
	}
	if shuffled.PlanHash != first.PlanHash {
		t.Fatalf("同一 DAG 乱序输入 planHash 应一致: %s != %s", shuffled.PlanHash, first.PlanHash)
	}
	// 内容变化（prompt 不同）哈希必须变化。
	changed := pipelinePlan()
	changed[1].Prompt = "实现接口与数据校验"
	diverted, err := ParsePlan(team, "目标", ModePipeline, "", changed)
	if err != nil {
		t.Fatalf("变体解析失败: %v", err)
	}
	if diverted.PlanHash == first.PlanHash {
		t.Fatal("prompt 变化后 planHash 不应相同")
	}
}

func TestParsePlanPromptLimitIncludesContextEnvelope(t *testing.T) {
	if PromptLimit(DefaultPolicy().MaxContextChars) != 87800 {
		t.Fatalf("默认 rune 上限应为 87800 字，实际 %d", PromptLimit(DefaultPolicy().MaxContextChars))
	}
	// 创建期字节上限（评审遗留②：与派发期账本的字节校验同源，多字节内容
	// 创建即拒，不再派发期才失败）：100000−12000×3−512=63488 字节。
	if PromptByteLimit(DefaultPolicy().MaxContextChars) != 63488 {
		t.Fatalf("默认字节上限应为 63488 字节，实际 %d", PromptByteLimit(DefaultPolicy().MaxContextChars))
	}
	team := testTeam() // MaxContextChars=12000 → rune 87800 / 字节 63488。
	atLimit := strings.Repeat("p", 63488)
	plan := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: atLimit},
		{ID: "b", MemberID: "rev", Prompt: "接续", DependsOn: []string{"a"}},
	}
	if parsed, err := ParsePlan(team, "目标", ModePipeline, "", plan); err != nil {
		t.Fatalf("恰好 63488 字节应通过: %v", err)
	} else if len(parsed.Tasks) != 2 {
		t.Fatalf("任务数错误: %d", len(parsed.Tasks))
	}
	// 多字节内容按字节计：22000 个 CJK 字（66000 字节）rune 在 87800 限内，
	// 创建期必须按字节拒绝并给出字节上限与约当字数。
	_, err := ParsePlan(team, "目标", ModePipeline, "", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: strings.Repeat("字", 22000)}})
	if err == nil {
		t.Fatal("超字节上限的多字节 prompt 应在创建期被拒绝")
	}
	if !strings.Contains(err.Error(), "当前上限 63488 字节") || !strings.Contains(err.Error(), "约合 21162 字") {
		t.Fatalf("报错应给出字节上限与约当字数，实际 %q", err.Error())
	}
	// 纯 ASCII 同受字节口径约束（取 rune 上限与字节上限的较小者）。
	if _, err := ParsePlan(team, "目标", ModePipeline, "",
		[]PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: strings.Repeat("p", 63489)}}); err == nil {
		t.Fatal("超字节上限的 ASCII prompt 应被拒绝")
	}
	// 目标即 prompt 的 single 空计划同样受限。
	_, err = ParsePlan(team, strings.Repeat("字", 22000), ModeSingle, "", nil)
	if err == nil || !strings.Contains(err.Error(), "运行目标过长") {
		t.Fatalf("single 超长目标应拒绝，实际 %v", err)
	}
	// 余量随策略变化：MaxContextChars=0 时字节上限 99488、rune 上限 99800。
	wide := testTeam()
	wide.Policy.MaxContextChars = 0
	if got := PromptByteLimit(0); got != 99488 {
		t.Fatalf("MaxContextChars=0 时字节上限应为 99488，实际 %d", got)
	}
	if _, err := ParsePlan(wide, "目标", ModePipeline, "",
		[]PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: strings.Repeat("p", 99488)}}); err != nil {
		t.Fatalf("MaxContextChars=0 时 99488 字节应通过: %v", err)
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
		{"独立工作区缺 workspaceId", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: WorkspaceIsolatedWorktree}}, "必须提供 workspaceId"},
		{"独立工作区非法 workspaceId", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "不是/合法键"}}, "必须提供 workspaceId"},
		{"共享工作区带 workspaceId", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: WorkspaceShared, WorkspaceID: "wt-1"}}, "不能指定 workspaceId"},
		{"非法工作区", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "a", WorkspaceMode: "cloud"}}, "工作区模式无效"},
	}
	for _, testCase := range cases {
		_, err := ParsePlan(testTeam(), "目标", ModePipeline, "", testCase.input)
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: 期望报错含 %q，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
	if _, err := ParsePlan(testTeam(), "  ", ModePipeline, "", pipelinePlan()); err == nil ||
		!strings.Contains(err.Error(), "请填写运行目标") {
		t.Fatalf("空目标应被拒绝，实际 %v", err)
	}
	// dependsOn 去重归一。
	dedup := []PlanTaskInput{
		{ID: "a", MemberID: "dev", Prompt: "a"},
		{ID: "b", MemberID: "rev", Prompt: "b", DependsOn: []string{"a", "a"}},
	}
	plan, err := ParsePlan(testTeam(), "目标", ModePipeline, "", dedup)
	if err != nil {
		t.Fatalf("重复依赖应去重而非报错: %v", err)
	}
	if got := plan.Tasks[1].DependsOn; len(got) != 1 || got[0] != "a" {
		t.Fatalf("依赖应去重为 [a]，实际 %v", got)
	}
}

func TestParsePlanIsolatedWorktree(t *testing.T) {
	plan, err := ParsePlan(testTeam(), "目标", ModePipeline, "", []PlanTaskInput{
		{ID: "iso", MemberID: "dev", Prompt: "在独立工作区做实验",
			WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-exp-1"},
	})
	if err != nil {
		t.Fatalf("isolated_worktree 计划不应再被阶段拒绝: %v", err)
	}
	task := plan.Tasks[0]
	if task.WorkspaceMode != WorkspaceIsolatedWorktree || task.WorkspaceID != "wt-exp-1" {
		t.Fatalf("workspaceMode/workspaceId 应写入任务: %+v", task)
	}
	if task.BaseRevision != "" {
		t.Fatalf("基线在首派才记录，创建期应为空，实际 %q", task.BaseRevision)
	}
	if task.WriteLock != TaskWriteLockExclusive {
		t.Fatalf("isolated 任务空写锁应归一为 exclusive（worktree 级写锁），实际 %q", task.WriteLock)
	}
	// broadcast 展开逐成员携带同一 workspaceId。
	broadcast, err := ParsePlan(testTeam(), "目标", ModeBroadcast, "", []PlanTaskInput{
		{ID: "scan", MemberIDs: []string{"dev", "rev"}, Prompt: "并行扫描",
			WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-scan"},
	})
	if err != nil {
		t.Fatalf("broadcast + isolated 不应报错: %v", err)
	}
	if len(broadcast.Tasks) != 2 {
		t.Fatalf("broadcast 应展开为 2 条任务，实际 %d", len(broadcast.Tasks))
	}
	for _, expanded := range broadcast.Tasks {
		if expanded.WorkspaceID != "wt-scan" {
			t.Fatalf("展开任务 %s 应携带 workspaceId，实际 %q", expanded.ID, expanded.WorkspaceID)
		}
	}
}

func TestInputHashForCoversDeterministicFields(t *testing.T) {
	team := testTeam()
	plan, err := ParsePlan(team, "目标", ModePipeline, "", pipelinePlan())
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

// ---- broadcast 模式（§5.3 阶段二：逐成员展开）----

func broadcastPlan() []PlanTaskInput {
	return []PlanTaskInput{
		{ID: "draft", Prompt: "各写一版方案", MemberIDs: []string{"lead", "dev"}, WriteLock: TaskWriteLockNone},
	}
}

func TestParsePlanBroadcastExpandsPerMember(t *testing.T) {
	plan := []PlanTaskInput{
		{ID: "draft", Prompt: "各写一版方案", MemberIDs: []string{"lead", "dev"}, WriteLock: TaskWriteLockNone},
		{ID: "merge", Prompt: "综合两版", MemberIDs: []string{"rev", "lead"}, DependsOn: []string{"draft"}, WriteLock: TaskWriteLockNone},
	}
	parsed, err := ParsePlan(testTeam(), "产出方案", ModeBroadcast, RunCollectFirstSuccess, plan)
	if err != nil {
		t.Fatalf("合法 broadcast 不应报错: %v", err)
	}
	if parsed.Mode != ModeBroadcast || parsed.CollectPolicy != RunCollectFirstSuccess {
		t.Fatalf("模式/收集策略未保留: %+v", parsed)
	}
	// 展开为 4 个任务，拓扑序：wave1 两成员先于 wave2。
	wantOrder := []string{"draft-lead", "draft-dev", "merge-rev", "merge-lead"}
	if len(parsed.Tasks) != len(wantOrder) {
		t.Fatalf("展开后任务数应为 %d，实际 %d", len(wantOrder), len(parsed.Tasks))
	}
	for i, want := range wantOrder {
		if parsed.Tasks[i].ID != want {
			t.Fatalf("拓扑序第 %d 位应为 %s，实际 %s", i, want, parsed.Tasks[i].ID)
		}
	}
	byID := map[string]Task{}
	for _, task := range parsed.Tasks {
		byID[task.ID] = task
	}
	// 组归属：同条目共享 BroadcastGroup=条目 ID。
	for _, id := range []string{"draft-lead", "draft-dev"} {
		if byID[id].BroadcastGroup != "draft" {
			t.Fatalf("任务 %s 的组应为 draft，实际 %q", id, byID[id].BroadcastGroup)
		}
	}
	for _, id := range []string{"merge-rev", "merge-lead"} {
		if byID[id].BroadcastGroup != "merge" {
			t.Fatalf("任务 %s 的组应为 merge，实际 %q", id, byID[id].BroadcastGroup)
		}
	}
	// 依赖 broadcast 条目 = 依赖其全部展开任务。
	for _, id := range []string{"merge-rev", "merge-lead"} {
		deps := byID[id].DependsOn
		if len(deps) != 2 || !containsAll(deps, "draft-lead", "draft-dev") {
			t.Fatalf("任务 %s 应依赖 draft 的全部展开任务，实际 %v", id, deps)
		}
	}
	// 同组同 prompt/写锁；成员各异。
	if byID["draft-lead"].Prompt != byID["draft-dev"].Prompt || byID["draft-lead"].WriteLock != byID["draft-dev"].WriteLock {
		t.Fatalf("同组任务应共享 prompt/写锁: %+v vs %+v", byID["draft-lead"], byID["draft-dev"])
	}
	if byID["draft-lead"].MemberID != "lead" || byID["draft-dev"].MemberID != "dev" {
		t.Fatalf("展开任务的成员归属错误: %s/%s", byID["draft-lead"].MemberID, byID["draft-dev"].MemberID)
	}
}

func containsAll(items []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, item := range items {
			if item == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestParsePlanBroadcastValidation(t *testing.T) {
	cases := []struct {
		name    string
		input   []PlanTaskInput
		wantErr string
	}{
		{"memberIds 少于 2", []PlanTaskInput{{ID: "a", Prompt: "p", MemberIDs: []string{"lead"}}}, "至少需要 2 个互异成员"},
		{"memberIds 空", []PlanTaskInput{{ID: "a", Prompt: "p"}}, "至少需要 2 个互异成员"},
		{"memberIds 重复", []PlanTaskInput{{ID: "a", Prompt: "p", MemberIDs: []string{"lead", "lead"}}}, "重复成员"},
		{"memberIds 引用不存在成员", []PlanTaskInput{{ID: "a", Prompt: "p", MemberIDs: []string{"lead", "nobody"}}}, "不存在的成员：nobody"},
		{"memberIds 引用停用成员", []PlanTaskInput{{ID: "a", Prompt: "p", MemberIDs: []string{"lead", "ghost"}}}, "已停用的成员：ghost"},
		{"broadcast 用 memberId", []PlanTaskInput{{ID: "a", Prompt: "p", MemberID: "dev", MemberIDs: []string{"lead", "dev"}}}, "不使用 memberId"},
		{"组合 ID 超长", []PlanTaskInput{{ID: strings.Repeat("x", 60), Prompt: "p", MemberIDs: []string{"lead", "dev"}}}, "组合出的任务 ID 超长"},
		{"依赖不存在条目", []PlanTaskInput{
			{ID: "a", Prompt: "p", MemberIDs: []string{"lead", "dev"}},
			{ID: "b", Prompt: "q", MemberIDs: []string{"lead", "dev"}, DependsOn: []string{"missing"}},
		}, "依赖了不存在的任务：missing"},
	}
	for _, testCase := range cases {
		_, err := ParsePlan(testTeam(), "目标", ModeBroadcast, "", testCase.input)
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: 期望报错含 %q，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
	// 非 broadcast 模式用 memberIds 拒绝；broadcast 空计划拒绝；broadcast 用 memberId 拒绝。
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, "", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "p", MemberIDs: []string{"lead"}}})
	if err == nil || !strings.Contains(err.Error(), "memberIds 仅并行分派模式支持") {
		t.Fatalf("pipeline 用 memberIds 应拒绝，实际 %v", err)
	}
	_, err = ParsePlan(testTeam(), "目标", ModeBroadcast, "", nil)
	if err == nil || !strings.Contains(err.Error(), "并行分派模式需要显式计划") {
		t.Fatalf("broadcast 空计划应拒绝，实际 %v", err)
	}
	_, err = ParsePlan(testTeam(), "目标", ModeBroadcast, "", []PlanTaskInput{{ID: "a", MemberID: "dev", Prompt: "p"}})
	if err == nil || !strings.Contains(err.Error(), "不使用 memberId") {
		t.Fatalf("broadcast 用 memberId 应拒绝，实际 %v", err)
	}
}

func TestParsePlanBroadcastCycleRejected(t *testing.T) {
	cycle := []PlanTaskInput{
		{ID: "a", Prompt: "a", MemberIDs: []string{"lead", "dev"}, DependsOn: []string{"b"}},
		{ID: "b", Prompt: "b", MemberIDs: []string{"lead", "dev"}, DependsOn: []string{"a"}},
	}
	_, err := ParsePlan(testTeam(), "目标", ModeBroadcast, "", cycle)
	if err == nil || !strings.Contains(err.Error(), "计划存在循环依赖") {
		t.Fatalf("broadcast 循环依赖应拒绝，实际 %v", err)
	}
	// 自依赖（条目依赖自身条目 → 展开后含自身）同样拒绝。
	self := []PlanTaskInput{{ID: "a", Prompt: "a", MemberIDs: []string{"lead", "dev"}, DependsOn: []string{"a"}}}
	_, err = ParsePlan(testTeam(), "目标", ModeBroadcast, "", self)
	if err == nil || !strings.Contains(err.Error(), "计划存在循环依赖：a-lead → a-lead") {
		t.Fatalf("broadcast 自依赖应报『a-lead → a-lead』，实际 %v", err)
	}
}

func TestParsePlanBroadcastTaskLimitCountsExpanded(t *testing.T) {
	team := testTeam()
	team.Policy.MaxTasksPerRun = 3
	plan := []PlanTaskInput{
		{ID: "a", Prompt: "p", MemberIDs: []string{"lead", "dev"}, WriteLock: TaskWriteLockNone},
		{ID: "b", Prompt: "q", MemberIDs: []string{"rev", "lead"}, WriteLock: TaskWriteLockNone},
	}
	// 2 条目展开 4 任务 > 上限 3。
	_, err := ParsePlan(team, "目标", ModeBroadcast, "", plan)
	if err == nil || !strings.Contains(err.Error(), "任务数超过上限：最多 3 条，展开后 4 条") {
		t.Fatalf("展开后超限应拒绝，实际 %v", err)
	}
}

func TestParsePlanCollectPolicyValidation(t *testing.T) {
	// first_success 仅 broadcast。
	_, err := ParsePlan(testTeam(), "目标", ModePipeline, RunCollectFirstSuccess, pipelinePlan())
	if err == nil || !strings.Contains(err.Error(), "first_success 收集策略仅并行分派模式支持") {
		t.Fatalf("pipeline + first_success 应拒绝，实际 %v", err)
	}
	_, err = ParsePlan(testTeam(), "目标", ModeSingle, RunCollectFirstSuccess, nil)
	if err == nil || !strings.Contains(err.Error(), "first_success 收集策略仅并行分派模式支持") {
		t.Fatalf("single + first_success 应拒绝，实际 %v", err)
	}
	// 未知取值拒绝。
	_, err = ParsePlan(testTeam(), "目标", ModeBroadcast, "quorum", broadcastPlan())
	if err == nil || !strings.Contains(err.Error(), "未知收集策略：quorum") {
		t.Fatalf("未知收集策略应拒绝，实际 %v", err)
	}
	// 空值归一 all；broadcast + all 合法。
	parsed, err := ParsePlan(testTeam(), "目标", ModeBroadcast, "", broadcastPlan())
	if err != nil {
		t.Fatalf("broadcast + 空收集策略应接受: %v", err)
	}
	if parsed.CollectPolicy != RunCollectAll {
		t.Fatalf("空收集策略应归一 all，实际 %q", parsed.CollectPolicy)
	}
}

func TestPlanHashIncludesCollectPolicy(t *testing.T) {
	first, err := ParsePlan(testTeam(), "目标", ModeBroadcast, RunCollectAll, broadcastPlan())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	second, err := ParsePlan(testTeam(), "目标", ModeBroadcast, RunCollectFirstSuccess, broadcastPlan())
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if first.PlanHash == second.PlanHash {
		t.Fatal("收集策略不同 planHash 不应相同")
	}
	again, err := ParsePlan(testTeam(), "目标", ModeBroadcast, RunCollectAll, broadcastPlan())
	if err != nil || again.PlanHash != first.PlanHash {
		t.Fatalf("同计划重算 planHash 应稳定: %v", err)
	}
}
