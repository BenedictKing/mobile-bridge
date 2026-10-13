#!/usr/bin/env python3
"""Replay ACP view scenarios against bridge/agents/acp_view.py (the oracle).

The clock is injected per view step group so begin_turn's startedAt ms field
is deterministic.

Run:  python3 conformance/acpview/oracle.py [--goldens]
"""
import argparse
import json
import sys
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.agents import acp_view  # noqa: E402


def run_view(step, emit_ctx):
    view = acp_view.AcpSessionView('sess-acp', title=None, cwd='/工作目录')
    acp_view.time = SimpleNamespace(time=lambda now=step['now']: now)
    for action in step['steps']:
        for key, value in action.items():
            if key == 'snapshot':
                emit_ctx(view.snapshot())
            elif key == 'begin_turn':
                view.begin_turn(value)
            elif key == 'update':
                view.on_update(value)
            elif key == 'end_turn':
                view.end_turn(value)
            elif key == 'fail_turn':
                view.fail_turn(value)
            elif key == 'finish_replay':
                view.finish_replay()
            elif key == 'add_permission':
                view.add_permission(value['id'], value['params'])
            elif key == 'add_input':
                view.add_input(value['id'], value['params'])
            elif key == 'remove_permission':
                view.remove_permission(value)
            elif key == 'apply_config':
                view.apply_config(value['options'], value['modes'])
            elif key == 'set_mode':
                view.set_mode(value)
            else:
                raise ValueError('unknown view action ' + key)


def run(scenario, emit):
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        try:
            if operation == 'form_supported':
                out = acp_view.acp_form_supported(step['schema'])
            elif operation == 'validate_form':
                out = acp_view.validate_acp_form(step['schema'], step['content'])
            elif operation == 'output':
                out = acp_view._output(step['value'])
            elif operation == 'command_summary':
                out = acp_view._command_summary(step['value'])
            elif operation == 'subagent_meta':
                out = acp_view._subagent_meta(step['input'], step.get('title', ''))
            elif operation == 'view':
                snapshots = []
                run_view(step, snapshots.append)
                out = snapshots
            else:
                raise ValueError('unknown op ' + operation)
        except (ValueError, TypeError, KeyError, AttributeError) as error:
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
