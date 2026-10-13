#!/usr/bin/env python3
"""Generate real timeline_read scenarios (annotated path, stub adapter)."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'


def saved(count=3):
    return {'id': 'chat', 'turns': [
        {'turnId': str(i), 'items': [{'type': 'agentMessage', 'id': str(i),
                                      'text': f'saved {i}'}]} for i in range(count)]}


def live(turn_ids, text='live'):
    return {'id': 'chat', 'cwd': '/工作目录', 'turns': [
        {'turnId': tid, 'items': [{'type': 'agentMessage', 'id': tid, 'text': text}]}
        for tid in turn_ids], 'requests': []}


SCENARIOS = {}
SCENARIOS['01_annotated_read'] = [
    {'op': 'bridge_session', 'steps': [
        {'set_history': saved(3)},
        {'state': live(('1', '2')), 'connected': True},
        {'read_page': {'limit': 1}},
        {'read_page': {'limit': 5, 'before_last': True}},
        {'read_detail': {}},
        {'state': live(('1', '2'), text='live streaming'), 'connected': True},
        {'changed': True},
        {'read_page': {}},
        {'read_changes': {'after': 1, 'epoch_last': True}},
        {'read_detail': {'offset': 3, 'version': 'wrong'}},
    ]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
