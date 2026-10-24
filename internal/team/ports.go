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
	// Enqueue 以 mode=queue 入账并返回入账状态。同 id 同内容的重复入账返回
	// duplicate=true 且无任何副作用（既有账本幂等）；同 id 不同内容返回错误
	// 「同一消息标识不能用于不同内容」。文本超过账本硬上限（100000 字节，
	// internal/httpd/queue.go validateSubmissionInput）同样返回错误。
	Enqueue(threadID, submissionID, text string) (status string, duplicate bool, err error)
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

// ErrRunNotWaiting 表示 resume 目标不处于 waiting_input（§10.2：resume 只用于
// 外因暂停；不得把确定性 failed 或 outcome_unknown 直接改成 running）。
// HTTP 侧映射 409。
var ErrRunNotWaiting = errors.New("只有等待用户处理的运行可以继续")

// ErrRunFinished 表示运行已达终态（completed/failed/cancelled/stopped/
// outcome_unknown），取消等写操作被拒绝。HTTP 侧映射 409。
var ErrRunFinished = errors.New("运行已结束，不能取消")
