package team

// questions.go 的单元测试（§13.1 team/escalation 行：问题登记、版本化回答、
// 撤回、上限和恢复幂等）：观察点登记（提问卡/输入卡/普通权限卡边界、归属
// 来源任务、幂等重入）、同 Task 3 个未决上限、decision 按 questionID+version
// 过期拒绝、decisionID 幂等 duplicate、答案只消费一次、Respond 错误三分支
//（哨兵过期→撤回+409；选项无效/格式不正确→400 问题仍 open）、任务终态与
// Cancel 同批撤回、超时豁免（taskTimeoutExpired 重臂、expireRun 跳过）、
// journal 三事件重建与 JSON 账本往返、注册与 Cancel 并发（-race）。全部直驱
//（不启动循环/watcher goroutine），状态机正确性不靠固定 sleep 判定（§13.1）；
// Respond 经 respondAgent 注入缝观测（手工构造的 Session 成功回应会在
// signalLocked 关闭 nil 通道 panic——cancelAgentTurn 同因）。runner 集成面在
// integration-tests-3。

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

// ---- 测试基座（直驱，仿 actionsExecHarness）----

// questionsHarness 是问题链路的测试基座：不启动调度循环与 watcher，观察/回答
// /撤回全部同步直调；Respond 经 respondRecorder 注入。
type questionsHarness struct {
	store    *memStore
	journal  *Journal
	queue    *fakeQueue
	resolver *fakeResolver
	service  *Service
	team     *Team
	respond  *respondRecorder
}

// respondCall 记录一次 Respond 投递（请求 ID 与原样报文）。
type respondCall struct {
	requestID string
	response  *jsoncompat.Object
}

// respondRecorder 是 respondAgent 的测试注入：记录调用并按序返回预设结果
// （耗尽后恒 nil）。
type respondRecorder struct {
	mu      sync.Mutex
	calls   []respondCall
	results []error
}

func (r *respondRecorder) record(requestID string, response *jsoncompat.Object) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, respondCall{requestID: requestID, response: response})
	if len(r.calls) <= len(r.results) {
		return r.results[len(r.calls)-1]
	}
	return nil
}

func (r *respondRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func newQuestionsHarness(t *testing.T) *questionsHarness {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	h := &questionsHarness{
		store:    newMemStore(),
		journal:  journal,
		queue:    newFakeQueue(),
		resolver: newFakeResolver(),
		respond:  &respondRecorder{},
	}
	h.team = testTeam()
	for _, member := range h.team.Members {
		h.resolver.setSession(member.SessionID, newFakeSession(member.SessionID, "/tmp/"+member.ID))
		h.resolver.setCwd(member.SessionID, "/tmp/"+member.ID)
	}
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	h.service = NewService(h.store, h.journal, h.queue, h.resolver)
	h.service.respondAgent = func(session *acpruntime.Session, requestID string, response *jsoncompat.Object) error {
		return h.respond.record(requestID, response)
	}
	return h
}

// saveQuestionRun 落快照并返回执行状态（stateFor 不启动循环）。
func (h *questionsHarness) saveQuestionRun(t *testing.T, run *Run) *runState {
	t.Helper()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
	return h.service.stateFor(run.ID)
}

// questionRunFixture 构造含一个 running 任务（dev1，成员 dev）的 Run。
func questionRunFixture() *Run {
	return &Run{
		ID: "run-q", TeamID: "team-1", Goal: "实现头像上传", Mode: ModeSingle,
		Status: RunDispatching, Round: 1,
		Tasks: []Task{{
			ID: "dev1", MemberID: "dev", Status: TaskRunning, Ordinal: 0,
			SubmissionID: TaskSubmissionID("team-1", "run-q", "dev1", 1),
		}},
	}
}

// openQuestionOf 构造 open 状态的登记条目（上限/豁免用例的快照前置）。
func openQuestionOf(taskID, requestID string, version uint64) PendingQuestion {
	return PendingQuestion{
		QuestionID: QuestionIDOf(taskID, requestID),
		TaskID:     taskID,
		Version:    version,
		Kind:       QuestionKindQuestion,
		Summary:    "问题：" + requestID,
		Status:     QuestionStatusOpen,
	}
}

// questionCard 用真实 acpview API 构造 AskUserQuestion 提问卡（manager.onRequest
// → View.AddPermission 的同款参数形态：question 文本在 toolCall.content，选项
// 是 permission options）。
func questionCard(requestID, questionText string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn("任务派发全文")
		toolCall := jsoncompat.NewObject()
		toolCall.Set("title", "AskUserQuestion")
		content := jsoncompat.NewObject()
		content.Set("text", questionText)
		toolCall.Set("content", content)
		options := &jsoncompat.Array{}
		for _, pair := range [][2]string{{"opt-1", "选项一"}, {"opt-2", "选项二"}} {
			option := jsoncompat.NewObject()
			option.Set("optionId", pair[0])
			option.Set("name", pair[1])
			options.Items = append(options.Items, option)
		}
		params := jsoncompat.NewObject()
		params.Set("toolCall", toolCall)
		params.Set("options", options)
		view.AddPermission(requestID, params)
	}
}

// inputCard 构造 elicitation 输入卡（View.AddInput 的同款参数形态；schema 用
// 受支持的简单对象形态，让 message 原样进入快照）。
func inputCard(requestID, message string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn("任务派发全文")
		params := jsoncompat.NewObject()
		params.Set("message", message)
		schema := jsoncompat.NewObject()
		schema.Set("type", "object")
		properties := jsoncompat.NewObject()
		properties.Set("dsn", jsoncompat.NewObject())
		schema.Set("properties", properties)
		params.Set("requestedSchema", schema)
		view.AddInput(requestID, params)
	}
}

// plainPermissionCard 构造普通权限卡（title 非 AskUserQuestion：不登记）。
func plainPermissionCard(requestID string) func(*acpview.View) {
	return func(view *acpview.View) {
		view.BeginTurn("任务派发全文")
		toolCall := jsoncompat.NewObject()
		toolCall.Set("title", "权限请求")
		toolCall.Set("rawInput", "rm -rf /tmp/x")
		params := jsoncompat.NewObject()
		params.Set("toolCall", toolCall)
		params.Set("options", &jsoncompat.Array{})
		view.AddPermission(requestID, params)
	}
}

func questionSnapshot(drive ...func(*acpview.View)) *jsoncompat.Object {
	return newFakeSession("s-dev", "/tmp/dev", drive...).Snapshot()
}

func (h *questionsHarness) eventsOf(t *testing.T, eventType string) []*Event {
	t.Helper()
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	var matched []*Event
	for _, event := range events {
		if event != nil && event.RunID == "run-q" && event.Type == eventType {
			matched = append(matched, event)
		}
	}
	return matched
}

// optionResponse 构造手机端 permission 报文（respondWire 口径）。
func optionResponse(optionID string) *jsoncompat.Object {
	response := jsoncompat.NewObject()
	response.Set("optionId", optionID)
	return response
}

// ---- 登记与归属 ----

// TestObserveQuestionsRegistersQuestionCard 提问卡经 watcher 观察点登记：
// QuestionID 归属来源 Task、Kind=question、摘要与选项有界还原、Version=登记时
// 快照版本；重复观察幂等（一条事件、一条登记）。
func TestObserveQuestionsRegistersQuestionCard(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	state := h.saveQuestionRun(t, run)
	snapshot := questionSnapshot(questionCard("req-1", "选择哪个方案？"))
	dispatch := &taskDispatch{taskID: "dev1", sessionID: "s-dev"}
	h.service.observeQuestions(state, dispatch, snapshot)

	fresh := h.store.Run("run-q")
	if len(fresh.PendingQuestions) != 1 {
		t.Fatalf("应登记 1 个问题，实际 %+v", fresh.PendingQuestions)
	}
	question := fresh.PendingQuestions[0]
	if question.QuestionID != "dev1:req-1" || question.TaskID != "dev1" {
		t.Fatalf("问题应归属来源任务: %+v", question)
	}
	if question.Kind != QuestionKindQuestion || question.Status != QuestionStatusOpen {
		t.Fatalf("Kind/Status 不符: %+v", question)
	}
	if !strings.Contains(question.Summary, "选择哪个方案") {
		t.Fatalf("摘要应含问题文本: %q", question.Summary)
	}
	if question.Version != fresh.Version-1 { // 登记时的快照版本（登记落盘 +1）。
		t.Fatalf("Version 应为登记时快照版本 %d，实际 %d", fresh.Version-1, question.Version)
	}
	if len(question.Options) != 2 || question.Options[0].ID != "opt-1" ||
		question.Options[0].Label != "选项一" || question.Options[1].ID != "opt-2" {
		t.Fatalf("选项应还原 {id,label}: %+v", question.Options)
	}
	events := h.eventsOf(t, EventQuestionOpened)
	if len(events) != 1 {
		t.Fatalf("应恰有一条 question_opened，实际 %d", len(events))
	}
	if payloadString(events[0].Payload, "questionId") != "dev1:req-1" ||
		payloadString(events[0].Payload, "taskId") != "dev1" ||
		payloadString(events[0].Payload, "kind") != QuestionKindQuestion {
		t.Fatalf("question_opened 载荷不符: %+v", events[0].Payload)
	}
	if payloadUint64(events[0].Payload, "version") != question.Version {
		t.Fatalf("载荷 version 不符: %+v", events[0].Payload)
	}
	if events[0].IdempotencyKey != questionOpenedKey("team-1", "run-q", "dev1:req-1") {
		t.Fatalf("question_opened 幂等键不符: %s", events[0].IdempotencyKey)
	}

	// 幂等重入：再观察同卡不重复登记、不重复记账。
	h.service.observeQuestions(state, dispatch, snapshot)
	if fresh = h.store.Run("run-q"); len(fresh.PendingQuestions) != 1 {
		t.Fatalf("重复观察不得重复登记: %+v", fresh.PendingQuestions)
	}
	if len(h.eventsOf(t, EventQuestionOpened)) != 1 {
		t.Fatal("重复观察不得重复记账")
	}
}

// TestObserveQuestionsKinds 边界：输入卡按 kind=input 登记；普通权限卡不登记
// （§9 完整暂停语义归阶段四的边界声明）。
func TestObserveQuestionsKinds(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	state := h.saveQuestionRun(t, run)
	dispatch := &taskDispatch{taskID: "dev1", sessionID: "s-dev"}

	h.service.observeQuestions(state, dispatch,
		questionSnapshot(inputCard("req-9", "请提供数据库连接串")))
	fresh := h.store.Run("run-q")
	if len(fresh.PendingQuestions) != 1 || fresh.PendingQuestions[0].Kind != QuestionKindInput {
		t.Fatalf("输入卡应按 kind=input 登记: %+v", fresh.PendingQuestions)
	}
	if fresh.PendingQuestions[0].Summary != "请提供数据库连接串" {
		t.Fatalf("输入卡摘要应取 message: %+v", fresh.PendingQuestions[0])
	}

	h.service.observeQuestions(state, dispatch,
		questionSnapshot(plainPermissionCard("req-10")))
	if fresh = h.store.Run("run-q"); len(fresh.PendingQuestions) != 1 {
		t.Fatalf("普通权限卡不得登记: %+v", fresh.PendingQuestions)
	}
	if len(h.eventsOf(t, EventQuestionOpened)) != 1 {
		t.Fatal("普通权限卡不得产生登记事件")
	}
}

// TestRegisterQuestionCapAtThree 同 Task 未决问题上限（§5.4「最多保留 3 个」）：
// 达上限的新卡不登记留日志；answered/withdrawn 不占额度。
func TestRegisterQuestionCapAtThree(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{
		openQuestionOf("dev1", "req-1", 1),
		openQuestionOf("dev1", "req-2", 1),
		openQuestionOf("dev1", "req-3", 1),
	}
	state := h.saveQuestionRun(t, run)
	dispatch := &taskDispatch{taskID: "dev1", sessionID: "s-dev"}
	h.service.observeQuestions(state, dispatch,
		questionSnapshot(questionCard("req-4", "第四个问题？")))
	if fresh := h.store.Run("run-q"); len(fresh.PendingQuestions) != 3 {
		t.Fatalf("达上限后不得登记第四个: %+v", fresh.PendingQuestions)
	}
	if findQuestionIn(h.store.Run("run-q"), "dev1:req-4") != nil {
		t.Fatal("req-4 不应被登记")
	}
	// answered 释放额度：置一个 answered 后第四个可登记。
	fresh := h.store.Run("run-q")
	fresh.PendingQuestions[0].Status = QuestionStatusAnswered
	if err := h.store.SaveRun(fresh); err != nil { // memStore 乐观版本：fresh.Version 已同步。
		t.Fatalf("快照更新失败: %v", err)
	}
	h.service.observeQuestions(state, dispatch,
		questionSnapshot(questionCard("req-4", "第四个问题？")))
	if fresh = h.store.Run("run-q"); countOpenQuestions(fresh, "dev1") != 3 ||
		findQuestionIn(fresh, "dev1:req-4") == nil {
		t.Fatalf("answered 释放额度后 req-4 应可登记: %+v", fresh.PendingQuestions)
	}
}

// ---- decision：版本、幂等、只消费一次 ----

// TestAnswerQuestionConsumesOnce 成功路径：Respond 收到原样报文（requestID 剥自
// 问题 ID）、question_resolved 落账（幂等键=decision 键）、快照 answered；
// 二次回答（新 decisionID）409；同 decisionID 重放 duplicate 且不再投递
// （答案只允许消费一次，§5.4）。
func TestAnswerQuestionConsumesOnce(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 1)}
	h.saveQuestionRun(t, run)
	question := findQuestionIn(h.store.Run("run-q"), "dev1:req-1")

	decisionID := uuid.NewString()
	answered, duplicate, err := h.service.AnswerQuestion("run-q", "dev1:req-1",
		question.Version, decisionID, optionResponse("opt-1"))
	if err != nil {
		t.Fatalf("首次回答应成功: %v", err)
	}
	if duplicate || answered.Status != QuestionStatusAnswered {
		t.Fatalf("首次回答应落 answered 非 duplicate: %+v duplicate=%v", answered, duplicate)
	}
	if h.respond.count() != 1 {
		t.Fatalf("应恰投递一次，实际 %d", h.respond.count())
	}
	if call := h.respond.calls[0]; call.requestID != "req-1" ||
		payloadString(jsonObjectToMap(call.response), "optionId") != "opt-1" {
		t.Fatalf("Respond 应收到剥出的 requestID 与原样报文: %+v", call)
	}
	events := h.eventsOf(t, EventQuestionResolved)
	if len(events) != 1 {
		t.Fatalf("应恰有一条 question_resolved，实际 %d", len(events))
	}
	if events[0].IdempotencyKey != DecisionIdempotencyKey("team-1", "run-q", decisionID) {
		t.Fatalf("question_resolved 幂等键应为 decision 键: %s", events[0].IdempotencyKey)
	}
	if !strings.Contains(payloadString(events[0].Payload, "answerHead"), "opt-1") {
		t.Fatalf("answerHead 应含答案摘要: %+v", events[0].Payload)
	}

	// 二次回答（新 decisionID）：409，问题只消费一次。
	_, _, err = h.service.AnswerQuestion("run-q", "dev1:req-1", question.Version,
		uuid.NewString(), optionResponse("opt-2"))
	if !errors.Is(err, ErrQuestionNotOpen) {
		t.Fatalf("二次回答应 409 ErrQuestionNotOpen，实际 %v", err)
	}
	if h.respond.count() != 1 {
		t.Fatalf("二次回答不得再次投递，实际 %d", h.respond.count())
	}

	// 同 decisionID 重放：duplicate=true、零副作用。
	replayed, duplicate, err := h.service.AnswerQuestion("run-q", "dev1:req-1",
		question.Version, decisionID, optionResponse("opt-1"))
	if err != nil || !duplicate {
		t.Fatalf("同 decisionID 重放应 duplicate=true，实际 err=%v duplicate=%v", err, duplicate)
	}
	if replayed == nil || replayed.Status != QuestionStatusAnswered {
		t.Fatalf("重放应返回登记在案的 answered 快照: %+v", replayed)
	}
	if h.respond.count() != 1 {
		t.Fatalf("重放不得再次投递，实际 %d", h.respond.count())
	}
}

// jsonObjectToMap 把 jsoncompat 对象压平成 map（断言辅助；键值均为标量时无损）。
func jsonObjectToMap(object *jsoncompat.Object) map[string]any {
	out := map[string]any{}
	for _, key := range object.Keys() {
		value, _ := object.Get(key)
		out[key] = value
	}
	return out
}

// TestAnswerQuestionRejectsStaleVersion 版本过期拒绝（§5.4「过期版本必须拒绝
// 并要求刷新」）：errors.Is ErrVersionConflict（HTTP 409 口径），问题保持 open、
// 不投递。
func TestAnswerQuestionRejectsStaleVersion(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 3)}
	h.saveQuestionRun(t, run)

	_, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 2, uuid.NewString(), optionResponse("opt-1"))
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应 409 ErrVersionConflict，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "已过期") {
		t.Fatalf("错误文案应要求刷新: %v", err)
	}
	question := findQuestionIn(h.store.Run("run-q"), "dev1:req-1")
	if question.Status != QuestionStatusOpen {
		t.Fatalf("版本拒绝后问题应保持 open: %+v", question)
	}
	if h.respond.count() != 0 {
		t.Fatal("版本拒绝不得投递")
	}
	// 问题不存在 → 404 口径（ErrNotFound）。
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:nope", 1, uuid.NewString(), optionResponse("opt-1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的问题应 404，实际 %v", err)
	}
	// decisionID 非 UUID 直接拒绝。
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 3, "not-a-uuid", optionResponse("opt-1")); err == nil {
		t.Fatal("decisionID 必须是 UUID")
	}
}

// TestAnswerQuestionDuplicateAcrossQuestions 同一 decisionID 用于不同问题按
// 「同一决策标识不能用于不同内容」拒绝（creations 账本同款语义）。
func TestAnswerQuestionDuplicateAcrossQuestions(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{
		openQuestionOf("dev1", "req-1", 1),
		openQuestionOf("dev1", "req-2", 1),
	}
	h.saveQuestionRun(t, run)
	decisionID := uuid.NewString()
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 1, decisionID, optionResponse("opt-1")); err != nil {
		t.Fatalf("首个回答应成功: %v", err)
	}
	_, _, err := h.service.AnswerQuestion("run-q", "dev1:req-2", 1, decisionID, optionResponse("opt-1"))
	if err == nil || !strings.Contains(err.Error(), "同一决策标识不能用于不同内容") {
		t.Fatalf("同 decisionID 换问题应拒绝，实际 %v", err)
	}
}

// ---- Respond 错误三分支（评审问题⑧的解）----

// TestAnswerQuestionRespondErrorBranches 过期（哨兵）→ 撤回 + 409；报文无效
// （选项无效/格式不正确）→ 400 问题仍 open；未分类错误 → 原样上抛可重试。
func TestAnswerQuestionRespondErrorBranches(t *testing.T) {
	newSeeded := func(t *testing.T, results ...error) *questionsHarness {
		h := newQuestionsHarness(t)
		h.respond.results = results
		run := questionRunFixture()
		run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 1)}
		h.saveQuestionRun(t, run)
		return h
	}
	answer := func(h *questionsHarness) error {
		_, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 1, uuid.NewString(), optionResponse("opt-1"))
		return err
	}

	t.Run("哨兵过期撤回并409", func(t *testing.T) {
		h := newSeeded(t, acpruntime.ErrRequestExpired)
		if err := answer(h); !errors.Is(err, ErrQuestionExpired) {
			t.Fatalf("过期应 409 ErrQuestionExpired，实际 %v", err)
		}
		question := findQuestionIn(h.store.Run("run-q"), "dev1:req-1")
		if question.Status != QuestionStatusWithdrawn {
			t.Fatalf("过期应撤回问题: %+v", question)
		}
		withdrawn := h.eventsOf(t, EventQuestionWithdrawn)
		if len(withdrawn) != 1 || payloadString(withdrawn[0].Payload, "reason") != "stale" {
			t.Fatalf("应落 reason=stale 的撤回事件: %+v", withdrawn)
		}
		// 撤回后 decision → 409。
		if err := answer(h); !errors.Is(err, ErrQuestionNotOpen) {
			t.Fatalf("撤回后回答应 409，实际 %v", err)
		}
	})

	t.Run("文案兜底同样过期", func(t *testing.T) {
		h := newSeeded(t, errors.New("请求已过期，请刷新聊天"))
		if err := answer(h); !errors.Is(err, ErrQuestionExpired) {
			t.Fatalf("错误串兜底应命中过期分支，实际 %v", err)
		}
	})

	for _, invalid := range []error{
		errors.New("请选择有效的选项"),
		errors.New("请求回应格式不正确"),
	} {
		t.Run("报文无效保持open可重试_"+invalid.Error(), func(t *testing.T) {
			h := newSeeded(t, invalid)
			err := answer(h)
			if !errors.Is(err, ErrQuestionInvalidResponse) {
				t.Fatalf("报文无效应 400 ErrQuestionInvalidResponse，实际 %v", err)
			}
			if !strings.Contains(err.Error(), invalid.Error()) {
				t.Fatalf("错误应携带会话侧原文: %v", err)
			}
			question := findQuestionIn(h.store.Run("run-q"), "dev1:req-1")
			if question.Status != QuestionStatusOpen {
				t.Fatalf("报文无效不消费问题: %+v", question)
			}
			if len(h.eventsOf(t, EventQuestionResolved)) != 0 || len(h.eventsOf(t, EventQuestionWithdrawn)) != 0 {
				t.Fatal("报文无效不得落回答/撤回事实")
			}
			// 可重试：修正报文后同一问题回答成功。
			h.respond.results = nil
			if err := answer(h); err != nil {
				t.Fatalf("重试应成功: %v", err)
			}
		})
	}

	t.Run("未分类错误保持open", func(t *testing.T) {
		h := newSeeded(t, errors.New("Agent 进程已退出；操作结果未知"))
		err := answer(h)
		if errors.Is(err, ErrQuestionExpired) || errors.Is(err, ErrQuestionInvalidResponse) {
			t.Fatalf("未分类错误不得误判: %v", err)
		}
		if findQuestionIn(h.store.Run("run-q"), "dev1:req-1").Status != QuestionStatusOpen {
			t.Fatal("未分类错误问题应保持 open")
		}
	})
}

// ---- 撤回：任务终态 / Cancel ----

// TestTaskTerminalWithdrawsQuestions 任务终态同批撤回其 open 问题（事件序任务
// 事实先于问题撤回）；answered 不回写。
func TestTaskTerminalWithdrawsQuestions(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{
		openQuestionOf("dev1", "req-1", 1),
		openQuestionOf("dev1", "req-2", 1),
		{QuestionID: "dev1:req-3", TaskID: "dev1", Version: 1, Status: QuestionStatusAnswered},
	}
	state := h.saveQuestionRun(t, run)
	if !h.service.stopTask(state, "run-q", "dev1", "消息已在队列中移除") {
		t.Fatal("任务停靠应成功")
	}
	fresh := h.store.Run("run-q")
	if fresh.Tasks[0].Status != TaskStopped {
		t.Fatalf("任务应落 stopped: %+v", fresh.Tasks[0])
	}
	if fresh.PendingQuestions[0].Status != QuestionStatusWithdrawn ||
		fresh.PendingQuestions[1].Status != QuestionStatusWithdrawn {
		t.Fatalf("open 问题应随任务终态撤回: %+v", fresh.PendingQuestions)
	}
	if fresh.PendingQuestions[2].Status != QuestionStatusAnswered {
		t.Fatalf("answered 是终态不回写: %+v", fresh.PendingQuestions[2])
	}
	withdrawn := h.eventsOf(t, EventQuestionWithdrawn)
	if len(withdrawn) != 2 {
		t.Fatalf("应恰有两条撤回事件，实际 %d", len(withdrawn))
	}
	for _, event := range withdrawn {
		if payloadString(event.Payload, "reason") != "task_terminal" {
			t.Fatalf("撤回原因应为 task_terminal: %+v", event.Payload)
		}
	}
	stopped := h.eventsOf(t, EventTaskStopped)
	if len(stopped) != 1 {
		t.Fatalf("应恰有一条任务停靠事件，实际 %d", len(stopped))
	}
	// 事件序：任务事实（sequence 较小）先于问题撤回。
	if withdrawn[0].Sequence < stopped[0].Sequence {
		t.Fatalf("事件序应为任务事实先于问题撤回: task=%d question=%d",
			stopped[0].Sequence, withdrawn[0].Sequence)
	}
	// 撤回后 decision → 409。
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 1, uuid.NewString(), optionResponse("opt-1")); !errors.Is(err, ErrQuestionNotOpen) {
		t.Fatalf("撤回后回答应 409，实际 %v", err)
	}
}

// TestCancelWithdrawsOpenQuestions Cancel 单批撤回全部 open 问题（§5.4：Task
// 取消或 Run 终止时撤回未解决问题）。
func TestCancelWithdrawsOpenQuestions(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 1)}
	h.saveQuestionRun(t, run)
	if _, err := h.service.Cancel("run-q"); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	fresh := h.store.Run("run-q")
	if fresh.Status != RunCancelled {
		t.Fatalf("run 应落 cancelled: %s", fresh.Status)
	}
	if fresh.PendingQuestions[0].Status != QuestionStatusWithdrawn {
		t.Fatalf("取消应撤回 open 问题: %+v", fresh.PendingQuestions[0])
	}
	withdrawn := h.eventsOf(t, EventQuestionWithdrawn)
	if len(withdrawn) != 1 || payloadString(withdrawn[0].Payload, "reason") != "run_finished" {
		t.Fatalf("应落 reason=run_finished 的撤回事件: %+v", withdrawn)
	}
	// 事件序：run 事实先于问题撤回。
	cancelled := h.eventsOf(t, EventRunCancelled)
	if len(cancelled) != 1 || withdrawn[0].Sequence < cancelled[0].Sequence {
		t.Fatalf("事件序应为 run 事实先于问题撤回: run=%d question=%d",
			cancelled[0].Sequence, withdrawn[0].Sequence)
	}
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 1, uuid.NewString(), optionResponse("opt-1")); !errors.Is(err, ErrQuestionNotOpen) {
		t.Fatalf("撤回后回答应 409，实际 %v", err)
	}
}

// ---- 超时豁免（§5.4：开放问题期间任务/run 超时停算）----

// TestTaskTimeoutExpiredRearmsWithOpenQuestion 开放问题期间任务超时停算：
// 超时线已过仍返回 false 并重臂 dispatchedAt；无开放问题照常超时。
func TestTaskTimeoutExpiredRearmsWithOpenQuestion(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 1)}
	state := h.saveQuestionRun(t, run)
	dispatch := &taskDispatch{taskID: "dev1", sessionID: "s-dev"}

	// 未到超时线。
	state.mu.Lock()
	state.dispatchedAt["dev1"] = time.Now()
	state.mu.Unlock()
	if h.service.taskTimeoutExpired(state, dispatch, time.Hour) {
		t.Fatal("未到超时线不得判超时")
	}
	// 超时线已过 + 开放问题：重臂并继续等待（expireTask 不触发）。
	state.mu.Lock()
	state.dispatchedAt["dev1"] = time.Now().Add(-2 * time.Hour)
	rearmBefore := state.dispatchedAt["dev1"]
	state.mu.Unlock()
	if h.service.taskTimeoutExpired(state, dispatch, time.Nanosecond) {
		t.Fatal("开放问题期间不得判超时")
	}
	state.mu.Lock()
	rearmed := state.dispatchedAt["dev1"]
	state.mu.Unlock()
	if rearmed.Before(rearmBefore.Add(time.Hour)) {
		t.Fatalf("dispatchedAt 应重臂为当前时刻: before=%v after=%v", rearmBefore, rearmed)
	}
	// 重臂后未再超时。
	if h.service.taskTimeoutExpired(state, dispatch, time.Hour) {
		t.Fatal("重臂后未到线不得判超时")
	}
	// 无开放问题：照常超时。
	fresh := h.store.Run("run-q")
	fresh.PendingQuestions[0].Status = QuestionStatusAnswered
	if err := h.store.SaveRun(fresh); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	state.dispatchedAt["dev1"] = time.Now().Add(-2 * time.Hour)
	state.mu.Unlock()
	if !h.service.taskTimeoutExpired(state, dispatch, time.Nanosecond) {
		t.Fatal("无开放问题时超时线已过应判超时")
	}
}

// TestExpireRunSkipsOpenQuestionTasks run 超时豁免：批量 failed 跳过有 open
// 问题的任务（保持非终态、run 维持非终态驻留不 fold）；答案落定后下一轮超时
// 路径按剩余事实收口。
func TestExpireRunSkipsOpenQuestionTasks(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{openQuestionOf("dev1", "req-1", 1)}
	state := h.saveQuestionRun(t, run)

	if finished, _ := h.service.expireRun(state); finished {
		t.Fatal("开放问题期间 expireRun 不得终局 run")
	}
	fresh := h.store.Run("run-q")
	if fresh.Status != RunDispatching || fresh.Tasks[0].Status != TaskRunning {
		t.Fatalf("任务/run 应保持非终态驻留: run=%s task=%s", fresh.Status, fresh.Tasks[0].Status)
	}
	if len(h.eventsOf(t, EventTaskFailed)) != 0 {
		t.Fatal("开放问题期间不得落 task_failed")
	}

	// 答案落定后重跑超时路径：任务 failed、run 终局。
	if _, _, err := h.service.AnswerQuestion("run-q", "dev1:req-1", 1, uuid.NewString(), optionResponse("opt-1")); err != nil {
		t.Fatalf("回答失败: %v", err)
	}
	if finished, _ := h.service.expireRun(state); !finished {
		t.Fatal("答案落定后超时路径应终局 run")
	}
	fresh = h.store.Run("run-q")
	if fresh.Status != RunFailed || fresh.Tasks[0].Status != TaskFailed ||
		fresh.Tasks[0].StopReason != "run_timeout" {
		t.Fatalf("超时收口不符: run=%s task=%s reason=%s",
			fresh.Status, fresh.Tasks[0].Status, fresh.Tasks[0].StopReason)
	}
}

// ---- 重建与持久化 ----

// TestRebuildQuestionEvents journal 三事件重建：question_opened 追加（幂等）、
// question_resolved 置 answered、question_withdrawn 置 withdrawn；重复事件
// （登记重试崩溃窗口）只认首条。
func TestRebuildQuestionEvents(t *testing.T) {
	run := &Run{ID: "run-rb", TeamID: "team-1", Status: RunDispatching, Round: 1}
	apply := func(eventType string, payload map[string]any) {
		applyJournalEventToRun(run, &Event{RunID: run.ID, Type: eventType, Payload: payload})
	}
	open := map[string]any{
		"questionId": "dev1:req-1",
		"taskId":     "dev1",
		"version":    json.Number("7"),
		"kind":       QuestionKindQuestion,
		"summary":    "选择哪个方案？",
		"options": []any{
			map[string]any{"id": "opt-1", "label": "选项一"},
			map[string]any{"id": "opt-2", "label": "选项二"},
		},
	}
	apply(EventQuestionOpened, open)
	if len(run.PendingQuestions) != 1 {
		t.Fatalf("question_opened 应重建登记，实际 %+v", run.PendingQuestions)
	}
	question := run.PendingQuestions[0]
	if question.TaskID != "dev1" || question.Version != 7 ||
		question.Kind != QuestionKindQuestion || question.Summary != "选择哪个方案？" ||
		question.Status != QuestionStatusOpen {
		t.Fatalf("重建字段不符: %+v", question)
	}
	if len(question.Options) != 2 || question.Options[0].ID != "opt-1" || question.Options[1].Label != "选项二" {
		t.Fatalf("选项重建不符: %+v", question.Options)
	}
	// 崩溃窗口重复事件：幂等只认首条。
	apply(EventQuestionOpened, open)
	if len(run.PendingQuestions) != 1 {
		t.Fatalf("重复登记事件不得重复 append: %+v", run.PendingQuestions)
	}
	// 回答 → answered（后续撤回不回写）。
	apply(EventQuestionResolved, map[string]any{"questionId": "dev1:req-1", "taskId": "dev1", "answerHead": `{"optionId":"opt-1"}`})
	if run.PendingQuestions[0].Status != QuestionStatusAnswered {
		t.Fatalf("question_resolved 应置 answered: %+v", run.PendingQuestions[0])
	}
	apply(EventQuestionWithdrawn, map[string]any{"questionId": "dev1:req-1", "taskId": "dev1", "reason": "stale"})
	if run.PendingQuestions[0].Status != QuestionStatusAnswered {
		t.Fatalf("answered 是终态不回写: %+v", run.PendingQuestions[0])
	}
	// open → withdrawn 直接撤回。
	apply(EventQuestionOpened, map[string]any{
		"questionId": "dev1:req-2", "taskId": "dev1", "version": json.Number("7"),
		"kind": QuestionKindInput, "summary": "请提供连接串",
	})
	apply(EventQuestionWithdrawn, map[string]any{"questionId": "dev1:req-2", "taskId": "dev1", "reason": "run_finished"})
	if run.PendingQuestions[1].Status != QuestionStatusWithdrawn {
		t.Fatalf("question_withdrawn 应置 withdrawn: %+v", run.PendingQuestions[1])
	}
}

// TestPendingQuestionsPersistRoundTrip pendingQuestions 经 JSON 账本
// （team-runs.json）无损往返：漏 runKnownFields/jsonObject/Clone 任一处都会让
// 重启后问题登记静默清零。
func TestPendingQuestionsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(dir+"/teams.json", dir+"/team-runs.json")
	team := testTeam()
	if err := store.SaveTeam(team); err != nil {
		t.Fatal(err)
	}
	run := questionRunFixture()
	run.PendingQuestions = []PendingQuestion{{
		QuestionID: "dev1:req-1", TaskID: "dev1", Version: 42,
		Kind: QuestionKindQuestion, Summary: "选择哪个方案？",
		Options:  []QuestionOption{{ID: "opt-1", Label: "选项一"}},
		Status:   QuestionStatusOpen,
		OpenedAt: testTime,
	}}
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	fresh := LoadStore(dir+"/teams.json", dir+"/team-runs.json").Run("run-q")
	if fresh == nil || len(fresh.PendingQuestions) != 1 {
		t.Fatalf("pendingQuestions 必须随快照持久化: %+v", fresh)
	}
	question := fresh.PendingQuestions[0]
	if question.QuestionID != "dev1:req-1" || question.TaskID != "dev1" ||
		question.Version != 42 || question.Kind != QuestionKindQuestion ||
		question.Summary != "选择哪个方案？" || question.Status != QuestionStatusOpen ||
		len(question.Options) != 1 || question.Options[0].ID != "opt-1" {
		t.Fatalf("问题字段往返不符: %+v", question)
	}
	if !question.OpenedAt.Equal(testTime) {
		t.Fatalf("OpenedAt 往返不符: %v", question.OpenedAt)
	}
	// Clone 深拷贝（Options 不共享底层数组）。
	cloned := fresh.Clone()
	cloned.PendingQuestions[0].Options[0].Label = "被改写"
	if fresh.PendingQuestions[0].Options[0].Label != "选项一" {
		t.Fatal("Clone 必须深拷贝选项")
	}
}

// TestQuestionIDHelpers 问题 ID 的组合与剥回（任务 ID 无冒号，首冒号无歧义）。
func TestQuestionIDHelpers(t *testing.T) {
	if got := QuestionIDOf("dev1", "req-1"); got != "dev1:req-1" {
		t.Fatalf("组合不符: %s", got)
	}
	if got := RequestIDOf("dev1:req-1"); got != "req-1" {
		t.Fatalf("剥回不符: %s", got)
	}
	// 请求 ID 含冒号也不受影响（首冒号是分隔符）。
	if got := RequestIDOf(QuestionIDOf("actfix-lead-1", "we:id")); got != "we:id" {
		t.Fatalf("含冒号请求 ID 应完整剥回: %s", got)
	}
	if got := RequestIDOf("noseparator"); got != "noseparator" {
		t.Fatalf("无分隔符原样返回: %s", got)
	}
}

// ---- 并发（-race）----

// TestRegisterQuestionConcurrentWithCancel 注册与 Cancel 并发：全部快照变更经
// state.mu 串行；终态一致性＝run cancelled 且无 open 问题残留（终局批撤回）。
// go test -race 下验证无数据竞争。
func TestRegisterQuestionConcurrentWithCancel(t *testing.T) {
	h := newQuestionsHarness(t)
	run := questionRunFixture()
	state := h.saveQuestionRun(t, run)
	dispatch := &taskDispatch{taskID: "dev1", sessionID: "s-dev"}

	// 每个并发观察者用独立请求 ID 的问题卡（登记互不相干，全部经 state.mu）。
	snapshots := make([]*jsoncompat.Object, 8)
	for i := range snapshots {
		snapshots[i] = questionSnapshot(questionCard("req-"+strings.Repeat("a", i+1), "并发问题"))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(snapshot *jsoncompat.Object) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				h.service.observeQuestions(state, dispatch, snapshot)
			}
		}(snapshots[i])
	}
	// 与注册并发的 Cancel：终局批必须撤回已登记的问题。
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		_, _ = h.service.Cancel("run-q")
	}()
	wg.Wait()
	<-cancelDone

	fresh := h.store.Run("run-q")
	if fresh.Status != RunCancelled {
		t.Fatalf("run 应落 cancelled，实际 %s", fresh.Status)
	}
	for _, question := range fresh.PendingQuestions {
		if question.Status == QuestionStatusOpen {
			t.Fatalf("取消后不得残留 open 问题: %+v", question)
		}
	}
}
