#!/usr/bin/env python3
"""Generate LiveSession→Timeline composition scenarios (timeline_read core
without bridge annotations). Cursor references are relative: before_last =
most recent page's cursor, before_prev = the one before it."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'


def saved(count=3):
    return {'id': 'chat', 'turns': [
        {'turnId': str(i), 'items': [{'type': 'agentMessage', 'id': str(i),
                                      'text': f'saved {i}'}]} for i in range(count)]}


def live(turn_ids, text='live', pagination=False):
    state = {'id': 'chat', 'turns': [
        {'turnId': tid, 'items': [{'type': 'agentMessage', 'id': tid, 'text': text}]}
        for tid in turn_ids], 'requests': []}
    if pagination:
        state['turnsPagination'] = {'hasLoadedOldest': False}
    return state


SCENARIOS = {}
SCENARIOS['01_composition'] = [
    {'op': 'session', 'steps': [
        {'set_history': saved(3)},
        {'state': live(('2',), pagination=True), 'connected': True},
        {'project': True},
        {'page': {'limit': 2}},
        {'page': {'limit': 2, 'before_last': True}},
        {'state': live(('2',), text='live streaming'), 'connected': True},
        {'changed': True},
        {'project': True},
        {'page': {}},
        {'changes': {'after': 1, 'epoch_last': True}},
        {'project': True},
        {'page': {}},
    ]},
    {'op': 'session', 'steps': [
        {'state': live(('a', 'b'), text='第一轮'), 'connected': True},
        {'project': True},
        {'page': {}},
        {'state': live(('b',), text='收缩即替换'), 'connected': True},
        {'changed': True},
        {'project': True},
        {'page': {}},
        {'page': {'before_prev': True}},
    ]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
