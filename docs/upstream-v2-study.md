# Codex Mobile Bridge v2 学习笔记

> 状态：调研完成，未实施 · 调研日期：2026-11-03 · 参考项目：[try2love/codex-mobile-bridge](https://github.com/try2love/codex-mobile-bridge)（完整克隆于 `refs/codex-mobile-bridge`，主分支 `0721aa7d`）

这次快进不是一次普通依赖更新，而是持续约三周半的 v2 大改版：产品从 Codex 手机网关扩成多客户端远程工作台，同时重排后端、前端、移动端和桌面端。最值得本项目学习的并非功能数量，而是上游把执行归属、未知写入、模块边界和验证范围变成了可执行、可审计的工程约束。本文路径默认相对 `refs/codex-mobile-bridge/`，决策记录链接均可直接打开。

## 1. 改版范围与产品方向

本地 reflog 显示，本次快进区间为 `faf44369..0721aa7d`（2026-10-08 至 2026-11-03）。不同版本区间不能混用统计口径：

| 区间 | 提交数 | 文件变化 | 行数变化 | 说明 |
| --- | ---: | ---: | ---: | --- |
| `faf44369..main` | 1366（其中 320 个 merge） | 1374 | +153940 / −8978 | 本次完整快进 |
| `v2.0.0..v2.1.7` | 1091 | 990 | +89498 / −3057 | v2.0.0 正式版至 v2.1.7 |
| `v2.1.7..v2.2.0-preview.1` | — | 229 | +9041 / −879 | 新增 ZCode 等预览改动 |
| `main..feature/network-access-redesign` | — | 599 | +2924 / −50211 | 未合并；删除 WSL 并重做网络引导 |

### 1.1 多客户端工作台

v2.0.0 将 Codex App/CLI/WSL、Claude Desktop/Code、DeepSeek Harness 纳入同一工作台，但会话、账号、模型、权限和进程归属仍按接入环境隔离。v2.1.x 主要补手机通知、扫码绑定、下载、主题、会话整理、弱网与离线恢复。关键不是“统一成一种会话”，而是**统一展示与操作入口，不统一执行身份**：`owner/provider/auth/cwd` 必须保留，被动读取不得激活或切换会话。

### 1.2 三轴代码重排

提交 `51340312` 一次搬迁 329 个文件，将后端整理成 `app / api / features / clients / platforms`，Web 拆成 `shell / features / shared / hosts / layouts`，`mobile`、`desktop`、`relay` 各自成块。职责划分见 [架构导航](../refs/codex-mobile-bridge/docs/architecture/README.md)：features 决定业务规则，clients 处理协议方言，platforms 只做操作系统交互。

外部命令入口 `run.py` / `desktop.py`、公开 HTTP URL、配置与数据格式保持不变；内部 Python import 不加兼容 alias，只提供 `docs/architecture/path-map.json` 供外部脚本迁移。`bridge/app/service.py` 从 1793 行增至 3576 行，上游没有按行数机械拆分，而是在架构文档中明确保留这个债务，后续沿真实功能逐块抽取。

### 1.3 原生移动外壳

Android/iOS 从单纯 WebView 页面外壳扩展为带原生菜单、通知、下载、Keychain/加密绑定和后台能力的 App。共同注入脚本只保留一份，[mobile/source-map.json](../refs/codex-mobile-bridge/mobile/source-map.json) 记录旧位置、抽取成员、两端入口及构建资源清单；宿主（iOS/Android/浏览器）与布局（窄/宽屏）是两个独立维度，不维护四份聊天业务代码。

### 1.4 连接矩阵与故障隔离

局域网、Cloudflare 临时/固定隧道、Tailscale、SSH 反代、NAS 与共享中继并存。更重要的是故障域隔离：普通原生操作按主机串行，不同主机独立推进；SSH 离线采用 30 秒起步、指数增长、0.8–1.2 抖动、10 分钟封顶的退避，冷却期快速失败，到期只放一个恢复探测。本机列表、活动和通知不再同步等待离线 SSH，缓存历史可读也不冒充会话可写。

## 2. 六项值得借鉴的工程做法

### 2.1 依赖门禁：例外必须带理由，也会过期

[check-boundaries.py](../refs/codex-mobile-bridge/scripts/check-boundaries.py) 用 AST 扫描直接、相对和字符串字面量动态导入，执行三条规则：platform 不得向上依赖、features/clients 不得新增 app/api 依赖、provider 不得互相导入。[dependency-exceptions.json](../refs/codex-mobile-bridge/docs/architecture/dependency-exceptions.json) 将 22 条存量边精确登记为“文件 + 依赖符号 + 规则 + 原因”。核心是比较两个集合：**新增违规失败，失效例外同样失败**，防止豁免漂移成永久许可。

对 Go 版可用 `go list -deps` 或 `golang.org/x/tools/go/packages` 生成实际 import 边，再与 `exceptions.json` 对比。先登记存量、禁止增量，后续每去掉一条旧依赖就必须同步删除例外；不要把例外写成目录级通配。

### 2.2 决策记录：维护规则之间的替代关系

`.agents/notes/` 当前恰好 100 篇：54 feature、43 bug-fix、2 architecture、1 testing。固定结构为问题、决策、备选方案、影响、验证；真正有价值的是“既有记录核对”：逐篇说明部分重叠、互补或取代，并明确旧规则的哪些部分仍有效。`CONTRIBUTING.md` 要求非平凡行为、契约或测试策略变化随代码更新记录，纯样式与机械改动不用。

这比 ADR 只追加不维护更适合长期演进：同一规则不会出现两个都自称有效的真相来源。对本项目可先从可靠性、账号切换和 provider 方言开始，不必为每次文案调整制造文档负担。

### 2.3 大重构：先建立可比基线

[目录重组验证记录](../refs/codex-mobile-bridge/docs/architecture/refactor-verification.md) 在改前保存 Git bundle、源码包与 SHA-256 manifest，并在隔离目录恢复验证 HEAD 一致。Python 基线 1021 项、重组后 1034 项，跳过范围保持相同；63 个迁移模块做 AST 对照，49 个去掉 import/docstring 后完全一致，其余差异逐项归因。公开 URL、配置格式和入口不变，内部路径用 map 对账而不是加运行时别名。

另一个容易漏掉的细节：早期 Node 统计把 18 份“只定义函数、没有执行断言”的浏览器脚本算作成功，发布记录主动剔除空跑并重新跑基线。可比不仅是“前后都绿”，还要确认两次运行的是同一类真实断言。

### 2.4 写操作：把“结果未知”作为一等状态

写操作先以 UUID 建私有回执，再执行；断连、超时和传输错误都进入 `unknown`，绝不自动重放。只有明确业务拒绝（例如 HTTP 400 `send_not_submitted`）才释放对应未确认标记。多标签页对同一提交的尝试逐次登记，任一次仍 unknown 就不能被另一次迟到拒绝清除。发送、建会话、归档、账号切换、APK 下载和更新安装都沿用这一思路。

恢复不是“再发一次”，而是按原 ID 只读查询回执或原生持久证据。原生已接受但 HTTP 响应丢失时，只清理相同草稿 revision；用户后续编辑保留。需要特别区分：**安全重试只适用于只读请求、幂等查询和明确建模的期望偏好**（如置顶只保留最终布尔值），不能因为某个偏好队列可重试，就把消息、审批或账号切换也放进自动重试。

### 2.5 发布验收：结论必须带证据边界

[VERIFICATION.md](../refs/codex-mobile-bridge/VERIFICATION.md) 每个版本用表格记录“结果 + 证据边界”：模拟器不等于真机签名安装，编译通过不等于原生菜单/通知/键盘可用，五架构构建通过不等于日常 App 已完成覆盖升级，源码测试不等于三方客户端实机联调。v2.1.7 还实测 `2.1.6 → 2.1.7 → 2.1.6`，核验配置字节、认证和原生绑定保留。

这种写法不仅防夸大，也能防止后续把旧证据错套到新候选。对本项目发布矩阵至少应分：Go 单测/集成、Web 真实浏览器、Android WebView、桌面安装与更新、真实 ACP CLI、公网隧道六类，每类单独写已执行、跳过、未执行及原因。

### 2.6 CI：既节省成本，也不把 skipped 当通过

[tests.yml](../refs/codex-mobile-bridge/.github/workflows/tests.yml) 的 `mobile-scope` 按改动路径决定是否编 APK/iOS；聚合必需检查 `desktop-tests` 使用 `if: always()`，在移动范围未命中时显式断言 `ANDROID_RESULT == skipped` 和 `IOS_RESULT == skipped`，命中时则必须为 success。这样失败、取消和“按预期跳过”不会混成一个绿色状态。

浏览器测试使用生产资源与合成 API，覆盖 Chromium/WebKit × 1280/390 四组合，并且实际调用断言。上游还用 `integration/*` 分支整合外部 PR，保留原作者提交、不 squash；本次区间 320 个 merge 提交体现了这一协作习惯。

## 3. Go 版优先阅读的 20 篇决策记录

按“对本项目当前 Go 网关、账号池、ACP 适配与 Web 可靠性的直接可迁移性”排序，而非按功能热度：

1. [浏览器回归与显式模块依赖检查](../refs/codex-mobile-bridge/.agents/notes/implemented/testing/2026-10-28-regression-and-dependency-gates.md)：依赖例外双向对账、浏览器真断言、移动 scope 与 skipped 门禁。
2. [Codex 页内统一 APP、CLI 与 WSL 接入](../refs/codex-mobile-bridge/.agents/notes/implemented/architecture/2026-10-24-codex-wsl-unified.md)：展示聚合但写入门仍绑定具体 endpoint，是三轴分层的完整案例。
3. [将源自 CLI 的聊天路由到其现有执行归属进程](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-existing-owner.md)：来源不等于当前 owner；发现超时不等于 owner 不存在。
4. [短暂读取超时后保留已加入的 CLI 写入者](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-goal-refresh-timeout.md)：只读超时可重试，真实断开才清除 writer；过期读取不能覆盖新连接。
5. [Codex 聊天列表按主机渐进显示](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-codex-list-first-paint.md)：慢主机不阻塞本机；只有全部成功才能确认全局空列表和推进游标。
6. [SSH 离线隔离、退避与本机历史刷新](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-30-ssh-offline-isolation-and-backoff.md)：按主机锁、共享退避、后台聚合读取，以及“离线不是空闲证据”。
7. [桌面 IPC 路由切换后的远程重建与发送回执恢复](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-30-desktop-ipc-rebuild-and-send-receipts.md)：显式重建 follower；按提交 UUID 与草稿 revision 恢复，不重发。
8. [代理 HTTP 启动积压队列与 GET 消息定界](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-proxy-http-backlog-framing.md)：连接积压、活跃并发和请求定界是三种不同预算，含请求走私负向测试。
9. [本地优先的 Bridge 会话置顶](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-local-first-session-pins.md)：期望偏好可安全重试，但 revision、身份隔离与旧 GET 防护一个不能少。
10. [经确认后停止、释放并归档](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-27-confirmed-disconnect-archive.md)：停止成功才归档，只停止精确目标，未知结果保留完整操作范围。
11. [原生归档的执行归属与 provider 可逆支持](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-native-session-archive-feedback.md)：能力按 provider 与版本核验，第二写入者和部分失败不可伪装成功。
12. [Claude 与 DSH 发送期间的即时反馈](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-desktop-send-feedback.md)：发送回执、正文持久化与实时状态是不同证据，不应相互冒充。
13. [共用原生任务状态提示](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-shared-chat-progress.md)：共享状态优先级，不按 Agent 名分支；界面不从错误文案猜任务活跃。
14. [跨轮次保留尚未回答的异步问题卡片](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-pending-async-cards-across-turns.md)：待处理请求不能因新轮次或列表投影而静默消失。
15. [手机收件箱与第三方发送隔离及多客户端通知补漏](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-notification-delivery-isolation.md)：外部 HTTP 不持锁、不阻塞本地通知；idle 不等于成功完成。
16. [原生运行失败与阻塞的待处理通知](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-terminal-attention-notifications.md)：失败与阻塞是 attention，不是 completion；恢复后撤销对应提醒。
17. [原生模型目录身份与手动设置兜底](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-agent-model-settings-fallback.md)：目录能力、手动设置能力、账号身份分别表达，写前重新核验。
18. [统一本地 Codex 账号](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-unified-codex-accounts.md)：共享主目录只显示一份账号，但 Desktop/CLI 可部分成功，反馈不可合并造假。
19. [启动资源恢复与脚本执行顺序](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-22-startup-resource-recovery.md)：GET 可有界重试，POST 不进通用重试；下载并发与执行顺序分开。
20. [已加入 CLI 聊天的权限切换](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-cli-permission-switching.md)：权限变更必须绑定当前 owner 与原生能力，未知写入仍走回执核对。

## 4. 100 篇决策记录主题索引

以下按每篇记录的主要问题域归类，每篇只出现一次，合计 100 篇；跨主题关系仍以记录内的“既有记录核对”为准。文件名即日期与主题，链接指向本地上游源码。

### 4.1 多客户端接入与执行归属（28）

- [将源自 CLI 的聊天路由到其现有执行归属进程](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-existing-owner.md)
- [短暂读取超时后保留已加入的 CLI 写入者](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-goal-refresh-timeout.md)
- [客户端状态读取避开启动锁](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-client-status-startup.md)
- [按目录身份读取 Windows 原生 Codex 历史](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-codex-windows-history-paths.md)
- [Windows 上更早完成 Claude 控制台回读](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-adaptive-console-readback-windows.md)
- [Claude Desktop 退出只依据桌面任务清单](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-desktop-quit-evidence.md)
- [在 Claude 前台控制台直接提交](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-direct-console-submit.md)
- [macOS 上更早回读 Claude 控制台](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-early-console-readback.md)
- [Claude 进程被发现前的启动进度](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-launch-progress.md)
- [Claude 已签名连接的就绪时机](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-signed-ready-latency.md)
- [网关在所有客户端停用时保持可管理](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-gateway-with-no-clients.md)
- [启动资源恢复与脚本执行顺序](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-22-startup-resource-recovery.md)
- [启动错误归属与失败资源缓存重试](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-27-startup-error-attribution-and-cache-retry.md)
- [SSH 目录读取与 Skill 等待](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-ssh-catalog-read-and-skill-latency.md)
- [WSL 时间线读取的锁顺序](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-wsl-timeline-lock-order.md)
- [Claude 与 DSH 发送期间的即时反馈](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-desktop-send-feedback.md)
- [Claude 与 DSH 列表的手动下拉刷新](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-12-shared-client-list-refresh.md)
- [Claude 与 DSH 列表状态只提示需关注的结果](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-13-quiet-client-list-status.md)
- [本地 Claude Code CLI 附件](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-cli-attachments.md)
- [已安装的 Claude Code CLI Skills](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-cli-skills.md)
- [Codex CLI 发现与显式原生接入](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-codex-cli-access.md)
- [终端退出后继续保存的 CLI 聊天](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-codex-cli-resume.md)
- [SSH Claude Code CLI 会话](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-ssh-claude-code.md)
- [SSH 主机上的原生 CLI Goal](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-ssh-native-cli-goal.md)
- [经确认后停止、释放并归档](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-27-confirmed-disconnect-archive.md)
- [DSH 通用插件与显式档案绑定](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-dsh-generic-plugin-binding.md)
- [重置动态的本地体验与接纳边界](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-reset-outlook-local-trial.md)
- [DSH 原生 Mobile 与独立插件兼容契约](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-dsh-mobile-and-plugin-compatibility.md)

### 4.2 会话组织、消息与任务状态（17）

- [被动查看时跟随 CLI 保存记录更新](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-passive-history-follow.md)
- [始终标明会话来源](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-always-session-source.md)
- [Codex 聊天列表按主机渐进显示](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-codex-list-first-paint.md)
- [客户端列表成功响应在置顶同步时失败](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-desktop-list-pin-read-token.md)
- [原生归档的执行归属与 provider 可逆支持](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-native-session-archive-feedback.md)
- [跨轮次保留尚未回答的异步问题卡片](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-pending-async-cards-across-turns.md)
- [继续已关闭的本地 Claude Code 会话](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-code-sessions.md)
- [统一 Claude 聊天列表](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-unified-chat-list.md)
- [源自 CLI 的聊天中的模型、Skills、计划和 Goal](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-cli-session-controls.md)
- [从已知项目目录创建 Codex CLI 聊天](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-cli-session-creation.md)
- [Bridge 会话置顶与原生归档](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-bridge-session-management.md)
- [复制原生会话身份](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-copy-native-session-link.md)
- [本地优先的 Bridge 会话置顶](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-local-first-session-pins.md)
- [Claude 与 DSH 的显示分类及改名回执](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-desktop-session-display-metadata.md)
- [原生会话改名与 DSH 项目成员边界](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-native-session-metadata.md)
- [共享会话菜单与原生分类写入](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-shared-session-menu.md)
- [共用原生任务状态提示](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-shared-chat-progress.md)

### 4.3 账号、模型与权限（14）

- [Claude 额度只在明确请求时查询](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-12-keychain-access-and-manual-quota.md)
- [按轮次记录的模型标注 CLI 回复](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-model-attribution.md)
- [恢复 Claude CLI 时忽略合成事件的模型标记](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-16-claude-cli-synthetic-model-reconnect.md)
- [账号展示与网关平台范围](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-account-ui-and-platform-scope.md)
- [DSH 回复展示实际模型与历史请求思考强度](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-deepseek-message-model.md)
- [原生模型目录身份与手动设置兜底](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-agent-model-settings-fallback.md)
- [Claude Code CLI 原生权限模式](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-cli-permissions.md)
- [Claude 上下文用量读数](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-claude-context-usage.md)
- [已加入 CLI 聊天的权限切换](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-cli-permission-switching.md)
- [可选的本地 CLI 账号同步](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-14-local-cli-account-sync.md)
- [本地 CLI 模型目录使用原生守护进程环境](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-18-local-cli-daemon-model-environment.md)
- [SSH CLI 上游模型目录](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-18-ssh-cli-model-catalog.md)
- [按环境隔离的 WSL 账号管理](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-25-wsl-account-management.md)
- [统一本地 Codex 账号](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-unified-codex-accounts.md)

### 4.4 网络、连接与弱网恢复（6）

- [网关会话 Cookie 按实例隔离](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-13-gateway-cookie-namespace.md)
- [显式继续会话时复用尚未完成的执行归属查询](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-14-cli-connect-retry.md)
- [代理 HTTP 启动积压队列与 GET 消息定界](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-26-proxy-http-backlog-framing.md)
- [SSH 离线隔离、退避与本机历史刷新](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-30-ssh-offline-isolation-and-backoff.md)
- [桌面 IPC 路由切换后的远程重建与发送回执恢复](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-30-desktop-ipc-rebuild-and-send-receipts.md)
- [用户自建穿透的 HTTPS 访问入口](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-12-custom-tunnel-entry.md)

### 4.5 移动端与桌面原生（9）

- [Android 更新下载与安装交接](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-12-android-update-handoff.md)
- [网页对话框打开期间隐藏手机原生菜单](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-22-mobile-native-menu-modal-visibility.md)
- [安卓 App 的统一主题与聊天状态保留](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-android-app-theme.md)
- [Android 更新下载方式由用户选择](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-12-android-update-download-choice.md)
- [网关协助 Android 查询和下载安装包](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-12-gateway-assisted-android-update.md)
- [原生设备绑定恢复](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-13-native-binding-recovery.md)
- [Android 电脑与聊天选项菜单](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-android-gateway-options-menu.md)
- [Android 可选后台通知与通知跳转保留聊天状态](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-27-android-background-notifications.md)
- [iOS 全局主题与独立前台通知同步](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-ios-theme-and-foreground-notifications.md)

### 4.6 通知与 Webhook（7）

- [Webhook 服务预设、签名与成功判定](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-21-feishu-webhook-delivery.md)
- [手机收件箱与第三方发送隔离及多客户端通知补漏](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-notification-delivery-isolation.md)
- [原生运行失败与阻塞的待处理通知](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-terminal-attention-notifications.md)
- [手机端第三方通知配置](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-13-third-party-notification-settings.md)
- [自定义 Webhook 通知通道](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-21-custom-webhook-notifications.md)
- [多个 Webhook 接收目标与旧配置兼容](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-22-multiple-webhook-destinations.md)
- [手机按连接地址选择任务通知来源](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-28-mobile-notification-sources.md)

### 4.7 工作区、文件与内容展示（16）

- [文件传输设置在可见且已登录时读取](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-13-file-transfer-settings-read.md)
- [紧接正文的块公式解析](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-28-adjacent-display-math.md)
- [项目引用使用附件身份并固定文件选择器滚动区域](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-29-native-project-attachments.md)
- [原生项目引用与 iOS 选择器取消恢复](../refs/codex-mobile-bridge/.agents/notes/implemented/bug-fix/2026-10-30-native-mentions-and-ios-upload-recovery.md)
- [PDF 阅读与隔离 HTML 预览及源码切换](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-13-pdf-html-preview-and-source-mode.md)
- [只读源码预览保留原文和独立行号](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-13-source-code-view.md)
- [点击模态对话框背景关闭](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-16-dialog-backdrop-dismissal.md)
- [文件阅读字号独立于聊天和终端](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-file-text-size.md)
- [Markdown 文件的阅读与源码模式](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-markdown-file-modes.md)
- [工作区菜单复制目标绝对路径](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-26-workspace-copy-path.md)
- [Codex App 项目确认分开核验信任与原生登记](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-codex-project-trust.md)
- [当前项目文件和目录引用复用工作台](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-project-file-references.md)
- [原生工作区信任的远程二次确认](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-remote-workspace-trust.md)
- [根据原生来源折叠注入上下文](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-native-context-folding.md)
- [共用原生历史图片引用与按需预览](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-29-shared-native-history-images.md)
- [聊天代码块标注语言并复用源码高亮](../refs/codex-mobile-bridge/.agents/notes/implemented/feature/2026-10-30-shared-code-block-syntax.md)

### 4.8 架构与测试门禁（3）

- [Codex 页内统一 APP、CLI 与 WSL 接入](../refs/codex-mobile-bridge/.agents/notes/implemented/architecture/2026-10-24-codex-wsl-unified.md)
- [DSH 独立插件源码与网关内置副本的边界](../refs/codex-mobile-bridge/.agents/notes/implemented/architecture/2026-10-29-dsh-plugin-source-boundary.md)
- [浏览器回归与显式模块依赖检查](../refs/codex-mobile-bridge/.agents/notes/implemented/testing/2026-10-28-regression-and-dependency-gates.md)

## 5. 上游后续方向

- `v2.2.0-preview.1`（2026-11-03）加入第四个客户端 ZCode，并继续共享工作模式与会话控件；相对 v2.1.7 为 229 文件、+9041/−879 行。预览版不等于已验收正式能力。
- `feature/network-access-redesign` 尚未合并：599 文件、+2924/−50211 行，删除 `endpoints/wsl.py`、`vendor/wsl_tomli` 及测试，同时重做网络访问引导。它说明上游会对实验性接入止损，不能因 v2.1.x 曾支持 WSL 就把它当稳定长期承诺。
- 上游发布记录仍持续区分“源码机制成立”和“特定客户端版本/平台已完成端到端验收”。跟进新功能时应同时读实现、决策记录和 VERIFICATION，不能只读 release notes。

## 6. 对本项目的落地建议

1. **先加 import 门禁，不做大搬迁。** 用 `go list` 生成 `internal/` 依赖边，按 features / clients / platforms 三轴定义禁止方向；`exceptions.json` 登记当前存量及理由，同时断言新增违规与失效例外。目录重排另开任务，并用公开 HTTP、配置和测试结果做可比基线。
2. **给危险写操作建立持久回执。** 发送、审批、停止、迁移、账号切换和 accpool 阈值写盘统一区分 `succeeded / rejected / unknown`；操作 ID 在副作用前落盘，多次尝试单独登记。明确业务拒绝才可释放，网络失败只查询原 ID，不自动重放。置顶等期望偏好若做自动重试，必须另行使用 revision 与最终值语义。
3. **把连接恢复按故障域隔离。** 慢主机/慢 provider 不得阻塞本机列表、通知和账号操作；只读请求按主机有界退避，真实断开才使 writer 失效。缓存历史可读、传输已连接、原生 owner 可写必须是三个独立状态。
4. **建立发布证据矩阵与 CI scope。** 每项写“结果 + 证据边界 + 未验证项”；跳过的移动/平台任务必须显式断言为预期 skipped，聚合检查用 `always()` 防失败被吞。浏览器测试执行真实生产资源和断言，空加载不计通过。
5. **为非平凡契约维护简短决策记录。** 先覆盖 owner 归属、unknown 写操作、账号切换、通知完成判定与 provider 能力五类；每篇写清与旧记录是互补还是取代。纯样式与机械整理不增加文档成本。

建议顺序是 1 → 2 → 4，再把 3 拆成连接与通知两个小任务；它们直接对应当前 Go 网关的依赖增长、accpool 写盘和弱网语义，且不需要先追上游的原生 App 功能。
