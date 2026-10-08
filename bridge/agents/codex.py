"""Codex protocol adapter: desktop IPC, SQLite store, app-server stdio, SSH."""
import subprocess
import uuid
from pathlib import Path

from .. import create as create_runtime
from ..account import Account
from ..accounts import Accounts
from ..catalog import Catalog
from ..create import CreationError, ForkUnavailable
from ..goal import GoalRPC, GoalUnavailable, read_native_goal, native_goal_absent
from ..ipc import DesktopIPC, IPCError
from ..model import apply_patches, computer_use_approval, ordered_turns
from ..remote import AppHosts, RemoteStore, RemoteCatalog, payload, upload_file
from ..store import SessionStore
from ..transport import ipc_endpoint
from .base import AgentAdapter, Capability


def _service():
    """Late binding through bridge.service keeps its public patch points valid."""
    from .. import service
    return service


class CodexAdapter(AgentAdapter):
    """Owns every Codex-specific transport the bridge delegates to."""

    kind = 'codex'
    label = 'Codex'
    capabilities = frozenset({
        Capability.STEER, Capability.GOALS, Capability.FORK, Capability.RENAME,
        Capability.SKILLS, Capability.FAST_MODE, Capability.ACCOUNTS,
        Capability.EDIT_MESSAGE, Capability.FULL_HISTORY,
        Capability.NOTIFICATION_SCAN, Capability.SERVICE_TIER,
    })

    def __init__(self, codex_home, data_dir=None, host='local', alias=None, ipc_path=None,
                 codex_bin=None, goal_rpc=None, owner_goal=None):
        super().__init__()
        self.host = host
        self.codex_home = Path(codex_home)
        self.alias = alias
        self.hosts = AppHosts(codex_home)
        self.store = RemoteStore(alias) if alias else SessionStore(codex_home)
        self.catalog_reader = RemoteCatalog(alias) if alias else Catalog(codex_home, codex_bin)
        self.account = Account(codex_home, data_dir, codex_bin) if host == 'local' else None
        self.accounts = None
        self.goal = goal_rpc or (GoalRPC(codex_home, codex_bin or Catalog.find_runtime()) if host == 'local' else None)
        self.owner_goal = owner_goal
        self.goal_transport = 'owner' if owner_goal else 'sidecar'
        self.ipc = DesktopIPC(ipc_path or ipc_endpoint(codex_home), self.handle_event, self.handle_disconnect)

    def bind(self, bridge):
        """Attach the owning bridge and create the local account manager."""
        super().bind(bridge)
        if self.accounts is None and self.host == 'local':
            self.accounts = Accounts(bridge)

    def validate_id(self, thread_id):
        """Codex session ids are UUIDs."""
        uuid.UUID(thread_id)

    @property
    def home(self):
        return self.store.home

    def upload_transport(self):
        return (lambda *args: upload_file(self.alias, *args)) if self.alias else None

    def close(self):
        self.ipc.close()
        if self.goal:
            self.goal.close()

    def handle_event(self, message):
        bridge = self.bridge
        if message.get("method") == "client-status-changed":
            params = message.get("params", {})
            if params.get("status") == "disconnected":
                with bridge.lock:
                    sessions = list(bridge.live.values())
                for session in sessions:
                    if session.owner == params.get("clientId"):
                        self._invalidate(session, "桌面会话连接已断开，正在等待重连")
            # Owner changes can make previously unanswered probes available.
            bridge.notification_cursor = 0
            bridge.notification_boundary.clear()
            bridge.notification_event.set()
            return
        if message.get("method") != "thread-stream-state-changed":
            return
        if message.get("version") != 11:
            self.handle_disconnect()
            return
        params = message.get("params", {})
        if params.get("hostId") != bridge.host:
            return
        with bridge.lock:
            session = bridge.live.get(params.get("conversationId"))
        if session is None:
            return
        change = params.get("change", {})
        with session.condition:
            if session.discovering and change.get("type") == "snapshot" and (change.get("conversationState") or {}).get("id") == session.id:
                session.owner = message.get("sourceClientId")
                session.discovering = False
            if not session.owner or message.get("sourceClientId") != session.owner:
                return
            try:
                if change.get("type") == "snapshot":
                    state = change["conversationState"]
                    if state.get("id") != session.id:
                        raise ValueError("Session mismatch")
                    session.state = state
                elif change.get("type") == "patches":
                    if session.revision is None or change.get("baseRevision") != session.revision:
                        raise ValueError("Revision mismatch")
                    session.state = apply_patches(session.state, change["patches"])
                else:
                    return
                session.revision = change["revision"]
                session.connected = True
                session.error = None
                session.changed()
                with bridge.lock:
                    bridge.notification_dirty.add(session.id)
                bridge.notification_event.set()
            except (ValueError, KeyError, IndexError, TypeError):
                session.connected = False
                session.revision = None
                session.error = "同步状态发生变化，正在重新读取桌面快照"
                session.changed()

    def _invalidate(self, session, message):
        with session.condition:
            session.connected = False
            session.revision = None
            session.error = message
            session.changed()

    def detach(self, session):
        if session.connected or session.discovering:
            try:
                self.ipc.follow(session.id, session.owner, False, host=self.bridge.host)
            except IPCError:
                pass

    def is_active(self, session):
        with session.condition:
            return (session.state or {}).get('threadRuntimeStatus', {}).get('type') == 'active'

    def require_ready(self, session):
        with session.condition:
            if not session.connected or not session.owner:
                raise IPCError(session.error or "请先在桌面 App 打开此聊天")

    def projects(self):
        return self.hosts.projects()

    def handle_disconnect(self):
        bridge = self.bridge
        bridge.notification_cursor = 0
        bridge.notification_boundary.clear()
        bridge.notification_event.set()
        with bridge.lock:
            sessions = list(bridge.live.values())
        for session in sessions:
            self._invalidate(session, "桌面 App 连接已断开，正在等待重连")

    def attach(self, session, timeout=1.5):
        bridge = self.bridge
        with session.attach_lock:
            if session.connected or bridge.closed.is_set():
                return
            try:
                # Owners answer a follow with a fresh snapshot. This also works
                # for remote/background owners absent from owner-discovery.
                self.ipc.connect()
                with session.condition:
                    session.owner = None
                    session.revision = None
                    session.discovering = True
                self.ipc.follow(session.id, None, host=bridge.host)
                with session.condition:
                    session.condition.wait_for(lambda: session.connected or bridge.closed.is_set(), timeout=timeout)
                    # Keep following: a cold chat can publish after this short
                    # wait. Missing live state does not make saved history fail.
            except IPCError as exc:
                with session.condition:
                    session.discovering = False
                    session.connected = False
                    session.error = str(exc)
                    session.changed()

    def check_provider(self, session):
        if self.bridge.host == 'local' and self.accounts and self.accounts.active_matches():
            row = self.accounts.row(self.accounts.index['activeId'])
            expected = ('openai',) if row['kind'] == 'chatgpt' else ('openai', 'bridge_api')
            if session.view().get('provider') not in expected:
                raise ValueError('此聊天保留了原提供商，请切回对应接入或新建聊天')

    def call(self, session, method, params, timeout=30):
        self.check_provider(session)
        return self.ipc.request(method, {"conversationId": session.id, **params}, target=session.owner, host=self.bridge.host, timeout=timeout)["result"]

    def dispatch_turn(self, session, submission_id, entry, files):
        request = {"threadId": session.id, "input": [{"type": "text", "text": entry["text"], "text_elements": []}], "clientUserMessageId": submission_id}
        request['input'].extend({'type': 'localImage', 'path': f['path']} for f in files if f['image'])
        context = {'attachments': [], 'commentAttachments': []}
        if files:
            context['fileAttachments'] = [{'path': f['path'], 'label': f['name']} for f in files]
        if entry.get("workMode") is not None:
            with session.condition:
                current = session.state
                model = current.get("latestModel") or (current.get("latestCollaborationMode") or {}).get("settings", {}).get("model")
                effort = current.get("latestReasoningEffort") or (current.get("latestThreadSettings") or {}).get("effort")
            if not model:
                raise ValueError("尚未取得桌面模型设置，请重新连接后再切换模式")
            request["collaborationMode"] = {"mode": "plan" if entry["workMode"] == "plan" else "default",
                                            "settings": {"model": model, "reasoning_effort": effort, "developer_instructions": None}}
        request["input"].extend({"type": "skill", "name": s["name"], "path": s["path"]} for s in entry.get("skills", []))
        if entry["mode"] != "steer":
            return self.bridge._call(session, "thread-follower-start-turn", {
                "turnStart": {"request": request, "context": {"inheritThreadSettings": True, **context}}}, timeout=90)
        return self.bridge._call(session, "thread-follower-steer-turn", {
            "input": request["input"], "clientUserMessageId": submission_id,
            "restoreMessage": {"request": request, "context": context},
            "attachments": []}, timeout=30)

    def update_settings(self, session, model, effort, fast_mode=None):
        with session.action_lock:
            # Recheck login/model/policy on the execution host before a speed change.
            catalog = self.catalog(session, refresh=fast_mode is not None)
            known = next((m for m in catalog["models"] if m["id"] == model), None)
            if known and known["efforts"] and effort not in known["efforts"]:
                raise ValueError("这个模型不支持所选推理强度")
            settings = {'model': model, 'effort': effort}
            if fast_mode is not None:
                if not catalog.get('fastMode', {}).get('allowed') or not known or not known.get('fastTier'):
                    raise ValueError('当前账号、模型或工作区不支持 Fast 模式，请刷新模型设置')
                # null can inherit a default; "default" explicitly opts out of Fast.
                settings['serviceTier'] = known['fastTier'] if fast_mode else 'default'
            result = self.bridge._call(session, "thread-follower-update-thread-settings", {"threadSettings": settings})
        if not result.get("applied"):
            raise IPCError("桌面未应用模型设置，请刷新后重试")
        with session.condition:
            confirmed = session.condition.wait_for(lambda: session.state.get("latestModel") == model and session.view().get("effort") == effort and
                (fast_mode is None or (session.state.get('latestThreadSettings') or {}).get('serviceTier') == settings['serviceTier']), timeout=5)
        return {"applied": True, "confirmed": confirmed, "model": model, "effort": effort,
                **({'serviceTier': settings['serviceTier']} if fast_mode is not None else {})}

    def catalog(self, session, refresh=False, kind=None, query='', offset=0, limit=200, ids=None):
        with session.condition:
            cwd = session.state.get("cwd")
            model = session.state.get("latestModel")
            effort = session.state.get("latestReasoningEffort") or (session.state.get("latestThreadSettings") or {}).get("effort")
            provider = session.view().get('provider')
        if kind in ('models', 'skills'):
            value = self.catalog_reader.get_kind(kind, cwd, refresh=refresh, provider=provider,
                                                 query=query, offset=offset, limit=limit, ids=ids)
            if kind == 'models':
                with session.condition:
                    view = session.view()
                return {**value, 'kind': 'models', 'currentModel': model or value.get('currentModel'),
                        'currentEffort': effort or value.get('currentEffort'),
                        'fastMode': {**value.get('fastMode', {}), 'allowed': view.get('provider') == 'openai' and value.get('fastMode', {}).get('allowed') is True},
                        **({'currentServiceTier': view['serviceTier']} if 'serviceTier' in view else {})}
            return {**value, 'kind': 'skills'}
        catalog = self.catalog_reader.get(cwd, refresh=refresh, provider=provider)
        with session.condition:
            view = session.view()
        return {**catalog, "currentModel": model or catalog.get("currentModel"),
                "currentEffort": effort or catalog.get("currentEffort"),
                'fastMode': {**catalog.get('fastMode', {}), 'allowed': view.get('provider') == 'openai' and catalog.get('fastMode', {}).get('allowed') is True},
                **({'currentServiceTier': view['serviceTier']} if 'serviceTier' in view else {})}

    def resolve_skills(self, session, skills):
        if not isinstance(skills, list) or len(skills) > 8 or not all(isinstance(s, str) for s in skills):
            raise ValueError("最多选择 8 个 Skill")
        if not skills:
            return []
        with session.condition:
            cwd = session.state.get("cwd") or str(self.codex_home)
        identifiers = sorted(set(skills))
        rows = self.catalog_reader.validate_skills(cwd, identifiers)
        by_id = {row["id"]: row for row in rows}
        selected = []
        for key in identifiers:
            skill = by_id.get(key)
            if not skill:
                raise ValueError("Skill 不可用，请刷新列表")
            selected.append({"id": key, "name": skill["name"], "path": skill["path"]})
        return selected

    def rename(self, session, thread_id, title):
        service = _service()
        if isinstance(self.store, RemoteStore):
            source = Path(create_runtime.__file__).read_text(encoding='utf-8')
            source += '\nimport shutil\nhome=Path(os.environ.get("CODEX_HOME", str(Path.home()/".codex")))\n'
            source += 'runtime=shutil.which("codex") or str(Path.home()/".local/bin/codex")\n'
            source += 'rename_thread(runtime, home, **' + payload({'thread_id': thread_id, 'title': title}) + ')\nprint(json.dumps({"ok":True}))\n'
            service.ssh_read(self.store.alias, source, timeout=90)
            with self.store.lock:
                self.store.cache.clear()
        else:
            service.rename_thread(self.catalog_reader.executable, self.codex_home, thread_id, title)

    def create_session(self, project, title):
        service = _service()
        if project['host'] == 'local':
            thread_id = service.create_empty(self.catalog_reader.executable, self.codex_home, project['cwd'], title)
        else:
            host = self.hosts.hosts()[project['host']]
            source = Path(create_runtime.__file__).read_text(encoding='utf-8')
            source += '\nimport shutil\nhome=Path(os.environ.get("CODEX_HOME", str(Path.home()/".codex")))\n'
            source += 'runtime=shutil.which("codex") or str(Path.home()/".local/bin/codex")\n'
            source += 'print(json.dumps({"id":create_empty(runtime, home, **' + payload({'cwd': project['cwd'], 'title': title}) + ')}))\n'
            thread_id = service.ssh_read(host['alias'], source, timeout=90)['id']
        self.validate_id(thread_id)
        return thread_id

    def open_created(self, thread_id, host):
        try:
            _service().open_in_desktop(thread_id, host)
            return True
        except (OSError, CreationError, subprocess.SubprocessError):
            return False

    @staticmethod
    def fork_settings(state):
        result = {'modelProvider': state.get('modelProvider'), 'model': state.get('latestModel')}
        if not all(result.values()):
            raise ValueError('尚未取得原会话的模型配置，请重新连接')
        effort = state.get('latestReasoningEffort') or (state.get('latestThreadSettings') or {}).get('effort')
        if effort:
            result['config'] = {'model_reasoning_effort': effort}
        latest = state.get('latestThreadSettings') or {}
        if 'serviceTier' in latest:
            result['serviceTier'] = latest['serviceTier']
        permissions = state.get('currentPermissions') or {}
        profile = latest.get('activePermissionProfile', permissions.get('activePermissionProfile'))
        if profile and profile.get('id'):
            result['permissions'] = profile['id']
        for key in ('approvalPolicy', 'approvalsReviewer', 'runtimeWorkspaceRoots'):
            if key in permissions:
                result[key] = permissions[key]
        return result

    def fork_thread(self, thread_id, args):
        service = _service()
        if self.bridge.host == 'local':
            return service.fork_copy(self.catalog_reader.executable, self.codex_home, **args)
        host = self.hosts.hosts()[self.bridge.host]
        source = Path(create_runtime.__file__).read_text(encoding='utf-8')
        source += '\nimport shutil\nhome=Path(os.environ.get("CODEX_HOME", str(Path.home()/".codex")))\n'
        source += 'runtime=shutil.which("codex") or str(Path.home()/".local/bin/codex")\n'
        source += 'try:\n print(json.dumps({"id":fork_copy(runtime, home, **' + payload(args) + ')}))\n'
        source += 'except ForkUnavailable as exc:\n print(json.dumps({"unavailable":str(exc)}))\n'
        result = service.ssh_read(host['alias'], source, timeout=120)
        if 'unavailable' in result:
            raise ForkUnavailable(result['unavailable'])
        return result['id']

    def edit_last_user_turn(self, target, params):
        return self.bridge._call(target, 'thread-follower-edit-last-user-turn', params, timeout=90)

    def respond_to_request(self, session, pending, response):
        method = pending.get("method", "")
        params = pending.get("params", {})
        mapping = {
            "item/commandExecution/requestApproval": "thread-follower-command-approval-decision",
            "item/fileChange/requestApproval": "thread-follower-file-approval-decision",
            "item/permissions/requestApproval": "thread-follower-permissions-request-approval-response",
            "item/tool/requestUserInput": "thread-follower-submit-user-input",
            "tool/requestUserInput": "thread-follower-submit-user-input",
            "mcpServer/elicitation/request": "thread-follower-submit-mcp-server-elicitation-response",
        }
        if method not in mapping:
            raise ValueError("此类请求请在桌面 App 中处理")
        payload = {"requestId": pending["id"]}
        if method in ("item/commandExecution/requestApproval", "item/fileChange/requestApproval"):
            decision = response.get("decision")
            if decision not in ("accept", "decline", "cancel"):
                raise ValueError("只支持本次批准、拒绝或取消")
            available = params.get("availableDecisions")
            if available and decision not in available:
                raise ValueError("该审批不支持所选操作")
            payload["decision"] = decision
        elif method == "item/permissions/requestApproval":
            if response.get("decision") not in ("accept", "decline"):
                raise ValueError("请选择批准或拒绝")
            payload["response"] = {"permissions": params.get("permissions", {}) if response["decision"] == "accept" else {}, "scope": "turn"}
        elif method in ("item/tool/requestUserInput", "tool/requestUserInput"):
            answers = response.get("answers")
            if not isinstance(answers, dict):
                raise ValueError("请填写问题答案")
            allowed_ids = {q["id"] for q in params.get("questions", [])}
            if not allowed_ids or set(answers) != allowed_ids or any(not isinstance(v, list) or not v or not all(isinstance(s, str) and len(s) <= 20000 for s in v) for v in answers.values()):
                raise ValueError("答案不完整或格式不正确")
            payload["response"] = {"answers": {k: {"answers": v} for k, v in answers.items()}}
        else:
            action = response.get("action")
            if action not in ("accept", "decline", "cancel"):
                raise ValueError("未知确认操作")
            computer = computer_use_approval(params)
            if computer:
                if set(response) - {'action', 'persist'} or ('persist' in response and
                        (action != 'accept' or response['persist'] not in computer['persistModes'])):
                    raise ValueError('此请求不支持所选授权范围')
                payload['response'] = {'action': action, 'content': {} if action == 'accept' else None}
                if 'persist' in response:
                    payload['response']['_meta'] = {'persist': response['persist']}
            else:
                if set(response) - {'action', 'content'}:
                    raise ValueError('此请求不支持所选授权范围')
                content = response.get("content")
                if action == "accept":
                    _service().validate_form(content, params.get("requestedSchema", {}))
                payload["response"] = {"action": action, "content": content if action == "accept" else None}
        return self.bridge._call(session, mapping[method], payload)

    def interrupt(self, session):
        with session.condition:
            active = next((t for t in reversed(ordered_turns(session.state)) if t.get("status") == "inProgress"), None)
            if not active or not active.get("turnId"):
                raise ValueError("当前没有可停止的任务")
            turn_id = active["turnId"]
        return self.bridge._call(session, "thread-follower-interrupt-turn", {"mode": "user-stop", "expectedTurnId": turn_id})

    def full_history(self, session):
        return self.bridge._call(session, "thread-follower-load-complete-history", {}, timeout=180)

    def activate_owner(self, session):
        try:
            _service().open_in_desktop(session.id, self.bridge.host)
        except (OSError, CreationError, subprocess.SubprocessError) as exc:
            raise IPCError('无法在电脑 Codex 中加载此聊天；操作未发送，请检查 Codex 是否已安装并运行。') from exc

    def goal_call(self, method, *args):
        """Use the owner when available, otherwise the isolated sidecar."""
        if self.owner_goal is not None:
            # A configured owner transport is authoritative. Never silently
            # fall back after the owner path was selected: that could create a
            # second writer and reintroduce split-brain state.
            return getattr(self.owner_goal, method)(*args)
        last = None
        for attempt in range(2 if method == 'get_goal' else 1):
            try:
                return getattr(self.goal, method)(*args)
            except GoalUnavailable as exc:
                last = exc
                self.goal.close()
                if method == 'get_goal' and attempt == 0 and not self.bridge.closed.is_set():
                    self.bridge.closed.wait(.25)
                    continue
                raise
        raise last

    def goal_read(self, thread_id):
        return read_native_goal(self.codex_home, thread_id)

    def goal_absent(self, thread_id):
        return native_goal_absent(self.codex_home, thread_id)

    def goal_runtime_available(self):
        if self.bridge.host != "local":
            return False
        if self.owner_goal is not None:
            return True
        executable = getattr(self.goal, "executable", None)
        return bool(executable and Path(executable).is_file())

    def session_meta(self, thread_id):
        return self.store.get(thread_id)

    def saved_history(self, session):
        return self.store.history(session.id, turn_limit=session.history_limit)

    def list_sessions(self, label, limit=100, offset=0, query='', archived=False):
        rows = self.store.list(limit=limit + offset, archived=archived, query=query)
        rows = self.hosts.decorate(rows, self.host, label)
        for row in rows:
            row['agent'] = self.kind
            row['capabilities'] = sorted(self.capabilities)
        return rows

    def recencies(self, identifiers):
        return self.store.recencies(identifiers)

    def notification_changes(self, cursor):
        return self.store.notification_changes(cursor)
