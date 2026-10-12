"""Jev risk triage for manual permission confirmations.

Jev (TypeSafe AI) answers typed questions only; it never generates text and
never grants permissions. The assessment is advisory: risk ranking, option
recommendations, and confidence that the phone renders next to the native
approval buttons.

Configuration is web-editable and process-wide: `JevSettings` persists under
the gateway root (jev.json, 0600). Launch-time env vars (TYPESAFE_API_KEY,
TYPESAFE_BASE_URL, JEV_MODEL) stay as defaults; saving a value in the file
overrides them and clearing the key disables Jev even when the env var is
still set. Without a key the module stays disabled and the phone keeps the
deterministic local labels.
"""
import hashlib
import json
import logging
import os
import re
import threading
import http.client
from pathlib import Path
from urllib.parse import urlsplit

from .notifications import write_json
from .tls import client_context

LOG = logging.getLogger(__name__)

DEFAULT_BASE_URL = 'https://api.typesafe.ai'
# Confirmations wait for a human anyway; a slow answer must not stall a view.
TIMEOUT = 4.0
MAX_ASSESSMENTS = 256

RISK_LABELS = ('routine', 'low', 'elevated', 'high')

# Requests that ask the user to confirm an operation. Other methods (user
# input, plan review) are conversations, not permissions.
ASSESSABLE_METHODS = {
    'acp/requestPermission',
    'item/commandExecution/requestApproval',
    'item/fileChange/requestApproval',
    'item/permissions/requestApproval',
}

RISK_RUBRIC = [
    '0 - Routine. Read-only or observation-only: listing files, searching, '
    'reading configuration, showing a diff that is not applied.',
    '1 - Low. Routine reversible work inside the current project: editing '
    'workspace files, running tests or builds, installing project-local '
    'dependencies.',
    '2 - Elevated. Effects reach beyond the workspace or are hard to undo: '
    'network calls to unfamiliar hosts, changing permissions or ownership, '
    'pushing or rewriting shared git history, editing user dotfiles or app '
    'settings, using credentials.',
    '3 - High. Destructive or system-level: deleting data broadly, sudo or '
    'administrator actions, force operations, credential or security changes, '
    'anything that could make the machine or remote service unusable.',
]

RECOMMEND_OPTIONS = {
    'accept_once': 'Allow this one operation only. Prefer when the action is '
                   'reasonable but the pattern is not established.',
    'accept_session': 'Allow for this session when the same action will '
                      'clearly repeat and stays reversible.',
    'accept_always': 'Allow permanently. Only when the action is routine and '
                     'low risk for any future context.',
    'decline': 'Refuse the operation. The command or change is unsafe, '
               'unexplained, or unrelated to the requested work.',
    'review': 'No safe automatic recommendation; the user must read the '
              'details before choosing.',
}


def assessable(request):
    """True for supported requests that ask the user to confirm an operation."""
    if not isinstance(request, dict) or not request.get('supported'):
        return False
    method = request.get('method')
    if method in ASSESSABLE_METHODS:
        return True
    # Computer-use elicitation cards are approvals of app control.
    return method == 'mcpServer/elicitation/request' and bool(
        isinstance(request.get('params'), dict) and request['params'].get('computerUse'))


def fingerprint(request):
    """Stable identity for one approval card across sequence bumps."""
    params = request.get('params') or {}
    payload = [request.get('method'),
               params.get('title'), params.get('command'), params.get('cwd'),
               params.get('reason'), params.get('permissions'),
               params.get('grantRoot'), params.get('changes'),
               params.get('networkApprovalContext'), params.get('computerUse'),
               [(o.get('id'), o.get('kind')) for o in params.get('options') or []
                if isinstance(o, dict)]]
    return hashlib.sha256(json.dumps(payload, ensure_ascii=False,
                                     sort_keys=True).encode()).hexdigest()[:32]


def assessment_state(request, context):
    """Compact, PII-light description of what the user is asked to allow."""
    params = request.get('params') or {}
    state = {'request_kind': request.get('method'), **context}
    for key in ('title', 'command', 'cwd', 'reason', 'permissions', 'networkApprovalContext'):
        value = params.get(key)
        if value:
            state[key] = value if not isinstance(value, str) else value[:4000]
    computer = params.get('computerUse')
    if computer:
        state['computerUse'] = computer
    changes = params.get('changes')
    if isinstance(changes, list):
        state['changes'] = [c.get('path') if isinstance(c, dict) else str(c)[:200]
                            for c in changes[:50]]
    options = [o for o in params.get('options') or [] if isinstance(o, dict)]
    if options:
        state['options'] = [{'kind': o.get('kind'), 'label': o.get('label') or o.get('id')}
                            for o in options]
    return state


def build_payload(request, context):
    """One Jev request: risk score, reversibility, recommended decision."""
    available = _available_decisions(request)
    # 'review' always stays selectable so Jev can punt instead of guessing.
    criteria = {key: text for key, text in RECOMMEND_OPTIONS.items()
                if key in available or key == 'review'}
    return {
        'model': 'jev-latest',
        'state': assessment_state(request, context),
        'questions': {
            'risk': {
                'type': 'score',
                'instructions': 'Rate the risk of allowing the operation described by this permission request.',
                'criteria': RISK_RUBRIC,
            },
            'reversible': {
                'type': 'noul',
                'instructions': 'If allowed, can the effects of this operation be undone cheaply and completely?',
                'criteria': {
                    'true': 'The effects stay local and reversible: edited files can be restored, '
                            'temporary state can be cleaned up, nothing external is modified.',
                    'false': 'The effects are destructive, external, or persistent: deleted data, '
                             'published or pushed state, changed permissions, remote side effects.',
                },
            },
            'recommend': {
                'type': 'choice',
                'instructions': 'Which decision should the user take on this permission request?',
                'criteria': criteria,
            },
        },
    }


def _available_decisions(request):
    """Normalize what the native card offers into recommendation keys."""
    params = request.get('params') or {}
    method = request.get('method')
    if method == 'acp/requestPermission':
        available = set()
        for option in params.get('options') or []:
            kind = str(option.get('kind') or '') if isinstance(option, dict) else ''
            if 'always' in kind and 'allow' in kind:
                available.add('accept_always')
            elif 'allow' in kind or 'accept' in kind:
                available.add('accept_once')
            elif 'reject' in kind or 'deny' in kind or 'decline' in kind:
                available.add('decline')
        return available or {'review'}
    if method == 'mcpServer/elicitation/request':
        modes = (params.get('computerUse') or {}).get('persistModes') or []
        available = {'accept_once', 'decline'}
        if 'session' in modes:
            available.add('accept_session')
        if 'always' in modes:
            available.add('accept_always')
        return available
    decisions = params.get('availableDecisions')
    if isinstance(decisions, list) and decisions:
        available = set()
        for decision in decisions:
            if decision == 'accept':
                available.add('accept_once')
            elif decision in ('decline', 'cancel'):
                available.add('decline')
        return available or {'review'}
    return {'accept_once', 'decline'}


def normalize_answer(payload):
    """Map a Jev response to the assessment shape the phone renders."""
    answers = (payload or {}).get('answers') or {}
    risk_answer, recommend_answer = answers.get('risk'), answers.get('recommend')
    reversible = answers.get('reversible') or {}
    if not isinstance(risk_answer, dict) or not isinstance(recommend_answer, dict):
        return None
    try:
        risk = max(0, min(3, int(round(float(risk_answer.get('score', 1))))))
    except (TypeError, ValueError):
        return None
    recommend = recommend_answer.get('choice')
    if recommend not in RECOMMEND_OPTIONS:
        return None
    assessment = {
        'source': 'jev',
        'risk': risk,
        'riskLabel': RISK_LABELS[risk],
        'reversible': _probability(reversible.get('noul')),
        'recommend': recommend,
        'confidence': _probability(recommend_answer.get('confidence')),
    }
    probabilities = recommend_answer.get('probabilities')
    if isinstance(probabilities, dict) and probabilities:
        assessment['options'] = {key: _probability(value) for key, value in probabilities.items()
                                 if key in RECOMMEND_OPTIONS}
    return assessment


def _probability(value):
    try:
        return max(0.0, min(1.0, round(float(value), 3)))
    except (TypeError, ValueError):
        return None


def parse_recommendation(request, assessment):
    """Translate the recommendation into ids the native card actually uses."""
    if not assessment or assessment.get('source') != 'jev':
        return None
    recommend = assessment.get('recommend')
    if not recommend or recommend == 'review':
        return None
    params = request.get('params') or {}
    method = request.get('method')
    if method == 'acp/requestPermission':
        wanted = 'always' if recommend == 'accept_always' else None
        for option in params.get('options') or []:
            if not isinstance(option, dict):
                continue
            kind = str(option.get('kind') or '')
            if recommend == 'decline':
                if any(word in kind for word in ('reject', 'deny', 'decline')):
                    return option.get('id')
            elif 'allow' in kind or 'accept' in kind:
                if wanted is None or wanted in kind:
                    return option.get('id')
        return None
    if method == 'mcpServer/elicitation/request':
        computer = params.get('computerUse') or {}
        if recommend == 'decline':
            return 'decline'
        if recommend == 'accept_always' and 'always' in (computer.get('persistModes') or []):
            return 'always'
        if recommend == 'accept_session' and 'session' in (computer.get('persistModes') or []):
            return 'session'
        return 'once'
    return {'accept_once': 'accept', 'accept_session': 'accept', 'accept_always': 'accept',
            'decline': 'decline'}.get(recommend)


class JevSettings:
    """Web-editable Jev configuration, one file per gateway.

    File values override the launch-time env vars; an empty saved key means
    explicitly disabled even when the env var is still set. Keys are never
    returned to the client in full — only a masked form for the settings form.
    """

    FIELDS = ('apiKey', 'baseUrl', 'model')
    ENV = {'apiKey': 'TYPESAFE_API_KEY', 'baseUrl': 'TYPESAFE_BASE_URL', 'model': 'JEV_MODEL'}

    def __init__(self, path):
        self.path = Path(path)
        self._lock = threading.Lock()
        self._values = self._load()

    def _load(self):
        try:
            data = json.loads(self.path.read_text(encoding='utf-8'))
        except (OSError, ValueError):
            return {}
        if not isinstance(data, dict):
            return {}
        return {key: value for key, value in data.items()
                if key in self.FIELDS and isinstance(value, str)}

    def read(self):
        """Effective values: saved file entry, else env var, else default."""
        with self._lock:
            saved = dict(self._values)
        effective = {}
        for field in self.FIELDS:
            if field in saved:
                effective[field] = saved[field]
            else:
                effective[field] = os.environ.get(self.ENV[field], '')
        effective['baseUrl'] = effective['baseUrl'] or DEFAULT_BASE_URL
        effective['model'] = effective['model'] or 'jev-latest'
        return effective

    def update(self, value):
        """Validate and persist a settings-form submission."""
        if not isinstance(value, dict):
            raise ValueError('Jev 设置格式不正确')
        parsed = {}
        with self._lock:
            saved = dict(self._values)
        if value.get('clearApiKey') is True:
            parsed['apiKey'] = ''
        else:
            submitted = value.get('apiKey')
            if submitted is None:
                # Not submitted: keep the saved one. A file that never held a
                # key stays that way so the env fallback survives unrelated
                # edits; the password input stays blank on reload by design.
                if 'apiKey' in saved:
                    parsed['apiKey'] = saved['apiKey']
            elif not isinstance(submitted, str):
                raise ValueError('API Key 格式不正确')
            else:
                key = submitted.strip()
                if key:
                    if not 8 <= len(key) <= 4096 or any(character.isspace() for character in key):
                        raise ValueError('API Key 格式不正确')
                    parsed['apiKey'] = key
                elif 'apiKey' in saved:
                    parsed['apiKey'] = saved['apiKey']
        for field, label, pattern in (('baseUrl', '端点', None), ('model', '模型',
                                     re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]{0,63}'))):
            raw = value.get(field, '')
            if not isinstance(raw, str):
                raise ValueError('Jev 设置格式不正确')
            raw = raw.strip()
            if raw and pattern is not None and not pattern.fullmatch(raw):
                raise ValueError('模型名格式不正确')
            if raw and field == 'baseUrl':
                parsed_url = urlsplit(raw)
                if parsed_url.scheme not in ('http', 'https') or not parsed_url.hostname:
                    raise ValueError('端点必须是 http(s) 地址')
                if parsed_url.scheme == 'http' and parsed_url.hostname not in ('localhost', '127.0.0.1', '::1'):
                    raise ValueError('端点请使用 HTTPS；HTTP 仅用于本机测试')
            parsed[field] = raw
        with self._lock:
            self._values = parsed
            write_json(self.path, parsed)
        return self.public()

    def public(self):
        """Client-facing view: masked key, effective endpoint and model."""
        effective = self.read()
        key = effective['apiKey']
        masked = (key[:3] + '…' + key[-4:]) if len(key) > 8 else ('已配置' if key else '')
        return {'configured': bool(key), 'apiKeyMasked': masked,
                'baseUrl': effective['baseUrl'], 'model': effective['model']}


_SHARED = None
_SHARED_LOCK = threading.Lock()


def init_shared_settings(path):
    """Bind the process-wide settings file; called once at gateway startup."""
    global _SHARED
    with _SHARED_LOCK:
        _SHARED = JevSettings(path)
    return _SHARED


def shared_settings():
    with _SHARED_LOCK:
        return _SHARED


def effective_field(field):
    """Current effective value for assessers, honoring the shared store."""
    store = shared_settings()
    if store is not None:
        return store.read()[field]
    return os.environ.get(JevSettings.ENV.get(field, ''), '')


class JevAssessor:
    """Cache-backed advisory assessment; failures fall back to local labels.

    api_key/base_url/model accept plain values (tests) or zero-argument
    callables so live settings edits apply to running bridges."""

    def __init__(self, base_url=None, api_key=None, model=None):
        self.base_url = base_url
        self.api_key = api_key
        self.model = model
        self._lock = threading.Lock()
        self._cache = {}
        self._inflight = set()

    @staticmethod
    def _resolve(value):
        return value() if callable(value) else value

    @property
    def enabled(self):
        return bool(self._resolve(self.api_key))

    def cached(self, key):
        with self._lock:
            return self._cache.get(key)

    def start(self, key, request, context, on_done):
        """Assess in the background; call on_done(assessment) exactly once."""
        if not self.enabled:
            return False
        with self._lock:
            if key in self._cache or key in self._inflight:
                return False
            self._inflight.add(key)
        threading.Thread(target=self._run, args=(key, request, context, on_done), daemon=True).start()
        return True

    def _run(self, key, request, context, on_done):
        assessment = None
        try:
            payload = self._call(build_payload(request, context))
            assessment = normalize_answer(payload)
        except Exception as exc:
            # Endpoint failures (HTTP 4xx/5xx, timeouts) are operational, not
            # code bugs: one log line, no traceback spam per approval card.
            LOG.warning('Jev assessment failed: %s', exc)
        finally:
            with self._lock:
                self._inflight.discard(key)
                # A failed lookup caches as None so one request never retries
                # in a loop; approvals are short-lived and rare.
                if len(self._cache) >= MAX_ASSESSMENTS:
                    self._cache.clear()
                self._cache[key] = assessment
        try:
            on_done(assessment)
        except Exception:
            LOG.warning("Jev assessment callback failed", exc_info=True)

    def _call(self, payload):
        payload = dict(payload, model=self._resolve(self.model) or 'jev-latest')
        body = json.dumps(payload, ensure_ascii=False).encode()
        headers = {'Authorization': 'Bearer ' + self._resolve(self.api_key),
                   'Content-Type': 'application/json',
                   'Content-Length': str(len(body))}
        connection = self._connect()
        try:
            connection.request('POST', '/v1/systemone', body=body, headers=headers)
            response = connection.getresponse()
            data = response.read(1 << 20)
            if response.status in (429, 529) or response.status >= 500:
                raise OSError(f'Jev endpoint returned HTTP {response.status}')
            if response.status != 200:
                raise ValueError(f'Jev endpoint returned HTTP {response.status}')
            return json.loads(data)
        finally:
            connection.close()

    def _connect(self):
        parsed = urlsplit(self._resolve(self.base_url) or DEFAULT_BASE_URL)
        if parsed.scheme == 'http':
            return http.client.HTTPConnection(parsed.hostname, parsed.port or 80, timeout=TIMEOUT)
        return http.client.HTTPSConnection(parsed.hostname, parsed.port or 443,
                                           timeout=TIMEOUT, context=client_context())
