package team

// dispatcher.go 是派发与取消的 I/O 编排（§3.1 原则 4、§6.1/6.2、§10.2）：
// SubmitTask 先 journal 记账（task_dispatching + submissionID + inputHash）再
// 入队（mode=queue），派发时机完全交给既有 3s worker 与 settle 钩子；绝不
// 调用 dispatchSubmission/queueSendNow、绝不绕过队列。CancelTask 先把任务标
// cancelled（记账），再撤账本条目或取消会话 turn（副作用）。纯决策在
// team-logic（planner/scheduler/reducer/recovery），本文件只做顺序与 I/O。

import (
	"fmt"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// firstAttempt 是阶段一的固定派发尝试号：任务文本不可变、不自动重试，
// attempt 恒为 1（§6.2）。
const firstAttempt = 1

// TaskIdempotencyKey 生成外部发送的稳定幂等键（§8.2）：
// team:{teamID}:run:{runID}:task:{taskID}:attempt:{attempt}。
func TaskIdempotencyKey(teamID, runID, taskID string, attempt int) string {
	return fmt.Sprintf("team:%s:run:%s:task:%s:attempt:%d", teamID, runID, taskID, attempt)
}

// TaskSubmissionID 从幂等键确定性派生 submissionID（§6.2：teamID+runID+taskID
// → submissionID；仿 internal/httpd/sessions.go:362 migrate 前例对
// uuid.NameSpaceURL 做 SHA1）。同一任务槽重算恒等，崩溃后对账与重复入队
// 天然幂等。
func TaskSubmissionID(teamID, runID, taskID string, attempt int) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte("mobile-bridge:"+TaskIdempotencyKey(teamID, runID, taskID, attempt))).String()
}

// taskDispatch 是一次成功派发的产物（watcher 的输入）。
type taskDispatch struct {
	taskID       string
	sessionID    string
	submissionID string
	sentText     string
}

// cancelTarget 是取消/超时副作用的目标描述。
type cancelTarget struct {
	sessionID    string
	submissionID string
	sentText     string
}

// submitTaskLocked 执行 §3.1 原则 4 的先记账后发送（调用方持有 state.mu）：
//
//  1. context builder 合成完整文本（§7），按账本 100000 字节硬上限预检；
//  2. 用真实注入上下文重算 inputHash 并确定性派生 submissionID；
//  3. journal 追加 task_dispatching（含幂等键）并把任务落 dispatching——
//     先记账后发送，崩溃后可按 submissionID 对账；
//  4. QueueAdapter.Enqueue（mode=queue）：同 id 同内容 duplicate=true 无副作用。
//
// 返回 (nil, nil) 表示本轮暂缓（写锁被占等，下轮重试）；(nil, err) 是确定性
// 失败（内容超限、账本拒绝），调用方应把任务落 failed。绝不在这里直接调
// dispatchSubmission——派发时机完全属于既有队列。
func (s *Service) submitTaskLocked(state *runState, team *Team, run *Run, taskID string) (*taskDispatch, error) {
	task := findTaskIn(run, taskID)
	if task == nil {
		return nil, fmt.Errorf("任务不存在：%s", taskID)
	}
	if task.Status != TaskPlanned && task.Status != TaskReady {
		return nil, nil // 已被并发处理，下一轮重新决策
	}
	member := memberOf(team, task.MemberID)
	if member == nil {
		return nil, fmt.Errorf("任务 %s 引用的成员不存在：%s", taskID, task.MemberID)
	}

	// ① 上下文合成 + 账本硬上限预检（planner 按 rune 预留余量，多字节内容
	//    仍可能超过账本的 100000 字节校验——此处兜底，任务确定性失败）。
	context := BuildTaskContext(run, task, team.Policy)
	sentText := ComposeTaskMessage(context.Text, task.Prompt)
	if len(sentText) > promptHardLimit {
		return nil, fmt.Errorf("任务 %s 内容过长：合成消息 %d 字节，超过账本上限 %d 字节",
			taskID, len(sentText), promptHardLimit)
	}

	// ② 幂等键与输入哈希（§6.2/§8.2：含真实上下文的哈希在此落 journal）。
	submissionID := TaskSubmissionID(run.TeamID, run.ID, task.ID, firstAttempt)
	inputHash := InputHashFor(task, PolicyDigest(team.Policy), context.Text)

	// 写锁：exclusive 任务先取 cwd 级写锁（§6.4 规则 1/2）；取不到暂缓。
	cwd := ""
	if task.WriteLock != TaskWriteLockNone {
		cwd = s.resolver.Cwd(member.SessionID)
		if !s.locks.Acquire(cwd, task.ID) {
			return nil, nil
		}
	}

	// ③ 先记账：journal 事件（含幂等键）+ 任务落 dispatching。
	//    崩溃窗口（journal 已落、快照未存）由 recovery 的 hasDispatch 分支对账。
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		current := findTaskIn(fresh, taskID)
		if current == nil || (current.Status != TaskPlanned && current.Status != TaskReady) {
			return nil, false
		}
		current.Status = TaskDispatching
		current.SubmissionID = submissionID
		current.InputHash = inputHash
		current.Attempt = firstAttempt
		event := newEvent(fresh.ID, EventTaskDispatching, "runner", map[string]any{
			"taskId":       taskID,
			"sessionId":    member.SessionID,
			"submissionId": submissionID,
			"inputHash":    inputHash,
			"attempt":      jsoncompat.Num(firstAttempt),
			"contextChars": jsoncompat.Num(context.InjectedChars),
		})
		event.IdempotencyKey = TaskIdempotencyKey(fresh.TeamID, fresh.ID, taskID, firstAttempt)
		return []*Event{event}, true
	})
	if err != nil {
		s.locks.Release(cwd, task.ID)
		return nil, fmt.Errorf("任务 %s 派发记账失败: %w", taskID, err)
	}
	state.taskCwds[taskID] = cwd

	// ④ 入队（外部副作用）：mode=queue，绝不立即派发。duplicate=true 是既有
	//    账本的幂等重入（崩溃恢复重放同 id 同内容），无二次副作用。
	status, _, err := s.queue.Enqueue(member.SessionID, submissionID, sentText)
	if err != nil {
		// 账本确定性拒绝（内容超限/同 id 不同内容）。阶段一任务文本不可变，
		// 正常路径不会走到这里；出现即为对账冲突，任务确定性失败、绝不重发。
		return nil, fmt.Errorf("任务 %s 入队失败（账本状态 %s）: %w", taskID, status, err)
	}
	return &taskDispatch{
		taskID:       taskID,
		sessionID:    member.SessionID,
		submissionID: submissionID,
		sentText:     sentText,
	}, nil
}

// cancelSideEffects 执行取消/超时的事后副作用（不持 state.mu；记账已先行，
// watcher 对已终态任务 no-op，与用户在原会话手动停止不存在竞争）：
// 账本仍 queued → 撤条目（queue.cancel）；已 accepted/unknown → 仅当本任务
// 自己的 turn 仍在进行时取消会话 turn——绝不误杀用户在原会话手动发起的回合。
func (s *Service) cancelSideEffects(target cancelTarget) {
	if target.submissionID == "" {
		return
	}
	if sub, ok := s.queue.Submission(target.sessionID, target.submissionID); ok {
		switch sub.Status {
		case SubStatusQueued:
			// 已离开队列（并发派发）视为良性：条目不再可撤。
			_ = s.queue.CancelSubmission(target.sessionID, target.submissionID)
			return
		case SubStatusAccepted, SubStatusUnknown:
		default:
			return // failed/cancelled/ignored：无需副作用。
		}
	}
	if _, session, _ := s.resolver.Resolve(target.sessionID); session != nil {
		s.cancelAgentTurn(session, target.sentText)
	}
}

// cancelTurnIfOurs 是 cancelAgentTurn 的默认实现：仅当快照里本任务的 turn
// （按 sentText 全文匹配）仍在进行时调用 session.Cancel（manager.go:1062-1083；
// 与用户手动停止同形态，view 落 interrupted）。返回是否真的发起了取消。
func (s *Service) cancelTurnIfOurs(session *acpruntime.Session, sentText string) bool {
	if session == nil {
		return false
	}
	turn := matchTurn(session.Snapshot(), sentText)
	if turn == nil {
		return false
	}
	if status, _ := turn.MustGet("status").(string); status != "inProgress" {
		return false
	}
	return session.Cancel() == nil
}

// matchTurn 在会话快照中按 sentText 全文匹配本任务的 turn（acpview
// BeginTurn 把完整 prompt 文本存为 turn 的 userMessage，view.go:150-160）。
// 同一文本出现多个 turn 时取最新一个：成员单活动任务语义下，后派发的任务
// 才会出现重复文本，取最新避免错认成历史回合。
func matchTurn(snapshot *jsoncompat.Object, sentText string) *jsoncompat.Object {
	if snapshot == nil {
		return nil
	}
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return nil
	}
	var matched *jsoncompat.Object
	for _, item := range turns.Items {
		turn, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
		if messages == nil {
			continue
		}
		for _, entry := range messages.Items {
			message, ok := entry.(*jsoncompat.Object)
			if !ok {
				continue
			}
			if role, _ := message.MustGet("role").(string); role != "user" {
				continue
			}
			if text, _ := message.MustGet("text").(string); text == sentText {
				matched = turn
				break
			}
		}
	}
	return matched
}

// turnStatus 读取 turn.status（inProgress/completed/failed/interrupted，
// view.go:47-57/169-199）。
func turnStatus(turn *jsoncompat.Object) string {
	status, _ := turn.MustGet("status").(string)
	return status
}

// turnErrorText 提取失败 turn 的错误文本：turn 顶层 error 字段恒为 nil
// （view.go:54），失败文本在 messages 里 kind=="error" 条目的 text
// （view.go:188-194）——覆盖 PromptWithBlocks 吞掉 Process.Submit 写错的坑
// （manager.go:776-784：账本 accepted 但 turn 已失败）。
func turnErrorText(turn *jsoncompat.Object) string {
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages != nil {
		for _, entry := range messages.Items {
			message, ok := entry.(*jsoncompat.Object)
			if !ok {
				continue
			}
			if kind, _ := message.MustGet("kind").(string); kind == "error" {
				if text, _ := message.MustGet("text").(string); strings.TrimSpace(text) != "" {
					return text
				}
			}
		}
	}
	if stopReason, _ := turn.MustGet("stopReason").(string); stopReason != "" {
		return "Agent 回合失败：" + stopReason
	}
	return "Agent 回合失败"
}

// cancelledTurn 报告 turn 是否以 cancelled 收场（session.Cancel 与用户手动
// 停止同形态：status=interrupted 或 stopReason=cancelled，view.go:172-173）。
func cancelledTurn(turn *jsoncompat.Object) bool {
	if status, _ := turn.MustGet("status").(string); status == "interrupted" {
		return true
	}
	stopReason, _ := turn.MustGet("stopReason").(string)
	return stopReason == "cancelled"
}

// serviceProbe 把 QueueAdapter + SessionResolver 适配成 recovery 的 Probe
// （§8.3 对账的只读视图）。
type serviceProbe struct{ service *Service }

// LedgerStatus 返回 submissions 账本状态；ok=false 只表示账本确无该条目
// （QueueAdapter 的契约），不会把查询失败伪装成无条目。
func (p *serviceProbe) LedgerStatus(threadID, submissionID string) (string, string, bool) {
	sub, ok := p.service.queue.Submission(threadID, submissionID)
	if !ok {
		return "", "", false
	}
	return sub.Status, sub.Error, true
}

// SessionLive 报告会话当前是否可解析（决定 accepted 状态能否重建 watcher）。
func (p *serviceProbe) SessionLive(threadID string) bool {
	_, session, _ := p.service.resolver.Resolve(threadID)
	return session != nil
}

// memberOf 返回 team 中指定 id 的成员（nil 表示不存在）。
func memberOf(team *Team, memberID string) *TeamMember {
	if team == nil {
		return nil
	}
	for i := range team.Members {
		if team.Members[i].ID == memberID {
			return &team.Members[i]
		}
	}
	return nil
}
