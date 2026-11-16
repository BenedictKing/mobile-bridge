# 网关持续高 CPU 排查（2026-11-16）

## 结论与边界

会话 SSE 的空闲等待反复执行昂贵的完整视图刷新；连接断开后处理协程又不会正常退出，两者叠加可造成持续负载。前者已由当前进程采样核实，后者已在当前源码的隔离 HTTP 测试中复现。尚未测量运行进程中残留 SSE 的数量或两者各自的 CPU 占比。

最初两轮仅做诊断与重启后复测；用户随后批准实施修复，结果见文末。未调用实际网关 API，未自行重启网关。原有 4 个 Android 配置文件改动不属于本次任务。

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

## 用户重启后复测

- 用户自行重启后，网关 PID 为 7442，仍监听 8787。首次检查已运行约 3 分 49 秒，CPU 为 18.7%；Agent Host 和 cloudflared 均为 0.0%。
- 2026-11-16 19:15:26 至 19:16:28（北京时间），用 `top` 连续观察约 62 秒。排除首个没有区间基线的 0.0% 读数后，12 个区间 CPU 为 32.7%～55.8%，算术平均 42.2%；线程数 25～26，内存读数约 107～166 MB。
- 5 秒调用栈采样仍命中 `streamIncremental → WaitFor → refreshLiveProjection → acpViewState → KimiStore.sessionDir / View.Snapshot`，空闲轮询中的昂贵工作依然存在。
- 新二进制 revision 为 `a3b21573174422330a4a45dbf234fb16bbdf43e6`，带 dirty 标记；该提交只新增本诊断文档，目前没有针对上述运行逻辑的修复提交。
- 结论：重启后的 CPU 已明显低于此前 242.3% 的单次观测，但仍持续消耗约 0.42 个 CPU 核心。两次并非相同负载的受控性能对比，不能据此精确归因或证明根治；断线退出和刷新开销仍需修复。
- 本次未操作浏览器连接、调用实际网关 API或再次重启。证据：`.tmp/gateway-after-restart-7442-top.txt`、`.tmp/gateway-after-restart-7442.sample.txt`、`.tmp/gateway-after-restart-7442-symbolized.sample.txt`。

## 修复实施与本地验收

- SSE 与长轮询的等待接入请求取消、网关关闭信号；普通/增量/Team SSE 写入与 flush 出错后退出，单次写入最多等待 10 秒。网关 HTTP shutdown 先通知流退出。
- ACP/Codex 变更通知驱动等待，先订阅再读取以避免漏唤醒；ACP 快照和源版本在同一把锁下读取，主动关闭也递增版本并唤醒等待者。
- 每会话共享完整视图和时间线投影；源版本不变时不深拷贝历史。空闲等待每秒补查一次独立元数据，Goal、子 Agent、队列、分支来源变化可推进投影版本。显式时间线读取立即检查元数据。
- Codex 桌面重连异步执行，同一会话合并尝试并限制重试频率，避免网络发现阻塞 SSE 取消；跟随通知的读取也加锁以避免并发漏唤醒。
- Kimi 索引按文件身份、大小和修改时间缓存；缺失项也复用索引，替换、删除、追加会失效，未写完整的尾部不固化为有效缓存。

验证结果：

- 全仓库 Go 包均已验证通过：全量执行中新增 Codex socket fixture 暴露 macOS 路径长度限制，修正后 Codex 包单独完整复测及 race 通过；其余包在全量执行中通过。
- 定向 race 覆盖 HTTP 并发读、流式 delta、取消/写入/flush 错误、路由认证撤销、共享投影、Goal/子 Agent 独立变化和 Kimi 缓存；10 次真实本地 HTTP 断开均在 1 秒的测试上限内退出。
- `go vet` 检查受影响包与网关入口通过，最终网关二进制已编译到 `dist/gateway-go`，未安装或替换运行服务。
- HTTP golden 对拍仍有唯一 `index-gzip` 摘要差异。用 Git HEAD 修改前源码覆盖构建并复测，诊断输出逐字相同，因此未更新 golden 或修改无关首页。
- 相同 fixture 的 3 轮微基准：共享空闲检查 177.3～179.5ns/op、0B/op、0allocs/op；完整 ACP 视图 2730～3372ns/op、4936B/op、43allocs/op。fixture 是单轮长文本，数字不代表真实多会话整进程 CPU 降幅；每秒元数据补查仍有少量开销。

本地结果见 `.tmp/gateway-cpu-all-tests.txt`、`.tmp/gateway-cpu-race-tests.txt`、`.tmp/gateway-cpu-reconnect-tests.txt`、`.tmp/gateway-cpu-vet.txt`、`.tmp/gateway-cpu-benchmarks.txt`、`.tmp/gateway-cpu-http-conformance.txt` 和 `.tmp/gateway-cpu-baseline-conformance.txt`。

尚待用户自行重新构建/重启运行服务后，以相同会话与浏览器连接数量观察 CPU、反复开关页面后的负载及实际消息流。当前运行进程尚未完成修复后的线上验收。

## 修复版运行观测与 Codex v13 兼容补充

用户再次重启后，PID 33310 的构建 revision 为 `151e7db7cfa316eabcf4b9d768b754699eae28a3`（dirty 标记）。20:15:19～20:16:21（北京时间）连续 12 个区间的 CPU 为 4.4%～23.0%，平均 13.59%；此前重启基线平均为 42.21%。本次有 7 条网关入站连接，线程数 24。采样主要命中会话列表发现/Claude 历史解析，没有再次命中原来密集的 SSE 全量刷新链。

不过用户随后指出当前 Codex 会话不更新，因此上述约 68% 的观测降幅只能作为阶段性记录，不能作为正常消息流下的最终性能验收。

具体原因与修复：

- ego 中生产页面仍显示未连接，时间线接口返回 `connected: false`，仅显示上一轮历史。
- 直接观察该会话的桌面 IPC：初始化和所有者发现成功，实际广播版本为 **13**，包含 canonical `turnHistory` 快照及带 `baseRevision/revision` 的 JSON patches。
- 网关原先仅接受版本 **11**；该硬编码在 CPU 修复前已经存在。v13 消息被静默丢弃，和浏览器登录状态无关。
- 新增明确的 v11/v13 兼容，继续使用现有 canonical 历史与 JSON patch 解析；不盲目接受未知未来版本。未知版本保留连接错误并记录日志，相同错误不逐事件重复唤醒。
- `stream_version_test.go` 覆盖两种版本的快照、canonical 增量、订阅唤醒、未知版本错误与恢复。捕获的真实会话 1 个快照和 5 个增量也已逐条成功回放，每次归一化均保留 5 个 turn。
- Codex/model/IPC 完整 race 测试、HTTP 流式与并发投影定向 race、vet 和构建通过。
- 在本机随机端口的独立只读预览中，使用真实桌面 IPC 和 ego 页面验证：`connected: true`、`status: active`、`sequence: 39`；页面不刷新即可自动出现新发送的本轮进度文字。临时预览已关闭，没有发送测试聊天消息，没有重启 8787 服务。

本次源码修改仍需用户加载新版本后再验证 8787 页面及正常更新时的 CPU。证据：`.tmp/gateway-fixed-33310-summary.json`、`.tmp/gateway-fixed-33310-top.txt`、`.tmp/gateway-fixed-33310-symbolized.sample.txt`、`.tmp/codex-v13-before.txt`、`.tmp/codex-v13-tests.txt`、`.tmp/codex-v13-replay-results.txt`、`.tmp/codex-v13-http-tests.txt`、`.tmp/codex-v13-live-preview.png`。包含真实会话内容的原始 IPC 捕获仅保存在忽略目录 `.tmp`，不纳入提交。
