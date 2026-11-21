# CCX 与团队功能下的 Agent 需求及 Snow 评估

日期：2026-11-21。范围：CCX 当前源码、Snow CLI v0.9.17 官方源码与本机只读检查；未实施接入。

## 1. 推荐结论

更理想的 Agent 是 **会话配置独立、能被程序精确控制、能把执行身份传给 CCX 的运行时**。模型厂商、上游故障转移和渠道选择可以由 CCX 负责；团队任务与验收由 mobile-bridge 负责。

| 层次 | 职责 |
| --- | --- |
| 主控模型 | 理解目标、选择工作成员、给出范围和完成条件、检查结果、纠偏 |
| mobile-bridge | Run/任务账本、成员生命周期、并发与工作区、消息控制、结果与验收 |
| Agent 运行时 | 原生模型循环、本地工具执行、结构化事件、会话恢复、指令干预 |
| CCX | 按协议及角色处理模型与渠道策略、协议转换、配额与故障转移、请求观测 |

**Pi RPC 仍适合作为可控工作成员候选；Snow 更接近开箱可用的完整多模型工作台。** Snow 的功能契合度较高，但当前 ACP 与身份标记存在具体缺口，适合先验证、补齐，再决定是否作为团队常用执行器。

主控也可以运行在这些 harness 上，关键在于给它接入团队控制工具。让它拥有派发、状态、干预与结果查询能力，才会形成持续指挥闭环。

## 2. CCX 已有能力与接入边界

本次读取 CCX worktree：`/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean`，HEAD 为 `fcb5ec72`。工作区已有无关修改，本次仅阅读。

- `config/subagent_routing.go:98`：只有策略已激活、`AgentRole=subagent` 且 `Confidence=exact` 才应用子代理策略。四文本协议分别配置。
- `utils/headers.go:112`：当前精确信号来自 Codex 风格 `client_metadata`，或 Claude Code 风格 session metadata 与 `X-Claude-Code-Agent-Id` 的组合。没有通用的 Snow/Pi 角色识别契约。
- `scheduler/select.go:975`：子代理候选限制在策略参与集合中，并按该协议策略序列赋予优先级；不是逐会话人工排序。
- `config/config_utils.go:165` 与 `config/subagent_routing.go:288`：非空子代理默认模型会覆盖子代理请求模型；空默认模型保留原请求模型，同时继续限制渠道集合。
- `scheduler/subagent_routing.go:12`：Responses 依赖密文续聊时固定实际成功模型、渠道与地址；来源无法恢复时明确报错，不随意迁移。

因此，“在手机团队里是 Worker”与“CCX 精确识别的 subagent 请求”是两个维度。独立启动的 Pi/Snow 会话，即使被团队标为工作成员，其 API 请求也可能仍进入普通池。

两种模型策略都可以合理使用：

1. 工作成员基本同档：让 CCX 子代理默认模型统一覆盖，减少客户端重复配置。
2. 快速成员与强模型审查并存：子代理默认模型保留为空，成员请求稳定别名或明确模型，CCX 负责实际映射；同一子代理池需支持这些模型。

例如 `team-main`、`team-fast`、`team-review` 可以作为自行配置的模型别名。这些名字是设计示例，不是 CCX 已内置的保留模型。

## 3. 理想 Agent 的接口要求

| 要求 | 对本项目的实际价值 |
| --- | --- |
| 启动参数可指定模型、快速辅助模型、工作目录与会话 | 一次启动即可明确生效配置；参数只影响当前进程/会话，避免改全局档案影响其他成员 |
| 每个成员拥有独立模型与工具配置 | 快速检索、复杂实现、独立审查可以分别选档位；快速辅助模型与工作成员模型分别表达 |
| Base URL、Key 来源与协议可配置 | 使用 CCX 的原生协议入口，认证配置集中管理；成员可选择符合模型能力的协议 |
| 每次 API 请求携带准确的身份 | session、parent、member、role 与 request ID 可关联；辅助摘要调用也能明确归属 |
| 稳定的 SDK/RPC 控制 | 创建、恢复、状态、发消息、steer、follow-up、停止、工具审批、模型设置及读回 |
| 角色、模型档位与权限分别表达 | Worker 可以使用强模型，Reviewer 可以只读；不能仅凭 role=worker 自动降档或扩大权限 |
| 完整会话恢复与任务完成证据 | 保留工具调用及回执；区分接收、运行、待审批、失败、中止与成功 |
| 上下文与迁移边界明确 | 模型切换须与历史兼容；不能把携带签名/密文的续轮当作任意模型都能接收 |

接入 CCX 时，建议增加客户端无关的结构化角色契约，由运行时或网关适配器逐请求生成并由 CCX 识别。它是待实现建议；目前不能假设任意 `X-Agent-Role` 自定义头已经有效，也不应为了触发现有规则冒充 Codex/Claude Code。

主控使用团队控制工具选择成员和档位；CCX 对同一套已授权策略做实际模型映射与渠道调度。审查、验收和任务身份统一归入 mobile-bridge 的账本。若使用 Snow 内部团队功能，应明确它与这本账之间的映射关系。

## 4. Snow 的契合点

本次确认的项目为 npm 包 `snow-ai`，仓库 `MayDay-wpf/snow-cli`。官方最新发布与本机版本均为 **v0.9.17**。

Snow 与需求相符的功能包括：

- `advancedModel` 用于主工作流，`basicModel` 用于辅助任务；API 支持 Chat、Responses、Anthropic 和 Gemini，Base URL 可配置。
- 自定义子 Agent 可以绑定不同 profile，分别指定模型、API、系统提示和工具列表。
- 运行中的子 Agent 接受消息，内部实现会在下一次模型调用前注入待处理消息；另有子 Agent 会话查询和继续执行能力。
- 原生 Team Mode 提供共享任务列表、依赖、队友消息、worktree 等较完整的协作功能。
- 提供 `snow --ask`、`snow --acp`、SSE，以及 `snow cmd ... --json` / `session-command-run` 命令控制面。

注意三个配置层并不等价：**basicModel 不代表所有子 Agent 自动使用快速模型**。`subAgentExecutor.ts:361` 为子 Agent 选择所绑定 profile 的 `advancedModel`，未绑定时使用主 profile 的 `advancedModel`。当前 Team Mode 的 `teamExecutor.ts:453` 也读取活动配置的 `advancedModel`。

因此需要明确配置快速子 Agent 的 profile；单独改 basicModel 无法证明 Team Mode 队友已降档。指定 profile 无法加载时，当前子 Agent 路径会警告后回退到主配置，这也需要在预算策略里处理。

## 5. Snow 的已发现缺口

| 缺口 | 证据与影响 |
| --- | --- |
| 顶层 CLI 没有直接模型启动参数 | 本机 `snow --help` 与 v0.9.17 `cli.tsx` 都未提供 `--model`、`--fast-model` 或会话级 profile 参数 |
| 命令控制面改的是持久配置 | `config set advancedModel=... basicModel=...` 调用 `updateSnowConfig`，写活动配置/profile；不是只影响一个新会话的启动覆盖 |
| ACP 模型与 steer 控制缺失 | `acpManager.ts:110` 的 handler 提供 initialize/auth/new/load/prompt/cancel，没有模型设置、config option 或 steer；prompt 读取全局活动配置 |
| 不存在的 ACP 会话也加载成功 | 本机随机不存在的 ID 调用 `session/load` 返回 `result:{}`；源码在内部会话缺失时用空消息继续创建内存会话 |
| ACP 历史恢复需要补齐 | load 只映射 role/content，未恢复原结构化工具字段；当前 ACP 文件未发现创建/保存内部会话的调用，不能据此承诺冷恢复完整性 |
| ACP 工作目录隔离不足 | new/load 使用进程级 `process.chdir`，目录切换失败后继续成功返回；多会话工作目录需要重新设计或限制为每进程一个会话 |
| 默认请求不满足 CCX 的精确角色标记 | Snow Responses payload 没有上述 Codex client_metadata；Anthropic 的 metadata.user_id 为普通字符串，不满足 CCX 的 Claude Code JSON 标记检查 |

源码中的 `isSubAgentRequest` 主要控制系统提示组织，并未在所读默认请求路径中转成 CCX 所需的外部角色标记。自定义头提供扩展入口，但需要逐请求区分主/子；整份 profile 固定为子代理头会误标主请求或辅助调用。

这些结论针对 v0.9.17 已读路径。Snow 的内部 UI/子 Agent 功能和对外 ACP 能力需要分开评价；SSE 与命令控制面可作为更深接入的路线，本次没有做它们的真实任务联调。

## 6. 验证记录与建议顺序

本机检查结果：

| 检查 | 结果 |
| --- | --- |
| `snow --version` | 0.9.17 |
| `snow --help` | 支持 ask、task、acp、sse、cmd；无直接模型/profile 启动参数 |
| `snow cmd config status --json` | 活动 profile 为 local-cx；advancedModel 与 basicModel 均为 gpt-5.4；协议为 responses |
| ACP initialize | 声明 loadSession 与 image 能力；没有模型/config option 信息 |
| ACP 随机不存在会话的 load | 返回成功的空对象 |
| 检查进程退出 | 关闭 stdin 后正常退出，exit=0 |

配置快照只说明请求模型，实际上游模型仍可能被 CCX 映射。当前配置本身尚未表达主/基础快慢分工。`cmd ... --json` 的本次 stdout 还包含 Loading 控制序列，集成方不能直接把全部 stdout 当成单个 JSON 文档。

检查只向新建的 Snow ACP 子进程发出 initialize/load；没有发送 prompt，没有调用真实模型。未改用户模型配置、未启动 Snow Team Mode、未操作 CCX 运行实例。隔离检查目录位于系统临时目录。

建议顺序：先补齐会话配置与恢复契约；再为 Snow/Pi 建立 CCX 可精确识别的客户端无关角色标记；随后用同一受控任务比较实际派发、steer/停止、错误、工具权限、会话恢复和成本；最后扩大到多成员。

若希望快速得到稳定的外部工作成员控制面，Pi RPC 可继续作为对照。若希望复用 Snow 丰富的内置工作流，应优先修复上述接入缺口，而不是仅启用更多内置 Agent。

## 7. 证据来源

CCX 当前工作区源码：

- [角色提取](/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean/backend-go/internal/utils/headers.go:112)
- [子代理策略启用条件](/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean/backend-go/internal/config/subagent_routing.go:98)
- [参与集合及优先级](/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean/backend-go/internal/scheduler/select.go:975)
- [角色模型解析](/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean/backend-go/internal/config/config_utils.go:165)
- [Responses 续聊固定成功来源](/Users/petaflops/works/api-gateways/llm-routing/ccx-v3-lean/backend-go/internal/scheduler/subagent_routing.go:12)

Snow v0.9.17 官方源码与文档：

- [发布版本](https://github.com/MayDay-wpf/snow-cli/releases/tag/v0.9.17)
- [CLI 参数实现](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/cli.tsx#L285)
- [主/基础模型配置](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/docs/usage/en/02.First%20Time%20Configuration.md#L338)
- [子 Agent 配置与消息](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/docs/usage/en/05.Sub-Agent%20Configuration.md)
- [子 Agent 选模](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/utils/execution/subAgentExecutor.ts#L361)
- [子 Agent API 调用](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/utils/execution/subAgentStreamProcessor.ts#L24)
- [Team Mode](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/docs/usage/en/22.Team%20Mode%20Guide.md)
- [Team Mode 选模](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/utils/execution/teamExecutor.ts#L453)
- [命令控制面](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/docs/usage/en/29.Session%20Command%20Control%20Plane.md)
- [配置写入实现](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/utils/execution/sessionCommandHandlersExtra.ts#L1510)
- [ACP handler 与会话恢复](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/utils/acp/acpManager.ts#L110)
- [Responses 请求 payload](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/api/responses.ts#L905)
- [Anthropic 请求 metadata](https://github.com/MayDay-wpf/snow-cli/blob/v0.9.17/source/api/anthropic.ts#L771)

候选工具的前次比较见 [团队 Agent 选型](2026-11-21-team-agent-selection.md)。本篇只补充 CCX 与 Snow 的证据，不把只读协议检查当成真实多模型任务验收。
