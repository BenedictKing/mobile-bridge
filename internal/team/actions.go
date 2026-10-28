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
	// n∈1..3）；第 4 次解析失败落 final=true 的 invalid_action_block，结果按
	// 普通文本保留、动作拒绝。
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
	"type": true, "sourceTaskId": true, "round": true, "member": true,
	"task": true, "fromTaskId": true, "writeLock": true,
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
//     fromTaskId, writeLock}；type ∈ {delegate, request_context, summarize}；
//     sourceTaskId 必填且存在于本 Run；round 为 1..policy.MaxRounds 的整数；
//     writeLock ∈ {exclusive, none} 且仅 delegate/summarize 可携带（§6.4
//     规则 3：纯读任务声明 none 后可与写任务并行）；
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
		if key != "actions" {
			violations = append(violations, fmt.Sprintf("动作块对象包含无法识别的字段 %q（合法字段只有 actions）", key))
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
		action.FromTaskID, _ = entry.MustGet("fromTaskId").(string)
		action.WriteLock, _ = entry.MustGet("writeLock").(string)
		action.ActionKey = ActionKey(run.TeamID, run.ID, action.SourceTaskID, action.Round, index)
		actions = append(actions, action)
	}
	return actions, nil
}

// validateActionEntry 校验单条动作（报错带序号与实际值，仿 planner.go 的
// 逐条首错风格；这里收集全部错误供修复回路一次回送）。
func validateActionEntry(entry *jsoncompat.Object, team *Team, run *Run, policy TeamPolicy, promptLimit, index int) []string {
	var violations []string
	ordinal := index + 1
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
	return violations
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
	return "放在成对的 team-actions 开始与结束标记之间，标记之间是单个 JSON 对象，且整段回复只包含这一个动作块。对象形如 {\"actions\": [...]}；每条动作只使用这些字段：type（delegate / request_context / summarize）、sourceTaskId（本 Run 内已存在的任务 ID）、round（1 到最大轮次之间的整数）、member 与 task（delegate 和 request_context 必填：member 必须是团队里存在且启用的成员 ID，task 是具体任务内容）、fromTaskId（request_context 必填：指向本 Run 内已完成的任务）、writeLock（可选，仅 delegate/summarize：默认 exclusive，纯读任务可声明 none 与写任务并行）。不要输出上述之外的字段；若无需任何动作，请输出 {\"actions\":[]}。"
}

// BuildLeadBriefing 返回 autoPlan Lead 的任务级成员目录与动作协议。修复提示
// 已自带协议，普通成员和显式计划不注入；内容只依赖持久配置，保证确定性。
func BuildLeadBriefing(team *Team, run *Run, task *Task, policy TeamPolicy) string {
	if team == nil || run == nil || task == nil || !run.AutoPlan ||
		team.LeadID == "" || task.MemberID != team.LeadID || strings.HasPrefix(task.ID, actionRepairPrefix) {
		return ""
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
		if member.ID == team.LeadID {
			builder.WriteString(" · Lead")
		}
		builder.WriteString("\n")
	}
	builder.WriteString("需要编排时，")
	builder.WriteString(actionSchemaInstruction())
	builder.WriteString("\n")
	if policy.AutoDispatch {
		builder.WriteString("当前 autoDispatch=true：delegate 与 request_context 校验通过后可自动执行；summarize 由 Lead 汇总。\n")
	} else {
		builder.WriteString("当前 autoDispatch=false：delegate 与 request_context 只生成待确认预览，不会自动执行；summarize 仍可执行。\n")
	}
	builder.WriteString("如果需要等成员结果后由 Lead 汇总或继续规划，请在首批动作中同时给 Lead 自己一条 delegate 作为后续任务；不要在该批动作中直接混用 summarize。后续 Lead 任务取得结果后，再输出 summarize。\n")
	actionLimit := policy.MaxTasksPerRun
	if actionLimit > maxActionsPerBlock {
		actionLimit = maxActionsPerBlock
	}
	fmt.Fprintf(&builder, "当前限制：round 为 1–%d；整个 Run 最多 %d 条任务；单个动作块最多 %d 条动作；团队最多 %d 个在途任务。\n",
		policy.MaxRounds, policy.MaxTasksPerRun, actionLimit, policy.MaxParallelTasks)
	fmt.Fprintf(&builder, "单个动作对象示例（仅展示格式，不是要求立即创建任务；把 member_id 替换为上方真实 ID，并按上述说明加上动作信封）：\n{\"type\":\"delegate\",\"sourceTaskId\":%q,\"round\":1,\"member\":\"member_id\",\"task\":\"只读检查并汇报结果\",\"writeLock\":\"none\"}\n", task.ID)
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
	builder.WriteString("请重新输出修正后的动作块：" + actionSchemaInstruction())
	return builder.String()
}

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
		if memberBusy != nil && memberBusy[targetMember] {
			return false, fmt.Sprintf("成员 %s 在本 Run 中已有运行中的任务", targetMember)
		}
		if pre != nil && pre.teamInFlight >= 0 && pre.teamInFlight >= policy.MaxParallelTasks {
			return false, fmt.Sprintf("团队并发已满（上限 %d 个在途任务），无法创建派生任务", policy.MaxParallelTasks)
		}
	}
	return true, ""
}

// buildActionTask 构造动作派生任务（确定性：同 run 快照 + 同动作恒等）：
//   - delegate：Member=action.Member、Prompt=action.Task、DependsOn 仅含
//     fromTaskId（schema 已校验其为既有已完成任务——新任务不可能成环，
//     §12 工作项 6 的循环依赖拒绝由 planner 创建期 DFS 保证，动作派生不
//     需要新环检测）；
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
	var deps []string
	if action.Type == ActionTypeDelegate && action.FromTaskID != "" {
		deps = []string{action.FromTaskID}
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
	// 写锁归一（schema 校验后 writeLock ∈ {exclusive, none} 或空）：空值按
	// 动作类型缺省——summarize 纯读 → none，delegate 保守 → exclusive。
	writeLock := action.WriteLock
	if writeLock != TaskWriteLockExclusive && writeLock != TaskWriteLockNone {
		if action.Type == ActionTypeSummarize {
			writeLock = TaskWriteLockNone
		} else {
			writeLock = TaskWriteLockExclusive
		}
	}
	task := Task{
		ID:               ActionDerivedTaskID(action),
		MemberID:         memberID,
		Prompt:           prompt,
		Attempt:          0,
		Ordinal:          ordinal + 1,
		DependsOn:        deps,
		WriteLock:        writeLock,
		WorkspaceMode:    WorkspaceShared,
		Status:           TaskPlanned,
		InstructionsHead: InstructionsHead(prompt),
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
