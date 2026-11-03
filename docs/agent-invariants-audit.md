# Agent 调用五条不变量验证报告

> 状态：验证完成，未修改产品代码 · 验证日期：2026-11-03 · 代码基线：`6fb21ee` · 范围：Go 版 Codex 专用通路与统一 ACP 运行时

本次按五条纪律逐项核对实现、测试与真实假 Agent 调用。结论不是“全部符合”：**Codex 专用通路整体较严谨，通用 ACP 路径有三类结构性差异，发送路径另有一个已复现的返回值缺陷。**“未达到”表示不满足这条严格 invariant，不必然表示当前产品行为在所有场景下都是错误。

## 1. 结论总览

| 不变量 | 结论 | 核心证据 |
| --- | --- | --- |
| 来源不等于当前 owner | **部分符合** | Codex 先发现 owner；ACP 先启动并 `session/load`，被 wrapper 拒绝后才只读降级 |
| 能力按当前会话与运行时判断 | **部分符合** | 模型/模式/附件是动态的；skills/steer/fork 等仍有 profile 静态声明 |
| 重要设置读回或版本校验 | **未达到严格标准** | SetMode、ACP/Codex settings 都以 RPC 成功为主要确认，缺少独立读回/CAS |
| 发送与完成状态分离 | **存在已复现缺陷** | 正常路径分层较好，但 Submit 写入失败会返回 nil，上层可误记 accepted |
| 只读打开不启动或恢复 Agent | **ACP 未达到** | List/Open 都会启动或连接 Agent；只读 transcript 是 load 被拒后的兜底 |

## 2. 来源不等于当前 owner，不得另起进程抢写

**Codex 通路符合。**`Bridge.Watch` 先执行 `thread-owner-discovery`，保存 `handledByClientId` 后 follow；`Call` 每次操作再次 Watch，并把请求 target 设为当前 owner。快照若来自另一个 `sourceClientId` 会被忽略（`internal/codex/bridge.go:64-103,225-277,336-393`）。它不会根据会话的来源标签另起 Codex CLI writer。

**ACP 通路只符合“最终不双写”，不符合“写前预检 owner”。**`Manager.Open` 在无现存句柄时直接进入 `openFresh`：`m.start()` 启动子进程或连接 Agent Host，随后调用 `session/load`。只有 load 返回包含 `already owned` 的错误，才通过 `openNative` 构造 transcript 只读会话（`internal/acpruntime/manager.go:664-746,1946-1997`）。因此原生写锁避免了实际双写，但实现仍然先尝试启动并恢复，而不是先发现 owner。

现有 `TestOpenFallsBackToReadOnlyTranscript` 证明拒绝后的只读降级和 Prompt/SetMode/Cancel 拦截有效，却没有证明 load 之前发生过 owner 预检（`internal/acpruntime/native_test.go:239-286`）。

> **DSH POSIX 第一阶段（1553a48）**：当前上游已在 `session.lock` 上持有跨进程 `flock`（早期“lease 尚未实现”的笔记已过时）。`OwnerProbe` 在 `openFresh` / `Reconcile` 前以非阻塞加锁、inode 校验、立即释放识别写者，命中后直接 `openNativeHistory`，不再启动后等待拒绝；锁空闲时 load 仍是最终互斥权威。锁持有/释放与 Open/Reconcile 不启动回归已覆盖。Windows 使用上游命名信号量，主仓暂未实现同等探针，保留旧降级，不能计为达标；其他 Agent 也仍按原路径。

## 3. 能力必须按当前会话和运行时判断

已有四类动态判断是有效的：

- `initialize` 保存 agentCapabilities，附件按当前进程的 `promptCapabilities.image/embeddedContext` 拒绝或放行（`internal/acp/acp.go:182-214`、`internal/acpadapter/acpadapter.go:45-83`）。
- 模型、effort 和工作模式读取当前 session 的 `modes/configOptions`，不是仅从 profile 猜（`internal/acpruntime/manager.go:837-874,1236-1325,1471-1522`）。
- Skill 选择会解析当前 session 的 slash commands，发送前再次验证（`internal/acpruntime/manager.go:1591-1632`）。
- 桌面 owner 的 transcript 只读会话会过滤 capabilities，仅保留网关合成的 fork；Goal 还会检查网关是否确有对应 transport（`internal/httpd/server_bridge.go:176-207`）。

但 `Manager.Capabilities` 仍直接来自 profile 的静态字符串，并进入 list/detail 元数据；`skills/steer/fork` 的可见性因此不完全等同于“当前原生 session 已证明支持”（`internal/acpagents/acpagents.go:167-305`、`internal/httpd/server_bridge.go:154-201`）。Steer 的最终判断依赖 `SteerMethod != ""`，运行时不支持时只能等 RPC 失败；fork 是网关合成能力，并非原生 fork 证据。

结论是**动态能力基础已存在，但尚未形成统一的三层判断：协议能力、当前会话状态、网关已验证能力**。不能把 profile 静态声明当成运行时证明。

> **steer/skills 起步收敛（784defa）**：`acpViewState` 的能力列表不再照搬 profile——`steer` 需 `SteerMethod != ""`（与发送路径同一判定，profile 声明 ≠ 运行时证据），`skills` 需会话真实广播过可选命令（`available_commands_update`；仅内置命令或旧会话不广播时隐藏入口，命令到达后随视图信号自动出现），`goals` 维持 transport 门控、`fork` 维持网关合成语义。降级测试覆盖「profile 声明支持但运行时拒绝」「旧会话不广告该能力」「仅内置命令」三形态。未做：统一的 effective-capabilities 框架与其余能力项（steer 运行时拒绝的记忆化降级等）。

## 4. RPC 成功不等于原生状态已生效

这条在三条设置路径上都没有完全满足：

- `Session.SetMode` 在 `session/set_mode` 返回成功后直接修改 `s.modes.currentModeId` 和 View，没有重新读取原生 mode（`internal/acpruntime/manager.go:1236-1307`）。
- `setConfigOption` 优先采用 RPC 返回的 configOptions；未返回时直接修改本地缓存的 currentValue。`UpdateSettings` 先写 model 再写 effort，第二步失败时第一步可能已经生效，接口没有部分成功或回滚语义（`internal/acpruntime/manager.go:1471-1567`）。
- Codex `UpdateSettings` 只返回 `thread-follower-update-thread-settings` 的 RPC 结果，没有在完成后读取 Snapshot 比对实际 model/effort/serviceTier（`internal/codex/bridge.go:556-570`）。

这些实现可以证明“Agent 接受了 RPC”，不能独立证明“原生设置已生效且仍是同一版本”。后续原生通知可能最终校正视图，但不能替代写入接口本身的确认边界。

> **部分修复（bac8e1c）**：`UpdateSettings` 已重写——前置校验（被拒值零 RPC）、模型切换后重读权威 configOptions（修掉 mcode 丢弃 thought_level 时的过期快照缺陷）、`SettingsPartialError` 显式申报「已生效：模型」的部分成功、ACP settings 响应新增 `confirmed` 字段（仅当 agent 返回权威 configOptions 才为真，手机端已有「已同步/等待同步」两档文案）。`setConfigOption` 优化路径（响应无 configOptions 时的本地补丁）与 SetMode 的 RPC 成功即证据语义维持现状。

## 5. accepted / queued / steered / completed 必须分开

正常路径的分层基本正确：

- 账本明确注释 accepted 只是 dispatched to agent；queued、unknown、failed 各自独立（`internal/httpd/queue.go:30-40`）。
- `mode=queue` 先入持久队列；运行中直接发送若被本地 `promptRunning` 明确拒绝，会回到 queued 并返回 queue 状态，而不是 accepted（`internal/httpd/sessions.go:724-760,853-877`）。
- Steer 只有原生返回 `outcome=injected` 或 mcode `mode=steered` 才设置 `accepted:true, steered:true`；`promptRequired` 会回退普通发送（`internal/acpruntime/manager.go:1148-1192`、`internal/httpd/sessions.go:805-824`）。
- ACP `Submit` 成功后异步等待真正的 `session/prompt` RPC 结果，以 stopReason 结束回合；静默超时发送 cancel、把回合标 failed，不把 idle 当成功（`internal/acpruntime/manager.go:1002-1077`、`internal/acpruntime/idle_test.go:48-114`）。

### 5.1 已确认的缺陷：Submit 失败却返回 nil

`Session.prompt` 在 `Process.Submit("session/prompt")` 返回错误时，确实会清理 promptRunning、把 View 标为 failed 并关闭 promptDone；但最后一行是 `return nil`，不是 `return submitErr`（`internal/acpruntime/manager.go:992-1003`）。

独立假 Agent 边界复现使用真实 `Manager.Create` 构造会话，再把传输替换为已关闭的 ACP Process，实际输出：

```text
prompt_error=<nil> session_status=idle turn_status=failed
```

因此上层看到的恰好是“本地回合失败 + 调用返回成功”。ACP HTTP 直接发送在 `callErr == nil` 时会设置 `dispatched=true`，延迟函数随后把账本置为 accepted（`internal/httpd/sessions.go:728-760,853-876`）；队列派发也会把 nil 解释为 outcomeAccepted（`internal/httpd/queue.go:450-457`）。这直接破坏了 accepted 的证据边界。

本次运行的 ACP 核心与 HTTP 全量测试都通过，说明现有测试没有覆盖 Submit 写入失败的错误传播。

> **已修复（cb0e759）**：`prompt()` 改为返回 `submitErr`；直接发送落 409 并回队，队列派发按 unavailable 归 outcomeUnknown（不自动重试）。三层回归见 `internal/acpruntime/submit_error_test.go` 与 `internal/httpd/submit_error_http_test.go`。
>
> **写入边界续修（9070cca）**：Submit 同步错误细分为写入前已关闭（`AcpUnavailable`，请求确定未发送，可回队）与写入中断（`AcpWriteUnknown`，结果不确定，禁止自动重试）；直接发送也不再对任何 Prompt 错误一律回队。传输、HTTP/队列分类回归分别见 `internal/acp/submit_test.go`、`internal/httpd/submit_error_http_test.go`。

### 5.2 stopReason 的完成映射偏宽

`View.EndTurn` 只把 `cancelled` 映射为 interrupted，其余所有 stopReason 都映射为 completed（`internal/acpview/view.go:209-222`）。因此 `max_tokens` 这类“输出被截断”的终止也会在会话列表显示“已完成，未查看”；Jev 后续虽会把 max_tokens 当作 interrupted 判断续跑，但这并不能修正已展示的 completed（`internal/jev/continue.go:120-160`、`web/activity.js:8-29`）。

这不是 idle 直接当成功，但仍是**终态与成功语义混用**。建议单独评估 max_tokens/max_turns 等异常终止是否应映射为 interrupted/failed，并保留 stopReason 供通知和自动继续区分。

> **已修复（8c56083）**：`EndTurn` 把 cancelled/max_tokens/max_turns/max_turn_requests 统一映射为 interrupted（Python 原版同缺陷，此处有意加宽偏离，conformance 金样不含预算停靠用例、不受影响）；Jev `DecideContinue` 同步区分“预算停靠的 interrupted”与“用户取消的 interrupted”，前者仍允许自动继续，并补齐 ACP 规范拼写 max_turn_requests。team 侧 `cancelledTurn` 对预算截断的成员任务按 stopped 收场，不再带截断结果落 completed。
>
> **refusal 续修（9070cca）**：普通 refusal 落 interrupted，列表显示“已停止，未查看”，Jev 以 `refusal` 原因禁止自动继续；带 CodeBuddy 错误元数据的 refusal 仍按 failed 处理。同步修复 `_meta` 或 `data` 缺失时 refusal 分类的 nil 解引用，并补运行时、Jev 与在线/离线列表徽标断言。

## 6. 只读打开不得启动或恢复 Agent

**ACP 不符合这条严格规则。**缓存失效或首次 `Manager.List` 会 `m.start()`，Initialize 后调用 `session/list`；即使后面使用 NativeList 补全，前面的 Agent 进程已经启动过（`internal/acpruntime/manager.go:300-376`）。

`Manager.Open` 同样先 `m.start()` 并调用 `session/load`；HTTP 的会话解析和详情读取会经 `resolveACP → openACP → manager.Open` 进入该路径（`internal/acpruntime/manager.go:664-746`、`internal/httpd/server_bridge.go:81-137`）。桌面已占用时的 transcript 只读视图不是一条独立的纯历史入口，而是启动并尝试 load 后的降级。

这属于当前 ACP 架构选择：打开会话就意味着建立控制通道。若要采用上游纪律，需要把“读取原生保存历史”和“显式连接/恢复控制”拆成两个 API；仅修改错误文案不会消除进程启动副作用。

> **DSH 第一阶段（1553a48）**：`NativeListExclusive` 直接读取 transcript header 的 id/cwd 与 projection 标题，列表不再启动；`OpenHistory` 复用原生只读 Session/poller，自动归属、显式 Agent 参数、View、Timeline、SSE 与启动预加载都走纯历史，显式 `reconnect upgrade=1` 才进入控制路径。缓存未变的冷重开会先 `NativeReset`，不会误判无历史。marker 假 Agent 证明 List/Open/Reconcile/HTTP/预加载均未启动。**其他 Agent 尚无统一 NativeHistory，仍不符合本节严格规则；本项仅完成 DSH，不代表全 ACP 达标。**

## 7. 实际执行的验证

以下命令均在代码基线 `6fb21ee` 上实际执行：

| 验证 | 结果 |
| --- | --- |
| `go test ./internal/acpruntime ./internal/acpview ./internal/acpadapter ./internal/codex ./internal/jev -count=1` | 五个包全部通过 |
| `go test ./internal/httpd -count=1` | 通过，覆盖 HTTP、队列和会话集成测试 |
| `node --test tests/test_session_activity.js` | 1 项通过 |
| 独立假 ACP Process 的 Submit 失败复现 | 确认 `prompt_error=<nil>` 且本地 turn 为 failed |

源码核对覆盖 owner 路由、ACP Open/List、动态 capabilities、SetMode/config、Codex/ACP settings、发送与队列、steer、stopReason 和只读 transcript。测试使用合成 Agent 与隔离临时目录；没有调用真实模型、真实桌面 App 或真实用户会话，因此不把这些结果扩展为各 Agent 的实机兼容性证明。

## 8. 建议修复顺序与补测清单

1. **先修 Submit 错误传播。**`Session.prompt` 清理本地状态后应把 `submitErr` 返回调用者。新增三层回归：Session 必须返回错误；直接发送不得显示成功；队列不得把该消息置为 accepted。还应分别覆盖写入前已关闭与写入中断线，避免把未知结果误改成可安全重试。
2. **为设置接口补确认语义。**至少补“RPC 成功但无原生生效证据”“model 成功而 effort 失败”“原生 revision 已变化”三组测试，再决定采用读回、revision/CAS，还是明确部分成功。不能仅修改本地 currentValue 后继续返回笼统成功。
3. **收窄 stopReason 映射。**为 max_tokens、max_turns、refusal、cancelled 分别断言回合状态、列表徽标和自动继续分类，避免 completed 同时承担“终态”和“成功”。
4. **把纯历史与显式 attach 分开。**先增加不启动进程的 NativeHistory 路径，再让 Open 只服务显式控制；owner 预检应在 session/load 前完成。该改动涉及架构，不能与 Submit 修复混成一个小补丁。
5. **收敛能力证据。**保留 profile 作为适配器先验，但 UI 最终能力还需结合当前 session 的协议响应、运行状态和 readOnly/owner；补“profile 声明支持但运行时拒绝”和“旧会话不广告该能力”的降级测试。

相关上游语义参考见 [upstream-agent-invocation-study.md](upstream-agent-invocation-study.md)。本报告只完成验证，没有对上述缺陷做实现修改。
