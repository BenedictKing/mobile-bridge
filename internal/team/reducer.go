package team

// reducer.go 是结果归约与 resume 的纯决策（§6.5 salvage、§4.3 终局分类、
// §10.2 resume）：ExtractResult 从已完成 turn 抽取有界结果；FoldRunStatus 是
// Run 终局分类的唯一权威；PlanResume 是外因暂停后的恢复决策。全部不读写
// 文件、不碰会话；状态迁移以 Transition 指令返回，由 team-dispatch 落
// journal 与账本（§3.1 原则 3）。

import (
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Transition 的 Kind 取值：任务状态迁移、Run 状态迁移、可派发指令。
const (
	TransitionTaskStatus = "task_status" // TaskID 的任务状态迁移到 Status
	TransitionRunStatus  = "run_status"  // Run 状态迁移到 Status（含 fold 结果）
	TransitionDispatch   = "dispatch"    // TaskID 可继续派发（§10.2 resume）
)

// Transition 是纯决策（PlanResume/ReconcileRun）产出的单条状态迁移指令。
// 调用方负责：按序应用、先写 journal 再改账本快照；EventType 非空时随迁移
// 追加对应类型的 journal 事件（空串表示纯重建——journal 里已有该事实，如
// 重放恢复的终态，不要重复记账）。
type Transition struct {
	Kind       string               // TransitionTaskStatus / TransitionRunStatus / TransitionDispatch
	TaskID     string               // 任务级转移的目标任务；Run 级与派发指令以外的转移为空
	Status     string               // 目标状态（任务级与 Run 级通用）
	StopReason string               // 终止原因（可选）
	Error      string               // 失败详情（可选）
	Outcome    string               // 失败分类（可选，§6.3 错误策略表；随任务迁移写快照）
	EventType  string               // 需随迁移落 journal 的事件类型（可选，见上）
	Result     *TaskResult          // 重建 completed 终态时携带的有界结果（可选）
	Retry      *taskRetryAssignment // 重臂/改派事实（可选，journal 重放专用）
}

// AssistantFullText 返回 turn.messages 中 assistant 文本块的按序拼接全文
// （排除 kind=="error" 与工具/过程类条目），未经任何截断。抽自 ExtractResult
// 的拼接段：team-actions 信封解析（§5.1，actions.go）必须用完整全文——
// ExtractResult 的结果按 MaxResultChars 截断（默认 12000 rune），尾部动作
// 标记会被截掉（Truncated/ResultChars 只是提示不是恢复手段）。
func AssistantFullText(turn *jsoncompat.Object) string {
	if turn == nil {
		return ""
	}
	var texts []string
	if list, ok := turn.MustGet("messages").(*jsoncompat.Array); ok {
		for _, item := range list.Items {
			message, ok := item.(*jsoncompat.Object)
			if !ok {
				continue
			}
			if kind, _ := message.MustGet("kind").(string); kind == "error" {
				continue
			}
			if role, _ := message.MustGet("role").(string); role != "assistant" {
				continue
			}
			text, _ := message.MustGet("text").(string)
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

// ExtractResult 从 turn 抽取有界结果（§6.5 只存摘要与引用）。调用契约：只对
// turn.status=="completed" 的 turn 调用——失败信号不在 turn 顶层 error 字段
// （该字段初始化后恒为 nil），失败文本在 messages 里 kind=="error" 条目的
// text（internal/acpview/view.go FailTurn），本函数一律排除。
//
// 结果 = AssistantFullText（assistant 文本按序拼接）按 maxChars 截断并标记
// truncated；maxChars≤0 一律回退默认上限（DefaultPolicy().MaxResultChars）
// ——策略层已拒绝 0（httpd 校验 min=1），此处兜底历史账本里的非法 0 值，
// 保证结果 payload 恒有界（§10.3/§15.3）。ResultChars 记录截断前的完整字数
// （rune），供前端显示"结果共 N 字"。
//
// 此外从 role=="activity"、kind=="toolCall" 的条目里挖掘两份有界摘要
// （§6.5 文件变更摘要与命令执行摘要）：编辑类工具（按名匹配 edit/write/
// apply 等）抽目标路径，命令类（bash/shell）抽命令首行；上限与截断标记见
// mineActivitySummaries。
func ExtractResult(sessionID string, turn *jsoncompat.Object, maxChars int) TaskResult {
	if maxChars <= 0 {
		maxChars = DefaultPolicy().MaxResultChars
	}
	result := TaskResult{SessionID: sessionID}
	if turn == nil {
		return result
	}
	if id, ok := turn.MustGet("id").(string); ok {
		result.TurnID = id
	}
	if stopReason, ok := turn.MustGet("stopReason").(string); ok {
		result.StopReason = stopReason
	}
	full := AssistantFullText(turn)
	result.ResultChars = len([]rune(full))
	if maxChars > 0 && result.ResultChars > maxChars {
		result.Result = truncateRunes(full, maxChars)
		result.Truncated = true
	} else {
		result.Result = full
	}
	result.FileSummary, result.CommandSummary = mineActivitySummaries(turn)
	return result
}

// §6.5 有界摘要的硬上限（每类）：条数 ≤20、每条 ≤120 rune、总量 ≤1200
// rune（截断标记附加在内容之后，含标记的完整字段仍 ≤1200 rune）。
const (
	activityMaxItems   = 20
	activityItemRunes  = 120
	activityTotalRunes = 1200
	activityTruncMark  = "（超出上限，已截断）"
)

// editToolProbes / commandToolProbes 是工具名（title，已折小写）的子串探针。
// 工具名跨 Agent 形态各异（Claude 的 Edit/Write/Bash、Gemini 的 edit_file/
// run_shell_command、codex 的 apply_patch、部分 Agent 的中文标题），按子串
// 覆盖常见形态；TodoWrite 等清单工具按 todo 排除（不是文件编辑）。
var (
	editToolProbes    = []string{"edit", "write", "apply", "str_replace", "patch", "编辑", "修改", "写入"}
	commandToolProbes = []string{"bash", "shell", "command", "terminal", "执行命令"}
)

// mineActivitySummaries 从 turn.messages 挖掘文件变更与命令执行摘要（§6.5）：
// 只看 role=="activity" 且 kind=="toolCall" 的条目（agentThought/plan/
// subagent/error 等过程条目一律跳过）。消息形态对齐 internal/acpview
// （view.go tool_call）：title 携带工具名，text 携带 QuestionSummary/
// CommandSummary 的产物，changes 数组携带 {path,oldText,newText} 的结构化
// 文件变更（changes.go appendContentChanges）。返回两份单行摘要（空串表示
// 该类无内容），上限由 joinBoundedItems 保证。
func mineActivitySummaries(turn *jsoncompat.Object) (fileSummary, commandSummary string) {
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages == nil {
		return "", ""
	}
	var paths, commands []string
	seenPath, seenCommand := map[string]bool{}, map[string]bool{}
	addPath := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || seenPath[path] {
			return
		}
		seenPath[path] = true
		paths = append(paths, path)
	}
	addCommand := func(command string) {
		command = strings.TrimSpace(command)
		if command == "" || seenCommand[command] {
			return
		}
		seenCommand[command] = true
		commands = append(commands, command)
	}
	for _, entry := range messages.Items {
		message, ok := entry.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if role, _ := message.MustGet("role").(string); role != "activity" {
			continue
		}
		if kind, _ := message.MustGet("kind").(string); kind != "toolCall" {
			continue
		}
		title, _ := message.MustGet("title").(string)
		folded := strings.ToLower(strings.TrimSpace(title))
		text, _ := message.MustGet("text").(string)
		firstLine := text
		if index := strings.IndexByte(text, '\n'); index >= 0 {
			firstLine = text[:index]
		}
		firstLine = strings.TrimSpace(firstLine)
		switch {
		case containsAny(folded, commandToolProbes):
			addCommand(firstLine)
		case containsAny(folded, editToolProbes) && !strings.Contains(folded, "todo"):
			// 结构化变更优先（acpview 已按 path 去重合并）；无 changes 时退回
			// text——edit 类 rawInput 的 CommandSummary 就是目标路径
			// （commandPreferred 含 file_path）。JSON 兜底串与 diff 首行
			// （{、-、* 开头或不含路径分隔符）不是路径，跳过。
			if changes, ok := message.MustGet("changes").(*jsoncompat.Array); ok && len(changes.Items) > 0 {
				for _, item := range changes.Items {
					change, ok := item.(*jsoncompat.Object)
					if !ok {
						continue
					}
					if path, _ := change.MustGet("path").(string); path != "" {
						addPath(path)
					}
				}
				continue
			}
			if looksLikePath(firstLine) {
				addPath(firstLine)
			}
		case folded == "工具调用" || folded == "toolcall":
			// 通用标题的 Agent 把工具名拼在 text 前缀里（migrate.go
			// isMigrationNoise 的同款形态）：bash: 前缀按命令类处理。
			if foldedLine := strings.ToLower(firstLine); strings.HasPrefix(foldedLine, "bash:") {
				addCommand(strings.TrimSpace(firstLine[len("bash:"):]))
			}
		}
	}
	return joinBoundedItems(paths, "、"), joinBoundedItems(commands, "；")
}

// containsAny 报告 text 是否包含 probes 里的任一子串。
func containsAny(text string, probes []string) bool {
	for _, probe := range probes {
		if strings.Contains(text, probe) {
			return true
		}
	}
	return false
}

// looksLikePath 是 text 兜底当作文件路径的启发式：单行、非 JSON 兜底串
// （{ 开头）、非 diff 首行（- 或 * 开头）、且含路径分隔符或扩展名点。
func looksLikePath(text string) bool {
	if text == "" || strings.HasPrefix(text, "{") || strings.HasPrefix(text, "-") || strings.HasPrefix(text, "*") {
		return false
	}
	return strings.ContainsAny(text, "/\\.") && !strings.Contains(text, "\n")
}

// joinBoundedItems 把 items 拼成单行有界摘要：每条超 activityItemRunes 截断、
// 条数超 activityMaxItems 丢弃余下、总量超 activityTotalRunes 停收；任一
// 上限触发时在行尾附截断标记，最终整行（含标记）不超过 activityTotalRunes。
func joinBoundedItems(items []string, sep string) string {
	if len(items) == 0 {
		return ""
	}
	needMark := len(items) > activityMaxItems
	contentRunes := 0
	capped := make([]string, 0, len(items))
	for i, item := range items {
		if i == activityMaxItems {
			break
		}
		if runes := len([]rune(item)); runes > activityItemRunes {
			item = truncateRunes(item, activityItemRunes)
			needMark = true
		}
		capped = append(capped, item)
		if contentRunes > 0 {
			contentRunes += len([]rune(sep))
		}
		contentRunes += len([]rune(item))
	}
	if contentRunes > activityTotalRunes {
		needMark = true
	}
	budget := activityTotalRunes
	if needMark {
		budget = activityTotalRunes - len([]rune(activityTruncMark))
	}
	kept := make([]string, 0, len(capped))
	used := 0
	for i, item := range capped {
		extra := len([]rune(item))
		if i > 0 {
			extra += len([]rune(sep))
		}
		if used+extra > budget {
			needMark = true
			break
		}
		kept = append(kept, item)
		used += extra
	}
	if len(kept) == 0 {
		return ""
	}
	joined := strings.Join(kept, sep)
	if needMark {
		joined += activityTruncMark
	}
	return joined
}

func truncateRunes(text string, limit int) string {
	count := 0
	for index := range text {
		if count == limit {
			return text[:index]
		}
		count++
	}
	return text
}

// FoldRunStatus 是 Run 终局分类的唯一权威（§4.3、§3.1 原则 5），仅在全部
// 任务终态时被调用。collect=all（含空值）维持阶段一优先级：全 completed→
// completed；任一 failed→failed；任一 cancelled→cancelled；任一
// outcome_unknown→outcome_unknown；否则任一 stopped→stopped。
//
// collect=first_success（§6.5 阶段二）：StopReason=superseded_by_first_success
// 的 cancelled 任务不参与分类（落败分支，结果让位首个成功者）；每个
// BroadcastGroup ≥1 completed 即视为满足，全组满足且无组外非完成终态→
// completed；组内全部终态且 0 completed 按成员状态归入 failed/cancelled/
// unknown/stopped（组内全 failed→failed）。已满足组内的失败成员不再定罪
// （首个成功即收集目标）。mode 供后续阶段扩展，当前不参与分类。
//
// 存在非终态任务时返回空状态（不可 fold）。
//
// salvage（§1.1 目标 9）：Run 终态绝不删除已完成结果——Run 详情始终携带
// 全部已完成任务的 TaskResult，本函数只做状态分类。outcome_unknown 分支是
// 防御性的：正常 runner 路径下未决 unknown 永远先把 run 停在 waiting_input
// （见 team-dispatch），不会带未决 unknown 来 fold；该分支仅服务恢复期对账
// 发现的异常终局。
func FoldRunStatus(tasks []Task, mode, collect string) (status string, stopReason string) {
	ordered := sortTasksByOrdinal(tasks)
	if collect == RunCollectFirstSuccess {
		return foldFirstSuccess(ordered)
	}
	return foldAll(ordered)
}

// foldAll 是 collect=all 的终局分类（阶段一优先级原样保留）。
func foldAll(ordered []Task) (status string, stopReason string) {
	allCompleted := true
	var firstFailed, firstCancelled, firstUnknown, firstStopped *Task
	for i := range ordered {
		task := &ordered[i]
		switch task.Status {
		case TaskCompleted:
		case TaskFailed:
			allCompleted = false
			if firstFailed == nil {
				firstFailed = task
			}
		case TaskCancelled:
			allCompleted = false
			if firstCancelled == nil {
				firstCancelled = task
			}
		case TaskOutcomeUnknown:
			allCompleted = false
			if firstUnknown == nil {
				firstUnknown = task
			}
		case TaskStopped:
			allCompleted = false
			if firstStopped == nil {
				firstStopped = task
			}
		default:
			// planned/ready/dispatching/running：尚未终局，不可 fold。
			return "", ""
		}
	}
	if allCompleted {
		return RunCompleted, ""
	}
	switch {
	case firstFailed != nil:
		return RunFailed, terminalReason(firstFailed, firstFailed.Error, "存在失败任务")
	case firstCancelled != nil:
		return RunCancelled, terminalReason(firstCancelled, "", "任务已取消")
	case firstUnknown != nil:
		return RunOutcomeUnknown, terminalReason(firstUnknown, "", "任务结果未知")
	default:
		return RunStopped, terminalReason(firstStopped, "", "任务已停止")
	}
}

// offenderClass 是 first_success 分类里的非完成终态优先级（数值越小越优先，
// 与阶段一 failed > cancelled > outcome_unknown > stopped 一致）。
const (
	offenderFailed    = 1
	offenderCancelled = 2
	offenderUnknown   = 3
	offenderStopped   = 4
)

func offenderClassOf(status string) int {
	switch status {
	case TaskFailed:
		return offenderFailed
	case TaskCancelled:
		return offenderCancelled
	case TaskOutcomeUnknown:
		return offenderUnknown
	case TaskStopped:
		return offenderStopped
	}
	return 0
}

// foldFirstSuccess 是 collect=first_success 的终局分类。
func foldFirstSuccess(ordered []Task) (status string, stopReason string) {
	groupSeen := map[string]bool{}      // 全部广播组（含 0 completed 的）
	groupCompleted := map[string]bool{} // 含 completed 成员的组
	groupOffenderClass := map[string]int{}
	groupOffenderTask := map[string]*Task{}
	outClass := 0
	var outTask *Task
	for i := range ordered {
		task := &ordered[i]
		if !terminalTask(task.Status) {
			return "", "" // 尚未终局，不可 fold。
		}
		if task.Status == TaskCancelled && task.StopReason == StopReasonSupersededByFirstSuccess {
			// 落败分支不参与分类；但其所在组仍要登记（全 superseded 的组
			// 正常不可能——superseded 依赖同组 completed——防御按未满足处理）。
			if task.BroadcastGroup != "" {
				groupSeen[task.BroadcastGroup] = true
			}
			continue
		}
		class := offenderClassOf(task.Status)
		if task.BroadcastGroup != "" {
			groupSeen[task.BroadcastGroup] = true
			if class == 0 {
				groupCompleted[task.BroadcastGroup] = true
				continue
			}
			if groupOffenderClass[task.BroadcastGroup] == 0 || class < groupOffenderClass[task.BroadcastGroup] {
				groupOffenderClass[task.BroadcastGroup] = class
				groupOffenderTask[task.BroadcastGroup] = task
			}
			continue
		}
		if class != 0 && (outClass == 0 || class < outClass) {
			outClass = class
			outTask = task
		}
	}
	// 未满足的组（0 completed）贡献组内最重非完成终态；已满足组的失败成员
	// 不定罪。防御：组既无 completed 也无 offender 按 stopped 归类。
	worstClass := outClass
	worstTask := outTask
	for group := range groupSeen {
		if groupCompleted[group] {
			continue
		}
		class := groupOffenderClass[group]
		task := groupOffenderTask[group]
		if class == 0 {
			class = offenderStopped
		}
		if worstClass == 0 || class < worstClass {
			worstClass = class
			worstTask = task
		}
	}
	if worstClass == 0 {
		return RunCompleted, ""
	}
	if worstTask == nil {
		// 只有未满足空组（无成员可引用）的防御路径：按类别给固定原因。
		switch worstClass {
		case offenderFailed:
			return RunFailed, "存在失败任务"
		case offenderCancelled:
			return RunCancelled, "任务已取消"
		case offenderUnknown:
			return RunOutcomeUnknown, "任务结果未知"
		default:
			return RunStopped, "任务已停止"
		}
	}
	switch worstClass {
	case offenderFailed:
		return RunFailed, terminalReason(worstTask, worstTask.Error, "存在失败任务")
	case offenderCancelled:
		return RunCancelled, terminalReason(worstTask, "", "任务已取消")
	case offenderUnknown:
		return RunOutcomeUnknown, terminalReason(worstTask, "", "任务结果未知")
	default:
		return RunStopped, terminalReason(worstTask, "", "任务已停止")
	}
}

// PlanCollect 是收集策略的纯决策（§6.5 first_success）：已出现 completed
// 成员的广播组，其余非终态成员取消（StopReason=superseded_by_first_success，
// EventType=task_stopped）。由 runner 每轮调度重算——first-wins 幂等
// （applyTaskTerminal/applyTransitions 对已终态 no-op），重启恢复后同样
// 由循环重放补齐。all 策略与无组的 run 返回 nil。
func PlanCollect(run *Run) []Transition {
	if run == nil || run.CollectPolicy != RunCollectFirstSuccess {
		return nil
	}
	satisfied := map[string]bool{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.BroadcastGroup != "" && task.Status == TaskCompleted {
			satisfied[task.BroadcastGroup] = true
		}
	}
	if len(satisfied) == 0 {
		return nil
	}
	var transitions []Transition
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if task.BroadcastGroup == "" || !satisfied[task.BroadcastGroup] {
			continue
		}
		if terminalTask(task.Status) {
			continue
		}
		transitions = append(transitions, Transition{
			Kind:       TransitionTaskStatus,
			TaskID:     task.ID,
			Status:     TaskCancelled,
			StopReason: StopReasonSupersededByFirstSuccess,
			EventType:  EventTaskStopped,
		})
	}
	return transitions
}

func terminalReason(task *Task, preferred, fallback string) string {
	if preferred != "" {
		return preferred
	}
	if task.StopReason != "" {
		return task.StopReason
	}
	if task.Error != "" {
		return task.Error
	}
	return fallback
}

// PlanResume 是 resume 的纯决策（§10.2：resume 只用于外因暂停；不得把确定
// 性 failed 或 outcome_unknown 直接改成 running）。仅 run.Status==waiting_input
// 返回 ok=true，产出转移：
//
//   - 所有未决 outcome_unknown 任务 → stopped（stopReason 固定，绝不重发）；
//   - quota/auth/not_configured 停靠任务（§6.3 错误策略表，Outcome 分类）→
//     可重派：statuses 视图先改回非终态（下游依赖不被级联终止——与
//     depGroupVerdict 的调用点约定一致），并产出 dispatch 指令；执行侧按
//     task.Attempt+1 派全新 submissionID（幂等键 attempt 递增），与停靠前
//     的旧 attempt 各自独立可审计；
//   - 依赖按 depGroupVerdict 判定（与调度/级联同规则，含 §6.5 组语义）被
//     永久阻塞的 planned/ready 任务 → stopped（分支已死，不再执行；否则
//     run 永远等不到全终态）；
//   - 可继续派发的任务集：status ∈ {planned, ready} 且依赖（按迁移后状态）
//     全部满足的任务，逐条给出 dispatch 指令——依赖仍在飞（非终态）的
//     任务保持原状等待释放；
//   - 若无可派发且全终态：给出 fold 指令（TransitionRunStatus 携带按迁移后
//     任务列表计算的 FoldRunStatus 结果——因存在 stopped，通常归 run stopped）。
//
// 无可派发但仍有运行中任务时不给 fold：runner 等其落定后另行 fold。
// 停靠重派决策可重入：崩溃于 resume 记账与重派之间时，journal 的
// task_stopped 终态仍在，重启对账把任务放回 stopped，用户再次 resume 重新
// 推导（不靠快照残留字段）。
func PlanResume(run *Run) ([]Transition, bool) {
	if run == nil || run.Status != RunWaitingInput {
		return nil, false
	}
	statuses := map[string]string{}
	stopReasons := map[string]string{}
	for i := range run.Tasks {
		statuses[run.Tasks[i].ID] = run.Tasks[i].Status
	}
	groups := broadcastGroupsOf(run.Tasks)
	var transitions []Transition
	stopTask := func(taskID, reason string) Transition {
		statuses[taskID] = TaskStopped
		stopReasons[taskID] = reason
		return Transition{
			Kind:       TransitionTaskStatus,
			TaskID:     taskID,
			Status:     TaskStopped,
			StopReason: reason,
			EventType:  EventTaskStopped,
		}
	}
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if task.Status != TaskOutcomeUnknown {
			continue
		}
		transitions = append(transitions, stopTask(task.ID, "结果未知，用户已确认不再重发"))
	}
	// quota/auth/not_configured 停靠任务重臂（§6.3 错误策略表）：statuses
	// 视图改回非终态（下游依赖判定不被级联终止——与 depGroupVerdict 的
	// 调用点约定一致），并落一条 stopped→ready 的任务迁移（不记事件：重派
	// 事实由后续 task_dispatching(attempt+1) 记账；Outcome 保留，执行侧按
	// task.Attempt+1 派全新 submissionID）。依赖仍在飞的停靠任务同样重臂为
	// ready——只改视图会让它们在依赖释放后永远停在 stopped（run 已回到
	// dispatching，再次 resume 会被 ErrRunNotWaiting 拒绝）。
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if task.Status == TaskStopped && stopOutcomeFreshSubmission(task.Outcome) {
			statuses[task.ID] = TaskReady
			transitions = append(transitions, Transition{
				Kind:   TransitionTaskStatus,
				TaskID: task.ID,
				Status: TaskReady,
			})
		}
	}
	// 单遍按拓扑序分类候选任务：可派发（依赖全满足）、被永久阻塞依赖
	// （立即标 stopped，级联传导给下游）、依赖仍在飞的保持原状。立即改写
	// statuses 让 a(stopped)←b←c 这类传递阻塞链一遍收敛。
	dispatchable := []string{}
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		switch statuses[task.ID] {
		case TaskPlanned, TaskReady:
		case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
			continue
		default:
			continue // dispatching/running：还在飞，resume 不碰。
		}
		released, blocked := depGroupVerdict(statuses, groups, task.DependsOn, run.CollectPolicy)
		switch {
		case blocked:
			transitions = append(transitions, stopTask(task.ID, "依赖分支已终止，任务不再执行"))
		case released:
			dispatchable = append(dispatchable, task.ID)
		}
	}
	for _, taskID := range dispatchable {
		transitions = append(transitions, Transition{
			Kind:   TransitionDispatch,
			TaskID: taskID,
			Status: TaskReady,
		})
	}
	if len(dispatchable) == 0 {
		allSettled := true
		for _, status := range statuses {
			switch status {
			case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
			default:
				allSettled = false
			}
		}
		if allSettled {
			after := append([]Task(nil), run.Tasks...)
			for i := range after {
				if status, ok := statuses[after[i].ID]; ok {
					after[i].Status = status
					if reason, hasReason := stopReasons[after[i].ID]; hasReason {
						after[i].StopReason = reason
					}
				}
			}
			if status, stopReason := FoldRunStatus(after, run.Mode, run.CollectPolicy); status != "" {
				transitions = append(transitions, Transition{
					Kind:       TransitionRunStatus,
					Status:     status,
					StopReason: stopReason,
				})
			}
		}
	}
	return transitions, true
}
