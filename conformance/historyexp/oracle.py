#!/usr/bin/env python3
"""Replay saved-history edge-expansion scenarios (the oracle).

The stub adapter's saved_history returns a scripted rollout sized by
session.history_limit, so the expansion arithmetic (50-window bumps) and the
re-projection after each reload are captured end to end.

Run:  python3 conformance/historyexp/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

import bridge.timeline as timeline_module  # noqa: E402
from bridge.agents.base import AgentAdapter  # noqa: E402
from bridge.service import Bridge, LiveSession  # noqa: E402
from types import SimpleNamespace  # noqa: E402


class DeterministicUuid:
    def __init__(self):
        self.counter = 0

    def uuid4(self):
        hex_text = format(self.counter, '032x')
        self.counter += 1
        return SimpleNamespace(hex=hex_text)


timeline_module.uuid = DeterministicUuid()

TID = 'sess-hist'


class StubAdapter(AgentAdapter):
    kind = 'stub'
    label = 'Stub Agent'
    capabilities = frozenset({'chat'})

    def __init__(self, total):
        self.total = total

    def session_meta(self, thread_id):
        return {}

    def saved_history(self, session):
        limit = session.history_limit
        return {'id': TID, 'turns': [
            {'turnId': f't{i:02d}', 'items': [
                {'type': 'agentMessage', 'id': f't{i:02d}', 'text': f'历史 {i:02d}'}]}
            for i in range(max(0, self.total - limit), self.total)] + [
            {'turnId': 'live', 'items': [
                {'type': 'agentMessage', 'id': 'live', 'text': '实时尾巴'}]}],
            'turnsPagination': {'hasLoadedOldest': limit >= self.total}}


def run_session(step, workspace, emit):
    bridge = Bridge(workspace / 'home', workspace / 'data',
                    adapter=StubAdapter(step['total']))
    session = bridge.live.setdefault(TID, LiveSession(TID))
    session.connected = True
    # Prime the saved view once (incomplete: only the tail is loaded).
    with session.condition:
        session.set_history(bridge.adapter.saved_history(session))
        session.state = {'id': TID, 'turns': [], 'turnsPagination': {'hasLoadedOldest': False}}
    pages = []
    for action in step['steps']:
        for key, value in action.items():
            if key != 'read_page':
                raise ValueError('unknown action ' + key)
            options = {'limit': value['limit']}
            if value.get('before_last') and pages:
                options['before'] = pages[-1]['before']
            result = bridge.timeline_read(TID, **options)
            pages.append(result)
            emit(('read_page', {'rows': [row['text'] for row in result['rows']],
                                'hasMore': result['hasMore'],
                                'historyLimit': session.history_limit}))
    bridge.closed.set()


def run(scenario, emit, workspace):
    for index, step in enumerate(scenario['steps']):
        records = []
        run_session(step, workspace, lambda pair: records.append({'op': pair[0], 'out': pair[1]}))
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': 'historyexp',
                         'out': {'records': records}},
                        ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    lines = []
    with tempfile.TemporaryDirectory() as workspace:
        for path in sorted(scenario_dir.glob('*.json')):
            run(json.loads(path.read_text(encoding='utf-8')), lines.append, Path(workspace))
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        (scenario_dir / 'golden.jsonl').write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
