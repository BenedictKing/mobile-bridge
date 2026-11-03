# mobile-bridge 架构改进清单（对照 t3code）

> 状态：评审，未实施 · 日期：2026-11-02 · 依据：[t3code-study.md](t3code-study.md) 调研 + 本仓库 `internal/` 实况核查

先核实三个事实，它们改变评审结论：

1. **Go 迁移已完成**：`bridge/*.py` 已删（目录只剩 `__pycache__`），`internal/` + `cmd/` 约 12 万行 Go 全覆盖。但 [ARCHITECTURE.md](../ARCHITECTURE.md) 的模块表仍列出 `bridge/ipc.py`、`bridge/service.py` 等 Python 模块——文档已与现实脱节。
2. **传输层比预期接近 t3code**：`internal/timeline` 已有游标 + delta + changes（`PageBytes 192KiB`、`changesMaxRows 100`、`versionWindow 16`、`errStaleCursor`，`internal/timeline/timeline.go:20-32`），不必推倒重来。
3. **最大的债在数据模型**：`internal/jsoncompat` 自述「keeps JSON byte-compatible with Python's json module…every value that round-trips through disk or wire is represented here as an insertion-ordered tree」。为了和已删除的 Python 实现逐字节对拍，整个 view/timeline/持久层是**无类型动态树**。

## 1. 退役「Python 字节兼容」脚手架（最高优先级，解锁其余）

`jsoncompat` 与各包注释里的 "Go port of bridge/X.py, parity enforced against Python oracle goldens" 是迁移脚手架，Python 侧消失后它从工具变成了永久数据模型：

- 代价：wire 与持久层 stringly-typed，字段名散落（如 `timeline.go` 的 `rowWhitelist` 手写白名单），没有编译期契约；新功能没有 Python oracle，却仍被迫按 Python 语义写 Go（插入序 map、原样数字 lexeme、无 HTML 转义）。
- 建议 three-step：
  1. **声明 oracle 退役**：conformance goldens 冻结为回归基线（防行为漂移），新代码不再追求字节对拍；
  2. **引入 typed 契约包**（对照 t3code `packages/contracts`）：view / timeline row / changes / 请求回应先定义 struct，未知字段保留为显式 `unknown` 成员而非丢弃或报错（对照 `ForwardCompatibleUnion`，`packages/contracts/src/baseSchemas.ts:151-177`）；
  3. **jsoncompat 收缩到读旧持久化文件**：wire 输出逐步换 struct 序列化——字段顺序对客户端本无意义，客户端只认字段名。
- 顺带修正 ARCHITECTURE.md 模块表。

## 2. 网关自有状态：散装 JSON → 单库 + receipt/outbox

现状：`auth-sessions.json`、creations、`notification-policies.json`、`pool/`（settings + history.jsonl）、队列状态分散各处，无事务；重启恢复靠 `resume_watched` 等内存重建。三个已知缺口都源于此（[opendots-openmuse-study.md](opendots-openmuse-study.md) §2.1/§2.3）：执行侧无 receipt、崩溃窗口丢通知、重启后未完成动作无「重试/废弃」依据。

建议：一个 SQLite（标准库 `database/sql` 即可）只承载**网关自己的意图**，四张表：

| 表 | 承载 | 对照 t3code |
| --- | --- | --- |
| command receipts | 发送/审批/停止/迁移/切换全走幂等键；**rejected 也落库不翻案** | `CommandReceiptStore.ts:43-52` |
| outbox | 待执行副作用分 `process-bound`（重启取消）与 `replay-safe`（重排）两类 | `EffectOutbox.ts:125-144` |
| 通知发送日志 | identity 去重 + 完成延迟确认 + 启动对账 | `AgentAwarenessRelay.ts:437-651` |
| 队列快照 | 排队消息/自动继续/切换续跑重启不丢 | queued run + continuation |

**不做完整事件溯源**——timeline 投影保持现状（它已经是对的），只在网关自有意图上记账。

## 3. 方言知识 profile 化 + 能力数据化

现状：方言按 agent 散在 `internal/{kimi,zcode,dsh,claude,codebuddy,codex}` 各包与 `internal/acpagents` 开关里；能力矩阵是 ARCHITECTURE.md 里的 Markdown 表格，不是代码。

t3code 的对策（`docs/orchestration-v2/provider-capability-system.md`）：能力是数据（12 组 schema，`identity: strong|weak|none`、`terminalStatusQuality`、`pendingRequestsSurviveRestart`），方言是声明式 flavor hooks，**orchestration 与客户端禁止 provider 名分支**。

建议：

- 把能力矩阵表翻成一个 per-agent 的 declarative profile（Go struct 常量）；view 的 `capabilities` 数组扩词表，前端门控全按能力位；
- 四类 quirk 收敛进 profile + 引擎通用路径：`KEEP_CREATED_PROCESS` ↔ `backgroundTasksEndWithProcess`；canonical id 惰性占位 ↔ scope 前缀 + 版本号；`set_mode` 回退 ↔ config-option 优先；steer 扩展 ↔ `supportsSteeringByInterruptRestart`（app 层 interrupt+restart 通用实现，引擎内 steer 一律 unsupported，`packages/provider-acp/src/server/adapter.ts:8177-8183`）。

## 4. 传输层三小件（现有 timeline 之上的增量）

1. **replay 预算超限自动降级快照**：t3code 的纯函数决策——回放 >128 条事件或 >1MiB 直接发快照（`ThreadStream.ts:18-27`）。长会话断连重连不再「硬补 or 全量」二选一。
2. **`synchronized` 标记**：初始追平后给前端一个信号，区分「还在补历史」与「已是实时」（`rpcInitialItems.ts`）。
3. **工具长输出移出主流**：CommandOutputHub 模式——展开的行才订阅、tail+append 双模式、16KiB 缓冲超限转全量 tail（`CommandOutputHub.ts:30-209`）；「忙命令刷爆事件流、阻塞所有线程摄入」是 t3code 写明的反面教训。

## 5. 通知：awareness 旁路投影

把通知从「各事件点硬编码」改为**单一会话状态投影**（running/waiting_approval/waiting_input/completed/failed）作为唯一来源：投影序列化成 identity 去重（不变不发）、完成态延迟 5s 二次确认（自动继续开启时必需）、启动时全量对账（配 §2 发送日志）、通知带会话 deep link。

## 6. 权限模式层 + Jev 统一 claim

四档 `RuntimeMode`（数值 rank 比较）+ 管理天花板 + **provider 不支持时降级回 approval 而非报错**（`RuntimePolicy.ts:102-196`）+ session 级授权枚举（`acceptForSession`）。Jev 自动审批与人工审批竞争同一个 claim 入口——谁的 receipt 先落库谁执行（§2 的 receipts 表直接提供）。

## 7. 原生跟随器家族收敛

现状五个 bespoke 只读 reader：codex SQLite/rollout、kimi `state.json`、zcode `db.sqlite`、workbuddy JSONL、dsh transcript。读只读纪律已好；建议：

- 统一 `NativeFollower` 接口，缓存/mtime/字节上限/静默缺失策略共享（`Manager.NativeList`/`acpview.NativeSnapshot` 已是雏形）；
- 评估 `codex app-server`（CLI 自带 headless 模式）作为目录/Goal/创建的长期官方接口——t3code 钉上游 commit 生成 schema、解码失败按 `(method, issue)` 去重告警后丢弃（`packages/effect-codex-app-server/src/client.ts:89-176`），证明该接口可长期依赖，比直读 App 私有格式稳。

## 8. Web 客户端纪律清单（不引框架）

- 订阅生命周期与缓存生命周期分离；**缓存快照的序号直接作续传游标**（`packages/client-runtime/src/state/threads.ts:208-243`）；
- 长后台回前台两档唤醒：长挂起直接放弃旧连接重建，不等慢超时（`supervisor.ts:622-695`）；
- 更优链路后台预检（连着隧道时定期无凭据探测 LAN，可用即切）；
- 离线缓存可读但不冒充在线、重连不覆盖更新的 live 数据；
- 写请求按会话 latest 合批，连点不堆请求。

## 9. 测试架构：录制回放 + 预算测试

- conformance golden 演进为「**录制 agent 帧 → 回放 → 断言投影**」：对照 t3code 的 replay runtime（唯一允许的替身是传输层，Orchestrator/adapter/投影全跑真的，`docs/orchestration-v2/testing-strategy.md`）与 `record-codex-app-server-replay-fixture.ts`；
- wire 字节预算写成会失败的测试（`transferBudgetV2.integration.test.ts` 先例），防 timeline 页预算悄悄膨胀。

## 10. 不建议做的

- **不上 WebSocket RPC 重写**：SSE + 长轮询 + 双链路直连已覆盖场景，契约化（§1）收益远大于换传输；
- **不搞完整事件溯源**：投影层已对，receipt/outbox 只管网关自有意图（§2）；
- **不追原生 App / 云 relay**：个人场景 Bark/ntfy + 双链路已够，移动 Web 的差距用 §8 纪律补；
- **agent-host 不一刀切**：按「重启打断代价」逐个评估（长任务、冷启动贵的先迁）。

## 优先级

§1 → §2 →（§3、§5、§6 可并行）→ §4、§8 → §9 → §7。§1 是其余各项的前置：typed 契约立起来，receipts/outbox/能力位才有地方放。
