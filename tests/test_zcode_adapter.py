"""ZCode ACP profile: wrapper discovery, argv shape, mode and effort mapping."""
import os
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents.acp import AcpUnavailable
from bridge.agents.zcode import ZcodeAdapter, discover_argv
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = '11111111-2222-3333-4444-555555555555'
UPDATED = '2026-01-02T03:04:05Z'

# Shapes captured live from zcode-acp-server 0.67.0 (npm): model values are
# providerId\modelId, the thought option's id is 'thought' with the standard
# thought_level category, and GLM-5.3 offers low/high/max.
CONFIG_OPTIONS = [
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'builtin:bigmodel-coding-plan\\GLM-5.3',
     'options': [{'value': 'builtin:bigmodel-coding-plan\\GLM-5.3', 'name': 'GLM-5.3'},
                 {'value': 'builtin:bigmodel-coding-plan\\GLM-5-Turbo', 'name': 'GLM-5-Turbo'}]},
    {'id': 'mode', 'name': 'Mode', 'type': 'select', 'category': 'mode',
     'currentValue': 'yolo',
     'options': [{'value': m, 'name': m.title()}
                 for m in ('plan', 'build', 'edit', 'yolo', 'auto')]},
    {'id': 'thought', 'name': 'Thought', 'type': 'select', 'category': 'thought_level',
     'currentValue': 'max',
     'options': [{'value': v, 'name': v} for v in ('low', 'high', 'max')]},
]
TURBO_THOUGHT = [
    {'id': 'thought', 'name': 'Thought', 'type': 'select', 'category': 'thought_level',
     'currentValue': 'enabled',
     'options': [{'value': v, 'name': v} for v in ('enabled', 'off')]},
]
MODES = {'currentModeId': 'yolo',
         'availableModes': [{'id': m, 'name': m.title()}
                            for m in ('plan', 'build', 'edit', 'yolo', 'auto')]}


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


def copy_options(thought=None):
    options = []
    for option in CONFIG_OPTIONS:
        clone = dict(option, options=list(option['options']))
        if thought is not None and option['id'] == 'thought':
            clone = dict(thought[0], options=list(thought[0]['options']))
        options.append(clone)
    return options


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        patcher = patch.dict(os.environ)
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop('ZCODE_ACP_BIN', None)

    def test_env_override_splits_like_a_shell(self):
        os.environ['ZCODE_ACP_BIN'] = '"/opt/zcode acp/zcode-acp-server" --verbose'
        self.assertEqual(discover_argv(),
                         ['/opt/zcode acp/zcode-acp-server', '--verbose'])

    def test_path_binaries_preferred_in_order(self):
        with patch('bridge.agents.zcode.shutil.which',
                   side_effect=lambda name: {'zcode-acp-server': '/usr/local/bin/zcode-acp-server',
                                             'zcode-acp': '/usr/local/bin/zcode-acp'}.get(name)):
            self.assertEqual(discover_argv(), ['/usr/local/bin/zcode-acp-server'])

    def test_unified_cli_bin_is_second_choice(self):
        with patch('bridge.agents.zcode.shutil.which',
                   side_effect=lambda name: '/usr/local/bin/zcode-acp' if name == 'zcode-acp' else None):
            self.assertEqual(discover_argv(), ['/usr/local/bin/zcode-acp'])

    def test_npx_fallback(self):
        with patch('bridge.agents.zcode.shutil.which',
                   side_effect=lambda name: '/usr/local/bin/npx' if name == 'npx' else None):
            self.assertEqual(discover_argv(), ['npx', '-y', 'zcode-acp-server'])

    def test_nothing_found(self):
        with patch('bridge.agents.zcode.shutil.which', return_value=None):
            self.assertIsNone(discover_argv())
            with self.assertRaises(AcpUnavailable):
                ZcodeAdapter()


class ProfileTests(unittest.TestCase):
    def test_profile_constants(self):
        adapter = ZcodeAdapter(['fake-zcode-acp'])
        self.assertEqual(adapter.kind, 'zcode')
        self.assertEqual(adapter.label, 'ZCode')
        self.assertEqual(adapter.capabilities, frozenset())
        self.assertEqual(adapter.INITIALIZE_TIMEOUT, 180)
        self.assertTrue(adapter.KEEP_CREATED_PROCESS)
        self.assertIn('ZCode', adapter.auth_hint)
        self.assertEqual(adapter.EFFORT_MAP['none'], 'off')
        self.assertEqual(adapter.EFFORT_MAP['minimal'], 'enabled')
        self.assertEqual(adapter.EFFORT_MAP['ultra'], 'ultra')

    def test_string_argv_and_full_argv(self):
        adapter = ZcodeAdapter('fake-zcode-acp')
        self.assertEqual(adapter.executable, 'fake-zcode-acp')
        self.assertEqual(adapter._argv(), ['fake-zcode-acp'])
        adapter = ZcodeAdapter(['npx', '-y', 'zcode-acp-server'])
        self.assertEqual(adapter.executable, 'npx')
        self.assertEqual(adapter._argv(), ['npx', '-y', 'zcode-acp-server'])


class ZcodeBridgeTests(unittest.TestCase):
    def make_bridge(self, fake=None, thought=None):
        fake = fake or FakeAcpAgent(
            config_options=copy_options(thought),
            modes=dict(MODES, availableModes=list(MODES['availableModes'])),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = ZcodeAdapter(['fake-zcode-acp'], data_dir=Path(tmp.name) / 'data',
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

    def test_spawn_uses_full_argv_without_acp_subcommand(self):
        fake, _, bridge = self.make_bridge()
        bridge.list()
        self.assertEqual(fake.spawned_argv, ['fake-zcode-acp'])

    def test_list_decorates_rows_with_agent(self):
        _, _, bridge = self.make_bridge()
        rows = bridge.list()
        self.assertEqual(rows[0]['agent'], 'zcode')
        self.assertEqual(rows[0]['capabilities'], [])

    def test_attach_seeds_model_effort_and_mode(self):
        _, _, bridge = self.make_bridge()
        view = bridge.view(SID)
        self.assertTrue(view['connected'])
        self.assertEqual(view['agent'], 'zcode')
        self.assertEqual(view['model'], 'builtin:bigmodel-coding-plan\\GLM-5.3')
        self.assertEqual(view['effort'], 'max')
        self.assertEqual(view['collaborationMode'], 'yolo')

    def test_update_settings_maps_effort_values(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        result = bridge.settings(SID, 'builtin:bigmodel-coding-plan\\GLM-5-Turbo', 'low')
        self.assertEqual(result['applied'], True)
        self.assertEqual(fake.config_values.get('model'), 'builtin:bigmodel-coding-plan\\GLM-5-Turbo')
        # The thought option id is 'thought'; the value passes through identity.
        self.assertEqual(fake.config_values.get('thought'), 'low')
        # 'none' maps to 'off', which GLM-5.3 does not offer.
        with self.assertRaises(ValueError):
            bridge.settings(SID, 'builtin:bigmodel-coding-plan\\GLM-5.3', 'none')

    def test_turbo_variant_efforts_pass_through(self):
        # GLM-5-Turbo's binary enabled/off vocabulary folds onto the bridge's
        # minimal/none levels; both directions stay within the 8-level ladder.
        fake, _, bridge = self.make_bridge(thought=TURBO_THOUGHT)
        bridge.view(SID)
        catalog = bridge.catalog(SID, refresh=True)
        efforts = next(m['efforts'] for m in catalog['models'] if m['id'] == 'builtin:bigmodel-coding-plan\\GLM-5.3')
        self.assertEqual(efforts, ['minimal', 'none'])
        # The fixture starts at 'enabled', so the first toggle lands on 'off'.
        bridge.settings(SID, 'builtin:bigmodel-coding-plan\\GLM-5.3', 'none')
        self.assertEqual(fake.config_values.get('thought'), 'off')
        bridge.settings(SID, 'builtin:bigmodel-coding-plan\\GLM-5.3', 'minimal')
        self.assertEqual(fake.config_values.get('thought'), 'enabled')

    def test_catalog_exposes_models_and_efforts(self):
        _, _, bridge = self.make_bridge()
        bridge.view(SID)
        catalog = bridge.catalog(SID, refresh=True)
        values = {m['id'] for m in catalog['models']}
        self.assertEqual(values, {'builtin:bigmodel-coding-plan\\GLM-5.3', 'builtin:bigmodel-coding-plan\\GLM-5-Turbo'})
        efforts = next(m['efforts'] for m in catalog['models'] if m['id'] == 'builtin:bigmodel-coding-plan\\GLM-5.3')
        self.assertEqual(efforts, ['low', 'high', 'max'])
        self.assertEqual(catalog['currentModel'], 'builtin:bigmodel-coding-plan\\GLM-5.3')
        self.assertEqual(catalog['currentEffort'], 'max')

    def test_work_mode_maps_onto_zcode_session_modes(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        # Already yolo: a plain default send stays a no-op.
        bridge.send(SID, 'hi', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(fake.mode_values, [])
        # Plan toggles zcode's plan mode; default returns to the create-time yolo.
        bridge.send(SID, 'plan it', str(uuid.uuid4()), work_mode='plan')
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(fake.mode_values, ['plan'])
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'plan')
        bridge.send(SID, 'do it', str(uuid.uuid4()), work_mode='default')
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(fake.mode_values, ['plan', 'yolo'])
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'yolo')

    def test_created_session_keeps_process_until_listed(self):
        # zcode-acp's session/new placeholder has no backend session until the
        # first prompt, so it never appears in session/list before that; the
        # create process stays alive as the session handle.
        fake, adapter, bridge = self.make_bridge()
        thread_id = adapter.create_session({'cwd': '/work'}, 'New chat')
        fake.list_rows = [r for r in fake.list_rows if r['sessionId'] != thread_id]
        rows = adapter.list_sessions('此电脑')
        self.assertIn(thread_id, {r['id'] for r in rows})
        self.assertEqual(adapter.session_meta(thread_id)['title'], 'New chat')
        # attach 复用创建进程：不再 spawn，也不发 session/load
        view = bridge.view(thread_id)
        self.assertTrue(view['connected'])
        self.assertEqual(view['model'], 'builtin:bigmodel-coding-plan\\GLM-5.3')
        self.assertEqual(len(fake.children), 2)  # create + list fetch
        self.assertEqual([r for r in fake.requests if r['method'] == 'session/load'], [])
        # 首发消息在创建进程内完成；agent 入列后 pending 占位被清除
        fake.prompt_updates = [chunk('agent_message_chunk', 'ok')]
        result = bridge.send(thread_id, 'hello', str(uuid.uuid4()))
        self.assertEqual(result['status'], 'accepted')
        self.assertTrue(self.wait_for(lambda: bridge.view(thread_id)['status'] == 'idle'))
        self.assertEqual(bridge.view(thread_id)['turns'][-1]['messages'][-1]['text'], 'ok')
        fake.list_rows.append({'sessionId': thread_id, 'cwd': '/work',
                               'title': 'New chat', 'updatedAt': UPDATED})
        adapter._list_at = 0
        rows = adapter.list_sessions('此电脑')
        self.assertEqual([r['id'] for r in rows].count(thread_id), 1)
        self.assertNotIn(thread_id, adapter._pending_created)

    def test_detach_unlisted_session_drops_placeholder(self):
        # 从未发消息的创建会话在 detach 后不可恢复，占位应从列表消失。
        fake, adapter, bridge = self.make_bridge()
        thread_id = adapter.create_session({'cwd': '/work'}, 'New chat')
        fake.list_rows = [r for r in fake.list_rows if r['sessionId'] != thread_id]
        session = bridge.session(thread_id)
        adapter.detach(session)
        adapter._list_at = 0
        self.assertNotIn(thread_id, {r['id'] for r in adapter.list_sessions('此电脑')})

    def test_send_and_cancel_roundtrip(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [('await_cancel',)]
        bridge.view(SID)
        bridge.send(SID, 'count', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'active'))
        bridge.interrupt(SID)
        self.assertTrue(self.wait_for(lambda: fake.cancelled))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(bridge.view(SID)['turns'][-1]['status'], 'interrupted')


if __name__ == '__main__':
    unittest.main()
