# mobile-bridge 团队功能总体设计

> 状态：总体设计 · 2026-10-30
>
> 参考：JetBrains ThinkRail 的 IDE 原生协作体验、OpenBot 的持久化消息与执行线程模型，以及本仓库现有的 Team Orchestrator 方案。

## 1. 设计结论

团队功能应被设计成一个长期入口，而不是多个聊天窗口的聚合器。

用户只需要面对一个 Team Chat。网关负责把用户目标拆成可恢复的 Run 和 Task，选择成员、分配执行线程、控制工作区、收集证据，并在需要时请求用户决策。Agent 可以提出计划和动作，但不能直接改变 Team、会话、工作区或审批状态。

目标链路：

```text
Team Chat
  → TeamInput
  → Route / Plan
  → Run
  → Task
  → ExecutionThread
  → ProviderSession
  → Result / Review / Verify
  → Delivery
```

这条链路把三个容易混淆的概念分开：

| 概念 | 作用 | 生命周期 |
| --- | --- | --- |
| Team Member | 稳定的成员身份、能力和策略引用 | 长期 |
| Execution Thread | 某个 Run 中成员的一次独立工作上下文 | 随 Run 创建和结束 |
| Provider Session | Codex、Kimi、ZCode、Claude 等底层会话 | 由 Agent 适配器管理 |

当前 `TeamMember.SessionID` 只能表达第三层，后续应增加 `ExecutionThread` 投影，避免团队任务继续污染成员的私人聊天上下文。

## 2. 两个参考项目的取舍

ThinkRail 值得借鉴的是 IDE 内的上下文连续性：项目、文件、任务和 Agent 活动处在同一个工作空间里，用户可以随时介入并查看每个 Agent 的实际产出。它适合指导产品交互和项目边界。

OpenBot 值得借鉴的是运行时边界：持久化 mailbox、独立执行线程、确定性路由、有限上下文和可恢复调度。它适合指导网关的数据模型和恢复语义。

两者都不应直接复制：团队不需要新增一套 Agent runtime，也不应把 Agent 目录误当成 git 隔离。文件隔离仍由 shared workspace、写锁和 worktree 负责。

## 3. 产品模型

### 3.1 Team 是长期入口

Team 绑定一个项目和一组成员。成员可以是用户手工绑定的现有会话，也可以在后续阶段由网关供给真实会话。Team 删除只删除聚合关系和事件，不删除成员原始会话。

Team 的稳定字段：

```text
teamId, name, project, projectCwd
members, leadMemberId, reviewerMemberId
policy, version, createdAt, updatedAt
```

成员建议扩展为：

```text
memberId, displayName, role, agentKind, capabilities
providerBinding, defaultWorkspaceId, enabled, availability
origin, provisionId
```

`role` 只描述协作职责，不能代替能力和权限判断。`reviewer` 不代表它天然可以运行测试或修改文件。

### 3.2 Run 是不可变的执行实例

每个用户目标、修订或独立反馈都产生一个 Run。终态 Run 不重开；后续意见通过 `parentRunId` 创建新的 Run，并按输入哈希和工作区证据选择可复用结果。

Run 保存任务图、策略快照、计划哈希、输入序号、结果摘要和事件引用。完整 transcript 仍保留在 Provider Session 中，Team 只保存有界摘要和原始会话链接。

### 3.3 Task 是可调度的工作单元

Task 必须绑定成员身份和执行线程，而不是直接把 prompt 发给任意 session：

```text
taskId, memberId, executionThreadId
prompt, inputHash, dependsOn, attempt
workspaceId, workspaceMode, writeLock
status, submissionId, result, outcome, stopReason
```

Task 的完成条件由三类证据组成：Agent 结果、Reviewer verdict、网关验证结果。它们分别记录，不能用 Agent 的“完成了”替代测试或审查。

## 4. 运行时架构

```text
Web / Mobile
    │
    ▼
Team API ───── Team Store + Journal ───── Projection
    │
    ▼
Team Service
    ├── Input Router：处理持续反馈和来源血缘
    ├── Planner：解析显式 Plan 或 Lead 意图
    ├── Scheduler：依赖、并发、超时、预算
    ├── Thread Manager：分配 execution thread
    ├── Dispatcher：复用现有 submission/ACP runtime
    ├── Context Builder：有界上下文和结果摘要
    ├── Workspace Manager：写锁、worktree、diff
    ├── Reducer：结果合流和交付条件
    └── Recovery：重放、对账、结果未知处理
    │
    ▼
真实 Agent Session / Provider
```

职责边界：

1. HTTP 层只做认证、校验、投影和命令转发。
2. Team Service 决定是否允许动作，并在 journal 记账后调用外部副作用。
3. Agent 只提交结构化意图和结果，不直接写 Team journal。
4. Provider adapter 负责真实 session 生命周期、流事件、审批和恢复。
5. Workspace Manager 负责 git 和目录，不把 git 操作暴露成 Agent 自由命令。

## 5. 核心执行流程

```text
用户发送 TeamInput
  → input_accepted
  → 路由：steer / follow_up / new_task / amend / answer / clarify
  → 生成并校验 Plan
  → 预览首批 Task 和资源
  → 用户确认或策略允许自动执行
  → 为 Task 分配 ExecutionThread
  → journal 写 dispatching + submissionId + inputHash
  → 通过现有队列发送到 Provider Session
  → 结果入账并释放依赖
  → 需要时执行 Review / Verify
  → 形成 Delivery 投影
```

发送前必须先写入 `dispatching`。调用超时不能直接重发；先用 `submissionId`、会话状态和历史对账。无法确认外部副作用时使用 `outcome_unknown`，等待用户或恢复流程处理。

### 5.1 状态机

```text
Task: planned → ready → dispatching → running → completed
                                      ├→ failed
                                      ├→ cancelled
                                      ├→ stopped
                                      └→ outcome_unknown

Run: created → planning → awaiting_confirmation → dispatching
                                      → collecting → reviewing → verifying
                                      → completed
                                      ├→ waiting_input
                                      ├→ failed
                                      ├→ cancelled
                                      └→ stopped
```

`failed` 表示确定性错误，必须修订 Plan 或创建新 Attempt；`stopped` 和 `outcome_unknown` 表示外部中断或副作用不明，不能被通用 retry 静默处理。

## 6. 工作区和并发

默认策略为 `shared_workspace + exclusive write`：

- 分析、汇总、Review、Verify 使用只读锁。
- 同一项目共享工作区同时只允许一个写 Task。
- Pipeline 默认复用同一工作区，使后续 Task 能看到前序产出。
- 两个独立写 Task 只有在启用 `autoWorktree` 后才各自创建 worktree。
- worktree 产出的 diff、Review 和 Verify 进入 Delivery，不能自动 merge、rebase、cherry-pick 或删除。

`readPaths` 和 `writePaths` 可以帮助规划，但不能被视为安全证明。第一版采用仓库级保守冲突判定，避免测试、代码生成和格式化造成隐式全局写入。

每个工作区记录：

```text
workspaceId, path, mode, baseRevision, workspaceGroup
ownerTaskId, status, diffHash, cleanupStatus
```

用户未提交的主工作区改动不能被静默带入新 worktree；基线不明确时进入 `waiting_input`。

## 7. Agent 协作协议

Lead 只返回结构化意图，例如：

```json
{
  "actions": [
    {"type":"delegate","member":"developer","task":"实现接口","dependsOn":[]},
    {"type":"delegate","member":"reviewer","task":"检查方案","writeLock":"none"}
  ]
}
```

网关按以下顺序处理：解析、schema 校验、成员/策略/依赖校验、生成确定性 `actionKey`、journal 入账、执行。未知成员、重复动作、超限任务和循环依赖必须返回可解释的拒绝事件。

第一版只保留 `delegate`、`request_context` 和 `summarize`。`stop`、`apply_changes`、创建/删除会话、创建/删除 worktree 和提高预算都不能由 Agent 自动执行。

上下文只通过显式来源注入：

```text
[Team context]
sourceTaskId, resultVersion, workspaceId, truncated
[Assigned task]
```

不做全量 transcript 两两广播。上下文上限、轮次、任务数、并发数和预算都由 Team Policy 控制。

## 8. 权限和交付

协作模式、Provider 权限模式和 Team 自动化策略保持独立：

- `collaborationMode` 表示 Plan/Default 等工作方式。
- `permissionPolicy` 表示 Provider 的审批策略。
- `autoDispatch` 表示网关是否自动执行已验证动作。
- `autoApplyChanges` 第一版固定关闭。

成员遇到审批或用户问题时，Task 进入 `waiting_input`，手机端显示原请求、来源会话、影响范围和可选操作。所有回答使用现有审批/输入 API，并记录 `decisionId` 与版本；Team 不代替用户伪造 Provider 回应。

Delivery 是独立投影，至少包含：

```text
runId, taskIds, workspaceIds, diffHash
resultSummary, reviewVerdict, verifyExitCode
truncated, pendingQuestions, deliveryStatus
```

“Agent 已完成”“Review 已通过”“验证已通过”“用户已接受交付”必须分别展示。

## 9. 前端交互

首版保持现有聊天入口和 Run 页面，不新建复杂画布：

1. Team Chat：唯一主入口，支持普通目标、持续反馈和附件。
2. 成员栏：显示 Agent、连接状态、当前 Task、执行线程和 Provider 会话入口。
3. Run 摘要：显示阶段轨道、依赖、当前状态、结果和等待原因。
4. Task 卡：显示成员、Prompt 摘要、workspace、输入哈希、停止原因和原会话入口。
5. 操作卡：预览、开始、暂停、继续、回答问题、取消、修订和打开原会话。
6. Delivery 卡：显示 diff、Review、Verify 和“应用/清理”入口；应用与清理由用户触发。

用户不需要判断反馈属于哪个 Run。系统自动路由后必须明确展示“续作、追加任务、修订、仅回答或需要澄清”及理由。

## 10. API 方向

保留现有 Team/Run API，并补齐长期入口：

```http
GET    /api/teams
POST   /api/teams
PATCH  /api/teams/:teamId
GET    /api/teams/:teamId

POST   /api/teams/:teamId/inputs
GET    /api/teams/:teamId/inputs/:inputId
GET    /api/teams/:teamId/runs
POST   /api/teams/:teamId/runs
GET    /api/teams/:teamId/runs/:runId
POST   /api/teams/:teamId/runs/:runId/cancel
POST   /api/teams/:teamId/runs/:runId/resume
POST   /api/teams/:teamId/runs/:runId/amend
GET    /api/teams/:teamId/runs/:runId/events
```

`POST /inputs` 先返回 `202 {inputId, routeStatus}`。同一 `inputId` 同内容重放返回原结果，异内容拒绝。当前已落地的 Lead Chat `POST /api/sessions/:sessionId/send` 继续兼容，内部创建 `single + autoPlan` Run。

## 11. 实施顺序

### 阶段一：执行线程契约

定义 `memberId → executionThreadId → providerSessionId` 映射、线程状态、任务来源和旧事件防护。一个 `Run + Member` 默认生成一个执行线程；不支持并发 turn 的 Provider 继续串行。先完成模型、journal、投影和恢复测试。

### 阶段二：稳定的 Team Chat 和显式 Plan

把当前主控聊天入口统一成 TeamInput，支持单任务和串行 pipeline。保留预览和确认，复用现有队列、权限、通知、DAG、输入哈希和 `outcome_unknown` 对账。

### 阶段三：并行任务和交付证据

增加 broadcast、结果合流、Context Builder、只读 Review、固定 Verify 命令和 diff 投影。默认共享工作区串行写；只在明确需要时启用 worktree。

### 阶段四：持续反馈和有限自动接力

增加 `TeamInput` 路由、follow-up、amend、pending questions 和有限轮次修复。每次反馈按 journal 顺序路由，任务仍可并行。

### 阶段五：自动 worktree、审查和成员供给

最后再加入自动 worktree、独立 Reviewer、验证失败修复和动态成员供给。创建真实会话或 worktree 的结果未知必须对账，不能盲目重试。

## 12. 现有能力复用与暂缓项

直接复用：

- `internal/team` 的 JSON Store、journal、DAG、scheduler、recovery、context、verify 和 worktree。
- `acpruntime.Manager` 的真实 Provider 会话、队列、审批和生命周期。
- 现有 submissions 幂等、通知 reconciliation、`inputHash`、`outcome_unknown` 和前端 Team Run 投影。

暂缓：

- OpenBot 的完整频道/mailbox 外壳。
- ThinkRail 式复杂 IDE 画布和无限 Agent 群聊。
- 自动 merge、自动批准高风险权限、默认 Full access。
- 每条反馈机械创建成员或 worktree。
- 新增独立的 DSL、VM 或第三方协调服务。

## 13. 验收标准

完成第一阶段前必须证明：

1. 同一成员的私人聊天和团队 Task 使用不同执行线程，互不污染上下文。
2. 同一 Run 的旧 attempt、旧线程和迟到事件不能完成新 Task。
3. 网关重启后能恢复线程投影，不重复发送已确认 submission。
4. Provider 不支持并发时任务进入队列，并保持现有 Send now、Edit、Delete 语义。
5. Team 页面能从 Run 任务跳转原始 Provider 会话。
6. 结果未知、等待用户输入、Review 未通过和 Verify 未通过在 UI 中有不同状态。
7. 共享工作区同时只有一个写 Task；worktree 的 diff 不会自动进入主工作区。
8. 现有 Team、队列、权限和恢复测试保持通过。
