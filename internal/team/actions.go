package team

// actions.go 是 Lead 自动动作协议的解析、校验与执行判定（§5.1/§5.2，§12
// 阶段三工作项 1/2/3/4）：<team-actions> 信封提取、严格→宽容两级 JSON 解码、
// schema 校验与中文 violation 列表、修复提示词的有界拼装，以及动作执行前的
// §5.2 拒绝清单判定与派生任务构造。全部纯函数——不读写文件、不碰会话（慢
// I/O 由 runner.go 的 passActions 锁外预取后经参数传入）；落账（action_proposed
// / invalid_action_block + 修复任务在 settleLeadTaskLocked；action_rejected /
// 派生 task_planned / task_forwarded+actionKey 在 passActions）都在 runner.go
// 的单批 commitRun 内完成（§3.1 原则 1/3）。
//
// 安全边界优先于功能广度：自然语言绝不触发动作，只有成对标记包裹且通过
// 同一 schema 校验的单个 JSON 对象才会产生 Action（§5.1 规则 1/2/6）。

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// §5.1 信封标记与动作词汇。
const (
	actionsOpenTag  = "<team-actions>"
	actionsCloseTag = "</team-actions>"

	// ActionTypeDelegate 给一个成员创建任务（§5.2，执行需 autoDispatch）。
	ActionTypeDelegate = "delegate"
	// ActionTypeRequestContext 请求另一成员提供已有结果（fromTaskId）。
	ActionTypeRequestContext = "request_context"
	// ActionTypeSummarize 请求 Lead 汇总已有结果（v1 汇总全部已完成任务，
	// 不引入 fromTaskIds 数组——简化声明）。
	ActionTypeSummarize = "summarize"
)

// 修复回路（§5.1 规则 3）与信封规模的常量。
const (
	// maxActionRepairs 是修复回路上限：最多 3 个修复回合（actfix-{root}-{n}，
	// n∈1..3）；第 4 次解析失败落 final=true 的 invalid_action_block，任务
	// 明确失败并保留原始回复，动作拒绝。
	maxActionRepairs = 3
	// actionRepairPrefix 是修复任务 ID 前缀（actfix-{root}-{n}，整体过成员
	// ID 正则——超长或非法字符时修复通道确定性收场为 final，绝不静默）。
	actionRepairPrefix = "actfix-"
	// maxActionsPerBlock 是单个信封的动作数硬上限（与 MaxTasksPerRun 取小，
	// §5.2「动作数量都必须校验」）。
	maxActionsPerBlock = 8
	// repairViolationRunes 是单条 violation 的截断长度；repairMaxViolations
	// 是列表条数上限——逐条先截再拼，绝不整包拼完才检查（账本 100000 字节
	// 上限，internal/httpd/queue.go validateSubmissionInput）。
	repairViolationRunes  = 200
	repairMaxViolations   = 20
	repairOmissionNoteFmt = "（其余 %d 条问题已省略）"
)

// actionKnownFields 是动作对象的合法字段集（§5.1 规则 2「无法识别的控制字段」
// 拒绝）；顶层对象只认 actions 一个键。writeLock 供 delegate/summarize 声明
// 派生任务写锁（§6.4 规则 3：纯读任务可 none 与写任务并行），request_context
// 不创建任务、不接受该字段（validateActionEntry 拒绝）。
var actionKnownFields = map[string]bool{
	"workflowId": true, "workspaceId": true,
	"type": true, "sourceTaskId": true, "round": true, "member": true,
	"task": true, "fromTaskId": true, "writeLock": true, "review": true,
	"key": true, "dependsOn": true, "reviewOf": true, "phase": true,
}

// ActionKey 生成动作幂等键（§8.2 变体）：
// team:{teamID}:run:{runID}:action:{sourceTaskID}:{round}:{index}。
func ActionKey(teamID, runID, sourceTaskID string, round, index int) string {
	return fmt.Sprintf("team:%s:run:%s:action:%s:%d:%d", teamID, runID, sourceTaskID, round, index)
}

// leadSettleKey 是 Lead 任务结算批（task_completed + 动作事实）的幂等键：
// team:{teamID}:run:{runID}:lead_settle:{taskID}。整批失败重试（journal 已
// 落、快照未存的崩溃窗口）时按它防重复落账——journal.Append 本身不去重
// （journal.go），防重靠调用方预扫描已记账键（forwardRecorded 同款）。
func leadSettleKey(teamID, runID, taskID string) string {
	return fmt.Sprintf("team:%s:run:%s:lead_settle:%s", teamID, runID, taskID)
}

// invalidActionKey 是第 n 次 invalid_action_block 的幂等键。
func invalidActionKey(teamID, runID, rootTaskID string, attempt int) string {
	return fmt.Sprintf("team:%s:run:%s:invalid_action:%s:%d", teamID, runID, rootTaskID, attempt)
}

// actionRepairPlannedKey 是修复任务 task_planned 事实的幂等键。
func actionRepairPlannedKey(teamID, runID, repairTaskID string) string {
	return fmt.Sprintf("team:%s:run:%s:actfix:%s", teamID, runID, repairTaskID)
}

// ExtractActionBlock 从 Lead 回复全文提取 <team-actions> 信封内容（§5.1
// 规则 1）。返回 (inner, nil)＝找到唯一成对块；("", nil)＝全文没有标记（普通
// 文本回复，不触发动作也不触发修复）；(_, violations)＝结构错误。不成对、
// 多个块、空块都产中文 violation；开始标记取首个闭合标记之前最后一个（引用
// 回显场景取最内层，修复提示词里的标记文本被 Lead 原样引用时不会吞掉真实
// 块——递归防护的一部分：回送消息本身不产生新动作）。
func ExtractActionBlock(fullText string) (string, []string) {
	firstOpen := strings.Index(fullText, actionsOpenTag)
	firstClose := strings.Index(fullText, actionsCloseTag)
	if firstOpen < 0 && firstClose < 0 {
		return "", nil // 普通文本回复：无动作块。
	}
	if firstClose >= 0 && (firstOpen < 0 || firstClose < firstOpen) {
		return "", []string{fmt.Sprintf("发现孤立的结束标记 %q：动作块的开始与结束标记必须成对出现", actionsCloseTag)}
	}
	if firstClose < 0 {
		return "", []string{fmt.Sprintf("开始标记 %q 没有配对的结束标记 %q", actionsOpenTag, actionsCloseTag)}
	}
	// 首个闭合标记之后的任何残留标记都是「多个块」（§5.1 规则 1：整段回复
	// 只允许一个动作块）。
	rest := fullText[firstClose+len(actionsCloseTag):]
	if strings.Contains(rest, actionsOpenTag) || strings.Contains(rest, actionsCloseTag) {
		return "", []string{"发现多个 team-actions 动作块：整段回复只能包含一个动作块"}
	}
	open := strings.LastIndex(fullText[:firstClose], actionsOpenTag)
	inner := fullText[open+len(actionsOpenTag) : firstClose]
	if strings.TrimSpace(inner) == "" {
		return "", []string{"动作块内容为空：标记之间必须是单个 JSON 对象"}
	}
	return inner, nil
}

// decodeActionPayload 对信封内容做两级解码（§5.1 规则 6）：先严格
// jsoncompat.Unmarshal（拒绝尾逗号/尾随数据/单引号）；失败给一次手写宽容
// normalizer（容忍尾逗号、单引号字符串、裸键名），改写产物再过同一严格
// Unmarshal。两级错误汇入同一 violations 构造——宽容改写绝不静默接受非法
// 结构；宽容成功者与严格成功者走同一 schema 校验（ValidateActions）。
func decodeActionPayload(inner string) (*jsoncompat.Object, []string) {
	value, strictErr := jsoncompat.Unmarshal([]byte(inner))
	if strictErr == nil {
		return requireJSONObject(value)
	}
	lenientValue, lenientErr := jsoncompat.Unmarshal([]byte(lenientJSONText(inner)))
	if lenientErr == nil {
		// 宽容成功仍走同一出口（requireJSONObject + ValidateActions 的同一
		// schema），绝不因改写而放松结构要求（§5.1 规则 6）。
		return requireJSONObject(lenientValue)
	}
	return nil, []string{fmt.Sprintf(
		"动作块内容不是合法 JSON：严格解析报「%s」；宽容解析（容忍尾逗号、单引号字符串、裸键名）后仍报「%s」",
		strictErr.Error(), lenientErr.Error())}
}

// requireJSONObject 断言解码结果是单个 JSON 对象（§5.1 规则 1）。
func requireJSONObject(value any) (*jsoncompat.Object, []string) {
	if object, ok := value.(*jsoncompat.Object); ok {
		return object, nil
	}
	return nil, []string{fmt.Sprintf("动作块内容必须是单个 JSON 对象，实际是 %s", jsonValueKind(value))}
}

// jsonValueKind 返回 JSON 值的中文类别名（violation 文案用）。
func jsonValueKind(value any) string {
	switch value.(type) {
	case *jsoncompat.Array:
		return "数组"
	case string:
		return "字符串"
	case json.Number:
		return "数字"
	case bool:
		return "布尔值"
	case nil:
		return "null"
	}
	return "未知形态"
}

// lenientJSONText 对常见手写 JSON 变体做一次最小改写（宽容 parse 的实现，
// 无第三方库）：①尾逗号（逗号后跳过空白即 `}`/`]`）删除；②单引号字符串改写
// 为双引号（串内裸 `"` 转义、`\'` 还原为 `'`，其余转义序列原样保留——非法
// 转义仍会被严格解码拒绝）；③对象裸键名（`{` 或 `,` 后的标识符）加双引号。
// 三类变换对合法 JSON 输入逐字节恒等（合法 JSON 不含这些形态），改写产物
// 仍交严格 Unmarshal 检验，绝不变形合法 JSON。
func lenientJSONText(input string) string {
	var out strings.Builder
	out.Grow(len(input))
	var stack []bool // 每层容器：true=对象（键位判定用）
	last := byte(0)  // 上一个有意义 token 的类别：'{' '[' ',' ':' 或 'v'（值/串/闭括号）
	for i := 0; i < len(input); {
		c := input[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			out.WriteByte(c)
			i++
		case c == '"':
			end := scanQuoted(input, i, '"')
			out.WriteString(input[i:end])
			i = end
			last = 'v'
		case c == '\'':
			end := scanQuoted(input, i, '\'')
			out.WriteByte('"')
			out.WriteString(requoteSingle(input[i+1 : end-1]))
			out.WriteByte('"')
			i = end
			last = 'v'
		case c == '{':
			out.WriteByte(c)
			stack = append(stack, true)
			i++
			last = '{'
		case c == '[':
			out.WriteByte(c)
			stack = append(stack, false)
			i++
			last = '['
		case c == '}' || c == ']':
			out.WriteByte(c)
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
			last = 'v'
		case c == ',':
			if trailing, next := jsonTrailingComma(input, i); trailing {
				i = next // 尾逗号：连同其后空白一并丢弃（后续 token 自然流过）。
			} else {
				out.WriteByte(c)
				i++
			}
			last = ','
		case c == ':':
			out.WriteByte(c)
			i++
			last = ':'
		case isBareIdentStart(c):
			if expectBareKey(stack, last) {
				j := i
				for j < len(input) && isBareIdentPart(input[j]) {
					j++
				}
				out.WriteByte('"')
				out.WriteString(input[i:j])
				out.WriteByte('"')
				i = j
			} else {
				out.WriteByte(c) // true/false/null 等字面量：值位不加引号。
				i++
			}
			last = 'v'
		default:
			out.WriteByte(c)
			i++
			last = 'v'
		}
	}
	return out.String()
}

// scanQuoted 返回从开引号起（含）到闭引号后（不含下一字符）的结束下标；
// 未闭合返回 len(input)（原样交还，严格解码照常拒绝）。
func scanQuoted(input string, start int, quote byte) int {
	for i := start + 1; i < len(input); i++ {
		if input[i] == '\\' {
			i++ // 跳过转义对的第二个字符。
			continue
		}
		if input[i] == quote {
			return i + 1
		}
	}
	return len(input)
}

// jsonTrailingComma 报告 input[at]（逗号）是否为尾逗号，并返回其后第一个
// 非空白字符的下标。
func jsonTrailingComma(input string, at int) (bool, int) {
	j := at + 1
	for j < len(input) {
		switch input[j] {
		case ' ', '\t', '\n', '\r':
			j++
		case '}', ']':
			return true, j
		default:
			return false, j
		}
	}
	return false, j
}

// requoteSingle 把单引号字符串的内部内容改写为双引号字符串的转义内容
// （不含外层引号，调用方负责包裹）。转义对逐个处理：`\'` 还原为裸 `'`
// （JSON 无此转义），其余 `\X` 原样保留（合法 JSON 转义照常解码，非法转义
// 交给严格解码拒绝）；未转义的 `"` 转成 `\"`。
func requoteSingle(content string) string {
	var builder strings.Builder
	builder.Grow(len(content))
	for k := 0; k < len(content); k++ {
		ch := content[k]
		if ch == '\\' && k+1 < len(content) {
			if content[k+1] == '\'' {
				builder.WriteByte('\'')
			} else {
				builder.WriteByte('\\')
				builder.WriteByte(content[k+1])
			}
			k++
			continue
		}
		if ch == '"' {
			builder.WriteString(`\"`)
			continue
		}
		builder.WriteByte(ch)
	}
	return builder.String()
}

func isBareIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isBareIdentPart(c byte) bool {
	return isBareIdentStart(c) || (c >= '0' && c <= '9')
}

// expectBareKey 报告当前是否处于对象键位（栈顶为对象，且上一 token 是 `{`
// 或 `,`）——只有键位的标识符才加引号，值位的 true/false/null 不动。
func expectBareKey(stack []bool, last byte) bool {
	if len(stack) == 0 || !stack[len(stack)-1] {
		return false
	}
	return last == '{' || last == ','
}

// ValidateActions 校验解码后的信封对象并产出规范 Action（§5.1 规则 2、
// §5.2）。任何 violation 都按 all-or-nothing 拒绝整块（§5.1 规则 3：解析与
// schema 校验失败走同一条修复回路）：
//
//   - 顶层只认 actions 数组（未知顶层键即 violation）；动作数 ≤
//     min(MaxTasksPerRun, 8)；
//   - 每条动作：字段集 ⊆ {type, sourceTaskId, round, member, task,
//     fromTaskId, writeLock, review}；type ∈ {delegate, request_context, summarize}；
//     sourceTaskId 必填且存在于本 Run；round 为 1..policy.MaxRounds 的整数；
//     writeLock ∈ {exclusive, none} 且仅 delegate/summarize 可携带（§6.4
//     规则 3：纯读任务声明 none 后可与写任务并行）；review 仅 delegate
//     可携带，true=独立审查，false=普通执行；
//   - delegate/request_context 必带 member（存在且 enabled）与 task（非空，
//     rune 上限同 PromptLimit 口径）；request_context 必带合法 fromTaskId
//     （属于本 Run 且已完成的任务——「请求另一成员提供已有结果」的来源）；
//     summarize 从简（v1 汇总全部已完成任务，不校验也不要求 fromTaskId）；
//     非空 fromTaskId 不论类型一律校验存在且已完成。
//
// 校验通过的动作落 Status=proposed 并携带确定性 ActionKey。
func ValidateActions(payload *jsoncompat.Object, team *Team, run *Run, policy TeamPolicy) ([]Action, []string) {
	if payload == nil {
		return nil, []string{"动作块内容为空"}
	}
	var violations []string
	for _, key := range payload.Keys() {
		if key != "actions" && key != "collaborationPattern" {
			violations = append(violations, fmt.Sprintf("动作块对象包含无法识别的字段 %q（合法字段只有 actions 与 collaborationPattern）", key))
		}
	}
	rawActions, has := payload.Get("actions")
	if !has {
		return nil, append(violations, "动作块对象缺少 actions 数组字段")
	}
	list, ok := rawActions.(*jsoncompat.Array)
	if !ok {
		return nil, append(violations, fmt.Sprintf("actions 字段必须是数组，实际是 %s", jsonValueKind(rawActions)))
	}
	limit := policy.MaxTasksPerRun
	if limit > maxActionsPerBlock {
		limit = maxActionsPerBlock
	}
	if len(list.Items) > limit {
		violations = append(violations, fmt.Sprintf("动作数量超过上限：最多 %d 条，收到 %d 条", limit, len(list.Items)))
	}
	promptLimit := PromptLimit(policy.MaxContextChars)
	for index, item := range list.Items {
		entry, ok := item.(*jsoncompat.Object)
		if !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作必须是 JSON 对象，实际是 %s", index+1, jsonValueKind(item)))
			continue
		}
		violations = append(violations, validateActionEntry(entry, team, run, policy, promptLimit, index)...)
	}
	if len(violations) > 0 {
		return nil, violations
	}
	actions := make([]Action, 0, len(list.Items))
	for index, item := range list.Items {
		entry := item.(*jsoncompat.Object)
		action := Action{
			Status: ActionStatusProposed,
		}
		action.Type, _ = entry.MustGet("type").(string)
		action.SourceTaskID, _ = entry.MustGet("sourceTaskId").(string)
		action.Round = actionRoundOf(entry)
		action.Member, _ = entry.MustGet("member").(string)
		action.Task, _ = entry.MustGet("task").(string)
		action.WorkflowID, _ = entry.MustGet("workflowId").(string)
		action.WorkspaceID, _ = entry.MustGet("workspaceId").(string)
		phase, _ := entry.MustGet("phase").(string)
		action.Phase = NormalizeTaskPhase(phase)
		action.FromTaskID, _ = entry.MustGet("fromTaskId").(string)
		action.WriteLock, _ = entry.MustGet("writeLock").(string)
		action.Key, _ = entry.MustGet("key").(string)
		if raw, has := entry.Get("dependsOn"); has {
			if dependsList, isArray := raw.(*jsoncompat.Array); isArray {
				deps := make([]string, 0, len(dependsList.Items))
				for _, entry2 := range dependsList.Items {
					if text, isString := entry2.(string); isString {
						deps = append(deps, text)
					}
				}
				action.DependsOn = deps
			}
		}
		if raw, has := entry.Get("reviewOf"); has {
			if reviewList, isArray := raw.(*jsoncompat.Array); isArray {
				refs := make([]string, 0, len(reviewList.Items))
				for _, entry2 := range reviewList.Items {
					if text, isString := entry2.(string); isString {
						refs = append(refs, text)
					}
				}
				action.ReviewOf = refs
			}
		}
		if review, exists := entry.MustGet("review").(bool); exists {
			action.Review = reviewFlag(review)
		}
		action.ActionKey = ActionKey(run.TeamID, run.ID, action.SourceTaskID, action.Round, index)
		actions = append(actions, action)
	}
	byKey, keyViolations := validateActionBatchKeys(team, run, actions)
	if len(keyViolations) > 0 {
		return nil, keyViolations
	}
	if orderViolations := validateActionReviewOrder(team, run, actions, byKey); len(orderViolations) > 0 {
		return nil, orderViolations
	}
	if planViolations := validateActionPlanGates(team, run, actions, byKey, policy); len(planViolations) > 0 {
		return nil, planViolations
	}
	return actions, nil
}

// delegateActionIsReview 判定 delegate 动作是否承担审查语义（docs/
// team-demand-queue-plan.md §8 阶段 0）：review=true 显式审查；未声明时默认
// 评审成员承接的委派按审查兼容（IsReviewTask 的缺省口径）；review=false
// 显式执行。request_context/summarize 不创建成员任务，一律不算。
func delegateActionIsReview(team *Team, action *Action) bool {
	if action == nil || action.Type != ActionTypeDelegate {
		return false
	}
	if action.Review != nil {
		return *action.Review
	}
	reviewer := team.ReviewerMemberID()
	return reviewer != "" && reviewer != team.LeadID && action.Member == reviewer
}

// runHasUnfinishedExecution 报告 Run 快照是否仍有未终态的执行任务：排除审查
// 任务（IsReviewTask）、Reduction 汇总与 Lead 任务（含 actfix 修复回合——编排
// 载体，可与审查并行）。2026-11-07 的真实 Run 正是「实现 17:19 才完成、审查
// 17:15 已派发」：该口径是阶段 0 的保守屏障，reviewOf/dependsOn 上线（阶段 5）
// 后由版本绑定的因果依赖替换。
func runHasUnfinishedExecution(team *Team, run *Run) bool {
	if team == nil || run == nil {
		return false
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if IsReviewTask(team, task) || task.Reduction || task.MemberID == team.LeadID {
			continue
		}
		switch task.Status {
		case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
			continue
		}
		return true
	}
	return false
}

// validateActionReviewOrder 是审查顺序屏障，只作用于 autoPlan 的 Lead 动作
// （ValidateActions 仅从 ParseLeadActions 进入；显式 Plan 走 ParsePlan 的
// DAG 依赖，不受此限制）。阶段 0 的保守形态是「混排拒绝 + 未完成执行拒
// 绝」；阶段 2 起 dependsOn 上线（docs/team-demand-queue-plan.md §8），
// 审查动作可用显式依赖覆盖执行任务：
//   - 同批混排时，每条审查动作的 dependsOn（经 byKey 解析）必须覆盖本批
//     创建的全部执行任务；
//   - Run 仍有未完成执行任务时，审查动作的显式依赖必须覆盖它们全部。
//
// 未覆盖的保持保守拒绝——审查先于实现完成启动的根因正是「都只依赖根
// Lead」。reviewOf 版本绑定（阶段 5）上线后按方案替换为因果+版本双闸门。
func validateActionReviewOrder(team *Team, run *Run, actions []Action, byKey map[string]string) []string {
	if run != nil && run.Planning != nil {
		var violations []string
		for i := range actions {
			if delegateActionIsReview(team, &actions[i]) && len(actions[i].ReviewOf) == 0 {
				violations = append(violations, "workflow 审查必须用 reviewOf 声明被审任务；被审依赖完成后才能执行")
			}
		}
		// reviewOf 在批级校验中解析，并由 buildActionTask 并入实际依赖。
		return violations
	}
	hasExecution := false
	reviews := make([]*Action, 0, 1)
	batchExecutions := make([]string, 0, len(actions)) // 本批执行动作的派生任务 ID
	for i := range actions {
		action := &actions[i]
		if delegateActionIsReview(team, action) {
			reviews = append(reviews, action)
			continue
		}
		if action.Type == ActionTypeDelegate {
			hasExecution = true
			if derived := ActionDerivedTaskID(action); derived != "" {
				batchExecutions = append(batchExecutions, derived)
			}
		}
	}
	if len(reviews) == 0 {
		return nil
	}
	// 既有未完成执行任务（阶段 0 口径的过滤器）。
	unfinished := make([]string, 0, 4)
	if run != nil {
		for i := range run.Tasks {
			task := &run.Tasks[i]
			if IsReviewTask(team, task) || task.Reduction || task.MemberID == team.LeadID {
				continue
			}
			switch task.Status {
			case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
				continue
			}
			unfinished = append(unfinished, task.ID)
		}
	}
	var violations []string
	for _, review := range reviews {
		covered := resolvedActionDeps(review, byKey)
		var uncovered []string
		if hasExecution {
			for _, derived := range batchExecutions {
				if !containsString(covered, derived) {
					uncovered = append(uncovered, derived)
				}
			}
		}
		for _, taskID := range unfinished {
			if !containsString(covered, taskID) {
				uncovered = append(uncovered, taskID)
			}
		}
		if len(uncovered) > 0 {
			violations = append(violations, fmt.Sprintf("审查动作（%s）未用 dependsOn 覆盖执行任务 %s：审查必须显式依赖本批与在途的全部实现任务，或等实现完成后单独成批",
				actionLabel(review, ActionDerivedTaskID(review)), strings.Join(boundedViolationIDs(uncovered), "、")))
		}
	}
	return violations
}

// resolvedActionDeps 把动作的 dependsOn 经同批 key 映射解析为任务 ID 列表
// （fromTaskId 一并计入——它同样是显式依赖）。
func resolvedActionDeps(action *Action, byKey map[string]string) []string {
	deps := make([]string, 0, len(action.DependsOn)+len(action.ReviewOf)+1)
	for _, ref := range action.DependsOn {
		if derived, isKey := byKey[ref]; isKey {
			deps = append(deps, derived)
		} else {
			deps = append(deps, ref)
		}
	}
	// reviewOf 同为显式因果绑定（阶段 5）：计入审查顺序屏障的覆盖口径。
	for _, ref := range action.ReviewOf {
		if derived, isKey := byKey[ref]; isKey {
			deps = append(deps, derived)
		} else {
			deps = append(deps, ref)
		}
	}
	if action.FromTaskID != "" {
		deps = append(deps, action.FromTaskID)
	}
	return deps
}

// boundedViolationIDs 把未覆盖清单裁到有界形态（≤8 条 + 省略说明）。
func boundedViolationIDs(ids []string) []string {
	const bound = 8
	if len(ids) <= bound {
		return ids
	}
	return append(append([]string(nil), ids[:bound]...), fmt.Sprintf("（其余 %d 项已省略）", len(ids)-bound))
}

// validateActionEntry 校验单条动作（报错带序号与实际值，仿 planner.go 的
// 逐条首错风格；这里收集全部错误供修复回路一次回送）。
func validateActionEntry(entry *jsoncompat.Object, team *Team, run *Run, policy TeamPolicy, promptLimit, index int) []string {
	var violations []string
	ordinal := index + 1
	for _, field := range []string{"workflowId", "workspaceId"} {
		if raw, present := entry.Get(field); present {
			if run.Planning == nil || entry.MustGet("type") != ActionTypeDelegate {
				violations = append(violations, fmt.Sprintf("第 %d 条动作的 %s 仅用于新版规划的 delegate", ordinal, field))
			}
			value, valid := raw.(string)
			if !valid || value == "" || len(value) > 64 || (field == "workflowId" && !ValidMemberID(value)) {
				violations = append(violations, fmt.Sprintf("第 %d 条动作的 %s 无效", ordinal, field))
			}
		}
	}
	for _, key := range entry.Keys() {
		if !actionKnownFields[key] {
			violations = append(violations, fmt.Sprintf("第 %d 条动作包含无法识别的字段 %q", ordinal, key))
		}
	}
	actionType, _ := entry.MustGet("type").(string)
	switch actionType {
	case ActionTypeDelegate, ActionTypeRequestContext, ActionTypeSummarize:
	default:
		violations = append(violations, fmt.Sprintf("第 %d 条动作的 type 无效：%q（可用：delegate、request_context、summarize）", ordinal, actionType))
	}
	sourceTaskID, _ := entry.MustGet("sourceTaskId").(string)
	if sourceTaskID == "" {
		violations = append(violations, fmt.Sprintf("第 %d 条动作缺少 sourceTaskId 字段", ordinal))
	} else if findTaskIn(run, sourceTaskID) == nil {
		violations = append(violations, fmt.Sprintf("第 %d 条动作引用了不存在的来源任务：%q", ordinal, sourceTaskID))
	}
	roundViolation := actionRoundViolation(entry, policy, ordinal)
	if roundViolation != "" {
		violations = append(violations, roundViolation)
	}
	member, _ := entry.MustGet("member").(string)
	if (actionType == ActionTypeDelegate || actionType == ActionTypeRequestContext) && member == "" {
		violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）缺少 member 字段", ordinal, actionType))
	}
	if member != "" {
		if target := memberOf(team, member); target == nil {
			violations = append(violations, fmt.Sprintf("第 %d 条动作引用了不存在的成员：%q", ordinal, member))
		} else if !target.Enabled {
			violations = append(violations, fmt.Sprintf("第 %d 条动作引用了已停用的成员：%q", ordinal, member))
		}
	}
	if raw, has := entry.Get("review"); has {
		review, isBool := raw.(bool)
		if !isBool {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 review 必须是布尔值，实际是 %s", ordinal, jsonValueKind(raw)))
		} else if actionType != ActionTypeDelegate {
			violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）不支持 review 字段", ordinal, actionType))
		} else if review {
			reviewer := team.ReviewerMemberID()
			if reviewer == "" || reviewer == team.LeadID || member != reviewer {
				violations = append(violations, fmt.Sprintf("第 %d 条动作的 review=true 只能指派给独立的默认评审成员", ordinal))
			}
		}
	}
	taskText, _ := entry.MustGet("task").(string)
	if actionType == ActionTypeDelegate || actionType == ActionTypeRequestContext {
		if strings.TrimSpace(taskText) == "" {
			violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）缺少 task 字段（任务内容不能为空）", ordinal, actionType))
		}
	}
	if taskText != "" {
		if runes := len([]rune(taskText)); runes > promptLimit {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的任务内容过长：%d 字，当前上限 %d 字", ordinal, runes, promptLimit))
		}
	}
	if raw, has := entry.Get("phase"); has && raw != nil {
		if _, ok := raw.(string); !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 phase 必须是字符串，实际是 %s", ordinal, jsonValueKind(raw)))
		} else if actionType != ActionTypeDelegate {
			violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）不支持 phase 字段", ordinal, actionType))
		}
	}
	fromTaskID, _ := entry.MustGet("fromTaskId").(string)
	if actionType == ActionTypeRequestContext && fromTaskID == "" {
		violations = append(violations, fmt.Sprintf("第 %d 条动作（request_context）缺少 fromTaskId 字段：必须指明请求哪个已有结果", ordinal))
	}
	if fromTaskID != "" {
		if from := findTaskIn(run, fromTaskID); from == nil {
			violations = append(violations, fmt.Sprintf("第 %d 条动作引用了不存在的结果来源任务：%q", ordinal, fromTaskID))
		} else if from.Status != TaskCompleted {
			violations = append(violations, fmt.Sprintf("第 %d 条动作引用了尚未完成的任务结果：%q", ordinal, fromTaskID))
		}
	}
	// writeLock（可选，仅 delegate/summarize）：取值校验与 planner 的计划任务
	// 同词汇（exclusive/none）；request_context 不创建任务，携带即拒绝。
	if raw, has := entry.Get("writeLock"); has && raw != nil {
		writeLock, ok := raw.(string)
		if !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 writeLock 必须是字符串，实际是 %s", ordinal, jsonValueKind(raw)))
		} else {
			switch writeLock {
			case TaskWriteLockExclusive, TaskWriteLockNone:
			default:
				violations = append(violations, fmt.Sprintf("第 %d 条动作的 writeLock 取值无效：%s（可用：exclusive、none）", ordinal, writeLock))
			}
			if actionType == ActionTypeRequestContext {
				violations = append(violations, fmt.Sprintf("第 %d 条动作（request_context）不支持 writeLock 字段：该动作不创建任务", ordinal))
			}
		}
	}
	// key/dependsOn（可选，仅 delegate，docs/team-demand-queue-plan.md §4 阶段 2）：
	// key 是同批次局部别名（成员 ID 正则词汇）；dependsOn 的条目在批级解析
	//（同批 key 或本 Run 既有任务 ID），这里只做形态校验。
	if raw, has := entry.Get("key"); has && raw != nil {
		key, ok := raw.(string)
		if !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 key 必须是字符串，实际是 %s", ordinal, jsonValueKind(raw)))
		} else if !ValidMemberID(key) {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 key 无效：%q（字母/数字/下划线/连字符，1-64 字符）", ordinal, key))
		} else if actionType != ActionTypeDelegate {
			violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）不支持 key 字段：同批依赖只用于创建任务的 delegate", ordinal, actionType))
		}
	}
	if raw, has := entry.Get("reviewOf"); has && raw != nil {
		list, ok := raw.(*jsoncompat.Array)
		if !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 reviewOf 必须是字符串数组，实际是 %s", ordinal, jsonValueKind(raw)))
		} else {
			for _, item := range list.Items {
				ref, isString := item.(string)
				if !isString || !ValidMemberID(ref) {
					violations = append(violations, fmt.Sprintf("第 %d 条动作的 reviewOf 条目无效：%v（应为同批 key 或本 Run 任务 ID）", ordinal, item))
					break
				}
			}
		}
	}
	if raw, has := entry.Get("dependsOn"); has && raw != nil {
		list, ok := raw.(*jsoncompat.Array)
		if !ok {
			violations = append(violations, fmt.Sprintf("第 %d 条动作的 dependsOn 必须是字符串数组，实际是 %s", ordinal, jsonValueKind(raw)))
		} else {
			if actionType != ActionTypeDelegate {
				violations = append(violations, fmt.Sprintf("第 %d 条动作（%s）不支持 dependsOn 字段：同批依赖只用于创建任务的 delegate", ordinal, actionType))
			}
			for _, item := range list.Items {
				ref, isString := item.(string)
				if !isString || !ValidMemberID(ref) {
					violations = append(violations, fmt.Sprintf("第 %d 条动作的 dependsOn 条目无效：%v（应为同批 key 或本 Run 任务 ID）", ordinal, item))
					break
				}
			}
		}
	}
	return violations
}

// validateActionBatchKeys 是同批 key/dependsOn 的批级校验与解析（§4 阶段 2）：
//   - key 批内唯一，且不得与既有任务 ID 同名（dependsOn 解析的二义性防线）；
//   - dependsOn 条目必须解析为同批 key 或本 Run 既有任务 ID；
//   - 合并图（新派生任务 + 既有任务）过 findDependencyCycle，环整体拒绝。
//
// 返回 key → 派生任务 ID 的映射（执行期 buildActionTask 复用同一解析）。
func validateActionBatchKeys(team *Team, run *Run, actions []Action) (map[string]string, []string) {
	byKey := map[string]string{}
	var violations []string
	for i := range actions {
		action := &actions[i]
		if action.Key == "" {
			continue
		}
		derived := ActionDerivedTaskID(action)
		if derived == "" {
			continue // 幂等键不可解析：ValidateExecutable 会拒绝，这里不重复报
		}
		if _, duplicate := byKey[action.Key]; duplicate {
			violations = append(violations, fmt.Sprintf("同批动作 key 重复：%s", action.Key))
			continue
		}
		if findTaskIn(run, action.Key) != nil {
			violations = append(violations, fmt.Sprintf("动作 key %s 与本 Run 既有任务 ID 同名，请换一个别名", action.Key))
			continue
		}
		byKey[action.Key] = derived
	}
	dependencies := map[string][]string{}
	for i := range run.Tasks {
		dependencies[run.Tasks[i].ID] = run.Tasks[i].DependsOn
	}
	for i := range actions {
		action := &actions[i]
		if len(action.DependsOn) == 0 {
			continue
		}
		derived := ActionDerivedTaskID(action)
		deps := make([]string, 0, len(action.DependsOn))
		for _, ref := range action.DependsOn {
			switch {
			case byKey[ref] != "":
				deps = append(deps, byKey[ref])
			case findTaskIn(run, ref) != nil:
				deps = append(deps, ref)
			default:
				violations = append(violations, fmt.Sprintf("动作 %s 的 dependsOn 引用无法解析：%q（既不是同批 key，也不是本 Run 任务）", actionLabel(action, derived), ref))
			}
		}
		if derived != "" {
			dependencies[derived] = deps
		}
	}
	// reviewOf（阶段 5）：审查动作的被审引用与 dependsOn 同规则解析（同批
	// key → 派生 ID；否则本 Run 既有任务），未解析整体拒绝；解析结果并入
	// 依赖图（审查等待被审实现）。
	for i := range actions {
		action := &actions[i]
		if len(action.ReviewOf) == 0 {
			continue
		}
		if !delegateActionIsReview(team, action) {
			violations = append(violations, fmt.Sprintf("动作 %s 的 reviewOf 只能用于审查动作", actionLabel(action, ActionDerivedTaskID(action))))
			continue
		}
		derived := ActionDerivedTaskID(action)
		deps := dependencies[derived]
		for _, ref := range action.ReviewOf {
			mapped := ref
			if target, isKey := byKey[ref]; isKey {
				mapped = target
			} else if findTaskIn(run, ref) == nil {
				violations = append(violations, fmt.Sprintf("审查动作 %s 的 reviewOf 引用无法解析：%q（既不是同批 key，也不是本 Run 任务）", actionLabel(action, derived), ref))
				continue
			}
			if !containsString(deps, mapped) {
				deps = append(deps, mapped)
			}
		}
		if derived != "" {
			dependencies[derived] = deps
		}
	}
	if len(violations) > 0 {
		return nil, violations
	}
	// 环检测以新派生任务为起点（findDependencyCycle 沿 dependencies 深入既有
	// 任务，合并图的回边都能被切出）。
	nodes := make([]PlanTaskInput, 0, len(actions))
	for i := range actions {
		if derived := ActionDerivedTaskID(&actions[i]); derived != "" {
			nodes = append(nodes, PlanTaskInput{ID: derived})
		}
	}
	if cycle := findDependencyCycle(nodes, dependencies); cycle != "" {
		return nil, []string{"同批动作依赖存在循环：" + cycle}
	}
	return byKey, nil
}

// actionLabel 返回动作的人类可读标识（key 优先，回退派生 ID）。
func actionLabel(action *Action, derived string) string {
	if action != nil && action.Key != "" {
		return action.Key
	}
	return derived
}

// containsString 报告切片是否含指定字符串。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// sortActionsByBatchDependencies 把同批 delegate 动作按 key 依赖稳定拓扑排序
// （解析失败的引用原样保留）：被依赖的先执行，派生任务先落 Run，下游
// DependsOn 引用的 ID 在创建时已存在。解析由 validateActionBatchKeys 保证，
// 这里防御性容忍未解析引用（不重排）。
func sortActionsByBatchDependencies(actions []Action, byKey map[string]string) {
	derivedOf := map[string]string{}
	for i := range actions {
		if actions[i].Key != "" {
			if derived := ActionDerivedTaskID(&actions[i]); derived != "" {
				derivedOf[actions[i].Key] = derived
			}
		}
	}
	position := make(map[string]int, len(actions))
	for i := range actions {
		position[actions[i].ActionKey] = i
	}
	visited := map[string]int{} // 0=未访问 1=访问中 2=完成
	var order []int
	var visit func(index int)
	visit = func(index int) {
		if visited[actions[index].ActionKey] != 0 {
			return
		}
		visited[actions[index].ActionKey] = 1
		for _, ref := range actions[index].DependsOn {
			derived := byKey[ref]
			if derived == "" {
				derived = derivedOf[ref]
			}
			if derived == "" {
				continue
			}
			for j := range actions {
				if j != index && ActionDerivedTaskID(&actions[j]) == derived && visited[actions[j].ActionKey] == 0 {
					visit(j)
				}
			}
		}
		visited[actions[index].ActionKey] = 2
		order = append(order, index)
	}
	for i := range actions {
		visit(i)
	}
	if len(order) != len(actions) {
		return // 防御：不应发生（解析已拒绝环）
	}
	sorted := make([]Action, 0, len(actions))
	for _, index := range order {
		sorted = append(sorted, actions[index])
	}
	copy(actions, sorted)
}

// actionRoundOf 读取 round 整数值（调用方已通过 actionRoundViolation 校验）。
func actionRoundOf(entry *jsoncompat.Object) int {
	number, _ := entry.MustGet("round").(json.Number)
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0
	}
	return value
}

// actionRoundViolation 校验 round：必填、整数、1..policy.MaxRounds（整数
// 读取由 actionRoundOf 独立完成）。
func actionRoundViolation(entry *jsoncompat.Object, policy TeamPolicy, ordinal int) string {
	raw, has := entry.Get("round")
	if !has || raw == nil {
		return fmt.Sprintf("第 %d 条动作缺少 round 字段", ordinal)
	}
	number, ok := raw.(json.Number)
	if !ok {
		return fmt.Sprintf("第 %d 条动作的 round 必须是整数，实际是 %s", ordinal, jsonValueKind(raw))
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return fmt.Sprintf("第 %d 条动作的 round 必须是整数，实际是 %q", ordinal, number.String())
	}
	if value < 1 || value > policy.MaxRounds {
		return fmt.Sprintf("第 %d 条动作的 round 超出范围：允许 1–%d，实际 %d", ordinal, policy.MaxRounds, value)
	}
	return ""
}

// actionSchemaInstruction 是 Lead 简报与修复提示共用的动作协议说明。
func actionSchemaInstruction() string {
	return "放在成对的 team-actions 开始与结束标记之间，标记之间是单个 JSON 对象。只输出一个 team-actions 动作块；如果本回合另有 Issue 归类要求，必须同时先输出一个 team-issues 块，再输出动作块。需要派发时输出相应机器块；纯沟通或澄清必须给可读回复，不能只返回归类或空动作块。对象形如 {\"collaborationPattern\":\"fan-out-and-synthesize\",\"actions\": [...]}；顶层 collaborationPattern 可选，只在协作模式明确或需要修订时声明，省略会保留已有声明。允许 classify-and-act（分类后路由）、fan-out-and-synthesize（独立分支后合并）、adversarial-verification（独立质疑验证）、tournament（同题多候选择优）、loop-until-done（有明确质量门槛和退出决策者的反复修订）；声明只表达规划意图，不替代 dependsOn、reviewOf 或验证，也不要为了形式把单 Agent 可完成的工作拆成团队。每条动作只使用这些字段：type（delegate / request_context / summarize）、sourceTaskId（本 Run 内已存在的任务 ID）、round（1 到最大轮次之间的整数）、member 与 task（delegate 和 request_context 必填：member 必须是团队里存在且启用的成员 ID，task 是具体任务内容）、fromTaskId（request_context 必填：指向本 Run 内已完成的任务）、writeLock（可选，仅 delegate/summarize：默认按任务类型决定，纯读任务可声明 none 与写任务并行）、review（可选，仅 delegate：true 表示独立审查，false 表示普通执行；默认评审成员未声明时按 true 兼容旧任务）、key 与 dependsOn（可选，仅 delegate：同一动作块内先实现后审查等先后关系用它们表达——key 是本批内的唯一别名，dependsOn 列出本批其它动作的 key 或本 Run 既有任务 ID，被依赖的动作先执行；不要用自然语言描述顺序代替依赖）、phase（可选，仅 delegate：阶段标签，implement 写任务必须依赖 phase=plan 的任务）、reviewOf（可选，仅审查 delegate：声明本条审查针对哪些任务——同批 key 或本 Run 任务 ID，被审实现完成后审查才派发，且审查结论绑定派发时的工作区版本）。不要输出上述之外的字段；动作块只用于确实需要执行的自动动作，不要为无需执行的步骤伪造任务。"
}

func actionSchemaForRun(run *Run) string {
	instruction := actionSchemaInstruction()
	if run == nil || run.Planning == nil {
		return instruction
	}
	instruction = strings.Replace(instruction, "对象形如 {\"collaborationPattern\":\"fan-out-and-synthesize\",\"actions\": [...]}；", "对象形如 {\"plan\": {...}, \"collaborationPattern\":\"fan-out-and-synthesize\", \"actions\": [...], \"feedback\": {...}}；plan/feedback 按目标规划协议提供，每条 delegate 必须带 workflowId，workspaceId 仅可引用该 workflow 已登记工作区；", 1)
	return instruction
}

// BuildLeadBriefing 返回 autoPlan Lead 的任务级成员目录与动作协议。修复提示
// 已自带协议，普通成员和显式计划不注入；内容只依赖持久配置，保证确定性。
func BuildLeadBriefing(team *Team, run *Run, task *Task, policy TeamPolicy) string {
	if team == nil || run == nil || task == nil || !run.AutoPlan ||
		team.LeadID == "" || task.MemberID != team.LeadID || task.IssueClassificationOnly || strings.HasPrefix(task.ID, actionRepairPrefix) {
		return ""
	}
	if task.ExecutionContext == ExecutionContextLeadRun {
		return "[Lead execution]\n这是已派发的 Lead 执行任务，不是团队控制回合。请直接完成 [Assigned task] 中的任务并输出普通结果正文；不要再次委派、不要输出 team-actions，也不要复述团队编排协议。"
	}
	var builder strings.Builder
	builder.WriteString("[Team delegation]\n")
	builder.WriteString("你是本次自动规划的 Lead。以下是启用成员；委派时 member 必须使用这里的真实 ID：\n")
	for i := range team.Members {
		member := &team.Members[i]
		if !member.Enabled {
			continue
		}
		name := strings.Join(strings.Fields(member.Name), " ")
		if name == "" {
			name = member.ID
		}
		role := member.Role
		if role == "" {
			role = RoleMember
		}
		fmt.Fprintf(&builder, "- id=%s · name=%s · agent=%s · role=%s", member.ID, truncateRunes(name, 120), member.AgentKind, role)
		if len(member.Tags) > 0 {
			tags := make([]string, 0, len(member.Tags))
			for _, tag := range member.Tags {
				tags = append(tags, truncateRunes(strings.Join(strings.Fields(tag), " "), 40))
			}
			builder.WriteString(" · tags=")
			builder.WriteString(strings.Join(tags, " | "))
		}
		if member.ID == team.LeadID {
			builder.WriteString(" · Lead")
		}
		builder.WriteString("\n")
	}
	builder.WriteString("tags 是用户维护的 Agent 或模型能力提示；制定工作流和选择成员时应参考 tags 与 agent 类型，但不要臆测未标注的能力。\n")
	if reviewer := team.ReviewerMemberID(); reviewer != "" && reviewer != team.LeadID {
		builder.WriteString("默认评审也是可执行任务的普通成员：实现、排查等执行任务委派给它时必须写 review=false，独立审查任务才写 review=true。审查应尽量由未参与实现的成员完成；没有合适的独立评审时明确说明未审查，不要伪造评审结论。")
		if run.Planning == nil {
			builder.WriteString("审查动作必须单独成批：不要与普通执行任务放在同一动作块，也不要在实现任务尚未全部完成时委派审查——需要等实现后再审查时，先给 Lead 自己委派后续任务，等实现完成、后续 Lead 任务取得结果后再单独输出审查动作。\n")
		} else {
			builder.WriteString("每个 workflow 的审查必须用 reviewOf 引用对应实现任务；网关会等待被审任务完成并绑定版本，无需等待无关分支。\n")
		}
	}
	if run.Planning == nil {
		builder.WriteString("默认把一件可独立验收的交付交给一个成员从需求到结果闭环完成；不要按产品、架构、前端、后端、测试等工序横切接力，也不要因为任务较大或需要分批就转交。只有交付物彼此独立且规范或权限不同，或确需独立审查时才拆分成员。接手任务的成员必须自己完成；缺少上下文时用 request_context 引用原始任务续读，不要只转发摘要。\n")
	} else {
		builder.WriteString("按完整目标拆分可独立验收的 workflows，优先同时派发没有真实依赖的分支；每个分支自己完成实现与验证。不要把整个大目标交成单个串行实现任务，也不要按工序横切接力。\n")
	}
	builder.WriteString("你是上传下达的团队主控，负责陪聊、澄清需求、制定工作流、分派和汇总。寒暄、进度说明与澄清问题等纯沟通可直接用自然语言回复；凡是需要排查、阅读或修改文件、运行命令、实现、评审等实质性工作的请求，都必须先通过 delegate 分派，不得在主控回合中不经委派直接执行。Lead 也是默认执行成员，允许 delegate 给自己；后续 Lead 执行任务应按任务内容直接完成，不要仅为转交而再次委派。Lead 自己的 delegate 也可用于汇总结果和继续规划。")
	builder.WriteString("派活门槛：交给其他成员要付出新会话与上下文交接成本。小范围修改、修复审查指出的阻塞问题、需要与界面或调用方反复对齐接口的工作，优先 delegate 给 Lead 自己完成；只有接口已明确、可独立验收的整块实现，或确需特定成员能力（参考 tags）时，才委派给其他成员。委派给其他成员时，task 必须写清要遵守的接口或约定、验收标准，以及需要同步修改的同类位置——对方只能看到任务文本和工作区，看不到你的对话。需要编排时，")
	builder.WriteString(actionSchemaForRun(run))
	builder.WriteString("\n")
	if policy.AutoDispatch {
		builder.WriteString("当前 autoDispatch=true：delegate 与 request_context 校验通过后可自动执行；summarize 由 Lead 汇总。\n")
	} else {
		builder.WriteString("当前 autoDispatch=false：delegate 与 request_context 只生成待确认预览，不会自动执行；summarize 仍可执行。\n")
	}
	if run.Planning == nil {
		builder.WriteString("如果需要等成员结果后由 Lead 汇总或继续规划，请在首批动作中同时给 Lead 自己一条 delegate 作为后续任务；不要在该批动作中直接混用 summarize。后续 Lead 任务取得结果后，再输出 summarize。\n")
	} else {
		builder.WriteString("网关会在本批任务结束后自动派发目标评估，无需创建仅为汇总的 Lead delegate。评估未完成时必须继续补缺口；round 是动作标签，不是目标完成结论。\n")
	}
	actionLimit := policy.MaxTasksPerRun
	if actionLimit > maxActionsPerBlock {
		actionLimit = maxActionsPerBlock
	}
	fmt.Fprintf(&builder, "当前限制：round 为 1–%d；整个 Run 最多 %d 条任务；单个动作块最多 %d 条动作；团队最多 %d 个在途任务。\n",
		policy.MaxRounds, policy.MaxTasksPerRun, actionLimit, policy.MaxParallelTasks)
	workflowField := ""
	if run.Planning != nil {
		workflowField = ",\"workflowId\":\"已定义的workflow ID\""
	}
	fmt.Fprintf(&builder, "单个动作对象示例（仅展示格式，不是要求立即创建任务；把 member_id 替换为上方真实 ID，并按上述说明加上动作信封）：\n{\"type\":\"delegate\",\"sourceTaskId\":%q,\"round\":1,\"member\":\"member_id\",\"task\":\"只读检查并汇报结果\",\"writeLock\":\"none\"%s}\n", task.ID, workflowField)
	return builder.String()
}

// RepairPrompt 拼装修复回送提示词（§5.1 规则 3：violation 列表回送 Lead
// 当前会话定向修复）。逐条 violation 先截断（truncateRunes）、条数超限附
// 省略说明——绝不整包拼接后才检查；文本不含任何易变字段（time.Now 等），
// 同输入恒等（low③ 同根：含时间戳的合成文本会破坏幂等内容比对）。提示词
// 不携带字面标记（修复回路引用回显时不吞真实块）。
func RepairPrompt(violations []string, attempt int) string {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("你上一条回复里的 team-actions 动作块第 %d 次解析失败，编排器没有执行其中任何动作。具体问题：\n", attempt))
	for i, violation := range boundedViolations(violations) {
		builder.WriteString(fmt.Sprintf("%d. %s\n", i+1, violation))
	}
	if containsString(violations, emptyLeadReplyViolation) {
		builder.WriteString("请根据当前用户消息和前轮目标处理：需要实质工作时通过 delegate 派发；无需执行或需要澄清时，必须给用户可读回复。不要只输出归类块或空动作块，也不要为了补格式伪造工作。\n")
	} else {
		builder.WriteString("请重新输出修正后的动作块：")
	}
	builder.WriteString(actionSchemaInstruction())
	return builder.String()
}

const emptyLeadReplyViolation = "主控只返回机器归类或空动作块，既未派发工作也没有可读回复"

// boundedViolations 把 violation 列表裁成有界形态：逐条先截 repairViolation
// Runes 个 rune，条数超 repairMaxViolations 丢弃余下并附省略说明。invalid_
// action_block 事件 payload 与 RepairPrompt 共用，保证事件流恒有界（§10.3）。
func boundedViolations(violations []string) []string {
	bounded := make([]string, 0, len(violations))
	for i, violation := range violations {
		if i == repairMaxViolations {
			bounded = append(bounded, fmt.Sprintf(repairOmissionNoteFmt, len(violations)-i))
			break
		}
		if len([]rune(violation)) > repairViolationRunes {
			violation = truncateRunes(violation, repairViolationRunes)
		}
		bounded = append(bounded, violation)
	}
	return bounded
}

// actionRepairRoot 返回修复链的根任务 ID。AutoPlan Run 的 Lead 任务只有三种
// 形态：合成根任务（planner 的 "lead"）、动作派生任务（{source}-{s|d}{round}-
// {index}，可嵌套）与修复任务（actfix-{root}-{n}）；先剥掉派生后缀（可嵌套，
// 循环剥到稳定）再把 actfix-{root}-{n} 剥回 root（末段恒为修复序号），其余
// 原样返回。不剥派生后缀时，「修复任务的派生子任务」（actfix-{root}-{n}-s{r}-
// {i}）会以自身为根，与既有修复任务撞号（countActionRepairs 前缀命中把子任务
// 误计入尝试号）且派生新链——修复回路的计数与幂等键都以根任务为锚，防止
// 「修复的修复」派生出无限新链。
func actionRepairRoot(taskID string) string {
	id := taskID
	for {
		stripped := stripDerivedTaskSuffix(id)
		if stripped == id {
			break
		}
		id = stripped
	}
	if !strings.HasPrefix(id, actionRepairPrefix) {
		return id
	}
	rest := strings.TrimPrefix(id, actionRepairPrefix)
	if index := strings.LastIndexByte(rest, '-'); index > 0 {
		if _, err := strconv.Atoi(rest[index+1:]); err == nil {
			return rest[:index]
		}
	}
	return rest
}

// stripDerivedTaskSuffix 剥除任务 ID 末尾的动作派生后缀「-{s|d}{round}-{index}」
// （ActionDerivedTaskID 的逆向形态）；不匹配派生形态时原样返回——末段必须是
// 纯数字（index），倒数第二段必须是 s/d 前缀加数字（round）。修复任务本体
// （actfix-{root}-{n}，倒数第二段是 root 的末段）天然不匹配。
func stripDerivedTaskSuffix(taskID string) string {
	last := strings.LastIndexByte(taskID, '-')
	if last <= 0 {
		return taskID
	}
	if _, err := strconv.Atoi(taskID[last+1:]); err != nil {
		return taskID // 末段不是纯数字（index）：不是派生后缀。
	}
	head := taskID[:last]
	second := strings.LastIndexByte(head, '-')
	if second < 0 {
		return taskID
	}
	marker := head[second+1:]
	if len(marker) < 2 || (marker[0] != 's' && marker[0] != 'd') {
		return taskID
	}
	if _, err := strconv.Atoi(marker[1:]); err != nil {
		return taskID
	}
	return head[:second]
}

// countActionRepairs 统计 run 里根任务 root 的既有修复任务数（actfix-{root}-
// 前缀且余段为纯整数——修复任务本体 actfix-{root}-{n}；修复任务的派生子任务
// （actfix-{root}-{n}-s{r}-{i}）前缀同样命中但不是修复回合，混入会虚高尝试号
// （修复回合提前 final）并埋下与子任务的撞号）。修复任务随快照持久化，计数
// 跨重启存活——无需独立计数字段。
func countActionRepairs(run *Run, root string) int {
	prefix := actionRepairPrefix + root + "-"
	count := 0
	for i := range run.Tasks {
		rest := strings.TrimPrefix(run.Tasks[i].ID, prefix)
		if rest == run.Tasks[i].ID {
			continue
		}
		if _, err := strconv.Atoi(rest); err == nil {
			count++
		}
	}
	return count
}

// actionRepairID 是修复任务 ID 的唯一拼装点（actfix-{root}-{attempt}）。
func actionRepairID(root string, attempt int) string {
	return fmt.Sprintf("%s%s-%d", actionRepairPrefix, root, attempt)
}

// nextActionRepairAttempt 计算修复链的下一个尝试号：从 countActionRepairs+1
// 起，向后跳过 ID 已被占用的尝试号（撞号守卫：绝不向 run.Tasks 追加重复 ID
// ——findTaskIn 取首条命中，重复 ID 会让修复任务永远派发不到自己、Run 停摆
// 到超时）；全部占用或超过 maxActionRepairs 时返回 maxActionRepairs+1（调用
// 方按 final 收场）。
func nextActionRepairAttempt(run *Run, root string) int {
	attempt := countActionRepairs(run, root) + 1
	for attempt <= maxActionRepairs && findTaskIn(run, actionRepairID(root, attempt)) != nil {
		attempt++
	}
	return attempt
}

// ParseLeadActions 是 settle 路径的解析编排（纯函数）：完整 assistant 全文 →
// 信封提取 → 两级解码 → schema 校验。返回 (actions, violations)：两者互斥；
// inner 为空串且无 violation 表示普通文本回复（无动作块）。
func ParseLeadActions(fullText string, team *Team, run *Run, policy TeamPolicy) ([]Action, []string) {
	inner, violations := ExtractActionBlock(fullText)
	if len(violations) > 0 || inner == "" {
		return nil, violations
	}
	payload, decodeViolations := decodeActionPayload(inner)
	if len(decodeViolations) > 0 {
		return nil, decodeViolations
	}
	return ValidateActions(payload, team, run, policy)
}

func parseLeadCollaborationPattern(fullText string) (string, []string) {
	inner, violations := ExtractActionBlock(fullText)
	if len(violations) > 0 || inner == "" {
		return "", violations
	}
	payload, violations := decodeActionPayload(inner)
	if len(violations) > 0 {
		return "", violations
	}
	raw, present := payload.Get("collaborationPattern")
	if !present {
		return "", nil
	}
	pattern, ok := raw.(string)
	if !ok || !ValidDeclaredCollaborationPattern(pattern) {
		return "", []string{"collaborationPattern 必须是 classify-and-act、fan-out-and-synthesize、adversarial-verification、tournament、loop-until-done 之一"}
	}
	return pattern, nil
}

// stripActionBlocks 剥离文本中成对的 <team-actions> 动作块（§7.2 移除重复
// Team envelope 的同族义务）：Lead 结果里携带的动作信封绝不再注入下游——
// 下游成员可执行的动作只来自编排器正式派发，绝不由上下文走私。只剥成对
// 块；残留的孤立标记只是普通文本，下游成员任务不做动作解析，无副作用。
func stripActionBlocks(text string) string {
	for {
		open := strings.Index(text, actionsOpenTag)
		if open < 0 {
			return text
		}
		relative := strings.Index(text[open:], actionsCloseTag)
		if relative < 0 {
			return text
		}
		text = text[:open] + text[open+relative+len(actionsCloseTag):]
	}
}

func stripIssueProposalBlocks(text string) string {
	for {
		open := strings.Index(text, issueBlockOpen)
		if open < 0 {
			return text
		}
		relative := strings.Index(text[open:], issueBlockClose)
		if relative < 0 {
			return text
		}
		text = text[:open] + text[open+relative+len(issueBlockClose):]
	}
}

// ResultDisplayText 返回给人看的结果正文：剥离成对动作块并去首尾空白（交付
// 摘要、阶段图预览等投影共用；动作本身由 Lead 动作列表单独展示）。
func ResultDisplayText(text string) string {
	return strings.TrimSpace(stripIssueProposalBlocks(stripActionBlocks(text)))
}

// ---- 动作执行（§5.2，阶段三 actions-exec）：判定与派生构造（纯函数）----
//
// 执行入口在 runner.go 的 passActions（loop 驱动）；本段只做可复判的决策：
// 拒绝清单逐条、派生任务 ID/prompt/依赖的确定性构造。慢 I/O（resolver.Resolve
// 的会话可达性、团队级在途、journal Replay）由执行侧锁外预取进 actionPrefetch。

// summarizePromptDefault 是 summarize 动作未携带 task 文本时的确定性任务
// 内容：schema 不要求 summarize 带 task（v1 汇总全部已完成任务），而派生任务
// 必须有非空 prompt 才能通过账本 1–100000 字节校验——缺省用固定摘要指令
// 兜底（同动作恒等，不含任何易变字段）。
const summarizePromptDefault = "请汇总上述全部已完成任务的结果，形成面向运行目标的最终摘要：先给结论，再分任务列出要点与遗留问题。"

// actionVerdictRejected / actionVerdictExecuted 是 journal 动作终局事实的
// 取值（runner.go 的 actionJournalFacts.settled）。
const (
	actionVerdictRejected = "rejected"
	actionVerdictExecuted = "executed"
)

// ActionDerivedTaskID 返回动作派生任务 ID（§5.2，评审问题⑤⑥）：delegate →
// {sourceTaskID}-d{round}-{index}，summarize → {sourceTaskID}-s{round}-{index}
// （s/d 前缀区分两类动作，index 保同源同轮多动作唯一）。index 取自 ActionKey
// 末段——键只由 ActionKey() 生成（末段恒为 index）；键形态不可解析时返回
// 空串，调用方按确定性拒绝处理。
func ActionDerivedTaskID(action *Action) string {
	if action == nil {
		return ""
	}
	index, ok := actionIndexFromKey(action.ActionKey)
	if !ok {
		return ""
	}
	prefix := "d"
	if action.Type == ActionTypeSummarize {
		prefix = "s"
	}
	return fmt.Sprintf("%s-%s%d-%d", action.SourceTaskID, prefix, action.Round, index)
}

// actionIndexFromKey 解析 ActionKey（team:{t}:run:{r}:action:{source}:{round}:
// {index}）末段的执行序号：无论前段是否含分隔符，末段恒为 index。
func actionIndexFromKey(actionKey string) (int, bool) {
	at := strings.LastIndexByte(actionKey, ':')
	if at < 0 {
		return 0, false
	}
	value, err := strconv.Atoi(actionKey[at+1:])
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// ActionTaskPrompt 返回派生任务的 prompt：动作 task 文本；summarize 允许
// 缺省（schema 不强制），用固定摘要指令兜底。
func ActionTaskPrompt(action *Action) string {
	if action != nil && strings.TrimSpace(action.Task) != "" {
		return action.Task
	}
	return summarizePromptDefault
}

// ActionRequestID 从动作幂等键确定性派生 request_context 的转发请求 UUID
// （ForwardSubmissionID 的同款 uuid5 模式）：同一动作重算恒等，执行重试不会
// 重复注入（ForwardToMember 的幂等键/ submissionID 全部由 RequestID 派生）。
func ActionRequestID(actionKey string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:action-forward:"+actionKey)).String()
}

// actionExecutionKey 是动作执行事实（携带 actionKey 的 task_planned）的幂等键。
func actionExecutionKey(teamID, runID, actionKey string) string {
	return fmt.Sprintf("team:%s:run:%s:action_exec:%s", teamID, runID, actionKey)
}

// actionRejectedPayload 构造 action_rejected 事件载荷：拒绝原因的唯一持久
// 落点是快照字段 Action.Status/Reason（run 级事件在前端 applyEvent 早退不留
// payload），事件供审计与 journal 重建。
func actionRejectedPayload(action *Action, reason string) map[string]any {
	payload := map[string]any{
		"taskId":    action.SourceTaskID,
		"type":      action.Type,
		"actionKey": action.ActionKey,
		"reason":    reason,
	}
	if index, ok := actionIndexFromKey(action.ActionKey); ok {
		payload["index"] = jsoncompat.Num(index)
	}
	return payload
}

// actionPrefetch 是执行判定的锁外预取输入（仿 resolveCandidateCwds 的分段
// 拆锁：慢 I/O 绝不进 state.mu）：目标成员会话可达性、团队级在途任务数与
// journal 动作事实。零值/nil 字段表示「未预取」——对应判定跳过，交给派发/
// 转发路径兜底。
type actionPrefetch struct {
	// sessionLive 记录目标成员（含 Lead）的会话可解析性；键不存在＝未预取
	// 或「归属明确但暂时打不开」（不判定会话失效，动作保持 proposed 由后续
	// 循环重试），键为 false＝会话不属于任何 ACP Agent（已删除/从未接入，
	// §5.2 拒绝项），键为 true＝会话可达。
	sessionLive map[string]bool
	// teamInFlight 是团队级在途任务数（§6.3 层 1 的 Team 级口径：全部 Run 的
	// dispatching/running 快照 + 派发池在途窗口）；<0 表示未统计。
	teamInFlight int
	// facts 是一次 journal Replay 提取的动作事实；nil 表示未提取。
	facts *actionJournalFacts
}

// countActionKey 统计快照里同 ActionKey 的动作条目数（不限状态：同键再次
// 出现即重复提案，§5.2「同一 Run 中产生相同的任务幂等键」）。
func countActionKey(run *Run, actionKey string) int {
	count := 0
	for i := range run.PendingActions {
		if run.PendingActions[i].ActionKey == actionKey {
			count++
		}
	}
	return count
}

// ValidateExecutable 是动作执行前的完整判定（§5.2 拒绝清单逐条，纯函数：
// 只读 run/team/policy 与锁外预取的 pre/memberBusy，不做任何 I/O）。返回
// (false, reason)＝拒绝（reason 是可解释中文原因，落 Action.Reason 与
// action_rejected 事件 payload）；(true, "")＝可执行。
//
// autoDispatch 执行闸门不在本函数（§5.1 规则 4/§5.2）：闸门关闭的
// delegate/request_context 停在 proposed（§11 动作预览的免执行形态），不是
// 拒绝；解析门（autoPlan）在 settleLeadTaskLocked，两道门分开（§10.2）。
func ValidateExecutable(run *Run, team *Team, policy TeamPolicy, action *Action, pre *actionPrefetch, memberBusy map[string]bool) (bool, string) {
	return ValidateExecutableWithDeps(run, team, policy, action, pre, memberBusy, nil)
}

// ValidateExecutableWithDeps 是 ValidateExecutable 的同批依赖版：resolvedDeps
// 是审查动作经 key 映射解析后的显式依赖（非审查动作忽略该参数）。
func ValidateExecutableWithDeps(run *Run, team *Team, policy TeamPolicy, action *Action, pre *actionPrefetch, memberBusy map[string]bool, resolvedDeps []string) (bool, string) {
	if run == nil || team == nil || action == nil {
		return false, "动作执行缺少运行上下文"
	}
	switch action.Type {
	case ActionTypeDelegate, ActionTypeRequestContext, ActionTypeSummarize:
	default:
		return false, "未知动作类型：" + action.Type
	}
	// 轮次（§5.2：超过 Team 的最大轮次。解析期已按当时的策略校验，策略收窄
	// 后在执行期复判）。
	if action.Round < 1 || action.Round > policy.MaxRounds {
		return false, fmt.Sprintf("动作轮次超出范围：允许 1–%d，实际 %d", policy.MaxRounds, action.Round)
	}
	// 来源任务：动作由其结算产生，执行时必须仍是本 Run 的已完成任务。
	if source := findTaskIn(run, action.SourceTaskID); source == nil {
		return false, fmt.Sprintf("动作引用了不存在的来源任务：%s", action.SourceTaskID)
	} else if source.Status != TaskCompleted {
		return false, fmt.Sprintf("动作来源任务尚未完成：%s", action.SourceTaskID)
	}
	// 同 Run 重复动作幂等键：快照同键条目 >1（任何状态）或 journal 里同键
	// action_proposed 事件 >1 都视为重复提案。
	if countActionKey(run, action.ActionKey) > 1 {
		return false, "同一 Run 中出现重复的动作幂等键：" + action.ActionKey
	}
	if pre != nil && pre.facts != nil && pre.facts.proposed[action.ActionKey] > 1 {
		return false, "同一 Run 中出现重复的动作幂等键：" + action.ActionKey
	}
	// 目标成员（delegate/request_context 用 member，summarize 用 Lead）：存在、
	// 启用、绑定会话可达（§5.2 前三条）。
	targetMember := action.Member
	if action.Type == ActionTypeSummarize {
		if team.LeadID == "" {
			return false, "团队未指定 Lead 成员，无法执行 summarize"
		}
		targetMember = team.LeadID
	}
	member := memberOf(team, targetMember)
	if member == nil {
		return false, fmt.Sprintf("动作引用了不存在的成员：%s", targetMember)
	}
	if !member.Enabled {
		return false, fmt.Sprintf("动作引用了已停用的成员：%s", targetMember)
	}
	if pre != nil && pre.sessionLive != nil {
		if live, resolved := pre.sessionLive[targetMember]; resolved && !live {
			return false, fmt.Sprintf("动作目标成员的绑定会话已失效：%s", targetMember)
		}
	}
	// 审查顺序屏障（docs/team-demand-queue-plan.md §8，阶段 2 起）：解析通过
	// 的动作可能延迟执行（崩溃恢复窗口、策略变更），执行期按当时快照复判，
	// 与解析期同一口径——审查动作必须显式依赖（resolvedDeps）覆盖全部未完成
	// 执行任务，否则等实现完成后单独委派。
	if delegateActionIsReview(team, action) && run.Planning == nil {
		var uncovered []string
		for i := range run.Tasks {
			task := &run.Tasks[i]
			if IsReviewTask(team, task) || task.Reduction || task.MemberID == team.LeadID {
				continue
			}
			switch task.Status {
			case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
				continue
			}
			if !containsString(resolvedDeps, task.ID) {
				uncovered = append(uncovered, task.ID)
			}
		}
		if len(uncovered) > 0 {
			return false, fmt.Sprintf("存在未被审查动作依赖覆盖的执行任务 %s：请补 dependsOn 或等实现任务全部完成后再委派审查",
				strings.Join(boundedViolationIDs(uncovered), "、"))
		}
	}
	// request_context：fromTaskId 必须指向本 Run 已完成且有可转发内容的任务
	// （ForwardToMember 会按同规则复验，这里提前给出可解释拒绝）。
	if action.Type == ActionTypeRequestContext {
		if action.FromTaskID == "" {
			return false, "request_context 动作缺少结果来源任务（fromTaskId）"
		}
		from := findTaskIn(run, action.FromTaskID)
		if from == nil {
			return false, fmt.Sprintf("动作引用了不存在的结果来源任务：%s", action.FromTaskID)
		}
		if from.Status != TaskCompleted {
			return false, fmt.Sprintf("动作引用的结果来源任务尚未完成：%s", action.FromTaskID)
		}
		content := from.Result
		if from.ResultDetail != nil && from.ResultDetail.Result != "" {
			content = from.ResultDetail.Result
		}
		if strings.TrimSpace(content) == "" {
			return false, fmt.Sprintf("结果来源任务没有可转发的内容：%s", action.FromTaskID)
		}
	}
	// 任务创建类（delegate/summarize）：内容上限、派生 ID、任务数、成员单
	// 活动任务与团队级并发（§5.2 后四条）。写锁不产生拒绝——动作声明的
	// writeLock 已在解析期校验，缺省按动作类型归一（buildActionTask：delegate
	// → exclusive、summarize → none），互斥由派发期的写锁机制执行。
	if action.Type == ActionTypeDelegate || action.Type == ActionTypeSummarize {
		prompt := ActionTaskPrompt(action)
		if runes := len([]rune(prompt)); runes > PromptLimit(policy.MaxContextChars) {
			return false, fmt.Sprintf("任务内容过长：%d 字，当前上限 %d 字", runes, PromptLimit(policy.MaxContextChars))
		}
		retriageTarget, retriage := failureRetriageTarget(run, action)
		if retriage {
			if retriageTarget == nil || retriageTarget.Status != TaskFailed {
				return false, "分诊改派的原任务已处理或不存在"
			}
		} else {
			derived := ActionDerivedTaskID(action)
			if derived == "" {
				return false, "动作幂等键缺少执行序号，无法派生任务 ID：" + action.ActionKey
			}
			if !ValidMemberID(derived) {
				return false, fmt.Sprintf("派生任务 ID 超长或含非法字符（字母/数字/下划线/连字符，1-64 字符）：%s", derived)
			}
			if findTaskIn(run, derived) != nil {
				return false, fmt.Sprintf("派生任务 ID 已存在：%s", derived)
			}
			if len(run.Tasks)+1 > policy.MaxTasksPerRun {
				return false, fmt.Sprintf("任务数已达上限 %d，无法创建派生任务", policy.MaxTasksPerRun)
			}
		}
		lease := targetMember
		if action.WorkflowID != "" {
			lease = actionLeaseKey(run, action)
		}
		if action.WorkflowID == "" && memberBusy != nil && memberBusy[lease] {
			return false, fmt.Sprintf("成员 %s 在本 Run 中已有运行中的任务", targetMember)
		}
		if action.WorkflowID == "" && pre != nil && pre.teamInFlight >= 0 && pre.teamInFlight >= policy.MaxParallelTasks {
			return false, fmt.Sprintf("团队并发已满（上限 %d 个在途任务），无法创建派生任务", policy.MaxParallelTasks)
		}
	}
	return true, ""
}

// buildActionTask 构造动作派生任务（确定性：同 run 快照 + 同动作恒等）：
//   - delegate：Member=action.Member、Prompt=action.Task；DependsOn 优先取
//     显式依赖——fromTaskId（schema 已校验为既有已完成任务）与 dependsOn
//     （同批 key 由 batchKeys 映射为派生任务 ID、其余为本 Run 既有任务，
//     批级校验已拒绝环与未解析引用，docs/team-demand-queue-plan.md §4 阶段2）；
//     无任何显式依赖时保留旧缺省——来源任务 + Lead 委派的「等全部在途」；
//   - summarize：Member=Lead、DependsOn=快照里全部已完成任务集——
//     BuildTaskContext 派发时自动注入依赖结果摘要（context.go），即「摘要
//     交回 Lead」的实现；
//   - WriteLock 取动作声明的 writeLock（schema 已校验 ∈ {exclusive, none}）；
//     缺省归一（§6.4 规则 3）：summarize 是纯读摘要 → none（与修复任务
//     WriteLock=none 同口径——exclusive 会让纯读摘要持有共享 cwd 的独占
//     写锁、阻塞同项目写者，且成员 cwd 解析不到时被 ReadyTasks 无声跳过），
//     delegate 缺省保守 exclusive（委派任务可能写，planner 创建期归一
//     同款）；WorkspaceMode 固定 shared（动作 schema 无 workspaceId，无法
//     引用预备 worktree）。
func buildActionTask(run *Run, team *Team, action *Action) Task {
	return buildActionTaskWithKeys(run, team, action, nil)
}

// buildActionTaskWithKeys 是 buildActionTask 的同批 key 解析版（执行流从
// passActions 传入本批 key → 派生任务 ID 映射）。
func buildActionTaskWithKeys(run *Run, team *Team, action *Action, batchKeys map[string]string) Task {
	var deps []string
	if action.Type == ActionTypeDelegate {
		explicit := false
		if len(action.DependsOn) > 0 {
			explicit = true
			for _, ref := range action.DependsOn {
				mapped := ref
				if derived, isKey := batchKeys[ref]; isKey {
					mapped = derived
				}
				if mapped != "" && !containsString(deps, mapped) {
					deps = append(deps, mapped)
				}
			}
		}
		// reviewOf（阶段 5）：解析后写入派生任务并并入依赖——被审实现完成
		// 才可派发审查。
		if len(action.ReviewOf) > 0 {
			explicit = true
			for _, ref := range action.ReviewOf {
				mapped := ref
				if derived, isKey := batchKeys[ref]; isKey {
					mapped = derived
				}
				if mapped != "" && !containsString(deps, mapped) {
					deps = append(deps, mapped)
				}
			}
		}
		if action.FromTaskID != "" {
			explicit = true
			if !containsString(deps, action.FromTaskID) {
				deps = append(deps, action.FromTaskID)
			}
		}
		if !explicit {
			deps = append(deps, action.SourceTaskID)
			if action.Member == team.LeadID && action.WorkflowID == "" {
				for i := range run.Tasks {
					status := run.Tasks[i].Status
					if run.Tasks[i].MemberID != team.LeadID && !run.Tasks[i].Reduction &&
						status != TaskFailed && status != TaskCancelled && status != TaskOutcomeUnknown && status != TaskStopped {
						deps = append(deps, run.Tasks[i].ID)
					}
				}
			}
		}
	}
	if action.Type == ActionTypeSummarize {
		for i := range run.Tasks {
			if run.Tasks[i].Status == TaskCompleted {
				deps = append(deps, run.Tasks[i].ID)
			}
		}
	}
	ordinal := 0
	for i := range run.Tasks {
		if run.Tasks[i].Ordinal > ordinal {
			ordinal = run.Tasks[i].Ordinal
		}
	}
	prompt := ActionTaskPrompt(action)
	memberID := action.Member
	if action.Type == ActionTypeSummarize {
		memberID = team.LeadID // summarize 的目标成员是团队 Lead（动作本身不携带 member）。
	}
	// 写锁归一（schema 校验后 writeLock ∈ {exclusive, none} 或空）：空值先按
	// 评审成员缺省只读（review.go），再按动作类型缺省——summarize 纯读 →
	// none，delegate 保守 → exclusive。
	writeLock := action.WriteLock
	if writeLock != TaskWriteLockExclusive && writeLock != TaskWriteLockNone {
		writeLock = defaultReviewWriteLock(team, memberID, "", action.Review)
	}
	if writeLock == "" {
		if action.Type == ActionTypeSummarize {
			writeLock = TaskWriteLockNone
		} else {
			writeLock = TaskWriteLockExclusive
		}
	}
	reviewOf := make([]string, 0, len(action.ReviewOf))
	for _, ref := range action.ReviewOf {
		if derived, isKey := batchKeys[ref]; isKey {
			reviewOf = append(reviewOf, derived)
		} else {
			reviewOf = append(reviewOf, ref)
		}
	}
	task := Task{
		ID:               ActionDerivedTaskID(action),
		MemberID:         memberID,
		Prompt:           prompt,
		Phase:            action.Phase,
		WorkflowID:       action.WorkflowID,
		WorkspaceID:      action.WorkspaceID,
		Attempt:          0,
		Ordinal:          ordinal + 1,
		DependsOn:        deps,
		ReviewOf:         reviewOf,
		WriteLock:        writeLock,
		Review:           action.Review,
		WorkspaceMode:    WorkspaceShared,
		Status:           TaskPlanned,
		InstructionsHead: InstructionsHead(prompt),
		Reduction:        action.Type == ActionTypeSummarize,
	}
	if workflow := planningWorkflow(run.Planning, action.WorkflowID); workflow != nil && workflow.WorkspaceID != "" {
		task.WorkspaceID = workflow.WorkspaceID
	}
	if task.WorkspaceID != "" {
		task.WorkspaceMode = WorkspaceIsolatedWorktree
	}
	if task.WorkflowID != "" {
		task.ExecutionThreadID = workflowThreadID(run.ID, memberID, task.WorkflowID)
	}
	if memberID == team.LeadID {
		task.ExecutionContext = ExecutionContextLeadRun
		if task.Reduction {
			task.ExecutionContext = ExecutionContextLeadControl
		}
	}
	// 创建期确定性字段（与 planner/修复任务同口径）：contextText 恒空串，
	// 派发期按真实注入上下文重算（accountDispatchLocked）。
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	return task
}

// hasRunnableProposedActions 报告 run 是否还有会被执行流处理的 proposed 动作
// （passFold 的拦截条件）：summarize 恒可执行；delegate/request_context 仅
// autoDispatch 开启时。闸门关闭的动作停在 proposed（§11 动作预览的免执行
// 形态），不构成 fold 阻塞——否则关闭 autoDispatch 的 Run 永远无法终局。
func hasRunnableProposedActions(run *Run, policy TeamPolicy) bool {
	if run == nil {
		return false
	}
	for i := range run.PendingActions {
		action := &run.PendingActions[i]
		if action.Status != ActionStatusProposed {
			continue
		}
		switch action.Type {
		case ActionTypeSummarize:
			return true
		case ActionTypeDelegate, ActionTypeRequestContext:
			if policy.AutoDispatch {
				return true
			}
		}
	}
	return false
}
