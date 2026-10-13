#!/usr/bin/env python3
"""Replay LiveSession→Timeline composition scenarios (the oracle).

project mirrors timeline_read's update trigger: the projection refreshes
only when the sequence moved or the goal/subagentRevision meta differs.

Run:  python3 conformance/projection/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.service import LiveSession  # noqa: E402
import bridge.timeline as timeline_module  # noqa: E402
from bridge.timeline import Timeline  # noqa: E402
from types import SimpleNamespace  # noqa: E402


class DeterministicUuid:
    def __init__(self):
        self.counter = 0

    def uuid4(self):
        hex_text = format(self.counter, '032x')
        self.counter += 1
        return SimpleNamespace(hex=hex_text)


timeline_module.uuid = DeterministicUuid()


def run_session(step, emit):
    session = LiveSession('chat')
    projection = Timeline()
    pages = []

    def session_action(key, value, action):
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
        elif key == 'changed':
            with session.condition:
                session.changed()
        else:
            raise ValueError('unknown session action ' + key)

    for action in step['steps']:
        for key, value in action.items():
            if key in ('set_history', 'state', 'connected', 'changed'):
                session_action(key, value, action)
            elif key == 'project':
                with session.condition:
                    view = session.view()
                    stale = (projection.sequence == session.sequence
                             and projection.meta.get('goal') == view.get('goal')
                             and projection.meta.get('subagentRevision') == view.get('subagentRevision'))
                if not stale:
                    projection.update(view)
                emit_out = {'updated': not stale, 'sequence': session.sequence}
                emit(('project', emit_out))
            elif key == 'page':
                before = None
                if value.get('before_last') and pages:
                    before = pages[-1]['before']
                elif value.get('before_prev') and len(pages) >= 2:
                    before = pages[-2]['before']
                result = projection.page(limit=value.get('limit', 20), before=before)
                pages.append(result)
                emit(('page', result))
            elif key == 'changes':
                epoch = pages[-1]['epoch'] if value.get('epoch_last') and pages else ''
                result = projection.changes(value.get('after'), epoch, value.get('start', ''))
                emit(('changes', result))
            else:
                raise ValueError('unknown op ' + key)


def run(scenario, emit):
    for index, step in enumerate(scenario['steps']):
        records = []
        run_session(step, lambda pair: records.append({'op': pair[0], 'out': pair[1]}))
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': 'session',
                         'out': {'records': records}},
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
