package team

// graph_test.go 覆盖 BuildRunGraph 的聚合与分层规则（§11.3/§12 阶段四工作项
// 6）：broadcast 组聚合单节点、autoPlan 派生链/修复链按根任务聚合 runCount
// 累加、依赖映射到节点 ID、Kahn 分层与环兜底、结果摘要 240 rune 有界、
// agentKinds 必带、ID 撞号消歧。全部纯函数表驱动，零 I/O。

import (
	"fmt"
	"strings"
	"testing"
)

func graphTestTeam() *Team {
	return &Team{ID: "t1", LeadID: "lead", Policy: DefaultPolicy(), Members: []TeamMember{
		{ID: "lead", SessionID: "s1", AgentKind: "kimi", Role: RoleLead, Enabled: true},
		{ID: "dev", SessionID: "s2", AgentKind: "zcode", Role: RoleMember, Enabled: true},
		{ID: "rev", SessionID: "s3", AgentKind: "claude", Role: RoleReviewer, Enabled: true},
	}}
}

func graphNodeOf(t *testing.T, graph RunGraph, id string) RunGraphNode {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("节点不存在: %s（现有 %+v）", id, graph.Nodes)
	return RunGraphNode{}
}

func TestBuildRunGraphNilRunEmptyArrays(t *testing.T) {
	graph := BuildRunGraph(nil, graphTestTeam())
	if graph.Nodes == nil || graph.Layers == nil {
		t.Fatalf("空图的 nodes/layers 应为空数组而非 null: %+v", graph)
	}
	if len(graph.Nodes) != 0 || len(graph.Layers) != 0 {
		t.Fatalf("空图应无节点无层: %+v", graph)
	}
}

func TestBuildRunGraphPipelineLayersAndKinds(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		{ID: "design", MemberID: "lead", Status: TaskCompleted, Ordinal: 1,
			Result: "设计完成", ResultDetail: &TaskResult{SessionID: "s1", TurnID: "turn-1"}},
		{ID: "implement", MemberID: "dev", Status: TaskRunning, Ordinal: 2, DependsOn: []string{"design"}},
		{ID: "review", MemberID: "rev", Status: TaskPlanned, Ordinal: 3, DependsOn: []string{"implement"}},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Nodes) != 3 {
		t.Fatalf("应有三节点: %+v", graph.Nodes)
	}
	if node := graphNodeOf(t, graph, "design"); node.RunCount != 1 || node.Status != TaskCompleted ||
		node.ResultHead != "设计完成" || node.Truncated {
		t.Fatalf("design 节点形状异常: %+v", node)
	}
	if kinds := graphNodeOf(t, graph, "implement").AgentKinds; len(kinds) != 1 || kinds[0] != "zcode" {
		t.Fatalf("implement agentKinds 应来自成员表: %+v", kinds)
	}
	if deps := graphNodeOf(t, graph, "review").DependsOn; len(deps) != 1 || deps[0] != "implement" {
		t.Fatalf("review 依赖应映射到节点 ID: %+v", deps)
	}
	if len(graph.Layers) != 3 || graph.Layers[0][0] != "design" ||
		graph.Layers[1][0] != "implement" || graph.Layers[2][0] != "review" {
		t.Fatalf("Kahn 分层应按依赖逐层: %+v", graph.Layers)
	}
}

func TestBuildRunGraphBroadcastGroupAggregates(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunCollecting, Tasks: []Task{
		{ID: "scan-lead", MemberID: "lead", Status: TaskCompleted, BroadcastGroup: "scan",
			DependsOn: []string{"prep"}, Result: "分支 A 完成"},
		{ID: "scan-dev", MemberID: "dev", Status: TaskFailed, BroadcastGroup: "scan", DependsOn: []string{"prep"}},
		{ID: "prep", MemberID: "lead", Status: TaskCompleted},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Nodes) != 2 {
		t.Fatalf("broadcast 组应聚合为单节点: %+v", graph.Nodes)
	}
	scan := graphNodeOf(t, graph, "scan")
	if scan.RunCount != 1 {
		t.Fatalf("组是同一次条目执行的多路展开，runCount 恒 1: %+v", scan)
	}
	if len(scan.MemberIDs) != 2 || scan.MemberIDs[0] != "lead" || scan.MemberIDs[1] != "dev" {
		t.Fatalf("组节点应聚合全部成员: %+v", scan.MemberIDs)
	}
	if len(scan.AgentKinds) != 2 || scan.AgentKinds[0] != "kimi" || scan.AgentKinds[1] != "zcode" {
		t.Fatalf("组节点 agentKinds 应按成员序去重: %+v", scan.AgentKinds)
	}
	if scan.Status != TaskFailed {
		t.Fatalf("组状态按注意力序归并（failed 先于 completed）: %+v", scan.Status)
	}
	if len(scan.DependsOn) != 1 || scan.DependsOn[0] != "prep" {
		t.Fatalf("组依赖应映射到节点 ID（展开任务依赖折叠）: %+v", scan.DependsOn)
	}
	if scan.ResultHead != "分支 A 完成" {
		t.Fatalf("组结果取首个携带结果的分支: %+v", scan.ResultHead)
	}
}

func TestBuildRunGraphSingleMemberGroupCollapsesToTaskNode(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		// 单成员 broadcast 组：节点 id 用真实任务 ID（前端 revealTask 定位）。
		{ID: "only-lead", MemberID: "lead", Status: TaskRunning, BroadcastGroup: "only"},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Nodes) != 1 || graph.Nodes[0].ID != "only-lead" {
		t.Fatalf("单成员组应退化为普通任务节点: %+v", graph.Nodes)
	}
	if graph.Nodes[0].RunCount != 1 || graph.Nodes[0].Status != TaskRunning {
		t.Fatalf("单任务节点形状异常: %+v", graph.Nodes[0])
	}
}

func TestBuildRunGraphRepairChainAggregatesRunCount(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		// §11.3 循环阶段只指同一阶段的重跑：根任务 + 修复任务（actfix-）聚合为
		// 稳定节点、runCount=2、状态取链上最新；动作派生任务是新阶段，各自成节点。
		{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 1, Result: "第一轮规划"},
		{ID: "lead-d1-0", MemberID: "dev", Status: TaskCompleted, Ordinal: 2,
			DependsOn: []string{"lead"}, Result: "委派执行完成"},
		{ID: "actfix-lead-1", MemberID: "lead", Status: TaskFailed, Ordinal: 3},
		{ID: "lead-s2-0", MemberID: "rev", Status: TaskRunning, Ordinal: 4, DependsOn: []string{"lead-d1-0"}},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Nodes) != 3 {
		t.Fatalf("修复链聚合、派生任务独立成节点，应有三节点: %+v", graph.Nodes)
	}
	lead := graphNodeOf(t, graph, "lead")
	if lead.RunCount != 2 || lead.Status != TaskFailed || lead.ResultHead != "第一轮规划" {
		t.Fatalf("修复链节点 runCount=2、状态取最新、结果取最近带结果的执行: %+v", lead)
	}
	if delegated := graphNodeOf(t, graph, "lead-d1-0"); delegated.RunCount != 1 || delegated.Status != TaskCompleted ||
		len(delegated.DependsOn) != 1 || delegated.DependsOn[0] != "lead" {
		t.Fatalf("委派任务应独立成节点并依赖 lead: %+v", delegated)
	}
	if summary := graphNodeOf(t, graph, "lead-s2-0"); summary.Status != TaskRunning ||
		len(summary.DependsOn) != 1 || summary.DependsOn[0] != "lead-d1-0" {
		t.Fatalf("汇总任务应独立成节点并依赖委派任务: %+v", summary)
	}
	if len(graph.Layers) != 3 || graph.Layers[0][0] != "lead" || graph.Layers[1][0] != "lead-d1-0" || graph.Layers[2][0] != "lead-s2-0" {
		t.Fatalf("派生任务按真实依赖分层: %+v", graph.Layers)
	}
}

func TestBuildRunGraphDelegateFanOutKeepsPerTaskStatus(t *testing.T) {
	// 一轮委派扇出（d157 形态）：每个任务保持自己的状态，绝不被末个 planned 覆盖。
	run := &Run{ID: "r1", TeamID: "t1", Status: RunWaitingInput, Tasks: []Task{
		{ID: "lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 1},
		{ID: "lead-d1-0", MemberID: "dev", Status: TaskStopped, Outcome: "auth", Ordinal: 2, DependsOn: []string{"lead"}},
		{ID: "lead-d1-1", MemberID: "rev", Status: TaskFailed, Ordinal: 3, DependsOn: []string{"lead"}},
		{ID: "lead-d1-2", MemberID: "lead", Status: TaskPlanned, Ordinal: 4, DependsOn: []string{"lead", "lead-d1-0", "lead-d1-1"}},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	want := map[string]string{"lead": TaskCompleted, "lead-d1-0": TaskStopped, "lead-d1-1": TaskFailed, "lead-d1-2": TaskPlanned}
	if len(graph.Nodes) != len(want) {
		t.Fatalf("扇出任务应各自成节点: %+v", graph.Nodes)
	}
	for id, status := range want {
		if node := graphNodeOf(t, graph, id); node.Status != status || node.RunCount != 1 || len(node.MemberIDs) != 1 {
			t.Fatalf("节点 %s 应保持自身状态 %s: %+v", id, status, node)
		}
	}
	if len(graph.Layers) != 3 || len(graph.Layers[1]) != 2 || graph.Layers[2][0] != "lead-d1-2" {
		t.Fatalf("应分三层（Lead / 并行 2 / 汇总）: %+v", graph.Layers)
	}
}

func TestBuildRunGraphResultHeadBounded(t *testing.T) {
	long := strings.Repeat("长", 300)
	run := &Run{ID: "r1", TeamID: "t1", Status: RunCompleted, Tasks: []Task{
		{ID: "a", MemberID: "lead", Status: TaskCompleted, Result: long,
			ResultDetail: &TaskResult{SessionID: "s1", TurnID: "t1"}},
		{ID: "b", MemberID: "lead", Status: TaskCompleted,
			ResultDetail: &TaskResult{SessionID: "s1", TurnID: "t2", Result: "短", Truncated: true}},
	}}
	// b 无 Result 本体但 ResultDetail 声明截断：无摘要可发（head 空是诚实
	// 形态——结果本体为空，truncated 标记无从附着）。
	graph := BuildRunGraph(run, graphTestTeam())
	node := graphNodeOf(t, graph, "a")
	if got := len([]rune(node.ResultHead)); got != graphResultHeadRunes {
		t.Fatalf("结果摘要应截 240 rune: %d", got)
	}
	if !node.Truncated {
		t.Fatal("摘要再截断应置 truncated")
	}
	node = graphNodeOf(t, graph, "b")
	if node.ResultHead != "" || node.Truncated {
		t.Fatalf("无结果本体的任务不应产出摘要: %+v", node)
	}
	// ResultDetail.Truncated 单独足以标记截断（本体已不是全文）。
	run.Tasks[0] = Task{ID: "a", MemberID: "lead", Status: TaskCompleted, Result: "短文本",
		ResultDetail: &TaskResult{SessionID: "s1", TurnID: "t3", Truncated: true}}
	graph = BuildRunGraph(run, graphTestTeam())
	if node = graphNodeOf(t, graph, "a"); node.ResultHead != "短文本" || !node.Truncated {
		t.Fatalf("ResultDetail 截断标记应透传: %+v", node)
	}
}

func TestBuildRunGraphResultHeadStripsActionBlocks(t *testing.T) {
	envelope := "<team-actions>\n{\"actions\":[{\"type\":\"delegate\",\"member\":\"dev\",\"task\":\"实现\"}]}\n</team-actions>"
	run := &Run{ID: "r1", TeamID: "t1", Status: RunCompleted, Tasks: []Task{
		{ID: "only", MemberID: "lead", Status: TaskCompleted, Result: envelope},
		{ID: "mixed", MemberID: "lead", Status: TaskCompleted, Result: "先说明计划\n" + envelope + "\n"},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	// 只含动作块的结果没有可读正文：不出摘要，绝不把协议 JSON 当预览。
	if node := graphNodeOf(t, graph, "only"); node.ResultHead != "" || node.Truncated {
		t.Fatalf("纯动作块结果不应产出摘要: %+v", node)
	}
	if node := graphNodeOf(t, graph, "mixed"); node.ResultHead != "先说明计划" {
		t.Fatalf("摘要应剥离动作块只留正文: %q", node.ResultHead)
	}
}

func TestBuildRunGraphUnknownDependencySkipped(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		{ID: "a", MemberID: "lead", Status: TaskPlanned, DependsOn: []string{"ghost", "a"}},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	node := graphNodeOf(t, graph, "a")
	if len(node.DependsOn) != 0 {
		t.Fatalf("计划外/自依赖应跳过: %+v", node.DependsOn)
	}
}

func TestBuildRunGraphCycleFallsBackToFinalLayer(t *testing.T) {
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		{ID: "a", MemberID: "lead", Status: TaskPlanned, DependsOn: []string{"b"}},
		{ID: "b", MemberID: "dev", Status: TaskPlanned, DependsOn: []string{"a"}},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Layers) != 1 || len(graph.Layers[0]) != 2 {
		t.Fatalf("环依赖应兜底为最后一行: %+v", graph.Layers)
	}
}

func TestBuildRunGraphNodeIDCollisionDisambiguated(t *testing.T) {
	// 病态计划形状：单成员组条目 "a"（成员 b → 任务 a-b）与条目 "a-b" 的组
	// 节点撞号——后者加 #2 后缀，节点 ID 保持唯一。
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		{ID: "a-b", MemberID: "lead", Status: TaskRunning, BroadcastGroup: "a"},
		{ID: "a-b-dev", MemberID: "dev", Status: TaskRunning, BroadcastGroup: "a-b"},
		{ID: "a-b-rev", MemberID: "rev", Status: TaskRunning, BroadcastGroup: "a-b"},
	}}
	graph := BuildRunGraph(run, graphTestTeam())
	if len(graph.Nodes) != 2 {
		t.Fatalf("应有两个节点: %+v", graph.Nodes)
	}
	ids := map[string]bool{}
	for _, node := range graph.Nodes {
		if ids[node.ID] {
			t.Fatalf("节点 ID 撞号: %+v", graph.Nodes)
		}
		ids[node.ID] = true
	}
	if !ids["a-b"] || !ids["a-b#2"] {
		t.Fatalf("撞号应加 #N 后缀消歧: %+v", ids)
	}
}

func TestBuildRunGraphAgentKindsAlwaysPresent(t *testing.T) {
	// 成员已被移除（引用失效保留任务）或 teamRow 为 nil：agentKinds 仍是
	// 空数组而非缺失/ null——web 成员徽标按数组消费。
	run := &Run{ID: "r1", TeamID: "t1", Status: RunDispatching, Tasks: []Task{
		{ID: "a", MemberID: "ghost", Status: TaskPlanned},
	}}
	for _, teamRow := range []*Team{nil, graphTestTeam()} {
		graph := BuildRunGraph(run, teamRow)
		node := graph.Nodes[0]
		if node.AgentKinds == nil || len(node.AgentKinds) != 0 {
			t.Fatalf("agentKinds 必带且可为空数组: %+v", node)
		}
		if node.MemberIDs == nil || len(node.MemberIDs) != 1 || node.MemberIDs[0] != "ghost" {
			t.Fatalf("成员引用保留原样: %+v", node.MemberIDs)
		}
	}
}

func TestBuildRunGraphStatusPrecedenceTable(t *testing.T) {
	cases := []struct {
		statuses []string
		want     string
	}{
		{[]string{TaskCompleted, TaskRunning}, TaskRunning},
		{[]string{TaskCompleted, TaskOutcomeUnknown}, TaskOutcomeUnknown},
		{[]string{TaskCancelled, TaskFailed}, TaskFailed},
		{[]string{TaskStopped, TaskCancelled}, TaskStopped},
		{[]string{TaskPlanned, TaskCancelled}, TaskCancelled},
		{[]string{TaskPlanned, TaskReady}, TaskPlanned},
		{[]string{TaskCompleted, TaskReady}, TaskReady},
		{[]string{TaskCompleted}, TaskCompleted},
	}
	for _, testCase := range cases {
		tasks := make([]*Task, 0, len(testCase.statuses))
		for i, status := range testCase.statuses {
			tasks = append(tasks, &Task{ID: fmt.Sprintf("g-m%d", i), MemberID: "lead", Status: status})
		}
		if got := graphAggregateStatus(tasks); got != testCase.want {
			t.Fatalf("状态归并 %v 应为 %s，实际 %s", testCase.statuses, testCase.want, got)
		}
	}
}
