#!/usr/bin/env python3
"""Replay lifecycle scenarios against bridge/lifecycle.py (the oracle).

Injections: os.getpid, secrets.token_hex, tempfile.mkdtemp and the watcher
thread (disabled), so the real GatewayControl.start() writes its control file
deterministically.

Run:  python3 conformance/lifecycle/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
import bridge.lifecycle as lifecycle  # noqa: E402

REAL_TOKEN_HEX = lifecycle.secrets.token_hex


class DummyThread:
    def __init__(self, *args, **kwargs):
        pass

    def start(self):
        pass

    def join(self, timeout=None):
        pass


def control_record(step, workspace):
    real_getpid, real_mkdtemp = lifecycle.os.getpid, lifecycle.tempfile.mkdtemp
    lifecycle.os.getpid = lambda: step['pid']
    lifecycle.secrets.token_hex = lambda n: step['token_hex'][:2 * n]
    lifecycle.tempfile.mkdtemp = lambda prefix=None, dir=None: str(Path(dir) / step['pairing_dir'])
    try:
        control = lifecycle.GatewayControl(workspace)
        control.start(shutdown=lambda: None, pairing=lambda payload: {},
                      instance_id=step['instance_id'], auth=object() if step['device'] else None,
                      account=object() if step['account'] else None,
                      notifications=object() if step['notifications'] else None,
                      accounts=object() if step['accounts'] else None)
        return {'bytes_hex': control.path.read_bytes().hex()}
    finally:
        lifecycle.os.getpid = real_getpid
        lifecycle.secrets.token_hex = REAL_TOKEN_HEX
        lifecycle.tempfile.mkdtemp = real_mkdtemp
        lifecycle.threading.Thread = DummyThread


def run(scenario, emit, workspace):
    lifecycle.threading.Thread = DummyThread
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        try:
            if operation == 'control_record':
                out = control_record(step, workspace)
            elif operation == 'dump_ascii':
                out = {'bytes_hex': json.dumps(step['value']).encode().hex()}
            elif operation == 'stop_validate':
                record = step['record']
                valid = (isinstance(record, dict) and isinstance(record.get('pid'), int)
                         and bool(record.get('token')))
                out = {'valid': valid} if valid else {
                    'valid': False,
                    'error': 'No gateway control record; use Ctrl+C in its terminal if it is running.'}
            elif operation == 'read_record':
                probe = workspace / 'probe.json'
                probe.write_text(step['text'], encoding='utf-8')
                out = lifecycle.read_record(probe)
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError, RuntimeError, OSError) as error:
            out = {'$error': str(error)}
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': operation,
                         'out': out}, ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    with tempfile.TemporaryDirectory() as workspace:
        root = Path(workspace)
        lines = []
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
