# OpenDots / OpenMuse 学习笔记

> 状态：调研完成，未实施 · 调研日期：2026-10-12 · 参考项目：[CopilotKit/OpenDots](https://github.com/CopilotKit/OpenDots)、[CopilotKit/openmuse](https://github.com/CopilotKit/openmuse)（浅克隆于 `/tmp/opendots-explore`、`/tmp/openmuse-explore`）

两份调研的源码结论在本文只保留与本项目相关的部分，按"机制 → 对照 → 可借鉴"组织。此前借鉴 Codeg 的产物见 [event-sync-plan.md](event-sync-plan.md)。

## 1. 项目概览

| | OpenDots | OpenMuse |
| --- | --- | --- |
| 定位 | 常驻 AI 同事工作台模板（Spaces 文档 + 每 Dot 独立容器计算机 + Slack + 语音） | 个人 agent alpha（委托任务 + 浏览器/终端执行 + iOS/Android/Web 三端 App） |
| 形态 | 单进程 Hono + React SPA，响应式适配手机，无原生 App、无推送 | Expo RN 三端一套代码 + server + 独立 Playwright worker + Docker 计算机 |
| 技术底座 | CopilotKit + AG-UI，对话持久化外包给 CopilotKit Intelligence（云/本地 Docker） | 同左；任务引擎自研（PGlite/PG 单表 jsonb + SQL CAS） |
| 移动端同步 | 3 秒全量轮询 `/api/state`，无增量 | 3 秒全量快照轮询 + 聊天走 AG-UI SSE，无推送 |

**先说结论**：两者在"手机端实时性与通知"上落后于本项目（增量 SSE + Bark/ntfy + 服务端持久排队都已具备；OpenMuse 的 follow-up 队列只在客户端内存，OpenDots 完全靠轮询）。真正值得学的是它们的**审批幂等执行、任务状态机、通知对账**这三块服务端纪律。

## 2. 值得借鉴的机制

### 2.1 幂等审批 receipt（两项目的汇合点，优先级最高）

OpenDots 的 Approve & save / Approve & run 与 OpenMuse 的 action review，核心是同一套模式：

- **receipt 以 `(threadId, toolCallId)` 为主键**，`BEGIN IMMEDIATE` 事务内先查后写；重试同参数返回原结果，参数变化返回 409 要求新审（OpenDots `src/server/pages.ts:156-201`）。
- **执行恰好一次**：`claim` 用原子 SQL 状态迁移（`awaiting_review → executing`，条件含过期时间与所属任务状态），双击/双端并发只成功一次（OpenMuse `apps/server/src/db.ts:116`）。
- **审批绑定内容 hash**：proposal 对 input+connection+target 计算 sha256，decide 时 hash 不匹配即 409，防止"批准 A 执行 B"（OpenMuse `packages/domain/src/index.ts:143`）。
- **审批卡渲染服务端存储的参数，不渲染模型转述**；执行路由只认存储的请求，绝不执行批准请求附带的参数（OpenDots `src/client/ConnectionActionCard.tsx:46-66`）。
- **过期即废**：TTL（OpenDots 1 小时 / OpenMuse 30 分钟）到期 CAS 到 `expired`，提示重新发起。
- **receipt 比 effect 活得久**：页面删除后 receipt 故意保留，重试旧审批只会拿到 `{deleted:true}` 不会重建页面（OpenDots `src/server/pages.ts:302-303`）。
- **outcome_unknown 不自动重试**：执行结果未知时状态单列，retry 被拒绝并要求先核对实际效果（OpenMuse `apps/server/src/engine/actions.ts`）。

对照本项目：审批回应已绑定 request ID、过期/不支持的请求不转发，且已有 auditable approval details。缺口在**执行侧幂等**——同一审批在双端/弱网重试下的"恰好一次"没有 receipt 保护。此模式对 Jev 受控自动批准尤其重要：自动批准与人工批准应竞争同一个 claim，谁先到谁执行。

### 2.2 waiting_input 一等状态（OpenMuse 任务状态机）

`AgentTask` 九态状态机（`queued / running / waiting_approval / waiting_input / scheduled / paused / succeeded / failed / cancelled`）。"等用户补充信息"是任务状态而非对话轮：执行中返回 `{status:'waiting_input', question, state:{missingFields}}` → 发通知 → 手机填完 `POST /tasks/:id/input` → CAS 回 `queued` 续跑，进度由 `state` checkpoint 保留（`apps/server/src/engine/service.ts:1011`、`:335`）。

配套的 worker 纪律：1 秒 tick 扫表认领（租约 CAS + 20 秒续期 + 每次外部调用前 guard 校验租约）；checkpoint 粒度是"每个外部副作用之后"；崩溃回 `queued` 显式等 Retry；取消传播到 pending 审批自动 deny。

对照本项目：permission request 已是类似的"暂停等人"，但开放性追问（例如 Jev 评估缺上下文、迁移需要用户确认目标）目前只能落回对话轮。waiting_input + 输入端点 + 重新入队与本项目的排队消息天然契合。

### 2.3 通知确定性 key + 重启对账（OpenMuse）

通知不用投递队列，而是**发布/对账**：任务 settle 时 `publishOutcome()`，另有一个每 60 秒把全部任务重放一遍的 `maintain()`——进程在"提交结果与发通知之间"崩溃也能补发，靠确定性 key（`task-done:${id}`、input 带 question hash、告警带 `alertSequence + 页面 hash`）用 `insertIfAbsent` 去重（`apps/server/src/engine/service.ts:85`、`:719`）。

对照本项目：完成通知已有 `_completion_key` 去重与 `resume_watched()` 重挂订阅，但**崩溃窗口内应发未发的通知没有重放对账**，网关重启即丢。可给关键通知（待确认、完成、封禁）补一个启动时/周期性对账。

### 2.4 前台感知轮询三件套（OpenMuse `apps/mobile/src/agent-workspace.tsx`）

- 监听 `AppState` / `visibilitychange`，回前台**立即** poll 而非等下一轮；
- `requestVersion` ref 丢弃乱序到达的旧响应；
- mutation 成功后跟一次 refresh，且"成功的写不因随后的读失败而回滚 UI"。

对照本项目：`web/app.js` 的 5 秒 activity 轮询是固定间隔，手机切回浏览器标签页时要白等最多 5 秒。三件套都是十行级改动。

### 2.5 配置指纹快速中断（OpenDots）

agent 每 100ms 比对 settings/Dot 配置/connections 的 `fingerprint(id:updatedAt)`（updatedAt 严格递增），任何变化 abort 当前 run（`src/server/dot-agent.ts:112-143`）；权限撤回由 50ms watcher 中止在途计算机动作。

对照本项目：Jev 设置已即时生效，但**在途的评估/自动批准请求是否会因配置（API key 清空、策略收紧）变化而中止**值得核对。

### 2.6 其他小件

- **durable 消息标记**：用消息 id 前缀（`opendots:voice_receipt:` 等）做跨同步的持久渲染标记，不依赖会丢的 metadata（OpenDots `src/shared/voice-receipt.ts`）——对 timeline 行的来源标记适用。
- **签名短期 URL**：15 分钟 HMAC URL 供 `<img>`/WebView 取截图文件（OpenMuse `apps/server/src/auth.ts:42`）。本项目附件走登录态同源路由已够用，仅在将来做"无 Cookie 客户端"时需要。
- **SSRF 防范范本**：独立只读浏览器服务对目标 DNS 解析后逐 IP 校验公网、禁 redirect、DNS-pinned（OpenDots `src/browser/security.ts`）——与 Jev HTTP 代理端点校验同一问题域，可比对现状。
- **命令 receipt + operationId**：每条 shell 命令持久化 stdout/exit/timeout/interrupted/truncated，中断时明确"结果未知，先检查再重跑"（OpenMuse `apps/server/src/computer.ts:416`）。
- **降噪声可视化**：只有最新的 browser tool call 卡片配实时截图（3 秒轮询），历史卡只显示动作与状态；人工接管后强制 agent 重新 snapshot。若本项目将来做"看 agent 干活"的预览，截图轮询 + 这个降噪策略是最低成本路线（两家都没做视频流）。
- **头像状态行**：手机端头像下一句话实时显示"agent 正在干什么"（"Ready to review ·…"），呈现成本极低。
- **多入口审批降级**：无 UI 的入口（Slack/定时/语音）显式禁用审批工具并提示回到有 UI 的入口——可对应"电脑前无人时审批自动落手机推送"。

## 3. 回归测试类型清单（两者共同的验证文化）

OpenMuse `tests/` 明确覆盖三类竞态回归，本项目可对号补齐：

1. **审批竞态**：双端同时批准 / 批准与取消并发 / 批准与任务状态变化并发（claim 只成功一次）；
2. **崩溃恢复**：进程在副作用边界重启后，重试不重放、通知补发、状态不悬挂；
3. **配置变更中断**：策略/key 变化时在途评估与自动批准的 Abort 语义。

（本项目已有 queue edit/dispatch race 覆盖，审批侧是缺口。）

## 4. 落地优先级建议

1. **审批执行幂等（claim + hash 绑定 + 过期即废）**——`bridge/service.py` 审批路径与 Jev 自动批准共用一个 claim 入口，双端/自动/重试只执行一次；
2. **通知对账补发**——关键通知加确定性 key 与启动重放，消除崩溃窗口丢通知；
3. **activity 轮询三件套**——`web/app.js` 回前台立即刷新 + 乱序丢弃；
4. **waiting_input 建模**——Jev/迁移场景的开放性追问走任务状态而非对话轮；
5. **审批竞态回归测试**——按第 3 节清单补 `tests/`。
