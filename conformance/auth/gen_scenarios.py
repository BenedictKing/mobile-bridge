#!/usr/bin/env python3
"""Generate auth pure-function scenarios for the Go/Python parity check."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'


def ips(count):
    return [f'10.0.{i // 256}.{i % 256}' for i in range(count)]


SCENARIOS = {}

SCENARIOS['01_session_hours'] = [
    {'op': 'session_hours', 'value': 12},
    {'op': 'session_hours', 'value': 0},
    {'op': 'session_hours', 'value': 87600},
    {'op': 'session_hours', 'value': -1},
    {'op': 'session_hours', 'value': 87601},
    {'op': 'session_hours', 'value': 3.5},
    {'op': 'session_hours', 'value': 3.0},
    {'op': 'session_hours', 'value': True},
    {'op': 'session_hours', 'value': '12'},
]

SCENARIOS['02_canonical_ip'] = [
    {'op': 'canonical_ip', 'value': v} for v in [
        '127.0.0.1', '1.2.3.4', '0.0.0.0', '255.255.255.255',
        '::1', '::', '2001:db8::1', '2001:0db8:0000:0000:0000:ff00:0042:8329',
        '2001:DB8::FF00:42:8329', '0:0:0:0:0:0:0:1', 'fd00::',
        '::ffff:192.168.1.1', '::ffff:0.0.0.0', '::0.0.0.0', '64:ff9b::1.2.3.4',
        'fe80::1', '2001:db8:0:1:1:1:1:1',
        '01.2.3.4', '256.1.1.1', '1.2.3', 'localhost', '', '1.2.3.4.5',
        'fe80::1%eth0', '::ffff:1.2.3.4 ',
    ]
]

SCENARIOS['03_access_policy'] = [
    {'op': 'access_policy', 'value': {}},
    {'op': 'access_policy', 'value': {'allowlistEnabled': False}},
    {'op': 'access_policy', 'value': {'allowlistEnabled': True}},
    {'op': 'access_policy', 'value': {'allowlistEnabled': 'yes'}},
    {'op': 'access_policy', 'value': {'allowlistEnabled': True, 'allowlist': ['10.0.0.1']}},
    {'op': 'access_policy', 'value': {
        'allowlist': [' 10.0.0.2 ', '10.0.0.1', '10.0.0.2', '::FFFF:10.0.0.3', '::1', '10.0.0.1'],
        'blocklist': ['2001:db8::0001', '2001:db8::1'],
        'trustedProxies': []}},
    {'op': 'access_policy', 'value': {'allowlist': ips(257)}},
    {'op': 'access_policy', 'value': {'allowlist': [42]}},
    {'op': 'access_policy', 'value': {'allowlist': ['10.0.0.%1']}},
    {'op': 'access_policy', 'value': {'blocklist': 'not-a-list'}},
    {'op': 'access_policy', 'value': None},
]

SCENARIOS['04_password_and_sessions'] = [
    {'op': 'password_record', 'password': 'correct password test',
     'salt_hex': '00112233445566778899aabbccddeeff'},
    {'op': 'password_record', 'password': '密码 🚀 with 中文',
     'salt_hex': 'ffeeddccbbaa99887766554433221100'},
    {'op': 'password_record', 'password': 'x' * 1000, 'salt_hex': '00' * 16},
    {'op': 'key', 'token': 'sample-token-1'},
    {'op': 'key', 'token': ''},
    {'op': 'valid', 'session': {'expires': 0}, 'now': 1000000},
    {'op': 'valid', 'session': {'expires': 1000001.5}, 'now': 1000000},
    {'op': 'valid', 'session': {'expires': 999999}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'persistent': False, 'expires': 2000000}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'expires': 0}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'expires': 1000000.2}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'expires': 1000000 + 3 * 86400 + 0.4}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'expires': 1000000 + 500 * 86400}, 'now': 1000000},
    {'op': 'cookie_age', 'session': {'expires': 999999}, 'now': 1000000},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
