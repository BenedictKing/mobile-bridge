"""DeepSeek Harness (dsh) ACP profile: discovery, steering, and mode mapping."""
import os
import shlex
import shutil
from pathlib import Path

from .acp import AcpUnavailable
from .acp_adapter import AcpAdapter
from .acp_view import AcpSessionView
from .base import Capability


def discover_argv():
    """Resolve the launch command: DSH_ACP_BIN, PATH binary, then npx."""
    candidate = os.environ.get('DSH_ACP_BIN')
    if candidate:
        return shlex.split(candidate)
    binary = shutil.which('dsh-acp')
    if binary:
        return [binary]
    if shutil.which('npx'):
        return ['npx', '-y', '@openma/deepseek-harness-acp']
    return None


def available():
    """True when a DeepSeek Harness install is detectable locally.

    The npx fallback does not count (same reason as claude), but a ~/.dsh
    directory does: dsh may only ever have run via npx yet still leave its
    credentials and config there.
    """
    return (bool(os.environ.get('DSH_ACP_BIN'))
            or shutil.which('dsh-acp') is not None
            or shutil.which('dsh') is not None
            or (Path.home() / '.dsh').is_dir())


class DshSessionView(AcpSessionView):
    """dsh's modes and its 'mode' config option are permission presets
    (read-only/workspace-write/danger-full-access); only the collaboration_mode
    option feeds the bridge's collaboration-mode slot."""

    def apply_config(self, config_options, modes):
        super().apply_config(config_options, None)

    def _apply(self, kind, update):
        if kind == 'current_mode_update':
            return  # Permission preset change, not a collaboration mode.
        super()._apply(kind, update)

    def _apply_config_option(self, option):
        slot = option.get('category') or option.get('id')
        if slot == 'mode':
            return
        if slot == 'collaboration_mode':
            value = option.get('currentValue')
            if value is not None:
                self.mode = value  # apply_config already holds the lock.
            return
        super()._apply_config_option(option)


class DshAdapter(AcpAdapter):
    kind = 'dsh'
    label = 'DeepSeek'
    capabilities = frozenset({Capability.STEER})
    auth_hint = '请先在电脑终端运行 dsh-acp login 完成登录'
    VIEW_CLASS = DshSessionView
    # The npx fallback downloads the wrapper on first run (measured ~28s warm),
    # far past the generic 10s handshake budget.
    INITIALIZE_TIMEOUT = 180
    # dsh lists a session right after session/new, and a gracefully closed
    # process releases its write handle, so creation needs no kept process.
    KEEP_CREATED_PROCESS = False
    # dsh effort select: off/low/high/max.
    EFFORT_MAP = {'none': 'off', 'minimal': 'low', 'low': 'low', 'medium': 'high',
                  'high': 'high', 'xhigh': 'max', 'max': 'max', 'ultra': 'max'}

    def __init__(self, argv=None, **kwargs):
        argv = argv or discover_argv()
        if not argv:
            raise AcpUnavailable('未找到 dsh-acp，请先安装 DeepSeek Harness（含 npx）')
        if isinstance(argv, str):
            argv = [argv]
        super().__init__(argv[0], **kwargs)
        self._full_argv = list(argv)

    def _argv(self):
        # The wrapper speaks ACP directly; there is no 'acp' subcommand.
        return list(self._full_argv)

    def dispatch_turn(self, session, submission_id, entry, files):
        if entry.get('mode') != 'steer':
            return super().dispatch_turn(session, submission_id, entry, files)
        if files:
            raise ValueError('补充任务暂不支持附件')
        handle = self._handles.get(session.id)
        if handle is None or handle.dead or handle.load_error is not None:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        result = handle.process.request('_session/steering', {
            'sessionId': session.id,
            'prompt': [{'type': 'text', 'text': entry['text']}],
            '_meta': {'steering': {'idleBehavior': 'promptRequired'}},
        }, timeout=30) or {}
        if result.get('outcome') == 'injected':
            return {'accepted': True, 'steered': True}
        # promptRequired: the turn ended in the race window; send normally.
        return super().dispatch_turn(session, submission_id, {**entry, 'mode': 'send'}, files)

    def _set_mode(self, handle, session, mode_id):
        # Bridge plan/default map to dsh's collaboration_mode config option;
        # session/set_mode would flip the permission preset instead.
        option = next((o for o in handle.config_options
                       if isinstance(o, dict) and o.get('id') == 'collaboration_mode'), None)
        values = {o.get('value') for o in (option or {}).get('options') or []
                  if isinstance(o, dict)}
        if option is None or (values and mode_id not in values):
            raise ValueError('当前 Agent 不支持此工作模式')
        if option.get('currentValue') != mode_id:
            self._set_config(handle, session, option['id'], mode_id)
