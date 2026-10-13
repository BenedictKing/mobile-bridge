#!/usr/bin/env python3
"""Generate LiveSession view-merge scenarios (service.py first slice)."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'


def saved(count=3):
    return {'id': 'chat', 'turns': [
        {'turnId': str(i), 'items': [{'type': 'agentMessage', 'id': str(i),
                                      'text': f'saved {i}'}]} for i in range(count)]}


def live(turn_ids=('2',), text='live'):
    return {'id': 'chat', 'turns': [
        {'turnId': tid, 'items': [{'type': 'agentMessage', 'id': tid, 'text': text}]}
        for tid in turn_ids]}


SCENARIOS = {}

SCENARIOS['01_saved_prefix_merge'] = [
    {'op': 'session', 'steps': [
        {'set_history': saved(3)},
        {'state': {'id': 'chat', 'turns': [
            {'turnId': '2', 'items': [{'type': 'agentMessage', 'id': '2', 'text': 'live'}]}],
            'turnsPagination': {'hasLoadedOldest': False}, 'requests': []},
         'connected': True},
        {'view': True},
        {'set': 'pagination_complete'},
        {'view': True},
    ]},
    {'op': 'session', 'steps': [
        {'set_history': {'id': 'chat', 'turns': [{'turnId': 'old', 'items': []}]}},
        {'state': {'id': 'chat', 'turns': [], 'turnsPagination': {'hasLoadedOldest': False}}},
        {'view': True},
    ]},
    {'op': 'session', 'steps': [
        {'set_history': saved(4)},
        {'state': live(('9',), text='全新回合'), 'connected': True},
        {'view': True},
    ]},
    {'op': 'session', 'steps': [
        {'set_history': saved(3)},
        {'state': live(('1', '2'), text='重叠两个'), 'connected': True},
        {'view': True},
    ]},
]

SCENARIOS['02_state_and_flags'] = [
    {'op': 'session', 'steps': [
        {'view': True},
        {'set_history': saved(2)},
        {'view': True},
        {'state': live(), 'connected': True},
        {'changed': True},
        {'error': 'Agent 进程已退出，请重新连接'},
        {'view': True},
        {'error': None},
        {'connecting': True},
        {'activating': True},
        {'changed': True},
        {'view': True},
        {'connecting': False},
        {'activating': False},
        {'set': 'pagination_complete'},
        {'view': True},
    ]},
    {'op': 'session', 'steps': [
        {'set_history': {'id': 'chat', 'turns': [],
                         'turnsPagination': {'hasLoadedOldest': True}}},
        {'state': live(), 'connected': True},
        {'view': True},
    ]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
