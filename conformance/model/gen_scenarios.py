#!/usr/bin/env python3
"""Generate model.normalization scenarios for the Go/Python parity check.

Everything here is pure: normalize_state / apply_patches / their helpers have
no clock or randomness, so goldens are exact.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'
SCENARIOS = {}

FILES_TEXT = ('查看附件\n# Files mentioned by the user:\n- /tmp/a.png\n\n'
              '## My request:\n\r\n\n真正的请求\n\r\n')

RICH_ITEMS = [
    {'id': 'u1', 'type': 'userMessage', 'clientId': 'sub-9', 'status': 'done',
     'content': [{'type': 'text', 'text': FILES_TEXT}]},
    {'id': 'u2', 'type': 'userMessage', 'content': [{'type': 'text', 'text': 'no client id'}]},
    {'id': 's1', 'type': 'steeringUserMessage', 'status': 'accepted',
     'input': [{'type': 'inputText', 'text': '转向中'}]},
    {'id': 'a1', 'type': 'agentMessage', 'text': '回答', 'phase': 'final'},
    {'id': 'a2', 'type': 'agentMessage'},
    {'id': 'im1', 'type': 'imageView', 'path': 'file://localhost/tmp/pic%20a.png'},
    {'id': 'im2', 'type': 'ImageView', 'path': 'file:///D:/win%20dir/x.txt'},
    {'id': 'im3', 'type': 'imageView', 'path': '/local/path.png'},
    {'id': 'im4', 'type': 'imageView', 'path': 'https://example.com/x.png'},
    {'id': 'im5', 'type': 'imageView', 'path': ''},
    {'id': 'r1', 'type': 'reasoning', 'summary': ['想到 A', '想到 B']},
    {'id': 'c1', 'type': 'commandExecution', 'command': 'npm test', 'aggregatedOutput': 'ok'},
    {'id': 'f1', 'type': 'fileChange', 'changes': [{'path': 'a.py', 'diff': '+1'},
                                                   {'path': 'b.py'}]},
    {'id': 'm1', 'type': 'mcpToolCall', 'server': 'srv', 'tool': 'tl',
     'arguments': {'a': 1, 'b': '中文'}, 'result': {'ok': True}},
    {'id': 'm2', 'type': 'mcpToolCall', 'toolName': 'only', 'error': 'bad'},
    {'id': 't1', 'type': 'toolCall', 'title': '自定义标题', 'text': 'x', 'output': 'y'},
    {'id': 't2', 'type': 'toolCall', 'text': 't', 'output': 'o', 'subagentId': 'sa-1',
     'subagentType': 'general', 'subagentDescription': '描述', 'subagentPrompt': '提示',
     'stopReason': 'end_turn'},
    {'id': 't3', 'type': 'subagent', 'subagentDescription': '只带描述'},
    {'id': 'e1', 'type': 'error'},
    {'id': 'e2', 'type': 'error', 'message': 'boom'},
    {'id': 'p1', 'type': 'planImplementation', 'planContent': '计划内容'},
    {'id': 'x1', 'type': 'weirdKind', 'whatever': [1, 'two', {'three': None}]},
    {'id': 'fc1', 'type': 'function_call', 'call_id': 'call-1', 'id': 'i-fc', 'name': 'npm',
     'arguments': '{"verbose": true}'},
    {'id': 'fo1', 'type': 'function_call_output', 'call_id': 'call-1',
     'output': {'status': 'done', 'items': [1, 2]}},
    {'id': 'ct1', 'type': 'custom_tool_call', 'call_id': 'call-2', 'input': 'raw input'},
    {'id': 'cto1', 'type': 'custom_tool_call_output', 'call_id': 'call-2', 'output': 'done'},
    {'id': 'fo2', 'type': 'function_call_output', 'call_id': 'orphan', 'output': 'x'},
    {'id': 'u3', 'type': 'userMessage', 'content': [
        {'type': 'text', 'text': '带附件'},
        {'type': 'image', 'url': '/img.png', 'name': '截图'},
        {'type': 'input_text', 'text': 'filtered'},
    ]},
]

SCENARIOS['01_normalize_items'] = [
    {'op': 'normalize_state', 'state': {
        'id': 'sess', 'title': '标题', 'turns': [
            {'turnId': 'turn-1', 'status': 'completed', 'turnStartedAtMs': 123,
             'items': RICH_ITEMS}]},
     'connected': False},
    {'op': 'normalize_state', 'state': {
        'sessionId': 'via-session', 'turns': [
            {'turnId': 'turn-2', 'status': 'inProgress', 'items': RICH_ITEMS}]}},
]

CANONICAL_HISTORY = {
    'kind': 'canonical',
    'history': {
        'entitiesByKey': {
            't1': {'turnId': 't1', 'status': 'completed',
                   'items': {'entitiesByKey': {'i1': {'id': 'i1', 'type': 'agentMessage',
                                                      'text': '来自实体图'}},
                             'islands': [{'entries': [{'value': 'i1'}, {'value': 'nope'},
                                                      {'value': 'i1'}]}]}},
            'dup': {'turnId': 'dup', 'items': []}},
        'islands': [{'entries': [{'value': 't1'}, {'value': 'ghost'},
                                 {'value': 't1'}, {'value': 'dup'}]}],
        'isComplete': False}}

SCENARIOS['02_history_and_meta'] = [
    {'op': 'normalize_state', 'state': {
        'turnHistory': CANONICAL_HISTORY,
        'turns': [{'turnId': 'ignored', 'items': []}],
        'latestThreadSettings': {'serviceTier': 'flex', 'effort': 'x'}}},
    {'op': 'normalize_state', 'state': {
        'turns': [{'turnId': '', 'params': {'input': [{'type': 'text', 'text': '开场白'}]},
                   'items': [{'id': 'a', 'type': 'agentMessage', 'text': '答'}]},
                  {'turnId': 'history', 'params': {'serviceTier': 'priority',
                                                  'input': '纯字符串输入'}},
                  {'status': 'queued', 'params': {'serviceTier': 'ignored-tier'}}],
        'completedThreadGoal': {'goal': '已完成目标'},
        'turnsPagination': {'hasLoadedOldest': False}}},
    {'op': 'normalize_state', 'state': {
        'turns': [], 'threadGoal': {'goal': '进行中目标'},
        'latestReasoningEffort': 'high',
        'threadRuntimeStatus': {'type': 'running'},
        'latestCollaborationMode': {'mode': 'review'},
        'turnHistory': {'kind': 'other', 'history': {'isComplete': True}}}},
]

REPLY_KEY = json.dumps(['request_user_input_async', 'q1', 0], ensure_ascii=False,
                       separators=(',', ':'))

SCENARIOS['03_requests'] = [
    {'op': 'normalize_state', 'state': {
        'turns': [
            {'turnId': 't-cmd', 'items': [
                {'id': 'ci', 'type': 'commandExecution', 'command': 'related-cmd',
                 'changes': [{'path': 'x'}]}]},
            {'turnId': 't-file', 'items': [
                {'id': 'fi', 'type': 'fileChange',
                 'changes': [{'path': 'a.py', 'diff': '+1'}]}]},
            {'turnId': 't-last', 'status': 'inProgress', 'items': [
                {'id': 'qu', 'type': 'userMessage', 'content': [{'type': 'text', 'text':
                    '<send_user_message_question_reply>[{"questionItemId": ' + json.dumps(REPLY_KEY) +
                    ', "question": "问", "answer": "答"}]</send_user_message_question_reply>'}]},
                {'id': 'q1', 'type': 'agentMessage', 'text': '问一下',
                 'questions': [{'title': '选择', 'options': ['甲', '乙']},
                               {'title': '第二问', 'options': []}]},
                {'id': 'q2', 'type': 'agentMessage', 'questions': [
                    {'title': '全答完', 'options': ['x']}]}]}],
        'requests': [
            {'id': 'r1', 'method': 'item/commandExecution/requestApproval',
             'params': {'itemId': 'ci', 'reason': 'why', 'cwd': '/w', 'networkApprovalContext': 'n'}},
            {'id': 'r2', 'method': 'item/fileChange/requestApproval',
             'params': {'itemId': 'fi', 'reason': 'r', 'grantRoot': '/g'}},
            {'id': 'r3', 'method': 'item/fileChange/requestApproval',
             'params': {'itemId': 'ghost', 'reason': 'r2'}},
            {'id': 'r4', 'method': 'item/permissions/requestApproval',
             'params': {'reason': '需要权限', 'permissions': ['net'],
                        '_meta': {'nested': ['openai/userVerification']}}},
            {'id': 'r5', 'method': 'mcpServer/elicitation/request',
             'params': {'serverName': 's', 'message': 'm', 'mode': 'form',
                        'requestedSchema': {'type': 'object', 'properties': {
                            'q': {'type': 'string', 'minLength': 1},
                            'n': {'type': 'integer', 'maximum': 9}}},
                        '_meta': {'codex_approval_kind': 'mcp_tool_call',
                                  'connector_id': 'Computer Use PRO!!',
                                  'tool_params': {'app': '浏览器'},
                                  'persist': ['always', 'session', 'junk']}}},
            {'id': 'r6', 'method': 'mcpServer/elicitation/request',
             'params': {'mode': 'json', 'requestedSchema': {}}},
            {'id': 'r7', 'method': 'mcpServer/elicitation/request',
             'params': {'mode': 'form', 'requestedSchema': {
                 'type': 'object', 'extra': 1, 'properties': {}}}},
            {'id': 'r8', 'method': 'tool/requestUserInput',
             'params': {'questions': [{'id': 'q', 'title': 't'}]}},
            {'id': 'r9', 'method': 'acp/requestPermission',
             'params': {'title': '允许', 'command': 'ls', 'options': ['allow']}},
            {'id': 'r10', 'method': 'unknown/method', 'params': {'x': 1}},
        ]}},
    {'op': 'normalize_state', 'state': {
        'turns': [{'turnId': 'done', 'status': 'completed', 'items': [
            {'id': 'q9', 'type': 'agentMessage', 'questions': [
                {'title': '完成后仍可问', 'options': None}]}]}]}},
    {'op': 'normalize_state', 'state': {
        'turns': [{'turnId': 'queued', 'status': 'queued', 'items': [
            {'id': 'qx', 'type': 'agentMessage', 'questions': [
                {'title': '不该出现', 'options': ['a']}]}]}]}},
]

SCENARIOS['04_patches_and_helpers'] = [
    {'op': 'apply_patches', 'state': {'a': 1},
     'patches': [{'op': 'replace', 'path': '', 'value': {'fresh': True}}]},
    {'op': 'apply_patches', 'state': {'b': [1, 2], 'o': {}},
     'patches': [{'path': '/b/1', 'op': 'add', 'value': 9},
                 {'path': ['b', '-'], 'op': 'add', 'value': 'end'},
                 {'path': ['b', 0], 'op': 'remove'},
                 {'path': ['o', 'k~1x'], 'op': 'replace', 'value': True},
                 {'path': ['o', 'j~0'], 'op': 'add', 'value': None}]},
    {'op': 'apply_patches', 'state': [1], 'patches': [{'op': 'add', 'value': 2}]},
    {'op': 'apply_patches', 'state': [1], 'patches': [{'path': [0], 'op': 'copy'}]},
    {'op': 'apply_patches', 'state': {}, 'patches': [{'path': ['x'], 'op': 'delete'}]},
    {'op': 'text_content', 'content': '直接字符串'},
    {'op': 'text_content', 'content': [{'type': 'text', 'text': '第一段'},
                                       {'type': 'inputText', 'text': '第二段'},
                                       {'type': 'image', 'url': '跳过'},
                                       {'type': 'output_text', 'text': '第三段'},
                                       'not-a-dict']},
    {'op': 'text_content', 'content': {'items': ['忽略']}},
    {'op': 'user_display_text', 'text': '没有标记的普通文本'},
    {'op': 'user_display_text', 'text': '# Files mentioned by the user:\n- x\n\n别的'},
    {'op': 'user_display_text', 'text': '# Files mentioned by the user:\n\n## My request:\n\n\r\n修剪\r\n'},
    {'op': 'user_display_text', 'text': '# Files mentioned by the user:\n\n## My request:\n\n'},
    {'op': 'question_replies', 'text': '<send_user_message_question_reply>'
     + json.dumps([{'questionItemId': 'k', 'question': 'q', 'answer': 'a'}], ensure_ascii=False)
     + '</send_user_message_question_reply>'},
    {'op': 'question_replies', 'text': '<send_user_message_question_reply>["不是对象"]'
     + '</send_user_message_question_reply>'},
    {'op': 'question_replies', 'text': '<send_user_message_question_reply>[broken'
     + '</send_user_message_question_reply>'},
    {'op': 'question_replies', 'text': '前置 <send_user_message_question_reply>[]'
     '</send_user_message_question_reply>'},
    {'op': 'form_supported', 'schema': {'type': 'object', 'title': 't',
                                        'properties': {'s': {'type': 'string'},
                                                       'n': {'minimum': 1},
                                                       'b': {'enum': [1]}}}},
    {'op': 'form_supported', 'schema': {'type': 'array'}},
    {'op': 'form_supported', 'schema': {'type': 'object', 'surprise': 1}},
    {'op': 'form_supported', 'schema': {'type': 'object',
                                        'properties': {'bad': {'type': 'object'}}}},
    {'op': 'normalize_request', 'request': {'id': 'n1', 'method': 'item/tool/requestUserInput',
                                            'params': {'questions': [], 'extra': 'drop'}}},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
