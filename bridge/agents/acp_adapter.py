"""ACP adapter: one long-lived agent process per open session."""
import base64
import hashlib
import logging
import threading
import time
from concurrent.futures import TimeoutError as FutureTimeout
from datetime import datetime, timezone
from pathlib import Path

from ..store import StoreUnavailable
from ..uploads import image_type
from .acp import AcpAuthRequired, AcpError, AcpProcess, AcpUnavailable
from .acp_view import AcpSessionView, acp_normalize
from .base import AgentAdapter

LOG = logging.getLogger(__name__)

LIST_CACHE_SECONDS = 30
PROMPT_TIMEOUT = 3600


class AcpHandle:
    """Everything the bridge owns for one attached ACP session."""

    def __init__(self, session_id, view):
        self.session_id = session_id
        self.view = view
        self.process = None
        self.ready = threading.Event()
        self.lock = threading.Lock()
        self.prompt_running = False
        self.pending_permissions = {}
        self.responders = {}
        self.config_options = []
        self.modes = None
        self.load_error = None
        self.deliberate = False
        self.dead = False


class AcpAdapter(AgentAdapter):
    """Generic Agent Client Protocol profile; subclasses set kind and maps."""

    kind = 'acp'
    label = 'ACP Agent'
    capabilities = frozenset()
    auth_hint = '请先在电脑终端完成 Agent 登录'
    # npx-wrapped agents download the package on first run; subclasses raise this.
    INITIALIZE_TIMEOUT = 10
    # claude-agent-acp persists a transcript only after the first message; until
    # then no other process can load or prompt the session, so creation keeps
    # its process alive as the session handle.
    KEEP_CREATED_PROCESS = False
    MODEL_CATEGORY = 'model'
    THINKING_CATEGORY = 'thought_level'
    EFFORT_MAP = {}
    # Subclasses may swap in a specialized AcpSessionView.
    VIEW_CLASS = AcpSessionView

    # Delegated bridge properties that stay inert for ACP sessions.
    accounts = None
    ipc = None
    goal = None
    owner_goal = None
    goal_transport = None
    hosts = None
    store = None
    catalog_reader = None
    codex_home = None
    account = None

    def __init__(self, executable, data_dir=None, workspaces=None, spawn=None):
        super().__init__()
        if not executable:
            raise AcpUnavailable('未找到 Agent 命令，请先安装')
        self.executable = executable
        self.data_dir = Path(data_dir) if data_dir else None
        self._workspaces = [str(w) for w in (workspaces or [])]
        self._spawn = spawn  # Test seam: FakeAcpAgent.spawn_factory().
        self._handles = {}
        self._list_lock = threading.Lock()
        self._list_cache = []
        self._list_at = 0
        self._meta_cache = {}
        # Sessions created here but not yet listed by the agent (claude-agent-acp
        # only persists a transcript after the first message).
        self._pending_created = {}

    def _argv(self):
        return [self.executable, 'acp']

    def _process(self, **kwargs):
        return AcpProcess(self._argv(), spawn=self._spawn, **kwargs)

    # -- session lifecycle ------------------------------------------------

    def adopt_session(self, session):
        session.normalizer = acp_normalize

    def attach(self, session, timeout=1.5):
        with session.attach_lock:
            if session.connected or self.bridge.closed.is_set():
                return
            handle = self._handles.get(session.id)
            if handle is not None and (handle.dead or handle.load_error is not None):
                self._teardown(session.id, handle)
                handle = None
            if handle is None:
                try:
                    handle = self._spawn_handle(session)
                except AcpError as exc:
                    with session.condition:
                        session.connected = False
                        session.error = str(exc)
                        session.changed()
                    return
                self._handles[session.id] = handle
                threading.Thread(target=self._load, args=(session, handle), daemon=True).start()
            elif handle.ready.is_set():
                # A pre-registered handle (kept create process) has no _load
                # thread to sync this session; do it here.
                self._sync(session)
            # A timeout only means the replay is still running; the load
            # thread keeps going and syncs the session when it finishes.
            handle.ready.wait(timeout)

    def _spawn_handle(self, session):
        meta = self._meta_cache.get(session.id) or {}
        handle = AcpHandle(session.id, self.VIEW_CLASS(session.id, title=meta.get('title'),
                                                       cwd=meta.get('cwd')))
        process = self._process(cwd=handle.view.cwd or None,
                                on_notification=lambda m: self._on_notification(handle, m),
                                on_request=lambda m, respond: self._on_request(handle, m, respond),
                                on_exit=lambda code: self._on_exit(handle, code))
        handle.process = process
        try:
            process.initialize(timeout=self.INITIALIZE_TIMEOUT)
        except AcpAuthRequired:
            process.close()
            raise AcpUnavailable(self.auth_hint) from None
        except AcpError:
            process.close()
            raise
        return handle

    def _load(self, session, handle):
        try:
            if not handle.view.cwd:
                row = next((r for r in self._list_rows_via(handle.process)
                            if r.get('sessionId') == session.id), None)
                if row is None:
                    raise AcpUnavailable('会话不存在或已被删除')
                handle.view.cwd = row.get('cwd')
                handle.view.title = row.get('title') or handle.view.title
            result = handle.process.request('session/load', {
                'sessionId': session.id, 'cwd': handle.view.cwd or '', 'mcpServers': [],
            }, timeout=120) or {}
            handle.config_options = result.get('configOptions') or handle.config_options
            handle.modes = result.get('modes') or handle.modes
            handle.view.apply_config(handle.config_options, handle.modes)
            # Replayed history carries no turn-end markers; close open turns.
            handle.view.finish_replay()
        except AcpAuthRequired:
            handle.load_error = self.auth_hint
        except AcpError as exc:
            handle.load_error = str(exc)
        except Exception:
            LOG.exception('ACP session load failed')
            handle.load_error = '读取会话失败，请重新连接或查看网关日志。'
        if handle.load_error is not None:
            with session.condition:
                session.error = handle.load_error
        handle.ready.set()
        self._sync(session)

    def detach(self, session):
        handle = self._handles.get(session.id)
        if handle is not None:
            self._teardown(session.id, handle)

    def _teardown(self, session_id, handle):
        """Close one handle; deliberately, so the exit handler stays quiet."""
        handle.deliberate = True
        if self._handles.get(session_id) is handle:
            self._handles.pop(session_id, None)
        with self._list_lock:
            # A session that was never listed has no transcript anywhere; once
            # its process is gone it is unrecoverable, so drop the placeholder.
            self._pending_created.pop(session_id, None)
        with handle.lock:
            responders = list(handle.responders.values())
            handle.responders.clear()
            handle.pending_permissions.clear()
        for respond in responders:
            try:
                respond({'outcome': {'outcome': 'cancelled'}})
            except AcpError:
                pass
        if handle.process is not None:
            try:
                handle.process.request('session/close', {'sessionId': session_id}, timeout=5)
            except AcpError:
                pass
            handle.process.close()

    def activate_owner(self, session):
        self.attach(session, timeout=20)
        handle = self._handles.get(session.id)
        if handle is not None and handle.load_error is not None:
            raise AcpUnavailable(handle.load_error)

    def _on_notification(self, handle, message):
        if message.get('method') != 'session/update':
            return
        params = message.get('params') or {}
        if params.get('sessionId') != handle.session_id:
            return
        handle.view.on_update(params.get('update') or {})
        with self.bridge.lock:
            session = self.bridge.live.get(handle.session_id)
        if session is not None:
            self._sync(session)

    def _on_request(self, handle, message, respond):
        # Answering here would block the reader thread; record the request and
        # let respond_to_request answer it from the HTTP worker instead.
        params = message.get('params') or {}
        request_id = message.get('id')
        with handle.lock:
            handle.pending_permissions[request_id] = params
            handle.responders[request_id] = respond
        handle.view.add_permission(request_id, params)
        with self.bridge.lock:
            session = self.bridge.live.get(handle.session_id)
        if session is not None:
            self._sync(session)

    def _on_exit(self, handle, returncode):
        handle.dead = True
        if handle.deliberate:
            return
        with self.bridge.lock:
            session = self.bridge.live.get(handle.session_id)
        if session is None:
            return
        with session.condition:
            session.connected = False
            session.error = 'Agent 进程已退出，请重新连接'
            session.changed()
        with self.bridge.lock:
            self.bridge.notification_dirty.add(handle.session_id)
        self.bridge.notification_event.set()

    def _sync(self, session):
        handle = self._handles.get(session.id)
        if handle is None:
            return
        snapshot = handle.view.snapshot()
        with session.condition:
            session.state = snapshot
            session.connected = (handle.ready.is_set() and handle.load_error is None
                                 and not handle.dead)
            session.changed()
        with self.bridge.lock:
            self.bridge.notification_dirty.add(session.id)
        self.bridge.notification_event.set()

    def is_active(self, session):
        handle = self._handles.get(session.id)
        if handle is not None and handle.prompt_running:
            return True
        with session.condition:
            return isinstance(session.state, dict) and session.state.get('status') == 'active'

    def close(self):
        for session_id, handle in list(self._handles.items()):
            try:
                self._teardown(session_id, handle)
            except Exception:
                LOG.exception('ACP handle teardown failed')

    # -- turns -------------------------------------------------------------

    def call(self, session, method, params, timeout=30):
        handle = self._handles.get(session.id)
        if handle is None:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        return handle.process.request(method, {'sessionId': session.id, **params}, timeout=timeout)

    def dispatch_turn(self, session, submission_id, entry, files):
        handle = self._handles.get(session.id)
        if handle is None or handle.dead or handle.load_error is not None:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        work_mode = entry.get('workMode')
        if work_mode == 'goal':
            raise ValueError('当前 Agent 不支持目标模式')
        if entry.get('mode') == 'steer':
            raise ValueError('当前 Agent 不支持补充任务，请使用排队发送')
        if work_mode in ('plan', 'default'):
            self._set_mode(handle, session, work_mode)
        blocks = [{'type': 'text', 'text': entry['text']}]
        blocks.extend(self._attachment_blocks(handle, files))
        with handle.lock:
            if handle.prompt_running:
                raise AcpUnavailable('任务正在执行，请稍候')
            handle.prompt_running = True
        handle.view.begin_turn(entry['text'])
        self._sync(session)
        try:
            _ident, future = handle.process.submit('session/prompt',
                                                   {'sessionId': session.id, 'prompt': blocks})
        except AcpError:
            with handle.lock:
                handle.prompt_running = False
            handle.view.fail_turn('发送失败，请重新连接后重试')
            self._sync(session)
            raise
        threading.Thread(target=self._await_prompt, args=(session, handle, future),
                         daemon=True).start()
        return {'accepted': True}

    def _set_mode(self, handle, session, mode_id):
        modes = handle.modes or {}
        available = {m.get('id') for m in modes.get('availableModes') or []
                     if isinstance(m, dict)}
        if mode_id not in available:
            raise ValueError('当前 Agent 不支持此工作模式')
        if modes.get('currentModeId') == mode_id:
            return
        handle.process.request('session/set_mode',
                               {'sessionId': session.id, 'modeId': mode_id}, timeout=30)
        modes['currentModeId'] = mode_id
        handle.modes = modes
        handle.view.set_mode(mode_id)

    def _attachment_blocks(self, handle, files):
        capabilities = (handle.process.capabilities.get('promptCapabilities') or {})
        blocks = []
        for item in files:
            if item['image']:
                if not capabilities.get('image'):
                    raise ValueError('此 Agent 暂不支持图片附件')
                data = Path(item['path']).read_bytes()
                blocks.append({'type': 'image', 'data': base64.b64encode(data).decode('ascii'),
                               'mimeType': image_type(data) or 'image/png'})
            else:
                if not capabilities.get('embeddedContext'):
                    raise ValueError('此 Agent 暂不支持文件附件')
                blocks.append({'type': 'resource_link', 'name': item['name'],
                               'uri': Path(item['path']).resolve().as_uri()})
        return blocks

    def _await_prompt(self, session, handle, future):
        try:
            result = future.result(PROMPT_TIMEOUT) or {}
        except FutureTimeout:
            handle.view.fail_turn('Agent 响应超时')
        except AcpError as exc:
            handle.view.fail_turn(str(exc))
        except Exception as exc:
            LOG.exception('ACP prompt wait failed')
            handle.view.fail_turn(f'任务失败：{exc}')
        else:
            handle.view.end_turn(result.get('stopReason') or 'end_turn')
        with handle.lock:
            handle.prompt_running = False
        self._sync(session)

    def interrupt(self, session):
        handle = self._handles.get(session.id)
        if handle is None or not handle.prompt_running:
            raise ValueError('当前没有可停止的任务')
        # ACP defines session/cancel as a notification; real agents (kimi)
        # reject it when sent as a request.
        handle.process.notify('session/cancel', {'sessionId': session.id})
        # The prompt answer settles the turn; flip the view immediately so the
        # phone stops offering actions for a turn that is already cancelled.
        handle.view.end_turn('cancelled')
        self._sync(session)
        return {'stopped': True}

    def full_history(self, session):
        raise ValueError('历史已在连接时完整加载')

    # -- approvals ---------------------------------------------------------

    def respond_to_request(self, session, pending, response):
        handle = self._handles.get(session.id)
        if handle is None:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        request_id = pending.get('id')
        with handle.lock:
            params = handle.pending_permissions.get(request_id)
            respond = handle.responders.get(request_id)
        if params is None or respond is None:
            raise ValueError('此请求已处理或已过期')
        if set(response) - {'optionId', 'cancel'}:
            raise ValueError('请求回应格式不正确')
        if response.get('cancel') is True:
            outcome = {'outcome': 'cancelled'}
        else:
            option_id = response.get('optionId')
            valid = {o.get('optionId') for o in params.get('options') or []
                     if isinstance(o, dict)}
            if option_id not in valid:
                raise ValueError('请选择有效的选项')
            outcome = {'outcome': 'selected', 'optionId': option_id}
        with handle.lock:
            handle.pending_permissions.pop(request_id, None)
            handle.responders.pop(request_id, None)
        try:
            respond({'outcome': outcome})
        finally:
            handle.view.remove_permission(request_id)
            self._sync(session)
        return {'ok': True}

    # -- metadata ----------------------------------------------------------

    def _list_rows_via(self, process):
        result = process.request('session/list', {}, timeout=15) or {}
        rows = [r for r in result.get('sessions') or [] if isinstance(r, dict)]
        with self._list_lock:
            for row in rows:
                if row.get('sessionId'):
                    self._meta_cache[row['sessionId']] = row
        return rows

    def _fetch_rows(self):
        with self._list_lock:
            if time.monotonic() - self._list_at < LIST_CACHE_SECONDS:
                return self._list_cache
        process = self._process()
        try:
            process.initialize(timeout=self.INITIALIZE_TIMEOUT)
            rows = self._list_rows_via(process)
        except AcpAuthRequired as exc:
            raise StoreUnavailable(self.auth_hint) from exc
        except AcpError as exc:
            raise StoreUnavailable(str(exc)) from exc
        finally:
            process.close()
        with self._list_lock:
            listed = {row.get('sessionId') for row in rows}
            for known in list(self._pending_created):
                if known in listed:
                    self._pending_created.pop(known, None)
            self._list_cache = rows
            self._list_at = time.monotonic()
        return rows

    def _visible_rows(self):
        rows = self._fetch_rows()
        listed = {row.get('sessionId') for row in rows}
        with self._list_lock:
            extra = [row for row in self._pending_created.values()
                     if row['sessionId'] not in listed]
        return rows + extra

    @staticmethod
    def _recency_ms(value):
        if isinstance(value, (int, float)) and value > 0:
            return int(value * 1000) if value < 1e12 else int(value)
        if isinstance(value, str) and value:
            try:
                parsed = datetime.fromisoformat(value.replace('Z', '+00:00'))
            except ValueError:
                return 0
            if parsed.tzinfo is None:
                parsed = parsed.replace(tzinfo=timezone.utc)
            return int(parsed.timestamp() * 1000)
        return 0

    def list_sessions(self, label, limit=100, offset=0, query='', archived=False):
        if archived:
            # ACP 会话没有归档概念，归档视图只展示 codex 会话。
            return []
        needle = (query or '').strip().casefold()
        rows = []
        for row in self._visible_rows():
            if not row.get('sessionId'):
                continue
            title = row.get('title') or '未命名聊天'
            cwd = row.get('cwd') or ''
            if needle and needle not in title.casefold() and needle not in cwd.casefold():
                continue
            rows.append({'id': row['sessionId'], 'title': title, 'name': title, 'cwd': cwd,
                         'recency': self._recency_ms(row.get('updatedAt')),
                         'host': 'local', 'hostLabel': label, 'agent': self.kind,
                         'capabilities': sorted(self.capabilities),
                         'projectKey': 'local|' + (cwd or 'unassigned'),
                         'projectName': cwd.replace('\\', '/').rstrip('/').rsplit('/', 1)[-1] or '未归类'})
        rows.sort(key=lambda row: (row['recency'], row['id']), reverse=True)
        return rows[:limit + offset]

    def recencies(self, identifiers):
        try:
            rows = self._fetch_rows()
        except StoreUnavailable:
            return {}
        wanted = set(identifiers)
        return {row['sessionId']: self._recency_ms(row.get('updatedAt'))
                for row in rows if row.get('sessionId') in wanted}

    def notification_changes(self, cursor):
        return []

    def session_meta(self, thread_id):
        meta = self._meta_cache.get(thread_id)
        if meta is None:
            handle = self._handles.get(thread_id)
            if handle is not None and handle.view.cwd:
                meta = {'sessionId': thread_id, 'title': handle.view.title,
                        'cwd': handle.view.cwd}
        if meta is None:
            self._fetch_rows()
            meta = self._meta_cache.get(thread_id)
        if meta is None:
            raise KeyError(thread_id)
        return {'id': thread_id, 'title': meta.get('title') or '未命名聊天',
                'cwd': meta.get('cwd')}

    def saved_history(self, session):
        handle = self._handles.get(session.id)
        view = handle.view if handle is not None else self.VIEW_CLASS(session.id)
        return view.snapshot()

    def projects(self):
        cwds = []
        for cwd in self._workspaces:
            if cwd and cwd not in cwds:
                cwds.append(cwd)
        try:
            rows = self._fetch_rows()
        except StoreUnavailable:
            rows = []
        for row in rows:
            cwd = row.get('cwd')
            if cwd and cwd not in cwds:
                cwds.append(cwd)
        return [{'key': self.kind + ':' + hashlib.sha256(cwd.encode('utf-8')).hexdigest()[:12],
                 'host': 'local', 'hostLabel': '此电脑',
                 'name': cwd.replace('\\', '/').rstrip('/').rsplit('/', 1)[-1] or cwd,
                 'cwd': cwd} for cwd in cwds]

    def create_session(self, project, title):
        cwd = project.get('cwd') or str(Path.home())
        if self.KEEP_CREATED_PROCESS:
            return self._create_session_live(cwd, title)
        process = self._process(cwd=cwd)
        try:
            process.initialize(timeout=self.INITIALIZE_TIMEOUT)
            result = process.request('session/new', {'cwd': cwd, 'mcpServers': []},
                                     timeout=60) or {}
        except AcpAuthRequired as exc:
            raise AcpUnavailable(self.auth_hint) from exc
        finally:
            process.close()
        thread_id = result.get('sessionId')
        self.validate_id(thread_id)
        self._remember_created(thread_id, title, cwd)
        return thread_id

    def _create_session_live(self, cwd, title):
        """Create a session and keep its process as the session handle."""
        handle = AcpHandle(None, self.VIEW_CLASS(None, title=title, cwd=cwd))
        process = self._process(cwd=cwd,
                                on_notification=lambda m: self._on_notification(handle, m),
                                on_request=lambda m, respond: self._on_request(handle, m, respond),
                                on_exit=lambda code: self._on_exit(handle, code))
        handle.process = process
        try:
            process.initialize(timeout=self.INITIALIZE_TIMEOUT)
            result = process.request('session/new', {'cwd': cwd, 'mcpServers': []},
                                     timeout=60) or {}
        except AcpAuthRequired:
            process.close()
            raise AcpUnavailable(self.auth_hint) from None
        except AcpError:
            process.close()
            raise
        thread_id = result.get('sessionId')
        try:
            self.validate_id(thread_id)
        except ValueError:
            process.close()
            raise
        handle.session_id = thread_id
        handle.view.session_id = thread_id
        handle.config_options = [o for o in result.get('configOptions') or []
                                 if isinstance(o, dict)]
        handle.modes = result.get('modes')
        handle.view.apply_config(handle.config_options, handle.modes)
        handle.ready.set()
        self._handles[thread_id] = handle
        self._remember_created(thread_id, title, cwd)
        return thread_id

    def _remember_created(self, thread_id, title, cwd):
        with self._list_lock:
            meta = {'sessionId': thread_id, 'title': title, 'cwd': cwd,
                    'updatedAt': int(time.time() * 1000)}
            self._meta_cache[thread_id] = meta
            self._pending_created[thread_id] = meta
            self._list_at = 0  # The next list must see the new session.

    def open_created(self, thread_id, host):
        return True

    # -- settings and catalog ---------------------------------------------

    @classmethod
    def _find_category(cls, options, category):
        for option in options:
            if isinstance(option, dict) and (option.get('category') or option.get('id')) == category:
                return option
        return None

    def _set_config(self, handle, session, config_id, value):
        result = handle.process.request('session/set_config_option',
                                        {'sessionId': session.id, 'configId': config_id,
                                         'value': value}, timeout=30) or {}
        updated = [o for o in result.get('configOptions') or [] if isinstance(o, dict)]
        if updated:
            # The response is authoritative: mcode 0.5.5 drops thought_level for
            # models without reasoning-effort support, and that must propagate.
            handle.config_options = updated
        else:
            for option in handle.config_options:
                if isinstance(option, dict) and option.get('id') == config_id:
                    option['currentValue'] = value
        handle.view.apply_config(handle.config_options, handle.modes)
        self._sync(session)

    def update_settings(self, session, model, effort, fast_mode=None):
        if fast_mode is not None:
            raise ValueError('当前 Agent 不支持 Fast 模式')
        handle = self._handles.get(session.id)
        if handle is None or handle.dead:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        with session.action_lock:
            model_option = self._find_category(handle.config_options, self.MODEL_CATEGORY)
            if model_option is None:
                raise ValueError('当前 Agent 不支持切换模型')
            values = {o.get('value') for o in model_option.get('options') or []
                      if isinstance(o, dict)}
            if values and model not in values:
                raise ValueError('此 Agent 不支持所选模型')
            if model_option.get('currentValue') != model:
                self._set_config(handle, session, model_option['id'], model)
            thinking = self._find_category(handle.config_options, self.THINKING_CATEGORY)
            mapped = self.EFFORT_MAP.get(effort)
            if thinking is None:
                if effort not in (None, 'medium'):
                    raise ValueError('当前 Agent 不支持调整推理强度')
            elif mapped is not None:
                think_values = {o.get('value') for o in thinking.get('options') or []
                                if isinstance(o, dict)}
                if think_values and mapped not in think_values:
                    raise ValueError('此 Agent 不支持所选推理强度')
                if thinking.get('currentValue') != mapped:
                    self._set_config(handle, session, thinking['id'], mapped)
        return {'applied': True, 'confirmed': True, 'model': model, 'effort': effort}

    def catalog(self, session, refresh=False, kind=None, query='', offset=0, limit=200, ids=None):
        handle = self._handles.get(session.id)
        options = list(handle.config_options) if handle is not None else []
        model_option = self._find_category(options, self.MODEL_CATEGORY) or {}
        thinking = self._find_category(options, self.THINKING_CATEGORY) or {}
        # One bridge level per advertised value, in the agent's own order; the
        # exact name wins so kimi's low/high/max do not expand into the eight
        # folded EFFORT_MAP levels.
        efforts = []
        for option in thinking.get('options') or []:
            if not isinstance(option, dict):
                continue
            value = option.get('value')
            if not value:
                continue
            name = value if value in self.EFFORT_MAP else next(
                (n for n, m in self.EFFORT_MAP.items() if m == value), None)
            if name is not None and name not in efforts:
                efforts.append(name)
        models = [{'id': o.get('value'), 'name': o.get('name') or o.get('value'),
                   'efforts': efforts}
                  for o in model_option.get('options') or []
                  if isinstance(o, dict) and o.get('value')]
        current_model = handle.view.model if handle is not None else None
        current_effort = handle.view.thinking if handle is not None else None
        if kind == 'skills':
            return {'kind': 'skills', 'skills': [], 'errors': []}
        result = {'models': models, 'currentModel': current_model,
                  'currentEffort': current_effort, 'fastMode': {'allowed': False}}
        if kind == 'models':
            return {'kind': 'models', **result}
        return {**result, 'skills': []}

    def resolve_skills(self, session, skills):
        if not skills:
            return []
        raise ValueError('当前 Agent 暂不支持 Skill 选择')
