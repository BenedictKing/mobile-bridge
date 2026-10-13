#!/usr/bin/env python3
"""Replay LiveSession view-merge scenarios against bridge/service.py (the
oracle). This is the service.py first slice: set_history/view state merging.

Run:  python3 conformance/service/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.service import LiveSession  # noqa: E402


def run_session(step, emit):
    session = LiveSession('chat')
    for action in step['steps']:
        for key, value in action.items():
            if key == 'set_history':
                with session.condition:
                    session.set_history(value)
            elif key == 'state':
                with session.condition:
                    session.state = value
                    if 'connected' in action:
                        session.connected = action['connected']
            elif key == 'connected':
                with session.condition:
                    session.connected = value
            elif key == 'error':
                with session.condition:
                    session.error = value
            elif key == 'connecting':
                with session.condition:
                    session.connecting = value
            elif key == 'activating':
                with session.condition:
                    session.activating = value
            elif key == 'changed':
                with session.condition:
                    session.changed()
            elif key == 'set' and value == 'pagination_complete':
                with session.condition:
                    if isinstance(session.state, dict):
                        pagination = session.state.setdefault('turnsPagination', {})
                        pagination['hasLoadedOldest'] = True
            elif key == 'view':
                emit(session.view())
            else:
                raise ValueError('unknown session action ' + key)


def run(scenario, emit):
    for index, step in enumerate(scenario['steps']):
        if step['op'] != 'session':
            raise ValueError('unknown op ' + step['op'])
        views = []
        run_session(step, views.append)
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': 'session',
                         'out': {'views': views}},
                        ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    lines = []
    for path in sorted(scenario_dir.glob('*.json')):
        run(json.loads(path.read_text(encoding='utf-8')), lines.append)
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        (scenario_dir / 'golden.jsonl').write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
