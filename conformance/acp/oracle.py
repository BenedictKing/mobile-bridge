#!/usr/bin/env python3
"""Replay ACP transport scenarios against bridge/agents/acp.py (the oracle).

Drives AcpProcess against conformance/acp/agent.py (the fake agent). The
golden records each op's outcome plus the wire log — every line the
implementation sent to the agent.

Run:  python3 conformance/acp/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.agents.acp import AcpProcess, AcpError  # noqa: E402


def wait_for(condition, timeout=3.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if condition():
            return True
        time.sleep(0.01)
    return False


def run(emit):
    with tempfile.TemporaryDirectory() as workspace:
        wire_log = Path(workspace) / 'wire.log'
        notifications = []
        process = AcpProcess(
            [sys.executable, str(Path(__file__).resolve().parent / 'agent.py'),
             str(wire_log)],
            on_notification=notifications.append,
            on_request=lambda message, respond: respond({'answer': '来自网关'}))

        def describe(error):
            return {'type': type(error).__name__, 'message': str(error),
                    'code': error.code, 'data': error.data}

        ops = [
            ('initialize', lambda: process.initialize()),
            ('echo', lambda: process.request('echo', {'文本': '你好 🚀', 'n': [1, 2.5]})),
            ('fail-auth', lambda: process.request('fail', {})),
            ('fail-generic', lambda: process.request('fail_generic', {})),
            ('null-result', lambda: process.request('null', {})),
            ('banner', lambda: process.request('banner', {})),
            ('orphan', lambda: process.request('orphan', {})),
            ('poke', lambda: process.request('poke', {'x': 1})),
            ('reverse', lambda: process.request('reverse', {})),
        ]
        outs = []
        for name, op in ops:
            try:
                result = op()
                out = {'ok': result}
            except AcpError as error:
                out = {'error': describe(error)}
            if name == 'poke':
                wait_for(lambda: len(notifications) >= 1)
            outs.append(out)
        process.close()
        wait_for(lambda: False, 0.1)
        for index, out in enumerate(outs):
            out['meta'] = {'capabilities': process.capabilities,
                           'agentInfo': process.agent_info,
                           'authMethods': process.auth_methods} if index == 0 else None
            if index == 7:
                out['notifications'] = notifications
            emit(json.dumps({'i': index, 'out': out},
                            ensure_ascii=False, separators=(',', ':')).encode())
        emit(json.dumps({'i': 'wire', 'out': {'log': wire_log.read_text(encoding='utf-8')}},
                        ensure_ascii=False, separators=(',', ':')).encode())


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--goldens', action='store_true')
    arguments = parser.parse_args()
    golden = Path(__file__).resolve().parent / 'golden.jsonl'
    lines = []
    run(lines.append)
    payload = b''.join(line + b'\n' for line in lines)
    if arguments.goldens:
        golden.write_bytes(payload)
        print(f'wrote {len(lines)} golden lines')
    else:
        sys.stdout.buffer.write(payload)


if __name__ == '__main__':
    main()
