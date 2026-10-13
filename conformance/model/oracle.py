#!/usr/bin/env python3
"""Replay model scenarios against bridge/model.py (the oracle). Pure functions
only — no injections needed.

Run:  python3 conformance/model/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge import model  # noqa: E402


def run(scenario, emit):
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        try:
            if operation == 'normalize_state':
                out = model.normalize_state(step['state'], step.get('connected', True))
            elif operation == 'apply_patches':
                out = model.apply_patches(step['state'], step['patches'])
            elif operation == 'text_content':
                out = model.text_content(step['content'])
            elif operation == 'user_display_text':
                out = model.user_display_text(step['text'])
            elif operation == 'question_replies':
                out = model.question_replies(step['text'])
            elif operation == 'form_supported':
                out = model.form_supported(step['schema'])
            elif operation == 'normalize_request':
                out = model.normalize_request(step['request'])
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError, KeyError, TypeError, IndexError) as error:
            out = {'$error': f'{type(error).__name__}: {error}'}
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
