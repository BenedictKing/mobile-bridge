package team

// recovery 的单元测试（§13.1 team/recovery 行）：五类账本状态映射、无账本
// 条目回 ready、unknown 不重发且 run 落 waiting_input、inputHash 漂移、
// journal 优先于快照、计划外任务与终态 run 的防御分支。

import (
	"fmt"
	"strings"
	"testing"

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
