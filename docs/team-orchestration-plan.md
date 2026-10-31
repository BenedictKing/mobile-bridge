# 多 Agent 自动编排统一方案

> 状态：设计中 · 起草日期：2026-10-23 · 整合 ZCode Dynamic Workflow 研究：2026-10-24
>
> 目标：在 mobile-bridge 网关内提供可恢复、可审计、受限的多 Agent 自动协作能力，统一调度 Codex、Claude Code、Kimi、ZCode、DeepSeek、OpenCode 等真实会话；吸收 Dynamic Workflow 的可重放、结构化结果和自适应并发经验，但保留 mobile-bridge 的 JSON 账本、移动端确认和真实会话边界。

## 1. 背景与目标

当前 mobile-bridge 已能管理多个 Agent 会话、展示部分原生子代理活动、发送消息、排队、迁移上下文并接收完成通知，但这些能力仍以“单会话控制”为中心。用户需要自行复制上下文，在多个聊天窗口之间切换，并手动决定下一步交给哪个 Agent。

本方案增加网关侧 Team Orchestrator。一个 Team 由多个已有会话组成，编排器负责任务拆分、分派、结果收集、上下文转发和有限次数的后续动作。Agent 本身不需要安装插件，也不需要理解 chief、lead 或 DS Bot 身份。

目标是支持以下闭环：

```text
用户提交目标
  → 编排器创建运行实例
  → Lead 生成结构化计划
  → 编排器分派子任务
  → 成员并行执行
  → 编排器收集结果
  → Lead 汇总或请求下一轮
  → 用户查看结果并决定是否应用改动
```

### 1.1 设计目标

1. 支持显式指派、并行分派、结果汇总和有限自动接力。
2. 复用现有真实会话、队列、权限、通知和会话投影。
3. 网关重启后可以恢复 Team 和 Run 的状态，不重复发送已确认任务。
4. 每个自动动作都有来源、原因、幂等键和审计记录。
5. 能明确区分成功、失败、取消和结果未知。
6. 通过预算、并发、轮次和上下文长度限制控制成本。
7. 以持久 journal 作为 Run 的唯一事实来源，恢复通过重放和外部对账完成。
8. 将计划错误、外部中断和结果未知分开建模，避免用同一个 retry 分支处理不同故障。
9. 结果随任务完成立即入账；Run 中止或失败时仍交付已经产生的有界结果。

### 1.2 非目标

1. 不实现新的 Agent runtime。
2. 不替代 Codex、Claude Code 或 dsh 自己的原生子代理系统。
3. 不引入 chief/lead 的隐式身份注入，不修改 Agent 的系统提示词；autoPlan Lead 只在具体任务的用户消息中携带可见的编排简报。
4. 不做无限群聊或自然语言自由接力。
5. 不在第一阶段引入第三方协调服务或 WebRTC 信令服务。
6. 不自动合并代码、自动执行高风险命令或自动批准权限请求。
7. 不在第一阶段复制 ZCode 的 TypeScript 编排语言、VM 沙箱、运行时 tracing 或 holes。

## 2. 现状与复用边界

### 2.1 已有能力

| 现有模块 | 复用方式 |
| --- | --- |
| `internal/acpruntime` | 发送、停止、状态读取、完成回调和真实会话生命周期 |
| `internal/httpd/queue.go` | 持久队列、去重键、结果未知和重启恢复语义 |
| `internal/subagents` | 展示 Agent 原生子代理活动；不作为 Team 成员管理接口 |
| `internal/store` | 会话发现和历史读取 |
| `internal/httpd/migrate.go` | 提取有界上下文并注入目标会话 |
| timeline changes/reset | 向手机端发送 Team 状态和运行进度 |
| 通知 reconciliation | Run 完成、失败和需要用户处理时通知 |
| Jev / permission route | 保留审批边界，自动编排不能绕过 Agent 权限 |

### 2.2 关键边界

Team 成员必须显式绑定到真实 `sessionID`。不能依赖扫描原生子代理目录，因为不同 Agent 的 detached child session 没有统一发现协议，且原生子代理通常由父 Agent 自己管理。

Team 账本只保存成员引用、运行状态和事件索引。真实会话历史仍由对应 Agent adapter 管理，Team 不复制完整 transcript，也不在删除 Team 时删除成员会话。

## 3. 总体架构

```text
HTTP / Web UI
      │
      ▼
Team API ─────────────── Team Store (.local/teams.json)
      │                            │
      ▼                            ▼
Team Orchestrator ─────── Run Store (.local/team-runs.json)
      │
      ├── Planner：解释结构化计划
      ├── Scheduler：并发、依赖、超时和预算
      ├── Dispatcher：调用 acpruntime.Manager
      ├── Context Builder：构造有界成员上下文
      ├── Reducer：收集和汇总结果
      └── Recovery：重启、重连和结果未知恢复
      │
      ▼
acpruntime.Manager → 真实 Agent Session A/B/C
```

建议新增包：

```text
internal/team/
  model.go       # Team、Run、Task、Event、Policy
  store.go       # 原子 JSON 账本读写
  planner.go     # 结构化计划解析和校验
  scheduler.go   # 可恢复任务调度
  dispatcher.go  # 对接 acpruntime.Manager
  context.go     # 成员间上下文提取与裁剪
  reducer.go     # 结果收集和汇总输入
  recovery.go    # 启动恢复和 outcome_unknown 对账
```

`internal/httpd` 只负责认证、参数校验、调用 Team 服务和返回投影，不直接实现调度循环。

### 3.1 统一执行原则

以下原则同时约束调度器、持久化和前端投影：

1. **Journal 是唯一事实来源。** 每个计划动作先写入事件和节点记录，再执行外部副作用；内存状态只是可丢弃的加速缓存。
2. **恢复等价于重放。** 同一计划、同一输入哈希和同一完成顺序才能复用已完成结果；输入发生漂移时返回 `input_hash_mismatch`，不静默套用旧结果。
3. **核心决策保持纯函数。** 调度、依赖释放、状态迁移和限流决策不直接读写会话或文件；真实 I/O 通过 Dispatcher、Store 和 Workspace 接口完成。
4. **外部副作用采用先记账后发送。** `dispatching`、`submissionID` 和幂等键必须在调用 Agent 前持久化，便于崩溃后对账。
5. **终局按停止原因分类。** 计划自身错误需要修订后重跑；用户、Provider、中断或结果未知属于可对账的外因；终局结算使用 first-wins。
6. **确认前生成完整预览。** Run 创建时计算阶段图、依赖和动作摘要；运行中只允许执行已验证的节点和动作。

这组原则是吸收 Dynamic Workflow 的运行语义，不要求引入其 TypeScript DSL。`await`、并行任务和有限循环仍由 Go 调度器和显式 JSON Plan 表达。

## 4. 核心对象

### 4.1 Team

```go
type Team struct {
    ID        string       `json:"id"`
    Name      string       `json:"name"`
    Members   []TeamMember `json:"members"`
    LeadID    string       `json:"leadId,omitempty"`
    Policy    TeamPolicy   `json:"policy"`
    CreatedAt time.Time    `json:"createdAt"`
    UpdatedAt time.Time    `json:"updatedAt"`
    Version   uint64       `json:"version"`
}

type TeamMember struct {
    ID        string `json:"id"`
    SessionID string `json:"sessionId"`
    AgentKind string `json:"agentKind"`
    Name      string `json:"name"`
    Role      string `json:"role"` // lead | member | reviewer
    Enabled   bool   `json:"enabled"`
}
```

成员引用失效时，Team 保留成员记录并标记 `unavailable`，避免静默改派到另一个会话。用户可以重新绑定或移除成员。

### 4.2 Policy

```go
type TeamPolicy struct {
    MaxParallelTasks  int `json:"maxParallelTasks"`
    MinParallelTasks  int `json:"minParallelTasks"`
    MaxProviderConcurrency int `json:"maxProviderConcurrency"`
    MaxRounds         int `json:"maxRounds"`
    MaxTasksPerRun    int `json:"maxTasksPerRun"`
    MaxContextChars   int `json:"maxContextChars"`
    MaxResultChars    int `json:"maxResultChars"`
    TaskTimeoutSecs   int `json:"taskTimeoutSecs"`
    RunTimeoutSecs    int `json:"runTimeoutSecs"`
    BudgetUnits       int `json:"budgetUnits"`
    WorkspaceMode     string `json:"workspaceMode"` // shared_workspace | isolated_worktree
    WritePolicy       string `json:"writePolicy"`   // exclusive | parallel
    AutoDispatch      bool `json:"autoDispatch"`
    AutoApplyChanges  bool `json:"autoApplyChanges"`
}
```

默认值建议：

```text
maxParallelTasks = 3
minParallelTasks = 1
maxProviderConcurrency = 5
maxRounds = 3
maxTasksPerRun = 12
maxContextChars = 12000
maxResultChars = 12000
taskTimeoutSecs = 1800
runTimeoutSecs = 7200
budgetUnits = 0  # 0 表示只统计，不主动暂停
workspaceMode = shared_workspace
writePolicy = exclusive
autoDispatch = false
autoApplyChanges = false
```

`autoDispatch` 是自动编排总开关，默认关闭。`autoApplyChanges` 即使打开也不能绕过 Agent 自身的权限和审批机制，第一阶段固定为关闭。预算只用于遥测和达到阈值后的 `waiting_input` 暂停，不把剩余额度注入 Agent 提示词，也不让编排脚本据此改变计划。

### 4.3 Run、Task 和 Event

```go
type Run struct {
    ID             string       `json:"id"`
    TeamID         string       `json:"teamId"`
    Goal           string       `json:"goal"`
    Status         string       `json:"status"`
    StopReason     string       `json:"stopReason,omitempty"`
    Round          int          `json:"round"`
    PlanHash       string       `json:"planHash"`
    Tasks          []Task       `json:"tasks"`
    PendingActions []Action     `json:"pendingActions,omitempty"`
    CreatedAt      time.Time    `json:"createdAt"`
    UpdatedAt      time.Time    `json:"updatedAt"`
    Version        uint64       `json:"version"`
}

type Task struct {
    ID             string   `json:"id"`
    MemberID       string   `json:"memberId"`
    Prompt         string   `json:"prompt"`
    InputHash      string   `json:"inputHash"`
    Attempt        int      `json:"attempt"`
    SiteID         string   `json:"siteId,omitempty"`
    Ordinal        int      `json:"ordinal,omitempty"`
    DependsOn      []string `json:"dependsOn,omitempty"`
    WorkspaceMode  string   `json:"workspaceMode,omitempty"`
    WorkspaceID    string   `json:"workspaceId,omitempty"`
    WriteLock      string   `json:"writeLock,omitempty"`
    Status         string   `json:"status"`
    SubmissionID   string   `json:"submissionId,omitempty"`
    Result         string   `json:"result,omitempty"`
    Error          string   `json:"error,omitempty"`
    Outcome        string   `json:"outcome,omitempty"`
    StopReason     string   `json:"stopReason,omitempty"`
    InstructionsHead string  `json:"instructionsHead,omitempty"`
}

type Event struct {
    ID         string         `json:"id"`
    RunID      string         `json:"runId"`
    Type       string         `json:"type"`
    Source     string         `json:"source"`
    Payload    map[string]any `json:"payload"`
    IdempotencyKey string     `json:"idempotencyKey,omitempty"`
    Sequence   uint64         `json:"sequence"`
    CreatedAt  time.Time      `json:"createdAt"`
}
```

任务状态：

```text
planned → ready → dispatching → running → completed
                                  ├──────→ failed
                                  ├──────→ cancelled
                                  ├──────→ outcome_unknown
                                  └──────→ stopped
```

`failed` 表示计划或任务自身的确定性错误，重放仍会失败，必须修订 Plan 或创建新 Attempt；`cancelled` / `stopped` 表示用户、Agent、Provider 或网关中断，可通过对账或恢复继续；`outcome_unknown` 表示外部副作用是否发生尚未确认，禁止自动重发。

Run 状态：

```text
created → planning → dispatching → collecting → reducing → completed
                                  ├────────────→ waiting_input
                                  ├────────────→ failed
                                  ├────────────→ cancelled
                                  ├────────────→ stopped
                                  └────────────→ outcome_unknown
```

Run 没有用户验收状态（2026-10-31 移除 `deliveryStatus`）。成员直接写工作区，“接受”不会让改动生效，“丢弃”也不会回滚文件，这样的验收只是一个标签。完成的 Run 只展示交付证据，用户认为有问题时在团队聊天继续反馈，或通过 amend 修订。旧快照中的 `deliveryStatus` 读入即丢弃，旧 journal 中的 `run_delivery_updated` 事件在重建时忽略。

## 5. 编排协议

### 5.1 为什么不能直接解析自然语言

如果编排器把 Agent 回复中的普通句子当作控制指令，任何一句“请让 reviewer 看一下”都可能触发新任务，导致循环、误派发和不可预测的费用增长。因此控制协议必须是严格标记包裹的结构化 JSON。

Lead 可以返回普通回答，也可以在回答中附加：

```text
<team-actions>
{"actions":[
  {"type":"delegate","member":"reviewer","task":"检查新增 API 的错误处理"},
  {"type":"delegate","member":"tester","task":"为该 API 补充回归测试"}
]}
</team-actions>
```

autoPlan Lead 的任务派发文本会在原始 goal 前附加 `[Team delegation]` 简报，列出启用成员的 ID、名称、Agent 类型、角色、动作字段、当前 `autoDispatch` 语义与轮次/任务/并发限制，使 Lead 不必猜测成员和协议。简报只用于 autoPlan 的 Lead 普通任务与派生任务；显式计划、普通成员、转发和修复任务不注入。最终顺序为 `[Team delegation]` → `[Team context]` → `[Assigned task]`/原始 Prompt，原始 goal 保留且只出现一次。

简报中的动作示例只展示 JSON 动作对象，不携带字面 `<team-actions>` 信封，避免 Agent 回显 prompt 时将示例误执行。若成员结果还需 Lead 汇总或继续规划，简报会要求首批动作同时给 Lead 自身安排一条 `delegate` 后续任务，而不是在该批动作中提前混入 `summarize`；后续 Lead 任务取得结果后再发起汇总。delegate 未显式携带 `fromTaskId` 时默认依赖 `sourceTaskId`，使 Worker 位于来源 Lead 的下一阶段；Lead 给自己委派的后续任务还会依赖此前已落账的非 Lead 工作任务，保证“先并行 Worker、再由 Lead 合并”的拓扑；显式 `fromTaskId` 仍优先。简报与依赖上下文共同进入派发期 `InputHash`；创建期哈希、PlanHash 和 amend 缓存规则不变。完整派发文本仍持久化为 `Task.SentText`，重启、收养、取消和 turn 匹配始终优先使用存量文本。

autoPlan 一旦存在已完成的非 Lead 任务，成功终局前由 Team Service 确定性补一条 `lead-auto-reduce` 收尾任务，不再依赖 Lead 自觉汇总。该任务依赖全部已完成的非收尾结果、`writeLock=none`、`reduction=true`，汇总目标、改动、验证和遗留问题；自动收尾结果不解析新 `team-actions`，但 Lead 主动 summarize 仍保留原有动作与修复能力。任务满额时落确定性失败，纯 Lead、显式计划及失败/取消路径不补收尾。`task_planned` 持久化 reduction、prompt 和 dependsOn，供 Journal 重建。

解析规则：

1. 标记必须成对出现，内容必须是单个 JSON 对象。
2. JSON schema、成员 ID、任务长度和动作数量都必须校验。
3. 解析失败或 schema 校验失败时，先记录 `invalid_action_block`，把结构化 violation 列表回送 Lead 当前会话进行定向修复；最多修复 3 次，仍失败才按普通文本处理并拒绝动作。
4. 只在 Team Policy 的 `autoDispatch=true` 时执行 `delegate`。
5. `stop`、`cancel`、`apply_changes` 等高影响动作第一阶段不允许由 Agent 自动产生。
6. 字符串结果先尝试一次宽容 `JSON.parse`，成功后仍必须经过同一 schema 校验。

### 5.2 支持的动作

第一版只支持三种动作：

| 动作 | 作用 | 默认权限 |
| --- | --- | --- |
| `delegate` | 给一个成员创建任务 | 需要 `autoDispatch` |
| `request_context` | 请求另一成员提供已有结果 | 需要 `autoDispatch` |
| `summarize` | 请求 Lead 汇总已有结果 | 可执行 |

动作必须带来源任务和轮次：

```json
{
  "type": "delegate",
  "sourceTaskId": "task-lead-1",
  "round": 1,
  "member": "reviewer",
  "task": "检查错误处理并给出建议",
  "writeLock": "none"
}
```

`delegate`/`summarize` 可带可选字段 `writeLock`（`exclusive`（默认）/`none`，取值与计划任务同词汇）：纯读任务（review/test 类）声明 `none` 后按 §6.4 规则 3 与共享工作区的写任务并行；`summarize` 派生的摘要任务缺省即 `none`（汇总已有结果不写工作区）。`request_context` 不创建任务，不接受该字段。

编排器拒绝以下情况：

- 目标成员不存在、禁用或绑定会话失效（会话不属于任何 ACP Agent：已删除/从未接入；归属明确但暂时打不开的会话不算失效——动作照常执行，由派发路径与会话恢复兜底重试）。
- 超过 Team 的最大轮次、任务数或并发数。
- 同一 Run 中产生相同的任务幂等键。
- 任务依赖未完成。
- 目标成员已经有相同 Run 的运行任务。
- 任务内容超过长度上限或包含无法识别的控制字段。

### 5.3 三种运行模式

用户创建 Run 时选择模式：

```text
single      只运行 Lead，允许 Lead 产生有限 delegate
broadcast   将同一目标分派给多个指定成员
pipeline    按依赖图逐阶段运行
```

`pipeline` 的第一版只支持 DAG，不支持循环依赖：

```text
分析 ─────┐
          ├── 实现 ── 测试 ── 汇总
审查 ─────┘
```

任何循环在创建 Run 时拒绝，不能等到运行过程中才发现。

### 5.4 成员主动提问

成员可以通过适配器的 `escalate` 能力登记需要主代理或用户处理的问题。每个 Task 最多保留 3 个未解决问题；问题包含 `questionID`、来源 Task、版本、问题摘要和取消状态，并在 Run 快照中暴露为 `pendingQuestions`。

- 问题登记后 Task 暂停，Run 进入 `waiting_input` 或继续执行其他独立分支。
- Lead 或用户通过 `POST .../decision` 按 `questionID + version` 回答；过期版本必须拒绝并要求刷新。
- Task 取消或 Run 终止时撤回未解决问题；恢复只允许消费一次答案。
- 问题答案原样作为适配器工具结果返回，完整会话仍保留在成员原始 session 中。

### 5.5 结构化结果与修复边界

每个可供下游消费的 Task 都声明结果 schema。适配器要求成员通过显式结果提交通道提交 JSON；服务端校验失败时返回字段路径、期望类型和实际值摘要，在同一会话内请求修复，最多 3 次，随后记录 `result_not_submitted` 并将 Task 标记为确定性失败。普通文本可以作为展示结果保存，但不能绕过下游 schema 或触发自动动作。

## 6. 调度和执行

### 6.1 调度器职责

Scheduler 维护 Run 的内存执行状态，但每次状态改变先写入持久账本，再触发外部发送。核心循环如下：

```text
读取 Run
  → 找到 status=ready 且依赖已完成的 Task
  → 按 Team 预算和 provider/model 桶取任务
  → 原子记录 dispatching + submissionID + inputHash
  → 调用 acpruntime.Manager
  → 根据确定结果或对账结果更新状态
  → 生成下一批 ready Task
```

外部发送之前必须先写入 `dispatching`，这样网关崩溃后可以根据 `submissionID` 查询或对账，而不是把任务误认为从未发送。

### 6.2 对接现有队列

Team Task 不直接绕过现有队列。调度器生成普通会话 submission：

```text
teamID + runID + taskID → submissionID
```

发送路径复用现有 `acpruntime.Manager` 和队列语义：

- 会话空闲时立即发送。
- 会话忙时进入已有队列。
- 发送结果未知时标记 `outcome_unknown`，等待对账。
- 不因为网络错误直接重复发送。
- 队列中的 Team Task 保留 Send now、Edit、Delete 等已有操作，但编辑后必须重新计算幂等键并记录审计事件。

### 6.3 并发控制

并发限制分三层，并由 AIMD 控制器动态调整 provider/model 桶：

1. Team 级：`maxParallelTasks`。
2. Run 级：同一 Run 同时运行的任务数不能超过 Team 限制。
3. Session 级：同一真实会话保持现有单活动任务语义，额外任务进入队列。
4. Provider/model 级：多个 Team 共享同一模型时共用一个自适应桶。

调度器使用带条件变量的 worker pool。每个 Task 有独立 context、超时和取消句柄；不能使用固定 sleep 轮询作为正确性条件。

AIMD 规则：

- 收到可识别的限流响应时，当前 epoch 只减小一次并按 `Retry-After` 冷却。
- 连续 4 次成功后并发窗口增加 1，保留 `lastGood` 用于探测恢复。
- 5 分钟无活动时恢复到 `minParallelTasks`，避免旧的限流状态永久影响新 Run。
- quota、auth、not_configured 归为 `stop`，只停止受影响 Run，不冻结共享桶；context 超限归为单 Task 可处理失败；网络瞬断和暂态 Provider 错误才进入受限重试。
- 20 分钟没有成功不自动停止 Run，只产生 stall 通知并等待用户或恢复事件。

### 6.4 工作区策略

Team 的工作区策略与 Agent 会话策略分开配置。worktree 负责隔离并行文件修改，不能替代任务之间的结果传递。

默认模式是 `shared_workspace` + `exclusive` 写锁：

```text
Lead：    shared_workspace / read
Developer：shared_workspace / write，独占项目写锁
Reviewer：shared_workspace / read，等待 Developer 完成
Tester：  shared_workspace / read，读取当前最新结果
```

只有以下情况使用 `isolated_worktree`：

- 两个任务需要同时修改同一个项目。
- 任务属于独立实验，可能破坏当前工作区。
- 安全审查、代码审查或测试审查需要隔离环境。
- 用户明确要求每个成员在独立分支中工作。

工作区模式：

```text
shared_workspace   使用项目当前工作区
isolated_worktree  从明确的 baseRevision 创建独立 worktree
```

调度规则：

1. `shared_workspace` 的写任务必须获取项目级写锁。
2. 同一项目最多一个共享工作区写任务运行。
3. 只读任务可以和共享工作区写任务并行，但读取到的是动态变化的工作区，UI 必须显示当前写入者。
4. `isolated_worktree` 任务可以并行运行，但必须记录 `workspaceID`、路径、baseRevision 和创建者。
5. Pipeline 默认沿用同一个共享工作区，后续任务直接读取前一任务结果。
6. 并行 worktree 的结果不能自动合并到主工作区；第一版只生成 diff 和审查结果，由用户决定是否应用。

Task 创建时可以覆盖 Team 默认值：

```json
{
  "id": "security-review",
  "memberId": "reviewer",
  "prompt": "检查认证边界",
  "workspaceMode": "isolated_worktree",
  "writeLock": "none"
}
```

worktree 生命周期必须可追踪：创建失败、路径丢失、基线变更、任务取消和清理失败都生成事件。第一版不让自动编排器执行 merge、rebase、cherry-pick、删除分支或删除 worktree；这些操作需要用户显式处理。

网关重启后：

- 共享工作区任务重新读取当前项目状态并进入对账。
- worktree 任务通过 `workspaceID` 和 baseRevision 恢复引用。
- 找不到 worktree 或发现基线不匹配时，任务进入 `waiting_input`。
- 不自动创建新的 worktree，也不自动把未提交改动复制到另一个 worktree。

### 6.4.1 确定性验证闸门与多轮缓存

Run 可以声明受支持的网关验证命令，例如 `make check`、指定 lint 或测试入口。命令名和固定参数在创建 Run 时写入 Plan 并展示给用户确认；Dispatcher 使用无 shell 的固定 argv 执行，非零退出码作为验证结果交给 Reducer，不把 Agent 的口头声明当作测试证据。

多轮接力采用有限的 amend 缓存：

1. 新 Run 记录 `parentRunID` 和修订后的 `planHash`；成员按稳定 ID 匹配，Task 按规范化输入的 `inputHash` 匹配。
2. 连续前缀中输入完全相同且没有依赖变化的已完成结果可以零 token 复用，并写入 `cache_hit` 事件。
3. 修订版第一次写入共享工作区或产生文件副作用时关闭依赖工作区状态的缓存；纯摘要任务仍需重新校验输入后才能复用。
4. 缓存命中不跳过权限、结果 schema、版本和事件写入；命中结果仍作为本 Run 的新节点实例入账。
5. 第一版不搬运部分 transcript，只通过 Context Builder 注入有界摘要。

### 6.5 结果收集

Task 完成后，Reducer 只提取有界结果：

- 最终 Agent 文本。
- 文件变更摘要。
- 命令执行摘要。
- 审批或用户输入需求。
- 失败原因和结果未知原因。

结果采用 salvage 语义：每个结果在产生时立即写入 journal 和 Run 摘要；Run 在第 N 个任务后失败、中止或被网关重启时，完成通知和重连快照仍必须包含前 N 个任务的有界结果、引用和截断标记。Reducer 不等待所有分支完成才提交结果，也不因为后续任务失败而删除已确认结果。

每条结果必须带 `taskID`、`sessionID`、`turnID`、`inputHash`、`resultChars` 和 `truncated`。完整 transcript 仍保留在原会话中，Team 只保存摘要和引用：

完整 transcript 仍保留在原会话中。Team 只保存摘要和引用：

```json
{
  "taskId": "task-reviewer-1",
  "sessionId": "session-reviewer",
  "turnId": "turn-7",
  "result": "发现两个错误处理问题。",
  "resultChars": 22
}
```

## 7. 上下文注入

### 7.1 显式来源和边界

成员收到的上下文必须标明来源、时间和截断信息：

```text
[Team context]
来源成员：reviewer
来源任务：task-reviewer-1
内容可能已截断，完整内容请打开原会话查看。

发现两个错误处理问题。

[Assigned task]
请修复上述问题，并说明修改了哪些文件。
```

默认只注入结果摘要，用户可以选择注入最近 N 条消息。禁止静默注入完整 transcript。

### 7.2 Context Builder 接口

```go
type ContextBuilder interface {
    BuildTaskContext(ctx context.Context, runID, taskID string) (string, error)
}
```

实现规则：

- 按 `MaxContextChars` 截断。
- 优先保留最新结果、错误、文件变更和用户明确引用的内容。
- 移除重复的 Team envelope，避免多轮接力指数增长。
- 记录实际注入长度和来源任务。
- 目标 Agent 不可读写 Team 账本，只能看到被注入的文本。

### 7.3 防止上下文爆炸

每轮只允许注入上一轮的摘要。若需要完整背景，必须由 Lead 重新汇总后生成新的摘要。编排器不把所有成员结果两两广播，否则 N 个成员会产生 O(N²) 的 token 消耗。

## 8. 持久化、幂等和恢复

### 8.1 文件布局

```text
.local/
  teams.json
  team-runs.json
  team-events.jsonl       # 对外增量投影
  team-journal.jsonl      # 运行事实来源，追加写入
```

第一版沿用项目现有 JSON 原子替换模式：写临时文件、同步、rename。`team-journal.jsonl` 追加记录每个 node、event、外部 submission 和终局结算；`team-runs.json` 是可重建的查询快照，不作为唯一事实来源。账本文件不存在时按空状态启动；未知字段保留以便未来迁移。每条 journal 记录带单调 `sequence`，重放时拒绝重复序号和破坏依赖顺序的事件。

### 8.2 幂等键

每个外部发送使用稳定幂等键：

```text
team:{teamID}:run:{runID}:task:{taskID}:attempt:{attempt}
```

幂等键之外，对规范化的成员、Prompt、依赖摘要、策略和上下文输入计算 `inputHash`。同一 `siteID + ordinal` 只能接受一个输入哈希；哈希不一致时产生 `input_hash_mismatch` 并进入 `waiting_input`，不能静默复用旧结果。并行分支的完成顺序也写入 journal，重放时按原顺序释放下游 ordinal，避免恢复后任务编号错位。

生成任务时先检查账本中是否已有相同 key：

- 已完成：返回已有结果。
- 已发送且状态未知：进入对账，不重复发送。
- 已取消：拒绝恢复，除非用户显式创建新 Attempt。
- 已有不同 `inputHash`：拒绝复用并要求创建修订后的新 Run。

### 8.3 网关重启恢复

启动顺序：

1. 读取 Team 和 Run 账本。
2. 校验版本、状态和引用完整性。
3. 将 `dispatching`、`running` 状态恢复为待对账状态，按 journal 重放已完成节点和完成顺序。
4. 通过 submissionID、会话历史和 Agent 当前状态查询结果。
5. 能确认未发送才回到 `ready`。
6. 无法确认时保持 `outcome_unknown`，等待用户处理。
7. 对确定性 `failed` 不重试；只能由用户修订 Plan 或创建 amend Run。

恢复期间不自动重放结果未知的 Task，避免重复修改文件或重复执行命令。

### 8.4 版本冲突

所有 Team 和 Run 更新都带 `version`。写入时检查期望版本，冲突则重新加载并重算。HTTP 请求、调度 worker 和恢复 worker 不能互相覆盖较新的状态。

## 9. 用户确认和安全边界

自动编排只负责调度，不能提升 Agent 权限。以下情况必须暂停 Run 并进入 `waiting_input`：

- Agent 请求权限或审批。
- Agent 请求用户回答问题。
- 任务需要修改 Team 成员或策略。
- 任务准备执行跨会话的高影响动作。
- Team 预算、轮次或并发额度耗尽。
- Task 结果未知且无法通过对账确认。
- `inputHash` 与已记录节点不一致，或恢复时发现完成顺序无法重放。
- 成员通过 `escalate` 提交的问题等待回答。

移动端显示：

- 哪个 Agent 触发了暂停。
- 原始会话和请求 ID。
- 需要用户做什么。
- 允许继续、拒绝、取消 Run 或打开原会话。

Team 自动动作不能调用现有 permission response API 代替用户确认。Jev 如果参与，也只能沿用现有低风险、可逆、默认关闭的边界，不能扩大到自动执行 Team 产生的高风险动作。

预算达到阈值时只暂停并请求用户选择“继续本轮、修订预算或停止 Run”；不把预算数字放入 Agent 上下文，也不由 Agent 自己决定提高预算。所有决策都写入 journal 并带 `decisionID`，重复提交返回 duplicate。

## 10. HTTP API

### 10.1 Team 管理

```http
GET    /api/teams
POST   /api/teams
GET    /api/teams/:teamID
PATCH  /api/teams/:teamID
DELETE /api/teams/:teamID

POST   /api/teams/:teamID/members
PATCH  /api/teams/:teamID/members/:memberID
DELETE /api/teams/:teamID/members/:memberID
```

创建 Team：

```json
{
  "id": "3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
  "name": "App 开发团队",
  "leadId": "lead",
  "members": [
    {"id":"lead","project":"acp:0123456789ab","agent":"kimi","role":"lead"},
    {"id":"reviewer","project":"acp:0123456789ab","agent":"kimi","role":"reviewer"}
  ],
  "policy": {"autoDispatch":true,"maxRounds":3}
}
```

`project` 使用 `GET /api/projects` 返回的项目键，`agent` 必须是网关已启用的非 Codex ACP Agent。网关在该项目目录中为每位成员创建专用会话后保存 `sessionId`；团队请求 `id` 与成员 `id` 共同派生会话幂等键，重放不会重复创建会话。旧客户端仍可直接提交已有 ACP 会话的 `sessionId`。

创建请求可同时携带顶层 `project`，并随 Team 账本持久化；未显式携带时从成员的项目键归一。项目型团队成员必须同属一个项目，后续新增或换绑会话也校验工作目录。旧的无项目 Team 账本继续按原规则读取。

项目默认团队通过 `PUT /api/projects/:projectKey/team` 幂等落实：首次打开时先通过 `GET /api/projects/:projectKey/agents` 让用户选择主控，确认后以 `{leadAgent}` 创建团队及项目历史 ACP Agent 的专用会话，取消则不创建；保存 `defaultForProject=true` 与稳定 `projectCwd`，重放复用已有团队且返回 `duplicate:true`。未携带 `leadAgent` 的旧客户端仍按候选首位创建，显式选择不在候选列表中的 Agent 返回 400。候选至少 3 个时默认分配主控、执行成员和末位评审；两个时为主控+执行成员，一个时仅主控，不为凑角色取消执行成员。默认团队查找同时接受项目键和 `projectCwd` 匹配，因此 `acp:` / `codex:` 前缀随项目去重策略变化时不会误创建或误显示待创建。项目列表中的虚拟默认行只在用户打开时调用 ensure，不因展示列表产生模型 turn。`#team=<teamId>` 深链不随主控会话变化。

成员添加接受 `{agent,role}` 并创建项目内专用会话，候选下拉只显示 Agent 类型而不混入会话标题。Team 以 `leadId` 与 `reviewerId` 分别持久化主控和默认评审；两者只需指向团队成员，允许同一成员同时担任主控和默认评审。PATCH 更换或移除默认评审在非终态 Run 存在时返回 409，移除该成员会清空失效的 `reviewerId`。旧 Lead 会话和历史 Run 保留。

团队主控聊天复用 Lead 会话的既有上传与发送契约：

```http
POST /api/sessions/:leadSessionID/uploads
POST /api/sessions/:leadSessionID/send
```

发送体在普通消息字段上增加可选 `teamId` 与 `teamMode`。`teamMode=team` 或缺省时，网关在 direct-send 记账前识别 Lead，将消息直接创建为 `single + autoPlan` Run，而不是先发送普通 Prompt 再补 Run；未携带 `teamId` 时仅自动识别唯一 Lead 团队，多团队歧义返回 409。`teamMode=direct` 明确跳过团队识别，走普通 Lead 会话直答且不创建 Run。请求 `id` 同时是 Run 幂等键，前端把 `teamId` 与 `teamMode` 都纳入消息身份。首条 Lead Task 持久化 `attachments`，附件随 submissions 进入既有 `PromptWithBlocks` 链路，journal 重建和任务重试不丢失；后续 delegate 任务不重复携带这些附件。纯图片消息使用明确的目标占位文案。团队执行响应为 `{status:"accepted",teamId,runId,duplicate?}`，直答沿用普通发送响应。

聊天页读取最近 Run 的任务投影，以阶段轨道、成员 pill 和结果摘要展示 workflow，并周期追随刷新；显式 pipeline、定时 Run、修订和高级控制仍走下述 Run API。团队输入区不支持 Skill、计划/目标模式和 steer，普通发送与完成后发送可用。

删除 Team 只删除 Team 账本和聚合事件。若仍有运行中的 Run，接口返回冲突；用户必须先取消 Run 或等待其结束。

### 10.2 Run 管理

```http
GET    /api/teams/:teamID/runs
POST   /api/teams/:teamID/runs
GET    /api/teams/:teamID/runs/:runID
POST   /api/teams/:teamID/runs/:runID/cancel
POST   /api/teams/:teamID/runs/:runID/resume
POST   /api/teams/:teamID/runs/:runID/amend
POST   /api/teams/:teamID/runs/:runID/delivery
POST   /api/teams/:teamID/runs/:runID/decision
GET    /api/teams/:teamID/runs/:runID/questions
GET    /api/teams/:teamID/runs/:runID/events
```

创建 Run：

```json
{
  "goal": "实现用户头像上传接口，并补齐测试",
  "mode": "pipeline",
  "workspaceMode": "shared_workspace",
  "writePolicy": "exclusive",
  "plan": [
    {"id":"design","memberId":"lead","prompt":"分析需求并拆分任务","writeLock":"none"},
    {"id":"implement","memberId":"developer","prompt":"实现接口","dependsOn":["design"],"writeLock":"exclusive"},
    {"id":"review","memberId":"reviewer","prompt":"检查实现并补测试","dependsOn":["implement"],"writeLock":"none"}
  ]
}
```

`plan` 为空时才允许 Lead 产生 `team-actions`。显式 Plan 优先级高于 Agent 自动计划，便于调试和复现。

`POST .../amend` 创建新的 Run，不修改旧 Run 的 journal；请求必须携带修订后的 Plan 或目标，服务端计算新的 `planHash`，并按 `inputHash` 导入允许复用的前驱结果。`resume` 只用于外因暂停和已回答问题，不得把确定性 `failed` 或 `outcome_unknown` 直接改成 `running`。

原 `POST .../delivery`（接受/丢弃）已移除，旧客户端调用返回 404。修订通过 amend 创建新 Run。

### 10.3 事件流

Team 事件沿用当前会话的 changes/reset 语义：

```http
GET /api/teams/:teamID/runs/:runID/events?incremental=1
```

事件至少包括：

```text
run_created
task_planned
task_dispatching
task_running
task_completed
task_failed
task_outcome_unknown
task_stopped
task_cache_hit
task_input_hash_mismatch
action_proposed
action_rejected
question_opened
question_resolved
run_waiting_input
run_completed
run_delivery_updated
run_failed
```

事件流只推送有界 payload。需要查看完整输出时，客户端跳转到成员的原始 session timeline。

## 11. 前端交互

第一版不新建复杂工作台，复用现有聊天和时间线组件：

1. Team 列表入口：创建、重命名、选择 Team。
2. 成员栏：显示 Agent 类型、连接状态、当前任务和额度状态。
3. Run 页面：显示阶段图、任务 DAG、每个成员的状态和结果摘要；循环阶段只显示一个稳定阶段节点和运行次数。
4. 每张任务卡显示 `instructionsHead`（Prompt 前 240 字符）、输入哈希状态、当前停止原因和原始会话入口。
5. 操作卡：继续、回答问题、拒绝、取消、创建 amend Run、打开原会话。
6. 结果卡：显示来源成员、来源任务、截断状态、验证命令退出码和转发按钮；Run 失败时仍展示 salvage 结果。
7. 设置页：自动编排总开关、最大轮次、最大并发、上下文上限和预算暂停策略。

默认交互：

```text
用户创建 Run
  → 预览完整阶段图、依赖和首批动作
  → 用户点击“开始自动编排”
  → 每次高影响动作前暂停
  → 用户可随时取消或打开原会话
```

即使 Team Policy 开启 `autoDispatch`，创建 Run 时也应允许用户预览 Lead 生成的第一批动作。第一版默认保留预览步骤，便于发现错误拆分和意外 token 消耗。

## 12. 分阶段实施

### 阶段一：Team 基础和显式编排

目标是完成可持久化的 Team、Run、Task 和事件模型，支持显式 Plan。

工作项：

1. 新增 `internal/team` 的模型、JSON 账本、journal 和版本化原子写入。
2. 新增 Team/Run HTTP 路由及认证、输入校验。
3. 对接 `acpruntime.Manager`，通过真实 `sessionID` 创建 submission。
4. 实现单任务、串行 pipeline 和取消。
5. 增加项目级 shared workspace 写锁和读写状态投影。
6. 增加 Team 事件投影和前端最小 Run 页面。
7. 完成重启恢复、输入哈希校验、三类终局原因和结果未知保护。
8. 为内存 Store、JSON Store 和 journal 实现同一份 contract 测试。

阶段一验收：

```text
创建 Team
→ 按项目空间创建两个成员专用会话
→ 创建 design → implement → review pipeline
→ 任务按依赖执行
→ 每个结果可在 Team 和原会话查看
→ Run 失败时已完成任务仍可在 completion 快照中查看
→ 网关重启后不重复发送任务
```

### 阶段二：并行分派和结果汇总

工作项：

1. 增加 `broadcast` 模式和 Team 级 worker pool。
2. 增加 `all` / `first_success` 收集策略。
3. 增加 Context Builder 和有界结果摘要。
4. 增加成员间显式转发。
5. 增加 `isolated_worktree` 任务引用、基线记录和 diff 展示。
6. 增加 provider/model 级 AIMD 限流、Retry-After 冷却和错误策略表。
7. 增加确定性 `make check`/lint 闸门，记录退出码和验证摘要。
8. 增加并发、超时、额度不足和成员离线处理。

阶段二验收：同一目标可以同时分派给两个 Agent，结果按来源合并，任一分支失败不会吞掉其他分支结果；独立 worktree 的 diff 可审查，但不会自动合并。

### 阶段三：Lead 自动动作

工作项：

1. 实现 `team-actions` envelope 和 JSON schema 校验。
2. 增加 violation 修复回路、宽容 JSON 解码和动作预览。
3. 实现 delegate、request_context、summarize 三种动作。
4. 增加轮次、任务数、并发数和重复动作限制。
5. 增加 `escalate` 问题注册、回答、撤回和 `pendingQuestions` 快照。
6. 增加循环依赖与动作拒绝事件。
7. 增加 amend Run、输入哈希匹配和首次工作区写入后的缓存失效。

阶段三验收：Lead 可以产生两个合法 delegate，编排器执行后把摘要交回 Lead；非法 JSON、未知成员和超预算动作都被拒绝并可解释。

### 阶段四：自动恢复和定时触发

工作项：

1. 将 Team Run 接入定时任务。
2. 增加启动恢复对账和运行中状态刷新。
3. 增加完成通知、失败通知和 `waiting_input` 通知。
4. 增加 Run 的暂停、恢复和 checkpoint。
5. 增加成本和耗时统计、stall 通知和通知 reconciliation。
6. 增加完整阶段图、`instructionsHead` 和断线恢复投影。

阶段四验收：定时 Run 在网关重启后能恢复可确认任务；结果未知任务不会自动重发；需要用户输入的 Run 能在手机上恢复。

## 13. 测试与验证

### 13.1 单元测试

| 区域 | 必测内容 |
| --- | --- |
| `team/store` | 空文件、坏文件、原子写入、版本冲突、未知字段；内存/JSON/journal contract 一致 |
| `team/journal` | sequence 单调性、重放、inputHash 漂移、并行完成顺序和 first-wins 终局 |
| `team/planner` | 合法 Plan、循环依赖、未知成员、超限任务、阶段图和 `instructionsHead` |
| `team/actions` | schema violation 修复回路、最多 3 次、宽容 JSON、动作幂等和拒绝原因 |
| `team/scheduler` | 依赖释放、并发上限、AIMD、Retry-After、取消、超时、重复调度 |
| `team/workspace` | 共享工作区写锁、读写并行、worktree 基线、路径丢失和清理失败 |
| `team/context` | 字符上限、来源标记、摘要优先、重复裁剪 |
| `team/recovery` | dispatching/running 对账、结果未知不重发、amend 缓存失效、salvage 快照 |
| `team/escalation` | 问题登记、版本化回答、撤回、上限和恢复幂等 |
| `team/verification` | 固定 argv、退出码分支、命令失败不依赖 Agent 口头声明 |
| `httpd` | 认证、路径校验、冲突状态、幂等请求 |
| `web` | Run 状态渲染、乱序事件、断线重连、取消和恢复 |

所有状态机测试使用 channel、barrier 或可控 clock 同步，不使用固定 sleep 判断正确性。

### 13.2 集成测试

使用现有 fake Agent/desktop fixture 验证：

1. 显式 pipeline 的完整执行。
2. 两个会话并行运行和独立完成。
3. Agent 返回审批请求时 Run 进入 `waiting_input`。
4. submission 结果未知时恢复后只对账、不重发。
5. Agent 会话失效时 Team 保留成员并显示 `unavailable`。
6. 共享工作区同时只有一个写任务，读任务能看到当前工作区状态。
7. 独立 worktree 任务记录基线和 diff，取消任务不会自动删除用户改动。
8. 重复 HTTP 请求只创建一个 Run 或一个 Task Attempt。
9. timeline changes/reset 能恢复 Team 视图。
10. Lead 返回非法动作时能在同一会话收到 violation 并修复；超过修复上限后动作被拒绝。
11. provider/model 限流只降低共享桶，quota/auth 错误只停止受影响 Run。
12. Run 在部分任务完成后失败时，完成通知和重连快照包含 salvage 结果。
13. amend Run 只复用 inputHash 相同的安全前缀，首次工作区写入后不复用依赖旧工作区的结果。
14. 成员提出问题时手机端能回答、取消或过期，重复回答只产生一个恢复动作。

### 13.3 真实运行验证

完成自动化测试后再验证真实网关：

1. 一个 Codex 会话作为 Lead，一个其他 Agent 作为 Reviewer。
2. 运行三阶段 pipeline，确认每阶段都能打开原始会话。
3. 在任务运行中重启网关，确认无重复 prompt。
4. 断开移动端连接后重新进入，确认 Run 状态和事件游标恢复。
5. 触发审批或用户问题，确认手机端暂停并能继续。
6. 达到最大轮次，确认 Run 可解释地停止。
7. 让验证闸门返回非零退出码，确认 Run 根据退出码暂停或失败，而不是采信 Agent 的成功声明。
8. 让 Run 在部分任务完成后中断，确认移动端仍显示已产生的 salvage 结果和截断信息。
9. 运行一次 amend，确认安全缓存命中、首次写入后缓存关闭且 `input_hash_mismatch` 可见。

建议在 `VERIFICATION.md` 增加 Team Orchestrator 验收记录，包括 Run ID、任务数、重启点和最终状态。

## 14. 兼容、发布和回滚

兼容策略：

- 新 Team 文件不存在时按空状态启动。
- `team-journal.jsonl` 不存在时从现有 Run/事件快照建立空 journal；旧版本只读取已知 JSON 文件，不删除新文件。
- 旧客户端访问 `/api/sessions` 的行为不变。
- Team API 独立于现有 Session API，旧客户端无需认识 Team。
- 不改变现有 submissions JSON 的字段含义；Team 只引用 submissionID。
- 新事件字段采用可选字段，旧客户端只显示通用状态。
- Team 自动编排默认关闭，升级后不会自动运行已有会话。

回滚策略：

1. 关闭 `autoDispatch`，保留已完成的 Team 和 Run 查询能力。
2. 停止 Team scheduler，已有 Session 和队列继续运行。
3. 若 Team API 出现问题，可只撤回 Team 路由注册，数据账本保留供恢复。
4. 可关闭 AIMD、amend 缓存和阶段图投影，回退到固定并发和基础结果摘要。
5. 不删除 Team 账本、不取消真实 Agent 会话、不清空 submissions 或 journal。

## 15. 验收标准

功能验收：

1. 用户能创建 Team、按项目空间创建成员（或绑定已有会话）、配置 Lead 和策略。
2. 显式 pipeline 能按依赖执行并展示每个成员结果。
3. broadcast 能并行执行，单个成员失败不会覆盖其他结果。
4. Team 默认使用共享工作区，并保证共享工作区写任务互斥。
5. Lead 只能通过合法 `team-actions` 触发自动动作。
6. 超过并发、轮次、任务数或上下文预算时 Run 会暂停或失败，并显示原因。
7. 审批、用户问题、worktree 丢失和结果未知会暂停 Run，不自动越权或重发。
8. 删除 Team 不删除真实会话或用户工作区。
9. 非法 `team-actions` 能收到有界 violation 修复机会；超过上限后动作被拒绝。
10. 阶段图、任务摘要、验证退出码、salvage 结果和 pending questions 都能在 Run 页面查看。

可靠性验收：

1. 网关重启后不重复发送已确认的 Task。
2. 相同请求重复提交不会创建重复 Run 或 Attempt。
3. 事件乱序、断线和 reset 后状态最终一致。
4. 失效成员不会被静默替换。
5. 所有自动动作都能追溯到 Run、Task、来源 Agent 和幂等键。
6. 重放时 inputHash 和并行完成顺序一致；不一致会显式进入 `input_hash_mismatch`。
7. amend 只复用可证明安全的输入前缀，首次工作区写入后不会复用依赖旧状态的结果。

性能和成本验收：

1. Team 默认最多同时运行 3 个任务。
2. 单个 Run 默认最多 12 个任务、3 轮、2 小时。
3. Team 聚合事件只保存摘要和引用，不能因 transcript 复制造成无界增长。
4. 前端增量更新只传输事件变化，不重复拉取全部成员历史。
5. AIMD 限流不会因为单个账号 quota/auth 错误冻结其他 Team 的共享 provider/model 桶。

## 16. 待决策事项

实现阶段开始前需要确定：

1. Team 是否允许跨项目绑定会话；默认建议允许，但 UI 明确显示项目路径。
2. `pipeline` 的 Lead 汇总是否必须使用固定 Lead，还是允许用户指定结果汇总成员。
3. 是否允许一个真实会话同时属于多个 Team；默认建议允许，但同一 Team 内仍保持单活动任务。
4. 是否把定时任务放在本方案第四阶段，还是先独立完成调度基础设施。
5. 是否在第一版支持文件变更冲突检查；默认建议先只展示冲突，不自动合并。
6. 是否允许 Team 自动创建 worktree；默认建议第一版只记录和使用用户准备好的 worktree，不自动创建或清理。
7. `team-journal.jsonl` 是否长期保留独立文件；默认建议保留，`team-runs.json` 只作为可重建快照。
8. 验证闸门允许哪些固定命令；默认建议使用仓库已登记的 `make check`、测试和 lint 入口，禁止任意 shell 字符串。
9. 预算达到阈值后的默认行为；默认建议进入 `waiting_input`，由用户决定继续、修订或停止。

建议默认决策为：允许跨项目绑定、Lead 固定但可更换、会话可属于多个 Team、串行 Pipeline 使用共享工作区、并行独立任务可使用 worktree、定时触发放到第四阶段、文件冲突只暂停并请求用户处理、第一版不自动创建或清理 worktree、使用独立 journal、固定验证命令和预算暂停策略。TS 编排 DSL、holes、部分 transcript 搬运和 chart/board 产物留到后续评估。
