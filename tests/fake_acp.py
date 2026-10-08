"""In-process fake ACP agent: speaks NDJSON JSON-RPC over os.pipe pairs."""
import json
import os
import queue
import subprocess
import threading
import time


class FakeChildProcess:
    """Minimal subprocess.Popen stand-in wired to a FakeAcpAgent."""

    def __init__(self, agent):
        c2a_read, c2a_write = os.pipe()  # client stdin -> agent
        a2c_read, a2c_write = os.pipe()  # agent -> client stdout
        e_read, e_write = os.pipe()      # agent stderr -> client drain thread
        self.stdin = os.fdopen(c2a_write, 'w', encoding='utf-8', buffering=1)
        self.stdout = os.fdopen(a2c_read, 'r', encoding='utf-8')
        self.stderr = os.fdopen(e_read, 'r', encoding='utf-8')
        self._returncode = None
        self._done = threading.Event()
        agent._attach(c2a_read, a2c_write, e_write, self)

    @property
    def returncode(self):
        return self._returncode

    def poll(self):
        return self._returncode

    def wait(self, timeout=None):
        if not self._done.wait(timeout):
            raise subprocess.TimeoutExpired('fake-acp', timeout)
        return self._returncode

    def terminate(self):
        self._agent_shutdown(0)

    def kill(self):
        self._agent_shutdown(-9)

    def _agent_shutdown(self, returncode):
        self._state['agent']._shutdown(self, returncode)

    def _mark(self, returncode):
        if self._returncode is None:
            self._returncode = returncode
            self._done.set()


class FakeAcpAgent:
    """Scriptable ACP peer running in a daemon thread."""

    def __init__(self, *, capabilities=None, agent_info=None, auth_methods=None,
                 config_options=None, modes=None, list_rows=None, extensions=None):
        self.capabilities = capabilities if capabilities is not None else {
            'loadSession': True,
            'sessionCapabilities': {'list': {}, 'resume': {}, 'close': {}},
            'promptCapabilities': {'image': True},
        }
        self.agent_info = agent_info if agent_info is not None else {'name': 'fake-acp', 'version': '0.0.1'}
        self.auth_methods = auth_methods if auth_methods is not None else []
        self.extensions = extensions  # e.g. ['mcode/session/steer', 'mcode/session/goal/get']
        self.config_options = config_options if config_options is not None else [
            {'id': 'model', 'name': 'Model', 'type': 'select', 'options': []},
        ]
        self.modes = modes if modes is not None else {'availableModes': [], 'currentModeId': 'default'}
        self.list_rows = list_rows if list_rows is not None else []
        # session/update payloads replayed by session/load and session/prompt.
        # A prompt step may also be ('permission', params) to round-trip one
        # session/request_permission reverse request before continuing.
        self.load_updates = []
        self.prompt_updates = []
        self.errors = {}  # method -> JSON-RPC error object
        self.stop_reason = 'end_turn'
        self.cancelled = False
        self.permission_outcome = None
        self.session_count = 0
        self.config_echo_all = True
        self.requests = []
        self.children = []
        self.spawned_argv = None
        self.closed_sessions = []
        self.config_values = {}
        self.mode_values = []
        self.steer_texts = []
        self.goal = None
        self.queue_items = []
        self._write_lock = threading.Lock()
        self._responses_lock = threading.Lock()
        self._client_responses = {}
        self._next_reverse = 9000
        self._hang = threading.Event()
        self._cancel_turn = threading.Event()
        self._active = None

    def spawn_factory(self):
        """Return a spawn callable for AcpProcess(spawn=...)."""
        def spawn(argv, **kwargs):
            self.spawned_argv = list(argv)
            return FakeChildProcess(self)
        return spawn

    def emit_raw(self, data):
        """Write raw bytes to the client stdout pipe (banners, garbage)."""
        if self._active is not None:
            self._write(self._active, data)

    def hang(self):
        """Stop answering requests without closing the pipes."""
        self._hang.set()

    def die(self, returncode=1):
        """Simulate an abrupt process exit."""
        if self._active is not None:
            self._shutdown(self._active['child'], returncode)

    def _attach(self, c2a_read, a2c_write, e_write, child):
        state = {'agent': self, 'child': child, 'read': c2a_read,
                 'out': a2c_write, 'err': e_write, 'dead': False}
        child._state = state
        self.children.append(child)
        self._active = state
        thread = threading.Thread(target=self._serve, args=(state,), daemon=True)
        state['thread'] = thread
        thread.start()

    def _shutdown(self, child, returncode):
        state = getattr(child, '_state', None)
        if state is None or state['dead']:
            return
        state['dead'] = True
        child._mark(returncode)
        for fd in (state['out'], state['err']):
            try:
                os.close(fd)
            except OSError:
                pass

    def _serve(self, state):
        # The reader must keep reading while a handler blocks on a reverse
        # request, so client requests run on a separate worker thread.
        work = queue.Queue()
        worker = threading.Thread(target=self._worker, args=(state, work), daemon=True)
        worker.start()
        reader = os.fdopen(state['read'], 'r', encoding='utf-8')
        try:
            while True:
                line = reader.readline()
                if line == '':
                    break
                if self._hang.is_set():
                    continue
                try:
                    message = json.loads(line)
                except ValueError:
                    continue
                if not isinstance(message, dict):
                    continue
                if 'method' not in message:
                    self._client_response(message)
                    continue
                self.requests.append(message)
                work.put(message)
        except OSError:
            pass
        finally:
            work.put(None)
            try:
                reader.close()
            except Exception:
                pass

    def _worker(self, state, work):
        while True:
            message = work.get()
            if message is None:
                return
            try:
                self._handle(state, message)
            except Exception:
                pass

    def _handle(self, state, message):
        method = message.get('method')
        ident = message.get('id')
        params = message.get('params') or {}
        if method in self.errors:
            self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'error': self.errors[method]})
            return
        handler = getattr(self, '_method_' + str(method).replace('/', '_'), None)
        if handler is None:
            self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                                'error': {'code': -32601, 'message': 'Method not found'}})
            return
        handler(state, ident, params)

    def _method_initialize(self, state, ident, params):
        result = {
            'protocolVersion': 1,
            'agentCapabilities': self.capabilities,
            'agentInfo': self.agent_info,
            'authMethods': self.auth_methods,
        }
        if self.extensions is not None:
            result['_meta'] = {'minimax-code/extensions': {'version': 1, 'methods': self.extensions}}
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': result})

    def _method_session_new(self, state, ident, params):
        self.session_count += 1
        session_id = f'fake-{self.session_count}'
        # A real agent lists a session right after session/new.
        self.list_rows.append({'sessionId': session_id, 'cwd': params.get('cwd') or '',
                               'title': '', 'updatedAt': '2026-01-02T03:04:05Z'})
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {
            'sessionId': session_id,
            'configOptions': self.config_options,
            'modes': self.modes,
        }})

    def _method_session_list(self, state, ident, params):
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {
            'sessions': self.list_rows, 'nextCursor': None}})

    def _method_session_load(self, state, ident, params):
        session_id = params.get('sessionId')
        for update in self.load_updates:
            self._notify(state, session_id, update)
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {
            'configOptions': self.config_options, 'modes': self.modes}})

    def _method_session_close(self, state, ident, params):
        self.closed_sessions.append(params.get('sessionId'))
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {}})

    def _method_session_set_config_option(self, state, ident, params):
        self.config_values[params.get('configId')] = params.get('value')
        for option in self.config_options:
            if isinstance(option, dict) and option.get('id') == params.get('configId'):
                option['currentValue'] = params.get('value')
        options = self.config_options
        if not self.config_echo_all:
            options = [o for o in options if isinstance(o, dict) and o.get('id') == params.get('configId')]
        self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                            'result': {'configOptions': options}})

    def _method_session_set_mode(self, state, ident, params):
        self.mode_values.append(params.get('modeId'))
        self.modes['currentModeId'] = params.get('modeId')
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {}})

    def _method_session_prompt(self, state, ident, params):
        session_id = params.get('sessionId')
        self._cancel_turn.clear()
        # The script runs off the worker so ('await_cancel',) cannot deadlock
        # a session/cancel handled by the same worker.
        def run():
            for step in self.prompt_updates:
                if self._cancel_turn.is_set():
                    break
                if isinstance(step, tuple) and step[0] == 'permission':
                    self._ask_permission(state, session_id, step[1])
                elif isinstance(step, tuple) and step[0] == 'await_cancel':
                    self._cancel_turn.wait(10)
                    break
                else:
                    self._notify(state, session_id, step)
            reason = 'cancelled' if self._cancel_turn.is_set() else self.stop_reason
            self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {'stopReason': reason}})
        threading.Thread(target=run, daemon=True).start()

    def _method_session_cancel(self, state, ident, params):
        self.cancelled = True
        self._cancel_turn.set()
        if ident is not None:
            self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {}})

    def _method_mcode_session_steer(self, state, ident, params):
        self.steer_texts.append(params.get('text'))
        self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                            'result': {'turnId': 'fake-turn', 'mode': 'steered'}})

    def _method_mcode_session_goal_get(self, state, ident, params):
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {'goal': self.goal}})

    def _method_mcode_session_goal_create(self, state, ident, params):
        now = int(time.time() * 1000)
        self.goal = {'goalId': 'tg_fake', 'sessionId': params.get('sessionId'),
                     'objective': params.get('objective'), 'status': 'active',
                     'createdAt': now, 'updatedAt': now, 'tokenBudget': None}
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {'goal': self.goal}})

    def _method_mcode_session_goal_patch(self, state, ident, params):
        if self.goal is None:
            self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                                'error': {'code': -32002, 'message': 'Resource not found'}})
            return
        for key in ('objective', 'status', 'tokenBudget'):
            if key in params:
                self.goal[key] = params[key]
        self.goal['updatedAt'] = int(time.time() * 1000)
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {'goal': self.goal}})

    def _method_mcode_session_goal_clear(self, state, ident, params):
        self.goal = None
        self._reply(state, {'jsonrpc': '2.0', 'id': ident, 'result': {'cleared': True}})

    def _method_mcode_session_queue_list(self, state, ident, params):
        self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                            'result': {'items': list(self.queue_items)}})

    def _ask_permission(self, state, session_id, params):
        with self._responses_lock:
            ident = self._next_reverse
            self._next_reverse += 1
            slot = {'event': threading.Event(), 'message': None}
            self._client_responses[ident] = slot
        payload = dict(params) if isinstance(params, dict) else {}
        payload['sessionId'] = session_id
        self._reply(state, {'jsonrpc': '2.0', 'id': ident,
                            'method': 'session/request_permission', 'params': payload})
        slot['event'].wait(10)  # Never block the fake forever.
        with self._responses_lock:
            self._client_responses.pop(ident, None)
        result = (slot['message'] or {}).get('result') or {}
        self.permission_outcome = result.get('outcome')

    def _client_response(self, message):
        with self._responses_lock:
            slot = self._client_responses.get(message.get('id'))
            if slot is not None:
                slot['message'] = message
                slot['event'].set()

    def _notify(self, state, session_id, update):
        self._reply(state, {'jsonrpc': '2.0', 'method': 'session/update',
                            'params': {'sessionId': session_id, 'update': update}})

    def _reply(self, state, message):
        self._write(state, (json.dumps(message, ensure_ascii=False) + '\n').encode('utf-8'))

    def _write(self, state, data):
        with self._write_lock:
            if state['dead']:
                return
            try:
                os.write(state['out'], data)
            except OSError:
                pass
