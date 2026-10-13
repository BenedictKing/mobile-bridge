#!/usr/bin/env python3
"""Generate IPC frame scenarios for the Go/Python parity check.

Each step produces at most one outgoing desktop-IPC frame (length-prefixed
compact JSON). requestIds come from a deterministic uuid counter shared with
the Go test, so frame bytes are fully reproducible.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'

SCENARIOS = {}

SCENARIOS['01_requests'] = [
    {'op': 'request', 'method': 'thread-owner-discovery',
     'params': {'hostId': 'local', 'conversationId': 'sess-1'}, 'timeout': 6},
    {'op': 'request', 'method': 'thread-follower-start-turn',
     'params': {'conversationId': 'sess-1', 'turn': {'userText': '你好 <b>&amp;'}},
     'host': 'remote-host', 'timeout': 15},
    {'op': 'request', 'method': 'thread-follower-interrupt-turn',
     'params': {'conversationId': 'sess-1'}, 'target': 'owner-client-7', 'timeout': 2.75},
    {'op': 'request', 'method': 'initialize', 'params': {'clientType': 'codex-mobile-bridge'},
     'timeout': 5},
    {'op': 'request', 'method': 'thread-follower-submit-user-input',
     'params': {'conversationId': '转义·会话', 'responses': ['是', '否']}, 'timeout': 0.5},
    {'op': 'request', 'method': 'no-such-method', 'params': {}, 'timeout': 5},
    {'op': 'request', 'method': 'thread-follower-load-complete-history',
     'params': {}, 'host': 'second', 'timeout': 15},
]

SCENARIOS['02_broadcast'] = [
    {'op': 'follow', 'session': 'sess-1', 'owner': 'owner-client-7', 'enabled': True},
    {'op': 'follow', 'session': 'sess-2', 'enabled': False, 'host': 'remote-host'},
    {'op': 'incoming',
     'message': {'type': 'client-discovery-request', 'requestId': 'desktop-req-1'}},
    {'op': 'incoming',
     'message': {'type': 'broadcast', 'method': 'agent-update', 'version': 3,
                 'params': {'text': '事件 ✓'}, 'targetClientIds': ['client-abc']}},
    {'op': 'incoming',
     'message': {'type': 'broadcast', 'method': 'agent-update', 'version': 3,
                 'params': {'text': '别人的事件'}, 'targetClientIds': ['other']}},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
