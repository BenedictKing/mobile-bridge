#!/usr/bin/env python3
"""Replay timeline scenarios against the Python implementation (the oracle).

Every step result is serialized with bridge/timeline.py's own ``encoded()``
(compact separators, ensure_ascii=False) so the Go port can be byte-compared.
``uuid`` is replaced with a deterministic counter before Timeline creation —
the Go test injects the same generator (format(n, '032x')).

Run:  python3 conformance/timeline/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
import bridge.timeline as timeline  # noqa: E402


class DeterministicUuid:
    def __init__(self):
        self.counter = 0

    def uuid4(self):
        value = SimpleNamespace(hex=format(self.counter, '032x'))
        self.counter += 1
        return value


def resolve(results, token):
    if isinstance(token, dict) and '$concat' in token:
        return ''.join(str(resolve(results, part)) for part in token['$concat'])
    if isinstance(token, str) and token.startswith('$'):
        cursor = results[int(token[1:].split('.')[0])]
        rest = token.split('.')[1:]
        for segment in rest:
            cursor = cursor[int(segment)] if isinstance(cursor, list) else cursor[segment]
        return cursor
    return token


def run(scenario, emit):
    timeline.uuid = DeterministicUuid()
    results, instance = [], timeline.Timeline()
    for index, step in enumerate(scenario['steps']):
        op = step['op']
        try:
            if op == 'update':
                instance.update(step['view'])
                out = {'ok': True}
            elif op == 'page':
                before = resolve(results, step['before']) if step.get('before') else None
                out = instance.page(limit=step.get('limit', 20), before=before)
            elif op == 'changes':
                start = resolve(results, step['start']) if step.get('start') else ''
                out = instance.changes(resolve(results, step['after']),
                                       resolve(results, step['epoch']), start)
            elif op == 'detail':
                version = resolve(results, step['version']) if step.get('version') else None
                out = instance.detail(resolve(results, step['cursor']),
                                      step.get('offset', 0), version)
            elif op == 'cursor':
                out = resolve(results, step['epoch']) + '.' + resolve(results, step['key'])
            elif op == 'state':
                out = {'epoch': instance.epoch, 'sequence': instance.sequence,
                       'versions': len(instance.versions), 'rows': len(instance.rows)}
            else:
                raise ValueError('unknown op ' + op)
        except ValueError as error:
            out = {'$error': str(error)}
        results.append(out)
        emit(timeline.encoded({'scenario': scenario['name'], 'i': index, 'op': op, 'out': out}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true',
                        help='rewrite golden.jsonl instead of writing to stdout')
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
