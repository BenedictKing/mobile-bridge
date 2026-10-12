"""Unit tests for the durable task-input ledger (bridge/inputs.py)."""
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from bridge.inputs import (TaskInputLedger, ANSWERED, CANCELLED, EXPIRED, WAITING,
                           normalize_questions, validate_answers)

ROOT = Path(__file__).resolve().parents[1]


def clock(start=1000.0):
    value = [start]

    def now():
        return value[0]
    now.advance = lambda seconds: value.__setitem__(0, value[0] + seconds)
    return now


QUESTIONS = [{'id': 'target', 'question': '迁移到哪个项目？'},
             {'id': 'note', 'question': '还有其他要求吗？', 'required': False}]


class QuestionTests(unittest.TestCase):
    def test_normalize_questions_shape(self):
        rows = normalize_questions([{'id': ' a ', 'question': ' 问？ '}])
        self.assertEqual(rows, [{'id': 'a', 'question': '问？', 'required': True, 'maxLength': 20000}])
        for bad in ([], 'x', [{}], [{'id': 'a'}], [{'id': 'a', 'question': ''}],
                    [{'id': 'a', 'question': 'q'}, {'id': 'a', 'question': 'r'}],
                    [{'id': 'a', 'question': 'q', 'maxLength': 0}]):
            with self.assertRaises(ValueError):
                normalize_questions(bad)

    def test_validate_answers_completeness_and_length(self):
        self.assertEqual(validate_answers(QUESTIONS, {'target': 'proj', 'note': ''}),
                         {'target': 'proj', 'note': ''})
        for bad in (None, 'x', {'target': ''}, {'target': 'x', 'extra': 'y'}, {'target': 3},
                    {'target': 'x' * 20001}):
            with self.assertRaises(ValueError):
                validate_answers(QUESTIONS, bad)


class TaskInputLedgerTests(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory(dir=str(ROOT / '.tmp'))
        self.addCleanup(self.temp.cleanup)
        self.now = clock()
        self.ledger = TaskInputLedger(Path(self.temp.name) / 'task-inputs.json', clock=self.now)

    def test_defer_is_idempotent_and_binds_questions(self):
        first = self.ledger.defer('t', 'sub1', QUESTIONS, '摘要')
        again = self.ledger.defer('t', 'sub1', QUESTIONS, '摘要')
        self.assertEqual(first, again)
        with self.assertRaisesRegex(ValueError, '不同问题'):
            self.ledger.defer('t', 'sub1', [{'id': 'x', 'question': '其他'}], '摘要')
        self.assertEqual(self.ledger.defer('t', 'sub2', QUESTIONS)['summary'], '')

    def test_submit_answers_once_then_duplicate_or_conflict(self):
        self.ledger.defer('t', 'sub1', QUESTIONS)
        outcome, record = self.ledger.submit('t', 'sub1', {'target': 'proj', 'note': '快点'})
        self.assertEqual(outcome, 'answered')
        self.assertEqual(record['answers'], {'target': 'proj', 'note': '快点'})
        outcome, _ = self.ledger.submit('t', 'sub1', {'target': 'proj', 'note': '快点'})
        self.assertEqual(outcome, 'duplicate')
        with self.assertRaisesRegex(ValueError, '不同回答'):
            self.ledger.submit('t', 'sub1', {'target': 'other', 'note': ''})
        # Answered tasks cannot be deferred again.
        with self.assertRaisesRegex(ValueError, '已处理'):
            self.ledger.defer('t', 'sub1', QUESTIONS)

    def test_version_drift_and_validation_reject(self):
        self.ledger.defer('t', 'sub1', QUESTIONS)
        with self.assertRaisesRegex(ValueError, '问题已变化'):
            self.ledger.submit('t', 'sub1', {'target': 'x', 'note': ''}, version=2)
        with self.assertRaisesRegex(ValueError, '请回答所有问题'):
            self.ledger.submit('t', 'sub1', {'target': '', 'note': ''}, version=1)
        with self.assertRaisesRegex(ValueError, '过长'):
            self.ledger.submit('t', 'sub1', {'target': 'x' * 20001, 'note': ''})
        # Rejections leave the record waiting.
        self.assertEqual(self.ledger.get('t', 'sub1')['state'], WAITING)

    def test_cancelled_and_expired_never_resume(self):
        self.ledger.defer('t', 'sub1', QUESTIONS)
        self.assertTrue(self.ledger.cancel('t', 'sub1'))
        with self.assertRaisesRegex(ValueError, '已取消'):
            self.ledger.submit('t', 'sub1', {'target': 'x', 'note': ''})
        self.ledger.defer('t', 'sub2', QUESTIONS, ttl=10)
        self.now.advance(11)
        with self.assertRaisesRegex(ValueError, '已过期'):
            self.ledger.submit('t', 'sub2', {'target': 'x', 'note': ''})
        self.assertEqual(self.ledger.get('t', 'sub2')['state'], EXPIRED)
        self.assertEqual(self.ledger.entries('t'), [])

    def test_waiting_and_entries_filters(self):
        self.ledger.defer('t', 'sub1', QUESTIONS)
        self.ledger.defer('t', 'sub2', QUESTIONS)
        self.ledger.submit('t', 'sub1', {'target': 'x', 'note': ''})
        self.assertEqual([record['inputId'] for record in self.ledger.waiting('t')], ['sub2'])
        # Answered records stay listed for status display; waiting ones come first.
        self.assertEqual([record['inputId'] for record in self.ledger.entries('t')], ['sub2', 'sub1'])

    def test_records_survive_reload(self):
        self.ledger.defer('t', 'sub1', QUESTIONS)
        self.ledger.submit('t', 'sub1', {'target': 'proj', 'note': ''})
        reloaded = TaskInputLedger(Path(self.temp.name) / 'task-inputs.json', clock=self.now)
        self.assertEqual(reloaded.get('t', 'sub1')['state'], ANSWERED)
        outcome, _ = reloaded.submit('t', 'sub1', {'target': 'proj', 'note': ''})
        self.assertEqual(outcome, 'duplicate')

    def test_submit_unknown_task(self):
        with self.assertRaisesRegex(ValueError, '没有等待补充的信息'):
            self.ledger.submit('t', 'missing', {'a': ''})


if __name__ == '__main__':
    unittest.main()
