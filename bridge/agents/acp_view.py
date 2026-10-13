"""ACP session view: synthesizes the normalized view contract from session/update streams."""
import copy
import json
import logging
import threading
import time

LOG = logging.getLogger(__name__)

TOOL_STATUS = {'pending': 'inProgress', 'in_progress': 'inProgress',
               'completed': 'completed', 'failed': 'failed'}

# Plan entries carry their own status vocabulary; these marks keep the plain
# text fallback (copy, detail endpoint) readable when entries are flattened.
PLAN_MARKS = {'completed': '✓', 'in_progress': '▸', 'pending': '○'}

# Bounds for one elicitation form the phone must render and answer.
MAX_FORM_FIELDS = 12
MAX_FORM_CHARS = 20000
MAX_FORM_ITEMS = 20


def _enum_values(field):
    """Extract the selectable string values of one elicitation property."""
    values = field.get('enum')
    if isinstance(values, list) and values:
        return [v for v in values if isinstance(v, str)]
    variants = field.get('oneOf') or field.get('anyOf') or []
    if isinstance(variants, list) and variants:
        picked = [v.get('const') for v in variants if isinstance(v, dict)]
        return [v for v in picked if isinstance(v, str)]
    return None


def acp_form_supported(schema):
    """Whether the phone can render this elicitation form schema.

    Wrappers build forms out of a tiny vocabulary (AskUserQuestion-style):
    string fields with a ``oneOf``/``enum`` pick list, multi-select arrays
    with ``items.anyOf`` consts, and free-text/number/boolean companions.
    Anything richer stays unsupported so the card points to the desktop.
    """
    if not isinstance(schema, dict) or schema.get('type', 'object') != 'object':
        return False
    properties = schema.get('properties')
    if not isinstance(properties, dict) or not 1 <= len(properties) <= MAX_FORM_FIELDS:
        return False
    required = schema.get('required')
    if required is not None and (not isinstance(required, list)
                                 or not set(required) <= set(properties)):
        return False
    if len(json.dumps(schema, ensure_ascii=False)) > 65536:
        return False
    for field in properties.values():
        if not isinstance(field, dict):
            return False
        allowed = {'type', 'title', 'description', 'enum', 'oneOf', 'anyOf', 'items',
                   'default', 'minLength', 'maxLength', 'minimum', 'maximum', '_meta'}
        if set(field) - allowed:
            return False
        kind = field.get('type', 'string')
        if kind not in ('string', 'number', 'integer', 'boolean', 'array'):
            return False
        if kind == 'array':
            items = field.get('items')
            if not isinstance(items, dict) or set(items) - {'anyOf', 'enum', 'oneOf'}:
                return False
    return True


def validate_acp_form(schema, content):
    """Check one elicitation answer against its schema; returns normalized content.

    Raises ``ValueError`` with a phone-friendly message on any mismatch, so an
    invalid answer keeps the form open instead of cancelling the agent's ask.
    """
    if not isinstance(content, dict):
        raise ValueError('请填写表单')
    schema = schema if isinstance(schema, dict) else {}
    properties = schema.get('properties') if isinstance(schema.get('properties'), dict) else {}
    if not properties:
        raise ValueError('此表单没有可填写的字段')
    if set(content) - set(properties):
        raise ValueError('回答包含未知字段')
    for key in schema.get('required') or []:
        if key not in content:
            raise ValueError('请填写所有必填项')
    result = {}
    for key, value in content.items():
        field = properties.get(key) or {}
        kind = field.get('type', 'string')
        choices = _enum_values(field) if kind in ('string', 'array') else None
        if kind == 'array':
            items = field.get('items') or {}
            choices = _enum_values(items) if choices is None else choices
            if not isinstance(value, list) or not value:
                raise ValueError('请至少选择一项：' + str(field.get('title') or key))
            if len(value) > MAX_FORM_ITEMS or any(not isinstance(v, str) for v in value):
                raise ValueError('选择数量超出限制：' + str(field.get('title') or key))
            if choices is not None:
                bad = [v for v in value if v not in choices]
                if bad:
                    raise ValueError('包含无效选项：' + str(field.get('title') or key))
            result[key] = value
            continue
        if kind == 'boolean':
            if not isinstance(value, bool):
                raise ValueError('请勾选或留空：' + str(field.get('title') or key))
            result[key] = value
            continue
        if kind in ('number', 'integer'):
            if not isinstance(value, (int, float)) or isinstance(value, bool):
                raise ValueError('请填写数字：' + str(field.get('title') or key))
            if kind == 'integer' and int(value) != value:
                raise ValueError('请填写整数：' + str(field.get('title') or key))
            minimum, maximum = field.get('minimum'), field.get('maximum')
            if minimum is not None and value < minimum or maximum is not None and value > maximum:
                raise ValueError('数值超出范围：' + str(field.get('title') or key))
            result[key] = value
            continue
        if not isinstance(value, str):
            raise ValueError('请填写文本：' + str(field.get('title') or key))
        if not field.get('minLength', 0) <= len(value) <= field.get('maxLength', MAX_FORM_CHARS):
            raise ValueError('文本长度超出限制：' + str(field.get('title') or key))
        if choices is not None and field.get('enum') is not None and value not in choices:
            # Plain-enum string fields are strict; oneOf lists may carry a skip
            # sentinel the wrapper interprets, so they stay open.
            raise ValueError('请选择有效的选项：' + str(field.get('title') or key))
        if not value:
            # Wrappers treat a blank "Other" companion as absent.
            if key in (schema.get('required') or []):
                raise ValueError('请填写所有必填项')
            continue
        result[key] = value
    return result


def acp_normalize(state, connected):
    """ACP state is already normalized; only the liveness flag is dynamic."""
    return {**state, 'connected': connected}


def _text(content):
    """Extract text from one ACP content block."""
    return content.get('text') or '' if isinstance(content, dict) else ''


def _output(items):
    """Flatten ACP tool output, including nested agent transcripts."""
    if isinstance(items, str):
        return items
    if isinstance(items, dict):
        parts = []
        for key in ('text', 'output', 'message', 'summary', 'content', 'result', 'transcript', 'events', 'messages'):
            if key in items:
                value = _output(items[key])
                if value:
                    parts.append(value)
        return '\n'.join(parts) or json.dumps(items, ensure_ascii=False, sort_keys=True)[:4000]
    if isinstance(items, list):
        return '\n'.join(part for part in (_output(item) for item in items) if part)
    return ''


def _command_summary(raw_input):
    """Extract the concrete command from ACP permission payloads.

    Agent wrappers do not agree on the raw input shape: some use ``command``
    or ``cmd``, while others nest it under ``input``/``arguments``. Preserve a
    bounded JSON fallback so a permission card never collapses to only a tool
    title such as ``Bash``.
    """
    if isinstance(raw_input, str):
        return raw_input
    if not isinstance(raw_input, dict):
        return ''
    preferred = ('command', 'cmd', 'script', 'shellCommand', 'path', 'file_path')
    for key in preferred:
        value = raw_input.get(key)
        if isinstance(value, str) and value.strip():
            return value
    for key in ('input', 'arguments', 'args', 'params', 'payload', 'toolCall'):
        value = raw_input.get(key)
        summary = _command_summary(value)
        if summary:
            return summary
    return json.dumps(raw_input, ensure_ascii=False, sort_keys=True)[:2000]


def _subagent_meta(raw_input, title=''):
    """Find common sub-agent identity fields across ACP wrapper payloads."""
    if not isinstance(raw_input, dict):
        return {}
    keys = {
        'agent_id': 'subagentId', 'agentId': 'subagentId', 'subagent_id': 'subagentId',
        'actual_subagent_type': 'subagentType', 'actualSubagentType': 'subagentType',
        'subagent_type': 'subagentType', 'description': 'subagentDescription',
        'prompt': 'subagentPrompt', 'stop_reason': 'stopReason', 'stopReason': 'stopReason',
    }
    found = {}
    pending = [raw_input]
    while pending:
        current = pending.pop(0)
        if not isinstance(current, dict):
            continue
        for key, output in keys.items():
            value = current.get(key)
            if isinstance(value, (str, int, float)) and str(value).strip() and output not in found:
                found[output] = str(value)
        for key in ('input', 'arguments', 'args', 'params', 'payload', 'toolCall'):
            value = current.get(key)
            if isinstance(value, dict):
                pending.append(value)
    if found or 'agent' in str(title).lower() or '子 agent' in str(title).lower():
        return found
    return {}


class AcpSessionView:
    """Thread-safe normalized view for one ACP session."""

    def __init__(self, session_id, title=None, cwd=None):
        self.session_id = session_id
        self.title = title
        self.cwd = cwd
        self.model = None
        self.thinking = None
        self.mode = None
        self.status = 'idle'
        self.turns = []
        self.requests = []
        self.commands = []
        self._lock = threading.Lock()
        self._ordinal = 0
        self._tool_index = {}

    def snapshot(self):
        """Deep-copy the normalized view for one LiveSession.state swap."""
        with self._lock:
            return {'id': self.session_id, 'title': self.title or '未命名聊天', 'cwd': self.cwd,
                    'model': self.model, 'latestModel': self.model,
                    'provider': None, 'effort': self.thinking, 'connected': True,
                    'status': self.status, 'collaborationMode': self.mode, 'goal': None,
                    'turns': copy.deepcopy(self.turns), 'requests': copy.deepcopy(self.requests),
                    'historyComplete': True}

    def begin_turn(self, text):
        """Open a live turn for one dispatched prompt."""
        with self._lock:
            self._turn({'id': self._next_id('turn'), 'status': 'inProgress',
                        'startedAt': int(time.time() * 1000),
                        'messages': [{'id': self._next_id('user'), 'kind': 'userMessage',
                                      'role': 'user', 'text': text}],
                        'actionable': False, 'error': None, 'diff': None})
            self.status = 'active'

    def on_update(self, update):
        """Fold one session/update payload (the params.update object) into the view."""
        kind = update.get('sessionUpdate') if isinstance(update, dict) else None
        with self._lock:
            try:
                self._apply(kind, update)
            except Exception:
                LOG.exception('ACP view update failed: %s', kind)

    def end_turn(self, stop_reason):
        with self._lock:
            turn = self._current_turn()
            if turn is not None and turn['status'] == 'inProgress':
                turn['status'] = 'interrupted' if stop_reason == 'cancelled' else 'completed'
                # Kept for auto-continue triage: max_tokens resumes, cancelled
                # never does.
                turn['stopReason'] = stop_reason
            self.status = 'idle'
            self.requests = []

    def fail_turn(self, message):
        with self._lock:
            turn = self._current_turn()
            if turn is not None and turn['status'] == 'inProgress':
                turn['status'] = 'failed'
                turn['messages'].append({'id': self._next_id('error'), 'kind': 'error',
                                         'role': 'error', 'text': str(message)})
            self.status = 'idle'
            self.requests = []

    def finish_replay(self):
        """Historical replay carries no end markers; close every open turn."""
        with self._lock:
            for turn in self.turns:
                if turn['status'] == 'inProgress':
                    turn['status'] = 'completed'
            self.status = 'idle'

    def add_permission(self, request_id, params):
        """Surface one session/request_permission reverse request as a card."""
        tool_call = params.get('toolCall') or {}
        options = [{'id': o.get('optionId'), 'label': o.get('name'), 'kind': o.get('kind')}
                   for o in params.get('options') or [] if isinstance(o, dict)]
        entry = {'id': request_id, 'method': 'acp/requestPermission', 'supported': True,
                 'params': {'title': tool_call.get('title') or '权限请求',
                            'command': _command_summary(tool_call.get('rawInput')),
                            'options': options}}
        with self._lock:
            self.requests = [r for r in self.requests if r.get('id') != request_id]
            self.requests.append(entry)

    def add_input(self, request_id, params):
        """Surface one elicitation/create form request as an answer card."""
        schema = params.get('requestedSchema')
        supported = acp_form_supported(schema)
        entry = {'id': request_id, 'method': 'acp/requestInput',
                 'supported': supported,
                 'params': {'message': params.get('message') or ''}
                 if supported else {'message': '此问题需要在桌面回答'}}
        if supported:
            entry['params']['schema'] = schema
        with self._lock:
            self.requests = [r for r in self.requests if r.get('id') != request_id]
            self.requests.append(entry)

    def remove_permission(self, request_id):
        with self._lock:
            self.requests = [r for r in self.requests if r.get('id') != request_id]

    def apply_config(self, config_options, modes):
        """Seed model/thinking/mode from a session/new or session/load result."""
        with self._lock:
            for option in config_options or []:
                self._apply_config_option(option)
            current = (modes or {}).get('currentModeId')
            if current:
                self.mode = current

    def set_mode(self, mode_id):
        with self._lock:
            self.mode = mode_id

    def _apply(self, kind, update):
        if kind == 'user_message_chunk':
            turn = self._current_turn()
            if turn is not None and any(m['role'] != 'user' for m in turn['messages']):
                if turn['status'] == 'inProgress':
                    turn['status'] = 'completed'
                turn = None
            if turn is None:
                turn = self._turn({'id': self._next_id('turn'), 'status': 'inProgress',
                                   'startedAt': None, 'messages': [],
                                   'actionable': False, 'error': None, 'diff': None})
            turn['messages'].append({'id': self._next_id('user'), 'kind': 'userMessage',
                                     'role': 'user', 'text': _text(update.get('content'))})
        elif kind == 'agent_message_chunk':
            text = _text(update.get('content'))
            if text:
                self._assistant_message(self._assistant_turn(), 'agentMessage',
                                        {'role': 'assistant', 'phase': 'final'},
                                        identity=update.get('messageId'))['text'] += text
        elif kind == 'agent_thought_chunk':
            text = _text(update.get('content'))
            if text:
                self._assistant_message(self._assistant_turn(), 'agentThought',
                                        {'role': 'activity', 'title': '思考摘要'},
                                        identity=update.get('messageId'))['text'] += text
        elif kind == 'tool_call':
            metadata = _subagent_meta(update.get('rawInput'), update.get('title') or '')
            message = {'id': self._next_id('tool'), 'kind': 'toolCall', 'role': 'activity',
                       'title': update.get('title') or update.get('kind') or '工具调用',
                       'text': _command_summary(update.get('rawInput')),
                       'output': _output(update.get('content')),
                       'status': TOOL_STATUS.get(update.get('status'), update.get('status'))}
            if metadata:
                message.update(metadata, kind='subagent')
            self._assistant_turn()['messages'].append(message)
            if update.get('toolCallId'):
                self._tool_index[update['toolCallId']] = message
        elif kind == 'tool_call_update':
            message = self._tool_index.get(update.get('toolCallId'))
            if message is not None:
                if update.get('status'):
                    message['status'] = TOOL_STATUS.get(update['status'], update['status'])
                # claude-agent-acp opens tool calls as placeholders and fills
                # in title and rawInput through updates.
                if update.get('title'):
                    message['title'] = update['title']
                message.update(_subagent_meta(update.get('rawInput'), update.get('title') or message.get('title', '')))
                if message.get('subagentId'):
                    message['kind'] = 'subagent'
                command = _command_summary(update.get('rawInput'))
                if command:
                    message['text'] = command
                output = _output(update.get('content'))
                if output:
                    message['output'] = output
        elif kind == 'plan':
            # Plan updates are whole-list snapshots (TodoWrite-style); fold them
            # into the turn's single plan panel so the timeline ends on the
            # final state instead of one stale bullet list per update.
            entries = []
            for entry in update.get('entries') or []:
                content = str(entry.get('content', '')) if isinstance(entry, dict) else str(entry)
                status = str(entry.get('status') or 'pending') if isinstance(entry, dict) else 'pending'
                entries.append({'content': content, 'status': status})
            if not entries:
                # Wrappers announce the (empty) todo baseline on every attach;
                # an empty list is not content. Never open a panel — or a
                # turn — for it; drop an existing one when the plan cleared.
                turn = self._current_turn()
                if turn is not None:
                    message = next((m for m in turn['messages'] if m.get('kind') == 'plan'), None)
                    if message is not None:
                        turn['messages'].remove(message)
                return
            turn = self._assistant_turn()
            message = next((m for m in turn['messages'] if m.get('kind') == 'plan'), None)
            if message is None:
                message = {'id': self._next_id('plan'), 'kind': 'plan', 'role': 'activity', 'title': '计划'}
                turn['messages'].append(message)
            message['entries'] = entries
            message['text'] = '\n'.join(PLAN_MARKS.get(e['status'], '○') + ' ' + e['content'] for e in entries)
            message['status'] = 'completed' if entries and all(e['status'] == 'completed' for e in entries) else None
        elif kind == 'available_commands_update':
            # Slash commands are not rendered in the mobile timeline, but kimi
            # advertises activatable skills here — keep them for the picker.
            commands = update.get('availableCommands') or []
            self.commands = [c for c in commands if isinstance(c, dict) and c.get('name')]
        elif kind == 'current_mode_update':
            self.mode = update.get('currentModeId') or self.mode
        elif kind == 'config_option_update':
            # kimi/mcode emit a singular configOption; claude-agent-acp sends
            # the whole updated configOptions list.
            options = update.get('configOptions')
            if isinstance(options, list):
                for option in options:
                    if isinstance(option, dict):
                        self._apply_config_option(option)
            else:
                option = update.get('configOption')
                if isinstance(option, dict):
                    self._apply_config_option(option)
        else:
            LOG.debug('Ignoring unknown ACP update: %s', kind)

    def _apply_config_option(self, option):
        value = option.get('currentValue')
        if value is None:
            return
        slot = option.get('category') or option.get('id')
        if slot == 'model':
            self.model = value
        elif slot in ('thought_level', 'thinking'):
            self.thinking = value
        elif slot == 'mode':
            self.mode = value

    def _next_id(self, prefix):
        self._ordinal += 1
        return f'{prefix}-{self._ordinal}'

    def _turn(self, turn):
        self.turns.append(turn)
        return turn

    def _current_turn(self):
        return self.turns[-1] if self.turns else None

    def _assistant_turn(self):
        turn = self._current_turn()
        if turn is None:
            turn = self._turn({'id': self._next_id('turn'), 'status': 'inProgress',
                               'startedAt': None, 'messages': [],
                               'actionable': False, 'error': None, 'diff': None})
        return turn

    def _assistant_message(self, turn, kind, extra, identity=None):
        """Fold one streamed chunk into the turn's current message of ``kind``.

        Chunks that carry a messageId (replay and the zcode live stream both
        do) merge only within the same id, so consecutive distinct messages —
        a preamble and the final reply of one turn — stay separate rows.
        Chunks without an id fall back to adjacency, preserving the plain
        streaming behavior of wrappers that never send one.
        """
        last = turn['messages'][-1] if turn['messages'] else None
        if (last is not None and last.get('kind') == kind
                and last.get('chunkIdentity') == identity):
            return last
        message = {'id': self._next_id(kind), 'kind': kind, 'text': '',
                   'chunkIdentity': identity, **extra}
        turn['messages'].append(message)
        return message
