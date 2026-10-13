## [Unreleased]

### Added

- Kimi sessions now support Skill selection in the phone UI: skills advertised by the agent as ACP slash commands appear in the picker, and the chosen skill is sent as a leading `/<command>` in the message (one skill per message).
- Optional Jev (TypeSafe AI) advisory triage for manual permission confirmations: each approval card gets a risk level, reversibility, confidence, and a starred suggested option, and the riskiest pending confirmations sort first. Jev only annotates and ranks — approvals still require an explicit tap, and unconfigured gateways keep the deterministic local labels. The API key, endpoint, and model are editable in the web settings dialog (persisted to `jev.json`, masked readback, live effect without a gateway restart); env vars remain launch-time defaults and an explicit key clear disables Jev even when they are set.
- ACP agents that archive sub-agents as independent on-disk sessions are now discoverable: kimi session directories (`state.json` + per-agent wire logs), zcode (`acp-lazy-sessions.json` mapping + `cli/agents` transcripts), and claude (`subagents/` sidechain files) all feed the same timeline activity rows, read-only with size caps and mtime caching.
- Periodic session discovery (`bridge/discovery.py`): a background loop re-reads every agent's native session list every 30 seconds while at least one client is polling `/api/activity`, versions identity/title/project changes into a `listRevision`, and ships that revision with activity rows so the phone list reloads on its own when a chat is started outside the web UI (terminal, desktop App). ACP `session/list` caches are force-refreshed by the loop; with nobody watching for two minutes the loop pauses instead of spawning agent processes. SSH remote lists stay request-driven.

### Changed / Fixed

- ACP prompt timeouts are now idle-based: active `session/update` traffic or a pending permission request no longer counts toward the limit, and an idle-expired turn is cancelled agent-side (`session/cancel` plus cancelled answers to pending approvals) so later messages are not rejected by an orphaned turn.
- Live chat updates now stream over incremental SSE (ready/changes/reset frames with `epoch.sequence` cursor resume); the web client uses one transport module for SSE and long-polling, with capped reconnect backoff and definitive logout on auth expiry. Legacy full-state SSE and the polling endpoints stay byte-compatible.
- Queued messages can now be sent immediately when idle, edited, or deleted; automatic dispatch uses the latest edited content.
- Plan (todo) panels now show completion state: ACP plan snapshots fold into one live panel per turn, entries render as a checklist with ✓/strikethrough for completed, ▸ for in-progress and ○ for pending, and the collapsed heading shows a `done/total` count (plus ✓ when everything is done) so a finished plan is recognizable at a glance.
- Updated queue API documentation and added regression coverage for queue actions and edit/dispatch races.
- Cross-agent migration can create a target session for an unmatched local project and exposes target capability metadata in timeline responses.
- Upload storage accepts path-safe Agent-native session identifiers, and the mobile copy dialog layout keeps long content scrollable.
- English UI now translates native list, send-mode, and work-mode options; migration documentation matches local and remote directory behavior.
- Child-agent transcripts now preserve nested ACP output, Codex child rollouts, agent identity, status, and command output.
- The per-chat queue on/off toggle is removed: queueing is always available, and the composer's send-mode selector is the single entry point.
- Fixed duplicate zcode chat entries: the lazy placeholder session is now withdrawn from the chat list once its materialized backend `sess_*` session is listed, and `sess_*` URLs reopen after a page refresh.
- DeepSeek Harness exposes its native permission modes through the phone UI; other ACP `mode` options remain untouched when they represent collaboration modes, and Codex retains desktop-owned permission policy.
- Affected files: `bridge/jev.py`, `bridge/agents/native_subagents.py`, `bridge/service.py`, `bridge/discovery.py`, `bridge/agents/{kimi,zcode,claude}.py`, `web/`, and related tests.
