package team

// dispatcher/runner 的单元测试（§13.1，对应 team-dispatch 工作流的必测清单）：
// journal 事件先于入账（先记账后发送）、duplicate 入队无二次副作用、
// unknown→outcome_unknown 且无第二次 Enqueue、watcher turn 分类表驱动（用
// acpview view.go:169-199 的真实字段形态构造 fixture）、POST-cancel 先记账后
// session.Cancel 且 watcher 对已终态 no-op、Resume 四分支、超时取消、上下文
// 截断与合成全文上限的派发期联合用例、多任务同会话按文本匹配、watcher 终态
// 后退出（channel 关闭断言）、重启恢复不重发。全部 Go 级 fake，不依赖
// python3；等待用 deadline 轮询（queue_test.go 惯用法），不用固定 sleep 判
// 正确性。

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// ---- Go 级 fake：QueueAdapter（复刻 internal/httpd/queue.go 状态机）----

// fakeQueueEntry 是 submissions 账本条目的内存形态。
type fakeQueueEntry struct {
	threadID     string
	submissionID string
	text         string
	status       string
	errMsg       string
}

// fakeQueue 复刻 queue.go 的关键语义：入账幂等（同 id 同内容 duplicate=true
// 无副作用、同 id 不同内容报「同一消息标识不能用于不同内容」）、100000 字节
// 上限、UUID 校验、cancel 仅对仍处 queued 的条目生效、unknown 永不自动重放
// （fake 没有 worker：状态由测试显式推进，模拟既有 3s worker / settle 钩子
// 的落账结果）。
type fakeQueue struct {
	mu        sync.Mutex
	entries   map[string]*fakeQueueEntry
	order     []string
	calls     map[string]int // submissionID → Enqueue 调用次数
	onEnqueue func(threadID, submissionID, text string)
}

func newFakeQueue() *fakeQueue {
	return &fakeQueue{entries: map[string]*fakeQueueEntry{}, calls: map[string]int{}}
}

func (q *fakeQueue) Enqueue(threadID, submissionID, text string) (string, bool, error) {
	if hook := q.onEnqueue; hook != nil {
		hook(threadID, submissionID, text)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.calls[submissionID]++
	if _, err := uuid.Parse(submissionID); err != nil {
		return "", false, errors.New("消息标识无效")
	}
	if strings.TrimSpace(text) == "" || len(text) > 100000 {
		return "", false, errors.New("请输入 1–100000 字的消息")
	}
	key := threadID + ":" + submissionID
	if prior := q.entries[key]; prior != nil {
		if prior.text != text {
			return "", false, errors.New("同一消息标识不能用于不同内容")
		}
		return prior.status, true, nil
	}
	q.entries[key] = &fakeQueueEntry{threadID: threadID, submissionID: submissionID, text: text, status: SubStatusQueued}
	q.order = append(q.order, key)
	return SubStatusQueued, false, nil
}

func (q *fakeQueue) Submission(threadID, submissionID string) (TeamSubmission, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries[threadID+":"+submissionID]
	if entry == nil {
		return TeamSubmission{}, false
	}
	return TeamSubmission{Status: entry.status, Error: entry.errMsg}, true
}

func (q *fakeQueue) CancelSubmission(threadID, submissionID string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.entries[threadID+":"+submissionID]
	if entry == nil || entry.status != SubStatusQueued {
		return errors.New("此消息已离开队列，请查看会话")
	}
	entry.status = SubStatusCancelled
	return nil
}

// setStatus 是测试推进辅助：直接落账本状态（模拟 worker/settle 钩子结果）。
func (q *fakeQueue) setStatus(threadID, submissionID, status, errMsg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry := q.entries[threadID+":"+submissionID]; entry != nil {
		entry.status = status
		entry.errMsg = errMsg
	}
}

func (q *fakeQueue) entriesFor(threadID string) []fakeQueueEntry {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries := []fakeQueueEntry{}
	for _, key := range q.order {
		if entry := q.entries[key]; entry != nil && entry.threadID == threadID {
			entries = append(entries, *entry)
		}
	}
	return entries
}

// firstEntry 返回 threadID 的首条入账（插入序）。
func (q *fakeQueue) firstEntry(threadID string) (fakeQueueEntry, bool) {
	entries := q.entriesFor(threadID)
	if len(entries) == 0 {
		return fakeQueueEntry{}, false
	}
	return entries[0], true
}

func (q *fakeQueue) callCount(submissionID string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls[submissionID]
}

// ---- Go 级 fake：SessionResolver（手工快照会话）----

// fakeResolver 按会话返回预先构造的 Session。会话对象一经构造不再修改
// （每次「视图变化」整体换新实例），杜绝与 watcher 并发读 View 的数据竞争
// ——acpruntime.Session 的 changed 通道未导出，Cancel 不可用，取消路径经
// service.cancelAgentTurn 钩子观测。
type fakeResolver struct {
	mu       sync.Mutex
	sessions map[string]*acpruntime.Session
	cwds     map[string]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{sessions: map[string]*acpruntime.Session{}, cwds: map[string]string{}}
}

func (r *fakeResolver) Resolve(threadID string) (*acpruntime.Manager, *acpruntime.Session, error) {
	r.mu.Lock()
	session := r.sessions[threadID]
	r.mu.Unlock()
	return nil, session, nil
}

func (r *fakeResolver) Cwd(threadID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cwds[threadID]
}

func (r *fakeResolver) setSession(threadID string, session *acpruntime.Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[threadID] = session
}

func (r *fakeResolver) setCwd(threadID, cwd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cwds[threadID] = cwd
}

// newFakeSession 构造手工快照会话；drive 用真实的 acpview API 落出
// view.go:169-199 的 turn 字段形态。
func newFakeSession(id, cwd string, drive ...func(*acpview.View)) *acpruntime.Session {
	view := acpview.NewView(id, nil, cwd, func() float64 { return 0 })
	for _, fn := range drive {
		fn(view)
	}
	return &acpruntime.Session{ID: id, Cwd: cwd, View: view}
}

func agentChunk(text string) *jsoncompat.Object {
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "agent_message_chunk")
	content := jsoncompat.NewObject()
	content.Set("text", text)
	update.Set("content", content)
	return update
}

func turnInProgress(text string) func(*acpview.View) {
	return func(view *acpview.View) { view.BeginTurn(text) }
}

func turnCompleted(text, assistant string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn(text)
		view.OnUpdate(agentChunk(assistant))
		view.EndTurn("end_turn")
	}
}

func turnFailedView(text, errText string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn(text)
		view.FailTurn(errText)
	}
}

func turnInterrupted(text string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn(text)
		view.EndTurn("cancelled")
	}
}

// ---- 测试基座 ----

type dispatchHarness struct {
	store    *memStore
	journal  *Journal
	queue    *fakeQueue
	resolver *fakeResolver
	service  *Service
	team     *Team
}

func newDispatchHarness(t *testing.T) *dispatchHarness {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	h := &dispatchHarness{
		store:    newMemStore(),
		journal:  journal,
		queue:    newFakeQueue(),
		resolver: newFakeResolver(),
	}
	h.team = testTeam()
	// exclusive 任务依赖 cwd 解析；三个成员会话各给固定目录。
	for _, member := range h.team.Members {
		h.resolver.setCwd(member.SessionID, "/tmp/"+member.ID)
	}
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	h.service = NewService(h.store, h.journal, h.queue, h.resolver)
	h.service.waitSlice = 2 * time.Millisecond
	// 收尾：把未终态的 run 全部取消，让 watcher/循环 goroutine 退出。
	t.Cleanup(func() {
		for _, run := range h.store.Runs() {
			_, _ = h.service.Cancel(run.ID)
		}
	})
	return h
}

func (h *dispatchHarness) createRun(t *testing.T, mode string, plan []PlanTaskInput) *Run {
	t.Helper()
	run, err := h.service.CreateRun(h.team, "实现用户头像上传接口", mode, plan)
	if err != nil {
		t.Fatalf("创建 run 失败: %v", err)
	}
	return run
}

// waitForSubmission 等到 threadID 的首条入账并返回 (submissionID, text)。
func (h *dispatchHarness) waitForSubmission(t *testing.T, threadID, what string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entry, ok := h.queue.firstEntry(threadID); ok {
			return entry.submissionID, entry.text
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s 未入队", what)
	return "", ""
}

// completeSubmission 把条目置 accepted 并给会话换上「本任务 turn 已完成」的
// 快照（模拟既有队列 worker 派发成功 + Agent 完成）。
func (h *dispatchHarness) completeSubmission(threadID, submissionID, sentText, assistant string) {
	h.queue.setStatus(threadID, submissionID, SubStatusAccepted, "")
	h.resolver.setSession(threadID, newFakeSession(threadID, "/tmp/"+strings.TrimPrefix(threadID, "s-"),
		turnCompleted(sentText, assistant)))
}

func (h *dispatchHarness) taskOf(runID, taskID string) Task {
	run := h.store.Run(runID)
	if run == nil {
		return Task{}
	}
	if task := findTaskIn(run, taskID); task != nil {
		return *task
	}
	return Task{}
}

func (h *dispatchHarness) runEventTypes(runID string) []string {
	events, err := h.journal.Replay()
	if err != nil {
		return nil
	}
	types := []string{}
	for _, event := range events {
		if event.RunID == runID {
			types = append(types, event.Type)
		}
	}
	return types
}

// grabWatcherExit 在任务在途期间抓取其 watcher 的退出信号通道。
func (h *dispatchHarness) grabWatcherExit(t *testing.T, runID, taskID string) <-chan struct{} {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state := h.service.stateFor(runID)
		state.mu.Lock()
		exit, watched := state.watchers[taskID]
		state.mu.Unlock()
		if watched {
			return exit
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watcher 未启动: %s/%s", runID, taskID)
	return nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func waitForClosed(t *testing.T, what string, channel <-chan struct{}) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		t.Fatalf("等待超时: %s", what)
	}
}

// ---- submissionID 派生 ----

func TestTaskSubmissionIDDeterministic(t *testing.T) {
	first := TaskSubmissionID("team-1", "run-1", "design", 1)
	if first != TaskSubmissionID("team-1", "run-1", "design", 1) {
		t.Fatal("同参数派生应恒等")
	}
	if first == TaskSubmissionID("team-1", "run-1", "design", 2) {
		t.Fatal("不同 attempt 应派生不同 submissionID")
	}
	if first == TaskSubmissionID("team-1", "run-2", "design", 1) {
		t.Fatal("不同 run 应派生不同 submissionID")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("submissionID 应为合法 UUID: %v", err)
	}
	if key := TaskIdempotencyKey("team-1", "run-1", "design", 1); key != "team:team-1:run:run-1:task:design:attempt:1" {
		t.Fatalf("幂等键格式与 §8.2 不符: %s", key)
	}
}

// ---- 先记账后发送 ----

func TestSubmitTaskJournalsBeforeEnqueue(t *testing.T) {
	h := newDispatchHarness(t)
	var taskStatusAtEnqueue string
	var journalInputHash, journalKey, journalAttempt string
	var journalHasDispatch bool
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		for _, run := range h.store.Runs() {
			if task := findTaskIn(run, "lead"); task != nil {
				taskStatusAtEnqueue = task.Status
			}
		}
		events, err := h.journal.Replay()
		if err != nil {
			t.Errorf("journal 重放失败: %v", err)
			return
		}
		for _, event := range events {
			if event.Type != EventTaskDispatching {
				continue
			}
			if id, _ := event.Payload["submissionId"].(string); id != submissionID {
				continue
			}
			journalHasDispatch = true
			journalInputHash, _ = event.Payload["inputHash"].(string)
			journalKey = event.IdempotencyKey
			if attempt, ok := event.Payload["attempt"].(json.Number); ok {
				journalAttempt = attempt.String()
			}
		}
	}
	run := h.createRun(t, ModeSingle, nil)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")

	if taskStatusAtEnqueue != TaskDispatching {
		t.Fatalf("入队时任务应已落 dispatching（先记账后发送），实际 %s", taskStatusAtEnqueue)
	}
	if !journalHasDispatch {
		t.Fatal("入队前 journal 必须已有 task_dispatching 事件")
	}
	if journalInputHash == "" {
		t.Fatal("task_dispatching 应携带派发期 inputHash")
	}
	if want := TaskIdempotencyKey(h.team.ID, run.ID, "lead", 1); journalKey != want {
		t.Fatalf("幂等键应为 %q，实际 %q", want, journalKey)
	}
	if journalAttempt != "1" {
		t.Fatalf("阶段一 attempt 恒为 1，实际 %s", journalAttempt)
	}
	if want := TaskSubmissionID(h.team.ID, run.ID, "lead", 1); submissionID != want {
		t.Fatalf("submissionID 应确定性派生: %s vs %s", submissionID, want)
	}
	// 收尾：完成任务，让循环与 watcher 干净退出。
	h.completeSubmission("s-lead", submissionID, sentText, "完成")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

func TestSubmitTaskDuplicateEnqueueHasNoSecondSideEffect(t *testing.T) {
	h := newDispatchHarness(t)
	// 手工构造 planned 任务（不经 CreateRun，无并发循环），直接驱动两次提交。
	run := &Run{ID: "run-dup", TeamID: h.team.ID, Goal: "g", Status: RunDispatching, Tasks: []Task{{
		ID: "design", MemberID: "lead", Prompt: "分析", Status: TaskPlanned,
		Ordinal: 0, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
	}}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	team := h.store.Team(h.team.ID)
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	dispatch, err := h.service.submitTaskLocked(state, team, h.store.Run(run.ID), "design")
	state.mu.Unlock()
	if err != nil || dispatch == nil {
		t.Fatalf("首次派发失败: %v / %v", dispatch, err)
	}
	firstID := dispatch.submissionID

	// 模拟恢复期回退：账本条目仍在，任务回 ready，再次提交。
	rolled := h.store.Run(run.ID)
	rolled.Tasks[0].Status = TaskReady
	if err := h.store.SaveRun(rolled); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	again, err := h.service.submitTaskLocked(state, team, h.store.Run(run.ID), "design")
	state.mu.Unlock()
	if err != nil {
		t.Fatalf("重复入队不应报错: %v", err)
	}
	if again == nil {
		t.Fatal("duplicate 路径应照常产出派发信息（watcher 观察既有条目）")
	}
	if again.submissionID != firstID {
		t.Fatalf("submissionID 应确定性一致: %s vs %s", again.submissionID, firstID)
	}
	// 无二次副作用：账本只有一条、仍处 queued、文本不变。
	entries := h.queue.entriesFor("s-lead")
	if len(entries) != 1 {
		t.Fatalf("重复入队不得新增账本条目，实际 %d 条", len(entries))
	}
	if entries[0].status != SubStatusQueued || entries[0].submissionID != firstID {
		t.Fatalf("既有条目应保持 queued 原样: %+v", entries[0])
	}
}

func TestLedgerConflictFailsTaskDeterministically(t *testing.T) {
	h := newDispatchHarness(t)
	// 预置同 id 不同内容的账本条目（对账冲突形态），派发必须确定性失败。
	run := &Run{ID: "run-conflict", TeamID: h.team.ID, Goal: "g", Status: RunDispatching, Tasks: []Task{{
		ID: "design", MemberID: "lead", Prompt: "分析", Status: TaskPlanned,
		Ordinal: 0, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
	}}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	submissionID := TaskSubmissionID(h.team.ID, run.ID, "design", 1)
	if _, _, err := h.queue.Enqueue("s-lead", submissionID, "完全不同的历史内容"); err != nil {
		t.Fatalf("预置账本条目失败: %v", err)
	}
	team := h.store.Team(h.team.ID)
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	_, err := h.service.submitTaskLocked(state, team, h.store.Run(run.ID), "design")
	state.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "同一消息标识不能用于不同内容") {
		t.Fatalf("账本冲突应确定性报错，实际 %v", err)
	}
}

// ---- 结果未知：outcome_unknown 且绝不重发 ----

func TestUnknownOutcomeStopsRunWaitingInput(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	submissionID, _ := h.waitForSubmission(t, "s-lead", "lead 任务")
	// 账本结果未知；会话活着但没有本任务 turn 且空闲。
	h.queue.setStatus("s-lead", submissionID, SubStatusUnknown, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead"))
	waitFor(t, "任务落 outcome_unknown", func() bool {
		return h.taskOf(run.ID, "lead").Status == TaskOutcomeUnknown
	})
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	if got := h.queue.callCount(submissionID); got != 1 {
		t.Fatalf("结果未知绝不重发：Enqueue 应只调 1 次，实际 %d", got)
	}
	types := h.runEventTypes(run.ID)
	if !containsInOrder(types, EventTaskOutcomeUnknown, EventRunWaitingInput) {
		t.Fatalf("journal 应先记 task_outcome_unknown 再记 run_waiting_input，实际 %v", types)
	}
	// resume：未决 unknown 转 stopped（确认不再重发），全阻塞 fold 为 stopped。
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("waiting_input 应可 resume: %v", err)
	}
	after := h.store.Run(run.ID)
	if after.Tasks[0].Status != TaskStopped {
		t.Fatalf("resume 后未决 unknown 应转 stopped，实际 %s", after.Tasks[0].Status)
	}
	if after.Status != RunStopped {
		t.Fatalf("全阻塞 fold 应为 stopped，实际 %s", after.Status)
	}
	if !strings.Contains(after.StopReason, "不再重发") {
		t.Fatalf("fold 原因应说明跳过： %q", after.StopReason)
	}
	if got := h.queue.callCount(submissionID); got != 1 {
		t.Fatalf("resume 不得重发: %d 次", got)
	}
	// 终态后的重复 resume / cancel 均拒绝（HTTP 侧映射 409）。
	if _, err := h.service.Resume(run.ID); !errors.Is(err, ErrRunNotWaiting) {
		t.Fatalf("终态 resume 应拒绝: %v", err)
	}
	if _, err := h.service.Cancel(run.ID); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("终态 cancel 应拒绝: %v", err)
	}
}

// ---- watcher turn 分类（表驱动，真实 acpview 字段形态）----

func TestWatcherClassifiesTurnOutcomes(t *testing.T) {
	cases := []struct {
		name     string
		drive    func(text string) func(*acpview.View)
		wantTask string
		wantRun  string
		verify   func(t *testing.T, task Task)
	}{
		{
			name: "completed",
			drive: func(text string) func(*acpview.View) {
				return turnCompleted(text, "发现两个问题。")
			},
			wantTask: TaskCompleted, wantRun: RunCompleted,
			verify: func(t *testing.T, task Task) {
				if task.ResultDetail == nil || task.ResultDetail.Result != "发现两个问题。" {
					t.Fatalf("结果应即时入账（§6.5），实际 %+v", task.ResultDetail)
				}
				if task.ResultDetail.TurnID == "" || task.ResultDetail.ResultChars != 7 {
					t.Fatalf("结果引用字段错误: %+v", task.ResultDetail)
				}
			},
		},
		{
			name: "failed",
			drive: func(text string) func(*acpview.View) {
				return turnFailedView(text, "管道写入失败")
			},
			wantTask: TaskFailed, wantRun: RunFailed,
			verify: func(t *testing.T, task Task) {
				if task.Error != "管道写入失败" {
					t.Fatalf("错误文本应取 turn messages 的 kind==error 条目，实际 %q", task.Error)
				}
			},
		},
		{
			name: "interrupted",
			drive: func(text string) func(*acpview.View) {
				return turnInterrupted(text)
			},
			wantTask: TaskStopped, wantRun: RunStopped,
			verify: func(t *testing.T, task Task) {
				if task.StopReason != "cancelled" {
					t.Fatalf("中断应落 stopped(stopReason=cancelled)，实际 %q", task.StopReason)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			h := newDispatchHarness(t)
			run := h.createRun(t, ModeSingle, nil)
			submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
			exit := h.grabWatcherExit(t, run.ID, "lead")
			h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
			h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead", testCase.drive(sentText)))
			waitFor(t, "任务终态", func() bool {
				return terminalTask(h.taskOf(run.ID, "lead").Status)
			})
			task := h.taskOf(run.ID, "lead")
			if task.Status != testCase.wantTask {
				t.Fatalf("任务应落 %s，实际 %s", testCase.wantTask, task.Status)
			}
			waitFor(t, "run 终态", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
			if status := h.store.Run(run.ID).Status; status != testCase.wantRun {
				t.Fatalf("run 应 fold 为 %s，实际 %s", testCase.wantRun, status)
			}
			testCase.verify(t, task)
			// watcher 在终态后退出（channel 关闭断言）。
			waitForClosed(t, "watcher 退出", exit)
			if !containsInOrder(h.runEventTypes(run.ID), EventTaskDispatching, EventTaskRunning, taskTerminalEvent(task.Status), runStatusEvent(testCase.wantRun)) {
				t.Fatalf("journal 事件序错误: %v", h.runEventTypes(run.ID))
			}
		})
	}
}

func taskTerminalEvent(status string) string {
	switch status {
	case TaskCompleted:
		return EventTaskCompleted
	case TaskFailed:
		return EventTaskFailed
	case TaskStopped:
		return EventTaskStopped
	case TaskOutcomeUnknown:
		return EventTaskOutcomeUnknown
	}
	return ""
}

func containsInOrder(items []string, want ...string) bool {
	at := 0
	for _, item := range items {
		if at < len(want) && item == want[at] {
			at++
		}
	}
	return at == len(want)
}

// ---- 取消：先记账后副作用，watcher no-op ----

func TestCancelRecordsBeforeCancellingTurn(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	exit := h.grabWatcherExit(t, run.ID, "lead")
	// 本任务 turn 运行中。
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead", turnInProgress(sentText)))
	waitFor(t, "任务 running", func() bool { return h.taskOf(run.ID, "lead").Status == TaskRunning })

	// cancelAgentTurn 钩子：Cancel 在测试 goroutine 内同步执行副作用，可在
	// 钩子里安全断言「取消 turn 之前任务已 cancelled」。
	var cancelCalls int
	var statusAtCancel, stopReasonAtCancel, journalTailAtCancel string
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		cancelCalls++
		statusAtCancel = h.taskOf(run.ID, "lead").Status
		stopReasonAtCancel = h.taskOf(run.ID, "lead").StopReason
		journalTailAtCancel = lastEventType(h, run.ID)
		return true
	}
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if cancelCalls != 1 {
		t.Fatalf("应取消一次本任务 turn，实际 %d 次", cancelCalls)
	}
	if statusAtCancel != TaskCancelled || stopReasonAtCancel != "用户取消运行" {
		t.Fatalf("取消 turn 前任务必须已落 cancelled（先记账后副作用），实际 %s/%s",
			statusAtCancel, stopReasonAtCancel)
	}
	if journalTailAtCancel != EventRunCancelled {
		t.Fatalf("取消 turn 前 journal 应已记到 run_cancelled，实际尾事件 %s", journalTailAtCancel)
	}
	after := h.store.Run(run.ID)
	if after.Status != RunCancelled {
		t.Fatalf("run 应落 cancelled，实际 %s", after.Status)
	}
	// watcher 对已终态任务 no-op：退出且不推翻取消结论。
	waitForClosed(t, "watcher 退出", exit)
	if task := h.taskOf(run.ID, "lead"); task.Status != TaskCancelled {
		t.Fatalf("watcher 不得推翻取消终态，实际 %s", task.Status)
	}
	if !containsInOrder(h.runEventTypes(run.ID), EventTaskStopped, EventRunCancelled) {
		t.Fatalf("journal 应先记任务取消再记 run_cancelled，实际 %v", h.runEventTypes(run.ID))
	}
}

func lastEventType(h *dispatchHarness, runID string) string {
	types := h.runEventTypes(runID)
	if len(types) == 0 {
		return ""
	}
	return types[len(types)-1]
}

func TestCancelWithdrawsQueuedSubmissionWithoutTouchingSession(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	_, _ = h.waitForSubmission(t, "s-lead", "lead 任务")
	cancelCalls := 0
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		cancelCalls++
		return true
	}
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	// 仍在队列 → 撤账本条目；未派发 → 不碰会话。
	entries := h.queue.entriesFor("s-lead")
	if len(entries) != 1 || entries[0].status != SubStatusCancelled {
		t.Fatalf("queued 条目应被撤销: %+v", entries)
	}
	if cancelCalls != 0 {
		t.Fatalf("未派发任务不应取消会话 turn，实际 %d 次", cancelCalls)
	}
	if task := h.taskOf(run.ID, "lead"); task.Status != TaskCancelled {
		t.Fatalf("任务应落 cancelled，实际 %s", task.Status)
	}
}

// ---- 超时 ----

func TestTaskTimeoutFailsTaskThenCancelsOwnTurn(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.TaskTimeoutSecs = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := h.createRun(t, ModeSingle, nil)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead", turnInProgress(sentText)))
	waitFor(t, "任务 running", func() bool { return h.taskOf(run.ID, "lead").Status == TaskRunning })

	var statusAtCancel, stopReasonAtCancel string
	var cancelCalls int
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		cancelCalls++
		statusAtCancel = h.taskOf(run.ID, "lead").Status
		stopReasonAtCancel = h.taskOf(run.ID, "lead").StopReason
		return true
	}
	waitFor(t, "任务超时落 failed", func() bool { return h.taskOf(run.ID, "lead").Status == TaskFailed })
	task := h.taskOf(run.ID, "lead")
	if task.StopReason != "task_timeout" {
		t.Fatalf("超时应记 stopReason=task_timeout，实际 %q", task.StopReason)
	}
	if cancelCalls != 1 || statusAtCancel != TaskFailed || stopReasonAtCancel != "task_timeout" {
		t.Fatalf("超时应先标终态再取消 turn，实际 calls=%d status=%s reason=%s",
			cancelCalls, statusAtCancel, stopReasonAtCancel)
	}
	waitFor(t, "run fold failed", func() bool { return h.store.Run(run.ID).Status == RunFailed })
}

func TestRunTimeoutFailsRunAndWithdrawsQueue(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.RunTimeoutSecs = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := h.createRun(t, ModeSingle, nil)
	_, _ = h.waitForSubmission(t, "s-lead", "lead 任务")
	// 不推进账本（保持 queued）：run 超时兜底。
	waitFor(t, "run 超时落 failed", func() bool { return h.store.Run(run.ID).Status == RunFailed })
	task := h.taskOf(run.ID, "lead")
	if task.Status != TaskFailed || task.StopReason != "run_timeout" {
		t.Fatalf("在途任务应随 run 超时终止，实际 %s/%s", task.Status, task.StopReason)
	}
	if entry, ok := h.queue.firstEntry("s-lead"); !ok || entry.status != SubStatusCancelled {
		t.Fatalf("仍在队列的条目应随超时撤销: %+v", entry)
	}
}

// ---- 流水线：依赖执行、有界上下文注入、写锁 ----

func TestPipelineRunsByDependencyWithBoundedContext(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, pipelinePlan())

	// design 先行：依赖未完成时下游不派发。
	designSub, designText := h.waitForSubmission(t, "s-lead", "design")
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("依赖未完成不应派发下游: %+v", entries)
	}
	h.completeSubmission("s-lead", designSub, designText, "设计方案：分三步走。")

	// implement 派发：sentText 带完整 §7.1 信封。
	implementSub, implementText := h.waitForSubmission(t, "s-dev", "implement")
	for _, want := range []string{"[Team context]", "来源任务：design", "设计方案：分三步走。", "[Assigned task]", "实现接口"} {
		if !strings.Contains(implementText, want) {
			t.Fatalf("implement 的注入文本应含 %q:\n%s", want, implementText)
		}
	}
	// exclusive 写锁由 implement 持有。
	waitFor(t, "写锁登记", func() bool { return h.service.locks.Holder("/tmp/dev") == "implement" })
	h.completeSubmission("s-dev", implementSub, implementText, "实现完成：改了 3 个文件。")

	// review 派发：每轮只注入上一轮摘要（§7.3），design 的结果不再出现。
	reviewSub, reviewText := h.waitForSubmission(t, "s-rev", "review")
	if !strings.Contains(reviewText, "来源任务：implement") || strings.Contains(reviewText, "来源任务：design") {
		t.Fatalf("review 应只注入上一轮摘要:\n%s", reviewText)
	}
	h.completeSubmission("s-rev", reviewSub, reviewText, "审查通过。")

	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	for _, taskID := range []string{"design", "implement", "review"} {
		if task := h.taskOf(run.ID, taskID); task.Status != TaskCompleted || task.Result == "" {
			t.Fatalf("任务 %s 应带结果完成: %+v", taskID, task)
		}
	}
	// 写锁随终态释放。
	if holder := h.service.locks.Holder("/tmp/dev"); holder != "" {
		t.Fatalf("写锁应随终态释放，实际持有者 %q", holder)
	}
	// 完整事件序（每任务 dispatching → running → completed，run 收尾）。
	want := []string{
		EventRunCreated, EventTaskPlanned, EventTaskPlanned, EventTaskPlanned,
		EventTaskDispatching, EventTaskRunning, EventTaskCompleted,
		EventTaskDispatching, EventTaskRunning, EventTaskCompleted,
		EventTaskDispatching, EventTaskRunning, EventTaskCompleted,
		EventRunCompleted,
	}
	if got := h.runEventTypes(run.ID); !equalStrings(got, want) {
		t.Fatalf("journal 事件序不符:\n got %v\nwant %v", got, want)
	}
}

func TestFailedTaskCascadesBlockedBranch(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, pipelinePlan())
	designSub, designText := h.waitForSubmission(t, "s-lead", "design")
	// design 的 turn 失败（view FailTurn 形态：kind==error 条目）。
	h.queue.setStatus("s-lead", designSub, SubStatusAccepted, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead", turnFailedView(designText, "模型拒绝执行")))
	waitFor(t, "run 失败", func() bool { return h.store.Run(run.ID).Status == RunFailed })
	if task := h.taskOf(run.ID, "design"); task.Status != TaskFailed || task.Error != "模型拒绝执行" {
		t.Fatalf("design 应带错误文本失败: %+v", task)
	}
	for _, taskID := range []string{"implement", "review"} {
		task := h.taskOf(run.ID, taskID)
		if task.Status != TaskStopped || !strings.Contains(task.StopReason, "依赖分支已终止") {
			t.Fatalf("下游 %s 应级联终止，实际 %+v", taskID, task)
		}
	}
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("失败分支的下游不应入队: %+v", entries)
	}
	if !strings.Contains(h.store.Run(run.ID).StopReason, "模型拒绝执行") {
		t.Fatalf("run 失败原因应携带首要任务错误: %q", h.store.Run(run.ID).StopReason)
	}
}

// ---- 多任务同会话按文本匹配 ----

func TestWatcherMatchesOwnTurnByText(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	// 会话已有无关历史回合 + 本任务 turn 进行中：必须匹配本任务。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnCompleted("用户早前手动发起的聊天", "历史回复"),
		turnInProgress(sentText)))
	waitFor(t, "匹配到本任务 turn", func() bool { return h.taskOf(run.ID, "lead").Status == TaskRunning })

	// 同文本双回合（重放/接力形态）：匹配最新一个，结果取最新回合。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnCompleted(sentText, "上一轮结果"),
		turnCompleted(sentText, "本轮结果")))
	waitFor(t, "任务完成", func() bool { return h.taskOf(run.ID, "lead").Status == TaskCompleted })
	if result := h.taskOf(run.ID, "lead").Result; result != "本轮结果" {
		t.Fatalf("应按最新回合取结果，实际 %q", result)
	}
}

// ---- Resume：可继续分支重启 ----

func TestResumeRedispatchesHealthyBranchOnly(t *testing.T) {
	h := newDispatchHarness(t)
	plan := []PlanTaskInput{
		{ID: "a", MemberID: "lead", Prompt: "分析", WriteLock: TaskWriteLockNone},
		{ID: "b", MemberID: "dev", Prompt: "实现", WriteLock: TaskWriteLockNone},
		{ID: "c", MemberID: "rev", Prompt: "审查", DependsOn: []string{"b"}, WriteLock: TaskWriteLockNone},
	}
	run := h.createRun(t, ModePipeline, plan)
	subA, _ := h.waitForSubmission(t, "s-lead", "任务 a")
	subB, textB := h.waitForSubmission(t, "s-dev", "任务 b")
	// 派发中的 run 不允许 resume。
	if _, err := h.service.Resume(run.ID); !errors.Is(err, ErrRunNotWaiting) {
		t.Fatalf("非 waiting_input 的 resume 应拒绝: %v", err)
	}
	// a 结果未知 → run 停 waiting_input；b 仍在途收尾。
	h.queue.setStatus("s-lead", subA, SubStatusUnknown, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead"))
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	h.completeSubmission("s-dev", subB, textB, "实现完成。")
	waitFor(t, "b 收尾入账", func() bool { return h.taskOf(run.ID, "b").Status == TaskCompleted })

	// resume：a 转 stopped（不再重发），c 的依赖已就绪 → 重新派发。
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	subC, textC := h.waitForSubmission(t, "s-rev", "任务 c")
	if !strings.Contains(textC, "来源任务：b") || !strings.Contains(textC, "实现完成。") {
		t.Fatalf("c 的上下文应含 b 的结果:\n%s", textC)
	}
	if got := h.queue.callCount(subA); got != 1 {
		t.Fatalf("未决 unknown 不得重发: %d 次", got)
	}
	h.completeSubmission("s-rev", subC, textC, "审查通过。")
	waitFor(t, "run 终态", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
	after := h.store.Run(run.ID)
	if after.Status != RunStopped {
		t.Fatalf("存在 stopped 任务应 fold 为 stopped，实际 %s", after.Status)
	}
	if task := h.taskOf(run.ID, "a"); task.Status != TaskStopped {
		t.Fatalf("a 应转 stopped，实际 %s", task.Status)
	}
}

// ---- 合成全文上限（派发期联合用例）----

func TestOversizedPromptFailsTaskBeforeEnqueue(t *testing.T) {
	h := newDispatchHarness(t)
	// 40000 个 CJK rune（120000 字节）：planner 按 rune 校验放行（<87800），
	// 账本按字节拒绝——SubmitTask 预检应把任务确定性拦下，绝不入队。
	goal := strings.Repeat("字", 40000)
	run, err := h.service.CreateRun(h.team, goal, ModeSingle, nil)
	if err != nil {
		t.Fatalf("planner 应放行 rune 限内的目标: %v", err)
	}
	waitFor(t, "任务确定性失败", func() bool {
		return terminalTask(h.taskOf(run.ID, "lead").Status)
	})
	task := h.taskOf(run.ID, "lead")
	if task.Status != TaskFailed || !strings.Contains(task.Error, "超过账本上限") {
		t.Fatalf("超限应确定性失败并说明原因: %+v", task)
	}
	if entries := h.queue.entriesFor("s-lead"); len(entries) != 0 {
		t.Fatalf("超限任务不得入队: %+v", entries)
	}
	waitFor(t, "run 失败", func() bool { return h.store.Run(run.ID).Status == RunFailed })
}

func TestLargeButLegalPromptAndContextPassLedger(t *testing.T) {
	h := newDispatchHarness(t)
	// planner 上限内的 ASCII prompt（87800 字）+ 依赖上下文：合成全文应过
	// 账本字节校验（上下文按字节兜底收缩）。
	plan := []PlanTaskInput{
		{ID: "design", MemberID: "lead", Prompt: "分析", WriteLock: TaskWriteLockNone},
		{ID: "impl", MemberID: "dev", Prompt: strings.Repeat("p", 87800), DependsOn: []string{"design"}, WriteLock: TaskWriteLockNone},
	}
	run := h.createRun(t, ModePipeline, plan)
	designSub, designText := h.waitForSubmission(t, "s-lead", "design")
	h.completeSubmission("s-lead", designSub, designText, strings.Repeat("果", 12000))
	subImpl, textImpl := h.waitForSubmission(t, "s-dev", "impl")
	if len(textImpl) > 100000 {
		t.Fatalf("合成全文 %d 字节超过账本上限", len(textImpl))
	}
	h.completeSubmission("s-dev", subImpl, textImpl, "实现完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

// ---- 重启恢复（§8.3 I/O 侧）：不重发 ----

// seedInflightRun 手工构造「journal 已记账、快照 dispatching」的在途 run
// （与 dispatcher 的落账序一致），并按需预置账本条目。
func (h *dispatchHarness) seedInflightRun(t *testing.T, runID string, seedQueue func(submissionID string)) *Run {
	t.Helper()
	task := Task{
		ID: "design", MemberID: "lead", Prompt: "分析", Status: TaskDispatching,
		Ordinal: 0, Attempt: 1, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
	}
	run := &Run{ID: runID, TeamID: h.team.ID, Goal: "g", Status: RunDispatching, Tasks: []Task{task}}
	policy := policyOf(h.team)
	// 先记账：run_created / task_planned / task_dispatching（真实派发哈希）。
	for _, event := range []*Event{
		newEvent(runID, EventRunCreated, "user", map[string]any{"mode": ModeSingle, "taskCount": jsoncompat.Num(1)}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "design", "memberId": "lead"}),
	} {
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	task.InputHash = InputHashFor(&task, PolicyDigest(policy), "")
	submissionID := TaskSubmissionID(h.team.ID, runID, "design", 1)
	task.SubmissionID = submissionID
	run.Tasks[0] = task
	dispatch := newEvent(runID, EventTaskDispatching, "runner", map[string]any{
		"taskId": "design", "sessionId": "s-lead", "submissionId": submissionID,
		"inputHash": task.InputHash, "attempt": jsoncompat.Num(1),
	})
	dispatch.IdempotencyKey = TaskIdempotencyKey(h.team.ID, runID, "design", 1)
	if err := h.journal.Append(dispatch); err != nil {
		t.Fatalf("journal 预置失败: %v", err)
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if seedQueue != nil {
		seedQueue(submissionID)
	}
	return run
}

func TestRecoverRunAdoptsQueuedWithoutResend(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.seedInflightRun(t, "run-rec-queued", func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-lead", submissionID, ComposeTaskMessage("", "分析")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
	})
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 账本 queued → 保持 dispatching、收养 watcher，不得再次入账。
	waitFor(t, "watcher 收养", func() bool {
		state := h.service.stateFor(run.ID)
		state.mu.Lock()
		defer state.mu.Unlock()
		_, watched := state.watchers["design"]
		return watched
	})
	if got := h.queue.callCount(run.Tasks[0].SubmissionID); got != 1 {
		t.Fatalf("恢复不得重发：Enqueue 应只 1 次，实际 %d", got)
	}
	// 收养的 watcher 继续收尾：推进 accepted + 完成回合 → run 完成。
	entry, _ := h.queue.firstEntry("s-lead")
	h.completeSubmission("s-lead", entry.submissionID, entry.text, "分析完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

func TestRecoverRunLedgerEmptyRollsBackToReady(t *testing.T) {
	h := newDispatchHarness(t)
	// journal 已记账、enqueue 未落盘的崩溃窗口：provably 未达发送路径。
	run := h.seedInflightRun(t, "run-rec-empty", nil)
	submissionID := run.Tasks[0].SubmissionID
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "回 ready 后重新派发", func() bool {
		return h.taskOf(run.ID, "design").Status == TaskDispatching
	})
	// 重新派发沿用同一确定性 submissionID，全新入账（此前账本确无条目）。
	waitFor(t, "入队", func() bool {
		_, ok := h.queue.firstEntry("s-lead")
		return ok
	})
	entry, _ := h.queue.firstEntry("s-lead")
	if entry.submissionID != submissionID {
		t.Fatalf("重派发应沿用同一 submissionID: %s vs %s", entry.submissionID, submissionID)
	}
	if got := h.queue.callCount(submissionID); got != 1 {
		t.Fatalf("崩溃窗口重派发只应入账一次，实际 %d", got)
	}
	h.completeSubmission("s-lead", entry.submissionID, entry.text, "分析完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

func TestRecoverRunAcceptedRebuildsWatcher(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.seedInflightRun(t, "run-rec-accepted", func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-lead", submissionID, ComposeTaskMessage("", "分析")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
		h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
			turnCompleted(ComposeTaskMessage("", "分析"), "已完成的分析")))
	})
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// accepted + 会话活 → running + watcher 收养 → 完成收尾。
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	if got := h.queue.callCount(run.Tasks[0].SubmissionID); got != 1 {
		t.Fatalf("恢复不得重发: %d 次", got)
	}
}

func TestRecoverRunUnknownParksWaitingInput(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.seedInflightRun(t, "run-rec-unknown", func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-lead", submissionID, ComposeTaskMessage("", "分析")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
		h.queue.setStatus("s-lead", submissionID, SubStatusUnknown, "")
		h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead"))
	})
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "任务落 outcome_unknown", func() bool {
		return h.taskOf(run.ID, "design").Status == TaskOutcomeUnknown
	})
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	if got := h.queue.callCount(run.Tasks[0].SubmissionID); got != 1 {
		t.Fatalf("结果未知不得重发: %d 次", got)
	}
}

// ---- 写锁生命周期：取消释放、重启收养重建（§6.4 规则 2）----

// TestCancelReleasesProjectWriteLock：带在途 exclusive 任务的 Run 被取消后，
// 项目写锁必须立即释放——否则 LockRegistry 的登记残留到进程重启，同 cwd
// 的后续 exclusive 任务（任何 Run/Team）永远无法派发，且投影
// （writeLockHeldBy 按任务状态推导）显示无持有者，掩盖死锁。
func TestCancelReleasesProjectWriteLock(t *testing.T) {
	h := newDispatchHarness(t)
	// lead 与 dev 的会话指向同一 cwd（跨成员写互斥只来自写锁）。
	h.resolver.setCwd("s-lead", "/shared-ws")
	h.resolver.setCwd("s-dev", "/shared-ws")
	run1 := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "w1", MemberID: "lead", Prompt: "独占写入一", WriteLock: TaskWriteLockExclusive},
	})
	_, _ = h.waitForSubmission(t, "s-lead", "w1 派发")
	waitFor(t, "w1 持锁", func() bool { return h.service.locks.Holder("/shared-ws") == "w1" })

	if _, err := h.service.Cancel(run1.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if holder := h.service.locks.Holder("/shared-ws"); holder != "" {
		t.Fatalf("取消后写锁应立即释放，实际持有者 %q", holder)
	}

	// 同 cwd 的后续 Run 必须能立即派发并持锁（锁不残留的端到端证据）。
	h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "w2", MemberID: "dev", Prompt: "独占写入二", WriteLock: TaskWriteLockExclusive},
	})
	_, _ = h.waitForSubmission(t, "s-dev", "w2 派发")
	waitFor(t, "w2 持锁", func() bool { return h.service.locks.Holder("/shared-ws") == "w2" })
}

// TestRecoverRunReacquiresWriteLockForAdoptedTask：重启恢复收养在途 exclusive
// 任务时必须重取 cwd 写锁（LockRegistry 纯内存、进程重启即空）——否则同一
// cwd 的其他 exclusive 任务会因 Holder 为空被立即派发，与被收养任务并发写
// 同一工作区（§6.4 规则 2 跨重启存活）。
func TestRecoverRunReacquiresWriteLockForAdoptedTask(t *testing.T) {
	h := newDispatchHarness(t)
	h.resolver.setCwd("s-lead", "/shared-ws")
	h.resolver.setCwd("s-dev", "/shared-ws")
	const runID = "run-rec-lock"
	policy := policyOf(h.team)
	design := Task{ID: "design", MemberID: "lead", Prompt: "分析", Status: TaskDispatching,
		Ordinal: 0, Attempt: 1, WriteLock: TaskWriteLockExclusive, WorkspaceMode: WorkspaceShared}
	design.InputHash = InputHashFor(&design, PolicyDigest(policy), "")
	design.SubmissionID = TaskSubmissionID(h.team.ID, runID, "design", 1)
	build := Task{ID: "build", MemberID: "dev", Prompt: "实现", Status: TaskPlanned,
		Ordinal: 1, WriteLock: TaskWriteLockExclusive, WorkspaceMode: WorkspaceShared}
	run := &Run{ID: runID, TeamID: h.team.ID, Goal: "g", Status: RunDispatching,
		Tasks: []Task{design, build}}
	// journal 事实：run_created + 双任务 planned + design 已记账派发。
	for _, event := range []*Event{
		newEvent(runID, EventRunCreated, "user", map[string]any{"teamId": h.team.ID, "mode": ModePipeline, "taskCount": jsoncompat.Num(2)}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "design", "memberId": "lead"}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "build", "memberId": "dev"}),
	} {
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	dispatchEvent := newEvent(runID, EventTaskDispatching, "runner", map[string]any{
		"taskId": "design", "sessionId": "s-lead", "submissionId": design.SubmissionID,
		"inputHash": design.InputHash, "attempt": jsoncompat.Num(1),
	})
	dispatchEvent.IdempotencyKey = TaskIdempotencyKey(h.team.ID, runID, "design", 1)
	if err := h.journal.Append(dispatchEvent); err != nil {
		t.Fatalf("journal 预置失败: %v", err)
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.queue.Enqueue("s-lead", design.SubmissionID, ComposeTaskMessage("", "分析")); err != nil {
		t.Fatalf("预置账本条目失败: %v", err)
	}
	// build 的入队时刻必须晚于 design 终态（写锁互斥的顺序证据）。
	designTerminalAtBuildEnqueue := false
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		if threadID == "s-dev" {
			designTerminalAtBuildEnqueue = terminalTask(h.taskOf(runID, "design").Status)
		}
	}

	if err := h.service.RecoverRun(runID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 收养即重取写锁：Holder 指向 design；build 在 design 落定前不得入队。
	waitFor(t, "收养重建写锁", func() bool { return h.service.locks.Holder("/shared-ws") == "design" })
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("design 在途期间 build 不得入队（写锁互斥跨重启存活）: %+v", entries)
	}

	// design 收尾 → 锁释放 → build 派发 → 全部完成。
	entry, _ := h.queue.firstEntry("s-lead")
	h.completeSubmission("s-lead", entry.submissionID, entry.text, "分析完成。")
	subBuild, textBuild := h.waitForSubmission(t, "s-dev", "build 派发")
	if !designTerminalAtBuildEnqueue {
		t.Fatal("build 的派发必须发生在 design 终态之后（写锁互斥）")
	}
	h.completeSubmission("s-dev", subBuild, textBuild, "实现完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(runID).Status == RunCompleted })
	if holder := h.service.locks.Holder("/shared-ws"); holder != "" {
		t.Fatalf("完成后写锁应释放，实际持有者 %q", holder)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
