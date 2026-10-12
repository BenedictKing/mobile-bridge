"""Durable task-input records: open questions that pause a queued task.

A deferred task keeps its queued submission parked in ``waiting_input`` until
the user answers the recorded questions through the input endpoint. Answers
are validated against the question version, applied exactly once, and the
submission is CAS-recovered to ``queued`` so the existing maintenance loop
reschedules it. Cancelled, expired or already-answered tasks never resume.
"""
import copy
import json
import logging
import threading
import time
from pathlib import Path

LOG = logging.getLogger(__name__)

WAITING = 'waiting_input'
ANSWERED = 'answered'
CANCELLED = 'cancelled'
EXPIRED = 'expired'
ACTIVE = (WAITING, ANSWERED)
MAX_ANSWER_CHARS = 20000
MAX_QUESTIONS = 20
# An open question nobody can answer within a day is dead; the parked task is
# cancelled rather than resurrected from a stale context.
DEFAULT_TTL = 24 * 3600


def normalize_questions(questions):
    """Validate the small question shape the phone renders."""
    if not isinstance(questions, list) or not 1 <= len(questions) <= MAX_QUESTIONS:
        raise ValueError('请提供 1–20 个问题')
    normalized, seen = [], set()
    for question in questions:
        if not isinstance(question, dict):
            raise ValueError('问题格式不正确')
        identifier = question.get('id')
        text = question.get('question')
        if not isinstance(identifier, str) or not identifier.strip() or len(identifier) > 128:
            raise ValueError('问题标识无效')
        if not isinstance(text, str) or not text.strip() or len(text) > 2000:
            raise ValueError('问题内容需为 1–2000 字')
        identifier = identifier.strip()
        if identifier in seen:
            raise ValueError('问题标识不能重复')
        seen.add(identifier)
        row = {'id': identifier, 'question': text.strip(),
               'required': bool(question.get('required', True))}
        length = question.get('maxLength', MAX_ANSWER_CHARS)
        if not isinstance(length, int) or not 1 <= length <= MAX_ANSWER_CHARS:
            raise ValueError('答案长度限制无效')
        row['maxLength'] = length
        normalized.append(row)
    return normalized


def validate_answers(questions, answers):
    """Check completeness and lengths; returns the normalized answer dict."""
    if not isinstance(answers, dict):
        raise ValueError('请填写所有问题')
    expected = {question['id']: question for question in questions}
    if set(answers) - set(expected):
        raise ValueError('回答包含未知问题')
    result = {}
    for identifier, question in expected.items():
        value = answers.get(identifier)
        if value is None:
            value = ''
        if not isinstance(value, str):
            raise ValueError('回答需为文本：' + question['question'])
        if len(value) > question.get('maxLength', MAX_ANSWER_CHARS):
            raise ValueError('回答过长：' + question['question'])
        if question.get('required', True) and not value.strip():
            raise ValueError('请回答所有问题')
        result[identifier] = value
    return result


class TaskInputLedger:
    """JSON-file input records; one instance per bridge data directory."""

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
            backup = self.path.with_name(self.path.name + '.corrupt')
            LOG.exception("Task input ledger unreadable; starting empty (backup: %s)", backup)
            try:
                self.path.replace(backup)
            except OSError:
                LOG.exception("Task input ledger backup failed")
            return {}
        if not isinstance(data, dict):
            return {}
        return {key: value for key, value in data.items() if isinstance(value, dict)}

    def _save(self):
        target = self.path.with_suffix('.tmp')
        target.write_text(json.dumps(self._entries, ensure_ascii=False), encoding='utf-8')
        target.chmod(0o600)
        target.replace(self.path)

    @staticmethod
    def _key(thread_id, input_id):
        return str(thread_id) + ':' + str(input_id)

    def defer(self, thread_id, input_id, questions, summary='', ttl=DEFAULT_TTL):
        """Park a task with open questions; idempotent per (thread, input) id."""
        questions = normalize_questions(questions)
        if not isinstance(summary, str) or len(summary) > 2000:
            raise ValueError('任务摘要需为不超过 2000 字的文本')
        key = self._key(thread_id, input_id)
        with self._lock:
            self._expire_locked()
            record = self._entries.get(key)
            if record is not None:
                if (record.get('questions') != questions or record.get('summary') != summary.strip()):
                    raise ValueError('同一任务标识不能用于不同问题')
                if record.get('state') != WAITING:
                    raise ValueError('此任务的补充信息已处理')
                return copy.deepcopy(record)
            record = {'threadId': str(thread_id), 'inputId': str(input_id),
                      'summary': summary.strip(), 'questions': questions,
                      'questionVersion': 1, 'state': WAITING,
                      'checkpoint': {'dispatched': False},
                      'createdAt': self._clock(), 'expiresAt': self._clock() + ttl}
            self._entries[key] = record
            self._save()
            return copy.deepcopy(record)

    def get(self, thread_id, input_id):
        with self._lock:
            record = self._entries.get(self._key(thread_id, input_id))
            return copy.deepcopy(record) if record is not None else None

    def entries(self, thread_id):
        """Active records for one thread, oldest first."""
        with self._lock:
            self._expire_locked()
            rows = [record for record in self._entries.values()
                    if record.get('threadId') == str(thread_id) and record.get('state') in ACTIVE]
        return sorted(rows, key=lambda record: (record.get('answeredAt') is None, record.get('createdAt', 0)),
                      reverse=True)

    def waiting(self, thread_id):
        """Records still awaiting an answer."""
        with self._lock:
            self._expire_locked()
            return copy.deepcopy([record for record in self._entries.values()
                                  if record.get('threadId') == str(thread_id)
                                  and record.get('state') == WAITING])

    def submit(self, thread_id, input_id, answers, version=None):
        """Answer one record exactly once.

        Returns ``('answered'|'duplicate', record)`` or raises ``ValueError``
        for version drift, invalid answers, and terminal states.
        """
        key = self._key(thread_id, input_id)
        with self._lock:
            self._expire_locked()
            record = self._entries.get(key)
            if record is None:
                raise ValueError('此任务没有等待补充的信息')
            state = record.get('state')
            if state == ANSWERED:
                try:
                    same = validate_answers(record['questions'], answers) == record.get('answers')
                except ValueError:
                    same = False
                if same:
                    return 'duplicate', copy.deepcopy(record)
                raise ValueError('此任务已提交过不同回答，请刷新查看任务状态')
            if state == EXPIRED:
                raise ValueError('此任务的补充信息已过期，任务已取消')
            if state == CANCELLED:
                raise ValueError('此任务已取消，不能恢复执行')
            if version is not None and int(version) != int(record.get('questionVersion', 1)):
                raise ValueError('问题已变化，请刷新后重新回答')
            normalized = validate_answers(record['questions'], answers)
            record['state'] = ANSWERED
            record['answers'] = normalized
            record['answeredAt'] = self._clock()
            self._save()
            return 'answered', copy.deepcopy(record)

    def cancel(self, thread_id, input_id):
        with self._lock:
            record = self._entries.get(self._key(thread_id, input_id))
            if record is not None and record.get('state') == WAITING:
                record['state'] = CANCELLED
                self._save()
                return True
            return False

    def expire_due(self):
        """Sweep overdue records; returns the freshly expired list."""
        with self._lock:
            return self._expire_locked()

    def _expire_locked(self):
        now = self._clock()
        expired = []
        for key, record in self._entries.items():
            if record.get('state') == WAITING and now > record.get('expiresAt', 0):
                record['state'] = EXPIRED
                expired.append(copy.deepcopy(record))
        if expired:
            self._save()
        return expired
