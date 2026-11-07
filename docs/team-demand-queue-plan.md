# 团队多需求编排与持久队列实施方案

> 状态：阶段 0–4 已实施（2026-11-07），阶段 5 未实施 · 原方案定稿 2026-11-07
>
> 基于 [团队总体设计](team-overall-design.md)、[持续反馈方案](team-autonomous-feedback-plan.md) 和现行 Go 网关。本文只定义增量实施与验收，不把目标设计描述为已实现功能。
>
> **实施记录（2026-11-07，提交见 git log）**
>
> - 阶段 0 ✅：autoPlan 动作的同批混排与未完成执行屏障（actions.go `validateActionReviewOrder`/`ValidateExecutableWithDeps`）；unknown 裁决一次有界格式修复（review.go `revfix-*`，与 completed 事实同批原子落账，journal 重建恢复）；汇总任务派发注入结构化交付事实（`BuildDeliveryBriefing`——unknown 只能表述为「未取得裁决」，无审查必须写明未安排）。
> - 阶段 1 ✅：输入账本 `internal/team/inputqueue.go`（team-inputs.jsonl 事实 + team-inputs.json 快照，损坏隔离重建）；`POST/GET /api/teams/:id/inputs`（queueVersion:2 分流）、`PATCH`（仅 queued、操作 UUID 幂等、版本冲突 409）、`prioritize`/`cancel`；容量背压 `maxPendingInputs`（默认 64，未路由积压口径，429）；会话发送 `teamQueueVersion:2` 分流；重启重放与操作幂等跨重启成立。
> - 阶段 2 ✅（机械路由部分）：同批 `key`/`dependsOn` 动作扩展（批级校验：key 唯一/不与既有任务同名/引用可解析/无环；拓扑序执行；审查可用显式依赖覆盖执行任务，未覆盖保持阶段 0 保守拒绝）；输入路由器 `internal/team/inputroute.go`（intent→幂等创建→routed 确认，new_task/follow_up/amend，多活跃 Run 停靠 waiting_input 并记录原因，崩溃重泵对账）；WorkItem 索引随 routed 落账。自动路由由 `TeamQueueAutoRoute` 灰度开关控制（main.go 已开启，测试环境关闭以获得确定排队态）。
> - 阶段 2 未尽事项：Lead 语义路由提案（answer/clarify 分类、多要求拆分的来源引用核对、超 Run 上限拆批）依赖 Lead 提案协议，属阶段 2 后续增量；当前对范围不明的输入停靠 waiting_input，不用猜测代替用户决定。
> - 阶段 3 ✅：跨 Run 成员 lease（`teamMemberPending` 窗口记账 + Team 级 memberBusy，同一成员跨 Run 最多一个在途任务）；活跃 Run 准入 `maxActiveRuns`（默认 2，活跃=有在途任务或派发窗口的非终态 Run，`teamRunPending` 窗口口径，轮转释放不抢占）；路由公平（`RouteOrderAt`：3:1 优先配额 + 10 分钟老化提升，排序只依赖账本状态、重启同序）；Team 队列滚动派发预算 `queueDispatchBudget`（默认 48 次/24 小时，journal 的 task_dispatching 计量，超出停靠并提示重置时间；计量失败退化为不限）。
> - 阶段 4 ✅（前端）：TeamChatController 渲染输入队列面板（`web/team-chat.js`）——每 Team 的 启用/停用排队 开关（localStorage 灰度，默认走旧路径）；面板列出逐输入的 状态/等待原因/目标 Run 跳转/工作项计数/修订次数，queued 行提供 立即发送/编辑/取消（操作 UUID + 期望版本），routing/waiting 行禁用并说明原因；发送侧 `teamSubmissionFields` 按开关附带 `teamQueueVersion:2`，202 queued 响应不依赖 runId 即刷新（`web/app.js`）；i18n 词条与手机窄屏样式已补。旧客户端（不带开关）契约不变。
> - 验证：`go test ./...`、`go test -race ./internal/team ./internal/httpd`、`go vet ./...`、`node --test tests/*.js`（164 例）全绿；`make conformance` 的 golden 失败为存量问题（干净 HEAD 同样失败，与本次无关）。§9.1 要求的隔离 Team 真实浏览器验证与 VERIFICATION.md 记录尚未执行（网关已由用户重启加载新代码；建议在测试团队开启「启用排队」试用并记录 Run/输入 ID）。

## 1. 问题与边界

现行 `POST /api/teams/:id/inputs` 与 Lead 会话发送共用 `createTeamInput`：首条输入创建 `single + autoPlan` Run；活动 Run 中的后续输入追加 Lead Task；终态后创建带 `parentRunId` 的新 Run。输入没有独立持久化队列或路由状态。多个活动 Run 时，未指定目标的团队输入返回 409。

Run 内已有 `DependsOn`、成员互斥、共享工作区写锁、Team 级 `maxParallelTasks`、provider/model 级 AIMD 与失败恢复。但这些机制只调度已进入 Run 的 Task，不负责大量输入的持久积压、跨 Run 公平性或修订路由。`FollowUpConversationRun` 追加的 Lead Task 没有指向既有任务的依赖，也不撤销已派发任务。

2026-11-07 的实际团队 Run `cebb1037-f3c0-4e02-8407-bb1e2f7c7ae6` 暴露了两个交付风险：实现与审查都只依赖根 Lead，审查先于实现完成启动；Reviewer 未输出 `<team-review>` 裁决，交付投影为 `unknown`，而 Lead 摘要使用了“独立审查未发现问题”。本方案将这两类问题作为进入多需求扩展前的前置修复。

范围：团队聊天输入、TeamInput 账本、自动规划动作、Run/Task 调度、审查与验证投影、团队聊天队列 UI。继续复用现有 Provider 会话、submission 队列和 Run journal；不另建 Agent runtime。

暂不做自动创建成员、默认自动 worktree、自动合并/推送、任意 shell 验证命令、模型 token 精确计费或复杂优先级学习。独立写任务在共享工作区仍串行；worktree 能力仅作为后续明确启用的隔离选项。

## 2. 目标行为与不变量

用户可以一次提交多个要求，也可以在运行中持续补充。每条原始输入先持久化并立即返回受理结果；主控可把它拆为多个可追踪工作项，相关工作项放入一个 DAG，独立工作项进入不同 Run。用户始终能看到每条要求被分到哪里、当前在排队还是执行、为什么等待以及最终证据。

1. **先记账后规划**：输入正文、附件引用和幂等键先持久化；模型调用失败或网关重启不能丢输入。同 ID 同内容重放返回原状态，异内容返回冲突。
2. **计划是意图，网关是裁决者**：Lead 只能提交结构化工作项、路由和依赖建议；网关校验成员、容量、循环、工作区、策略版本与权限后持久化幂等意图，再对账生成 Run/Task。自然语言不能触发隐式派单。
3. **修订不改历史**：已完成 Task/Run、已发送 Prompt、文件副作用和证据不被覆盖。未开始的受影响任务可确定性标记为 superseded；在途写任务不能被隐式取消，冲突时暂停并请求用户决策。
4. **执行与审查有因果关系**：审查必须依赖被审实现的完成事实，并绑定实际工作区版本；审查后的写入使旧结论失效。`unknown` 不得在交付摘要里表述为通过。
5. **容量是硬边界**：排队和优先级只决定准入顺序，不绕过 Team、成员、provider/model、预算和工作区写锁。已受理但尚无容量的输入保持可见、可恢复的排队状态。
6. **无静默丢弃**：超过单 Run 任务/轮次或输入积压上限时，网关分批编排或明确拒绝并说明原因；不能截断需求、只保留最后一条或让 Run 提前完成。

## 3. 持久模型与 API

### 3.1 输入与工作项

新增 TeamInput 事实流和可重建投影。输入的每个正文版本不可变；排队中的“编辑”新增版本并将旧版本标记为被替换，不原地改写已接受事实。最小字段如下：

```text
TeamInput: id, teamId, revision, text, attachmentIds, contentHash,
           acceptedAt, queueStatus, routeStatus, targetRunId?, targetTaskId?,
           priority, policyDigest, version, runIds[], workItemIds[]
WorkItem:  id, sourceInputId, sourceRevision, requirementText, runId?,
           taskIds[], dependsOnItemIds[], status, evidenceRef?
```

`queueStatus` 只表示入口生命周期：`queued → routing → routed`，以及 `waiting_input / failed / cancelled`；Task/Run 的执行状态仍由原账本负责。一个输入拆出多个 WorkItem 时，输入的“完成”由全部工作项的交付投影派生，不能用 `routed` 冒充完成。WorkItem 是需求到 Run/Task 的索引，不持有第二份执行状态。

事件至少包括 `input_accepted`、`input_revised`、`input_prioritized`、`input_route_intent`、`input_routed`、`input_waiting_input`、`input_cancelled` 和 `work_item_linked`。新增输入 journal 与可重建 JSON 投影，Run 仍用原 journal；两份账本没有跨文件原子事务。先持久化带规范化计划、确定性 Run/Task ID 和幂等键的 `input_route_intent`，再幂等创建 Run/Task，最后写 `input_routed`。崩溃后按 intent 对账补齐；未全部落成前一直显示 routing，不得永久显示已路由却没有任务。长输入分成有界路由批次，每批独立 intent 和游标，不能靠一条无限大的 journal 事件承载所有需求。

### 3.2 接口契约

```http
POST   /api/teams/:teamId/inputs                 # 202，先持久化再返回
GET    /api/teams/:teamId/inputs?cursor=...      # 分页查看积压和历史
GET    /api/teams/:teamId/inputs/:inputId         # 路由与工作项投影
PATCH  /api/teams/:teamId/inputs/:inputId         # 仅 queued 可编辑，带 version
POST   /api/teams/:teamId/inputs/:inputId/prioritize # “立即发送”只提前路由
POST   /api/teams/:teamId/inputs/:inputId/cancel  # 仅未派发部分可取消
```

创建请求使用客户端 UUID 作为幂等键；同 ID 同正文和附件重放返回同一输入，异内容返回 409。编辑、优先和取消携带操作 UUID 与期望版本；重复操作返回既有结果，版本冲突返回 409。排队达到 `maxPendingInputs` 时返回 429 和当前容量说明，不自动丢弃旧输入。初始建议默认 64 条待路由输入/Team，作为独立于 `maxTasksPerRun` 的可调上限；容量与速率应依据真实负载再调。

现有 `POST /api/sessions/:leadSessionId/send` 不能突然从“立即给 runId”变成“先排队、runId 暂缺”。`POST /inputs` 请求体带 `queueVersion:2`、会话发送请求体带 `teamQueueVersion:2` 时才走持久队列，返回 `202 {inputId, queueStatus:"queued", runIds:[]}`；旧请求继续返回原有 `routeStatus/runId` 契约。新页面用 inputId 追踪路由，完成切换且旧客户端兼容窗口结束后，再考虑收敛双入口。`POST /runs` 和显式 amend API 保留。

## 4. 多需求路由与 DAG

每 Team 同时只路由一批输入，按持久序号处理；执行中的不同 Run 可以并行。仅合并**尚未开始路由**且用户明确属于同一目标的输入，原始输入 ID、正文、附件和各自的 WorkItem 仍逐条保留。Lead 返回结构化路由提案，网关先完整校验，再创建 Run/Task；模型输出不能直接改 Team 或工作区。

路由规则按以下顺序执行：显式 `targetTaskId` / `targetRunId` → 与唯一兼容的活跃 Run 续作 → 创建独立 Run → 目标或范围不明时 `waiting_input`。多个活跃 Run 不应使已受理输入消失，也不应猜最近一个；保留输入并要求选择。纯问答可产生答复而不创建 Run，但必须有输入记录和终局投影。

单条输入的每个可检查要求分配稳定 WorkItem ID，保留原句、来源片段和约束；不能让 Lead 把“Web 和桌面”缩成“仅 Web”。Lead 可建议关联、依赖和成员；网关可机械校验来源引用完整、字段合法和有无未映射片段，但无法单凭程序证明语义覆盖。多要求计划在派发前展示映射供用户核对；无歧义且策略允许的计划可自动派发，范围歧义会影响副作用时先澄清，不用猜测代替用户决定。

同一目标的工作项组成一个 Run DAG；彼此独立的目标可以形成不同 Run。`parentRunId` 只连接同一工作流的修订，不把所有新需求都挂到最近 Run。`maxTasksPerRun` 计算 Lead、实现、审查、修复、汇总等全部节点；规划前预留收尾容量。装不下时按依赖边界拆成后续 Run，记录 WorkItem 到 Run 的映射，不静默删项。一个输入可关联多个 Run，一个 Run 可消费多个输入，但同一 WorkItem 只能有一个当前执行归属。单消息仍受现有 10000 字上限；超出时明确提示拆成多条输入并保持顺序。

现有 `delegate.fromTaskId` 要求引用**已完成**任务，不能在同一个动作块表达“先实现、后审查”。新增同批次局部 `key` 与 `dependsOn`，网关解析全部提案后将局部 key 映射到确定性 Task ID，用现有 `ParsePlan`/DAG 校验循环、成员和容量。旧 `fromTaskId` 保留“引用既有结果”的语义，不改写旧动作：

```json
{
  "tasks": [
    {"key": "web", "member": "member-kimi", "task": "修改 Web 删除逻辑", "writeLock": "exclusive"},
    {"key": "desktop", "member": "member-kimi", "task": "修改桌面删除逻辑", "writeLock": "exclusive"},
    {"key": "review", "member": "member-zcode", "task": "审查两端最终改动", "review": true,
     "writeLock": "none", "dependsOn": ["web", "desktop"]}
  ]
}
```

该示例只在用户确认两端均属范围时成立。共享目录的两个写任务会被写锁串行；审查只有两项实现均完成才就绪。规划、审查和最终汇总分别是不同 Task，不能用 `round=2` 或自然语言“之后再审查”替代 DAG 依赖。

运行中的补充意见先按影响范围分类：无关新增要求可以进入独立 Run；影响未派发节点时把旧节点标记 superseded 并建新节点；影响已完成结果时创建有血缘的 amend Run；与在途写任务冲突时等待任务结束或请求用户选择暂停/取消。所有新节点指向输入修订与被替代节点，旧证据仍可查看。一个 Run 的终局必须等待其相关输入全部路由、任务终态、审查与验证完成；新输入与终局竞争时按 journal 顺序裁决。

## 5. 排队与并发调度

排队分两层，不能混成一个状态：**输入队列**保存尚未路由的用户要求；**执行队列**是各 Run 中 `planned/ready` 的 Task。前者保证接收与排序，后者按依赖和资源准入。现有 ACP submission 队列仍只处理某个 Provider 会话的发送，不负责团队级公平性。

输入队列每 Team 按接受序号 FIFO；“立即发送”将**尚未路由**的输入放入优先队列队首，已在 routing/执行中的任务不抢占。每处理 3 条优先输入至少处理 1 条普通输入；普通输入等待超过 10 分钟即提升到下一次路由，以防持续插队导致饥饿。所有排序决定落事件，重启后顺序相同。一次只允许一个 Team 路由事务；模型规划可以异步，但提交前必须重新检查输入和 Team 版本。

执行队列在不同活跃 Run 之间轮转，每个 Run 内继续按 DAG 就绪与 Ordinal 取任务。第一阶段建议每 Team 最多 2 个活跃 Run，其他已路由 Run 保持 queued；这只是 Run 准入上限，实际 Task 在途总数仍受现有 `maxParallelTasks` 约束。`waiting_input/paused` 且无在途任务的 Run 不占活动槽，恢复时重新排队争取槽位。轮转只跳过当前没有就绪任务的 Run，避免一个等待 Reviewer/锁的 Run 阻塞独立 Run。Run 优先级继承其最高未完成输入；不得抢占已派发任务。

一次派发同时满足以下全部条件：

| 资源层 | 沿用或补强的约束 |
| --- | --- |
| DAG | 所有 `dependsOn` 已满足；失败依赖走现有级联终止/分诊 |
| 成员会话 | 新增 Team 范围的成员 lease，跨 Run 同一成员最多一个活动 Task；Provider 会话原有队列继续兜底 |
| Team | 所有 Run 的在途任务与派发预留合计不超过 `maxParallelTasks` |
| Provider/model | 沿用全局 AIMD 准入、429 冷却和逐步恢复；冷却任务保留 planned |
| 工作区 | 共享 cwd 的 `exclusive` 写任务串行；`none` 只读任务可并行，但实施审查仍须等待依赖 |
| 预算 | 沿用每 Run 按派发次数计的 `budgetUnits`；另设 Team 队列滚动 24 小时派发总额，避免拆成多个 Run 绕过成本上限；不声称 token 有硬预算 |

Team 队列预算建议默认 48 次派发/滚动 24 小时，可由用户调整；这是任务派发次数而非 token 额度。已有 `budgetUnits=0` 的旧路径仍按原语义运行，新入口按队列预算停靠并提示重置时间或调整额度。所有 lease/预留必须在提交派发事实、确定性失败、取消和恢复路径对称释放；崩溃后从 Run/journal 重建，不能依赖仅在内存中的占位。跨 Team 对同一项目 cwd 的写锁继续共用。路由/规划的模型调用也计入可见的尝试次数；达到每输入规划上限时转 `waiting_input`，不无限自我修复。

当队列满、Run 达任务上限、成员不可用、额度不足、预算耗尽或项目锁长期占用时，UI 和 API 应分别给出等待原因与下一步；没有可证明的写锁 cwd 时保持排队，不无锁派发。独立写任务若以后启用自动 worktree，需另做基线、未提交改动、diff 与集成验证，不能仅因 `writePaths` 不重叠就并行写主工作区。

## 6. 审查、验证与交付

Review Task 必须绑定 `reviewOf[]` 被审 Task ID、`workspaceId`、基线与审查时的 `headRevision/diffHash`。它在全部被审实现完成、产物版本可定位后才可派发；审查本身只读。实现后再次写入使 verdict 变 `stale`，不能把旧 `passed` 用作新版本的交付证据。若工作区无法确定改动版本，审查转 `waiting_input`，不能凭当前 HEAD 猜待审提交。

Reviewer 仍使用现有 `<team-review>passed/failed</team-review>` 末行协议。缺失或格式错误为 `unknown`：先给 Reviewer 一次有界的格式修复机会；仍未知时停止该分支的“已审查”表述并展示待重审。失败可按固定上限返回实现成员修复，重新生成 diff 后重审；修复上限到达转 `waiting_input`，不循环派单。

Verify 继续使用既有固定 argv 白名单，记录被验证 Task 的实际工作区与同一 diffHash。验证失败与审查失败分开展示，不用 Agent 的口头“测试通过”替代网关验证。没有配置网关验证时显示“未配置”；成员报告的测试命令可作为来源证据，但不提升 Verify 状态。

WorkItem 明确 `reviewPolicy=off/best_effort/required`，默认 `best_effort` 保持旧团队的可完成性；用户或计划指定 `required` 时必须有独立 Reviewer，否则先请求调整成员或策略。Run 成功终局要求：相关输入都已路由、依赖内任务均终态、`required` 分支有当前版本的明确通过 verdict、配置的 Verify 已结算、没有待答问题或未处理的 action。`best_effort` 缺评审或裁决未知时允许结束，但交付必须明确标为“未审查/结论未知”，不能显示“通过”。自动汇总从结构化 delivery 证据生成结论字段；Lead 只撰写说明，不能覆盖网关的 `unknown/stale/failed`。

## 7. 前端交互

继续以 Team Chat 为主入口，输入框可一次接收多个要求和附件。发送后立即在时间线显示“已受理/排队中”，随后展示路由结果；不要等 Lead 规划完成才让用户确认消息已保存。顶部提供队列计数和当前活动 Run 数；展开后按输入顺序列出摘要、优先级、等待原因、目标 Run/Task、受理时间和最近变化。

排队中的输入提供与现有会话队列一致的 **立即发送、编辑、删除** 操作。立即发送只提升路由顺序；编辑产生新 revision 并显示旧版本已替换；删除是取消未路由输入，保留审计记录。输入一旦开始 routing，这三个操作禁用并说明原因；运行中的修订通过新的 TeamInput 提交，不能伪装成原消息编辑。沿用现有“每聊天启用/停用排队”的用户心智，但 TeamInput 积压与 ACP 会话发送队列在 UI 上分别命名。

一个输入拆成多个 WorkItem 时，先显示原始要求，再以简洁列表显示每项归属及状态；一个 WorkItem 关联多个 Task 时可展开任务时间线。多个 Run 以工作流分组而不是在聊天里复制消息；“第 N 次修订”展示 parentRunId 血缘。任务等待时区分依赖、成员忙、模型冷却、写锁、预算和待用户输入；不把不同原因一律显示为“处理中”。

交付区按工作项列“实现、独立审查、网关验证”三种证据，并显示对应工作区版本。`unknown/stale/未配置` 必须有独立文案；Lead 的自然语言总结不能覆盖机器状态。失败或范围歧义提供澄清、重新排队、取消未派发部分的入口。手机窄屏优先显示当前动作和等待原因，完整 DAG/事件仍可进入 Run 详情页查看。

## 8. 分阶段实施

### 阶段 0：修复现有交付顺序与结论

- 自动规划动作校验暂时拒绝“同一批次同时创建普通执行与 Review”，且存在未完成执行任务时不接受新的 Review 动作；要求 Lead 在实现完成后再委派审查。显式 Plan 继续使用现有 DAG 依赖，不受此限制。`reviewOf/dependsOn` 上线前采用此保守规则，防止审查抢跑。
- 最终交付摘要以 delivery 的结构化 verdict 为准；`unknown` 明示未取得裁决。审查格式修复只重试一次，次数与结果入账。
- 增加真实案例回归：实现 17:19 才完成时，Review 绝不在 17:15 派发；无末行标记不能称已通过。

### 阶段 1：持久输入队列

- 实现输入事件、快照、重放、队列容量、创建/查询/编辑/优先/取消 API；先保留旧发送路径。已接受输入在规划前重启后仍可查询和继续。
- 新增 `teamQueueVersion:2` 兼容分流与输入状态投影，不改变旧客户端对 `runId` 的同步预期。
- 故障注入覆盖接受后、修订后、路由启动前的重启；同 ID 重放与版本冲突均可审计。

### 阶段 2：多工作项路由与依赖

- Lead 输出带来源文本引用的工作项与路由提案；引入同批次 `key/dependsOn`，复用 `ParsePlan` 的 DAG 校验和既有 Task 模型。实现需求到 Run/Task 的持久索引。
- 先支持 `answer / clarify / new_task / follow_up / amend`；不自动 steer 运行中的 Provider turn。对范围不明或与在途写入冲突的输入停靠等待。
- 验收：一条消息含两个独立需求和一个依赖审查时，全部有工作项；审查不会早于实现；超 Run 上限拆批且无遗漏。

### 阶段 3：跨 Run 调度与公平性

- 在 Team 级执行循环中轮转活跃 Run，增加跨 Run 成员 lease 与活动 Run 准入上限，继续复用写锁、派发预留、AIMD 和预算闸门。
- 验收：大量积压输入逐步入 Run；较早普通输入不会被连续“立即发送”饿死；同一成员、同一共享 cwd 不出现双写；重启后无重复派发。

### 阶段 4：团队聊天队列与工作项视图

- 接入新 API，展示队列操作、路由、工作项、等待原因和多 Run 血缘；旧客户端按原协议工作。先以隔离测试 Team 验证桌面与手机视口，再切换默认入口。
- 验收：用户能从一条输入追到每项任务及交付；编辑/删除只作用于排队输入；“立即发送”不能绕过资源约束。

### 阶段 5：版本绑定的审查与验证

- 用 `reviewOf + workspaceId + headRevision/diffHash` 替换阶段 0 的全 Run 保守屏障；实现审查过期与有限修复回路。Verify 绑定同一工作区版本。
- 验收：审查后发生写入时旧 verdict 自动变 stale；失败/unknown 不被自动汇总成成功；独立 worktree 的扩展另行立项，不作为本阶段上线前提。

## 9. 验证、兼容与回滚

### 9.1 测试矩阵

| 场景 | 必须验证的结果 |
| --- | --- |
| 输入幂等和编辑 | 同 ID 同内容只落一条；异内容冲突；排队编辑保留 revision；routing 后拒绝原地编辑 |
| 多要求 | 一条输入拆出全部 WorkItem；同批依赖可前向引用；循环、漏映射和越界被拒绝或待澄清 |
| 多 Run 并发 | 不相关 Run 可推进；同成员跨 Run 单活动；Team/模型上限与共享 cwd 写锁不超发 |
| 公平与背压 | 64 条积压边界明确；优先输入不饿死旧输入；Run 满额后分批而非截断 |
| 审查/验证 | Review 晚于实现；缺裁决为 unknown；写入使旧 verdict stale；Verify 绑定相同 diff |
| 故障恢复 | 在接受、路由 intent、Run/Task 落账、路由确认、派发预留各点崩溃后重启，无丢输入、重复任务或重复发送 |
| 前端 | 桌面与手机可查看排队原因、工作项归属和交付证据；编辑/删除/立即发送的禁用边界正确 |

每阶段先运行相关 `go test ./internal/team ./internal/httpd`、竞态用例与 `node --test tests/*.js`；完成跨模块阶段时运行 `go test ./...`、`go test -race ./internal/...`、`go vet ./...` 和 `make conformance`。前端阶段在隔离测试 Team 用真实浏览器验证，记录 Run ID、输入 ID、重启点与交付状态到 `VERIFICATION.md`。测试中用可控时钟/故障注入，避免用固定 sleep 证明队列或恢复正确性。

### 9.2 兼容与上线

新增字段和事件只增不删；旧 Run、旧 journal 与既有 session 队列照常读取。新输入队列起初由显式 `teamQueueVersion:2` 触发，新页面先在隔离 Team 灰度；旧客户端仍走同步创建 Run 的路径。迁移后历史输入可通过旧 Run 投影只读展示，无需批量改写历史记录。

上线顺序为：阶段 0 修复 → 新账本只读/重放（不对真实用户开放）→ 路由与调度在隔离 Team 完整验证 → 新入口灰度 → 前端默认切换。逐阶段观察队列深度、最长等待、路由失败/重试、活动 Run 数、资源等待原因、`unknown/stale` 审查占比与重复派发计数。任何阶段未通过验收时不开放下一阶段。不得自行执行 `make restart`；需要加载新网关或静态资源时由用户决定并重启。

### 9.3 回滚

关闭新输入准入只影响**新消息**，不能遗弃已接受的排队输入。回退旧客户端 UI 时，新版网关继续处理或明确暂停并显示现存队列；只有队列已清空、全部路由 intent 已对账，才可回退到不认识新账本的旧网关。保留输入 journal 与 Run journal 原件，不删除、重写或用旧快照覆盖。若灰度中发现路由错误，停止新派发、保留已发送任务和文件副作用，按输入/Run 血缘修订，不自动 reset、rebase 或清理工作区。
