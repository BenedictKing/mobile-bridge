# T3 Code 控制 Codex：消息同步、对话锁与网关约定

> 状态：源码研究，未实施 · 日期：2026-11-21
>
> 对象：[pingdotgg/t3code](https://github.com/pingdotgg/t3code)，本地 `refs/t3code`，commit [`6ba3de70aff98460e573ffc57773e45e148026ba`](https://github.com/pingdotgg/t3code/commit/6ba3de70aff98460e573ffc57773e45e148026ba)（2026-11-11）。下文源码路径均相对 `refs/t3code/`；行号以该 commit 为准。结论是静态源码核验结果，未启动真实 Codex。

## 结论摘要

1. **T3 Code 不控制 Codex 桌面 App**；它在拥有工作区的 server 上启动 `codex app-server` 子进程，以 stdio 上的 JSON-RPC/JSONL 调用 `thread/start`、`thread/resume`、`turn/start`、`turn/steer`、`turn/interrupt` 等原生方法。
2. **外部控制协议与 Codex 原生协议是两层**。Web/Desktop/Mobile 走认证 WebSocket 上的 Effect RPC；server 把客户端命令归一化为编排命令，再由 Codex adapter 翻译成 app-server RPC。前端不直接接触 Codex request id、native thread id 或审批回调。
3. **命令 ACK 不是执行完成**。`orchestration.dispatchCommand` 只返回提交 `sequence`；它表示事件、投影、receipt 和 outbox effect 已在同一 SQLite 事务落盘。Codex 启动、运行、审批与完成都通过后续事件通知客户端。
4. **不存在一个叫 `conversationLock` 的总锁**。真正的互斥来自每线程 `ThreadCommandExecutor`、消息专用 `messageIntake`、provider session 打开/附着锁、outbox 的同线程领取谓词，以及 Codex adapter 的初始化和 turn 终态信号量。
5. **消息不重复靠稳定 `commandId` + 服务端 receipt**，不是靠“socket 没断”。同一命令重放返回原 receipt 和仍保留的历史事件；被拒绝的命令也保存 receipt，重试不会换一套判定重新执行。
6. **消息不丢失、不乱序靠全局 `sequence` + 先订阅后回放 + 快照降级**。live 与历史重叠部分由客户端按 `sequence <= cursor` 去重；回放超过 128 条或 1 MiB、历史已裁剪或游标越界时改发 snapshot。
7. **运行中追加消息不是简单加锁等待**。Codex 声明支持 active steering，策略层优先产生 `turn/steer {expectedTurnId}`；turn 尚未运行、Stop 已到达或能力不满足时，消息排队或走 interrupt+restart。
8. **移动端另有持久 outbox**。它保存相同 `messageId/commandId`，单条在途投递、指数退避、收到 ACK 后删除；共享发送路径在 socket 丢失时也最多原样重发 2 次。移动 outbox 自己重试时关闭内层重发，避免两套重试叠加。

## 1. 控制面分层：两套 RPC，不要混成一个协议

```text
Web / Desktop renderer / Mobile
        │  认证 WebSocket + Effect RPC
        │  orchestration.dispatchCommand / subscribeThread / subscribeShell
        ▼
T3 server：CommandPolicy → Orchestrator → EventSink
        │  每线程命令锁；事件 + 投影 + receipt + outbox 同事务
        ▼
EffectWorker → ProviderSessionManager → CodexAdapterV2
        │  stdio JSON-RPC（每行一个 JSON 值）
        ▼
codex app-server → Codex 原生 thread / turn / approval / notification
```

| 层 | 主要标识 | 责任 |
| --- | --- | --- |
| 客户端命令 | `commandId`、`messageId`、`threadId` | 用户意图、幂等重试、乐观消息对账 |
| 编排执行 | `runId`、`activeAttemptId`、`providerTurnId` | 排队、steer/restart、Stop、拒绝旧 attempt 的迟到写入 |
| Codex 原生 | native `threadId`、`turnId`、JSON-RPC `id` | 真正执行与原生流式回调 |
| 客户端同步 | `sequence`、`snapshotSequence`、`afterSequence` | 事件全序、断点续传、重叠去重 |

这些 ID 不可互相替代。尤其 `commandId` 是“这次命令只提交一次”，`messageId` 是“这条用户消息是谁”，`runId` 是“这次执行及后续工作”，native turn id 是“Codex 当前回合”；一个 run 在 Goal 等场景还可拥有多个 native turn（`docs/internals/providers.md:132-138`）。

外部 RPC 名称集中定义于 `packages/contracts/src/orchestrationV2.ts:3644-3668`，请求/响应 Schema 位于同文件 `4305-4390`。`message.dispatch` 同时携带命令、消息与投递策略字段（同文件 `3209-3249`），`run.interrupt` 和 `runtime-request.respond` 也都有独立 `commandId`（`3294-3300`、`3343-3351`）。

Codex 传输层只对 JSON-RPC `id` 做响应路由：request id 单调递增，`pending: Map<id, Deferred>` 保存调用者；多个请求可并发在途，没有“一个 Codex 进程同时只能发一个 RPC”的全局锁（`packages/effect-codex-app-server/src/protocol.ts:171-172,268-308,497-529`）。所有出站帧进入单一 writer 队列，保证 JSONL 帧本身不交错（同文件 `231-256,482-495`）。

## 2. 一次发送消息的完整链路

以普通 `message.dispatch` 为例，完整路径是：

1. **客户端分配稳定身份**。共享命令层缺省时用 UUID 生成 `commandId`（`packages/client-runtime/src/operations/commands.ts:279-285`）；移动 outbox 则把 `messageId/commandId` 与消息正文一起持久化（`apps/mobile/src/state/thread-outbox-model.ts:50-67`）。
2. **先排消息摄入，再排线程命令**。`messageIntake.withLock(threadId)` 包在线程锁外，避免较早到达的消息在读取 lineage 等异步步骤让位时，被后发消息插队；随后 `threadDispatch.withLock` 串行判定和提交（`apps/server/src/orchestration-v2/Orchestrator.ts:14794-14838`）。
3. **在线程锁内读最新投影并决定投递方式**。没有活动 run 就立即启动；有活动 run 时，Codex 通常在原生 turn 正在运行时 steer，在 preparing/starting 或 provider turn 空窗期排队（`CommandPolicy.ts:154-191`）。
4. **原子提交意图**。`EventSink.commitCommand` 先 `insertIfAbsent(commandId)` 预留 receipt；新命令才追加事件、逐条应用投影、写入 outbox effect，最后更新 `resultSequence`。这些操作在一个事务中（`EventSink.ts:689-765`）。重复命令直接读旧 receipt 和事件，不再执行决策（`673-706`）。
5. **提交后发布、唤醒 worker**。`publishLane` 把 commit 与 publish 纳入同一串行 lane，防止较大 sequence 先被订阅者看到、较小 sequence 随后又被客户端当旧事件丢弃（`EventSink.ts:279-318`）。此时 RPC 才返回 `{sequence}`；它不代表 provider 已完成（契约见 `orchestrationV2.ts:3747-3750`，架构约束见 `docs/internals/overview.md:45-59`）。
6. **outbox worker 执行副作用**。`provider-turn.start` 是 process-bound effect；同线程正在运行或更早的 pending effect 会挡住后续领取（`EffectOutbox.ts:327-381`）。worker 调 provider session 和 Codex adapter，而不是在命令事务里做进程 I/O。
7. **原生通知回流为同一事件源**。adapter 把 `turn/started`、文本 delta、工具事件、`turn/completed` 归一化为 `ProviderAdapterV2Event`；ProviderEventIngestor 再经线程命令锁写入编排事件与投影，客户端从 live stream 收到更新（`ProviderEventIngestor.ts:364`、`CodexAdapterV2.ts:1886-1904`）。

因此，UI 的“已发送”应理解成“服务端已接受并持久化”，真正的“已启动/运行中/等待审批/已完成”必须来自后续投影，不能用 RPC 返回直接伪造。

## 3. Codex 适配器怎样控制原生会话

### 3.1 进程与会话

- 启动参数是 `app-server -c thread_unload_delay_secs=0`（`apps/server/src/provider/codexLaunchArgs.ts:12-18`）。adapter 经 `ChildProcess.make` 持有进程，SIGTERM 后最多等 2 秒再强杀（`CodexAdapterV2.ts:1495-1526`）。
- 一个 app-server 可服务多个 native thread；模型、cwd、sandbox 等配置在 `thread/start/resume` 时按 thread 下发。创建走 `thread/start`（`7494-7526`），恢复走 `thread/resume {excludeTurns:true}`（`7527-7560`），卸载走 `thread/unsubscribe`（`7709-7720`）。
- 共享进程只 initialize 一次，`initializePermit = Semaphore(1)` 防止并发重复握手；若中断造成“远端已初始化、本地标志未置位”，吞掉特定 `Already initialized` 错误后继续（`1833-1877`）。
- resume 遇到 archived 会先 unarchive 再重试；遇到仍在 closing 的 thread，等待 `thread/closed` 对应的 Deferred，最多 10 秒后重试（`7148-7175`）。这些属于原生生命周期适配，不是客户端对话锁。

### 3.2 Turn 控制

- `startTurn` 最终调用 `turn/start`。`pendingRootTurns[threadId]` 连接“请求尚未返回”和“`turn/started` 可能先到”的两种注册入口；若返回的是旧 `attemptId` 的 turn，则明确报协议错误（`7177-7248`）。
- `steerTurn` 调用 `turn/steer {threadId, expectedTurnId, input}`。`expectedTurnId` 把追加消息绑定到调用时认定的 native turn，避免 steer 错回合；目标不在 active 集合时直接拒绝（`7664-7707`）。
- `interruptTurn` 先解析 Goal/重试状态及整个子代理 lineage，注册 waiter 后调用 `turn/interrupt`，再处理迟到后代与后台终端（`7734` 起）。`turnTerminalizationPermit` 串行 native completion、interrupt timeout 和终态发布，保证只有一个路径发布终态（`2035-2036`）。
- `turn/completed` 映射原生状态；若该 turn 正在 interrupt，即使 Codex 报 completed 也归一为 interrupted（`6940-6978`）。

Codex 能力声明明确包含 `supportsActiveSteering`、`supportsInterrupt`、`supportsQueuedMessages` 和 `terminalStatusQuality:"strong"`（`254-281`）。所以“运行中发消息”是能力驱动策略，不是 Codex 特例散落在 UI。

### 3.3 原生事件与审批

文本、推理、计划、命令输出等 notification 被归一化为 T3 事件，例如 `item/agentMessage/delta` 先进入 delta 合并器，`item/commandExecution/outputDelta` 映射为 `command_output.delta`（`4594-4625`）。schema 解码失败的通知按 `(method, issueKinds)` 去重告警后丢弃，不让单个未知通知杀死整个会话（`packages/effect-codex-app-server/src/client.ts:155-201`）。

阻塞审批则是 Codex→T3 的反向 JSON-RPC request：adapter 发出 `runtime_request.updated`，把原生 handler 挂在 `Deferred` 上；用户 `runtime-request.respond` 完成 Deferred 后，handler 才把 decision 回给 Codex（`CodexAdapterV2.ts:5585-5653`）。这类 live callback 不能跨进程恢复，能力声明为 `pendingRequestsSurviveRestart:false`、`approvalCallbacksAreLiveOnly:true`。Codex 的异步提问是另一条路径：答案作为新用户消息发送，请求可跨 turn/重启保留，不能与阻塞审批混写（`docs/internals/providers.md:116-130`）。

协议层允许最多 32 个反向 request handler 并发，超限返回 `overloaded`；`serverRequest/resolved` 会回收仍在等待的 handler，避免占用槽位（`protocol.ts:310-365`）。

## 4. 对话消息同步：权威状态在服务端，不在 socket 或缓存

### 4.1 写入一致性：receipt 解决“到底收到没有”

`commitCommand` 的 receipt 包含 `commandId/threadId/commandType/resultSequence/status/error`。重发时：

- 已接受：返回原 receipt 和该命令仍保留的事件，标记 `replayed`，不重复产生消息或 effect；
- 已拒绝：返回原 rejection，调用方得到 `PreviouslyRejected`，不会因状态变化而悄悄改判；
- 相同 `commandId` 却属于另一线程：报 `OrchestratorCommandIdConflictError`，不能拿别人的 receipt 冒充成功（`Orchestrator.ts:14728-14743`）。

共享客户端只给消息发送开启“socket 丢失后同命令重发”，最多 2 次；它等待 replacement session 后原样重发，因为服务端可能已收帧但 ACK 丢失（`packages/client-runtime/src/operations/commands.ts:287-328`）。其它命令不是默认自动重发，避免把非通信错误误当可重试。

### 4.2 读取一致性：sequence 是同步游标

服务端发布 `{kind:"event", sequence, event}`；snapshot 带 `snapshotSequence + projection`，可选 `historyCursor/hasMoreHistory`。首次订阅或无法安全回放时发 snapshot；可恢复时发 replay，再接 live tail（`apps/server/src/ws.ts:880-1110`）。

关键顺序是**先建立 live 订阅，再读持久事件**。读取期间的新事件会留在 live 队列，所以 replay 与 live 可能重叠，但不会留下中间空洞；注释明确要求客户端按 sequence 去重（`ws.ts:1007-1008`）。客户端 `applyEventsLocked` 对 `item.sequence <= sequence` 直接跳过，未知事件类型也推进游标，防止旧客户端永久卡住（`packages/client-runtime/src/state/threads.ts:523-545`）。

恢复不是无限硬补：

- 事件数 >128、原始 payload >1 MiB 或 wire 编码 >1 MiB：改发 snapshot；
- 游标指向已裁剪区间或越出可恢复水位：改发 snapshot；
- 回放读取期间发生裁剪：再次检查并降级 snapshot；
- 有界 snapshot 用不透明 `historyCursor` 向前翻历史，实时续传仍只用 `snapshotSequence`（`ThreadStream.ts:18-27,99-118`、`ws.ts:1019-1105`、`orchestrationV2.ts:3893-3920`）。

初始 replay/snapshot 后可发 `{kind:"synchronized"}`。客户端收到它才把状态从 `synchronizing` 置为 `live`，并且只把“数据与游标一起完成”的投影用于权威缓存；取消中的旧订阅不能保存一个事件尚未应用的新游标（`ws.ts:959-962`、`threads.ts:299-321,642-676`）。

事件日志只保留约一天的实时恢复窗口，projection 才是长期状态；receipt 保留更久，因此老命令仍能去重，但重放结果不一定再带已裁剪事件（`docs/internals/overview.md:61-73`）。

### 4.3 弱网与多客户端

移动端的持久 outbox 与服务端 outbox 是不同组件：前者保存“尚未确认提交的用户消息”，后者保存“已提交命令的待执行副作用”。移动消息记录带 schema version，按 `(environmentId, threadId)` 分组、按 `createdAt` 排序，并以 `messageId` 去重（`thread-outbox-model.ts:35-67,166-192`）。

投递器只有一个 `dispatchingQueuedMessageId` 在途槽，完成后才驱动下一条；失败按 1、2、4…16 秒退避（`threadOutboxDelivery.ts:682-740`）。实际发送沿用原 `commandId/messageId`，并传 `resendAfterSocketLoss:false`，因为外层 outbox 已负责重试（`926-976`）。ACK 后先记录 delivered sequence，再删除本地记录；若 ACK 后删除失败，下一轮只补删除，不再次发送。进程若整个重启，内存标记丢失也没关系：重发相同 commandId 会命中服务端 receipt。

新建会话更谨慎：shell 尚未 live 时，列表里“没有该 thread”不能证明创建没成功，所以等待同步，而不是立刻重复创建（`thread-outbox-model.ts:197-221`）。

多客户端没有“谁打开页面谁持锁”的概念。所有客户端都能提交命令，服务端按线程锁确定顺序；所有客户端都应用同一 sequence 事件源。本地 optimistic UI、SQLite 缓存和移动 outbox 都只是待对账状态，最终由服务端 projection/replay/snapshot 纠正。

## 5. “对话锁”实际是一组分层约束

| 约束 | 粒度与实现 | 解决的问题 |
| --- | --- | --- |
| `messageIntake` | `KeyedLock<ThreadId>` | 消息在正式拿线程锁前保持摄入顺序，覆盖 lineage 读取等异步空窗 |
| `ThreadCommandExecutor` | 每 thread 一个 `Semaphore(1)` | 命令读取投影、判定、提交互斥；客户端命令和 provider 回流共用 |
| 父子线程锁顺序 | parent 先于 side chat；部分父子路径顺序获取、不嵌套 | 避免 fork/delete/delegation 交叉与锁顺序反转 |
| provider `sessionOpen` | `KeyedLock<ProviderSessionId>` | 同一 provider session 打开单飞 |
| provider `threadAttachment` | `KeyedLock<providerSessionId + threadId>` | 同一会话附着、切换、卸载互斥 |
| effect outbox | SQL `UPDATE…RETURNING` + `NOT EXISTS` 领取谓词 | 同线程 effect 按持久顺序执行；retry backoff 也挡住后续动作 |
| `publishLane` | 全局 `Semaphore(1)` | 提交与发布顺序一致，避免客户端因乱序而丢旧 sequence |
| Codex `initializePermit` | 每 app-server client 一个信号量 | 共享进程 initialize 单飞 |
| Codex `turnTerminalizationPermit` | 每 adapter session 一个信号量 | native completion、interrupt、超时只结算一次 |

`KeyedLock` 的代码明确写着**不可重入**：持有同一个 key 时再次获取会死锁（`packages/shared/src/KeyedLock.ts:15-17,48-53`）。因此锁顺序是协议的一部分，不只是实现细节；Orchestrator 也在父子任务路径旁明确警告 keyed semaphore 无死锁检测（`Orchestrator.ts:14845-14848`）。

outbox 不是内存 mutex，而是数据库约束：候选 effect 只有在同线程没有 running、也没有更早 pending 时才可领取（`EffectOutbox.ts:327-381`）。`thread-title.generate` 有独立分组与全局并发上限，不阻塞主对话 effect。进程丢失后，`provider-turn.start/steer/interrupt/restart` 等 process-bound effect 一律取消；checkpoint、cleanup 等 replay-safe effect 才重新入队（`EffectOutbox.ts:135-155,515-547`）。

除互斥锁外，还有**写事务内的乐观守卫**：`writeIfRunCurrent` 校验 run status + `activeAttemptId`，provider thread 写入还校验 `lastRunOrdinal` 或完整旧 payload。守卫失败就不提交，从而拒绝旧进程、旧 attempt 的迟到事件（`EventSink.ts:500-671`）。这类 CAS 守卫是“对话锁”不可替代的另一半：锁只能约束当前进程内的顺序，版本校验还能挡住异步世界里的过期写入。

## 6. 关键竞态怎样收敛

### 6.1 发送 vs. 运行结束

客户端看到的 running 可能已过期，所以 auto 模式不能盲目 steer。服务端在线程锁内重新读取 projection：preparing/starting、provider turn 已结束等空窗一律排队；只有当前 attempt 确有 running provider turn 且能力允许时才 steer（`CommandPolicy.ts:169-191`）。真正提交 steer 前还会再次检查 run、session、turn 与 Stop 标记（`Orchestrator.ts:5472-5538`）。

不支持 active steering 的 provider 可选择 interrupt+restart；`/compact`、`/logout`、`/goal` 这类维护命令禁止 steer，必须独立排队（`Orchestrator.ts:5409-5434`）。这把“追加一条消息”变成明确的服务端状态机，而不是客户端碰运气。

### 6.2 Stop vs. steer / 迟到完成 / 排队启动

Stop 先持久化确定性的 `run_interrupt_request` item，再发 interrupt effect。`stopReachedRun` 只看这个持久事实，不依赖内存布尔；因此 Stop 之后到达的 steer 会被拒绝，即使 run 在投影里还显示 running（`Orchestrator.ts:5509-5516,11805-11871`）。

RunExecutionService 写入迟到终态时使用 `writeIfRunCurrent(..., unlessInterruptRequested)`：事务内若发现 interrupt request 尚无 result，就拒绝这次旧写入（`EventSink.ts:524-551`）。同命令重复 Stop 也因 request item 已存在而不重复发事件。force stop 则先在线程锁内完成“检查目标 + kill”，释放锁后再重新 dispatch 结算，避免 check-and-act 空窗和可重入死锁（`ThreadManagementService.ts:1209-1239`）。

### 6.3 Stop vs. 用户回答审批

终态清理可能先读到 pending request，随后用户回答，再执行旧清理。`guardUserInputCancellations` 因此在**写事务内重读** request；若已回答、已取消、providerTurnId 不符，或是 message-mode 问题，就过滤过期 cancellation 及关联 node/turn-item 更新（`EventSink.ts:320-364`）。这是“读时检查”不够、必须“提交时再检查”的典型例子。

### 6.4 notification vs. turn/start 响应

Codex 可能先推 `turn/started` 或 item notification，再返回 `turn/start` response。adapter 同时支持两条注册路径，并用 `pendingRootTurns`、`attemptId` 和 active turn context 对账；不是假设 JSON-RPC response 一定先于 notification（`CodexAdapterV2.ts:7207-7233`）。

### 6.5 进程崩溃

进程终止会一次性 fail 所有 pending JSON-RPC Deferred、关闭出站队列并结束 request handler scope（`protocol.ts:197-229`）。编排层再把 process-bound effect 取消，而不是重启后盲目重放 `turn/start/steer/interrupt`；否则可能在新的 Codex 进程里重复执行一个已失去原语义的旧动作。

## 7. 可提炼的网关约定

下面是从源码提炼的**最小约定**，不是要求照抄 Effect RPC 或完整事件溯源。传输可以仍是 SSE/长轮询，存储也可以不重建全部历史，但这些语义应保持稳定。

### 7.1 命令约定

```text
Command  = { commandId, threadId, type, ...payload }
Ack      = { sequence }             // 仅表示意图已提交
Receipt  = { commandId, status, resultSequence, error? }
Message  = { messageId, ... }       // 与 commandId 分工，不互相复用
```

1. 客户端首次生成 `commandId` 后，所有弱网重试必须原样保留；服务端全局唯一约束它，并保存 accepted/rejected receipt。
2. 同 ID 重试返回第一次结果，不重新执行；同 ID 跨 thread 或换 payload 应视为冲突，而不是复用成功结果。
3. ACK 只承诺 durable intent。执行进度、完成和失败必须通过可订阅状态下发，调用方不得把 ACK 显示成“agent 已完成”。
4. 同 thread 的“读投影→决策→提交”必须串行；消息若还有锁前异步步骤，需要额外摄入序列化或等价的单调序号。
5. 事件、权威投影、receipt、待执行 effect 必须原子提交；提交后才发布事件、执行外部 I/O。
6. Stop、审批回应、排队编辑等控制命令与发送消息一样需要幂等，不能只保护用户正文。

### 7.2 同步约定

```text
StreamItem = { kind:"event", sequence, event }
           | { kind:"snapshot", snapshotSequence, projection, historyCursor? }
           | { kind:"synchronized" }
```

1. `sequence` 单调；客户端忽略 `sequence <= cursor`，包括 replay/live 重叠和未知事件类型。
2. 必须先订阅 live 再读 replay/snapshot，或提供等价的无空洞屏障；不允许“先读历史、后开订阅”。
3. 回放必须有事件数与字节预算，超限、游标过期或历史裁剪时自动 snapshot，不能永久补洞。
4. `synchronized` 只表示初始数据追平，不表示 agent 空闲；缓存的数据与游标必须来自同一次完整应用。
5. 旧 attempt/provider 的迟到写入必须带版本守卫，例如 `(runId, activeAttemptId, expectedStatus)`；只靠进程内 mutex 不够。

### 7.3 对话控制约定

“对话锁”应对外表现为状态机，而不是暴露一个 `locked:true`：无活动 run 时 start；活动中按能力 steer/restart/queue；Stop 先形成持久事实再取消副作用；审批回复绑定 request 与 attempt；同 thread effect 顺序执行。客户端只声明意图，最终裁决永远在 server 最新投影上完成。

## 8. 对 mobile-bridge 的落地取舍

结合本仓库已有 timeline 游标与 delta，不建议为了获得这些语义重写为 WebSocket RPC，也不必建立可回放全部历史的完整事件溯源。最小可落地顺序是：

1. **先统一命令身份与 ACK 语义**：发送、停止、审批、迁移、账号切换全部使用稳定 command id；返回“已接受 sequence”，前端等待后续状态，不直接当执行完成。
2. **把并发不变量写成网关内部约定**：每会话命令串行、消息摄入顺序、同会话副作用串行、父子会话锁顺序、禁止可重入假设。锁服务进程内判定，数据库守卫负责迟到写入。
3. **推广持久 receipt，但只记网关自有意图**：accepted/rejected 都可重放；发送已有提交 ID 可复用其身份。无需为了 receipt 把 provider 所有流式 delta 永久保存。
4. **给控制副作用加持久 outbox**：区分 process-bound 与 replay-safe，避免网关重启后旧 start/stop 在新 provider 进程中盲放。
5. **在现有 `?since=` 上补同步闭环**：live 先行或等价屏障、重叠按序号去重、回放预算超限自动 snapshot、初始追平标记。
6. **运行中输入改为服务端策略**：auto 由最新 run/provider turn/Stop 状态裁决 steer、queue 或 restart；客户端传递意图而非预先锁死结论。
7. **移动端复刻持久 outbox 的身份纪律**：持久保存 command/message id，单条在途，ACK 后删除，创建会话在列表未同步时不得仅凭“看不到”重发。

最需要避免的两个过度设计是：把 Codex JSON-RPC 的 request id 泄露为网关幂等键；把“每会话命令锁”误当成全部一致性方案。前者层级错误，后者无法覆盖 ACK 丢失、进程重启和迟到 provider 事件。

## 9. 研究边界与主要源码索引

本研究核对的是控制与一致性路径，没有评估模型质量、登录、账号切换、checkpoint 或 PR 功能；未启动 T3 server、未连接真实 Codex、未做网络故障实测。协议行为均以固定 commit 的源码和项目内架构文档为依据。

主要源码索引：

- 外部命令/流契约：`packages/contracts/src/orchestrationV2.ts`
- WebSocket thread 恢复：`apps/server/src/ws.ts:880-1110`
- 每线程锁与锁顺序：`packages/shared/src/KeyedLock.ts`、`apps/server/src/orchestration-v2/ThreadCommandExecutor.ts`、`Orchestrator.ts:14785-14838`
- 投递策略：`apps/server/src/orchestration-v2/CommandPolicy.ts:93-191`
- 原子提交与乐观守卫：`apps/server/src/orchestration-v2/EventSink.ts:279-318,500-790`
- effect 串行与崩溃恢复：`apps/server/src/orchestration-v2/EffectOutbox.ts:135-155,327-381,515-547`
- Codex 进程/thread/turn/审批：`apps/server/src/orchestration-v2/Adapters/CodexAdapterV2.ts`
- Codex JSON-RPC：`packages/effect-codex-app-server/src/protocol.ts`、`client.ts`
- 客户端去重与同步态：`packages/client-runtime/src/state/threads.ts:299-321,523-686`
- 客户端丢 socket 重发：`packages/client-runtime/src/operations/commands.ts:279-328`
- 移动持久 outbox：`apps/mobile/src/state/thread-outbox-model.ts`、`thread-outbox-manager.ts`、`threadOutboxDelivery.ts`
- 架构约束：`docs/internals/overview.md:45-77`、`docs/internals/providers.md:116-143`

相关既有研究：[T3 Code 整体学习笔记](t3code-study.md)、[mobile-bridge 对照架构评审](t3code-architecture-review.md)。本文只深入 Codex 控制链、消息同步与并发约定，不重复整体功能清单。
