"""Remotely managed Cloudflare connector, with token kept out of argv and logs."""
import os
import subprocess

from .tunnel import CloudflareConnector
from .notifications import write_json


class NamedTunnel(CloudflareConnector):
    SUPERVISOR_NAME = 'named-tunnel'

    def __init__(self, executable, entry, data_dir, token):
        super().__init__(data_dir)
        self.executable, self.entry, self.token = str(executable), entry, token
        # Legacy stop signal kept for callers that predate the shared base.
        self.stopped = self.closed

    def status(self, state, message):
        write_json(self.data_dir/('ssh-status-'+self.entry['id']+'.json'),
                   {'pid': os.getpid(), 'state': state, 'message': message})

    def start(self):
        self.start_supervisor()

    def _arguments(self):
        # Ignore ambient cloudflared config and credentials. Token is not a command argument.
        config = self.data_dir/('cloudflare-'+self.entry['id']+'.yml')
        config.write_text('{}\n', encoding='utf-8')
        config.chmod(0o600)
        return [self.executable, 'tunnel', '--config', str(config), '--no-autoupdate',
                '--protocol', 'http2', '--metrics', '127.0.0.1:' + str(self.metrics_port), 'run']

    def _popen(self, arguments):
        environment = {key: value for key, value in os.environ.items() if not key.startswith('TUNNEL_')}
        environment['TUNNEL_TOKEN'] = self.token
        flags = {'creationflags': subprocess.CREATE_NO_WINDOW} if os.name == 'nt' else {'start_new_session': True}
        return subprocess.Popen(arguments, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                stderr=subprocess.STDOUT, text=True, encoding='utf-8', env=environment, **flags)

    def _starting(self):
        self.status('connecting', '正在连接 Cloudflare 固定隧道…')

    def _lost(self):
        # The fixed hostname stays valid; the retrying status drops it from
        # the published entries until the connector registers again.
        reason = {'wake': '检测到休眠唤醒，正在重建固定隧道。',
                  'unhealthy': '固定隧道探测无响应，正在自动重连。'}.get(self.failure,
                  '固定隧道已断开，正在自动重连。')
        self.status('retrying', reason)

    def _pump(self, process):
        for line in process.stdout:
            # Do not persist raw provider output: it can contain credentials or remote config.
            if 'Registered tunnel connection' in line:
                self.status('connected', '固定隧道已连接；请检测域名是否指向当前网关。')
                self.ready.set()
            elif 'ERR' in line or 'Unregistered tunnel connection' in line:
                self.status('retrying', '固定隧道连接异常，请检查 Token、公开主机名和网络。')

    def _stopped(self):
        self.status('stopped', '固定隧道已停止')
