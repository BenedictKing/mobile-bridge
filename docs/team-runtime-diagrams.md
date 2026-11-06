# 团队编排：运行结构与状态转换图

> 适用范围：当前 Go 网关中的 Team 编排实现（2026-11-04）。图中的实线为已实现路径；规划中的 TeamInput 自动路由、自动 worktree 与成员供给不计入现行状态机。

本页的 Mermaid 图可直接在 GitHub 文档中阅读。[交互式运行时结构图](diagrams/team-runtime.html)提供缩放、查找与导出；[总体产品图](diagrams/mobile-bridge-team.html)描述用户视角，两者与本页的代码级图用途不同。

持久状态机覆盖 Run、Task、Lead Action、PendingQuestion 和 submissions；`VerifyResult`、`PendingTriggerID`、事件游标和 TeamInput 路由是流程/标记，不另造状态枚举。`RunPlanning` / `RunCollecting` 是兼容常量，当前执行流不写入。

## 图表索引

1. [运行时组件](#1-运行时组件)
2. [对象与事实归属](#2-对象与事实归属)
3. [创建与派发流程](#3-创建与派发流程)
4. [重启与运行中对账](#4-重启与运行中对账)
5. [Run 状态](#5-run-状态)
6. [Task 状态](#6-task-状态)
7. [Lead Action 状态](#7-lead-action-状态)
8. [成员问题状态](#8-成员问题状态)
9. [提交队列状态](#9-提交队列状态)
10. [验证闸门与工作区](#10-验证闸门与工作区)
11. [事件投影与页面同步](#11-事件投影与页面同步)
12. [当前 TeamInput 路由](#12-当前-teaminput-路由)
13. [定时触发与通知](#13-定时触发与通知)

## 1. 运行时组件

```mermaid
flowchart LR
    subgraph Browser[浏览器]
        UI[Team Chat 与 Run 页面]
    end
    subgraph Gateway[Go 网关]
        API[Team HTTP API]
        Feed[EventFeed 事件投影]
        Service[Team Service]
        Decision[Planner / Scheduler / Reducer / Recovery]
        Journal[(team-journal.jsonl)]
        Snapshot[(teams.json / team-runs.json)]
        Queue[QueueAdapter]
        Workspace[WorkspaceAdapter]
    end
    subgraph Agents[真实执行]
        Ledger[(submissions.json)]
        Runtime[acpruntime.Manager]
        Session[成员 Agent 会话]
    end
    UI -->|HTTP 请求| API --> Service
    Service --> Decision
    Service -->|先记运行事实| Journal
    Service -->|保存查询快照| Snapshot
    Journal -->|启动重建| Snapshot
    Journal --> Feed -->|changes / reset| API
    Service --> Queue --> Ledger --> Runtime --> Session
    Service --> Workspace
```

`internal/httpd/teams.go` 负责认证、路由和投影；`internal/team/runner.go` 控制运行；外部 I/O 经 `internal/team/ports.go` 的接口装配。成员会话的完整转录归 Agent runtime，Team 仅保存有界结果与引用。

## 2. 对象与事实归属

```mermaid
flowchart TB
    Team[Team：成员、Lead、Policy] -->|创建| Run[Run：目标、状态、版本]
    Run --> Task[Task：依赖、尝试、提交 ID]
    Run --> Action[PendingActions]
    Run --> Question[PendingQuestions]
    Run --> Thread[ExecutionThreads]
    Run --> Verify[VerifyResult]
    Team --> TeamsFile[(teams.json)]
    Run --> RunsFile[(team-runs.json 可重建快照)]
    Run --> Journal[(team-journal.jsonl 运行事实)]
    Journal -->|损坏或重启时重放| RunsFile
    Task --> Submission[(submissions.json 独立提交账本)]
    Thread --> Session[真实 Agent Session]
    Task -->|引用 workspaceId| Workspace[(team-workspaces.json 用户预备工作区)]
```

`teams.json` 保存 Team 配置；Run/Task/Action/Question/Thread 的事件事实写入 journal，`team-runs.json` 是可重建的查询快照。提交账本和工作区注册表由 `internal/httpd` 持有，不能把它们当作 Team journal 的一部分。依据：`internal/team/model.go`、`store.go`、`journal.go`、`recovery.go` 与 `cmd/gateway/main.go`。

## 3. 创建与派发流程

```mermaid
sequenceDiagram
    actor U as 用户
    participant W as Team Chat / Run 页
    participant H as Team HTTP API
    participant S as Team Service
    participant J as Team Journal
    participant Q as 提交队列
    participant A as 成员 Agent
    U->>W: 提交目标或显式 Plan
    W->>H: POST /api/teams/.../inputs 或 /runs
    H->>S: CreateConversationRun / CreateRun
    S->>J: run_created + task_planned
    S-->>H: Run 快照
    H-->>W: Run ID 与状态
    loop 调度到终局或停靠
        S->>S: 依赖、预算、写锁、并发与动作判定
        S->>J: task_dispatching + submissionId + inputHash
        S->>Q: Enqueue（同 ID 幂等）
        Q->>A: 会话空闲时发送
        A-->>S: watcher 读取回合与结果
        S->>J: task_running / 终态事件
    end
    J-->>W: changes / reset 事件投影
```

派发先记录事实，再调用队列；队列提交结果不明时不自动重发。完整顺序见 `internal/team/runner.go` 的 `loopPass` / `executeDispatch`、`internal/team/dispatcher.go` 与 `internal/httpd/queue.go`。

## 4. 重启与运行中对账

```mermaid
flowchart TD
    Start[启动 RecoverAll] --> Replay[重放 journal 并校正 Run 快照]
    Replay --> InFlight{Task 有在途派发事实?}
    InFlight -- 否 --> Keep[保留已证实终态或待派发任务]
    InFlight -- 是 --> Hash{inputHash 一致?}
    Hash -- 否 --> Mismatch[Task stopped；Run waiting_input]
    Hash -- 是 --> Probe{提交账本与会话探测}
    Probe -- 确认未入账 --> Ready[Task ready，可安全重派]
    Probe -- queued --> Queued[保持在途，交队列 worker]
    Probe -- accepted 且会话存活 --> Watch[Task running，重建 watcher]
    Probe -- failed / cancelled --> Settled[按确定事实结算]
    Probe -- unknown / ignored / 会话不可用 --> Unknown[Task outcome_unknown；Run waiting_input]
    Unknown --> Refresh[周期 RefreshRun 只读重探]
    Refresh --> Certain{账本有新的确定事实?}
    Certain -- failed / cancelled --> Correct[单向纠正 Task 终态]
    Certain -- 否 --> Unknown
```

`ReconcileRun` 是启动时按 journal 和提交账本进行的完整对账；`ReconcileUnknown` 只处理运行中已标为 `outcome_unknown` 的 Task，只有账本明确 `failed` / `cancelled` 才纠正，不会把“现在查不到”当成“从未发送”。依据：`internal/team/recovery.go`、`runner.go` 的 `RecoverRun` / `RefreshRun`。

## 5. Run 状态

```mermaid
stateDiagram-v2
    [*] --> created: CreateRun
    created --> dispatching: 调度循环启动
    dispatching --> reducing: 全任务成功且需要验证
    dispatching --> completed: 全任务成功且无验证闸门
    reducing --> completed: 验证通过
    reducing --> waiting_input: 验证失败
```

暂停与停靠恢复：

```mermaid
stateDiagram-v2
    created --> paused: Pause
    dispatching --> paused: Pause
    reducing --> paused: Pause
    dispatching --> waiting_input: 停靠 / 未决 / 预算
    reducing --> waiting_input: 验证失败
    paused --> waiting_input: 在途任务停靠或未决
    paused --> dispatching: Resume
    paused --> reducing: Resume 且 Verify 待判
    waiting_input --> dispatching: Resume 后仍可运行
```

终局折叠与显式取消：

```mermaid
stateDiagram-v2
    dispatching --> completed: 无待验证
    dispatching --> failed: 失败
    dispatching --> stopped: 停止
    dispatching --> outcome_unknown: 异常未决
```

```mermaid
stateDiagram-v2
    waiting_input --> completed: 已验证
    waiting_input --> failed: 失败
    waiting_input --> stopped: 停止
    waiting_input --> cancelled: 取消
```

```mermaid
flowchart TB
    Active[created / dispatching / reducing / paused / waiting_input] -->|Cancel| Cancelled[cancelled]
```

`paused` 和 `waiting_input` 都停止新派发，但在途 watcher 仍可结算；后者的停靠语义优先。图中的 `waiting_input` 终局折叠仅由用户 Resume 触发；全成功但验证未决时先回 `dispatching`，通过验证后才能 `completed`。`planning` / `collecting` 常量仍可读旧数据，当前 runner 不写入这两态。终态 Run 不可重新打开，后续工作创建新 Run / amend。依据：`internal/team/model.go`、`runner.go` 的 `passAdvance` / `passFold` / `Pause` / `Resume` / `Cancel`、`verify.go`。

| 当前状态 | 触发 | 下一状态 |
| --- | --- | --- |
| `created` | 调度 / Pause / Cancel | `dispatching` / `paused` / `cancelled` |
| `dispatching` | 任务折叠 / 验证待执行 / 停靠 / Pause / Cancel | 终态 / `reducing` / `waiting_input` / `paused` / `cancelled` |
| `reducing` | 验证通过 / 失败 / Pause / Cancel | `completed` / `waiting_input` / `paused` / `cancelled` |
| `paused` | Resume / 在途停靠 / Cancel | `dispatching` 或 `reducing` / `waiting_input` / `cancelled` |
| `waiting_input` | Resume / Cancel | `dispatching` 或已满足条件的终态 / `cancelled` |

## 6. Task 状态

```mermaid
stateDiagram-v2
    [*] --> planned: Plan / 派生 Task
    planned --> ready: Resume 释放依赖
    planned --> dispatching: 依赖释放并通过派发门控
    ready --> dispatching: 新 attempt 派发
    dispatching --> running: 已确认 Agent 回合
    dispatching --> completed: 回合已完成
    running --> completed: 回合已完成
    planned --> completed: amend 缓存命中
```

派发前终止：

```mermaid
flowchart TB
    Candidate[planned / ready] -->|预检失败| Failed[failed]
    Candidate -->|依赖阻断| Stopped[stopped]
    Candidate -->|取消 / 首胜| Cancelled[cancelled]
```

派发后的失败、停靠与取消：

```mermaid
flowchart TB
    InFlight[dispatching / running] -->|确定性失败| Failed[failed]
    InFlight -->|可恢复停靠| Stopped[stopped]
```

```mermaid
flowchart TB
    InFlight[dispatching / running] -->|未决| Unknown[outcome_unknown]
    InFlight -->|取消 / 首胜| Cancelled[cancelled]
```

重试与结果未知的裁决：

```mermaid
stateDiagram-v2
    dispatching --> ready: 确认未入账
    failed --> ready: 受限重试 / 改派
    stopped --> ready: Resume 重臂
```

```mermaid
stateDiagram-v2
    outcome_unknown --> failed: 确认失败
    outcome_unknown --> cancelled: 确认撤销
    outcome_unknown --> stopped: 人工停止
```

`ready` 主要来自恢复、受限重试或停靠重臂，正常新 Task 可从 `planned` 直接派发。`failed → ready` 与 `stopped → ready` 都必须增加 attempt 并生成新的 submission ID；`outcome_unknown` 不能自动回 `ready`。`task_stopped` 事件的 `payload.status` 区分 `cancelled` / `stopped`，旧事件按固定取消原因兼容。依据：`internal/team/scheduler.go`、`runner.go`、`reducer.go`、`recovery.go`。

恢复时 journal 是权威事实，损坏或滞后的快照可从任意旧状态校正到已记录的终态；这类快照修复由第 4 图描述，不视为正常执行迁移。

| 当前状态 | 触发 | 下一状态 |
| --- | --- | --- |
| `planned` / `ready` | 派发 / 缓存命中 / 预检失败 / 阻断 / 取消 | `dispatching` / `completed` / `failed` / `stopped` / `cancelled` |
| `dispatching` / `running` | 回合结果 / 失败 / 停靠 / 未决 / 取消 | `completed` / `failed` / `stopped` / `outcome_unknown` / `cancelled` |
| `failed` | 限流、暂态受限重试或 Lead 改派 | `ready`（新 attempt） |
| `stopped` | 用户继续且停靠允许重派 | `ready`（新 attempt） |
| `outcome_unknown` | 只读对账或用户确认不再重发 | `failed` / `cancelled` / `stopped` |

## 7. Lead Action 状态

```mermaid
stateDiagram-v2
    [*] --> proposed: Lead 动作解析并入账
    proposed --> executed: task_planned / task_forwarded 落账
    proposed --> rejected: 执行门控拒绝
    proposed --> proposed: 可重试 I/O 失败
    executed --> [*]
    rejected --> [*]
```

`delegate` / `summarize` 通过派生 Task 记录执行事实，`request_context` 通过转发事件记录。`autoDispatch` 关闭时 `proposed` 仅作预览，可随 Run 终局保留原状态；journal 已落执行事实而快照仍是 `proposed` 时按幂等键收敛，不重复执行。依据：`internal/team/actions.go`、`runner.go` 的 `passActions` / `convergeAction`。

## 8. 成员问题状态

```mermaid
stateDiagram-v2
    [*] --> open: question_opened
    open --> answered: 回答成功且 decision 入账
    open --> withdrawn: Task 终态 / Run 终局 / 会话卡过期
    open --> open: 无效答案或可重试失败
    answered --> [*]
    withdrawn --> [*]
```

每个 Task 最多登记 3 个 `open` 问题；回答按 `questionId + version` 消费一次。撤回事实为 `question_withdrawn`，原因分别为 `task_terminal`、`run_finished`、`stale`。问题状态属于 Run 的 `pendingQuestions`，不是 Task 自身的 `waiting_input` 状态。依据：`internal/team/questions.go`、`model.go`。

## 9. 提交队列状态

```mermaid
stateDiagram-v2
    [*] --> queued: Enqueue 持久化
    queued --> unknown: 开始一次派发
    queued --> cancelled: 用户撤回尚未发送的条目
    unknown --> accepted: Agent 确认接收
    unknown --> queued: 可证明未送达且重试次数未满
    unknown --> failed: 确定性拒绝或重试耗尽
    unknown --> ignored: 用户忽略未决条目
    failed --> queued: 用户显式重试
    failed --> cancelled: 用户删除失败条目
    accepted --> [*]
    cancelled --> [*]
    ignored --> [*]
```

这是 `submissions.json` 的**提交状态**，不是 Task 状态。`unknown → queued` 只发生在发送路径明确判定“未送达”的可安全重排情形，最多 10 次；调用已可能到达 Agent 时保持 `unknown`，Team 对应 Task 进入 `outcome_unknown` 并等待对账。依据：`internal/httpd/queue.go`、`internal/team/ports.go`、`recovery.go`。

## 10. 验证闸门与工作区

```mermaid
flowchart TD
    Candidate[Task 可派发] --> Mode{workspaceMode}
    Mode -- shared --> Lock{写锁要求}
    Lock -- exclusive --> ProjectLock[同项目写任务互斥]
    Lock -- none --> SharedRead[不占项目写锁]
    Mode -- isolated --> Registry[解析用户预备的 worktree]
    Registry --> Head{路径与 HEAD 基线有效?}
    Head -- 否，首派 --> PreflightFail[Task failed]
    Head -- 否，恢复收养 --> WorkspaceLost[Task stopped + Run waiting_input]
    Head -- 是 --> WorktreeLock[按 worktree 路径登记写锁]
    ProjectLock --> Dispatch[派发]
    SharedRead --> Dispatch
    WorktreeLock --> Dispatch
    Dispatch --> Fold{任务折叠为 completed?}
    Fold -- 否 --> OtherTerminal[按任务终态收口]
    Fold -- 是且无验证命令 --> Done[Run completed]
    Fold -- 是且有验证命令 --> Reducing[Run reducing：执行固定 argv]
    Reducing --> Verdict{退出码为 0?}
    Verdict -- 是 --> Done
    Verdict -- 否 --> Waiting[Run waiting_input]
    Waiting -->|用户 Resume：清除旧 Verify| DispatchAgain[Run dispatching]
    DispatchAgain --> Fold
```

工作区注册表只引用用户已准备的 worktree；网关不自动创建或合并。验证结果写入 `Run.Verify` 与 `run_verify` 事件，失败后允许用户修复再复验。依据：`internal/team/scheduler.go`、`runner.go`、`verify.go`、`internal/httpd/teams_workspace.go`。

## 11. 事件投影与页面同步

```mermaid
flowchart LR
    Journal[(全量 Journal)] --> Prime[启动时预热 EventFeed]
    Journal --> Add[运行中 Ingest 新事件]
    Prime --> Ring[每 Run 最近 500 条]
    Add --> Ring
    Client[Run 页面 cursor: epoch + sequence] --> Compare{epoch 与游标仍有效?}
    Ring --> Compare
    Compare -- 是 --> Changes[changes：只送新事件与 meta]
    Compare -- 否 --> Reset[reset：最近至多 20 条]
    Changes --> Project[就地投影 Task / Run 状态]
    Reset --> Detail[重读权威 RunDetail]
    Project --> Detail
    Detail --> Guard{页面代次、游标、meta 仍匹配?}
    Guard -- 是 --> Render[更新页面]
    Guard -- 否 --> Retry[丢弃旧响应并补读]
```

`sequence` 取全局 journal 序号，单 Run 内允许空洞；重启生成新 `epoch`，旧游标走 `reset`。页面关闭后代次改变，晚到读取不能重新订阅；详情补读也不能覆盖更新的事件或 meta。依据：`internal/team/projection.go`、`internal/httpd/teams.go`、`web/team.js`。

## 12. 当前 TeamInput 路由

```mermaid
flowchart TD
    Chat[Lead 会话发送 teamMode=team] --> Validate[校验目标、附件与 inputId]
    Input[POST /api/teams/:id/inputs] --> Validate
    Validate --> Existing{同 inputId 的 Run 或任务已存在?}
    Existing -- 同内容 --> Duplicate[返回原 Run + duplicate]
    Existing -- 不同内容 --> Conflict[拒绝复用 ID]
    Existing -- 否 --> Active{恰有一个活跃 autoPlan Run?}
    Active -- 是 --> Append[追加 Lead Task 到原 Run]
    Active -- 多个 --> Conflict
    Active -- 无且有终态 --> Amend[从最近终态 Run 创建 parentRunId 修订]
    Active -- 无任何 Run --> Create[创建初始 single + autoPlan Run]
    Append --> Route[routeStatus = new_task]
    Amend --> Route
    Create --> Route
    Direct[teamMode=direct] --> Normal[普通会话发送链路]
    Plan[POST /api/teams/:id/runs] --> Explicit[显式 Plan Run]
```

`inputId` 是消息幂等键；后续消息的 `runId` 可以是被复用的原 Run，任务 ID 由 `inputId` 确定性派生，尚无独立 TeamInput 状态账本。活跃 Run 追加新任务；终态 Run 不重开，自动形成 amend 血缘。结构化 `follow_up`、`amend`、`answer`、`clarify`、`steer` 仍是预留路由词汇，当前普通消息统一记为 `new_task`。依据：`internal/httpd/teams.go` 的 `createTeamInput`、`handleTeamConversationSend`。

## 13. 定时触发与通知

```mermaid
flowchart LR
    Schedule[(team-schedules.json)] --> Due{启用且到期?}
    Due -- 是 --> Pending[先保存 PendingTriggerID]
    Pending --> Create[以 scheduledFrom 创建新 Run]
    Create --> Journal[(run_created.scheduledBy)]
    Journal --> Clear[清 PendingTriggerID；记 LastRun]
    Pending -->|重启或提交失败| Reconcile[按 triggerID 回扫 journal]
    Reconcile -->|已有 run_created| Clear
    Reconcile -->|未创建且仍可重试| Create
    Journal --> Status[Run 终态 / waiting_input 事件]
    Status --> Notify[(Run 通知账本)]
    Notify --> Worker[后台送达与重试 worker]
    Worker --> Phone[手机通知渠道]
```

定时触发以 `PendingTriggerID` 和 `run_created.scheduledBy` 对账，避免重启后重复建 Run。通知在状态事实落账后记录并由独立 worker 送达，通知失败不回滚 Run；`run_stalled` 只提醒，不停止运行。依据：`internal/team/schedule.go`、`notify.go`、`internal/notifications/runledger.go`、`cmd/gateway/main.go`。
