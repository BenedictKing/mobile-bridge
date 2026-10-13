#!/usr/bin/env python3
"""Generate ACP-adapter decision-slice scenarios (attachments + mode)."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'

CAPS_FULL = {'promptCapabilities': {'image': True, 'embeddedContext': True}}
CAPS_TEXT_ONLY = {'promptCapabilities': {'embeddedContext': True}}
CAPS_IMAGE_ONLY = {'promptCapabilities': {'image': True}}
CAPS_NONE = {}

FILES = [
    {'name': '图.png', 'kind': 'png', 'image': True},
    {'name': 'photo.bin', 'kind': 'unknown', 'image': True},
    {'name': '笔记.txt', 'kind': 'text', 'image': False},
]

SCENARIOS = {}

SCENARIOS['01_attachment_blocks'] = [
    {'op': 'attachment_blocks', 'capabilities': CAPS_FULL,
     'files': FILES},
    {'op': 'attachment_blocks', 'capabilities': CAPS_FULL, 'files': []},
    {'op': 'attachment_blocks', 'capabilities': CAPS_TEXT_ONLY, 'files': FILES[:1]},
    {'op': 'attachment_blocks', 'capabilities': CAPS_IMAGE_ONLY, 'files': FILES[2:]},
    {'op': 'attachment_blocks', 'capabilities': CAPS_NONE, 'files': FILES[:1]},
    {'op': 'attachment_blocks', 'capabilities': {'promptCapabilities': {}},
     'files': FILES[2:]},
    {'op': 'attachment_blocks', 'capabilities': CAPS_FULL, 'files': [
        {'name': 'j.bin', 'kind': 'jpeg', 'image': True},
        {'name': 'g.gif', 'kind': 'gif', 'image': True},
        {'name': 'w.webp', 'kind': 'webp', 'image': True}]},
]

MODES = {'availableModes': [{'id': 'default'}, {'id': 'plan'}, {'name': '坏形状'}],
         'currentModeId': 'default'}

SCENARIOS['02_set_mode'] = [
    {'op': 'set_mode', 'modes': MODES, 'set': 'plan'},
    {'op': 'set_mode', 'modes': MODES, 'set': 'default'},
    {'op': 'set_mode', 'modes': MODES, 'set': 'goal'},
    {'op': 'set_mode', 'modes': {'availableModes': [], 'currentModeId': 'chat'},
     'set': 'chat'},
    {'op': 'set_mode', 'modes': {'currentModeId': 'chat'}, 'set': 'chat'},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
