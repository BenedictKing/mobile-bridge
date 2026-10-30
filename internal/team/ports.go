package team

// ports.go 声明 team 包对外部世界的全部依赖面（§3.1 原则 3：核心决策纯函数、
// 真实 I/O 走接口）。适配器由 internal/httpd 装配提供——team 严禁 import
// internal/httpd（生产 submissions 状态机在其未导出类型上）与 internal/service
// （那是 Python conformance 的 stub port，无真实 adapter）；team 只 import
// internal/acpruntime（无环）。

import (
	"errors"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
)

// submissions 账本（internal/httpd/queue.go）的 submission 状态取值，与
// recovery.go 的 Probe 词汇共用同一套常量拼写。
const (
	// SubStatusQueued：已入账、尚未派发（既有 3s worker / settle 钩子会派发）。
	SubStatusQueued = ledgerQueued
	// SubStatusUnknown：派发结果未确认，永不自动重放（§6.2）。
	SubStatusUnknown = ledgerUnknown
	// SubStatusAccepted：已派发到 Agent。
	SubStatusAccepted = ledgerAccepted
	// SubStatusFailed：派发确定性失败（账本带错误文本）。
	SubStatusFailed = ledgerFailed
	// SubStatusCancelled：仍在队列时被用户撤销。
	SubStatusCancelled = ledgerCancelled
	// SubStatusIgnored：结果未确认后被用户忽略（终态、隐藏）。
	SubStatusIgnored = ledgerIgnored
)

// TeamSubmission 是 submissions 账本条目的 team 侧只读视图。
type TeamSubmission struct {
	Status string
	Error  string
}

// QueueAdapter 把任务投递到既有 submissions 队列（§6.2）。实现方（httpd）：
// Enqueue 内部走 mode=queue 入账，绝不立即派发——派发时机完全交给既有 3s
// worker 与 settle 钩子（会话忙时排队、空闲即发），从而天然获得 queueMu
// 串行化与幂等去重。
type QueueAdapter interface {
	// Enqueue 以 mode=queue 入账并返回入账状态。attachments 是该 Lead 会话
	// 上传账本中的附件 ID，普通显式任务为空。同 id 同内容（含附件）的重复入账
	// 返回 duplicate=true 且无任何副作用；同 id 不同内容返回错误。文本超过
	// 账本硬上限（100000 字节，internal/httpd/queue.go）同样返回错误。
	Enqueue(threadID, submissionID, text string, attachments ...string) (status string, duplicate bool, err error)
	// Submission 返回账本条目；ok=false 只应表示「账本确无该条目」——查询
	// 本身失败时必须返回 err，伪装成 ok=false 会导致重复发送（recovery.go
	// Probe 的同款警告）。
	Submission(threadID, submissionID string) (TeamSubmission, bool)
	// CancelSubmission 撤销仍处 queued 的条目（queue.cancel 语义）。条目已
	// 离开队列时返回错误，调用方按良性处理（副作用目标已不存在）。
	CancelSubmission(threadID, submissionID string) error
}

// SessionResolver 解析成员会话归属（httpd 侧 resolveACP 的映射：live session
// > manager.Knows > 冷 List 探测）。Resolve 返回的 session 可能为 nil（归属
// 明确但当前打不开）；err 携带打开失败原因。
type SessionResolver interface {
	Resolve(threadID string) (*acpruntime.Manager, *acpruntime.Session, error)
	// Cwd 返回成员会话的工作目录；解析不到返回空串（exclusive 写任务缺
	// cwd 时调度会跳过该轮，绝不无锁派发）。
	Cwd(threadID string) string
}

// SessionRecreator 是可选的会话恢复端口：仅在 session/load 明确报告会话
// 不存在时创建同 Agent、cwd 的替代会话；调用方负责回写成员绑定。
type SessionRecreator interface {
	Recreate(agentKind, threadID, cwd string) (sessionID string, err error)
}

// TranscriptSource 提取成员会话的有界转录（§7.1「用户可以选择注入最近 N 条
// 消息」的阶段二实现：按 scope 裁剪，绝不静默注入完整 transcript）。实现方
// 只读会话快照，不产生任何会话副作用。
type TranscriptSource interface {
	// BoundedTranscript 返回 sessionID 按 scope 裁剪的转录正文（不含任何
	// 信封）；truncated 表示因预算被截断。scope 的合法取值与校验由实现方
	// 定义（httpd 侧复用迁移的 main/full/latest）。
	BoundedTranscript(sessionID, scope string) (text string, truncated bool, err error)
}

// ErrTranscriptUnavailable 表示网关未装配会话转录提取端口（Service.Transcripts
// 为 nil）：sourceSessionId 的成员间转发不可用。
var ErrTranscriptUnavailable = errors.New("网关未装配会话转录提取")

// WorkspaceDiff 是 worktree 相对基线的单文件有界 diff（§6.4 规则 6：并行
// worktree 的结果不自动合并到主工作区，第一版只生成 diff 供用户审查）。
// Truncated 标记该文件内容因超限被截断（Added/Removed 是截断前的全量计数）。
type WorkspaceDiff struct {
	Path      string `json:"path"`
	DiffText  string `json:"diff"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Truncated bool   `json:"truncated"`
}

// WorkspaceAdapter 是用户预备 worktree 的 I/O 端口（§6.4 + §16 默认决策：
// 第一版只引用用户准备好的 worktree，绝不自动创建/合并/清理）。实现方
// （httpd）全部走固定 argv 的 exec.Command，绝不过 shell：
//   - Resolve 从 {dataDir}/team-workspaces.json 注册表（{id:{path,createdAt,
//     createdBy}}）解析 workspaceID → 路径；注册/列表 HTTP 入口归 httpd 侧
//     阶段二 API；
//   - HeadRevision 读取 worktree 当前 HEAD。基线校验只比 HEAD——工作区脏
//     （未提交改动是任务的正常产出）不算基线变更；
//   - Diff 返回相对 baseRevision 的逐文件 diff（unified=3 重组），双层截断：
//     单文件超限置该文件 Truncated，整包超文件数/字节上限置顶层 truncated；
//   - Cleanup 删除 worktree（不带 --force：脏区失败上抛，绝不丢用户改动）。
type WorkspaceAdapter interface {
	// Resolve 返回 workspaceID 注册的 worktree 路径；未注册/注册表不可读
	// 返回错误（派发侧按确定性失败处理）。
	Resolve(workspaceID string) (path string, err error)
	// HeadRevision 返回 path 的当前 HEAD 提交（空仓库/非 git 目录报错）。
	HeadRevision(path string) (string, error)
	// Diff 返回 path 相对 baseRevision 的有界 diff 与顶层截断标记。
	Diff(path, baseRevision string) ([]WorkspaceDiff, bool, error)
	// Cleanup 删除 workspaceID 注册的 worktree；脏区失败返回错误。
	Cleanup(workspaceID string) error
}

// ErrWorkspacesUnavailable 表示网关未装配工作区注册表端口（Service.Workspaces
// 为 nil）：isolated 任务的派发校验与 diff/清理不可用，任务确定性失败。
var ErrWorkspacesUnavailable = errors.New("网关未装配工作区注册表")

// ErrRunNotWaiting 表示 resume 目标不处于 waiting_input（§10.2：resume 只用于
// 外因暂停；不得把确定性 failed 或 outcome_unknown 直接改成 running）。
// HTTP 侧映射 409。
var ErrRunNotWaiting = errors.New("只有等待用户处理的运行可以继续")

// ErrRunFinished 表示运行已达终态（completed/failed/cancelled/stopped/
// outcome_unknown），取消等写操作被拒绝。HTTP 侧映射 409。
var ErrRunFinished = errors.New("运行已结束，不能取消")
