#!/usr/bin/env python3
"""Generate submission-ledger scenarios (send/queue/retry/cancel state machine)."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'

ID1 = '11111111-1111-1111-1111-111111111111'
ID2 = '22222222-2222-2222-2222-222222222222'
ID3 = '33333333-3333-3333-3333-333333333333'


def send(text, sid, **extra):
    return {'op': 'send', 'value': {'text': text, 'submission_id': sid, **extra}}


SCENARIOS = {}

SCENARIOS['01_send_and_dispatch'] = [
    {'op': 'session', 'steps': [
        send('   ', ID1),                                    # empty text rejected
        send('你好', ID1),                                   # dispatch ok
        send('重复内容', ID1),                                # duplicate id, different text
        send('你好', ID1),                                   # duplicate id, same content
        {'op': 'meta'},
        {'set_active': True},
        send('忙时直发', ID2),                                # busy send rejected
        send('排队消息', ID2, mode='queue'),                  # queued
        {'op': 'meta'},
        {'set_active': False},
        send('闲时补充', ID2, mode='steer'),                  # steer while idle
        send('排重发', ID2, mode='queue'),
    ]},
]

SCENARIOS['02_failures_and_retry'] = [
    {'op': 'session', 'steps': [
        {'dispatch': {'error': '此 Agent 不支持图片附件'}},
        send('带坏附件', ID1),                               # ValueError -> failed
        {'op': 'meta'},
        {'op': 'queue_action', 'value': {'id': ID1, 'action': 'send'}},
        {'dispatch': {'ok': True}},
        {'op': 'queue_action', 'value': {'id': ID1, 'action': 'send'}},
        {'op': 'meta'},
        {'dispatch': {'ok': True}},
        send('直发成功', ID2),
        {'op': 'mark_failed', 'value': {'id': ID2, 'error': 'Agent 请求失败'}},
        {'op': 'meta'},
        {'op': 'queue_action', 'value': {'id': ID2, 'action': 'send'}},
        {'op': 'meta'},
        {'dispatch': {'timeout': True}},
        send('结果未知', ID3),
        {'op': 'meta'},
        {'op': 'ignore_submission', 'value': {'id': ID3}},
        {'op': 'meta'},
        {'op': 'ignore_submission', 'value': {'id': ID3}},
    ]},
]

SCENARIOS['03_requeue_limit'] = [
    {'op': 'session', 'steps': [
        {'dispatch': {'not_dispatched': '会话尚未连接，请重新打开'}},
        *[_ for _ in []],  # placeholder
    ]},
]

# Build the 10-attempt requeue ladder programmatically.
ladder = [{'dispatch': {'not_dispatched': '会话尚未连接，请重新打开'}},
          send('重试 climbing', ID1),
          {'op': 'meta'}]
for i in range(10):
    ladder.append({'op': 'queue_action', 'value': {'id': ID1, 'action': 'send'}})
    ladder.append({'op': 'meta'})
SCENARIOS['03_requeue_limit'] = [{'op': 'session', 'steps': ladder}]

SCENARIOS['04_queue_edit_delete'] = [
    {'op': 'session', 'steps': [
        send('排队一', ID1, mode='queue'),
        send('排队二', ID2, mode='queue'),
        {'op': 'meta'},
        {'op': 'queue_action', 'value': {'id': ID1, 'action': 'edit', 'text': '改后的排队一'}},
        {'op': 'meta'},
        {'op': 'queue_action', 'value': {'id': ID2, 'action': 'delete'}},
        {'op': 'meta'},
        {'op': 'queue_action', 'value': {'id': ID2, 'action': 'delete'}},
        send('立即发送', ID3, mode='queue'),
        {'op': 'queue_action', 'value': {'id': ID3, 'action': 'send'}},
        {'op': 'meta'},
    ]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
