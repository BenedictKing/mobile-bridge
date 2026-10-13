#!/usr/bin/env python3
"""Replay real Bridge.timeline_read scenarios (the oracle).

A minimal Bridge over a stub AgentAdapter subclass (empty temp codex_home and
data_dir) exercises the annotated read path: subagentRevision injection, goal
overlay pass-through, meta enrichment (host/agent/hostLabel/capabilities/
goalRuntimeAvailable/submissions/goalSubmission/forkedFrom) and the projection
update trigger.

Run:  python3 conformance/timelineread/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

import bridge.timeline as timeline_module  # noqa: E402
from bridge.agents.base import AgentAdapter  # noqa: E402
from bridge.service import Bridge, LiveSession  # noqa: E402


class DeterministicUuid:
    def __init__(self):
        self.counter = 0

    def uuid4(self):
        hex_text = format(self.counter, '032x')
        self.counter += 1
        return SimpleNamespace(hex=hex_text)


timeline_module.uuid = DeterministicUuid()


class StubAdapter(AgentAdapter):
    kind = 'stub'
    label = 'Stub Agent'
    capabilities = frozenset({'resume', 'chat'})

    def session_meta(self, thread_id):
        return {}


TID = 'sess-read'


def run_session(step, workspace, emit):
    bridge = Bridge(workspace / 'home', workspace / 'data', adapter=StubAdapter())
    # Pre-register a connected session: background refresh would otherwise
    # race the golden via adapter.attach.
    session = bridge.live.setdefault(TID, LiveSession(TID))
    session.connected = True
    pages = []

    def read_options(value):
        options = {}
        if value.get('limit') is not None:
            options['limit'] = value['limit']
        if value.get('before_last') and pages:
            options['before'] = pages[-1]['before']
        return options

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
            elif key == 'changed':
                with session.condition:
                    session.changed()
            elif key == 'read_page':
                result = bridge.timeline_read(TID, **read_options(value))
                pages.append(result)
                emit(('read_page', result))
            elif key == 'read_changes':
                options = {'after': value['after'], 'start': ''}
                if value.get('epoch_last') and pages:
                    options['epoch'] = pages[-1]['epoch']
                emit(('read_changes', bridge.timeline_read(TID, 'changes', **options)))
            elif key == 'read_detail':
                row = pages[-1]['rows'][0]
                emit(('read_detail', bridge.timeline_read(
                    TID, 'detail', cursor=pages[-1]['epoch'] + '.' + row['key'],
                    offset=value.get('offset', 0), version=value.get('version'))))
            else:
                raise ValueError('unknown action ' + key)
    bridge.closed.set()


def run(scenario, emit, workspace):
    for index, step in enumerate(scenario['steps']):
        records = []
        run_session(step, workspace, lambda pair: records.append({'op': pair[0], 'out': pair[1]}))
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': 'bridge_session',
                         'out': {'records': records}},
                        ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    lines = []
    with tempfile.TemporaryDirectory() as workspace:
        root = Path(workspace)
        for path in sorted(scenario_dir.glob('*.json')):
            run(json.loads(path.read_text(encoding='utf-8')), lines.append, root)
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        (scenario_dir / 'golden.jsonl').write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
