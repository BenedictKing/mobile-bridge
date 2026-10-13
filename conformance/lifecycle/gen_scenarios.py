#!/usr/bin/env python3
"""Generate lifecycle control-file scenarios for the Go/Python parity check.

The control record (gateway-control.json / gateway.stop / pairing responses)
is serialized with json.dumps defaults: ", "/": " separators and
ensure_ascii=True, so non-ASCII payloads escape to \\uXXXX.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'

SCENARIOS = {}

SCENARIOS['01_control_record'] = [
    {'op': 'control_record', 'pid': 12345, 'token_hex': 'ab' * 32,
     'pairing_dir': '.pairing-ab12cd34', 'instance_id': None,
     'device': True, 'accounts': True, 'account': False, 'notifications': True},
    {'op': 'control_record', 'pid': 2, 'token_hex': '00' * 32,
     'pairing_dir': '.pairing-zzz', 'instance_id': 'inst-9',
     'device': False, 'accounts': False, 'account': False, 'notifications': False},
]

SCENARIOS['02_dump_ascii'] = [
    {'op': 'dump_ascii', 'value': {'ok': True, 'result': {'list': [], 'n': None}}},
    {'op': 'dump_ascii', 'value': {'ok': False, 'error': '扫码登录操作超时，请重试'}},
    {'op': 'dump_ascii', 'value': {'text': 'quote " backslash \\ emoji 🚀 tab\tnew\nend'}},
    {'op': 'dump_ascii', 'value': {'nested': {'a': [1, 2, {'b': False}], 'c': '中文'}}},
    {'op': 'dump_ascii', 'value': 12345},
    {'op': 'dump_ascii', 'value': 'plain string'},
]

SCENARIOS['03_stop_and_read'] = [
    {'op': 'stop_validate', 'record': {'pid': 12345, 'token': 'ab' * 32,
                                       'pairingDir': '.pairing-x', 'instanceId': None,
                                       'deviceManagement': True, 'accountsManagement': False,
                                       'accountManagement': False, 'notificationManagement': False}},
    {'op': 'stop_validate', 'record': {'pid': 1.5, 'token': 'ab'}},
    {'op': 'stop_validate', 'record': {'pid': '12345', 'token': 'ab'}},
    {'op': 'stop_validate', 'record': {'pid': 12345, 'token': ''}},
    {'op': 'stop_validate', 'record': None},
    {'op': 'stop_validate', 'record': 'text'},
    {'op': 'read_record', 'text': '{"pid": 1, "token": "ab", "x": {"y": [1, 2]}}'},
    {'op': 'read_record', 'text': 'not json at all'},
    {'op': 'read_record', 'text': ''},
    {'op': 'read_record', 'text': '{"pid": 1, "pid": 2}'},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
