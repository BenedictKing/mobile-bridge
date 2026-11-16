# 网关持续高 CPU 排查（2026-11-16）

## 结论与边界

会话 SSE 的空闲等待反复执行昂贵的完整视图刷新；连接断开后处理协程又不会正常退出，两者叠加可造成持续负载。前者已由当前进程采样核实，后者已在当前源码的隔离 HTTP 测试中复现。尚未测量运行进程中残留 SSE 的数量或两者各自的 CPU 占比。

本次仅诊断，未修改运行逻辑，未调用实际网关 API，未重启网关。原有 4 个 Android 配置文件改动不属于本次任务。

## 运行证据

- 网关 PID 1417，监听端口 8787；进程检查时 CPU 242.3%，运行约 27 小时，累计 CPU 时间 1159 分钟。
- 同次检查 Agent Host、cloudflared 均为 0.0%；该读数只能说明采集时的状态。
- 18:42:06（北京时间）开始的 5 秒调用栈采样再次命中 `streamIncremental → WaitFor → refresh → refreshLiveProjection → acpViewState`。
- 下游主要应用栈包含 `KimiStore.sessionDir → iterLines → jsoncompat.Unmarshal`、`Session.Snapshot → View.Snapshot → Clone`，也包含 `gatewayGoalFor → Manager.Aliases`。
- 二进制记录的 Git revision 为 `d81c8b0066a574cb9bf8d9adadb6f2f65bb5e3e1`，与工作区 HEAD 一致，但构建标记为 `vcs.modified=true`，不能据此声称二进制与当前源码逐字一致。
- 采集时有 5 条已建立 TCP 连接；连接数不能用于计算 SSE 总数、残留协程数或刷新总频率。

## 代码机制

1. `internal/httpd/sessions.go:60` 的 `WaitFor` 在入口、每次 50ms 休眠后及超时后调用 `refresh`。50ms 是等待间隔，不含刷新耗时，并非严格的每秒 20 次。
2. `internal/httpd/server_bridge.go:45` 将 `Live` 绑定到 `refreshLiveProjection`；后者在第 314 行开始构造完整视图，再进入共享时间线投影。
3. `internal/acpview/view.go:156` 的快照深拷贝全部 turns 和 requests。即使后续投影可以复用，前面的快照成本也已发生。
4. `internal/subagents/subagents.go:541` 每次解析 Kimi 会话索引。`Children` 先调用它，再检查 roster 缓存；`Flattener.Activity` 也先读取 Children，才检查自身缓存。
5. `internal/httpd/sessions.go:197` 忽略 Write 错误；普通 SSE（第 1427 行）和增量 SSE（第 1484 行）均未监听请求 Context 取消，循环尾部仅等待 bridge 关闭或 200ms 定时器。`WaitFor` 本身也不接收取消信号。

## 隔离复现

通过 Go `-overlay` 注入临时测试文件，复用项目认证 fixture；业务源码未改变。测试进程与实际网关独立，退出前关闭测试 bridge，确保测试协程完成。

| 检查 | 结果 |
| --- | --- |
| 空闲会话等待约 1 秒 | 无状态变化，仍调用完整刷新回调 22 次（包含首尾额外检查） |
| 普通/增量 SSE 请求取消 | 两种模式均未在 750ms 内退出，继续刷新 11～12 次 |
| 普通/增量 SSE 写入返回 `io.ErrClosedPipe` | 两种模式均未在 750ms 内退出，继续刷新 11 次 |
| 本地真实 HTTP 客户端关闭响应体 | 已确认服务端 Context 取消，13 秒后处理协程仍在，额外刷新 250 次 |

退出断言失败是已复现的代码缺陷，不是环境或依赖失败。真实 HTTP 复现跨过 12 秒心跳周期；隔离测试不能证明实际进程目前具体泄漏了多少协程。

## 修复与验收顺序

1. 普通/增量 SSE、长轮询的等待和外层循环响应请求取消；写入或 flush 失败后退出，并有明确的清理路径。
2. 用轻量版本检查与状态变化通知驱动刷新，变化后再构造视图；并发读写必须避免漏唤醒、快照与版本不一致。
3. 子 Agent 磁盘变化、Goal、连接状态等可能独立于主会话 sequence 变化，需要保留有界的补充检查或独立版本通知，不能只按主 sequence 缓存。
4. Kimi 会话目录索引按文件变化失效缓存；同会话多订阅者共享刷新结果，缓存需覆盖替换、删除与新增会话。
5. 验收包括断线后协程退出、空闲刷新次数受控、长会话多连接开销、增量消息/权限请求/子 Agent/Goal 正常更新及 race 检查。运行服务的 CPU 改善须在用户自行重启后再测量。

本地证据保存在 `.tmp/gateway-cpu-current-symbolized.sample.txt`、`.tmp/gateway-cpu-diagnostic_test.go`、`.tmp/gateway-cpu-overlay.json`、`.tmp/gateway-cpu-diagnostic-results.txt` 和 `.tmp/gateway-cpu-http-disconnect-results.txt`；这些临时文件不纳入版本控制。
