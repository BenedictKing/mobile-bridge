#!/usr/bin/env python3
"""Generate notification pure-layer scenarios (URL validation, settings
save/preservation, destination/channel derivation)."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'

SCENARIOS = {}

SCENARIOS['01_valid_url'] = [
    {'op': 'valid_url', 'value': v, 'allow_path': ap} for v, ap in [
        ('https://ntfy.example.com', False), ('https://ntfy.example.com/', False),
        ('https://ntfy.example.com/topic', False), ('https://ntfy.example.com/topic', True),
        ('http://localhost:8080', False), ('http://127.0.0.1:8080/base', True),
        ('http://[::1]:8080', False), ('http://insecure.example.com', False),
        ('ftp://x.example.com', False), ('https://', False),
        ('https://u:p@x.example.com', False), ('https://x.example.com?q=1', False),
        ('https://x.example.com#f', False), ('https://中文.example.com/', False),
        ('  https://ntfy.example.com  ', False),
    ]
]

BASE = {'enabled': True, 'topic': 'my-topic', 'token': 'tk_abc',
        'barkEnabled': False, 'barkServer': 'https://api.day.app', 'barkKey': '',
        'clickBase': '', 'includeTitle': True}

SCENARIOS['02_settings'] = [
    {'op': 'settings_read', 'file': None},
    {'op': 'settings_save', 'value': BASE},
    {'op': 'settings_read', 'file': 'after'},
    {'op': 'settings_save', 'value': {**BASE, 'token': ''}},
    {'op': 'settings_save', 'value': {**BASE, 'token': '', 'server': 'https://ntfy.cn.example.com'}},
    {'op': 'settings_read', 'file': 'after'},
    {'op': 'settings_save', 'value': {**BASE, 'server': 'https://ntfy.cn.example.com',
                                      'token': '', 'clearToken': True}},
    {'op': 'settings_read', 'file': 'after'},
    {'op': 'settings_save', 'value': {**BASE, 'barkEnabled': True, 'barkKey': 'dev-key/',
                                      'server': 'https://ntfy.cn.example.com'}},
    {'op': 'settings_save', 'value': {'enabled': 'yes', 'topic': 'x'}},
    {'op': 'settings_save', 'value': {**BASE, 'server': 'http://remote.example.com'}},
    {'op': 'settings_save', 'value': {**BASE, 'server': 'not-a-url'}},
    {'op': 'settings_save', 'value': {**BASE, 'topic': '非法主题!'}},
    {'op': 'settings_save', 'value': {**BASE, 'topic': ''}},
    {'op': 'settings_save', 'value': {**BASE, 'addressName': '带\n换行'}},
    {'op': 'settings_save', 'value': {**BASE, 'barkKey': 'k k'}},
    {'op': 'settings_save', 'value': {**BASE, 'clickBase': 'https://lan.example.com/p'}},
    {'op': 'settings_read', 'file': 'after'},
    {'op': 'destination', 'channel': 'ntfy'},
    {'op': 'destination', 'channel': 'bark'},
    {'op': 'destination', 'channel': 'sms'},
    {'op': 'channels', 'config': None},
    {'op': 'channels', 'config': {'enabled': True, 'server': 'https://n.example.com',
                                  'topic': 't', 'barkEnabled': True,
                                  'barkServer': 'https://b.example.com', 'barkKey': 'k'}},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
