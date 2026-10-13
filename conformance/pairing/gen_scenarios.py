#!/usr/bin/env python3
"""Generate pairing scenarios for the Go/Python parity check.

phone_origin is pure; Pairing.control/exchange get an injected clock, token
and identifier source, and a deterministic session issuer.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'
SCENARIOS = {}

SCENARIOS['01_phone_origin'] = [
    {'op': 'phone_origin', 'value': v} for v in [
        'https://example.com', 'http://192.168.1.5:8787', 'https://a.b.c/path',
        'http://x/y', 'https://x?q=1', 'https://x#f', 'ftp://x', 'https://',
        'https://u:p@x', 'https://127.0.0.1:8787', 'http://localhost:9000',
        'http://dev.localhost', 'http://[::1]:8787', 'http://[fe80::1]',
        'HTTPS://EXAMPLE.COM:443', 'https://xn--fiqs8s.example', 'https://x/',
        'https://x//', 'not a url', 42,
    ]
]

TOKEN = 't' * 43

SCENARIOS['02_control'] = [
    {'op': 'create', 'url': 'https://example.com', 'now': 1000,
     'token': TOKEN, 'id': 'id-1'},
    {'op': 'status_single', 'id': 'id-1', 'now': 1001},
    {'op': 'status_list', 'ids': ['id-1', 'missing', 5, None], 'now': 1002},
    {'op': 'create', 'url': 'https://example.com', 'now': 1100,
     'token': 'u' * 43, 'id': 'id-2'},
    {'op': 'status_list', 'ids': ['id-1', 'id-2'], 'now': 1101},
    {'op': 'create', 'url': 'http://192.168.1.5:8787', 'now': 1200,
     'token': 'v' * 43, 'id': 'id-3'},
    {'op': 'create', 'url': 'https://not-in-origins.example', 'now': 1201,
     'token': 'w' * 43, 'id': 'id-4'},
    {'op': 'revoke', 'id': 'id-3', 'now': 1202},
    {'op': 'status_single', 'id': 'id-3', 'now': 1203},
    {'op': 'status_single', 'id': 'id-2', 'now': 9999},
    {'op': 'bogus', 'now': 1204},
]

SCENARIOS['03_exchange'] = [
    {'op': 'create', 'url': 'https://example.com', 'now': 5000,
     'token': TOKEN, 'id': 'grant-1'},
    {'op': 'exchange', 'token': TOKEN, 'origin': 'https://example.com',
     'address': '203.0.113.7', 'now': 5100, 'session': 'first'},
    {'op': 'exchange', 'token': TOKEN, 'origin': 'https://example.com',
     'address': '203.0.113.7', 'now': 5101, 'session': 'again'},
    {'op': 'create', 'url': 'https://example.com', 'now': 5200,
     'token': 'a' * 43, 'id': 'grant-2'},
    {'op': 'exchange', 'token': 'b' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.8', 'now': 5201, 'session': 'miss'},
    {'op': 'exchange', 'token': 'a' * 43, 'origin': 'http://192.168.1.5:8787',
     'address': '203.0.113.8', 'now': 5202, 'session': 'wrong-origin'},
    {'op': 'exchange', 'token': 'a' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.8', 'now': 6000, 'session': 'expired'},
    {'op': 'exchange', 'token': 'short', 'origin': 'https://example.com',
     'address': '203.0.113.8', 'now': 6001, 'session': 'bad-length'},
    {'op': 'exchange', 'token': 'c' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6100, 'session': 'burst-1'},
    {'op': 'exchange', 'token': 'd' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6101, 'session': 'burst-2'},
    {'op': 'exchange', 'token': 'e' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6102, 'session': 'burst-3'},
    {'op': 'exchange', 'token': 'f' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6103, 'session': 'burst-4'},
    {'op': 'exchange', 'token': 'g' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6104, 'session': 'burst-5'},
    {'op': 'exchange', 'token': 'h' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6105, 'session': 'burst-6'},
    {'op': 'exchange', 'token': 'i' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6106, 'session': 'burst-7'},
    {'op': 'exchange', 'token': 'j' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6107, 'session': 'burst-8'},
    {'op': 'exchange', 'token': 'k' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6108, 'session': 'burst-9'},
    {'op': 'exchange', 'token': 'l' * 43, 'origin': 'https://example.com',
     'address': '203.0.113.9', 'now': 6400, 'session': 'after-window'},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
