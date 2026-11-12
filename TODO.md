# TODO

## 团队协作

- [ ] 支持将 Codex 添加为团队成员（记录于 2026-11-12，待实施）。
  - 问题：团队设置的 Agent 下拉框不显示 Codex，无法加入现有团队。
  - 当前限制：`web/team-chat.js` 显式过滤 `codex`；`internal/httpd/teams.go` 的成员校验只接受 ACP 会话；`internal/httpd/teams_adapter.go` 的会话解析与恢复依赖 ACP Manager。
  - 实施范围：将团队会话接口从 ACP 专用接口解耦，分别适配 ACP 与 Codex；补齐 Codex 的成员会话创建、解析、恢复、任务投递、执行状态跟踪、结果收集与取消。
  - 验收：Codex 可添加为成员并与 ACP 成员协作完成任务；未配置或不可用时提供明确提示；现有 ACP 团队流程回归通过。仅解除前端过滤不视为完成。
