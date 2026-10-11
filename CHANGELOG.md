## [Unreleased]

### Changed / Fixed

- Queued messages can now be sent immediately when idle, edited, or deleted; automatic dispatch uses the latest edited content.
- Updated queue API documentation and added regression coverage for queue actions and edit/dispatch races.
- Cross-agent migration can create a target session for an unmatched local project and exposes target capability metadata in timeline responses.
- Upload storage accepts path-safe Agent-native session identifiers, and the mobile copy dialog layout keeps long content scrollable.
- English UI now translates native list, send-mode, and work-mode options; migration documentation matches local and remote directory behavior.
- Affected files: `bridge/service.py`, `bridge/uploads.py`, `web/`, and related tests.
