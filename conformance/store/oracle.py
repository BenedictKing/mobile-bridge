#!/usr/bin/env python3
"""Replay rollout-history scenarios against bridge/store.py (the oracle).

SessionStore.get is patched to return the scenario meta, so history() runs
its real line parser and _recent_lines backward reader over temp rollout
files placed inside a fake Codex home.

Run:  python3 conformance/store/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.store import SessionStore  # noqa: E402


def run(scenario, emit, workspace, store):
    for index, step in enumerate(scenario['steps']):
        meta = dict(step['meta'])
        rollout = workspace / 'sessions' / f'rollout-{scenario["name"]}-{index}.jsonl'
        rollout.write_text('\n'.join(step['lines']) + ('\n' if step['lines'] else ''),
                           encoding='utf-8')
        meta['rollout_path'] = str(rollout)
        store.get = lambda thread_id, meta=meta: meta
        store.get_any = lambda thread_id, meta=meta: meta
        store.history_cache.clear()
        limit = step['turn_limit']
        try:
            out = store.history('thread-id', turn_limit=limit)
        except (ValueError, KeyError, OSError) as error:
            out = {'$error': f'{type(error).__name__}: {error}'}
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': step['op'],
                         'out': out}, ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    scenario_dir = Path(__file__).resolve().parent / 'scenarios'
    lines = []
    with tempfile.TemporaryDirectory() as home:
        (Path(home) / 'sessions').mkdir()
        store = SessionStore(home)
        for path in sorted(scenario_dir.glob('*.json')):
            run(json.loads(path.read_text(encoding='utf-8')), lines.append, Path(home), store)
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        (scenario_dir / 'golden.jsonl').write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
