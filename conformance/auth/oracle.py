#!/usr/bin/env python3
"""Replay auth pure-function scenarios against bridge/auth.py (the oracle).

Salt (for password_record) and the clock (for valid/cookie_age) are injected
per step so results are fully deterministic.

Run:  python3 conformance/auth/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
import bridge.auth as auth  # noqa: E402

REAL_TIME = auth.time


def run(scenario, emit):
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        salt = bytes.fromhex(step.get('salt_hex', '00' * 16))
        auth.secrets.token_bytes = lambda n, salt=salt: salt[:n]
        if 'now' in step:
            auth.time = SimpleNamespace(time=lambda now=step['now']: now)
        else:
            auth.time = REAL_TIME
        try:
            if operation == 'session_hours':
                out = auth.session_hours(step['value'])
            elif operation == 'canonical_ip':
                out = auth.canonical_ip(step['value'])
            elif operation == 'access_policy':
                out = auth.access_policy(step['value'])
            elif operation == 'password_record':
                out = auth.password_record(step['password'])
            elif operation == 'key':
                out = auth.Auth.key(step['token'])
            elif operation == 'valid':
                out = auth.Auth.valid(step['session'])
            elif operation == 'cookie_age':
                instance = auth.Auth({'mode': 'password', 'username': 'u', 'salt': '00' * 16,
                                      'hash': '00' * 64, 'iterations': 1})
                token = 'stub-token'
                instance.sessions[auth.Auth.key(token)] = dict(step['session'])
                out = auth.Auth.cookie_age(instance, token)
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError, PermissionError) as error:
            out = {'$error': str(error)}
        finally:
            auth.time = REAL_TIME
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
