#!/usr/bin/env python3
"""Replay notification pure-layer scenarios against
bridge/notifications.py (the oracle). save_settings persists into a temp
data dir so the token-preservation interplay across saves is captured.

Run:  python3 conformance/notifications/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge import notifications  # noqa: E402
from bridge.validation import FieldError  # noqa: E402


def describe(error):
    fields = {'field': error.validation['field']} if isinstance(error, FieldError) else {}
    return {'$error': f'{type(error).__name__}: {error}', **fields}


def run(scenario, emit, workspace):
    data_dir = workspace / f'data-{scenario["name"]}'
    data_dir.mkdir(parents=True, exist_ok=True)
    saved = None
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        try:
            if operation == 'valid_url':
                out = notifications.valid_url(step['value'],
                                              allow_path=step.get('allow_path', False))
            elif operation == 'settings_read':
                out = notifications.settings(data_dir)
            elif operation == 'settings_save':
                saved = notifications.save_settings(data_dir, step['value'])
                out = saved
            elif operation == 'destination':
                out = notifications.destination(saved or {}, step['channel'])
            elif operation == 'channels':
                config = saved if step.get('config') is None else step['config']
                out = notifications.channels(config)
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError,) as error:
            out = describe(error)
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': operation,
                         'out': out}, ensure_ascii=False, separators=(',', ':')).encode())


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
