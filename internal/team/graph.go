package team

// graph.go 是完整阶段图的纯函数投影（docs/team-orchestration-plan.md §11.3、
// §12 阶段四工作项 6）：从 Run 快照 + Team 成员表推导「稳定阶段节点 + 运行
// 次数」的阶段图。两条聚合规则：
//   - broadcast 组（BroadcastGroup 相同且成员 >1）聚合为单节点——并行分支
//     是一个阶段的多路执行，不是一个阶段的多次执行；
//   - autoPlan 修复链（actfix-{root}-{n}）按根任务聚合，runCount=链上任务数——
//     循环阶段只显示一个稳定阶段节点和运行次数（§11.3）；动作派生任务
//     （{source}-{s|d}{round}-{index}）是 Lead 新开的阶段，各自成节点
//     （graphStageRoot）。
//
// 边界（阶段四范围）：纯函数零 I/O、不落账、不发事件；分层用 Kahn 算法
// （planner 已拒绝显式环，防御性兜底把无法分层的剩余节点按原序追加为最后
// 一行，绝不因图数据异常报错）；不做图推送（拉取式端点）、不做 SVG 连线
// （web/team.js 以层间 ↓ 表达）。

import "fmt"

// graphResultHeadRunes 是节点结果摘要的截断长度（与 instructionsHead 的
// 240 rune 同口径，§10.3 有界 payload）。
const graphResultHeadRunes = instructionsHeadRunes

// graphStatusPrecedence 是聚合节点（broadcast 组/派生链）状态归并的注意力
// 序：在飞 > 需用户裁决 > 失败类 > 未开始 > 完成——一个阶段有多路/多次
// 执行时，徽标显示最需要关注的那一路。
var graphStatusPrecedence = []string{
	TaskDispatching, TaskRunning, TaskOutcomeUnknown,
	TaskFailed, TaskStopped, TaskCancelled,
	TaskPlanned, TaskReady, TaskCompleted,
}

// RunGraphNode 是阶段图的单个节点（HTTP /graph 端点的节点契约，键名与
// web/team.js 的 renderGraphNode 一一对应）：id 用于点击定位任务卡（单任务
// 节点是真实任务 ID；broadcast 聚合节点是计划条目 ID，memberIds>1 时前端
// 展开成员列表）；agentKinds 必带（成员类型徽标依赖，解析不到成员时为空
// 数组而非缺失）；runCount 是该阶段的执行次数（派生链聚合累加，普通节点
// 恒 1）；resultHead/truncated 是最新一次执行的有界结果摘要。
type RunGraphNode struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Status     string   `json:"status"`
	MemberIDs  []string `json:"memberIds"`
	AgentKinds []string `json:"agentKinds"`
	DependsOn  []string `json:"dependsOn"`
	RunCount   int      `json:"runCount"`
	ResultHead string   `json:"resultHead,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
}

// RunGraph 是一次 Run 的阶段图：nodes 按首次出现序稳定排列，layers 是 Kahn
// 分层后的节点 ID 行序（同层节点无相互依赖；前端也可自行按 dependsOn 分层，
// layers 供移动端直接布局）。
type RunGraph struct {
	Nodes  []RunGraphNode `json:"nodes"`
	Layers [][]string     `json:"layers"`
}

// BuildRunGraph 从 Run 快照推导阶段图（纯函数）：teamRow 仅供成员 agentKind
// 解析（成员已被移除时该成员无类型徽标，节点照常输出）；run 为 nil 返回空图
// （nodes/layers 恒为空数组而非 null）。
func BuildRunGraph(run *Run, teamRow *Team) RunGraph {
	graph := RunGraph{Nodes: []RunGraphNode{}, Layers: [][]string{}}
	if run == nil {
		return graph
	}
	// ① 节点归属：broadcast 组先数规模——单成员组退化为普通任务节点
	//（节点 id 用真实任务 ID，前端 revealTask 才能定位任务卡）；其余任务按
	// actionRepairRoot 归并派生链/修复链（actions.go 的根推导：先剥 -{s|d}
	// {round}-{index} 后缀（可嵌套）再剥 actfix-{root}-{n}）。
	groupSize := map[string]int{}
	for i := range run.Tasks {
		if run.Tasks[i].BroadcastGroup != "" {
			groupSize[run.Tasks[i].BroadcastGroup]++
		}
	}
	taskByID := map[string]*Task{}
	for i := range run.Tasks {
		taskByID[run.Tasks[i].ID] = &run.Tasks[i]
	}
	keyOfTask := func(task *Task) string {
		if task.BroadcastGroup != "" && groupSize[task.BroadcastGroup] > 1 {
			return "group:" + task.BroadcastGroup
		}
		return "task:" + graphStageRoot(task.ID)
	}
	type nodeBuilder struct {
		id      string // 展示 ID（重复时加 #N 后缀消歧）
		isGroup bool
		tasks   []*Task
	}
	builders := map[string]*nodeBuilder{}
	order := []string{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		key := keyOfTask(task)
		builder, exists := builders[key]
		if !exists {
			id := graphStageRoot(task.ID)
			if task.BroadcastGroup != "" && groupSize[task.BroadcastGroup] > 1 {
				id = task.BroadcastGroup
				builder = &nodeBuilder{id: id, isGroup: true}
			} else {
				builder = &nodeBuilder{id: id}
			}
			builders[key] = builder
			order = append(order, key)
		}
		builder.tasks = append(builder.tasks, task)
	}
	// 展示 ID 消歧：单成员 broadcast 组的真实任务 ID（{entry}-{member}）在
	// 病态计划形状下可能与另一条目的组 ID 撞号（条目 "a"+成员 "b" 对条目
	// "a-b"），重复时后者加 #N 后缀——分层索引与前端定位都以唯一 ID 为键。
	seenIDs := map[string]int{}
	for _, key := range order {
		builder := builders[key]
		seenIDs[builder.id]++
		if seenIDs[builder.id] > 1 {
			builder.id = fmt.Sprintf("%s#%d", builder.id, seenIDs[builder.id])
		}
	}
	// ② 逐节点组装：成员/类型去重保序、依赖映射到节点 ID（组内互依赖折叠、
	// 未知依赖跳过——计划外引用不阻塞渲染）、状态与结果按聚合规则取值。
	agentKindOf := func(memberID string) string {
		if teamRow == nil {
			return ""
		}
		for i := range teamRow.Members {
			if teamRow.Members[i].ID == memberID {
				return teamRow.Members[i].AgentKind
			}
		}
		return ""
	}
	appendUnique := func(values []string, value string) []string {
		for _, existing := range values {
			if existing == value {
				return values
			}
		}
		return append(values, value)
	}
	for _, key := range order {
		builder := builders[key]
		node := RunGraphNode{
			ID:         builder.id,
			Label:      builder.id,
			MemberIDs:  []string{},
			AgentKinds: []string{},
			DependsOn:  []string{},
			RunCount:   1,
		}
		for _, task := range builder.tasks {
			node.MemberIDs = appendUnique(node.MemberIDs, task.MemberID)
			if kind := agentKindOf(task.MemberID); kind != "" {
				node.AgentKinds = appendUnique(node.AgentKinds, kind)
			}
			for _, dep := range task.DependsOn {
				if depTask, exists := taskByID[dep]; exists {
					if depKey := keyOfTask(depTask); depKey != key {
						node.DependsOn = appendUnique(node.DependsOn, builders[depKey].id)
					}
				}
			}
		}
		if builder.isGroup {
			// broadcast 组：一次条目执行的多路展开——runCount 恒 1，状态按
			// 注意力序归并，结果取首个携带结果的分支。
			node.Status = graphAggregateStatus(builder.tasks)
			for _, task := range builder.tasks {
				if head, truncated, found := graphResultHead(task); found {
					node.ResultHead, node.Truncated = head, truncated
					break
				}
			}
		} else {
			// 派生链/单任务：快照序即创建序（计划批次在前、派生/修复随轮次
			// 追加），链上最后一次执行代表阶段现状；runCount=链长。
			node.Status = builder.tasks[len(builder.tasks)-1].Status
			node.RunCount = len(builder.tasks)
			for index := len(builder.tasks) - 1; index >= 0; index-- {
				if head, truncated, found := graphResultHead(builder.tasks[index]); found {
					node.ResultHead, node.Truncated = head, truncated
					break
				}
			}
		}
		graph.Nodes = append(graph.Nodes, node)
	}
	graph.Layers = graphLayers(graph.Nodes)
	return graph
}

// graphStageRoot 返回任务在阶段图上的稳定阶段 ID：只有修复任务本体
// （actfix-{root}-{n}，同一阶段的重跑）折回 root；动作派生任务（{source}-
// {s|d}{round}-{index}）成员/提示词/依赖各异，是新阶段，保持自身 ID——按
// actionRepairRoot 剥派生后缀会把整轮委派压成一个「lead ×N」节点。
func graphStageRoot(taskID string) string {
	if stripDerivedTaskSuffix(taskID) != taskID {
		return taskID
	}
	return actionRepairRoot(taskID)
}

// graphAggregateStatus 归并聚合节点的成员状态：按 graphStatusPrecedence 取
// 首个出现的等级（在飞 > 需用户 > 失败类 > 未开始 > 完成），全部未知时退
// 回最后一个成员的状态。
func graphAggregateStatus(tasks []*Task) string {
	present := map[string]bool{}
	for _, task := range tasks {
		present[task.Status] = true
	}
	for _, status := range graphStatusPrecedence {
		if present[status] {
			return status
		}
	}
	if len(tasks) > 0 {
		return tasks[len(tasks)-1].Status
	}
	return ""
}

// graphResultHead 提取任务的有界结果摘要：head 截 240 rune（与任务卡
// instructionsHead 同口径）；truncated 在结果本体已被 ResultDetail.Truncated
// 截断或摘要再截断时为 true（用户据此知道看到的不是全文）。无结果返回
// found=false。
func graphResultHead(task *Task) (string, bool, bool) {
	if task == nil {
		return "", false, false
	}
	// 只含动作块的 Lead 结果没有可读正文：视为无结果，节点回退链上更早的结果。
	text := ResultDisplayText(task.Result)
	if text == "" {
		return "", false, false
	}
	truncated := len([]rune(text)) > graphResultHeadRunes
	if task.ResultDetail != nil && task.ResultDetail.Truncated {
		truncated = true
	}
	return truncateRunes(text, graphResultHeadRunes), truncated, true
}

// graphLayers 按 Kahn 算法分层：节点入层当且仅当其（节点集内的）依赖全部
// 落在更早层，同层按 nodes 序稳定排列。显式计划由 planner 拒绝环；防御性
// 兜底把无法分层的剩余节点按原序追加为最后一行（派生动作的依赖在异常快照
// 上可能成环，绝不因图数据异常阻塞渲染）。
func graphLayers(nodes []RunGraphNode) [][]string {
	if len(nodes) == 0 {
		return [][]string{}
	}
	index := map[string]int{}
	placed := map[string]bool{}
	for i := range nodes {
		index[nodes[i].ID] = i
	}
	remaining := make([]int, 0, len(nodes))
	for i := range nodes {
		remaining = append(remaining, i)
	}
	layers := [][]string{}
	for len(remaining) > 0 {
		layer := []string{}
		next := []int{}
		for _, i := range remaining {
			ready := true
			for _, dep := range nodes[i].DependsOn {
				if at, exists := index[dep]; exists && !placed[dep] && at != i {
					ready = false
					break
				}
			}
			if ready {
				layer = append(layer, nodes[i].ID)
			} else {
				next = append(next, i)
			}
		}
		if len(layer) == 0 {
			// 环或自依赖残留：按原序收尾，不再迭代。
			for _, i := range remaining {
				layer = append(layer, nodes[i].ID)
			}
			layers = append(layers, layer)
			return layers
		}
		for _, id := range layer {
			placed[id] = true
		}
		layers = append(layers, layer)
		remaining = next
	}
	return layers
}
