#!/usr/bin/env python3
"""Generate language-neutral timeline scenarios for the Go/Python parity check.

Scenarios are plain JSON so the Python oracle and the Go port replay the exact
same inputs. Cross-step references use "$N.path.to.field" tokens resolved
against earlier step results (see oracle.py / scenario_test.go).

Run:  python3 conformance/timeline/gen_scenarios.py
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'
TEXT_PREVIEW, DETAIL_CHARS = 8000, 16000


def messages(count, text='message '):
    return [{'id': str(i), 'kind': 'agentMessage', 'role': 'assistant',
             'text': text + str(i)} for i in range(count)]


def view(count=320, sequence=1, turns=None, **extra):
    body = {'sequence': sequence,
            'turns': turns if turns is not None
            else [{'id': 'turn', 'messages': messages(count)}]}
    body['requests'] = [{'id': 'approval'}]
    body.update(extra)
    return body


def chunks(length, step=DETAIL_CHARS):
    offsets, offset = [], 0
    while offset < length:
        offsets.append(offset)
        offset += step
    return offsets


def state_op():
    return {'op': 'state'}


SCENARIOS = {}

# --- basic paging, backfill chain, changes after append, bad epoch ------------
SCENARIOS['01_basic_pagination'] = [
    {'op': 'page'},
    state_op(),
    {'op': 'update', 'view': view(320, 1)},
    {'op': 'page'},
    {'op': 'page', 'limit': 80, 'before': '$3.before'},
    {'op': 'page', 'limit': 100, 'before': '$4.before'},
    state_op(),
    {'op': 'update', 'view': view(323, 2)},
    {'op': 'changes', 'after': 1, 'epoch': '$3.epoch', 'start': '$4.before'},
    {'op': 'changes', 'after': 2, 'epoch': '$3.epoch', 'start': '$4.before'},
    {'op': 'changes', 'after': 1, 'epoch': 'deadbeef', 'start': '$4.before'},
    {'op': 'page', 'limit': 0},
    {'op': 'page', 'limit': 1000},
]

# --- same-key streaming edit, version flips, empty delta ----------------------
SCENARIOS['02_streaming'] = [
    {'op': 'update', 'view': view(5, 1)},
    {'op': 'page'},
    {'op': 'update', 'view': view(5, 2, turns=[{'id': 'turn', 'messages': messages(4) + [
        {'id': '4', 'kind': 'agentMessage', 'role': 'assistant', 'text': 'message 4 streaming'}]}])},
    {'op': 'changes', 'after': 1, 'epoch': '$1.epoch', 'start': '$1.before'},
    {'op': 'changes', 'after': 2, 'epoch': '$1.epoch', 'start': '$1.before'},
    {'op': 'page'},
]

# --- activity bounds + lossless detail chunking + stale cursor ----------------
SCENARIOS['03_activity_detail'] = [
    {'op': 'update', 'view': view(0, 1, turns=[{'id': 'turn', 'messages': [
        {'id': str(i), 'kind': 'agentMessage', 'role': 'activity',
         'text': '长日志 🚀 \n' * 10000} for i in range(3)] + [
        {'id': 'final', 'kind': 'agentMessage', 'role': 'assistant', 'text': '完成'}]}])},
    {'op': 'page'},
    *[{'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.0.key']},
       'offset': offset, 'version': '$1.rows.0.version'} for offset in chunks(70000)],
    {'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.0.key']},
     'offset': 16000, 'version': 'wrong-version'},
    {'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.0.key']},
     'offset': 999999, 'version': '$1.rows.0.version'},
    {'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.3.key']}, 'offset': -5},
    {'op': 'update', 'view': view(0, 2, turns=[{'id': 'turn', 'messages': messages(2)}])},
    {'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.0.key']}},
    {'op': 'page', 'before': '$1.rows.2.key'},
]

# --- page byte budget and EOF behaviour ----------------------------------------
SCENARIOS['04_budget_eof'] = [
    {'op': 'update', 'view': view(0, 1, turns=[{'id': 'turn', 'messages': [
        {'id': str(i), 'kind': 'agentMessage', 'role': 'assistant', 'text': '内容' * 10000}
        for i in range(30)]}])},
    {'op': 'page', 'limit': 100},
    *[{'op': 'page', 'limit': 100, 'before': f'${i + 1}.before'} for i in range(5)],
    {'op': 'cursor', 'epoch': '$1.epoch', 'key': '$1.rows.0.key'},
    {'op': 'page', 'limit': 100, 'before': '$7'},
]

# --- source replacement resets; version window trims to 16 ---------------------
SCENARIOS['05_replacement_trim'] = [
    {'op': 'update', 'view': view(320, 1)},
    {'op': 'page'},
    {'op': 'update', 'view': view(310, 2)},
    {'op': 'page', 'before': '$1.before'},
    {'op': 'changes', 'after': 1, 'epoch': '$1.epoch'},
    *[{'op': 'update', 'view': view(320, sequence)} for sequence in range(3, 27)],
    state_op(),
    {'op': 'changes', 'after': 1, 'epoch': '$3.epoch'},
    {'op': 'changes', 'after': 26, 'epoch': '$3.epoch'},
]

# --- plan rows, turn errors, goal activations, attachments, dup identities -----
def plan_turn():
    return {'id': 'plan-turn', 'actionable': True, 'status': 'completed', 'messages': [
        {'id': 'u1', 'kind': 'userMessage', 'role': 'user', 'text': '列个计划'},
        {'kind': 'plan', 'role': 'activity', 'title': '计划', 'status': 'in_progress',
         'text': '▸ one', 'entries': [{'content': 'one', 'status': 'pending'},
                                      {'content': 'two ' + '很' * 30, 'status': 'pending'}]},
        {'id': 'dup', 'kind': 'agentMessage', 'role': 'assistant', 'text': '第一段'},
        {'id': 'dup', 'kind': 'agentMessage', 'role': 'assistant', 'text': '第二段 同 id'},
        {'role': 'assistant', 'kind': 'agentMessage', 'phase': 'commentary', 'text': '旁白不可 fork'},
        {'role': 'assistant', 'kind': 'agentMessage', 'text': '正文可 fork', 'output': '附加输出'},
        {'role': 'activity', 'kind': 'tool', 'text': '短活动', 'requestId': 'req-other',
         'attachments': [{'type': 'image', 'name': '屏幕 截图.png', 'path': '/tmp/a b/c.png'},
                          {'type': 'log', 'name': 42, 'path': True},
                          {'name': None, 'extra': 'dropped'}]},
    ]}


def plan_view(sequence):
    turns = [plan_turn(),
             {'id': 'err-str', 'error': 'boom', 'messages': []},
             {'id': 'err-dict', 'error': {'code': 4, 'msg': '坏了'}, 'messages': []},
             {'id': 'activated', 'actionable': False, 'messages': [
                 {'id': 'skipme', 'kind': 'userMessage', 'role': 'user',
                  'text': '被目标激活吞掉', 'requestId': 'req-act'}]}]
    if sequence > 1:
        plan = dict(turns[0]['messages'][1], status='completed', text='✓ one\n▸ two',
                    entries=[{'content': 'one', 'status': 'completed'},
                             {'content': 'two ' + '很' * 30, 'status': 'in_progress'}])
        turns[0] = dict(turns[0], status='running',
                        messages=[turns[0]['messages'][0], plan] + turns[0]['messages'][2:])
    return view(0, sequence, turns=turns, goalActivationIds=['req-act'], savedHistoryMore=True)


SCENARIOS['06_plan_errors_misc'] = [
    {'op': 'update', 'view': plan_view(1)},
    {'op': 'page', 'limit': 100},
    {'op': 'cursor', 'epoch': '$1.epoch', 'key': '$1.rows.0.key'},
    {'op': 'detail', 'cursor': '$2'},
    {'op': 'update', 'view': plan_view(2)},
    {'op': 'changes', 'after': 1, 'epoch': '$1.epoch'},
    {'op': 'page', 'limit': 3},
]

# --- prepending turns keeps keys/orders (order_origin goes negative) -----------
SCENARIOS['07_order_origin'] = [
    {'op': 'update', 'view': view(0, 1, turns=[{'id': 'A', 'messages': messages(2)}])},
    {'op': 'page'},
    {'op': 'update', 'view': view(0, 2, turns=[{'id': 'B', 'messages': messages(2)},
                                               {'id': 'A', 'messages': messages(2)}])},
    {'op': 'page', 'limit': 10},
    {'op': 'changes', 'after': 1, 'epoch': '$1.epoch'},
]

# --- string escaping, passthrough meta, whitelist copy order -------------------
WHITELIST_MSG = {
    'stopReason': 'end_turn', 'subagentPrompt': 'sub prompt', 'subagentDescription': '子代理说明',
    'subagentType': 'general', 'subagentId': 'sub-1', 'requestId': 'req-9',
    'phase': 'final', 'title': '标题 "引号" <b>&amp;', 'status': 'streaming',
    'kind': 'agentMessage', 'role': 'assistant', 'text': '<b>&"\\\n\t 中文🚀',
    'id': 'wl-1'}

SCENARIOS['08_escapes_passthrough'] = [
    {'op': 'update', 'view': {'sequence': 1, 'turns': [{'id': '转义·turn', 'messages': [
        WHITELIST_MSG,
        {'id': 'floats', 'kind': 'agentMessage', 'role': 'assistant', 'text': 'x'}]}],
        'requests': [], 'nested': {'b': 1.0, 'a': [1, 2.5, 'x', None, True]},
        'big': 12345678901234567890, 'unicodekey': '值'}},
    {'op': 'page', 'limit': 10},
]

# --- cursor edge cases ---------------------------------------------------------
SCENARIOS['09_cursor_edges'] = [
    {'op': 'update', 'view': view(10, 1)},
    {'op': 'page'},
    {'op': 'cursor', 'epoch': '$1.epoch', 'key': '$1.rows.0.key'},
    {'op': 'page', 'limit': 5, 'before': '$2'},
    {'op': 'page', 'before': 'no-dot-cursor'},
    {'op': 'page', 'before': {'$concat': ['wrongepoch.', '$1.rows.0.key']}},
    {'op': 'page', 'before': {'$concat': ['$1.epoch', '.nonexistent-key']}},
    {'op': 'detail', 'cursor': {'$concat': ['$1.epoch', '.', '$1.rows.0.key']}},
    {'op': 'detail', 'cursor': ''},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
