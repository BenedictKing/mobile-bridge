# Team 主控与执行会话隔离方案

> 状态：已实现并通过相关回归测试；运行中的网关仍需由用户重启后加载后端与静态资源。

## 问题

当前 `TeamMember.SessionID` 同时承担 Lead 对话入口与 Lead 被委派任务的执行目标。`ExecutionThread` 虽有 Run/成员维度，但首派时 `ProviderSessionID` 仍绑定 `TeamMember.SessionID`，所以线程 ID 的分离没有隔离实际上下文。执行内容、桌面端活动和控制对话因此混在一起。

## 目标边界

- `TeamMember.SessionID` 始终指向用户可见的成员/Lead 控制聊天。用户输入、Lead 对话规划、回答澄清和最终汇总继续走该会话。
- Lead 在 `delegate` 中给自己分派的实质工作使用独立执行会话。同一 Run 的同一 Lead 执行线程复用该新会话；不同 Run 不复用它。
- 新执行会话使用同一 Agent、项目目录和可继承的模型/思考设置。创建请求使用确定性幂等键，并把 provider session ID 写入 `ExecutionThread` 后才入队。
- 普通成员继续使用其现有专用会话。显式计划中的 Lead 执行任务同样与 Lead 控制聊天隔离。
- 首个 autoPlan/修订规划输入和 `Reduction`/`summarize` 收尾任务回到 Lead 控制聊天，确保用户能在原 Team 对话中收到结果。
- 子 Agent 只读活动展示它的可用实时记录；完成后可读的 ZCode 结果摘要仍可查看。无法从磁盘取得完整 transcript 时准确标注可用内容，不伪称已有 transcript。

## 数据与恢复

为 Task 持久化明确的 Lead 控制任务标记，避免用任务 ID 前缀推断角色。计划任务落账与 journal 重建都保留该标记。执行会话创建由 `internal/httpd` 端口实现，使用共享创建账本保存 Team/Run/成员、Agent、cwd、标题和会话 ID。`ExecutionThread.ProviderSessionID` 是队列派发和结果归属的唯一会话来源。

崩溃发生在 provider 创建后、Run 绑定前时，重启按相同创建键找回已有会话；已绑定后直接从 Run 快照/journal 恢复。创建结果不确定时停靠并展示检查原因，不能盲目再建。已有 Run 没有执行线程绑定时按旧行为继续，以免自动迁移历史任务到新会话。

## 实施阶段

1. **独立回归**：修复 ACP 权限卡 Markdown 渲染、迁移 prepare/confirm 项目标识漂移、ZCode 子 Agent metadata 缓存和完成结果文件读取。每项先补对应失败用例。
2. **控制任务标记**：在 planner、输入路由、action task 与 reduction 生成路径写入 `leadControl` 事实；补 snapshot、journal 重建和 amend 兼容测试。
3. **执行会话端口**：为 Lead Run 执行线程新增幂等 create/find 端口，继承同一 Agent 的会话设置，并保留 TeamMember 控制 SessionID 不变。
4. **派发切换**：首派先解析或创建目标执行会话，再原子记录 `thread_bound` 和 `task_dispatching`；所有后续队列、取消、恢复、结果关联都使用绑定会话。Reduction/控制任务明确绑定 Lead 控制会话。
5. **端到端验收**：用隔离 Team 和真实 ACP 测试用户输入、Lead 自委派执行、结果汇总回控制会话、重启重放、多个 Run 隔离、普通成员不变及审批仍可从原会话处理。

## 验收与回滚

核心断言：Lead 控制 SessionID、Lead 执行 ProviderSessionID、另一个 Run 的执行 ProviderSessionID 三者按设计区分；一次逻辑创建只产生一个 provider session；最终摘要消息只落在控制会话；不存在未绑定或重复派发的任务。运行 `go test ./internal/team ./internal/httpd`、相关 `-race` 测试、ZCode/ACP 集成测试、`node --test tests/*.js` 和 `go test ./...`。

新增字段可选并只增不删，旧快照和 journal 按原逻辑读取。回滚时关闭新执行会话创建只影响新 Run，已绑定执行线程仍继续在原 provider session 上恢复与结算，不改写或删除旧账本。不得执行 `make restart`；加载后端或静态资源时由用户决定何时重启。
