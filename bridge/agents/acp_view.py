"""ACP session view: synthesizes the normalized view contract from session/update streams."""
import copy
import json
import logging
import threading
import time

LOG = logging.getLogger(__name__)

TOOL_STATUS = {'pending': 'inProgress', 'in_progress': 'inProgress',
               'completed': 'completed', 'failed': 'failed'}


def acp_normalize(state, connected):
    """ACP state is already normalized; only the liveness flag is dynamic."""
    return {**state, 'connected': connected}


def _text(content):
    """Extract text from one ACP content block."""
    return content.get('text') or '' if isinstance(content, dict) else ''


def _output(items):
    """Flatten ACP tool-call content items into one plain-text output."""
    parts = [_text(item.get('content')) for item in items or [] if isinstance(item, dict)]
    return '\n'.join(part for part in parts if part)


def _command_summary(raw_input):
    """Best-effort one-line summary of a tool call's raw input."""
    if not isinstance(raw_input, dict):
        return ''
    for key in ('command', 'cmd', 'path', 'file_path'):
        value = raw_input.get(key)
        if isinstance(value, str) and value:
            return value
    return json.dumps(raw_input, ensure_ascii=False)[:500]


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
                                        {'role': 'assistant', 'phase': 'final'})['text'] += text
        elif kind == 'agent_thought_chunk':
            text = _text(update.get('content'))
            if text:
                self._assistant_message(self._assistant_turn(), 'agentThought',
                                        {'role': 'activity', 'title': '思考摘要'})['text'] += text
        elif kind == 'tool_call':
            message = {'id': self._next_id('tool'), 'kind': 'toolCall', 'role': 'activity',
                       'title': update.get('title') or update.get('kind') or '工具调用',
                       'text': _command_summary(update.get('rawInput')),
                       'output': _output(update.get('content')),
                       'status': TOOL_STATUS.get(update.get('status'), update.get('status'))}
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
                command = _command_summary(update.get('rawInput'))
                if command:
                    message['text'] = command
                output = _output(update.get('content'))
                if output:
                    message['output'] = output
        elif kind == 'plan':
            entries = update.get('entries') or []
            text = '\n'.join('• ' + (str(e.get('content', '')) if isinstance(e, dict) else str(e)) for e in entries)
            self._assistant_turn()['messages'].append(
                {'id': self._next_id('plan'), 'kind': 'plan', 'role': 'activity',
                 'title': '计划', 'text': text})
        elif kind == 'available_commands_update':
            pass  # Slash commands are not rendered in the mobile timeline.
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

    def _assistant_message(self, turn, kind, extra):
        if turn['messages'] and turn['messages'][-1].get('kind') == kind:
            return turn['messages'][-1]
        message = {'id': self._next_id(kind), 'kind': kind, 'text': '', **extra}
        turn['messages'].append(message)
        return message
