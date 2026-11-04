# mobile-bridge：项目级运行结构与关键状态

> 范围：当前 Go 网关及网页客户端（2026-11-04）。图示只描述代码中已装配的路径；不同 Agent、平台和入口的能力由运行时门控，图中的并列模块不表示每条会话都支持全部能力。

团队编排的 Run、Task、Action、成员问题及团队恢复细节单独见 [团队编排运行图册](team-runtime-diagrams.md)。本页聚焦网关共享控制面与其他有明确持久状态的子系统。

## 图表索引

1. [网关与外部执行器](#1-网关与外部执行器)
2. [会话读取和发送](#2-会话读取和发送)
3. [普通提交与实时同步](#3-普通提交与实时同步)
4. [Goal 状态与命令账本](#4-goal-状态与命令账本)
5. [Codex 账号切换](#5-codex-账号切换)
6. [ACP 账号池登录](#6-acp-账号池登录)
7. [公网连接状态](#7-公网连接状态)
8. [通知投递](#8-通知投递)
9. [审批与提问回应](#9-审批与提问回应)
10. [会话迁移](#10-会话迁移)

## 1. 网关与外部执行器

```mermaid
flowchart TB
    Phone[手机网页 / Android WebView] --> Access[LAN / HTTPS / SSH 入口]
    Access --> HTTP[单一 Go HTTP 网关]
    HTTP --> Codex[Codex Bridge]
    HTTP --> ACP[ACP Managers]
    HTTP --> Team[Team Service]
    Codex --> Desktop[原桌面 App IPC owner]
    ACP --> Agents[直连进程或 Agent Host 会话]
    Team --> TeamData[(Team journal 与快照)]
```

网关内部的共享控制面：

```mermaid
flowchart TB
    HTTP[Go HTTP 网关] --> Auth[认证 / Origin / CSRF]
    HTTP --> SessionData[(SQLite 只读发现与历史)]
    HTTP --> Ledgers[(提交 / 创建 / Goal 账本)]
```

```mermaid
flowchart TB
    HTTP[Go HTTP 网关] --> Pool[账号池与 Goal 控制]
    HTTP --> Notify[通知对账 worker]
    HTTP --> EventFeed[会话与 Run 事件投影]
    EventFeed --> Browser[手机网页]
```

单一 `cmd/gateway` 二进制装配 `internal/httpd`、Codex 原生 IPC、ACP runtime、Team、账号和通知服务。Quick Tunnel 与 SSH 反向转发只提供访问路径，不拥有会话执行；Agent 的原始历史留在各自的原生存储。依据：`cmd/gateway/main.go`、`internal/httpd/httpd.go`、`internal/codex`、`internal/acpruntime`、`internal/team`。

## 2. 会话读取和发送

```mermaid
flowchart TD
    Request[GET view / history 或 POST send] --> Router[HTTP 按会话归属路由]
    Router --> Owner{原生 owner 类型}
    Owner -- Codex --> Discover[SQLite 只读发现与历史]
    Discover --> IPC[桌面 App IPC 快照 / follower 操作]
    Owner -- ACP --> Manager[acpruntime.Manager]
    Manager --> Native[可选原生转录 / 元数据]
    Manager --> Process[session/list · load · prompt · cancel]
    IPC --> View[规范化会话投影]
    Native --> View
    Process --> View
    View --> Timeline[网页 timeline]
    Router -->|写入先登记提交 ID| Ledger[(submissions.json)]
    Ledger --> IPC
    Ledger --> Process
```

读请求不等于发起模型任务。Codex 写操作要求原桌面 owner，ACP 写操作走所属 Manager；发送结果不确定时保持未知记录，不能换传输链路自动重放。依据：`internal/httpd/sessions.go`、`queue.go`、`internal/codex`、`internal/acpruntime/manager.go`。

## 3. 普通提交与实时同步

普通消息的持久提交状态与团队 Task 使用同一套 `submissions.json` 词汇，状态图见 [提交队列状态](team-runtime-diagrams.md#9-提交队列状态)。队列的 `unknown → queued` 仅限明确未送达的尝试；可能已到达 Agent 的请求保持 `unknown`。

```mermaid
flowchart TD
    Auth[GET /api/auth] --> Mode{声明的传输方式}
    Mode -- sse --> SSE[EventSource：events?since=cursor]
    Mode -- poll --> Poll[长轮询：changes?after=sequence&epoch=...]
    SSE --> Frames[ready / changes / delta / reset]
    Poll --> Frames
    Frames --> Reset{epoch 或游标失效?}
    Reset -- 否 --> Delta[应用增量]
    Reset -- 是 --> Reload[读取完整快照]
    Delta --> View[更新 timeline 与状态]
    Reload --> View
    SSE -->|网络异常| Probe[重新探测认证]
    Poll -->|网络异常| Probe
    Probe -->|仍已登录| Backoff[封顶退避后续传]
    Backoff --> Mode
    Probe -->|明确失去认证| Logout[停止订阅并要求登录]
    Wake[页面回到前台 / online] -->|唤醒停滞连接| Mode
```

SSE 和长轮询共用事件游标与回调；只有明确的认证失效才退出登录。实时帧不是提交成功证明，写入结果以提交账本与原生会话状态为准。依据：`web/transport.js`、`web/connection.js`、`internal/httpd/changes.go`。

## 4. Goal 状态与命令账本

原生 Goal 状态由 Codex 或声明 `goals` 能力的 ACP Agent 持有；手机的操作记录另存于 `goal-commands.json`。两者不能混成一个状态机。

```mermaid
stateDiagram-v2
    [*] --> active: create
    active --> paused: pause
    blocked --> paused: pause
    usageLimited --> paused: pause
    budgetLimited --> paused: pause
    paused --> active: resume
    paused --> paused: edit 目标内容
    active --> complete: 原生运行完成
    active --> [*]: cancel / clear
    paused --> [*]: cancel / clear
    complete --> active: 新目标 create
```

```mermaid
stateDiagram-v2
    [*] --> sent: 写前持久化命令
    [*] --> confirmed: 原生状态已证明同一操作
    [*] --> failed: 输入或状态校验失败
    sent --> confirmed: 写后回读匹配
    sent --> failed: 确定性写入失败
    sent --> unknown: 发送或回读结果不明
    confirmed --> [*]
    failed --> [*]
    unknown --> [*]
```

`sent` 若在进程中断后遗留，对同一请求 ID 返回“结果未知”，不会重发写入。原生 Goal 的 `complete` / `blocked` 等由 provider 报告，Go 命令机只允许代码明确列出的转换。依据：`internal/httpd/goal_send.go`、`internal/goal/goal.go`。

命令账本启动时必须成功解析；首次外部写入前先持久占用请求 ID，写盘失败即拒绝操作。确认结果写盘失败会返回错误，但已持久的 `sent` 占用保留，后续按结果未知处理。

## 5. Codex 账号切换

```mermaid
stateDiagram-v2
    [*] --> idle
    idle --> preparing: Switch
    preparing --> stopping: 回滚快照已保存
    stopping --> applying: App 已退出
    applying --> starting: 目标已写入
    starting --> verifying: App 已重启
    verifying --> complete: 核验通过
```

失败与恢复路径：

```mermaid
flowchart TB
    Before[preparing / stopping] -->|尚未改写接入| Failed[failed]
    Changed[stopping / applying / starting / verifying] -->|需要回滚| Restoring[restoring]
    Restoring -->|原接入恢复| Restored[restored]
    Restoring -->|自动恢复失败| Interrupted[interrupted]
    Interrupted -->|用户显式恢复| Restoring
    Interrupted -->|回滚记录缺失| Failed
```

活动 phase 在进程重启后归一为 `interrupted`，阻止继续修改聊天；`complete/failed/restored/idle` 可发起新切换。此图是 Codex 桌面接入切换，不是 ACP 多账号池的槽位切换。依据：`internal/account/switch.go`、`account.go`。

## 6. ACP 账号池登录

```mermaid
stateDiagram-v2
    [*] --> pending: StartLogin
    pending --> done: 凭据持久化成功
    pending --> failed: 登录或持久化失败
    pending --> cancelled: 用户取消 / 同 kind 新登录取代
    done --> [*]
    failed --> [*]
    cancelled --> [*]
```

Kimi、ZCode、MiniMax、WorkBuddy 的登录驱动共享此状态机；同 kind 只保留一条在途登录。被取消的旧登录即使迟到完成也不能写入账号池。账号快照另有 `active/reauth_required/disabled/cooling_down` 可用性字段，不等同于登录会话状态。依据：`internal/accpool/login.go`、`pool.go`。

账号快照的常见转换是 `active → reauth_required`（刷新或鉴权失败）以及 `reauth_required → active`（重新登录或刷新成功）。`disabled` 可来自已有账号数据并阻止切换目标；`cooling_down` 是声明的状态值，当前 Go 路径没有为它写入自动迁移，不应把它当作已实现的冷却状态机。

## 7. 公网连接状态

```mermaid
stateDiagram-v2
    [*] --> connecting: Quick.Start
    connecting --> ready: cloudflared 确认连接
    connecting --> stopped: 进程退出 / Close
    ready --> stopped: 进程退出 / Close
```

```mermaid
stateDiagram-v2
    [*] --> connecting: SSHForward.Start
    connecting --> connected: 远端转发确认
    connecting --> retrying: 连接未建立即退出
    connected --> stopped: 进程退出 / Close
    retrying --> connecting: 外部再次调用 Start
    stopped --> connecting: 再次调用 Start
```

Quick 与 SSH 状态分别写入本地状态文件；状态文件不等于公网端点健康证明。`internal/tunnel` 本身没有在 `retrying` 内执行循环重连，是否重新调用 `Start` 由上层管理。局域网监听不依赖这些进程。依据：`internal/tunnel/quick.go`、`ssh.go`。

## 8. 通知投递

```mermaid
flowchart LR
    Fact[任务 / Run 的权威状态事实] --> Record[通知账本 Record 幂等登记]
    Record --> Pending[待送达行]
    Pending --> Worker[后台 Reconcile]
    Worker --> Result{通道投递成功?}
    Result -- 是 --> Delivered[记录 DeliveredAt 去重]
    Result -- 否 --> Retry[Attempts + NextAttempt 退避]
    Retry --> Worker
    Delivered --> Expire[保留期后清理痕迹]
    Pending --> Expire
```

通知发布失败不回滚业务状态。Run 的终态通知已送达后永久按类别去重，`waiting_input` / `stall` 可随同一 Run 后续事件重新登记；启动补发由水位与已送达痕迹限制范围。依据：`internal/notifications/runledger.go`、`outcomes.go`、`cmd/gateway/main.go`。

## 9. 审批与提问回应

```mermaid
flowchart TB
    Agent[ACP 反向权限或输入请求] --> Pending[Session 登记 pending requestId]
    Pending --> Jev{启用 Jev 自动审核?}
    Jev -->|满足低风险等门槛| Auto[自动回应一次或本次会话]
    Jev -->|未启用或未过门槛| Card[手机展示原生选项与上下文]
    Card --> Human[用户提交 optionId / 文本 / 取消]
    Human --> Validate{请求仍存在且回应格式有效?}
    Validate -->|否| Keep[拒绝，保留仍有效的卡片]
    Validate -->|是| Respond[Session.Respond 转成 ACP 线格式]
    Auto --> Respond
    Respond -->|明确送达| Agent
    Respond -->|写入结果未知| Unknown[保留卡片并禁用再次回应]
```

Codex 的回应走桌面 owner IPC，不经过 ACP 的 `Session.Respond`。ACP 写入结果未知时不会再提交回应或在清理阶段补发取消；卡片保留供核实，明确未发送的失败仍可重试。图中的“已提交”不应被解释为 Agent 已执行副作用。依据：`internal/httpd/sessions.go`、`internal/acpruntime/manager.go`、`internal/jev`。

## 10. 会话迁移

```mermaid
flowchart TB
    Source[读取源会话规范化视图] --> Target[选择目标 Agent]
    Target --> Scope[选择 main / full / latest 范围]
    Scope --> Claim[创建账本持久占用请求 ID]
    Claim --> Create[prepare：幂等创建空目标会话]
    Create --> Settings[选择并保存目标模型 / effort]
    Create -->|返回或关闭面板| Abandon[abandon：放弃预备会话]
    Settings --> Dispatch[持久占用 dispatching]
    Dispatch --> Send[派生提交 ID，注入转录]
    Send --> Result{注入已确认?}
    Result -->|是| Sent[返回新会话 sent=true]
    Result -->|发送前失败| Created[保留已建会话，允许重试]
    Result -->|发送后结果未知| Unknown[保留 dispatching，禁止自动重发]
```

源会话不变。`creations.json` 损坏或不可读时网关拒绝启动；创建占用写盘失败时不调用目标 Agent。目标会话已创建但 ID 写盘失败时保留无 ID 占用并拒绝重建。注入前失败可沿用请求 ID 重试；注入开始后的结果未知须在目标会话核实，不能自动重发。迁移与压缩的目标选择及预备状态见 `internal/httpd/migrate.go`、`creations.go` 和 `web/migrate.js`。
