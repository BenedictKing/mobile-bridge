# 网关会话命令一致性

本次补全沿用 Codex app-server / desktop follower、ACP 与现有 SSE；发送继续使用 submissions 账本，不另建一套消息队列。

## 实施范围

1. 同一会话的发送、停止、回应、设置、队列修改与执行权交接共用串行入口；后台队列使用相同入口，网关串行锁以会话为粒度。
2. 停止与原生审批/提问回应先持久 claim，再调用 provider，最后保存结果。相同命令重试复用回执，冲突内容拒绝，未知结果禁止自动重放。
3. 人工和 Jev 回应共用 provider 层的回执入口；原生请求身份包含运行时世代，避免重启后复用 request id。
4. 停止绑定客户端看到的 turn，过期停止不能取消后来启动的任务。
5. 直接发送与队列使用同一提交账本；服务端重新判定忙闲。未知提交挡住后续自动派发，用户核实后可忽略该记录。
6. 手机持久保存未确认发送的完整 payload 和身份；运行状态变化不重新生成发送 id，确认后才删除。恢复不盲目自动发送未知命令。
7. SSE 初始追平明确标记；旧帧不能回退游标，delta 必须整帧成功后才能推进游标，补读失败可恢复。

## 不变量

- 会话串行锁不可重入；后台 settle 回调在独立 goroutine 获取锁。
- 锁只保护网关动作；Codex native writer 锁继续负责跨运行时执行权。
- provider I/O 前 claim 必须成功持久化；落盘失败不得调用 provider。
- claimed 在进程重启后按 outcome_unknown 处理，不重新执行。
- 回执表示命令交付结果；任务是否完成由 provider 投影决定。
- live 审批回调不能跨原生进程重启恢复；原命令的回执仍可查询。
- timeline 数据和游标一起应用；SSE ready 只代表连接建立。

## 验证与兼容

- 用屏障驱动同会话竞态、不同会话并发、重复审批/停止、claim 写失败、ACK 丢失与重启恢复测试。
- 保留旧客户端无 commandId 的接口；新客户端使用稳定 commandId 与 expectedTurnId。
- 当前前端为原生 JavaScript，无需安装框架依赖；验证 Node 测试和真实交互。
- 执行仓库 Go build/vet/test/race 门禁及适用的前端回归。
- 不重启正在运行的网关；验证完成后由用户重启加载。

## 实施记录

已实施，2026-11-21。

### 命令与执行

- `command-receipts.json` 保存停止与阻塞回应的 claim、accepted、rejected、outcome_unknown 及原始结果。写临时文件、fsync、原子 rename；读取损坏文件会拒绝启动。
- 相同 commandId 同内容重放第一次结果；同 ID 换内容报冲突。人工与 Jev 通过原生请求 target 共用 claim，不重复发送或互相覆盖决定。
- 原生请求身份包含运行时世代和原始参数，保留 UI 规范化会省略的 native turnId；RPC id 被新回合复用时不会误命中旧回执。
- 停止带 commandId 与 expectedTurnId，后端再次核验目标；CLI adapter 保留 expectedTurnId。ACP 在旧 prompt 收尾前保留运行权，防止旧响应覆盖新 prompt。
- 每会话锁只查缓存会话及别名，不冷探测、不自动接管；发送、停止、回应、设置、队列及交接从同一入口进入。Native writer 锁仍负责跨运行时交接。
- submissions 继续承担发送持久记录与待发送 outbox。忙碌、已有更早 queued 或未知写入时，新直接发送改为 queued；unknown 阻止自动派发，核实后可忽略。
- 队列领取、编辑和取消在账本锁内重新检查状态；已领取消息不能被过期编辑/删除复活或覆盖。provider 交付成功的接受状态落盘后才返回成功。

### 手机与同步

- 未确认 payload 独立保存在 localStorage 的版本化命名空间；按原 ID 重试，busy/idle 变化不改投递模式。旧 sessionStorage pending 自动迁移。
- 恢复卡提供原样重试和移除本地记录。存储失败先阻止提交；未知交付不自动重发。ACK 只删除该 ID 的本地记录，保留后来编辑的内容。
- 新 SSE 客户端请求 `sync=1`，服务端追平后发送 synchronized。ready 表示连接建立，synchronized 表示初始数据已应用，均不代表任务完成。
- 重连使用已应用游标；退役连接的回调被过滤。delta 全部校验后才修改行和游标，补读失败重连，旧 generation 的补读不覆盖新快照。
- activeTurnId 只加入生产 live 元数据，不改共享 timeline 的历史字节契约。原有历史窗口、changes 预算与 snapshot/reset 降级继续复用。

### 验证记录

- 隔离副本按本次补丁验证：`go build ./...`、`go vet ./...`、`go test -p 1 ./...` 全部通过；共享 timeline/service 的历史字节回归通过。
- `node --test tests/test_*.js`：342 项通过；覆盖持久身份、ACK 丢失、忙闲变化、较新草稿、原子 delta、旧连接和旧 generation。
- Windows amd64 网关交叉编译通过；Windows 保留文件 fsync，跳过不支持的目录 fsync。
- 安装的官方 Codex app-server：`TestCLISharedOfficialRuntime`、`TestCLIWriterHandoffOfficialRuntime` 通过，使用独立临时 CODEX_HOME，没有发送模型请求。
- ego-browser：390×844 页面中断网提交→刷新恢复→按原 ID 重试；隔离 ACP Agent 收到消息，账本只有一个 accepted 条目、本地 pending 删除。移除另一条本地记录后，服务端条目仍只有一个。
- 团队回归按任务的 dispatchSessionId 释放实际执行会话；原主控会话不是执行会话，两条下线/换绑回归在 3.4 秒内完成。
- 隔离副本 `go test -race -p 1 -count=1 ./...` 全部通过；最终源文件与验证副本一致。
- 验收实例使用临时数据，浏览器与实例已关闭。未重启用户网关；需用户重启加载新的后端和静态资源。

### 变更文件

- 装配：[cmd/gateway/main.go](../cmd/gateway/main.go)。
- 回执：[ledger.go](../internal/command/ledger.go)、[json.go](../internal/command/json.go)、[serial.go](../internal/command/serial.go)、[ledger_test.go](../internal/command/ledger_test.go)。
- Codex：[bridge.go](../internal/codex/bridge.go)、[cli.go](../internal/codex/cli.go)、[cli_operations.go](../internal/codex/cli_operations.go)、[command_receipts_test.go](../internal/codex/command_receipts_test.go)。
- ACP：[manager.go](../internal/acpruntime/manager.go)、[command_receipts_test.go](../internal/acpruntime/command_receipts_test.go)。
- HTTP：[commands.go](../internal/httpd/commands.go)、[commands_test.go](../internal/httpd/commands_test.go)、[httpd.go](../internal/httpd/httpd.go)、[sessions.go](../internal/httpd/sessions.go)、[queue.go](../internal/httpd/queue.go)、[live_projection.go](../internal/httpd/live_projection.go)。
- 执行会话回归：[team_member_scenarios_test.go](../internal/httpd/team_member_scenarios_test.go)、[teams_integration_test.go](../internal/httpd/teams_integration_test.go)。
- 前端：[app.js](../web/app.js)、[pending-commands.js](../web/pending-commands.js)、[timeline.js](../web/timeline.js)、[transport.js](../web/transport.js)、[i18n.js](../web/i18n.js)、[index.html](../web/index.html)。
- 前端回归：[test_pending_commands.js](../tests/test_pending_commands.js)、[test_transport_sync.js](../tests/test_transport_sync.js)、[test_timeline.js](../tests/test_timeline.js)、[test_queue.js](../tests/test_queue.js)、[test_gateway_goal.js](../tests/test_gateway_goal.js)、[test_plan_feedback.js](../tests/test_plan_feedback.js)、[pending_commands.js](../tests/helpers/pending_commands.js)。
- 实施与验收：[gateway-command-consistency.md](gateway-command-consistency.md)。
