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
//	task_outcome_unknown / task_stopped: {taskId, stopReason}
//	task_input_hash_mismatch: {taskId, expected, actual}

import (
	"encoding/json"
	"fmt"
	"strconv"
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
// run 停进 waiting_input；resume 确认过的 stopped 不再触发。
type journalTerminal struct {
	status     string
	stopReason string
	failure    string
	needsUser  bool
	result     *TaskResult
}

// journalDispatch 是任务最近一次派发记录（task_dispatching 重建）。
type journalDispatch struct {
	sessionID    string
	submissionID string
	inputHash    string
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
//  4. 确定性 failed 不重试：failed 终态原样保留，永不回到 ready（§8.3 第 7 条）；
//  5. 快照终态无 journal 佐证 → outcome_unknown + run waiting_input（cancelled
//     例外：阶段一事件表没有 task_cancelled 事件，用户取消自证）；
//  6. 对账发现计划外任务（journal 引用了 Run 任务表之外的 taskId）或任何新
//     产生的 outcome_unknown → run 一律落 waiting_input（§9），不直接 fold 成
//     run outcome_unknown。
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
			record := dispatch[taskID]
			if record == nil {
				record = &journalDispatch{}
				dispatch[taskID] = record
			}
			record.sessionID = payloadString(event.Payload, "sessionId")
			record.submissionID = payloadString(event.Payload, "submissionId")
			if hash := payloadString(event.Payload, "inputHash"); hash != "" {
				if hashes[taskID] == nil {
					hashes[taskID] = map[string]bool{}
				}
				hashes[taskID][hash] = true
				record.inputHash = hash
			}
		case EventTaskCompleted:
			entry := &journalTerminal{status: TaskCompleted}
			result := &TaskResult{
				SessionID:  payloadString(event.Payload, "sessionId"),
				TurnID:     payloadString(event.Payload, "turnId"),
				Result:     payloadString(event.Payload, "result"),
				StopReason: payloadString(event.Payload, "stopReason"),
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
		case EventTaskFailed:
			terminal[taskID] = &journalTerminal{
				status:  TaskFailed,
				failure: payloadString(event.Payload, "error"),
			}
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
		EventTaskOutcomeUnknown, EventTaskStopped, EventTaskInputHashMismatch:
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
