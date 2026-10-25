package team

// context.go 是成员间上下文注入的有界构造（§7）：阶段一结果源只有 Run 内
// 已入账的依赖任务 Task.ResultDetail（本身就是有界摘要，§6.5），不读成员
// transcript（§2.2）；每轮只注入上一轮摘要（§7.3）。纯函数——策略、依赖
// 结果与截断全部就地决定，真实 I/O 由 dispatcher 执行。

import (
	"sort"
	"strings"
)

// §7.1 信封的固定文案与标记。
const (
	teamContextHeader     = "[Team context]"
	assignedTaskHeader    = "[Assigned task]"
	contextTruncationNote = "内容可能已截断，完整内容请打开原会话查看。"
)

// ContextSource 记录一段注入上下文的来源（§7.2「记录实际注入长度和来源任务」）。
type ContextSource struct {
	TaskID    string
	MemberID  string
	Chars     int  // 该来源实际注入的 rune 数（含来源头）
	Truncated bool // 该来源内容因上限被截断
}

// ContextResult 是 BuildTaskContext 的输出。
type ContextResult struct {
	// Text 是完整的 [Team context] 段；无依赖结果时为空串（此时最终消息
	// 只有任务本身，不带信封）。
	Text string
	// InjectedChars 是 Text 的 rune 数（实际注入长度，随 task_dispatching
	// 事件落 journal）。
	InjectedChars int
	// Truncated 表示任一来源被截断或有来源因上限被整块丢弃。
	Truncated bool
	// Sources 按注入顺序（依赖拓扑序）列出各来源。
	Sources []ContextSource
}

// contextBlock 是单个依赖来源的内部表示，便于截断时按块收缩。
type contextBlock struct {
	taskID    string
	memberID  string
	content   string
	truncated bool
}

// BuildTaskContext 构造 task 的注入上下文（§7.1/7.2）：依赖任务的已完成结果
// 按拓扑序（Ordinal）拼装在同一个 [Team context] 信封下，每个来源带
// 来源成员/来源任务/截断提示三行头。截断规则：
//
//   - 上下文总量按 policy.MaxContextChars（rune）硬截断，优先保留最新
//     （Ordinal 最大）的结果，最旧的先被丢弃（§7.2）；
//   - 合成全文（信封+上下文+[Assigned task]+prompt）再按账本硬上限
//     100000 字节兜底收缩（internal/httpd/queue.go validateSubmissionInput
//     按 len(text) 字节校验；创建期 planner 只按 rune 预留余量，多字节
//     内容仍可能超限，这里保证派发文本一定能过账本校验）；
//   - 拼接前剥离上游结果里已有的 [Team context] 信封标记，防多轮接力
//     指数增长。
//
// policy.MaxContextChars ≤ 0 表示不注入任何上下文（planner 同语义）。
func BuildTaskContext(run *Run, task *Task, policy TeamPolicy) ContextResult {
	result := ContextResult{}
	if run == nil || task == nil || len(task.DependsOn) == 0 || policy.MaxContextChars <= 0 {
		return result
	}
	byID := map[string]*Task{}
	for i := range run.Tasks {
		byID[run.Tasks[i].ID] = &run.Tasks[i]
	}
	// 依赖任务按拓扑序收集，只取已完成且有结果的（§4.3：非完成依赖不释放
	// 下游，本函数不会被调度调用；防御性跳过）。
	var deps []*Task
	for _, depID := range task.DependsOn {
		if dep, ok := byID[depID]; ok {
			deps = append(deps, dep)
		}
	}
	sort.SliceStable(deps, func(a, b int) bool { return deps[a].Ordinal < deps[b].Ordinal })
	blocks := make([]contextBlock, 0, len(deps))
	for _, dep := range deps {
		content := ""
		if dep.ResultDetail != nil {
			content = dep.ResultDetail.Result
		} else {
			content = dep.Result
		}
		if dep.Status != TaskCompleted || strings.TrimSpace(content) == "" {
			continue
		}
		blocks = append(blocks, contextBlock{
			taskID:   dep.ID,
			memberID: dep.MemberID,
			// 先剥 <team-actions> 动作块（§5.1：Lead 结果里的动作信封绝不由
			// 上下文走私进下游——下游可执行的动作只来自编排器的正式派发），
			// 再剥 [Team context] 信封（§7.2 防多轮接力指数增长）。
			content: appendSourceSummaries(stripTeamEnvelope(stripActionBlocks(content)), dep.ResultDetail),
		})
	}
	if len(blocks) == 0 {
		return result
	}

	// 第一遍：按 rune 预算从最新（Ordinal 最大）往前收块。[Team context]
	// 头本身也计入预算（InjectedChars 量的是整段 Text）。
	bodyBudget := policy.MaxContextChars - (len([]rune(teamContextHeader)) + 1)
	if bodyBudget <= 0 {
		return result
	}
	var dropped bool
	blocks, dropped = fitRuneBudget(blocks, bodyBudget)
	// 第二遍：合成全文超过账本字节硬上限时继续收缩（仍优先保最新）。
	for len(blocks) > 0 && len(composeContext(blocks))+assignedFraming(task.Prompt) > promptHardLimit {
		blocks = shrinkOldest(blocks)
		dropped = true
	}

	result.Text = composeContext(blocks)
	result.InjectedChars = len([]rune(result.Text))
	for _, block := range blocks {
		result.Sources = append(result.Sources, ContextSource{
			TaskID:    block.taskID,
			MemberID:  block.memberID,
			Chars:     block.chars(),
			Truncated: block.truncated,
		})
		if block.truncated {
			result.Truncated = true
		}
	}
	if dropped {
		result.Truncated = true
	}
	return result
}

// ComposeTaskMessage 拼装最终发送文本：有上下文时为
// [Team context] 段 + [Assigned task] + prompt（§7.1 版式），无上下文时只
// 发任务本身（与普通排队消息一致）。
func ComposeTaskMessage(contextText, prompt string) string {
	if contextText == "" {
		return prompt
	}
	return contextText + assignedTaskHeader + "\n" + prompt
}

// composeContext 把来源块渲染为 [Team context] 段文本。
func composeContext(blocks []contextBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(teamContextHeader)
	builder.WriteString("\n")
	for _, block := range blocks {
		builder.WriteString(block.header())
		builder.WriteString("\n\n")
		builder.WriteString(block.content)
		builder.WriteString("\n\n")
	}
	return builder.String()
}

// assignedFraming 是 [Assigned task] 段的固定字节开销（含前导换行），供字节
// 上限测算。
func assignedFraming(prompt string) int {
	return len(assignedTaskHeader) + 1 + len(prompt)
}

func (b *contextBlock) header() string {
	return "来源成员：" + b.memberID + "\n来源任务：" + b.taskID + "\n" + contextTruncationNote
}

// chars 是该来源块的 rune 数（含来源头与空行）。
func (b *contextBlock) chars() int {
	return len([]rune(b.header())) + 2 + len([]rune(b.content)) + 2
}

// fitRuneBudget 从最新到最旧收块：整块放得下就整块收；放不下但剩余预算
// 还装得下来源头时截内容；否则停止（更旧的来源整块丢弃）。返回保留块
// （拓扑序）与是否有来源被丢弃。
func fitRuneBudget(blocks []contextBlock, budget int) ([]contextBlock, bool) {
	kept := make([]contextBlock, 0, len(blocks))
	remaining := budget
	for i := len(blocks) - 1; i >= 0; i-- {
		block := blocks[i]
		chars := block.chars()
		if chars <= remaining {
			kept = append(kept, block)
			remaining -= chars
			continue
		}
		if headerChars := len([]rune(block.header())) + 4; remaining > headerChars {
			// 块总开销 = 来源头 + 头后空行 + 内容 + 尾随空行。
			block.content = truncateRunes(block.content, remaining-headerChars)
			block.truncated = true
			kept = append(kept, block)
		}
		// 预算耗尽：更旧的来源整块丢弃。
		break
	}
	// 反转回拓扑序。
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return kept, len(kept) < len(blocks)
}

// shrinkOldest 收缩最旧（Ordinal 最小，即切片头部）的来源：先减半内容，
// 内容减空则整块丢弃；只剩一块且已减空时返回空切片（上下文清零，调用方
// 停止收缩，由 SubmitTask 的字节预检兜底报错）。
func shrinkOldest(blocks []contextBlock) []contextBlock {
	if len(blocks) == 0 {
		return blocks
	}
	oldest := &blocks[0]
	half := len([]rune(oldest.content)) / 2
	if half > 0 {
		oldest.content = truncateRunes(oldest.content, half)
		oldest.truncated = true
		return blocks
	}
	return blocks[1:]
}

// summaryLineRunes 是来源块里摘要行（含「变更文件：/执行命令：」固定头）
// 的 rune 上限。
const summaryLineRunes = 400

// appendSourceSummaries 在来源块正文后追加 §6.5 的两份有界摘要行（§7.2
// 「优先保留……文件变更」）：依赖 ResultDetail 带 FileSummary/CommandSummary
// 时各拼一行（『变更文件：…』『执行命令：…』），各截 400 rune。行随块内容
// 一起进 MaxContextChars 预算（fitRuneBudget/shrinkOldest 原样作用）；上游
// 回显的旧行已由 stripTeamEnvelope 剥掉，这里只拼本任务的事实摘要。
func appendSourceSummaries(content string, detail *TaskResult) string {
	if detail == nil || (detail.FileSummary == "" && detail.CommandSummary == "") {
		return content
	}
	var builder strings.Builder
	builder.WriteString(content)
	if detail.FileSummary != "" {
		builder.WriteString("\n")
		builder.WriteString(truncateRunes("变更文件："+detail.FileSummary, summaryLineRunes))
	}
	if detail.CommandSummary != "" {
		builder.WriteString("\n")
		builder.WriteString(truncateRunes("执行命令："+detail.CommandSummary, summaryLineRunes))
	}
	return builder.String()
}

// stripTeamEnvelope 剥离上游结果里已有的信封标记（[Team context] /
// [Assigned task] 头、来源行、截断提示行与两份摘要行），保留正文，防多轮
// 接力时信封嵌套指数增长（§7.2「移除重复的 Team envelope」）。
func stripTeamEnvelope(text string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == teamContextHeader, trimmed == assignedTaskHeader:
			continue
		case strings.HasPrefix(trimmed, "来源成员："), strings.HasPrefix(trimmed, "来源任务："),
			strings.HasPrefix(trimmed, "来源会话："), strings.HasPrefix(trimmed, "时间："):
			continue
		case strings.HasPrefix(trimmed, "变更文件："), strings.HasPrefix(trimmed, "执行命令："):
			continue
		case trimmed == contextTruncationNote:
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
