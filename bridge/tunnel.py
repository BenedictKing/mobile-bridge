"""Shared cloudflared supervision plus the opt-in Quick Tunnel.

Connector states: stopped -> starting -> ready -> unhealthy -> restarting.
A child that stays alive with stale connections (typical after sleep/wake or
a network change) is detected by a local metrics probe and clock-jump wake
detection, then retired with terminate/kill escalation and respawned with
capped exponential backoff, instead of waiting on a hung process forever.
"""
import os
import re
import socket
import subprocess
import threading
import time
from pathlib import Path
from urllib.error import URLError
from urllib.request import urlopen

from .notifications import write_json


class CloudflareConnector:
    """Lifecycle shared by the quick and named Cloudflare connectors.

    The subclass supplies argv construction, the module-local Popen call,
    output parsing and status-file mapping; the base owns spawn, monitoring,
    health probing and restarts so both tunnels recover identically.
    """

    CHECK_INTERVAL = 5.0
    PROBE_TIMEOUT = 2.0
    FAIL_STREAK = 3
    WAKE_JUMP = 30.0
    RESTART_DELAY = 2
    MAX_DELAY = 60
    SUPERVISOR_NAME = 'cloudflare-connector'

    def __init__(self, data_dir):
        self.data_dir = Path(data_dir)
        self.closed = threading.Event()
        self.ready = threading.Event()
        self.broken = threading.Event()
        self.finished = threading.Event()
        self.unhealthy = threading.Event()
        self.failure = None
        self.process = None
        self.reader = None
        self.watchdog = None
        self.metrics_port = None
        self.lifecycle_lock = threading.Lock()
        self.supervisor = None

    # -- subclass surface -------------------------------------------------
    def _arguments(self):
        """Full cloudflared argv; --metrics must use self.metrics_port."""
        raise NotImplementedError

    def _popen(self, arguments):
        raise NotImplementedError

    def _pump(self, process):
        """Consume child output until EOF; may set ready/broken."""
        raise NotImplementedError

    def _starting(self):
        pass

    def _lost(self):
        """Invalidate the published entry before a restart."""

    def _closing(self):
        pass

    def _stopped(self):
        pass

    # -- shared machinery ---------------------------------------------------
    def start_supervisor(self):
        self.supervisor = threading.Thread(target=self._run, name=self.SUPERVISOR_NAME, daemon=True)
        self.supervisor.start()

    def _run(self):
        delay = 0
        while not self.closed.wait(delay):
            try:
                became_ready = self._run_child()
            except (OSError, ValueError):
                became_ready = False
            if self.closed.is_set():
                break
            self._lost()
            self._retire()
            delay = self._next_delay(delay, became_ready)
        self._stopped()

    def _next_delay(self, delay, became_ready):
        return self.RESTART_DELAY if became_ready else min(self.MAX_DELAY, max(self.RESTART_DELAY, delay * 2))

    def _run_child(self):
        """Run one child until it exits, reports a loss or fails probing."""
        with self.lifecycle_lock:
            if self.closed.is_set():
                return False
            self.metrics_port = self._free_metrics_port()
            self.ready.clear()
            self.broken.clear()
            self.finished.clear()
            self.unhealthy.clear()
            self.failure = None
            self._starting()
            process = self._popen(self._arguments())
            self.process = process
            self.reader = threading.Thread(target=self._read, args=(process,), daemon=True)
            self.watchdog = threading.Thread(target=self._watch, args=(process,), daemon=True)
            self.reader.start()
            self.watchdog.start()
        became_ready = False
        while not self.closed.is_set():
            if self.ready.is_set():
                became_ready = True
            if self.broken.is_set() or self.finished.is_set() or self.unhealthy.is_set():
                break
            time.sleep(0.2)
        return became_ready

    @staticmethod
    def _free_metrics_port():
        # Reserved per child so the watchdog can probe this connector only.
        with socket.socket() as probe:
            probe.bind(('127.0.0.1', 0))
            return probe.getsockname()[1]

    def _read(self, process):
        try:
            self._pump(process)
        finally:
            self.finished.set()

    def _watch(self, process):
        """Health probe plus sleep/wake clock-jump detection for one child."""
        failures = 0
        last_mono, last_wall = time.monotonic(), time.time()
        while not self.closed.wait(self.CHECK_INTERVAL):
            if self.process is not process or self.finished.is_set():
                return
            mono, wall = time.monotonic(), time.time()
            # time.monotonic() pauses during suspend on macOS/Linux but runs
            # through it on Windows; the wall-vs-monotonic gap catches both.
            mono_gap, wall_gap = mono - last_mono, wall - last_wall
            last_mono, last_wall = mono, wall
            if self._wake_jump(mono_gap, wall_gap):
                self.failure = 'wake'
                self.unhealthy.set()
                return
            if not self.ready.is_set():
                continue  # Never judge an unfinished handshake.
            if self._probe():
                failures = 0
                continue
            failures += 1
            if failures >= self.FAIL_STREAK:
                self.failure = 'unhealthy'
                self.unhealthy.set()
                return

    def _wake_jump(self, mono_gap, wall_gap):
        return mono_gap > self.WAKE_JUMP or wall_gap - mono_gap > self.WAKE_JUMP

    def _probe(self):
        try:
            with urlopen('http://127.0.0.1:%d/ready' % self.metrics_port, timeout=self.PROBE_TIMEOUT) as response:
                return 200 <= response.status < 300
        except (OSError, URLError):
            return False

    def _retire(self):
        with self.lifecycle_lock:
            process, reader = self.process, self.reader
            self.process = None
            self.reader = None
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        if reader and reader is not threading.current_thread():
            reader.join(timeout=2)

    def close(self):
        # Mark closed before waiting for a concurrent spawn to finish.
        self.closed.set()
        self._retire()
        self._closing()
        if self.supervisor and self.supervisor is not threading.current_thread():
            self.supervisor.join(timeout=5)


class QuickTunnel(CloudflareConnector):
    URL = re.compile(r'https://(?!api\.)[a-z0-9-]+\.trycloudflare\.com\b')
    LOST_TUNNEL = re.compile(r'Unauthorized: Tunnel not found')
    START_POLL = 0.5
    SUPERVISOR_NAME = 'quick-tunnel'

    def __init__(self, executable, port, data_dir, on_origin):
        super().__init__(data_dir)
        self.executable = Path(executable)
        self.port = port
        self.on_origin = on_origin
        self.url = None

    def status(self, state, message):
        write_json(self.data_dir/'cloudflare-status.json', {'pid': os.getpid(), 'state': state, 'message': message})

    def start(self):
        if self.closed.is_set():
            raise RuntimeError("隧道已停止")
        if not self.executable.is_file():
            raise RuntimeError("缺少 cloudflared，请查看 README 的外网访问配置")

        # A Quick Tunnel URL is created by cloudflared.  If the tunnel is
        # tombstoned after sleep/network loss, retrying the same process cannot
        # recover the old DNS name; a new process creates the replacement URL.
        self.start_supervisor()
        # Cloudflare retries Quick Tunnel creation inside the same process.
        # Keep the gateway available while that handshake is unhealthy; the
        # supervisor only replaces the child when it exits or reports a loss.
        while not self.closed.wait(self.START_POLL):
            if self.ready.is_set():
                return self.url
        raise RuntimeError("隧道已停止")

    def _arguments(self):
        config = self.data_dir / 'cloudflared.yml'
        config.write_text('{}\n', encoding='utf-8')
        config.chmod(0o600)
        return [str(self.executable), 'tunnel', '--config', str(config), '--no-autoupdate',
                '--url', 'http://127.0.0.1:' + str(self.port), '--protocol', 'http2',
                '--metrics', '127.0.0.1:' + str(self.metrics_port), '--grace-period', '2s']

    def _popen(self, arguments):
        return subprocess.Popen(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, encoding='utf-8', start_new_session=True)

    def _starting(self):
        self.url = None
        self.status('connecting', '正在建立临时 HTTPS 连接，局域网可独立使用。')

    def _lost(self):
        # The stale entry is invalidated by the reconnecting status, the
        # dropped address file and the broken/finished events; ready stays set
        # until the replacement spawn so a concurrent start() still observes it.
        self._drop_address_file()
        reason = {'wake': '检测到休眠唤醒，正在重建临时 HTTPS；网址会变化。',
                  'unhealthy': '临时 HTTPS 探测无响应，正在自动重建连接；网址会变化。'}.get(self.failure,
                  '临时 HTTPS 已失效，正在自动重建连接；网址会变化。' if self.broken.is_set()
                  else '临时 HTTPS 已中断，正在自动重建连接；网址会变化。')
        self.status('reconnecting', reason)

    def _drop_address_file(self):
        path = self.data_dir / '外网地址.txt'
        if path.exists() and self.url and path.read_text(encoding='utf-8').startswith(self.url):
            path.unlink()

    def _pump(self, process):
        with (self.data_dir / 'tunnel.log').open('a', encoding='utf-8') as log:
            for line in process.stdout:
                log.write(line)
                log.flush()
                match = self.URL.search(line)
                with self.lifecycle_lock:
                    if self.closed.is_set():
                        break
                    if match and not self.url:
                        self.url = match[0]
                        self.on_origin(self.url)
                    if 'Registered tunnel connection' in line and self.url:
                        (self.data_dir / '外网地址.txt').write_text(self.url + '\n\n账号和密码与局域网网关相同。重启隧道后地址会变化。\n', encoding='utf-8')
                        self.status('ready', '临时 HTTPS 已连接。')
                        self.ready.set()
                    if self.LOST_TUNNEL.search(line) and not self.closed.is_set():
                        self.broken.set()
                        self.ready.clear()

    def _closing(self):
        with self.lifecycle_lock:
            self._drop_address_file()
