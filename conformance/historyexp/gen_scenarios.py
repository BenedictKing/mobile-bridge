#!/usr/bin/env python3
"""Generate saved-history edge-expansion scenarios."""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'
SCENARIOS = {}

# Walk pages of 2 until every backfill hits the loaded edge; each hit bumps
# the window by 50 and reloads the saved prefix.
SCENARIOS['01_edge_walk'] = [
    {'op': 'session', 'total': 45, 'steps': [
        {'read_page': {'limit': 2}},
        *[{'read_page': {'limit': 2, 'before_last': True}} for _ in range(12)],
    ]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
