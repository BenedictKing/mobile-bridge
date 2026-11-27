# 原版对照：离线历史与终端 / Git 工作台

状态：后续设计，尚未实施。已交付的 iOS 信道切换见[连接特性文档](specs/connections-clients-notifications.md#ios-信道切换)。

## 1. 离线历史

原版把实时缓存与主动下载分开：在网关固定一份历史清单，手机校验分段后落盘，断网冷启动走本地只读入口。参见[历史传输决策](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-11-23-history-transfer-from-22bce.md)、[清单快照边界](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-11-23-transfer-inventory-and-snapshot-boundary.md)及[原生存储实现](../refs/codex-mobile-bridge/mobile/ios/BridgePreview/OfflineArchive.swift)。

本项目已经有按会话维护的时间线、按版本读取的详情、只读 SSH 历史，以及离线待发队列。待发队列是写入恢复，不能回答“重启手机、网关停机后还能否查看聊天”。现行时间线有窗口和大小限制，在线分页也可能换 epoch；顺着可变分页下载完毕不能证明已取得完整历史。

建议先交付一条端到端路径：从单个本地会话手动下载截至最近一个已完成回合的正文和可读过程，在 iOS 冷启动时从电脑列表进入离线副本。第一步只开放已能从持久转录完整读取的来源；没有完整记录、来源还在加载或超过上限时说清楚原因。采用现有时间线的稳定行标识和详情版本，不复制一份 Agent 解析器；读取不得 Attach Agent、改变未读或接受审批。

下载按网关、登录绑定、主机、Agent、会话隔离，切换同一绑定的 LAN/公网入口不重复保存；下载途中校验失败或停止时保留上一份已完成记录。离线副本标明保存时间和截止回合，只呈现历史，提供显式移除；退出登录、解除绑定后不可访问。验证用合成超长历史、途中新增回合、切换账号、同 ID 不同主机和彻底关闭网关后的冷启动。

附件、跨 App 导入、后台恢复和空间回收应另行验收；正式接入时先更新[聊天](specs/chat-and-timeline.md)和[客户端](specs/connections-clients-notifications.md)的现行契约。

## 2. 终端 / Git 工作台

原版的[Git 面板](../refs/codex-mobile-bridge/web/features/workspace/git-panel.js)随工作区切换；[终端管理器](../refs/codex-mobile-bridge/bridge/features/terminals/manager.py)在网关持有进程和有限输出缓冲，[终端 WebSocket](../refs/codex-mobile-bridge/bridge/api/terminal_ws.py)负责附着和交互。可复用的是工作目录归属和连接生命周期，不能以重复提交命令来伪造续接。

本项目已有会话的 Agent / 主机区分、项目键到本机目录的[解析入口](../internal/httpd/httpd.go)和新建聊天时的 worktree 机制；工具调用、Agent 的后台终端和自有服务器 SSH 回程都不等于独立交互终端。远端 SSH 聊天目前明确只读，工作台不能借远端项目键落到网关本机。

先交付只读 Git 检视：在当前本机聊天打开工作区面板，从会话事实定位实际 cwd，包含已存在的 worktree；用固定 Git 参数读取分支、暂存和未暂存变更。外部 diff、textconv 与可写命令不进入首版；输出和执行时间设上限。验收需在临时仓库覆盖不同 worktree、包含换行或 Unicode 的文件名、非仓库、SSH 会话和连切两个聊天后的迟到响应。

连续终端是独立切片：网关拥有 PTY 和终端 ID，默认关闭，只在本机明确允许的项目启用。首次打开与重连均鉴权，重连按输出游标附着同一进程，缓冲缺口显式标记；关闭连接不结束任务，显式结束、闲置回收或网关停止才清理进程。不自动重放键入，重连也不重跑命令。先验证 macOS/Linux 的进程与子进程清理、断线回放和项目隔离；Windows 需有 ConPTY 验收才开放。

Git 写入另行定义预览、确认和结果回读；暂不扩展现有 SSH 写入边界。落地前先同步[网关](specs/gateway-and-agent-runtime.md)与[客户端](specs/connections-clients-notifications.md)现行契约。
