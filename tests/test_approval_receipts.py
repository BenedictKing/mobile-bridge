"""Unit tests for the durable approval receipt ledger (bridge/approvals.py)."""
import json
import threading
import unittest
from pathlib import Path
from tempfile import TemporaryDirectory

from bridge.approvals import (ApprovalLedger, ACCEPTED, AWAITING, CLAIMED, DECLINED,
                              EXPIRED, UNKNOWN, RETENTION_SECONDS, decision_outcome,
                              request_hash)

ROOT = Path(__file__).resolve().parents[1]


def normalized(method='item/commandExecution/requestApproval', **params):
    return {'id': 'r1', 'method': method, 'supported': True, 'params': params}


def clock(start=1000.0):
    value = [start]

    def now():
        return value[0]
    now.advance = lambda seconds: value.__setitem__(0, value[0] + seconds)
    return now


class DecisionOutcomeTests(unittest.TestCase):
    def test_outcomes_by_method_and_response(self):
        self.assertEqual(decision_outcome('item/commandExecution/requestApproval',
                                          {'decision': 'accept'}, {}), ACCEPTED)
        self.assertEqual(decision_outcome('item/commandExecution/requestApproval',
                                          {'decision': 'decline'}, {}), DECLINED)
        self.assertEqual(decision_outcome('item/permissions/requestApproval',
                                          {'decision': 'cancel'}, {}), DECLINED)
        self.assertEqual(decision_outcome('mcpServer/elicitation/request',
                                          {'action': 'decline'}, {}), DECLINED)
        self.assertEqual(decision_outcome('item/tool/requestUserInput',
                                          {'answers': {}}, {}), ACCEPTED)
        self.assertEqual(decision_outcome('anything', 'not-a-dict', {}), ACCEPTED)

    def test_acp_option_kind_decides_the_outcome(self):
        params = {'options': [{'optionId': 'allow', 'kind': 'allow_once'},
                              {'optionId': 'deny', 'kind': 'reject_once'}]}
        self.assertEqual(decision_outcome('acp/requestPermission', {'optionId': 'allow'}, params), ACCEPTED)
        self.assertEqual(decision_outcome('acp/requestPermission', {'optionId': 'deny'}, params), DECLINED)
        self.assertEqual(decision_outcome('acp/requestPermission', {'cancel': True}, params), DECLINED)


class ApprovalLedgerTests(unittest.TestCase):
    def setUp(self):
        self.temp = TemporaryDirectory(dir=str(ROOT / '.tmp'))
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name) / 'approval-receipts.json'
        self.now = clock()
        self.ledger = ApprovalLedger(self.path, clock=self.now)

    def test_claim_then_duplicate_replays_stored_result(self):
        first = self.ledger.claim('t', 7, normalized(command='ls'), {'decision': 'accept'})
        self.assertEqual(first['status'], 'claimed')
        self.ledger.resolve('t', 7, ACCEPTED, result={'ok': True, 'native': 1})
        again = self.ledger.claim('t', 7, normalized(command='ls'), {'decision': 'accept'})
        self.assertEqual(again['status'], 'duplicate')
        self.assertEqual(again['result'], {'ok': True, 'native': 1})
        self.assertEqual(again['outcome'], ACCEPTED)

    def test_duplicate_replay_without_pending_request(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'decline'}, source='mobile')
        self.ledger.resolve('t', 7, DECLINED)
        replay = self.ledger.replay('t', 7, {'decision': 'decline'})
        self.assertEqual(replay['status'], 'duplicate')
        self.assertIsNone(self.ledger.replay('t', 'missing', {}))

    def test_conflicting_response_after_terminal_is_rejected(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        self.ledger.resolve('t', 7, ACCEPTED)
        with self.assertRaisesRegex(ValueError, '不同操作'):
            self.ledger.claim('t', 7, normalized(), {'decision': 'decline'})
        with self.assertRaisesRegex(ValueError, '不同操作'):
            self.ledger.replay('t', 7, {'decision': 'decline'})

    def test_claimed_and_unknown_states_block_retries(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        with self.assertRaisesRegex(ValueError, '正在处理'):
            self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        with self.assertRaisesRegex(ValueError, '正在处理'):
            self.ledger.replay('t', 7, {'decision': 'accept'})
        self.ledger.resolve('t', 7, UNKNOWN)
        with self.assertRaisesRegex(ValueError, '结果未知'):
            self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        with self.assertRaisesRegex(ValueError, '结果未知'):
            self.ledger.replay('t', 7, {'decision': 'accept'})

    def test_release_allows_a_corrected_submission(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        self.ledger.release('t', 7)
        receipt = self.ledger.receipt('t', 7)
        self.assertEqual(receipt['state'], AWAITING)
        self.assertNotIn('response', receipt)
        corrected = self.ledger.claim('t', 7, normalized(), {'decision': 'decline'})
        self.assertEqual(corrected['status'], 'claimed')
        self.ledger.resolve('t', 7, DECLINED)
        self.assertEqual(self.ledger.receipt('t', 7)['state'], DECLINED)

    def test_hash_change_expires_the_receipt(self):
        self.ledger.claim('t', 7, normalized(command='ls'), {'decision': 'accept'})
        self.ledger.resolve('t', 7, ACCEPTED)
        with self.assertRaisesRegex(ValueError, '已变化'):
            self.ledger.claim('t', 7, normalized(command='rm -rf /'), {'decision': 'accept'})
        self.assertEqual(self.ledger.receipt('t', 7)['state'], EXPIRED)
        with self.assertRaisesRegex(ValueError, '已过期'):
            self.ledger.claim('t', 7, normalized(command='rm -rf /'), {'decision': 'accept'})

    def test_awaiting_receipt_without_pending_expires(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        self.ledger.release('t', 7)
        with self.assertRaisesRegex(ValueError, '已处理或已过期'):
            self.ledger.replay('t', 7, {'decision': 'accept'})
        self.assertEqual(self.ledger.receipt('t', 7)['state'], EXPIRED)

    def test_concurrent_claims_produce_exactly_one_winner(self):
        barrier = threading.Barrier(8)
        outcomes = []
        lock = threading.Lock()

        def contender(index):
            barrier.wait()
            try:
                result = self.ledger.claim('t', 7, normalized(command='ls'),
                                           {'decision': 'accept'}, source=f'source-{index}')
                with lock:
                    outcomes.append(result['status'])
            except ValueError:
                with lock:
                    outcomes.append('rejected')

        threads = [threading.Thread(target=contender, args=(i,)) for i in range(8)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=5)
        self.assertEqual(outcomes.count('claimed'), 1)
        self.assertEqual(outcomes.count('rejected'), 7)
        self.assertEqual(self.ledger.receipt('t', 7)['state'], CLAIMED)

    def test_receipts_survive_reload_and_keep_claimed_tombstones(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        self.ledger.resolve('t', 7, ACCEPTED, result={'ok': True})
        self.ledger.claim('t', 8, normalized(), {'decision': 'accept'})
        reloaded = ApprovalLedger(self.path, clock=self.now)
        self.assertEqual(reloaded.receipt('t', 7)['state'], ACCEPTED)
        self.assertEqual(reloaded.receipt('t', 8)['state'], CLAIMED)
        again = reloaded.claim('t', 7, normalized(), {'decision': 'accept'})
        self.assertEqual(again['status'], 'duplicate')
        with self.assertRaisesRegex(ValueError, '正在处理'):
            reloaded.claim('t', 8, normalized(), {'decision': 'accept'})

    def test_settled_receipts_are_dropped_after_retention(self):
        self.ledger.claim('t', 7, normalized(), {'decision': 'accept'})
        self.ledger.resolve('t', 7, ACCEPTED)
        self.ledger.claim('t', 8, normalized(), {'decision': 'accept'})
        self.ledger.resolve('t', 8, UNKNOWN)
        self.now.advance(RETENTION_SECONDS + 1)
        reloaded = ApprovalLedger(self.path, clock=self.now)
        # The settled answer is gone; the unknown tombstone stays forever.
        self.assertIsNone(reloaded.receipt('t', 7))
        self.assertIsNone(reloaded.replay('t', 7, {'decision': 'accept'}))
        self.assertEqual(reloaded.receipt('t', 8)['state'], UNKNOWN)

    def test_corrupt_file_degrades_to_empty_with_backup(self):
        self.path.write_text('{not json', encoding='utf-8')
        ledger = ApprovalLedger(self.path, clock=self.now)
        self.assertEqual(ledger.claim('t', 7, normalized(), {'decision': 'accept'})['status'], 'claimed')
        backups = list(Path(self.temp.name).glob('approval-receipts.json.corrupt*'))
        self.assertEqual(len(backups), 1)

    def test_request_hash_is_canonical(self):
        first = request_hash(normalized(command='ls', availableDecisions=['accept', 'decline']))
        second = request_hash({'id': 'other', 'method': 'item/commandExecution/requestApproval',
                               'supported': True,
                               'params': {'availableDecisions': ['accept', 'decline'], 'command': 'ls'}})
        self.assertEqual(first, second)
        # List order is significant: a reordered option set is different content.
        self.assertNotEqual(first, request_hash(normalized(command='ls', availableDecisions=['decline', 'accept'])))
        self.assertNotEqual(first, request_hash(normalized(command='pwd')))

    def test_saved_file_permissions_and_format(self):
        self.ledger.claim('t', 'req', normalized(), {'decision': 'accept'})
        data = json.loads(self.path.read_text(encoding='utf-8'))
        self.assertIn('t:req', data)
        receipt = data['t:req']
        for field in ('threadId', 'requestId', 'method', 'requestHash', 'state', 'response', 'createdAt', 'claimedAt', 'source'):
            self.assertIn(field, receipt)
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)


if __name__ == '__main__':
    unittest.main()
