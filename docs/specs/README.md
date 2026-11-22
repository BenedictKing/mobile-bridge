# Mobile / Bridge 特性文档

状态：现行特性文档总入口。

## 1. 文档地图

| 特性域 | 现行基准 | 覆盖内容 |
| --- | --- | --- |
| 网关与 Agent | [网关、会话与 Agent 运行时](gateway-and-agent-runtime.md) | 会话发现、项目、新建、Codex/ACP、模型、Skill、能力与生命周期 |
| 聊天与消息 | [聊天、时间线与消息操作](chat-and-timeline.md) | 历史、实时同步、发送队列、编辑分支、审批、附件与产物 |
| 迁移与压缩 | [会话迁移与网关压缩](../session-migration-and-gateway-compaction.md) | 迁移步骤、压缩、源会话完成提示、重试与转录边界 |
| 团队编排 | [团队、Run 与编排交付](teams-and-orchestration.md) | TeamInput、Issue、Run/Task、审查验证、工作区、Swarm |
| Goal 与评估 | [Goal、计划与自动评估](goals-and-evaluation.md) | 工作模式、原生/托管 Goal、Jev、自动继续与权限预判 |
| 账号与额度 | [账号、接入与额度管理](accounts-and-quotas.md) | Codex 接入、ACP 账号池、额度、自动切换与续跑 |
| 连接与客户端 | [连接、客户端与通知](connections-clients-notifications.md) | 登录、网络入口、直连、SSH、Web/移动端、通知与未读 |
| 部署与可靠性 | [部署、数据与可靠性](deployment-data-reliability.md) | 部署、账本、幂等恢复、安全、测试和运维 |

跨域需求同时修改所有受影响文档。例如“团队主控压缩”同时涉及团队编排和迁移压缩；“账号耗尽后 Goal 续跑”同时涉及账号额度和 Goal 评估。

## 2. 现行文档的写作契约

每份现行基准应随行为维护以下内容：

1. **用户目标与范围**：功能解决什么问题，明确不在范围内的能力。
2. **逐步行为**：用户操作、网关动作、目标状态和可见反馈按顺序对应。
3. **状态与异常**：区分中间态、成功、确定失败和结果未知，不只写理想路径。
4. **数据与安全**：说明事实来源、持久化、权限、隐私和资源边界。
5. **验收标准**：给出可测试的正向、重试、重启和异常场景。
6. **实现参考**：链接详细设计、运行图或代码入口，避免在多篇文档复制易漂移的协议细节。

“已完成”“已接受”“执行结束”“目标达成”等词必须按各领域定义使用，不能互换。内部 API 状态与面向用户的措辞也应分开说明。

## 3. 既有详细文档

以下文档作为对应基准的详细参考；若与代码或新确认的现行基准冲突，应先核实并修正文档状态，而不是直接沿用旧描述。

- 总体：[用户说明](../../README.md)、[实现说明](../../ARCHITECTURE.md)、[项目运行图册](../system-runtime-diagrams.md)、[验证记录](../../VERIFICATION.md)。
- 团队：[团队运行图册](../team-runtime-diagrams.md)、[Run 执行与目标达成契约](../team-run-outcome-contract.md)、[TeamInput 队列](../team-demand-queue-plan.md)、[目标工作流规划](../team-goal-workflow-planning.md)、[控制与执行会话隔离](../team-control-execution-separation-plan.md)。
- Goal 与可靠性：[ACP 网关托管 Goal](../acp-gateway-goal-plan.md)、[事件同步](../event-sync-plan.md)、[可靠性演进](../reliability-evolution-plan.md)。
- 账号：[Codex 账号切换](../account-switching.md)、[ACP 额度自动切换](../acp-quota-auto-switch-design.md)。
- 客户端与部署：[Android](../android.md)、[iOS](../ios.md)、[固定域名](../fixed-domain.md)、[服务器 SSH](../server-ssh.md)、[部署 Agent](../deployment-agent.md)。
- Swarm：[本地执行实施记录](../team-swarm-implementation.md) 描述当前范围；[目标设计](../team-swarm-riding-design.md) 中尚未实现的自动供给与隔离能力不得写成现状。

## 4. 历史资料与未实施方案

`docs/` 中带日期的修复记录、调研、审计、brainstorm、study、go-migration 过程文档，以及明确标记“设计中”“待评审”“未实施”的计划，均属于历史或背景资料，不是现行特性契约。

这些文件不需要随每次代码变化机械更新，但必须保留其原始状态，不能删除“未实施”标记后冒充现行文档。新需求若要沿用其中方案，应先把确认后的行为写入本目录对应基准，再实施。

## 5. 需求到交付的同步流程

1. **定位特性域**：在本索引找到受影响基准；没有对应领域时，先补充索引和文档骨架。
2. **先写需求**：修改正常步骤、状态反馈、异常结果、兼容策略和验收标准；跨域需求同步修改所有相关文档。
3. **核对现状**：区分已有行为、新增需求和不再支持的旧行为，避免把历史计划当成现状。
4. **实现与测试**：代码、协议、前端和测试共同满足文档；新增持久字段还要覆盖重载与恢复。
5. **回写偏差**：实施中确认的约束立即回写，交付前不得留下相互矛盾的现行描述。
6. **验证交付**：运行受影响测试，更新 `VERIFICATION.md` 中需要人工记录的真实验证范围，并说明未验证部分。

仅修复不改变行为的错字、链接和表述时，可直接修改文档。任何用户可见行为、状态语义、接口、权限或持久化变化，都必须文档先行。
