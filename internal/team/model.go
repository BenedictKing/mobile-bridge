// Package team 提供多 Agent 编排的核心对象与持久化（docs/team-orchestration-plan.md
// §4、§8）：Team/Run/Task 模型、teams.json / team-runs.json 原子账本（乐观版本，
// §8.4）、追加式 team-journal.jsonl 运行事实（§8.1），以及每 Run 的事件投影
// （§10.3）。崩溃一致性优先于功能广度：账本写入为原子替换且落盘成功后才翻内存
// 值；journal 以单调序号追加，重放拒绝重复/回退序号；Team/Run/Task/TaskResult
// 保留未知字段，未来版本的账本可以无损读回。
package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// 任务状态（§4.3 状态图）。outcome_unknown 表示外部副作用是否发生尚未确认，
// 禁止自动重发；cancelled/stopped 可通过对账或恢复继续。
const (
	TaskPlanned        = "planned"
	TaskReady          = "ready"
	TaskDispatching    = "dispatching"
	TaskRunning        = "running"
	TaskCompleted      = "completed"
	TaskFailed         = "failed"
	TaskCancelled      = "cancelled"
	TaskOutcomeUnknown = "outcome_unknown"
	TaskStopped        = "stopped"
)

// Run 状态（§4.3 状态图）。planning 由阶段三 Lead 自动规划使用，显式 Plan
// 不经过该状态；reducing 是验证闸门窗口（§6.4.1，阶段二）：全任务终态且
// fold 判 completed 后先过闸门再终局——闸门未过 → waiting_input 可修可
// 重跑，过了才落 completed。collecting 预留给收集策略的中间态（当前
// fold 路径直接从 dispatching 终局）。paused 是用户主动的可恢复软停——
// §12 阶段四工作项 4：非终态、不进 terminalRun，只停新派发，在途任务
// 自然收尾入账（停靠/结果未知会把 run 升级 waiting_input，语义严格强于
// paused）；run 超时与 stall 时钟冻结（软停不烧时钟），resume 推回
// dispatching 并重臂。
const (
	RunCreated        = "created"
	RunPlanning       = "planning"
	RunDispatching    = "dispatching"
	RunCollecting     = "collecting"
	RunReducing       = "reducing"
	RunCompleted      = "completed"
	RunWaitingInput   = "waiting_input"
	RunPaused         = "paused"
	RunFailed         = "failed"
	RunCancelled      = "cancelled"
	RunStopped        = "stopped"
	RunOutcomeUnknown = "outcome_unknown"
)

// 事件类型（§10.3 的阶段一子集）。task_cache_hit 等留给后续阶段；
// action_* / invalid_action_block 属阶段三（§5.1，本包 actions.go 是唯一
// 写入方——动作的解析与落账不经过 httpd 或执行侧直写）；question_* 属
// 阶段三 §5.4（本包 questions.go 是唯一写入方）。
const (
	EventRunCreated            = "run_created"
	EventTaskPlanned           = "task_planned"
	EventTaskDispatching       = "task_dispatching"
	EventTaskRunning           = "task_running"
	EventTaskCompleted         = "task_completed"
	EventTaskFailed            = "task_failed"
	EventTaskOutcomeUnknown    = "task_outcome_unknown"
	EventTaskStopped           = "task_stopped"
	EventTaskInputHashMismatch = "task_input_hash_mismatch"
	// EventTaskRetryScheduled 是受限重试的重臂迁移事实（§6.3，阶段二）：
	// 预算内的 rate_limited/transient 失败在 task_failed 之后「回 ready +
	// attempt+1」的快照迁移必须与失败事实同批落 journal——重试常因 AIMD
	// 冷却滞留 ready 数十秒（Retry-After 上限 300s），期间重启的话
	// ReconcileRun 只有 task_failed 终态可重放，任务会被折成确定性 failed、
	// 重试预算被静默丢弃（§8.3 第 7 条对可重试分类的豁免依赖本事件）。
	// payload {taskId, attempt, outcome}，attempt 是重臂后的下一次尝试号。
	EventTaskRetryScheduled = "task_retry_scheduled"
	EventRunWaitingInput    = "run_waiting_input"
	EventRunCompleted       = "run_completed"
	EventRunFailed          = "run_failed"
	EventRunCancelled       = "run_cancelled"
	// EventProviderThrottled 是 AIMD 窗口实际减小时的限流事实（§6.3，
	// 阶段二）：payload {bucket, window, retryAfterSecs}，每 epoch 一次。
	// run 级事件（无 taskId），HTTP/SSE 路由归 httpd-api-2。
	EventProviderThrottled = "provider_throttled"
	// worktree 生命周期事件（§6.4 阶段二，§16 默认决策 6：只引用用户预备的
	// worktree）：
	//   - workspace_bound {taskId, workspaceId, path, baseRevision}——isolated
	//     任务首派随 task_dispatching 同次记账写入，基线随 Task 持久化；
	//   - workspace_lost {taskId, workspaceId, reason}——reason ∈ path_missing |
	//     base_changed，重启恢复/收养时发现工作区丢失或基线变更：任务停靠
	//     （stopped）+ run waiting_input，绝不自动创建新 worktree；
	//   - workspace_cleanup_failed {taskId, workspaceId, error}——用户显式清理
	//     失败（脏区等），错误上抛给触发方。
	EventWorkspaceBound         = "workspace_bound"
	EventWorkspaceLost          = "workspace_lost"
	EventWorkspaceCleanupFailed = "workspace_cleanup_failed"
	// EventRunVerify 是确定性验证闸门的执行事实（§6.4.1，阶段二）：payload
	// {exitCode, durationMs, outputHead, truncated[, reason]}，run 级事件（无
	// taskId）；VerifyResult 随 Run 快照持久化，resume 复验后再次落账。
	EventRunVerify = "run_verify"
	// 阶段三 Lead 自动动作的事件（§5.1/§10.3）：action_proposed 是合法动作
	// 入账事实（payload {taskId, index, actionKey, type, round, member?,
	// taskHead?, fromTaskId?}，IdempotencyKey=actionKey）——由 actions.go 在
	// Lead 任务结算的单批 commitRun 里写入，执行侧只消费不另写；
	// action_rejected 是执行侧的拒绝事实（payload {actionKey, reason}，写入
	// 归执行流，重建语义归本包）；invalid_action_block 是解析/校验失败事实
	// （payload {taskId, attempt, final, round, violations}，每次失败一条，
	// final=true 表示修复回路耗尽按普通文本收场）。
	EventActionProposed     = "action_proposed"
	EventActionRejected     = "action_rejected"
	EventInvalidActionBlock = "invalid_action_block"
	// EventTaskCacheHit 是 amend 缓存命中的入账事实（§6.4.1/§10.2 amend 段，
	// 阶段三，amend.go 是唯一写入方）：payload {taskId, parentRunId,
	// parentTaskId, inputHash, sessionId, turnId, result, resultChars,
	// truncated}。命中结果仍作为本 Run 的新节点实例入账（§6.4.1 规则 4：
	// 不跳过事件写入）——任务在创建批次内直接落 completed，task 级事件。
	EventTaskCacheHit = "task_cache_hit"
	// 成员主动提问的事件（§5.4，阶段三）：question_opened {questionId,
	// taskId, version, kind, summary, options}、question_resolved
	// {questionId, taskId, answerHead}（IdempotencyKey=decision 键，§9 决策
	// 带 decisionID 且重复提交返回 duplicate）。question_withdrawn 是 §10.3
	// 事件表的表外增量（照 EventRunStopped 先例补登）：Task 取消/Run 终止/
	// 会话卡过期都必须有撤回事实可审计，payload {questionId, taskId, reason}，
	// reason ∈ task_terminal | run_finished | stale。
	EventQuestionOpened    = "question_opened"
	EventQuestionResolved  = "question_resolved"
	EventQuestionWithdrawn = "question_withdrawn"
	// EventRunPaused 是用户暂停的 checkpoint 事实（§12 阶段四工作项 4，
	// runner.go Pause 是唯一写入方）：payload {reason, sequence}——sequence
	// 是暂停落账前 journal 已有事实的最大序号，即「Run 快照 + journal 游标」
	// checkpoint 的游标锚点（不加新存储：恢复续跑不重发由 submissions 账本
	// 对账 + submissionID 确定性幂等保证，§8.3）。store.go 的 journal 重建
	// 据此把 paused Run 复原为 paused（缺了它会在 run_created 锚点上以
	// created 复活、被恢复路径自动 fold）。resume 是纯状态推进，不落镜像
	// 事件（快照迁移的 journal 佐证是暂停后的派发/终态事实本身）。
	EventRunPaused = "run_paused"
	// EventRunBudgetResumed 是预算停靠后用户续跑的预算追加事实（§9
	// 「继续本轮」，阶段四 cost-stats，runner.go Resume 的 budget 分支是
	// 唯一写入方）：payload {ceiling, units, used}——ceiling=已派发数
	// （journal task_dispatching 计数）+ 当前 BudgetUnits，随事件同步写
	// Run.BudgetCeiling。上限推导钉死为 journal 事实 + 快照双载体：重启
	// 后 store.go applyJournalEventToRun 重放本事件回填 ceiling（单调取
	// 高），不丢失、不漂移；预算停靠本身（run_waiting_input payload
	// {units,used,stopReason}）先于本事件，重建按事件序自然还原停靠语义。
	EventRunBudgetResumed = "run_budget_resumed"
)

// 策略取值（§4.2）。isolated_worktree 属阶段二：只引用用户预备的 worktree
// （§16 默认决策：第一版不自动创建/合并/清理）。
const (
	WorkspaceShared           = "shared_workspace"
	WorkspaceIsolatedWorktree = "isolated_worktree"
	WritePolicyExclusive      = "exclusive"
	WritePolicyParallel       = "parallel"
)

// 收集策略取值（§6.5 阶段二）：all 等待全部分支终态（失败分支不吞掉已完成
// 结果，salvage）；first_success 仅 broadcast 模式允许——组内首个 completed
// 即满足，其余成员取消（superseded）。空值一律按 all 归一。
const (
	RunCollectAll          = "all"
	RunCollectFirstSuccess = "first_success"
)

// StopReasonSupersededByFirstSuccess 是 first_success 收集策略取消组内落败
// 成员的固定停止原因：fold 时该类 cancelled 不参与终局分类（结果保留给
// 首个成功者，§6.5）。
const StopReasonSupersededByFirstSuccess = "superseded_by_first_success"

// StopReasonBudgetExhausted 是预算停靠原因的固定前缀（§9：预算达到阈值只
// 暂停并请求用户选择「继续本轮、修订预算或停止 Run」，绝不自动续）。Resume
// 按前缀路由到预算追加分支（照 StopReasonVerifyFailed 先例：机器前缀+中文
// 说明，落 Run 快照与 run_waiting_input payload.stopReason 双载体，journal
// 重建按 payload 还原）。计量=任务派发次数（journal task_dispatching 计数）
// ——token 无法归因到 Run/Task（zcode turn_usage 的 turn id 双空间错配、
// 其余 Agent 无用量数据源），不做 token 预算。
const StopReasonBudgetExhausted = "budget_exhausted"

// 成员角色与不可用标记（§4.1：成员引用失效时保留记录并标记 unavailable）。
const (
	RoleLead     = "lead"
	RoleMember   = "member"
	RoleReviewer = "reviewer"

	MemberUnavailable = "unavailable"
)

// memberIDPattern 约束成员 id：字母/数字/下划线/连字符，1-64 字符。
var memberIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidMemberID 报告 id 是否可作为 TeamMember.ID。
func ValidMemberID(id string) bool { return memberIDPattern.MatchString(id) }

// instructionsHeadRunes 是任务卡摘要的截断长度（§11.4：Prompt 前 240 字符）。
const instructionsHeadRunes = 240

// InstructionsHead 截取 Prompt 前 240 个 rune 作为任务卡摘要。
func InstructionsHead(prompt string) string {
	count := 0
	for index := range prompt {
		if count == instructionsHeadRunes {
			return prompt[:index]
		}
		count++
	}
	return prompt
}

// Team 是一个多 Agent 编排单元（§4.1）。extra 保留账本中的未知字段。
type Team struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Members   []TeamMember `json:"members"`
	LeadID    string       `json:"leadId,omitempty"`
	Policy    TeamPolicy   `json:"policy"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
	Version   uint64       `json:"version"`

	extra *jsoncompat.Object
}

// TeamMember 引用一个既有会话（§4.1）。
type TeamMember struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	AgentKind string `json:"agentKind"`
	Name      string `json:"name"`
	Role      string `json:"role"` // lead | member | reviewer
	Enabled   bool   `json:"enabled"`
}

// TeamPolicy 是编排策略（§4.2）。零值不可直接使用，请用 DefaultPolicy。
type TeamPolicy struct {
	MaxParallelTasks       int    `json:"maxParallelTasks"`
	MinParallelTasks       int    `json:"minParallelTasks"`
	MaxProviderConcurrency int    `json:"maxProviderConcurrency"`
	MaxTaskRetries         int    `json:"maxTaskRetries"`
	MaxRounds              int    `json:"maxRounds"`
	MaxTasksPerRun         int    `json:"maxTasksPerRun"`
	MaxContextChars        int    `json:"maxContextChars"`
	MaxResultChars         int    `json:"maxResultChars"`
	TaskTimeoutSecs        int    `json:"taskTimeoutSecs"`
	RunTimeoutSecs         int    `json:"runTimeoutSecs"`
	BudgetUnits            int    `json:"budgetUnits"`
	WorkspaceMode          string `json:"workspaceMode"` // shared_workspace | isolated_worktree
	WritePolicy            string `json:"writePolicy"`   // exclusive | parallel
	AutoDispatch           bool   `json:"autoDispatch"`
	AutoApplyChanges       bool   `json:"autoApplyChanges"`
}

// DefaultPolicy 返回 §4.2 的默认策略。
func DefaultPolicy() TeamPolicy {
	return TeamPolicy{
		MaxParallelTasks:       3,
		MinParallelTasks:       1,
		MaxProviderConcurrency: 5,
		MaxTaskRetries:         1, // §6.3：仅限流/瞬态错误的受限重试次数
		MaxRounds:              3,
		MaxTasksPerRun:         12,
		MaxContextChars:        12000,
		MaxResultChars:         12000,
		TaskTimeoutSecs:        1800,
		RunTimeoutSecs:         7200,
		BudgetUnits:            0, // 0 表示只统计，不主动暂停
		WorkspaceMode:          WorkspaceShared,
		WritePolicy:            WritePolicyExclusive,
		AutoDispatch:           false,
		AutoApplyChanges:       false,
	}
}

// Run 是一次编排执行（§4.3）。team-runs.json 是可重建的查询快照，
// 运行事实以 team-journal.jsonl 为准。VerifyCommand 是 Run 声明的确定性
// 验证闸门（§6.4.1，固定 argv 白名单，见 verify.go）：全任务终态且 fold 为
// completed 时先过闸门再终局，绝不采信 Agent 的口头成功声明；Verify 持久化
// 闸门结果，resume 清空后重跑即复验。
type Run struct {
	ID            string `json:"id"`
	TeamID        string `json:"teamId"`
	Goal          string `json:"goal"`
	Mode          string `json:"mode,omitempty"`
	CollectPolicy string `json:"collectPolicy,omitempty"`
	AutoPlan      bool   `json:"autoPlan,omitempty"`
	Status        string `json:"status"`
	StopReason    string `json:"stopReason,omitempty"`
	Round         int    `json:"round"`
	PlanHash      string `json:"planHash"`
	// ParentRunID 是 amend 修订链的父 Run（§6.4.1/§10.2 amend 段，阶段三）：
	// 创建新 Run、绝不改写父 Run 的 journal；父 Run 的可复用前驱结果按
	// inputHash 匹配导入（amend.go），随 run_created payload 落账供
	// team-runs.json 损坏后的 journal 重建。空串=非 amend 产出的 Run。
	ParentRunID string `json:"parentRunId,omitempty"`
	// ScheduledFrom 是定时触发的可追溯标记（§12 阶段四工作项 1）：值为
	// schedule.go 生成的确定性 triggerID（uuid5(nsURL,
	// "mobile-bridge:team-schedule:{teamID}:{dueUnix}")），随 run_created
	// payload.scheduledBy 落账；崩溃窗口的回扫对账（journal 有无该标记的
	// run_created 事件）是触发幂等的唯一裁决。空串=用户手工创建的 Run。
	ScheduledFrom string `json:"scheduledFrom,omitempty"`
	// BudgetCeiling 是预算停靠的有效上限（§9，阶段四 cost-stats）：0=未追加
	// 过预算（阈值为当前策略 BudgetUnits）；用户在 budget_exhausted 停靠后
	// Resume 时=已派发数+BudgetUnits，随 run_budget_resumed 事件落 journal
	// 并同步写本快照字段——重启后按事件重放回填（store.go
	// applyJournalEventToRun，单调取高），journal 事实+快照双载体不漂移。
	// 四处同步纪律：runKnownFields / jsonObject Set / Clone（值拷贝天然覆盖）
	// / applyJournalEventToRun。
	BudgetCeiling  int      `json:"budgetCeiling,omitempty"`
	Tasks          []Task   `json:"tasks"`
	PendingActions []Action `json:"pendingActions,omitempty"`
	// PendingQuestions 是成员主动提问的登记表（§5.4）：detail 顶层直出
	//（teamRunDetail 自动携带），绝不进 changes meta（meta 逐键变更触发整页
	// 重渲染契约，teams.go teamRunMeta）。
	PendingQuestions []PendingQuestion `json:"pendingQuestions,omitempty"`
	VerifyCommand    []string          `json:"verifyCommand,omitempty"`
	Verify           *VerifyResult     `json:"verify,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
	Version          uint64            `json:"version"`

	extra *jsoncompat.Object
}

// VerifyResult 是验证闸门的执行结果（§6.4.1/§13.3.7）：退出码交 Reducer 判
// 定（非零 → run waiting_input 可修可重跑；零 → 正常 fold），OutputHead 是
// 合并 stdout/stderr 的有界头部（≤2048 字节，verify.go），随 Run 快照跨重启
// 存活。ExitCode=-1 统一表示「命令未能给出退出码」（cwd 解析失败/超时/启动
// 失败），具体归因见 run_verify 事件的 reason 字段。
type VerifyResult struct {
	ExitCode   int       `json:"exitCode"`
	OutputHead string    `json:"outputHead"`
	Truncated  bool      `json:"truncated"`
	FinishedAt time.Time `json:"finishedAt"`

	extra *jsoncompat.Object
}

// Task 是 Run 内的一个执行单元（§4.3）。Result 是结果文本本体；
// ResultDetail 携带 §6.5 要求的引用字段（sessionId/turnId/…），随 Run
// 持久化，供 salvage 快照与重启对账使用。BaseRevision 是 isolated_worktree
// 任务首派记录的基线提交（§6.4 规则 4，随 task_dispatching 同次记账写入），
// 重启对账（workspace_lost）与 diff 数据面都以它为准。SentText 是派发时合成
// 的完整发送文本（评审遗留④：随 task_dispatching 同次落快照），收养/超时/
// 取消路径直接复用存量文本——matchTurn 按全文匹配 turn（dispatcher.go），策略
// 或上下文合成的任何变更都不再让在途任务错认 turn。HTTP 投影不外发（§10.3
// 有界 payload，teams.go teamRunDetail 显式剔除）。
type Task struct {
	ID             string      `json:"id"`
	MemberID       string      `json:"memberId"`
	Prompt         string      `json:"prompt"`
	InputHash      string      `json:"inputHash"`
	Attempt        int         `json:"attempt"`
	SiteID         string      `json:"siteId,omitempty"`
	Ordinal        int         `json:"ordinal,omitempty"`
	DependsOn      []string    `json:"dependsOn,omitempty"`
	BroadcastGroup string      `json:"broadcastGroup,omitempty"`
	WorkspaceMode  string      `json:"workspaceMode,omitempty"`
	WorkspaceID    string      `json:"workspaceId,omitempty"`
	BaseRevision   string      `json:"baseRevision,omitempty"`
	WriteLock      string      `json:"writeLock,omitempty"`
	Status         string      `json:"status"`
	SubmissionID   string      `json:"submissionId,omitempty"`
	Result         string      `json:"result,omitempty"`
	ResultDetail   *TaskResult `json:"resultDetail,omitempty"`
	// CachedFrom 是 amend 缓存导入的来源任务 ID（§6.4.1，阶段三）：本 Run 的
	// 新节点实例在创建批次内直接落 completed，Result/ResultDetail 深拷贝自
	// 父 Run 的同名任务，SubmissionID/SentText 留空——缓存任务绝不重派发。
	// 空串=常规执行产出。
	CachedFrom       string `json:"cachedFrom,omitempty"`
	Error            string `json:"error,omitempty"`
	Outcome          string `json:"outcome,omitempty"`
	StopReason       string `json:"stopReason,omitempty"`
	InstructionsHead string `json:"instructionsHead,omitempty"`
	SentText         string `json:"sentText,omitempty"`

	extra *jsoncompat.Object
}

// TaskResult 是任务结果的结构化引用（§6.5）：每条结果必须带 sessionId、
// turnId、resultChars 与 truncated 标记，跨网关重启存活。FileSummary/
// CommandSummary 是从 turn 的 activity 条目挖掘的有界摘要（§6.5 文件变更
// 摘要与命令执行摘要，随 Run 持久化并随 task_completed 事件携带）。
type TaskResult struct {
	SessionID      string `json:"sessionId"`
	TurnID         string `json:"turnId"`
	Result         string `json:"result"`
	ResultChars    int    `json:"resultChars"`
	Truncated      bool   `json:"truncated"`
	StopReason     string `json:"stopReason,omitempty"`
	FileSummary    string `json:"fileSummary,omitempty"`
	CommandSummary string `json:"commandSummary,omitempty"`

	extra *jsoncompat.Object
}

// Action 是 Lead 提出的待确认动作（§5.2）。阶段三起由 actions.go 从 Lead
// 回复的 <team-actions> 信封解析落账（Status=proposed）；执行/拒绝由执行流
// 改写（executed/rejected）。FromTaskID 是 request_context 的结果来源任务
// （§5.2「请求另一成员提供已有结果」——校验要求存在于本 Run 且已完成）；
// WriteLock 是 delegate/summarize 动作可选声明的派生任务写锁（§6.4 规则 3，
// ∈ {exclusive, none}；空＝按动作类型缺省归一，见 buildActionTask），
// request_context 不创建任务、不接受该字段；ActionKey 是动作的幂等键
// （team:{t}:run:{r}:action:{sourceTaskID}:{round}:{index}）；Reason 在
// rejected 时非空。
type Action struct {
	Type         string `json:"type"`
	SourceTaskID string `json:"sourceTaskId"`
	Round        int    `json:"round"`
	Member       string `json:"member,omitempty"`
	Task         string `json:"task,omitempty"`
	FromTaskID   string `json:"fromTaskId,omitempty"`
	WriteLock    string `json:"writeLock,omitempty"`
	ActionKey    string `json:"actionKey,omitempty"`
	Status       string `json:"status,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// Action 状态取值（§5.2/§12 阶段三）：proposed 已入账待执行；executed 已由
// 执行流落地（task_planned/task_forwarded 携带 actionKey）；rejected 被执行
// 流拒绝（Reason 说明原因）。
const (
	ActionStatusProposed = "proposed"
	ActionStatusExecuted = "executed"
	ActionStatusRejected = "rejected"
)

// 问题 Kind 取值（§5.4）：question 是 AskUserQuestion 形态的提问卡（成员把
// 提问桥接在 session/request_permission 上，acpview 以 title==AskUserQuestion
// 标记 question:true）；input 是 elicitation/create 的补充输入卡（acpruntime
// pendingKinds 的 input 口径）。
const (
	QuestionKindQuestion = "question"
	QuestionKindInput    = "input"
)

// PendingQuestion 状态取值（§5.4）：open 已登记待回答；answered 已按
// questionID+version 消费（答案只允许消费一次）；withdrawn 已随 Task 取消/
// Run 终止/会话卡过期撤回。
const (
	QuestionStatusOpen      = "open"
	QuestionStatusAnswered  = "answered"
	QuestionStatusWithdrawn = "withdrawn"
)

// QuestionOption 是提问卡的一个可选答案（acpview 把 permission options 映射
// 为 {id, label, kind}；回答报文按 optionId 引用）。
type QuestionOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// PendingQuestion 是成员主动提问的登记条目（§5.4）：QuestionID={taskID}:
// {requestID}（会话请求 ID 在进程内可能跨成员撞号，任务前缀保证 Run 内唯一，
// 末段可剥回会话侧 requestID 供 Respond 投递）；Version 是登记时的 Run 快照
// 版本（decision 必须按 questionID+version 回答，过期版本拒绝并要求刷新）；
// Summary/Options 是有界摘要（逐条截断，§10.3 有界 payload）；Status 见
// QuestionStatus* 常量。任务保持 running、Run 不强制 waiting_input（§5.4
// 独立分支继续；开放问题期间任务与 run 超时豁免）。
type PendingQuestion struct {
	QuestionID string           `json:"questionId"`
	TaskID     string           `json:"taskId"`
	Version    uint64           `json:"version"`
	Kind       string           `json:"kind"`
	Summary    string           `json:"summary"`
	Options    []QuestionOption `json:"options,omitempty"`
	Status     string           `json:"status"`
	OpenedAt   time.Time        `json:"openedAt"`
}

// Event 是写入 journal 的运行事实（§4.3）。Sequence 由 Journal.Append
// 统一编号，调用方无需自行赋值。
type Event struct {
	ID             string         `json:"id"`
	RunID          string         `json:"runId"`
	Type           string         `json:"type"`
	Source         string         `json:"source"`
	Payload        map[string]any `json:"payload"`
	IdempotencyKey string         `json:"idempotencyKey,omitempty"`
	Sequence       uint64         `json:"sequence"`
	CreatedAt      time.Time      `json:"createdAt"`
}

// ---- 未知字段保留（§8.1）----
//
// Team/Run/Task/TaskResult 自定义 UnmarshalJSON：已知字段逐个解码，
// 多余键存入 extra（保持文档顺序与数字字面量），Marshal 时原样回写。

var teamKnownFields = map[string]bool{
	"id": true, "name": true, "members": true, "leadId": true,
	"policy": true, "createdAt": true, "updatedAt": true, "version": true,
}

var runKnownFields = map[string]bool{
	"id": true, "teamId": true, "goal": true, "mode": true,
	"collectPolicy": true, "autoPlan": true, "status": true,
	"stopReason": true, "round": true, "planHash": true, "parentRunId": true,
	"scheduledFrom": true, "budgetCeiling": true,
	"tasks": true, "pendingActions": true, "pendingQuestions": true,
	"verifyCommand": true, "verify": true,
	"createdAt": true, "updatedAt": true, "version": true,
}

var verifyResultKnownFields = map[string]bool{
	"exitCode": true, "outputHead": true, "truncated": true, "finishedAt": true,
}

var taskKnownFields = map[string]bool{
	"id": true, "memberId": true, "prompt": true, "inputHash": true,
	"attempt": true, "siteId": true, "ordinal": true, "dependsOn": true,
	"broadcastGroup": true, "workspaceMode": true, "workspaceId": true,
	"baseRevision": true, "writeLock": true,
	"status": true, "submissionId": true, "result": true, "resultDetail": true,
	"cachedFrom": true,
	"error":      true, "outcome": true, "stopReason": true, "instructionsHead": true,
	"sentText": true,
}

var taskResultKnownFields = map[string]bool{
	"sessionId": true, "turnId": true, "result": true,
	"resultChars": true, "truncated": true, "stopReason": true,
	"fileSummary": true, "commandSummary": true,
}

// UnmarshalJSON 解码 Team 并保留未知字段。
func (t *Team) UnmarshalJSON(data []byte) error {
	type teamAlias Team
	var decoded teamAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, teamKnownFields)
	if err != nil {
		return err
	}
	*t = Team(decoded)
	t.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (t *Team) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(t.jsonObject()), nil
}

func (t *Team) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", t.ID)
	object.Set("name", t.Name)
	members := &jsoncompat.Array{}
	for i := range t.Members {
		members.Items = append(members.Items, t.Members[i].jsonObject())
	}
	object.Set("members", members)
	if t.LeadID != "" {
		object.Set("leadId", t.LeadID)
	}
	object.Set("policy", t.Policy.jsonObject())
	object.Set("createdAt", t.CreatedAt.Format(time.RFC3339Nano))
	object.Set("updatedAt", t.UpdatedAt.Format(time.RFC3339Nano))
	object.Set("version", json.Number(strconv.FormatUint(t.Version, 10)))
	mergeExtras(object, t.extra)
	return object
}

func (m *TeamMember) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", m.ID)
	object.Set("sessionId", m.SessionID)
	object.Set("agentKind", m.AgentKind)
	object.Set("name", m.Name)
	object.Set("role", m.Role)
	object.Set("enabled", m.Enabled)
	return object
}

func (p *TeamPolicy) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("maxParallelTasks", jsoncompat.Num(p.MaxParallelTasks))
	object.Set("minParallelTasks", jsoncompat.Num(p.MinParallelTasks))
	object.Set("maxProviderConcurrency", jsoncompat.Num(p.MaxProviderConcurrency))
	object.Set("maxTaskRetries", jsoncompat.Num(p.MaxTaskRetries))
	object.Set("maxRounds", jsoncompat.Num(p.MaxRounds))
	object.Set("maxTasksPerRun", jsoncompat.Num(p.MaxTasksPerRun))
	object.Set("maxContextChars", jsoncompat.Num(p.MaxContextChars))
	object.Set("maxResultChars", jsoncompat.Num(p.MaxResultChars))
	object.Set("taskTimeoutSecs", jsoncompat.Num(p.TaskTimeoutSecs))
	object.Set("runTimeoutSecs", jsoncompat.Num(p.RunTimeoutSecs))
	object.Set("budgetUnits", jsoncompat.Num(p.BudgetUnits))
	object.Set("workspaceMode", p.WorkspaceMode)
	object.Set("writePolicy", p.WritePolicy)
	object.Set("autoDispatch", p.AutoDispatch)
	object.Set("autoApplyChanges", p.AutoApplyChanges)
	return object
}

// Clone 深拷贝 Team（含未知字段），账本入库与出库都返回副本。
func (t *Team) Clone() *Team {
	if t == nil {
		return nil
	}
	copied := *t
	copied.Members = append([]TeamMember(nil), t.Members...)
	copied.extra = cloneExtra(t.extra)
	return &copied
}

// UnmarshalJSON 解码 Run 并保留未知字段。
func (r *Run) UnmarshalJSON(data []byte) error {
	type runAlias Run
	var decoded runAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, runKnownFields)
	if err != nil {
		return err
	}
	*r = Run(decoded)
	r.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (r *Run) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(r.jsonObject()), nil
}

func (r *Run) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", r.ID)
	object.Set("teamId", r.TeamID)
	object.Set("goal", r.Goal)
	if r.Mode != "" {
		object.Set("mode", r.Mode)
	}
	if r.CollectPolicy != "" {
		object.Set("collectPolicy", r.CollectPolicy)
	}
	// AutoPlan（§5.1/§10.2：plan 为空才允许 Lead 产生 team-actions）必须落盘：
	// 漏 Set 会让重启后解析闸门静默关闭（false 是保守方向，但显式声明的
	// Run 丢标记会让合法动作被当普通文本丢弃）。
	if r.AutoPlan {
		object.Set("autoPlan", true)
	}
	object.Set("status", r.Status)
	if r.StopReason != "" {
		object.Set("stopReason", r.StopReason)
	}
	object.Set("round", jsoncompat.Num(r.Round))
	object.Set("planHash", r.PlanHash)
	// ParentRunID 必须落盘（§6.4.1 amend 链）：漏 Set 或漏 runKnownFields 会让
	// 网关重启后修订血缘静默丢失——前端「基于此 Run 修订」的入口与缓存命中
	// 事实的溯源都依赖它。
	if r.ParentRunID != "" {
		object.Set("parentRunId", r.ParentRunID)
	}
	// ScheduledFrom（定时触发标记，§12 阶段四）必须落盘：漏 Set 或漏
	// runKnownFields 会让重启后定时 Run 的溯源静默丢失——回扫对账按
	// run_created payload.scheduledBy 裁决（journal 侧），快照侧的该字段
	// 是投影与审计的来源。
	if r.ScheduledFrom != "" {
		object.Set("scheduledFrom", r.ScheduledFrom)
	}
	// BudgetCeiling（预算追加上限，§9）必须落盘：漏 Set 或漏 runKnownFields
	// 会让重启后已追加的预算上限静默归零——passDispatch 的阈值
	// max(BudgetCeiling,BudgetUnits) 退化回策略值，下一次派发立刻重复停靠，
	// 用户「继续本轮」的决定丢失。0（从未追加）不带键，与 omitempty 一致。
	if r.BudgetCeiling > 0 {
		object.Set("budgetCeiling", jsoncompat.Num(r.BudgetCeiling))
	}
	tasks := &jsoncompat.Array{}
	for i := range r.Tasks {
		tasks.Items = append(tasks.Items, r.Tasks[i].jsonObject())
	}
	object.Set("tasks", tasks)
	if len(r.PendingActions) > 0 {
		actions := &jsoncompat.Array{}
		for i := range r.PendingActions {
			actions.Items = append(actions.Items, r.PendingActions[i].jsonObject())
		}
		object.Set("pendingActions", actions)
	}
	// pendingQuestions 必须落盘（§5.4 快照四处同步硬纪律）：漏 Set 或漏
	// runKnownFields 会让网关重启后问题登记静默清零——decision 按 Version
	// 比对更是无从谈起。
	if len(r.PendingQuestions) > 0 {
		questions := &jsoncompat.Array{}
		for i := range r.PendingQuestions {
			questions.Items = append(questions.Items, r.PendingQuestions[i].jsonObject())
		}
		object.Set("pendingQuestions", questions)
	}
	object.Set("createdAt", r.CreatedAt.Format(time.RFC3339Nano))
	object.Set("updatedAt", r.UpdatedAt.Format(time.RFC3339Nano))
	object.Set("version", json.Number(strconv.FormatUint(r.Version, 10)))
	// 闸门字段（§6.4.1）：verifyCommand 非空才落盘（未声明闸门的 Run 不带键）；
	// verify 手工枚举四键——漏 Set 会让闸门结果在重启后丢失，resume 复验退化。
	if len(r.VerifyCommand) > 0 {
		command := &jsoncompat.Array{}
		for _, part := range r.VerifyCommand {
			command.Items = append(command.Items, part)
		}
		object.Set("verifyCommand", command)
	}
	if r.Verify != nil {
		object.Set("verify", r.Verify.jsonObject())
	}
	mergeExtras(object, r.extra)
	return object
}

func (a *Action) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("type", a.Type)
	object.Set("sourceTaskId", a.SourceTaskID)
	object.Set("round", jsoncompat.Num(a.Round))
	if a.Member != "" {
		object.Set("member", a.Member)
	}
	if a.Task != "" {
		object.Set("task", a.Task)
	}
	if a.FromTaskID != "" {
		object.Set("fromTaskId", a.FromTaskID)
	}
	if a.ActionKey != "" {
		object.Set("actionKey", a.ActionKey)
	}
	if a.Status != "" {
		object.Set("status", a.Status)
	}
	if a.Reason != "" {
		object.Set("reason", a.Reason)
	}
	return object
}

func (q *PendingQuestion) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("questionId", q.QuestionID)
	object.Set("taskId", q.TaskID)
	object.Set("version", json.Number(strconv.FormatUint(q.Version, 10)))
	object.Set("kind", q.Kind)
	object.Set("summary", q.Summary)
	if len(q.Options) > 0 {
		options := &jsoncompat.Array{}
		for i := range q.Options {
			entry := jsoncompat.NewObject()
			entry.Set("id", q.Options[i].ID)
			entry.Set("label", q.Options[i].Label)
			options.Items = append(options.Items, entry)
		}
		object.Set("options", options)
	}
	object.Set("status", q.Status)
	object.Set("openedAt", q.OpenedAt.Format(time.RFC3339Nano))
	return object
}

// Clone 深拷贝 PendingQuestion（含选项）。
func (q *PendingQuestion) Clone() *PendingQuestion {
	if q == nil {
		return nil
	}
	copied := *q
	copied.Options = append([]QuestionOption(nil), q.Options...)
	return &copied
}

// Clone 深拷贝 Run（含任务、动作、问题登记、验证闸门与未知字段）。
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	copied := *r
	copied.Tasks = nil
	if len(r.Tasks) > 0 {
		copied.Tasks = make([]Task, len(r.Tasks))
		for i := range r.Tasks {
			copied.Tasks[i] = *r.Tasks[i].Clone()
		}
	}
	copied.PendingActions = append([]Action(nil), r.PendingActions...)
	if len(r.PendingQuestions) > 0 {
		copied.PendingQuestions = make([]PendingQuestion, len(r.PendingQuestions))
		for i := range r.PendingQuestions {
			copied.PendingQuestions[i] = *r.PendingQuestions[i].Clone()
		}
	}
	copied.VerifyCommand = append([]string(nil), r.VerifyCommand...)
	copied.Verify = r.Verify.Clone()
	copied.extra = cloneExtra(r.extra)
	return &copied
}

// UnmarshalJSON 解码 Task 并保留未知字段。
func (t *Task) UnmarshalJSON(data []byte) error {
	type taskAlias Task
	var decoded taskAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, taskKnownFields)
	if err != nil {
		return err
	}
	*t = Task(decoded)
	t.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (t *Task) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(t.jsonObject()), nil
}

func (t *Task) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", t.ID)
	object.Set("memberId", t.MemberID)
	object.Set("prompt", t.Prompt)
	object.Set("inputHash", t.InputHash)
	object.Set("attempt", jsoncompat.Num(t.Attempt))
	if t.SiteID != "" {
		object.Set("siteId", t.SiteID)
	}
	if t.Ordinal != 0 {
		object.Set("ordinal", jsoncompat.Num(t.Ordinal))
	}
	if len(t.DependsOn) > 0 {
		depends := &jsoncompat.Array{}
		for _, id := range t.DependsOn {
			depends.Items = append(depends.Items, id)
		}
		object.Set("dependsOn", depends)
	}
	if t.BroadcastGroup != "" {
		object.Set("broadcastGroup", t.BroadcastGroup)
	}
	if t.WorkspaceMode != "" {
		object.Set("workspaceMode", t.WorkspaceMode)
	}
	if t.WorkspaceID != "" {
		object.Set("workspaceId", t.WorkspaceID)
	}
	// BaseRevision（isolated worktree 首派基线，§6.4 规则 4）必须落盘：
	// 重启恢复的 workspace_lost 对账与 diff 数据面都依赖它，漏序列化会让
	// 基线校验静默失效（重启后误判工作区丢失）。
	if t.BaseRevision != "" {
		object.Set("baseRevision", t.BaseRevision)
	}
	if t.WriteLock != "" {
		object.Set("writeLock", t.WriteLock)
	}
	object.Set("status", t.Status)
	if t.SubmissionID != "" {
		object.Set("submissionId", t.SubmissionID)
	}
	if t.Result != "" {
		object.Set("result", t.Result)
	}
	if t.ResultDetail != nil {
		object.Set("resultDetail", t.ResultDetail.jsonObject())
	}
	// CachedFrom（amend 缓存导入来源，§6.4.1）必须落盘：重建与 detail 投影
	// 都按它区分「导入结果」与「本 Run 执行产出」，漏序列化会让命中任务在
	// 重启后退化为无来源的普通 completed。
	if t.CachedFrom != "" {
		object.Set("cachedFrom", t.CachedFrom)
	}
	if t.Error != "" {
		object.Set("error", t.Error)
	}
	if t.Outcome != "" {
		object.Set("outcome", t.Outcome)
	}
	if t.StopReason != "" {
		object.Set("stopReason", t.StopReason)
	}
	if t.InstructionsHead != "" {
		object.Set("instructionsHead", t.InstructionsHead)
	}
	// SentText（派发全文，遗留④）必须落盘：漏 Set 会让收养/超时/取消路径
	// 退回「按当前策略重算」，策略变更后在途任务的 turn 匹配即刻漂移。
	if t.SentText != "" {
		object.Set("sentText", t.SentText)
	}
	mergeExtras(object, t.extra)
	return object
}

// Clone 深拷贝 Task（含依赖、结果引用与未知字段）。
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	copied := *t
	copied.DependsOn = append([]string(nil), t.DependsOn...)
	copied.ResultDetail = t.ResultDetail.Clone()
	copied.extra = cloneExtra(t.extra)
	return &copied
}

// UnmarshalJSON 解码 TaskResult 并保留未知字段。
func (tr *TaskResult) UnmarshalJSON(data []byte) error {
	type taskResultAlias TaskResult
	var decoded taskResultAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, taskResultKnownFields)
	if err != nil {
		return err
	}
	*tr = TaskResult(decoded)
	tr.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (tr *TaskResult) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(tr.jsonObject()), nil
}

func (tr *TaskResult) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("sessionId", tr.SessionID)
	object.Set("turnId", tr.TurnID)
	object.Set("result", tr.Result)
	object.Set("resultChars", jsoncompat.Num(tr.ResultChars))
	object.Set("truncated", tr.Truncated)
	if tr.StopReason != "" {
		object.Set("stopReason", tr.StopReason)
	}
	if tr.FileSummary != "" {
		object.Set("fileSummary", tr.FileSummary)
	}
	if tr.CommandSummary != "" {
		object.Set("commandSummary", tr.CommandSummary)
	}
	mergeExtras(object, tr.extra)
	return object
}

// Clone 深拷贝 TaskResult（含未知字段）。
func (tr *TaskResult) Clone() *TaskResult {
	if tr == nil {
		return nil
	}
	copied := *tr
	copied.extra = cloneExtra(tr.extra)
	return &copied
}

// UnmarshalJSON 解码 VerifyResult 并保留未知字段。
func (v *VerifyResult) UnmarshalJSON(data []byte) error {
	type verifyAlias VerifyResult
	var decoded verifyAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, verifyResultKnownFields)
	if err != nil {
		return err
	}
	*v = VerifyResult(decoded)
	v.extra = extra
	return nil
}

// MarshalJSON 输出四键固定顺序（exitCode/outputHead/truncated/finishedAt），
// 未知字段原样回写在末尾。
func (v *VerifyResult) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(v.jsonObject()), nil
}

func (v *VerifyResult) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("exitCode", jsoncompat.Num(v.ExitCode))
	object.Set("outputHead", v.OutputHead)
	object.Set("truncated", v.Truncated)
	object.Set("finishedAt", v.FinishedAt.Format(time.RFC3339Nano))
	mergeExtras(object, v.extra)
	return object
}

// Clone 深拷贝 VerifyResult（含未知字段）。
func (v *VerifyResult) Clone() *VerifyResult {
	if v == nil {
		return nil
	}
	copied := *v
	copied.extra = cloneExtra(v.extra)
	return &copied
}

// UnmarshalJSON 按有序树解码 Event：数字保持字面量，payload 的键序与
// 数值精度无损进入 map。
func (e *Event) UnmarshalJSON(data []byte) error {
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
		return err
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		return errors.New("event 记录不是 JSON 对象")
	}
	for _, key := range object.Keys() {
		raw, _ := object.Get(key)
		switch key {
		case "id":
			e.ID, _ = raw.(string)
		case "runId":
			e.RunID, _ = raw.(string)
		case "type":
			e.Type, _ = raw.(string)
		case "source":
			e.Source, _ = raw.(string)
		case "payload":
			if raw == nil {
				e.Payload = nil
				continue
			}
			payload, ok := raw.(*jsoncompat.Object)
			if !ok {
				return errors.New("event 的 payload 不是 JSON 对象")
			}
			e.Payload = objectToMap(payload)
		case "idempotencyKey":
			e.IdempotencyKey, _ = raw.(string)
		case "sequence":
			number, ok := raw.(json.Number)
			if !ok {
				return errors.New("event 的 sequence 不是数字")
			}
			sequence, err := strconv.ParseUint(number.String(), 10, 64)
			if err != nil {
				return fmt.Errorf("event 的 sequence 非法: %w", err)
			}
			e.Sequence = sequence
		case "createdAt":
			text, ok := raw.(string)
			if !ok {
				return errors.New("event 的 createdAt 不是字符串")
			}
			createdAt, err := time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return fmt.Errorf("event 的 createdAt 非法: %w", err)
			}
			e.CreatedAt = createdAt
		}
	}
	return nil
}

// MarshalJSON 输出 §4.3 声明顺序的字段；payload 键排序保证字节级确定性。
func (e *Event) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(eventToObject(e)), nil
}

// eventToObject 把 Event 组装为有序 JSON 树（journal 行与事件流行共用）。
func eventToObject(event *Event) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", event.ID)
	object.Set("runId", event.RunID)
	object.Set("type", event.Type)
	object.Set("source", event.Source)
	object.Set("payload", payloadValue(event.Payload))
	if event.IdempotencyKey != "" {
		object.Set("idempotencyKey", event.IdempotencyKey)
	}
	object.Set("sequence", json.Number(strconv.FormatUint(event.Sequence, 10)))
	object.Set("createdAt", event.CreatedAt.Format(time.RFC3339Nano))
	return object
}

// payloadValue 把 map 载荷转为键排序的有序对象（nil 保持 null）。
func payloadValue(payload map[string]any) any {
	if payload == nil {
		return nil
	}
	object := jsoncompat.NewObject()
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		object.Set(key, goValue(payload[key]))
	}
	return object
}

// goValue 把任意 Go 值转为 jsoncompat 树：标准标量直通，数值统一成
// json.Number，容器递归；其余类型经 encoding/json 兜底，绝不 panic。
func goValue(value any) any {
	switch v := value.(type) {
	case nil, bool, string, json.Number, *jsoncompat.Object, *jsoncompat.Array:
		return v
	case int:
		return jsoncompat.Num(v)
	case int32:
		return json.Number(strconv.FormatInt(int64(v), 10))
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case uint64:
		return json.Number(strconv.FormatUint(v, 10))
	case float64:
		return json.Number(strconv.FormatFloat(v, 'g', -1, 64))
	case map[string]any:
		object := jsoncompat.NewObject()
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			object.Set(key, goValue(v[key]))
		}
		return object
	case []any:
		array := &jsoncompat.Array{}
		for _, item := range v {
			array.Items = append(array.Items, goValue(item))
		}
		return array
	case []string:
		array := &jsoncompat.Array{}
		for _, item := range v {
			array.Items = append(array.Items, item)
		}
		return array
	default:
		if raw, err := json.Marshal(v); err == nil {
			if parsed, parseErr := jsoncompat.Unmarshal(raw); parseErr == nil {
				return parsed
			}
		}
		return nil
	}
}

// objectToMap 把有序树转回 map（数字保持 json.Number 字面量）。
func objectToMap(object *jsoncompat.Object) map[string]any {
	out := make(map[string]any, object.Len())
	for _, key := range object.Keys() {
		value, _ := object.Get(key)
		out[key] = treeToGo(value)
	}
	return out
}

func treeToGo(value any) any {
	switch v := value.(type) {
	case *jsoncompat.Object:
		return objectToMap(v)
	case *jsoncompat.Array:
		items := make([]any, len(v.Items))
		for i, item := range v.Items {
			items[i] = treeToGo(item)
		}
		return items
	default:
		return v
	}
}

// collectExtras 提取文档中不属于 known 的键，保持出现顺序与原始值。
func collectExtras(data []byte, known map[string]bool) (*jsoncompat.Object, error) {
	value, err := jsoncompat.Unmarshal(data)
	if err != nil {
		return nil, err
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		return nil, errors.New("记录不是 JSON 对象")
	}
	var extras *jsoncompat.Object
	for _, key := range object.Keys() {
		if known[key] {
			continue
		}
		if extras == nil {
			extras = jsoncompat.NewObject()
		}
		raw, _ := object.Get(key)
		extras.Set(key, raw)
	}
	return extras, nil
}

func mergeExtras(object *jsoncompat.Object, extra *jsoncompat.Object) {
	if extra == nil {
		return
	}
	for _, key := range extra.Keys() {
		if value, ok := extra.Get(key); ok {
			object.Set(key, value)
		}
	}
}

func cloneExtra(extra *jsoncompat.Object) *jsoncompat.Object {
	if extra == nil {
		return nil
	}
	cloned, ok := jsoncompat.Clone(extra).(*jsoncompat.Object)
	if !ok {
		return nil
	}
	return cloned
}
