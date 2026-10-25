package team

// forward.go 是成员间显式转发的 I/O 编排（§7 阶段二工作项 4）：把一个成员
// 的有界结果——Run 内 completed 任务的 TaskResult（含 §6.5 两份摘要与截断
// 标记），或按 scope 裁剪的会话转录——经 §7.1 信封注入另一成员的会话队列。
// 转发是会话级旁路消息：不创建 Task、不进调度依赖图、不占并发额度；目标
// 成员会话原 timeline 可见全文。顺序硬性规定（§3.1 原则 4）：先 journal
// 记账（task_forwarded 事件带幂等键）再 Enqueue（mode=queue，同 id 同内容
// 幂等）；同 requestID 重放零副作用。

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// EventTaskForwarded 是成员间转发的 journal 事件类型（§10.3 事件表的阶段二
// 增量；无 taskId——转发不是任务）。
const EventTaskForwarded = "task_forwarded"

// forwardNoteDefault 是转发请求未携带 Note 时的缺省指派语。
const forwardNoteDefault = "请参考上述内容继续处理"

// forwardTimeLayout 是信封时间行的格式（RFC3339，§7.1「必须标明来源、时间
// 和截断信息」）。
const forwardTimeLayout = time.RFC3339

// ErrForwardRunFinished 表示目标 Run 已终态，拒绝转发（§12 阶段二验收：
// Run 结束后不再产生新的成员消息）。HTTP 侧映射 409。
var ErrForwardRunFinished = errors.New("运行已结束，不能转发")

// ForwardRequest 描述一次成员间显式转发。来源二选一：SourceTaskID（该 Run
// 内 completed 任务的结果）或 SourceSessionID+Scope（按 scope 裁剪的会话
// 转录）；RequestID 是幂等键的组成部分（必填 UUID）。ActionKey 非空表示
// 本转发是 request_context 动作的执行（阶段三 actions-exec）：task_forwarded
// 事件 payload 增载该键——journal 重建据此把对应动作置 executed
// （markRebuildActionExecuted）；它不参与幂等身份比对（身份仍是 RequestID）。
type ForwardRequest struct {
	RequestID       string
	SourceTaskID    string
	SourceSessionID string
	Scope           string // 会话转录裁剪范围（仅会话源有意义）
	TargetMemberID  string
	Note            string // 指派语；缺省「请参考上述内容继续处理」
	ActionKey       string // 动作执行凭据（仅 request_context 动作携带）
}

// ForwardIdempotencyKey 生成转发的稳定幂等键（§8.2 变体）：
// team:{teamID}:run:{runID}:forward:{requestID}。
func ForwardIdempotencyKey(teamID, runID, requestID string) string {
	return fmt.Sprintf("team:%s:run:%s:forward:%s", teamID, runID, requestID)
}

// ForwardSubmissionID 从幂等键确定性派生 submissionID（TaskSubmissionID 的
// 同款 uuid5 模式）：同一转发请求重算恒等，网络重试不会重复注入。
func ForwardSubmissionID(teamID, runID, requestID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte("mobile-bridge:"+ForwardIdempotencyKey(teamID, runID, requestID))).String()
}

// forwardTranscript 是锁外预取的会话转录（composeForward 的会话源正文）。
type forwardTranscript struct {
	text      string
	truncated bool
}

// ForwardToMember 把一个成员的有界结果注入另一成员的会话队列（§7）。
//
//   - 来源与上限：SourceTaskID 取该 Run 内 completed 任务的 TaskResult
//     （Result + 两份摘要 + 截断标记）；SourceSessionID 经 TranscriptSource
//     按 scope 裁剪（未装配时报 ErrTranscriptUnavailable）。两类源正文都先
//     剥 <team-actions> 动作块（BuildTaskContext 的同族义务：Lead 结果里的
//     动作信封绝不由转发走私进下游——下游可执行的动作只来自编排器的正式
//     派发）。正文按 policy.MaxContextChars（rune）截断，合成全文再按账本
//     100000 字节硬上限兜底收缩（正文减半，与 BuildTaskContext 的
//     shrinkOldest 同思路）；
//   - 信封（§7.1 版式）：[Team context] 头 + 来源成员/来源任务（或来源会话）
//   - 时间行（RFC3339）+ 截断提示 + 正文 + [Assigned task] 头 + Note；
//   - 幂等：submissionID 由幂等键确定性派生；同 requestID 重放先查 journal
//     与账本，已转发过则零副作用返回 duplicate=true。崩溃窗口（journal 已落、
//     入队未达）只补入队、不重复记账；时间串固化于 payload（recordedAt），
//     补投/并发抢先记账后的重组与首次入队字节全等（时间行参与内容身份，
//     现取时间会被账本按「同一消息标识不能用于不同内容」拒绝，评审遗留③）；
//   - 记账：先 journal（task_forwarded，IdempotencyKey=幂等键，payload 带固化的
//     recordedAt）后 Enqueue（mode=queue，派发时机交给既有 3s worker 与
//     settle 钩子）。
//
// 并发形态：转录提取（resolveACP 冷探测可达 15s）与幂等快查都在 run 锁外
// 执行；记账+入队段短暂持有 state.mu（与 Cancel/Resume 同惯例，本地磁盘
// I/O）。run 已终态时拒绝（ErrForwardRunFinished）。
func (s *Service) ForwardToMember(team *Team, run *Run, req ForwardRequest) (string, bool, error) {
	// ① 请求校验（无锁）。
	if team == nil || run == nil {
		return "", false, errors.New("team 或 run 不能为 nil")
	}
	if _, err := uuid.Parse(req.RequestID); err != nil {
		return "", false, errors.New("转发请求标识无效")
	}
	if (req.SourceTaskID == "") == (req.SourceSessionID == "") {
		return "", false, errors.New("转发来源必须且只能二选一：来源任务或来源会话")
	}
	if req.SourceTaskID != "" && req.Scope != "" {
		return "", false, errors.New("任务来源不支持 scope")
	}
	member := memberOf(team, req.TargetMemberID)
	if member == nil {
		return "", false, fmt.Errorf("目标成员不存在：%s", req.TargetMemberID)
	}
	submissionID := ForwardSubmissionID(team.ID, run.ID, req.RequestID)
	idempotencyKey := ForwardIdempotencyKey(team.ID, run.ID, req.RequestID)

	// ② 幂等快查（无锁；journal 是本地磁盘读）：同 id 重放在记账与入队都
	// 已完成时零副作用返回——不做转录提取、不碰 run 锁。来源与目标必须与
	// 已记账事实一致（creations 账本「同一创建请求不能用于不同内容」的同款
	// 语义；Note 是指派语，不参与身份比对）。
	// recordedAt 非空表示 journal 已有记账事实（崩溃窗口/重放）：补投的内容
	// 必须复用 payload 固化的时间串——时间行参与内容身份，现取时间会重组出
	// 与首次不同的字节，同 submissionID 入队被账本按「同一消息标识不能用于
	// 不同内容」拒绝（评审遗留③）。
	recordedAt := ""
	if recorded := s.forwardRecorded(run.ID, idempotencyKey); recorded != nil {
		if payloadString(recorded.Payload, "sourceTaskId") != req.SourceTaskID ||
			payloadString(recorded.Payload, "sourceSessionId") != req.SourceSessionID ||
			payloadString(recorded.Payload, "targetMemberId") != req.TargetMemberID {
			return "", false, errors.New("同一转发请求不能用于不同内容")
		}
		if _, ok := s.queue.Submission(member.SessionID, submissionID); ok {
			return submissionID, true, nil
		}
		// 崩溃窗口（journal 已落、入队未达）：落入 ③④ 补投（duplicate=true）。
		recordedAt = payloadString(recorded.Payload, "recordedAt")
	}

	// ③ 会话源转录提取（无锁——绝不占 run 锁）。任务源正文在 ④ 里从 run
	// 快照读取（纯内存）。
	var transcript *forwardTranscript
	if req.SourceSessionID != "" {
		if s.Transcripts == nil {
			return "", false, ErrTranscriptUnavailable
		}
		text, truncated, err := s.Transcripts.BoundedTranscript(req.SourceSessionID, req.Scope)
		if err != nil {
			return "", false, fmt.Errorf("会话转录提取失败: %w", err)
		}
		if strings.TrimSpace(text) == "" {
			return "", false, errors.New("来源会话没有可转发的转录内容")
		}
		transcript = &forwardTranscript{text: text, truncated: truncated}
	}

	// ④ 记账与入队（state.mu 串行，与 Cancel/Resume 同惯例）。
	state := s.stateFor(run.ID)
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	if fresh == nil {
		return "", false, fmt.Errorf("%w: run=%s", ErrNotFound, run.ID)
	}
	if terminalRun(fresh.Status) {
		return "", false, fmt.Errorf("%w: 当前状态 %s", ErrForwardRunFinished, fresh.Status)
	}
	// ②③ 间隙被并发同 id 请求抢先记账：不再重复记账，只保证补投。记账唯一
	// 写入路径就是本函数（state.mu 串行），取锁后 journal 里的事实不再变化——
	// 先复取 payload 的 recordedAt 再合成，重组内容与首次入队字节全等。
	alreadyRecorded := recordedAt != ""
	if !alreadyRecorded {
		if recorded := s.forwardRecorded(fresh.ID, idempotencyKey); recorded != nil {
			recordedAt = payloadString(recorded.Payload, "recordedAt")
			alreadyRecorded = true
		}
	}
	contextText, note, truncated, at, err := s.composeForward(team, fresh, req, transcript, recordedAt)
	if err != nil {
		return "", false, err
	}
	if alreadyRecorded {
		// 只补入队（账本幂等，同 id 同内容零副作用；内容与首次字节全等）。
		if _, _, err := s.queue.Enqueue(member.SessionID, submissionID, ComposeTaskMessage(contextText, note)); err != nil {
			return "", false, fmt.Errorf("转发入队失败: %w", err)
		}
		return submissionID, true, nil
	}
	payload := map[string]any{
		"targetMemberId": req.TargetMemberID,
		"sessionId":      member.SessionID,
		"submissionId":   submissionID,
		"recordedAt":     at,
		"contextChars":   jsoncompat.Num(len([]rune(contextText))),
		"truncated":      truncated,
	}
	if req.SourceTaskID != "" {
		payload["sourceTaskId"] = req.SourceTaskID
	} else {
		payload["sourceSessionId"] = req.SourceSessionID
	}
	if req.ActionKey != "" {
		payload["actionKey"] = req.ActionKey
	}
	event := newEvent(fresh.ID, EventTaskForwarded, "user", payload)
	event.IdempotencyKey = idempotencyKey
	if err := s.journal.Append(event); err != nil {
		return "", false, fmt.Errorf("转发记账失败: %w", err)
	}
	s.emit(event)
	// 先记账后入队（§3.1 原则 4）：崩溃在两者之间由上方重放路径补投。
	status, duplicate, err := s.queue.Enqueue(member.SessionID, submissionID, ComposeTaskMessage(contextText, note))
	if err != nil {
		return "", false, fmt.Errorf("转发入队失败（账本状态 %s）: %w", status, err)
	}
	return submissionID, duplicate, nil
}

// composeForward 组装转发信封（调用方持有 state.mu；会话源转录已在锁外
// 预取）。recordedAt 非空表示复用已记账 payload 的时间串（重放/并发补投
// 必须与首次入队字节全等，见 ForwardToMember ④）——时间行参与内容身份，
// 现取现记会破坏「同 id 同内容」的账本幂等；空串则现取当前时间并作为
// recordedAt 返回，供调用方固化进 journal payload。返回 [Team context] 段、
// 指派语、是否发生截断与生效的时间串。
func (s *Service) composeForward(team *Team, run *Run, req ForwardRequest, transcript *forwardTranscript, recordedAt string) (string, string, bool, string, error) {
	var headerLines []string
	var body string
	truncated := false
	if req.SourceTaskID != "" {
		task := findTaskIn(run, req.SourceTaskID)
		if task == nil {
			return "", "", false, "", fmt.Errorf("来源任务不存在：%s", req.SourceTaskID)
		}
		if task.Status != TaskCompleted {
			return "", "", false, "", fmt.Errorf("来源任务尚未完成：%s", req.SourceTaskID)
		}
		detail := task.ResultDetail
		if detail == nil {
			detail = &TaskResult{Result: task.Result}
		}
		// 先剥 <team-actions> 动作块（BuildTaskContext 注入依赖结果前的同族
		// 义务，context.go）：Lead 结果里的动作信封绝不由转发走私进下游——
		// 下游可执行的动作只来自编排器的正式派发。转发目标是 Lead 时回显
		// 信封会以新 sourceTaskId 产生新的合法 ActionKey 并在 autoDispatch
		// 下被执行。只剥成对块；孤立标记是普通文本，无副作用。
		body = stripActionBlocks(detail.Result)
		if detail.FileSummary != "" {
			body += "\n变更文件：" + detail.FileSummary
		}
		if detail.CommandSummary != "" {
			body += "\n执行命令：" + detail.CommandSummary
		}
		if detail.Truncated {
			body += "\n（结果过长，已截断）"
			truncated = true
		}
		if strings.TrimSpace(body) == "" {
			return "", "", false, "", fmt.Errorf("来源任务没有可转发的结果内容：%s", req.SourceTaskID)
		}
		headerLines = []string{"来源成员：" + task.MemberID, "来源任务：" + task.ID}
	} else {
		// 会话源同样剥动作块（Lead 会话转录里的动作信封与任务结果同源，
		// 走私路径与防护同条）。
		body = stripActionBlocks(transcript.text)
		if strings.TrimSpace(body) == "" {
			return "", "", false, "", errors.New("来源会话没有可转发的转录内容")
		}
		truncated = transcript.truncated
		headerLines = []string{"来源会话：" + req.SourceSessionID}
	}
	policy := policyOf(team)
	if maxChars := policy.MaxContextChars; maxChars > 0 && len([]rune(body)) > maxChars {
		body = truncateRunes(body, maxChars)
		truncated = true
	}
	note := strings.TrimSpace(req.Note)
	if note == "" {
		note = forwardNoteDefault
	}
	at := recordedAt
	if at == "" {
		at = time.Now().Format(forwardTimeLayout)
	}
	headerLines = append(headerLines,
		"时间："+at,
		contextTruncationNote)
	contextText := teamContextHeader + "\n" + strings.Join(headerLines, "\n") + "\n\n" + body + "\n\n"
	// 账本字节硬上限兜底（与派发预检共享 promptHardLimit）：正文减半收缩，
	// 正文清零仍超限（Note 本身过长等）则确定性报错。
	for len(ComposeTaskMessage(contextText, note)) > promptHardLimit {
		if body == "" {
			return "", "", false, "", fmt.Errorf("转发内容过长：合成消息超过账本上限 %d 字节", promptHardLimit)
		}
		body = truncateRunes(body, len([]rune(body))/2)
		truncated = true
		contextText = teamContextHeader + "\n" + strings.Join(headerLines, "\n") + "\n\n" + body + "\n\n"
	}
	return contextText, note, truncated, at, nil
}

// forwardRecorded 返回 journal 里该幂等键的转发记账事件（无则 nil；跨重启
// 仍可判定）。转发是低频用户操作，全量重放扫描可接受——与恢复路径共用
// Journal.Replay 的读法。
func (s *Service) forwardRecorded(runID, idempotencyKey string) *Event {
	events, err := s.journal.Replay()
	if err != nil {
		return nil
	}
	for _, event := range events {
		if event == nil || event.RunID != runID || event.Type != EventTaskForwarded {
			continue
		}
		if event.IdempotencyKey == idempotencyKey {
			return event
		}
	}
	return nil
}
