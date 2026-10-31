# 团队 Review 需求逐项验证

> 状态：设计草案，未实施 · 2026-10-31
>
> 参考：[phodal/pero](https://github.com/phodal/pero) ROADMAP「软件需求验证」条目（每条需求给出结论与证据，未知不计为满足）。上游设计见 [team-overall-design.md](team-overall-design.md) §8 与 [team-autonomous-feedback-plan.md](team-autonomous-feedback-plan.md) §6.1。

## 1. 目标

需求和意见目前直接在 Team Chat 中提出，每条输入成为一个 Run 的 `goal`。Review 现在只给整体裁决，用户看到“审查已通过”时，无法知道哪条需求被检查过、依据是什么。

本设计让 Review 对每条需求单独给出结论：

| 结论 | 含义 | 是否计为满足 |
| --- | --- | --- |
| `met` 满足 | 有代码位置、命令结果或输出摘录作为证据 | 是 |
| `unmet` 未满足 | 已确认不符合，附原因 | 否 |
| `unknown` 无法判断 | 证据不足、未检查或无法检查 | 否 |

核心规则：**只有 `met` 计为满足。`unknown` 不能折算为通过，缺项、无证据或格式错误一律按 `unknown` 处理。**

不在范围内：任务来源导入（Issue、Roadmap）、自动修复回路、diff 绑定的验收。见 §12。

## 2. 现状复核

对照 Pero ROADMAP 已勾选或计划的条目，逐项核对本仓库代码（2026-10-31 `main`）：

| Pero 条目 | 本仓库现状 | 结论 |
| --- | --- | --- |
| 执行租约：过期执行不能覆盖新结果 | `Task.Attempt` + `ExecutionThreadID`，结算侧丢弃旧 attempt / 旧线程迟到事件（`internal/team/runner.go`） | 已具备 |
| 重复请求对账 | submission 幂等；`POST /inputs` 同 id 同内容返回原结果，异内容拒绝 | 已具备 |
| 取消与恢复、未知结果待核对 | `RunOutcomeUnknown` / `task_outcome_unknown`，恢复时不重放已确认 submission | 已具备 |
| 工作区单写者 | `shared_workspace + exclusive write`，Review 默认 `WriteLock=none` | 已具备 |
| 任务依赖、多任务容量 | DAG `DependsOn`、`MaxParallelTasks`，provider/model 级 AIMD 并发桶（`aimd.go`） | 已具备，且比 Pero 计划项更细 |
| 实时进度、断线补齐 | SSE `?since=` 游标续传 | 已具备 |
| 移动端客户端与通知 | 手机 Web、Android 容器、Bark/ntfy 通知；Pero 均未实施 | 领先 |
| 交付各维度分开展示 | 交付卡分别显示 Agent 完成度、审查、验证、待答问题（`web/team-chat.js`） | 已具备 |
| 定时任务忙时策略 | 团队忙时跳过到期点并留痕（`schedule.go`），无排队 / 合并选项 | 部分具备 |
| 执行快照 | 保存 `SentText` 全文和 `inputHash`（含 `PolicyDigest`）；**未记录成员实际模型、思考强度、Agent 版本** | 部分具备 |
| 人工决定绑定产物版本 | `SetDeliveryStatus` 只校验 `Run.Version`；`diffHash` 只出现在设计文档，代码未实现。共享工作区在 Run 完成后被改动，验收无法察觉 | **未具备**，此前说法需更正 |
| 软件需求逐项验证 | Review 只解析末行 `<team-review>passed/failed</team-review>`（`review.go`），无逐项结论 | 未具备，本文设计 |

另有两点影响本设计：

1. **Review 任务不会自动产生。** 设置默认评审（`Team.ReviewerID`）只决定“哪个成员的任务算评审”，仍需 Lead 主动 delegate 给评审成员；Lead 不委派时，交付卡显示“审查：未参与”。
2. **`autonomous-feedback-plan` §6.1 设计的 `approved / changes_requested + findings` 结构尚未落地**，当前代码仍是 `passed / failed`。本文沿用现有词汇，结构上为 findings 预留位置（§4.3）。

## 3. 需求清单

逐项验证的前提是“项”在执行前固定。如果由 Reviewer 在审查时自行拆分需求，它可以漏掉难以满足的条目，再宣布全部满足。因此清单由 Lead 在规划时给出，网关冻结后才派发 Review。

### 3.1 来源

| 来源 | 适用 Run | `source` |
| --- | --- | --- |
| Lead 首个动作块的 `requirements` 字段 | `autoPlan` Run | `lead` |
| Reviewer 按 goal 原文自拟 | 显式计划 Run，或 Lead 未提供清单 | `reviewer` |

`source=reviewer` 的清单在界面上标注“评审自拟”，提示用户核对范围是否完整。

### 3.2 Lead 动作块扩展

在现有 `{"actions":[...]}` 顶层增加可选字段：

```json
{
  "requirements": ["登录失败 5 次后封禁该 IP", "封禁后发送安全通知", "电脑端可解除封禁"],
  "actions": [ ... ]
}
```

- 只接受 Lead 在本 Run **第一个**被采纳的动作块中提供；后续动作块再带该字段记为 violation，走现有修复回送。
- 数组元素为字符串，去空白后非空，每条 ≤200 rune，最多 20 条，去重后保持顺序。
- 编号由网关按顺序分配 `R1…Rn`，Lead 不写编号，避免编号校验。
- Lead 简报（`BuildLeadBriefing`）增加说明：把 goal 中可检查的要求逐条列出，用户原话中的约束不得合并或省略；没有可检查要求时可省略该字段。

### 3.3 冻结

- 清单随 `run_requirements_set` 事件入 journal，写入 `Run.Requirements`，此后不可修改。
- `autoDispatch=false` 时，清单与待确认动作一起显示在预览中；用户拒绝预览即放弃该清单。
- 本版不提供清单编辑。用户认为清单不对时，用修订（amend）或新输入重述需求。
- 只有 Review 任务注入清单。普通成员任务和 Lead 收尾任务不注入，避免改变现有派发文本与 `inputHash`。

## 4. Review 协议

### 4.1 简报

`BuildReviewBriefing` 在现有裁决协议前追加清单段。内容只依赖 `Run.Requirements` 与常量，保证派发文本确定：

```text
[Team review]
你是本团队的评审成员，本任务只做审查，不要修改文件。
请逐条检查以下需求（来源：Lead 规划）：
R1 登录失败 5 次后封禁该 IP
R2 封禁后发送安全通知
R3 电脑端可解除封禁
每条给出 met / unmet / unknown：
- met 必须附证据：代码位置（path:line）、执行的命令及退出码，或输出摘录；
- unmet 写明不符合之处；
- 没有检查、无法检查或证据不足时写 unknown，不要猜测为 met。
<team-checks> 与 </team-checks> 之间放一个 JSON 对象，紧接着在最后一行输出裁决标记。
```

清单为空、需要 Reviewer 自拟时，第二段改为：“先按下面的运行目标原文逐条列出可检查的需求，再逐条检查”，并附 `Run.Goal` 原文。Reviewer 自拟的条目同样使用 `R1…Rn`。

### 4.2 回复格式

```text
……审查说明……
<team-checks>
{"checks":[
  {"id":"R1","status":"met","evidence":"internal/auth/ban.go:88 连续 5 次失败调用 ban()；go test ./internal/auth 退出码 0"},
  {"id":"R2","status":"unmet","evidence":"publish.go 未区分登录安全通知开关"},
  {"id":"R3","status":"unknown","evidence":"桌面端界面未能在本环境运行"}
]}
</team-checks>
<team-review>failed</team-review>
```

- 自拟清单时，每条额外带 `"text":"需求原文"`。
- 末行 `<team-review>` 协议保持不变，旧解析器继续可用。
- 整段回复只允许一个 `<team-checks>` 块，取法与动作信封一致：成对标记、取最内层开始标记，多于一个视为无效。

### 4.3 预留

每条 check 允许可选的 `findings` 数组，字段沿用 `autonomous-feedback-plan` §6.1（`severity / path / line / summary / suggestion`）。本版只透传展示，不驱动修复。

## 5. 网关判定规则

解析在 `ExtractResult` 中进行，与 `ExtractReviewVerdict` 一样基于**截断前全文**，纯函数、无 I/O。

### 5.1 逐项归一

以冻结清单为准逐条生成结论，Reviewer 的输出只能填充，不能增删条目：

| 情况 | 归一结果 | 记录的 `reason` |
| --- | --- | --- |
| 无 `<team-checks>` 块、多个块、JSON 解析失败 | 全部 `unknown` | `checks_missing` / `checks_invalid` |
| 清单中某 ID 未出现 | 该条 `unknown` | `not_reported` |
| 同一 ID 出现多次 | 该条 `unknown` | `duplicate` |
| `status` 不在 `met / unmet / unknown` 中 | 该条 `unknown` | `invalid_status` |
| `met` 但 `evidence` 去空白后为空 | 该条 `unknown` | `no_evidence` |
| 清单外的 ID | 丢弃，计入 `ignored` 数 | — |

`evidence` 每条截断到 300 rune 并标记 `truncated`。

自拟清单（`source=reviewer`）时，Reviewer 输出的条目即为清单，上限同样 20 条；超出部分丢弃并标记 `truncated`。没有任何有效条目时，视为“没有可检查的需求”，`total=0`。

### 5.2 整体裁决

保留 Reviewer 末行原始裁决 `reviewTag`，另算派生裁决 `verdict`，交付卡只用 `verdict`：

| 条件（按顺序匹配） | `verdict` |
| --- | --- |
| 任一条 `unmet` | `failed` |
| `reviewTag=failed` | `failed` |
| `total>0`，全部 `met`，且 `reviewTag=passed` | `passed` |
| `total=0` 且 `reviewTag=passed` | `passed`，但 `requirements.total=0` 单独展示 |
| 其余（含有 `unknown`、无末行裁决） | `unknown` |

由此保证：

- 有一条 `unknown`，整体就不可能是 `passed`。
- Reviewer 写 `passed` 不能覆盖逐项中的 `unmet`。
- 全部 `met` 但缺末行裁决，仍是 `unknown`，不替 Reviewer 下结论。

### 5.3 证据的边界

网关只校验证据“有没有”，不校验“对不对”。`met` 仍是 Reviewer 的判断，界面不能把它描述为已验证。确定性证据仍然只来自 Verify 闸门的退出码。

## 6. 失效与修订

### 6.1 审查后又有写入

Review 是 `WriteLock=none`，可以和写任务并行；Lead 也可能在 Review 之后再派修复任务。此时逐项结论针对的是旧状态。

规则：同一 Run 中，任一 `exclusive` 任务的 `task_completed` 事件 `Sequence` 大于某 Review 任务的 `task_dispatching` 事件 `Sequence`，该 Review 的逐项结论标记 `stale`。

- `stale` 时派生 `verdict` 固定为 `unknown`，原始结论保留展示，标注“审查后有新改动”。
- 判定只用 journal 序号，不依赖文件内容；Reviewer 自己造成的写入不在此规则内（它是只读任务），由 `autonomous-feedback-plan` §6.1 的额外 diff 检测处理。
- 同一 Run 有多个 Review 任务时，交付卡取 `Ordinal` 最大者（与现有 `teamDeliveryEvidence` 一致）。新的 Review 完成即覆盖旧的 `stale`。

### 6.2 修订 Run（amend）

- goal 不变的 amend 继承父 Run 的 `Requirements` 与 `source`；goal 改变时清空，由新 Run 的 Lead 重新给出或 Reviewer 自拟。
- **需要修补的点**：`ImportPrefix` 按创建期 `inputHash` 匹配，该哈希不含简报，清单变化后旧 Review 结果仍可能被缓存导入。Review 任务的创建期哈希需加入清单摘要（`requirementsDigest`），或 amend 时 Review 任务一律不导入。推荐前者，改动只影响 `isReviewTask` 为真的任务。

### 6.3 持续意见

每条新的 Team Chat 输入是新 Run，清单只覆盖这条输入。“新意见是否破坏了前一个 Run 已满足的需求”不在本版范围，见 §12。

## 7. 持久化与投影

### 7.1 数据模型

```text
Run.Requirements:
  source: lead | reviewer
  items:  [{id, text}]          // 最多 20 条
  digest: sha256(source + items)

TaskResult.RequirementChecks:   // 仅 Review 任务
  reviewTag: passed | failed | unknown
  items:     [{id, text?, status, evidence, truncated, reason?}]
  ignored:   int                // 清单外条目数
  parse:     ok | checks_missing | checks_invalid
```

- `TaskResult.ReviewVerdict` 保留，继续存 `reviewTag`，旧读者不受影响。
- 两个新字段都遵循现有快照四处同步纪律：`runKnownFields`（及 TaskResult 的已知字段表）/ `jsonObject` Set / `Clone` / `applyJournalEventToRun`。
- `task_completed` 事件载荷携带有界的 `requirementChecks`（20 条 × ≤300 rune 证据），`recovery.go` 与 `store.go` 从 journal 重建时同步读取，与 `reviewVerdict` 现有做法一致。
- `stale` 不落快照，投影时按 §6.1 由事件序号计算，避免重启后与 journal 不一致。

### 7.2 交付证据投影

`teamDeliveryEvidence` 的 `review` 块扩展为：

```json
{
  "taskId": "review-1",
  "taskStatus": "completed",
  "verdict": "unknown",
  "reviewTag": "passed",
  "stale": false,
  "requirements": {
    "source": "lead",
    "total": 3, "met": 1, "unmet": 0, "unknown": 2,
    "items": [{"id":"R1","text":"…","status":"met","evidence":"…"}]
  }
}
```

`verdict` 改由 §5.2 派生；没有 `requirementChecks` 的旧结果，`verdict` 维持原逻辑（即 `reviewTag`），`requirements` 键缺省。

## 8. 前端展示

团队聊天交付卡与 Run 页共用 `renderTeamDeliveryEvidence`，在“审查”维度后补充：

```text
审查：结果未知 · 需求 满足 1 / 未满足 0 / 无法判断 2（无法判断不计为满足）
▸ 逐项结果（Lead 规划）
  ✓ R1 登录失败 5 次后封禁该 IP
      internal/auth/ban.go:88 …；go test ./internal/auth 退出码 0
  ? R2 封禁后发送安全通知 · 未报告
  ? R3 电脑端可解除封禁 · 桌面端界面未能在本环境运行
```

- 逐项默认折叠；有 `unmet` 或 `stale` 时默认展开。
- 状态用文字区分（满足 / 未满足 / 无法判断），图标只做辅助，不只靠颜色。
- `reason` 映射为短文案：未报告、无证据、重复、格式无效、审查后有新改动。
- `source=reviewer` 标注“评审自拟”；`total=0` 显示“未列出可检查的需求”。
- `reviewTag=passed` 而 `verdict` 不是 `passed` 时，显示“评审标记为通过，但存在未满足或无法判断的需求”。
- 验收按钮保持可用：是否接受由用户决定。存在 `unmet` 或 `unknown` 时，在按钮上方显示一行提示，不弹确认框。
- 文案进 `web/i18n.js`，英文界面同步。

## 9. 兼容与边界

| 场景 | 行为 |
| --- | --- |
| 旧 Run、旧 Review 结果 | 无 `Requirements` / `RequirementChecks`，交付卡与现在一致 |
| Lead 未委派 Review | 无逐项结果，仍显示“审查：未参与”；清单（若有）在 Run 页单独列出，标注“未经审查” |
| Lead 兼任评审 | `isReviewTask` 为假，不注入清单，与现有规则一致 |
| Reviewer 回复被截断 | 解析用截断前全文，不受 `MaxResultChars` 影响 |
| Review 任务失败、停靠或结果未知 | 不产生逐项结论，`verdict=unknown`（现有行为） |
| 清单使 Review 派发文本超长 | 清单计入简报长度；派发前按现有 `promptHardLimit` 校验，超限时任务确定性失败并提示缩短需求，不静默截断清单 |
| Reviewer 输出格式错误 | 不走修复回送（Review 不是 Lead 动作），直接全部 `unknown`；需要重新审查时通过 Run 页修订 |

单 Run 最多 20 条需求是刻意的上限：手机端需要在一屏内读完结论，超过时应拆成多个输入。

## 10. 实施顺序

### 阶段 A：Reviewer 自拟清单 + 逐项结论

不改 Lead 协议，先打通解析、判定与展示：

1. `review.go`：`BuildReviewBriefing` 增加自拟清单说明与 `<team-checks>` 协议；新增 `ExtractRequirementChecks` 与 §5 判定函数。
2. `model.go` / `reducer.go` / `recovery.go` / `store.go`：`TaskResult.RequirementChecks` 落快照与 journal。
3. `httpd/teams.go`：`review` 投影扩展，`verdict` 改为派生值。
4. `web/team-chat.js` + `i18n.js`：交付卡逐项展示。

阶段 A 完成后即可使用，只是清单由 Reviewer 给出。

### 阶段 B：Lead 冻结清单

1. `actions.go`：动作块解析 `requirements`，校验并产生 `run_requirements_set`；`BuildLeadBriefing` 增加说明。
2. `model.go`：`Run.Requirements` 四处同步。
3. `review.go`：有冻结清单时改用固定清单简报。
4. 预览卡（`autoDispatch=false`）显示清单。

### 阶段 C：失效与修订

1. §6.1 `stale` 投影计算。
2. §6.2 Review 任务创建期哈希加入 `requirementsDigest`。

### 可选阶段 D：有清单时自动补 Review

团队设置了默认评审、Run 有清单、却没有完成的 Review 任务时，网关在 Lead 收尾前补一个 Review 任务，做法同 `ensureAutoPlanReductionLocked`。这会改变现有“Review 只能由 Lead 委派”的行为，需单独确认后再做。

## 11. 测试与验收

单元测试（`internal/team/review_test.go` 表驱动）：

1. §5.1 每一行归一规则各有用例：缺块、多块、坏 JSON、缺 ID、重复 ID、非法 status、`met` 无证据、清单外 ID。
2. §5.2 裁决表逐行覆盖，重点：一条 `unknown` + `reviewTag=passed` → `unknown`；一条 `unmet` + `reviewTag=passed` → `failed`；全部 `met` 无末行标记 → `unknown`。
3. 证据截断按 rune 边界，不劈多字节字符。
4. 截断前全文解析：结果超过 `MaxResultChars` 时逐项结论不丢失。
5. 简报确定性：同一清单两次组装字节一致，`inputHash` 稳定。

持久化与恢复（`store_contract_test.go`、`recovery_test.go`）：

6. 删除 `team-runs.json` 后从 journal 重建，`Requirements` 与 `RequirementChecks` 完整恢复。
7. 未知字段保留：旧版本写入的 Run 经新版本读写后不丢键。

投影与前端（`teams_delivery_evidence_test.go`、`tests/test_team_chat.js`）：

8. 旧 Run 投影与改动前逐字段一致。
9. 计数、`source`、`stale`、`reason` 文案正确；英文界面无漏翻。

阶段 B/C 追加：Lead 第二个动作块带 `requirements` 产生 violation；amend 时清单变化的 Review 不被缓存导入；Review 后完成的写任务使结论变为 `stale`。

验收标准：手机交付卡上能看到每条需求的结论与证据，且任何含 `unknown` 的审查都不显示为“已通过”。

## 12. 暂不做

- **任务来源收件箱**（GitHub Issue、Roadmap 导入）：需求目前直接在 Team Chat 提出，暂不需要。
- **清单编辑**：用修订或新输入代替。
- **跨 Run 回归检查**：新意见是否破坏前一 Run 已满足的需求。需要团队级需求台账，等持续反馈路由（`autonomous-feedback-plan` 阶段一）落地后再评估。
- **`unmet` 自动触发修复**：属于 `autonomous-feedback-plan` §6.1 修复回路。
- **验收绑定 diff**：`SetDeliveryStatus` 目前只校验 `Run.Version`，共享工作区在 Run 完成后被改动不可察觉。这是独立问题，建议单独立项。
- **执行快照补记模型与思考强度**：同样独立，可在 `ExecutionThread` 投影中补充。
- **网关核验证据真实性**（例如检查 `path:line` 是否存在）：可作为后续增强，本版只校验证据非空。
