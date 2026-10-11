# 实现说明

## 目标与执行边界

手机网页控制 Codex App 聊天，并可在已保存项目中新建空聊天。网关不拥有模型执行器，不替换已有聊天。已有聊天的执行与审批操作绑定 `hostId + conversationId + ownerClientId`，由 App 中相应 owner 执行。重命名是独立的元数据操作，使用所选主机的短期官方运行时调用 `thread/name/set`，不加载执行任务、不调用 `turn/start`，也不改变 provider、认证或审批策略。

默认继承已有会话的设置。显式选择模型时更新 `model`、`effort` 或速度档位。账号切换是独立的全局操作：仅桌面端添加凭据，双端选择已保存接入，退出官方桌面后替换本机认证及默认 provider，再启动核验。已有聊天的 provider 和审批策略不直接迁移；详情见 [账号与接入](docs/account-switching.md)。

## 原生 Goal 控制

本地 Goal 元数据通过受限的官方运行时连接读写，只允许 `initialize` 和 `thread/goal/get/set/clear`；该连接不加载或恢复聊天，不调用 `turn/start`，也不修改 provider、认证或审批设置。原桌面 owner 继续拥有实际执行和审批。当前 follower IPC 没有 Goal 写接口，SSH Goal 因此禁用；不能把此通路当作任意 App Server RPC 代理。

创建和每一次恢复有独立的持久化操作 ID；该操作的重试沿用 ID 和启动消息，后续恢复不能复用历史启动 ID。写入断连、超时和重启遗留的 sent/pending 均作为结果未知处理，只依据可确认的原生状态核验，不自动重放写入。手机提交所见目标的身份和状态，拒绝对已经变化的目标执行新操作；原生接口尚无原子条件更新，桌面与手机同时修改仍应避免。

修改目标要求 paused，通过官方 set 替换内容，重置统计、保留预算且保持 paused。暂停、修改和关闭撤销未发送的启动消息；队列派发前再次要求匹配的 active 目标。暂停/关闭本身不发送 turn interrupt，用户需要立即中断当前回复时使用独立的停止操作。

## 会话发现

本地通过只读 SQLite 连接读取 `$CODEX_HOME/state_*.sqlite`，筛选桌面聊天并排除子代理。尚无 IPC 快照时，可读取原始会话记录作为历史展示。WAL 模式且 `-wal` 缺失时，先复制到私有临时目录，并核对复制前后主文件的身份、大小和修改时间以及 WAL 仍不存在，再读取副本；并发变化时丢弃副本并等待重试。已有 WAL 时直接只读查询，保留最新提交。发现与历史读取不直接写入原数据库或会话文件；用户新建聊天时由官方运行时持久化新记录。

发现兼容 `Codex Desktop`、`codex_work_desktop`、网关创建来源 `codex_mobile_bridge`，以及 `originator` 为空且 `source=vscode` 的旧桌面记录。列表与单会话读取使用同样的来源和子代理限制。

HTTP 阅读请求先返回，保存历史和实时连接在后台加载，通过原有 SSE/长轮询更新页面。每条会话只发起一个后台连接任务，失败后间隔重试，手动重新连接可跳过重试间隔。SSH/SQLite 元数据读取不占用会话总锁；写操作仍须确认原生 owner 已连接。只读超时与写入结果未知使用不同提示。

网页点击或打开会话时，并行读取渐进历史并通过带 CSRF 的重连接口自动连接原生 owner。首屏不等待激活完成，切换会话取消旧页面请求，首次历史读取失败会自动重试。后台阅读、轮询和通知监听本身只订阅状态。打开会话、发送、处理授权或点击“重新连接”时，优先等待后台订阅；仍无 owner 才通过已有 `codex://threads/…` 链接让原 App 加载聊天，最多等待约 20 秒。该兜底可能切换桌面当前聊天，不调用第二个 app-server，不改 provider。连接失败时操作尚未发送，草稿保留。原 App 因 provider 缺失等配置问题无法恢复的聊天，网关也不会擅自迁移。

项目与 SSH 主机映射来自 App 的 `.codex-global-state.json`。远端只读取 App 已保存且有关联项目/会话的 SSH 别名；只读辅助脚本使用 `ssh` 和远端 Python，不向远端安装文件。

SSH 参数包含非交互认证、严格主机指纹检查和禁用额外转发。远端模型/Skill 目录由远端 Codex 运行时读取，不复用 本机文件路径。

本地与远端列表在合并后按最近交互时间排序、分页。项目优先采用显式归属，其次按最长工作目录前缀匹配。项目键带主机 ID，避免同名项目相互混淆。

## 新建聊天

登录后的 `GET /api/projects` 返回 App 已保存的本机及 SSH 项目；`POST /api/sessions` 接受项目键、名称和请求 UUID。服务端重新解析已保存项目，不接受客户端任意目录、主机、程序路径或配置覆盖。创建接口沿用 Host、Origin、登录和 CSRF 校验。

`bridge/create.py` 使用所选主机的官方运行时完成 `initialize → thread/start → thread/name/set → thread/read(includeTurns=true)`，随后关闭 stdio 并等待进程退出。读取完整空历史是必需步骤：仅设置名称会留下目录元数据，却未必落盘可恢复的 rollout。整个辅助进程不调用 `turn/start`，不会发送模型任务。新线程使用该主机、工作目录的运行时默认配置；不克隆已有聊天的临时设置。

创建结果按 UUID 写入网关自己的 `creations.json`，重试复用同一聊天；请求可能提交但结果未知时不自动重放。辅助进程退出后，通过 `codex://threads/<id>?hostId=...` 打开原桌面 App，桌面接管后才允许发送和审批。此步骤会切换桌面当前页面；打开失败保留已创建 ID。SSH 创建借助已有非交互 SSH 通道执行相同辅助代码，不在远端安装网关。

新建入口与原生 IPC 均受 App/运行时版本影响。Mac 空聊天创建和实际 App 接管已验证；Windows 及 SSH 创建目前只有模拟路径检查，不能视为实机通过。

## 原生 IPC

macOS 使用 `$CODEX_HOME/ipc/ipc.sock`，Windows 使用 `\\.\pipe\codex-ipc`，两者均为 4 字节小端长度前缀，后接 UTF-8 JSON 帧。`--ipc-path` 可覆盖地址。

Windows 使用 CPython 标准库 `_winapi` 的 overlapped I/O 读取原始字节流，不使用 multiprocessing 的消息封装。关闭时先取消未完成 I/O，等待其结束后再释放 handle；发送有超时，空闲读取可被停止操作唤醒。不会另起 agent 替代桌面 owner。

| 操作 | 当前实现 |
| --- | --- |
| 初始化 | `initialize`，获得当前 IPC client ID |
| owner 识别 | 匹配 host / conversation 的订阅快照来源 |
| 会话订阅 | `thread-stream-following-changed` |
| 状态 | `thread-stream-state-changed` 的 snapshot / patches |
| 新消息 | `thread-follower-start-turn` |
| 补充与停止 | `thread-follower-steer-turn` / `thread-follower-interrupt-turn` |
| 模型设置 | `thread-follower-update-thread-settings` |
| 历史加载 | `thread-follower-load-complete-history` |
| 授权与回答 | 对应的 command/file/permissions/user-input/MCP follower 方法 |

本地和 SSH 均先广播带 `hostId` 的订阅，从匹配会话的快照识别 owner，后续只接受该 owner 的状态。首次短等待结束后保留订阅，以接收延迟到达的快照；不把尚未加载的聊天当作历史读取错误。远端 follower 操作携带外层 `hostId`，协议版本按当前 App 约定调整。

这些方法来自当前安装版本的协议适配，不代表 OpenAI 对该接口稳定性的承诺。仓库仅分发网关实现，不分发 App 包或提取出的 App 源文件。

## 状态与重连

网页接收规范化后的会话快照。原生 patches 只有在 `baseRevision` 与本地一致时应用；失配时重新订阅完整快照。历史、工具输出、文件差异和待回答请求来自该状态。

桌面分页快照只含最近若干轮时，展示层按首个重合 turn ID 保留保存历史的前缀，再接上实时内容；完整快照仍具有最终权威，可正确反映回退或删除。此合并不改变原生 patch 基础，也不从保存历史推断新的待授权请求。

局域网默认使用 SSE，Cloudflare Quick Tunnel 使用有登录校验的长轮询；SSE 连续失败后也会回退。切换聊天时用代次标识排除旧连接回包，浏览器草稿和 Skill 选择按主机与会话隔离。

## 消息与审批

每条手机消息携带唯一提交 ID；发送前把状态写入私有记录。确认失败时保留 unknown 状态，不自动重放。排队消息在原会话空闲时发送，用户可撤回。SSH 主机分别保存提交记录。

审批回应绑定当前仍存在的 request ID，不允许通过 HTTP 发起任意 RPC。命令、文件和权限请求仅支持实现中明确允许的决定；过期或不支持的请求不会转发。

App 的异步问题通过原生 questionItemId 格式回答。若原任务仍在运行，则补充当前任务；否则在同一聊天启动回答该问题的新一轮。

## 模型与 Skill

Windows 自动发现用户目录中的 App 运行时、常见安装目录、MSIX 包及 PATH；`--codex-bin` 可明确选择桌面对应版本。仅本地目录查询使用该覆盖，SSH 目录仍由远端运行时读取。

目录辅助进程只允许 `initialize`、`model/list`、`skills/list`。它不会调用 `thread/start`、`thread/resume` 或 `turn/start`。

网页选择的 Skill ID 来自目录，服务端按 ID 解析受信目录项，再转为原生 `{type: "skill", name, path}` 输入；网页不能直接传入任意技能文件路径。

## HTTP 与文件

账号密码采用 PBKDF2-HMAC-SHA256，登录 Cookie 使用 HttpOnly、SameSite，HTTPS 入口加 Secure。Host 和 Origin 必须在允许列表；写操作还需 CSRF 令牌。

聊天内容、事件流、长轮询和附件接口均需要登录。文件访问仅允许当前聊天引用的本地工作目录或 visualizations 文件，并校验解析后的真实路径及大小。远端文件不映射到 本机文件系统。

## 模块

桌面控制器通过白名单 Electron IPC 调用 `desktop.py` 的独立 JSON 工作进程；网关启停、更新和网络配置管理不监听网络。通知配置只在电脑端修改，沿用 Host、Origin、登录及写入 CSRF 校验，不返回 Token。该配置影响整个网关的关注聊天，不按登录设备隔离。打包版本执行随包提供的 PyInstaller 运行时，网关以独立后台进程运行，退出控制器不会停止网关。Windows 使用 NSIS 安装包或完整 ZIP 目录，避免临时自解压目录随控制器退出被删除而破坏后台运行时。

Windows `desktop/tray.cjs` 管理托盘状态、手机地址和两种退出动作；仅显式停止才调用网关的协作停止协议。主窗口关闭时隐藏，第二次启动恢复原窗口；退出前销毁托盘并允许窗口关闭。状态请求合并，菜单操作避免重复提交；停止失败保留控制器并显示错误。macOS 不创建此托盘，保留原窗口生命周期。

网关停止使用带随机实例令牌的本地控制文件，由服务主动调用 `shutdown`，再关闭隧道、IPC 和 HTTP。`stop.py --config` 与启动配置对应，不依赖 Unix 信号或 Windows PID 强制终止。文件统一使用 UTF-8；Windows 权限继承目录 ACL。

| 模块 | 职责 |
| --- | --- |
| `run.py` / `stop.py` | 启动参数、进程记录、停止 |
| `bridge/ipc.py` | 原生帧传输、请求白名单和事件分发 |
| `bridge/transport.py` | Unix socket / Windows 命名管道字节流 |
| `bridge/lifecycle.py` | 跨平台停止请求与实例令牌校验 |
| `bridge/service.py` | 会话同步、操作路由、去重和队列 |
| `bridge/agents/` | Agent 适配层：base 接口与 Registry、codex 实现、ACP 传输与 kimi/mcode/claude/dsh/zcode 方言 |
| `bridge/store.py` / `remote.py` | 只读发现、SSH、项目与主机映射 |
| `bridge/model.py` | 历史和请求的规范化 |
| `bridge/catalog.py` | 模型与 Skill 元数据 |
| `bridge/create.py` | 创建并持久化空聊天，退出辅助运行时，交给桌面接管 |
| `deploy/nas/` | NAS HTTPS 反代教程及可选 Docker 入口 |
| `bridge/auth.py` / `httpd.py` | 登录、同源校验、HTTP、SSE、轮询 |
| `bridge/files.py` | 引用文件的范围校验与解析 |
| `bridge/tunnel.py` | 临时 HTTPS 隧道生命周期与 cloudflared 共用监控基类（指标探测、休眠时钟跳变检测、terminate/kill 升级重启与封顶退避） |
| `web/` | 手机浏览器界面 |
| `tests/` | 合成数据与模拟 App IPC 回归 |

## 多 Agent 适配

### 控制模型

Codex 与 ACP 两类 Agent 的控制面完全不同。Codex 由桌面 App 宿主：网关只读其 SQLite 会话库做发现，运行控制走桌面 App 暴露的原生 IPC（`bridge/ipc.py` 帧协议），任务存续依赖桌面 App 进程。Kimi（`kimi`）、MiniMax（`mcode`）、Claude（`claude`，经 `@agentclientprotocol/claude-agent-acp` 包装层）、DeepSeek（`dsh`，经 `@openma/deepseek-harness-acp` 包装层）与 ZCode（`zcode`，经 `zcode-acp-server` 包装层）是 CLI 型 Agent，没有桌面宿主：网关在进程内直接 spawn 它们的 ACP 子进程，会话发现、历史、发送、审批、中断全部走 ACP JSON-RPC。`run.py --agents codex,kimi,mcode,claude,dsh,zcode` 决定注册哪些后端，默认仅 codex。

### Adapter 接口与路由

`bridge/agents/base.py` 定义 `AgentAdapter` 抽象：会话列表/元数据、历史快照、发送与排队、审批响应、中断、目录（模型/Skill）、能力集合（`Capability`）与 `label`。`bridge/agents/codex.py` 是既有实现；`acp_adapter.py` 是 ACP 通用基类，`kimi.py`/`mcode.py`/`claude.py`/`dsh.py`/`zcode.py` 只声明启动命令、能力与方言差异。claude 的发现依次读 `CLAUDE_ACP_BIN` 环境变量、PATH 上的 `claude-agent-acp`、`npx -y @agentclientprotocol/claude-agent-acp`；dsh 同理（`DSH_ACP_BIN` → `dsh-acp` → `npx -y @openma/deepseek-harness-acp`）；zcode 同理（`ZCODE_ACP_BIN` → PATH 上的 `zcode-acp-server`/`zcode-acp` → `npx -y zcode-acp-server`，凭据取自 `~/.zcode`，包装层需要 Node.js 22+）；npx 首次运行要下载包装层，握手机超时随子类上调。

每个后端包装为一个 `Bridge`（`bridge/service.py`），`Registry` 维护 kind → bridge 映射。会话路由先查各 bridge 的 `listed` 集合（列表时填充）；未命中时（如重启后直接打开深链）逐个探测非 codex adapter 的 `session_meta`，命中即缓存，避免 `session_*`/`mvs_*` 标识落入 codex 的 UUID 校验而误报 400。HTTP 层按 `?agent=` 或该路由选择 bridge。

### ACP 传输与视图

`bridge/agents/acp.py` 实现 NDJSON JSON-RPC 传输：initialize 能力协商、`session/new`/`list`/`load`/`prompt`/`cancel`、`session/set_config_option` 与 `session/set_mode`，入向 `session/update` 通知与 `session/request_permission` 请求。`acp_view.py` 把 ACP 更新流规范化成与 codex 相同的 view 形状（turns/items/requests/状态），前端因此无需感知传输差异。`session/list` 结果带 15–30 秒缓存与后台刷新，避免每请求 spawn。

claude-agent-acp 只在首条消息后才把会话落盘：零消息会话跨进程 `session/load`/`resume`/`prompt` 均报 "Resource not found"。因此 `AcpAdapter.create_session` 对 `KEEP_CREATED_PROCESS` 的子类（claude）把创建进程保活为会话 handle（配置在 `session/new` 响应里直接播种，跳过 load/replay），首条消息就在创建进程内发出；会话入列前由 `_pending_created` 占位补进列表与 `session_meta`，agent 一旦列出即撤占位，保活进程被 detach 的未入列会话同时撤占位。

dsh 的语义与 kimi 相反方向地不同：modes 与 `mode` 配置项是权限预设（read-only/workspace-write/danger-full-access），计划模式在独立的 `collaboration_mode` 配置项（default/plan）里。`DshAdapter._set_mode` 因此把桥接层 plan/default 映射为 `session/set_config_option` 写 `collaboration_mode`（服务端内部执行 `/plan` 命令），绝不调用会改权限预设的 `session/set_mode`；`AcpAdapter.VIEW_CLASS` seam 让 `DshSessionView` 忽略 modes、`mode` 配置项与 `current_mode_update`，只把 `collaboration_mode` 映射进 view.mode。dsh 的 steer 走 `_session/steering` 扩展（`prompt` 块加 `_meta.steering.idleBehavior: promptRequired`），返回 `{outcome: 'injected'}` 或 `{outcome: 'promptRequired'}`——后者说明任务恰在竞态间隙结束，适配器回落为普通 prompt。dsh 创建会话即入列，优雅关闭释放写句柄后跨进程 load 正常，故 `KEEP_CREATED_PROCESS=False`（强杀进程会留下 "already owned by an active write handle" 残留，需避免）。

zcode（经 `zcode-acp-server` 接入 ZCode CLI 的 app-server）的方言：`session/new` 返回惰性占位 id，后端会话在首次 prompt 前不存在，占位因此从不入列——与 claude 同因走 `KEEP_CREATED_PROCESS=True`（创建进程保活为 handle）。ZCode 3.3.0 的会话模式是 plan/build/edit/yolo/auto，没有桥接层的 `default`：`ZcodeAdapter._set_mode` 把 plan 原样透传、default 映射为包装层建会话时硬编码的 `yolo`，统一走标准 `session/set_mode`（zcode 的 `mode` 配置项与 modes 是同一状态，无需 dsh 式的视图隔离）。推理强度词表按模型变化（GLM-5.3 为 low/high/max 恒等映射；GLM-5-Turbo 只有 enabled/off，折叠到桥接层 minimal/none 两档），模型值形如 `providerId\modelId`（服务层模型 ID 校验因此放行反斜杠）。上游 ZCode CLI ≥0.16 删除了 steer/rewind，zcode 不声明 STEER 能力。

### Capability 门控

view 携带 `capabilities` 字符串数组与 `agent` 字段，前端据此门控：无 `steer` 隐藏"补充当前任务"选项；无 `goals` 隐藏目标模式；无 `skills` 隐藏 Skill 按钮；fork/edit 按钮依赖每行 `forkable`/`editable`（ACP 会话天然为假）；Fast 模式与额度档位随目录 `fastMode.allowed` 隐藏；`fullHistory` 由快照 `historyComplete` 决定。审批卡片对 `acp/requestPermission` 按 agent 返回的 `options` 动态渲染，响应只回传 `optionId`（或 `cancel`）。状态文案与会话行标题按 `BridgeI18n.agentLabel(view.agent)` 显示 Agent 名称。通知路由见 `bridge/notifications.py`：watch/policy 接受非 UUID 会话标识，经 Registry 路由到对应 bridge；推送标题使用 adapter 的 `label`；`notification_candidates` 的 SQLite 全局发现仍是 codex-only，ACP 会话依赖显式关注。

### 生命周期限制

ACP 会话的任务存续依赖网关进程：网关停止时其 spawn 的 Agent 子进程随之退出，进行中的 turn 中断且不续跑。网关重启后，`Notifications.resume_watched`（`bridge/notifications.py`）在后台对每个 watched 的 ACP 会话重新 `session(background=True)` 附接，通知监控随之恢复；单个会话失败只记日志，不影响其他会话或启动。未被关注的会话不自动重连，打开聊天时再附接。codex 会话不受此限（任务由桌面 App 持有）。

多 Agent 也可用环境变量启用：`--agents` 未显式给出时读 `MOBILE_BRIDGE_AGENTS`（逗号分隔），未设置默认 codex。桌面打包 App 因此无需改动即可注入多 Agent（macOS 可用 `launchctl setenv` 后启动 App）；PyInstaller 静态分析跟随函数内的条件 import，`bridge/agents/` 全部模块随包打进。

### 会话迁移

权限请求保留 Agent 提供的具体命令、工作目录、网络上下文和文件变更，前端按请求类型显示可审计详情及风险标签；批准动作仍由 Agent 原生选项和用户明确点击决定。Jev 可作为后续风险分类的可选辅助，不能直接授予权限，未配置时使用本地确定性标签。

`GET/POST /api/sessions/<sid>/permissions` 读取或切换 ACP Agent 暴露的权限模式。Codex 当前只保留桌面权限上下文并显示单次审批，未通过未经确认的 IPC 字段写入桌面权限策略。

`POST /api/sessions/<sid>/queue`（body `{id, action, text?}`）管理排队消息：`send` 在当前任务空闲时立即发送，`edit` 修改待发送文本，`delete` 删除排队项。队列项在当前任务完成后自动发送；目标/计划激活消息不可编辑。

队列策略按聊天持久化，`action=disable/enable` 控制是否允许新消息进入队列；关闭排队不会删除已经排队的消息。

子 Agent：Codex 本地与 SSH 会话从 `thread_spawn_edges` 读取 child thread；ACP Agent 从 `tool_call` 的嵌套输出读取子 Agent transcript。时间线保留子 Agent ID、类型、状态、停止原因和命令输出，并将其作为可展开 activity 展示。

`POST /api/sessions/<sid>/migrate`（body `{agent, scope?, project?, id, uiLocale?}`）把当前会话的上下文搬到另一个（或同一个）Agent 的新会话。`scope` 选择复制范围，默认 `main`：

- `main`：主要历史。user/assistant 保留全文，activity 压缩成"标题+首行"一行，输出丢弃；上限为最近 20 条且总量 30k 字符（单条 4k 截断），超出从头部丢弃并注明。
- `full`：完全复制历史。同样的摊平规则，预算放大到 500 条 / 90k 字符（单条 12k，总量仍低于 send 的 100k 上限）。codex 源的保存历史是分页的，`Bridge.migrate` 会先把 `history_limit` 置为 None 重读完整 rollout（读取失败直接报错，不静默截断头部）；ACP 源附接时已是完整历史，无需扩展。
- `latest`：只复制最新提问和回答。仅保留 user/assistant 角色，从最后一条 user 消息截到结尾（无 user 时只留最后一条助手消息），预算 12 条 / 40k 字符（单条 20k，长计划不被单条上限切断）；注入消息使用独立的"最新问答"头尾文案。

源会话只读：`Bridge.migrate` 先阻塞附接取到规范化 view，`migration_transcript` 按 scope 摊平转录。目标项目默认按源会话 cwd 匹配目标 adapter 的 `projects()`；本机源匹配不到时自动以源目录在目标上创建（合成与 `AcpAdapter.projects()` 同构的 resolved 项目，key 为 `kind:sha256(cwd)[:12]`，结果带 `projectCreated: true`，目标随后列出该会话即自然收录该目录），远端源的 cwd 是远端路径、无法在本机目标上创建，仍报错并要求显式传 `project`。新会话经目标 `create_chat`（按请求 id 幂等）创建，转录作为首条消息经 `send` 注入（submission id 由请求 id 派生，重试不会重复创建或重复发送）。注入失败（如 codex 桌面尚未加载新会话）不视为迁移失败：返回 `{status: 'created', sent: false, warning}`，会话已可用。前端入口在聊天详情弹层：先按 view 注入的 `agents` 列表选择当前以外的已启用 Agent，再选择复制范围（完全复制历史/只复制主要历史/只复制最新提问和结果）后确认执行。

### 能力矩阵

| 能力 | Codex | Kimi | MiniMax | Claude | DeepSeek | ZCode |
| --- | --- | --- | --- | --- | --- | --- |
| 列表/历史/发送/审批/中断/排队/通知 | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| 模型与档位切换 | ✓ | ✓（configOptions） | ✓（configOptions，按模型动态增删） | ✓（configOptions：model + effort） | ✓（configOptions：model + effort，off/low/high/max） | ✓（configOptions：model + thought，词表按模型变化） |
| 新建会话 | ✓ | ✓ | ✓ | ✓（创建进程保活至首条消息落盘） | ✓ | ✓（创建进程保活至首条消息，惰性占位） |
| 计划模式 | ✓ | ✓ | ✓ | ✓ | ✓（collaboration_mode 配置项，非 session/set_mode） | ✓（default 映射为 yolo） |
| steer（补充当前任务） | ✓ | ✗（前端隐藏，后端拒绝） | ✓（扩展 RPC） | ✗（前端隐藏，后端拒绝） | ✓（`_session/steering`，promptRequired 回落普通发送） | ✗（上游 ≥0.16 已删除） |
| goal 模式 | ✓ | ✗（ACP 无接口） | ✓（扩展 RPC） | ✗（ACP 无接口） | ✗（ACP 无接口） | ✗（ACP 无接口） |
| rename/fork | ✓ | ✗ | ✗（fork 有扩展，v1 不开放） | ✗ | ✗（fork 有 `session/fork`，v1 不开放） | ✗（fork 有 `session/fork`，v1 不开放） |
| Skill 选择 | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ |
| Fast 模式/账号切换 | ✓ | ✗ | ✗ | ✗ | ✗ | ✗ |

## 固定 HTTPS 入口

桌面 `connections` 保存可独立启用的 Quick Tunnel、自有服务器 SSH 和 NAS 配置，局域网监听由 `lan` 控制，`lanAddresses: null` 保持所有 IPv4 网卡监听，数组仅绑定选中地址，空数组只保留回环。各监听器共享同一个认证、配对、通知和会话状态。`localAccess: false` 关闭本机网页入口，回环仅允许无会话信息的私有 `/api/health` 检查和保留 Host 的已授权 HTTPS 隧道；停机关闭全部监听器。桌面用系统网卡名称和 IPv4 地址呈现选择，地址不可绑定时失败，不回退至全网卡。旧单入口配置在读取时转换，保存前不改写文件。`bridge/access.py` 验证地址，生成不含凭据的 Caddy/Nginx Compose 部署包；所有启用的固定 URL 自动加入允许源，第一个启用的固定 URL 优先用于通知链接（显式 clickBase 仍优先）。停用或删除配置时移除对应托管固定源，保留其他手动 HTTPS 源。

`bridge/ssh_tunnel.py` 管理独立 OpenSSH 子进程，复用已有 SSH 目标和身份，以 `-R 127.0.0.1:服务器端口:127.0.0.1:电脑端口` 回程。启用 BatchMode、StrictHostKeyChecking、ExitOnForwardFailure 与保活，不复用 ControlMaster。失败后台退避重连，不阻塞 LAN 服务。各 SSH 配置独立保存状态与日志，状态关联当前网关 PID；服务器 GatewayPorts 应使用 no/clientspecified，不能强制公网绑定。

`/api/auth` 返回非秘密的本次运行 instanceId。固定入口检测仅从本机管理进程请求本机与已保存 HTTPS 地址，核对实例一致，不携带登录凭据，不跟随重定向。配置包导出、剪贴板和入口检测仅经受保护的本机 Electron IPC 提供，手机 HTTP 不提供这些操作。

临时 Cloudflare 握手在后台进行，避免等待外网隧道拖延 LAN 和其他入口的服务。关闭网关时停止各隧道。

`web/i18n.js` 是电脑与手机共享的简体中文 / English 文案表，无外部翻译服务。静态节点通过 data-i18n 标记，动态界面显式翻译；聊天正文、命令、用户输入及外部模型/Skill 描述不进入翻译流程。语言保存在各界面的 localStorage，切换不重载会话或提交表单。日志按源展示最新记录优先，保留多行错误记录的内部顺序。


## 一次性扫码登录

`bridge/pairing.py` 为每个允许源保存一个有效的内存授权：32 字节随机值的 SHA-256 摘要、随机 ID、5 分钟过期时间与消费状态。刷新替换同源旧授权，消费在锁内完成，正确源且未过期的授权只生成一次标准 12 小时登录会话。网关重启不恢复授权和会话。失败兑换限速独立于密码登录。

签发只走本机 stdio/Electron IPC 与 `GatewayControl` 的私有文件通道：每实例随机 `.pairing-*` 目录，独立请求/响应文件和控制令牌，响应读取后删除、服务正常退出时清理目录。Windows 继承本机目录 ACL；数据目录不能放在共享公共目录。HTTP 没有签发接口。桌面签发前检查地址属于当前显示入口，并从该入口读取 `/api/auth` 核对本机 instanceId；公网探测不携带凭据、不跟随重定向。

Electron 主进程使用 qrcode 本地生成 PNG，渲染器接收图片、授权 ID 与有效期。每张地址卡片的折叠区域独立保留状态，收起撤销、刷新替换、轮询显示已使用/已过期；语言和常规状态刷新不重新签发。

手机首次加载时捕获 `#pair=` 片段并立即通过 `history.replaceState` 清除，随后 `POST /api/pair` 换取 HttpOnly/SameSite Cookie 和 CSRF。兑换要求 HTTP Host 对应的完整 Origin 与授权源一致；HTTPS Cookie 使用 Secure。片段不会作为 HTTP 请求路径或查询串发送，接口也不记录请求体。手机失败时提供原有密码登录入口。二维码仅授予与普通登录相同的访问权限，不绕过 Codex 原生人工确认。

## 桌面应用更新

`desktop/updater.cjs` 从固定 GitHub Release 源获取签名清单，使用内置 Ed25519 公钥验证原始载荷，再流式下载并核对长度与 SHA-256。自动检查不自动安装；安装按钮只通过受来源校验的桌面 IPC 调用，HTTP 网关无更新管理接口。

`bridge/updater.py` 负责原安装目录旁的准备、替换与恢复。准备阶段拒绝路径穿越和危险链接，核对包版本、平台、macOS 签名和内置运行时。独立复制的旧运行时等控制面板退出、协作停止原网关后替换目录，再等待新面板确认加载并恢复此前运行的网关。失败时尝试回滚；配置目录保持独立。发布签名身份和事务恢复操作见 [桌面更新文档](docs/desktop-updates.md)。


## PR8 验收反馈调整

Goal 元数据写入后立即读取原生状态确认，不再打开桌面页面或同步调用完整历史 RPC。暂停、修改和清除不依赖 owner 激活；创建与恢复的实际执行仍由原 owner 发送，并保留未知结果不重放和持久化操作 ID。

保存历史从 rollout 末尾按完整轮次读取，首批 20 轮，向前翻页按 50 轮扩展；本地缓存按路径、inode、大小和修改时间失效，最多保留 8 个不超过 4 MB 的解析结果。远端沿用相同读取器。历史 IO 与 owner 订阅独立推进；向前补页保留已有消息键、游标和顺序。历史可读不代表 owner 已就绪。

全会话通知策略保存在 `notification-policies.json`，请求处理默认开启、运行完毕默认关闭，通道本身仍默认关闭。已有显式 watches 保留其选择，每个聊天可对两种事件独立继承或覆盖。通知通过只读列表发现聊天，并仅 follow 原 owner 状态；不读取 rollout、不激活聊天。首次实时状态作为完成通知边界，不补发旧完成事件。网页关闭后网关继续监听；依赖桌面 owner 与相关 SSH 主机可用。

主页快捷入口属于浏览器显示偏好。最近排序在发送或回复被接受后立即更新，并通过活动查询中的数据库时间校准；仅阅读不更新交互时间。计划意见非空时只允许继续规划，避免执行按钮忽略意见。
