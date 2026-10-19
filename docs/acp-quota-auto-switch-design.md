# ACP Agent 额度自动切换与自动继续设计

> 状态：阶段 1–8 已全部实现（2026-10-19）
> 日期：2026-10-18（阶段 6/7/8 更新于 2026-10-19）
> 背景：把 zcode-switcher（/Users/petaflops/works/unsorted/zcode-switcher）的账号快照管理与额度查询能力集成进 mobile-bridge，实现"agent 额度（如 5 小时窗口限额）用尽后自动切换其他可用账号，必要时自动发送继续"。

## 1. 目标与非目标

### 目标

1. 为 ACP 型 agent（首批 **kimi**，其次 **zcode**）引入**多账号池**：复用 zcode-switcher 的登录态快照格式，网关在 `<dataDir>/pool/<agent>/` 下管理若干账号。
2. **额度用尽自动切换**：当 agent 的窗口限额（如 kimi 的 5 小时窗口）耗尽时，网关自动把该 agent 切到池内另一个"当前可用"账号，无需人工干预。
3. **切换后自动继续**：切换完成且会话可恢复时，自动向受影响会话发送"继续"（复用现有 `AutoContinuePrompt` 链路），让被打断的任务接着跑。
4. 提供 Web 端可见性：账号池列表、各账号额度、切换历史、开关与手动切换入口。

### 非目标（本期不做）

- 不改变 **Codex** 的账号切换机制（现有 `internal/account` 重型状态机保持不动；Codex 侧如需纳入，走后续阶段）。
- 不做跨 agent 的统一额度度量（各 agent 额度语义不同，各自适配）。
- 不实现登录流程本身（扫码/OAuth 仍在各官方 CLI 或 zcode-switcher 桌面版完成）；本期只做"导入已登录凭证快照 + 轮换"。
- ~~不做 MiniMax / WorkBuddy 的集成~~（已于阶段 6/7 落地，见 §10）。

## 2. 现状盘点

### 2.1 zcode-switcher 可复用的能力

| 能力 | 位置 | 说明 |
|---|---|---|
| 额度查询 | `src/kimiApi.js`、`src/quota.js` | 纯官方 HTTP API，无 CLI 输出解析。Kimi：`GET api.kimi.com/coding/v1/usages` → 周额度 + `limits[]` 窗口限额（含 `window.timeUnit/duration`、`detail.used/limit/resetTime`，5 小时窗即 300 分钟项）+ booster 余额。ZCode：`zcode.z.ai/api/v1/zcode-plan/billing/current`+`balance`，BigModel 兜底。 |
| 凭证快照 | `src/kimiManager.js`、`src/manager.js` | Kimi：每账号单文件（accessToken/refreshToken/expiresAt/quota/status），切换 = 写 `~/.kimi-code/credentials/kimi-code.json`，切走前回读槽位最新 token 更新旧快照（官方 CLI 会轮换 refresh_token），目标 token 临期先刷新。ZCode：`~/.zcode/v2/credentials.json`（`enc:v1:` 机器绑定加密）+ `config.json` 整体快照、原子替换。 |
| 429/401 处理 | `src/quota.js` | 429 渐进退避（500ms/1.5s/4s），全 401 等 1.5s 重试一次防"首次激活延迟"误报。 |

**明确没有、需要新做的**：用尽触发切换（现仅 UI 阈值变色）、自动发送继续（全库无此逻辑）、周期额度轮询服务。

### 2.2 mobile-bridge 现有机制

| 能力 | 位置 | 说明 |
|---|---|---|
| ACP 进程托管 | `internal/acpruntime/manager.go` | 网关在进程内 spawn 各 ACP agent 子进程（kimi/zcode/claude/mcode/dsh/opencode），会话级 `Prompt`/`Steer`/`Cancel`。Agent Host 模式下经 Unix socket 接入常驻 `agent-host`。 |
| 自动继续 | `internal/acpruntime/manager.go:631` `AutoContinuePrompt`；`cmd/gateway/jevhooks.go:66` `jevContinueHook` | 回合落定（完成/失败/超时）触发 `AutoContinue` 钩子链；连续自动续跑预算 `MaxAutoContinue=5`，人工动作重置。 |
| 额度关键词闸门 | `internal/jev/continue.go:24` `QuotaStop` | 中英混合正则识别"额度不足/usage limit/rate limit/402/429"等，命中则不自动继续——**是被动信号，但精确性和结构化程度不足以驱动切换决策**。 |
| 账号切换 | `internal/account/switch.go` | 仅 Codex：快照→准备→停桌面 App→应用→启动→核验的重型状态机，依赖桌面进程控制（AppleScript 等）。**不适合 ACP agent**（ACP 子进程网关自己就能重启，无需桌面 App 参与）。 |
| HTTP 挂载 | `internal/httpd/httpd.go:366` | `/api/accounts*` 路由区可扩展；`Server` 结构聚合各 Manager。 |

### 2.3 缺口分析

1. **ACP agent 无账号池概念**：kimi/zcode 等直接用 `~/.kimi-code/`、`~/.zcode/` 的当前登录态，网关不知道"还有哪些账号可用"。
2. **无主动额度检测**：`QuotaStop` 只在回合结束后扫文本，等它发现时回合已失败；且无法区分"窗口限额耗尽（等重置或换号）"与"余额欠费（换号也未必有用）"。
3. **无切换执行原语**：缺少"替换 ACP agent 凭证文件 + 重启该 agent 的 ACP 进程池"的轻量切换器。
4. **切换与继续未串联**：即使手动切换完成，被限额打断的会话不会自动续跑。

## 3. 总体架构

新增一个模块 **`internal/accpool`**（账号池 + 额度轮询 + 切换编排），与各 ACP `acpruntime.Manager` 协作：

```
                 ┌────────────────────────────────────────────┐
                 │              cmd/gateway (组装)             │
                 └───────────────┬────────────────────────────┘
                                 │
        ┌────────────────────────┼─────────────────────────┐
        ▼                        ▼                         ▼
┌───────────────┐      ┌──────────────────┐      ┌──────────────────┐
│ internal/     │      │ internal/        │      │ internal/httpd   │
│ accpool       │      │ acpruntime       │      │ /api/pool/*      │
│ ─ Pool        │◄────►│ ─ Manager(kimi)  │      │ 池状态/手动切换  │
│ ─ QuotaPoller │ 切换  │ ─ Manager(zcode) │      │ 开关/历史        │
│ ─ Switcher    │ 回调  │   AutoContinue   │      └──────────────────┘
│ ─ Decider     │      │   钩子链          │
└──────┬────────┘      └──────────────────┘
       │ 凭证快照读写             ▲
       ▼                        │ 回合落定事件（含限额命中）
<dataDir>/pool/<agent>/  ───────┘
~/.kimi-code/credentials/  ~/.zcode/v2/   （各 agent 登录态槽位）
```

数据流两条路径：

- **主动路径（轮询）**：`QuotaPoller` 周期调用各 agent 官方额度 API（移植 zcode-switcher 的 `kimiApi`/`quota` 逻辑），把结构化额度写回账号池条目；发现当前账号窗口限额 ≥ 阈值即交给 `Decider`。
- **被动路径（事件）**：回合失败/落定时，`acpruntime` 的错误文本先过结构化判定（ACP 错误码 + `QuotaStop` 正则增强版），确认"限额耗尽"事件后同样交给 `Decider`——这条路径覆盖轮询间隙的突发用尽。

`Decider` 选定目标账号 → `Switcher` 执行凭证替换 + 重启该 agent 的 ACP 进程 → 成功后向受影响会话发 `AutoContinuePrompt("继续")`。

## 4. 账号池与凭证管理

### 4.1 存储布局

```
<dataDir>/pool/
  kimi/
    index.json            # {accounts: [...], activeId}
    <id>.json             # 账号快照（0600）
    history.jsonl         # 切换审计日志（追加）
  zcode/
    index.json
    <id>.snap.json        # credentials + config 整体快照
    <id>.meta.json
```

kimi 账号条目字段（对齐 zcode-switcher `kimiManager`）：

```json
{
  "id": "u_<user_id>",
  "label": "账号备注",
  "email": "...",
  "accessToken": "...", "refreshToken": "...", "expiresAt": 0,
  "quota": {"weekly": {...}, "limits": [{"window": {"timeUnit": "MINUTE", "duration": 300}, "detail": {"used": 0, "limit": 0, "resetTime": "..."}}], "booster": 0},
  "quotaUpdatedAt": 0,
  "status": "ok | cooling_down | reauth_required | disabled"
}
```

### 4.2 导入来源

- **从 zcode-switcher 存储导入**：读取其 `accounts-kimi/`、`accounts-zcode/` 目录（路径可配），复制快照进池。首次集成的主路径。
- **从当前登录态导入**：把 `~/.kimi-code/credentials/kimi-code.json` / `~/.zcode/v2/` 当前状态捕获为新池条目（等价 zcode-switcher 的 `capture`）。
- 添加/删除账号**只在桌面端**（与现有"账号与接入只能在桌面端改"的安全约定一致，见 docs/account-switching.md）；Web 端只读 + 切换。

### 4.3 凭证刷写规则（移植自 kimiManager.use）

1. 切走前：回读槽位最新 token 更新旧账号快照（官方 CLI 会轮换 refresh_token，不回读会导致旧快照失效）。
2. 切向目标前：`expiresAt` 临期（提前量 = max(expires_in/2, 300s)）先走 OAuth refresh。
3. 写入用 `.tmp` + rename 原子替换；zcode 的 `enc:v1:` 加密 token 不解密、整体搬运（机器绑定，同机有效）。
4. 去重优先级：userId > refreshToken 哈希 > email（同 zcode-switcher）。

## 5. 额度检测

### 5.1 主动轮询（QuotaPoller）

- 周期：默认 5 分钟（可配 `pool.pollInterval`），跟随前端现有 5 分钟额度查询节奏；切换后 60s 内加密一轮以确认新账号状态。
- 实现：把 zcode-switcher 的 `kimiApi.usages` / `quota.js` 移植为 Go（`internal/accpool/quota_kimi.go`、`quota_zcode.go`），含 429 渐进退避与 401 重试逻辑。
- 每个 agent 一个 `QuotaProvider` 接口：

```go
type QuotaProvider interface {
    Fetch(ctx context.Context, acct *Account) (*QuotaSnapshot, error)
}
type QuotaSnapshot struct {
    Windows []WindowQuota // 窗口限额：时长、已用、上限、重置时间
    Weekly  *WindowQuota  // 周额度（kimi）
    Balance *int64        // 余额类（zcode/booster）
}
```

### 5.2 被动事件（回合失败信号）

- `acpruntime.Manager` 在 `awaitPrompt` 失败分支产出结构化 `TurnError` 时，额外跑 `accpool.ClassifyError(text, code)`：
  - 优先用 ACP/CLI 的结构化错误码（429、402、quota exceeded 类）；
  - 兜底用增强版 `jev.QuotaStop` 正则（现有正则保留，补充 kimi/zcode 实际报错语料）。
- 判定为 `quota_exhausted` → 事件送 `Decider`，并把该账号的对应窗口在池内标记为"耗尽（估算）"，避免轮询延迟期间重复踩坑。

### 5.3 "可用账号"判定

账号可被选为目标需同时满足：

1. `status == ok`（非 `reauth_required` / `disabled` / `cooling_down`）；
2. 所有窗口限额的 `used/limit < 阈值`（默认 95%，可配），或额度快照过期 ≤ 10 分钟时以最近一次被动事件修正；
3. 不在冷却期（见 §6.3）；
4. token 未过期或可刷新。

无可用账号时：不切换，发通知（复用 `internal/notifications`），该 agent 进入"等待窗口重置"状态，按最近 `resetTime` 定时复检。

## 6. 切换决策与执行

### 6.1 决策器（Decider）

输入：触发源（轮询阈值 / 被动事件）、当前账号、池内快照。输出：目标账号或"等待重置"。

- 目标选择策略（默认，可配）：**剩余额度最多者**；并列时选最久未用者（均衡磨损）。
- 防抖门控（借鉴 zcode-switcher `workbuddy/rotate.rs` 的三重门控，参数收紧）：
  - 单 agent 切换冷却：默认 10 分钟（额度用尽场景比积分到期紧迫，不能等 120 分钟）；
  - 同一账号 24h 内被切走 ≥ 3 次则当天不再作为切换源（防异常抖动）；
  - 当前账号窗口 `resetTime` 在 5 分钟内 → 不切换，直接等重置。

### 6.2 执行器（Switcher）

ACP 切换比 Codex 轻得多，无需桌面进程控制：

1. **冻结**：调 `acpruntime.Manager` 暂停该 agent 新会话创建；进行中的回合**不抢占**（见门控）。
2. **回读**：把槽位当前凭证写回旧账号快照（§4.3-1）。
3. **替换**：原子写入目标账号凭证到 `~/.kimi-code/credentials/` 或 `~/.zcode/v2/`。
4. **重启**：关闭该 agent 的全部 ACP 子进程（`Manager` 已有生命周期管理），按新凭证重新 spawn；Agent Host 模式下经 socket 让 `agent-host` 重启对应进程。
5. **核验**：对新进程发一次轻量探针（如 `session/new` 或额度 API 复测）确认凭证生效；失败则回滚到旧账号快照并标记目标账号 `cooling_down`。
6. **记账**：追加 `history.jsonl`（时间、from、to、触发源、结果）。

### 6.3 进行中回合的门控

- 有**活跃回合**（prompt 进行中/有待答审批）时：切换延迟到回合落定后执行（挂起为 pending，落定钩子触发时检查）。
- 被动事件本身就意味着回合已失败落定，走"切换 → 继续"主路径。
- 冻结期间新 prompt 请求排队（复用 `internal/queue`），切换完成后放行。

## 7. 自动继续与会话恢复

### 7.1 会话连续性前提

ACP 子进程重启后，会话能否续上取决于 agent 自身：

- kimi / zcode 等 ACP agent 支持 `session/load`（或等价恢复）时：新进程加载原会话 → 上下文完整 → 直接 `AutoContinuePrompt("继续", ...)`。
- 不支持恢复的 agent：无法无损续跑——通知用户，并提供"新开会话并附带原任务摘要"的降级按钮（摘要用现有 timeline 投影生成），**不自动降级**。

首批只对有恢复能力的 agent 开启"切换后自动继续"。

### 7.2 与现有 jevContinueHook 的关系

现有链路在命中 `QuotaStop` 时是**跳过**继续的（等额度是正确行为）。本设计在其前面加一层：

```
回合落定
  ├─ 限额耗尽？─ 是 ─► accpool 切换流程 ─► 切换成功 ─► AutoContinuePrompt("继续")
  │                                         └ 失败/无可用号 ─► 通知 + 等重置
  └─ 否 ─► 原 jevContinueHook 逻辑（不变）
```

- 复用 `AutoContinuePrompt` 的 `MaxAutoContinue` 预算，但**切换触发的续跑不消耗意外中断预算**，单独记 `switchResumeRuns`（上限 3），防止"切换→又限额→再切换"无限循环。
- 被动事件触发的切换，只对"被打断的那批会话"发继续（`awaitPrompt` 失败时记录 sessionId 集合）；轮询触发的预防性切换不发继续（没有被打断的会话）。
- 人工消息/取消（`NoteHumanAction`）同时重置 `switchResumeRuns`。

## 8. HTTP API 与前端

路由挂在 `internal/httpd/httpd.go` 的 `handleMethod`，前缀 `/api/pool`：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/pool` | 所有 agent 的池概览：账号列表（脱敏）、各自额度、activeId、开关状态 |
| GET | `/api/pool/{agent}` | 单 agent 详情 + 切换历史（近 50 条） |
| POST | `/api/pool/{agent}/switch` | 手动切换到指定账号（走同一 Switcher，带 requestId 幂等） |
| POST | `/api/pool/{agent}/settings` | 开关：autoSwitchEnabled、pollInterval、阈值、目标策略（仅桌面端来源可写，同账号管理约定） |
| POST | `/api/pool/{agent}/import` | 从 zcode-switcher 存储/当前登录态导入（仅桌面端） |

前端：`web/accounts.js` 增加"账号池"区块——每 agent 一张卡片列表，显示窗口限额进度条（复用现有额度 UI 组件）、状态徽标（当前/冷却/需重登）、"设为当前"按钮、自动切换开关。切换事件复用 `internal/notifications` 推送到手机端。

## 9. 安全与边界

- **凭证保护**：池目录 0700、账号文件 0600（对齐 zcode-switcher 与现有 `jev.json` 约定）；API 返回一律脱敏（email 打码、token 不出库）。
- **桌面端写、Web 端读**：导入/删除/开关修改需桌面端来源（复用现有区分逻辑）；Web 只允许"手动切换"这一低风险写操作，且经 CSRF + 登录态校验。
- **审计**：每次切换写 `history.jsonl`（谁触发、原因、结果），通知推送含 from/to 账号标识。
- **失败回滚**：核验失败必须回滚到切换前快照；回滚也失败则标记 `reauth_required` 并强通知，不静默留在坏状态。
- **频率上限**：全局每小时自动切换 ≤ 6 次，超过则暂停自动切换并通知（防凭证问题导致的切换风暴）。

## 10. 分阶段实施计划

**阶段 1：只读额度池（kimi）** ✅ 已实现（2026-10-18）
- `internal/accpool`：存储布局、QuotaProvider 接口、kimi usages API 移植（含退避/重试）。
- `GET /api/pool`、`/api/pool/kimi`；前端只读展示。
- 验收：池中账号额度数据与 zcode-switcher 桌面版显示一致。
- 落地说明：
  - 实现为 `internal/accpool/{kimi,pool,poller}.go` + `internal/httpd/pool.go` + `web/pool.js`；
  - 导入走 `POST /api/pool/kimi/import`（登录态 + CSRF 保护，网关本机运行，未单独做"仅桌面端"限制——Web 端同样可操作，见 §9 的偏差记录）；
  - 额度轮询策略采用开放问题 4 的建议方案：**仅当前槽位账号按周期轮询（默认 5 分钟，`GATEWAY_POOL_POLL_SECONDS` 可调，负值关闭），备用账号用时再查**；
  - 导入源目录由 `GATEWAY_ZCAS_DIR` 指定，指向 zcode-switcher 数据目录（含 `accounts-kimi/`）。

**阶段 2：手动切换（kimi）** ✅ 已实现（2026-10-18）
- Switcher（回读/替换/重启/核验/回滚/记账）+ `POST /api/pool/kimi/switch` + 前端按钮。
- 验收：手动切换后新会话走新账号；旧快照 token 被正确回读更新。
- 落地说明：
  - 切换器在 `internal/accpool/switch.go`；`acpruntime.Manager` 新增 `LiveSessions`/`BusyReasons`/`Reattach`（`internal/acpruntime/switching.go`），`Reattach` 复用现成的 `Reconcile`（新进程 + `session/load` 成功后才替换旧句柄）——kimi 的 ACP 会话恢复能力因此得到确认（开放问题 1 关闭）；
  - 门控：回合进行中或有待答审批时拒绝切换（409，列出来源会话）；切换与额度刷新共用 `Manager.mu` 互斥，防槽位写回竞争；
  - 重挂失败自动回滚槽位并反向重挂；每次切换（含失败/拒绝）记录 `history.jsonl`，随 `GET /api/pool/{kind}` 返回近 50 条；
  - Agent 未启用 ACP 时仍可切换（仅换凭证，供 CLI 使用）。

**阶段 3：自动切换 + 自动继续（kimi）** ✅ 已实现（2026-10-18）
- Decider、被动事件分类器、会话恢复检测、`switchResumeRuns` 预算、通知接入。
- 验收：模拟限额耗尽（注入 `quota_exhausted` 事件）→ 自动切换 → 被打断会话收到"继续"并续跑。
- 落地说明：
  - 编排器为 `internal/accpool/auto.go` 的 `AutoSwitcher`；入口有两条：**被动钩子**（`cmd/gateway/poolhooks.go` 的 `poolQuotaHook`，挂在 AutoContinue 链首位，用 `jev.ContinueContext`+`jev.QuotaStop` 初筛疑似额度故障）与**轮询预警**（`poller.go` 刷新当前账号后越过 95% 阈值即回调 `OnExhausted`）；
  - 误报防护（开放问题 3 的当前方案）：被动事件先用 jev 规则初筛，再 `RefreshQuota` 复确认——只有额度数据确实越过阈值才真正切换，瞬时 429 不误切；真实语料校准仍待收集；
  - 门控全部基于 `history.jsonl`（重启不丢）：切换冷却 10 分钟、每小时最多 6 次、24h 内切走 3 次即放弃该账号、最紧窗口距重置不足 5 分钟时宁可等待；无可用账号时发通知而不动作；
  - 目标选择（`decide.go` 的 `ChooseTarget`）：逐候选刷新额度，选最紧窗口余量最大者，并列取最久未用；
  - 切换续跑走 `ResumeAfterSwitchPrompt` 独立预算 `MaxSwitchResume = 3`（不占用 `MaxAutoContinue`），人为操作同时重置两个计数；切换时 `ignoreBusy` 放行被打断的失败会话本身；
  - 开关持久化在 `<dataDir>/pool/settings.json`（默认启用，`GATEWAY_POOL_AUTOSWITCH=0` 全局关闭），`POST /api/pool/{kind}/settings` + 前端 checkbox；
  - 通知复用 `internal/notifications` 的渠道配置。

**阶段 4：zcode 接入** ✅ 已实现（2026-10-18）
- zcode QuotaProvider + 快照格式（`enc:v1:` 整体搬运）+ 阶段 2/3 等价能力。
- 落地说明：
  - `internal/accpool/zcode.go`：enc:v1 加解密（AES-256-GCM，key=sha256(secret)，与 zcode-switcher 的 zcodeCrypto.js 交叉验证——JS 生成的密文 Go 可解）、账号指纹（config provider apiKey 的 JWT payload → userId/shortId，邮箱 → `em-<djb2>` 去重 id）、候选 token 收集（zcodejwttoken → oauth access_token → config apiKey，加密值解密失败即跳过）；
  - 额度查询（`queryZcodeQuota`）：billing/current+balance（Bearer、app_version/platform 参数、429 退避 0.5/1.5/4s、全 401 等 1.5s 重试一次），全灭后兜底 BigModel monitor（authorization 裸 token、code==200 校验）；归一化为池内 `Quota.Limits`（balance.balances 逐项成窗，无明细时聚合总数合成单窗；monitor 的 TOKENS_LIMIT/TIME_LIMIT → "Token 用量"/"MCP 用量"）；resetTime 统一归一为 RFC3339 供自动切换门控解析；
  - 池内 zcode 账号仍用单文件 `<id>.json`（Account 增加 `credentials`/`config`/`provider` 字段，与 §4.1 的 snap+meta 对设计有出入——导入时折叠，池内格式保持统一）；
  - 切换（`switchZcode`）：镜像 zcode-switcher 的安全策略——**ZCode 桌面端运行时会回写 slot 文件，切换前先 SIGTERM 关闭（8s 超时则拒绝），切换完成后 best-effort 重启**（darwin `open`）；回读漂移 slot、原子写双文件、重挂失败回滚与 kimi 路径一致；
  - 无 token 刷新流程（provider apiKey 为 JWT）：全 401 直接标记 `reauth_required`；
  - `Kinds()` 加入 zcode 后，轮询预警、`poolQuotaHook` 自动切换续跑、settings 开关、通知全部自动生效（阶段 3 的链路本来就是 kind 参数化的）；
  - 前端 pool.js 参数化为双面板（Kimi/ZCode），app.js/index.html 各挂一个实例；
  - 冒烟：JS 端加密夹具 → Go 导入/捕获/切换/回切全通过；假 token 打真实 zcode.z.ai 正确走 401→monitor 兜底→`reauth_required` 路径。

**阶段 5（可选）**：~~Codex 纳入统一池视图（只读展示 + 跳转现有切换流程）~~ ✅ 已实现（2026-10-18）；MiniMax/WorkBuddy 视 ACP 支持情况再议。
- 落地说明：
  - Codex 的重型切换状态机（`internal/account`）保持不变；`/api/pool` 概览追加 codex 只读行（activeId/accountCount），`GET /api/pool/codex` 返回只读详情（`readOnly: true`），所有写操作 400 并指向「账号与接入」面板的既有切换流程；
  - 前端账号对话框新增 Codex 只读面板（无导入/刷新/切换按钮，带指引文案）；
  - MiniMax（mcode）已有 ACP 接入但 zcode-switcher 的 minimax 快照/额度格式未移植，留待有真实需求时再做。

**阶段 6：MiniMax 账号池** ✅ 已实现（2026-10-19）
- `internal/accpool/minimax.go`（池 kind `minimax` ↔ agent kind `mcode`，经 `AgentKind()/PoolKind()` 显式映射）：
  - 快照导入自 zcode-switcher `accounts-minimax/`（其 Tauri 版真实数据目录为 `~/Library/Application Support/com.zcode.account-switcher.tauri/`，plist 已注入 `GATEWAY_ZCAS_DIR` 指向它）；zcode-switcher 快照的浮点数字段（如 `65.0`）经容错 UnmarshalJSON 收纳；
  - 额度：官方 remains API 的 `current_interval`/`current_weekly` 剩余百分比归一为窗口模型；未订阅 Coding Plan 的账号无窗口（与桌面版显示一致）；
  - 切换（`switchMinimax`）：槽位为 `~/.minimax` 六文件登录态——auth token + 投影文件 generation 保持一致、identity 缓存删除、loginEpoch 更换，原子写入、回读校验、失败回滚与 kimi/zcode 同款；
  - 冒烟（真实数据）：导入 5 账号 → 额度刷新全部 active → 真实切换后 `~/.minimax` 六文件自洽 → 回切还原。
- 同期修正：`WindowQuota.Auxiliary` 标记——zcode 的 MCP 用量（TIME_LIMIT）是辅助额度，不参与 `ExhaustedWindows`/`ChooseTarget` 判定（只有主 AI 额度能把账号踢出候选），前端标注「辅助，不影响切换」。

**阶段 7：WorkBuddy agent + 账号池** ✅ 已实现（2026-10-19）
- agent 接入（`internal/acpagents`）：kind `workbuddy`，`codebuddy --acp`（`CODEBUDDY_BIN` → `~/.codebuddy/bin` → PATH），握手超时 60s（技能目录冷启动 ~20s）。已端到端冒烟：网关创建会话 → prompt → completed → 回复正确。
- 账号池（`internal/accpool/workbuddy.go`，移植自 zcode-switcher 的 Rust `workbuddy` 模块）：
  - 双档位 cn（www.codebuddy.cn）/ ai（www.workbuddy.ai），`WORKBUDDY_HOME` 沙箱钩子；时间戳为**毫秒**（与 kimi/minimax 的秒不同）；
  - 信封态凭据（`{$wbEncrypted}`）：导入/捕获/写回全链路原样往返，但不能调 API（额度查询短路报错）、不作为切换目标；信封保护——采集到信封但已有记录持未过期明文时不覆盖；
  - 额度：cn 三路 billing（summary/paid/free，固定 PackageCodes 码表）+ 全空兜底 `get-user-resource`，合成单窗「积分」+ Credits 明细（到期高亮：>730 天视为长期有效、7 天内到期标记）；401 触发一次刷新重试，刷新失败账号置 `reauth_required` 并带真实原因（不再吞成「接口未返回任何资源数据」）；
  - 切换（`switchWorkbuddy`）：桌面槽位四段 `{account, auth, accounts, allAccounts}` 写后按值校验 + `.logged-out` 标记清理 + 切前备份；CLI 生效路径为 apiKeyHelper 模式（`~/.codebuddy-rotate/helper.cjs` 打印当前账号 token、`state.json` 记账、区域 env 与 local_storage 缓存改写；首次切换才接管 `settings.json`，拒绝覆盖他人 helper）；桌面 App 优雅退出（osascript → 8s 窗口 → 残留 kill）后 best-effort 重启；
  - **保守门控**：自动切换（`trigger=="auto-quota"`）在 WorkBuddy.app 运行且 CLI 有活会话（`~/.codebuddy/sessions/*.json` 心跳 <120s）时拒绝，仅通知；
  - 冒烟（真实服务）：导入 2 账号成功；额度刷新正确识别两个存量账号均需重登（一个信封态、一个 refresh token 已失效，真实 API 返回 `12153: refresh token is invalid`）——与桌面版现状一致，需扫码重登后可用。
- 与 Rust 真源的已知偏差：不扫杀 CLI 进程（网关会话经 `Reattach` 重挂，门控覆盖防破坏意图）；cn 三路并发改顺序；信封态目标直接拒绝切换（CLI helper 用不了信封 token）；会话迁移（session.rs）与积分轮换策略（rotate.rs）未移植（后者职责已由池内 quota 自动切换覆盖）。

**阶段 8：网关内原生登录（摆脱 zcode-switcher 依赖）** ✅ 已实现（2026-10-19）
- 状态机 `internal/accpool/login.go`：`Manager.StartLogin/LoginStatus/CancelLogin`；驱动契约 `loginStart` 由各 kind 文件 `init()` 注册（`registerLoginDriver`）；同 kind 新登录取代旧登录；被取代/取消后的迟到完成**不落盘**；`reauthId` 走原位 token 替换；登录成功后后台 goroutine 刷新额度，不阻塞完成。
- HTTP：`POST /api/pool/{kind}/login`（`{provider?, reauthId?}` → `{login, challenge}`）、`GET /api/pool/{kind}/login`（状态轮询）、`POST /api/pool/{kind}/login/cancel`；codex 只读，写操作 400。结构体经 `poolJSONValue` 转 jsoncompat 树输出。
- 前端 `web/pool.js`：各池面板「添加账号」按钮 + `reauth_required` 行「重新登录」；登录弹层支持 provider 选择（minimax cn/en、workbuddy cn/ai、zcode zai/bigmodel）→ 展示 userCode + 授权链接 + 复制 + 剩余分钟，2s 轮询状态，成功/失败/取消均有终态反馈。
- 各 provider 驱动（协议均对照 zcode-switcher Rust 真源移植）：
  - **kimi**：OAuth device flow（`auth.kimi.com` device_authorization + token 轮询，全套 X-Msh-* 设备头，device-id 持久化于 `pool/kimi/device-id`），身份走 `coding/v1/me`；pending/slow_down/15 分钟上限。
  - **minimax**：device flow + PKCE S256（cn=account.minimax.cn / en=account.minimax.io），兼容标准与 account 两种响应变体（后者无 device_code、以 user_code 轮询、毫秒 interval）；身份走 matrix `user/info`（复用 yy/x-timestamp/x-signature 签名头）。
  - **workbuddy**：扫码登录（`auth/state` → 2s 轮询 `auth/token`，10 分钟上限），直接取得**明文 token**——信封态账号重登即恢复可用；cn/ai domain 档位一致性校验，profile 拉取失败降级不阻断。
  - **zcode**：代理 OAuth（`zcode.z.ai/api/v1/oauth/cli/init` → 轮询 `poll/{flow_id}`；authorize_url 必须 https 且带非空 state）；zai 专属 business token 交换 + billing 就绪渐进等待（尽力而为）；credentials/config 用既有 enc:v1 加密，与 zcode-switcher 同机互通。
- 测试：`accpool.RegisterLoginDriverForTest` 供 httpd 等外部包注入假驱动，避免测试触网；四家 httptest 套件覆盖全流程/变体/拒绝/超时/取消。

## 11. 开放问题

1. ~~**kimi ACP 会话恢复能力**~~（已关闭，阶段 2 落地）：切换后的会话重挂复用 `Manager.Reconcile` → `openFresh` → `session/load`，与手机端打开既有 kimi 会话是同一路径，架构上已确认可用；冒烟中重挂调用链按预期执行。
2. **Agent Host 模式下的进程重启**：经 Unix socket 让 `agent-host` 重启指定 agent 的协议需要新增控制消息，还是直接复用现有断开重连？需看 `internal/acphost` 细节。
3. **被动事件的误报率**：ACP 错误文本里"额度耗尽"与"瞬时 429 限流"如何区分？前者触发切换，后者应退避重试。需要收集真实报错语料校准分类器。
4. ~~**轮询频次与账号封禁风险**~~（已决策，阶段 1 落地）：仅当前槽位账号高频轮询（默认 5 分钟，可调/可关），备用账号在手动刷新或切换时才查询。
5. ~~**zcode-switcher 的代码复用方式**~~（已决策，阶段 4 落地）：额度/凭证逻辑已移植为 Go（`internal/accpool/zcode.go`），与网关同进程同语言；enc:v1 加解密与 zcode-switcher 的 JS 实现交叉验证通过。
