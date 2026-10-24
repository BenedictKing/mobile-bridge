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
	Kind       string      // TransitionTaskStatus / TransitionRunStatus / TransitionDispatch
	TaskID     string      // 任务级转移的目标任务；Run 级与派发指令以外的转移为空
	Status     string      // 目标状态（任务级与 Run 级通用）
	StopReason string      // 终止原因（可选）
	Error      string      // 失败详情（可选）
	EventType  string      // 需随迁移落 journal 的事件类型（可选，见上）
	Result     *TaskResult // 重建 completed 终态时携带的有界结果（可选）
}

// ExtractResult 从 turn 抽取有界结果（§6.5 只存摘要与引用）。调用契约：只对
// turn.status=="completed" 的 turn 调用——失败信号不在 turn 顶层 error 字段
// （该字段初始化后恒为 nil），失败文本在 messages 里 kind=="error" 条目的
// text（internal/acpview/view.go FailTurn），本函数一律排除。
//
// 结果 = turn.messages 中 assistant 文本块（role=="assistant"）按序拼接，
// 排除 kind=="error" 及工具/过程类条目（role=="activity"：toolCall/
// agentThought/plan/subagent）。maxChars 为正时按 rune 截断并标 truncated；
// maxChars≤0 一律回退默认上限（DefaultPolicy().MaxResultChars）——策略层
// 已拒绝 0（httpd 校验 min=1），此处兜底历史账本里的非法 0 值，保证结果
// payload 恒有界（§10.3/§15.3）。ResultChars 记录截断前的完整字数（rune），
// 供前端显示"结果共 N 字"。
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
	full := strings.Join(texts, "\n")
	result.ResultChars = len([]rune(full))
	if maxChars > 0 && result.ResultChars > maxChars {
		result.Result = truncateRunes(full, maxChars)
		result.Truncated = true
	} else {
		result.Result = full
	}
	return result
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
// 任务终态时被调用。优先级：全 completed→completed；任一 failed→failed；
// 任一 cancelled→cancelled；任一 outcome_unknown→outcome_unknown；否则任一
// stopped→stopped。存在非终态任务时返回空状态（不可 fold）。
//
// salvage（§1.1 目标 9）：Run 终态绝不删除已完成结果——Run 详情始终携带
// 全部已完成任务的 TaskResult，本函数只做状态分类。outcome_unknown 分支是
// 防御性的：正常 runner 路径下未决 unknown 永远先把 run 停在 waiting_input
// （见 team-dispatch），不会带未决 unknown 来 fold；该分支仅服务恢复期对账
// 发现的异常终局。
func FoldRunStatus(tasks []Task) (status string, stopReason string) {
	ordered := sortTasksByOrdinal(tasks)
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
//   - 被 failed/cancelled/stopped/未决 unknown 依赖永久阻塞的 planned/ready
//     任务 → stopped（分支已死，不再执行；否则 run 永远等不到全终态）；
//   - 可继续派发的任务集：status ∈ {planned, ready} 且依赖（按迁移后状态）
//     全部 completed 的任务，逐条给出 dispatch 指令——依赖仍在飞（非终态）
//     的任务保持原状等待释放；
//   - 若无可派发且全终态：给出 fold 指令（TransitionRunStatus 携带按迁移后
//     任务列表计算的 FoldRunStatus 结果——因存在 stopped，通常归 run stopped）。
//
// 无可派发但仍有运行中任务时不给 fold：runner 等其落定后另行 fold。
func PlanResume(run *Run) ([]Transition, bool) {
	if run == nil || run.Status != RunWaitingInput {
		return nil, false
	}
	statuses := map[string]string{}
	stopReasons := map[string]string{}
	for i := range run.Tasks {
		statuses[run.Tasks[i].ID] = run.Tasks[i].Status
	}
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
	// 单遍按拓扑序分类候选任务：可派发（依赖全 completed）、被终态非完成
	// 依赖永久阻塞（立即标 stopped，级联传导给下游）、依赖仍在飞的保持原状。
	// 立即改写 statuses 让 a(stopped)←b←c 这类传递阻塞链一遍收敛。
	dispatchable := []string{}
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		switch statuses[task.ID] {
		case TaskPlanned, TaskReady:
		case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
			continue
		default:
			continue // dispatching/running：还在飞，resume 不碰。
		}
		released := true
		permanentlyBlocked := false
		for _, dep := range task.DependsOn {
			switch statuses[dep] {
			case TaskCompleted:
			case TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
				permanentlyBlocked = true
			default:
				released = false // 依赖未终局：可能随后释放。
			}
		}
		switch {
		case permanentlyBlocked:
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
			if status, stopReason := FoldRunStatus(after); status != "" {
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
