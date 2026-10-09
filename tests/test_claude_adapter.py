"""Claude ACP profile: wrapper discovery, argv shape, and claude-flavored updates."""
import os
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents.acp import AcpUnavailable
from bridge.agents.claude import ClaudeAdapter, discover_argv
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = 'claude-fake-0001'
UPDATED = '2026-01-02T03:04:05Z'

# Shapes captured from @agentclientprotocol/claude-agent-acp 0.87.0.
CONFIG_OPTIONS = [
    {'id': 'mode', 'name': 'Mode', 'type': 'select', 'category': 'mode',
     'currentValue': 'auto',
     'options': [{'value': 'default', 'name': 'Manual'}, {'value': 'auto', 'name': 'Auto'}]},
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'claude-opus-4-8[1m]',
     'options': [{'value': 'default', 'name': 'Default (recommended)'},
                 {'value': 'opus', 'name': 'Opus 5.5'},
                 {'value': 'sonnet', 'name': 'Sonnet 5.5'},
                 {'value': 'claude-opus-4-8[1m]', 'name': 'Opus 4.8 (1M context)'}]},
    {'id': 'effort', 'name': 'Effort', 'type': 'select', 'category': 'thought_level',
     'currentValue': 'xhigh',
     'options': [{'value': v, 'name': v.title()}
                 for v in ('default', 'low', 'medium', 'high', 'xhigh', 'max')]},
]
MODES = {'currentModeId': 'auto',
         'availableModes': [{'id': 'default', 'name': 'Manual'}, {'id': 'auto', 'name': 'Auto'}]}


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        patcher = patch.dict(os.environ)
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop('CLAUDE_ACP_BIN', None)

    def test_env_override_splits_like_a_shell(self):
        os.environ['CLAUDE_ACP_BIN'] = '"/opt/claude acp/claude-agent-acp" --verbose'
        self.assertEqual(discover_argv(),
                         ['/opt/claude acp/claude-agent-acp', '--verbose'])

    def test_path_binary_wins_over_npx(self):
        with patch('bridge.agents.claude.shutil.which',
                   side_effect=lambda name: {'claude-agent-acp': '/usr/local/bin/claude-agent-acp'}.get(name)):
            self.assertEqual(discover_argv(), ['/usr/local/bin/claude-agent-acp'])

    def test_npx_fallback(self):
        with patch('bridge.agents.claude.shutil.which',
                   side_effect=lambda name: '/usr/local/bin/npx' if name == 'npx' else None):
            self.assertEqual(discover_argv(), ['npx', '-y', '@agentclientprotocol/claude-agent-acp'])

    def test_nothing_found(self):
        with patch('bridge.agents.claude.shutil.which', return_value=None):
            self.assertIsNone(discover_argv())
            with self.assertRaises(AcpUnavailable):
                ClaudeAdapter()


class ProfileTests(unittest.TestCase):
    def test_profile_constants(self):
        adapter = ClaudeAdapter(['fake-claude-acp'])
        self.assertEqual(adapter.kind, 'claude')
        self.assertEqual(adapter.label, 'Claude')
        self.assertEqual(adapter.capabilities, frozenset())
        self.assertEqual(adapter.INITIALIZE_TIMEOUT, 180)
        self.assertIn('claude', adapter.auth_hint)
        self.assertEqual(adapter.EFFORT_MAP['none'], 'default')
        self.assertEqual(adapter.EFFORT_MAP['ultra'], 'max')

    def test_string_argv_and_full_argv(self):
        adapter = ClaudeAdapter('fake-claude-acp')
        self.assertEqual(adapter.executable, 'fake-claude-acp')
        self.assertEqual(adapter._argv(), ['fake-claude-acp'])
        adapter = ClaudeAdapter(['npx', '-y', '@agentclientprotocol/claude-agent-acp'])
        self.assertEqual(adapter.executable, 'npx')
        self.assertEqual(adapter._argv(), ['npx', '-y', '@agentclientprotocol/claude-agent-acp'])


class ClaudeBridgeTests(unittest.TestCase):
    def make_bridge(self, fake=None):
        fake = fake or FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = ClaudeAdapter(['fake-claude-acp'], data_dir=Path(tmp.name) / 'data',
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
        self.assertEqual(fake.spawned_argv, ['fake-claude-acp'])

    def test_list_decorates_rows_with_agent(self):
        _, _, bridge = self.make_bridge()
        rows = bridge.list()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]['agent'], 'claude')
        self.assertEqual(rows[0]['capabilities'], [])

    def test_attach_seeds_model_effort_and_mode(self):
        _, _, bridge = self.make_bridge()
        view = bridge.view(SID)
        self.assertTrue(view['connected'])
        self.assertEqual(view['agent'], 'claude')
        self.assertEqual(view['model'], 'claude-opus-4-8[1m]')
        self.assertEqual(view['effort'], 'xhigh')
        self.assertEqual(view['collaborationMode'], 'auto')

    def test_update_settings_maps_effort_values(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        result = bridge.settings(SID, 'sonnet', 'none')
        self.assertEqual(result['applied'], True)
        self.assertEqual(fake.config_values.get('model'), 'sonnet')
        self.assertEqual(fake.config_values.get('effort'), 'default')
        # Bracketed model ids (claude-opus-4-8[1m]) pass bridge validation.
        bridge.settings(SID, 'claude-opus-4-8[1m]', 'ultra')
        self.assertEqual(fake.config_values.get('model'), 'claude-opus-4-8[1m]')
        self.assertEqual(fake.config_values.get('effort'), 'max')

    def test_catalog_exposes_models_and_efforts(self):
        _, _, bridge = self.make_bridge()
        bridge.view(SID)
        catalog = bridge.catalog(SID, refresh=True)
        values = {m['id'] for m in catalog['models']}
        self.assertIn('opus', values)
        self.assertIn('sonnet', values)
        efforts = next(m['efforts'] for m in catalog['models'] if m['id'] == 'opus')
        self.assertEqual(efforts, ['none', 'low', 'medium', 'high', 'xhigh', 'max'])
        self.assertEqual(catalog['currentModel'], 'claude-opus-4-8[1m]')
        self.assertEqual(catalog['currentEffort'], 'xhigh')
        self.assertFalse(catalog['fastMode']['allowed'])

    def test_placeholder_tool_call_filled_by_updates(self):
        # claude-agent-acp opens tool calls with empty rawInput and a
        # placeholder title, then streams the real title/input as updates.
        fake = FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        fake.load_updates = [
            {'sessionUpdate': 'tool_call', 'toolCallId': 'toolu_1', 'title': 'Preparing file…',
             'kind': 'edit', 'status': 'pending', 'rawInput': {}, 'content': []},
            {'sessionUpdate': 'tool_call_update', 'toolCallId': 'toolu_1',
             'rawInput': {'file_path': '/tmp/probe.txt', 'content': 'hello'},
             'title': 'Write /tmp/probe.txt'},
            {'sessionUpdate': 'tool_call_update', 'toolCallId': 'toolu_1', 'status': 'completed'},
        ]
        _, _, bridge = self.make_bridge(fake)
        view = bridge.view(SID)
        tool = view['turns'][0]['messages'][0]
        self.assertEqual(tool['kind'], 'toolCall')
        self.assertEqual(tool['title'], 'Write /tmp/probe.txt')
        self.assertEqual(tool['text'], '/tmp/probe.txt')
        self.assertEqual(tool['status'], 'completed')

    def test_plural_config_option_update_applies(self):
        # claude-agent-acp notifies config changes as a full configOptions list.
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [
            {'sessionUpdate': 'config_option_update', 'configOptions': [
                dict(o, options=list(o['options']), currentValue='default')
                if o['id'] == 'mode' else dict(o, options=list(o['options']))
                for o in CONFIG_OPTIONS]},
            chunk('agent_message_chunk', 'done'),
        ]
        bridge.view(SID)
        bridge.send(SID, 'hello', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'default')

    def test_created_session_keeps_process_until_listed(self):
        # claude-agent-acp never lists a session before its first message, and
        # no other process can load or prompt it; creation keeps its process.
        fake, adapter, bridge = self.make_bridge()
        thread_id = adapter.create_session({'cwd': '/work'}, 'New chat')
        fake.list_rows = [r for r in fake.list_rows if r['sessionId'] != thread_id]
        rows = adapter.list_sessions('此电脑')
        self.assertIn(thread_id, {r['id'] for r in rows})
        self.assertEqual(adapter.session_meta(thread_id)['title'], 'New chat')
        # attach 复用创建进程：不再 spawn，也不发 session/load
        view = bridge.view(thread_id)
        self.assertTrue(view['connected'])
        self.assertEqual(view['model'], 'claude-opus-4-8[1m]')
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
