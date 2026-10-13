#!/usr/bin/env python3
"""Classify the Python test suite for the Go migration (plan doc section 6 / P0).

Every tests/test_*.py file is assigned:
  - a test-method count,
  - category flags: A (pure logic), B (HTTP behavior), C (IPC / private protocol),
  - the bridge modules it exercises, mapped to migration phases P1-P7.

Heuristic by static text: imports and well-known symbols. Review by hand before
relying on a single row; totals are reliable, individual rows may drift.
"""
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

HTTP_MARKERS = ('http.client', 'urllib.request', 'GatewayServer', 'urlopen', 'HTTPConnection')
IPC_MARKERS = ('pipe_fixture', 'bridge.ipc', 'bridge.transport', 'DesktopIPC', 'NamedPipe',
               'ipc_path', 'GOAWAY')
# test file -> migration phase, by the module under test (heuristic name mapping)
PHASE_BY_MODULE = [
    (('timeline',), 'P1'),
    (('store', 'model', 'auth', 'pairing', 'platform', 'tls', 'network', 'access',
      'lifecycle', 'config'), 'P2'),
    (('http',), 'P3'),
    (('acp', 'adapter', 'kimi', 'mcode', 'claude', 'dsh', 'zcode'), 'P4'),
    (('codex', 'goal', 'native_subagents', 'discovery'), 'P5'),
    (('bridge', 'service', 'session', 'notifications', 'accounts', 'account', 'create',
      'migrate', 'fast_mode', 'modes', 'uploads', 'activity', 'jev', 'desktop',
      'tunnel', 'updater', 'watch', 'worktree', 'message_actions', 'task_inputs',
      'approval', 'v140'), 'P6'),
]
C_PHASE_OVERRIDES = {  # IPC-shaped tests ride with the transport phase P2
    'test_desktop.py': 'P2', 'test_acp_transport.py': 'P4',
}
B_PHASE_OVERRIDES = {
    'test_accounts_http.py': 'P3', 'test_tls.py': 'P3',
}


def phase_for(name, flags, modules):
    if name in C_PHASE_OVERRIDES:
        return C_PHASE_OVERRIDES[name]
    if name in B_PHASE_OVERRIDES:
        return B_PHASE_OVERRIDES[name]
    for keys, phase in PHASE_BY_MODULE:
        if any(k in modules for k in keys):
            return phase
    return 'P6'


def classify():
    rows = []
    for path in sorted((ROOT / 'tests').glob('test_*.py')):
        text = path.read_text(encoding='utf-8')
        name = path.name
        count = len(re.findall(r'^\s+def test_', text, re.M))
        flags = set()
        if any(m in text for m in HTTP_MARKERS):
            flags.add('B')
        if any(m in text for m in IPC_MARKERS):
            flags.add('C')
        if not flags:
            flags.add('A')
        mods = sorted({m for m in re.findall(r'bridge(?:\.agents)?\.([a-z_]+)', text)})
        rows.append({'file': name, 'tests': count, 'flags': ''.join(sorted(flags)),
                     'modules': mods, 'phase': phase_for(name, flags, mods)})
    return rows


if __name__ == '__main__':
    rows = classify()
    (ROOT / 'conformance').mkdir(exist_ok=True)
    print(json.dumps(rows, ensure_ascii=False, indent=1))
    total = sum(r['tests'] for r in rows)
    by_flag = {}
    for r in rows:
        for f in r['flags']:
            by_flag[f] = by_flag.get(f, 0) + r['tests']
    print(f"# total methods: {total}; by flag: {by_flag}", file=__import__('sys').stderr)
