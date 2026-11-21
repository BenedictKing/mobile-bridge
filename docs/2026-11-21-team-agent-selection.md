# 团队 Agent 选型与控制能力调研

调研日期：2026-11-21。状态：调研完成，接入建议未实施。

## 1. 推荐结论

针对“团队主控持续调度成员、按任务选择快慢模型”的目标，优先评估 **Pi RPC**；希望复用现有接入并缩短落地时间时，优先补 **OpenCode 原生 API**。这里比较的是运行时可控性，不能据此推断同一模型在新工具里一定更聪明。

| 候选 | 模型配置 | 主控可用的控制面 | 适合本项目的方式 |
| --- | --- | --- | --- |
| Pi v1.6.0 | CLI 指定 provider/model/thinking；RPC 切换模型；快慢成员分别启动 | steer、follow_up、abort、状态查询、会话分支；扩展拦截工具调用 | 团队管理长期运行的成员进程；控制与模型选择由本项目负责 |
| OpenCode v2.3.2 | 会话模型与每个子 Agent 模型独立；CLI 显式指定模型 | 原生服务的消息、收件箱、中断、事件与权限；ACP 能力较窄 | 复用已接入的 OpenCode，按需增加原生服务控制 |
| Claude Code / Agent SDK | 主模型＋具名子 Agent 模型；CLI 可用 JSON 一次定义 | SDK 工具、权限、Hooks、会话及子 Agent 控制 | 以 Claude 模型为主，希望较少自建工具循环时 |
| Codex app-server | thread/turn 指定模型、provider、effort等 | turn/steer、turn/interrupt、审批、流式事件、会话恢复 | 深度接入 Codex 的原生执行；需接通现有团队的成员控制端口 |
| Aider | --model、--editor-model、--weak-model | architect/editor 双阶段编辑；持续成员控制需另做集成 | 作为专门的代码编辑成员，选型优先级低于前四项 |

Pi 的定位是可扩展 Agent harness，默认没有内置子 Agent 与 plan mode。团队编排可继续由 mobile-bridge 负责；角色、权限和审批扩展需要接入代码实现。

OpenCode 已在项目的 Agent profile 中，新增同名入口无法获得更多原生能力。应优先核对现有 ACP 通路实际遗漏了什么。

## 2. 当前团队接入的约束

本次读取的当前源码显示：

- [成员模型](../internal/team/model.go#L259)引用既有会话，保存 Agent kind、角色与标签，没有独立的 provider/model/effort/tools 配置契约。标签可以辅助主控选人，但不等于运行时已应用模型设置。
- [团队派发适配器](../internal/httpd/teams_adapter.go#L93)以 `mode=queue` 投递任务。[队列端口](../internal/team/ports.go#L42)提供投递、查询和取消待发送条目；它本身没有按任务暴露 steer 或模型切换。
- [成员归属解析](../internal/httpd/teams_adapter.go#L286)在 ACP manager 集合里寻找成员。[解析器端口](../internal/team/ports.go#L60)也直接返回 ACP runtime 类型。Pi RPC 与 Codex 原生 app-server 不能只靠增加一个 profile 字符串接入。
- [OpenCode profile](../internal/acpagents/acpagents.go#L297)已存在，声明 skills/fork，没有声明 steer；[Claude profile](../internal/acpagents/acpagents.go#L226)走 ACP wrapper 的专用 steering 方法。
- [Run 专用执行会话](../internal/httpd/teams_adapter.go#L157)通过 ACP manager 创建，并继承成员会话设置。未来增加任务级模型配置时，需要在首条消息前确认设置已实际应用。

这些是控制端口的限制，不能单独证明用户感到配合不佳的全部原因。现有任务拆分、上下文传递、模型遵循能力和反馈闭环仍需用同一任务对照。

推荐把团队需要的操作表达为独立控制端口，再由 ACP、Pi RPC 或原生服务实现。保留现有任务账本、Run 身份、幂等投递和完成证据，避免重写整个团队系统。

## 3. 主模型、快速模型与命令行启动

先区分三种用途：主控推理模型、执行简单任务的快速成员模型，以及标题/摘要等辅助模型。下面的 ID 是占位符，需要替换为实际模型目录中的 ID；命令未向真实模型发送任务。

### Pi：按进程或会话明确指定模型

启动交互对话并带首条消息，或启动供团队控制的长期 RPC 进程：

```sh
pi --provider openai --model "MAIN_MODEL_ID" --thinking high "分析这个项目"
pi --mode rpc --provider openai --model "MAIN_MODEL_ID" --session-id team-lead
pi --mode rpc --provider openai --model "FAST_MODEL_ID" --session-id team-fast
```

RPC 通过 stdin 的 JSONL 接收消息。可用 `set_model` 切换同一会话的模型，也可为每个成员保留独立上下文。Pi 没有通用的 `--main-model + --fast-model` 内置团队分工参数；统一一次启动快慢成员应由本项目的团队配置或 launcher 实现。`--models` 是模型选择/轮换范围，也不等于自动快慢分工。

### OpenCode：主会话模型与快速子 Agent 分别配置

V2 配置示意：

```json
{
  "agents": {
    "fast": {
      "description": "快速检索和归纳",
      "mode": "subagent",
      "model": "PROVIDER/FAST_MODEL_ID",
      "system": "收集证据并返回简短结论。"
    }
  }
}
```

```sh
opencode run --model "PROVIDER/MAIN_MODEL_ID" --agent build "分析需求，简单检索交给 fast"
```

`--model` 明确选主会话模型。V2 文档指出：只按 ID 切换 primary agent 不会改变会话已保存的模型；子 Agent 未配置 model 时继承父模型。V1 的 `small_model` 会被兼容转换为 `agents.title.model`，主要控制标题生成，不能替代 `agents.fast.model`。

### Claude Code：一条命令定义主模型与快速成员

```sh
claude --model opus --agents '{"fast":{"description":"快速检索和归纳","prompt":"使用只读工具收集证据，返回简短结论。","model":"haiku","tools":["Read","Grep","Glob"]}}' "分析这个项目，按需要使用 fast"
```

该命令定义原生子 Agent；接到 mobile-bridge 的成员调度、审查和账本仍需映射。`--fallback-model` 用于主模型不可用时的回退，不能作为正常快慢任务路由。

### Aider：主推理、编辑、摘要三种模型可一次指定

```sh
aider --architect --model "MAIN_MODEL_ID" --editor-model "FAST_MODEL_ID" --weak-model "FAST_MODEL_ID"
```

`editor-model` 负责把 architect 方案转成编辑，`weak-model` 负责提交信息和历史摘要；两者都不是通用团队 Worker。

## 4. 主控如何更细致地指挥成员

Pi v1.6.0 的原生 RPC 已明确区分这些操作：

| 操作 | 主控获得的能力 | 必须保留的语义 |
| --- | --- | --- |
| `prompt` | 发起任务 | 成功响应表示已处理或接收，不代表完成 |
| `steer` | 成员执行中补充或纠正方向 | 当前 assistant turn 的工具调用完成后、下次模型调用前投递；不会撤销正在执行的工具 |
| `follow_up` | 安排成员结束后的下一步 | 等没有更多工具调用或 steering 时才投递 |
| `clear_queue`＋`abort` | 清掉后续工作并停止当前操作 | 只 abort 时，仍在队列里的消息可能继续执行 |
| `set_model`＋`get_state` | 明确切换模型并核对状态 | 优先在空闲边界切换；不把设置成功解释为在途模型请求已换模型 |
| `set_thinking_level` | 调整支持的思考档位 | 以所选模型实际支持的档位为准 |
| `get_messages` / `fork` | 续读结果、复用或分支上下文 | 分支会话不等于文件系统回滚 |

运行中指挥示例：

```json
{"id":"steer-1","type":"steer","message":"先停止扩大范围，只完成认证模块；其他问题记录下来。"}
{"id":"next-1","type":"follow_up","message":"当前任务结束后，检查相关测试并给出证据。"}
{"id":"model-1","type":"set_model","provider":"openai","modelId":"MAIN_MODEL_ID"}
```

更细的权限与工具控制需要 Pi 扩展：`tool_call` handler 可以阻止调用，`pi.setActiveTools()` 可以改变可用工具，`pi.registerTool()` 可以把现有团队操作变成主控可直接调用的工具。它们不是默认 RPC 的同名命令。

例如，主控可直接调用“读取成员状态”“派发任务”“补充当前任务”“切换成员模型”，得到结构化回执后继续推理。这样能减少依赖模型在最终正文里输出动作信封。工具执行仍交给团队服务校验成员、任务、权限和版本。

启动参数的工具列表不能冒充 OS 沙箱；限制文件写入范围需要执行器或工具权限层实现。Pi v1.6.0 的 MCP 工具默认可以保留在普通 `--tools` 选择之外，严格的只读成员应同时处理 MCP 授权；不需要 MCP 时可显式 `--no-mcp`。

Pi 的 `agent_end` 后还可能发生重试、压缩恢复或后续任务，等待运行稳定应使用 `agent_settled`，再检查是否成功、失败或中止。网关不得仅凭 idle 或接收响应结算任务。

OpenCode V2 的 ACP 文档明确每会话只能同时运行一个 `session/prompt`，第二个会报错；公开 ACP 文档没有给出与 Pi `steer` 等价的指挥入口。原生 API 提供消息控制与收件箱、中断、权限及事件，因此深度控制应核对原生接口，不能用重发 prompt 模拟 steer。

## 5. 建议接入顺序与验收

建议先做一个 Lead、一个快速成员、一个独立 Reviewer 的小范围接入，再决定是否迁移整个团队：

1. 让团队控制端口摆脱对 ACP Manager 的直接依赖；先抽取实际要用的会话创建、状态、投递、steer、停止和模型设置操作，不增加通用插件框架。
2. 增加 Pi RPC 驱动，并提供少量扩展工具，把既有团队服务的派发、状态与干预操作交给 Lead。沿用现有 Run 与提交账本。
3. 配置成员的 provider、model、thinking、工具策略和工作目录；把每次 Run 使用的有效配置记下来。快速成员处理检索、日志归纳和明确的小改动，主模型处理拆解、复杂修复和最终判断。
4. 对缺少某项能力的驱动明确拒绝该操作。OpenCode 可先继续 ACP，再逐项接原生服务；原生 Codex app-server 作为后续独立驱动评估。
5. 在相同任务、同样模型和相同权限下，对照当前 ACP 通路与新驱动，分别观察质量、延迟和成本。不要把换运行时与换模型的效果混在一起。

实际验收至少包含：首条消息前模型设置读回；执行中 steer 不误投旧任务；follow-up 顺序正确；停止后没有遗留任务继续；权限限制真实生效；重启后会话和任务可恢复；错误/限流不被当成完成；主控能依据成员结果继续安排任务。

模型成本和速度需要实际调用测量。快速模型必须支持该成员所需的工具调用；任务标签或“fast”名称不能证明工具能力和响应速度。

本次采取先检查控制边界、再推荐最小适配的方式：角色与策略留在团队服务，运行时负责原生执行，避免重复实现任务账本与模型内部循环，符合 KISS、DRY 与职责分离。

## 6. 来源与验证范围

Pi 官方最新 release 查询结果为 [v1.6.0](https://github.com/earendil-works/pi/releases/tag/v1.6.0)，发布于 2026-11-20。RPC、命令参考与 CLI 文档已从该 tag 读取；本机未安装 Pi。

- Pi：[CLI](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/cli.md)、[RPC 协议](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc.md)、[RPC 命令](https://github.com/earendil-works/pi/blob/v1.6.0/packages/coding-agent/docs/rpc-commands.md)、[扩展](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md)、[模型连接](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/models.md)。扩展与模型说明使用调研时 main 文档，接入时需按安装版本复核。
- OpenCode：[V2 Agents](https://opencode.ai/v2/docs/agents)、[模型](https://opencode.ai/v2/docs/models)、[V1 迁移](https://opencode.ai/v2/docs/migrate-v1)、[ACP](https://opencode.ai/v2/docs/cli/acp)、[API](https://opencode.ai/v2/docs/server)、[SDK](https://opencode.ai/v2/docs/sdk)。
- Claude：[CLI](https://code.claude.com/docs/en/cli-reference)、[Agent SDK](https://platform.claude.com/docs/en/agent-sdk/overview)、[子 Agent](https://platform.claude.com/docs/en/agent-sdk/subagents)。
- Codex：[app-server](https://developers.openai.com/codex/app-server/)。
- Aider：[选项](https://aider.chat/docs/config/options.html)、[architect/editor 模式](https://aider.chat/docs/usage/modes.html)。

本机只读核验：OpenCode v2.3.2 的 `run --help` 确认支持 `--model provider/model#variant`、`--agent`、`--session`、`--format json`；Claude Code 2.1.322 的帮助确认 model/agents/effort/tools 与 stream-json 选项；Codex CLI 0.171.0 的帮助确认 model/profile 选项。

当前团队源码通过 CodeGraph 阅读。开始调研时 HEAD 为 `4398c4cf`，工作区干净；过程中出现其他 Codex 相关并行改动，本次提交仅包含此文档。没有安装新 Agent、启动真实模型对话、修改团队运行时或重启网关；官方能力与本机 help 核验不等于多模型联调已通过。
