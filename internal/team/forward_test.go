package team

// forward_test 是成员间显式转发（§7 阶段二工作项 4）的单元测试：任务源/
// 会话源两路径、信封版式（含时间行）、字节兜底、幂等重放零副作用、先记账
// 后入队、终态 run 拒绝与崩溃窗口补投。

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- 测试基座 ----

// fakeTranscripts 是 TranscriptSource 的可控实现。
type fakeTranscripts struct {
	text      string
	truncated bool
	err       error
	calls     []string // "sessionID/scope"
}

func (f *fakeTranscripts) BoundedTranscript(sessionID, scope string) (string, bool, error) {
	f.calls = append(f.calls, sessionID+"/"+scope)
	if f.err != nil {
		return "", false, f.err
	}
	return f.text, f.truncated, nil
}

// forwardHarness 复用 dispatchHarness 的 store/journal/queue/resolver 基座，
// 但 run 直接落账（不经 CreateRun——不启动调度循环，转发本身不依赖循环）。
type forwardHarness struct {
	*dispatchHarness
	emitted []*Event
}

func newForwardHarness(t *testing.T) *forwardHarness {
	t.Helper()
	h := &forwardHarness{dispatchHarness: newDispatchHarness(t)}
	h.service.OnEvent = func(event *Event) { h.emitted = append(h.emitted, event) }
	return h
}

// saveForwardRun 直接落一条带 completed 任务的 run（版本乐观推进）。
func (h *forwardHarness) saveForwardRun(t *testing.T, run *Run) *Run {
	t.Helper()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	return run
}

func forwardRunFixture(id string) *Run {
	return &Run{
		ID: id, TeamID: "team-1", Status: RunDispatching, Round: 1,
		PlanHash: "hash",
		Tasks: []Task{
			{
				ID: "analysis", MemberID: "lead", Ordinal: 0, Status: TaskCompleted,
				Result: "分析结论：接口需要鉴权。",
				ResultDetail: &TaskResult{
					SessionID: "s-lead", TurnID: "turn-1",
					Result: "分析结论：接口需要鉴权。", ResultChars: 12,
					FileSummary:    "src/a.go、src/b.go",
					CommandSummary: "go test ./...；make check",
				},
			},
			{ID: "pending", MemberID: "dev", Ordinal: 1, Status: TaskPlanned},
		},
	}
}

func forwardRequest(requestID, sourceTaskID, sourceSessionID, target string) ForwardRequest {
	return ForwardRequest{
		RequestID: requestID, SourceTaskID: sourceTaskID,
		SourceSessionID: sourceSessionID, TargetMemberID: target,
		Note: "请基于上述分析继续实现",
	}
}

// ---- 任务源 ----

func TestForwardToMemberTaskSourceEnvelopeAndDelivery(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	requestID := uuid.NewString()
	req := forwardRequest(requestID, "analysis", "", "rev")

	submissionID, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || duplicate {
		t.Fatalf("任务源转发应成功且非重复: id=%s duplicate=%v err=%v", submissionID, duplicate, err)
	}
	if want := ForwardSubmissionID(h.team.ID, run.ID, requestID); submissionID != want {
		t.Fatalf("submissionID 应由幂等键确定性派生，实际 %s 期望 %s", submissionID, want)
	}
	// 投递：mode=queue 经账本，目标成员会话可见全文。
	entry, ok := h.queue.firstEntry("s-rev")
	if !ok || entry.submissionID != submissionID {
		t.Fatalf("目标成员会话应收到转发消息: %+v ok=%v", entry, ok)
	}
	text := entry.text
	lines := strings.Split(text, "\n")
	want := []string{
		"[Team context]",
		"来源成员：lead",
		"来源任务：analysis",
		"", // 时间行在下面单独断言（RFC3339 可解析）。
		contextTruncationNote,
		"",
		"分析结论：接口需要鉴权。",
		"变更文件：src/a.go、src/b.go",
		"执行命令：go test ./...；make check",
		"",
		"[Assigned task]",
		"请基于上述分析继续实现",
	}
	if len(lines) < len(want) {
		t.Fatalf("信封行数不足:\n%s", text)
	}
	for i, wantLine := range want {
		if wantLine == "" {
			continue
		}
		if lines[i] != wantLine {
			t.Fatalf("信封第 %d 行应为 %q，实际 %q:\n%s", i, wantLine, lines[i], text)
		}
	}
	if !strings.HasPrefix(lines[3], "时间：") {
		t.Fatalf("第 4 行应为时间行，实际 %q:\n%s", lines[3], text)
	}
	if _, err := time.Parse(time.RFC3339, strings.TrimPrefix(lines[3], "时间：")); err != nil {
		t.Fatalf("时间行应为 RFC3339，实际 %q: %v", lines[3], err)
	}
	// 事件：journal 里有 task_forwarded（幂等键 + payload 契约）。
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var forwarded *Event
	for _, event := range events {
		if event.Type == EventTaskForwarded {
			forwarded = event
		}
	}
	if forwarded == nil {
		t.Fatal("journal 应有 task_forwarded 事件")
	}
	if forwarded.IdempotencyKey != ForwardIdempotencyKey(h.team.ID, run.ID, requestID) {
		t.Fatalf("事件应携带幂等键，实际 %q", forwarded.IdempotencyKey)
	}
	for key, want := range map[string]any{
		"sourceTaskId":   "analysis",
		"targetMemberId": "rev",
		"sessionId":      "s-rev",
		"submissionId":   submissionID,
	} {
		if got := payloadString(forwarded.Payload, key); got != want {
			t.Fatalf("事件 payload[%s] 应为 %v，实际 %q", key, want, got)
		}
	}
	if got, _ := forwarded.Payload["truncated"].(bool); got {
		t.Fatal("未触上限不应标记 truncated")
	}
	if number, ok := forwarded.Payload["contextChars"].(json.Number); !ok || number.String() == "0" {
		t.Fatalf("事件应携带 contextChars，实际 %v", forwarded.Payload["contextChars"])
	}
	// OnEvent 同步回调（httpd 装配侧接事件流）。
	found := false
	for _, event := range h.emitted {
		if event.Type == EventTaskForwarded {
			found = true
		}
	}
	if !found {
		t.Fatal("task_forwarded 应经 OnEvent 发出")
	}
	// 转发不创建 Task、不进调度：run 快照任务数不变、pending 保持 planned。
	fresh := h.store.Run(run.ID)
	if len(fresh.Tasks) != 2 || fresh.Tasks[1].Status != TaskPlanned {
		t.Fatalf("转发不得改动任务表: %+v", fresh.Tasks)
	}
}

func TestForwardToMemberTaskSourceTruncationMarker(t *testing.T) {
	h := newForwardHarness(t)
	run := forwardRunFixture("run-trunc")
	run.Tasks[0].ResultDetail.Truncated = true
	h.saveForwardRun(t, run)
	_, _, err := h.service.ForwardToMember(h.team, run, forwardRequest(uuid.NewString(), "analysis", "", "dev"))
	if err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	entry, _ := h.queue.firstEntry("s-dev")
	if !strings.Contains(entry.text, "（结果过长，已截断）") {
		t.Fatalf("结果截断标记应随正文携带:\n%s", entry.text)
	}
}

// ---- 会话源 ----

func TestForwardToMemberSessionSource(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	transcripts := &fakeTranscripts{
		text:      "用户：请检查登录流程\n助手：发现缺少过期校验",
		truncated: true,
	}
	h.service.Transcripts = transcripts
	req := forwardRequest(uuid.NewString(), "", "s-lead", "dev")
	req.Scope = "main"
	req.Note = "" // 验证缺省指派语。

	submissionID, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || duplicate {
		t.Fatalf("会话源转发应成功: err=%v duplicate=%v", err, duplicate)
	}
	if len(transcripts.calls) != 1 || transcripts.calls[0] != "s-lead/main" {
		t.Fatalf("转录提取应收到 sessionID+scope，实际 %v", transcripts.calls)
	}
	entry, _ := h.queue.firstEntry("s-dev")
	if entry.submissionID != submissionID {
		t.Fatalf("入账条目应使用派生 submissionID: %s vs %s", entry.submissionID, submissionID)
	}
	for _, want := range []string{
		"[Team context]",
		"来源会话：s-lead",
		"用户：请检查登录流程",
		"助手：发现缺少过期校验",
		"[Assigned task]",
	} {
		if !strings.Contains(entry.text, want) {
			t.Fatalf("信封应包含 %q:\n%s", want, entry.text)
		}
	}
	if strings.Contains(entry.text, "来源成员：") || strings.Contains(entry.text, "来源任务：") {
		t.Fatalf("会话源信封不应有任务来源行:\n%s", entry.text)
	}
	// 源截断标记传导进事件 payload；Note 缺省值生效。
	if !strings.Contains(entry.text, forwardNoteDefault) {
		t.Fatalf("未携带 Note 应使用缺省指派语:\n%s", entry.text)
	}
	events, _ := h.journal.Replay()
	for _, event := range events {
		if event.Type != EventTaskForwarded {
			continue
		}
		if payloadString(event.Payload, "sourceSessionId") != "s-lead" {
			t.Fatalf("payload 应带 sourceSessionId，实际 %v", event.Payload)
		}
		if got, _ := event.Payload["truncated"].(bool); !got {
			t.Fatal("源转录截断应传导进 truncated")
		}
	}
}

func TestForwardToMemberSessionSourceWithoutPort(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	_, _, err := h.service.ForwardToMember(h.team, run, forwardRequest(uuid.NewString(), "", "s-lead", "dev"))
	if !errors.Is(err, ErrTranscriptUnavailable) {
		t.Fatalf("未装配转录端口应报 ErrTranscriptUnavailable，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "网关未装配会话转录提取") {
		t.Fatalf("错误文案应可读，实际 %v", err)
	}
}

// ---- 上限与字节兜底 ----

func TestForwardToMemberByteFloorShrinksBody(t *testing.T) {
	h := newForwardHarness(t)
	team := h.team.Clone()
	team.Policy.MaxContextChars = 60000 // rune 上限不约束 50000 rune 的 CJK 正文。
	run := forwardRunFixture("run-byte-1")
	run.Tasks[0].Result = strings.Repeat("析", 50000) // 150000 字节。
	run.Tasks[0].ResultDetail.Result = run.Tasks[0].Result
	h.saveForwardRun(t, run)

	submissionID, _, err := h.service.ForwardToMember(team, run, forwardRequest(uuid.NewString(), "analysis", "", "dev"))
	if err != nil {
		t.Fatalf("字节兜底应收缩正文而非报错: %v", err)
	}
	entry, _ := h.queue.firstEntry("s-dev")
	if len(entry.text) > promptHardLimit {
		t.Fatalf("合成消息 %d 字节超过账本上限 %d", len(entry.text), promptHardLimit)
	}
	if !strings.Contains(entry.text, "[Assigned task]") || !strings.Contains(entry.text, "请基于上述分析继续实现") {
		t.Fatalf("收缩不应破坏信封结构:\n%s", entry.text[:200])
	}
	// truncated 进事件 payload。
	events, _ := h.journal.Replay()
	for _, event := range events {
		if event.Type == EventTaskForwarded && event.Payload["submissionId"] == submissionID {
			if got, _ := event.Payload["truncated"].(bool); !got {
				t.Fatal("字节收缩应标记 truncated")
			}
		}
	}
	// Note 自身超限且正文已收缩殆尽 → 确定性报错。
	run2 := forwardRunFixture("run-byte-2")
	h.saveForwardRun(t, run2)
	req := forwardRequest(uuid.NewString(), "analysis", "", "dev")
	req.Note = strings.Repeat("嘱", 60000) // 180000 字节，正文清零也装不下。
	if _, _, err := h.service.ForwardToMember(team, run2, req); err == nil ||
		!strings.Contains(err.Error(), "转发内容过长") {
		t.Fatalf("Note 超限应确定性报错，实际 %v", err)
	}
}

func TestForwardToMemberRuneCapByPolicy(t *testing.T) {
	h := newForwardHarness(t)
	team := h.team.Clone()
	team.Policy.MaxContextChars = 20 // 默认策略 12000 收紧到 20 rune。
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	_, _, err := h.service.ForwardToMember(team, run, forwardRequest(uuid.NewString(), "analysis", "", "dev"))
	if err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	entry, _ := h.queue.firstEntry("s-dev")
	if body := "分析结论：接口需要鉴权。"; !strings.Contains(entry.text, truncateRunes(body, 20)) {
		t.Fatalf("正文应按 MaxContextChars 截断:\n%s", entry.text)
	}
}

// ---- 幂等与记账顺序 ----

func TestForwardToMemberIdempotentReplay(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	req := forwardRequest(uuid.NewString(), "analysis", "", "rev")

	first, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || duplicate {
		t.Fatalf("首次转发应成功: err=%v duplicate=%v", err, duplicate)
	}
	countForwardEvents := func() int {
		events, _ := h.journal.Replay()
		count := 0
		for _, event := range events {
			if event.Type == EventTaskForwarded {
				count++
			}
		}
		return count
	}
	if countForwardEvents() != 1 {
		t.Fatalf("首次转发后应恰有 1 条事件，实际 %d", countForwardEvents())
	}
	// 同 id 同内容重放：duplicate=true，journal 与账本零副作用。
	second, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || !duplicate {
		t.Fatalf("重放应返回 duplicate=true: err=%v duplicate=%v", err, duplicate)
	}
	if second != first {
		t.Fatalf("重放 submissionID 应恒等: %s vs %s", second, first)
	}
	if countForwardEvents() != 1 {
		t.Fatalf("重放不应追加 journal 事件，实际 %d", countForwardEvents())
	}
	if calls := h.queue.callCount(first); calls != 1 {
		t.Fatalf("重放不应再次入队，实际 Enqueue %d 次", calls)
	}
	// 同 id 不同内容：拒绝（creations 账本同款语义）。
	if _, _, err := h.service.ForwardToMember(h.team, run,
		forwardRequest(req.RequestID, "pending", "", "rev")); err == nil ||
		!strings.Contains(err.Error(), "同一转发请求不能用于不同内容") {
		t.Fatalf("同 id 异源应拒绝，实际 %v", err)
	}
}

func TestForwardJournalsBeforeEnqueue(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	requestID := uuid.NewString()
	journalFirst := false
	h.queue.onEnqueue = func(threadID, submissionID, text string) {
		// Enqueue 执行时 journal 里必须已有本转发事件（§3.1 原则 4）。
		if event := h.service.forwardRecorded(run.ID, ForwardIdempotencyKey(h.team.ID, run.ID, requestID)); event != nil {
			journalFirst = true
		}
	}
	if _, _, err := h.service.ForwardToMember(h.team, run, forwardRequest(requestID, "analysis", "", "rev")); err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	if !journalFirst {
		t.Fatal("入队执行时 journal 应已落 task_forwarded（先记账后入队）")
	}
}

// crashWindowQueue 模拟「journal 已落、入队未达」的崩溃窗口：首次 Enqueue
// 确定性失败，重试时恢复。
type crashWindowQueue struct {
	*fakeQueue
	failFirst bool
}

func (q *crashWindowQueue) Enqueue(threadID, submissionID, text string) (string, bool, error) {
	if q.failFirst {
		q.failFirst = false
		return "", false, errors.New("账本暂不可用")
	}
	return q.fakeQueue.Enqueue(threadID, submissionID, text)
}

func TestForwardToMemberCrashWindowHealsEnqueueOnly(t *testing.T) {
	h := newDispatchHarness(t) // 不挂 OnEvent 的裸基座。
	queue := &crashWindowQueue{fakeQueue: h.queue, failFirst: true}
	service := NewService(h.store, h.journal, queue, h.resolver)
	run := forwardRunFixture("run-crash")
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	req := forwardRequest(uuid.NewString(), "analysis", "", "rev")
	if _, _, err := service.ForwardToMember(h.team, run, req); err == nil ||
		!strings.Contains(err.Error(), "转发入队失败") {
		t.Fatalf("首次入队失败应报错，实际 %v", err)
	}
	// 重试：journal 已有事件 → 只补入队，不重复记账。
	submissionID, duplicate, err := service.ForwardToMember(h.team, run, req)
	if err != nil || !duplicate {
		t.Fatalf("崩溃窗口重试应补投并返回 duplicate=true: err=%v duplicate=%v", err, duplicate)
	}
	events, _ := h.journal.Replay()
	count := 0
	for _, event := range events {
		if event.Type == EventTaskForwarded {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("补投不应重复记账，实际 %d 条事件", count)
	}
	if entry, ok := queue.firstEntry("s-rev"); !ok || entry.submissionID != submissionID {
		t.Fatalf("补投后账本应有转发条目: %+v ok=%v", entry, ok)
	}
}

// ---- 拒绝分支 ----

func TestForwardToMemberRejections(t *testing.T) {
	h := newForwardHarness(t)
	run := h.saveForwardRun(t, forwardRunFixture("run-fwd"))
	service := h.service
	cases := []struct {
		name string
		req  ForwardRequest
		want string
	}{
		{"请求标识非 UUID", ForwardRequest{RequestID: "not-uuid", SourceTaskID: "analysis", TargetMemberID: "rev"}, "转发请求标识无效"},
		{"来源双填", forwardRequest(uuid.NewString(), "analysis", "s-lead", "rev"), "二选一"},
		{"来源双空", ForwardRequest{RequestID: uuid.NewString(), TargetMemberID: "rev"}, "二选一"},
		{"任务源带 scope", func() ForwardRequest {
			req := forwardRequest(uuid.NewString(), "analysis", "", "rev")
			req.Scope = "main"
			return req
		}(), "任务来源不支持 scope"},
		{"目标成员不存在", forwardRequest(uuid.NewString(), "analysis", "", "nobody"), "目标成员不存在"},
		{"来源任务不存在", forwardRequest(uuid.NewString(), "ghost-task", "", "rev"), "来源任务不存在"},
		{"来源任务未完成", forwardRequest(uuid.NewString(), "pending", "", "rev"), "来源任务尚未完成"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := service.ForwardToMember(h.team, run, testCase.req)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("应报 %q，实际 %v", testCase.want, err)
			}
		})
	}
	// 终态 run 拒绝（含各终态）。
	for _, status := range []string{RunCompleted, RunFailed, RunCancelled, RunStopped, RunOutcomeUnknown} {
		terminal := forwardRunFixture("run-term-" + status)
		terminal.Status = status
		if err := h.store.SaveRun(terminal); err != nil {
			t.Fatalf("run 保存失败: %v", err)
		}
		_, _, err := service.ForwardToMember(h.team, terminal, forwardRequest(uuid.NewString(), "analysis", "", "rev"))
		if !errors.Is(err, ErrForwardRunFinished) {
			t.Fatalf("状态 %s 应拒绝转发（ErrForwardRunFinished），实际 %v", status, err)
		}
	}
	// waiting_input 不属于终态，允许转发。
	waiting := forwardRunFixture("run-waiting")
	waiting.Status = RunWaitingInput
	h.saveForwardRun(t, waiting)
	if _, _, err := service.ForwardToMember(h.team, waiting, forwardRequest(uuid.NewString(), "analysis", "", "rev")); err != nil {
		t.Fatalf("waiting_input 应允许转发，实际 %v", err)
	}
	// run 不在账本：ErrNotFound。
	missing := &Run{ID: "run-missing", TeamID: "team-1", Status: RunDispatching}
	if _, _, err := service.ForwardToMember(h.team, missing, forwardRequest(uuid.NewString(), "analysis", "", "rev")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("账本外 run 应报 ErrNotFound，实际 %v", err)
	}
}
