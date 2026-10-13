"""Jev risk triage for permission confirmations and task continuation.

Jev (TypeSafe AI) answers typed questions only; it never generates text. By
default the assessment is advisory. An explicit auto-approve setting may
submit only high-confidence, low-risk, reversible allow-once/session choices.

A separate auto-continue setting lets the gateway resume unfinished work:
when a live turn settles, Jev judges whether the user's task is complete and
why the agent stopped; an incomplete task (or an unexpectedly interrupted
turn, even without a Jev key) sends one automatic "继续" message. Stops that
look like quota or billing exhaustion never continue — a deterministic
keyword check runs before and below the Jev classification.

Configuration is web-editable and process-wide: `JevSettings` persists under
the gateway root (jev.json, 0600). Launch-time env vars (TYPESAFE_API_KEY,
TYPESAFE_BASE_URL, JEV_MODEL, JEV_PROXY) stay as defaults; saving a value in
the file overrides them and clearing the key disables Jev even when the env
var is still set. Without a key the module stays disabled and the phone keeps
the deterministic local labels.

An optional HTTP proxy (`proxy`, e.g. http://127.0.0.1:7890) routes every
assessment through one CONNECT tunnel. The endpoint is unreachable from some
networks (HTTP 451 regional blocks) without one; stdlib http.client never
consults the system proxy on its own. An empty value connects directly and
also disables the JEV_PROXY fallback.
"""
import hashlib
import json
import logging
import os
import re
import socket
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

# Automatic approval is intentionally narrow. A missing value, low confidence,
# non-reversible effect, or elevated risk always falls back to a human card.
AUTO_MAX_RISK = 1
AUTO_MIN_REVERSIBLE = 0.8
AUTO_MIN_CONFIDENCE = 0.8


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


def auto_response(request, assessment):
    """Return a safe native acceptance response, or ``None`` for review.

    Jev never gets to choose a permanent grant automatically. The response is
    limited to an allow-once/session decision whose risk and confidence meet
    the explicit policy above.
    """
    if not assessment or assessment.get('source') != 'jev':
        return None
    if assessment.get('recommend') not in ('accept_once', 'accept_session'):
        return None
    try:
        if (int(assessment.get('risk', 99)) > AUTO_MAX_RISK or
                float(assessment.get('reversible', -1)) < AUTO_MIN_REVERSIBLE or
                float(assessment.get('confidence', -1)) < AUTO_MIN_CONFIDENCE):
            return None
    except (TypeError, ValueError):
        return None
    native = parse_recommendation(request, assessment)
    if native is None:
        return None
    if request.get('method') == 'acp/requestPermission':
        return {'optionId': native}
    if request.get('method') == 'mcpServer/elicitation/request':
        return {'action': 'accept', 'persist': native} if native != 'once' else {'action': 'accept'}
    return {'decision': native}


# -- auto continue -------------------------------------------------------
#
# A turn that settles while quota or billing is exhausted must never be
# resumed: every "继续" would just bounce off the same paywall. The keyword
# scan is deliberately wide and runs below Jev — a false positive costs one
# skipped nudge, a false negative costs an error loop.
QUOTA_STOP_RE = re.compile(
    r'额度不足|额度已|余额不足|余额耗尽|余额已|积分不足|欠费|充值|配额不足|配额已'
    r'|用量限制|用量已达|用量超|达到.{0,6}上限|请求过于频繁|频率限制|限流'
    # Separators must be real (space/hyphen): usage_limit / RateLimiter are
    # code identifiers, not paywall messages.
    r'|usage[\s-]+limit|rate[\s-]+limit|rate-limit|quota|insufficient\s+credit'
    r'|out\s+of\s+credit|credit\s+balance|no\s+credits?\b|exceeded\s+your'
    r'|limit\s+reached|too\s+many\s+requests|payment\s+required|billing'
    r'|subscription\s+limit|\b402\b|\b429\b', re.IGNORECASE)

CONTINUE_STOP_KINDS = ('finished', 'interrupted', 'quota', 'awaiting_user', 'other')
# A resume needs the same certainty bar as an auto approval: below it the
# task waits for the human, not for a coin flip.
CONTINUE_MIN_CONFIDENCE = 0.8


def quota_stop(*texts):
    """True when any stop context text signals quota/billing exhaustion."""
    return any(isinstance(text, str) and QUOTA_STOP_RE.search(text)
               for text in texts)


def build_continue_payload(context):
    """One Jev request: is the task done, why did it stop, should we resume."""
    return {
        'model': 'jev-latest',
        'state': dict(context or {}),
        'questions': {
            'complete': {
                'type': 'noul',
                'instructions': 'Based on this conversation tail, is the '
                                "user's task fully completed?",
                'criteria': {
                    'true': 'The agent finished the requested work: the final '
                            'message states or shows the outcome, and nothing '
                            'requested is left open.',
                    'false': 'Work remains: the agent stopped midway, the task '
                             'was interrupted, the plan is only partly done, '
                             'or the final message defers or truncates work.',
                },
            },
            'stop_kind': {
                'type': 'choice',
                'instructions': 'Why did the agent stop after this turn?',
                'criteria': {
                    'finished': 'The agent chose to end the turn because the '
                                'task is done.',
                    'interrupted': 'The turn was cut off unexpectedly: a '
                                   'timeout, process exit, output/token limit, '
                                   'or transport drop.',
                    'quota': 'The account ran out of quota, credits, or rate '
                             'limit; the message mentions usage limits, '
                             'billing, or asks to upgrade or recharge.',
                    'awaiting_user': 'The agent asked the user a question or '
                                     'needs more input before it can proceed.',
                    'other': 'None of the above clearly applies.',
                },
            },
            'continue': {
                'type': 'choice',
                'instructions': 'Should the gateway automatically send one '
                                '"继续" (continue) message so the agent keeps '
                                'working on this task?',
                'criteria': {
                    'continue': 'The task is incomplete, the agent is able to '
                                'make progress, and resuming is safe.',
                    'stop': 'Do not resume: the task is complete, blocked on '
                            'the user, out of quota, or resuming would be '
                            'unsafe or pointless.',
                    'review': 'Not confident enough to decide either way.',
                },
            },
        },
    }


def normalize_continue_answer(payload):
    """Map a Jev continuation response to the decision shape, or None."""
    answers = (payload or {}).get('answers') or {}
    complete = answers.get('complete') if isinstance(answers.get('complete'), dict) else {}
    kind = answers.get('stop_kind') if isinstance(answers.get('stop_kind'), dict) else {}
    resume = answers.get('continue') if isinstance(answers.get('continue'), dict) else {}
    choice = resume.get('choice')
    if choice not in ('continue', 'stop', 'review'):
        return None
    result = {'source': 'jev',
              'shouldContinue': choice == 'continue',
              'confidence': _probability(resume.get('confidence'))}
    noul = _probability(complete.get('noul'))
    if noul is not None:
        result['complete'] = noul >= 0.5
        result['incomplete'] = noul < 0.5
    if kind.get('choice') in CONTINUE_STOP_KINDS:
        result['stopKind'] = kind.get('choice')
    return result


def decide_continue(context, assessment):
    """Whether to send the automatic continue; returns (send, reason).

    ``context`` is the shape ``build_continue_payload`` serializes:
    turnStatus/stopReason/error plus the conversation tail. The deterministic
    quota check always wins over Jev; a missing assessment (no key or an API
    failure) still resumes unexpectedly interrupted turns.
    """
    stop_reason = str(context.get('stopReason') or '')
    error = str(context.get('error') or '')
    tail = str(context.get('assistantTail') or '')[-800:]
    if quota_stop(stop_reason, error, tail):
        return False, 'quota'
    status = str(context.get('turnStatus') or '')
    if status == 'interrupted' or stop_reason == 'cancelled':
        # The user stopped the turn; resuming would undo their decision.
        return False, 'cancelled'
    interrupted = status == 'failed' or stop_reason in ('max_tokens', 'max_turns')
    if assessment is None or assessment.get('source') != 'jev':
        return (True, 'interrupted') if interrupted else (False, 'undecided')
    kind = assessment.get('stopKind')
    if kind == 'quota':
        return False, 'quota'
    if kind == 'awaiting_user':
        return False, 'awaiting_user'
    if not assessment.get('incomplete'):
        return False, 'complete' if assessment.get('incomplete') is False else 'unsure'
    if assessment.get('shouldContinue') is not True:
        return False, 'jev_stop' if assessment.get('shouldContinue') is False else 'jev_review'
    confidence = assessment.get('confidence') or 0.0
    if confidence < CONTINUE_MIN_CONFIDENCE:
        return False, 'low_confidence'
    return True, 'jev_continue'


class JevSettings:
    """Web-editable Jev configuration, one file per gateway.

    File values override the launch-time env vars; an empty saved key means
    explicitly disabled even when the env var is still set. Keys are never
    returned to the client in full — only a masked form for the settings form.
    """

    FIELDS = ('apiKey', 'baseUrl', 'model', 'proxy', 'autoApprove', 'autoContinue')
    ENV = {'apiKey': 'TYPESAFE_API_KEY', 'baseUrl': 'TYPESAFE_BASE_URL',
           'model': 'JEV_MODEL', 'proxy': 'JEV_PROXY'}
    BOOL_FIELDS = ('autoApprove', 'autoContinue')

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
        values = {key: value for key, value in data.items()
                  if key in ('apiKey', 'baseUrl', 'model', 'proxy') and isinstance(value, str)}
        for field in self.BOOL_FIELDS:
            if isinstance(data.get(field), bool):
                values[field] = data[field]
        return values

    def read(self):
        """Effective values: saved file entry, else env var, else default."""
        with self._lock:
            saved = dict(self._values)
        effective = {}
        for field in ('apiKey', 'baseUrl', 'model', 'proxy'):
            if field in saved:
                effective[field] = saved[field]
            else:
                effective[field] = os.environ.get(self.ENV[field], '')
        effective['baseUrl'] = effective['baseUrl'] or DEFAULT_BASE_URL
        effective['model'] = effective['model'] or 'jev-latest'
        effective['proxy'] = effective['proxy'] or ''
        for field in self.BOOL_FIELDS:
            effective[field] = bool(saved.get(field, False))
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
        if 'proxy' in value:
            raw_proxy = value['proxy']
            if not isinstance(raw_proxy, str):
                raise ValueError('Jev 设置格式不正确')
            raw_proxy = raw_proxy.strip()
            if raw_proxy:
                # CONNECT tunnels ride a plain HTTP proxy; credentials would
                # be silently ignored, so reject them instead of half-working.
                parsed_proxy = urlsplit(raw_proxy)
                try:
                    proxy_port = parsed_proxy.port
                except ValueError:
                    proxy_port = None
                if (parsed_proxy.scheme != 'http' or not parsed_proxy.hostname or
                        parsed_proxy.username or parsed_proxy.password or
                        proxy_port is None or not 1 <= proxy_port <= 65535 or
                        parsed_proxy.path or parsed_proxy.query or parsed_proxy.fragment):
                    raise ValueError('代理必须是 http://主机:端口 形式')
            parsed['proxy'] = raw_proxy
        elif 'proxy' in saved:
            parsed['proxy'] = saved['proxy']
        if 'autoApprove' in value:
            if not isinstance(value['autoApprove'], bool):
                raise ValueError('自动审核设置格式不正确')
            parsed['autoApprove'] = value['autoApprove']
        elif 'autoApprove' in saved:
            parsed['autoApprove'] = saved['autoApprove']
        if 'autoContinue' in value:
            if not isinstance(value['autoContinue'], bool):
                raise ValueError('自动继续设置格式不正确')
            parsed['autoContinue'] = value['autoContinue']
        elif 'autoContinue' in saved:
            parsed['autoContinue'] = saved['autoContinue']
        with self._lock:
            self._values = parsed
            write_json(self.path, parsed)
        return self.public()

    def public(self):
        """Client-facing view: masked key, effective endpoint, model, proxy."""
        effective = self.read()
        key = effective['apiKey']
        masked = (key[:3] + '…' + key[-4:]) if len(key) > 8 else ('已配置' if key else '')
        return {'configured': bool(key), 'apiKeyMasked': masked,
                'baseUrl': effective['baseUrl'], 'model': effective['model'],
                'proxy': effective['proxy'], 'autoApprove': effective['autoApprove'],
                'autoContinue': effective['autoContinue']}


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


def auto_approve_enabled():
    store = shared_settings()
    return bool(store and store.read().get('autoApprove'))


def auto_continue_enabled():
    store = shared_settings()
    return bool(store and store.read().get('autoContinue'))


class JevAssessor:
    """Cache-backed advisory assessment; failures fall back to local labels.

    api_key/base_url/model/proxy accept plain values (tests) or zero-argument
    callables so live settings edits apply to running bridges."""

    def __init__(self, base_url=None, api_key=None, model=None, proxy=None):
        self.base_url = base_url
        self.api_key = api_key
        self.model = model
        self.proxy = proxy
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
        """Assess one permission request in the background."""
        return self._start(key, lambda: build_payload(request, context),
                           normalize_answer, on_done)

    def start_continue(self, key, context, on_done):
        """Judge one settled turn in the background; on_done gets the
        continuation decision shape (or None on failure)."""
        return self._start(key, lambda: build_continue_payload(context),
                           normalize_continue_answer, on_done)

    def _start(self, key, build, normalize, on_done):
        """Assess in the background; call on_done(assessment) exactly once."""
        if not self.enabled:
            return False
        with self._lock:
            if key in self._cache or key in self._inflight:
                return False
            self._inflight.add(key)
        threading.Thread(target=self._run, args=(key, build, normalize, on_done),
                         daemon=True).start()
        return True

    def _run(self, key, build, normalize, on_done):
        assessment = None
        try:
            payload = self._call(build())
            assessment = normalize(payload)
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
        proxy = urlsplit(self._resolve(self.proxy) or '')
        secure = parsed.scheme != 'http'
        port = parsed.port or (443 if secure else 80)
        if proxy.hostname:
            # One CONNECT tunnel per assessment; only the proxy sees the
            # target, and the TLS session (if any) is end-to-end with it.
            raw = socket.create_connection((proxy.hostname, proxy.port or 80), timeout=TIMEOUT)
            try:
                _connect_tunnel(raw, parsed.hostname, port)
                sock = (client_context().wrap_socket(raw, server_hostname=parsed.hostname)
                        if secure else raw)
            except OSError:
                raw.close()
                raise
            connection = http.client.HTTPConnection(parsed.hostname, port, timeout=TIMEOUT)
            connection.sock = sock
            return connection
        if secure:
            return http.client.HTTPSConnection(parsed.hostname, port, timeout=TIMEOUT,
                                               context=client_context())
        return http.client.HTTPConnection(parsed.hostname, port, timeout=TIMEOUT)


def _connect_tunnel(sock, hostname, port):
    """Complete an HTTP proxy CONNECT handshake on an open socket."""
    target = '%s:%d' % (hostname, port)
    sock.sendall(('CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n' % (target, target)).encode('ascii'))
    reply = b''
    while b'\r\n\r\n' not in reply:
        chunk = sock.recv(4096)
        if not chunk:
            raise OSError('代理在 CONNECT 握手期间关闭了连接')
        reply += chunk
        if len(reply) > 8192:
            raise OSError('代理的 CONNECT 响应异常')
    status_line = reply.split(b'\r\n', 1)[0]
    if b' 200 ' not in status_line:
        raise OSError('代理拒绝了 CONNECT 请求：%s' % status_line.decode('ascii', 'replace')[:120])
