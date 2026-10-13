# Go 迁移测试资产审计（P0 交付物）

> 生成方式：`python3 conformance/classify_tests.py`（静态启发式：import 与符号扫描），本文件为 2026-10-13 快照。**总量可靠，单行可能有偏差**——B 类含 mock 掉 `GatewayServer` 的用例（下表已标注），人工复核后可把误报行移入 A。

**总计 742 个测试方法 / 47 个文件**。文件级分布：A 纯逻辑 28、B HTTP 9、C IPC 7、BC 3。
方法级（BC 双计）：A≈340、B≈274、C≈250。

**对工作量的含义**：

- 真正能直接进差分套件的 B 类比数字显示的少——部分 B 是 mock HTTP（标注 `mock`），实际驱动 socket 的约 9-12 个文件。这与方案 2.1 节「12 个文件真正建 HTTP 连接」的实测一致。
- A 类 340+ 个测试断言 Python 内部 API，**不能直接验收 Go**，但它们是移植时的「规格来源」：每个 A 测试要么改写为 Go 单测，要么升格为差分场景。这就是 30-40% 测试改造预算的出处。
- C 类（IPC/私有协议）集中在 `test_bridge.py`、`test_platform.py`、`test_acp_transport.py`、`test_desktop.py`，对拍基建（帧 fixture）一次建好可全部复用。

| 文件 | 类 | 测试 | 迁移阶段 | 覆盖模块 |
| --- | --- | --- | --- | --- |
| `test_access.py` | B *(mock)* | 18 | P2 | access, desktop, notifications, ssh_tunnel, tunnel |
| `test_account.py` | A | 16 | P2 | account, lifecycle |
| `test_account_import.py` | A | 9 | P6 | account_models, accounts |
| `test_account_info.py` | A | 7 | P6 | account_info, accounts |
| `test_accounts.py` | C | 21 | P4 | _check_provider, account, accounts, adapter, codex, desktop_app, host, ipc, live, service, submissions |
| `test_accounts_http.py` | A | 4 | P3 | accounts |
| `test_acp_adapter.py` | B | 55 | P2 | _send_queued, acp, acp_adapter, adapter, agents, auth, base, catalog, claude, close, create_chat, dsh, httpd, interrupt, kimi, list, listed, live, mcode, notifications, permission_settings, respond, send, service, settings, submissions, view, zcode |
| `test_acp_transport.py` | A | 10 | P4 | acp |
| `test_address_notifications.py` | B *(mock)* | 13 | P6 | address_notifications, desktop, notifications |
| `test_api_catalog.py` | A | 18 | P6 | catalog, remote |
| `test_approval_receipts.py` | A | 15 | P6 | approvals |
| `test_auth.py` | A | 11 | P2 | auth, desktop, lifecycle, pairing |
| `test_bridge.py` | BC | 85 | P2 | _attach, _call, _event, _expire_inputs, _goal_runtime_available, _refresh_async, _send_queued, account, accounts, activate, adapter, approvals, auth, cancel_queued, catalog, catalog_reader, close, closed, create_chat, defer_input, edit_queued, files, for_host, host, host_errors, hosts, httpd, ignore_submission, inputs, ipc, ledger_path, list, live, model, queue_action, remote, respond, send, service, session, settings, store, submissions, submit_input, timeline_read, view, waiting_input_requests |
| `test_claude_adapter.py` | A | 16 | P4 | acp, catalog, claude, close, interrupt, list, send, service, session, settings, view |
| `test_connection_setup.py` | A | 17 | P6 | address_notifications, desktop, named_tunnel, notifications |
| `test_create.py` | C | 11 | P2 | adapter, close, create, create_chat, creations_path, hosts, ipc, live, remote, rename, service, store |
| `test_desktop.py` | A | 14 | P2 | desktop, notifications |
| `test_discovery.py` | B | 8 | P5 | base, close, discovery, httpd, listed, service |
| `test_dsh_adapter.py` | A | 20 | P4 | acp, catalog, close, dsh, interrupt, list, permission_settings, send, service, session, set_permission_mode, settings, view |
| `test_fast_mode.py` | C | 10 | P2 | catalog, catalog_reader, host, ipc, model, session, settings, view |
| `test_goal_controls.py` | C | 15 | P2 | _reconcile_goal_commands, _save_goal_commands, _send_queued, cancel_goal, edit_goal, goal, goal_commands, goal_commands_path, ipc, live, send, set_goal_status, store, submissions |
| `test_goal_rpc.py` | A | 8 | P5 | catalog, goal |
| `test_jev.py` | B | 27 | P2 | _annotate_assessments, assessor, auth, close, data_dir, httpd, jev, kimi, respond, send, service, session, view |
| `test_linux.py` | A | 8 | P6 | catalog, create |
| `test_login_security.py` | B | 10 | P2 | auth, httpd, notifications, security_notifications |
| `test_mcode_adapter.py` | B *(mock)* | 9 | P4 | acp, cancel_goal, close, edit_goal, interrupt, mcode, send, service, session, set_goal_status, settings, view |
| `test_message_actions.py` | C | 13 | P2 | _fork_origin, close, create, host, hosts, ipc, live, message_action, message_actions, remote, service, store |
| `test_migrate.py` | C | 17 | P4 | acp_adapter, base, close, ipc, kimi, service |
| `test_modes.py` | C | 41 | P1 | _goal_activation_ids, _goal_call, _goal_command, _goal_runtime_available, _open_desktop, _refresh_goal_snapshot, _send_goal_activation, _send_queued, cancel_goal, goal, goal_commands, goal_transport, host, ipc, live, model, owner_goal, respond, send, service, session, set_goal_status, submissions, timeline, upload, view |
| `test_native_subagents.py` | A | 10 | P4 | close, kimi, native_subagents, service, view |
| `test_network.py` | B | 5 | P2 | desktop, httpd, lifecycle |
| `test_notification_activity.py` | A | 11 | P2 | _disconnected, notification_candidates, notification_dirty, notification_session, notification_updates, notifications, release_notification_session, service, store |
| `test_notifications.py` | A | 32 | P6 | notifications, service |
| `test_pairing.py` | B | 7 | P2 | access, auth, desktop, httpd, lifecycle, pairing |
| `test_platform.py` | BC | 14 | P2 | catalog, ipc, lifecycle, remote, store, transport |
| `test_store.py` | A | 9 | P1 | files, model, store, timeline |
| `test_task_inputs.py` | A | 9 | P6 | inputs |
| `test_timeline.py` | A | 10 | P1 | model, service, timeline |
| `test_tls.py` | A | 3 | P3 | access, notifications |
| `test_tunnel_recovery.py` | A | 6 | P6 | named_tunnel, tunnel |
| `test_updater.py` | A | 25 | P6 | — |
| `test_uploads_activity.py` | BC | 23 | P2 | _annotate_upload_attachments, _disconnected, _send_queued, activity, artifact, desktop_image_preview, files, host, ipc, list, live, remote, send, service, session, store, submissions, timeline_read, upload, upload_preview, upload_thumb, uploads, view |
| `test_v140_regressions.py` | A | 6 | P6 | catalog, remote |
| `test_watch_management.py` | A | 5 | P2 | desktop, lifecycle, notifications, service |
| `test_worktree.py` | A | 7 | P6 | worktree |
| `test_zcode_adapter.py` | A | 18 | P4 | acp, catalog, close, interrupt, list, send, service, session, settings, view, zcode |
| `test_zcode_creds.py` | A | 16 | P6 | agents |

阶段汇总（文件数 / 测试方法数）：

- **P1**：3 个文件，60 个测试
- **P2**：18 个文件，350 个测试
- **P3**：2 个文件，7 个测试
- **P4**：8 个文件，121 个测试
- **P5**：2 个文件，16 个测试
- **P6**：14 个文件，188 个测试

## P1 首批验收锚点（timeline）

`test_timeline.py`(10)、`timeline.test.js`（前端投影消费方）、`test_acp_adapter.py` 中 view 构造相关用例。差分对拍场景已固化为 `conformance/timeline/scenarios/`，golden 由 Python oracle 生成。
