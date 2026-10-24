package team

// planner.go 是结构化计划的解析与校验（§5.2/§5.3、§12 工作项 4）：DAG 校验、
// 循环创建时拒绝（报错含环路径）、prompt 长度预留上下文信封余量、任务数与
// 成员可用性检查，输出规范化任务列表（拓扑 Ordinal）、planHash 与创建期
// InputHash。纯函数：不读写文件、不碰会话；真实 I/O 由 team-dispatch 经
// 接口完成（§3.1 原则 3）。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// 运行模式（§5.3）。broadcast 属阶段二，创建期直接拒绝。
const (
	ModeSingle    = "single"
	ModeBroadcast = "broadcast"
	ModePipeline  = "pipeline"
)

// 任务级写锁取值（§6.4/§10.2）：exclusive 需独占 cwd 写锁，none 只读可并行。
const (
	TaskWriteLockExclusive = "exclusive"
	TaskWriteLockNone      = "none"
)

// promptHardLimit 是账本侧（internal/httpd/queue.go validateSubmissionInput）
// 对合成后全文的字数硬上限；envelopeReserve 是 [Team context]/[Assigned task]
// 信封与来源行的固定 rune 开销（§7.1）。创建期校验按 rune 计：
// len([]rune(prompt)) ≤ promptHardLimit − policy.MaxContextChars − envelopeReserve，
// 不留余量则 88000–100000 字区间的带依赖任务会在派发时才确定性失败。
const (
	promptHardLimit = 100000
	envelopeReserve = 200
)

// PlanTaskInput 是创建 Run 请求里的单条计划任务（§10.2 plan 数组元素）。
type PlanTaskInput struct {
	ID            string   `json:"id"`
	MemberID      string   `json:"memberId"`
	Prompt        string   `json:"prompt"`
	DependsOn     []string `json:"dependsOn,omitempty"`
	WriteLock     string   `json:"writeLock,omitempty"`
	WorkspaceMode string   `json:"workspaceMode,omitempty"`
}

// Plan 是 ParsePlan 的规范化输出：任务按拓扑序排列并赋 Ordinal（0 起），
// planHash 覆盖规范化后的计划本体（模式 + 目标 + 任务确定性字段）。
type Plan struct {
	Mode     string
	Goal     string
	Tasks    []Task
	PlanHash string
}

// ParsePlan 校验并规范化显式计划（§5.2/§5.3）。校验规则：
//   - 任务 ID 唯一且过成员 ID 正则（字母/数字/下划线/连字符，1-64 字符）；
//   - memberId 必须是 Team 的既有且 enabled 成员；
//   - prompt 非空，且 rune 数 ≤ 100000 − MaxContextChars − 200（信封余量）；
//   - dependsOn 引用存在的任务，自依赖与循环在创建时即拒绝；
//   - 任务数 ≤ policy.MaxTasksPerRun；
//   - writeLock ∈ {none, exclusive}（空值归一为 exclusive，§6.4 默认写策略）；
//   - workspaceMode 阶段一只接受 shared_workspace（isolated_worktree 属阶段二）。
//
// 模式规则（§5.3）：single 空计划合成单条 Lead 任务（prompt=goal），恰一条
// 也接受，多于一条拒绝；pipeline 必须显式 plan ≥1；broadcast 直接拒绝。
func ParsePlan(team *Team, goal string, mode string, raw []PlanTaskInput) (*Plan, error) {
	if team == nil {
		return nil, errors.New("team 不能为 nil")
	}
	switch mode {
	case ModeSingle, ModePipeline:
	case ModeBroadcast:
		return nil, errors.New("并行分派在后续阶段提供")
	default:
		return nil, fmt.Errorf("未知运行模式：%s", mode)
	}
	if strings.TrimSpace(goal) == "" {
		return nil, errors.New("请填写运行目标")
	}
	limit := PromptLimit(policyMaxContextChars(team))
	if len(raw) > team.Policy.MaxTasksPerRun {
		return nil, fmt.Errorf("任务数超过上限：最多 %d 条，收到 %d 条", team.Policy.MaxTasksPerRun, len(raw))
	}
	if mode == ModeSingle && len(raw) > 1 {
		return nil, errors.New("单任务模式最多一条任务；多条任务请使用流水线模式")
	}

	// 逐条校验字段（按输入顺序报第一处错误），并归一化写锁与工作区模式。
	normalized := make([]PlanTaskInput, 0, len(raw))
	seen := map[string]bool{}
	members := map[string]TeamMember{}
	for i := range team.Members {
		members[team.Members[i].ID] = team.Members[i]
	}
	for i, input := range raw {
		if !ValidMemberID(input.ID) {
			return nil, fmt.Errorf("第 %d 条任务 ID 无效：%q（字母/数字/下划线/连字符，1-64 字符）", i+1, input.ID)
		}
		if seen[input.ID] {
			return nil, fmt.Errorf("任务 ID 重复：%s", input.ID)
		}
		seen[input.ID] = true
		member, exists := members[input.MemberID]
		if !exists {
			return nil, fmt.Errorf("任务 %s 引用了不存在的成员：%s", input.ID, input.MemberID)
		}
		if !member.Enabled {
			return nil, fmt.Errorf("任务 %s 引用了已停用的成员：%s", input.ID, input.MemberID)
		}
		if strings.TrimSpace(input.Prompt) == "" {
			return nil, fmt.Errorf("任务 %s 的内容不能为空", input.ID)
		}
		if runes := len([]rune(input.Prompt)); runes > limit {
			return nil, fmt.Errorf("任务 %s 的内容过长：%d 字，加上成员上下文与信封后不能超过 %d 字，当前上限 %d 字",
				input.ID, runes, promptHardLimit, limit)
		}
		switch input.WriteLock {
		case "", TaskWriteLockExclusive, TaskWriteLockNone:
		default:
			return nil, fmt.Errorf("任务 %s 的写锁取值无效：%s（可用：exclusive、none）", input.ID, input.WriteLock)
		}
		if input.WorkspaceMode == "" {
			input.WorkspaceMode = team.Policy.WorkspaceMode
		}
		switch input.WorkspaceMode {
		case WorkspaceShared:
		case WorkspaceIsolatedWorktree:
			return nil, fmt.Errorf("任务 %s 要求独立工作区：独立工作区在后续阶段提供", input.ID)
		default:
			return nil, fmt.Errorf("任务 %s 的工作区模式无效：%s", input.ID, input.WorkspaceMode)
		}
		normalized = append(normalized, input)
	}

	// 依赖引用与循环检测（§5.3：循环在创建时即拒绝）。
	dependencies := map[string][]string{}
	for _, input := range normalized {
		deps := make([]string, 0, len(input.DependsOn))
		for _, dep := range input.DependsOn {
			if !seen[dep] {
				return nil, fmt.Errorf("任务 %s 依赖了不存在的任务：%s", input.ID, dep)
			}
			if dep == input.ID {
				return nil, fmt.Errorf("计划存在循环依赖：%s → %s", input.ID, input.ID)
			}
			duplicate := false
			for _, existing := range deps {
				if existing == dep {
					duplicate = true
					break
				}
			}
			if !duplicate {
				deps = append(deps, dep)
			}
		}
		dependencies[input.ID] = deps
	}
	if cycle := findDependencyCycle(normalized, dependencies); cycle != "" {
		return nil, errors.New("计划存在循环依赖：" + cycle)
	}

	// single 空计划：合成单条 Lead 任务（prompt=goal）。
	if mode == ModeSingle && len(normalized) == 0 {
		if team.LeadID == "" {
			return nil, errors.New("单任务模式需要先为团队指定 Lead 成员")
		}
		lead, exists := members[team.LeadID]
		if !exists {
			return nil, fmt.Errorf("团队 Lead 成员不存在：%s", team.LeadID)
		}
		if !lead.Enabled {
			return nil, fmt.Errorf("团队 Lead 成员已停用：%s", team.LeadID)
		}
		if runes := len([]rune(goal)); runes > limit {
			return nil, fmt.Errorf("运行目标过长：%d 字，当前上限 %d 字", runes, limit)
		}
		normalized = append(normalized, PlanTaskInput{
			ID:            "lead",
			MemberID:      team.LeadID,
			Prompt:        goal,
			WriteLock:     TaskWriteLockExclusive,
			WorkspaceMode: WorkspaceShared,
		})
	}
	if mode == ModePipeline && len(normalized) == 0 {
		return nil, errors.New("流水线模式需要显式计划：至少提供一条任务")
	}
	if len(normalized) > team.Policy.MaxTasksPerRun {
		return nil, fmt.Errorf("任务数超过上限：最多 %d 条", team.Policy.MaxTasksPerRun)
	}

	// 拓扑排序（稳定：同层按输入顺序），赋 Ordinal 与创建期确定性字段。
	order := topologicalOrder(normalized, dependencies)
	policyDigest := PolicyDigest(team.Policy)
	tasks := make([]Task, 0, len(order))
	for ordinal, input := range order {
		writeLock := input.WriteLock
		if writeLock == "" {
			writeLock = TaskWriteLockExclusive
		}
		task := Task{
			ID:               input.ID,
			MemberID:         input.MemberID,
			Prompt:           input.Prompt,
			Attempt:          0,
			Ordinal:          ordinal,
			DependsOn:        dependencies[input.ID],
			WorkspaceMode:    input.WorkspaceMode,
			WriteLock:        writeLock,
			Status:           TaskPlanned,
			InstructionsHead: InstructionsHead(input.Prompt),
		}
		task.InputHash = InputHashFor(&task, policyDigest, "")
		tasks = append(tasks, task)
	}
	return &Plan{
		Mode:     mode,
		Goal:     goal,
		Tasks:    tasks,
		PlanHash: PlanHash(mode, goal, tasks),
	}, nil
}

// PromptLimit 返回创建期 prompt 的有效字数上限（rune）：
// 100000 − maxContextChars − 200。默认策略（12000）下为 87800 字。
func PromptLimit(maxContextChars int) int {
	return promptHardLimit - maxContextChars - envelopeReserve
}

func policyMaxContextChars(team *Team) int { return team.Policy.MaxContextChars }

// PolicyDigest 是策略的规范化摘要（InputHash 的组成部分，§8.2）：对
// jsonObject 的有序编码取 sha256，字段顺序固定所以同一策略摘要稳定。
func PolicyDigest(policy TeamPolicy) string {
	sum := sha256.Sum256(jsoncompat.EncodeCompact(policy.jsonObject()))
	return hex.EncodeToString(sum[:])
}

// PlanHash 计算规范化计划的摘要：模式 + 目标 + 每任务的确定性字段
// （id/memberId/prompt/dependsOn/workspaceMode/writeLock），拓扑序排列。
// 与任务运行态字段（status/result/submissionId 等）无关，同一计划重算稳定。
func PlanHash(mode, goal string, tasks []Task) string {
	object := jsoncompat.NewObject()
	object.Set("mode", mode)
	object.Set("goal", goal)
	list := &jsoncompat.Array{}
	for i := range tasks {
		task := &tasks[i]
		entry := jsoncompat.NewObject()
		entry.Set("id", task.ID)
		entry.Set("memberId", task.MemberID)
		entry.Set("prompt", task.Prompt)
		depends := &jsoncompat.Array{}
		for _, dep := range task.DependsOn {
			depends.Items = append(depends.Items, dep)
		}
		entry.Set("dependsOn", depends)
		entry.Set("workspaceMode", task.WorkspaceMode)
		entry.Set("writeLock", task.WriteLock)
		list.Items = append(list.Items, entry)
	}
	object.Set("tasks", list)
	sum := sha256.Sum256(jsoncompat.EncodeCompact(object))
	return hex.EncodeToString(sum[:])
}

// InputHashFor 计算任务输入摘要（§8.2）：规范化的成员、Prompt、依赖摘要、
// 工作区与写锁、策略摘要与上下文文本。planner 版本只覆盖创建期确定性字段
// （contextText 为空）；完整含上下文的哈希由 dispatcher 派发时用真实注入
// 上下文重算并落 journal——同一任务槽出现两个不同哈希即 input_hash_mismatch。
func InputHashFor(task *Task, policyDigest, contextText string) string {
	object := jsoncompat.NewObject()
	object.Set("memberId", task.MemberID)
	object.Set("prompt", task.Prompt)
	depends := &jsoncompat.Array{}
	for _, dep := range task.DependsOn {
		depends.Items = append(depends.Items, dep)
	}
	object.Set("dependsOn", depends)
	object.Set("workspaceMode", task.WorkspaceMode)
	object.Set("writeLock", task.WriteLock)
	object.Set("policyDigest", policyDigest)
	object.Set("contextText", contextText)
	sum := sha256.Sum256(jsoncompat.EncodeCompact(object))
	return hex.EncodeToString(sum[:])
}

// findDependencyCycle 沿 dependsOn 边做 DFS，遇到回边时返回环路径
// （"a → b → a"）；无环返回空串。
func findDependencyCycle(inputs []PlanTaskInput, dependencies map[string][]string) string {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var stack []string
	var visit func(id string) string
	visit = func(id string) string {
		state[id] = visiting
		stack = append(stack, id)
		for _, dep := range dependencies[id] {
			switch state[dep] {
			case visiting:
				// 回边：从栈中依赖的位置切出环，补上当前节点闭合。
				at := 0
				for i, node := range stack {
					if node == dep {
						at = i
						break
					}
				}
				return strings.Join(append(append([]string{}, stack[at:]...), dep), " → ")
			case done:
			default:
				if cycle := visit(dep); cycle != "" {
					return cycle
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = done
		return ""
	}
	for _, input := range inputs {
		if state[input.ID] == 0 {
			stack = stack[:0]
			if cycle := visit(input.ID); cycle != "" {
				return cycle
			}
		}
	}
	return ""
}

// topologicalOrder 按 Kahn 算法稳定排序：每轮取出输入顺序最靠前的已就绪
// 任务，保证同一计划重算结果确定。findDependencyCycle 已在调用前拒绝环路。
func topologicalOrder(inputs []PlanTaskInput, dependencies map[string][]string) []PlanTaskInput {
	pending := map[string]int{}
	for id, deps := range dependencies {
		// 依赖引用已在前面校验为既有任务，未完成依赖数即依赖总数。
		pending[id] = len(deps)
	}
	ordered := map[string]bool{}
	order := make([]PlanTaskInput, 0, len(inputs))
	for len(order) < len(inputs) {
		progress := false
		for i, input := range inputs {
			if ordered[input.ID] || pending[input.ID] > 0 {
				continue
			}
			order = append(order, inputs[i])
			ordered[input.ID] = true
			progress = true
			for id, deps := range dependencies {
				if ordered[id] {
					continue
				}
				for _, dep := range deps {
					if dep == input.ID {
						pending[id]--
						break
					}
				}
			}
		}
		if !progress {
			break // 防御：有环时放弃，剩余任务按输入顺序补尾。
		}
	}
	for i, input := range inputs {
		if !ordered[input.ID] {
			order = append(order, inputs[i])
			ordered[input.ID] = true
		}
	}
	return order
}

// sortTasksByOrdinal 按 Ordinal 稳定排序任务副本（同 Ordinal 保持原顺序），
// 供调度与 fold 以确定顺序遍历。
func sortTasksByOrdinal(tasks []Task) []Task {
	sorted := append([]Task(nil), tasks...)
	sort.SliceStable(sorted, func(a, b int) bool {
		return sorted[a].Ordinal < sorted[b].Ordinal
	})
	return sorted
}
