"""Kimi Code CLI profile: executable discovery and the thinking-level map."""
from .acp import AcpUnavailable, discover_executable
from .acp_adapter import AcpAdapter


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

    def __init__(self, executable=None, **kwargs):
        executable = executable or discover_executable('KIMI_BIN', '.kimi-code/bin/kimi', 'kimi')
        if not executable:
            raise AcpUnavailable('未找到 kimi 命令，请先安装 Kimi Code CLI')
        super().__init__(executable, **kwargs)
