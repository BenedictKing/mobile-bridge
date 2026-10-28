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

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
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

func (q *crashWindowQueue) Enqueue(threadID, submissionID, text string, attachments ...string) (string, bool, error) {
	if q.failFirst {
		q.failFirst = false
		return "", false, errors.New("账本暂不可用")
	}
	return q.fakeQueue.Enqueue(threadID, submissionID, text, attachments...)
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

// ---- 评审遗留③：时间串固化于 journal payload 的幂等判别 ----

// TestForwardToMemberCrashWindowReplayReusesRecordedAt 判别（评审遗留③）：
// 崩溃窗口（journal 已落 task_forwarded、账本未达）补投时，重组内容必须复用
// payload 固化的 recordedAt——时间行参与内容身份，现取时间会重组出不同字节
// 的「首次内容」，重放不再可判定（真实队列按同 id 不同内容拒绝）。
func TestForwardToMemberCrashWindowReplayReusesRecordedAt(t *testing.T) {
	h := newDispatchHarness(t) // 不挂 OnEvent 的裸基座。
	run := forwardRunFixture("run-recorded")
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	requestID := uuid.NewString()
	submissionID := ForwardSubmissionID(h.team.ID, run.ID, requestID)
	idempotencyKey := ForwardIdempotencyKey(h.team.ID, run.ID, requestID)
	const at = "2026-01-02T15:04:05+08:00"
	req := forwardRequest(requestID, "analysis", "", "rev")
	// 预演首次合成（补投内容必须与其字节全等）。
	contextText, note, _, _, err := h.service.composeForward(h.team, h.store.Run(run.ID), req, nil, at)
	if err != nil {
		t.Fatalf("预演合成失败: %v", err)
	}
	want := ComposeTaskMessage(contextText, note)
	// 预置 journal 事实（recordedAt 已固化；账本条目缺失=崩溃窗口）。
	event := newEvent(run.ID, EventTaskForwarded, "user", map[string]any{
		"targetMemberId": "rev",
		"sessionId":      "s-rev",
		"submissionId":   submissionID,
		"recordedAt":     at,
		"contextChars":   jsoncompat.Num(len([]rune(contextText))),
		"truncated":      false,
		"sourceTaskId":   "analysis",
	})
	event.IdempotencyKey = idempotencyKey
	if err := h.journal.Append(event); err != nil {
		t.Fatalf("预置事件失败: %v", err)
	}
	// 补投：duplicate=true、零新事件、内容与预演字节全等（时间行复用 at）。
	got, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || !duplicate {
		t.Fatalf("崩溃窗口补投应成功且 duplicate=true: err=%v duplicate=%v", err, duplicate)
	}
	if got != submissionID {
		t.Fatalf("补投 submissionID 应恒等，实际 %s", got)
	}
	entry, ok := h.queue.firstEntry("s-rev")
	if !ok {
		t.Fatal("补投后账本应有转发条目")
	}
	// 时间行判别（不依赖生产函数重算）：必须逐字复用 payload 的 recordedAt。
	if !strings.Contains(entry.text, "时间："+at) {
		t.Fatalf("补投时间行必须复用 payload 固化的 recordedAt=%s:\n%s", at, entry.text)
	}
	if entry.text != want {
		t.Fatalf("补投内容必须与首次合成字节全等（时间行复用 payload 的 recordedAt）:\n%s\n--- 实际 ---\n%s", want, entry.text)
	}
	events, _ := h.journal.Replay()
	count := 0
	for _, e := range events {
		if e.Type == EventTaskForwarded {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("补投不得重复记账，实际 %d 条事件", count)
	}
}

// seedingTranscripts 在首次转录提取（ForwardToMember 的锁外 ③ 段）时模拟
// 「并发同 id 请求抢先完成记账 + 入队」——本请求随后在 ④ 复查发现事实，
// 只补投。
type seedingTranscripts struct {
	text string
	seed func()
	done bool
}

func (f *seedingTranscripts) BoundedTranscript(sessionID, scope string) (string, bool, error) {
	if !f.done {
		f.done = true
		f.seed()
	}
	return f.text, false, nil
}

// TestForwardToMemberConcurrentPreemptEnqueuesIdenticalContent 判别（评审
// 遗留③的并发形态）：②③ 间隙被并发同 id 请求抢先记账后，本请求的补投内容
// 必须与抢先者入队的内容字节全等——现取时间重组会在「同 submissionID 不同
// 内容」上被账本拒绝（「同一消息标识不能用于不同内容」）。
func TestForwardToMemberConcurrentPreemptEnqueuesIdenticalContent(t *testing.T) {
	h := newDispatchHarness(t)
	run := forwardRunFixture("run-preempt")
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	requestID := uuid.NewString()
	submissionID := ForwardSubmissionID(h.team.ID, run.ID, requestID)
	idempotencyKey := ForwardIdempotencyKey(h.team.ID, run.ID, requestID)
	const at = "2026-01-02T15:04:05+08:00"
	req := forwardRequest(requestID, "", "s-lead", "dev")
	req.Scope = "main"
	transcriptText := "用户：请复核方案\n助手：方案可行，附两处修改建议"
	// 抢先者的内容（recordedAt=at 的首次合成）。
	contextText, note, _, _, err := h.service.composeForward(h.team, h.store.Run(run.ID), req, &forwardTranscript{text: transcriptText}, at)
	if err != nil {
		t.Fatalf("预演合成失败: %v", err)
	}
	want := ComposeTaskMessage(contextText, note)
	seed := func() {
		event := newEvent(run.ID, EventTaskForwarded, "user", map[string]any{
			"targetMemberId":  "dev",
			"sessionId":       "s-dev",
			"submissionId":    submissionID,
			"recordedAt":      at,
			"contextChars":    jsoncompat.Num(len([]rune(contextText))),
			"truncated":       false,
			"sourceSessionId": "s-lead",
		})
		event.IdempotencyKey = idempotencyKey
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("抢先记账失败: %v", err)
		}
		if _, _, err := h.queue.Enqueue("s-dev", submissionID, want); err != nil {
			t.Fatalf("抢先入队失败: %v", err)
		}
	}
	h.service.Transcripts = &seedingTranscripts{text: transcriptText, seed: seed}
	sub, duplicate, err := h.service.ForwardToMember(h.team, run, req)
	if err != nil || !duplicate {
		t.Fatalf("并发抢先后的补投应零错误且 duplicate=true: err=%v duplicate=%v", err, duplicate)
	}
	if sub != submissionID {
		t.Fatalf("补投 submissionID 应恒等，实际 %s", sub)
	}
	if calls := h.queue.callCount(submissionID); calls != 2 {
		t.Fatalf("补投应第二次 Enqueue（同 id 同内容幂等），实际 %d 次", calls)
	}
	entry, _ := h.queue.firstEntry("s-dev")
	// 时间行判别（不依赖生产函数重算）：补投不得带新时间戳。
	if !strings.Contains(entry.text, "时间："+at) {
		t.Fatalf("补投时间行必须复用抢先者记账的 recordedAt=%s:\n%s", at, entry.text)
	}
	if entry.text != want {
		t.Fatalf("补投内容必须与抢先者入队的内容字节全等:\n%s\n--- 实际 ---\n%s", want, entry.text)
	}
	events, _ := h.journal.Replay()
	count := 0
	for _, e := range events {
		if e.Type == EventTaskForwarded {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("并发抢先不得重复记账，实际 %d 条事件", count)
	}
}

// TestForwardToMemberStripsActionBlocks 转发正文先剥 <team-actions> 动作块
// （BuildTaskContext 注入依赖结果的同族义务，context.go）：Lead 结果里的
// 动作信封绝不由转发走私进下游——转发目标是 Lead 时，回显信封会以新
// sourceTaskId 产生新的合法 ActionKey 并在 autoDispatch 下被执行。任务源与
// 会话源转录同剥；正文只剩动作块的任务源转发确定性报错（没有可转发的
// 结果内容）。
func TestForwardToMemberStripsActionBlocks(t *testing.T) {
	h := newForwardHarness(t)
	run := forwardRunFixture("run-fwd-strip")
	envelope := "<team-actions>{\"actions\":[{\"type\":\"summarize\"}]}</team-actions>"
	run.Tasks[0].Result = envelope + "\n分析结论：接口需要鉴权。"
	run.Tasks[0].ResultDetail.Result = run.Tasks[0].Result
	h.saveForwardRun(t, run)

	// 任务源 → Lead（最危险的走私目标）：入队文本剥块但保留正文。
	if _, _, err := h.service.ForwardToMember(h.team, run, forwardRequest(uuid.NewString(), "analysis", "", "lead")); err != nil {
		t.Fatalf("剥离后应可转发: %v", err)
	}
	entry, ok := h.queue.firstEntry("s-lead")
	if !ok {
		t.Fatal("目标成员会话应收到转发消息")
	}
	if strings.Contains(entry.text, actionsOpenTag) || strings.Contains(entry.text, actionsCloseTag) {
		t.Fatalf("转发文本不得携带动作信封:\n%s", entry.text)
	}
	if !strings.Contains(entry.text, "分析结论：接口需要鉴权。") {
		t.Fatalf("剥离只去动作块，正文应保留:\n%s", entry.text)
	}
	// 来源自带的摘要行照常保留（剥离只作用于动作块）。
	if !strings.Contains(entry.text, "变更文件：src/a.go、src/b.go") {
		t.Fatalf("来源摘要行应保留:\n%s", entry.text)
	}

	// 会话源转录里的动作块同样剥除。
	h.service.Transcripts = &fakeTranscripts{text: "Lead：" + envelope + "\n会上结论如下"}
	if _, _, err := h.service.ForwardToMember(h.team, run, ForwardRequest{
		RequestID: uuid.NewString(), SourceSessionID: "s-lead", TargetMemberID: "rev",
	}); err != nil {
		t.Fatalf("会话源剥离后应可转发: %v", err)
	}
	sessionEntry, ok := h.queue.firstEntry("s-rev")
	if !ok {
		t.Fatal("目标成员会话应收到会话源转发消息")
	}
	if strings.Contains(sessionEntry.text, actionsOpenTag) || strings.Contains(sessionEntry.text, actionsCloseTag) {
		t.Fatalf("会话源转发文本不得携带动作信封:\n%s", sessionEntry.text)
	}
	if !strings.Contains(sessionEntry.text, "会上结论如下") {
		t.Fatalf("会话源正文应保留:\n%s", sessionEntry.text)
	}

	// 正文只剩动作块（无摘要可拼）的任务源转发：确定性报错，不投空壳信封。
	onlyBlock := forwardRunFixture("run-fwd-onlyblock")
	onlyBlock.Tasks[0].Result = envelope
	onlyBlock.Tasks[0].ResultDetail = &TaskResult{SessionID: "s-lead", TurnID: "turn-9", Result: envelope}
	h.saveForwardRun(t, onlyBlock)
	if _, _, err := h.service.ForwardToMember(h.team, onlyBlock, forwardRequest(uuid.NewString(), "analysis", "", "rev")); err == nil ||
		!strings.Contains(err.Error(), "没有可转发的结果内容") {
		t.Fatalf("只剩动作块的结果应确定性报错，实际 %v", err)
	}
}
