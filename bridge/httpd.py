"""Same-origin HTTP/SSE gateway. Desktop RPC is never exposed directly."""
import gzip
import hmac
import json
import logging
import mimetypes
import re
import secrets
import socket
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http import HTTPStatus
from http.cookies import SimpleCookie
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from .uploads import MAX_FILE
from socketserver import TCPServer
from urllib.parse import parse_qs, urlsplit, quote

from .pairing import Pairing
from .auth import Auth, LoginRejected
from .ipc import IPCError
from .goal import GoalError
from .catalog import CatalogError
from .store import StoreUnavailable
from .remote import RemoteUnavailable
from .create import CreationError
from .account import AccountError
from .jev import shared_settings
from .agents.acp import AcpError

LOG = logging.getLogger(__name__)
STATIC = {"/": ("index.html", "text/html; charset=utf-8"),
          "/i18n.js": ("i18n.js", "text/javascript; charset=utf-8"),
          "/app.js": ("app.js", "text/javascript; charset=utf-8"),
          "/modes.js": ("modes.js", "text/javascript; charset=utf-8"),
          "/attachments.js": ("attachments.js", "text/javascript; charset=utf-8"),
          "/activity.js": ("activity.js", "text/javascript; charset=utf-8"),
          "/fast-mode.js": ("fast-mode.js", "text/javascript; charset=utf-8"),
          "/accounts.js": ("accounts.js", "text/javascript; charset=utf-8"),
          "/migrate.js": ("migrate.js", "text/javascript; charset=utf-8"),
          "/account.js": ("account.js", "text/javascript; charset=utf-8"),
          "/account.css": ("account.css", "text/css; charset=utf-8"),
          "/presentation.js": ("presentation.js", "text/javascript; charset=utf-8"),
          "/presentation.css": ("presentation.css", "text/css; charset=utf-8"),
          "/markdown.js": ("markdown.js", "text/javascript; charset=utf-8"),
          "/message-actions.js": ("message-actions.js", "text/javascript; charset=utf-8"),
          "/timeline.js": ("timeline.js", "text/javascript; charset=utf-8"),
          "/transport.js": ("transport.js", "text/javascript; charset=utf-8"),
          "/vendor/markdown-it.min.js": ("vendor/markdown-it.min.js", "text/javascript; charset=utf-8"),
          "/vendor/texmath.js": ("vendor/texmath.js", "text/javascript; charset=utf-8"),
          "/vendor/katex/katex.min.js": ("vendor/katex/katex.min.js", "text/javascript; charset=utf-8"),
          "/vendor/katex/katex.min.css": ("vendor/katex/katex.min.css", "text/css; charset=utf-8"),
          "/style.css": ("style.css", "text/css; charset=utf-8"),
          "/manifest.webmanifest": ("manifest.webmanifest", "application/manifest+json"),
          "/icon.png": ("icon.png", "image/png")}
THREAD_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})(?:/(events|send|stop|history|respond|reconnect|queue|catalog|settings|permissions|poll|timeline|changes|detail|notifications|uploads|message-action|rename|migrate))?$")
FONT_ROUTE = re.compile(r"^/vendor/katex/fonts/(KaTeX_[A-Za-z0-9_-]+\.(woff2|woff|ttf))$")
UPLOAD_PREVIEW_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/uploads/([0-9a-f-]{36})/preview$")
UPLOAD_THUMB_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/uploads/([0-9a-f-]{36})/thumb$")
DESKTOP_IMAGE_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/desktop-images/([0-9a-f]{64})$")
GOAL_CANCEL_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/goal/cancel$")
GOAL_EDIT_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/goal/edit$")
GOAL_STATUS_ROUTE = re.compile(r"^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/goal/status$")


class GatewayServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, bridge, config, web_dir, data_dir=None, shared=None, registry=None):
        self.local_access = config.get('localAccess', True)
        if shared is None:
            self.bridge = bridge
            self.registry = registry
            self.notifications = None
            self.instance_id = secrets.token_hex(16)
            self.auth = Auth(config["auth"], data_dir)
            self.origins = set(config["origins"])
            self.pairing = Pairing(self.auth, self.origins)
            self.hosts = {urlsplit(o).netloc for o in self.origins}
            self.secure_hosts = {urlsplit(o).netloc for o in self.origins if o.startswith("https://")}
            self.web_dir = Path(web_dir)
            self.slots = threading.BoundedSemaphore(48)
        else:
            # Listeners serve one gateway: shared sessions, pairing, limits and
            # mutable tunnel origins, with a single notification manager.
            for name in ('bridge', 'notifications', 'instance_id', 'auth', 'origins', 'pairing', 'hosts', 'secure_hosts', 'web_dir', 'slots', 'registry'):
                setattr(self, name, getattr(shared, name))
        super().__init__(address, Handler)

    def server_bind(self):
        # HTTPServer resolves the listening IP with getfqdn(), which can stall
        # startup on machines without working reverse DNS. Origins are explicit.
        TCPServer.server_bind(self)
        self.server_name, self.server_port = self.server_address[:2]

    def process_request(self, request, client_address):
        if not self.slots.acquire(blocking=False):
            request.close()
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            self.slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self.slots.release()

    def handle_error(self, request, client_address):
        LOG.warning("HTTP connection failed")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "CodexMobile/0.1"
    sys_version = ""

    def setup(self):
        super().setup()
        self.connection.settimeout(20)

    def log_message(self, format, *args):
        # Request bodies, cookies, query strings and conversation IDs are private.
        pass

    def headers_common(self, cache_control="no-store"):
        self.send_header("Cache-Control", cache_control)
        self.send_header("X-Content-Type-Options", "nosniff")
        self.send_header("Referrer-Policy", "no-referrer")
        self.send_header("X-Frame-Options", "DENY")
        # KaTeX emits inline layout styles; scripts and stylesheets stay same-origin.
        self.send_header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
        self.send_header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")

    def output(self, status, data, content_type="application/json; charset=utf-8", cookie=None, content_disposition=None):
        body = json.dumps(data, ensure_ascii=False).encode() if not isinstance(data, bytes) else data
        accepts_gzip = re.search(r'(?:^|,)\s*gzip\s*(?:;\s*q=([01](?:\.\d+)?))?\s*(?:,|$)',
                                 self.headers.get('Accept-Encoding', ''), re.IGNORECASE)
        compressible = content_type.startswith(('text/', 'application/json', 'image/svg+xml'))
        compressed = False
        if compressible and len(body) >= 1024 and accepts_gzip and float(accepts_gzip[1] or '1') > 0:
            candidate = gzip.compress(body, compresslevel=5, mtime=0)
            if len(candidate) < len(body):
                body, compressed = candidate, True
        self.send_response(status)
        self.headers_common()
        self.send_header("Content-Type", content_type)
        self.send_header('Vary', 'Accept-Encoding')
        if compressed:
            self.send_header('Content-Encoding', 'gzip')
        self.send_header("Content-Length", str(len(body)))
        if content_disposition:
            self.send_header("Content-Disposition", content_disposition)
        if cookie:
            self.send_header("Set-Cookie", cookie)
        self.end_headers()
        self.wfile.write(body)

    def cookie(self, token, clear=False):
        secure = "; Secure" if self.headers.get("Host") in self.server.secure_hosts else ""
        age = 0 if clear else self.server.auth.cookie_age(token)
        lifetime = f"; Max-Age={age}" if age is not None else ""
        return f"{Auth.COOKIE}={token}; Path=/; HttpOnly; SameSite=Strict{lifetime}{secure}"

    def client(self):
        return self.server.auth.client(self.client_address[0], self.headers, self.headers.get('Host') in self.server.secure_hosts)

    def login_session(self):
        return self.server.auth.get(self.token(), self.client(), self.headers.get('User-Agent', ''))

    def token(self):
        cookie = SimpleCookie()
        try:
            cookie.load(self.headers.get("Cookie", ""))
            return cookie[Auth.COOKIE].value if Auth.COOKIE in cookie else ""
        except Exception:
            return ""

    def check_request(self, write=False):
        if len(self.headers.get_all("Host", [])) != 1 or self.headers.get("Host") not in self.server.hosts:
            raise PermissionError("此访问地址未在网关配置中允许")
        host = self.headers.get('Host', '')
        local_host = urlsplit('http://' + host).hostname in ('127.0.0.1', 'localhost')
        if not self.server.local_access and (local_host or (self.connection.getsockname()[0] == '127.0.0.1' and host not in self.server.secure_hosts)):
            raise PermissionError('本机网页访问已关闭')
        origin = self.headers.get("Origin")
        if (origin and origin not in self.server.origins) or (write and not origin):
            raise PermissionError("不允许跨站请求")
        # External links may open the public HTML shell. Fetches, embedded pages
        # and API requests must still pass the cross-site restriction.
        homepage_navigation = (not write and self.command == 'GET' and urlsplit(self.path).path == '/'
                               and self.headers.get('Sec-Fetch-Mode') == 'navigate'
                               and self.headers.get('Sec-Fetch-Dest') == 'document')
        if self.headers.get("Sec-Fetch-Site") == "cross-site" and not homepage_navigation:
            raise PermissionError("不允许跨站请求")

    def authorized(self, write=False):
        if not self.server.auth.permitted(self.client()['ip']):
            raise PermissionError('此 IP 已被访问规则禁止')
        session = self.login_session()
        if not session:
            self.close_connection = True
            self.output(401, {"error": "请登录", "code": "unauthenticated"})
            return None
        if write and not hmac.compare_digest(self.headers.get("X-CSRF-Token", ""), session["csrf"]):
            raise PermissionError("登录验证已失效，请刷新页面")
        return session

    def read_json(self):
        if self.headers.get("Transfer-Encoding"):
            raise ValueError("不支持分块请求体")
        if self.headers.get_content_type() != "application/json":
            raise ValueError("需要 JSON 请求")
        sizes = self.headers.get_all("Content-Length", [])
        if len(sizes) != 1 or not sizes[0].isdigit():
            raise ValueError("请求长度无效")
        size = int(sizes[0])
        if not 0 < size <= 512000:
            raise ValueError("请求过大或为空")
        value = json.loads(self.rfile.read(size))
        if not isinstance(value, dict):
            raise ValueError("请求内容必须为对象")
        return value

    def do_GET(self):
        self.handle_method(False)

    def do_POST(self):
        self.handle_method(True)

    def handle_method(self, write):
        try:
            if not write and self.path == '/api/health':
                expected = {'127.0.0.1:' + str(self.server.server_port), 'localhost:' + str(self.server.server_port)}
                if (self.client_address[0] != '127.0.0.1' or self.connection.getsockname()[0] != '127.0.0.1'
                        or self.headers.get_all('Host', []) not in [[host] for host in expected]
                        or self.headers.get('Origin') or self.headers.get('Sec-Fetch-Site') == 'cross-site'):
                    raise PermissionError('仅允许本机状态检查')
                return self.output(200, {'service': 'codex-mobile-bridge', 'instanceId': self.server.instance_id,
                                         'notifications': self.server.notifications is not None})
            self.check_request(write)
            path = urlsplit(self.path).path
            query = parse_qs(urlsplit(self.path).query)
            if not write and path in STATIC:
                name, content_type = STATIC[path]
                return self.output(200, (self.server.web_dir / name).read_bytes(), content_type)
            font = FONT_ROUTE.fullmatch(path)
            if not write and font:
                file = self.server.web_dir / 'vendor/katex/fonts' / font[1]
                if file.is_file():
                    return self.output(200, file.read_bytes(), 'font/'+font[2])
            if not write and path == "/api/auth":
                session = self.login_session()
                return self.output(200, {"authenticated": bool(session), "csrf": session["csrf"] if session else None,
                                         "loginStatus": self.server.auth.login_status(self.client()["ip"]),
                                         "instanceId": self.server.instance_id,
                                         "notifications": self.server.notifications is not None,
                                         "passwordless": self.server.auth.config.get("mode") == "none",
                                         "transport": "poll" if self.headers.get("Host", "").endswith(".trycloudflare.com") else "sse"},
                                   cookie=self.cookie(self.token()) if session else None)
            if not self.server.auth.permitted(self.client()['ip']) and path != '/api/login':
                raise PermissionError('此 IP 已被访问规则禁止')
            if write and path == "/api/login":
                body = self.read_json()
                username, password = body.get("username", ""), body.get("password", "")
                if not isinstance(username, str) or not isinstance(password, str) or len(username) > 200 or len(password) > 1000:
                    raise ValueError("账号或密码格式不正确")
                token, session = self.server.auth.login(username, password, self.client()['ip'], self.headers.get('User-Agent', ''), self.client(), remember=body.get('remember'))
                self.server.auth.logout(self.token())
                return self.output(200, {"csrf": session["csrf"]}, cookie=self.cookie(token))
            if write and path == '/api/pair':
                body = self.read_json()
                host = self.headers.get('Host', '')
                origin = ('https://' if host in self.server.secure_hosts else 'http://') + host
                if self.headers.get('Origin') != origin:
                    raise PermissionError('不允许的请求来源')
                token, session = self.server.pairing.exchange(body.get('token'), origin, self.client()['ip'], self.headers.get('User-Agent', ''), self.client())
                self.server.auth.logout(self.token())
                return self.output(200, {'csrf': session['csrf']}, cookie=self.cookie(token))
            auth = self.authorized(write)
            if not auth:
                return
            if write and path == "/api/logout":
                self.read_json()
                self.server.auth.logout(self.token())
                return self.output(200, {"ok": True}, cookie=self.cookie("", clear=True))
            if not write and path == '/api/accounts':
                result = self.server.bridge.accounts.public()
                result['canSwitch'] = self.server.auth.config.get('mode') != 'none'
                return self.output(200, result)
            if write and path == '/api/accounts/details':
                body = self.read_json()
                if set(body) - {'id', 'section', 'refresh'} or not {'id', 'section'} <= set(body):
                    raise ValueError('账号详情请求包含不支持的字段')
                return self.output(202, self.server.bridge.accounts.info.request(body))
            if write and path == '/api/accounts/ignore-submission':
                body = self.read_json()
                if set(body) != {'threadId', 'submissionId', 'host'}:
                    raise ValueError('忽略请求包含不支持的字段')
                return self.output(200, self.server.bridge.accounts.ignore_submission(body))
            if write and path == '/api/accounts/switch':
                if self.server.auth.config.get('mode') == 'none':
                    raise PermissionError('免密访问不能切换账号，请在桌面端操作')
                body = self.read_json()
                if set(body) != {'id', 'requestId', 'confirmed', 'tasksConfirmed'}:
                    raise ValueError('账号切换请求包含不支持的字段')
                return self.output(202, self.server.bridge.accounts.switch(body))
            if not write and path == '/api/account':
                return self.output(200, self.server.bridge.account.read(refresh=query.get('cached', [''])[0] != '1'))
            if write and path == '/api/account/reset':
                return self.output(200, self.server.bridge.account.consume(self.read_json()))
            if not write and path == "/api/sessions":
                registry = self.server.registry
                if registry is not None and len(registry.kinds()) > 1:
                    agent = query.get("agent", [None])[0]
                    if agent is not None:
                        try:
                            selected = [registry.for_agent(agent)]
                        except KeyError:
                            raise ValueError('未知的 Agent 类型') from None
                    else:
                        selected = [registry.for_agent(kind) for kind in registry.kinds()]
                    offset = max(0, int(query.get("offset", [0])[0]))
                    archived = query.get("archived", ["false"])[0] == "true"
                    text = query.get("q", [""])[0][:200]
                    rows, host_errors = [], []
                    # 各 Agent 的列表可能涉及进程发现与握手，并行拉取避免串行叠加延迟。
                    with ThreadPoolExecutor(max_workers=len(selected)) as pool:
                        groups = list(pool.map(
                            lambda item: item.list(query=text, limit=100 + offset, offset=0, archived=archived),
                            selected))
                    for item, group in zip(selected, groups):
                        rows.extend(group)
                        host_errors.extend(item.host_errors)
                    rows.sort(key=lambda row: (row['recency'], row['id'], row['host']), reverse=True)
                    return self.output(200, {"sessions": rows[offset:offset + 100], "unavailableHosts": host_errors})
                rows = self.server.bridge.list(query=query.get("q", [""])[0][:200], offset=max(0, int(query.get("offset", [0])[0])), archived=query.get("archived", ["false"])[0] == "true")
                return self.output(200, {"sessions": rows, "unavailableHosts": self.server.bridge.host_errors})
            if not write and path == '/api/projects':
                registry = self.server.registry
                if registry is not None:
                    projects, seen = [], set()
                    for kind in registry.kinds():
                        for project in registry.for_agent(kind).adapter.projects():
                            if project['key'] not in seen:
                                seen.add(project['key'])
                                projects.append({**project, 'agent': kind})
                    return self.output(200, {'projects': projects,
                                             'agents': [{'kind': kind,
                                                         'label': registry.for_agent(kind).adapter.label}
                                                        for kind in registry.kinds()]})
                return self.output(200, {'projects': self.server.bridge.hosts.projects()})
            if write and path == '/api/activity':
                groups = self.read_json().get('hosts')
                if not isinstance(groups, dict) or len(groups) > 20:
                    raise ValueError('会话状态请求无效')
                rows = []
                for host, identifiers in groups.items():
                    rows.extend(self.server.bridge.for_host(host).activity(identifiers))
                return self.output(200, {'sessions': rows})
            if write and path == '/api/sessions':
                body = self.read_json()
                worktree = body.get('worktree')
                if worktree is not None and (not isinstance(worktree, dict) or set(worktree) != {'base'}
                                             or worktree['base'] not in ('fresh', 'head')):
                    raise ValueError('worktree 参数无效')
                root = self.server.bridge
                registry = self.server.registry
                if registry is not None and len(registry.kinds()) > 1:
                    agent = body.get('agent')
                    project = None
                    if agent is not None:
                        try:
                            root = registry.for_agent(agent)
                        except KeyError:
                            raise ValueError('未知的 Agent 类型') from None
                        # An explicit agent pairs with any known project
                        # directory; the target's own projects win.
                        project = next((p for p in root.adapter.projects()
                                        if p['key'] == body.get('project')), None)
                    if project is None:
                        for kind in registry.kinds():
                            candidate = registry.for_agent(kind)
                            project = next((p for p in candidate.adapter.projects()
                                            if p['key'] == body.get('project')), None)
                            if project is not None:
                                if agent is None:
                                    root = candidate
                                elif project['host'] != 'local':
                                    raise ValueError('跨 Agent 创建仅支持本机项目目录')
                                break
                    if project is None:
                        raise ValueError('请选择电脑 App 中已保存的项目')
                    if worktree is None:
                        return self.output(200, root.create_chat(project['key'], body.get('title'),
                                                                 body.get('id'), resolved=project))
                    return self.output(200, root.create_chat(project['key'], body.get('title'),
                                                             body.get('id'), resolved=project, worktree=worktree))
                if worktree is None:
                    return self.output(200, root.create_chat(body.get('project'), body.get('title'), body.get('id')))
                return self.output(200, root.create_chat(body.get('project'), body.get('title'), body.get('id'),
                                                         worktree=worktree))
            if path == '/api/notifications/defaults':
                if self.server.notifications is None:
                    raise ValueError('当前网关未启用通知服务')
                return self.output(200, self.server.notifications.defaults(self.read_json() if write else None))
            if path == '/api/jev':
                store = shared_settings()
                if store is None:
                    raise ValueError('Jev 设置存储未初始化')
                return self.output(200, store.update(self.read_json()) if write else store.public())
            root = self.server.bridge
            registry = self.server.registry
            if registry is None:
                agent = query.get("agent", [None])[0]
                if agent is not None and agent != 'codex':
                    raise ValueError('未知的 Agent 类型')
            else:
                agent = query.get("agent", [None])[0]
                if agent is not None:
                    try:
                        root = registry.for_agent(agent)
                    except KeyError:
                        raise ValueError('未知的 Agent 类型') from None
                else:
                    session_match = re.match(r"^/api/sessions/([^/]+)", path)
                    root = (session_match and registry.bridge_for(session_match.group(1))) or registry.default()
            bridge = root.for_host(query.get("host", ["local"])[0])
            file_match = re.fullmatch(r"/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/files/([a-f0-9]{64})", path)
            if not write and file_match:
                return self.download(bridge, *file_match.groups())
            preview_match = UPLOAD_PREVIEW_ROUTE.fullmatch(path)
            if not write and preview_match:
                return self.upload_preview(bridge, *preview_match.groups(), variant=query.get('variant', ['thumb'])[0])
            desktop_image_match = DESKTOP_IMAGE_ROUTE.fullmatch(path)
            if not write and desktop_image_match:
                return self.desktop_image_preview(bridge, *desktop_image_match.groups())
            thumb_match = UPLOAD_THUMB_ROUTE.fullmatch(path)
            if write and thumb_match:
                sizes = self.headers.get_all('Content-Length', [])
                if self.headers.get('Transfer-Encoding') or len(sizes) != 1 or not sizes[0].isdigit() or not 0 < int(sizes[0]) <= 1024 * 1024:
                    raise ValueError('缩略图需为 1 字节至 1 MB')
                data = self.rfile.read(int(sizes[0]))
                if len(data) != int(sizes[0]):
                    raise ValueError('缩略图上传中断，请重试')
                try:
                    width, height = int(query.get('width', ['0'])[0]), int(query.get('height', ['0'])[0])
                except ValueError:
                    raise ValueError('缩略图尺寸无效') from None
                return self.output(200, bridge.upload_thumb(thumb_match[1], thumb_match[2], data, width, height))
            goal_match = GOAL_CANCEL_ROUTE.fullmatch(path)
            if write and goal_match:
                body = self.read_json()
                return self.output(200, bridge.cancel_goal(goal_match[1], body.get("id", ""), expected=body.get("expected")))
            goal_edit_match = GOAL_EDIT_ROUTE.fullmatch(path)
            if write and goal_edit_match:
                body = self.read_json()
                return self.output(200, bridge.edit_goal(goal_edit_match[1], body.get('objective'),
                                                        body.get('id', ''), expected=body.get('expected')))
            goal_status_match = GOAL_STATUS_ROUTE.fullmatch(path)
            if write and goal_status_match:
                body = self.read_json()
                return self.output(200, bridge.set_goal_status(goal_status_match[1], body.get("status"),
                                                              body.get("id", ""), body.get("uiLocale"), expected=body.get("expected")))
            match = THREAD_ROUTE.fullmatch(path)
            if not match:
                return self.output(404, {"error": "页面不存在"})
            thread_id, action = match.groups()
            if write and action == 'uploads':
                sizes = self.headers.get_all('Content-Length', [])
                if self.headers.get('Transfer-Encoding') or len(sizes) != 1 or not sizes[0].isdigit() or not 0 < int(sizes[0]) <= MAX_FILE:
                    raise ValueError('每个附件需为 1 字节至 20 MB')
                size = int(sizes[0])
                data = self.rfile.read(size)
                if len(data) != size:
                    raise ValueError('附件上传中断，请重试')
                return self.output(200, bridge.upload(thread_id, query.get('id', [''])[0], query.get('name', [''])[0], data))
            if action == 'notifications':
                if self.server.notifications is None:
                    return self.output(200, {'available': False, 'watching': False, 'notifyOnCompletion': False})
                body = self.read_json() if write else {}
                return self.output(200, self.server.notifications.policy(thread_id, bridge.host, body if write else None))
            if not write:
                if action == 'timeline':
                    return self.output(200, self.with_timeline_agents(
                        bridge.timeline_read(thread_id, limit=int(query.get('limit', ['20'])[0]), before=query.get('before', [None])[0])))
                if action == 'detail':
                    return self.output(200, bridge.timeline_read(thread_id, 'detail', cursor=query.get('key', [''])[0],
                                                               offset=int(query.get('offset', ['0'])[0]), version=query.get('version', [None])[0]))
                if action == 'changes':
                    return self.changes(bridge, thread_id, int(query.get('after', ['-1'])[0]),
                                        query.get('epoch', [''])[0], query.get('start', [''])[0])
                if action is None:
                    result = bridge.view(thread_id, background=True)
                    agents = self.enabled_agents()
                    if agents is not None:
                        result['agents'] = agents
                    return self.output(200, result)
                if action == "catalog":
                    kind = query.get("kind", [None])[0]
                    try:
                        offset = max(0, int(query.get("offset", ["0"])[0]))
                        limit = min(500, max(1, int(query.get("limit", ["200"])[0])))
                    except ValueError:
                        raise ValueError("Skill 分页参数无效") from None
                    requested_ids = [value for value in query.get("id", []) if value]
                    if len(requested_ids) > 8:
                        raise ValueError("最多关联 8 个已选 Skill")
                    ids = list(dict.fromkeys(requested_ids))
                    return self.output(200, bridge.catalog(
                        thread_id, refresh=query.get("refresh") == ["true"], kind=kind,
                        query=query.get("q", [""])[0][:200], offset=offset, limit=limit, ids=ids))
                if action == 'permissions':
                    return self.output(200, bridge.permission_settings(thread_id))
                if action == "poll":
                    return self.poll(bridge, thread_id, int(query.get("after", ["-1"])[0]))
                if action == "events":
                    return self.stream(bridge, thread_id, query)
                return self.output(404, {"error": "接口不存在"})
            body = self.read_json()
            if action == "message-action":
                result = bridge.message_action(thread_id, body)
            elif action == "send":
                result = bridge.send(thread_id, body.get("text"), body.get("id", ""), body.get("mode", "send"),
                                     body.get("skills", []), work_mode=body.get("workMode"),
                                     attachments=body.get('attachments'), ui_locale=body.get("uiLocale"))
            elif action == "rename":
                result = bridge.rename(thread_id, body.get("title"))
            elif action == "migrate":
                registry = self.server.registry
                target_kind = body.get('agent')
                if registry is None:
                    if target_kind not in (None, 'codex'):
                        raise ValueError('未知的 Agent 类型')
                    target = self.server.bridge
                else:
                    try:
                        target = registry.for_agent(target_kind)
                    except KeyError:
                        raise ValueError('未知的 Agent 类型') from None
                result = bridge.migrate(thread_id, target, body.get('project'), body.get('id', ''),
                                        body.get('uiLocale'), scope=body.get('scope'))
            elif action == "settings":
                if 'fastMode' in body and not isinstance(body['fastMode'], bool):
                    raise ValueError('Fast 模式开关无效')
                result = bridge.settings(thread_id, body.get("model"), body.get("effort"), **({'fast_mode': body['fastMode']} if 'fastMode' in body else {}))
            elif action == 'permissions':
                result = bridge.set_permission_mode(thread_id, body.get('mode'))
            elif action == "stop":
                result = bridge.interrupt(thread_id)
            elif action == "history":
                result = bridge.full_history(thread_id)
            elif action == "respond":
                response = body.get("response")
                if not isinstance(response, dict):
                    raise ValueError("请求回应格式不正确")
                result = bridge.respond(thread_id, body.get("requestId"), response)
            elif action == "reconnect":
                if body.get('activate') is True:
                    session = bridge.activate(thread_id)
                    result = {'ok': True, 'connected': session.connected}
                elif body.get('progressive'):
                    bridge.session(thread_id, background=True, force=True)
                    result = {'ok': True}
                else:
                    result = bridge.view(thread_id, background=True, force=True)
            elif action == "queue":
                result = bridge.queue_action(thread_id, body.get("id", ""), body.get("action", ""), body.get("text"))
            else:
                return self.output(404, {"error": "接口不存在"})
            self.output(200, result)
        except LoginRejected as exc:
            self.close_connection = True
            self.output(403, {"error": str(exc), "loginStatus": exc.status})
        except PermissionError as exc:
            self.close_connection = True
            self.output(403, {"error": str(exc)})
        except KeyError:
            self.close_connection = True
            self.output(404, {"error": "找不到这个会话"})
        except ValueError as exc:
            self.close_connection = True
            self.output(400, {"error": str(exc)})
        except (IPCError, GoalError, CatalogError, RemoteUnavailable, CreationError, StoreUnavailable, AccountError, AcpError) as exc:
            self.close_connection = True
            self.output(409, {"error": str(exc), "code": "desktop_unavailable"})
        except (BrokenPipeError, ConnectionResetError, socket.timeout):
            self.close_connection = True
        except Exception:
            LOG.exception("Gateway operation failed")
            self.close_connection = True
            self.output(500, {"error": "网关操作失败，请查看本机日志"})

    def download(self, bridge, thread_id, artifact_id):
        artifact = bridge.artifact(thread_id, artifact_id)
        data = artifact["path"].read_bytes()
        self.send_response(200)
        self.headers_common()
        mime = mimetypes.guess_type(artifact["name"])[0] if artifact["image"] else "application/octet-stream"
        self.send_header("Content-Type", mime or "application/octet-stream")
        self.send_header("Content-Length", str(len(data)))
        disposition = "inline" if artifact["image"] else "attachment"
        self.send_header("Content-Disposition", disposition + "; filename*=UTF-8''" + quote(artifact["name"], safe=""))
        self.end_headers()
        self.wfile.write(data)

    def upload_preview(self, bridge, thread_id, upload_id, variant='thumb'):
        artifact, served_variant = bridge.upload_preview(thread_id, upload_id, variant)
        data = Path(artifact["previewPath"]).read_bytes()
        etag = '"' + artifact["previewSha256"] + '"'
        cache_control = 'private, no-store' if served_variant == 'fallback-original' else 'private, max-age=31536000, immutable'
        if self.headers.get("If-None-Match") == etag:
            self.send_response(304)
            self.headers_common(cache_control)
            self.send_header("ETag", etag)
            self.send_header("X-Preview-Variant", served_variant)
            self.end_headers()
            return
        self.send_response(200)
        self.headers_common(cache_control)
        self.send_header("Content-Type", artifact["previewMime"])
        self.send_header("Content-Length", str(len(data)))
        self.send_header("ETag", etag)
        self.send_header("X-Preview-Variant", served_variant)
        self.send_header("Content-Disposition", "inline; filename*=UTF-8''" + quote(artifact["name"], safe=""))
        self.end_headers()
        self.wfile.write(data)

    def desktop_image_preview(self, bridge, thread_id, image_id):
        artifact, served_variant = bridge.desktop_image_preview(thread_id, image_id)
        data = Path(artifact["previewPath"]).read_bytes()
        etag = '"' + artifact["previewSha256"] + '"'
        if self.headers.get("If-None-Match") == etag:
            self.send_response(304)
            self.headers_common("private, max-age=31536000, immutable")
            self.send_header("ETag", etag)
            self.send_header("X-Preview-Variant", served_variant)
            self.end_headers()
            return
        self.send_response(200)
        self.headers_common("private, max-age=31536000, immutable")
        self.send_header("Content-Type", artifact["previewMime"])
        self.send_header("Content-Length", str(len(data)))
        self.send_header("ETag", etag)
        self.send_header("X-Preview-Variant", served_variant)
        self.send_header("Content-Disposition", "inline; filename*=UTF-8''" + quote(artifact["name"], safe=""))
        self.end_headers()
        self.wfile.write(data)

    def poll(self, bridge, thread_id, after):
        session = bridge.session(thread_id, background=True)
        with session.condition:
            session.viewers += 1
        try:
            with session.condition:
                session.condition.wait_for(lambda: session.sequence != after or bridge.closed.is_set(), timeout=12)
                changed = session.sequence != after
            if not self.authorized():
                return
            self.output(200, {"state": bridge.view(thread_id, attach=False, background=True) if changed else None})
        finally:
            with session.condition:
                session.viewers -= 1
                session.touched = time.monotonic()

    def enabled_agents(self):
        """Enabled agent descriptors, or None when running without a registry."""
        registry = self.server.registry
        if registry is None:
            return None
        return [{'kind': kind, 'label': registry.for_agent(kind).adapter.label}
                for kind in registry.kinds()]

    def with_timeline_agents(self, result):
        """Expose the agent roster in timeline meta so live views keep it.

        The chat screen renders exclusively from timeline pages and change
        polls; without this the migrate control would lose its target list on
        every refresh after the initial load.
        """
        agents = self.enabled_agents()
        if agents is not None and 'meta' in result:
            result['meta']['agents'] = agents
        return result

    def changes(self, bridge, thread_id, after, epoch, start):
        session = bridge.session(thread_id, background=True)
        with session.condition:
            session.viewers += 1
        try:
            with session.condition:
                session.condition.wait_for(lambda: session.sequence != after or epoch != session.timeline.epoch or bridge.closed.is_set(), timeout=12)
            if self.authorized():
                self.output(200, self.with_timeline_agents(
                    bridge.timeline_read(thread_id, 'changes', after=after, epoch=epoch, start=start)))
        finally:
            with session.condition:
                session.viewers -= 1
                session.touched = time.monotonic()

    def sse_frame(self, event, payload, cursor=None):
        data = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
        head = f"id: {cursor}\n" if cursor else ""
        self.wfile.write(f"{head}event: {event}\ndata: {data}\n\n".encode())
        self.wfile.flush()

    def stream(self, bridge, thread_id, query=None):
        query = query or {}
        incremental = query.get('incremental', [''])[0] == '1'
        epoch, sequence = '', -1
        if incremental:
            # EventSource replays the last frame id on its own reconnects;
            # ?since= covers clients that reconnect manually.
            since = self.headers.get('Last-Event-ID') or query.get('since', [''])[0]
            head, _, tail = since.partition('.')
            if head and tail.isdigit():
                epoch, sequence = head, int(tail)
        session = bridge.session(thread_id, background=True)
        with session.condition:
            session.viewers += 1
        self.send_response(200)
        self.headers_common()
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("X-Accel-Buffering", "no")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True
        try:
            if incremental:
                self.stream_incremental(bridge, thread_id, session, epoch, sequence)
                return
            sequence = -1
            while not bridge.closed.is_set():
                if not self.login_session():
                    self.wfile.write(b'event: logout\ndata: {}\n\n')
                    self.wfile.flush()
                    break
                with session.condition:
                    session.condition.wait_for(lambda: session.sequence != sequence or bridge.closed.is_set(), timeout=12)
                    updated = session.sequence != sequence
                    sequence = session.sequence
                if not self.login_session():
                    self.wfile.write(b'event: logout\ndata: {}\n\n')
                    self.wfile.flush()
                    break
                if updated:
                    view = bridge.view(thread_id, attach=False, background=True)
                    payload = json.dumps(view, ensure_ascii=False, separators=(",", ":"))
                    self.wfile.write(f"id: {sequence}\nevent: state\ndata: {payload}\n\n".encode())
                else:
                    self.wfile.write(b": heartbeat\n\n")
                self.wfile.flush()
                bridge.closed.wait(0.2)
        except (BrokenPipeError, ConnectionResetError, socket.timeout):
            pass
        finally:
            with session.condition:
                session.viewers -= 1
                session.touched = time.monotonic()

    def stream_incremental(self, bridge, thread_id, session, epoch, sequence):
        """Snapshot/incremental frames behind an explicit ready gate.

        The timeline versions window is the replay buffer; changes() already
        falls back to a reset page whenever a cursor is unusable, so this loop
        only maps that result onto frame types. ready carries no id: a resumed
        connection must never skip past an unsent snapshot.
        """
        with session.condition:
            welcome = {'epoch': session.timeline.epoch, 'sequence': session.sequence}
        self.sse_frame('ready', welcome)
        while not bridge.closed.is_set():
            if not self.login_session():
                self.sse_frame('logout', {})
                return
            with session.condition:
                session.condition.wait_for(
                    lambda: session.sequence != sequence or session.timeline.epoch != epoch or bridge.closed.is_set(),
                    timeout=12)
                changed = session.sequence != sequence or session.timeline.epoch != epoch
            if not self.login_session():
                self.sse_frame('logout', {})
                return
            if changed:
                payload = self.with_timeline_agents(
                    bridge.timeline_read(thread_id, 'changes', after=sequence, epoch=epoch, start=''))
                epoch, sequence = payload['epoch'], payload['sequence']
                self.sse_frame('reset' if payload.get('reset') else 'changes', payload,
                               cursor=f"{epoch}.{sequence}")
            else:
                self.wfile.write(b": heartbeat\n\n")
                self.wfile.flush()
            bridge.closed.wait(0.2)
