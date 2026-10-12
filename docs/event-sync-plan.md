# 事件同步协议升级计划（借鉴 Codeg）

> 状态：已实施（阶段一/二完成并通过自动回归；阶段三第 2 条弱网手工验收待做）· 起草日期：2026-10-11 · 实施日期：2026-10-11 · 参考项目：[spacering-net/codeg](https://github.com/spacering-net/codeg)

## 1. 背景与目标

项目当前通过两条通道向手机网页推送会话更新：SSE（`/api/sessions/<id>/events`）与长轮询（`/poll`、`/changes`）。两条通道语义割裂——SSE 每次变更推送**全量 view**，长轮询已是增量；且仓库内前端只使用长轮询，SSE 增量能力缺失。

借鉴 Codeg（多智能体工作台，`src-tauri/src/web/ws_attach.rs`、`acp/event_stream.rs`）验证过的「快照 / 增量重放」同步协议，统一本项目两条通道的语义，达成：

1. **重连不全量**：弱网断线重连后按游标续传增量，只有游标失效才回退快照。
2. **流量可衡量下降**：SSE 不再每次推全量 view，大会话（数千行消息）单次更新从数百 KB 降到单条变更行。
3. **重连行为可预期**：明确的 ready 握手、指数退避、401 与网络错误区分处理。

非目标见第 9 节；与 Codeg 的差距分析见第 3 节。

## 2. 现状盘点（实施前快照）

> 本节描述起草时的代码状态，行号与「无消费者」等结论以当时为准；最终落地形态与代码位置见文末「实施结果」。

### 2.1 服务端

- **会话状态计数**：`bridge/service.py:157-200` —— 每个会话持有单调递增的 `sequence` 与 `threading.Condition`，任何状态变更 `sequence += 1` 后 `notify_all()`。
- **时间线投影**：`bridge/timeline.py` —— `Timeline` 把桌面权威 view 投影为手机端行模型：
  - `epoch`（UUID）：消息顺序被替换/重排时轮换，显式作废所有旧游标；
  - `versions`：每个 sequence 保存全部行的哈希版本，仅保留最近 16 个 sequence；
  - `changes(after, epoch, start)`：只返回两次 sequence 间哈希变化的行；epoch 不符、游标超出 16 窗口、增量超过 100 行或 192 KiB 时回退 `{**page(), 'reset': True}`。
- **HTTP 端点**（`bridge/httpd.py`）：
  - `stream()`（`:696-735`，`GET .../events`）：SSE，每次 sequence 变化推送**全量 view**（`event: state`），12 秒心跳注释，登录失效推 `event: logout`；忽略 `Last-Event-ID`，重连必全量；
  - `poll()`（`:645-659`，`GET .../poll?after=N`）：长轮询，返回全量 view 或 `null`；
  - `changes()`（`:681-694`，`GET .../changes?after&epoch&start`）：长轮询 12 秒，返回增量行或 reset 页；
  - `/api/auth`（`:256-264`）：按 Host 宣告传输方式——`*.trycloudflare.com` 隧道走 `poll`，其余 `sse`。

### 2.2 前端

- **聊天视图**：`web/timeline.js` 的 `ChatTimeline.updates()`（`:218-231`）是唯一实时更新源——`changes` 长轮询循环，成功后停 200ms，失败后固定 3 秒重试；`apply(page, reset)` 已支持增量行应用与整页重置；`generation` / `windowRevision` 双重守卫丢弃过期响应（`:222-227`）。
- **会话列表**：`web/app.js:74` 每 5 秒 `setInterval` 轮询 `/api/activity`。
- **锚点滚动恢复**：`web/timeline.js:48-57` 基于可视锚点的滚动位置保持，与同步协议无关但接入时需保留。

### 2.3 已核实的关键事实

以下结论经全仓库检索核实，直接影响方案边界：

1. **仓库内前端没有任何 `EventSource` 或 `/events` 的消费代码**（`web/*.js` 全文检索无匹配）——SSE 端点是兼容/预留通道，改造它不影响现有网页行为。
2. `/api/auth` 宣告的 `transport` 字段同样没有仓库内消费者；它面向外部移动端客户端，字段语义必须保持。
3. SSE 与长轮询共享同一认证链路（cookie + `/api/auth` 探测），401 时 SSE 推 `logout` 事件、长轮询返回 401。
4. Python 测试为 unittest（CI：`python -B -m unittest discover -s tests -v`），前端测试为 `node --test tests/*.test.cjs` / `*.test.js`。

## 3. 差距分析（对照 Codeg 同步协议）

Codeg 的 WS attach 协议（`web/ws_attach.rs`、`acp/event_stream.rs`）与本项目现状逐项对照：

| 协议要素 | Codeg | 本项目现状 | 差距 |
| --- | --- | --- | --- |
| 游标续传 | attach 带 `since_seq`，服务端决定 replay/snapshot | `changes` 端点已有 `(after, epoch)` 游标 + reset 回退 | SSE 通道无游标，重连必全量 |
| 就绪握手 | `__ready__` 帧门控，收到才算订阅生效 | 无 | 重连窗口期可能丢更新（长轮询靠下一轮请求兜底，时延高） |
| 锁序不变量 | 读锁内完成「读状态 + 注册订阅」，emit 走写锁 | `condition.wait_for` 模式天然满足（先读后等） | 无差距，需在新代码中保持 |
| 重放窗口 | 环形缓冲 128 条 / 128 KiB，超限清缓冲 | `versions` 保留 16 个 sequence，超限回退 reset | 语义等价，无需改造 |
| 重连退避 | 1s→32s 左移退避，收到 ready 才清零 | 前端固定 3 秒重试 | 缺失 |
| 错误分类 | 401 → 登出；网络错误 → 永远只重连 | 前端不区分，无限重试 | 缺失 |
| 多路复用 | 单 WS 按 `subscription_id` 复用 | 每会话一条连接 | 不引入（见第 9 节） |
| 事件溯源 | 事件级 envelope + seq | sequence 级 view/行投影 | 不引入（见第 9 节） |

## 4. 总体决策

1. **不引入 WebSocket**。网关基于 stdlib `ThreadingHTTPServer`（`bridge/httpd.py:66`），引入 WS 需要新增依赖或手写帧编解码，违背项目零前端构建、零重依赖的约束。事件流是服务端→客户端单向，SSE + 长轮询完全够用。
2. **游标统一为 `<epoch>.<sequence>`**。epoch 作废语义沿用 `Timeline` 现有实现，SSE 经 `Last-Event-ID`（EventSource 自动携带）与 `?since=` 查询参数兜底传递。
3. **SSE 增量化为 opt-in**。请求带 `?incremental=1` 时启用新帧协议；不带的请求保持现有全量 `event: state` 行为，外部客户端与旧版前端零影响。
4. **前端收敛出单一传输模块** `web/transport.js`，按 `/api/auth` 宣告选择 SSE 订阅或长轮询循环，对上层（`ChatTimeline`）暴露同一订阅接口。
5. **复用现有增量语义，不新造**。`timeline.changes()` 的 reset 回退就是 Codeg 的 snapshot fallback；本计划只把它接上 SSE 与游标，不重写投影层。

## 5. 协议设计

> 本节为设计稿；实现偏差（首帧 reset、ready 帧无 `id:`、前端 API 签名、SSE 重连自管等）以文末「实施结果」为准。

### 5.1 SSE 增量帧协议

`GET /api/sessions/<id>/events?incremental=1` 的帧类型（`bridge/httpd.py` `stream()` 改造）：

| 帧 | data | 时机 |
| --- | --- | --- |
| `event: ready` | `{epoch, sequence}` | 订阅建立后首帧；客户端收到才认为通道生效（对应 Codeg `__ready__`） |
| `event: changes` | `timeline.changes()` 同构 payload（增量行） | 每次 sequence 前进且可增量表达 |
| `event: reset` | `timeline.page()` + `reset: true` | epoch 不符、游标超窗、增量超限时替代 changes |
| `event: logout` | `{}` | 登录失效（沿用现有语义） |
| `: heartbeat` | — | 无变更时每 12 秒（沿用现有） |

`id:` 一律为 `<epoch>.<sequence>`，浏览器自动重连时经 `Last-Event-ID` 回传。

**锁序不变量**（沿用现有 condition 模式，等价 Codeg 读锁内 subscribe）：进入流循环时，在 `session.condition` 内完成「解析游标 → 判定首帧（changes/reset）→ `viewers += 1`」，然后才发送 ready 帧与首帧。保证判定与订阅之间无遗漏。

**每次推送的增量来源**：循环内记录 `last_seq`，唤醒后调用 `timeline.changes(last_seq, epoch, start='')`；返回带 `reset` 时改发 reset 帧。无需改动 `Timeline` 数据结构——`versions` 窗口即重放缓冲。

### 5.2 游标与重连判定

服务端收到带游标的连接（`Last-Event-ID: <epoch>.<sequence>` 或 `?since=`）时，在 condition 内按序判定：

1. 游标缺失或格式非法 → 视为新订阅，首帧发 `changes`（从 sequence -1 起，等价全量行集）；
2. 游标 epoch 与当前不符 → 首帧发 `reset`；
3. 游标 sequence 超出 `versions` 16 窗口 → 首帧发 `reset`；
4. 其余 → 首帧发 `changes`（`after` = 游标 sequence），进入正常增量循环。

判定逻辑直接复用 `timeline.changes()` 自身的回退分支（`timeline.py:120-129`），服务端只负责把返回值映射到帧类型：`reset in payload` → `event: reset`，否则 → `event: changes`。

### 5.3 长轮询通道对齐

- `changes` / `poll` 端点**保持不变**——它们已是增量 + reset 语义，是隧道（trycloudflare）场景的传输层。
- 前端两条通道共用同一份帧应用逻辑（见 5.4），长轮询响应直接映射为等价的 changes/reset 帧。
- `/api/auth` 的 `transport` 字段语义不变，新增内部字段需另议（本期不加）。

### 5.4 前端传输层

新增 `web/transport.js`（无框架、无构建，注册进 `httpd.py` 的 `STATIC` 表），对上层暴露：

```js
BridgeTransport.subscribeSession(id, host, {onChanges, onReset, onStatus}) => unsubscribe
```

内部实现要点：

- **通道选择**：`/api/auth` 宣告 `sse` 时用 `EventSource`（cookie 认证天然兼容，CSRF 只约束写操作）；宣告 `poll` 时走 `changes` 长轮询循环，把响应映射为同一组回调。
- **退避**：失败计数左移 `Math.min(1000 << Math.min(n-1, 8), 32000)`；**收到 ready 帧（SSE）或一次成功响应（轮询）才清零**——对应 Codeg「只有应用层就绪才重置退避」。
- **错误分类**：SSE `onerror` / 轮询 catch 后先 `fetch('/api/auth')` 探测——401 → 调 `onStatus('unauthorized')` 走登录流程，停止重试；其余 → `onStatus('reconnecting')` 并继续退避。**只有确定性 401 才登出，网络错误永远只重连**。
- **游标维护**：SSE 侧浏览器自动处理 `Last-Event-ID`；轮询侧沿用 `timeline.js` 现有 `(sequence, epoch, before)`。
- **过期守卫**：沿用 `generation` / `windowRevision` 模式——每次会话切换递增 generation，回调内校验。

`web/timeline.js` 的 `updates()` 循环改为订阅驱动：`onChanges` → 现有 `apply(page)`，`onReset` → `apply(page, true)`；`apply`、锚点恢复、详情加载逻辑不动。

## 6. 分阶段实施

### 阶段一：服务端增量 SSE

1. `bridge/httpd.py` `stream()`：解析 `?incremental=1` 与游标（`Last-Event-ID` 优先，`?since=` 兜底）；按 5.1/5.2 实现 ready / changes / reset 帧；保留无参数请求的全量 `state` 帧行为。
2. `bridge/timeline.py`：仅在 `changes()` 返回值形态不足以区分「无变化」与「空增量」时补充辅助方法；预期零改动。
3. 单测：`tests/test_timeline.py` 沿用现有 fake view 基建，新增游标判定分支用例；`tests/test_bridge.py` 参照 `test_poll_is_authenticated_and_returns_current_state`（`:1108`）新增增量 SSE 用例（ready 首帧、游标续传、epoch 失效回退 reset、logout 帧保留、无 incremental 参数时旧行为不变）。
4. 验证：`python -B -m unittest discover -s tests -v` 全量通过（基线 570 过 / 10 跳过）。

### 阶段二：前端传输层与接入

1. 新增 `web/transport.js`；`bridge/httpd.py` 的 `STATIC` 表与 `web/index.html` 各注册一处。
2. `web/timeline.js`：`updates()` 改为 `BridgeTransport.subscribeSession` 驱动；保留 `apply` / 滚动锚点 / `pause` 语义。
3. 前端单测：新增 `tests/transport.test.js`（node --test 风格，参照 `tests/session-open.test.js`），mock EventSource 与 fetch，覆盖通道选择、退避公式、401 停重试、generation 过期守卫。
4. 验证：`node --test tests/transport.test.js` 及既有前端测试通过；i18n 无需新增词条（状态文案复用现有「重新连接中」）。

### 阶段三：验证与收尾

1. 全量回归：`python -B -m unittest discover -s tests -v` + `npm run test:desktop`（确认无前端回归）+ `node --test tests/*.test.js`。
2. 手工验收（参照 `VERIFICATION.md` 的既有格式追加一节）：
   - 局域网 SSE 模式：发送长回答，确认只传增量帧；中途断网 30 秒恢复，确认游标续传而非全量；
   - trycloudflare 隧道模式：确认自动落回长轮询且行为不变；
   - 浏览器 devtools 3G 节流下重复上述两项；
   - 退出登录后确认 SSE 收到 logout、前端进入登录流程且不再重试。
3. 文档：更新 `VERIFICATION.md`；本计划文末补「实施结果」小节。

## 7. 测试计划

**服务端（unittest）**：

| 用例 | 断言 |
| --- | --- |
| 新订阅无游标 | 首帧 ready，随后 changes 含全部行 |
| 有效游标重连 | 首帧 changes 只含游标之后的变更行 |
| epoch 不符 | 首帧 reset |
| sequence 超出 16 窗口 | 首帧 reset |
| 增量超 100 行 / 192 KiB | reset 帧（复用 changes 回退分支） |
| 登录失效 | logout 帧后连接关闭 |
| 无 `incremental=1` | 帧格式与升级前逐字节一致（兼容） |

**前端（node --test）**：通道选择（sse/poll）、退避序列（1s→2s→…→32s 封顶）、ready 清零退避、401 停止重试并回调 unauthorized、unsubscribe 幂等。

**弱网手工**：见阶段三第 2 条。

## 8. 兼容性与回滚

- **协议兼容**：新 SSE 行为由 `?incremental=1` 显式开启；旧客户端（无参数）收到的字节流与升级前一致。`changes` / `poll` / `/api/auth` 的响应格式均不变。
- **回滚路径**：前端传输层保留长轮询实现为永久回退通道（隧道场景本来就用它）；SSE 增量若出问题，`/api/auth` 宣告侧改回 `poll` 即整体回退，无需发版回滚。
- **外部客户端**：`/api/auth` 的 `transport` 字段与 `events` 旧帧格式是外部移动端客户端的契约（见 2.3），本计划不破坏。

## 9. 非目标（YAGNI）

1. 不引入 WebSocket（见第 4 节决策 1）。
2. 不做多会话多路复用——手机端单会话浏览，每会话一条连接可接受。
3. 不引入事件溯源 / 事件级 envelope——sequence 级投影已满足 UI 需求。
4. 不动 `web/app.js` 的 5 秒活动轮询与通知体系——与本次通道升级正交。
5. 不实现 Codeg 的多端订阅扇出（InternalEventBus / 侧频道）——本项目无多窗口场景。

## 10. 验收标准

1. **功能**：SSE 增量模式下，断线 30 秒内重连成功且不重传历史；epoch 轮换（分支编辑/消息重排）后客户端自动整页重置。
2. **流量**：1000 行会话中单条消息更新，SSE 单帧 < 增量行大小 + 200 字节帧头（对比升级前全量 view 的数百 KB）。
3. **重连**：连续失败时退避间隔序列为 1, 2, 4, …, 32 秒封顶；401 后零重试。
4. **回归**：Python 全量测试通过且无新增跳过；既有前端测试全部通过；无 `incremental=1` 的 SSE 字节流与升级前一致（用例锁定）。
5. **文档**：`VERIFICATION.md` 新增本节验收记录。

## 附：参考实现索引（Codeg）

| 主题 | 位置（`/tmp/codeg-explore` 克隆，上游 `spacering-net/codeg`） |
| --- | --- |
| attach 协议帧定义 | `src-tauri/src/web/ws_attach.rs:38-90` |
| 读锁内「判定+订阅」 | `src-tauri/src/web/ws_attach.rs:115-196` |
| 环形缓冲与洞判定 | `src-tauri/src/acp/event_stream.rs:90-194` |
| emit 锁序 | `src-tauri/src/web/event_bridge.rs:530-614` |
| 前端退避与 401 三分法 | `src/lib/transport/web-transport.ts:303-547` |
| 前端游标与去重 | `src/lib/transport/web-event-stream.ts:79-192` |

## 实施结果（2026-10-11）

阶段一、阶段二已落地并通过全量自动回归（Python 639 过/6 跳过；`npm run test:desktop` 158 过，含新增 7 项）。与计划文本的偏差及补充决策：

1. **新订阅首帧是 `reset` 而非字面的「changes 全量行集」**。严格按 §5.2 末段「服务端只把 `timeline.changes()` 返回值映射为帧类型」实现：`after=-1` 必然落入 `changes()` 的 epoch/窗口回退分支，因此新订阅收到的是 reset 快照页（含 `before`/`hasMore` 分页游标）。语义与计划的快照建立等价，且与 `/changes` 端点既有行为一致。
2. **ready 帧不携带 `id:`**。若 ready 携带当前游标，只收到 ready 即断线的重连会带着「已到最新」的游标跳过快照。
3. **唤醒谓词同时比对 `session.sequence` 与 `session.timeline.epoch`**（对齐 `/changes` 端点的等待条件）。否则「epoch 轮换但 sequence 恰好相等」的连接会静默等待不发帧。
4. **前端 API 落地为 `BridgeTransport.subscribe(url, handlers)`**（`url` 为 action→path 构造器，复用 `app.js` 的 `sessionUrl`/host 模式），与计划的 `(id, host, opts)` 签名等价，但避免了改动 `openChat` 的构造现场。轮询侧游标每次请求前经 `cursor()` 回读 timeline 当前的 `(sequence, epoch, before)`——保留原 `updates()` 循环「历史窗口扩张竞态不丢增量」的语义；原 `generation`/`windowRevision` 守卫针对的是异步轮询间隙，订阅回调同步到达后不再需要。
5. **SSE 重连由 transport 自管**：`onerror → close → 退避重建`，`?since=` 携带最后帧 id；不使用 EventSource 内建重试，退避与游标才可同时受控。
6. **错误分类取三源**：轮询 401、`/api/auth` `authenticated:false`、SSE `logout` 帧，均立即终止订阅并回调 `unauthorized`（`app.js` 映射 `showLogin`）；其余错误按 1s→32s 封顶退避，ready 帧/一次成功响应清零计数。
7. **单测文件为 `tests/transport.test.cjs`**（仓库约定 `.js` 为浏览器人工检查脚本、`.cjs` 为 node --test 自动化用例），已加入 `npm run test:desktop`。服务端新增 4 项 HttpTests 帧协议用例与 1 项 IntegrationTests 端到端用例（真实 Bridge + DesktopFixture：ready → 首帧 → 桌面 snapshot → 增量 changes 帧，id 即 `epoch.sequence`）。
8. **人工浏览器检查脚本同步**：`timeline.test.js` 在文件头 stub 掉 `BridgeTransport`（保持渲染测试聚焦）；`session-open.test.js` 与 `modes.test.js` 的 fetch fixture 补 `/api/auth`。
9. **待手工验收（阶段三第 2 条）**：真机弱网（断线 30 秒游标续传、devtools 3G 节流）、trycloudflare 隧道回落长轮询、真机登出后的 logout→登录流程。`VERIFICATION.md` 已记录本次自动化验证的边界。
