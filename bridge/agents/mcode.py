"""MiniMax Code (mcode) profile: steer and goal ACP extension methods."""
from ..goal import GoalError, GoalUnavailable, GoalUnsupported
from .acp import AcpError, AcpTimeout, AcpUnavailable, discover_executable
from .acp_adapter import AcpAdapter
from .base import Capability


class MCodeAdapter(AcpAdapter):
    kind = 'mcode'
    label = 'MiniMax'
    capabilities = frozenset({Capability.STEER, Capability.GOALS})
    auth_hint = '请先在电脑终端运行 mcode login 完成登录'
    # mcode goal/create kicks the turn off by itself; a bridge activation turn
    # would double-start the goal.
    goal_needs_activation = False
    # mcode thinkingEffort select: default/low/medium/high/xhigh/max.
    EFFORT_MAP = {'none': 'default', 'minimal': 'low', 'low': 'low', 'medium': 'medium',
                  'high': 'high', 'xhigh': 'xhigh', 'max': 'max', 'ultra': 'max'}

    def __init__(self, executable=None, **kwargs):
        executable = executable or discover_executable('MCODE_BIN', '.minimax-code/bin/mcode', 'mcode')
        if not executable:
            raise AcpUnavailable('未找到 mcode 命令，请先安装 MiniMax Code')
        super().__init__(executable, **kwargs)

    @property
    def goal(self):
        # service checks `self.goal is None` before enabling goal routes.
        return self

    @property
    def goal_transport(self):
        return 'acp'

    def dispatch_turn(self, session, submission_id, entry, files):
        if entry.get('mode') != 'steer':
            return super().dispatch_turn(session, submission_id, entry, files)
        if files:
            raise ValueError('补充任务暂不支持附件')
        handle = self._handles.get(session.id)
        if handle is None or handle.dead or handle.load_error is not None:
            raise AcpUnavailable('会话尚未连接，请重新打开')
        handle.process.request('mcode/session/steer',
                               {'sessionId': session.id, 'text': entry['text']}, timeout=30)
        return {'accepted': True, 'steered': True}

    # -- native goal extension ---------------------------------------------

    def goal_runtime_available(self):
        return True

    def goal_call(self, method, *args):
        thread_id = args[0]
        handle = self._goal_handle(thread_id)
        if method == 'get_goal':
            rpc, extra = 'mcode/session/goal/get', {}
        elif method == 'set_goal':
            rpc, extra = 'mcode/session/goal/create', {'objective': args[1]}
        elif method == 'set_goal_status':
            rpc, extra = 'mcode/session/goal/patch', {'status': args[1]}
        elif method == 'edit_goal':
            rpc, extra = 'mcode/session/goal/patch', {'objective': args[1]}
        elif method == 'clear_goal':
            rpc, extra = 'mcode/session/goal/clear', {}
        else:
            raise GoalUnsupported(method)
        try:
            return handle.process.request(rpc, {'sessionId': thread_id, **extra}, timeout=30)
        except (AcpTimeout, AcpUnavailable) as exc:
            raise GoalUnavailable(str(exc)) from exc
        except AcpError as exc:
            raise GoalError(str(exc)) from exc

    def goal_read(self, thread_id):
        handle = self._handles.get(thread_id)
        if handle is None or handle.dead or not handle.ready.is_set():
            return None
        try:
            result = handle.process.request('mcode/session/goal/get',
                                            {'sessionId': thread_id}, timeout=5) or {}
        except AcpError:
            return None
        return result.get('goal')

    def goal_absent(self, thread_id):
        handle = self._handles.get(thread_id)
        if handle is None or handle.dead or not handle.ready.is_set():
            return False
        try:
            result = handle.process.request('mcode/session/goal/get',
                                            {'sessionId': thread_id}, timeout=5) or {}
        except AcpError:
            return False
        return not result.get('goal')

    def _goal_handle(self, thread_id):
        """Goal RPCs need the session loaded in a live process; attach on demand."""
        handle = self._handles.get(thread_id)
        if handle is not None and not handle.dead and handle.ready.is_set():
            return handle
        try:
            self.bridge.activate(thread_id)
        except Exception as exc:
            raise GoalUnavailable(f'无法连接会话进程：{exc}') from exc
        handle = self._handles.get(thread_id)
        if handle is None or handle.dead:
            raise GoalUnavailable('会话尚未连接，无法操作目标')
        return handle
