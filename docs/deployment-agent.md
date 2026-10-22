# 给部署 Agent 的执行说明 / Deployment Agent Instructions

> 本文从 README 拆出：面向替用户部署本项目的 Agent 的完整执行说明。中文为完整版；[英文为摘要版](#english-condensed)。

## 中文

桌面 App 的日常使用无需 Agent。需要源码部署、自定义网络入口或协助排错时，可将下面这段话直接复制给电脑上的 Agent：

```text
请帮我部署并运行 https://github.com/BenedictKing/mobile-bridge ：先识别当前电脑是 Windows 还是 macOS，阅读仓库 docs/deployment-agent.md 的完整说明，优先使用已发布的桌面 App 或复用现有安装，按我的需求完成配置、启动和验收；复用现有 Codex App 会话及模型认证，默认启用账号密码和局域网访问，保持已有局域网端口。若我需要外网访问，优先复用已有 NAS/HTTPS 反向代理，有自有服务器和域名时可准备 SSH 回程与固定 HTTPS 的配置参考，涉及服务器管理员权限的操作由我手动完成，没有现成入口再配置临时 HTTPS 隧道；验证聊天读取、实时同步和可用的操作链路，保持服务运行，最后给我可点击的手机访问地址、登录凭据获取方式、App 启停操作或命令、验证结果及仍需我完成的步骤。
```

本节面向替用户部署本项目的 Agent。交付目标是：**用户拿到可用的手机访问地址和登录方式，打开原 Codex App 的同一条会话，消息仍由原 电脑 或 SSH 服务器执行，并能收到结果与待确认请求。** 用户只要求其中一部分时，按其范围部署；已有选择和授权可以复用。

### 1. 确定目标环境与访问方式

先读取本说明、[实现说明](../ARCHITECTURE.md)和[验证记录](../VERIFICATION.md)，再检查用户当前环境。仓库中的验证记录是已知兼容性证据，实际部署仍需按下文验收。

| 项目 | Agent 应执行的动作 |
| --- | --- |
| 部署主机 | 在运行 Codex App 的 电脑 上部署网关；SSH 服务器保留原有 Codex 执行环境 |
| 安装目录 | 使用用户指定目录；已有仓库先检查版本与本地修改，保留 `.local/` 内的账号、去重和队列记录 |
| 系统与运行时 | 检查 macOS/Windows（Linux 为实验性）、App 是否运行，以及 App 内置 Codex 运行时版本；仅源码运行时检查 Go（版本见 `go.mod`），ACP 多 Agent 的 npx 回退另需 Node.js 22+；不要给安装包用户额外安装 Go、Python 或 Node.js；不要因版本不同就宣称兼容 |
| 数据目录 | 按实际 `--codex-home`、`CODEX_HOME` 或默认 `~/.codex` 定位；检查会话数据库和 macOS 的 `ipc/ipc.sock` 或 Windows 的 `\\.\pipe\codex-ipc`，保留已有认证配置 |
| 端口与进程 | 检查计划使用的端口及本项目 `gateway.pid`；已有可用服务优先复用，停止或重启前核对进程归属 |
| 登录 | 默认账号密码；仅在用户明确选择免密时，在桌面 App 开启或将 `.local/config.json` 的 `auth.mode` 设为 `none`；不要求用户提供模型 API key |
| 局域网 | 网关默认同时监听 `127.0.0.1` 与所有活动网卡地址，无需额外参数；监听范围由数据目录 `desktop.json` 的 `lan` / `lanAddresses` / `localAccess` 控制（或桌面 App「网络与登录」）。交付手机能访问的实际局域网 IP |
| 命令行参数 | `serve` 只接受 `--data-dir`、`--port`、`--codex-home`、`--ipc-path`、`--codex-bin`，其他参数（如旧版的 `--lan`、`--no-auth`、`--agents`）会报错退出；多 Agent 用环境变量 `GATEWAY_AGENTS` |
| 外网 | 优先复用已有 NAS/HTTPS 反代，按需采用 `deploy/nas/` 配置；没有入口时再考虑临时隧道；涉及新安装和对外暴露时遵守已有授权范围 |
| SSH | 检查 App 已保存的主机与项目，以及已有 SSH 别名能否非交互连接；不把本机模型目录、技能路径或登录状态套用到远端 |

如果尚不清楚用户是否需要外网，只询问这个会改变部署方案的问题，继续完成不依赖答案的本地检查。系统权限、软件安装、专用测试聊天等也按当前会话已有授权处理，不重复索要已给出的许可。

不要为了让网关显示“已连接”而修改 Codex 原数据库、替换 provider、重启远端模型服务，或另起 CLI 会话冒充 App 会话。需要修改实现时，先定位版本或协议差异，并保留原会话。

### 2. 准备并启动服务

1. 优先使用已发布的桌面 App，按快速开始配置并启动。只有源码部署才需要准备仓库并在仓库根目录运行自动测试；测试不需要真实模型账号。受限工具环境若禁止临时 TCP、Unix socket 或 Windows 命名管道，应按该环境的权限流程处理并如实记录。
2. 只有选择隧道时才准备 `cloudflared`。复用已有可执行文件，核对路径；缺少时按用户授权使用官方安装来源。
3. App 用户在“网络与登录”添加并启用所需连接，可同时保留局域网、临时隧道和固定入口。命令行用户把入口写进 `.local/config.json`（`origins` 数组 / `connections`），或经桌面 App 配置。
4. 沿用已有数据目录与端口。源码启动统一走 make：`make start`（launchd 常驻，KeepAlive 自动拉活）、`make serve`（前台）、`make stop` / `make status` / `make logs`；默认使用项目内 `.local/` 配置，用户要求换端口时统一更新 `GATEWAY_PORT` 与交付地址。
5. 为用户保留可以持续运行的进程，并记录启动方式、PID、日志和停止方式。

以下终端方式仅用于源码部署，App 用户直接使用界面启停。

**前台方式：** 在用户可以保留的终端中执行 `make serve`。交付时说明该终端需要保持运行，以及如何用 `Ctrl+C` 停止。

**Windows 前台方式：** 构建后使用 `start.cmd`（运行 `dist\gateway\codex-mobile-gateway.exe`），停止用 `stop.cmd`。

**后台方式（macOS）：** `make start` 交给 launchd 托管（KeepAlive 崩溃自动拉活、开机自启），运行固定二进制 `~/.local/bin/mobile-bridge-gateway`（自动编译安装并签名），日志在 `~/Library/Logs/mobile-bridge-gateway.log`（`make logs` 跟随）。`make start` 同时以 launchd 托管 ACP Agent Host（默认 Kimi），`make stop` 一并停止。停止与重启用 `make stop` / `make restart`，`make status` 查看服务状态与可达性。建议先 `make dev-cert` 一次性生成本机签名证书，macOS 的目录访问授权不会因重编译而重置。

PID 只表示子进程已创建。**启动工具调用返回之后，再用独立的一次检查确认进程存活、HTTP 就绪、App 会话可连接。** 工具环境可能回收后台进程；若不能持续保留，改用用户终端等允许的运行方式并说明剩余操作。前台 `make serve` 不配置开机启动；macOS 的 `make start` 由 launchd 开机自启。两者都不保证电脑睡眠后继续在线。

默认端口可先做以下无凭据检查；使用其他端口时替换 `8787`：

```sh
curl --noproxy '*' -sS -m 5 http://127.0.0.1:8787/api/auth
curl --noproxy '*' -sS -m 5 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8787/api/sessions
```

第一条应返回认证状态 JSON；第二条在默认密码模式下应输出 `401`，否则说明未登录即可读取聊天，需先检查认证。Windows PowerShell 中请使用 `curl.exe`。

临时隧道建立时可能需要等待一段时间。结合本次进程、最新启动日志和当前 `.local/外网地址.txt` 判断状态，并实际请求该 HTTPS 地址；隧道失败时网关仍可能提供本地服务，应分别记录两个结果。

### 3. 按用户需求验收真实链路

在浏览器登录并验收用户需要的功能。复用获准的专用测试聊天；需要新建聊天或执行真实模型测试时，先确认已有授权覆盖该动作。不要向用户正在进行的工作聊天注入测试消息，也不要为测试改变其权限策略。

| 验收项 | 判定依据 |
| --- | --- |
| 登录与访问保护 | 正确凭据可登录；未登录读取聊天被拒绝；默认密码模式下错误密码不能登录 |
| 原 App 会话读取 | 选中已知聊天，核对标题、主机、工作目录与历史；状态为“已连接”，并能接收后续更新 |
| 同一会话发消息 | 在获准测试聊天发送一个短标记，确认原 App 的同一会话收到输入并产生助手回复；不能把用户消息里的标记当作助手回复 |
| 手机回应 | 在获准测试中回答问题卡片；有实际人工审批时验证其对应请求。自动审批未出现人工卡片时，应标注人工审批未实测 |
| 模型与 Skill | 在获准测试聊天更改模型/推理强度，确认生效后恢复；选择一个已安装且适合测试的 Skill，确认 App 收到原生 Skill 输入，provider 不变 |
| SSH | 核对远端主机、cwd 与实时内容；模型/Skill 来自该主机。SSH 发送和人工审批分别记录是否实测 |
| 列表 | “最近交互”排序正确；“按项目”可展开/收起，同名但不同主机的项目可区分 |
| 外网 | 用实际 HTTPS 地址登录并读取会话；隧道入口确认长轮询有更新，其余入口确认 SSE 增量更新。只测本机回环地址不构成外网验收 |
| 持续运行 | 启动工具调用结束后仍能访问；完成交付时服务存活，并能按记录的方式停止、重新启动 |

若无法操作真实手机，可先完成桌面浏览器手机尺寸及公网 URL 验收，再请用户用手机确认；交付中明确区分两者。手机蜂窝网络可达性需要真实移动网络验证，电脑 上访问公网地址不能代替这一项。

每项结果使用“当前环境实测通过 / 自动测试覆盖 / 待用户确认 / 未验证 / 不适用”等明确状态。仓库测试通过、页面打开、模型返回成功，各自只证明对应环节。

### 4. 遇到问题时定位到对应环节

| 现象 | 下一步 |
| --- | --- |
| 端口已占用或进程退出 | 核对该端口服务、PID 和本次日志；复用已有网关，或按授权停止本项目旧实例，不终止不明进程 |
| 找不到聊天数据库或 IPC | 核对实际 Codex 数据目录、App 运行状态和文件访问权限 |
| 只有历史记录，不能发送 | 核对 App 连接与协议版本；冷聊天可能需在桌面打开一次。SSH 普通 owner 查询失败不等于聊天未打开，应检查本项目的订阅发现路径 |
| SSH 列表为空或主机不可达 | 核对 App 主机/项目映射、App 内该主机的 SSH 连接状态与远端 Codex 数据目录；远端会话经原 App 的连接获取，网关不直接登录远端执行脚本 |
| 模型或 Skill 目录读取失败 | 核对对应主机的 Codex 运行时路径、会话 cwd 和目录错误；不要替换原会话 provider 来绕过问题 |
| 本机可用，局域网不可用 | 核对 `desktop.json` 的 `lan` / `lanAddresses`（或 App「局域网访问范围」）、实际 IP、同网访问条件及系统防火墙；网络地址变化后按需重启网关 |
| HTTPS 不可用，本地正常 | 检查当前隧道进程、最新地址、TCP 7844 出站条件和代理路径；直连检查不能证明所有用户网络都可达 |
| 扫码登录提示 `CERTIFICATE_VERIFY_FAILED` | 当前版本安装包已内置可信根证书；若仍失败，检查系统时间、HTTPS 代理及自有域名证书链，不要关闭证书校验。ntfy 使用新版 TLS 实现需重启网关 |
| 发送结果待确认 | 先查原 App 会话与该消息状态，保持原提交 ID，不盲目重复发送 |

保留已验证有效的部分，修复有证据的问题。需要用户操作时给出具体动作及原因；不要把未完成环节写成成功，也不要通过关闭认证、放宽 Host/Origin 或修改原始数据库来制造成功结果。

### 5. 最终给用户返回什么

最终回复应让用户可以立即打开手机开始使用，并可以自行停止和重启。使用**当前环境的真实值**填写以下内容：

- **状态与入口**：部署完成或部分完成；可点击的局域网/HTTPS 地址及各自测试状态。`127.0.0.1` 只供本机检查，不能作为手机访问地址。
- **登录方式**：实际账号，以及初始网关密码的交付方式。在合适的私密渠道按用户需要提供网关密码，或给出可点击的本机凭据文件；若密码已修改、初始文件已删除，则说明沿用现有密码及重置方法。公开 issue、README 和日志中不写入密码。
- **运行与管理**：部署目录、版本/commit、前台或后台运行方式、PID、准确的停止和再次启动命令；后台方式给出日志路径，自定义配置时给出实际路径。
- **验收结果**：列出本地聊天、发送、回应、模型、Skill、SSH 和外网的适用结果，区分真实测试、自动测试和待确认项。
- **使用条件与后续动作**：电脑、App、网关和所需 SSH 连接需保持在线；临时域名重启会变化；只列出尚需用户完成的具体操作。

可以使用以下模板，删掉不适用项。方括号必须替换为实测值或明确的未完成状态，不要照抄示例地址：

```text
部署状态：[已完成 / 部分完成及原因]

手机访问：
- 局域网：[实际可点击 URL]（[验证状态]）
- 外网：[实际可点击 HTTPS URL / 未启用]（[验证状态]）

登录：
- 账号：[实际账号]
- 密码：[私密交付的网关密码 / 可点击的凭据文件 / 沿用现有密码]
- 修改密码：[在实际部署目录执行的命令]

运行管理：
- 目录与版本：[绝对路径]，[commit]
- 当前进程：[前台终端或后台方式]，PID [实际值]
- 停止：[与本次启动方式相符的命令]
- 再次启动：[含真实端口、隧道程序路径或 origin 的完整命令]
- 日志：[实际文件路径 / 对应前台终端]

验收：
- 原 App 会话读取与发送：[状态]
- 手机问题回应与人工审批：[分别说明状态]
- 模型与 Skill：[状态]
- SSH 会话：[主机与已验证范围 / 不适用]
- 列表与外网：[状态]

使用时请保持 电脑、Codex App、网关及所需 SSH 连接运行。
[启用临时隧道时：重启后从实际“外网地址.txt”取得新地址。]
还需你完成：[具体步骤 / 无；真实手机尚未验证时明确写出]
```

## English (condensed)

The Chinese section above is the complete version; this English section is condensed. An Agent is optional for everyday App use. For source deployment, custom network setup or troubleshooting, copy this to an Agent on the computer:

```text
Deploy and run https://github.com/BenedictKing/mobile-bridge for me. Identify whether this computer runs Windows or macOS and read docs/deployment-agent.md in the repository. Prefer a published desktop App or reuse an existing installation; configure, start and verify it for my needs. Reuse existing Codex App chats and their model authentication. Keep username/password authentication and LAN access enabled by default, preserving any existing LAN port. For external access, reuse an existing NAS/HTTPS reverse proxy when available; with an owned server and domain, prepare references for SSH forwarding and fixed HTTPS, leaving administrative server operations for me to perform manually; otherwise configure a temporary HTTPS tunnel. Connection methods may run together. Verify chat reading, live updates and the available interaction paths. Keep the service running, then return clickable phone URLs, how to obtain login credentials, App start/stop actions or commands, verified results and any steps I still need to complete.
```

### 1. Inspect the environment

Read this document, the README and the verification records. Identify the OS, project version/branch, running Codex App, current gateway/config directory and protected LAN port. Preserve existing chat owners, model authentication, providers, permission policies and SSH credentials. Reuse an existing gateway rather than starting duplicates.

Choose connections based on actual reachability: LAN, temporary tunnel, an existing NAS proxy, or an owned Linux server with SSH. Profiles can run together. Confirm authorization before installing missing software, changing firewall/DNS/server configuration or creating public exposure. Never publish credentials or copy `.codex` to a NAS.

### 2. Prepare and start

Use the packaged App for the guided workflow, or the OS-specific CLI commands in the README. Keep default password authentication unless the user explicitly chooses otherwise. Preserve the current port. Use an independent temporary directory for tests, and never commit it.

For SSH server entries, export or copy the manual setup reference. The user must perform all administrative server operations manually; Agents may explain and review the configuration. NAS entries retain their deployment ZIP and Agent instructions. Inspect existing sites and ports before applying configuration. If server access is unavailable, deliver the configuration and explain what remains unconfigured. Do not claim a service is deployed solely because the files were generated.

Keep the process running using the user's chosen launcher or an authorized process-management method. Verify which process actually owns the port. Stop through the project's control mechanism; do not kill arbitrary processes based on stale PIDs.

### 3. Verify the requested paths

1. Confirm the gateway responds at the preserved LAN URL; anonymous chat APIs must reject access in password mode.
2. Verify login and chat listing, recent history, older-page loading and live synchronization.
3. Use only an explicitly authorized test chat for real sends, creation, approvals or model calls. Do not spend model credits or mutate user chats merely to test a page.
4. For SSH, verify the existing desktop owner rather than replacing it with an unrelated CLI process.
5. For fixed HTTPS, verify the current instance with Check fixed entry and then verify from the phone's external network. Proxy, TLS and phone reachability are separate checks.
6. For ntfy, confirm both phone receipt and click-through to the intended chat. Server acceptance alone is insufficient.
7. State which checks were automated, simulated or performed on actual hardware.

### 4. Diagnose by layer

| Symptom | Check |
| --- | --- |
| Phone cannot reach LAN | Computer wake state, listening address/port, Wi-Fi isolation, VPN/routing, firewall |
| HTTP 502/504 | Proxy upstream, SSH connection and remote loopback port |
| Host/Origin rejection | Exact allowed HTTPS origin and preserved external Host header |
| Login keeps resetting | HTTPS throughout the external path, cookies preserved, caching disabled |
| Only saved history | Open the chat in the original desktop App and confirm owner connectivity |
| Tunnel fails but LAN works | Outbound network restrictions, executable path and tunnel logs |
| Live updates keep dropping | Proxy timeouts and response buffering; SSE requires unbuffered `text/event-stream`; do not cache `/api/` |
| QR login shows `CERTIFICATE_VERIFY_FAILED` | Current packages bundle trusted CA roots. If it persists, check system time, HTTPS interception and the domain's certificate chain; keep verification enabled. Restart the gateway for ntfy to use the updated TLS implementation |
| ntfy receives but chat link fails | Notification click URL, gateway availability and phone network reachability |
| Missing IPC/runtime | Correct OS/user, App version, explicit executable/endpoint overrides |

### 5. Final handoff to the user

Return concrete results, not just a process-start message:

```text
Status: running / partially configured / blocked
Phone URLs: clickable LAN and configured HTTPS URLs
Login: username and safe credential-retrieval method
Installation: directory, branch/commit, process and log locations
Connections: enabled profiles, server/NAS deployment location, known dependencies
Start / stop / restart: exact commands or App actions
Verified: local/SSH read, live update, send, model/Skill, approval, external access and ntfy — only those actually checked
Not verified / user steps: clear remaining actions and limitations
```

Do not include actual passwords, tokens or API keys in public issues, commits, screenshots or deployment bundles.
