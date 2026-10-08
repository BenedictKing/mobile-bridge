"""MCode adapter: steer/goal extension methods behind the real Bridge stack."""
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents.acp import AcpUnavailable
from bridge.agents.mcode import MCodeAdapter
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = 'mvs_fake000000000000000000000001'
UPDATED = '2026-01-02T03:04:05Z'

EXTENSIONS = ['mcode/session/steer', 'mcode/session/queue/list', 'mcode/session/queue/enqueue',
              'mcode/session/queue/update', 'mcode/session/queue/delete',
              'mcode/session/goal/get', 'mcode/session/goal/create',
              'mcode/session/goal/patch', 'mcode/session/goal/clear']

MCODE_CAPABILITIES = {
    'loadSession': True,
    'sessionCapabilities': {'list': {}, 'fork': {}, 'resume': {}, 'close': {}},
    'promptCapabilities': {'image': False, 'audio': False, 'embeddedContext': False},
}
MCODE_CONFIG = [
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'm:minimax:MiniMax-M3:v:thinking',
     'options': [{'value': 'm:minimax:MiniMax-M3:v:', 'name': 'M3'},
                 {'value': 'm:minimax:MiniMax-M3:v:thinking', 'name': 'M3 · thinking'}]},
    {'id': 'thinkingEffort', 'name': 'Thinking effort', 'type': 'select',
     'category': 'thought_level', 'currentValue': 'max',
     'options': [{'value': v, 'name': v.title()}
                 for v in ('default', 'low', 'medium', 'high', 'xhigh', 'max')]},
]
MCODE_MODES = {'currentModeId': 'default',
               'availableModes': [{'id': 'default', 'name': 'Default'},
                                  {'id': 'plan', 'name': 'Plan'}]}


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


class MCodeAdapterTests(unittest.TestCase):
    def make_bridge(self, fake=None):
        fake = fake or FakeAcpAgent(
            capabilities=dict(MCODE_CAPABILITIES),
            config_options=[dict(o, options=list(o['options'])) for o in MCODE_CONFIG],
            modes=dict(MCODE_MODES), extensions=list(EXTENSIONS),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'MCode', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = MCodeAdapter('fake-mcode', data_dir=Path(tmp.name) / 'data',
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

    def test_view_advertises_steer_and_goal_capabilities(self):
        _, _, bridge = self.make_bridge()
        view = bridge.view(SID)
        self.assertEqual(view['agent'], 'mcode')
        self.assertEqual(sorted(view['capabilities']), ['goals', 'steer'])
        self.assertEqual(view['goalTransport'], 'acp')
        self.assertTrue(view['goalRuntimeAvailable'])
        self.assertEqual(view['model'], 'm:minimax:MiniMax-M3:v:thinking')
        self.assertEqual(view['effort'], 'max')

    def test_steer_delivers_mid_turn(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'working'), ('await_cancel',)]
        bridge.view(SID)
        bridge.send(SID, 'count slowly', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'active'))
        result = bridge.send(SID, 'stop early', str(uuid.uuid4()), 'steer')
        self.assertEqual(result['status'], 'accepted')
        self.assertEqual(result['result'], {'accepted': True, 'steered': True})
        self.assertEqual(fake.steer_texts, ['stop early'])
        bridge.interrupt(SID)
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))

    def test_steer_rejected_when_idle(self):
        _, _, bridge = self.make_bridge()
        bridge.view(SID)
        with self.assertRaises(ValueError):
            bridge.send(SID, 'steer text', str(uuid.uuid4()), 'steer')

    def test_attachments_rejected_by_capability(self):
        _, adapter, bridge = self.make_bridge()
        session = bridge.session(SID)
        entry = {'text': 'with file', 'mode': 'send'}
        with self.assertRaises(ValueError) as ctx:
            adapter.dispatch_turn(session, 'sub-1', entry,
                                  [{'name': 'a.png', 'path': '/tmp/a.png', 'image': True}])
        self.assertIn('图片附件', str(ctx.exception))
        with self.assertRaises(ValueError) as ctx:
            adapter.dispatch_turn(session, 'sub-2', entry,
                                  [{'name': 'a.txt', 'path': '/tmp/a.txt', 'image': False}])
        self.assertIn('文件附件', str(ctx.exception))

    def test_goal_lifecycle_without_activation_turn(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        result = bridge.send(SID, '盯住这个目标', str(uuid.uuid4()), work_mode='goal')
        self.assertEqual(result['status'], 'accepted')
        self.assertTrue(result['confirmed'])
        self.assertNotIn('activation', result)
        self.assertEqual(fake.goal['objective'], '盯住这个目标')
        self.assertEqual(fake.goal['status'], 'active')
        # create must not produce a prompt turn: mcode kicks the goal off itself.
        self.assertEqual([r for r in fake.requests if r['method'] == 'session/prompt'], [])
        view = bridge.view(SID)
        self.assertEqual(view['goal']['objective'], '盯住这个目标')
        self.assertEqual(view['goal']['status'], 'active')

        paused = bridge.set_goal_status(SID, 'paused', str(uuid.uuid4()))
        self.assertTrue(paused['confirmed'])
        self.assertEqual(fake.goal['status'], 'paused')
        edited = bridge.edit_goal(SID, '编辑后的目标', str(uuid.uuid4()))
        self.assertTrue(edited['confirmed'])
        self.assertEqual(fake.goal['objective'], '编辑后的目标')
        resumed = bridge.set_goal_status(SID, 'active', str(uuid.uuid4()))
        self.assertTrue(resumed['confirmed'])
        self.assertNotIn('activation', resumed)
        self.assertEqual(fake.goal['status'], 'active')
        cancelled = bridge.cancel_goal(SID, str(uuid.uuid4()))
        self.assertTrue(cancelled['confirmed'])
        self.assertIsNone(fake.goal)
        self.assertIsNone(bridge.view(SID)['goal'])

    def test_settings_map_thinking_effort_select(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        result = bridge.settings(SID, 'm:minimax:MiniMax-M3:v:', 'medium')
        self.assertTrue(result['confirmed'])
        self.assertEqual(fake.config_values,
                         {'model': 'm:minimax:MiniMax-M3:v:', 'thinkingEffort': 'medium'})
        self.assertEqual(bridge.view(SID)['effort'], 'medium')
        bridge.settings(SID, 'm:minimax:MiniMax-M3:v:', 'ultra')
        self.assertEqual(fake.config_values['thinkingEffort'], 'max')
        with self.assertRaises(ValueError):
            bridge.settings(SID, 'm:other:model', 'high')

    def test_settings_partial_config_echo_drops_thinking(self):
        # mcode 0.5.5 removes thought_level from the echoed configOptions for
        # models without reasoning-effort support; the response is authoritative.
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        fake.config_echo_all = False
        with self.assertRaises(ValueError) as ctx:
            bridge.settings(SID, 'm:minimax:MiniMax-M3:v:', 'high')
        self.assertIn('推理强度', str(ctx.exception))
        # The model switch itself was applied before the effort rejection.
        self.assertEqual(fake.config_values['model'], 'm:minimax:MiniMax-M3:v:')
        self.assertEqual(bridge.view(SID)['model'], 'm:minimax:MiniMax-M3:v:')
        # medium is the tolerated no-op effort when thought_level is absent.
        result = bridge.settings(SID, 'm:minimax:MiniMax-M3:v:', 'medium')
        self.assertTrue(result['confirmed'])

    def test_mcode_adapter_requires_executable(self):
        with patch('bridge.agents.mcode.discover_executable', return_value=None):
            with self.assertRaises(AcpUnavailable) as ctx:
                MCodeAdapter()
        self.assertEqual(str(ctx.exception), '未找到 mcode 命令，请先安装 MiniMax Code')

    def test_run_agents_accepts_mcode(self):
        import run
        from unittest.mock import MagicMock
        server = MagicMock()
        server.serve_forever.side_effect = KeyboardInterrupt
        adapters = []

        def fake_adapter_cls(name):
            def build():
                adapter = MagicMock()
                adapter.kind = name
                adapters.append(name)
                return adapter
            return build

        with tempfile.TemporaryDirectory(dir=ROOT / '.tmp') as directory:
            config = Path(directory) / 'config.json'
            config.write_text('{"auth": {"mode": "none"}, "origins": []}', encoding='utf-8')
            with patch.object(run.sys, 'argv',
                              ['run.py', '--config', str(config), '--agents', 'codex,mcode']), \
                 patch.object(run.sys, 'stdout', MagicMock()), \
                 patch.object(run.sys, 'stderr', MagicMock()), \
                 patch.object(run, 'Bridge'), patch.object(run, 'Notifications'), \
                 patch.object(run, 'GatewayControl'), patch.object(run.signal, 'signal'), \
                 patch.object(run, 'AddressNotifications'), \
                 patch.object(run, 'GatewayServer', return_value=server) as make_server, \
                 patch('bridge.agents.mcode.MCodeAdapter', fake_adapter_cls('mcode')):
                run.main()
        self.assertEqual(adapters, ['mcode'])
        registry = make_server.call_args.kwargs.get('registry')
        self.assertIsNotNone(registry)


if __name__ == '__main__':
    unittest.main()
