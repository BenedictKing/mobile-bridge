## [Unreleased]

### Changed / Fixed

- Live chat updates now stream over incremental SSE (ready/changes/reset frames with `epoch.sequence` cursor resume); the web client uses one transport module for SSE and long-polling, with capped reconnect backoff and definitive logout on auth expiry. Legacy full-state SSE and the polling endpoints stay byte-compatible.
- Queued messages can now be sent immediately when idle, edited, or deleted; automatic dispatch uses the latest edited content.
- Updated queue API documentation and added regression coverage for queue actions and edit/dispatch races.
- Cross-agent migration can create a target session for an unmatched local project and exposes target capability metadata in timeline responses.
- Upload storage accepts path-safe Agent-native session identifiers, and the mobile copy dialog layout keeps long content scrollable.
- English UI now translates native list, send-mode, and work-mode options; migration documentation matches local and remote directory behavior.
- Child-agent transcripts now preserve nested ACP output, Codex child rollouts, agent identity, status, and command output; queueing can be disabled per chat.
- ACP agents expose native permission modes through the phone UI; Codex retains desktop-owned permission policy and single-request approvals.
- Affected files: `bridge/service.py`, `bridge/uploads.py`, `web/`, and related tests.
