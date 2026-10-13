#!/usr/bin/env python3
"""Replay submission-ledger scenarios against bridge/service.py (the oracle).

A stub AgentAdapter scripts dispatch outcomes (ok / ValueError /
TurnNotDispatched) and is_active. The session is pre-registered and connected
so the background maintain loop never races the scenario.

Step forms:
  {"op": "send", "value": {text, submission_id, mode?, ...}}
  {"op": "meta"}                      -> _submission_meta output
  {"op": "mark_failed", "value": {id, error}}
  {"op": "queue_action", "value": {id, action, text?}}
  {"op": "ignore_submission", "value": {id}}
  {"set_active": bool} / {"dispatch": {"ok":true} | {"error": "..."} | {"not_dispatched": "..."}}

Run:  python3 conformance/submissions/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

from bridge.agents.acp import AcpTimeout  # noqa: E402
from bridge.agents.acp_adapter import TurnNotDispatched  # noqa: E402
from bridge.agents.base import AgentAdapter  # noqa: E402
from bridge.service import Bridge, LiveSession  # noqa: E402

TID = 'sess-sub'


class StubAdapter(AgentAdapter):
    kind = 'stub'
    label = 'Stub Agent'
    capabilities = frozenset({'resume', 'chat', 'steer'})

    def __init__(self):
        self.active = False
        self.next_dispatch = {'ok': True}

    def session_meta(self, thread_id):
        return {}

    def require_ready(self, session):
        pass

    def check_provider(self, session):
        pass

    def is_active(self, session):
        return self.active

    def dispatch_turn(self, session, submission_id, entry, files):
        script = self.next_dispatch
        if script.get('error'):
            raise ValueError(script['error'])
        if script.get('not_dispatched'):
            raise TurnNotDispatched(script['not_dispatched'])
        if script.get('timeout'):
            raise AcpTimeout('Agent 请求超时：session/prompt')
        return {'accepted': True}

    def resolve_skills(self, session, skills):
        return skills


def run_session(step, name, workspace, emit):
    adapter = StubAdapter()
    room = workspace / f'room-{name}-{step["op"]}'
    bridge = Bridge(room / 'home', room / 'data', adapter=adapter)
    session = bridge.live.setdefault(TID, LiveSession(TID))
    session.connected = True
    session.state = {'id': TID, 'latestModel': 'stub-model', 'turns': [],
                     'requests': []}

    def attempt(operation):
        try:
            return operation()
        except Exception as error:  # noqa: BLE001 - errors are part of the contract
            return {'$error': f'{type(error).__name__}: {error}'}

    for action in step['steps']:
        if 'set_active' in action:
            adapter.active = action['set_active']
            continue
        if 'dispatch' in action:
            adapter.next_dispatch = action['dispatch']
            continue
        name = action['op']
        value = action.get('value', {})
        if name == 'send':
            spec = dict(value)
            emit((name, attempt(lambda: bridge.send(
                TID, spec.pop('text'), spec.pop('submission_id'),
                mode=spec.pop('mode', 'send'), **spec))))
        elif name == 'meta':
            def meta():
                view = {}
                bridge._submission_meta(session, view)
                return view
            emit((name, attempt(meta)))
        elif name == 'mark_failed':
            emit((name, attempt(lambda: bridge.mark_submission_failed(
                TID, value['id'], value['error']))))
        elif name == 'queue_action':
            emit((name, attempt(lambda: bridge.queue_action(
                TID, value['id'], value['action'], value.get('text')))))
        elif name == 'ignore_submission':
            emit((name, attempt(lambda: bridge.ignore_submission(TID, value['id']))))
        else:
            raise ValueError('unknown op ' + name)
    bridge.closed.set()


def run(scenario, emit, workspace):
    for index, step in enumerate(scenario['steps']):
        records = []
        run_session(step, scenario['name'], workspace, lambda pair: records.append({'op': pair[0], 'out': pair[1]}))
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': 'submissions',
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
