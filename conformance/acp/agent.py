#!/usr/bin/env python3
"""Deterministic fake ACP agent for the transport parity check.

Logs every received stdin line verbatim to argv[1] (the wire contract) and
answers from a fixed script: initialize/echo/fail/banner/orphan/poke/reverse.
"""
import json
import sys

LOG = open(sys.argv[1], 'w', encoding='utf-8')
PENDING_REVERSE = None


def send(message):
    sys.stdout.write(json.dumps(message, ensure_ascii=False) + '\n')
    sys.stdout.flush()


def reply(ident, result):
    send({'jsonrpc': '2.0', 'id': ident, 'result': result})


for line in sys.stdin:
    LOG.write(line if line.endswith('\n') else line + '\n')
    LOG.flush()
    try:
        message = json.loads(line)
    except ValueError:
        continue
    if 'method' in message and 'id' in message:
        method, ident, params = message['method'], message['id'], message.get('params')
        if method == 'initialize':
            reply(ident, {'protocolVersion': 1,
                          'agentCapabilities': {'loadSession': True},
                          'agentInfo': {'name': '假 Agent', 'version': '1.0'},
                          'authMethods': ['local']})
        elif method == 'echo':
            reply(ident, params)
        elif method == 'fail':
            send({'jsonrpc': '2.0', 'id': ident,
                  'error': {'code': -32000, 'message': '需要认证', 'data': {'hint': '登录'}}})
        elif method == 'fail_generic':
            send({'jsonrpc': '2.0', 'id': ident,
                  'error': {'code': -100, 'message': '普通失败', 'data': [1, 'x']}})
        elif method == 'null':
            send({'jsonrpc': '2.0', 'id': ident, 'result': None})
        elif method == 'banner':
            sys.stdout.write('# 非协议横幅 banner\n')
            sys.stdout.flush()
            reply(ident, 'banner-ok')
        elif method == 'orphan':
            send({'jsonrpc': '2.0', 'id': 9999, 'result': 'dropped'})
            reply(ident, 'orphan-ok')
        elif method == 'poke':
            reply(ident, 'poke-ok')
            send({'jsonrpc': '2.0', 'method': 'agent/notice',
                  'params': {'text': '异步通知 ✓'}})
        elif method == 'reverse':
            PENDING_REVERSE = ident
            send({'jsonrpc': '2.0', 'id': 'ag-1', 'method': 'ask',
                  'params': {'question': '反向请求'}})
        else:
            send({'jsonrpc': '2.0', 'id': ident,
                  'error': {'code': -32601, 'message': 'Method not found'}})
    elif 'id' in message and PENDING_REVERSE is not None and message['id'] == 'ag-1':
        reply(PENDING_REVERSE, {'echoed': message.get('result')})
        PENDING_REVERSE = None
LOG.close()
