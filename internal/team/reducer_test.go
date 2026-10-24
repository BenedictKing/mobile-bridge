package team

// reducer 的单元测试（§13.1 对应行）：ExtractResult 截断/truncated/排除 error
// 与过程条目；FoldRunStatus 全分支表驱动（终局优先级）；PlanResume 的拒绝、
// 未知任务转 stopped 不重发、可继续分支与全阻塞 fold 指令。

import (
	"reflect"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// completedTurn 构造一个 turn.status=="completed" 的视图 turn（结构对齐
// internal/acpview：messages 数组按 kind/role/text 描述）。
func completedTurn(id string, entries ...map[string]string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", "completed")
	turn.Set("stopReason", "end_turn")
	turn.Set("error", nil)
	messages := &jsoncompat.Array{}
	for _, entry := range entries {
		message := jsoncompat.NewObject()
		message.Set("id", entry["id"])
		message.Set("kind", entry["kind"])
		message.Set("role", entry["role"])
		message.Set("text", entry["text"])
		messages.Items = append(messages.Items, message)
	}
	turn.Set("messages", messages)
	return turn
}

func turnEntry(id, kind, role, text string) map[string]string {
	return map[string]string{"id": id, "kind": kind, "role": role, "text": text}
}

func TestExtractResultConcatenatesAssistantTextOnly(t *testing.T) {
	turn := completedTurn("turn-7",
		turnEntry("user-1", "userMessage", "user", "请检查"),
		turnEntry("thought-1", "agentThought", "activity", "思考过程"),
		turnEntry("tool-1", "toolCall", "activity", "运行测试"),
		turnEntry("msg-1", "agentMessage", "assistant", "发现两个错误处理问题。"),
		turnEntry("msg-2", "agentMessage", "assistant", "建议补上重试。"),
		turnEntry("error-1", "error", "error", "Agent 响应超时"),
	)
	result := ExtractResult("session-rev", turn, 12000)
	if result.SessionID != "session-rev" || result.TurnID != "turn-7" {
		t.Fatalf("引用字段错误: %+v", result)
	}
	if result.StopReason != "end_turn" {
		t.Fatalf("stopReason 应为 end_turn，实际 %q", result.StopReason)
	}
	want := "发现两个错误处理问题。\n建议补上重试。"
	if result.Result != want {
		t.Fatalf("结果应只含 assistant 文本按序拼接，实际 %q", result.Result)
	}
	if result.ResultChars != len([]rune(want)) || result.Truncated {
		t.Fatalf("字数/截断标记错误: chars=%d truncated=%v", result.ResultChars, result.Truncated)
	}
}

func TestExtractResultTruncatesWithFullCharCount(t *testing.T) {
	long := strings.Repeat("长", 96) + "尾部文本" // 100 rune。
	turn := completedTurn("turn-9", turnEntry("msg-1", "agentMessage", "assistant", long))
	result := ExtractResult("s", turn, 60)
	if !result.Truncated {
		t.Fatal("超过 maxChars 应标记 truncated")
	}
	if got := len([]rune(result.Result)); got != 60 {
		t.Fatalf("截断后应保留 60 字，实际 %d", got)
	}
	if result.ResultChars != len([]rune(long)) {
		t.Fatalf("ResultChars 应记录截断前完整字数 %d，实际 %d", len([]rune(long)), result.ResultChars)
	}
	if result.Result != string([]rune(long)[:60]) {
		t.Fatal("截断应保留前 60 个 rune")
	}
	// 恰好等于 maxChars 不截断。
	exact := ExtractResult("s", turn, len([]rune(long)))
	if exact.Truncated || exact.Result != long {
		t.Fatalf("恰好等于上限不应截断，实际 truncated=%v", exact.Truncated)
	}
	// maxChars≤0 回退默认上限 12000（策略层已拒绝 0；这里兜底历史账本里的
	// 非法 0 值——结果 payload 必须恒有界，§10.3/§15.3）。
	huge := strings.Repeat("长", 12001)
	hugeTurn := completedTurn("turn-10", turnEntry("msg-1", "agentMessage", "assistant", huge))
	fallback := ExtractResult("s", hugeTurn, 0)
	if !fallback.Truncated || len([]rune(fallback.Result)) != 12000 {
		t.Fatalf("maxChars=0 应回退默认上限 12000 截断，实际 truncated=%v len=%d",
			fallback.Truncated, len([]rune(fallback.Result)))
	}
	if fallback.ResultChars != 12001 {
		t.Fatalf("ResultChars 应记录截断前完整字数，实际 %d", fallback.ResultChars)
	}
}

func TestExtractResultDefensiveInputs(t *testing.T) {
	empty := ExtractResult("s", nil, 100)
	if empty.Result != "" || empty.SessionID != "s" || empty.TurnID != "" {
		t.Fatalf("nil turn 应返回空结果，实际 %+v", empty)
	}
	// 失败 turn（FailTurn 产物）：error 条目被排除，结果为空文本——失败文本
	// 不进入结果，由失败路径自行处理。
	failed := jsoncompat.NewObject()
	failed.Set("id", "turn-1")
	failed.Set("status", "failed")
	failed.Set("error", nil)
	messages := &jsoncompat.Array{}
	errorEntry := jsoncompat.NewObject()
	errorEntry.Set("id", "error-1")
	errorEntry.Set("kind", "error")
	errorEntry.Set("role", "error")
	errorEntry.Set("text", "Agent 响应超时")
	messages.Items = append(messages.Items, errorEntry)
	failed.Set("messages", messages)
	result := ExtractResult("s", failed, 100)
	if result.Result != "" || result.ResultChars != 0 {
		t.Fatalf("失败 turn 的 error 文本不应进入结果，实际 %+v", result)
	}
}

func TestFoldRunStatusTable(t *testing.T) {
	cases := []struct {
		name       string
		statuses   []string
		wantStatus string
	}{
		{"全 completed", []string{TaskCompleted, TaskCompleted}, RunCompleted},
		{"任一 failed", []string{TaskCompleted, TaskFailed}, RunFailed},
		{"failed 优先于 cancelled", []string{TaskCancelled, TaskFailed}, RunFailed},
		{"任一 cancelled", []string{TaskCompleted, TaskCancelled}, RunCancelled},
		{"cancelled 优先于 outcome_unknown", []string{TaskOutcomeUnknown, TaskCancelled}, RunCancelled},
		{"任一 outcome_unknown", []string{TaskCompleted, TaskOutcomeUnknown}, RunOutcomeUnknown},
		{"outcome_unknown 优先于 stopped", []string{TaskStopped, TaskOutcomeUnknown}, RunOutcomeUnknown},
		{"任一 stopped", []string{TaskCompleted, TaskStopped}, RunStopped},
		{"空任务列表", nil, RunCompleted},
		{"存在非终态", []string{TaskCompleted, TaskRunning}, ""},
		{"存在 planned", []string{TaskPlanned}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			tasks := make([]Task, 0, len(testCase.statuses))
			for i, status := range testCase.statuses {
				tasks = append(tasks, Task{ID: string(rune('a' + i)), Status: status, Ordinal: i})
			}
			status, stopReason := FoldRunStatus(tasks)
			if status != testCase.wantStatus {
				t.Fatalf("终局应为 %q，实际 %q（stopReason=%q）", testCase.wantStatus, status, stopReason)
			}
		})
	}
}

func TestFoldRunStatusStopReasonCarriesCause(t *testing.T) {
	tasks := []Task{
		{ID: "a", Status: TaskCompleted, Ordinal: 0},
		{ID: "b", Status: TaskFailed, Error: "队列派发失败", Ordinal: 1},
	}
	status, stopReason := FoldRunStatus(tasks)
	if status != RunFailed || stopReason != "队列派发失败" {
		t.Fatalf("failed 分支应携带任务错误文本，实际 %q/%q", status, stopReason)
	}
	stopped := []Task{
		{ID: "a", Status: TaskStopped, StopReason: "结果未知，用户已确认不再重发", Ordinal: 0},
	}
	status, stopReason = FoldRunStatus(stopped)
	if status != RunStopped || stopReason != "结果未知，用户已确认不再重发" {
		t.Fatalf("stopped 分支应携带停止原因，实际 %q/%q", status, stopReason)
	}
}

func waitingRun(tasks ...Task) *Run {
	for i := range tasks {
		if tasks[i].Ordinal == 0 {
			tasks[i].Ordinal = i
		}
	}
	return &Run{ID: "run-1", TeamID: "team-1", Status: RunWaitingInput, Tasks: tasks}
}

func TestPlanResumeRejectsNonWaitingRun(t *testing.T) {
	for _, status := range []string{RunDispatching, RunCompleted, RunFailed, RunStopped, RunCreated} {
		run := waitingRun(taskWith("a", "dev", TaskPlanned))
		run.Status = status
		if transitions, ok := PlanResume(run); ok {
			t.Fatalf("状态 %s 不应允许 resume，实际返回 %v", status, transitions)
		}
	}
	if transitions, ok := PlanResume(nil); ok {
		t.Fatalf("nil run 不应允许 resume，实际返回 %v", transitions)
	}
}

func TestPlanResumeStopsUnknownTasksWithoutRedispatch(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskOutcomeUnknown),
		taskWith("b", "rev", TaskPlanned, "a"),
	)
	transitions, ok := PlanResume(run)
	if !ok {
		t.Fatal("waiting_input 的 run 应允许 resume")
	}
	var stopped Transition
	found := false
	for _, transition := range transitions {
		if transition.Kind == TransitionTaskStatus && transition.TaskID == "a" {
			stopped = transition
			found = true
		}
		if transition.TaskID == "a" && transition.Kind == TransitionDispatch {
			t.Fatal("outcome_unknown 任务绝不允许重发（不得出现 dispatch 指令）")
		}
	}
	if !found {
		t.Fatalf("应产出 a→stopped 迁移，实际 %v", transitions)
	}
	if stopped.Status != TaskStopped || stopped.StopReason != "结果未知，用户已确认不再重发" {
		t.Fatalf("a 应转 stopped 并带固定停止原因，实际 %+v", stopped)
	}
	if stopped.EventType != EventTaskStopped {
		t.Fatalf("a→stopped 应携带 task_stopped 事件，实际 %q", stopped.EventType)
	}
}

func TestPlanResumeReturnsDispatchableBranch(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskOutcomeUnknown),
		taskWith("b", "rev", TaskPlanned),
		taskWith("c", "lead", TaskPlanned, "a"),
	)
	transitions, _ := PlanResume(run)
	var dispatched []string
	cStopped := false
	for _, transition := range transitions {
		if transition.Kind == TransitionDispatch {
			dispatched = append(dispatched, transition.TaskID)
		}
		if transition.TaskID == "c" && transition.Status == TaskStopped {
			cStopped = true // c 被 stopped 依赖永久阻塞，级联终止。
		}
	}
	if !reflect.DeepEqual(dispatched, []string{"b"}) {
		t.Fatalf("只有无依赖的 b 可继续派发（c 被 stopped 依赖阻塞），实际 %v", dispatched)
	}
	if !cStopped {
		t.Fatalf("被 stopped 依赖阻塞的 c 应级联终止，实际 %v", transitions)
	}
}

func TestPlanResumeFoldWhenFullyBlocked(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskOutcomeUnknown),
		taskWith("b", "rev", TaskPlanned, "a"),
	)
	transitions, _ := PlanResume(run)
	var fold Transition
	found := false
	for _, transition := range transitions {
		if transition.Kind == TransitionRunStatus {
			fold = transition
			found = true
		}
	}
	if !found {
		t.Fatalf("无可派发且全终态应返回 fold 指令，实际 %v", transitions)
	}
	// 存在 stopped → fold 归 run stopped（§10.2 resume 语义）。
	if fold.Status != RunStopped {
		t.Fatalf("fold 应归 run stopped，实际 %q", fold.Status)
	}
	if fold.StopReason != "结果未知，用户已确认不再重发" {
		t.Fatalf("fold 应携带 stopped 原因，实际 %q", fold.StopReason)
	}
	// 空任务 run：全终态（空）→ fold 为 completed。
	empty := waitingRun()
	transitions, _ = PlanResume(empty)
	if len(transitions) != 1 || transitions[0].Kind != TransitionRunStatus || transitions[0].Status != RunCompleted {
		t.Fatalf("空任务 waiting run 应 fold 为 completed，实际 %v", transitions)
	}
}

func TestPlanResumeCascadeStopsTransitivelyBlockedTasks(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskOutcomeUnknown),
		taskWith("b", "rev", TaskPlanned, "a"),
		taskWith("c", "lead", TaskPlanned, "b"),
	)
	transitions, _ := PlanResume(run)
	bStopped, cStopped := false, false
	for _, transition := range transitions {
		if transition.Status == TaskStopped {
			switch transition.TaskID {
			case "b":
				bStopped = true
			case "c":
				cStopped = true
			}
		}
	}
	if !bStopped || !cStopped {
		t.Fatalf("传递阻塞链 a←b←c 应一遍级联终止，实际 %v", transitions)
	}
	// 全部终态 → fold 指令归 run stopped。
	fold, ok := findRunTransition(transitions)
	if !ok || fold.Status != RunStopped {
		t.Fatalf("级联终止后应 fold 为 run stopped，实际 %v", transitions)
	}
}

func TestPlanResumeRunningTasksBlockFold(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskOutcomeUnknown),
		taskWith("b", "rev", TaskRunning),
		taskWith("c", "lead", TaskPlanned, "b"),
	)
	transitions, _ := PlanResume(run)
	for _, transition := range transitions {
		if transition.Kind == TransitionRunStatus {
			t.Fatalf("仍有运行中任务时不应 fold，实际 %v", transition)
		}
		if transition.TaskID == "b" {
			t.Fatalf("resume 不得触碰运行中任务，实际 %v", transition)
		}
	}
	// a 仍然转 stopped。
	stopped := false
	for _, transition := range transitions {
		if transition.TaskID == "a" && transition.Status == TaskStopped {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("未决 unknown 应转 stopped，实际 %v", transitions)
	}
}

func TestPlanResumeDoesNotResurrectFailedTasks(t *testing.T) {
	run := waitingRun(
		taskWith("a", "dev", TaskFailed),
		taskWith("b", "rev", TaskPlanned),
	)
	transitions, _ := PlanResume(run)
	for _, transition := range transitions {
		if transition.TaskID == "a" {
			t.Fatalf("确定性 failed 不得被 resume 改动，实际 %v", transition)
		}
	}
	var dispatched []string
	for _, transition := range transitions {
		if transition.Kind == TransitionDispatch {
			dispatched = append(dispatched, transition.TaskID)
		}
	}
	if !reflect.DeepEqual(dispatched, []string{"b"}) {
		t.Fatalf("独立分支 b 应可派发，实际 %v", dispatched)
	}
}
