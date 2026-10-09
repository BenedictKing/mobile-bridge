"""Claude Code profile: claude-agent-acp wrapper discovery and the effort map."""
import os
import shlex
import shutil

from .acp import AcpUnavailable
from .acp_adapter import AcpAdapter


def discover_argv():
    """Resolve the launch command: CLAUDE_ACP_BIN, PATH binary, then npx."""
    candidate = os.environ.get('CLAUDE_ACP_BIN')
    if candidate:
        return shlex.split(candidate)
    binary = shutil.which('claude-agent-acp')
    if binary:
        return [binary]
    if shutil.which('npx'):
        return ['npx', '-y', '@agentclientprotocol/claude-agent-acp']
    return None


def available():
    """True when a Claude Code install is detectable locally.

    The npx fallback does not count: any machine with npx would otherwise
    auto-enable claude and trigger the first-run download.
    """
    return (bool(os.environ.get('CLAUDE_ACP_BIN'))
            or shutil.which('claude-agent-acp') is not None
            or shutil.which('claude') is not None)


class ClaudeAdapter(AcpAdapter):
    kind = 'claude'
    label = 'Claude'
    auth_hint = '请先在电脑终端运行 claude 并完成登录'
    # The npx fallback downloads the wrapper on first run (measured ~130s),
    # far past the generic 10s handshake budget.
    INITIALIZE_TIMEOUT = 180
    # claude-agent-acp persists a transcript only after the first message; until
    # then no other process can load or prompt the session, so creation keeps
    # its process alive as the session handle.
    KEEP_CREATED_PROCESS = True
    # claude effort select: default/low/medium/high/xhigh/max.
    EFFORT_MAP = {'none': 'default', 'minimal': 'low', 'low': 'low', 'medium': 'medium',
                  'high': 'high', 'xhigh': 'xhigh', 'max': 'max', 'ultra': 'max'}

    def __init__(self, argv=None, **kwargs):
        argv = argv or discover_argv()
        if not argv:
            raise AcpUnavailable('未找到 claude-agent-acp，请先安装 Claude Code（含 npx）')
        if isinstance(argv, str):
            argv = [argv]
        super().__init__(argv[0], **kwargs)
        self._full_argv = list(argv)

    def _argv(self):
        # The wrapper speaks ACP directly; there is no 'acp' subcommand.
        return list(self._full_argv)
