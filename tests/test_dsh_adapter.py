"""DeepSeek Harness (dsh) ACP profile: discovery, steer, and collaboration_mode mapping."""
import os
import tempfile
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents.acp import AcpUnavailable
from bridge.agents.dsh import DshAdapter, discover_argv
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = 'dsh-fake-0001'
UPDATED = '2026-01-02T03:04:05Z'

# Shapes captured from @openma/deepseek-harness-acp 0.4.37: modes and the
# 'mode' config option are permission presets; plan lives in collaboration_mode.
CONFIG_OPTIONS = [
    {'id': 'mode', 'name': 'Permission mode', 'type': 'select', 'category': 'mode',
     'currentValue': 'workspace-write',
     'options': [{'value': 'read-only', 'name': 'Read-only'},
                 {'value': 'workspace-write', 'name': 'Workspace write'},
                 {'value': 'danger-full-access', 'name': 'Danger full access'}]},
    {'id': 'collaboration_mode', 'name': 'Collaboration mode', 'type': 'select',
     'currentValue': 'default',
     'options': [{'value': 'default', 'name': 'Default'},
                 {'value': 'plan', 'name': 'Plan'}]},
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'deepseek-official::deepseek-v4-pro',
     'options': [{'value': 'deepseek-official::deepseek-flash', 'name': 'deepseek-flash'},
                 {'value': 'deepseek-official::deepseek-v4-pro', 'name': 'deepseek-v4-pro'}]},
    {'id': 'effort', 'name': 'Effort', 'type': 'select', 'category': 'thought_level',
     'currentValue': 'high',
     'options': [{'value': v, 'name': v.title()} for v in ('off', 'low', 'high', 'max')]},
]
MODES = {'currentModeId': 'workspace-write',
         'availableModes': [{'id': 'read-only', 'name': 'Read-only'},
                            {'id': 'workspace-write', 'name': 'Workspace write'},
                            {'id': 'danger-full-access', 'name': 'Danger full access'}]}


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


class DiscoveryTests(unittest.TestCase):
    def setUp(self):
        patcher = patch.dict(os.environ)
        patcher.start()
        self.addCleanup(patcher.stop)
        os.environ.pop('DSH_ACP_BIN', None)

    def test_env_override_splits_like_a_shell(self):
        os.environ['DSH_ACP_BIN'] = '"/opt/deepseek harness/dsh-acp" --verbose'
        self.assertEqual(discover_argv(),
                         ['/opt/deepseek harness/dsh-acp', '--verbose'])

    def test_path_binary_wins_over_npx(self):
        with patch('bridge.agents.dsh.shutil.which',
                   side_effect=lambda name: {'dsh-acp': '/usr/local/bin/dsh-acp'}.get(name)):
            self.assertEqual(discover_argv(), ['/usr/local/bin/dsh-acp'])

    def test_npx_fallback(self):
        with patch('bridge.agents.dsh.shutil.which',
                   side_effect=lambda name: '/usr/local/bin/npx' if name == 'npx' else None):
            self.assertEqual(discover_argv(), ['npx', '-y', '@openma/deepseek-harness-acp'])

    def test_nothing_found(self):
        with patch('bridge.agents.dsh.shutil.which', return_value=None):
            self.assertIsNone(discover_argv())
            with self.assertRaises(AcpUnavailable):
                DshAdapter()


class ProfileTests(unittest.TestCase):
    def test_profile_constants(self):
        adapter = DshAdapter(['fake-dsh-acp'])
        self.assertEqual(adapter.kind, 'dsh')
        self.assertEqual(adapter.label, 'DeepSeek')
        self.assertEqual(sorted(adapter.capabilities), ['steer'])
        self.assertEqual(adapter.INITIALIZE_TIMEOUT, 180)
        self.assertFalse(adapter.KEEP_CREATED_PROCESS)
        self.assertIn('dsh-acp', adapter.auth_hint)
        self.assertEqual(adapter.EFFORT_MAP['none'], 'off')
        self.assertEqual(adapter.EFFORT_MAP['ultra'], 'max')

    def test_string_argv_and_full_argv(self):
        adapter = DshAdapter('fake-dsh-acp')
        self.assertEqual(adapter.executable, 'fake-dsh-acp')
        self.assertEqual(adapter._argv(), ['fake-dsh-acp'])
        adapter = DshAdapter(['npx', '-y', '@openma/deepseek-harness-acp'])
        self.assertEqual(adapter.executable, 'npx')
        self.assertEqual(adapter._argv(), ['npx', '-y', '@openma/deepseek-harness-acp'])


class DshBridgeTests(unittest.TestCase):
    def make_bridge(self, fake=None, config_options=None):
        fake = fake or FakeAcpAgent(
            config_options=[dict(o, options=list(o['options']))
                            for o in (config_options or CONFIG_OPTIONS)],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = DshAdapter(['fake-dsh-acp'], data_dir=Path(tmp.name) / 'data',
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
        self.assertEqual(fake.spawned_argv, ['fake-dsh-acp'])

    def test_list_decorates_rows_with_agent(self):
        _, _, bridge = self.make_bridge()
        rows = bridge.list()
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]['agent'], 'dsh')
        self.assertEqual(rows[0]['capabilities'], ['steer'])

    def test_attach_seeds_model_effort_and_collaboration_mode(self):
        # 权限预设不得进入 collaborationMode；视图只反映 collaboration_mode。
        _, _, bridge = self.make_bridge()
        view = bridge.view(SID)
        self.assertTrue(view['connected'])
        self.assertEqual(view['agent'], 'dsh')
        self.assertEqual(view['model'], 'deepseek-official::deepseek-v4-pro')
        self.assertEqual(view['effort'], 'high')
        self.assertEqual(view['collaborationMode'], 'default')

    def test_current_mode_update_is_permission_preset_not_collaboration(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [
            {'sessionUpdate': 'current_mode_update', 'currentModeId': 'read-only'},
            chunk('agent_message_chunk', 'done'),
        ]
        bridge.view(SID)
        bridge.send(SID, 'hello', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'default')

    def test_update_settings_maps_effort_values(self):
        fake, _, bridge = self.make_bridge()
        bridge.view(SID)
        result = bridge.settings(SID, 'deepseek-official::deepseek-flash', 'none')
        self.assertEqual(result['applied'], True)
        self.assertEqual(fake.config_values.get('model'), 'deepseek-official::deepseek-flash')
        self.assertEqual(fake.config_values.get('effort'), 'off')
        bridge.settings(SID, 'deepseek-official::deepseek-flash', 'ultra')
        self.assertEqual(fake.config_values.get('effort'), 'max')

    def test_catalog_exposes_models_and_efforts(self):
        _, _, bridge = self.make_bridge()
        bridge.view(SID)
        catalog = bridge.catalog(SID, refresh=True)
        values = {m['id'] for m in catalog['models']}
        self.assertIn('deepseek-official::deepseek-flash', values)
        efforts = next(m['efforts'] for m in catalog['models']
                       if m['id'] == 'deepseek-official::deepseek-flash')
        self.assertEqual(efforts, ['none', 'low', 'high', 'max'])
        self.assertEqual(catalog['currentModel'], 'deepseek-official::deepseek-v4-pro')
        self.assertEqual(catalog['currentEffort'], 'high')
        self.assertFalse(catalog['fastMode']['allowed'])

    def test_work_mode_maps_to_collaboration_mode_not_set_mode(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'ok')]
        bridge.view(SID)
        result = bridge.send(SID, '先做个计划', str(uuid.uuid4()), work_mode='plan')
        self.assertEqual(result['status'], 'accepted')
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(fake.config_values.get('collaboration_mode'), 'plan')
        # 绝不能触碰 session/set_mode（那是权限预设）。
        self.assertEqual(fake.mode_values, [])
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'plan')
        bridge.send(SID, '开始执行', str(uuid.uuid4()), work_mode='default')
        self.assertTrue(self.wait_for(lambda: fake.config_values.get('collaboration_mode') == 'default'))
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'default')

    def test_work_mode_rejected_without_collaboration_option(self):
        options = [o for o in CONFIG_OPTIONS if o['id'] != 'collaboration_mode']
        _, adapter, bridge = self.make_bridge(config_options=options)
        bridge.view(SID)
        with self.assertRaises(ValueError) as ctx:
            bridge.send(SID, '做个计划', str(uuid.uuid4()), work_mode='plan')
        self.assertIn('不支持此工作模式', str(ctx.exception))

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
        steers = [r for r in fake.requests if r['method'] == '_session/steering']
        self.assertEqual(len(steers), 1)
        self.assertEqual(steers[0]['params']['_meta'],
                         {'steering': {'idleBehavior': 'promptRequired'}})
        bridge.interrupt(SID)
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))

    def test_steer_prompt_required_falls_back_to_prompt(self):
        # 任务恰在竞态间隙结束：dsh 回答 promptRequired，适配器回落为普通发送。
        fake, adapter, bridge = self.make_bridge()
        fake.steer_outcome = 'promptRequired'
        fake.prompt_updates = [chunk('agent_message_chunk', 'fresh turn')]
        bridge.view(SID)
        session = bridge.session(SID)
        result = adapter.dispatch_turn(session, 'sub-1', {'text': '再来一轮', 'mode': 'steer'}, [])
        self.assertEqual(result, {'accepted': True})
        self.assertEqual(fake.steer_texts, ['再来一轮'])
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(bridge.view(SID)['turns'][-1]['messages'][-1]['text'], 'fresh turn')

    def test_steer_rejected_when_idle(self):
        _, _, bridge = self.make_bridge()
        bridge.view(SID)
        with self.assertRaises(ValueError):
            bridge.send(SID, 'steer text', str(uuid.uuid4()), 'steer')

    def test_created_session_loads_in_a_fresh_process(self):
        # dsh 创建即入列且优雅关闭释放写句柄：创建不保留进程，attach 时新进程 load。
        fake, adapter, bridge = self.make_bridge()
        thread_id = adapter.create_session({'cwd': '/work'}, 'New chat')
        rows = adapter.list_sessions('此电脑')
        self.assertIn(thread_id, {r['id'] for r in rows})
        view = bridge.view(thread_id)
        self.assertTrue(view['connected'])
        self.assertEqual(view['model'], 'deepseek-official::deepseek-v4-pro')
        loads = [r for r in fake.requests if r['method'] == 'session/load']
        self.assertEqual([r['params']['sessionId'] for r in loads], [thread_id])
        self.assertNotIn(thread_id, adapter._pending_created)

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
