"""Durable approval receipts: claim-before-effect idempotency for approvals.

Every decision routed to an adapter's respond path first claims a receipt
keyed by ``thread_id + request_id``. The receipt binds the request content
through a canonical hash and stores the accepted response, so a weak-network
retry or a double tap replays the recorded result instead of hitting the
desktop twice, and a conflicting second decision is rejected outright.

Outcome discipline: an adapter callback that times out, disconnects or dies
leaves ``outcome_unknown``. Such a receipt is never automatically replayed —
the user must reconcile the actual effect on the desktop before a new
approval cycle can start.
"""
import copy
import hashlib
import json
import logging
import threading
import time
from pathlib import Path

LOG = logging.getLogger(__name__)

AWAITING = 'awaiting_review'
CLAIMED = 'claimed'
ACCEPTED = 'accepted'
DECLINED = 'declined'
UNKNOWN = 'outcome_unknown'
EXPIRED = 'expired'
TERMINAL = (ACCEPTED, DECLINED)
DROPPABLE = TERMINAL + (EXPIRED,)
# Receipts outlive their effects, but not forever: settled records only exist
# to answer retries, and no retry is plausible after a week.
RETENTION_SECONDS = 7 * 86400


def request_hash(normalized):
    """Canonical digest of one normalized request (method plus whitelisted params)."""
    payload = [normalized.get('method'), normalized.get('params') or {}]
    return hashlib.sha256(json.dumps(payload, ensure_ascii=False,
                                     sort_keys=True).encode('utf-8')).hexdigest()


def decision_outcome(method, response, params):
    """Map one accepted response onto the terminal receipt state."""
    if not isinstance(response, dict):
        return ACCEPTED
    if method == 'acp/requestPermission':
        if response.get('cancel') is True:
            return DECLINED
        for option in (params or {}).get('options') or []:
            if isinstance(option, dict) and option.get('optionId') == response.get('optionId'):
                kind = str(option.get('kind') or '')
                return DECLINED if any(word in kind for word in ('reject', 'deny', 'decline')) else ACCEPTED
        return ACCEPTED
    decision = response.get('decision', response.get('action'))
    return DECLINED if decision in ('decline', 'cancel') else ACCEPTED


class ApprovalLedger:
    """JSON-file receipt store; one instance per bridge data directory.

    All state transitions happen under one process-wide lock and are persisted
    with the temp-file replace pattern before the caller proceeds, so a crash
    between claim and effect leaves a durable ``claimed`` tombstone instead of
    a silent second execution after restart.
    """

    def __init__(self, path, clock=time.time):
        self.path = Path(path)
        self._lock = threading.Lock()
        self._clock = clock
        self._entries = self._load()

    def _load(self):
        try:
            data = json.loads(self.path.read_text(encoding='utf-8'))
        except FileNotFoundError:
            return {}
        except (OSError, ValueError):
            # A corrupt ledger degrades loudly: keep the broken bytes beside
            # the live file and start empty rather than overwrite evidence.
            backup = self.path.with_name(self.path.name + '.corrupt')
            LOG.exception("Approval ledger unreadable; starting empty (backup: %s)", backup)
            try:
                self.path.replace(backup)
            except OSError:
                LOG.exception("Approval ledger backup failed")
            return {}
        if not isinstance(data, dict):
            return {}
        now = self._clock()
        return {key: value for key, value in data.items()
                if isinstance(value, dict) and not self._expired_retention(value, now)}

    @staticmethod
    def _expired_retention(receipt, now):
        return (receipt.get('state') in DROPPABLE and
                now - (receipt.get('resolvedAt') or receipt.get('claimedAt')
                       or receipt.get('createdAt') or now) > RETENTION_SECONDS)

    def _save(self):
        now = self._clock()
        live = {key: value for key, value in self._entries.items()
                if not self._expired_retention(value, now)}
        self._entries = live
        target = self.path.with_suffix('.tmp')
        target.write_text(json.dumps(live, ensure_ascii=False), encoding='utf-8')
        target.chmod(0o600)
        target.replace(self.path)

    @staticmethod
    def _key(thread_id, request_id):
        return str(thread_id) + ':' + str(request_id)

    def claim(self, thread_id, request_id, normalized, response, source=None):
        """Atomically move a (non-existent or awaiting) receipt to ``claimed``.

        Returns ``{'status': 'claimed'}`` for the single winner and
        ``{'status': 'duplicate', 'result': ...}`` for a retry that matches a
        settled receipt. Every other situation raises ``ValueError``.
        """
        key = self._key(thread_id, request_id)
        digest = request_hash(normalized)
        with self._lock:
            receipt = self._entries.get(key)
            if receipt is None:
                receipt = {'threadId': str(thread_id), 'requestId': str(request_id),
                           'method': normalized.get('method'), 'requestHash': digest,
                           'state': AWAITING, 'createdAt': self._clock(), 'source': source}
                self._entries[key] = receipt
            state = receipt.get('state')
            if state == EXPIRED:
                raise ValueError('此审批已过期，请刷新后重新处理')
            if receipt.get('requestHash') != digest:
                # The desktop re-issued this id with different content; the old
                # decision no longer binds. Tombstone it and require a refresh.
                receipt['state'] = EXPIRED
                self._save()
                raise ValueError('审批内容已变化，请刷新后重试')
            if state in TERMINAL:
                if receipt.get('response') != response:
                    raise ValueError('此审批已提交过不同操作，请在电脑 App 核对结果')
                return {'status': 'duplicate', 'result': copy.deepcopy(receipt.get('result')),
                        'outcome': state}
            if state == CLAIMED:
                raise ValueError('此审批正在处理中，请稍后刷新查看结果')
            if state == UNKNOWN:
                raise ValueError('上次审批结果未知，请在电脑 App 核对实际效果后再处理')
            if state == EXPIRED:
                raise ValueError('此审批已过期，请刷新后重新处理')
            receipt['state'] = CLAIMED
            receipt['response'] = copy.deepcopy(response)
            receipt['claimedAt'] = self._clock()
            if source:
                receipt['source'] = source
            self._save()
            return {'status': 'claimed'}

    def replay(self, thread_id, request_id, response):
        """Answer a retry whose desktop request is no longer pending.

        Returns ``None`` when no receipt exists (the caller reports the usual
        stale-request error), a duplicate result for a matching settled
        receipt, or raises for states that must not auto-retry.
        """
        with self._lock:
            receipt = self._entries.get(self._key(thread_id, request_id))
            if receipt is None:
                return None
            state = receipt.get('state')
            if state in TERMINAL:
                if receipt.get('response') != response:
                    raise ValueError('此审批已提交过不同操作，请在电脑 App 核对结果')
                return {'status': 'duplicate', 'result': copy.deepcopy(receipt.get('result')),
                        'outcome': state}
            if state == CLAIMED:
                raise ValueError('此审批正在处理中，请稍后刷新查看结果')
            if state == UNKNOWN:
                raise ValueError('上次审批结果未知，请在电脑 App 核对实际效果后再处理')
            # Awaiting or expired with the request already gone on the desktop:
            # nothing can ever be delivered for this id again.
            if state != EXPIRED:
                receipt['state'] = EXPIRED
                self._save()
            raise ValueError('此请求已处理或已过期')

    def release(self, thread_id, request_id):
        """Return a claimed receipt to ``awaiting_review`` after a pre-effect rejection.

        Adapter validation failures happen strictly before any side effect, so
        the caller may correct the response and submit again.
        """
        with self._lock:
            receipt = self._entries.get(self._key(thread_id, request_id))
            if receipt is not None and receipt.get('state') == CLAIMED:
                receipt['state'] = AWAITING
                receipt.pop('response', None)
                receipt.pop('claimedAt', None)
                self._save()

    def resolve(self, thread_id, request_id, outcome, result=None):
        """Settle a claimed receipt; unknown outcomes stay unanswerable."""
        with self._lock:
            receipt = self._entries.get(self._key(thread_id, request_id))
            if receipt is None or receipt.get('state') != CLAIMED:
                return False
            receipt['state'] = outcome if outcome in TERMINAL else UNKNOWN
            receipt['resolvedAt'] = self._clock()
            if result is not None:
                receipt['result'] = copy.deepcopy(result)
            self._save()
            return True

    def receipt(self, thread_id, request_id):
        """Read-only copy of one receipt (diagnostics and tests)."""
        with self._lock:
            return copy.deepcopy(self._entries.get(self._key(thread_id, request_id)))
