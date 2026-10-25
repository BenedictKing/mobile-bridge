package team

// dispatcher.go 是派发与取消的 I/O 编排（§3.1 原则 4、§6.1/6.2、§10.2）：
// SubmitTask 先 journal 记账（task_dispatching + submissionID + inputHash）再
// 入队（mode=queue），派发时机完全交给既有 3s worker 与 settle 钩子；绝不
// 调用 dispatchSubmission/queueSendNow、绝不绕过队列。CancelTask 先把任务标
// cancelled（记账），再撤账本条目或取消会话 turn（副作用）。纯决策在
// team-logic（planner/scheduler/reducer/recovery），本文件只做顺序与 I/O。

import (
	"fmt"
	"path/filepath"
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

// taskDispatch 是一次成功派发的产物（watcher 的输入）。bucketKey 是该次
// 派发归属的 AIMD 桶（executeDispatch 在锁外解析会话后传入；收养路径可能
// 为空——空键的 watcher 不做桶的成败/在途记账，§6.3）。
type taskDispatch struct {
	taskID       string
	sessionID    string
	submissionID string
	sentText     string
	bucketKey    string
}

// cancelTarget 是取消/超时副作用的目标描述。
type cancelTarget struct {
	sessionID    string
	submissionID string
	sentText     string
}

// workspaceBinding 是 isolated 任务派发前解析出的工作区绑定（§6.4 规则 4）：
// path 用作写锁键（与共享 cwd 同一 LockRegistry）与事件载荷；baseRevision
// 是首派记录的基线提交（任务已有基线时沿用既有值，不因重派漂移）。
type workspaceBinding struct {
	workspaceID  string
	path         string
	baseRevision string
}

// resolveWorkspaceBinding 校验 isolated 任务的工作区引用并解析首派基线
// （锁外 I/O：注册表读取 + git rev-parse；调用方绝不持有 state.mu）：
//
//  1. 注册表解析失败（端口未装配/未注册）→ 确定性失败；
//  2. 成员会话 cwd 与 worktree 路径经 filepath.Clean 后全等——会话开在别的
//     目录时空 cwd/不相等路径同样失败，绝不 spawn 新会话（§6.4：worktree
//     任务要求成员会话已开在 worktree 中）；
//  3. 首派（Task.BaseRevision 为空）读取当前 HEAD 作为基线，读取失败同样
//     确定性失败；重派（重试/resume 重臂）沿用既有基线。
func (s *Service) resolveWorkspaceBinding(task *Task, member *TeamMember) (*workspaceBinding, error) {
	if s.Workspaces == nil {
		return nil, ErrWorkspacesUnavailable
	}
	path, err := s.Workspaces.Resolve(task.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("工作区不存在：%s（%v）", task.WorkspaceID, err)
	}
	if cwd := s.resolver.Cwd(member.SessionID); filepath.Clean(cwd) != filepath.Clean(path) {
		return nil, fmt.Errorf("成员会话工作区与任务工作区不一致，请把会话开在 worktree 中（%s）", path)
	}
	base := task.BaseRevision
	if base == "" {
		if base, err = s.Workspaces.HeadRevision(path); err != nil {
			return nil, fmt.Errorf("读取工作区基线失败: %w", err)
		}
	}
	return &workspaceBinding{workspaceID: task.WorkspaceID, path: path, baseRevision: base}, nil
}

// DispatchAdmission 是派发准入缝（§6.3 AIMD 的注入点，team-aimd 实现）：
// 在派发主体（executeDispatch）锁外、记账前调用，携带任务、成员与已解析
// 的会话。返回 false 表示本轮暂缓——任务保持 planned，下一轮重新决策
// （AIMD 冷却窗口语义）。实现不得长时间阻塞（在派发池工作体内执行）。
type DispatchAdmission interface {
	Admit(task *Task, member *TeamMember, session *acpruntime.Session) bool
}

// submitTaskLocked 是同步派发的兼容入口（旧单测直驱；生产路径经团队派发池
// 的 executeDispatch 异步执行：cwd 锁外解析 → 记账 → 入队）。调用方持有
// state.mu。返回 (nil, nil)=本轮暂缓；(nil, err)=确定性失败。
func (s *Service) submitTaskLocked(state *runState, team *Team, run *Run, taskID string) (*taskDispatch, error) {
	task := findTaskIn(run, taskID)
	if task == nil {
		return nil, fmt.Errorf("任务不存在：%s", taskID)
	}
	member := memberOf(team, task.MemberID)
	if member == nil {
		return nil, fmt.Errorf("任务 %s 引用的成员不存在：%s", taskID, task.MemberID)
	}
	cwd := ""
	var binding *workspaceBinding
	if task.WorkspaceMode == WorkspaceIsolatedWorktree {
		// isolated：注册表/基线校验 + 写锁键改用 worktree 路径（§6.4）。
		// 此兼容入口在锁内做 I/O 仅供旧单测直驱；生产路径经 executeDispatch
		// 在锁外完成同一校验。
		var bindErr error
		binding, bindErr = s.resolveWorkspaceBinding(task, member)
		if bindErr != nil || binding == nil {
			return nil, bindErr
		}
		cwd = binding.path
	} else if task.WriteLock != TaskWriteLockNone {
		cwd = s.resolver.Cwd(member.SessionID)
	}
	dispatch, err := s.accountDispatchLocked(state, team, run, taskID, cwd, binding)
	if err != nil || dispatch == nil {
		return nil, err
	}
	// 入队（外部副作用）：mode=queue，绝不立即派发。duplicate=true 是既有
	// 账本的幂等重入（崩溃恢复重放同 id 同内容），无二次副作用。
	status, _, err := s.queue.Enqueue(dispatch.sessionID, dispatch.submissionID, dispatch.sentText)
	if err != nil {
		// 账本确定性拒绝（内容超限/同 id 不同内容）。任务文本不可变，正常
		// 路径不会走到这里；出现即为对账冲突，任务确定性失败、绝不重发。
		return nil, fmt.Errorf("任务 %s 入队失败（账本状态 %s）: %w", taskID, status, err)
	}
	return dispatch, nil
}

// accountDispatchLocked 是派发的记账体（§3.1 原则 4 的先记账后发送，调用方
// 持有 state.mu；cwd 与 isolated 绑定由派发池在锁外解析传入）：
//
//  1. context builder 合成完整文本（§7），按账本 100000 字节硬上限预检；
//  2. 用真实注入上下文重算 inputHash 并确定性派生 submissionID；
//  3. exclusive 任务取 cwd 级写锁（§6.4 规则 1/2；isolated 任务的 cwd 即
//     worktree 路径——同 worktree 并行写与共享 cwd 锁天然互斥）；取不到暂缓；
//  4. journal 追加 task_dispatching（含幂等键）并把任务落 dispatching；
//     isolated 首派在同一 commitRun 写入 Task.BaseRevision 并追加
//     workspace_bound 事件（§6.4 规则 4：基线与派发事实原子落账）。
//
// 返回 (nil, nil) 表示本轮暂缓（写锁被占等，下轮重试）或「记账体复查发现
// 任务已被并发置终态」（Cancel/级联终止/收集策略可以在调用方的锁外慢 I/O
// ——Resolve 冷探测可达 15s、git rev-parse——期间把任务落终态；此时代价的
// 写锁就地释放、不入账 taskCwds、不入队，调用方按本轮放弃处理），(nil, err)
// 是确定性失败（内容超限等），调用方应把任务落 failed。入队（queue.Enqueue）
// 由调用方在锁外执行——绝不在这里直接调 dispatchSubmission，派发时机完全
// 属于既有队列。
func (s *Service) accountDispatchLocked(state *runState, team *Team, run *Run, taskID, cwd string, binding *workspaceBinding) (*taskDispatch, error) {
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
	//    attempt 语义（§6.3 阶段二）：快照 Attempt 即本次派发的尝试号——
	//    rate_limited/transient 的受限重试在结算时已递增（task_failed 事实
	//    + attempt+1 回 ready）；quota/auth/not_configured 停靠任务经 resume
	//    重臂为 ready 时 Outcome 仍留在快照，此处按 task.Attempt+1 派全新
	//    submissionID（旧 attempt 的账本条目已存在，复用同键会被幂等去重
	//    拒绝或误对账），记账时原子清除 Outcome。
	attempt := task.Attempt
	if attempt < firstAttempt {
		attempt = firstAttempt
	}
	if stopOutcomeFreshSubmission(task.Outcome) {
		attempt++
	}
	submissionID := TaskSubmissionID(run.TeamID, run.ID, task.ID, attempt)
	inputHash := InputHashFor(task, PolicyDigest(team.Policy), context.Text)

	// ③ 写锁：exclusive 任务先取 cwd 级写锁（§6.4 规则 1/2）；取不到暂缓。
	if task.WriteLock != TaskWriteLockNone {
		if !s.locks.Acquire(cwd, task.ID) {
			return nil, nil
		}
	}

	// ④ 先记账：journal 事件（含幂等键）+ 任务落 dispatching。
	//    崩溃窗口（journal 已落、快照未存）由 recovery 的 hasDispatch 分支对账。
	//    isolated 首派在同一事务写入 BaseRevision + workspace_bound 事件：
	//    基线缺失会让重启对账与 diff 数据面静默失效，绝不与派发事实分离落账。
	//    applied 标记「变更确已应用」：调用方锁外慢 I/O（Resolve/rev-parse）期间
	//    任务可能已被 Cancel/级联终止/收集策略置终态，closure 复查按 (nil,false)
	//    放弃且 commitRun 返回 nil 错误——此时代价是刚获取的写锁必须就地释放，
	//    否则 LockRegistry 里的登记无人再释放（终态任务不经过任何 release 路径，
	//    run 终态后调度循环也不再运行），同 cwd 的 exclusive 任务从此永远无法
	//    派发（Cancel 的注释警告过的残留场景，本路径曾是漏网实现）。
	applied := false
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		applied = false // 版本冲突重试会在新副本上重跑：以最后一次复查为准
		current := findTaskIn(fresh, taskID)
		if current == nil || (current.Status != TaskPlanned && current.Status != TaskReady) {
			return nil, false
		}
		applied = true
		current.Status = TaskDispatching
		current.SubmissionID = submissionID
		current.InputHash = inputHash
		current.Attempt = attempt
		current.Outcome = "" // 新 attempt 无失败分类（停靠重飞在此清 Outcome）
		// 派发全文随快照落盘（遗留④）：收养/超时/取消路径复用存量文本，
		// 策略或上下文合成的后续变更不再让在途任务的 matchTurn 漂移。
		current.SentText = sentText
		event := newEvent(fresh.ID, EventTaskDispatching, "runner", map[string]any{
			"taskId":       taskID,
			"sessionId":    member.SessionID,
			"submissionId": submissionID,
			"inputHash":    inputHash,
			"attempt":      jsoncompat.Num(attempt),
			"contextChars": jsoncompat.Num(context.InjectedChars),
		})
		event.IdempotencyKey = TaskIdempotencyKey(fresh.TeamID, fresh.ID, taskID, attempt)
		events := []*Event{event}
		if binding != nil && current.BaseRevision == "" {
			current.BaseRevision = binding.baseRevision
			events = append(events, newEvent(fresh.ID, EventWorkspaceBound, "runner", map[string]any{
				"taskId":       taskID,
				"workspaceId":  binding.workspaceID,
				"path":         binding.path,
				"baseRevision": binding.baseRevision,
			}))
		}
		return events, true
	})
	if err != nil {
		s.locks.Release(cwd, task.ID)
		return nil, fmt.Errorf("任务 %s 派发记账失败: %w", taskID, err)
	}
	if !applied {
		// 任务已被并发置终态：释放刚获取的写锁、不登记 taskCwds、不入队
		//（终态任务派发是零价值副作用——账本条目会成孤儿，3s worker 会把
		// prompt 真正发给 Agent）。调用方按本轮放弃处理。
		s.locks.Release(cwd, task.ID)
		return nil, nil
	}
	state.taskCwds[taskID] = cwd

	return &taskDispatch{
		taskID:       taskID,
		sessionID:    member.SessionID,
		submissionID: submissionID,
		sentText:     sentText,
	}, nil
}

// sentTextFor 返回任务当前派发全文（遗留④）：Task.SentText 非空直接用——
// 派发时随 task_dispatching 原子落快照，收养 watcher、超时与取消的副作用
// （matchTurn 全文匹配、撤账本条目）都按存量文本执行，策略或上下文合成的
// 任何后续变更（policy 改动、成员转发、轮次摘要）都不再让在途任务错认
// turn；空串是旧账本的迁移窗口（该版本前派发、尚未终态的任务），退回
// 「不可变 prompt + 依赖结果」的确定性重算（阶段一语义）。
func (s *Service) sentTextFor(run *Run, task *Task, policy TeamPolicy) string {
	if task.SentText != "" {
		return task.SentText
	}
	context := BuildTaskContext(run, task, policy)
	return ComposeTaskMessage(context.Text, task.Prompt)
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
