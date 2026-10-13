# Go 迁移方案（网关）

> 状态：**已完成（2026-10-14 收官）**。Go 网关与 Wails 桌面壳已接管生产：Electron 壳与 Node 构建链移除（`52b2787`），Go 网关接入生产 Bridge、Codex 会话创建、LAN 监听、Quick Tunnel 与 SSH 反向转发，Wails 桌面壳托管网关生命周期（`1931cd3`）。收官验收（2026-10-14 独立复核）：`go test ./...` 与 wails 模块测试全绿；`make conformance` Python/Go 双端 26/26 逐字节一致；`make build-matrix` 五平台产物齐备；真实服务双地址监听 8790、`/api/health` 200、LAN `/api/auth` 200、嵌入前端资源可访问。下文各阶段「进行中」批注为过程记录，保留作历史。本文件是「Rust 还是 Go」结论的落地细化与路线图。
>
> 一句话：**迁 Go**；网关和桌面壳统一使用 Go，桌面壳采用 Wails v3；手机 Web 前端保持不变。

---

> **执行归属（2026-10-13 起）：Go 迁移由单一会话串行推进。** 2026-10-13 上午曾出现两场会话并行认领
> 同一模块（acp 传输层），在动手阶段被写入保护拦下、零损失，但已确认并行必然撞车。此后迁移的
> conformance/、internal/、cmd/、go.mod、Makefile 及本文档只由承接会话修改；功能开发（bridge/、
> web/、tests/、wails/）与迁移统一推进。
>
> （2026-10-14 迁移收官，上述归属约定完成使命，此后不再适用。）

## 1. 迁移边界

**迁**：`bridge/**/*.py`、`run.py`、`stop.py` 及网关自身的构建脚本。

**不迁**：
- `web/`：3,781 行原生 HTML/CSS/JS，无构建步骤。原样 `go:embed` 进二进制，手机端与桌面内嵌页零改动。
- `wails/`：Go/Wails 控制面板，直接绑定 Go gateway action 协议和 GatewayControl。
- `site/`、`docs/`、`deploy/`、`scripts/smoke-*.cjs`。

**三条硬约束**（违反任意一条就等于换了个产品）：

1. **HTTP API 兼容**：状态码、关键响应头、gzip、SSE 帧、JSON body 字节。手机端无需重新发版即可连接 Go 网关。
2. **数据目录兼容**：文件名、JSON 字节、`0600` 权限、目录布局不变；新旧二进制可以交替启动同一目录而互不损坏（不能同时写）。
3. **私有协议兼容**：桌面 IPC 帧、ACP NDJSON、停止控制文件与实例令牌协议不变。

---

## 2. 现状基线（实测，2026-10）

| 项 | 数值 |
| --- | --- |
| 网关 Python | **15,217 行**（`bridge/**/*.py`） |
| Python 测试 | **13,514 行，47 个文件，742 个 `test_*` 方法** |
| 前端 | 3,781 行（不含 `web/vendor/`），无构建 |
| 运行时依赖 | **0**（`pyproject.dependencies = []`，纯标准库；可选 `desktop` 组只用 paramiko/keyring） |
| 入口 | `run.py:main`、`stop.py`、`bridge/lifecycle.py` 协作停止 |
| 平台 | macOS / Windows / Linux；Go/Wails 打包 |
| 本机工具链 | go1.27.2、rustc1.99.0、Python3.14.8、Node26.11.0 均已安装 |
| 当前服务 | launchd `com.mobile-bridge.gateway`，监听 8787 |

**按迁移工作量排序的模块**：

| 模块 | 行数 | 迁移难度 |
| --- | --- | --- |
| `service.py` | 2,087 | 高（编排状态机，最后动） |
| `agents/acp_adapter.py` | 828 | 高（5 种方言） |
| `httpd.py` | 815 | 中 |
| `accounts.py` | 771 | 中 |
| `notifications.py` | 699 | 中（对账语义） |
| `catalog.py` | 678 | 低 |
| `jev.py` | 579 | 低（外部 HTTP 顾问） |
| `agents/codex.py` | 521 | 高（依赖桌面 App 私有 IPC + SQLite） |
| `updater.py` | 496 | 中（跨平台替换/回滚） |
| `desktop.py` | 431 | 低（独立 JSON 工作进程） |
| `agents/native_subagents.py` | 387 | 中（三套磁盘布局） |
| `agents/zcode_creds.py` | 385 | 低 |
| `goal.py` / `access.py` / `agents/acp_view.py` / `model.py` / `tunnel.py` | 365/358/330/307/288 | 低-中 |

**持久化文件**（`.local/` 数据目录，需字节兼容）：
`auth-sessions.json`、`config.json`、`connection-vault.json`、`creations.json`、`jev.json`、`submissions.json`、`approval-receipts.json`、`task-inputs.json`、`goal-commands.json`、`message-actions.json`、`notification-*.json`、`address-notifications.json`、`security-notifications.json`、`gateway-control.json`、`gateway.pid`，以及 `agents/<kind>/` 下的 per-agent 缓存。
Codex 侧**只读**：`~/.codex/state_*.sqlite`、`history.sqlite`、`skills-v2.sqlite3`。

**HTTP 面**：
- 13 个全局路由：`/api/{health,auth,login,pair,activity,sessions,projects,account,accounts,accounts/details,accounts/switch,accounts/ignore-submission,account/reset,jev,notifications/defaults}`
- 会话路由：`/api/sessions/<sid>` + 20 个子动作 `events / send / stop / history / respond / reconnect / queue / catalog / settings / permissions / poll / timeline / changes / detail / notifications / uploads / message-action / rename / migrate / input`
- 附加：`goal/{cancel,edit,status}`、`uploads/<id>/{preview,thumb}`、`desktop-images/<sha>`

### 2.1 必须正视的事实：742 个测试不是现成的黑盒规格

实测分类：

- 只有 **12 个** 测试文件真正建立 HTTP 连接（`http.client` / `GatewayServer` / `urlopen`）。
- **46 个** 文件直接 `from bridge import ...`，断言的是 Python 内部 API。
- `test_bridge.py` 一个文件就有 85 个测试，且大量直接构造 `Bridge`、`LiveSession`、mock `DesktopIPC`。

**这意味着「测试套件就是规格书」只成立一半。** 这些测试保护的是 Python 实现的内部结构，Go 版本无法直接跑。P0 的差分测试台不是可选项，而是迁移的前置条件——否则整个迁移没有验收标准。

---

## 3. 语言能力映射

| Python 现状 | Go 等价物 | 风险 |
| --- | --- | --- |
| `threading.Thread` + `Lock` + `Event` | goroutine + `sync.Mutex`/`RWMutex` + `context` + channel | 中：GIL 掩盖的竞态在 Go 变成真并行，锁边界要显式化 |
| `ThreadingHTTPServer` + `ThreadPoolExecutor` | `net/http` + 自建 semaphore | 中：Go 默认无限 goroutine，需按原 `max_workers` 限流 |
| `BaseHTTPRequestHandler` 手写 SSE | `http.Flusher` | 中：分块/keep-alive/头顺序差异 |
| `subprocess.Popen` + 手写 NDJSON | `os/exec` + `bufio.Scanner` | 低 |
| `sqlite3`（只读 Codex 库） | `modernc.org/sqlite`（纯 Go，无 CGO） | 低：避免 mattn 以保交叉编译 |
| `paramiko`（SSH） | `golang.org/x/crypto/ssh` | 中：密钥/known_hosts 行为对齐 |
| `keyring`（系统凭据） | `zalando/go-keyring` 或 `99designs/keyring` | 中：Linux 无 Secret Service 时的降级要与 Python 一致 |
| Unix socket | `net.Dial("unix", ...)` | 低 |
| Windows 命名管道 | `github.com/Microsoft/go-winio`（build tag） | 中 |
| Ed25519 / SHA-256 / zip / tar | `crypto/ed25519` 等标准库 | 低 |
| Python runtime + old desktop shell | Go/Wails 二进制 + `go:embed` | 低 |
| `json` | `encoding/json` + 自写兼容层 | **高**，见第 5 节 |

建议依赖清单（全部纯 Go、可交叉编译）：`modernc.org/sqlite`、`golang.org/x/crypto`、`github.com/Microsoft/go-winio`、`github.com/zalando/go-keyring`。其余走标准库。

---

## 4. P0 必须先钉死的兼容契约

在写第一行 Go 之前，先产出这四份「规格」：

1. **HTTP 契约清单**：每个路由的方法、请求体 schema、状态码、响应 schema、错误码。来源：`httpd.py` 路由表 + 12 个 HTTP 测试 + `web/` 里的 `fetch` 调用点。
2. **磁盘格式样例**：用真实数据目录脱敏后固化为 golden（注意排除 token/密钥）。
3. **IPC 帧规格**：`bridge/ipc.py` 的长度前缀帧 + `bridge/transport.py` 的 socket/pipe 语义（`tests/pipe_fixture.py` 已有等价物）。
4. **停止协议规格**：`gateway-control.json` 结构、随机实例令牌校验、退出码。

---

## 5. JSON 兼容专项（最容易静默出错的地方）

Python `json` 与 Go `encoding/json` 的默认行为差异，逐条列出：

| 差异 | Python | Go 默认 | 影响 |
| --- | --- | --- | --- |
| 分隔符 | `(', ', ': ')`（`indent=None`） | `(',', ':')` 无空格 | 所有持久化文件字节不一致 |
| HTML 转义 | 不转义 | `< > &` 转成 `\u003c` 等 | body 与文件污染 |
| 非 ASCII | `ensure_ascii=True`，转 `\uXXXX` | 原样 UTF-8 | 中文文件字节不一致 |
| U+2028/2029 | 原样 | 总是转义 | 罕见但会踩 |
| map 顺序 | 插入序（dict 保序） | **按键名排序** | 读改写 config 后键序被打乱 |
| 数字类型 | int 任意精度、float 保留 `.0` | 大 int 溢出、`1.0` → `1` | 精度/格式丢失 |
| 尾随换行 | 视调用点 | 无 | golden diff |

**对策**：新增 `internal/jsoncompat`：
- 统一 `Encoder` 设置 `SetEscapeHTML(false)`、自定义 separator、`ensure_ascii` 兼容模式、尾部换行策略。
- 用**保序 map**（`orderedmap`）承载未知键/配置对象，禁止用 `map[string]any` 直接落盘。
- 解码一律 `Decoder.UseNumber()`，数字按原文本透传，只在明确是整数处解析。
- 每个持久化文件配一个 round-trip 测试：Python 写 → Go 读 → Go 写 → 与 Python 原文**字节相等**。

---

## 6. 分阶段路线图

原则：**自底向上移植为纯函数/库，用差分测试验收；最后一次性切换 `main`。** 中间阶段不追求「Go 网关能独立服务」，但要求每个已移植单元都有 golden 对拍。

### P0 · 契约冻结与差分测试台（不写 Go 业务逻辑）

- 新建 `conformance/`：给定一个 `base_url` 和固定数据目录，回放一组 HTTP 请求，抓取规范化响应（status、排序后的关键头、body）。
- 把当前 Python 网关当 **oracle**：先在 Python 上跑两遍确认 golden 稳定（消除时间戳、随机端口、UUID、实例令牌带来的抖动）。
- 对 742 个测试做三分类标注：**A 纯逻辑**（1:1 移植成 Go test）、**B HTTP 行为**（进差分套件）、**C IPC/私有协议**（fixture 驱动对拍）。
- 交付：`conformance/` + `make conformance BASE_URL=...`；一份分类清单。
- 验收：Python 自身对拍零 diff。

### P1 · 骨架 + timeline 投影层

- `go.mod`、`cmd/gateway/main.go`、`go:embed web/`。
- 先迁 `bridge/timeline.py`：纯函数、零 I/O、已有 `tests/test_timeline.py`(10) 与 `tests/timeline.test.js` 双端锚点。
- 验收：同一批 view fixture，Go 输出与 Python **字节一致**；移植 10 个测试；前端 `timeline.js` 不改。

### P2 · 基础设施层

> **进行中（2026-10-13）**。已交付三块（模式与 P1 相同，全部字节对拍全绿）：
> - `internal/ipc` + `conformance/ipc/`：帧协议（长度前缀 + 紧凑 JSON）、13 个方法版本表、
>   `thread-follower-*` 远程 host 版本 bump、发现应答、广播分发规则；含帧解码-重编码往返校验。
> - `internal/auth` + `conformance/auth/`：`session_hours`/`canonical_ip`（含 IPv4-mapped 折叠与
>   zone 透传，Go netip 与 Python ipaddress 在 25 个向量上逐字节一致）/`access_policy`（保序去重）/
>   PBKDF2 密码记录（x/crypto/pbkdf2）/会话有效期与 cookie 寿命数学。错误消息逐字对拍。
> - `internal/lifecycle` + `conformance/lifecycle/`：控制文件字节格式（`json.dumps` 默认分隔符 +
>   `ensure_ascii=True`，中文 `\uXXXX`、emoji 代理对）、停止请求校验、`read_record` 宽容解析。
>   jsoncompat 新增第三种序列化模式 `EncodePythonDefaultASCII`。
>
> - `internal/model` + `conformance/model/`（2026-10-13 第二批，源码已随 bb3f9c3/7dbeeb5 落定）：
>   `normalize_state` 全链（`normalize_item` 15 个分支、实体图 turns、存储工具调用配对、开场白注入、
>   元数据头）、`apply_patches`（含数字路径段与 ~0/~1 转义）、审批规范化（方法白名单、桌面身份校验
>   隐藏、elicitation 表单门、computer-use 授权）、异步问答（原始字符串键匹配）。jsoncompat 新增
>   `indent=2` 模式与深拷贝。**已知上游怪癖按现状固化**：`normalize_item` 中 `if request_id:` 判断的是
>   函数对象（恒真），userMessage 的 `requestId` 无客户端 id 时为 `"None"`——移植未"修复"，如需修正应
>   先改 Python 并重生成 golden。
>
> - `internal/pairing` + `conformance/pairing/`（第三批）：`phone_origin` 校验（含 IPv6/大小写/子域 localhost）、
>   授权生命周期（同源刷新撤销前码、TTL+300 清理、状态查询）、`exchange` 的 8 次尝试限流与 300s 窗口。
> - `internal/store` + `conformance/store/`（第三批）：rollout 历史**解析层**——`history` 的 JSONL → turns/items
>   全分支（含 `**payload` 键序覆盖 quirk：`function_call` 的自身 type 会覆盖 `storedToolEvent`）与
>   `_recent_lines` 的 64KB 倒序分块读取（turn_limit 截断与 `hasLoadedOldest` 完整性标志）。
>   SQLite 发现层（`list`/`get`）已迁入 `internal/store`，使用 modernc SQLite 只读打开 `state_*.sqlite`；
>   `notification_changes`、WAL 无 sidecar 快照复制和真实 service 接线仍留在 P5/P6。
>
> **P2 全部完成（2026-10-13）**：`internal/transport` 补齐 Windows 侧——端点解析（NT 固定管道路径 vs unix socket）
> + build tag 双实现（POSIX `net.DialTimeout` / Windows `go-winio.DialPipe`），三平台
> `GOOS={darwin,linux,windows} go build ./...` 全绿；管道路径 I/O 行为待真机验证。
> 十六个对拍套件全部字节级全绿，`make parity-goldens` 一键重建；`make build-matrix` 五平台产物 8.3–8.9MB。

`config`、`lifecycle`（控制文件/实例令牌/停止）、`store`（Codex 只读 SQLite）、`model`（`normalize_state`/`apply_patches`）、`auth`、`pairing`、`transport`+`ipc`。
- 验收：`test_store/model/auth/pairing/platform/tls` 等移植；IPC 用 `tests/pipe_fixture.py` 的 Go 等价物对拍帧。

### P3 · HTTP 层 + 静态资源

> **进行中（2026-10-13，第一批已过验收门）**：`internal/httpd`（Host/Origin/Sec-Fetch 校验、鉴权+CSRF 门、
> 静态资源路由先于鉴权、错误映射含 401/403/404 文案、Python 默认分隔符 JSON 输出、gzip）+
> `internal/auth` 有状态层（会话/失败计数/封禁/Cookie 渲染）+ `cmd/gateway` 真实接线（env 注入鉴权配置）。
> **`conformance/http --target go`：22/22 响应与 Python golden 逐字节一致（2026-10-13 第二批扩容）**：
> 会话路由四件套（timeline/detail/changes/poll，含 THREAD_ROUTE 正则、host 查询参数解析、
> with_timeline_agents 包装）+ SSE 流（状态帧、增量 ready/changes 帧、`id:` 游标、紧凑 JSON data）。
> 差分台用剧本桥（scripted bridge，两端同 fixture 定值），golden 仅可由 Python oracle 重写（已加保护）；
> `Transfer-Encoding` 列入传输层忽略头（net/http 分块 vs 手写流，均为合法 HTTP）。
> 剩余：uploads、心跳/登出 SSE 向量、13 全局路由中未覆盖者、真实 LiveSession 接线（P6）。

`net/http`、gzip、SSE、路由表、静态 embed、13 全局 + 20 会话子路由 + uploads。
- 验收：P0 差分套件对 Go 后端全绿（业务逻辑此阶段可先接桩或代理到 Python）。

### P4 · ACP 层

> **进行中（2026-10-13，第一批）**：`internal/acpview` + `conformance/acpview/`（35 步 golden，首跑全绿）——
> `acp_view.py` 全量：elicitation 表单支持判定与答案校验（错误文案逐字对拍）、`_output`/`_command_summary`
> 递归扁平化（含 `sort_keys` 兜底与 rune 截断，jsoncompat 新增排序键编码模式）、`_subagent_meta` BFS
> 首见优先、`AcpSessionView` 状态机（10 种 session/update 折叠、回合滚动、计划面板全列表快照、
> 工具调用占位与更新、权限/表单卡片、配置联动、快照键序）。时钟注入使 `startedAt` 确定。
> - `internal/acp` + `conformance/acp/`（第二批，首跑全绿）：ACP stdio 传输——NDJSON 帧（默认分隔符 +
>   `ensure_ascii=False` + `\n`）、顺序整数 id、JSON-RPC 分发（反向请求、`-32000` → AcpAuthRequired、
>   banner 容忍、孤儿响应丢弃、进程退出全员失败）。差分方式：两端各自驱动**同一个假 agent 子进程**
>   （conformance/acp/agent.py），golden 记录逐操作结果与完整线日志（实现发给 agent 的每一行）。
> - `internal/acpadapter` + `conformance/acpadapter/`（第三批，决策切片，12 步 golden 首跑全绿）：
>   附件块构造（能力门控、魔数嗅探、base64、`as_uri` 百分号编码）与 `_set_mode`。**quirk 入册**：
>   未知魔数回退 `image/png`；`availableModes` 缺失时即便目标已是 current 也先报「不支持此工作模式」。
> 剩余：`acp_adapter` 生命周期/队列/审批应答（需 FakeAcpAgent 双端化，建议与 P6 桥接一并）、五个方言适配器。

`agents/acp.py` 传输 + `acp_adapter` 通用基类 + `acp_view` 规范化 + 五种方言（kimi/mcode/claude/dsh/zcode）。
- 验收：`test_acp_transport`(10)、`test_acp_adapter`(55)、各方言测试（dsh20/claude16/zcode18/mcode9）全绿地移植；能力矩阵回归。

### P5 · Codex 适配 + 原生 IPC + 子 Agent 发现（最高风险，后置）

`agents/codex.py`（SQLite 发现 + IPC 控制）、`goal`、`native_subagents`（kimi/zcode/claude 三套磁盘布局）。
- 依赖桌面 App 私有协议与存储，无官方文档、会随 App 版本漂移。
- 验收：`test_bridge.py`(85) 核心用例 + `test_goal_*` + `test_native_subagents`；上线前必须真机验证。

### P6 · 编排内核 `service.py`

> **进行中（2026-10-13，首切片）**：`internal/service` + `conformance/service/`（6 步 golden 首跑全绿）——
> `LiveSession` 视图合并：`set_history` 的快照规范化与非连接态收养、`view()` 的保存前缀拼接
> （触界回合 id 匹配、空不完整快照不清空、不匹配的新历史直通）、`savedHistoryMore` 与连接状态字段追加。
> 这是 P1 时三个依赖 LiveSession 的 A 类测试的能力补齐。
> - `internal/service` 组合链（同日第二批，`conformance/projection/`，首跑全绿）：`LiveSession.view()`
>   → `Timeline.update()` 的真实喂数——timeline_read 的**无标注内核**：投影更新触发（sequence/goal/
>   subagentRevision 三条件）、分页/回填/changes、保存前缀合并下的游标稳定，以及权威完整快照收缩
>   引发的 epoch 滚动与旧游标 RESET。**quirk 入册**：`LiveSession.__init__` 内部构造 Timeline 会消耗
>   一个 epoch id（跨场景共享计数时的账目）。
> - `internal/service` 真实 `timeline_read`（同日第三批，`conformance/timelineread/`，全绿）：真 Bridge +
>   桩 AgentAdapter 子类（空临时目录）驱动**真函数**——`_add_subagents` 的 `subagentRevision:0` 无条件注入、
>   goal 覆盖空路径、meta 增补（host/agent/hostLabel/排序 capabilities/goalRuntimeAvailable/submissions/
>   goalSubmission/forkedFrom，**page 与 changes 都增补、仅 detail 跳过**——对拍器抓到的分歧）、
>   `files` 空数组、更新触发含 subagentRevision。会话预注册 + connected 规避后台 attach 竞态。
> - `internal/service` 提交账本与队列状态机（2026-10-13 第四批，`conformance/submissions/`，首跑全绿）：
>   send 校验链/同 ID 去重（内容全等比对）、dispatch 三分支（accepted / ValueError→failed /
>   TurnNotDispatched→重排队）、**重试上限 10 次**（quirk：手动 `send` 重试不攀升计数——计数入口仅在
>   unknown 状态的重排队）、`mark_submission_failed` 仅作用于 accepted（其余静默无操作→null）、
>   edit/delete/send 队列动作（edit 清 attempt/error 三键回 queued）、ignore 仅接受 unknown/ignored、
>   `_submission_meta` 的可见状态集与 error/lastQueueError 折叠。
> 剩余：通知对账、保存历史边缘扩展（store 接线）、ACP 生命周期（FakeAcpAgent 双端化）。


`Bridge` / `LiveSession` 状态机、去重、队列、submission ledger、`timeline_read` 分页、uploads GC、通知对账。
- 验收：全量差分包 + 可靠性语义专项（approval receipt claim 幂等、`waiting_input` CAS 恢复、通知只补发一次）。

### P7 · 隧道 / 更新 / 分发

cloudflared 监控、OpenSSH 回程、`updater`、`desktop` 控制进程。五平台交叉编译 + 签名 + DMG/NSIS/AppImage。
- Wails 直接嵌入 Go 控制面板与网关 action，cloudflared SHA-256 锁定逻辑继续保留。

### P8 · 灰度切换与回滚

- Wails 产物直接携带 Go gateway，P0 差分套件作为放行门槛。
- 同一数据目录**不可同时**被两个网关写：灰度按「先停后起」的短窗口切换，或用只读镜像。
- 保留可回滚的 Python 包，定义回滚触发条件（HTTP 5xx 比例、通知漏发、审批卡死）。

---

## 7. 风险登记

| 风险 | 等级 | 对策 |
| --- | --- | --- |
| 742 测试多为内部单测，无法直接验收 Go | **高** | P0 差分测试台；三分类标注；预算 30-40% 工作量在测试改造 |
| JSON/浮点/键序字节差异 | **高** | `jsoncompat` + round-trip golden |
| 桌面 IPC 私有协议漂移 | **高** | 保留与 Python 一致的宽容解析；真机验证；版本探测 |
| Go 真并行暴露 Python 被 GIL 掩盖的竞态 | 中 | 每会话 channel 串行化 + 显式锁边界；race detector 进 CI |
| Windows 命名管道 / 无 SIGTERM | 中 | `go-winio` + 沿用控制文件停止协议，不依赖信号 |
| keyring Linux 后端缺失 | 中 | 与 Python 相同降级；集成测试覆盖无后端场景 |
| `modernc.org/sqlite` 性能/兼容性 | 低 | 只读、按列名取值、缺失字段降级；准备 CGO 回退方案 |
| 磁盘格式回写破坏旧数据 | 中 | 上线前双向往返测试；切换前自动备份 |

---

## 8. 收益与工作量

**确定的收益**：
- 五平台 Go 网关单静态二进制，`GOOS/GOARCH` 交叉编译；Wails 桌面壳在目标系统链接原生 WebView。
- `go:embed` 把 `web/` 打进二进制，无需运行时找资源路径。
- 无解释器冷启动、无 pycache、无 Python 版本矩阵（当前 `requires-python >=3.9` 需同时支持 3.9–3.14）。
- 依赖归零（当前网关已是 stdlib-only，Go 进一步免除「本机 Python 环境」这一部署前置条件）。

**待实测后才能写进结论的项**（建议 P0 顺手量）：
- 当前网关常驻内存 RSS（上一轮口头给过 ~150MB，**未实测**）
- Go/Wails 产物体积 / 冷启动时间
- Go 版对应数值

**工作量粗估**：迁移本身约 15k 行 Go，测试改造 5k–8k 行，合计与现有 28k 行体量同阶。分 P0–P8 可独立交付，不存在必须 big-bang 的环节。

---

## 9. 建议的第一步

> **已执行（2026-10-13），P0+P1 交付如下：**
>
> - **P0 测试审计**：`conformance/classify_tests.py` + `docs/go-migration-test-audit.md`（742 个方法三分类 + 阶段映射）。
> - **P0 timeline oracle**：`conformance/timeline/{gen_scenarios.py,oracle.py}`，9 个场景 96 步，Python 双跑字节零 diff；golden 固化在 `scenarios/golden.jsonl`。
> - **P0 HTTP 差分抓取**：`conformance/http/run.py`，15 条脱敏响应（health/登录/CSRF/静态资源/404），双跑零 diff；`make conformance` 验证。
> - **P1 Go 骨架**：`go.mod`、`cmd/gateway`（静态服务 + health 探针）、`embed.go`（`go:embed web`）、`Makefile`。单二进制 10MB。
> - **P1 timeline 移植**：`internal/jsoncompat`（保序 Object/Array、Python 字符串转义、数字原词透传）+ `internal/timeline`。
>   **验收达成：`go test ./internal/timeline` 首跑即与 Python golden 96 步全部字节一致。**
>
> 下一步是 P2（config/lifecycle/store/model/auth/pairing/transport+ipc），照 P1 的「oracle → 移植 → golden 对拍」模式复制即可。

按投入产出，**先做 P0 + P1**：

1. 写 `conformance/` 抓取器，把 Python 网关固化成 oracle（不写 Go）。
2. `go mod init` + `go:embed web/` + `internal/timeline`，用 `tests/test_timeline.py` 的 fixture 完成第一次 Go/Python 字节对拍。

这两步都**不触碰现有运行时**，风险最低，且立刻验证「测试资产可复用」这个最大的未知数。若 P1 对拍顺利，后续阶段照此复制即可；若对拍暴露大量非确定性（时间、UUID、端口），则要在 P0 先把这些抽成可注入的 clock/id 源——这一步对 Go 迁移是纯收益，对 Python 主线也无害。
