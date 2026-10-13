#!/usr/bin/env python3
"""Generate rollout-history scenarios for the Go/Python parity check.

The oracle patches SessionStore.get and feeds the real history() parser plus
_recent_lines' backward chunked reader.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'


def ev(payload_type, **extra):
    return json.dumps({'type': 'event_msg', 'payload': {'type': payload_type, **extra}},
                      ensure_ascii=False)


def ri(payload, **record):
    return json.dumps({'type': 'response_item', 'payload': payload, **record},
                      ensure_ascii=False)


META = {'name': '会话名', 'cwd': '/工作目录', 'model': 'gpt-x', 'model_provider': 'oai',
        'thread_source': None, 'source': 'vscode'}

SCENARIOS = {}

SCENARIOS['01_history_parse'] = [
    {'op': 'history', 'meta': META, 'turn_limit': None, 'lines': [
        '不是 JSON 的垃圾行',
        ev('task_started', turn_id='t1'),
        ri({'type': 'message', 'role': 'user',
            'content': [{'type': 'text', 'text': '你好'}, {'type': 'other'}, '纯字符串']}),
        ri({'type': 'message', 'role': 'assistant', 'phase': 'final',
            'content': [{'text': '第一段'}, {}, {'text': '第二段'}]}),
        ri({'type': 'message', 'role': 'system', 'content': []}),
        ev('item_completed', item={'type': 'imageView', 'id': 'im1', 'path': '/x 贴图.png'}),
        ev('item_completed', item={'type': 'reasoning', 'summary': []}),
        ev('task_complete'),
        ev('task_started', turn_id='t2'),
        ri({'type': 'agent_message', 'id': 'am1', 'text': '智能体消息'}),
        ri({'type': 'agentMessage'}),
        ri({'type': 'function_call', 'call_id': 'c1', 'name': 'npm',
            'arguments': '{}', 'id': '覆盖前置 id'}),
        ri({'type': 'custom_tool_call_output', 'output': 'done'}),
        ev('turn_aborted'),
        ev('task_complete'),
    ]},
    {'op': 'history', 'meta': {**META, 'name': None, 'title': '标题回退'}, 'turn_limit': None,
     'lines': [
         ri({'type': 'message', 'role': 'user', 'content': '无回合开场的字符串内容'}),
         ev('task_complete'),
     ]},
    {'op': 'history', 'meta': {**META, 'name': None, 'title': None}, 'turn_limit': None,
     'lines': []},
]

# Backward chunked reading: > 64 KiB with markers spread across boundaries.
BIG_LINES = []
for i in range(400):
    if i in (0, 150, 320):
        BIG_LINES.append(ev('task_started', turn_id=f'turn-{i}'))
    else:
        BIG_LINES.append(ri({'type': 'message', 'role': 'assistant',
                             'content': [{'text': f'填充行 {i} ' + 'x' * 150}]}))

SCENARIOS['02_recent_lines'] = [
    {'op': 'history', 'meta': META, 'turn_limit': None, 'lines': BIG_LINES},
    {'op': 'history', 'meta': META, 'turn_limit': 2, 'lines': BIG_LINES},
    {'op': 'history', 'meta': META, 'turn_limit': 1, 'lines': BIG_LINES},
    {'op': 'history', 'meta': META, 'turn_limit': 99, 'lines': BIG_LINES},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
