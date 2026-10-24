package httpd

// teams_adapter.go 把 Server 的既有能力适配成 internal/team 的两个端口
//（docs/team-orchestration-plan.md §3.1 原则 3：team 只依赖接口，生产实现由
// httpd 装配提供，team 严禁 import internal/httpd）：
//   - QueueAdapter → submissions 账本（queue.go）：Enqueue 固定 mode="queue"，
//     绝不立即派发——派发时机完全交给既有 3s worker 与 settle 钩子；
//   - SessionResolver → resolveACP 的归属解析（server_bridge.go）与
//     manager.CwdFor 的工作目录读取。
//
// 本文件不做任何调度决策；成员归属与 cwd 的「免 spawn」判定也在这里
//（teamMemberKind / teamMemberAvailable），供 teams.go 的成员绑定与列表
// available 标记复用。

import (
	"errors"
	"sort"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// NewTeamAdapters 返回 team 服务所需的队列与会话解析端口。装配发生在任何
// ServeHTTP 之前（构造后直赋无同步的既有惯例）。
func NewTeamAdapters(server *Server) (team.QueueAdapter, team.SessionResolver) {
	return teamQueueAdapter{server: server}, teamSessionResolver{server: server}
}

// teamQueueAdapter 把任务投递对接到生产 submissions 账本。
type teamQueueAdapter struct{ server *Server }

// Enqueue 以 mode="queue" 入账（queue.go enqueue）：同 id 同内容返回 prior 与
// duplicate=true 且零副作用；同 id 不同内容报「同一消息标识不能用于不同内容」；
// 文本超过账本 100000 字节硬上限同样报错——都映射为任务的确定性失败。
func (a teamQueueAdapter) Enqueue(threadID, submissionID, text string) (string, bool, error) {
	if a.server.Queue == nil {
		return "", false, errors.New("当前网关未启用排队发送")
	}
	entry, duplicate, err := a.server.Queue.enqueue(threadID, submissionID, text,
		"queue", "", nil, nil, nil)
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
