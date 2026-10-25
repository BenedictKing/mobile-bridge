package team

// reducer 的单元测试（§13.1 对应行）：ExtractResult 截断/truncated/排除 error
// 与过程条目；FoldRunStatus 全分支表驱动（终局优先级）；PlanResume 的拒绝、
// 未知任务转 stopped 不重发、可继续分支与全阻塞 fold 指令。

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
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

// ---- §6.5 activity 摘要挖掘 ----

// toolCallMessage 构造一条 role=="activity"、kind=="toolCall" 的消息（形态
// 对齐 internal/acpview view.go tool_call：title/text/changes）。
func toolCallMessage(id, title, text string, paths ...string) *jsoncompat.Object {
	message := jsoncompat.NewObject()
	message.Set("id", id)
	message.Set("kind", "toolCall")
	message.Set("role", "activity")
	message.Set("title", title)
	message.Set("text", text)
	if len(paths) > 0 {
		changes := &jsoncompat.Array{}
		for _, path := range paths {
			change := jsoncompat.NewObject()
			change.Set("path", path)
			changes.Items = append(changes.Items, change)
		}
		message.Set("changes", changes)
	}
	return message
}

// messageFromEntry 把 turnEntry 形态的 map 组装为消息对象。
func messageFromEntry(entry map[string]string) *jsoncompat.Object {
	message := jsoncompat.NewObject()
	message.Set("id", entry["id"])
	message.Set("kind", entry["kind"])
	message.Set("role", entry["role"])
	message.Set("text", entry["text"])
	return message
}

// turnWithMessages 构造 completed turn（消息由调用方逐条给出）。
func turnWithMessages(id string, messages ...*jsoncompat.Object) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", "completed")
	turn.Set("stopReason", "end_turn")
	list := &jsoncompat.Array{}
	for _, message := range messages {
		list.Items = append(list.Items, message)
	}
	turn.Set("messages", list)
	return turn
}

func TestExtractResultMinesActivitySummariesTable(t *testing.T) {
	cases := []struct {
		name            string
		messages        []*jsoncompat.Object
		wantFileSummary string
		wantCmdSummary  string
		wantResult      string // 非空时校验 Result 本体不受摘要挖掘影响
	}{
		{
			name: "编辑类按 changes 抽路径",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "Edit", "src/a.go", "src/a.go"),
				toolCallMessage("t2", "MultiEdit", "", "src/b.go", "src/c.go"),
			},
			wantFileSummary: "src/a.go、src/b.go、src/c.go",
		},
		{
			name: "编辑类无 changes 退回 text 路径",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "Write", "src/new.go"),
				toolCallMessage("t2", "apply_patch", "{\"patch\": \"*** Begin Patch\"}"),
			},
			wantFileSummary: "src/new.go", // JSON 兜底串与 diff 首行不是路径。
		},
		{
			name: "同文件多次编辑只记一次",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "Edit", "src/a.go", "src/a.go"),
				toolCallMessage("t2", "Edit", "src/a.go", "src/a.go"),
			},
			wantFileSummary: "src/a.go",
		},
		{
			name: "命令类抽首行",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "Bash", "go test ./internal/team/...\n覆盖行不进摘要"),
				toolCallMessage("t2", "run_shell_command", "ls -la src"),
			},
			wantCmdSummary: "go test ./internal/team/...；ls -la src",
		},
		{
			name: "通用标题 bash: 前缀按命令处理",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "工具调用", "bash: git status --short"),
			},
			wantCmdSummary: "git status --short",
		},
		{
			name: "中文执行命令标题",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "执行命令", "make check"),
			},
			wantCmdSummary: "make check",
		},
		{
			name: "噪声条目一律跳过",
			messages: []*jsoncompat.Object{
				toolCallMessage("t1", "Read", "src/a.go"),
				toolCallMessage("t2", "TodoWrite", "todowrite: 更新清单"),
				toolCallMessage("t3", "Grep", "rg TODO"),
				toolCallMessage("t4", "Task", "启动子代理"),
				toolCallMessage("t5", "工具调用", "read: src/a.go"),
				messageFromEntry(turnEntry("t6", "agentThought", "activity", "思考过程")),
				messageFromEntry(turnEntry("t7", "plan", "activity", "计划")),
				messageFromEntry(turnEntry("t8", "subagent", "activity", "子代理")),
				messageFromEntry(turnEntry("e1", "error", "error", "Agent 响应超时")),
				messageFromEntry(turnEntry("m1", "agentMessage", "assistant", "结论文本")),
			},
			wantResult: "结论文本",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			turn := turnWithMessages("turn-mine", testCase.messages...)
			result := ExtractResult("s", turn, 12000)
			if result.FileSummary != testCase.wantFileSummary {
				t.Fatalf("文件摘要应为 %q，实际 %q", testCase.wantFileSummary, result.FileSummary)
			}
			if result.CommandSummary != testCase.wantCmdSummary {
				t.Fatalf("命令摘要应为 %q，实际 %q", testCase.wantCmdSummary, result.CommandSummary)
			}
			if testCase.wantResult != "" && result.Result != testCase.wantResult {
				t.Fatalf("assistant 文本应照常进入 Result，实际 %q", result.Result)
			}
		})
	}
}

func TestExtractResultSummaryBounds(t *testing.T) {
	// 条数上限：25 个编辑只保留 20 条并附截断标记。
	messages := []*jsoncompat.Object{}
	for i := 0; i < 25; i++ {
		messages = append(messages, toolCallMessage(
			fmt.Sprintf("t%d", i), "Edit", fmt.Sprintf("src/file%02d.go", i), fmt.Sprintf("src/file%02d.go", i)))
	}
	result := ExtractResult("s", turnWithMessages("turn-1", messages...), 12000)
	if !strings.Contains(result.FileSummary, activityTruncMark) {
		t.Fatalf("超出条数上限应附截断标记，实际 %q", result.FileSummary)
	}
	if strings.Contains(result.FileSummary, "src/file20.go") {
		t.Fatalf("第 21 条起应被丢弃，实际 %q", result.FileSummary)
	}
	if runes := len([]rune(result.FileSummary)); runes > activityTotalRunes {
		t.Fatalf("含标记的摘要应 ≤%d rune，实际 %d", activityTotalRunes, runes)
	}
	// 每条上限：200 rune 的路径截到 120。
	long := strings.Repeat("深", 200)
	result = ExtractResult("s", turnWithMessages("turn-2",
		toolCallMessage("t1", "Edit", long, long)), 12000)
	if !strings.Contains(result.FileSummary, activityTruncMark) {
		t.Fatalf("单条超限应附截断标记，实际 %q", result.FileSummary)
	}
	if got := len([]rune(result.FileSummary)); got > activityItemRunes+len([]rune(activityTruncMark)) {
		t.Fatalf("截断后单条+标记应 ≤%d rune，实际 %d", activityItemRunes, got)
	}
	// 总量上限：20 条 × 110 rune + 分隔符 = 2219 > 1200，截到预算内。
	messages = messages[:0]
	for i := 0; i < 20; i++ {
		path := fmt.Sprintf("%s%03d", strings.Repeat("路", 107), i) // 110 rune。
		messages = append(messages, toolCallMessage(
			fmt.Sprintf("t%d", i), "Edit", path, path))
	}
	result = ExtractResult("s", turnWithMessages("turn-3", messages...), 12000)
	if !strings.Contains(result.FileSummary, activityTruncMark) {
		t.Fatalf("超出总量上限应附截断标记，实际 %q", result.FileSummary)
	}
	if runes := len([]rune(result.FileSummary)); runes > activityTotalRunes {
		t.Fatalf("含标记的摘要应 ≤%d rune，实际 %d", activityTotalRunes, runes)
	}
	// 恰好不触上限：不加标记。
	result = ExtractResult("s", turnWithMessages("turn-4",
		toolCallMessage("t1", "Bash", "make check")), 12000)
	if strings.Contains(result.CommandSummary, activityTruncMark) {
		t.Fatalf("未触上限不应有截断标记，实际 %q", result.CommandSummary)
	}
}

func TestTaskResultSummariesPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	team := testTeam()
	if err := store.SaveTeam(team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	run := &Run{
		ID: "run-rt", TeamID: team.ID, Status: RunCompleted, PlanHash: "hash",
		Tasks: []Task{{
			ID: "a", MemberID: "dev", Status: TaskCompleted,
			Result: "结果文本",
			ResultDetail: &TaskResult{
				SessionID: "s-1", TurnID: "t-1", Result: "结果文本", ResultChars: 4,
				FileSummary:    "src/a.go、src/b.go",
				CommandSummary: "go test ./...；make check",
			},
		}},
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	reloaded := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	fresh := reloaded.Run("run-rt")
	if fresh == nil || len(fresh.Tasks) != 1 {
		t.Fatalf("重载后 run 应存在，实际 %+v", fresh)
	}
	detail := fresh.Tasks[0].ResultDetail
	if detail == nil {
		t.Fatal("重载后 ResultDetail 应存在")
	}
	if detail.FileSummary != "src/a.go、src/b.go" || detail.CommandSummary != "go test ./...；make check" {
		t.Fatalf("两份摘要应随 Run 持久化，实际 file=%q command=%q",
			detail.FileSummary, detail.CommandSummary)
	}
	// journal 重建路径（applyJournalEventToRun）同样保留两摘要。
	rebuilt := &Run{ID: "run-rt", TeamID: team.ID, Status: RunDispatching}
	payload := map[string]any{
		"taskId": "a", "sessionId": "s-1", "turnId": "t-1", "result": "结果文本",
		"resultChars": json.Number("4"),
		"fileSummary": "src/a.go、src/b.go", "commandSummary": "go test ./...；make check",
	}
	applyJournalEventToRun(rebuilt, &Event{Type: EventTaskCompleted, Payload: payload})
	if detail := rebuilt.Tasks[0].ResultDetail; detail == nil ||
		detail.FileSummary != "src/a.go、src/b.go" || detail.CommandSummary != "go test ./...；make check" {
		t.Fatalf("journal 重建应保留两摘要，实际 %+v", rebuilt.Tasks[0].ResultDetail)
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
			status, stopReason := FoldRunStatus(tasks, ModePipeline, RunCollectAll)
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
	status, stopReason := FoldRunStatus(tasks, ModePipeline, RunCollectAll)
	if status != RunFailed || stopReason != "队列派发失败" {
		t.Fatalf("failed 分支应携带任务错误文本，实际 %q/%q", status, stopReason)
	}
	stopped := []Task{
		{ID: "a", Status: TaskStopped, StopReason: "结果未知，用户已确认不再重发", Ordinal: 0},
	}
	status, stopReason = FoldRunStatus(stopped, ModePipeline, RunCollectAll)
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

// ---- first_success 收集策略（§6.5 阶段二）----

func groupTask(id, group, status string, ordinal int) Task {
	task := taskWith(id, "m-"+id, status)
	task.BroadcastGroup = group
	task.Ordinal = ordinal
	return task
}

func TestFoldRunStatusFirstSuccessTable(t *testing.T) {
	cases := []struct {
		name       string
		tasks      []Task
		wantStatus string
		wantReason string
	}{
		{
			name: "全组满足（含 superseded 兄弟）",
			tasks: []Task{
				groupTask("a-lead", "a", TaskCompleted, 0),
				groupTask("a-dev", "a", TaskCancelled, 1),
				{ID: "a-dev", Status: TaskCancelled, StopReason: StopReasonSupersededByFirstSuccess, Ordinal: 1, BroadcastGroup: "a", MemberID: "m"},
			},
			wantStatus: RunCompleted,
		},
		{
			name: "组内全 failed",
			tasks: []Task{
				groupTask("a-lead", "a", TaskFailed, 0),
				groupTask("a-dev", "a", TaskFailed, 1),
			},
			wantStatus: RunFailed,
		},
		{
			name: "已满足组内的失败成员不定罪",
			tasks: []Task{
				groupTask("a-lead", "a", TaskCompleted, 0),
				groupTask("a-dev", "a", TaskFailed, 1),
			},
			wantStatus: RunCompleted,
		},
		{
			name: "全组满足但组外 failed",
			tasks: []Task{
				groupTask("a-lead", "a", TaskCompleted, 0),
				groupTask("a-dev", "a", TaskCancelled, 1),
				taskWith("solo", "rev", TaskFailed),
			},
			wantStatus: RunFailed,
		},
		{
			name: "组外 cancelled 优先于组内 unknown",
			tasks: []Task{
				groupTask("a-lead", "a", TaskCompleted, 0),
				groupTask("a-dev", "a", TaskOutcomeUnknown, 1),
				taskWith("solo", "rev", TaskCancelled),
			},
			wantStatus: RunCancelled,
		},
		{
			name: "存在非终态不可 fold",
			tasks: []Task{
				groupTask("a-lead", "a", TaskCompleted, 0),
				groupTask("a-dev", "a", TaskRunning, 1),
			},
			wantStatus: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, reason := FoldRunStatus(testCase.tasks, ModeBroadcast, RunCollectFirstSuccess)
			if status != testCase.wantStatus {
				t.Fatalf("终局应为 %q，实际 %q（reason=%q）", testCase.wantStatus, status, reason)
			}
			if testCase.wantReason != "" && reason != testCase.wantReason {
				t.Fatalf("原因应为 %q，实际 %q", testCase.wantReason, reason)
			}
		})
	}
}

func TestFoldRunStatusFirstSuccessCarriesFailureText(t *testing.T) {
	tasks := []Task{
		{ID: "a-lead", Status: TaskFailed, Error: "模型拒绝执行", Ordinal: 0, BroadcastGroup: "a", MemberID: "m"},
		{ID: "a-dev", Status: TaskStopped, StopReason: "cancelled", Ordinal: 1, BroadcastGroup: "a", MemberID: "m"},
	}
	status, reason := FoldRunStatus(tasks, ModeBroadcast, RunCollectFirstSuccess)
	if status != RunFailed || reason != "模型拒绝执行" {
		t.Fatalf("组内全终态 0 completed 应带首要错误归 failed，实际 %q/%q", status, reason)
	}
	// all 策略下同一列表维持阶段一优先级（failed 优先）。
	status, reason = FoldRunStatus(tasks, ModeBroadcast, RunCollectAll)
	if status != RunFailed || reason != "模型拒绝执行" {
		t.Fatalf("all 应维持阶段一优先级，实际 %q/%q", status, reason)
	}
}

func TestPlanCollectCancelsLosingSiblings(t *testing.T) {
	run := &Run{
		ID: "run-1", TeamID: "team-1", Status: RunDispatching,
		Mode: ModeBroadcast, CollectPolicy: RunCollectFirstSuccess,
		Tasks: []Task{
			{ID: "a-lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0, BroadcastGroup: "a"},
			{ID: "a-dev", MemberID: "dev", Status: TaskRunning, Ordinal: 1, BroadcastGroup: "a", SubmissionID: "sub-dev"},
			{ID: "a-rev", MemberID: "rev", Status: TaskPlanned, Ordinal: 2, BroadcastGroup: "a"},
			{ID: "b-lead", MemberID: "lead", Status: TaskPlanned, Ordinal: 3, BroadcastGroup: "b"},
		},
	}
	transitions := PlanCollect(run)
	if len(transitions) != 2 {
		t.Fatalf("应取消组 a 的两个非终态成员，实际 %v", transitions)
	}
	for _, transition := range transitions {
		if transition.TaskID != "a-dev" && transition.TaskID != "a-rev" {
			t.Fatalf("不得触碰组 b 或已完成成员: %+v", transition)
		}
		if transition.Kind != TransitionTaskStatus || transition.Status != TaskCancelled {
			t.Fatalf("转移应为 cancelled: %+v", transition)
		}
		if transition.StopReason != StopReasonSupersededByFirstSuccess {
			t.Fatalf("停止原因应为 superseded: %+v", transition)
		}
		if transition.EventType != EventTaskStopped {
			t.Fatalf("应携带 task_stopped 事件: %+v", transition)
		}
	}
	// all 策略、无组 run、组无成功者：一律不产出。
	run.CollectPolicy = RunCollectAll
	if transitions := PlanCollect(run); transitions != nil {
		t.Fatalf("all 策略不应产出收集转移: %v", transitions)
	}
	run.CollectPolicy = RunCollectFirstSuccess
	run.Tasks[0].Status = TaskRunning // 组 a 无成功者。
	if transitions := PlanCollect(run); transitions != nil {
		t.Fatalf("组内无成功者不应取消成员: %v", transitions)
	}
	if transitions := PlanCollect(nil); transitions != nil {
		t.Fatalf("nil run 不应产出: %v", transitions)
	}
}

func TestPlanResumeKeepsDownstreamOfSupersededSibling(t *testing.T) {
	// first_success：组 a 首个成功者已落、兄弟被 superseded；下游 merge 依赖
	// 组 a 的两个成员——resume 不得把 merge 误杀（依赖分支已终止），而应
	// 给出 dispatch 指令。另有全失败组 b 的下游应级联终止。
	run := &Run{
		ID: "run-1", TeamID: "team-1", Status: RunWaitingInput,
		Mode: ModeBroadcast, CollectPolicy: RunCollectFirstSuccess,
		Tasks: []Task{
			{ID: "a-lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0, BroadcastGroup: "a"},
			{ID: "a-dev", MemberID: "dev", Status: TaskCancelled, StopReason: StopReasonSupersededByFirstSuccess, Ordinal: 1, BroadcastGroup: "a"},
			{ID: "b-lead", MemberID: "lead", Status: TaskFailed, Ordinal: 2, BroadcastGroup: "b"},
			{ID: "b-dev", MemberID: "dev", Status: TaskFailed, Ordinal: 3, BroadcastGroup: "b"},
			{ID: "merge", MemberID: "rev", Status: TaskPlanned, Ordinal: 4, DependsOn: []string{"a-lead", "a-dev"}},
			{ID: "孤儿下游", MemberID: "rev", Status: TaskPlanned, Ordinal: 5, DependsOn: []string{"b-lead", "b-dev"}},
			{ID: "unresolved", MemberID: "lead", Status: TaskOutcomeUnknown, Ordinal: 6, BroadcastGroup: "u"},
		},
	}
	transitions, ok := PlanResume(run)
	if !ok {
		t.Fatal("waiting_input 的 run 应允许 resume")
	}
	var dispatched []string
	cascadeStopped := map[string]bool{}
	for _, transition := range transitions {
		switch {
		case transition.Kind == TransitionDispatch:
			dispatched = append(dispatched, transition.TaskID)
		case transition.Kind == TransitionTaskStatus && transition.Status == TaskStopped && transition.TaskID != "unresolved":
			cascadeStopped[transition.TaskID] = true
		}
	}
	if !reflect.DeepEqual(dispatched, []string{"merge"}) {
		t.Fatalf("merge 应可继续派发（依赖组已有成功者），实际 %v", dispatched)
	}
	if !cascadeStopped["孤儿下游"] {
		t.Fatalf("全失败组 b 的下游应级联终止，实际 %v", transitions)
	}
	if cascadeStopped["merge"] {
		t.Fatalf("merge 不得被 superseded 兄弟误杀，实际 %v", transitions)
	}
}

func TestPlanResumeCollectPolicyFromRun(t *testing.T) {
	// all 策略：组内已有成功者但兄弟仍在飞 → 下游保持等待（不派发不终止）。
	run := &Run{
		ID: "run-1", TeamID: "team-1", Status: RunWaitingInput,
		Mode: ModeBroadcast, CollectPolicy: RunCollectAll,
		Tasks: []Task{
			{ID: "a-lead", MemberID: "lead", Status: TaskCompleted, Ordinal: 0, BroadcastGroup: "a"},
			{ID: "a-dev", MemberID: "dev", Status: TaskRunning, Ordinal: 1, BroadcastGroup: "a"},
			{ID: "merge", MemberID: "rev", Status: TaskPlanned, Ordinal: 2, DependsOn: []string{"a-lead", "a-dev"}},
		},
	}
	transitions, _ := PlanResume(run)
	for _, transition := range transitions {
		if transition.TaskID == "merge" {
			t.Fatalf("all 组未全终态时 merge 应保持原状，实际 %v", transition)
		}
	}
}

// ---- §6.5 摘要经 task_completed 事件与对账重建存活 ----

// toolCallUpdate 构造 session/update 的 tool_call 事件（形态对齐
// internal/acpview view.go 的 tool_call 分支）。
func toolCallUpdate(title string, rawInput *jsoncompat.Object) *jsoncompat.Object {
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "tool_call")
	update.Set("title", title)
	update.Set("rawInput", rawInput)
	return update
}

func rawInputOf(pairs ...[2]string) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	for _, pair := range pairs {
		object.Set(pair[0], pair[1])
	}
	return object
}

// TestTaskCompletedEventCarriesSummaries 走完整 watcher 路径：会话 turn 带
// 编辑/命令 activity 完成 → task_completed 事件 payload 携带两摘要 →
// ReconcileRun 从事件重建的 TaskResult 保留两摘要（§8.3 salvage）。
func TestTaskCompletedEventCarriesSummaries(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "work", MemberID: "dev", Prompt: "修个问题", WriteLock: TaskWriteLockNone},
	})
	submissionID, sentText := h.waitForSubmission(t, "s-dev", "任务派发")
	// 模拟队列 worker 派发 + Agent 完成：turn 里带编辑与命令 activity。
	editInput := rawInputOf([2]string{"file_path", "src/a.go"}, [2]string{"old_string", "x"}, [2]string{"new_string", "y"})
	bashInput := rawInputOf([2]string{"command", "go test ./internal/team/..."})
	h.queue.setStatus("s-dev", submissionID, SubStatusAccepted, "")
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", func(view *acpview.View) {
		view.BeginTurn(sentText)
		view.OnUpdate(toolCallUpdate("Edit", editInput))
		view.OnUpdate(toolCallUpdate("Bash", bashInput))
		view.OnUpdate(agentChunk("已修复。"))
		view.EndTurn("end_turn")
	}))
	waitFor(t, "任务完成", func() bool {
		return h.taskOf(run.ID, "work").Status == TaskCompleted
	})
	// 快照与事件 payload 都携带两摘要。
	task := h.taskOf(run.ID, "work")
	if task.ResultDetail == nil || task.ResultDetail.FileSummary != "src/a.go" ||
		task.ResultDetail.CommandSummary != "go test ./internal/team/..." {
		t.Fatalf("完成入账应带两摘要: %+v", task.ResultDetail)
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var completed *Event
	for _, event := range events {
		if event.Type == EventTaskCompleted {
			completed = event
		}
	}
	if completed == nil {
		t.Fatal("journal 应有 task_completed 事件")
	}
	if payloadString(completed.Payload, "fileSummary") != "src/a.go" ||
		payloadString(completed.Payload, "commandSummary") != "go test ./internal/team/..." {
		t.Fatalf("task_completed payload 应携带两摘要: %v", completed.Payload)
	}
	// ReconcileRun 重建：快照退回 dispatching（崩溃窗口：journal 已记完成、
	// 快照未及保存）时按 journal 事实恢复 completed，两摘要一并重建。
	fresh := h.store.Run(run.ID)
	fresh.Status = RunDispatching
	fresh.Tasks[0].Status = TaskDispatching
	transitions := ReconcileRun(fresh, events, &serviceProbe{service: h.service})
	found := false
	for _, transition := range transitions {
		if transition.Kind == TransitionTaskStatus && transition.TaskID == "work" &&
			transition.Status == TaskCompleted && transition.Result != nil {
			found = true
			if transition.Result.FileSummary != "src/a.go" ||
				transition.Result.CommandSummary != "go test ./internal/team/..." {
				t.Fatalf("重建的 TaskResult 应保留两摘要: %+v", transition.Result)
			}
		}
	}
	if !found {
		t.Fatalf("对账应产出 completed 重建迁移: %+v", transitions)
	}
}

// TestPlanResumeRearmsBlockedStopOutcomes：quota/auth/not_configured 停靠任务
// 在 resume 时重臂为 ready（§6.3 停靠重飞：Outcome 保留在快照，派发侧按
// task.Attempt+1 派全新 submissionID）并产出 dispatch 指令；下游依赖不被
// 级联终止（statuses 视图先改非终态）；无 Outcome 的 stopped（级联/取消类）
// 与确定性 failed 不重臂。
func TestPlanResumeRearmsBlockedStopOutcomes(t *testing.T) {
	run := waitingRun(
		Task{ID: "a", MemberID: "dev", Status: TaskStopped, Outcome: OutcomeQuota,
			Attempt: 1, StopReason: "账号额度或余额不足，任务已停靠", Ordinal: 0},
		Task{ID: "b", MemberID: "rev", Status: TaskPlanned, DependsOn: []string{"a"}, Ordinal: 1},
		Task{ID: "c", MemberID: "lead", Status: TaskStopped,
			StopReason: "依赖分支已终止，任务不再执行", Ordinal: 2},
		Task{ID: "d", MemberID: "dev", Status: TaskFailed, Error: "模型拒绝执行", Ordinal: 3},
	)
	transitions, ok := PlanResume(run)
	if !ok {
		t.Fatal("waiting_input 的 run 应允许 resume")
	}
	var rearmA Transition
	rearmAFound, dispatchAFound := false, false
	for _, transition := range transitions {
		switch {
		case transition.TaskID == "a" && transition.Kind == TransitionTaskStatus && transition.Status == TaskReady:
			rearmA, rearmAFound = transition, true
		case transition.TaskID == "a" && transition.Kind == TransitionDispatch:
			dispatchAFound = true
		case transition.TaskID == "b":
			t.Fatalf("依赖仍在飞的 b 不应产生迁移（等待重飞后的 a 释放）: %+v", transition)
		case transition.TaskID == "c" || transition.TaskID == "d":
			t.Fatalf("无 Outcome 的 stopped 与确定性 failed 不应重臂: %+v", transition)
		case transition.Kind == TransitionRunStatus:
			t.Fatalf("存在非终态任务不应 fold: %+v", transition)
		}
	}
	if !rearmAFound || rearmA.EventType != "" {
		t.Fatalf("a 应有无事件的重臂迁移（stopped→ready）: %+v（全部 %v）", rearmA, transitions)
	}
	if !dispatchAFound {
		t.Fatalf("a 应产出 dispatch 指令: %v", transitions)
	}
}

// TestPlanResumeRearmsAuthAndNotConfiguredToo：auth/not_configured 停靠同样
// 重臂（三类停靠 Outcome 同语义）。
func TestPlanResumeRearmsAuthAndNotConfiguredToo(t *testing.T) {
	for _, outcome := range []string{OutcomeAuth, OutcomeNotConfigured} {
		run := waitingRun(Task{ID: "a", MemberID: "dev", Status: TaskStopped,
			Outcome: outcome, Attempt: 1, Ordinal: 0})
		transitions, ok := PlanResume(run)
		if !ok {
			t.Fatalf("outcome=%s 应允许 resume", outcome)
		}
		rearmed, dispatched := false, false
		for _, transition := range transitions {
			if transition.TaskID == "a" && transition.Kind == TransitionTaskStatus && transition.Status == TaskReady {
				rearmed = true
			}
			if transition.TaskID == "a" && transition.Kind == TransitionDispatch {
				dispatched = true
			}
		}
		if !rearmed || !dispatched {
			t.Fatalf("outcome=%s 应重臂并重派: %v", outcome, transitions)
		}
	}
}

// TestAssistantFullTextEquivalence AssistantFullText 与 ExtractResult 的文本
// 拼接段等价（抽函数重构不改行为）；team-actions 解析依赖它拿未截断全文
// （§5.1：ExtractResult 的结果按 MaxResultChars 截断，尾部标记会丢）。
func TestAssistantFullTextEquivalence(t *testing.T) {
	turns := []*jsoncompat.Object{
		nil,
		completedTurn("t-empty"),
		completedTurn("t-1",
			turnEntry("user-1", "userMessage", "user", "请检查"),
			turnEntry("thought-1", "agentThought", "activity", "思考"),
			turnEntry("msg-1", "agentMessage", "assistant", "第一段。"),
			turnEntry("msg-2", "agentMessage", "assistant", "第二段。"),
			turnEntry("error-1", "error", "error", "被排除的错误"),
		),
		completedTurn("t-2", turnEntry("msg-1", "agentMessage", "assistant",
			"正文\n<team-actions>{\"actions\":[]}</team-actions>")),
	}
	for _, turn := range turns {
		full := AssistantFullText(turn)
		// 不截断（上限给全文长度的两倍）时 ExtractResult 的结果就是全文。
		result := ExtractResult("s", turn, len([]rune(full))*2+1)
		if result.Result != full {
			t.Fatalf("全文与未截断结果应相等：full=%q result=%q", full, result.Result)
		}
		if result.ResultChars != len([]rune(full)) {
			t.Fatalf("ResultChars 应等于全文字数：%d vs %d", result.ResultChars, len([]rune(full)))
		}
	}
	// 尾部标记在截断结果里丢失、在全文里保留——解析必须用全文。
	long := strings.Repeat("长", 200) + "\n<team-actions>{\"actions\":[]}</team-actions>"
	turn := completedTurn("t-3", turnEntry("msg-1", "agentMessage", "assistant", long))
	truncated := ExtractResult("s", turn, 100)
	if strings.Contains(truncated.Result, actionsCloseTag) {
		t.Fatal("基线：截断结果应丢掉尾部标记")
	}
	if inner, violations := ExtractActionBlock(AssistantFullText(turn)); len(violations) != 0 || inner != `{"actions":[]}` {
		t.Fatalf("全文应保留尾部标记并成功提取，实际 inner=%q violations=%v", inner, violations)
	}
}
