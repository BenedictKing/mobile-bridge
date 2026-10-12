"""ZCode profile: zcode-acp-server wrapper discovery, mode and effort maps."""
import os
import shlex
import shutil
from pathlib import Path

from .acp import AcpUnavailable
from .acp_adapter import AcpAdapter
from .native_subagents import ZcodeSubagentStore, _read_json
from . import zcode_creds


def discover_argv():
    """Resolve the launch command: ZCODE_ACP_BIN, PATH binary, then npx."""
    candidate = os.environ.get('ZCODE_ACP_BIN')
    if candidate:
        return shlex.split(candidate)
    for name in ('zcode-acp-server', 'zcode-acp'):
        binary = shutil.which(name)
        if binary:
            return [binary]
    if shutil.which('npx'):
        return ['npx', '-y', 'zcode-acp-server']
    return None


def available():
    """True when a ZCode install is detectable locally.

    The npx fallback does not count (same reason as claude), but a ~/.zcode
    directory does: the wrapper reads its credentials from there, so a machine
    that only ever ran the desktop app still hosts usable sessions.
    """
    try:
        zcode_dir = (Path.home() / '.zcode').is_dir()
    except RuntimeError:  # Windows environments without HOME/USERPROFILE
        zcode_dir = False
    return (bool(os.environ.get('ZCODE_ACP_BIN'))
            or shutil.which('zcode-acp-server') is not None
            or shutil.which('zcode-acp') is not None
            or shutil.which('zcode') is not None
            or zcode_dir)


class ZcodeAdapter(AcpAdapter):
    kind = 'zcode'
    label = 'ZCode'
    auth_hint = '请先在电脑安装 ZCode 并完成登录'
    # The npx fallback downloads the wrapper on first run, far past the
    # generic 10s handshake budget.
    INITIALIZE_TIMEOUT = 180
    # zcode-acp's session/new returns a lazy placeholder; the backend session
    # exists only after the first prompt, and until then no other process can
    # list or load the session — creation keeps its process alive as the handle.
    KEEP_CREATED_PROCESS = True
    # Bridge plan/default map onto zcode's session modes (plan/build/edit/
    # yolo/auto). 'default' becomes 'yolo', the mode zcode-acp itself
    # hardcodes at backend session/create.
    MODE_MAP = {'plan': 'plan', 'default': 'yolo'}
    # The vocabulary is per-model: GLM-5.3 offers low/high/max (identity),
    # GLM-5-Turbo only enabled/off — a binary toggle folded onto the bridge's
    # minimal/none levels. The generic settings path validates every mapped
    # value against the options the session actually advertises.
    EFFORT_MAP = {'none': 'off', 'minimal': 'enabled', 'low': 'low', 'medium': 'medium',
                  'high': 'high', 'xhigh': 'xhigh', 'max': 'max', 'ultra': 'ultra'}

    def __init__(self, argv=None, lazy_map=None, **kwargs):
        discovered = argv is None
        argv = argv or discover_argv()
        if not argv:
            raise AcpUnavailable('未找到 zcode-acp-server，请先安装 ZCode CLI（需要 Node.js 22+ 与 npx）')
        if isinstance(argv, str):
            argv = [argv]
        super().__init__(argv[0], **kwargs)
        self._full_argv = list(argv)
        if lazy_map is not None:
            self._lazy_map = Path(lazy_map)
        else:
            try:
                self._lazy_map = Path.home() / '.zcode' / 'v2' / 'acp-lazy-sessions.json'
            except RuntimeError:  # Windows environments without HOME/USERPROFILE
                self._lazy_map = None
        # Real launches (gateway startup) keep the coding-plan key slot in
        # ~/.zcode/v2/config.json healed; explicit argv (tests) must not touch
        # the machine's real ZCode state.
        if discovered:
            zcode_creds.ensure_and_watch()

    def _argv(self):
        # The wrapper speaks ACP directly; there is no 'acp' subcommand.
        return list(self._full_argv)

    def _canonical_id(self, thread_id):
        # session/new's lazy placeholder never appears in session/list; once
        # the first prompt materializes the backend session, the list carries
        # the sess_* id this map records, and that covers the placeholder.
        if self._lazy_map is None or thread_id.startswith('sess_'):
            return None
        entry = (_read_json(self._lazy_map) or {}).get(thread_id)
        if isinstance(entry, dict) and entry.get('zcodeSid'):
            return entry['zcodeSid']
        return None

    _subagent_store = None

    def _native_subagents(self):
        # The ACP wrapper never replays spawned children; zcode archives them
        # under ~/.zcode/cli/agents with parent metadata per directory.
        if self._subagent_store is None:
            try:
                self._subagent_store = ZcodeSubagentStore(Path.home() / '.zcode')
            except RuntimeError:  # Windows environments without HOME/USERPROFILE
                return None
        return self._subagent_store

    def subagent_threads(self, thread_id):
        store = self._native_subagents()
        return store.children(thread_id) if store else []

    def subagent_history(self, thread_id):
        store = self._native_subagents()
        if store is None:
            raise KeyError('此会话没有可读取的子 Agent 记录')
        return store.history(thread_id)

    def _set_mode(self, handle, session, mode_id):
        super()._set_mode(handle, session, self.MODE_MAP.get(mode_id, mode_id))
