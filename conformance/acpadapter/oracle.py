#!/usr/bin/env python3
"""Replay ACP-adapter decision scenarios against bridge/agents/acp_adapter.py.

Drives the real AcpAdapter methods with stub handles (no agent process).
Attachment files are materialized with fixed bytes inside a temp workspace;
resolved workspace paths are masked to <WS> in the golden.

Run:  python3 conformance/acpadapter/oracle.py [--goldens]
"""
import argparse
import json
import sys
import tempfile
import threading
from pathlib import Path
from types import SimpleNamespace

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))
from bridge.agents.acp_adapter import AcpAdapter  # noqa: E402
from bridge.agents.acp_view import AcpSessionView  # noqa: E402

MAGIC = {
    'png': b'\x89PNG\r\n\x1a\n' + b'png-bytes',
    'jpeg': b'\xff\xd8\xff' + b'jpeg-bytes',
    'gif': b'GIF89a' + b'gif-bytes',
    'webp': b'RIFF' + b'----' + b'WEBP' + b'webp-bytes',
    'unknown': b'\x00\x01plain-binary',
    'text': '文本内容 📎\n'.encode('utf-8'),
}


def make_handle(capabilities, modes):
    requests = []
    view = AcpSessionView('sess-1')

    def request(method, params, timeout=30):
        requests.append({'method': method, 'params': params})
        return {}

    process = SimpleNamespace(capabilities=capabilities, request=request)
    return SimpleNamespace(lock=threading.Lock(), process=process,
                           modes=json.loads(json.dumps(modes)), view=view), requests


def run(scenario, emit, workspace):
    adapter = AcpAdapter('unused')
    masked = str(workspace.resolve())
    for index, step in enumerate(scenario['steps']):
        operation = step['op']
        try:
            if operation == 'attachment_blocks':
                handle, _ = make_handle(step['capabilities'], {})
                files = []
                for item in step['files']:
                    path = workspace / item['name']
                    path.write_bytes(MAGIC[item['kind']])
                    files.append({'image': item['image'], 'name': item['name'],
                                  'path': str(path)})
                out = {'blocks': adapter._attachment_blocks(handle, files)}
            elif operation == 'set_mode':
                handle, requests = make_handle({}, step['modes'])
                adapter._set_mode(handle, SimpleNamespace(id='sess-1'), step['set'])
                out = {'requests': requests,
                       'currentModeId': (handle.modes or {}).get('currentModeId'),
                       'viewMode': handle.view.snapshot()['collaborationMode']}
            else:
                raise ValueError('unknown op ' + operation)
        except ValueError as error:
            out = {'$error': str(error)}
        payload = json.dumps(out, ensure_ascii=False, separators=(',', ':'))
        payload = payload.replace(masked, '<WS>')
        emit(json.dumps({'scenario': scenario['name'], 'i': index, 'op': operation,
                         'out': json.loads(payload)},
                        ensure_ascii=False, separators=(',', ':')).encode())


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
