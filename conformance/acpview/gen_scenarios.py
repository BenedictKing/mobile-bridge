#!/usr/bin/env python3
"""Generate ACP session-view scenarios for the Go/Python parity check.

Pure helpers (form validation, output flattening, command summaries, subagent
metadata) plus a scripted AcpSessionView driven through every update kind and
snapshot. The clock is injected so begin_turn's startedAt is deterministic.
"""
import json
from pathlib import Path

SCEN_DIR = Path(__file__).resolve().parent / 'scenarios'
SCENARIOS = {}

FORM = {'type': 'object', 'required': ['mode'], 'properties': {
    'mode': {'type': 'string', 'title': '模式', 'enum': ['快', '慢'],
             'oneOf': [{'const': '快'}, {'const': '慢'}]},
    'tags': {'type': 'array', 'items': {'enum': ['a', 'b']},
             'title': '标签'},
    'count': {'type': 'integer', 'minimum': 1, 'maximum': 9, 'title': '数量'},
    'ratio': {'type': 'number'},
    'agree': {'type': 'boolean', 'title': '同意'},
    'note': {'type': 'string', 'minLength': 2, 'maxLength': 10},
}}

SCENARIOS['01_form'] = [
    {'op': 'form_supported', 'schema': FORM},
    {'op': 'form_supported', 'schema': {'type': 'object', 'properties': {}}},
    {'op': 'form_supported', 'schema': {'type': 'object', 'properties': {
        'x': {'type': 'object'}}}},
    {'op': 'form_supported', 'schema': {'type': 'object', 'required': ['nope'],
                                        'properties': {'x': {'type': 'string'}}}},
    {'op': 'validate_form', 'schema': FORM,
     'content': {'mode': '快', 'tags': ['a', 'b'], 'count': 3, 'ratio': 0.5,
                 'agree': True, 'note': '备注'}},
    {'op': 'validate_form', 'schema': FORM, 'content': 'not-a-dict'},
    {'op': 'validate_form', 'schema': FORM, 'content': {}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'extra': 1}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'note': 'x'}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'tags': []}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'tags': ['c']}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'count': 2.5}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'count': 99}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'agree': 'yes'}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'note': '一'}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '快', 'note': '长了超出限制的文本'}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'mode': '别的', 'note': '有效文本'}},
    {'op': 'validate_form', 'schema': FORM, 'content': {'note': '有效文本'}},
]

SCENARIOS['02_flatten'] = [
    {'op': 'output', 'value': '纯字符串'},
    {'op': 'output', 'value': {'text': '一', 'result': {'output': '二', 'empty': ''}}},
    {'op': 'output', 'value': {'only_null': None, 'nums': [1, 2]}},
    {'op': 'output', 'value': [{'text': 'a'}, 'b', {'summary': None}, []]},
    {'op': 'output', 'value': 42},
    {'op': 'command_summary', 'value': 'rm -rf /tmp'},
    {'op': 'command_summary', 'value': {'command': ' git status ', 'cmd': 'ignored'}},
    {'op': 'command_summary', 'value': {'input': {'arguments': {'shellCommand': 'npm test'}}}},
    {'op': 'command_summary', 'value': {'path': '  ', 'whatever': {'z': 1, 'a': [True]}}},
    {'op': 'command_summary', 'value': ['not', 'a', 'dict']},
    {'op': 'subagent_meta', 'input': {'agentId': 'sa-9', 'description': '描述'}, 'title': ''},
    {'op': 'subagent_meta',
     'input': {'input': {'actual_subagent_type': 'general', 'stop_reason': 'end'}},
     'title': '普通标题'},
    {'op': 'subagent_meta', 'input': {'prompt': 42}, 'title': '子 Agent 任务'},
    {'op': 'subagent_meta', 'input': {'agent_id': '  ', 'prompt': ' p '}, 'title': ''},
    {'op': 'subagent_meta', 'input': {'nested': {'deep': {'agentId': 'x'}}}, 'title': ''},
]

VIEW_STEPS = [
    {'apply_config': {'options': [
        {'id': 'model', 'currentValue': 'kimi-k3'},
        {'category': 'thought_level', 'currentValue': 'high'},
        {'id': 'ignored', 'currentValue': None}], 'modes': {'currentModeId': 'plan'}}},
    {'snapshot': True},
    {'begin_turn': '第一句提问'},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '回答'}}},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '（续）'}}},
    {'update': {'sessionUpdate': 'agent_thought_chunk', 'content': {'text': '想一想'}}},
    {'update': {'sessionUpdate': 'tool_call', 'toolCallId': 'tc-1', 'title': 'Bash',
                'kind': 'execute', 'status': 'in_progress',
                'rawInput': {'command': 'ls -la'},
                'content': [{'text': '文件列表'}, {'output': 'done'}]}},
    {'update': {'sessionUpdate': 'tool_call', 'toolCallId': 'tc-2', 'status': 'completed',
                'rawInput': {'agentId': 'sa-1', 'description': '搜索'},
                'content': '子代理输出'}},
    {'update': {'sessionUpdate': 'tool_call_update', 'toolCallId': 'tc-1',
                'status': 'completed', 'title': 'Bash 更新', 'content': '完整输出'}},
    {'update': {'sessionUpdate': 'tool_call_update', 'toolCallId': 'tc-2',
                'rawInput': {'subagent_type': 'researcher'}}},
    {'update': {'sessionUpdate': 'plan', 'entries': [
        {'content': '步骤一', 'status': 'completed'},
        {'content': '步骤二', 'status': 'in_progress'},
        '裸字符串',
        {'content': None, 'status': None}]}},
    {'update': {'sessionUpdate': 'plan', 'entries': [
        {'content': '步骤一', 'status': 'completed'},
        {'content': '步骤二', 'status': 'completed'}]}},
    {'update': {'sessionUpdate': 'plan', 'entries': []}},
    {'snapshot': True},
    {'update': {'sessionUpdate': 'plan', 'entries': [{'content': '重建', 'status': 'pending'}]}},
    {'snapshot': True},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '前导'},
                'messageId': 'm-1'}},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '正文'},
                'messageId': 'm-2'}},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '（续）'},
                'messageId': 'm-2'}},
    {'snapshot': True},
    {'update': {'sessionUpdate': 'user_message_chunk', 'content': {'text': '中途追问'}}},
    {'add_permission': {'id': 'req-1', 'params': {
        'toolCall': {'title': '危险命令', 'rawInput': {'cmd': 'rm x'}},
        'options': [{'optionId': 'allow', 'name': '允许', 'kind': 'allow_once'},
                    {'name': '坏形状'}, 'not-dict']}}},
    {'add_input': {'id': 'req-2', 'params': {
        'message': '选择模式', 'requestedSchema': {'type': 'object', 'properties': {
            'q': {'type': 'string', 'enum': ['是', '否']}}}}}},
    {'add_input': {'id': 'req-3', 'params': {
        'message': '复杂表单', 'requestedSchema': {'type': 'object', 'properties': {
            'x': {'type': 'object'}}}}}},
    {'snapshot': True},
    {'remove_permission': 'req-1'},
    {'update': {'sessionUpdate': 'available_commands_update',
                'availableCommands': [{'name': '/plan'}, {'noname': 1}, 'junk']}},
    {'update': {'sessionUpdate': 'current_mode_update', 'currentModeId': 'chat'}},
    {'update': {'sessionUpdate': 'config_option_update',
                'configOptions': [{'category': 'model', 'currentValue': 'kimi-k4'}]}},
    {'update': {'sessionUpdate': 'config_option_update',
                'configOption': {'category': 'mode', 'currentValue': 'agent'}}},
    {'update': {'sessionUpdate': 'unknown_kind', 'whatever': 1}},
    {'snapshot': True},
    {'end_turn': 'end_turn'},
    {'fail_turn': '出错了'},
    {'snapshot': True},
    {'begin_turn': '重试提问'},
    {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '部分回答'}}},
    {'end_turn': 'cancelled'},
    {'snapshot': True},
]

SCENARIOS['03_view'] = [
    {'op': 'view', 'now': 1700000000.5, 'steps': VIEW_STEPS},
    {'op': 'view', 'now': 1700000000.5, 'steps': [
        {'update': {'sessionUpdate': 'agent_message_chunk', 'content': {'text': '无开场'}}},
        {'finish_replay': True},
        {'snapshot': True}]},
]

for name, steps in SCENARIOS.items():
    SCEN_DIR.mkdir(parents=True, exist_ok=True)
    (SCEN_DIR / f'{name}.json').write_text(
        json.dumps({'name': name, 'steps': steps}, ensure_ascii=False, indent=1) + '\n',
        encoding='utf-8')
print(f'wrote {len(SCENARIOS)} scenarios to {SCEN_DIR}')
