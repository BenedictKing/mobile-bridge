#!/usr/bin/env python3
"""Replay pairing scenarios against bridge/pairing.py (the oracle).

Injections: clock, token_urlsafe/token_hex, and the session issuer used by
exchange (returns a deterministic tuple).

Run:  python3 conformance/pairing/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
import bridge.pairing as pairing  # noqa: E402

ORIGINS = ['https://example.com', 'http://192.168.1.5:8787']


class FakeSecrets:
    def __init__(self, token, identifier):
        self.token, self.identifier = token, identifier

    def token_urlsafe(self, n):
        return self.token

    def token_hex(self, n):
        return self.identifier[:2 * n]

    @staticmethod
    def compare_digest(a, b):
        return a == b


def run(scenario, emit):
    instance = pairing.Pairing(None, set(ORIGINS))
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        pairing.secrets = FakeSecrets(step.get('token', ''), step.get('id', ''))
        pairing.time = SimpleNamespace(time=lambda now=step.get('now', 0): now)
        try:
            if operation == 'phone_origin':
                out = pairing.phone_origin(step['value'])
            elif operation == 'create':
                out = instance.control({'action': 'create', 'url': step['url']})
            elif operation == 'status_single':
                out = instance.control({'action': 'status', 'id': step['id']})
            elif operation == 'status_list':
                out = instance.control({'action': 'status', 'ids': step['ids']})
            elif operation == 'revoke':
                out = instance.control({'action': 'revoke', 'id': step['id']})
            elif operation == 'bogus':
                out = instance.control({'action': 'nope'})
            elif operation == 'exchange':
                instance.auth = SimpleNamespace(
                    new_session=lambda address, agent='', client=None,
                    label=step['session']: ('<token>', {'csrf': '<CSRF>', 'label': label}))
                out = instance.exchange(step['token'], step['origin'], step['address'])
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError, PermissionError) as error:
            out = {'$error': str(error)}
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': operation,
                         'out': out}, ensure_ascii=False, separators=(',', ':')).encode())


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
