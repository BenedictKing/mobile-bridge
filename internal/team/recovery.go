package team

// recovery.go 是网关重启对账的纯决策（§8.3、§8.2、§9）：对 dispatching/running
// 的任务经 Probe 查询 submissions 账本与会话存活性，按 journal 重放校验任务
// 终态；所有结论以 Transition 返回，真实 I/O（账本查询、journal 追加、账本
// 快照更新、watcher 重建）由 team-dispatch 执行（§3.1 原则 3）。
//
// journal 事件 payload 约定（dispatcher 写入侧必须遵守）：
//
//	task_dispatching: {taskId, sessionId, submissionId, inputHash, attempt}
//	task_completed:   {taskId, sessionId, turnId, result, resultChars, truncated, stopReason}
//	task_failed:      {taskId, error}
//	task_retry_scheduled: {taskId, attempt, outcome, memberId?, prompt?, writeLock?, actionKey?}
//	task_outcome_unknown / task_stopped: {taskId, stopReason}
//	task_input_hash_mismatch: {taskId, expected, actual}
//	task_cache_hit:   {taskId, parentRunId, parentTaskId, inputHash, sessionId, turnId, result, resultChars, truncated}

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// submissions 账本（internal/httpd/queue.go:33-40）的 submission 状态取值。
const (
	ledgerQueued    = "queued"
	ledgerUnknown   = "unknown"
	ledgerAccepted  = "accepted"
	ledgerFailed    = "failed"
	ledgerCancelled = "cancelled"
	ledgerIgnored   = "ignored"
)

// Probe 是对账所需的外部只读视图。实现方注意：LedgerStatus 的 ok=false 只应
// 表示"账本确无该条目"（确证未达账本，可安全回 ready 重发）；查询本身
// 失败时不得调 ReconcileRun——把它伪装成 ok=false 会导致重复发送。
type Probe interface {
	// LedgerStatus 返回 threadID+submissionID 在 submissions 账本中的状态
	// （queued/unknown/accepted/failed/cancelled/ignored）与错误文本；
	// ok=false 表示账本无该条目。
	LedgerStatus(threadID, submissionID string) (status, errorText string, ok bool)
	// SessionLive 返回 threadID 会话当前是否活着（决定 accepted 状态能否
	// 重建 watcher）。
	SessionLive(threadID string) bool
}

// journalTerminal 是按 journal 事件重建出的任务终态。needsUser 标记该终局
// 需要用户处理（未决 outcome_unknown 与 inputHash 漂移）：只有这类终局才把
// run 停进 waiting_input；resume 确认过的 stopped 不再触发。outcome/attempt
// 是 §6.3 错误策略表的分类与失败尝试号：rate_limited/transient 的 failed
// 与 quota/auth/not_configured 的 stopped 可被同任务更高 attempt 的
// task_dispatching 取代（受限重试/停靠重飞的在途事实优先于既有终态）。
type journalTerminal struct {
	status     string
	stopReason string
	failure    string
	outcome    string
	attempt    int
	needsUser  bool
	result     *TaskResult
}

// journalDispatch 是任务最近一次派发记录（task_dispatching 重建）。attempt
// 单调递增（重试/停靠重飞各 +1）；非单调的派发事件按 journal 乱序处理——
// 不更新记录、不触发终态取代。
type journalDispatch struct {
	sessionID    string
	submissionID string
	inputHash    string
	attempt      int
}

// ReconcileRun 是 §8.3 的对账决策（纯函数）：
//
//  1. 按 events 重放任务终态并与快照比对，差异以 journal 为准（§3.1 原则 1）；
//  2. 快照 dispatching/running（或有派发记录却停在 planned/ready 的崩溃窗口）
//     且 journal 无终态的任务查 Probe：
//     账本无条目→ready（provably 未发送）；queued→保持（交给既有队列 worker）；
//     accepted→会话活着转 running 重建 watcher、会话不在转 outcome_unknown；
//     unknown→outcome_unknown（禁止重发）；failed→failed；cancelled→cancelled；
//     ignored→outcome_unknown；
//  3. 派发期 inputHash 漂移（同一任务多个派发记录哈希不一致，或与快照
//     InputHash 不一致）→ task_input_hash_mismatch + run waiting_input（§8.2）；
//  4. 确定性 failed 不重试：failed 终态原样保留，永不回到 ready（§8.3
//     第 7 条）。例外（§6.3）：task_failed payload.outcome∈
//     {rate_limited,transient}（受限重试事实）且其后存在同任务更高
//     attempt 的 task_dispatching → 终态被该派发取代，按在途对账；
//     stopped(outcome∈{quota,auth,not_configured})（停靠重飞）同理；
//     task_failed 之后跟着 task_retry_scheduled（重臂迁移：回 ready@
//     attempt+1）→ 失败终态被重试事实取代，任务按 ready 交调度循环
//     重派，重试预算不因重启丢失；
//  5. 快照终态无 journal 佐证 → outcome_unknown + run waiting_input（cancelled
//     例外：阶段一事件表没有 task_cancelled 事件，用户取消自证）；
//  6. 对账发现计划外任务（journal 引用了 Run 任务表之外的 taskId）或任何新
//     产生的 outcome_unknown → run 一律落 waiting_input（§9），不直接 fold 成
//     run outcome_unknown。
//
// paused Run（§12 阶段四工作项 4）照常对账：任务按 journal 事实迁移（在途
// watcher 由 RecoverRun 的收养路径重建），run 本身保持 paused；仅当对账产生
// 需用户处理的事实（未决 unknown/哈希漂移/计划外任务）时升级 waiting_input
// ——语义严格强于 paused，升级后 Resume 走 PlanResume 既有路径。恢复续跑
// 不重发由账本对账（有条目不回 ready）+ submissionID 确定性幂等保证，paused
// 不是重发许可。
//
// 已终态的 Run（completed/failed/cancelled/stopped/outcome_unknown）无需对账，
// 返回 nil。
func ReconcileRun(run *Run, events []*Event, probe Probe) []Transition {
	if run == nil {
		return nil
	}
	switch run.Status {
	case RunCompleted, RunFailed, RunCancelled, RunStopped, RunOutcomeUnknown:
		return nil
	}
	if probe == nil {
		return nil
	}
	known := map[string]bool{}
	for i := range run.Tasks {
		known[run.Tasks[i].ID] = true
	}

	// 重放 journal：任务终态、最近派发记录与派发哈希集合。
	terminal := map[string]*journalTerminal{}
	dispatch := map[string]*journalDispatch{}
	retry := map[string]taskRetryAssignment{} // taskId → 重臂/改派事实（task_retry_scheduled）
	hashes := map[string]map[string]bool{}
	unknownTaskID := ""
	for _, event := range events {
		if event == nil || (event.RunID != "" && event.RunID != run.ID) {
			continue
		}
		taskID, _ := event.Payload["taskId"].(string)
		if !isTaskScopedEvent(event.Type) {
			continue
		}
		if taskID != "" && !known[taskID] && unknownTaskID == "" {
			unknownTaskID = taskID
		}
		if taskID == "" || !known[taskID] {
			continue
		}
		switch event.Type {
		case EventTaskDispatching:
			attempt := payloadInt(event.Payload, "attempt")
			record := dispatch[taskID]
			if record == nil {
				record = &journalDispatch{}
				dispatch[taskID] = record
			}
			if attempt < record.attempt {
				// attempts 必须单调（受限重试/停靠重飞各 +1）：回退的派发
				// 事件按 journal 乱序处理，不更新记录、不触发终态取代。
				break
			}
			record.attempt = attempt
			record.sessionID = payloadString(event.Payload, "sessionId")
			record.submissionID = payloadString(event.Payload, "submissionId")
			// 重臂后的新派发事实取代重试标记：任务按在途对账（下述终态
			// 取代逻辑同理）。
			delete(retry, taskID)
			if hash := payloadString(event.Payload, "inputHash"); hash != "" {
				if hashes[taskID] == nil {
					hashes[taskID] = map[string]bool{}
				}
				hashes[taskID][hash] = true
				record.inputHash = hash
			}
			// §6.3 重试/停靠重飞：更高 attempt 的派发取代既有可重派终态
			// （「确定性 failed 永不回 ready」的例外）——重试决策与重派
			// 事实都已在 journal，在途对账按最新派发进行。
			if entry, ok := terminal[taskID]; ok && retryableTerminal(entry) && attempt > entry.attempt {
				delete(terminal, taskID)
			}
		case EventTaskCompleted:
			entry := &journalTerminal{status: TaskCompleted}
			result := &TaskResult{
				SessionID:      payloadString(event.Payload, "sessionId"),
				TurnID:         payloadString(event.Payload, "turnId"),
				Result:         payloadString(event.Payload, "result"),
				StopReason:     payloadString(event.Payload, "stopReason"),
				FileSummary:    payloadString(event.Payload, "fileSummary"),
				CommandSummary: payloadString(event.Payload, "commandSummary"),
				ReviewVerdict:  payloadString(event.Payload, "reviewVerdict"),
			}
			if number, ok := event.Payload["resultChars"].(json.Number); ok {
				if chars, err := strconv.Atoi(number.String()); err == nil {
					result.ResultChars = chars
				}
			}
			result.Truncated, _ = event.Payload["truncated"].(bool)
			entry.result = result
			entry.stopReason = result.StopReason
			terminal[taskID] = entry
		case EventTaskCacheHit:
			// amend 缓存命中（§6.4.1，阶段三）：创建批次落账的 completed
			// 事实，按 task_completed 同款重建终态与结果引用——不补此 case
			// 的话，全缓存任务（快照 completed、journal 无 task_completed）
			// 会被规则 5 折成 outcome_unknown + run waiting_input。
			entry := &journalTerminal{status: TaskCompleted}
			result := &TaskResult{
				SessionID:   payloadString(event.Payload, "sessionId"),
				TurnID:      payloadString(event.Payload, "turnId"),
				Result:      payloadString(event.Payload, "result"),
				ResultChars: payloadInt(event.Payload, "resultChars"),
			}
			result.Truncated, _ = event.Payload["truncated"].(bool)
			entry.result = result
			terminal[taskID] = entry
		case EventTaskFailed:
			terminal[taskID] = &journalTerminal{
				status:  TaskFailed,
				failure: payloadString(event.Payload, "error"),
				outcome: payloadString(event.Payload, "outcome"),
				attempt: payloadInt(event.Payload, "attempt"),
			}
		case EventTaskRetryScheduled:
			// 受限重试的重臂迁移（§6.3）：task_failed 的「回 ready +
			// attempt+1」事实。它取代之前的失败终态（任务不是 failed，重试
			// 预算仍在），后续按快照 ready 交调度循环重派；更高 attempt 的
			// task_dispatching 到来时标记再被派发事实取代（上方 delete）。
			delete(terminal, taskID)
			retry[taskID] = retryAssignmentFromEvent(event)
		case EventTaskOutcomeUnknown:
			terminal[taskID] = &journalTerminal{
				status:     TaskOutcomeUnknown,
				stopReason: payloadString(event.Payload, "stopReason"),
				needsUser:  true,
			}
		case EventTaskStopped:
			terminal[taskID] = &journalTerminal{
				status:     TaskStopped,
				stopReason: payloadString(event.Payload, "stopReason"),
				outcome:    payloadString(event.Payload, "outcome"),
				attempt:    payloadInt(event.Payload, "attempt"),
			}
		case EventTaskInputHashMismatch:
			terminal[taskID] = &journalTerminal{
				status:     TaskStopped,
				stopReason: "输入哈希与派发记录不一致",
				needsUser:  true,
			}
		}
	}

	var transitions []Transition
	waitingReason := ""
	needsWaiting := func(reason string) {
		if waitingReason == "" {
			waitingReason = reason
		}
	}
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if entry, ok := terminal[task.ID]; ok {
			// journal 终态为准：与快照一致时不产生迁移（纯重建不重复记账）。
			if entry.status != task.Status {
				transitions = append(transitions, Transition{
					Kind:       TransitionTaskStatus,
					TaskID:     task.ID,
					Status:     entry.status,
					StopReason: entry.stopReason,
					Error:      entry.failure,
					Outcome:    entry.outcome,
					Result:     entry.result,
				})
			}
			if entry.needsUser {
				// 未决 unknown / 哈希漂移：run 必须停在等待用户；resume 已
				// 确认过的 stopped 不再触发。
				needsWaiting(fmt.Sprintf("任务 %s 待人工处理：%s", task.ID, entry.stopReason))
			}
			continue
		}
		if assignment, retried := retry[task.ID]; retried {
			// 重臂/改派事实未被新派发取代：不按旧 attempt 的派发记录探账本，
			// 按 journal 中的新成员、新说明与 attempt 恢复后交调度循环。
			if !taskRetryAssignmentApplied(&task, assignment) {
				transitions = append(transitions, Transition{
					Kind: TransitionTaskStatus, TaskID: task.ID, Status: TaskReady, Retry: &assignment,
				})
			}
			continue
		}
		switch task.Status {
		case TaskCompleted, TaskFailed, TaskOutcomeUnknown, TaskStopped:
			// 快照终态无 journal 佐证：账本快照不可信，按未决处理交人工对账。
			// cancelled 例外：阶段一事件表没有 task_cancelled 事件。
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskOutcomeUnknown,
				StopReason: "快照终态缺少 journal 佐证，需人工对账",
				EventType:  EventTaskOutcomeUnknown,
			})
			needsWaiting(fmt.Sprintf("任务 %s 结果未知，需人工对账", task.ID))
			continue
		}
		_, hasDispatch := dispatch[task.ID]
		inFlight := task.Status == TaskDispatching || task.Status == TaskRunning ||
			(hasDispatch && (task.Status == TaskPlanned || task.Status == TaskReady))
		if !inFlight {
			continue // 无派发记录的 planned/ready：留给调度循环。
		}

		// 派发期 inputHash 漂移检测（§8.2）：同一任务槽只接受一个输入哈希。
		if detectHashDrift(hashes[task.ID], task.InputHash, dispatch[task.ID]) {
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskStopped,
				StopReason: "输入哈希与派发记录不一致",
				EventType:  EventTaskInputHashMismatch,
			})
			needsWaiting(fmt.Sprintf("任务 %s 输入哈希与派发记录不一致，需修订计划", task.ID))
			continue
		}

		record := dispatch[task.ID]
		submissionID := task.SubmissionID
		if submissionID == "" && record != nil {
			submissionID = record.submissionID // journal 先记账、快照未保存的崩溃窗口。
		}
		threadID := ""
		if record != nil {
			threadID = record.sessionID
		}
		if submissionID == "" || threadID == "" {
			// 无法定位派发记录：不能证明未发送，绝不回 ready 重发。
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskOutcomeUnknown,
				StopReason: "无法定位派发记录，需人工对账",
				EventType:  EventTaskOutcomeUnknown,
			})
			needsWaiting(fmt.Sprintf("任务 %s 结果未知，需人工对账", task.ID))
			continue
		}
		status, errorText, ok := probe.LedgerStatus(threadID, submissionID)
		switch {
		case !ok:
			// 账本无该 submissionID：journal 先记账、enqueue 未落盘的崩溃窗口，
			// provably 未达发送路径，可安全回 ready。
			transitions = append(transitions, Transition{
				Kind:   TransitionTaskStatus,
				TaskID: task.ID,
				Status: TaskReady,
			})
		case status == ledgerQueued:
			// 保持现状，交给既有队列 worker 派发。
		case status == ledgerAccepted:
			if probe.SessionLive(threadID) {
				transitions = append(transitions, Transition{
					Kind:   TransitionTaskStatus,
					TaskID: task.ID,
					Status: TaskRunning,
				})
			} else {
				transitions = append(transitions, Transition{
					Kind:       TransitionTaskStatus,
					TaskID:     task.ID,
					Status:     TaskOutcomeUnknown,
					StopReason: "会话不可用，无法确认结果",
					EventType:  EventTaskOutcomeUnknown,
				})
				needsWaiting(fmt.Sprintf("任务 %s 结果未知，需人工对账", task.ID))
			}
		case status == ledgerFailed:
			transitions = append(transitions, Transition{
				Kind:      TransitionTaskStatus,
				TaskID:    task.ID,
				Status:    TaskFailed,
				Error:     firstNonEmpty(errorText, "队列派发失败"),
				EventType: EventTaskFailed,
			})
		case status == ledgerCancelled:
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskCancelled,
				StopReason: "队列消息已被取消",
			})
		default:
			// unknown / ignored / 未知取值：无法确认外部副作用，保持
			// outcome_unknown，等待用户处理（§8.3 第 6 条，禁止重发）。
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskOutcomeUnknown,
				StopReason: firstNonEmpty(errorText, "发送结果未知，需人工对账"),
				EventType:  EventTaskOutcomeUnknown,
			})
			needsWaiting(fmt.Sprintf("任务 %s 结果未知，需人工对账", task.ID))
		}
	}
	if unknownTaskID != "" {
		needsWaiting(fmt.Sprintf("对账发现计划外任务 %s，需人工确认", unknownTaskID))
	}
	if waitingReason != "" && run.Status != RunWaitingInput {
		transitions = append(transitions, Transition{
			Kind:       TransitionRunStatus,
			Status:     RunWaitingInput,
			StopReason: waitingReason,
			EventType:  EventRunWaitingInput,
		})
	}
	return transitions
}

// ReconcileUnknown 是 outcome_unknown 任务的只读重探决策（§8.3 第 6 条 +
// §12 阶段四工作项 2「运行中状态刷新」，纯函数）：仅遍历快照
// status=outcome_unknown 且携带派发凭据（SentText/SubmissionID）的任务，按
// Probe 查 submissions 账本做单向纠正——探到确定事实才迁移动作，绝不重发、
// 绝不改 run 状态。账本分支：
//
//	确凿 failed/cancelled（确定性拒绝，消息从未送达 Agent）→ 迁移
//	  outcome_unknown→failed/cancelled（事件随 RefreshRun 的专用 mutate 落账）；
//	仍 unknown / 无条目 → no-op（无条目只证明「现在查不到」，对已派发任务
//	  不构成「从未发送」的证明——Probe 契约见上，绝不回 ready）；
//	queued / accepted / ignored → no-op（queued 交既有队列 worker，accepted
//	  交 watcher 观察；ignored 与 ReconcileRun 的 default 分支同调保持未决）。
//
// team 用于解析任务的成员会话（账本按 threadID+submissionID 键控，快照 Task
// 不携带 sessionId）。成员已从团队移除或凭据缺失的任务跳过（无从对账，等
// 用户 resume）。会话侧 turn 发现（matchTurn）不进本决策：turn 缺席本是
// settleUnknown 的落账条件，事后出现归用户 resume 裁量。
//
// 与 ReconcileRun 的分工：ReconcileRun 只在启动对账时跑、按 journal 重放
// 裁决全部任务；本函数只看快照、只动 outcome_unknown 单一终态，供周期
// reconciler（RefreshRun）反复调用。
func ReconcileUnknown(run *Run, team *Team, probe Probe) []Transition {
	if run == nil || probe == nil {
		return nil
	}
	var transitions []Transition
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if task.Status != TaskOutcomeUnknown {
			continue
		}
		if task.SubmissionID == "" || task.SentText == "" {
			continue // 无派发凭据：无从探账本，等用户 resume。
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		status, errorText, ok := probe.LedgerStatus(member.SessionID, task.SubmissionID)
		switch {
		case !ok, status == ledgerUnknown, status == ledgerQueued,
			status == ledgerAccepted, status == ledgerIgnored:
			// 无条目/未决/仍在途：探不到确定事实，一律 no-op。
			continue
		case status == ledgerFailed:
			transitions = append(transitions, Transition{
				Kind:      TransitionTaskStatus,
				TaskID:    task.ID,
				Status:    TaskFailed,
				Error:     firstNonEmpty(errorText, "队列派发失败"),
				EventType: EventTaskFailed,
			})
		case status == ledgerCancelled:
			// 阶段一事件表没有 task_cancelled 事件：事件用 task_stopped
			//（用户取消把任务落 cancelled 的同款先例，runner Cancel），快照
			// 状态与 ReconcileRun 的 ledgerCancelled 分支同为 cancelled。
			transitions = append(transitions, Transition{
				Kind:       TransitionTaskStatus,
				TaskID:     task.ID,
				Status:     TaskCancelled,
				StopReason: "队列消息已被取消",
				EventType:  EventTaskStopped,
			})
		}
	}
	return transitions
}

// applyReconciledUnknown 把 ReconcileUnknown 的迁移应用到 fresh 快照
// （RefreshRun 的专用 mutate 段）：允许覆写 outcome_unknown 单一终态——
// first-wins 的显式例外（照「更高 attempt 派发事实取代可重派终态」的
// recovery.go 重放先例），其余任何状态一律不动（快照已被用户 resume 等先落
// 时本轮放弃）。事件沿用既有 task_failed/task_stopped 类型并携带
// reconciled:true（journal 重放 last-wins 天然兼容——重探事实落在
// task_outcome_unknown 之后，下次 ReconcileRun 重放以它为准，不新增事件
// 类型）。
func applyReconciledUnknown(run *Run, transitions []Transition, source string) []*Event {
	var events []*Event
	for _, transition := range transitions {
		task := findTaskIn(run, transition.TaskID)
		if task == nil || task.Status != TaskOutcomeUnknown {
			continue
		}
		task.Status = transition.Status
		task.StopReason = transition.StopReason
		task.Error = transition.Error
		payload := map[string]any{
			"taskId":     task.ID,
			"reconciled": true,
		}
		switch transition.EventType {
		case EventTaskFailed:
			payload["error"] = transition.Error
			if transition.StopReason != "" {
				payload["stopReason"] = transition.StopReason
			}
		default: // task_stopped（cancelled 迁移）
			payload["stopReason"] = transition.StopReason
		}
		if task.Attempt > 0 {
			// 失败/停靠事实携带 attempt（与 mutateTaskTerminal 的事件约定
			// 一致；reconciled 迁移不设 outcome，重放永不触发终态取代）。
			payload["attempt"] = jsoncompat.Num(task.Attempt)
		}
		events = append(events, newEvent(run.ID, transition.EventType, source, payload))
	}
	return events
}

// retryableTerminal 报告该 journal 终态是否可被同任务更高 attempt 的
// task_dispatching 取代（§6.3 错误策略表）：failed 的 rate_limited/
// transient（受限重试：结算递增 attempt 后重派）与 stopped 的 quota/
// auth/not_configured（停靠：resume 按 attempt+1 重飞）。其余终态
// （确定性 failed、超限 failed、无 outcome 的 stopped/cancelled 等）一律
// 保留，永不回 ready。
func retryableTerminal(entry *journalTerminal) bool {
	if entry == nil {
		return false
	}
	switch entry.status {
	case TaskFailed:
		return entry.outcome == OutcomeRateLimited || entry.outcome == OutcomeTransient
	case TaskStopped:
		return stopOutcomeFreshSubmission(entry.outcome)
	}
	return false
}

// detectHashDrift 报告任务槽的输入哈希是否漂移：journal 里出现多个不同
// inputHash，或最近一次派发哈希与账本快照记录的 InputHash 不一致。
// 快照哈希为空（dispatcher 尚未回写）时只比较 journal 内部一致性。
func detectHashDrift(hashes map[string]bool, snapshotHash string, record *journalDispatch) bool {
	if len(hashes) > 1 {
		return true
	}
	if record == nil || record.inputHash == "" || snapshotHash == "" {
		return false
	}
	return record.inputHash != snapshotHash
}

func isTaskScopedEvent(eventType string) bool {
	switch eventType {
	case EventTaskDispatching, EventTaskRunning, EventTaskCompleted, EventTaskFailed,
		EventTaskOutcomeUnknown, EventTaskStopped, EventTaskInputHashMismatch,
		EventTaskRetryScheduled, EventTaskCacheHit:
		return true
	}
	return false
}

func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	text, _ := payload[key].(string)
	return text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
