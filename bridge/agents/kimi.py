"""Kimi Code CLI profile: executable discovery and the thinking-level map."""
from pathlib import Path

from .acp import AcpUnavailable, discover_executable
from .acp_adapter import AcpAdapter
from .native_subagents import KimiSubagentStore


def available():
    """True when a Kimi Code CLI install is detectable on this machine."""
    return discover_executable('KIMI_BIN', '.kimi-code/bin/kimi', 'kimi') is not None


class KimiAdapter(AcpAdapter):
    kind = 'kimi'
    label = 'Kimi'
    auth_hint = '请先在电脑终端运行 kimi login 完成登录'
    # Kimi exposes one thinking select with low/high/max; bridge efforts fold
    # onto those three values.
    EFFORT_MAP = {'none': 'low', 'minimal': 'low', 'low': 'low', 'medium': 'high',
                  'high': 'high', 'xhigh': 'max', 'max': 'max', 'ultra': 'max'}

    _subagent_store = None

    def __init__(self, executable=None, **kwargs):
        executable = executable or discover_executable('KIMI_BIN', '.kimi-code/bin/kimi', 'kimi')
        if not executable:
            raise AcpUnavailable('未找到 kimi 命令，请先安装 Kimi Code CLI')
        super().__init__(executable, **kwargs)

    def _native_subagents(self):
        # Kimi archives spawned agents inside the session directory
        # (state.json plus per-agent wire logs), readable without the process.
        if self._subagent_store is None:
            try:
                self._subagent_store = KimiSubagentStore(Path.home() / '.kimi-code')
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
