package team

// threads_test.go 覆盖阶段一执行线程契约（docs/team-overall-design.md §11）：
// memberId → executionThreadId → providerSessionId 映射、thread_bound journal
// 事实、快照/重建双载体、恢复对账不受线程事件干扰，以及旧 attempt/旧线程
// 迟到 watcher 的结算防护。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// threadEventsFromJournal 统计 journal 里各成员的 thread_bound 事件序号
// （memberId → 事件下标列表，按 journal 顺序）。
func threadEventsFromJournal(t *testing.T, journal *Journal) map[string][]int {
	t.Helper()
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	seen := map[string][]int{}
	for i, event := range events {
		if event.Type != EventThreadBound {
			continue
		}
		member := payloadString(event.Payload, "memberId")
		if member == "" {
			t.Fatalf("thread_bound 缺 memberId: %+v", event.Payload)
		}
		if _, has := event.Payload["taskId"]; has {
			t.Fatalf("thread_bound 是 run 级事件，不得携带 taskId 键: %+v", event.Payload)
		}
		seen[member] = append(seen[member], i)
	}
	return seen
}

func findThreadByMember(run *Run, memberID string) *ExecutionThread {
	return findThreadIn(run, ExecutionThreadIDFor(memberID))
}

// 契约①：成员首派即绑定线程——每成员一条、任务携带线程归属、journal 先落
// thread_bound 再落该成员的首条 task_dispatching。
func TestExecutionThreadBoundOnFirstDispatchPerMember(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createBroadcastRun(t, RunCollectAll, []string{"lead", "dev"})
	h.waitForSubmission(t, "s-lead", "draft-lead")
	h.waitForSubmission(t, "s-dev", "draft-dev")

	fresh := h.store.Run(run.ID)
	if len(fresh.Threads) != 2 {
		t.Fatalf("两个成员应各有一条执行线程，实际 %d: %+v", len(fresh.Threads), fresh.Threads)
	}
	lead := findThreadByMember(fresh, "lead")
	if lead == nil || lead.ProviderSessionID != "s-lead" || lead.AgentKind != "kimi" || lead.SourceTaskID == "" {
		t.Fatalf("lead 线程绑定不符: %+v", lead)
	}
	dev := findThreadByMember(fresh, "dev")
	if dev == nil || dev.ProviderSessionID != "s-dev" || dev.AgentKind != "codex" {
		t.Fatalf("dev 线程绑定不符: %+v", dev)
	}
	taskMember := map[string]string{}
	for _, task := range fresh.Tasks {
		taskMember[task.ID] = task.MemberID
		if task.ExecutionThreadID != ExecutionThreadIDFor(task.MemberID) {
			t.Fatalf("任务 %s 应携带所属线程 %s: %+v", task.ID, ExecutionThreadIDFor(task.MemberID), task)
		}
	}
	boundAt := threadEventsFromJournal(t, h.journal)
	if len(boundAt) != 2 {
		t.Fatalf("应恰有两个成员的 thread_bound 事实，实际 %v", boundAt)
	}
	events, _ := h.journal.Replay()
	for i, event := range events {
		if event.Type != EventTaskDispatching {
			continue
		}
		member := taskMember[payloadString(event.Payload, "taskId")]
		first, ok := boundAt[member]
		if !ok || len(first) != 1 || first[0] > i {
			t.Fatalf("成员 %s 的 thread_bound 应先于其首条 task_dispatching 唯一落账: bound=%v dispatch=%d", member, first, i)
		}
		if got := payloadString(event.Payload, "threadId"); got != ExecutionThreadIDFor(member) {
			t.Fatalf("task_dispatching 应携带 threadId，实际 %q", got)
		}
	}
}

// nthEntry 等到 threadID 的第 n 条入账（1 起）并返回（同会话多任务时
// waitForSubmission 的首条口径不够用）。
func nthEntry(t *testing.T, q *fakeQueue, threadID string, n int) fakeQueueEntry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entries := q.entriesFor(threadID); len(entries) >= n {
			return entries[n-1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s 第 %d 条未入队", threadID, n)
	return fakeQueueEntry{}
}

// 契约②：一个 Run + Member 默认一条线程——同成员的后续任务复用线程，
// 不再追加 thread_bound 事实。
func TestExecutionThreadReusedAcrossTasksOfSameMember(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "t1", MemberID: "dev", Prompt: "先实现", WriteLock: TaskWriteLockNone},
		{ID: "t2", MemberID: "dev", Prompt: "再补测试", DependsOn: []string{"t1"}, WriteLock: TaskWriteLockNone},
	})
	first := nthEntry(t, h.queue, "s-dev", 1)
	h.completeSubmission("s-dev", first.submissionID, first.text, "第一段完成。")
	second := nthEntry(t, h.queue, "s-dev", 2)
	h.completeSubmission("s-dev", second.submissionID, second.text, "第二段完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })

	fresh := h.store.Run(run.ID)
	if len(fresh.Threads) != 1 || fresh.Threads[0].MemberID != "dev" {
		t.Fatalf("同成员的两个任务应共用一条线程: %+v", fresh.Threads)
	}
	for _, task := range fresh.Tasks {
		if task.ExecutionThreadID != ExecutionThreadIDFor("dev") {
			t.Fatalf("任务 %s 线程归属漂移: %+v", task.ID, task)
		}
	}
	if bound := threadEventsFromJournal(t, h.journal)["dev"]; len(bound) != 1 {
		t.Fatalf("同成员重派不得追加 thread_bound，实际 %d 条", len(bound))
	}
}

// 契约③：成员绑定会话被恢复流程改写后，重派按新会话重落绑定（同 threadId
// 的第二条 thread_bound，线程快照取最新）。
func TestExecutionThreadRebindsAfterMemberSessionRecovery(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "t1", MemberID: "dev", Prompt: "先实现", WriteLock: TaskWriteLockNone},
		{ID: "t2", MemberID: "dev", Prompt: "再补测试", DependsOn: []string{"t1"}, WriteLock: TaskWriteLockNone},
	})
	sub, text := h.waitForSubmission(t, "s-dev", "t1")
	h.completeSubmission("s-dev", sub, text, "第一段完成。")

	// 模拟 session_recovery.go 的回写：成员 dev 换绑到新会话。
	fresh := h.store.Team(h.team.ID)
	for i := range fresh.Members {
		if fresh.Members[i].ID == "dev" {
			fresh.Members[i].SessionID = "s-dev-2"
		}
	}
	if err := h.store.SaveTeam(fresh); err != nil {
		t.Fatal(err)
	}
	h.resolver.setCwd("s-dev-2", "/tmp/dev")

	entry, ok := h.waitForEntry(t, "s-dev-2", "t2 到新会话")
	if !ok {
		t.Fatal("重派应进入新绑定会话")
	}
	h.completeSubmission("s-dev-2", entry.submissionID, entry.text, "第二段完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })

	final := h.store.Run(run.ID)
	thread := findThreadByMember(final, "dev")
	if thread == nil || thread.ProviderSessionID != "s-dev-2" {
		t.Fatalf("线程应重绑到新会话 s-dev-2: %+v", thread)
	}
	if bound := threadEventsFromJournal(t, h.journal)["dev"]; len(bound) != 2 {
		t.Fatalf("重绑应追加一条 thread_bound，实际 %d 条", len(bound))
	}
	if task := h.taskOf(run.ID, "t2"); task.ResultDetail == nil || task.ResultDetail.SessionID != "s-dev-2" {
		t.Fatalf("t2 的结果引用应指向新会话: %+v", task.ResultDetail)
	}
}

// 契约④：快照落盘往返与 journal 重建（team-runs.json 损坏路径）都保住线程
// 血缘——任务的线程归属与线程绑定双载体不漂移。
func TestExecutionThreadSurvivesSnapshotAndJournalRebuild(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createBroadcastRun(t, RunCollectAll, []string{"lead", "dev"})
	h.waitForSubmission(t, "s-lead", "draft-lead")
	h.waitForSubmission(t, "s-dev", "draft-dev")
	fresh := h.store.Run(run.ID)
	if len(fresh.Threads) != 2 {
		t.Fatalf("派发后应有两线程: %+v", fresh.Threads)
	}

	// 快照往返：team-runs.json 落盘再读回（新账本按 Version=0 种入）。
	dir := t.TempDir()
	disk := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	seedTeam := h.team.Clone()
	seedTeam.Version = 0
	if err := disk.SaveTeam(seedTeam); err != nil {
		t.Fatal(err)
	}
	seedRun := fresh.Clone()
	seedRun.Version = 0
	if err := disk.SaveRun(seedRun); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	persisted := reloaded.Run(run.ID)
	if persisted == nil || len(persisted.Threads) != 2 {
		t.Fatalf("快照应保住线程投影: %+v", persisted)
	}
	for _, task := range persisted.Tasks {
		if task.ExecutionThreadID == "" {
			t.Fatalf("快照应保住任务线程归属: %+v", task)
		}
	}

	// journal 重建：team-runs.json 整文件损坏后的权威路径（照搬存量重建
	// 测试的 torn 账本形态）。
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	rebuildDir := t.TempDir()
	teamsPath := filepath.Join(rebuildDir, "teams.json")
	runsPath := filepath.Join(rebuildDir, "team-runs.json")
	rebuildTeam := h.team.Clone()
	rebuildTeam.Version = 0
	if err := LoadStore(teamsPath, runsPath).SaveTeam(rebuildTeam); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	rebuiltStore := LoadStore(teamsPath, runsPath)
	rebuiltStore.RebuildRunsFromJournal(events)
	fromJournal := rebuiltStore.Run(run.ID)
	if fromJournal == nil || len(fromJournal.Threads) != 2 {
		t.Fatalf("journal 重建应保住线程: %+v", fromJournal)
	}
	thread := findThreadByMember(fromJournal, "dev")
	if thread == nil || thread.ProviderSessionID != "s-dev" || thread.SourceTaskID == "" {
		t.Fatalf("重建的 dev 线程不符: %+v", thread)
	}
	for _, task := range fromJournal.Tasks {
		if task.ExecutionThreadID != ExecutionThreadIDFor(task.MemberID) {
			t.Fatalf("重建的任务 %s 线程归属丢失: %+v", task.ID, task)
		}
	}
}

// 契约⑤：thread_bound 是 run 级事件，恢复对账（ReconcileRun）不得把它折成
// 计划外任务或任何迁移。
func TestReconcileRunIgnoresThreadBoundEvents(t *testing.T) {
	run := inflightRun(Task{
		ID: "a", MemberID: "dev", Status: TaskDispatching,
		SubmissionID: "sub-1", InputHash: "h1", WriteLock: TaskWriteLockNone,
	})
	events := []*Event{
		journalEvent("run-1", EventThreadBound, 1, map[string]any{
			"threadId": ExecutionThreadIDFor("dev"), "memberId": "dev",
			"providerSessionId": "s-dev", "agentKind": "codex", "sourceTaskId": "a",
		}),
		dispatchEvent("run-1", "a", "s-dev", "sub-1", "h1", 2),
	}
	probe := newFakeProbe()
	probe.ledger["s-dev:sub-1"] = ledgerQueued
	probe.live["s-dev"] = true
	if transitions := ReconcileRun(run, events, probe); len(transitions) != 0 {
		t.Fatalf("queued 存活应零迁移，thread_bound 不得引入任何对账迁移: %+v", transitions)
	}
}

// 契约⑥：旧 attempt / 旧线程的迟到 watcher 不得结算新任务——pollTask 在
// 血缘不符时立即退出，即便旧会话里已有匹配 sentText 的 completed turn。
// 时序纪律：先改快照（真实 watcher 下一片即因血缘不符退出）、再布置会话
// 侧事实、并为新 submissionID 入队占位（防收养路径把伪造在途态对账成
// outcome_unknown）——被测的只有直接调用的 pollTask 防护分支。
func TestPollTaskStaleWatcherDoesNotSettleRedispatchedTask(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "solo", MemberID: "lead", Prompt: "单独一版方案", WriteLock: TaskWriteLockNone},
	})
	sub, text := h.waitForSubmission(t, "s-lead", "solo")

	// 模拟重派已发生：任务快照换新 submissionID / 新线程（仍非终态），
	// 并给新 submissionID 入队占位（queued：收养的 watcher 只等不结算）。
	fresh := h.store.Run(run.ID)
	for i := range fresh.Tasks {
		if fresh.Tasks[i].MemberID == "lead" {
			fresh.Tasks[i].SubmissionID = "11111111-1111-4111-8111-111111111111"
			fresh.Tasks[i].ExecutionThreadID = "th_lead_renewed"
		}
	}
	if err := h.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.queue.Enqueue("s-lead", "11111111-1111-4111-8111-111111111111", text); err != nil {
		t.Fatal(err)
	}

	// 旧 attempt 的会话侧事实齐全：账本 accepted + 会话里有匹配的 completed
	// turn——若防护缺失，按旧血缘观察的 pollTask 会立刻把任务折成 completed。
	h.completeSubmission("s-lead", sub, text, "旧回合的结果。")

	state := &runState{runID: run.ID, done: make(chan struct{})}
	cases := []struct {
		name     string
		dispatch *taskDispatch
	}{
		{"旧 attempt", &taskDispatch{taskID: "solo", sessionID: "s-lead", threadID: ExecutionThreadIDFor("lead"), submissionID: sub, sentText: text}},
		{"旧线程", &taskDispatch{taskID: "solo", sessionID: "s-lead", threadID: "th_lead_old", submissionID: "11111111-1111-4111-8111-111111111111", sentText: text}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			settled, _ := h.service.pollTask(state, testCase.dispatch, h.team.Policy, nil)
			if !settled {
				t.Fatal("迟到 watcher 应立即退出（settled=true）")
			}
			task := h.taskOf(run.ID, "solo")
			if task.Status != TaskDispatching || task.Result != "" {
				t.Fatalf("迟到 watcher 不得结算新任务: %+v", task)
			}
			events, _ := h.journal.Replay()
			for _, event := range events {
				if event.Type == EventTaskCompleted && payloadString(event.Payload, "taskId") == "solo" {
					t.Fatal("迟到 watcher 不得落 task_completed 事实")
				}
			}
		})
	}
}

// 契约⑦：HTTP 投影侧的线程关闭态派生依据（RunTerminal）与终态口径一致。
func TestRunTerminalDerivesThreadClosedStatus(t *testing.T) {
	if !RunTerminal(RunCompleted) || !RunTerminal(RunOutcomeUnknown) || RunTerminal(RunWaitingInput) || RunTerminal(RunPaused) {
		t.Fatal("RunTerminal 应与 terminalRun 同口径")
	}
}

// 附带：threads 数组按声明顺序序列化，未派发的 Run 不带 threads 键。
func TestRunMarshalOmitsEmptyThreads(t *testing.T) {
	run := inflightRun(Task{ID: "a", MemberID: "dev", Status: TaskPlanned})
	run.Threads = []ExecutionThread{{
		ID: "th_dev", MemberID: "dev", AgentKind: "codex",
		ProviderSessionID: "s-dev", SourceTaskID: "a",
	}}
	raw, err := run.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	threads, ok := decoded["threads"].([]any)
	if !ok || len(threads) != 1 {
		t.Fatalf("threads 应序列化: %s", raw)
	}
	first := threads[0].(map[string]any)
	if first["id"] != "th_dev" || first["providerSessionId"] != "s-dev" || first["sourceTaskId"] != "a" {
		t.Fatalf("线程字段不符: %v", first)
	}
	empty := inflightRun(Task{ID: "b", MemberID: "dev", Status: TaskPlanned})
	rawEmpty, _ := empty.MarshalJSON()
	value, _ := jsoncompat.Unmarshal(rawEmpty)
	object, _ := value.(*jsoncompat.Object)
	if object == nil || object.MustGet("threads") != nil {
		t.Fatalf("无线程的 Run 不应带 threads 键: %s", rawEmpty)
	}
}
