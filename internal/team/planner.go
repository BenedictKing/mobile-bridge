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

// 运行模式（§5.3）：single 单任务、broadcast 并行分派（阶段二）、pipeline
// 依赖图。
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
// promptByteFloor 是 PromptByteLimit 的下限钳制（极端策略下不许把创建期
// 上限压成负数/零）。
const (
	promptHardLimit     = 100000
	envelopeReserve     = 200
	promptByteFloor     = 1000
	byteEnvelopeReserve = 512
)

// PlanTaskInput 是创建 Run 请求里的单条计划任务（§10.2 plan 数组元素）。
// broadcast 模式用 memberIds 指定 ≥2 个互异成员（memberId 留空）；其余模式
// 用 memberId（memberIds 留空）。workspaceId 是 isolated_worktree 任务的
// 工作区注册键（§6.4，必填且过成员 ID 正则——注册表键空间与成员 ID 同形）。
type PlanTaskInput struct {
	ID            string   `json:"id"`
	MemberID      string   `json:"memberId"`
	MemberIDs     []string `json:"memberIds,omitempty"`
	Prompt        string   `json:"prompt"`
	DependsOn     []string `json:"dependsOn,omitempty"`
	WriteLock     string   `json:"writeLock,omitempty"`
	WorkspaceMode string   `json:"workspaceMode,omitempty"`
	WorkspaceID   string   `json:"workspaceId,omitempty"`
}

// Plan 是 ParsePlan 的规范化输出：任务按拓扑序排列并赋 Ordinal（0 起），
// planHash 覆盖规范化后的计划本体（模式 + 收集策略 + 目标 + 任务确定性
// 字段）。broadcast 条目在输出前已展开为逐成员 Task（BroadcastGroup=条目
// ID，DependsOn 指向展开后的具体任务 ID）。
type Plan struct {
	Mode          string
	CollectPolicy string
	Goal          string
	Tasks         []Task
	PlanHash      string
}

// ParsePlan 校验并规范化显式计划（§5.2/§5.3）。校验规则：
//   - 任务 ID 唯一且过成员 ID 正则（字母/数字/下划线/连字符，1-64 字符）；
//   - 成员必须是 Team 的既有且 enabled 成员（broadcast 用 memberIds ≥2 个
//     互异成员，其余模式用 memberId）；
//   - prompt 非空，且 rune 数 ≤ 100000 − MaxContextChars − 200（信封余量）；
//   - dependsOn 引用存在的条目（broadcast 下引用其全部展开任务），自依赖
//     与循环在创建时即拒绝；
//   - 任务数 ≤ policy.MaxTasksPerRun（broadcast 按展开后计数）；
//   - writeLock ∈ {none, exclusive}（空值归一为 exclusive，§6.4 默认写策略）；
//   - workspaceMode ∈ {shared_workspace, isolated_worktree}（空值归一为团队
//     策略）；isolated_worktree 必须携带 workspaceId（工作区注册键，过成员
//     ID 正则，§6.4 阶段二：只引用用户预备的 worktree），shared_workspace
//     不允许携带 workspaceId。
//
// 模式规则（§5.3）：single 空计划合成单条 Lead 任务（prompt=goal），恰一条
// 也接受，多于一条拒绝；pipeline 必须显式 plan ≥1；broadcast 必须显式
// plan ≥1，每条展开为逐成员 Task（ID={entryID}-{memberID}，同组任务共享
// BroadcastGroup=entryID 与 prompt/writeLock，依赖 broadcast 条目=依赖其
// 全部展开任务）。收集策略：first_success 仅 broadcast 允许，其余强制 all。
func ParsePlan(team *Team, goal string, mode string, collect string, raw []PlanTaskInput) (*Plan, error) {
	if team == nil {
		return nil, errors.New("team 不能为 nil")
	}
	switch mode {
	case ModeSingle, ModePipeline, ModeBroadcast:
	default:
		return nil, fmt.Errorf("未知运行模式：%s", mode)
	}
	switch collect {
	case "", RunCollectAll:
		collect = RunCollectAll
	case RunCollectFirstSuccess:
		if mode != ModeBroadcast {
			return nil, errors.New("first_success 收集策略仅并行分派模式支持")
		}
	default:
		return nil, fmt.Errorf("未知收集策略：%s（可用：all、first_success）", collect)
	}
	if strings.TrimSpace(goal) == "" {
		return nil, errors.New("请填写运行目标")
	}
	limit := PromptLimit(policyMaxContextChars(team))
	byteLimit := PromptByteLimit(policyMaxContextChars(team))
	if len(raw) > team.Policy.MaxTasksPerRun {
		return nil, fmt.Errorf("任务数超过上限：最多 %d 条，收到 %d 条", team.Policy.MaxTasksPerRun, len(raw))
	}
	if mode == ModeSingle && len(raw) > 1 {
		return nil, errors.New("单任务模式最多一条任务；多条任务请使用流水线模式")
	}
	if mode == ModeBroadcast && len(raw) == 0 {
		return nil, errors.New("并行分派模式需要显式计划：至少提供一条任务")
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
		if mode == ModeBroadcast {
			if input.MemberID != "" {
				return nil, fmt.Errorf("任务 %s 请通过 memberIds 指定成员（并行分派不使用 memberId）", input.ID)
			}
			if len(input.MemberIDs) < 2 {
				return nil, fmt.Errorf("任务 %s 的 memberIds 至少需要 2 个互异成员", input.ID)
			}
			seenMembers := map[string]bool{}
			for _, memberID := range input.MemberIDs {
				member, exists := members[memberID]
				if !exists {
					return nil, fmt.Errorf("任务 %s 引用了不存在的成员：%s", input.ID, memberID)
				}
				if !member.Enabled {
					return nil, fmt.Errorf("任务 %s 引用了已停用的成员：%s", input.ID, memberID)
				}
				if seenMembers[memberID] {
					return nil, fmt.Errorf("任务 %s 的 memberIds 存在重复成员：%s", input.ID, memberID)
				}
				seenMembers[memberID] = true
				if !ValidMemberID(input.ID + "-" + memberID) {
					return nil, fmt.Errorf("任务 %s 与成员 %s 组合出的任务 ID 超长（字母/数字/下划线/连字符，1-64 字符）", input.ID, memberID)
				}
			}
		} else {
			if len(input.MemberIDs) > 0 {
				return nil, fmt.Errorf("任务 %s 的 memberIds 仅并行分派模式支持", input.ID)
			}
			member, exists := members[input.MemberID]
			if !exists {
				return nil, fmt.Errorf("任务 %s 引用了不存在的成员：%s", input.ID, input.MemberID)
			}
			if !member.Enabled {
				return nil, fmt.Errorf("任务 %s 引用了已停用的成员：%s", input.ID, input.MemberID)
			}
		}
		if strings.TrimSpace(input.Prompt) == "" {
			return nil, fmt.Errorf("任务 %s 的内容不能为空", input.ID)
		}
		// 创建期上限双口径（评审遗留②）：账本按字节校验（len(text)），旧创建
		// 校验只按 rune 预留余量——多字节内容建成了注定在派发期才失败的 Run。
		// 现按字节预检（PromptByteLimit 同源折算），rune 上限保留、取小者。
		if runes := len([]rune(input.Prompt)); runes > limit {
			return nil, fmt.Errorf("任务 %s 的内容过长：%d 字，加上成员上下文与信封后不能超过 %d 字，当前上限 %d 字",
				input.ID, runes, promptHardLimit, limit)
		}
		if size := len(input.Prompt); size > byteLimit {
			return nil, fmt.Errorf("任务 %s 的内容过长：%d 字节，加上成员上下文与信封后不能超过 %d 字节，当前上限 %d 字节（约合 %d 字）",
				input.ID, size, promptHardLimit, byteLimit, byteLimit/3)
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
			if input.WorkspaceID != "" {
				return nil, fmt.Errorf("任务 %s 未使用独立工作区，不能指定 workspaceId", input.ID)
			}
		case WorkspaceIsolatedWorktree:
			// §6.4 + §16 默认决策 6：只引用用户预备的 worktree——注册键必填
			// 且过成员 ID 正则（注册表键空间），创建时即拒绝，绝不静默落到
			// 派发期才发现。
			if !ValidMemberID(input.WorkspaceID) {
				return nil, fmt.Errorf("任务 %s 使用独立工作区时必须提供 workspaceId（字母/数字/下划线/连字符，1-64 字符的工作区注册键）", input.ID)
			}
		default:
			return nil, fmt.Errorf("任务 %s 的工作区模式无效：%s", input.ID, input.WorkspaceMode)
		}
		normalized = append(normalized, input)
	}

	// broadcast 展开（§5.3）：每条 memberIds 条目 → 每成员一个任务，ID=
	// {entryID}-{memberID}，BroadcastGroup=entryID；广播组不靠 ID 反解
	// （成员 ID 可含连字符），显式携带映射。
	expanded := make([]PlanTaskInput, 0, len(normalized))
	broadcastGroup := map[string]string{} // 展开后任务 ID → 条目 ID（BroadcastGroup）
	expansionOf := map[string][]string{}  // 条目 ID → 展开后任务 ID 列表
	for _, input := range normalized {
		if mode != ModeBroadcast {
			expanded = append(expanded, input)
			continue
		}
		for _, memberID := range input.MemberIDs {
			taskID := input.ID + "-" + memberID
			expanded = append(expanded, PlanTaskInput{
				ID:            taskID,
				MemberID:      memberID,
				Prompt:        input.Prompt,
				DependsOn:     input.DependsOn,
				WriteLock:     input.WriteLock,
				WorkspaceMode: input.WorkspaceMode,
				WorkspaceID:   input.WorkspaceID,
			})
			broadcastGroup[taskID] = input.ID
			expansionOf[input.ID] = append(expansionOf[input.ID], taskID)
		}
	}

	// 依赖引用与循环检测（§5.3：循环在创建时即拒绝）。依赖按条目 ID 引用，
	// broadcast 条目的依赖展开为其全部成员任务；引用存在的条目才可展开。
	dependencies := map[string][]string{}
	for _, input := range expanded {
		deps := make([]string, 0, len(input.DependsOn))
		for _, dep := range input.DependsOn {
			targets := []string{dep}
			if mode == ModeBroadcast {
				expandedTargets, ok := expansionOf[dep]
				if !ok {
					return nil, fmt.Errorf("任务 %s 依赖了不存在的任务：%s", input.ID, dep)
				}
				targets = expandedTargets
			} else if !seen[dep] {
				return nil, fmt.Errorf("任务 %s 依赖了不存在的任务：%s", input.ID, dep)
			}
			for _, target := range targets {
				if target == input.ID {
					return nil, fmt.Errorf("计划存在循环依赖：%s → %s", input.ID, input.ID)
				}
				duplicate := false
				for _, existing := range deps {
					if existing == target {
						duplicate = true
						break
					}
				}
				if !duplicate {
					deps = append(deps, target)
				}
			}
		}
		dependencies[input.ID] = deps
	}
	if cycle := findDependencyCycle(expanded, dependencies); cycle != "" {
		return nil, errors.New("计划存在循环依赖：" + cycle)
	}

	// single 空计划：合成单条 Lead 任务（prompt=goal）。
	if mode == ModeSingle && len(expanded) == 0 {
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
		if size := len(goal); size > byteLimit {
			return nil, fmt.Errorf("运行目标过长：%d 字节，当前上限 %d 字节（约合 %d 字）", size, byteLimit, byteLimit/3)
		}
		expanded = append(expanded, PlanTaskInput{
			ID:            "lead",
			MemberID:      team.LeadID,
			Prompt:        goal,
			WriteLock:     TaskWriteLockExclusive,
			WorkspaceMode: WorkspaceShared,
		})
	}
	if mode == ModePipeline && len(expanded) == 0 {
		return nil, errors.New("流水线模式需要显式计划：至少提供一条任务")
	}
	if len(expanded) > team.Policy.MaxTasksPerRun {
		return nil, fmt.Errorf("任务数超过上限：最多 %d 条，展开后 %d 条", team.Policy.MaxTasksPerRun, len(expanded))
	}

	// 拓扑排序（稳定：同层按输入顺序），赋 Ordinal 与创建期确定性字段。
	order := topologicalOrder(expanded, dependencies)
	policyDigest := PolicyDigest(team.Policy)
	tasks := make([]Task, 0, len(order))
	for ordinal, input := range order {
		// 评审成员缺省只读（review.go）；其余缺省独占。
		writeLock := defaultReviewWriteLock(team, input.MemberID, input.WriteLock)
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
			BroadcastGroup:   broadcastGroup[input.ID],
			WorkspaceMode:    input.WorkspaceMode,
			WorkspaceID:      input.WorkspaceID,
			WriteLock:        writeLock,
			Status:           TaskPlanned,
			InstructionsHead: InstructionsHead(input.Prompt),
		}
		task.InputHash = InputHashFor(&task, policyDigest, "")
		tasks = append(tasks, task)
	}
	return &Plan{
		Mode:          mode,
		CollectPolicy: collect,
		Goal:          goal,
		Tasks:         tasks,
		PlanHash:      PlanHash(mode, collect, goal, tasks),
	}, nil
}

// PromptLimit 返回创建期 prompt 的有效字数上限（rune）：
// 100000 − maxContextChars − 200。默认策略（12000）下为 87800 字。
func PromptLimit(maxContextChars int) int {
	return promptHardLimit - maxContextChars - envelopeReserve
}

// PromptByteLimit 返回创建期 prompt 的有效字节上限（评审遗留②：与派发期
// 的账本字节校验同源）。MaxContextChars 是 rune 预算，多字节字符最多 3
// 字节，保守折算 ×3 并预留信封字节（512），钳到 ≥1000 防极端策略把上限压
// 成负数。默认策略（12000）下为 100000−36000−512=63488 字节。多字节
// prompt 创建即拒，不再派发期才失败；纯 ASCII 内容受此口径约束（取 rune
// 上限与字节上限的较小者）。
func PromptByteLimit(maxContextChars int) int {
	limit := promptHardLimit - maxContextChars*3 - byteEnvelopeReserve
	if limit < promptByteFloor {
		return promptByteFloor
	}
	return limit
}

func policyMaxContextChars(team *Team) int { return team.Policy.MaxContextChars }

// PolicyDigest 是策略的规范化摘要（InputHash 的组成部分，§8.2）：对
// jsonObject 的有序编码取 sha256，字段顺序固定所以同一策略摘要稳定。
func PolicyDigest(policy TeamPolicy) string {
	sum := sha256.Sum256(jsoncompat.EncodeCompact(policy.jsonObject()))
	return hex.EncodeToString(sum[:])
}

// PlanHash 计算规范化计划的摘要：模式 + 收集策略 + 目标 + 每任务的确定性
// 字段（id/memberId/prompt/dependsOn/workspaceMode/writeLock/attachments），拓扑序排列。
// 与任务运行态字段（status/result/submissionId 等）无关，同一计划重算稳定。
func PlanHash(mode, collect, goal string, tasks []Task) string {
	object := jsoncompat.NewObject()
	object.Set("mode", mode)
	object.Set("collect", collect)
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
		if len(task.Attachments) > 0 {
			attachments := &jsoncompat.Array{}
			for _, id := range task.Attachments {
				attachments.Items = append(attachments.Items, id)
			}
			entry.Set("attachments", attachments)
		}
		list.Items = append(list.Items, entry)
	}
	object.Set("tasks", list)
	sum := sha256.Sum256(jsoncompat.EncodeCompact(object))
	return hex.EncodeToString(sum[:])
}

// InputHashFor 计算任务输入摘要（§8.2）：规范化的成员、Prompt、依赖摘要、
// 工作区与写锁、附件、策略摘要与上下文文本。planner 版本只覆盖创建期确定性字段
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
	if len(task.Attachments) > 0 {
		attachments := &jsoncompat.Array{}
		for _, id := range task.Attachments {
			attachments.Items = append(attachments.Items, id)
		}
		object.Set("attachments", attachments)
	}
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
