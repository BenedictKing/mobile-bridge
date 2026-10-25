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
	"os"
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
	run, err := h.service.CreateRun(h.team, "", "实现用户头像上传接口", mode, "", nil, plan)
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

// waitForEntry 等到 threadID 的首条入账并返回条目（含状态，供闸门放行后
// 观察入队落账完成）。
func (h *dispatchHarness) waitForEntry(t *testing.T, threadID, what string) (fakeQueueEntry, bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if entry, ok := h.queue.firstEntry(threadID); ok {
			return entry, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
	return fakeQueueEntry{}, false
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
	cancelled := make(chan struct{})
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		cancelCalls++
		statusAtCancel = h.taskOf(run.ID, "lead").Status
		stopReasonAtCancel = h.taskOf(run.ID, "lead").StopReason
		// watcher goroutine 落完断言变量后发信号：主 goroutine 不得在写入
		// 完成前读取（channel 同步，§13.1；本用例断言 cancelCalls==1，
		// 二次调用会因重复 close 暴露）。
		close(cancelled)
		return true
	}
	waitFor(t, "任务超时落 failed", func() bool { return h.taskOf(run.ID, "lead").Status == TaskFailed })
	waitForClosed(t, "超时取消 turn 副作用完成", cancelled)
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
	// 40000 个 CJK rune（120000 字节）：rune 在 87800 限内、字节远超创建期
	// 上限（评审遗留②）——创建即拒，不再建成注定在派发期才失败的 Run。
	goal := strings.Repeat("字", 40000)
	if _, err := h.service.CreateRun(h.team, "", goal, ModeSingle, "", nil, nil); err == nil || !strings.Contains(err.Error(), "字节") {
		t.Fatalf("超字节上限的多字节目标应在创建期拒绝，实际 %v", err)
	}
	// 派发期预检仍是防御底线（旧账本/直写快照可能绕过创建校验）：直落一个
	// 超限 prompt 的 planned 任务，调度循环派发时应确定性失败、绝不入队。
	run := &Run{
		ID: "run-oversize", TeamID: h.team.ID, Goal: "g", Status: RunDispatching, Round: 1,
		Tasks: []Task{{
			ID: "big", MemberID: "lead", Prompt: strings.Repeat("字", 40000),
			Status: TaskPlanned, Attempt: 0, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
		}},
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复启动调度循环失败: %v", err)
	}
	waitFor(t, "任务确定性失败", func() bool {
		return terminalTask(h.taskOf(run.ID, "big").Status)
	})
	task := h.taskOf(run.ID, "big")
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
	// 创建期字节上限内（63488）的 ASCII prompt（63000 字节）+ 依赖上下文：
	// 合成全文应过账本字节校验（上下文按字节兜底收缩）。
	plan := []PlanTaskInput{
		{ID: "design", MemberID: "lead", Prompt: "分析", WriteLock: TaskWriteLockNone},
		{ID: "impl", MemberID: "dev", Prompt: strings.Repeat("p", 63000), DependsOn: []string{"design"}, WriteLock: TaskWriteLockNone},
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

// ---- 派发全文落盘（遗留④）：收养/取消不随策略漂移 ----

// TestCancelUsesPersistedSentTextAfterPolicyChange 派发全文随 task_dispatching
// 落快照；任务在途期间团队策略变更后取消，副作用目标必须仍是存量全文
// （matchTurn 按全文匹配 turn，重算文本会错认或落空）。
func TestCancelUsesPersistedSentTextAfterPolicyChange(t *testing.T) {
	h := newDispatchHarness(t)
	plan := []PlanTaskInput{
		{ID: "design", MemberID: "lead", Prompt: "分析", WriteLock: TaskWriteLockNone},
		{ID: "impl", MemberID: "dev", Prompt: "实现接口", DependsOn: []string{"design"}, WriteLock: TaskWriteLockNone},
	}
	run := h.createRun(t, ModePipeline, plan)
	designSub, designText := h.waitForSubmission(t, "s-lead", "design")
	h.completeSubmission("s-lead", designSub, designText, strings.Repeat("果", 600))
	implSub, implText := h.waitForSubmission(t, "s-dev", "impl")
	// 派发全文已随 task_dispatching 原子落盘（含依赖结果的上下文信封）。
	if task := h.taskOf(run.ID, "impl"); task.SentText != implText {
		t.Fatalf("派发全文应落快照: 快照 %q vs 账本 %q", task.SentText, implText)
	}
	// 任务在途（accepted + 本任务 turn 运行中）。
	h.queue.setStatus("s-dev", implSub, SubStatusAccepted, "")
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnInProgress(implText)))
	waitFor(t, "impl running", func() bool { return h.taskOf(run.ID, "impl").Status == TaskRunning })
	// 策略改为几乎不注入上下文：此刻重算会得到与派发全文完全不同的文本。
	teamRow := h.store.Team(h.team.ID)
	teamRow.Policy.MaxContextChars = 10
	if err := h.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	var cancelledWith string
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		cancelledWith = text
		return true
	}
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if cancelledWith != implText {
		t.Fatalf("取消目标应使用存量派发全文（策略变更后不得漂移）: %q vs %q", cancelledWith, implText)
	}
}

// TestAdoptUsesPersistedSentTextAfterPolicyChange 重启收养同样用存量全文：
// 构造「依赖已 completed、实现任务在途、SentText 已落盘」的 run，恢复前把
// 团队策略改成会剧烈改变重算结果的值——收养的 watcher 必须按存量文本匹配
// 到本任务 turn 并完成收尾（重算文本会 matchTurn 落空、任务永远停不了）。
func TestAdoptUsesPersistedSentTextAfterPolicyChange(t *testing.T) {
	h := newDispatchHarness(t)
	result := strings.Repeat("果", 600)
	design := Task{
		ID: "design", MemberID: "lead", Prompt: "分析", Status: TaskCompleted, Ordinal: 0,
		Result: result, ResultDetail: &TaskResult{SessionID: "s-lead", TurnID: "t1", Result: result, ResultChars: len([]rune(result))},
		WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
	}
	impl := Task{
		ID: "impl", MemberID: "dev", Prompt: "实现接口", Status: TaskDispatching, Ordinal: 1,
		DependsOn: []string{"design"}, Attempt: 1, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
	}
	shell := &Run{ID: "run-adopt", TeamID: h.team.ID, Tasks: []Task{design, impl}}
	context := BuildTaskContext(shell, &shell.Tasks[1], policyOf(h.team))
	if context.Text == "" {
		t.Fatal("测试构造错误：依赖结果应注入上下文")
	}
	sentText := ComposeTaskMessage(context.Text, impl.Prompt)
	impl.InputHash = InputHashFor(&impl, PolicyDigest(policyOf(h.team)), context.Text)
	impl.SubmissionID = TaskSubmissionID(h.team.ID, shell.ID, "impl", 1)
	impl.SentText = sentText
	run := &Run{ID: shell.ID, TeamID: h.team.ID, Goal: "g", Status: RunDispatching, Round: 1,
		Tasks: []Task{design, impl}}
	// journal 事实与快照一致（design 的 completed 必须有事件佐证，否则恢复
	// 对账会把快照终态判成缺证未知）。
	for _, event := range []*Event{
		newEvent(run.ID, EventRunCreated, "user", map[string]any{"mode": ModePipeline, "taskCount": jsoncompat.Num(2)}),
		newEvent(run.ID, EventTaskPlanned, "user", map[string]any{"taskId": "design", "memberId": "lead"}),
		newEvent(run.ID, EventTaskPlanned, "user", map[string]any{"taskId": "impl", "memberId": "dev"}),
		newEvent(run.ID, EventTaskCompleted, "watcher", map[string]any{
			"taskId": "design", "sessionId": "s-lead", "turnId": "t1",
			"result": result, "resultChars": jsoncompat.Num(len([]rune(result))), "truncated": false,
		}),
		newEvent(run.ID, EventTaskDispatching, "runner", map[string]any{
			"taskId": "impl", "sessionId": "s-dev", "submissionId": impl.SubmissionID,
			"inputHash": impl.InputHash, "attempt": jsoncompat.Num(1),
		}),
	} {
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.queue.Enqueue("s-dev", impl.SubmissionID, sentText); err != nil {
		t.Fatalf("预置账本条目失败: %v", err)
	}
	h.queue.setStatus("s-dev", impl.SubmissionID, SubStatusAccepted, "")
	// 会话里本任务的 turn 已按「存量派发全文」完成——重算文本（策略改变后）
	// 匹配不到这个 turn。
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnCompleted(sentText, "实现完成。")))
	// 策略改成几乎不注入上下文，再走重启恢复：收养必须用存量全文。
	teamRow := h.store.Team(h.team.ID)
	teamRow.Policy.MaxContextChars = 10
	if err := h.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "收养后按存量全文完成收尾", func() bool {
		return h.store.Run(run.ID).Status == RunCompleted
	})
	if got := h.queue.callCount(impl.SubmissionID); got != 1 {
		t.Fatalf("收养不得重发：Enqueue 应只 1 次，实际 %d", got)
	}
	if task := h.taskOf(run.ID, "impl"); task.Status != TaskCompleted {
		t.Fatalf("impl 应 completed，实际 %s", task.Status)
	}
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

// admissionHook 把函数适配成 DispatchAdmission（测试注入缝）。
type admissionHook func(task *Task, member *TeamMember, session *acpruntime.Session) bool

func (f admissionHook) Admit(task *Task, member *TeamMember, session *acpruntime.Session) bool {
	return f(task, member, session)
}

// TestCancelDuringDispatchWindowReleasesWriteLock：派发池 worker 在锁外慢
// I/O（准入缝模拟 Resolve 冷探测/rev-parse 的秒级窗口）期间被 Cancel 抢先
// 落账——accountDispatchLocked 的 commitRun 复查发现任务已终态时必须：
// ① 释放刚获取的写锁（终态任务不经过任何 release 路径、run 终态后调度循环
// 也已退出，残留即永久泄漏，同 cwd 的 exclusive 任务从此无法派发，且
// writeLockHeldBy 投影显示无持有者掩盖死锁）；② 不入队（孤儿账本条目会被
// 3s worker 真正发给 Agent）。
func TestCancelDuringDispatchWindowReleasesWriteLock(t *testing.T) {
	h := newDispatchHarness(t)
	h.resolver.setCwd("s-lead", "/shared-ws")
	h.resolver.setCwd("s-dev", "/shared-ws")
	// exclusive 任务的 cwd 解析依赖会话（Admission 缝也只对有会话者生效）。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/shared-ws"))
	entered, release := make(chan struct{}), make(chan struct{})
	h.service.Admission = admissionHook(func(task *Task, member *TeamMember, session *acpruntime.Session) bool {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return true
	})
	run := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "w1", MemberID: "lead", Prompt: "独占写入", WriteLock: TaskWriteLockExclusive},
	})
	// worker 到达准入缝：步骤①状态复查已过（任务仍 planned）、步骤③记账未达。
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("派发 worker 未到达准入缝")
	}
	// Cancel 抢先落账：任务 cancelled、run cancelled、releaseAllTaskLocks 执行
	//（此刻 w1 尚未 Acquire，无账可放）。
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	close(release) // 放行 worker → 步骤③记账：复查应发现 cancelled 并放弃
	// 等 worker 收尾（pendingDispatch 清除即 executeDispatch 全部出口跑完）。
	waitFor(t, "派发 worker 收尾", func() bool {
		h.service.mu.Lock()
		state := h.service.runs[run.ID]
		h.service.mu.Unlock()
		if state == nil {
			return false
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		return !state.pendingDispatch["w1"]
	})
	if task := h.taskOf(run.ID, "w1"); task.Status != TaskCancelled {
		t.Fatalf("任务应保持 cancelled: %+v", task)
	}
	// ② 无孤儿账本条目（旧实现会照常入队，条目再无人撤回）。
	if entries := h.queue.entriesFor("s-lead"); len(entries) != 0 {
		t.Fatalf("终态任务不应入队: %+v", entries)
	}
	// ① 写锁不残留：可被其他任务立即获取（同 cwd 后续 Run 可派发的直接证据）。
	if holder := h.service.locks.Holder("/shared-ws"); holder != "" {
		t.Fatalf("取消竞态后写锁不应残留，实际持有者 %q", holder)
	}
	if !h.service.locks.Acquire("/shared-ws", "probe") {
		t.Fatal("写锁应可被其他任务获取")
	}
	h.service.locks.Release("/shared-ws", "probe")
	// 端到端：同 cwd 的后续 Run 立即派发成功（泄漏会让它永远拿不到锁）。
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/shared-ws"))
	h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "w2", MemberID: "dev", Prompt: "独占写入二", WriteLock: TaskWriteLockExclusive},
	})
	_, _ = h.waitForSubmission(t, "s-dev", "w2 派发")
	waitFor(t, "w2 持锁", func() bool { return h.service.locks.Holder("/shared-ws") == "w2" })
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

// ---- broadcast 模式与派发池（§5.3/§6.3/§6.5 阶段二）----

// createBroadcastRun 建一个单条目的 broadcast Run（逐成员展开为 draft-{member}）。
func (h *dispatchHarness) createBroadcastRun(t *testing.T, collect string, memberIDs []string) *Run {
	t.Helper()
	plan := []PlanTaskInput{
		{ID: "draft", Prompt: "各写一版方案", MemberIDs: memberIDs, WriteLock: TaskWriteLockNone},
	}
	run, err := h.service.CreateRun(h.team, "", "产出方案", ModeBroadcast, collect, nil, plan)
	if err != nil {
		t.Fatalf("创建 broadcast run 失败: %v", err)
	}
	return run
}

func TestBroadcastDispatchesToBothMembersInParallel(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createBroadcastRun(t, RunCollectAll, []string{"lead", "dev"})
	// 展开任务携带组归属（§5.3：BroadcastGroup=条目 ID）。
	for taskID, member := range map[string]string{"draft-lead": "lead", "draft-dev": "dev"} {
		task := h.taskOf(run.ID, taskID)
		if task.MemberID != member || task.BroadcastGroup != "draft" {
			t.Fatalf("展开任务 %s 归属错误: member=%s group=%s", taskID, task.MemberID, task.BroadcastGroup)
		}
	}
	// 两个成员同时分派：两条入账并存，互不等待（谁也不依赖谁完成）。
	leadSub, leadText := h.waitForSubmission(t, "s-lead", "draft-lead")
	devSub, devText := h.waitForSubmission(t, "s-dev", "draft-dev")
	// 两份任务文本一致（同 prompt），但 submissionID 不同（任务槽不同）。
	if leadText != devText {
		t.Fatalf("同条目展开任务应共享 prompt:\nlead: %s\ndev: %s", leadText, devText)
	}
	if leadSub == devSub {
		t.Fatal("不同成员任务的 submissionID 必须不同")
	}
	h.completeSubmission("s-lead", leadSub, leadText, "Lead 版方案。")
	h.completeSubmission("s-dev", devSub, devText, "Dev 版方案。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	// 结果按来源合并（§12 阶段二验收）：两份结果各带自己的会话引用。
	wantResults := []struct{ taskID, session, result string }{
		{"draft-lead", "s-lead", "Lead 版方案。"},
		{"draft-dev", "s-dev", "Dev 版方案。"},
	}
	for _, want := range wantResults {
		task := h.taskOf(run.ID, want.taskID)
		if task.Status != TaskCompleted || task.Result != want.result {
			t.Fatalf("任务 %s 应带结果完成: %+v", want.taskID, task)
		}
		if task.ResultDetail == nil || task.ResultDetail.SessionID != want.session {
			t.Fatalf("任务 %s 的结果引用应指向来源会话: %+v", want.taskID, task.ResultDetail)
		}
	}
}

func TestFirstSuccessWinnerCancelsSiblingsAndWithdrawsSideEffects(t *testing.T) {
	h := newDispatchHarness(t)
	// 取消钩子在创建 Run 前装好（避免与运行中的 watcher 竞争字段写）；
	// run 变量的写经 resolver/queue 的 mutex 链与钩子读建立 happens-before。
	var run *Run
	devCancelled := make(chan struct{})
	var closeOnce sync.Once
	var devCancelCalls int
	var devStatusAtCancel, devReasonAtCancel string
	h.service.cancelAgentTurn = func(session *acpruntime.Session, text string) bool {
		if session.ID != "s-dev" || run == nil {
			return true
		}
		devCancelCalls++
		devStatusAtCancel = h.taskOf(run.ID, "draft-dev").Status
		devReasonAtCancel = h.taskOf(run.ID, "draft-dev").StopReason
		closeOnce.Do(func() { close(devCancelled) })
		return true
	}
	run = h.createBroadcastRun(t, RunCollectFirstSuccess, []string{"lead", "dev", "rev"})
	leadSub, leadText := h.waitForSubmission(t, "s-lead", "draft-lead")
	devSub, devText := h.waitForSubmission(t, "s-dev", "draft-dev")
	_, _ = h.waitForSubmission(t, "s-rev", "draft-rev")
	// dev 的回合进行中（turn 副作用路径）；rev 留在队列（账本撤回路径）。
	h.queue.setStatus("s-dev", devSub, SubStatusAccepted, "")
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnInProgress(devText)))
	waitFor(t, "dev running", func() bool { return h.taskOf(run.ID, "draft-dev").Status == TaskRunning })

	// 首个成功者（lead）完成。
	h.queue.setStatus("s-lead", leadSub, SubStatusAccepted, "")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead", turnCompleted(leadText, "Lead 版方案。")))

	// 落败分支被取消：dev 的 turn 取消前任务必须已落 cancelled+superseded。
	waitForClosed(t, "dev turn 取消副作用", devCancelled)
	if devCancelCalls != 1 || devStatusAtCancel != TaskCancelled || devReasonAtCancel != StopReasonSupersededByFirstSuccess {
		t.Fatalf("取消 turn 前落败任务必须已落 cancelled/superseded: calls=%d status=%s reason=%s",
			devCancelCalls, devStatusAtCancel, devReasonAtCancel)
	}
	// rev 仍在队列：账本条目被撤销（不留给 3s worker 派发）。
	waitFor(t, "rev 队列条目撤销", func() bool {
		if entry, ok := h.queue.firstEntry("s-rev"); ok {
			return entry.status == SubStatusCancelled
		}
		return false
	})
	for _, taskID := range []string{"draft-dev", "draft-rev"} {
		waitFor(t, taskID+" 落 cancelled", func() bool {
			return h.taskOf(run.ID, taskID).Status == TaskCancelled
		})
		task := h.taskOf(run.ID, taskID)
		if task.StopReason != StopReasonSupersededByFirstSuccess {
			t.Fatalf("落败分支停止原因应为 superseded: %+v", task)
		}
	}
	// 组满足 → run 完成，首个成功者的结果保留。
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	if task := h.taskOf(run.ID, "draft-lead"); task.Status != TaskCompleted || task.Result != "Lead 版方案。" {
		t.Fatalf("首个成功者的结果必须保留: %+v", task)
	}
	// superseded 取消记 task_stopped 事件（可审计）。
	found := false
	for _, eventType := range h.runEventTypes(run.ID) {
		if eventType == EventTaskStopped {
			found = true
		}
	}
	if !found {
		t.Fatal("superseded 取消应记 task_stopped 事件")
	}
}

func TestCollectAllKeepsCompletedResultsWhenBranchFails(t *testing.T) {
	h := newDispatchHarness(t)
	// all 收集策略（§12 阶段二验收：任一分支失败不会吞掉其他分支结果）：
	// run 可折 failed，但已完成分支的结果始终携带（salvage）。
	run := h.createBroadcastRun(t, RunCollectAll, []string{"lead", "dev"})
	leadSub, leadText := h.waitForSubmission(t, "s-lead", "draft-lead")
	devSub, devText := h.waitForSubmission(t, "s-dev", "draft-dev")
	h.completeSubmission("s-lead", leadSub, leadText, "Lead 版方案。")
	waitFor(t, "lead 完成", func() bool { return h.taskOf(run.ID, "draft-lead").Status == TaskCompleted })
	// dev 分支失败。
	h.queue.setStatus("s-dev", devSub, SubStatusAccepted, "")
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev", turnFailedView(devText, "模型拒绝执行")))
	waitFor(t, "run 折 failed（all 维持阶段一优先级）", func() bool {
		return h.store.Run(run.ID).Status == RunFailed
	})
	winner := h.taskOf(run.ID, "draft-lead")
	if winner.Status != TaskCompleted || winner.Result != "Lead 版方案。" {
		t.Fatalf("失败分支不得吞掉已完成分支的结果: %+v", winner)
	}
	if loser := h.taskOf(run.ID, "draft-dev"); loser.Status != TaskFailed || loser.Error != "模型拒绝执行" {
		t.Fatalf("失败分支应带错误文本: %+v", loser)
	}
}

// ---- 取消窗口一：信号量等待期被 Cancel ----

func TestDispatchPoolCancelWhileWaitingForSlot(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.MaxParallelTasks = 1
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	// run1 独占团队派发池唯一槽位：入队闸门挂起（executeDispatch 阻塞在
	// Enqueue 内，持有信号量）。
	gate := make(chan struct{})
	enqueueEntered := make(chan string, 8)
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		select {
		case enqueueEntered <- threadID:
		default:
		}
		if threadID == "s-lead" {
			<-gate
		}
	}
	run1 := h.createRun(t, ModeSingle, nil)
	if thread := <-enqueueEntered; thread != "s-lead" {
		t.Fatalf("首个入队应来自 run1 的 lead 任务，实际 %s", thread)
	}
	// run2 的任务投递池后在信号量上排队等待（取消窗口一）。
	run2 := h.createRun(t, ModePipeline, []PlanTaskInput{
		{ID: "w", MemberID: "dev", Prompt: "独立任务", WriteLock: TaskWriteLockNone},
	})
	waitFor(t, "run2 任务已投递派发池", func() bool {
		state := h.service.stateFor(run2.ID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.pendingDispatch["w"]
	})
	// 等待期间取消整个 run2。
	if _, err := h.service.Cancel(run2.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	// 放行 run1：槽位释放后 run2 的等待者获槽，复查终态即退出（绝不入队）。
	close(gate)
	entry, ok := h.waitForEntry(t, "s-lead", "run1 入队完成")
	if !ok {
		t.Fatal("run1 应已入队")
	}
	h.completeSubmission("s-lead", entry.submissionID, entry.text, "完成。")
	waitFor(t, "run1 完成", func() bool { return h.store.Run(run1.ID).Status == RunCompleted })
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("等待期被取消的任务绝不入队: %+v", entries)
	}
	waitFor(t, "run2 派发窗口清理", func() bool {
		state := h.service.stateFor(run2.ID)
		state.mu.Lock()
		defer state.mu.Unlock()
		return len(state.pendingDispatch) == 0
	})
	state := h.service.stateFor(run2.ID)
	state.mu.Lock()
	_, watched := state.watchers["w"]
	state.mu.Unlock()
	if watched {
		t.Fatal("未入队任务不得启动 watcher（pollTask 会误判账本条目丢失）")
	}
	if status := h.store.Run(run2.ID).Status; status != RunCancelled {
		t.Fatalf("run2 应保持 cancelled，实际 %s", status)
	}
}

// ---- 取消窗口二：记账后、入队完成前被 Cancel ----

func TestDispatchCancelDuringEnqueueWithdrawsOrphanEntry(t *testing.T) {
	h := newDispatchHarness(t)
	gate := make(chan struct{})
	enqueueEntered := make(chan struct{}, 1)
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		select {
		case enqueueEntered <- struct{}{}:
		default:
		}
		<-gate
	}
	run := h.createRun(t, ModeSingle, nil)
	waitFor(t, "任务已记账 dispatching", func() bool {
		return h.taskOf(run.ID, "lead").Status == TaskDispatching
	})
	<-enqueueEntered // Enqueue 已进入且被闸门挂起。
	// 闸门期间：watcher 尚未启动（只在 Enqueue 成功后启动）。
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	_, watched := state.watchers["lead"]
	state.mu.Unlock()
	if watched {
		t.Fatal("watcher 必须在 Enqueue 完成后才启动")
	}
	// Cancel：任务落 cancelled；Cancel 的副作用拿不到条目（尚未入账）。
	if _, err := h.service.Cancel(run.ID); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	// 放行入队：条目落账后 executeDispatch 复查终态，立即撤销孤儿条目。
	close(gate)
	waitFor(t, "孤儿条目被撤销", func() bool {
		if entry, ok := h.queue.firstEntry("s-lead"); ok {
			return entry.status == SubStatusCancelled
		}
		return false
	})
	task := h.taskOf(run.ID, "lead")
	if task.Status != TaskCancelled {
		t.Fatalf("任务应保持 cancelled，实际 %s", task.Status)
	}
	if status := h.store.Run(run.ID).Status; status != RunCancelled {
		t.Fatalf("run 应保持 cancelled，实际 %s", status)
	}
	for _, eventType := range h.runEventTypes(run.ID) {
		if eventType == EventTaskOutcomeUnknown {
			t.Fatal("取消窗口绝不能把任务误判为 outcome_unknown")
		}
	}
}

// ---- watcher 只在 Enqueue 成功后启动 ----

func TestWatcherSpawnsOnlyAfterEnqueueSucceeds(t *testing.T) {
	h := newDispatchHarness(t)
	gate := make(chan struct{})
	enqueueEntered := make(chan struct{}, 1)
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		select {
		case enqueueEntered <- struct{}{}:
		default:
		}
		<-gate
	}
	run := h.createRun(t, ModeSingle, nil)
	waitFor(t, "任务已记账 dispatching", func() bool {
		return h.taskOf(run.ID, "lead").Status == TaskDispatching
	})
	<-enqueueEntered
	// 入队被挂起期间：无 watcher、无 outcome_unknown、run 不落 waiting_input
	// （阶段一「记账后即起 watcher」会在同样的窗口把任务误判为账本条目丢失）。
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	_, watched := state.watchers["lead"]
	state.mu.Unlock()
	if watched {
		t.Fatal("入队未完成前不得启动 watcher")
	}
	if task := h.taskOf(run.ID, "lead"); task.Status != TaskDispatching {
		t.Fatalf("闸门期间任务应保持 dispatching，实际 %s", task.Status)
	}
	if status := h.store.Run(run.ID).Status; status != RunDispatching {
		t.Fatalf("闸门期间 run 不得落 waiting_input，实际 %s", status)
	}
	close(gate)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.completeSubmission("s-lead", submissionID, sentText, "完成。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
	for _, eventType := range h.runEventTypes(run.ID) {
		if eventType == EventTaskOutcomeUnknown {
			t.Fatal("正常路径绝不应出现 outcome_unknown")
		}
	}
}

// ---- 池容量限流：同时在途不超过 maxParallelTasks（含 broadcast 展开）----

func TestDispatchPoolCapsConcurrentInFlightTasks(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.MaxParallelTasks = 2
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := h.createBroadcastRun(t, RunCollectAll, []string{"lead", "dev", "rev"})
	leadSub, leadText := h.waitForSubmission(t, "s-lead", "draft-lead")
	devSub, devText := h.waitForSubmission(t, "s-dev", "draft-dev")
	// 前两个在途时第三个绝不被派发（并发上限硬护栏）。
	if entries := h.queue.entriesFor("s-rev"); len(entries) != 0 {
		t.Fatalf("并发上限内第三条任务不得入队: %+v", entries)
	}
	// rev 的入队必须发生在 lead 终态之后（释放才补位）。
	leadTerminalAtRevEnqueue := false
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		if threadID == "s-rev" {
			leadTerminalAtRevEnqueue = terminalTask(h.taskOf(run.ID, "draft-lead").Status)
		}
	}
	h.completeSubmission("s-lead", leadSub, leadText, "Lead 版方案。")
	revSub, revText := h.waitForSubmission(t, "s-rev", "draft-rev 补位")
	if !leadTerminalAtRevEnqueue {
		t.Fatal("rev 的派发必须发生在 lead 终态之后（池容量限流）")
	}
	h.completeSubmission("s-dev", devSub, devText, "Dev 版方案。")
	h.completeSubmission("s-rev", revSub, revText, "Rev 版方案。")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

// ---- 序列化往返：mode/collectPolicy/broadcastGroup 落 team-runs.json ----

func TestRunSnapshotKeepsModeCollectPolicyAndBroadcastGroup(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	store := LoadStore(teamsPath, runsPath)
	team := testTeam()
	team.ID = "team-persist"
	if err := store.SaveTeam(team); err != nil {
		t.Fatal(err)
	}
	run := &Run{
		ID: "run-persist", TeamID: "team-persist", Goal: "产出方案",
		Mode: ModeBroadcast, CollectPolicy: RunCollectFirstSuccess,
		Status: RunDispatching,
		Tasks: []Task{{
			ID: "draft-lead", MemberID: "lead", Prompt: "各写一版方案",
			Status: TaskPlanned, BroadcastGroup: "draft", Ordinal: 0,
		}},
	}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	// 逐键验证确实落盘（store_contract_test 只测未知字段，抓不到漏 Set；
	// 账本文件是缩进格式，按键名检查而非紧凑片段）。
	raw, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatal(err)
	}
	var ledger map[string]map[string]any
	if err := json.Unmarshal(raw, &ledger); err != nil {
		t.Fatalf("team-runs.json 解析失败: %v\n%s", err, raw)
	}
	persisted := ledger["run-persist"]
	if persisted == nil {
		t.Fatalf("team-runs.json 应含 run-persist:\n%s", raw)
	}
	if persisted["mode"] != ModeBroadcast {
		t.Fatalf("mode 应落盘，实际 %v", persisted["mode"])
	}
	if persisted["collectPolicy"] != RunCollectFirstSuccess {
		t.Fatalf("collectPolicy 应落盘，实际 %v", persisted["collectPolicy"])
	}
	tasks, _ := persisted["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("任务应落盘: %v", persisted["tasks"])
	}
	if task, _ := tasks[0].(map[string]any); task["broadcastGroup"] != "draft" {
		t.Fatalf("broadcastGroup 应落盘，实际 %v", tasks[0])
	}
	fresh := LoadStore(teamsPath, runsPath).Run("run-persist")
	if fresh == nil {
		t.Fatal("重载后 run 应存在")
	}
	if fresh.Mode != ModeBroadcast || fresh.CollectPolicy != RunCollectFirstSuccess {
		t.Fatalf("重启恢复必须能读回 mode/collectPolicy: %+v", fresh)
	}
	if task := findTaskIn(fresh, "draft-lead"); task == nil || task.BroadcastGroup != "draft" {
		t.Fatalf("任务组归属应随快照持久化: %+v", fresh.Tasks)
	}
}

// ---- first_success 跨重启恢复：PlanCollect 每轮重算、first-wins 幂等 ----

func TestRecoverRunReplaysCollectPolicy(t *testing.T) {
	h := newDispatchHarness(t)
	const runID = "run-rec-collect"
	// 手工构造重启前状态：组内 draft-lead 已完成带结果、draft-dev 在途
	// （journal 已记账派发、账本条目 queued）。
	lead := Task{
		ID: "draft-lead", MemberID: "lead", Prompt: "各写一版方案", Status: TaskCompleted,
		Ordinal: 0, Attempt: 1, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
		BroadcastGroup: "draft",
		Result:         "Lead 版方案。",
	}
	lead.ResultDetail = &TaskResult{SessionID: "s-lead", TurnID: "t-1", Result: "Lead 版方案。", ResultChars: 8}
	lead.InputHash = InputHashFor(&lead, PolicyDigest(policyOf(h.team)), "")
	dev := Task{
		ID: "draft-dev", MemberID: "dev", Prompt: "各写一版方案", Status: TaskDispatching,
		Ordinal: 1, Attempt: 1, WriteLock: TaskWriteLockNone, WorkspaceMode: WorkspaceShared,
		BroadcastGroup: "draft",
	}
	dev.InputHash = InputHashFor(&dev, PolicyDigest(policyOf(h.team)), "")
	dev.SubmissionID = TaskSubmissionID(h.team.ID, runID, "draft-dev", 1)
	run := &Run{
		ID: runID, TeamID: h.team.ID, Goal: "产出方案", Mode: ModeBroadcast,
		CollectPolicy: RunCollectFirstSuccess, Status: RunDispatching, Tasks: []Task{lead, dev},
	}
	for _, event := range []*Event{
		newEvent(runID, EventRunCreated, "user", map[string]any{
			"teamId": h.team.ID, "mode": ModeBroadcast, "collectPolicy": RunCollectFirstSuccess, "taskCount": jsoncompat.Num(2),
		}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "draft-lead", "memberId": "lead"}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "draft-dev", "memberId": "dev"}),
	} {
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	completed := newEvent(runID, EventTaskCompleted, "watcher", map[string]any{
		"taskId": "draft-lead", "sessionId": "s-lead", "turnId": "t-1",
		"result": "Lead 版方案。", "resultChars": jsoncompat.Num(8),
	})
	if err := h.journal.Append(completed); err != nil {
		t.Fatalf("journal 预置失败: %v", err)
	}
	devDispatch := newEvent(runID, EventTaskDispatching, "runner", map[string]any{
		"taskId": "draft-dev", "sessionId": "s-dev", "submissionId": dev.SubmissionID,
		"inputHash": dev.InputHash, "attempt": jsoncompat.Num(1),
	})
	devDispatch.IdempotencyKey = TaskIdempotencyKey(h.team.ID, runID, "draft-dev", 1)
	if err := h.journal.Append(devDispatch); err != nil {
		t.Fatalf("journal 预置失败: %v", err)
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.queue.Enqueue("s-dev", dev.SubmissionID, "各写一版方案"); err != nil {
		t.Fatalf("预置账本条目失败: %v", err)
	}

	if err := h.service.RecoverRun(runID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 恢复后的调度循环重算收集策略：组内已有成功者 → 在途兄弟取消并撤条目。
	waitFor(t, "draft-dev 落 cancelled", func() bool {
		return h.taskOf(runID, "draft-dev").Status == TaskCancelled
	})
	if reason := h.taskOf(runID, "draft-dev").StopReason; reason != StopReasonSupersededByFirstSuccess {
		t.Fatalf("恢复后的取消原因应为 superseded，实际 %q", reason)
	}
	waitFor(t, "队列条目撤销", func() bool {
		if entry, ok := h.queue.firstEntry("s-dev"); ok {
			return entry.status == SubStatusCancelled
		}
		return false
	})
	// 首个成功者的结果跨重启保留；run 折 completed。
	waitFor(t, "run 完成", func() bool { return h.store.Run(runID).Status == RunCompleted })
	if task := h.taskOf(runID, "draft-lead"); task.Result != "Lead 版方案。" {
		t.Fatalf("首个成功者的结果必须跨重启保留: %+v", task)
	}
}

// ---- §6.3 AIMD：错误策略表与共享桶 ----

// aimdEventOf 找出 run 的指定类型事件并返回其 payload 列表。
func aimdEventOf(t *testing.T, journal *Journal, runID, eventType string) []map[string]any {
	t.Helper()
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var payloads []map[string]any
	for _, event := range events {
		if event.RunID == runID && event.Type == eventType {
			payloads = append(payloads, event.Payload)
		}
	}
	return payloads
}

// TestRateLimitedFailureRetriesWithNewSubmissionID：429 文本 → 单次 commitRun
// 落 task_failed 事实（含 outcome）+ attempt+1 回 ready → 冷却过后按
// TaskSubmissionID(...,2) 重派；预算耗尽（MaxTaskRetries=1）按确定性 failed
// 收场（Outcome=rate_limited）；窗口实际减小时每 epoch 记一次
// provider_throttled。
func TestRateLimitedFailureRetriesWithNewSubmissionID(t *testing.T) {
	h := newDispatchHarness(t)
	clock := newAIMDClock()
	h.service.AIMD.now = clock.Now
	run := h.createRun(t, ModeSingle, nil)
	firstSub, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	if firstSub != TaskSubmissionID(h.team.ID, run.ID, "lead", 1) {
		t.Fatalf("首次派发应为 attempt 1 幂等键: %s", firstSub)
	}
	// attempt 1 以 429 失败（Retry-After: 30 → 冷却 30s，时钟可控）。
	// 先备会话再放行账本：生产路径 BeginTurn 先于账本 accepted
	// （manager.go prompt：s.View.BeginTurn 后才 Submit/返回），fake 必须
	// 建模该不变式，否则 watcher 会在「accepted 但新 turn 未见」窗口抢先
	// 匹配上一 attempt 的旧 turn。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "429 Too Many Requests (Retry-After: 30)")))
	h.queue.setStatus("s-lead", firstSub, SubStatusAccepted, "")
	waitFor(t, "受限重试回 ready", func() bool {
		task := h.taskOf(run.ID, "lead")
		return task.Status == TaskReady && task.Attempt == 2
	})
	task := h.taskOf(run.ID, "lead")
	if task.SubmissionID != "" || task.Error != "" || task.Outcome != "" {
		t.Fatalf("重试回 ready 应清引用与错误: %+v", task)
	}
	// task_failed 事实已落 journal（outcome + 失败 attempt=1）。
	failures := aimdEventOf(t, h.journal, run.ID, EventTaskFailed)
	if len(failures) != 1 || failures[0]["outcome"] != OutcomeRateLimited {
		t.Fatalf("应落一条带 outcome 的 task_failed: %+v", failures)
	}
	if attempt, ok := failures[0]["attempt"].(json.Number); !ok || attempt.String() != "1" {
		t.Fatalf("task_failed 应携带失败 attempt=1: %+v", failures[0])
	}
	// 窗口实际减小：每 epoch 记一次 provider_throttled。
	throttled := aimdEventOf(t, h.journal, run.ID, EventProviderThrottled)
	if len(throttled) != 1 || throttled[0]["window"] != jsoncompat.Num(4) {
		t.Fatalf("首次减窗应记 provider_throttled(window=4): %+v", throttled)
	}
	// 冷却期内（假时钟冻结）不入队：桶冷却把重派挡在 planned/ready。
	if entries := h.queue.entriesFor("s-lead"); len(entries) != 1 {
		t.Fatalf("冷却期内不应有第二次入队: %+v", entries)
	}
	// 冷却结束：按 attempt 2 的全新 submissionID 重派。
	clock.Advance(31 * time.Second)
	var secondSub string
	waitFor(t, "attempt 2 重派", func() bool {
		entries := h.queue.entriesFor("s-lead")
		if len(entries) >= 2 {
			secondSub = entries[1].submissionID
			return true
		}
		return false
	})
	if secondSub != TaskSubmissionID(h.team.ID, run.ID, "lead", 2) {
		t.Fatalf("重派应为 attempt 2 幂等键: %s", secondSub)
	}
	// attempt 2 再次 429：预算耗尽（MaxTaskRetries=1）→ 确定性 failed。
	// 同样先备会话（含 attempt-2 的失败回合）再放行账本。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "429 too many requests")))
	h.queue.setStatus("s-lead", secondSub, SubStatusAccepted, "")
	waitFor(t, "预算耗尽确定性失败", func() bool {
		task := h.taskOf(run.ID, "lead")
		return task.Status == TaskFailed && task.Outcome == OutcomeRateLimited
	})
	// 第二次减窗在新 epoch：第二条 provider_throttled。
	waitFor(t, "第二条限流事件", func() bool {
		return len(aimdEventOf(t, h.journal, run.ID, EventProviderThrottled)) == 2
	})
	// 任务 failed → run 折 failed（级联语义不变）。
	waitFor(t, "run 失败", func() bool { return h.store.Run(run.ID).Status == RunFailed })
}

// TestRetryBudgetSurvivesRestartDuringCooldown：受限重试把任务滞留在 ready
// （AIMD 冷却最长 300s——重试窗口真实存在）期间网关重启：journal 里的
// task_retry_scheduled 让 ReconcileRun 保住「回 ready@attempt+1」的事实——
// 任务不被折成确定性 failed，新进程照常按 attempt 2 重派，重试预算不因
// 重启丢失（§6.3 受限重试语义；修复前重放只见 task_failed 终态）。
func TestRetryBudgetSurvivesRestartDuringCooldown(t *testing.T) {
	h := newDispatchHarness(t)
	clock := newAIMDClock()
	h.service.AIMD.now = clock.Now // 冻结冷却：重启前任务持续滞留 ready
	run := h.createRun(t, ModeSingle, nil)
	firstSub, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "429 Too Many Requests (Retry-After: 30)")))
	h.queue.setStatus("s-lead", firstSub, SubStatusAccepted, "")
	waitFor(t, "受限重试回 ready", func() bool {
		task := h.taskOf(run.ID, "lead")
		return task.Status == TaskReady && task.Attempt == 2
	})
	// 重启：同一 store/journal/queue/resolver 上重建服务（新 AIMD 无冷却）。
	restarted := NewService(h.store, h.journal, h.queue, h.resolver)
	restarted.waitSlice = 2 * time.Millisecond
	if err := restarted.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	// 重启后不得折 failed：按 attempt 2 的全新幂等键重派。
	waitFor(t, "重启后按 attempt 2 重派", func() bool {
		entries := h.queue.entriesFor("s-lead")
		return len(entries) >= 2 && entries[1].submissionID == TaskSubmissionID(h.team.ID, run.ID, "lead", 2)
	})
	if task := h.taskOf(run.ID, "lead"); task.Status == TaskFailed {
		t.Fatalf("重试预算不应因重启丢失: %+v", task)
	}
	// attempt 2 完成 → run 正常完成（重试预算被真正用上而非丢弃）。
	entries := h.queue.entriesFor("s-lead")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnCompleted(entries[1].text, "完成。")))
	h.queue.setStatus("s-lead", entries[1].submissionID, SubStatusAccepted, "")
	waitFor(t, "run 完成", func() bool { return h.store.Run(run.ID).Status == RunCompleted })
}

// TestTransientFailureRetriesWithoutThrottle：瞬态错误（timeout）同样受限
// 重试（attempt 2 全新幂等键），但不压 AIMD 桶、不记 provider_throttled；
// 无冷却 → 重派立即进行。
func TestTransientFailureRetriesWithoutThrottle(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	firstSub, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "request timeout after 30s")))
	h.queue.setStatus("s-lead", firstSub, SubStatusAccepted, "")
	// 瞬态无冷却：ready 是瞬态（循环立刻重派），按第二条入账观察重试。
	var secondSub string
	waitFor(t, "瞬态受限重试", func() bool {
		entries := h.queue.entriesFor("s-lead")
		if len(entries) >= 2 {
			secondSub = entries[1].submissionID
			return true
		}
		return false
	})
	if secondSub != TaskSubmissionID(h.team.ID, run.ID, "lead", 2) {
		t.Fatalf("重派应为 attempt 2 幂等键: %s", secondSub)
	}
	failures := aimdEventOf(t, h.journal, run.ID, EventTaskFailed)
	if len(failures) != 1 || failures[0]["outcome"] != OutcomeTransient {
		t.Fatalf("应落一条 outcome=transient 的 task_failed: %+v", failures)
	}
	if throttled := aimdEventOf(t, h.journal, run.ID, EventProviderThrottled); len(throttled) != 0 {
		t.Fatalf("瞬态错误不应记 provider_throttled: %+v", throttled)
	}
	for _, state := range h.service.AIMDSnapshot() {
		if state.Window != state.Ceiling {
			t.Fatalf("瞬态错误不应压桶: %+v", state)
		}
	}
}

// TestQuotaFailureStopsTaskAndPausesRun：quota 文本 → 任务停靠（stopped +
// Outcome=quota + 中文原因）+ run 停 waiting_input；共享桶不变；resume 后
// 停靠任务按 attempt+1 派全新 submissionID。
func TestQuotaFailureStopsTaskAndPausesRun(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModePipeline, pipelinePlan())
	designSub, designText := h.waitForSubmission(t, "s-lead", "design")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(designText, "账户额度不足，请充值后重试")))
	h.queue.setStatus("s-lead", designSub, SubStatusAccepted, "")
	waitFor(t, "任务停靠", func() bool {
		task := h.taskOf(run.ID, "design")
		return task.Status == TaskStopped && task.Outcome == OutcomeQuota
	})
	task := h.taskOf(run.ID, "design")
	if !strings.Contains(task.StopReason, "停靠") || task.Error == "" {
		t.Fatalf("停靠原因与错误文本应保留: %+v", task)
	}
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	// 停靠事实携带 outcome 与 attempt（recovery 重放的依据）。
	stops := aimdEventOf(t, h.journal, run.ID, EventTaskStopped)
	if len(stops) != 1 || stops[0]["outcome"] != OutcomeQuota {
		t.Fatalf("task_stopped 应携带 outcome=quota: %+v", stops)
	}
	// 桶不受 quota 影响：窗口仍是 ceiling，且无 provider_throttled。
	for _, state := range h.service.AIMDSnapshot() {
		if state.Window != state.Ceiling {
			t.Fatalf("quota 不应压共享桶: %+v", state)
		}
	}
	if throttled := aimdEventOf(t, h.journal, run.ID, EventProviderThrottled); len(throttled) != 0 {
		t.Fatalf("quota 不应记 provider_throttled: %+v", throttled)
	}
	// run 暂停期间下游不派发。
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("暂停期间下游不应入队: %+v", entries)
	}
	// resume：停靠任务重臂重飞（attempt+1 全新 submissionID），下游等待其完成。
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	var secondSub string
	waitFor(t, "停靠任务重飞", func() bool {
		entries := h.queue.entriesFor("s-lead")
		if len(entries) >= 2 {
			secondSub = entries[1].submissionID
			return true
		}
		return false
	})
	if secondSub != TaskSubmissionID(h.team.ID, run.ID, "design", 2) {
		t.Fatalf("resume 重飞应为 attempt 2 幂等键: %s", secondSub)
	}
	rearmed := h.taskOf(run.ID, "design")
	if rearmed.Status != TaskDispatching || rearmed.Attempt != 2 || rearmed.Outcome != "" {
		t.Fatalf("重飞任务应为 dispatching/attempt=2/Outcome 清空: %+v", rearmed)
	}
	// 重飞完成后下游照常释放（先备完成回合再放行账本，理由同上）。
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnCompleted(designText, "设计完成。")))
	h.queue.setStatus("s-lead", secondSub, SubStatusAccepted, "")
	waitFor(t, "下游释放", func() bool {
		_, ok := h.queue.firstEntry("s-dev")
		return ok
	})
}

// TestQuotaStopDoesNotAffectOtherTeamSameBucket：同 kind（共享桶）的另一
// Team 不因 quota 停靠被冻结——quota 只停受影响 Run，不冻结共享桶（§6.3）。
func TestQuotaStopDoesNotAffectOtherTeamSameBucket(t *testing.T) {
	h := newDispatchHarness(t)
	// 二队成员与一队 lead 同为 kimi：共享 "kimi:" 桶（会话无 model，降级键）。
	second := &Team{
		ID: "team-2", Name: "二队", LeadID: "alpha", Policy: DefaultPolicy(),
		Members: []TeamMember{
			{ID: "alpha", SessionID: "s-alpha", AgentKind: "kimi", Role: RoleLead, Enabled: true},
		},
	}
	h.resolver.setCwd("s-alpha", "/tmp/alpha")
	if err := h.store.SaveTeam(second); err != nil {
		t.Fatalf("二队保存失败: %v", err)
	}
	run1 := h.createRun(t, ModeSingle, nil)
	sub1, text1 := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(text1, "余额不足，请充值")))
	h.queue.setStatus("s-lead", sub1, SubStatusAccepted, "")
	if !waitForSoft(5*time.Second, "一队停靠", func() bool {
		return h.store.Run(run1.ID).Status == RunWaitingInput
	}) {
		state := h.service.stateFor(run1.ID)
		state.mu.Lock()
		watchers := make([]string, 0, len(state.watchers))
		for taskID := range state.watchers {
			watchers = append(watchers, taskID)
		}
		state.mu.Unlock()
		t.Fatalf("一队未停靠诊断: run=%+v tasks=%+v watchers=%v s-lead 条目=%+v 事件=%v buckets=%+v",
			h.store.Run(run1.ID), h.store.Run(run1.ID).Tasks, watchers,
			h.queue.entriesFor("s-lead"), h.runEventTypes(run1.ID), h.service.AIMDSnapshot())
	}
	// 二队的同桶任务照常派发。
	run2, err := h.service.CreateRun(second, "", "二队目标", ModeSingle, "", nil, nil)
	if err != nil {
		t.Fatalf("二队 run 创建失败: %v", err)
	}
	waitFor(t, "二队任务入队", func() bool {
		_, ok := h.queue.firstEntry("s-alpha")
		return ok
	})
	if task := h.taskOf(run2.ID, "lead"); task.Status != TaskDispatching {
		t.Fatalf("二队任务应正常派发: %+v", task)
	}
	// 对照：限流（429）才压共享桶——二队任务会被冷却挡住。
}

// TestBucketCooldownKeepsTaskPlanned：桶冷却期任务留 planned（准入拒绝后
// 靠调度循环片重试，registry 不主动唤醒）；冷却结束后按序恢复派发。
func TestBucketCooldownKeepsTaskPlanned(t *testing.T) {
	h := newDispatchHarness(t)
	clock := newAIMDClock()
	h.service.AIMD.now = clock.Now
	// 同 kind 两成员 + MaxParallelTasks=1：t1 先占额度，t2 的唯一阻碍是桶冷却。
	team := &Team{
		ID: "team-cd", Name: "冷却测试", LeadID: "a1", Policy: DefaultPolicy(),
		Members: []TeamMember{
			{ID: "a1", SessionID: "s-a1", AgentKind: "kimi", Role: RoleLead, Enabled: true},
			{ID: "a2", SessionID: "s-a2", AgentKind: "kimi", Role: RoleMember, Enabled: true},
		},
	}
	team.Policy.MaxParallelTasks = 1
	for _, member := range team.Members {
		h.resolver.setCwd(member.SessionID, "/tmp/"+member.ID)
	}
	if err := h.store.SaveTeam(team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	h.team = team
	plan := []PlanTaskInput{
		{ID: "t1", MemberID: "a1", Prompt: "任务一", WriteLock: TaskWriteLockNone},
		{ID: "t2", MemberID: "a2", Prompt: "任务二", WriteLock: TaskWriteLockNone},
	}
	run := h.createRun(t, ModePipeline, plan)
	sub1, text1 := h.waitForSubmission(t, "s-a1", "任务一")
	h.resolver.setSession("s-a1", newFakeSession("s-a1", "/tmp/a1",
		turnFailedView(text1, "429 rate limit exceeded")))
	h.queue.setStatus("s-a1", sub1, SubStatusAccepted, "")
	waitFor(t, "t1 受限重试回 ready", func() bool {
		task := h.taskOf(run.ID, "t1")
		return task.Status == TaskReady && task.Attempt == 2
	})
	// 冷却期（假时钟冻结）：t2 留 planned，二成员均无新入队。
	if task := h.taskOf(run.ID, "t2"); task.Status != TaskPlanned {
		t.Fatalf("冷却期 t2 应留 planned: %+v", task)
	}
	if entries := h.queue.entriesFor("s-a2"); len(entries) != 0 {
		t.Fatalf("冷却期 t2 不应入队: %+v", entries)
	}
	// 冷却结束：t1（ordinal 在前）先重飞，成功后 t2 才派发。
	clock.Advance(31 * time.Second)
	var sub2 string
	waitFor(t, "t1 重飞", func() bool {
		entries := h.queue.entriesFor("s-a1")
		if len(entries) >= 2 {
			sub2 = entries[1].submissionID
			return true
		}
		return false
	})
	if sub2 != TaskSubmissionID(team.ID, run.ID, "t1", 2) {
		t.Fatalf("t1 重飞应为 attempt 2: %s", sub2)
	}
	// 先备完成回合再放行账本（生产不变式：BeginTurn 先于 accepted）。
	h.resolver.setSession("s-a1", newFakeSession("s-a1", "/tmp/a1",
		turnCompleted(text1, "一完成。")))
	h.queue.setStatus("s-a1", sub2, SubStatusAccepted, "")
	if !waitForSoft(5*time.Second, "t2 解除冷却后派发", func() bool {
		_, ok := h.queue.firstEntry("s-a2")
		return ok
	}) {
		state := h.service.stateFor(run.ID)
		state.mu.Lock()
		watchers := make([]string, 0, len(state.watchers))
		for taskID := range state.watchers {
			watchers = append(watchers, taskID)
		}
		pending := make([]string, 0, len(state.pendingDispatch))
		for taskID := range state.pendingDispatch {
			pending = append(pending, taskID)
		}
		state.mu.Unlock()
		t.Fatalf("t2 未派发诊断: tasks=%+v watchers=%v pending=%v buckets=%+v s-a1 条目=%+v 事件=%v",
			h.store.Run(run.ID).Tasks, watchers, pending, h.service.AIMDSnapshot(),
			h.queue.entriesFor("s-a1"), h.runEventTypes(run.ID))
	}
	if task := h.taskOf(run.ID, "t2"); task.Status != TaskDispatching {
		t.Fatalf("冷却结束后 t2 应已派发: %+v", task)
	}
}

// waitForSoft 是 waitFor 的非 Fatal 版本（诊断转储用）。
func waitForSoft(timeout time.Duration, what string, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestRecoverQuotaStoppedRunParksWaitingInput：quota 停靠的 run 重启对账后
// 驻留 waiting_input——任务保持 stopped+Outcome，绝不重发（§6.3：停靠等
// 用户 resume 重飞，对账不自动派发）。
func TestRecoverQuotaStoppedRunParksWaitingInput(t *testing.T) {
	h := newDispatchHarness(t)
	run := h.createRun(t, ModeSingle, nil)
	submissionID, sentText := h.waitForSubmission(t, "s-lead", "lead 任务")
	h.resolver.setSession("s-lead", newFakeSession("s-lead", "/tmp/lead",
		turnFailedView(sentText, "配额不足，请充值")))
	h.queue.setStatus("s-lead", submissionID, SubStatusAccepted, "")
	waitFor(t, "任务停靠", func() bool {
		return h.taskOf(run.ID, "lead").Status == TaskStopped
	})
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })

	// 重启对账：驻留 waiting_input，任务保持停靠与 Outcome，零重发。
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	if status := h.store.Run(run.ID).Status; status != RunWaitingInput {
		t.Fatalf("停靠 run 恢复后应驻留 waiting_input，实际 %s", status)
	}
	task := h.taskOf(run.ID, "lead")
	if task.Status != TaskStopped || task.Outcome != OutcomeQuota || task.Attempt != 1 {
		t.Fatalf("停靠任务应保持 stopped/outcome=quota/attempt=1: %+v", task)
	}
	if entries := h.queue.entriesFor("s-lead"); len(entries) != 1 {
		t.Fatalf("恢复不得重发停靠任务: %+v", entries)
	}
	if got := h.queue.callCount(submissionID); got != 1 {
		t.Fatalf("恢复不得重复入账: %d 次", got)
	}
	// 调度循环未重启（waiting_input 无循环），无新派发事实。
	if dispatches := aimdEventOf(t, h.journal, run.ID, EventTaskDispatching); len(dispatches) != 1 {
		t.Fatalf("恢复后不应有第二次派发记账: %+v", dispatches)
	}
}
