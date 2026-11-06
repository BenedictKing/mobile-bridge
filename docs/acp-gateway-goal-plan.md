# ACP 网关托管 Goal 实施计划

> 状态：首版已实现并部署（2026-11-06）；fake ACP/Jev 自动续轮、无 Jev 人工复核、浏览器能力门控已验证，真实 Agent 的多轮执行仍待专门验收。
>
> 范围：为没有原生 Goal 接口的 ACP Agent 增加受 Jev 评估约束的网关托管 Goal。

## 目录

1. [摘要](#1-摘要)
2. [目标与非目标](#2-目标与非目标)
3. [实施前基线](#3-实施前基线)
4. [产品语义](#4-产品语义)
5. [状态机](#5-状态机)
6. [架构与持久化](#6-架构与持久化)
7. [Jev 评估策略](#7-jev-评估策略)
8. [HTTP 与前端](#8-http-与前端)
9. [分阶段实施](#9-分阶段实施)
10. [验证与验收](#10-验证与验收)
11. [兼容、发布与回滚](#11-兼容发布与回滚)
12. [决策记录](#12-决策记录)

## 1. 摘要

为没有原生 Goal 接口的 ACP Agent 提供一个由网关托管的 Goal 执行循环：网关保存长期目标，在每轮 ACP 响应结束后让 Jev 对照目标判断完成状态；只有 Jev 明确判断仍未完成、继续安全且置信度达标时，网关才向同一 ACP 会话提交下一步提示。

这是网关编排能力，不是 ACP 标准能力，也不是 Jev 直接执行任务。ACP Agent 仍负责模型调用、工具执行和原生审批；Jev 只对目标进展作判断。Codex 原生 Goal 与 MiniMax/mcode 的扩展 Goal 保持原有路径。

实现重点是可恢复与有界：目标状态持久化；每个自动派发有稳定 ID；未知送达结果不重放；待审批、用户停止、额度问题、Jev 错误或低置信度均停止自动推进并呈现可处理状态。

## 2. 目标与非目标

### 目标

- 为 Kimi、Claude、DeepSeek、ZCode、OpenCode、WorkBuddy 等没有 Goal transport 的 ACP 会话提供网关托管 Goal。
- 保留用户可见的创建、状态读取、暂停、继续、修改和取消操作；跨网关重启保留目标及其派发状态。
- 复用 ACP 的普通 `session/prompt` 执行任务，并用独立的 Jev Goal 评估判断迭代是否完成。
- 让审批卡、提问卡、队列、停止按钮和既有时间线继续承载 Agent 的实际执行交互。

### 非目标

- 不把 Goal 扩展包装成 ACP 原生接口，不为没有对应协议的 Agent 伪造 `session/set_mode` 或 Goal RPC。
- 不替代 Codex 原生 Goal、mcode 原生扩展、团队 Run 或 Jev 权限审核；不因启用 Goal 而自动批准权限。
- 不在没有 Jev 评估或 Jev 结果不确定时，基于启发式猜测自动继续。
- 初版不承诺 ACP Agent 之间迁移 Goal，不提供跨会话并行 Goal，也不提供通用 Token 预算；只有来源准确的 Agent 用量才能用于硬预算。

## 3. 实施前基线

实施前只有 `mcode` profile 声明 `goals`，并通过自定义 RPC 接入 Goal；Codex 则由 `internal/goal` 侧车读取和写入桌面原生 Goal。既有实现位于 `internal/acpagents/acpagents.go`、`internal/httpd/goal_send.go` 和 `internal/goal/goal.go`。

现有 Jev 自动继续解决的是“已结束的一轮是否应该恢复”，不是有持久目标的多轮执行。`ContinueContext` 只收集最近一轮的用户请求、assistant 输出片段、错误和停止原因，没有独立保存的目标文本；`ContinueAssess` 使用固定的 complete / stop_kind / continue 问题集。生产 hook 还要求全局 `autoContinue` 开启、会话近期被查看且无待答请求，随后发送固定文本“继续”，并消耗 `MaxAutoContinue=5`。相关代码位于 `internal/jev/continue.go`、`cmd/gateway/jevhooks.go` 与 `internal/acpruntime/manager.go`。

本次实现将 Goal 存储放在 `internal/acpgoal`，Goal 专用 Jev 评估放在 `internal/jev/goal.go`，HTTP 与派发放在 `internal/httpd/acp_goal.go`；它不借用 `ContinueAssess` 的中断回退，也不共用普通自动继续预算。

当前 provider 路由与差异见 [架构能力矩阵](../ARCHITECTURE.md#能力矩阵)。以下仍是完整目标契约；涉及重启对账、通知和真实 Agent 行为的条目以实际测试结果为准。

## 4. 产品语义

### 创建与运行

- 创建 Goal 时保存用户输入的目标，并在同一 ACP 会话发送首轮任务。首版沿用原生 Goal 的发送约束：只接受直接发送，不接受 queue、steer 或附件；目标长度上限为 4,000 字符。
- 网关和 ACP Agent 进程必须保持运行。浏览器关闭不等于暂停；Goal 是用户显式启动的后台任务。继续执行仍受 Jev、待审批状态和硬迭代上限约束。
- Goal 自动派发使用可见的 ACP 用户消息和普通时间线，不伪装成 Agent 内部系统调用。派发文本包含目标和已确认的进展，避免每轮只发送空泛的“继续”。
- 同一会话最多有一个活动的网关 Goal。已经有原生 Goal 或另一项活动 Goal 时，创建请求返回冲突并说明现有状态。

### 人工控制

- **暂停**阻止后续 Jev 评估和自动派发；在途 ACP turn 自然结束，不调用 `session/cancel`。若 pause/cancel 到达时状态为 `dispatching`，先持久写入待处理控制请求；远端 prompt 结果确认后应用控制，结果未知则保留 unknown 并要求核实。
- **继续**恢复暂停中的 Goal；从 `waiting_review` 继续时，须明确选择“人工继续一轮”，只授权一个新 prompt。恢复前重新读取最新会话状态；暂停恢复先评估，人工单轮继续不重放上一次结果未知的 prompt。
- **标记完成**允许用户在 `waiting_review` 中确认目标已经达成；记录用户来源与 revision，不伪装成 Jev 判定。
- **修改**只允许在 paused 状态；写入新 revision，保留旧目标和决策记录。revision 变更不清零整个 Goal 的硬迭代上限。
- **取消**关闭 Goal 自动推进并保留审计状态，不等价于停止当前 ACP turn。需要立即打断模型时仍使用独立的“停止”操作。
- 用户追加消息是人工介入，随目标 revision 的时间线记录；它不重置累计硬上限。队列和 steer 不作为 Goal 自动派发手段。

## 5. 状态机

| 状态 | 含义 | 可进入状态 |
| --- | --- | --- |
| `dispatching` | 已持久占用派发 ID，正在尝试向 ACP 发送一轮 | `active`、`paused`、`cancelled`、`outcome_unknown`、`blocked` |
| `active` | Goal 轮次正在运行 | `evaluating`、`waiting_input`、`paused`、`cancelled`、`blocked` |
| `evaluating` | 一轮已结束，正在请求 Jev 判断 | `dispatching`、`completed`、`waiting_review`、`waiting_input`、`paused`、`cancelled`、`blocked` |
| `waiting_input` | ACP 有未处理的 permission 或 input 请求 | `evaluating`、`paused`、`cancelled` |
| `paused` | 用户暂停，禁止自动评估和派发 | `evaluating`、`dispatching`（用户确认单轮）、`cancelled` |
| `waiting_review` | Jev 不确定/不可用、达到上限或需要用户决定 | `dispatching`（用户确认单轮）、`evaluating`、`paused`、`cancelled` |
| `blocked` | 配额、拒绝、运行时错误等阻止安全推进 | `dispatching`（问题排除且用户确认）、`evaluating`、`paused`、`cancelled` |
| `outcome_unknown` | prompt 可能已送达但网关无法确认 | 人工核实后恢复或取消 |
| `completed` / `cancelled` | Goal 已完成或被用户关闭 | 终态；新目标需新 ID |

状态迁移必须带 `revision` 和 `expectedState` 做 CAS 检查，避免旧请求覆盖并发暂停/取消。pause/cancel 在 dispatch 已 claim 时保存为待处理控制，不假定 ACP write 已撤回。每个已结算 turn 仅能评估一次；paused 期间只记录已结算 turn，不调用 Jev，Resume 后再评估。评估请求可在恢复后重做，但评估结论必须绑定 `goalId + revision + turnId`，迟到结果不得应用到新 revision。

Goal 当前有 ACP 请求待答时显示 `waiting_input`，不调用 Jev、不发下一轮。人工批准/回答后，待答请求消失且当前 turn 结束，再进入 `evaluating`。用户显式停止、Agent refusal、明确 quota stop 或非预期运行错误均不可当成普通未完成结果自动续发。

## 6. 架构与持久化

### 组件边界

- `internal/jev` 增加 Goal 专用评估模型和 schema 校验；复用现有 TypeSafe `/v1/systemone` HTTP client、代理、超时和凭据解析，不复用审批自动回应。
- `internal/acpruntime` 提供已结算 turn 的单次事件，包含会话、turn ID、stop reason 和时间线快照。Goal 控制器必须能区分人工 prompt、自动 Goal prompt、warmup 与账号切换恢复 prompt。
- 新增 ACP Goal service/store，负责状态迁移、操作幂等、Jev 调用协调、自动派发预算和恢复对账；HTTP handler 只做认证、参数校验和调用 service。
- 保持原生 `goalTransport` 语义不变。HTTP 层根据会话 owner 与能力选择原生 Codex/mcode provider，或 ACP GatewayGoal provider；不能把网关状态伪装成 Agent 原生 goal RPC。
- 显式区分 Goal 实现类型（例如 `none` / `native-mcode` / `gateway`）。当前 `acpGoalTransport` 对非 mcode 的 `goals` capability 也会创建 mcode RPC transport；新能力不能仅靠给其他 profile 增加 `goals` 字符串，否则会把请求发到错误的 Agent 方法。

### 持久数据

建议新增网关数据文件 `acp-goals.json`，按 `agent + threadId` 索引，包含 `schemaVersion`、`goalId`、`objective`、`revision`、`status`、`autoStepsUsed`、`autoStepsLimit`、`lastEvaluatedTurnId`、`lastDecision`、`activeDispatchId`、`createdAt`、`updatedAt` 与可展示的 `stopReason`。不保存完整 transcript 或 Jev API key。

Goal 快照使用专用 mutex、0600 临时文件和原子 rename；先成功落盘再替换内存状态。写入失败必须阻止相应的外部 ACP prompt。损坏文件不得静默覆盖：保留原文件、让 Goal store 标记不可用并返回可操作错误；普通聊天和网关其他功能继续运行。

每一轮自动 prompt 需有稳定且唯一的 dispatch ID（由 Goal ID、revision、step 派生或一次性生成后持久化）。派发前落 `dispatching` claim；确认 ACP write accepted 后落 `active`/accepted。超时、断连、进程退出、状态回读不匹配或 claim 后崩溃均落 `outcome_unknown`，绝不自动重发。若复用现有 `submissions.json`，需明确将系统 Goal prompt 作为独立来源纳入相同的去重与未知结果语义；不得用普通用户请求 ID 冒充。

### 重启恢复

启动时读取目标快照；文件缺失按空 store 处理。对活动 Goal 延迟到 ACP session 可解析后再核对：读取最新会话状态、待答请求和 turn ID，再对账最后一次 dispatch。`dispatching`/`outcome_unknown` 只允许通过会话事实确认已接受或标为等待人工核实，不能再次发 prompt。若最新 turn 已结束且尚未评估，可重新进行纯评估；按 `lastEvaluatedTurnId` 防止重复状态迁移。网关离线期间 Agent 状态无法确认时进入 `waiting_review`。

## 7. Jev 评估策略

### 输入与输出

新增 `GoalAssess` / `GoalAssessment`，复用现有 `Settings.post` 与配置，不改变 `ContinueAssess` 的中断恢复问题集。每次评估至少带：目标文本、Goal revision/step、Agent 类型、当前 turn 的用户请求、受限长度的最新 Agent 输出、stop reason、错误摘要和是否仍有 ACP 请求。传输使用 `/v1/systemone`。

评估 schema 至少要求：`complete`（是否完成原目标）、`continue`（continue / stop / review）、`confidence`（0–1）、`stop_kind`（finished / interrupted / quota / awaiting_user / refusal / other）。缺字段、值域错误、矛盾回答、超时、HTTP 错误或无 API key 一律是 `review`，不自动推进。v1 只允许在 `complete=false`、`continue=continue`、`stop_kind=finished` 且 confidence ≥ 0.80 时发自动续轮；中断、拒绝、额度不足、等待输入及其他原因都要求人工查看。

仅当 `complete=true` 且 `continue=stop` 时可标记 `completed`。其他 stop、review、低置信度和结论不一致均进入 `waiting_review` 或 `blocked`，由用户查看最后回复后决定继续、修改、暂停或取消。Jev 的“继续”建议不能触发权限卡回应，也不能覆盖用户 stop。

### 隐私与调用约束

使用和现有 Jev 一致的单次 HTTP 超时（当前 4 秒）和配置代理；一次 turn 最多评估一次，且不阻塞 ACP reader。发送目标和最近必要文本前做长度上限：目标不超过 4,000 字，assistant tail 建议不超过 6,000 字，错误摘要不超过 1,500 字。不得发送完整历史、附件内容、文件内容、环境变量、密钥或整段 tool output。

Goal Jev 日志只记录 `agent/goalId/revision/turnId`、结构化 verdict、confidence、耗时与错误类别，不记录目标全文或 assistant 输出。UI 明确表明 Goal 内容及最近输出摘要会发送到当前 Jev 配置的服务端；Jev 未配置时 Goal 可执行首轮，但首轮结束后进入 `waiting_review`，不自动续跑。

## 8. HTTP 与前端

### API 契约

复用现有 `/api/sessions/{id}/goal/*` 路由形状和前端操作，但由 provider 做能力路由。创建、修改、暂停、继续、取消均要求客户端操作 ID 与期望 revision；重复 ID 同内容返回先前结果，异内容复用 ID 返回 400。`status` 为只读，不得因轮询而 spawn Agent 或触发 Jev 评估。

创建响应至少返回 `goalId`、`provider`（native 或 gateway）、`status`、`revision`、`autoStepsUsed`、`autoStepsLimit` 和操作确认状态。结果未知时显式返回 `confirmed:false` / `status:unknown`，禁止 UI 自动重复提交。修改只在 paused 且 revision 匹配时成功；标记完成只在 `waiting_review` 且 revision 匹配时成功。

`workMode=goal` 的 ACP 创建路径先持久化 Goal claim，再走有稳定 submission ID 的 prompt 派发。已有活动 Goal 时拒绝重复创建；普通消息不得悄悄替换目标。pause/cancel 不隐式发送 ACP prompt；resume 必须先做状态对账。

### 前端与文档

在会话能力中同时表达“是否可用”和 provider 类型。前端只有在 backend 返回可用 provider 时显示 Goal 模式；不要再把 ACP profile 的任意 `goals` capability 直接解释成 native transport。状态栏应区分执行中、等待回答、等待复核、暂停、阻塞、结果未知和完成，并显示剩余自动轮数与停止原因。

停止按钮仍只控制当前 ACP turn；暂停/取消 Goal 只控制后续自动派发。遇到 `outcome_unknown` 时显示核实提示，不提供“立即重试”按钮。中英文 README 和 `ARCHITECTURE.md` 能力矩阵标记“网关托管 Goal”，明确它依赖 Jev 且不同于 native Goal；更新记录前完成代码与浏览器验证。

Goal 状态通过现有会话增量同步刷新。完成、等待人工复核和阻塞状态可走现有通知策略；仅当用户已启用对应通知时发送，禁止为 Goal 绕过通知退订。

## 9. 分阶段实施

### 阶段 0：契约冻结（无运行时代码）

确认本计划的状态机、ACP Agent 范围、Jev 阈值、隐私字段、最大轮数和 unknown 语义。用 fake ACP 写清首轮、续轮、权限等待、人工停止、Jev 失败和重启对账时序；更新架构图/能力矩阵草案。本阶段产物是可评审的数据结构、API 表和状态转换图。

### 阶段 1：持久化模型与纯状态机

新增 ACP Goal store、版本/CAS 转换、严格输入校验、原子写入、操作 ID 去重和状态恢复纯函数。先不开放 HTTP、不自动发 prompt。测试并发 pause/cancel、重复 operation ID、损坏文件保留、写入失败不产生外部副作用及版本冲突。

### 阶段 2：Jev Goal 评估与 turn-settle 接口

新增独立 `GoalAssess` 问题集和严格 schema decoder；将 settled turn ID 及受限输出交给 Goal service。为 session settle hook 添加明确的 prompt purpose / Goal identity，排除 warmup、普通用户 turn、账号切换恢复和非 Goal 自动继续事件。未有 Jev verdict 时只能停靠，不能借用普通 autoContinue 的中断回退。

### 阶段 3：有界自动派发与恢复对账

实现 `dispatching → active/outcome_unknown` 的持久协议、稳定 submission ID、每 Goal 单飞锁、迭代硬上限和重启恢复。每轮结束先处理 pending requests，再 claim turn evaluation；Jev 允许继续才创建下一步 prompt。用假 ACP 覆盖写前失败、写后断连、进程退出、late event、并发取消和重启窗口。

### 阶段 4：API、能力路由与 UI

加入网关 Goal provider 和 operation revision 校验；逐类接入不支持 native Goal 的 ACP profile。Codex/mcode 走旧 provider，其他 Agent 走 gateway provider。再显示 Goal 控件、状态、等待原因和预算；补充中英文文档与能力矩阵。

### 阶段 5：隔离验收与发布

在独立数据目录、独立端口和 fake ACP/本地 Jev stub 上端到端验证；不向真实工作会话注入 Goal。通过测试后启用能力标记并发布。运行网关的重启由用户决定，不执行 `make restart`。

## 10. 验证与验收

### 单元与并发测试

- Jev：complete / continue / review / quota / awaiting_user 组合；置信度 0.80 边界；缺字段、未知选择、越界值、冲突结论、无 key、HTTP 失败和超时均 fail closed。
- Store/state machine：所有合法与非法迁移；旧 revision CAS 失败；相同 operation ID 回放；异内容 ID 冲突；并发 pause/cancel/dispatch 只有一个胜者；读写失败不吞掉文件、不执行 prompt。
- ACP runtime：Goal 自动步骤只在目标 turn settle 后触发一次；普通消息、warmup、autoContinue 和 pool-switch resume 不误触发；存在 permission/input request 时禁止 Jev 评估和派发；用户 Stop 不被 Goal hook 撤销。
- 使用 channel/barrier 驱动竞态，不用固定 sleep。模拟文件 rename 故障、ACP write-before-send 与 write-after-send 断开、状态落盘失败及重复 settle 回调。

### 集成与端到端测试

- Fake ACP 首轮输出未完成，Jev 返回安全 continue，网关派发带确定 ID 的第二轮，Jev 返回 complete 后 Goal 收口；普通 `autoContinue` 保持原行为。
- ACP 请求权限/提问后 Goal 转 `waiting_input`；用户回应并由 Agent 完成后才重新评估。quota、refusal、用户停止、Agent 失败、低置信度和 Jev 不可用均不自动派发。
- 在各 crash window 重启：claim 前、claim 后未写 ACP、ACP 已写但未记 accepted、turn 已结算未评估、评估完成未派发、派发确认未写 Goal 快照。验证任何 outcome_unknown 都不重放。
- API 集成覆盖 CRUD、revision 冲突、owner 路由、read-only 状态读取不 spawn、重复请求、旧客户端和 native provider 回归。
- 前端覆盖 capability provider 分流、按钮显隐、状态渲染、pause/resume/edit/cancel、未知结果提示、迟到响应不覆盖新 revision 和中英文文案。

### 检查命令与通过条件

先运行聚焦检查：`go test ./internal/jev ./internal/acpruntime ./internal/httpd ./cmd/gateway`、相应包的 `go test -race`，以及 `node --test tests/*.js`。随后执行 `go test ./...`、`go test -race ./internal/acpruntime ./internal/httpd ./cmd/gateway`、`go vet ./...`、`git diff --check`。

通过条件：重复启动/请求不会重复发 prompt；未知结果停在可核实状态；任何人工停止、待答请求、quota、Jev 失败/不确定都不会自动越过；goal 完成后不再派发；普通会话、mcode 原生 Goal、Codex 原生 Goal 和普通 autoContinue 的既有测试均保持通过。浏览器隔离验收至少覆盖 390×844 与桌面视口。

## 11. 兼容、发布与回滚

### 数据与 API 兼容

- 新目录缺少 `acp-goals.json` 时按空 store 启动；不迁移 Codex 原生 Goal 或 `goal-commands.json` 数据。
- 现有 `/goal/*` 响应字段保持兼容；新增 `provider`、`revision`、`autoStepsUsed`、`autoStepsLimit` 是可选附加字段。旧客户端忽略新字段仍可读写 native Goal。
- GatewayGoal 的会话数据使用独立 provider 标记，不改变原生 mcode 方法、不向其他 ACP Agent 发送 mcode RPC，也不改变其 `capabilities` 中 Plan/steer/skills 语义。
- 文件缺失、老版本未知字段和 schema version 必须有明确行为。低版本不识别新账本时不得清理或重写该文件；回滚后 Agent 聊天记录仍由 ACP 自己保存。

### 发布与监控

先在隔离网关和 fake ACP 上完成完整验收；再选一个非生产测试 ACP 会话验证真实 Agent 的连续 prompt、审批等待、关闭浏览器后的后台行为及 Jev 端点。确认无无限续跑后再将支持列表扩展到其他 Agent。

日志只输出状态迁移、goalId、revision、turnId、step、Jev verdict/置信度、耗时和错误类别。监控 unknown、blocked、waiting_review 比例和每 Goal prompt 次数；不记录目标正文、聊天输出、密钥或完整文件路径。

### 回滚

提供全局/按 Agent 的关闭开关。关闭后拒绝创建新 GatewayGoal，活动 Goal 停在当前状态，不再派发；不自动发取消、不删除状态文件。先通过 UI/API 暂停活动 Goal，再关闭能力。若进程或 Agent 当前 prompt 的送达结果未知，标为 `outcome_unknown` 并由用户核实。

回滚到旧版时保留 `acp-goals.json`；旧版继续提供原生 Codex/mcode Goal 和普通聊天。重新升级时先对账保存状态再允许 Resume。只有确认文件可由新旧版本安全读取后，才考虑单独制定账本清理流程；本计划不包含自动删除或降级改写。

## 12. 决策记录

以下是进入阶段 0 前的建议默认值；实现开始前应以代码评审确认。更改这些值须同步更新状态机、API 与测试。

| 项目 | 建议决定 | 原因 |
| --- | --- | --- |
| 支持范围 | 默认给没有 native Goal 的六个内置 ACP Agent 提供 gateway provider；mcode/Codex 保持 native | 避免复制已有 RPC 语义 |
| provider 路由 | 增加显式 Goal provider 类型；`capabilities.goals` 不单独决定 transport | 当前 `goals` 会被当作 mcode RPC 能力 |
| Jev 无法使用 | 首轮仍可运行，之后进入 `waiting_review`；不自动继续 | 避免把缺少评估误当作“未完成且可安全继续” |
| 自动继续门槛 | `complete=false`、continue 明确为 continue、stop_kind 可继续、confidence ≥ 0.80 | 使用现有 Continue 置信度门槛，缺失或冲突均停止 |
| 自动派发上限 | 每个 Goal 最多 10 次自动续轮；到顶后 `waiting_review` | 独立于 `MaxAutoContinue`，给长任务足够空间并限制失控循环 |
| 人工越过复核 | 用户确认一次“继续当前目标”只授权一个新 prompt；之后重新评估，不清零累计计数 | 人工决策可推进任务但不重置自动预算 |
| 用户停止 | 永不由 Jev 自动继续恢复；继续 Goal 必须是新的用户操作 | 用户停止是优先级最高的控制信号 |
| 用量限制 | v1 使用续轮数和现有每轮超时；无可靠 ACP 用量源时不显示 Token 预算 | 避免把估算值宣传成硬预算 |
| 后台运行 | Goal 显式启动后可在浏览器关闭时继续；网关/ACP 进程退出时按恢复规则停靠 | 区分显式 Goal 与只服务于前台的普通 autoContinue |

评审通过前，这些默认值仍是方案，不应被实现或文档描述为当前运行行为。
