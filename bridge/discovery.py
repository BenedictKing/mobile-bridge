"""定时会话发现：留意网页之外新开的聊天。

网关的会话列表只在请求时读取各 Agent 的原生存储；网页自己不刷新时，
在终端或桌面 App 里新开的会话就永远不会出现。本模块在后台周期性重读
每个 Agent 的会话列表，维护一个单调递增的 ``revision``：身份、标题或
项目发生变化的任何一刻都会使其自增。``/api/activity`` 把该值随行下发，
前端据此自动重载列表，无需额外轮询。

发现只读各 bridge 自己的原生主机（Codex 走本地 SQLite，ACP Agent 强制
刷新 ``session/list`` 缓存）；SSH 远端列表每次都要跨网络读取，仍只在
显式列表请求时发现。刷新有节制：没有客户端在轮询 activity（无人观看）
超过 ``IDLE_AFTER`` 秒时跳过本轮，避免为无人看的列表反复 spawn Agent
进程。
"""
import logging
import threading
import time

LOG = logging.getLogger(__name__)

# 发现循环只比对身份/标题/项目；recency 的变化由 activity 轮询单独下发，
# 不触发列表重载。
FINGERPRINT_FIELDS = ('id', 'title', 'projectKey')


class ListDiscovery:
    """Periodically re-read agent session lists and version their contents."""

    INTERVAL = 30.0
    IDLE_AFTER = 120.0

    def __init__(self, bridges):
        # bridges: kind → bridge；注册在网关启动前完成，之后不再变动。
        self.bridges = dict(bridges)
        self.revision = 0
        self._fingerprint = None
        self._watched = time.monotonic()
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._thread = None

    def touch(self):
        """Record that a client is still watching the session list."""
        with self._lock:
            self._watched = time.monotonic()

    def _watched_recently(self):
        with self._lock:
            return time.monotonic() - self._watched <= self.IDLE_AFTER

    def start(self):
        if self._thread is not None:
            return
        self._thread = threading.Thread(target=self._run, daemon=True,
                                        name='list-discovery')
        self._thread.start()

    def close(self):
        self._stop.set()

    def _run(self):
        while not self._stop.wait(self.INTERVAL):
            if not self._watched_recently():
                continue
            try:
                self.refresh()
            except Exception:  # 单轮失败只记日志，不影响下一轮
                LOG.debug('会话发现失败', exc_info=True)

    def refresh(self):
        """Re-read every bridge's own session list; bump revision on changes."""
        rows = []
        for kind, bridge in self.bridges.items():
            try:
                bridge.adapter.refresh_list()
                rows.extend((kind, row) for row in bridge.refresh_discovery())
            except Exception:
                LOG.debug('Agent 会话发现失败：%s', kind, exc_info=True)
        fingerprint = sorted(
            (kind,) + tuple(str(row.get(field) or '') for field in FINGERPRINT_FIELDS)
            for kind, row in rows)
        if fingerprint != self._fingerprint:
            self._fingerprint = fingerprint
            self.revision += 1
        return fingerprint
