"""ACP (Agent Client Protocol) stdio transport: NDJSON JSON-RPC 2.0 over pipes."""
import json
import logging
import os
import shutil
import subprocess
import threading
from concurrent.futures import Future, TimeoutError as FutureTimeout
from pathlib import Path

LOG = logging.getLogger(__name__)

# One NDJSON frame may never exceed 64MB; a larger line breaks the framing.
MAX_LINE = 64 * 1024 * 1024
AUTH_REQUIRED = -32000


class AcpError(Exception):
    """One ACP failure. JSON-RPC error objects keep their code and data."""

    def __init__(self, message, *, code=None, data=None):
        super().__init__(message)
        self.code = code
        self.data = data


class AcpUnavailable(AcpError):
    """The process cannot serve the request; the delivery outcome is unknown."""


class AcpTimeout(AcpError):
    """The agent did not answer in time."""


class AcpProtocol(AcpError):
    """The agent violated the NDJSON framing."""


class AcpAuthRequired(AcpError):
    """The agent requires authentication before serving requests."""


def discover_executable(env_var, default_rel, name):
    """Resolve an agent binary: env override, then ~/<default_rel>/<name>, then PATH."""
    candidate = os.environ.get(env_var)
    if candidate:
        return candidate
    local = Path.home() / default_rel / name
    if local.is_file():
        return str(local)
    return shutil.which(name)


class AcpProcess:
    """One ACP agent subprocess with serialized writes and paired futures."""

    def __init__(self, argv, *, cwd=None, env=None, on_notification=None, on_request=None,
                 on_exit=None, spawn=None):
        self.argv = list(argv)
        self._on_notification = on_notification
        self._on_request = on_request
        self._on_exit = on_exit
        self._lock = threading.Lock()
        self._write_lock = threading.Lock()
        self._pending = {}
        self._next_id = 0
        self._closed = False
        self._exited = False
        self.capabilities = {}
        self.agent_info = {}
        self.auth_methods = []
        factory = spawn or subprocess.Popen
        try:
            self._proc = factory(self.argv, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                 stderr=subprocess.PIPE, cwd=cwd, env=env,
                                 text=True, encoding='utf-8', bufsize=1)
        except (OSError, ValueError) as exc:
            raise AcpUnavailable(f'无法启动 Agent 进程：{exc}') from exc
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._drain = threading.Thread(target=self._drain_stderr, daemon=True)
        self._reader.start()
        self._drain.start()

    def _enqueue(self, method, params):
        """Register one client request and write it; returns (id, future)."""
        with self._lock:
            if self._closed or self._exited:
                raise AcpUnavailable('Agent 进程已退出；操作结果未知')
            ident = self._next_id
            self._next_id += 1
            future = Future()
            self._pending[ident] = future
        try:
            self._send({'jsonrpc': '2.0', 'id': ident, 'method': method, 'params': params})
        except AcpUnavailable:
            with self._lock:
                self._pending.pop(ident, None)
            raise
        return ident, future

    def submit(self, method, params):
        """Send one client request without waiting; the caller awaits the future."""
        return self._enqueue(method, params)

    def request(self, method, params, timeout=30):
        """Send one client request and wait for its result."""
        ident, future = self._enqueue(method, params)
        try:
            return future.result(timeout)
        except FutureTimeout:
            with self._lock:
                self._pending.pop(ident, None)
            raise AcpTimeout(f'Agent 请求超时：{method}') from None

    def initialize(self, timeout=10):
        """Perform the ACP handshake and capture the advertised agent metadata."""
        result = self.request('initialize', {
            'protocolVersion': 1,
            'clientCapabilities': {'fs': {'readTextFile': False, 'writeTextFile': False},
                                   'terminal': False},
        }, timeout=timeout) or {}
        self.capabilities = result.get('agentCapabilities') or {}
        self.agent_info = result.get('agentInfo') or {}
        self.auth_methods = result.get('authMethods') or []
        return result

    def notify(self, method, params):
        """Send one client notification (no id, no response expected)."""
        self._send({'jsonrpc': '2.0', 'method': method, 'params': params})

    def respond(self, request_id, result):
        """Answer one reverse request from the agent."""
        self._send({'jsonrpc': '2.0', 'id': request_id, 'result': result})

    def respond_error(self, request_id, code, message):
        """Reject one reverse request from the agent."""
        self._send({'jsonrpc': '2.0', 'id': request_id,
                    'error': {'code': code, 'message': message}})

    def close(self):
        """Idempotent shutdown: stdin EOF, terminate, wait 3s, then kill."""
        with self._lock:
            if self._closed:
                return
            self._closed = True
        try:
            self._proc.stdin.close()
        except Exception:
            pass
        try:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self._proc.kill()
                try:
                    self._proc.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    pass
        except Exception:
            pass
        self._finish_exit()
        self._reader.join(timeout=5)
        self._drain.join(timeout=5)
        for stream in (self._proc.stdout, self._proc.stderr):
            try:
                stream.close()
            except Exception:
                pass

    def _send(self, message):
        data = json.dumps(message, ensure_ascii=False) + '\n'
        with self._write_lock:
            if self._closed:
                raise AcpUnavailable('Agent 进程已退出；操作结果未知')
            try:
                self._proc.stdin.write(data)
                self._proc.stdin.flush()
            except (OSError, ValueError) as exc:
                # A failed write may or may not have reached the agent.
                raise AcpUnavailable('Agent 进程已退出；操作结果未知') from exc

    def _read_loop(self):
        failure = None
        try:
            while True:
                line = self._proc.stdout.readline(MAX_LINE + 1)
                if line == '':
                    break
                if len(line) > MAX_LINE and not line.endswith('\n'):
                    failure = AcpProtocol('Agent 消息超过 64MB')
                    break
                line = line.strip()
                if not line:
                    continue
                try:
                    message = json.loads(line)
                except ValueError:
                    # Real agents can print banners to stdout; never let one
                    # non-JSON line kill the protocol stream.
                    LOG.warning('Dropping non-JSON line from agent: %.200s', line)
                    continue
                try:
                    self._dispatch(message)
                except Exception:
                    LOG.exception('Agent message handler failed')
        except (OSError, ValueError) as exc:
            LOG.debug('Agent stdout read failed: %s', exc)
        finally:
            if failure is not None:
                self._fail_all(failure)
                try:
                    self._proc.terminate()
                except Exception:
                    pass
            self._finish_exit()

    def _drain_stderr(self):
        try:
            for line in self._proc.stderr:
                LOG.debug('agent stderr: %s', line.rstrip())
        except (OSError, ValueError):
            pass

    def _dispatch(self, message):
        if not isinstance(message, dict):
            LOG.warning('Dropping malformed agent message: %r', message)
            return
        if 'method' in message and 'id' in message:
            self._reverse(message)
            return
        if 'id' in message:
            with self._lock:
                future = self._pending.pop(message['id'], None)
            if future is None:
                LOG.warning('Dropping response with unknown id: %r', message.get('id'))
                return
            error = message.get('error')
            if error is not None:
                text = (error.get('message') if isinstance(error, dict) else None) or 'Agent 请求失败'
                code = error.get('code') if isinstance(error, dict) else None
                data = error.get('data') if isinstance(error, dict) else None
                kind = AcpAuthRequired if code == AUTH_REQUIRED else AcpError
                future.set_exception(kind(text, code=code, data=data))
            else:
                future.set_result(message.get('result'))
            return
        if 'method' in message and self._on_notification is not None:
            self._on_notification(message)

    def _reverse(self, message):
        ident = message['id']
        if self._on_request is None:
            # An unanswered reverse request blocks the agent forever.
            try:
                self.respond_error(ident, -32601, 'Method not found')
            except AcpUnavailable:
                pass
            return
        self._on_request(message, lambda result: self.respond(ident, result))

    def _fail_all(self, exc):
        with self._lock:
            pending = list(self._pending.values())
            self._pending.clear()
        for future in pending:
            future.set_exception(exc)

    def _finish_exit(self):
        with self._lock:
            if self._exited:
                return
            self._exited = True
        try:
            returncode = self._proc.poll()
        except Exception:
            returncode = None
        self._fail_all(AcpUnavailable('Agent 进程已退出；操作结果未知'))
        if self._on_exit is not None:
            try:
                self._on_exit(returncode)
            except Exception:
                LOG.exception('Agent exit handler failed')
