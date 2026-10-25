package team

// questions.go 是成员主动提问的登记、回答与撤回（§5.4、§9，§12 阶段三工作项
// 5，§13.1 team/escalation 行）：watcher 观察在途任务时兼读会话快照顶层的
// requests——AskUserQuestion 提问卡（成员把提问桥接在 session/request_
// permission 上，acpview 以 params.question=true 标记）与 elicitation 输入卡
// ——把未见条目登记为 Run 快照的 PendingQuestions。登记后任务保持 running、
// Run 不强制 waiting_input（§5.4 独立分支继续），开放问题期间任务与 run 超时
// 豁免（用户未回答不烧时钟）；回答走 decision：按 questionID+version 过期
// 拒绝、decisionID 幂等（重复提交 duplicate）、答案原样经 session.Respond
// 送达成员会话（适配器工具结果），只允许消费一次；Task 取消/Run 终止/会话卡
// 过期撤回未解决问题。
//
// 边界声明（v1 取舍，差异写入 CHANGELOG）：普通权限卡不登记（维持既有超时
// 路径）；§9「问题等待必须暂停」的完整 waiting_input 语义归阶段四。本文件的
// 落账全部走 runner.go 的 commitRun 单批（§3.1 原则 1：任务/问题/run 事实
// 原子落账，绝不两段提交）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// maxOpenQuestionsPerTask 是同 Task 的未决问题上限（§5.4「每个 Task 最多保留
// 3 个未解决问题」）：达到上限的新卡不再登记、留日志（会话侧卡片不受影响，
// 用户仍可在原会话直接回答）。
const maxOpenQuestionsPerTask = 3

// questionSummaryRunes 是问题摘要的截断长度（§10.3 有界 payload：逐条截断，
// 绝不整包拼接后才检查）；answerHeadRunes 是答案有界头部（question_resolved
// 载荷，完整答案在会话侧，事件只留审计摘要）。
const (
	questionSummaryRunes = 240
	answerHeadRunes      = 200
)

// 撤回原因（question_withdrawn payload.reason）：task_terminal 任务终态
// （取消/停靠/完成/失败等）；run_finished Run 终局/取消；stale 会话侧问题卡
// 已消失（Respond 报过期）。
const (
	questionWithdrawTaskTerminal = "task_terminal"
	questionWithdrawRunFinished  = "run_finished"
	questionWithdrawStale        = "stale"
)

var (
	// ErrQuestionNotOpen 表示问题已回答或已撤回——答案只允许消费一次
	//（§5.4），撤回后 decision 同样拒绝。HTTP 侧映射 409。
	ErrQuestionNotOpen = errors.New("问题已回答或已撤回，请刷新")
	// ErrQuestionExpired 表示成员会话里的问题卡已消失（Respond 报过期哨兵）：
	// 登记随之撤回（withdrawn，reason=stale）。文案与 acpruntime.ErrRequestExpired
	// 一致（HTTP 文本不变），HTTP 侧映射 409。
	ErrQuestionExpired = errors.New("请求已过期，请刷新聊天")
	// ErrQuestionInvalidResponse 表示回答报文未通过会话侧校验（选项无效/
	// 格式不正确——respondWire 不消费请求、卡仍可答）。HTTP 侧映射 400，
	// 问题保持 open。
	ErrQuestionInvalidResponse = errors.New("问题回应无效")
)

// QuestionIDOf 组合成 Run 内唯一的问题 ID：{taskID}:{requestID}。会话请求 ID
// 在进程内可能跨成员撞号（acpview nextID 按 session 计数），任务前缀保证唯一，
// 并可直接回溯来源 Task（§5.4「问题包含来源 Task」）。任务 ID 过成员 ID 正则
// （无冒号），首个冒号即分隔符。
func QuestionIDOf(taskID, requestID string) string {
	return taskID + ":" + requestID
}

// RequestIDOf 从问题 ID 剥回会话侧请求 ID（QuestionIDOf 的逆；任务 ID 无
// 冒号，首个冒号无歧义，请求 ID 含冒号也不受影响）。
func RequestIDOf(questionID string) string {
	if at := strings.IndexByte(questionID, ':'); at >= 0 {
		return questionID[at+1:]
	}
	return questionID
}

// DecisionIdempotencyKey 生成决策的稳定幂等键（§8.2 变体；§9「所有决策都
// 写入 journal 并带 decisionID」）：team:{teamID}:run:{runID}:decision:{decisionID}。
func DecisionIdempotencyKey(teamID, runID, decisionID string) string {
	return fmt.Sprintf("team:%s:run:%s:decision:%s", teamID, runID, decisionID)
}

// questionOpenedKey 是问题登记事实的幂等键：team:{teamID}:run:{runID}:
// question:{questionID}。journal.Append 不去重（journal.go），登记重试（journal
// 已落、快照未存的崩溃窗口）靠它预扫描防重——settleLeadTaskLocked 同款。
func questionOpenedKey(teamID, runID, questionID string) string {
	return fmt.Sprintf("team:%s:run:%s:question:%s", teamID, runID, questionID)
}

// findQuestionIn 返回 run 快照中指定 ID 的问题（无则 nil）。
func findQuestionIn(run *Run, questionID string) *PendingQuestion {
	if run == nil {
		return nil
	}
	for i := range run.PendingQuestions {
		if run.PendingQuestions[i].QuestionID == questionID {
			return &run.PendingQuestions[i]
		}
	}
	return nil
}

// hasOpenQuestion 报告 taskID 是否仍有未决问题（超时豁免的判定依据，纯函数）。
func hasOpenQuestion(run *Run, taskID string) bool {
	return countOpenQuestions(run, taskID) > 0
}

// countOpenQuestions 统计 taskID 的未决（open）问题数（纯函数）。
func countOpenQuestions(run *Run, taskID string) int {
	if run == nil {
		return 0
	}
	count := 0
	for i := range run.PendingQuestions {
		if run.PendingQuestions[i].TaskID == taskID && run.PendingQuestions[i].Status == QuestionStatusOpen {
			count++
		}
	}
	return count
}

// questionEntry 从快照 requests 条目提取问题形态（纯函数）：仅 AskUserQuestion
// 提问卡（method=acp/requestPermission 且 params.question==true，acpview
// view.go AddPermission 的标记）与输入卡（method=acp/requestInput，acpruntime
// pendingKinds 的 input 口径）登记；普通权限卡返回空 kind（边界声明：维持既有
// 超时路径，§9 完整暂停语义归阶段四）。返回 (kind, summary, options)。
func questionEntry(entry *jsoncompat.Object) (kind, summary string, options []QuestionOption) {
	if entry == nil {
		return "", "", nil
	}
	method, _ := entry.MustGet("method").(string)
	params, _ := entry.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		return "", "", nil
	}
	switch method {
	case "acp/requestPermission":
		if question, _ := params.MustGet("question").(bool); !question {
			return "", "", nil
		}
		text, _ := params.MustGet("text").(string)
		title, _ := params.MustGet("title").(string)
		return QuestionKindQuestion, firstNonEmpty(strings.TrimSpace(text), title), questionOptions(params)
	case "acp/requestInput":
		message, _ := params.MustGet("message").(string)
		return QuestionKindInput, strings.TrimSpace(message), nil
	}
	return "", "", nil
}

// questionOptions 提取提问卡的答案选项（acpview 把 permission options 映射为
// {id, label, kind}；无 id 的残缺条目跳过）。
func questionOptions(params *jsoncompat.Object) []QuestionOption {
	list, _ := params.MustGet("options").(*jsoncompat.Array)
	if list == nil {
		return nil
	}
	var options []QuestionOption
	for _, item := range list.Items {
		entry, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		option := QuestionOption{}
		option.ID, _ = entry.MustGet("id").(string)
		option.Label, _ = entry.MustGet("label").(string)
		if option.ID == "" {
			continue
		}
		options = append(options, option)
	}
	return options
}

// observeQuestions 是 watcher 的问题观察点（pollTask 在 matchTurn 命中且 turn
// inProgress 时调用——成员单活动任务保证 requests 归属本任务；不持 state.mu）：
// 对快照顶层的未见条目逐个登记。快照先查（廉价内存克隆），已登记（任何状态）
// 的 ID 不重复登记——首个登记生效，会话侧同 ID 新卡由 Respond 过期路径自愈。
func (s *Service) observeQuestions(state *runState, dispatch *taskDispatch, snapshot *jsoncompat.Object) {
	requests, _ := snapshot.MustGet("requests").(*jsoncompat.Array)
	if requests == nil {
		return
	}
	for _, item := range requests.Items {
		entry, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		requestID, _ := entry.MustGet("id").(string)
		if requestID == "" {
			continue
		}
		kind, summary, options := questionEntry(entry)
		if kind == "" {
			continue // 普通权限卡：不登记（边界声明）。
		}
		questionID := QuestionIDOf(dispatch.taskID, requestID)
		run := s.store.Run(state.runID)
		if run == nil || terminalRun(run.Status) {
			return
		}
		if findQuestionIn(run, questionID) != nil {
			continue
		}
		task := findTaskIn(run, dispatch.taskID)
		if task == nil || terminalTask(task.Status) {
			return
		}
		s.registerQuestion(state, state.runID, dispatch.taskID, questionID, kind, summary, options)
	}
}

// registerQuestion 单批 commitRun 登记一个问题（§5.4）：question_opened 事件
// （幂等键预扫描防重）+ 快照 append（Status=open、Version=登记时 Run 快照版本）。
// 同 Task 未决达 maxOpenQuestionsPerTask 不登记、留日志；任务保持 running、Run
// 不强制 waiting_input（独立分支继续）。
func (s *Service) registerQuestion(state *runState, runID, taskID, questionID, kind, summary string, options []QuestionOption) {
	state.mu.Lock()
	defer state.mu.Unlock()
	recorded := s.recordedActionKeys(runID)
	key := ""
	capped := false
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		capped = false
		if terminalRun(fresh.Status) {
			return nil, false
		}
		if findQuestionIn(fresh, questionID) != nil {
			return nil, false // 已被并发登记（first-wins）。
		}
		if countOpenQuestions(fresh, taskID) >= maxOpenQuestionsPerTask {
			capped = true
			return nil, false
		}
		question := PendingQuestion{
			QuestionID: questionID,
			TaskID:     taskID,
			Version:    fresh.Version,
			Kind:       kind,
			Summary:    truncateRunes(strings.TrimSpace(summary), questionSummaryRunes),
			Options:    options,
			Status:     QuestionStatusOpen,
			OpenedAt:   time.Now(),
		}
		fresh.PendingQuestions = append(fresh.PendingQuestions, question)
		key = questionOpenedKey(fresh.TeamID, fresh.ID, questionID)
		if recorded[key] {
			return nil, true // 崩溃窗口收敛：事实已在 journal，只补快照。
		}
		payload := map[string]any{
			"questionId": questionID,
			"taskId":     taskID,
			"version":    json.Number(strconv.FormatUint(question.Version, 10)),
			"kind":       kind,
			"summary":    question.Summary,
		}
		if len(options) > 0 {
			items := make([]any, 0, len(options))
			for i := range options {
				items = append(items, map[string]any{"id": options[i].ID, "label": options[i].Label})
			}
			payload["options"] = items
		}
		event := newEvent(fresh.ID, EventQuestionOpened, "watcher", payload)
		event.IdempotencyKey = key
		return []*Event{event}, true
	})
	if err != nil {
		log.Printf("team run %s 问题 %s 登记记账失败（watcher 继续观察重试）: %v", runID, questionID, err)
		return
	}
	if capped {
		log.Printf("team run %s 任务 %s 的未决问题已达上限 %d，新问题 %s 不再登记（可在原会话直接回答）",
			runID, taskID, maxOpenQuestionsPerTask, questionID)
	}
}

// withdrawTaskQuestions 撤回 run 快照里 taskID 的全部 open 问题（§5.4：Task
// 终态收口），返回配套 question_withdrawn 事件。事件序约定：任务事实先于问题
// 撤回——调用方把本函数的事件追加在任务终态事件之后（同一 commitRun 批）。
// 纯快照变更，落账与保存归调用方。
func withdrawTaskQuestions(run *Run, taskID, reason, source string) []*Event {
	if run == nil {
		return nil
	}
	var events []*Event
	for i := range run.PendingQuestions {
		question := &run.PendingQuestions[i]
		if question.TaskID != taskID || question.Status != QuestionStatusOpen {
			continue
		}
		question.Status = QuestionStatusWithdrawn
		events = append(events, newEvent(run.ID, EventQuestionWithdrawn, source, map[string]any{
			"questionId": question.QuestionID,
			"taskId":     question.TaskID,
			"reason":     reason,
		}))
	}
	return events
}

// withdrawRunQuestions 撤回 run 内全部 open 问题（Cancel/终局收口：Run 终止时
// 撤回未解决问题，撤回后 decision → 409）。纯快照变更，落账归调用方。
func withdrawRunQuestions(run *Run, reason, source string) []*Event {
	if run == nil {
		return nil
	}
	var events []*Event
	for i := range run.PendingQuestions {
		question := &run.PendingQuestions[i]
		if question.Status != QuestionStatusOpen {
			continue
		}
		question.Status = QuestionStatusWithdrawn
		events = append(events, newEvent(run.ID, EventQuestionWithdrawn, source, map[string]any{
			"questionId": question.QuestionID,
			"taskId":     question.TaskID,
			"reason":     reason,
		}))
	}
	return events
}

// withdrawQuestionStale 撤回因会话卡消失而过期的问题（AnswerQuestion 的
// Respond 报过期后的收口）：question_withdrawn（reason=stale）与快照置 withdrawn
// 单批落账；问题已被并发回答/撤回时 no-op（first-wins）。
func (s *Service) withdrawQuestionStale(runID, questionID string) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false // 终局路径已撤回。
		}
		question := findQuestionIn(fresh, questionID)
		if question == nil || question.Status != QuestionStatusOpen {
			return nil, false
		}
		question.Status = QuestionStatusWithdrawn
		return []*Event{newEvent(runID, EventQuestionWithdrawn, "user", map[string]any{
			"questionId": questionID,
			"taskId":     question.TaskID,
			"reason":     questionWithdrawStale,
		})}, true
	})
	if err != nil {
		log.Printf("team run %s 过期问题 %s 撤回记账失败: %v", runID, questionID, err)
	}
}

// questionAnswerable 校验问题可被回答（纯函数）：必须 open，且提交版本与登记
// 版本一致（过期版本拒绝并要求刷新，§5.4）。错误文案复用既有哨兵口径——
// ErrQuestionNotOpen / ErrVersionConflict（HTTP 侧 409 映射不变）。
func questionAnswerable(question *PendingQuestion, version uint64) error {
	if question.Status != QuestionStatusOpen {
		return fmt.Errorf("%w: 问题 %s 当前状态 %s", ErrQuestionNotOpen, question.QuestionID, question.Status)
	}
	if version != question.Version {
		return fmt.Errorf("%w: 问题 %s 已过期，请刷新后重试（登记 version=%d，提交 version=%d）",
			ErrVersionConflict, question.QuestionID, question.Version, version)
	}
	return nil
}

// questionMember 解析问题来源任务绑定的成员（答案投递目标）。
func (s *Service) questionMember(run *Run, question *PendingQuestion) *TeamMember {
	if run == nil || question == nil {
		return nil
	}
	task := findTaskIn(run, question.TaskID)
	if task == nil {
		return nil
	}
	return memberOf(s.store.Team(run.TeamID), task.MemberID)
}

// AnswerQuestion 按问题 ID 与版本回答成员问题（§5.4 的 decision 入口）：
//
//   - 幂等：decisionID 必须是 UUID；同 decisionID 重放返回 (最新问题快照,
//     duplicate=true, nil)（§9「重复提交返回 duplicate」），用于不同问题则
//     拒绝（「同一创建请求不能用于不同内容」的同款语义）；
//   - 版本：问题不存在 → ErrNotFound（404）；已回答/已撤回 →
//     ErrQuestionNotOpen（409）；version 与登记时不符 → ErrVersionConflict
//     包装的「问题已过期请刷新」（409）；
//   - 投递：response 原样作为手机端报文经 session.Respond 送达成员会话
//     （permission 类 {optionId}/{cancel:true}、input 类 {action,content}，
//     respondWire 校验；§5.4「答案原样作为适配器工具结果返回，完整会话仍
//     保留在成员原始 session 中」）。过期（ErrRequestExpired 哨兵 + 文案
//     contains 兜底）→ 撤回该问题（withdrawn，reason=stale）+ 409；报文
//     无效（选项无效/格式不正确——不消费请求、卡仍可答）→ 400 且问题保持
//     open；其余错误原样上抛（问题保持 open，可重试）；
//   - 成功：单批 commitRun 落 question_resolved（幂等键=decision 键）+ 快照
//     Status=answered——答案只允许消费一次（§5.4），二次回答 409。
//
// 并发形态（forward.go 的锁形）：Resolve/Respond 是锁外慢 I/O；记账段短暂
// 持有 state.mu 并在锁内复查新鲜快照（Cancel/终局可能在投递期间撤回问题）。
func (s *Service) AnswerQuestion(runID, questionID string, version uint64, decisionID string, response *jsoncompat.Object) (*PendingQuestion, bool, error) {
	if _, err := uuid.Parse(decisionID); err != nil {
		return nil, false, errors.New("决策标识无效：必须是 UUID")
	}
	if response == nil {
		return nil, false, errors.New("回答内容不能为空")
	}
	run := s.store.Run(runID)
	if run == nil {
		return nil, false, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	decisionKey := DecisionIdempotencyKey(run.TeamID, run.ID, decisionID)
	// ① 幂等快查（journal 本地磁盘读，锁外）：同 decisionID 重放零副作用。
	if recorded := s.decisionRecorded(run.ID, decisionKey); recorded != nil {
		if payloadString(recorded.Payload, "questionId") != questionID {
			return nil, false, errors.New("同一决策标识不能用于不同内容")
		}
		if fresh := s.store.Run(runID); fresh != nil {
			if question := findQuestionIn(fresh, questionID); question != nil {
				return question.Clone(), true, nil
			}
		}
		return nil, true, nil // 问题条目已被清理的极端窗口：仍按 duplicate 报告。
	}
	// ② 版本与状态初查（锁外；锁内 ④ 复查为准）。
	question := findQuestionIn(run, questionID)
	if question == nil {
		return nil, false, fmt.Errorf("%w: 问题=%s", ErrNotFound, questionID)
	}
	if err := questionAnswerable(question, version); err != nil {
		return nil, false, err
	}
	member := s.questionMember(run, question)
	if member == nil {
		return nil, false, fmt.Errorf("%w: 问题来源任务或成员不存在：%s", ErrNotFound, question.TaskID)
	}
	// ③ 投递（锁外慢 I/O：resolver.Resolve 冷探测可达 15s）。
	_, session, resolveErr := s.resolver.Resolve(member.SessionID)
	if session == nil {
		return nil, false, fmt.Errorf("成员会话不可达，回答未送达（问题保持待回答）: %w", resolveErr)
	}
	if err := s.respondAgent(session, RequestIDOf(questionID), response); err != nil {
		expired, invalid := classifyRespondError(err)
		switch {
		case expired:
			// 卡已消失（在别处被回答/回合结束清理/会话回收）：登记随之撤回。
			s.withdrawQuestionStale(runID, questionID)
			return nil, false, ErrQuestionExpired
		case invalid:
			return nil, false, fmt.Errorf("%w: %v", ErrQuestionInvalidResponse, err)
		}
		return nil, false, fmt.Errorf("回答投递失败（问题保持待回答）: %w", err)
	}
	// ④ 记账（state.mu；入锁后复查新鲜快照）。
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	resolved := (*PendingQuestion)(nil)
	checkErr := error(nil)
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		resolved = nil
		checkErr = nil
		if terminalRun(fresh.Status) {
			checkErr = fmt.Errorf("%w: 运行已结束，问题已撤回", ErrQuestionNotOpen)
			return nil, false
		}
		current := findQuestionIn(fresh, questionID)
		if current == nil {
			checkErr = fmt.Errorf("%w: 问题=%s", ErrNotFound, questionID)
			return nil, false
		}
		if err := questionAnswerable(current, version); err != nil {
			checkErr = err
			return nil, false
		}
		current.Status = QuestionStatusAnswered
		event := newEvent(fresh.ID, EventQuestionResolved, "user", map[string]any{
			"questionId": questionID,
			"taskId":     current.TaskID,
			"answerHead": truncateRunes(string(jsoncompat.EncodeCompact(response)), answerHeadRunes),
		})
		event.IdempotencyKey = decisionKey
		resolved = current.Clone()
		return []*Event{event}, true
	})
	if err != nil {
		return nil, false, err
	}
	if resolved == nil {
		if checkErr == nil {
			checkErr = ErrQuestionNotOpen // 防御：闭包未落账也未置错。
		}
		return nil, false, checkErr
	}
	return resolved, false, nil
}

// decisionRecorded 返回 journal 里该决策键的问题回答记账事件（无则 nil；
// forwardRecorded 的同款读法）。决策是低频用户操作，全量重放扫描可接受。
func (s *Service) decisionRecorded(runID, idempotencyKey string) *Event {
	events, err := s.journal.Replay()
	if err != nil {
		return nil
	}
	for _, event := range events {
		if event == nil || event.RunID != runID || event.Type != EventQuestionResolved {
			continue
		}
		if event.IdempotencyKey == idempotencyKey {
			return event
		}
	}
	return nil
}

// classifyRespondError 把 session.Respond 的错误分成三类：expired——问题卡已
// 消失（ErrRequestExpired 哨兵 errors.Is + 文案 contains 兜底，防 Process.
// Respond 下层错误串形态变化）；invalid——报文未过会话校验（「请选择有效的
// 选项」/「请求回应格式不正确」：请求未被消费、卡仍可答）；其余（会话关闭、
// 进程退出等瞬态）两者皆非，问题保持 open 交上层按可重试处理。
func classifyRespondError(err error) (expired, invalid bool) {
	if err == nil {
		return false, false
	}
	if errors.Is(err, acpruntime.ErrRequestExpired) || errors.Is(err, ErrQuestionExpired) {
		return true, false
	}
	switch message := err.Error(); {
	case strings.Contains(message, "请求已过期"):
		return true, false
	case strings.Contains(message, "请选择有效的选项"), strings.Contains(message, "请求回应格式不正确"):
		return false, true
	}
	return false, false
}
