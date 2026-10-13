"""ACP projection reconcile: store-driven rebuild after externally driven turns.

The wrapper's event stream only describes turns driven by its own child
process; a turn driven elsewhere (desktop app, CLI) advances the stored
history silently. These tests pin the whole contract: freshness detection,
atomic handle swap, failure retention, late-event isolation, the manual
reconnect path, and the empty-plan panel filter.
"""
import tempfile
import types
import threading
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents import acp_adapter
from bridge.agents.acp_adapter import AcpAdapter, TurnNotDispatched
from bridge.agents.acp_view import AcpSessionView
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = 'session_fake-0001'
T1 = 1790000000000
T2 = T1 + 60_000
T3 = T2 + 60_000
CONFIG_OPTIONS = [
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'm1', 'options': [{'value': 'm1', 'name': 'M1'}]},
]
MODES = {'currentModeId': 'default', 'availableModes': [{'id': 'default', 'name': 'Default'}]}


def chunk(kind, text, mid=None):
    """One update; mid mimics the messageId wrappers attach per message."""
    update = {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}
    if mid is not None:
        update['messageId'] = mid
    return update


def agent_texts(view):
    return [m['text'] for t in view['turns'] for m in t['messages']
            if m.get('role') == 'assistant' and m.get('kind') == 'agentMessage']


class AcpReconcileTests(unittest.TestCase):
    def make_bridge(self, load_updates=None, updated=T1):
        fake = FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Chat', 'updatedAt': updated}],
            )
        fake.load_updates = load_updates if load_updates is not None else [
            chunk('user_message_chunk', 'hi'),
            chunk('agent_message_chunk', 'hello', mid='m1'),
        ]
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = AcpAdapter('fake-acp', data_dir=Path(tmp.name) / 'data',
                             spawn=fake.spawn_factory())
        bridge = Bridge(Path(tmp.name), Path(tmp.name) / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        return fake, adapter, bridge

    def wait_for(self, predicate, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.02)
        return False

    def view(self, bridge):
        return bridge.view(SID)

    def session(self, bridge):
        return bridge.session(SID)

    def external_turn(self, fake, text, updated):
        """Simulate a desktop-driven turn: store gains content + recency."""
        fake.load_updates.append(chunk('agent_message_chunk', text, mid='m%d' % updated))
        for row in fake.list_rows:
            if row.get('sessionId') == SID:
                row['updatedAt'] = updated

    def test_reconcile_picks_up_desktop_driven_turn(self):
        fake, adapter, bridge = self.make_bridge()
        view = self.view(bridge)
        self.assertIn('hello', agent_texts(view))
        self.assertNotIn('看完了', agent_texts(view))
        old = adapter._handles[SID]
        loads_before = len([r for r in fake.requests if r['method'] == 'session/load'])
        self.external_turn(fake, '看完了。最终回复', T2)
        with patch.object(acp_adapter, 'RECONCILE_IDLE_GRACE', 0.0):
            self.assertTrue(adapter.reconcile(self.session(bridge)))
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))
        self.assertIsNot(adapter._handles[SID], old)
        self.assertEqual(len([r for r in fake.requests if r['method'] == 'session/load']),
                         loads_before + 1)
        # The retired handle gets a closing session/close, off the swap path.
        self.assertTrue(self.wait_for(lambda: SID in fake.closed_sessions))
        self.assertTrue(self.view(bridge)['connected'])

    def test_reconcile_refuses_while_own_prompt_running(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        fake.prompt_updates = [('sleep', 30)]
        bridge.send(SID, 'work', str(uuid.uuid4()))
        handle = adapter._handles[SID]
        self.assertTrue(self.wait_for(lambda: handle.prompt_running))
        with patch.object(acp_adapter, 'RECONCILE_IDLE_GRACE', 0.0):
            self.assertFalse(adapter.reconcile(self.session(bridge), force=True))
        self.assertIs(adapter._handles[SID], handle)
        bridge.interrupt(SID)
        self.assertTrue(self.wait_for(lambda: not handle.prompt_running))

    def test_own_turn_settles_without_reconcile(self):
        fake, adapter, bridge = self.make_bridge()
        session = self.session(bridge)
        session.viewers = 1
        bridge._check_source_freshness(session, queued=False, active=False)
        self.assertFalse(session.reconciling)  # baseline seeded, nothing stale
        fake.prompt_updates = [chunk('agent_message_chunk', 'answer')]
        # Persistence lands while the turn streams; the settle sampler then
        # re-aligns the store cursor so our own turn reads as ours.
        for row in fake.list_rows:
            if row.get('sessionId') == SID:
                row['updatedAt'] = T2
        with patch.object(acp_adapter, 'RECONCILE_SETTLE_SAMPLE_DELAY', 0.05):
            bridge.send(SID, 'hello', str(uuid.uuid4()))
            self.assertTrue(self.wait_for(lambda: self.view(bridge)['status'] == 'idle'))
            deadline = time.monotonic() + 5
            while time.monotonic() < deadline and (adapter._handles[SID].view_synced_recency or 0) < T2:
                time.sleep(0.02)
        self.assertGreaterEqual(adapter._handles[SID].view_synced_recency or 0, T2)
        bridge._check_source_freshness(session, queued=False, active=False)
        self.assertFalse(self.wait_for(lambda: session.reconciling, timeout=0.3))
        loads = [r for r in fake.requests if r['method'] == 'session/load']
        self.assertEqual(len(loads), 1)  # attach replay only, no rebuild
        self.assertEqual(session.source_recency, T2)

    def test_reconcile_failure_keeps_old_view(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        self.external_turn(fake, '看完了。最终回复', T2)
        fake.errors['session/load'] = {'code': -32000, 'message': 'store unreadable'}
        old = adapter._handles[SID]
        with patch.object(acp_adapter, 'RECONCILE_IDLE_GRACE', 0.0):
            self.assertFalse(adapter.reconcile(self.session(bridge)))
        view = self.view(bridge)
        self.assertIn('hello', agent_texts(view))
        self.assertNotIn('看完了', agent_texts(view))
        self.assertTrue(view['connected'])
        self.assertIs(adapter._handles[SID], old)
        # Backoff holds the next automatic attempt off; force still recovers.
        self.assertGreater(adapter._handles[SID].reconcile_not_before, time.monotonic())
        fake.errors.pop('session/load')
        self.assertTrue(adapter.reconcile(self.session(bridge), force=True))
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))

    def test_retired_handle_events_never_republish(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        first_child = fake.children[0]
        self.external_turn(fake, '看完了。最终回复', T2)
        with patch.object(acp_adapter, 'RECONCILE_IDLE_GRACE', 0.0):
            self.assertTrue(adapter.reconcile(self.session(bridge)))
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))
        # A late update from the retired process folds into its private view
        # and must never reach the published projection.
        state = getattr(first_child, '_state', None)
        if state is not None:
            with fake._write_lock:
                try:
                    import json
                    os_write = __import__('os').write
                    message = {'jsonrpc': '2.0', 'method': 'session/update',
                               'params': {'sessionId': SID,
                                          'update': chunk('agent_message_chunk', 'GHOST')}}
                    os_write(state['out'], (json.dumps(message) + '\n').encode('utf-8'))
                except OSError:
                    pass  # already retired — equally acceptable
        time.sleep(0.2)
        view = self.view(bridge)
        self.assertNotIn('GHOST', agent_texts(view))
        self.assertIn('看完了。最终回复', agent_texts(view))

    def test_reconnect_button_forces_reconcile(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        self.external_turn(fake, '看完了。最终回复', T2)
        result = bridge.force_reconcile(SID)
        self.assertTrue(result['ok'])
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))

    def test_concurrent_force_reconciles_collapse(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        self.external_turn(fake, '看完了。最终回复', T2)
        spawned_before = len(fake.children)
        results = []
        def trigger():
            results.append(bridge.force_reconcile(SID))
        threads = [threading.Thread(target=trigger) for _ in range(3)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))
        self.wait_for(lambda: not self.session(bridge).reconciling)
        self.assertLessEqual(len(fake.children), spawned_before + 1)

    def test_maintain_freshness_check_triggers_reconcile(self):
        fake, adapter, bridge = self.make_bridge()
        session = self.session(bridge)
        session.viewers = 1
        bridge._check_source_freshness(session, queued=False, active=False)
        self.assertFalse(session.reconciling)
        self.external_turn(fake, '看完了。最终回复', T2)
        adapter.refresh_list()  # discovery forces the next list read
        with patch.object(acp_adapter, 'RECONCILE_IDLE_GRACE', 0.0):
            bridge._check_source_freshness(session, queued=False, active=False)
            self.assertTrue(self.wait_for(lambda: not session.reconciling))
        self.assertTrue(self.wait_for(lambda: '看完了。最终回复' in agent_texts(self.view(bridge))))
        self.assertEqual(session.source_recency, T2)

    def test_dispatch_backs_off_when_handle_swapped(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        old = adapter._handles[SID]
        from bridge.agents.acp_adapter import AcpHandle
        swapped = AcpHandle(SID, None)
        swapped.process = types.SimpleNamespace(capabilities={})
        real_attachments = adapter._attachment_blocks

        def race_mid_dispatch(handle_arg, files):
            # The rebuild lands between dispatch's map lookup and its lock.
            adapter._handles[SID] = swapped
            return real_attachments(handle_arg, files)
        try:
            with patch.object(adapter, '_attachment_blocks', side_effect=race_mid_dispatch):
                with self.assertRaises(TurnNotDispatched):
                    adapter.dispatch_turn(self.session(bridge), str(uuid.uuid4()),
                                          {'text': 'x', 'mode': 'send'}, [])
            self.assertFalse(old.prompt_running)
        finally:
            adapter._handles[SID] = old

    def test_empty_plan_update_never_opens_panel(self):
        view = AcpSessionView('s')
        view.on_update({'sessionUpdate': 'plan', 'entries': []})
        self.assertEqual(view.turns, [])  # attach-time baseline noise, not content
        view.on_update({'sessionUpdate': 'user_message_chunk', 'content': {'type': 'text', 'text': 'go'}})
        view.on_update({'sessionUpdate': 'plan', 'entries': [
            {'content': 'one', 'status': 'completed'}, {'content': 'two', 'status': 'pending'}]})
        plan = [m for t in view.turns for m in t['messages'] if m.get('kind') == 'plan']
        self.assertEqual(len(plan), 1)
        self.assertEqual(len(plan[0]['entries']), 2)
        view.on_update({'sessionUpdate': 'plan', 'entries': []})  # agent cleared the plan
        plan = [m for t in view.turns for m in t['messages'] if m.get('kind') == 'plan']
        self.assertEqual(plan, [])

    def test_empty_plan_in_replay_creates_no_row(self):
        _, _, bridge = self.make_bridge(load_updates=[
            chunk('user_message_chunk', 'hi'),
            chunk('agent_message_chunk', 'hello'),
            {'sessionUpdate': 'plan', 'entries': []},
        ])
        view = self.view(bridge)
        kinds = [m.get('kind') for t in view['turns'] for m in t['messages']]
        self.assertNotIn('plan', kinds)


if __name__ == '__main__':
    unittest.main()
