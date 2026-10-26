package team

// recovery 的单元测试（§13.1 team/recovery 行）：五类账本状态映射、无账本
// 条目回 ready、unknown 不重发且 run 落 waiting_input、inputHash 漂移、
// journal 优先于快照、计划外任务与终态 run 的防御分支。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// fakeProbe 是 Probe 的进程内假实现：账本状态与会话存活性由 map 直接给定。
type fakeProbe struct {
	ledger map[string]string // key: threadID + ":" + submissionID → 状态
	live   map[string]bool
}

func (p *fakeProbe) LedgerStatus(threadID, submissionID string) (string, string, bool) {
	status, ok := p.ledger[threadID+":"+submissionID]
	if !ok {
		return "", "", false
	}
	return status, "", true
}

func (p *fakeProbe) SessionLive(threadID string) bool { return p.live[threadID] }

func newFakeProbe() *fakeProbe {
	return &fakeProbe{ledger: map[string]string{}, live: map[string]bool{}}
}

func journalEvent(runID, eventType string, sequence uint64, payload map[string]any) *Event {
	return &Event{
		ID:       fmt.Sprintf("e-%d", sequence),
		RunID:    runID,
		Type:     eventType,
		Source:   "runner",
		Payload:  payload,
		Sequence: sequence,
	}
}

func dispatchEvent(runID, taskID, sessionID, submissionID, inputHash string, sequence uint64) *Event {
	return journalEvent(runID, EventTaskDispatching, sequence, map[string]any{
		"taskId": taskID, "sessionId": sessionID, "submissionId": submissionID,
		"inputHash": inputHash, "attempt": jsoncompat.Num(1),
	})
}

// inflightRun 构造一个运行中的 Run（对账对象），任务 InputHash 与 journal
// 派发哈希一致（不触发漂移分支）。
func inflightRun(tasks ...Task) *Run {
	for i := range tasks {
		tasks[i].Ordinal = i
	}
	return &Run{ID: "run-1", TeamID: "team-1", Status: RunDispatching, Tasks: tasks}
}

func findTransition(transitions []Transition, kind, taskID string) (Transition, bool) {
	for _, transition := range transitions {
		if transition.Kind == kind && transition.TaskID == taskID {
			return transition, true
		}
	}
	return Transition{}, false
}

func findRunTransition(transitions []Transition) (Transition, bool) {
	for _, transition := range transitions {
		if transition.Kind == TransitionRunStatus {
			return transition, true
		}
	}
	return Transition{}, false
}

func TestReconcileRunLedgerStatusMapping(t *testing.T) {
	prepare := func(ledger string, live bool) (*Run, *fakeProbe, []*Event) {
		run := inflightRun(Task{
			ID: "a", MemberID: "dev", Status: TaskDispatching,
			SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
		})
		events := []*Event{dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1)}
		probe := newFakeProbe()
		if ledger != "" {
			probe.ledger["s-dev:sub-1"] = ledger
		}
		probe.live["s-dev"] = live
		return run, probe, events
	}
	cases := []struct {
		ledger   string
		live     bool
		wantNone bool       // 期望零迁移（queued 保持）
		wantTask Transition // 期望的任务迁移（Kind/TaskID/Status/EventType）
		wantRun  bool       // 是否期望 run → waiting_input
	}{
		{ledger: "", live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskReady}},
		{ledger: ledgerQueued, live: true, wantNone: true},
		{ledger: ledgerAccepted, live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskRunning}},
		{ledger: ledgerAccepted, live: false, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskOutcomeUnknown, EventType: EventTaskOutcomeUnknown}, wantRun: true},
		{ledger: ledgerUnknown, live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskOutcomeUnknown, EventType: EventTaskOutcomeUnknown}, wantRun: true},
		{ledger: ledgerFailed, live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskFailed, EventType: EventTaskFailed}},
		{ledger: ledgerCancelled, live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskCancelled}},
		{ledger: ledgerIgnored, live: true, wantTask: Transition{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskOutcomeUnknown, EventType: EventTaskOutcomeUnknown}, wantRun: true},
	}
	for _, testCase := range cases {
		t.Run("账本="+testCase.ledger+" 存活="+fmt.Sprint(testCase.live), func(t *testing.T) {
			run, probe, events := prepare(testCase.ledger, testCase.live)
			transitions := ReconcileRun(run, events, probe)
			if testCase.wantNone {
				if len(transitions) != 0 {
					t.Fatalf("queued 应保持现状零迁移，实际 %v", transitions)
				}
				return
			}
			got, ok := findTransition(transitions, TransitionTaskStatus, "a")
			if !ok {
				t.Fatalf("应产出 a 的任务迁移，实际 %v", transitions)
			}
			if got.Status != testCase.wantTask.Status || got.EventType != testCase.wantTask.EventType {
				t.Fatalf("迁移应为 %+v，实际 %+v", testCase.wantTask, got)
			}
			_, runTransition := findRunTransition(transitions)
			if runTransition != testCase.wantRun {
				t.Fatalf("run waiting 迁移期望 %v，实际 %v（%+v）", testCase.wantRun, runTransition, transitions)
			}
		})
	}
}

func TestReconcileRunMissingLedgerEntryReturnsReady(t *testing.T) {
	// 崩溃窗口一：快照仍是 planned（journal 先记账、快照未保存）——只要有
	// 派发记录就必须对账，绝不能直接重新派发。
	run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskPlanned})
	events := []*Event{dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1)}
	probe := newFakeProbe() // 账本无条目。
	transitions := ReconcileRun(run, events, probe)
	ready, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || ready.Status != TaskReady {
		t.Fatalf("账本无条目应回 ready（provably 未发送），实际 %v", transitions)
	}
	if ready.EventType != "" {
		t.Fatalf("纯重建迁移不应重复记账，实际 %+v", ready)
	}
	if _, runTransition := findRunTransition(transitions); runTransition {
		t.Fatalf("回 ready 不需要 run 迁移，实际 %v", transitions)
	}
	// 崩溃窗口二：快照有 dispatching 但 SubmissionID 未回写——从 journal 派发
	// 记录取 submissionId 再查账本。
	run2 := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskDispatching, InputHash: "h1"})
	probe2 := newFakeProbe()
	probe2.ledger["s-dev:sub-1"] = ledgerFailed
	transitions = ReconcileRun(run2, events, probe2)
	failed, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || failed.Status != TaskFailed {
		t.Fatalf("应按 journal 的 submissionId 查账本并归 failed，实际 %v", transitions)
	}
}

func TestReconcileRunUnknownNeverRedispatches(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskRunning,
		SubmissionID: "sub-1", InputHash: "h1",
	})
	events := []*Event{
		dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1),
		journalEvent("run-1", EventTaskRunning, 2, map[string]any{"taskId": "a"}),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-1"] = ledgerUnknown
	transitions := ReconcileRun(run, events, probe)
	unknown, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || unknown.Status != TaskOutcomeUnknown {
		t.Fatalf("账本 unknown 应归 outcome_unknown，实际 %v", transitions)
	}
	if unknown.EventType != EventTaskOutcomeUnknown {
		t.Fatalf("新事实应携带 task_outcome_unknown 事件，实际 %+v", unknown)
	}
	runTransition, ok := findRunTransition(transitions)
	if !ok || runTransition.Status != RunWaitingInput {
		t.Fatalf("unknown 应把 run 落 waiting_input，实际 %v", transitions)
	}
	if !strings.Contains(runTransition.StopReason, "a") {
		t.Fatalf("run 停止原因应指向任务 a，实际 %q", runTransition.StopReason)
	}
	// 任何路径都不允许出现 ready（重发）。
	for _, transition := range transitions {
		if transition.TaskID == "a" && transition.Status == TaskReady {
			t.Fatal("outcome_unknown 任务绝不允许回 ready 重发")
		}
	}
}

func TestReconcileRunInputHashDrift(t *testing.T) {
	// 同一任务两次派发记录哈希不同 → 漂移。
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskRunning,
		SubmissionID: "sub-2", InputHash: "h2",
	})
	events := []*Event{
		dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1),
		dispatchEvent("run-1", "a", "s-dev", "sub-2", "h2", 2),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-2"] = ledgerAccepted
	probe.live["s-dev"] = true
	transitions := ReconcileRun(run, events, probe)
	stopped, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || stopped.Status != TaskStopped {
		t.Fatalf("哈希漂移应把任务停住，实际 %v", transitions)
	}
	if stopped.EventType != EventTaskInputHashMismatch {
		t.Fatalf("应携带 task_input_hash_mismatch 事件，实际 %+v", stopped)
	}
	if runTransition, ok := findRunTransition(transitions); !ok || runTransition.Status != RunWaitingInput {
		t.Fatalf("哈希漂移应把 run 落 waiting_input，实际 %v", transitions)
	}
	// journal 最近派发哈希与快照 InputHash 不一致 → 同样漂移。
	run2 := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching,
		SubmissionID: "sub-1", InputHash: "h-diverted",
	})
	events2 := []*Event{dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1)}
	probe2 := newFakeProbe()
	probe2.ledger["s-dev:sub-1"] = ledgerQueued
	transitions = ReconcileRun(run2, events2, probe2)
	if stopped, ok := findTransition(transitions, TransitionTaskStatus, "a"); !ok ||
		stopped.Status != TaskStopped || stopped.EventType != EventTaskInputHashMismatch {
		t.Fatalf("快照哈希与派发记录不一致应触发 mismatch，实际 %v", transitions)
	}
}

func TestReconcileRunJournalWinsOverSnapshot(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskRunning,
		SubmissionID: "sub-1", InputHash: "h1",
	})
	events := []*Event{
		dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1),
		journalEvent("run-1", EventTaskCompleted, 2, map[string]any{
			"taskId": "a", "sessionId": "s-dev", "turnId": "turn-7",
			"result": "发现两个错误处理问题。", "resultChars": jsoncompat.Num(11),
			"truncated": false, "stopReason": "end_turn",
		}),
	}
	transitions := ReconcileRun(run, events, newFakeProbe())
	completed, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || completed.Status != TaskCompleted {
		t.Fatalf("journal 终态应覆盖快照 running，实际 %v", transitions)
	}
	if completed.EventType != "" {
		t.Fatalf("journal 已有事实的重建迁移不应重复记账，实际 %+v", completed)
	}
	if completed.Result == nil {
		t.Fatalf("completed 重建应携带 journal 里的有界结果，实际 %+v", completed)
	}
	result := completed.Result
	if result.SessionID != "s-dev" || result.TurnID != "turn-7" ||
		result.Result != "发现两个错误处理问题。" || result.ResultChars != 11 || result.Truncated {
		t.Fatalf("结果字段应从 journal 事件恢复，实际 %+v", result)
	}
	if _, runTransition := findRunTransition(transitions); runTransition {
		t.Fatalf("completed 重建不需要 run 迁移，实际 %v", transitions)
	}
	// 快照已是同一终态时不产生迁移（幂等）。
	settled := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskCompleted,
		SubmissionID: "sub-1", InputHash: "h1",
	})
	if transitions := ReconcileRun(settled, events, newFakeProbe()); len(transitions) != 0 {
		t.Fatalf("快照与 journal 一致时应零迁移，实际 %v", transitions)
	}
}

func TestReconcileRunDefensiveBranches(t *testing.T) {
	t.Run("终态 run 无需对账", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskRunning, SubmissionID: "sub-1"})
		run.Status = RunCompleted
		if transitions := ReconcileRun(run, nil, newFakeProbe()); transitions != nil {
			t.Fatalf("终态 run 应返回 nil，实际 %v", transitions)
		}
		if transitions := ReconcileRun(nil, nil, newFakeProbe()); transitions != nil {
			t.Fatalf("nil run 应返回 nil，实际 %v", transitions)
		}
		if transitions := ReconcileRun(inflightRun(), nil, nil); transitions != nil {
			t.Fatalf("nil probe 应返回 nil，实际 %v", transitions)
		}
	})
	t.Run("快照终态无 journal 佐证归未知", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskCompleted})
		transitions := ReconcileRun(run, nil, newFakeProbe())
		unknown, ok := findTransition(transitions, TransitionTaskStatus, "a")
		if !ok || unknown.Status != TaskOutcomeUnknown {
			t.Fatalf("无佐证终态应归 outcome_unknown，实际 %v", transitions)
		}
		if runTransition, ok := findRunTransition(transitions); !ok || runTransition.Status != RunWaitingInput {
			t.Fatalf("无佐证终态应把 run 落 waiting_input，实际 %v", transitions)
		}
	})
	t.Run("cancelled 快照无需事件佐证", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskCancelled})
		if transitions := ReconcileRun(run, nil, newFakeProbe()); len(transitions) != 0 {
			t.Fatalf("cancelled（阶段一无事件类型）应保持现状，实际 %v", transitions)
		}
	})
	t.Run("计划外任务落 waiting_input", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskPlanned})
		events := []*Event{
			journalEvent("run-1", EventTaskCompleted, 1, map[string]any{"taskId": "ghost-task"}),
		}
		transitions := ReconcileRun(run, events, newFakeProbe())
		runTransition, ok := findRunTransition(transitions)
		if !ok || runTransition.Status != RunWaitingInput {
			t.Fatalf("计划外任务应把 run 落 waiting_input，实际 %v", transitions)
		}
		if !strings.Contains(runTransition.StopReason, "ghost-task") {
			t.Fatalf("停止原因应点名计划外任务，实际 %q", runTransition.StopReason)
		}
	})
	t.Run("resume 已确认的 stopped 不再触发 waiting", func(t *testing.T) {
		run := inflightRun(
			Task{ID: "a", MemberID: "dev", Status: TaskStopped, StopReason: "结果未知，用户已确认不再重发"},
			Task{ID: "b", MemberID: "rev", Status: TaskPlanned},
		)
		events := []*Event{
			journalEvent("run-1", EventTaskStopped, 1, map[string]any{
				"taskId": "a", "stopReason": "结果未知，用户已确认不再重发",
			}),
		}
		transitions := ReconcileRun(run, events, newFakeProbe())
		if _, runTransition := findRunTransition(transitions); runTransition {
			t.Fatalf("resume 已确认的 stopped 不应再把 run 落 waiting_input，实际 %v", transitions)
		}
	})
	t.Run("无派发记录的在途任务归未知", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskRunning})
		transitions := ReconcileRun(run, nil, newFakeProbe())
		unknown, ok := findTransition(transitions, TransitionTaskStatus, "a")
		if !ok || unknown.Status != TaskOutcomeUnknown {
			t.Fatalf("无 journal 派发记录的 running 任务不能回 ready，实际 %v", transitions)
		}
	})
	t.Run("确定性 failed 不重试", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskFailed, Error: "x"})
		events := []*Event{
			journalEvent("run-1", EventTaskFailed, 1, map[string]any{"taskId": "a", "error": "x"}),
		}
		transitions := ReconcileRun(run, events, newFakeProbe())
		if len(transitions) != 0 {
			t.Fatalf("快照与 journal 一致的 failed 应保持原状且绝不重试，实际 %v", transitions)
		}
	})
	t.Run("无派发记录的 planned 任务留给调度", func(t *testing.T) {
		run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskPlanned})
		if transitions := ReconcileRun(run, nil, newFakeProbe()); len(transitions) != 0 {
			t.Fatalf("无派发记录的 planned 任务不需对账，实际 %v", transitions)
		}
	})
}

// ---- §6.3 重试/停靠重飞的对账 ----

// dispatchEventAttempt 构造带指定 attempt 的 task_dispatching 事件。
func dispatchEventAttempt(runID, taskID, sessionID, submissionID, inputHash string, attempt, sequence uint64) *Event {
	return journalEvent(runID, EventTaskDispatching, sequence, map[string]any{
		"taskId": taskID, "sessionId": sessionID, "submissionId": submissionID,
		"inputHash": inputHash, "attempt": jsoncompat.Num(int(attempt)),
	})
}

// TestReconcileRunRetryFailureSupersededByLaterDispatch：受限重试中崩溃——
// journal 已有 task_failed(outcome=rate_limited, attempt=1) 与其后更高
// attempt 的 task_dispatching(2)。终态被在途派发取代：按 attempt 2 的账本
// 对账（accepted+存活 → running），不回 ready、不误判 inputHash 漂移。
func TestReconcileRunRetryFailureSupersededByLaterDispatch(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching, Attempt: 2,
		SubmissionID: "sub-2", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskFailed, 2, map[string]any{
			"taskId": "a", "error": "429 too many requests", "outcome": OutcomeRateLimited,
			"attempt": jsoncompat.Num(1),
		}),
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-2", "h1", 2, 3),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-2"] = ledgerAccepted
	probe.live["s-dev"] = true
	// 旧 attempt 的账本条目仍是 failed：取代后不应按它对账。
	probe.ledger["s-dev:sub-1"] = ledgerFailed

	transitions := ReconcileRun(run, events, probe)
	got, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || got.Status != TaskRunning {
		t.Fatalf("取代终态后应按 attempt 2 对账为 running，实际 %+v（全部 %+v）", got, transitions)
	}
	for _, transition := range transitions {
		if transition.Status == TaskStopped && transition.EventType == EventTaskInputHashMismatch {
			t.Fatalf("同内容重试不应误判 inputHash 漂移: %+v", transitions)
		}
		if transition.Status == TaskReady || transition.Status == TaskFailed {
			t.Fatalf("在途派发不应回 ready/failed: %+v", transitions)
		}
	}
}

// TestReconcileRunRetryFailureWithoutLaterDispatchStaysFailed：journal 终止于
// task_failed(outcome=rate_limited) 而 task_retry_scheduled 尚未落账——重臂
// 修复后这只剩 commitRun 两条 journal 追加之间的崩溃窗口（微秒级；重臂事件
// 落账后快照才会保存）——终态保留（确定性 failed 永不回 ready 的例外要求
// 其后存在更高 attempt 的派发或重臂事件），快照的 ready 被 journal 事实纠正
// 回 failed。
func TestReconcileRunRetryFailureWithoutLaterDispatchStaysFailed(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskReady, Attempt: 2,
		SubmissionID: "", InputHash: "", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskFailed, 2, map[string]any{
			"taskId": "a", "error": "429 too many requests", "outcome": OutcomeRateLimited,
			"attempt": jsoncompat.Num(1),
		}),
	}
	transitions := ReconcileRun(run, events, newFakeProbe())
	got, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || got.Status != TaskFailed {
		t.Fatalf("无后续派发的重试终态应保留 failed: %+v（全部 %+v）", got, transitions)
	}
	if got.Outcome != OutcomeRateLimited {
		t.Fatalf("终态迁移应透传 outcome: %+v", got)
	}
}

// forbidProbe 把任何 Probe 调用变成测试失败（断言「不按旧派发记录对账」）。
type forbidProbe struct{ t *testing.T }

func (p forbidProbe) LedgerStatus(threadID, submissionID string) (string, string, bool) {
	p.t.Fatalf("重臂任务不应探查旧派发记录的账本: %s/%s", threadID, submissionID)
	return "", "", false
}

func (p forbidProbe) SessionLive(threadID string) bool {
	p.t.Fatalf("重臂任务不应探查旧派发会话: %s", threadID)
	return false
}

// TestReconcileRunRetryScheduledKeepsReady：受限重试的重臂事实已落 journal
// （task_failed 后跟 task_retry_scheduled，§6.3）——重试冷却/等待片中重启
// 时任务保持 ready：失败终态被重臂事件取代，且不按旧 attempt 的派发记录探
// 账本（旧派发结论已随 task_failed 落账，账本状态与重派无关），重试预算
// 不因重启丢失、交调度循环按快照 attempt 重派。
func TestReconcileRunRetryScheduledKeepsReady(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskReady, Attempt: 2,
		SubmissionID: "", InputHash: "", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskFailed, 2, map[string]any{
			"taskId": "a", "error": "429 too many requests", "outcome": OutcomeRateLimited,
			"attempt": jsoncompat.Num(1),
		}),
		journalEvent("run-1", EventTaskRetryScheduled, 3, map[string]any{
			"taskId": "a", "attempt": jsoncompat.Num(2), "outcome": OutcomeRateLimited,
		}),
	}
	if transitions := ReconcileRun(run, events, forbidProbe{t}); len(transitions) != 0 {
		t.Fatalf("重臂后任务已是 ready：应零迁移交调度循环重派，实际 %+v", transitions)
	}
}

// TestReconcileRunRetryScheduledRescuesCrashWindow：重臂事件已落 journal、
// 快照保存未达（commitRun 的崩溃窗口）——快照仍 dispatching@attempt 1 时，
// 对账按重臂事实把任务拉回 ready（不折 failed、不按旧派发对账）。
func TestReconcileRunRetryScheduledRescuesCrashWindow(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching, Attempt: 1,
		SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskFailed, 2, map[string]any{
			"taskId": "a", "error": "request timeout", "outcome": OutcomeTransient,
			"attempt": jsoncompat.Num(1),
		}),
		journalEvent("run-1", EventTaskRetryScheduled, 3, map[string]any{
			"taskId": "a", "attempt": jsoncompat.Num(2), "outcome": OutcomeTransient,
		}),
	}
	transitions := ReconcileRun(run, events, forbidProbe{t})
	got, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || got.Status != TaskReady {
		t.Fatalf("崩溃窗口的快照应被重臂事实拉回 ready: %+v（全部 %+v）", got, transitions)
	}
	for _, transition := range transitions {
		if transition.Status == TaskFailed {
			t.Fatalf("重臂事实存在时不得折 failed: %+v", transitions)
		}
	}
}

// TestReconcileRunQuotaStoppedStaysWaiting：quota 停靠（stopped+outcome）的
// run 重启后驻留 waiting_input——终态与快照一致零迁移，不因 outcome 重入
// waiting/failed。
func TestReconcileRunQuotaStoppedStaysWaiting(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskStopped, Attempt: 1, Outcome: OutcomeQuota,
		SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
		StopReason: "账号额度或余额不足，任务已停靠",
	})
	run.Status = RunWaitingInput
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskStopped, 2, map[string]any{
			"taskId": "a", "stopReason": "账号额度或余额不足，任务已停靠",
			"outcome": OutcomeQuota, "attempt": jsoncompat.Num(1),
		}),
	}
	if transitions := ReconcileRun(run, events, newFakeProbe()); len(transitions) != 0 {
		t.Fatalf("停靠 run 重启应零迁移驻留 waiting_input，实际 %+v", transitions)
	}
}

// TestReconcileRunQuotaStoppedSupersededByRedispatch：停靠任务 resume 重飞
// （dispatching attempt 2）后崩溃——stopped(outcome=quota) 终态被更高
// attempt 派发取代，按 attempt 2 在途对账。
func TestReconcileRunQuotaStoppedSupersededByRedispatch(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching, Attempt: 2,
		SubmissionID: "sub-2", InputHash: "h1", Outcome: "", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskStopped, 2, map[string]any{
			"taskId": "a", "stopReason": "账号额度或余额不足，任务已停靠",
			"outcome": OutcomeQuota, "attempt": jsoncompat.Num(1),
		}),
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-2", "h1", 2, 3),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-2"] = ledgerQueued
	transitions := ReconcileRun(run, events, probe)
	if got, ok := findTransition(transitions, TransitionTaskStatus, "a"); ok {
		t.Fatalf("queued 应保持现状零迁移（终态已被取代），实际 %+v", got)
	}
	if len(transitions) != 0 {
		t.Fatalf("停靠重飞后按 attempt 2 对账应零迁移，实际 %+v", transitions)
	}
}

// TestReconcileRunNonRetryableFailureNotSuperseded：确定性 failed（无
// outcome 或非重试 outcome）即使其后出现同任务派发也不被取代（防御分支：
// 正常流程不会对 failed 任务再派发）。
func TestReconcileRunNonRetryableFailureNotSuperseded(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching, Attempt: 2,
		SubmissionID: "sub-2", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 1),
		journalEvent("run-1", EventTaskFailed, 2, map[string]any{
			"taskId": "a", "error": "模型拒绝执行",
		}),
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-2", "h1", 2, 3),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-2"] = ledgerAccepted
	probe.live["s-dev"] = true
	transitions := ReconcileRun(run, events, probe)
	got, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || got.Status != TaskFailed {
		t.Fatalf("确定性 failed 不应被派发取代: %+v（全部 %+v）", got, transitions)
	}
}

// TestReconcileRunAttemptMonotonicViolationIgnored：attempt 回退的派发事件
// 按 journal 乱序处理——不更新最近派发记录（对账仍按最高 attempt 的
// submission）。
func TestReconcileRunAttemptMonotonicViolationIgnored(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching, Attempt: 2,
		SubmissionID: "sub-2", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 2, 1),
		// 乱序回退的 attempt 1：不得把对账目标拉回 sub-1。
		dispatchEventAttempt("run-1", "a", "s-dev", "sub-1", "h1", 1, 2),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-1"] = ledgerFailed
	transitions := ReconcileRun(run, events, probe)
	for _, transition := range transitions {
		if transition.Status == TaskFailed {
			t.Fatalf("乱序回退的派发不应触发按旧 attempt 的失败对账: %+v", transitions)
		}
	}
}

// ---- 评审遗留④：工作区停靠的单批原子判别 ----

// TestStopTaskWorkspaceLostSingleBatchAtomic 判别（评审遗留④）：
// stopTaskWorkspaceLost 的「workspace_lost + 任务 stopped + run
// waiting_input」必须单批原子落账（旧实现先落任务事实、另批落 run 状态，
// failAt=3 注入点会留下任务已 stopped 而 run 仍 dispatching 的撕裂快照，
// passFold 会把全终态快照折成 RunStopped、堵死 Resume）。写锁释放已随单批
// 后移，锁存续与任务在途状态一致。
func TestStopTaskWorkspaceLostSingleBatchAtomic(t *testing.T) {
	for _, failAt := range []int{2, 3} {
		t.Run(fmt.Sprintf("SaveRun第%d次失败", failAt), func(t *testing.T) {
			store, service, team := newSettleService(t, failAt)
			run := &Run{
				ID: "run-wslost", TeamID: team.ID, Goal: "交付功能", Status: RunDispatching, Round: 1,
				Tasks: []Task{{
					ID: "t1", MemberID: "dev", Prompt: "实现功能", Ordinal: 0,
					Status: TaskDispatching, SubmissionID: "sub-t1",
					WriteLock: TaskWriteLockExclusive, WorkspaceMode: WorkspaceIsolatedWorktree,
					WorkspaceID: "ws-t1",
				}},
			}
			if err := store.SaveRun(run); err != nil {
				t.Fatalf("run 夹具落账失败: %v", err)
			}
			state := service.stateFor(run.ID)
			service.stopTaskWorkspaceLost(state, run.ID, "t1", "ws-t1", "path_missing")
			assertDockAtomic(t, store, run.ID, "t1")
			if store.Run(run.ID).Status != RunWaitingInput {
				if ok := service.stopTaskWorkspaceLost(state, run.ID, "t1", "ws-t1", "path_missing"); !ok {
					t.Fatal("重试应落齐全部事实")
				}
				assertDockAtomic(t, store, run.ID, "t1")
			}
			fresh := store.Run(run.ID)
			if task := findTaskIn(fresh, "t1"); task.Status != TaskStopped {
				t.Fatalf("任务应停靠 stopped，实际 %s", task.Status)
			}
			// 事件序：workspace_lost → task_stopped → run_waiting_input（§3.1 原则 4）。
			store.disarm() // 停靠事实已验：解除注入，Resume 不受影响。
			if !eventOrderOK(service, run.ID, EventWorkspaceLost, EventTaskStopped, EventRunWaitingInput) {
				t.Fatal("事件序应为 workspace_lost → task_stopped → run_waiting_input")
			}
			// Resume 仍可达（用户恢复 worktree 后经 resume 继续）。
			resumed, err := service.Resume(run.ID)
			if err != nil {
				t.Fatalf("Resume 应可达: %v", err)
			}
			if resumed.Status == RunWaitingInput {
				t.Fatal("Resume 后应离开 waiting_input")
			}
		})
	}
}

// ---- §12 阶段四工作项 2：unknown 只读重探（ReconcileUnknown/RefreshRun）----
//
// 重探决策只读外部状态（submissions 账本）、探到确定事实才单向纠正；收养
// 与重探共用 RefreshRun 入口，三条铁律（不改 run.Status / 不回 ready /
// 不 startLoop）由下方用例逐条判别。

// failingProbe 返回固定状态与错误文本（fakeProbe 不携带错误文本，重探的
// 错误透传判定专用）。
type failingProbe struct {
	status  string
	errText string
}

func (p failingProbe) LedgerStatus(string, string) (string, string, bool) {
	return p.status, p.errText, true
}

func (p failingProbe) SessionLive(string) bool { return true }

// TestReconcileUnknownLedgerBranches 三分支表驱动：确凿 failed/cancelled 才
// 单向纠正；unknown/无条目/queued/accepted/ignored 一律 no-op（无条目只证明
// 「现在查不到」，对已派发任务不构成「从未发送」的证明——Probe 契约）。
func TestReconcileUnknownLedgerBranches(t *testing.T) {
	team := testTeam()
	newTask := func(id string) Task {
		return Task{
			ID: id, MemberID: "dev", Status: TaskOutcomeUnknown, Ordinal: 0,
			SubmissionID: "sub-" + id, SentText: "派发全文-" + id, Attempt: 1,
		}
	}
	cases := []struct {
		name       string
		ledger     string // 空串=账本无条目
		wantStatus string // 空=no-op
		wantEvent  string
	}{
		{"确凿 failed 单向纠正", ledgerFailed, TaskFailed, EventTaskFailed},
		{"确凿 cancelled 单向纠正", ledgerCancelled, TaskCancelled, EventTaskStopped},
		{"仍 unknown 不动", ledgerUnknown, "", ""},
		{"账本无条目不动且绝不 ready", "", "", ""},
		{"queued 不动", ledgerQueued, "", ""},
		{"accepted 不动", ledgerAccepted, "", ""},
		{"ignored 不动（与 ReconcileRun default 分支同调）", ledgerIgnored, "", ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			run := &Run{ID: "run-1", TeamID: team.ID, Status: RunWaitingInput, Tasks: []Task{newTask("a")}}
			probe := newFakeProbe()
			if testCase.ledger != "" {
				probe.ledger["s-dev:sub-a"] = testCase.ledger
			}
			transitions := ReconcileUnknown(run, team, probe)
			if testCase.wantStatus == "" {
				if len(transitions) != 0 {
					t.Fatalf("no-op 分支应零迁移，实际 %v", transitions)
				}
				return
			}
			got, ok := findTransition(transitions, TransitionTaskStatus, "a")
			if !ok || got.Status != testCase.wantStatus {
				t.Fatalf("应单向纠正到 %s，实际 %v", testCase.wantStatus, transitions)
			}
			if got.EventType != testCase.wantEvent {
				t.Fatalf("事件类型应为 %s，实际 %q", testCase.wantEvent, got.EventType)
			}
			if got.Status == TaskReady {
				t.Fatal("outcome_unknown 任务绝不回 ready 重发")
			}
		})
	}
	// 错误文本透传：账本 error 优先，缺省回退队列派发失败文案。
	run := &Run{ID: "run-1", TeamID: team.ID, Status: RunWaitingInput, Tasks: []Task{newTask("a")}}
	if got := ReconcileUnknown(run, team, failingProbe{status: ledgerFailed, errText: "队列连接失败"}); len(got) != 1 ||
		got[0].Error != "队列连接失败" {
		t.Fatalf("failed 纠正应透转账本错误文本，实际 %v", got)
	}
	if got := ReconcileUnknown(run, team, failingProbe{status: ledgerFailed}); len(got) != 1 ||
		got[0].Error != "队列派发失败" {
		t.Fatalf("缺省错误文本应为队列派发失败，实际 %v", got)
	}
	// 防御分支与凭据门槛。
	t.Run("无派发凭据的任务跳过", func(t *testing.T) {
		run := &Run{ID: "run-1", TeamID: team.ID, Status: RunWaitingInput, Tasks: []Task{
			{ID: "nosub", MemberID: "dev", Status: TaskOutcomeUnknown, SentText: "文本", Ordinal: 0},
			{ID: "notext", MemberID: "dev", Status: TaskOutcomeUnknown, SubmissionID: "sub-notext", Ordinal: 1},
		}}
		probe := newFakeProbe()
		probe.ledger["s-dev:sub-notext"] = ledgerFailed
		if transitions := ReconcileUnknown(run, team, probe); len(transitions) != 0 {
			t.Fatalf("缺 SentText/SubmissionID 的任务无从对账，实际 %v", transitions)
		}
	})
	t.Run("成员已移除与在途任务不碰", func(t *testing.T) {
		run := &Run{ID: "run-1", TeamID: team.ID, Status: RunWaitingInput, Tasks: []Task{
			{ID: "orphan", MemberID: "no-such-member", Status: TaskOutcomeUnknown, SubmissionID: "sub-orphan", SentText: "文本", Ordinal: 0},
			{ID: "fly", MemberID: "dev", Status: TaskRunning, SubmissionID: "sub-fly", SentText: "文本", Ordinal: 1},
		}}
		probe := newFakeProbe()
		probe.ledger["s-dev:sub-fly"] = ledgerFailed
		if transitions := ReconcileUnknown(run, team, probe); len(transitions) != 0 {
			t.Fatalf("只读重探只动 outcome_unknown，实际 %v", transitions)
		}
	})
	t.Run("nil 入参防御", func(t *testing.T) {
		if ReconcileUnknown(nil, testTeam(), newFakeProbe()) != nil ||
			ReconcileUnknown(&Run{ID: "r"}, nil, nil) != nil {
			t.Fatal("nil run/probe 应返回 nil")
		}
	})
}

// TestApplyReconciledUnknownMutateSemantics 专用 mutate 的覆写边界：允许且仅
// 允许覆写 outcome_unknown 单一终态（first-wins 的显式例外）；其余状态
// （含已被并发 resume 演进的终态）一律不动；事件带 reconciled:true 与
// attempt，幂等重放零事件。
func TestApplyReconciledUnknownMutateSemantics(t *testing.T) {
	run := &Run{ID: "run-1", TeamID: "team-1", Status: RunWaitingInput, Tasks: []Task{
		{ID: "a", MemberID: "dev", Status: TaskOutcomeUnknown, Attempt: 2, Ordinal: 0},
		{ID: "b", MemberID: "dev", Status: TaskFailed, Ordinal: 1},
	}}
	transitions := []Transition{
		{Kind: TransitionTaskStatus, TaskID: "a", Status: TaskFailed, Error: "队列连接失败", EventType: EventTaskFailed},
		{Kind: TransitionTaskStatus, TaskID: "b", Status: TaskCancelled, StopReason: "队列消息已被取消", EventType: EventTaskStopped},
	}
	events := applyReconciledUnknown(run, transitions, "runner")
	if run.Tasks[0].Status != TaskFailed || run.Tasks[0].Error != "队列连接失败" {
		t.Fatalf("outcome_unknown 应被单向纠正为 failed: %+v", run.Tasks[0])
	}
	if run.Tasks[1].Status != TaskFailed {
		t.Fatalf("非 outcome_unknown 终态不得覆写（first-wins 例外仅限 unknown）: %+v", run.Tasks[1])
	}
	if len(events) != 1 {
		t.Fatalf("仅 a 的事件应落账，实际 %v", events)
	}
	if events[0].Type != EventTaskFailed || events[0].Payload["reconciled"] != true {
		t.Fatalf("事件应为 task_failed 且带 reconciled:true，实际 %+v", events[0])
	}
	if attempt, ok := events[0].Payload["attempt"].(json.Number); !ok || attempt.String() != "2" {
		t.Fatalf("事件应携带 attempt（与 mutateTaskTerminal 事件约定一致），实际 %v", events[0].Payload)
	}
	// 幂等：任务已离开 outcome_unknown，同一批迁移再跑零事件。
	if again := applyReconciledUnknown(run, transitions, "runner"); len(again) != 0 {
		t.Fatalf("重探迁移应幂等，实际 %v", again)
	}
}

// seedWaitingRun 落一个 waiting_input 的 Run 与对应 journal 事实（不经
// CreateRun——收养/重探路径的直达夹具，照 seedInflightRun 的记账形态）：
// run_created / task_planned / 每个有 SubmissionID 的任务一条 task_dispatching
// / outcome_unknown 任务补 task_outcome_unknown / stopped(outcome) 任务补
// task_stopped。seedQueue 由调用方预置账本条目（fakeQueue 的 setStatus 只改
// 既有条目）。
func seedWaitingRun(t *testing.T, h *dispatchHarness, runID string, tasks []Task, seedQueue func(h *dispatchHarness)) *Run {
	t.Helper()
	for i := range tasks {
		tasks[i].Ordinal = i
		if tasks[i].InputHash == "" {
			tasks[i].InputHash = "h-" + tasks[i].ID
		}
	}
	run := &Run{
		ID: runID, TeamID: h.team.ID, Goal: "交付功能", Status: RunWaitingInput,
		StopReason: "任务停靠，等待用户处理", Round: 1, Tasks: tasks,
	}
	mustAppend := func(event *Event) {
		t.Helper()
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	mustAppend(newEvent(runID, EventRunCreated, "user", map[string]any{"mode": ModePipeline, "taskCount": jsoncompat.Num(len(tasks))}))
	for i := range tasks {
		task := &tasks[i]
		mustAppend(newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": task.ID, "memberId": task.MemberID}))
		if task.SubmissionID == "" {
			continue
		}
		mustAppend(newEvent(runID, EventTaskDispatching, "runner", map[string]any{
			"taskId": task.ID, "sessionId": "s-" + task.MemberID, "submissionId": task.SubmissionID,
			"inputHash": task.InputHash, "attempt": jsoncompat.Num(firstAttempt),
		}))
		switch task.Status {
		case TaskOutcomeUnknown:
			mustAppend(newEvent(runID, EventTaskOutcomeUnknown, "watcher", map[string]any{
				"taskId": task.ID, "stopReason": "发送结果未知，需人工对账",
			}))
		case TaskStopped:
			payload := map[string]any{"taskId": task.ID, "stopReason": task.StopReason}
			if task.Outcome != "" {
				payload["outcome"] = task.Outcome
			}
			mustAppend(newEvent(runID, EventTaskStopped, "watcher", payload))
		}
	}
	if seedQueue != nil {
		seedQueue(h)
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 夹具落账失败: %v", err)
	}
	return run
}

// submissionOf 按 team/run/task/attempt 派生确定性 submissionID（与派发路径
// 同源，fakeQueue 的 UUID 校验可过）。
func submissionOf(h *dispatchHarness, runID, taskID string) string {
	return TaskSubmissionID(h.team.ID, runID, taskID, firstAttempt)
}

// loopStateOf 读取 run 的循环状态句柄（deadline/代际号的铁律断言用）。
func loopStateOf(h *dispatchHarness, runID string) (deadlineIsZero bool, generation uint64) {
	state := h.service.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.deadline.IsZero(), state.loopGeneration
}

// TestRefreshRunReconcilesUnknownWithoutResend RefreshRun 的重探主路径：
// 账本确凿 failed → 单向纠正（事件带 reconciled:true、序在 task_outcome_unknown
// 之后）；run 状态铁律（仍 waiting_input、全终态也不 fold）；绝不重发
// （Enqueue 零新增）；幂等；终局只由 Resume 触发。
func TestRefreshRunReconcilesUnknownWithoutResend(t *testing.T) {
	h := newDispatchHarness(t)
	sub := submissionOf(h, "run-refresh", "unk")
	run := seedWaitingRun(t, h, "run-refresh", []Task{{
		ID: "unk", MemberID: "dev", Status: TaskOutcomeUnknown,
		SubmissionID: sub, SentText: "派发全文", Attempt: 1,
	}}, func(h *dispatchHarness) {
		if _, _, err := h.queue.Enqueue("s-dev", sub, "派发全文"); err != nil {
			t.Fatalf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-dev", sub, SubStatusFailed, "队列连接失败")
	})
	if err := h.service.RefreshRun(run.ID); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	task := h.taskOf(run.ID, "unk")
	if task.Status != TaskFailed || task.Error != "队列连接失败" {
		t.Fatalf("outcome_unknown 应被单向纠正为 failed，实际 %+v", task)
	}
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunWaitingInput {
		t.Fatalf("刷新绝不改 run.Status（全终态也驻留等 resume），实际 %s", fresh.Status)
	}
	// 事件：task_failed 带 reconciled:true，序在 task_outcome_unknown 之后。
	if !eventOrderOK(h.service, run.ID, EventTaskOutcomeUnknown, EventTaskFailed) {
		t.Fatal("事件序应为 task_outcome_unknown → task_failed(reconciled)")
	}
	reconciled := 0
	for _, event := range h.eventsOfType(run.ID, EventTaskFailed) {
		if event.Payload["reconciled"] == true {
			reconciled++
		}
	}
	if reconciled != 1 {
		t.Fatalf("应恰好一条 reconciled task_failed 事件，实际 %d", reconciled)
	}
	// 绝不重发：账本 Enqueue 仍是夹具那一次。
	if got := h.queue.callCount(sub); got != 1 {
		t.Fatalf("重探不得重发：Enqueue 应只 1 次，实际 %d", got)
	}
	// 幂等：再刷零新事件、状态稳定。
	if err := h.service.RefreshRun(run.ID); err != nil {
		t.Fatalf("二次刷新失败: %v", err)
	}
	if got := len(h.eventsOfType(run.ID, EventTaskFailed)); got != 1 {
		t.Fatalf("幂等刷新应零新事件，实际 task_failed 共 %d 条", got)
	}
	if h.taskOf(run.ID, "unk").Status != TaskFailed {
		t.Fatal("幂等刷新不得改写已纠正终态")
	}
	// 跨重启稳定：journal 的 task_failed(reconciled) last-wins，重启对账
	// 不把已纠正终态弹回 outcome_unknown。
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("重启对账失败: %v", err)
	}
	if task := h.taskOf(run.ID, "unk"); task.Status != TaskFailed {
		t.Fatalf("重启对账不得回弹已纠正终态，实际 %s", task.Status)
	}
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("重启对账后 run 应仍驻留 waiting_input，实际 %s", fresh.Status)
	}
	// 终局只由用户 Resume 触发：fold 落 RunFailed（首败任务定罪）。
	resumed, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("Resume 应可达: %v", err)
	}
	if resumed.Status != RunFailed {
		t.Fatalf("Resume 后应 fold 出 RunFailed，实际 %s", resumed.Status)
	}
}

// TestRefreshRunCancelledUnknownAcrossRestart 账本确凿 cancelled 的纠正与
// 跨重启归一：纠正落 TaskCancelled + task_stopped(reconciled)（阶段一事件表
// 没有 task_cancelled，照用户取消先例）；重启对账按 journal last-wins 把快照
// 一次性归一为 stopped（ReconcileRun 的 task_stopped 重放），waiting_input
// 驻留与 Resume 语义不受影响。
func TestRefreshRunCancelledUnknownAcrossRestart(t *testing.T) {
	h := newDispatchHarness(t)
	sub := submissionOf(h, "run-cancel", "unk")
	run := seedWaitingRun(t, h, "run-cancel", []Task{{
		ID: "unk", MemberID: "dev", Status: TaskOutcomeUnknown,
		SubmissionID: sub, SentText: "派发全文", Attempt: 1,
	}}, func(h *dispatchHarness) {
		if _, _, err := h.queue.Enqueue("s-dev", sub, "派发全文"); err != nil {
			t.Fatalf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-dev", sub, SubStatusCancelled, "")
	})
	if err := h.service.RefreshRun(run.ID); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	if task := h.taskOf(run.ID, "unk"); task.Status != TaskCancelled {
		t.Fatalf("确凿 cancelled 应纠正为 cancelled，实际 %s", task.Status)
	}
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("纠正后 run 应仍 waiting_input，实际 %s", fresh.Status)
	}
	if !eventOrderOK(h.service, run.ID, EventTaskOutcomeUnknown, EventTaskStopped) {
		t.Fatal("事件序应为 task_outcome_unknown → task_stopped(reconciled)")
	}
	// 重启对账：journal 佐证（task_stopped）last-wins，快照归一 stopped。
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("重启对账失败: %v", err)
	}
	if task := h.taskOf(run.ID, "unk"); task.Status != TaskStopped {
		t.Fatalf("重启重放应把 cancelled 归一为 stopped（journal 佐证），实际 %s", task.Status)
	}
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("重启对账后 run 应仍驻留 waiting_input，实际 %s", fresh.Status)
	}
	// Resume 收口：无 outcome 的 stopped 不重飞，fold 落 RunStopped。
	resumed, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("Resume 应可达: %v", err)
	}
	if resumed.Status != RunStopped {
		t.Fatalf("Resume 后应 fold 出 RunStopped，实际 %s", resumed.Status)
	}
}

// TestRefreshRunAdoptsWaitingInputInflight 收养解耦主判定：waiting_input Run
// 的在途任务经 RefreshRun/RecoverRun 重新有人观察（watcher 存活），在途收尾
// 入账照常发生，而 run 驻留 waiting_input、循环不启动（deadline/代际号不动）。
func TestRefreshRunAdoptsWaitingInputInflight(t *testing.T) {
	h := newDispatchHarness(t)
	sub := submissionOf(h, "run-adopt", "fly")
	sentText := ComposeTaskMessage("", "实现接口")
	run := seedWaitingRun(t, h, "run-adopt", []Task{
		{ID: "fly", MemberID: "dev", Status: TaskDispatching, SubmissionID: sub, SentText: sentText, Attempt: 1, WriteLock: TaskWriteLockNone},
		{ID: "unk", MemberID: "rev", Status: TaskOutcomeUnknown, SubmissionID: "sub-unk", SentText: "另一任务", Attempt: 1},
	}, func(h *dispatchHarness) {
		if _, _, err := h.queue.Enqueue("s-dev", sub, sentText); err != nil {
			t.Fatalf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-dev", sub, SubStatusAccepted, "")
		// 在途 turn 进行中：收养后 watcher 应观察到 inProgress 并驻留观察。
		h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnInProgress(sentText)))
	})
	if err := h.service.RefreshRun(run.ID); err != nil {
		t.Fatalf("刷新失败: %v", err)
	}
	// 收养断言：fly 的 watcher 存活（watcher 本就独立于调度循环）。
	exit := h.grabWatcherExit(t, run.ID, "fly")
	// 铁律：run 状态不动、不 startLoop（deadline 零值、代际号 0）。
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("刷新后 run 应仍 waiting_input，实际 %s", fresh.Status)
	}
	if zero, generation := loopStateOf(h, run.ID); !zero || generation != 0 {
		t.Fatal("刷新不得启动调度循环（deadline 不重臂、代际号不动）")
	}
	// 在途收尾入账：turn 落 completed → 任务 completed（无循环也照常结算），
	// run 仍驻留 waiting_input（fold 只由 Resume 触发）。
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnCompleted(sentText, "接口已实现。")))
	waitFor(t, "收养 watcher 收尾入账", func() bool {
		return h.taskOf(run.ID, "fly").Status == TaskCompleted
	})
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("在途收尾不得改变 run 状态（驻留等 resume），实际 %s", fresh.Status)
	}
	waitForClosed(t, "watcher 退出", exit)
}

// TestRecoverRunAdoptsWaitingInputInflight 重启路径的收养解耦：waiting_input
// Run 恢复后不启动调度循环（deadline 不重臂），但在途任务重新有人观察且带
// 超时兜底——账本 queued、无人推进时 watcher 按任务超时收口 failed，run 仍
// 驻留 waiting_input 等 resume。
func TestRecoverRunAdoptsWaitingInputInflight(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.TaskTimeoutSecs = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	sub := submissionOf(h, "run-rec-wait", "fly")
	run := seedWaitingRun(t, h, "run-rec-wait", []Task{
		{ID: "fly", MemberID: "dev", Status: TaskDispatching, SubmissionID: sub, SentText: "实现接口", Attempt: 1, WriteLock: TaskWriteLockNone},
		{ID: "unk", MemberID: "rev", Status: TaskOutcomeUnknown, SubmissionID: "sub-unk", SentText: "另一任务", Attempt: 1},
	}, func(h *dispatchHarness) {
		// 账本 queued：无人派发（fakeQueue 无 worker），watcher 只能等任务
		// 超时兜底——这正是收养要补的能力。
		if _, _, err := h.queue.Enqueue("s-dev", sub, "实现接口"); err != nil {
			t.Fatalf("预置账本条目失败: %v", err)
		}
	})
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 恢复收养：watcher 存活，但循环绝不启动。
	h.grabWatcherExit(t, run.ID, "fly")
	if zero, generation := loopStateOf(h, run.ID); !zero || generation != 0 {
		t.Fatal("waiting_input 恢复不得启动调度循环（deadline 不重臂、代际号不动）")
	}
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("恢复后 run 应仍 waiting_input，实际 %s", fresh.Status)
	}
	// 超时兜底：收养的 watcher 按任务超时收口（1s 实时钟，dispatcher_test
	// 的 TaskTimeoutSecs=1 先例），run 仍驻留。
	waitFor(t, "收养任务超时收口", func() bool {
		return h.taskOf(run.ID, "fly").Status == TaskFailed
	})
	task := h.taskOf(run.ID, "fly")
	if task.StopReason != "task_timeout" {
		t.Fatalf("超时收口应记 task_timeout，实际 %q", task.StopReason)
	}
	if fresh := h.store.Run(run.ID); fresh.Status != RunWaitingInput {
		t.Fatalf("任务收口后 run 仍应驻留 waiting_input（fold 只由 Resume），实际 %s", fresh.Status)
	}
	// 仍在队列的条目随超时撤销（cancelSideEffects）。
	if entry, ok := h.queue.firstEntry("s-dev"); !ok || entry.status != SubStatusCancelled {
		t.Fatalf("queued 条目应随超时撤销: %+v", entry)
	}
	// Resume 收口：unk 落 stopped（用户确认不再重发）、fly 已 failed → fold。
	resumed, err := h.service.Resume(run.ID)
	if err != nil {
		t.Fatalf("Resume 应可达: %v", err)
	}
	if resumed.Status != RunFailed {
		t.Fatalf("Resume 后应 fold 出 RunFailed，实际 %s", resumed.Status)
	}
	if task := h.taskOf(run.ID, "unk"); task.Status != TaskStopped {
		t.Fatalf("unknown 应由 Resume 落 stopped，实际 %s", task.Status)
	}
}

// TestRefreshRunKeepsDocksWaitingInput 两条停靠 Run 的刷新铁律：verify_failed
// 停靠与 quota 停靠刷新后仍 waiting_input、闸门结果与停靠事实原样保留，
// Resume 仍可达（复验/重飞各自的恢复语义）。
func TestRefreshRunKeepsDocksWaitingInput(t *testing.T) {
	t.Run("verify_failed 停靠", func(t *testing.T) {
		h, _ := newVerifyHarness(t)
		t.Setenv("VERIFY_EXIT", "7")
		run, err := h.service.CreateRun(h.team, "", "产出实现", ModeSingle, "", []string{"make", "check"}, nil, false, "")
		if err != nil {
			t.Fatalf("创建 run 失败: %v", err)
		}
		completeVerifyRunTask(t, h, run)
		waitFor(t, "闸门未过停靠", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
		docked := h.store.Run(run.ID)
		if err := h.service.RefreshRun(run.ID); err != nil {
			t.Fatalf("刷新失败: %v", err)
		}
		fresh := h.store.Run(run.ID)
		if fresh.Status != RunWaitingInput || fresh.StopReason != docked.StopReason {
			t.Fatalf("刷新不得改写停靠状态: 应保持 %q，实际 %s/%q", docked.StopReason, fresh.Status, fresh.StopReason)
		}
		if fresh.Verify == nil || fresh.Verify.ExitCode != 7 {
			t.Fatalf("刷新不得清除闸门结果（复验只由 Resume 触发）: %+v", fresh.Verify)
		}
		// Resume 复验闭环仍可达。
		t.Setenv("VERIFY_EXIT", "")
		if _, err := h.service.Resume(run.ID); err != nil {
			t.Fatalf("Resume 复验应可达: %v", err)
		}
		waitFor(t, "复验通过 run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	})
	t.Run("quota 停靠", func(t *testing.T) {
		h := newDispatchHarness(t)
		run := seedWaitingRun(t, h, "run-quota", []Task{{
			ID: "q", MemberID: "dev", Status: TaskStopped, Outcome: OutcomeQuota,
			StopReason: "账号额度或余额不足，任务已停靠", Attempt: 1, WriteLock: TaskWriteLockNone,
		}}, nil)
		if err := h.service.RefreshRun(run.ID); err != nil {
			t.Fatalf("刷新失败: %v", err)
		}
		fresh := h.store.Run(run.ID)
		if fresh.Status != RunWaitingInput {
			t.Fatalf("停靠 Run 刷新后应仍 waiting_input，实际 %s", fresh.Status)
		}
		task := h.taskOf(run.ID, "q")
		if task.Status != TaskStopped || task.Outcome != OutcomeQuota {
			t.Fatalf("刷新不得改写停靠任务（不回 ready、不改 outcome）: %+v", task)
		}
		if zero, generation := loopStateOf(h, run.ID); !zero || generation != 0 {
			t.Fatal("刷新不得启动调度循环")
		}
		// Resume 重飞仍可达：stopped(quota) 重臂 ready → run 回 dispatching。
		resumed, err := h.service.Resume(run.ID)
		if err != nil {
			t.Fatalf("Resume 重飞应可达: %v", err)
		}
		if resumed.Status == RunWaitingInput {
			t.Fatal("Resume 后应离开 waiting_input")
		}
	})
}

// TestReconcilerLoopRefreshesAndWaitStops 周期循环：每 tick 对 waiting_input
// Run 依次 RefreshRun（无循环驻留的 Run 被 reconciler 兜底）；Stop 同步等待
// 循环退出（done 在 Stop 返回前关闭）、幂等、可重启。
func TestReconcilerLoopRefreshesAndWaitStops(t *testing.T) {
	h := newDispatchHarness(t)
	seed := func(runID string) *Run {
		sub := submissionOf(h, runID, "unk")
		return seedWaitingRun(t, h, runID, []Task{{
			ID: "unk", MemberID: "dev", Status: TaskOutcomeUnknown,
			SubmissionID: sub, SentText: "派发全文", Attempt: 1,
		}}, func(h *dispatchHarness) {
			if _, _, err := h.queue.Enqueue("s-dev", sub, "派发全文"); err != nil {
				t.Fatalf("预置账本条目失败: %v", err)
			}
			h.queue.setStatus("s-dev", sub, SubStatusFailed, "队列连接失败")
		})
	}
	run := seed("run-loop-1")
	if zero, _ := loopStateOf(h, run.ID); !zero {
		t.Fatal("前置：无循环驻留（waiting_input 不 startLoop）")
	}
	h.service.StartReconciler(2 * time.Millisecond)
	waitFor(t, "周期重探纠正 unknown", func() bool {
		return h.taskOf(run.ID, "unk").Status == TaskFailed
	})
	// Stop 同步退出：done 在 Stop 返回前已关闭。
	h.service.reconcilerMu.Lock()
	done := h.service.reconcilerDone
	h.service.reconcilerMu.Unlock()
	h.service.StopReconciler()
	select {
	case <-done:
	default:
		t.Fatal("StopReconciler 返回时循环应已退出（同步等待）")
	}
	h.service.StopReconciler() // 幂等：未启动时无副作用。
	// 可重启：重启后的循环继续兜底新停靠的 Run。
	run2 := seed("run-loop-2")
	h.service.StartReconciler(2 * time.Millisecond)
	waitFor(t, "重启后的循环继续刷新", func() bool {
		return h.taskOf(run2.ID, "unk").Status == TaskFailed
	})
	h.service.StopReconciler()
}

// ---- 阶段四：paused Run 的对账与 journal 重建（§12 工作项 4）----

// TestReconcileRunPausedReconcilesTasksKeepsPaused：paused Run 照常按 journal
// 事实对账任务（accepted+活会话 → running），run 本身保持 paused（不产生
// run 状态迁移、不自动重启派发）。
func TestReconcileRunPausedReconcilesTasksKeepsPaused(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching,
		SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	run.Status = RunPaused
	events := []*Event{dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1)}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-1"] = ledgerAccepted
	probe.live["s-dev"] = true

	transitions := ReconcileRun(run, events, probe)
	task, ok := findTransition(transitions, TransitionTaskStatus, "a")
	if !ok || task.Status != TaskRunning {
		t.Fatalf("在途任务应照常对账为 running: %+v", transitions)
	}
	if _, hasRun := findRunTransition(transitions); hasRun {
		t.Fatalf("无待用户事实时 run 应保持 paused（零 run 迁移）: %v", transitions)
	}
}

// TestReconcileRunPausedUnknownUpgradesWaitingInput：对账产生未决事实时
// paused 升级 waiting_input（语义严格强于 paused；升级后 resume 走 PlanResume）。
func TestReconcileRunPausedUnknownUpgradesWaitingInput(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching,
		SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	run.Status = RunPaused
	events := []*Event{dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 1)}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-1"] = ledgerUnknown
	probe.live["s-dev"] = true

	transitions := ReconcileRun(run, events, probe)
	if task, ok := findTransition(transitions, TransitionTaskStatus, "a"); !ok ||
		task.Status != TaskOutcomeUnknown || task.EventType != EventTaskOutcomeUnknown {
		t.Fatalf("未决事实应落任务 outcome_unknown: %+v", transitions)
	}
	runTransition, ok := findRunTransition(transitions)
	if !ok || runTransition.Status != RunWaitingInput || runTransition.EventType != EventRunWaitingInput {
		t.Fatalf("paused 应升级 waiting_input: %+v", runTransition)
	}
}

// TestRebuildRunsFromJournalPausedRunStaysPaused：team-runs.json 损坏后的
// journal 重建认得 run_paused（评审缺失项①的判别测试：缺 store.go 的
// EventRunPaused 分支时，paused Run 会在 run_created 锚点上以 Status=created
// 复活、被恢复路径自动推进/fold）。
func TestRebuildRunsFromJournalPausedRunStaysPaused(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(testTeam()); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	for _, event := range []*Event{
		newEvent("run-p1", EventRunCreated, "user", map[string]any{
			"teamId": "team-1", "goal": "暂停重建", "mode": "pipeline", "planHash": "hash-p",
		}),
		newEvent("run-p1", EventTaskPlanned, "user", map[string]any{
			"taskId": "design", "memberId": "lead", "instructionsHead": "分析",
		}),
		newEvent("run-p1", EventTaskDispatching, "runner", map[string]any{
			"taskId": "design", "sessionId": "s-lead", "submissionId": "sub-p1",
			"inputHash": "ih", "attempt": jsoncompat.Num(1),
		}),
		newEvent("run-p1", EventRunPaused, "user", map[string]any{
			"reason": "用户暂停运行", "sequence": jsoncompat.Num(3),
		}),
	} {
		if err := journal.Append(event); err != nil {
			t.Fatalf("journal 追加失败: %v", err)
		}
	}
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if !store.RunsLedgerCorrupt() {
		t.Fatal("损坏的 runs 账本应置 RunsLedgerCorrupt")
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	store.RebuildRunsFromJournal(events)
	run := store.Run("run-p1")
	if run == nil {
		t.Fatal("重建后 run 应存在")
	}
	if run.Status != RunPaused {
		t.Fatalf("重建应把 paused Run 复原为 paused（而非 run_created 锚点上的 created），实际 %s", run.Status)
	}
	task := findTaskIn(run, "design")
	if task == nil || task.Status != TaskDispatching || task.SubmissionID != "sub-p1" {
		t.Fatalf("重建的暂停快照应保留在途任务事实: %+v", task)
	}
}

// TestRebuildRunsFromJournalPausedThenWaitingInputWins：run_paused 之后的
// run_waiting_input（暂停期间在途停靠/结果未知的升级事实）按事件序覆盖
// paused——重建的升级语义与运行期一致。
func TestRebuildRunsFromJournalPausedThenWaitingInputWins(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(testTeam()); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	for _, event := range []*Event{
		newEvent("run-p2", EventRunCreated, "user", map[string]any{
			"teamId": "team-1", "goal": "升级重建", "mode": "single", "planHash": "hash-w",
		}),
		newEvent("run-p2", EventRunPaused, "user", map[string]any{
			"reason": "用户暂停运行", "sequence": jsoncompat.Num(1),
		}),
		newEvent("run-p2", EventRunWaitingInput, "watcher", map[string]any{
			"reason": "任务 design 结果未知，等待用户处理",
		}),
	} {
		if err := journal.Append(event); err != nil {
			t.Fatalf("journal 追加失败: %v", err)
		}
	}
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	store.RebuildRunsFromJournal(events)
	if run := store.Run("run-p2"); run == nil || run.Status != RunWaitingInput {
		t.Fatalf("run_waiting_input 应按事件序覆盖 paused，实际 %+v", run)
	}
}
