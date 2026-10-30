package httpd

// teams_adapter.go 把 Server 的既有能力适配成 internal/team 的端口
//（docs/team-orchestration-plan.md §3.1 原则 3：team 只依赖接口，生产实现由
// httpd 装配提供，team 严禁 import internal/httpd）：
//   - QueueAdapter → submissions 账本（queue.go）：Enqueue 固定 mode="queue"，
//     绝不立即派发——派发时机完全交给既有 3s worker 与 settle 钩子；
//   - SessionResolver → resolveACP 的归属解析（server_bridge.go）与
//     manager.CwdFor 的工作目录读取；
//   - TranscriptSource → 迁移的源视图解析与转录压平（§7 成员间转发的
//     来源侧，只读会话快照）。
//
// 本文件不做任何调度决策；成员归属与 cwd 的「免 spawn」判定也在这里
//（teamMemberKind / teamMemberAvailable），供 teams.go 的成员绑定与列表
// available 标记复用。

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/google/uuid"
)

// NewTeamAdapters 返回 team 服务所需的队列与会话解析端口。装配发生在任何
// ServeHTTP 之前（构造后直赋无同步的既有惯例）。
func NewTeamAdapters(server *Server) (team.QueueAdapter, team.SessionResolver) {
	return teamQueueAdapter{server: server}, teamSessionResolver{server: server}
}

// NewTeamTranscriptSource 返回成员间转发（§7）所需的会话转录提取端口。
// 装配方在 NewService 之后直赋 Service.Transcripts。
func NewTeamTranscriptSource(server *Server) team.TranscriptSource {
	return teamTranscriptSource{server: server}
}

// teamTranscriptSource 只读解析成员会话的有界转录：源视图复用迁移的
// migrationSourceView（ACP 快照 > codex 历史/视图），压平复用
// migrationTranscript（按 scope 尾部偏置截断），但绝不用 migrationMessage——
// 它自带 migrate 专属头/标题/目录行并以「继续」指令尾行收束，套进
// [Team context] 会形成双层信封且尾行语义不当。这里由 teamTranscriptText
// 自拼正文行（role 标签复用 migrationTextsByLocale 的 roles 表）。
type teamTranscriptSource struct{ server *Server }

// BoundedTranscript 返回 sessionID 按 scope 裁剪的转录正文（无任何信封）。
// 找不到会话时返回错误（迁移的同款文案）。
func (t teamTranscriptSource) BoundedTranscript(sessionID, scope string) (string, bool, error) {
	normalized, valid := migrationScope(scope)
	if !valid {
		return "", false, fmt.Errorf("未知的转录范围：%s", scope)
	}
	// migrationSourceView 只读源会话（不创建会话、不发消息）；handler 的
	// writer/request 字段在该路径上不被触碰。
	h := &handler{server: t.server}
	view := h.migrationSourceView(sessionID, normalized)
	if view == nil {
		return "", false, errors.New("找不到这个会话，请稍后刷新列表")
	}
	return teamTranscriptText(view, normalized)
}

// teamTranscriptText 把规范化视图压平为转发正文：每条转录一行「角色：文本」，
// 复用 migrationTranscript 的 scope 预算与截断标记。role 标签固定 zh-CN
// （[Team context] 信封是中文，roles 表与信封语言保持一致）。
func teamTranscriptText(view *jsoncompat.Object, scope string) (string, bool, error) {
	entries, truncated := migrationTranscript(view, scope)
	if len(entries) == 0 {
		return "", truncated, nil
	}
	roles := migrationTextsByLocale["zh-CN"].roles
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		role := roles[entry.role]
		if role == "" {
			role = entry.role
		}
		lines = append(lines, role+"："+entry.text)
	}
	return strings.Join(lines, "\n"), truncated, nil
}

// teamQueueAdapter 把任务投递对接到生产 submissions 账本。
type teamQueueAdapter struct{ server *Server }

// Enqueue 以 mode="queue" 入账（queue.go enqueue）：同 id 同内容返回 prior 与
// duplicate=true 且零副作用；同 id 不同内容报「同一消息标识不能用于不同内容」；
// 文本超过账本 100000 字节硬上限同样报错——都映射为任务的确定性失败。
func (a teamQueueAdapter) Enqueue(threadID, submissionID, text string, attachments ...string) (string, bool, error) {
	if a.server.Queue == nil {
		return "", false, errors.New("当前网关未启用排队发送")
	}
	names := (&handler{server: a.server}).resolveAttachmentNames(threadID, attachments)
	entry, duplicate, err := a.server.Queue.enqueue(threadID, submissionID, text,
		"queue", "", nil, attachments, names)
	if err != nil {
		return "", false, err
	}
	return entry.Status, duplicate, nil
}

// Submission 返回账本条目；ok=false 只表示账本确无该条目（queue.go get 的语义）。
func (a teamQueueAdapter) Submission(threadID, submissionID string) (team.TeamSubmission, bool) {
	if a.server.Queue == nil {
		return team.TeamSubmission{}, false
	}
	entry := a.server.Queue.get(threadID, submissionID)
	if entry == nil {
		return team.TeamSubmission{}, false
	}
	return team.TeamSubmission{Status: entry.Status, Error: entry.Error}, true
}

// CancelSubmission 撤销仍处 queued 的条目（queue.go cancel）；条目已离开队列
// 时返回错误，team 侧按良性处理（副作用目标已不存在）。
func (a teamQueueAdapter) CancelSubmission(threadID, submissionID string) error {
	if a.server.Queue == nil {
		return errSubmissionGone
	}
	return a.server.Queue.cancel(threadID, submissionID)
}

// teamSessionResolver 映射 resolveACP：live session > manager.Knows > 冷 List
// 探测（server_bridge.go:81-138），不在此另写归属解析。
type teamSessionResolver struct{ server *Server }

// Resolve 返回成员会话的归属（manager 为 nil 表示不属于任何 ACP Agent）；
// session 可能为 nil（归属明确但当前打不开），err 携带打开失败原因。
func (r teamSessionResolver) Resolve(threadID string) (*acpruntime.Manager, *acpruntime.Session, error) {
	resolution := r.server.resolveACP(threadID)
	return resolution.manager, resolution.session, resolution.err
}

// Recreate 用成员绑定的 Agent 类型创建替代会话；旧 sessionId 已不在任何
// 列表中也可恢复。懒物化 Agent 继续走既有预热，避免重启后再次丢失。
func (r teamSessionResolver) Recreate(agentKind, threadID, cwd string) (string, error) {
	manager := r.server.acpAgentSet()[agentKind]
	if manager == nil {
		return "", fmt.Errorf("Agent %s 未配置", agentKind)
	}
	if cwd == "" {
		cwd = r.Cwd(threadID)
	}
	if cwd == "" {
		return "", errors.New("无法确定原会话工作目录")
	}
	session, err := manager.Create("sess_"+uuid.NewString(), cwd, "团队会话恢复", 60*time.Second)
	if err != nil {
		return "", err
	}
	warmupMemberSession(manager, session)
	return session.ID, nil
}

// Cwd 返回成员会话的工作目录：只查 live 句柄与既有 list 缓存（CwdFor），
// 绝不为了拿 cwd 去开进程——resolveACP 的 openACP 分支会 spawn，调度循环
// 每轮都会走到这里。解析不到返回空串（exclusive 写任务缺 cwd 时本轮跳过）。
func (r teamSessionResolver) Cwd(threadID string) string {
	agents := r.server.acpAgentSet()
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil {
			continue
		}
		if manager.Get(threadID) == nil && !manager.Knows(threadID) {
			continue
		}
		if cwd := manager.CwdFor(threadID); cwd != "" {
			return cwd
		}
	}
	return ""
}

// teamMemberKind 解析成员会话归属的 agent kind：live 句柄或既有 list 缓存
// （含 canonical 占位）命中即得。绝不 spawn、绝不冷 List 探测——绑定成员是
// 创建请求的一部分，不允许以拉起 Agent 进程为副作用。前端只能从 /api/sessions
// 列表选择成员会话，而该列表本身来自各 manager 的 list 缓存，所以正常路径
// 总能命中；解析不到（未接入、属 codex 或已删除）阶段一一律拒绝。
func (s *Server) teamMemberKind(sessionID string) (string, bool) {
	agents := s.acpAgentSet()
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		manager := agents[kind]
		if manager == nil {
			continue
		}
		if manager.Get(sessionID) != nil || manager.Knows(sessionID) {
			return kind, true
		}
	}
	return "", false
}

// teamMemberAvailable 报告成员会话当前是否可达（live 或 Knows 命中），作为
// Team 列表/详情里成员的 available 标记（§4.1：引用失效时保留记录并标记，
// 不静默改派）。同样不做冷探测，避免列表读取被成员探测阻塞。
func (s *Server) teamMemberAvailable(sessionID string) bool {
	_, ok := s.teamMemberKind(sessionID)
	return ok
}
