package team

// notify.go 是 §12 阶段四工作项 3/5 的 Run 通知侧（docs/team-orchestration-
// plan.md §6.3/§9）：完成/失败/取消/停止/waiting_input 五类 run 状态事件经
// Service.OnRunStatus 挂点交给装配方（main.go 从 store 补读 Run、组装标题
// 并写通知账本）；stall（§6.3「20 分钟无成功只通知不停止」）在调度循环的
// 推进段判定并落 run_stalled 表外事件。通知的发送、退避与重启补发（水位/
// 送达痕迹）在 internal/notifications/runledger.go——本文件只负责「何时该
// 通知」的事实判定与文案组装，绝不触碰通知基础设施。

import (
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
)

// EventRunStalled 是 §6.3 的 stall 事实（§10.3 事件表的表外增量，照
// EventRunStopped/EventQuestionWithdrawn 先例补登）：run 超过 stallAfter 无
// 任务成功完成时落账，payload {sinceMs, lastCompletedTaskId?}。每段停滞至多
// 一条（任务 completed 后计时重置、可再次触发）；绝不改 run 状态、绝不取消
// 任务——只通知，等待用户或恢复事件。
const EventRunStalled = "run_stalled"

// defaultStallAfter 是 §6.3 的 stall 判定线：20 分钟无成功只通知不停止。
const defaultStallAfter = 20 * time.Minute

// stallEnabled 报告 stall 检查是否开启（stallAfter≤0 关闭；默认
// defaultStallAfter，测试经 Service.stallAfter 注入缝调整）。
func (s *Service) stallEnabled() bool { return s.stallAfter > 0 }

// now 取当前时刻（可控 clock 注入缝：stall 计时与 lastSuccessAt 锚点都用它，
// 测试注入假时钟推进 20 分钟而不真睡，§13.1；生产恒 time.Now）。
func (s *Service) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// runStatusKind 把 run 状态事件类型映射为通知类别（notifications.RunNotify*
// 词汇）；非状态事件返回空串。
func runStatusKind(eventType string) string {
	switch eventType {
	case EventRunCompleted:
		return notifications.RunNotifyCompleted
	case EventRunFailed:
		return notifications.RunNotifyFailed
	case EventRunCancelled:
		return notifications.RunNotifyCancelled
	case EventRunStopped:
		return notifications.RunNotifyStopped
	case EventRunWaitingInput:
		return notifications.RunNotifyWaitingInput
	}
	return ""
}

// RunNotificationKindOfStatus 把 Run 快照状态映射为通知类别（启动补发扫描
// 用）：终态四类 + waiting_input。stall 不在扫描词汇里——它只在运行期通知，
// 重启补发会把历史 Run 无差别重发（轰炸），§6.3 的「等待用户或恢复事件」
// 也只对活循环有意义。非通知状态（created/planning/dispatching/collecting/
// reducing/outcome_unknown）返回空串。
func RunNotificationKindOfStatus(status string) string {
	return runStatusKind(runStatusEvent(status))
}

// notifyRunStatusEvents 在 run 状态事件成功落账（journal+快照，commitRun 的
// SaveRun 成功点）后逐事件回调 OnRunStatus。集中挂在 commitRun 而非各落账
// 点：waiting_input 的五处触发点（工作区丢失/账号停靠/结果未知/闸门未过/
// 未来的预算停靠——cost-stats 落 waiting_input 后走同一挂点）与全部终局
// （fold/Cancel/重启恢复对账）都经 commitRun 收口，新增路径自动覆盖；去重
// 双保险＝事件 first-wins 落账（每状态迁移只发一次事件）+ 账本按 run+kind
// 键去重（runledger.go Record）。
func (s *Service) notifyRunStatusEvents(events []*Event) {
	if s.OnRunStatus == nil {
		return
	}
	for _, event := range events {
		kind := runStatusKind(event.Type)
		if kind == "" {
			continue
		}
		s.OnRunStatus(event.RunID, kind)
	}
}

// noteTaskSuccessLocked 刷新 stall 计时锚点（任务 completed 落账后调用，
// 调用方持有 state.mu）：lastSuccessAt=now、记住最近完成任务、清 stalled
// 标记——新一段停滞可再次通知（§6.3「等待用户或恢复事件」：成功即恢复）。
func (s *Service) noteTaskSuccessLocked(state *runState, taskID string) {
	state.lastSuccessAt = s.now()
	state.lastCompletedTaskID = taskID
	state.stalled = false
}

// resetStallClockLocked 在 Resume 重启调度循环前重置 stall 计时（调用方持有
// state.mu）：waiting_input 驻留时长是用户治理时间，不计入「无成功」——
// 从 resume 时刻重新起算，绝不因停靠数小时立刻误报 stall。
func (s *Service) resetStallClockLocked(state *runState) {
	state.lastSuccessAt = s.now()
	state.stalled = false
}

// rebuildLastSuccessAt 在重启恢复时重建 stall 锚点：取 journal 里最近一条
// task_completed 的 createdAt。施工顺序依赖：newEvent 的 createdAt 盖章属
// cost-stats 工作项（本文件落地时尚未合入，事件 createdAt 恒为零值）——
// 查不到有效时间戳时回退当前时刻：宁可晚 20 分钟才报 stall，绝不把重启前
// 的历史成功误报成停滞。
func (s *Service) rebuildLastSuccessAt(state *runState, events []*Event) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.RunID != state.runID || event.Type != EventTaskCompleted || event.CreatedAt.IsZero() {
			continue
		}
		state.lastSuccessAt = event.CreatedAt
		state.lastCompletedTaskID, _ = event.Payload["taskId"].(string)
		return
	}
	state.lastSuccessAt = s.now()
}

// maybeStallLocked 是 §6.3 的 stall 检查（调用方持有 state.mu；dispatching
// 与 reducing 态的推进段每轮调用）。到线（now−lastSuccessAt>stallAfter）且
// 本段未报过 → 落 run_stalled 事件 + OnRunStatus 回调；绝不改 run 状态、
// 绝不取消任务。
//
// 盲区声明（有意设计，照 verify_failed 停靠豁免 runner.go expireRun 的注释
// 先例写明）：stall 只覆盖「有活调度循环」的 Run（dispatching/reducing 态，
// 即 passAdvance 末尾与 RunReducing 分支的调用点）。waiting_input 不参与
// 计时——停靠时刻已有独立的 waiting_input 通知，驻留时长是用户治理时间；
// paused（用户主动暂停，pause-checkpoint 语义）同理不计时。两类也绝不派
// reconciler 兜底补发 stall（防轰炸：停靠可长达数天，按停滞补发只会刷屏）。
func (s *Service) maybeStallLocked(state *runState, run *Run) {
	if !s.stallEnabled() || state.stalled || state.lastSuccessAt.IsZero() {
		return
	}
	since := s.now().Sub(state.lastSuccessAt)
	if since <= s.stallAfter {
		return
	}
	payload := map[string]any{"sinceMs": jsoncompat.Num(int(since.Milliseconds()))}
	if state.lastCompletedTaskID != "" {
		payload["lastCompletedTaskId"] = state.lastCompletedTaskID
	}
	event := newEvent(run.ID, EventRunStalled, "runner", payload)
	if err := s.journal.Append(event); err != nil {
		// 记账失败：不置 stalled，下一轮 pass 整套重试（绝不丢事实）。
		log.Printf("team run %s stall 事件记账失败（下轮重试）: %v", run.ID, err)
		return
	}
	state.stalled = true
	s.emit(event)
	if s.OnRunStatus != nil {
		s.OnRunStatus(run.ID, notifications.RunNotifyStall)
	}
}

// ---- 通知文案组装（纯函数；装配方在 OnRunStatus 回调里读 store 后调用）----

// notifyGoalRunes 是标题里 goal 的截断长度（评审定案：goal 截 60 rune）。
const notifyGoalRunes = 60

// notifyResultRunes 是完成通知正文里每条任务结果摘要的截断长度（§6.5
// salvage：完成通知含前 3 个任务结果，各截 200 rune）。
const notifyResultRunes = 200

// notifyResultTasks 是通知正文携带的任务结果条数上限（前 3 个已完成任务）。
const notifyResultTasks = 3

// RunNotificationText 组装 Run 通知的标题与正文。标题=类别前缀+goal 截 60
// rune；正文带 stopReason（如有）与任务计数；completed/failed/stopped 额外
// 带前 3 个已完成任务的结果摘要（§6.5 salvage：Run 失败/停止也必须保留已
// 完成结果，绝不因后续失败吞掉）。stall 的精确时长只在 journal 事件的
// payload 里，正文按默认判定线表述。kind 不在通知词汇内时返回空串。
func RunNotificationText(run *Run, kind string) (string, string) {
	if run == nil {
		return "", ""
	}
	goal := truncateRunes(strings.TrimSpace(run.Goal), notifyGoalRunes)
	switch kind {
	case notifications.RunNotifyCompleted:
		return notifyTitle("运行完成", goal), notifyBody(run, true)
	case notifications.RunNotifyFailed:
		return notifyTitle("运行失败", goal), notifyBody(run, true)
	case notifications.RunNotifyCancelled:
		return notifyTitle("运行已取消", goal), notifyBody(run, false)
	case notifications.RunNotifyStopped:
		return notifyTitle("运行已停止", goal), notifyBody(run, true)
	case notifications.RunNotifyWaitingInput:
		return notifyTitle("需要你的输入", goal), notifyBody(run, false)
	case notifications.RunNotifyStall:
		return notifyTitle("运行疑似停滞", goal),
			"已超过 20 分钟没有任务成功完成；运行未停止，继续等待任务推进。"
	}
	return "", ""
}

func notifyTitle(prefix, goal string) string {
	if goal == "" {
		return prefix
	}
	return prefix + "：" + goal
}

// notifyBody 组装正文：stopReason（如有）+ 任务计数行；withResults 时再附
// 前 3 个已完成任务的结果摘要（各截 200 rune，换行压成空格保持单行）。
func notifyBody(run *Run, withResults bool) string {
	lines := []string{}
	if run.StopReason != "" {
		lines = append(lines, run.StopReason)
	}
	lines = append(lines, notifyTaskCounts(run))
	if withResults {
		lines = append(lines, notifyResultLines(run)...)
	}
	return strings.Join(lines, "\n")
}

// notifyTaskCounts 输出任务计数行（固定序：完成/失败/停靠/取消/未知/在途，
// 只列非零类）。
func notifyTaskCounts(run *Run) string {
	counts := map[string]int{}
	inFlight := 0
	for i := range run.Tasks {
		switch run.Tasks[i].Status {
		case TaskCompleted, TaskFailed, TaskStopped, TaskCancelled, TaskOutcomeUnknown:
			counts[run.Tasks[i].Status]++
		default:
			inFlight++
		}
	}
	segments := []string{}
	for _, pair := range [...][2]string{
		{TaskCompleted, "完成"}, {TaskFailed, "失败"}, {TaskStopped, "停靠"},
		{TaskCancelled, "取消"}, {TaskOutcomeUnknown, "结果未知"},
	} {
		if counts[pair[0]] > 0 {
			segments = append(segments, pair[1]+" "+strconv.Itoa(counts[pair[0]]))
		}
	}
	if inFlight > 0 {
		segments = append(segments, "在途 "+strconv.Itoa(inFlight))
	}
	total := "任务 " + strconv.Itoa(len(run.Tasks)) + " 个"
	if len(segments) == 0 {
		return total
	}
	return total + "：" + strings.Join(segments, "、")
}

// notifyResultLines 输出前 3 个已完成任务的结果摘要行（快照序；结果截 200
// rune，截断加省略号标记，换行压成空格——通知正文单行可读）。
func notifyResultLines(run *Run) []string {
	lines := []string{}
	for i := range run.Tasks {
		if len(lines) >= notifyResultTasks {
			break
		}
		task := &run.Tasks[i]
		if task.Status != TaskCompleted {
			continue
		}
		result := strings.TrimSpace(task.Result)
		if result == "" {
			result = "（无文本结果）"
		}
		result = strings.ReplaceAll(result, "\n", " ")
		if truncated := truncateRunes(result, notifyResultRunes); truncated != result {
			result = truncated + "…"
		} else {
			result = truncated
		}
		lines = append(lines, "- "+task.ID+"："+result)
	}
	return lines
}
