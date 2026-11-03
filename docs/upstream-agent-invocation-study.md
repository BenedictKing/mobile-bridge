# 上游 Agent 调用与特性支持学习笔记

> 状态：调研完成，未实施 · 调研日期：2026-11-03 · 稳定版基线：`refs/codex-mobile-bridge` 主分支 `0721aa7d` / v2.1.7；ZCode 另参考 `v2.2.0-preview.1`

本文只聚焦 Agent 接入与特性支持，路径默认相对 `refs/codex-mobile-bridge/`。核心问题是：一次手机操作最终由哪个进程执行、使用什么原生协议、怎样证明已生效，以及同一个共享按钮在不同 Agent 上为什么必须呈现不同能力。工程门禁和发布文化见 [upstream-v2-study.md](upstream-v2-study.md)。

## 1. 总体模型：统一工作台，不统一 Agent 执行协议

上游没有把 Codex、Claude、DSH、ZCode 强行转换成一种“标准 Agent 协议”。它保留多条原生执行通路，再在适配层输出统一会话、消息、请求、目录与能力字段：

- **Codex App**：桌面 IPC follower；执行仍属于 App 内的 owner。
- **Codex CLI**：官方 app-server daemon；显式 resume 后由原 CLI 运行时执行。
- **Claude Desktop**：注入 renderer 连接器，经签名文件邮箱调用 Desktop 内部 store。
- **Claude CLI**：网关启动并持有 `claude -p` 的 stream-json 双向进程。
- **DSH**：在 Harness 内运行 cordis 插件，经认证回环 HTTP 调用 `sessionController` 等原生服务。
- **ZCode（预览）**：经 loopback CDP 在原 App renderer 内调用原生会话服务，不另起 CLI。

因此“共享”发生在页面、状态模型、草稿、通知、回执与事务保护上；**执行归属、认证、cwd、权限策略和能力证据仍属于各原生客户端**。这也是上游能支持很多特性却不制造第二写入者的原因。

## 2. 各 Agent 的真实调用链

### 2.1 Codex App：跟随原桌面 owner

手机打开会话时先读 SQLite/rollout 历史，再通过 Unix socket 或 Windows named pipe 连接桌面 IPC。广播 follow 后，从匹配 `hostId + conversationId` 的快照识别 `ownerClientId`，之后只接受该 owner 的状态。发送、steer、interrupt、审批和设置分别走 `thread-follower-*` 白名单方法，不允许网页发起任意 RPC（`ARCHITECTURE.md:59-79`、`bridge/clients/codex/ipc.py`）。

辅助能力刻意与执行通路分开：短期官方运行时用于 `model/list`、`skills/list`、空线程创建或本地 Goal 读写，不调用 `turn/start`，也不接管执行。创建空聊天完成 `thread/start → thread/name/set → thread/read` 并确认落盘后，再交回原 App；目录查询不会启动或恢复聊天（`ARCHITECTURE.md:47-57,103-109`）。

### 2.2 Codex CLI：显式接入原生 app-server daemon

CLI 会话先被动读保存历史并查找是否已被 Desktop owner 接管；只有用户点击“继续会话”，才连接或启动已安装的 daemon。`CodexAppServer` 经 `codex app-server proxy --sock …` 连接已有控制 socket，SSH 则在目标主机运行同一 proxy；不是网关另起一套通用执行器（`bridge/clients/codex/app_server.py:127-195`、`cli_session.py:65-84,241-294`）。

接入顺序为 `thread/read` 预检 → 比对保存记录与运行时的 `id/cwd/modelProvider` → `thread/resume(excludeTurns=true)` → 再次核对身份 → `thread/read(includeTurns=true)`。只有显式 attach 后才允许 `turn/start`、`turn/steer(expectedTurnId)`、`turn/interrupt`、`thread/settings/update` 和审批回应（`cli_session.py:295-448,450-568`）。只读超时可重试，写超时或断连则结果未知，绝不重放（`app_server.py:210-238`）。

### 2.3 Claude Desktop Code / Cowork：签名邮箱 + 渲染进程连接器

桌面连接器在 Claude renderer 内访问 `LocalSessions`（Code）或 `LocalAgentModeSessions`（Cowork）。网关与连接器之间不是普通文件队列，而是 HMAC-SHA256 签名的单槽 `request.json/response.json`：每次启动新 generation，请求带过期时间，写文件使用临时文件 + 原子替换，超时请求不重发（`bridge/clients/claude/mailbox.py:1-27,129-162`）。

调用链为 HTTP 操作 → `Claude.dispatch()` → `FileDesktop.call(surface, method, …)` → connector → 原生 store。连接器逐项用 `typeof api[surface]?.method === 'function'` 探测 Code/Cowork 能力；调用前再次检查当前 generation 的 connector revision 与能力（`bridge/clients/claude/connector.js:97-123`、`mailbox.py:189-238`）。因此同一个 `sendMessage` 名称下仍有差异：Code 可传 now/next，Cowork 明确拒绝 steer；Code 可分支/归档，Cowork 不开放。

### 2.4 Claude Code CLI：网关持有的 stream-json 进程

这条通路只管理**网关自己启动并持有的进程，以及确认已关闭会话的显式恢复**，不接管另一终端里活跃 CLI。创建/恢复使用 `claude -p --session-id/--resume … --input-format stream-json --output-format stream-json`，stdin 写用户消息，stdout 读 init/result，stdin 上的 control request 负责 `list_models/set_model/apply_flag_settings/set_permission_mode/interrupt`（`bridge/clients/claude/cli_session.py:702-767,859-908`）。

恢复前重新核验账号指纹、cwd、权限模式和目录 dev/inode，并确认没有其他 CLI writer。新建消息或 SSH 发送还会按原始用户 UUID 回读持久 jsonl，只有同会话、同 cwd 的记录落盘才报告成功（`cli_session.py:702-758`、`remote.py:297-380`）。这条路线支持模型、权限、Skill、附件、审批和中断，但明确不支持 queue/steer/fork（`cli_session.py:838-849,909-945`）。

### 2.5 DeepSeek Harness：原生插件 + 回环 HTTP

网关在 Harness 配置中登记 cordis 插件；插件在 Harness 进程内监听随机 `127.0.0.1` 端口，原子发布 `{port,pid,generation}`。网关读取私有 token 后 POST `/mobile {action,sid,body}`，插件再调用 `sessionController`、`workspaceRegistry`、`permissionPresets`、`sessionSkillCatalog` 等原生服务（`bridge/clients/deepseek/adapter.py:177-234`、`host.mjs:1-6,184-215,299-304`）。

这不是转发 DSH 官方 Web，而是直接操作原生会话模型：创建先用 workspace registry 建立项目成员关系，再 `sessionController.create`；历史用事件序号分页；发送用 `prompt(mode=queue|steer)`；停止用 `cancel`；归档前先停止并轮询原生任务、后代和待审批全部结清（`host.mjs:198-227,257-296`）。

### 2.6 ZCode：原 App 内的原生会话协议（预览）

ZCode 预览版也不另起 Agent CLI，而是连接正常启动原 App 的 loopback 调试端点，在 renderer 内通过 SessionDataLayer 租约共享会话投影。宿主、进程、会话和绑定都有世代；旧世代不能继续写。操作 UUID、原生 revision/logEpoch 和回执查询共同防止超时重发与过期覆盖。

该通路在预览标签中已覆盖发送/queue/steer、原生附件、模型与 thought、Skills、权限、plan、上下文、通知、停止、改名、分支、编辑、压缩、后台任务与 Goal 等入口，但每个写操作仍由原 App 裁决。例如 `requestedDelivery` 可被原生降级为 queue；模型/权限/编辑使用 CAS 或写后读回，不把请求值直接当生效值。

**边界很重要**：这些结论对应 `v2.2.0-preview.1` 的 ZCode 3.17.1 原型与指定 Test 会话，不代表主分支稳定版、任意 ZCode 版本或 Windows/Linux 均已支持。源码入口为 `v2.2.0-preview.1:bridge/clients/zcode/{adapter.py,renderer.mjs,transport.py}`。

## 3. 特性支持矩阵

下表描述适配器**可能开放的原生能力**，实际仍以当前版本、会话状态和逐项能力探测为准；“无”表示明确不提供，而不是尚未接按钮。

| Agent / 通路 | 已实现的共享特性 | 关键限制或降级 |
| --- | --- | --- |
| Codex App follower | 空会话创建、发送/queue/steer、停止、审批、模型/effort/Fast、权限、Skills、编辑/分支、归档、通知 | 执行由原 App owner 完成；Goal 辅助通路不等同于任意 App Server 代理 |
| Codex CLI daemon | 空会话创建、显式恢复、发送/queue/steer、停止、审批、模型/effort、权限、Skills、plan、Goal | 只有显式继续才启动/恢复；编辑/分支不开放；SSH App-owned 会话不提供 Goal |
| Claude Desktop Code / Cowork | 创建并发送首条消息、模型/effort、权限、Skills、图片、审批、停止、改名 | queue/steer/fork/归档仅 Code；Cowork 无 steer；上下文汇总取决于原生接口 |
| Claude Code CLI（本机/SSH/WSL） | 创建/冷恢复、发送、模型/effort、权限、Skill、文件与图片、审批、中断、文件归档 | 仅空闲时发送；无 queue/steer/fork；不接管其他终端的活跃 writer |
| DSH 原生插件 | 项目会话、历史分页、queue/steer、模型/effort、权限、Skills、图片、分支、归档、通知 | 普通消息附件仅图片；无 Codex 式 Goal；冷历史观察不得激活 Agent |
| ZCode 原生预览 | 发送方式、附件、模型/effort、Skills、权限、plan/Goal、上下文、分支/编辑、任务控制等 | 版本与平台受限；原生 queue 降级、CAS、回执和绑定世代不可省略 |

### 3.1 发送、排队与补充当前任务不是同义词

共享输入区有“立即发送 / 完成后发送 / 补充当前任务”，但适配器不能把三者都映射成 prompt：

- Codex App 使用 follower start/steer；CLI 使用 `turn/start` 与带 `expectedTurnId` 的 `turn/steer`，防止补充到已切换的旧轮次。
- Claude Code 用原生 now/next；Cowork 拒绝 steer；Claude CLI 在 active/pending 时直接拒绝，queue 和 steer 都不伪造。
- DSH 调 `prompt(mode=queue|steer)`；ZCode 发 `requestedDelivery`，但最终可能是 queue，界面必须按原生回执显示 queued。
- “已接受请求”“已排队”“已注入当前任务”“模型已完成回答”是四个不同节点，不能共用一个 accepted 就宣称全部完成。

### 3.2 模型、权限与工作模式必须读回原生结果

模型目录、模型设置能力和手动输入能力分别报告。目录为空不代表不能设置，缓存写入失败不代表账号变化；当前模型必须来自实际会话，不用全局 preferred model 代替。Claude Desktop 设置后 `getSession` 读回，CLI 用 `get_settings` 检查 applied；DSH 校验 `selectModel` 返回的 selected；ZCode 用 revision/logEpoch 做 CAS（`claude/adapter.py:916-954`、`claude/cli_session.py:114-194`、`deepseek/host.mjs:282-284`）。

权限也不是统一枚举：Claude Desktop 只开放 default/acceptEdits/plan，Claude CLI 可使用原生六档且高风险项要确认，DSH 选项来自 permission preset catalog，ZCode 映射 build/guarded/yolo。工作模式与权限模式必须分开——DSH 的 modes 可能是权限面，而 collaboration_mode 才是计划/默认工作模式。

### 3.3 Skills 与附件使用各 Agent 的原生表达

“选择 Skill”只是共享交互，落地格式完全不同：Codex 将服务端受信目录项转换成原生 `{type:"skill",name,path}` 输入；Claude Desktop/DSH 生成已安装技能提示前缀；Claude CLI 每条消息只允许一个 `/name`；ZCode 使用原生 composer 的 `$名称` 与文件路径 Markdown 链接。共同点是发送前重新核验 ID、enabled/selectable 和当前会话身份，网页不能提交任意 Skill 路径。

附件同样如此：ACP 可根据 promptCapabilities 选择 image 或 resource_link；Claude Desktop/DSH 使用原生图片 block；Claude CLI 通过受控上传目录和 `--add-dir` 传文件；DSH 拒绝普通文件消息附件；ZCode 走原 App 上传协议。共享上传按钮不能反过来证明所有 Agent 都支持相同文件类型。

### 3.4 上下文、完成状态与通知只信对应证据

上下文圆环至少有四种语义：Codex 最近请求用量、Claude Desktop 原生当前窗口、DSH context pressure（可估算）、ZCode 原生 `usage.contextWindow`。Claude CLI 的 assistant usage/compact boundary 只能标注为最近一次输入，不能冒充当前占用；容量未知就显示未知，不用累计计费 Token 或模型名猜窗口（`docs/deepseek-harness.md:51-57`）。

完成状态也只认原生终止证据：ACP result、Codex turn end、DSH 明确 completed 及事件水位、ZCode turnHeader。idle 可能是错误、中止或数据未刷新，不能据此发完成通知。运行失败/等待审批属于 attention，恢复后可撤销；外部 Webhook 超时属于结果未知，因为对方可能已经收到。

### 3.5 分支、编辑、归档与 Goal 不能靠共享 UI 推断

分支锚点因原生模型而异：Claude Code 使用消息 UUID，DSH 只接受已完成轮次的 turn/end seq，ZCode 使用 `{rowId,entityId,logEpoch}`；编辑还携带读取 revision，且不回滚已执行文件操作。没有原生锚点就不显示分支/编辑，而不是复制正文新建聊天冒充原生分支。

归档先证明无 writer 或先停止精确目标，再执行可逆归档；停止失败、状态未知或存在待审批时不得继续。Goal 也走原生接口：Codex 是 `thread/goal/get/set/clear`，不是把 Goal 文本降级为普通 prompt；其他 Agent 没有等价能力就明确不支持。

## 4. 能力开放机制

上游的能力判断实际有三层，不能合并成一个布尔值：

1. **协议是否提供**：通过连接器/插件方法探测、ACP initialize 与 session 响应确认。
2. **当前会话此刻是否允许**：owner 已接入、任务空闲、无待审批、目录与账号未变化、目标 revision 未过期。
3. **网关是否已实现并验证**：旧 connector 缺新能力字段时全部关闭并提示升级；某平台或客户端版本未实测时不宣称支持。

能力数据随 list/detail/catalog 返回，共享 UI 只按能力渲染，不按 Agent 名称复制业务。适配器负责协议转换和证据核验，执行前再次检查，防止页面打开后原生状态已变化。这个模式比“profile 里静态写 supported=true”更适合多版本原生客户端。

## 5. 对照当前 Go 版

当前 Go 版是“Codex 专用通路 + 统一 ACP 运行时”：

- Codex 通过桌面 IPC follow owner，另有短期 `codex app-server --listen stdio://` 读取模型与 Skills（`internal/codex/bridge.go:64-103`、`internal/codex/catalog.go:89-131`）。
- ACP 通过子进程 stdio 或 Agent Host socket 启动/连接，`initialize` 保存 agentCapabilities，统一执行 `session/new/load/prompt/cancel`、set_mode/config_option 和反向审批（`internal/acp/acp.go:62-109,182-214`、`internal/acpruntime/manager.go:688-838,880-1002`）。
- 当前有 Kimi、MiniMax、Claude、DSH、ZCode、OpenCode、CodeBuddy 七种 ACP profile，再加 Codex 专用通路（`internal/acpagents/acpagents.go:45-65`）。
- 已有方言处理：各 Agent 独立 steer method、effort map、mode map/config option、lazy session ID；附件按 promptCapabilities 放行；DSH 的 collaboration_mode 不会误写权限 modes。
- ZCode 桌面占用会话时已能退回原生 transcript 只读投影，写操作被拒绝，只保留网关分支能力（`internal/acpruntime/manager.go:1946-1973`、`internal/httpd/server_bridge.go:189-201`）。

主要差异不在 Agent 数量，而在调用对象：上游大量能力直接作用于**原 App 已拥有的会话**，Go 版多数 ACP 能力作用于网关或 wrapper 持有的进程。另一个差异是能力证据：Go 的 profile 仍以静态 `[]string{"skills","steer","fork"}` 表达部分特性（`internal/acpagents/acpagents.go:167-305`），上游则进一步结合当前会话状态、原生方法探测、账号/目录身份和写后读回。Go 的设置目前通常以 RPC 成功与返回 configOptions 更新本地视图，尚不能直接等同于原生独立读回或 CAS。

因此不能通过“给 Claude/DSH/ZCode 的 ACP profile 加 capability 字符串”复制上游特性；那只会开放按钮，不会获得原 App 的原生能力。

## 6. 值得落实的改造顺序

1. **先升级能力模型，不增加 Agent。** 将静态 profile 能力与 ACP 协议能力、当前会话运行条件、网关已验证能力分层；至少区分 `native / acp / gateway-synthesized` 来源。分支等网关合成能力要显式标注，不能冒充原生 fork。
2. **把接入过程统一成明确阶段。** `读取保存历史 → 被动发现 owner → 用户显式 attach → 身份预检 → 写操作 → 原生回执/读回`。只读列表和历史永远不进入 attach；writer 变化后旧连接与旧 revision 全部失效。
3. **优先补 Codex CLI 原生 daemon 通路。** 当前已有 app-server 目录客户端，上游又给出完整的 attach、turn、审批、settings、Skills、Goal 映射，迁移成本最低。不要复用短期目录进程执行聊天；执行应连接原 daemon，并保持 owner/provider/auth/cwd。
4. **再评估 ZCode 原生写通路。** 现有原生只读投影可复用，但必须新增显式 attach、版本绑定、loopback 限制、操作 UUID、CAS 和回执查询；不能把 ACP 写成功当成桌面原生写成功。
5. **Claude/DSH 保留 ACP 与原生两条路线。** ACP 适合网关自有会话，原生连接器/插件适合跟随桌面已有会话；二者按 owner 分流，不互相冒名。只有确有“控制原 App 已有会话”的需求时，才值得承担私有协议的版本维护成本。
6. **逐特性做方言合约测试。** 至少覆盖 queue/steer 差异、Skill 序列化、普通文件附件拒绝、模型/权限读回、上下文 measurement、无 Goal 时拒绝、写超时 unknown。测试要证明旧 Agent/旧 connector 会关闭能力，而不只证明最新版成功。

## 7. 关键源码与决策记录索引

- 总约束与 Codex IPC：[ARCHITECTURE.md](../refs/codex-mobile-bridge/ARCHITECTURE.md)、[Codex CLI 接入说明](../refs/codex-mobile-bridge/docs/codex-cli.md)。
- Codex 原生调用：[app_server.py](../refs/codex-mobile-bridge/bridge/clients/codex/app_server.py)、[cli_session.py](../refs/codex-mobile-bridge/bridge/clients/codex/cli_session.py)、[catalog.py](../refs/codex-mobile-bridge/bridge/clients/codex/catalog.py)。
- Claude Desktop：[connector.js](../refs/codex-mobile-bridge/bridge/clients/claude/connector.js)、[mailbox.py](../refs/codex-mobile-bridge/bridge/clients/claude/mailbox.py)、[adapter.py](../refs/codex-mobile-bridge/bridge/clients/claude/adapter.py)。
- Claude CLI：[cli_session.py](../refs/codex-mobile-bridge/bridge/clients/claude/cli_session.py)、[remote.py](../refs/codex-mobile-bridge/bridge/clients/claude/remote.py)、[接入可行性核查](../refs/codex-mobile-bridge/docs/claude-code-feasibility.md)。
- DSH：[host.mjs](../refs/codex-mobile-bridge/bridge/clients/deepseek/host.mjs)、[adapter.py](../refs/codex-mobile-bridge/bridge/clients/deepseek/adapter.py)、[多客户端接入说明](../refs/codex-mobile-bridge/docs/deepseek-harness.md)。
- 关键决策：[CLI 现有 owner](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-existing-owner.md)、[原生模型与手动设置](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-agent-model-settings-fallback.md)、[共享任务状态](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-shared-chat-progress.md)、[停止后归档](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-27-confirmed-disconnect-archive.md)。
- ZCode 预览：[原生会话 README](https://github.com/try2love/codex-mobile-bridge/blob/v2.2.0-preview.1/bridge/clients/zcode/README.md)、[共享能力决策](https://github.com/try2love/codex-mobile-bridge/blob/v2.2.0-preview.1/.agents/notes/implemented/feature/2026-10-31-zcode-shared-capabilities.md)、[传输复用决策](https://github.com/try2love/codex-mobile-bridge/blob/v2.2.0-preview.1/.agents/notes/implemented/architecture/2026-11-01-zcode-bundled-transport.md)。
- 本项目对照：[ACP profile](../internal/acpagents/acpagents.go)、[ACP 传输](../internal/acp/acp.go)、[ACP 会话运行时](../internal/acpruntime/manager.go)、[Codex 目录运行时](../internal/codex/catalog.go)。
