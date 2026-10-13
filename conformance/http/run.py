#!/usr/bin/env python3
"""HTTP differential capture harness (plan doc P0).

Boots the Python gateway in-process with a stub bridge, replays a fixed
request sequence and records normalized responses (status, key headers,
masked body) into golden.jsonl. The Python gateway is the oracle; a later Go
implementation must reproduce the golden byte-for-byte.

  python3 conformance/http/run.py            # rewrite golden.jsonl
  python3 conformance/http/run.py --verify   # diff a fresh run against it

Volatile values (instance ids, csrf tokens, session cookies, server/date
headers) are masked; everything else must be stable across runs.
"""
import argparse
import hashlib
import http.client
import json
import re
import socket
import sys
import time
import threading
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT))

from bridge.auth import password_record  # noqa: E402
from bridge.httpd import GatewayServer  # noqa: E402

USERNAME, PASSWORD = 'admin', 'correct password test'

# name, method, path, body, extra headers, requires session cookie+csrf
REQUESTS = [
    ('health', 'GET', '/api/health', None, {'Origin': None}, False),
    ('auth-unauthenticated', 'GET', '/api/auth', None, {}, False),
    ('login-wrong-password', 'POST', '/api/login',
     {'username': USERNAME, 'password': 'wrong'}, {}, False),
    ('login-ok', 'POST', '/api/login',
     {'username': USERNAME, 'password': PASSWORD}, {}, False),
    ('auth-authenticated', 'GET', '/api/auth', None, {}, True),
    ('write-no-csrf', 'POST', '/api/sessions/abcde/stop',
     {}, {'X-CSRF-Token': None}, True),
    ('write-bad-csrf', 'POST', '/api/sessions/abcde/stop',
     {}, {'X-CSRF-Token': 'forged'}, True),
    ('sessions-ok', 'GET', '/api/sessions', None, {}, True),
    ('accounts', 'GET', '/api/accounts', None, {}, True),
    ('logout', 'POST', '/api/logout', {}, {}, True),
    ('auth-after-logout', 'GET', '/api/auth', None, {}, True),
    ('unknown-api-route', 'GET', '/api/nope', None, {}, True),
    ('unknown-session', 'POST', '/api/sessions/abcde/send',
     {'content': 'hi'}, {}, True),
    ('static-timeline-js', 'GET', '/timeline.js', None, {}, True),
    ('static-missing', 'GET', '/no-such-file.js', None, {}, True),
    ('login-again', 'POST', '/api/login',
     {'username': USERNAME, 'password': PASSWORD}, {}, False),
    ('timeline-page', 'GET', '/api/sessions/abcde/timeline?limit=5', None, {}, True),
    ('timeline-detail', 'GET', '/api/sessions/abcde/detail?key=k-1&offset=0', None, {}, True),
    ('poll-after', 'GET', '/api/sessions/abcde/poll?after=6', None, {}, True),
    ('changes-immediate', 'GET', '/api/sessions/abcde/changes?after=6&epoch=other', None, {}, True),
]

# SSE streams captured separately: (name, path, frames-to-read)
SSE_REQUESTS = [
    ('sse-state', '/api/sessions/abcde/events', 1),
    ('sse-incremental', '/api/sessions/abcde/events?incremental=1', 2),
]

MASKS = [
    (re.compile(r'"csrf": ?"[A-Za-z0-9_-]+"'), '"csrf": "<CSRF>"'),
    (re.compile(r'"instanceId": ?"[^"]*"'), '"instanceId": "<INSTANCE>"'),
    (re.compile(r'"token": ?"[A-Za-z0-9_-]+"'), '"token": "<TOKEN>"'),
]
HEADER_MASKS = re.compile(r'^(session|token|csrf)=.*?(;|$)')

# Transport-layer details that differ between HTTP stacks (BaseHTTPRequestHandler
# writes raw, net/http chunk-encodes streams) without affecting clients.
DROPPED_HEADERS = {'date', 'server', 'transfer-encoding'}


SCRIPTED_PAGE = {'epoch': 'scripted-epoch', 'sequence': 7,
                 'rows': [{'key': 'k-1', 'order': 0, 'text': '页行 中文'}],
                 'before': 'scripted-epoch.k-0', 'hasMore': False,
                 'meta': {'sequence': 7, 'requests': [], 'latestUserTurnId': None}}
SCRIPTED_DETAIL = {'key': 'k-1', 'version': 'v1', 'text': '明细内容', 'offset': 0, 'next': None}
SCRIPTED_CHANGES = {'epoch': 'scripted-epoch', 'sequence': 7, 'rows': [], 'meta': {}}
SCRIPTED_VIEW = {'id': 'sess-1', 'title': '脚本会话', 'sequence': 7, 'turns': []}


class ScriptedSession:
    timeline = type('T', (), {'epoch': 'scripted-epoch'})()
    sequence = 7
    viewers = 0
    touched = 0.0
    condition = threading.Condition()


class EmptyBridge:
    host_errors = []
    closed = threading.Event()

    def for_host(self, host):
        if host != 'local':
            raise KeyError(host)
        return self

    def list(self, **kwargs):
        return []

    def session(self, thread_id, background=False):
        return ScriptedSession()

    def view(self, thread_id, attach=False, background=True):
        return SCRIPTED_VIEW

    def timeline_read(self, thread_id, *args, **kwargs):
        mode = args[0] if args else 'page'
        return {'page': SCRIPTED_PAGE, 'detail': SCRIPTED_DETAIL,
                'changes': SCRIPTED_CHANGES}[mode]

    accounts = type('Accounts', (), {
        'public': staticmethod(lambda: {'accounts': [], 'activeId': None,
                                        'switch': {'phase': 'idle'}})})()


def normalize_headers(headers):
    kept = []
    for name, value in headers:
        lowered = name.lower()
        if lowered in DROPPED_HEADERS:
            continue
        if lowered == 'set-cookie':
            value = re.sub(r'^([^=;]+)=[^;]*(;.*)?$', r'\1=<TOKEN>\2', value)
        kept.append(f'{name}: {value}')
    return sorted(kept)


def mask(text):
    for pattern, replacement in MASKS:
        text = pattern.sub(replacement, text)
    return text


def boot():
    config = {'auth': {'mode': 'password', 'username': USERNAME,
                       **password_record(PASSWORD)}, 'origins': []}
    server = GatewayServer(('127.0.0.1', 0), EmptyBridge(), config, ROOT / 'web')
    server.notifications = None
    origin = f'http://127.0.0.1:{server.server_address[1]}'
    server.origins.add(origin)
    server.hosts.add(f'127.0.0.1:{server.server_address[1]}')
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, origin


def replay(port):
    origin = f'http://127.0.0.1:{port}'
    session = {}

    def request(name, method, path, body, extra, authenticated):
        headers = {'Origin': origin, 'Content-Type': 'application/json',
                   'Host': f'127.0.0.1:{port}'}
        if authenticated:
            headers['Cookie'] = session.get('cookie', '')
            headers['X-CSRF-Token'] = session.get('csrf', '')
        for key, value in extra.items():
            if value is None:
                headers.pop(key, None)
            else:
                headers[key] = value
        connection = http.client.HTTPConnection('127.0.0.1', port, timeout=5)
        connection.request(method, path,
                           body=json.dumps(body) if body is not None else None,
                           headers=headers)
        response = connection.getresponse()
        status = response.status
        response_headers = response.getheaders()
        data = response.read()
        connection.close()
        if name.startswith('login-') and 'wrong' not in name:
            session['cookie'] = response.getheader('Set-Cookie', '').split(';')[0]
            session['csrf'] = json.loads(data)['csrf']
        entry = {'name': name, 'status': status,
                 'headers': normalize_headers(response_headers)}
        content_type = response.getheader('Content-Type', '')
        if 'json' in content_type or data[:1] in (b'{', b'['):
            entry['body'] = mask(data.decode('utf-8', 'replace'))
        else:
            entry['body'] = {'sha256': hashlib.sha256(data).hexdigest(),
                             'length': len(data)}
        return json.dumps(entry, ensure_ascii=False, separators=(',', ':'),
                          sort_keys=True)

    lines = [request(*spec) for spec in REQUESTS]

    def capture_sse(name, path, frames_wanted):
        headers = {'Origin': origin, 'Host': f'127.0.0.1:{port}',
                   'Cookie': session.get('cookie', ''),
                   'X-CSRF-Token': session.get('csrf', '')}
        connection = http.client.HTTPConnection('127.0.0.1', port, timeout=6)
        connection.request('GET', path, headers=headers)
        response = connection.getresponse()
        status, response_headers = response.status, response.getheaders()
        buffer, frames = '', []
        deadline = time.monotonic() + 4
        while len(frames) < frames_wanted and time.monotonic() < deadline:
            chunk = response.read(1)
            if not chunk:
                break
            buffer += chunk.decode('utf-8', 'replace')
            while '\n\n' in buffer:
                frame, buffer = buffer.split('\n\n', 1)
                frames.append(frame + '\n\n')
        connection.close()
        entry = {'name': name, 'status': status,
                 'headers': normalize_headers(response_headers), 'frames': frames}
        return json.dumps(entry, ensure_ascii=False, separators=(',', ':'),
                          sort_keys=True)

    lines += [capture_sse(*spec) for spec in SSE_REQUESTS]
    return lines


def capture():
    server, origin = boot()
    try:
        return replay(server.server_address[1])
    finally:
        server.shutdown()
        server.server_close()


def capture_go():
    import os
    import subprocess
    binary = ROOT / 'dist' / 'gateway-go'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/gateway'],
                   cwd=ROOT, check=True)
    record = password_record(PASSWORD)
    probe = socket.socket()
    probe.bind(('127.0.0.1', 0))
    port = probe.getsockname()[1]
    probe.close()
    environment = dict(os.environ, GATEWAY_PORT=str(port),
                       GATEWAY_AUTH=json.dumps({'username': USERNAME, **record}))
    process = subprocess.Popen([str(binary)], env=environment,
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            try:
                connection = http.client.HTTPConnection('127.0.0.1', port, timeout=1)
                connection.request('GET', '/api/health', headers={'Host': f'127.0.0.1:{port}'})
                if connection.getresponse().status == 200:
                    connection.close()
                    break
                connection.close()
            except OSError:
                pass
            time.sleep(0.1)
        else:
            raise RuntimeError('Go gateway did not become healthy')
        return replay(port)
    finally:
        process.terminate()
        process.wait(timeout=5)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--verify', action='store_true')
    parser.add_argument('--target', choices=('python', 'go'), default='python')
    arguments = parser.parse_args()
    golden_path = Path(__file__).resolve().parent / 'golden.jsonl'
    lines = capture_go() if arguments.target == 'go' else capture()
    payload = ''.join(line + '\n' for line in lines)
    if arguments.verify:
        expected = golden_path.read_text(encoding='utf-8')
        if payload == expected:
            print(f'conformance: {len(lines)} responses match golden')
            return 0
        for index, (produced, wanted) in enumerate(zip(lines, expected.splitlines())):
            if produced != wanted:
                name = json.loads(produced).get('name', '?') if produced.startswith('{') else '?'
                print(f'MISMATCH line {index} ({name}):\n  want: {wanted[:400]}\n'
                      f'   got: {produced[:400]}', file=sys.stderr)
        sys.exit(1)
    if arguments.target != 'python':
        sys.exit('golden.jsonl may only be rewritten from the Python oracle (no --target go)')
    golden_path.write_text(payload, encoding='utf-8')
    print(f'wrote {len(lines)} golden responses to {golden_path}')


if __name__ == '__main__':
    main()
