# 团队自治与持续反馈编排方案

> 状态：设计中 · 起草日期：2026-10-28
>
> 前置方案：`docs/team-orchestration-plan.md`。本文只扩展持续用户反馈、动态成员、自动 worktree、自动审查与验证；沿用既有 journal、真实会话、权限审批和结果未知语义。

## 1. 目标与核心判断

用户的主要交互不是手工建 Run、拆任务和挑选成员，而是持续对团队说“这里再改一下”“顺便看看另一个问题”。系统需要把这些意见转成可追溯的续作、新任务或修订，并自动完成成员选择、冲突隔离、审查和验证。

核心判断：

1. **团队是长期入口，终态 Run 不可改写。** 活跃 Run 可以接收反馈并派生任务；终态 Run 不重开，后续意见创建带血缘的新 Run。
2. **AI 控制团队不等于 Agent 直接控制系统。** Lead 负责理解和规划，网关负责校验、授权、调度、供给和记账。
3. **复用优先于扩招。** 只有现有成员缺少所需能力或工作区不匹配时才创建新会话，禁止把“新建成员”当作默认并行手段。
4. **隔离优先于隐式串行。** 并行写同一仓库时自动使用 worktree；无法判断写入范围时采取保守策略。
5. **完成必须有证据。** Agent 的口头成功、Reviewer 的批准和测试通过是三类不同事实，不能互相替代。
6. **自动交付不等于自动合并。** 第一阶段自动产出 diff、审查和验证结果，应用改动、合并和清理仍由用户决定。

目标闭环：

```text
用户持续提出意见
  → 网关持久化输入
  → Lead 提交结构化路由/计划意图
  → 网关选择成员或供给新成员
  → 网关按写入冲突选择共享工作区或 worktree
  → 成员执行 → 固定命令验证 → 独立 Reviewer 审查
  → 网关交付有证据的结果，并继续接收下一条意见
```

## 2. 控制面：AI 提意图，网关做决定

保留现有 `<team-actions>` JSON 信封，但把动作解释为**意图**而非已授权命令。Lead 不直接调用成员管理、worktree、验证或 HTTP API，也不能读写 Team journal。

```text
Lead / Reviewer / 成员
  → 结构化意图（delegate、ensure_member 等）
  → Schema 校验 + Team Policy 授权 + Run 版本检查
  → 网关生成确定性 Task / Provision / Workspace / Verification
  → journal 先记账，再执行外部副作用
```

Dynamic Workflow 的可重放 DAG、结构化结果和自适应并发继续保留；扩展点是“持续输入”和“网关资源供给”，不是引入自由群聊或另一套 Agent runtime。

### 2.1 权限边界

| 行为 | Agent | 网关 | 用户 |
| --- | --- | --- | --- |
| 解释意见、拆分任务、选择建议成员 | 提议 | 校验并决定 | 可修改 |
| 创建普通任务、请求审查、运行已登记验证 | 提议 | 按策略自动执行 | 查看/取消 |
| 创建新会话、创建 worktree | 提议并说明原因 | 按额度和白名单执行 | 可停用自治 |
| 提高预算、增加轮次、修改团队策略 | 不得执行 | 拒绝并暂停 | 明确确认 |
| 回复权限请求、应用 diff、merge/rebase/cherry-pick | 不得代替用户 | 不自动执行 | 明确操作 |
| 删除会话、分支或 worktree | 不得执行 | 仅提供显式入口 | 明确操作 |

Agent 不能通过 Prompt 注入扩大自己的权限。所有自动动作必须能追溯到用户输入、Lead 意图、策略版本和幂等键。

### 2.2 团队自治策略

在现有 `TeamPolicy` 上增加：

```text
autoRouteFeedback       是否自动路由活跃 Run 的用户意见
autoProvisionMembers    是否允许创建新 Agent 会话
autoWorktree            是否允许按冲突自动创建 worktree
autoReview              写任务完成后是否自动派生独立审查
autoVerify              是否自动派生分支级验证；既有显式 verifyCommand 不受影响
allowedAgentKinds       允许自动供给的 Agent 类型
maxAutoMembersPerRun    单个 Run 最多自动新增成员数，默认 1
maxWorkspacesPerRun     单个 Run 最多自动创建 worktree 数，默认 3
maxReviewRounds         验证/审查失败后最多修复轮次，默认 2
maxPendingInputs        未路由用户输入上限，默认 20
```

新增开关对既有 Team 默认关闭，保持升级兼容。用户开启“团队自治”时，可以一次启用反馈路由、审查和验证；成员供给与 worktree 仍显示独立开关和额度。`autoApplyChanges` 继续固定为 `false`。

策略摘要进入 `inputHash` 和动作审计。自治额度耗尽时进入 `waiting_input`，Agent 不得自行提高额度。

## 3. 持续反馈：一个团队入口，不强迫用户管理 Run

前端始终提供一个团队输入框。用户不需要先判断“继续当前任务”还是“新建 Run”，但必须能看到系统最终选择了哪条路径。

每条输入先登记为 `TeamInput`；请求正文不可变，路由投影随版本更新：

```text
inputID、teamID、runID、text、createdAt
routeStatus、routeReason、derivedTaskIDs、childRunID、policyDigest、version
```

网关在收到输入时先写 `input_accepted`，再调用 Lead 规划；HTTP 返回已受理，而不是等待整个规划完成。输入正文和派生关系进入 journal，Lead 只看到当前所需的有界上下文。

路由过程不能直接改写已完成的 Task，也不能撤销已产生的文件副作用。任何基于旧快照的决策都要在提交前重新检查 Run、Task 和 Team 版本。

### 3.1 反馈路由

Lead 必须返回以下一种路由，不能同时把同一意见复制给多个成员：

| 路由 | 适用情况 | 网关动作 |
| --- | --- | --- |
| `steer` | 补充当前运行任务的约束 | Agent 支持 steering 时注入当前 turn，否则排入该会话后续任务 |
| `follow_up` | 延续某个已完成任务 | 在同一成员/工作区创建新 Task，并依赖来源 Task |
| `new_task` | 意见是独立的新增工作 | 选择现有成员或请求供给成员，加入当前 DAG |
| `amend` | 目标、验收标准或计划整体变化 | 创建子 Run，保留父 Run 和修订血缘 |
| `answer` | 只需解释，不需要执行 | 返回答复，不创建任务 |
| `clarify` | 目标歧义会导致错误分派或副作用 | Run 进入 `waiting_input`，向用户提问 |

路由约束：

1. 用户明确指定 Task 时优先作为来源，但网关仍检查它是否属于当前 Run。
2. 运行中的写任务不能被 `amend` 隐式取消；存在冲突副作用时先暂停并让用户选择。
3. `follow_up` 新节点保留原结果，不覆盖原 Task 的结果、状态或验证证据。
4. 无法可靠路由时选择 `clarify`，不能为了“全自动”猜测高影响意图。
5. 每次路由只生成一组带确定性 `actionKey` 的动作，重复规划不得重复派单。

### 3.2 连续意见与终态 Run

同一 Run 的输入按 journal sequence 串行路由，任务执行仍可并行。这样可以避免两条相近意见同时触发互相覆盖的计划。

- 可以对“尚未开始路由”的连续意见做一次批量规划，但每条输入都保留独立 ID、正文和派生结果，不能静默丢弃或只保存最后一条。
- 正在路由的输入不与其他输入合并；新输入排在其后。
- Run 完成前必须检查 `pendingInputs`、待执行动作、审查和验证均为空。完成快照记录已消费的最大输入序号。
- 输入在 Run 终态后到达时，网关自动创建 `parentRunID` 指向旧 Run 的 amend Run；旧 Run 永不重开。
- 新 amend Run 只复用满足既有 `inputHash` 和工作区安全规则的结果，新意见会影响的任务必须重新执行。
- 超过 `maxPendingInputs` 时拒绝新自动规划并提示用户处理积压，不能无界消耗 token。

用户看到的是一条连续团队时间线；Run 边界以“第 N 次修订”展示，而不是要求用户理解底层状态机。

## 4. 动态成员：先复用，确有能力缺口再创建

成员选择按固定顺序进行：

1. 用户明确指定的成员。
2. 已完成来源任务的原成员，适合保持上下文和工作区连续的 `follow_up`。
3. 具备所需角色、能力和相同工作区的其他启用成员。
4. 以上均不存在且策略允许时，供给一个新成员。

“暂时离线”不等于“没有能力”。用户指定的成员失效时仍按现有规则显示 `unavailable`，不能静默改派；只有未指定成员且团队确实缺少能力时，才允许自动扩招。

TeamMember 扩展 `capabilities`、`origin`（manual/auto）、`provisionID`、`workspaceID` 和供给状态。角色描述能力意图，Agent 类型和声明能力决定能否执行，不能只按名称中的 “reviewer” 猜测。

### 4.1 成员供给协议

新增 `ensure_member` 意图：

```json
{
  "type": "ensure_member",
  "sourceTaskId": "task-lead-1",
  "round": 1,
  "member": "security-reviewer",
  "agentKind": "kimi",
  "role": "reviewer",
  "capabilities": ["code-review"],
  "reason": "现有成员没有独立安全审查能力",
  "reuse": "if_compatible"
}
```

网关校验 member ID、Agent 白名单、能力、额度和是否已有兼容成员。`provisionID` 由 team/run/actionKey 确定性生成，重复动作返回同一供给记录。

供给记录状态：

```text
proposed → provisioning → ready
                     ├──→ failed
                     └──→ outcome_unknown
```

依赖该成员的 Task 保持 `planned`，直到成员 `ready`；不能先向占位 sessionID 派发。自动成员使用普通真实 ACP 会话，创建后仍受现有队列、权限和单活动任务约束。

### 4.2 创建顺序与失败恢复

自动供给按以下顺序执行：

1. 根据任务确定最终 cwd；需要隔离时先创建并登记 worktree。
2. 写入 `member_provisioning`，包含 provisionID、Agent 类型、cwd、workspaceID 和策略摘要。
3. 调用现有 ACP `session/new` 能力创建真实会话。
4. 确认 native sessionID 后写入成员绑定和 `member_ready`，再释放依赖任务。

不能颠倒 worktree 与会话顺序，否则新会话会绑定错误 cwd。会话的工作目录视为不可变，后续任务需要另一个 worktree 时必须复用已绑定该工作区的会话或供给新会话。

`session/new` 没有统一的服务端幂等请求 ID，因此超时或断线后禁止直接再建一个会话。网关先通过占位 ID、canonical 映射和会话列表对账；无法唯一确认时标记 `outcome_unknown` 并等待用户处理。已创建的 worktree 或会话在后续步骤失败时保留供审计，不自动删除。

## 5. 自动 worktree 与工作区归属

新增 `workspaceMode=auto`。规划任务可声明规范化的 `readPaths`、`writePaths` 和 `workspaceGroup`；它们是调度提示，不是对 Agent 行为的信任假设。实际完成时仍以 git diff 为准。

```text
单个写任务或串行依赖链
  → shared_workspace + 既有项目写锁

同一仓库出现多个并行写任务
  → autoWorktree=true：每个写任务独立 worktree
  → autoWorktree=false：退化为独占串行，不强行并行

无法判断写入范围
  → 独占串行；若用户要求并行则先隔离，不按“无冲突”处理

follow_up / 审查后修复
  → 沿用来源任务的 workspaceGroup 和 worktree
```

同一 `workspaceGroup` 表示后续任务需要看到前序未提交改动，必须复用同一工作区，不能为每个节点机械地新建 worktree。

### 5.1 冲突判定

第一版采用仓库级保守冲突判定，不凭路径声明允许共享工作区并行写。即使两个任务声称修改不同文件，测试、代码生成和格式化仍可能产生全仓库副作用。

调度器在每次释放任务时重新计算冲突，而不是只检查创建 Run 时的静态 Plan。用户反馈派生的新任务也进入同一判定。

自动 worktree 创建前必须：

1. 解析项目根目录和明确的 `baseRevision`，拒绝路径逃逸。
2. 记录主工作区是否脏；未提交改动不会出现在新 worktree 中时，必须提示并等待用户选择基线，不能静默漏带。
3. 使用固定 argv 执行 git，生成受网关管理的 workspaceID 和路径。
4. 先登记 `workspace_creating`，成功后再记录 `workspace_ready`；失败保留原因，不自动重试副作用不明的操作。
5. 将会话 cwd、Task、workspaceID、baseRevision 和 workspaceGroup 绑定后入账。

声明路径若包含绝对路径、`..` 或仓库外目标则拒绝。实际 diff 与声明不一致不伪造“安全”，只在结果和集成检查中显式标记。

### 5.2 生命周期与集成边界

自动创建只扩展 worktree 的供给侧，不改变既有数据面红线：

- 任务完成、失败、取消和 Run 终止都不自动清理 worktree。
- 网关展示相对 baseRevision 的逐文件 diff、实际变更文件、截断标记和验证证据。
- 多个分支修改同一文件时生成 `integration_conflict` 预览；baseRevision 已落后时标记 `base_stale`。
- 不自动 apply、merge、rebase、cherry-pick、提交或删除分支；用户显式选择如何集成。
- 重启后按 workspaceID 对账。路径丢失或基线不一致时进入 `waiting_input`，不自动在新目录重放写任务。
- 用户显式清理仍使用不带 `--force` 的删除，脏区拒绝并保留改动。

因此“自动避免冲突”指执行期隔离，不等于自动解决集成冲突。

## 6. 自动审查与确定性验证

`autoReview` 只作用于产生文件或命令副作用的任务，纯分析和摘要任务不机械增加 Reviewer。默认顺序为：

```text
实现 Task
  → 工作区确定性验证
  → 独立 Reviewer 只读审查
  → approved：完成该分支
  → changes_requested：原实现成员在同一工作区修复
  → 重新验证并重新审查（最多 maxReviewRounds）
```

Reviewer 必须与实现者是不同真实会话，使用 `writeLock=none`，输入只包含目标、相关用户意见、有界 diff、验证证据和既有审查发现。缺少 Reviewer 时可按动态成员规则供给；策略不允许供给则暂停，不能把未审查结果标记为已审查。

验证失败时先修复，不消耗 Reviewer token。审查修复沿用来源 worktree，并依赖上一轮实现、验证和审查结果。验证失败与审查打回共用 `maxReviewRounds` 修复预算，避免两类问题交替触发无限循环。

### 6.1 审查闭环

审查结果使用结构化 schema：

```json
{
  "verdict": "changes_requested",
  "findings": [
    {
      "severity": "major",
      "path": "internal/example.go",
      "line": 42,
      "summary": "并发写入未关闭旧句柄",
      "suggestion": "在替换前关闭并等待旧句柄退出"
    }
  ]
}
```

`verdict` 只能是 `approved` 或 `changes_requested`。后者必须至少有一条可执行 finding；普通文本可以展示，但不能自动批准或触发修复。

审查绑定 `reviewOf`、workspaceID、baseRevision、headRevision 和 diffHash。工作区在审查后发生变化、相关新意见到达或依赖结果被替换时，旧 verdict 自动变为 `stale`，必须重新审查。超过修复轮次仍未批准时 Run 进入 `waiting_input`，保留全部发现和现有改动。

`writeLock=none` 是调度声明，不是文件系统沙箱。Reviewer 若造成额外 diff，审查立即失效并提示用户，不能用该 verdict 批准它自己修改的结果。

### 6.2 验证证据与完成条件

验证继续复用固定 argv 白名单，但执行目录必须从“Lead cwd”扩展为**被验证 Task 的实际 workspace**。Agent 可以建议验证名称，不能提交任意 shell 字符串。

每次证据至少包含：

```text
taskID、workspaceID、argv、cwd、baseRevision、headRevision、diffHash
exitCode、startedAt、durationMs、outputHead、truncated
```

任何文件变化都会使旧验证证据失效；不能拿其他 worktree、旧 diff 或 Agent 口头声明替代本次验证。声明了验证要求但无法执行时进入 `waiting_input`，用户可补充命令或明确标记“不适用”。

Run 完成必须同时满足：

1. 所有必需 Task 成功，且没有未处理的动作、问题或用户输入。
2. 需要审查的分支具有当前 diff 的 `approved`。
3. 需要验证的分支具有当前 diff 的零退出码证据。
4. 所有 worktree diff 和截断状态已经可读取；无法读取时不能声称交付完整。

多 worktree 场景只证明各分支独立通过。用户集成后需要重新运行整体验证，不能把分支通过描述成“合并后已通过”。

## 7. 网关约定

对外仍只有 HTTP/Web UI 与网关通信，Agent 之间不新增私有控制通道。内部扩展三个端口：

```text
FeedbackRouter      将有界输入和 Run 摘要交给 Lead，返回结构化路由
MemberProvisioner   对账并创建真实 ACP 会话
WorkspaceCreator    创建、登记和解析自动 worktree
```

现有 Scheduler 继续负责依赖、并发和锁；Dispatcher 继续复用 submissions 队列；Reducer 继续保存有界结果。HTTP 层不直接执行 git 或 session/new，所有副作用都由 Team Service 在记账后调用端口完成。

审查是带结果 schema 的特殊 Task，验证是绑定 workspace 的确定性闸门，两者都进入同一个 DAG 和 journal，而不是另建不可追踪的后台流程。

### 7.1 统一团队输入 API

```http
POST /api/teams/:teamID/inputs
GET  /api/teams/:teamID/inputs/:inputID
```

```json
{
  "id": "client-generated-uuid",
  "runId": "optional-explicit-run",
  "targetTaskId": "optional-source-task",
  "text": "这里不要改协议，另外补一个并发回归测试"
}
```

成功返回 `202 {inputId, runId, routeStatus:"accepted", duplicate}`。同一 `id` 同内容重放返回原输入，异内容拒绝。

未指定 runID 时：恰好一个活跃 Run 就投向它；没有活跃 Run 就从最近 Run 创建 amend，尚无 Run 则创建初始 autoPlan Run；多个活跃 Run 返回 409 要求明确选择。显式投向终态 Run 同样创建 amend，不重开旧 Run。

`POST /runs` 和 `POST /runs/:id/amend` 保留给高级显式编排，不由新入口替代。

### 7.2 结构化动作扩展

反馈规划仍使用 `<team-actions>`，新增 `route_feedback` 与 `ensure_member`；`delegate` 增加可选 `workspaceMode=auto`、`workspaceGroup`、`readPaths` 和 `writePaths`。

```json
{
  "actions": [
    {
      "type": "route_feedback",
      "inputId": "input-1",
      "route": "follow_up",
      "targetTaskId": "task-implement-1",
      "reason": "意见收紧了原任务约束"
    },
    {
      "type": "delegate",
      "member": "developer",
      "task": "保持协议不变并补并发回归测试",
      "fromTaskId": "task-implement-1",
      "workspaceGroup": "workspace-implement-1"
    }
  ]
}
```

同一次输入的路由及其派生动作按一个批次校验和入账，任一项非法则整批不执行，并走既有 violation 修复回路。`route_feedback` 必须引用当前待路由输入；其他成员不能为未授权输入伪造路由。审查结论走 Task 结果 schema，不通过动作信封自动批准。

### 7.3 事件与投影

新增事件：

```text
input_accepted / input_routed / input_rejected
member_provisioning / member_ready / member_provision_failed / member_outcome_unknown
workspace_creating / workspace_ready / workspace_failed / integration_conflict
review_completed / review_stale
```

复用 `run_verify` 并增加 taskID、workspaceID、headRevision 和 diffHash。所有事件只携带有界摘要；完整会话仍在原 session，完整 diff 仍走专门端点。

RunDetail 增加 `pendingInputs`、供给状态、工作区归属、当前审查 verdict 和每分支验证摘要。完成通知必须包含已消费输入序号、审查/验证状态和交付入口，不能只写“团队任务已完成”。

## 8. 状态、幂等与恢复

幂等键分别由 inputID、provisionID、workspaceID、actionKey、taskID/attempt 和 verificationID 派生，不能共用一个“重试计数”混淆不同资源。

| 恢复时发现 | 网关行为 |
| --- | --- |
| 输入已接受、未路由 | 用确定性路由 Task ID 继续规划，重复 submission 走既有队列去重 |
| 路由已入账、派生任务未入账 | 按同一动作批次补齐，已存在节点返回原结果 |
| member provisioning | 先对账真实会话，不能唯一确认则 outcome_unknown |
| workspace creating | 检查注册表、路径和 git 元数据，不能唯一确认则等待用户 |
| review/verify 在飞 | 审查按会话结果对账；已登记验证命令可在相同 diff 上重跑 |
| 工作区或 verdict 已变化 | 丢弃旧审查/验证的完成资格，按当前 diff 重做 |

所有终局仍 first-wins。恢复不自动重发结果未知的 prompt、session/new 或写任务，也不把 `outcome_unknown` 直接改成 `ready`。

amend 缓存除了既有 inputHash，还必须考虑反馈血缘和 workspace/diff 绑定；任何会影响结果的新意见都使命中无效。

## 9. 前端交互

第一版复用现有 Team/Run 页面，不新建复杂工作台：

1. 团队页顶部提供持续输入框，发送后立即显示“已受理”。
2. 每条意见显示路由结果：续作、新任务、修订、仅回答或需要澄清，并可查看理由。
3. 任务卡显示成员、分派原因、workspaceID、是否自动创建成员/worktree，以及原始会话入口。
4. 审查卡和验证卡分开显示；旧证据标记“已过期”，失败显示具体 finding、退出码和输出摘要。
5. 多个 worktree 以分支交付卡展示 diff 和集成冲突，应用与清理按钮始终由用户触发。
6. 设置页集中展示自治开关、成员/worktree 额度、审查轮次和预算；暂停时提供继续、修订或停止。

连续 Run 在时间线上折叠为同一团队对话，但每个执行代次、结果和证据仍可独立打开。

## 10. 分阶段实施

### 阶段一：持续反馈闭环

实现 TeamInput、统一输入 API、串行路由、steer/follow_up/new_task/amend，以及完成前 pendingInputs 检查。先只使用既有成员和共享工作区。

验收：连续发送两条意见不会重复派单；运行中可续作，终态后自动生成 amend，原结果不变。

### 阶段二：自动 worktree 与分支验证

实现 workspaceMode=auto、WorkspaceCreator、脏基线保护、工作区绑定和按实际 workspace 执行验证。

验收：两个并行写任务不共享目录；禁用 autoWorktree 时自动串行；重启不重复创建；diff 和验证指向同一 workspace/diff。

### 阶段三：自动审查修复回路

实现 Reviewer schema、独立会话约束、verdict stale、修复轮次和完成闸门。

验收：验证失败先修复；changes_requested 自动回到原实现成员；新 diff 不沿用旧批准；超轮次可解释暂停。

### 阶段四：动态成员供给

实现 ensure_member、MemberProvisioner、能力匹配、供给额度和 outcome_unknown 对账，再接入自动 Reviewer 供给。

验收：优先复用兼容成员；确有需要才创建一个真实会话；创建结果未知不会重复扩招；会话 cwd 与 worktree 一致。

## 11. 测试与验收

在现有 `internal/team` 与 `internal/httpd` 测试体系中增加：

1. **路由**：六种路由、非法批次原子拒绝、重复输入、乱序版本、连续输入、终态 amend 和 pendingInputs 完成拦截。
2. **成员**：兼容复用、指定成员不可用时禁止静默改派、额度、白名单、cwd 绑定、创建崩溃点和未知结果不重试。
3. **工作区**：并行写自动隔离、关闭开关后串行、workspaceGroup 复用、脏基线、路径逃逸、创建对账和 diff 冲突。
4. **审查**：Reviewer 与实现者不同会话、只读、schema 修复、diff 变化后 stale、修复轮次和超轮次暂停。
5. **验证**：固定 argv、实际 workspace cwd、diffHash 失效、非零退出码、输出截断和多分支不能冒充集成验证。
6. **恢复**：在 input、provision、workspace、dispatch、review、verify 和 finish 各记账边界注入重启，验证无重复副作用和 salvage 结果。
7. **真实验收**：两个不同 Agent 持续接收用户意见，至少一次并行 worktree、一次审查打回和一次验证失败，最终手机端能查看每个证据并手动取得 diff。

任何测试失败都不能以 Agent 自报成功替代。状态机测试沿用可控 clock/channel，不使用固定 sleep 判断正确性。

## 12. 明确不做

1. 不让 Agent 通过自然语言直接控制网关或其他成员。
2. 不引入新的 Agent runtime，也不把原生子代理当作统一 Team 成员协议。
3. 不自动应用、合并、提交或清理代码，不自动批准权限。
4. 不在工作区有未提交改动时静默复制、stash 或丢弃用户修改。
5. 不因成员离线而静默替换用户明确指定的成员。
6. 不承诺外部副作用的绝对 exactly-once；以幂等、对账和 outcome_unknown 防止重复。
7. 不为每个反馈机械新建成员或 worktree，不做全量 transcript 两两广播。
8. 不把独立分支的审查和测试通过宣称为集成后的完成。
9. 不让 Agent 自行提高预算、自治额度、审查轮次或关闭验证要求。
