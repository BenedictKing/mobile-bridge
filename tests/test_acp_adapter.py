"""ACP adapter integration: fake agent behind the real Bridge/httpd stack."""
import http.client
import json
import tempfile
import threading
import time
import types
import unittest
import uuid
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import patch

from bridge.agents.acp import AcpUnavailable
from bridge.agents.acp_adapter import AcpAdapter
from bridge.agents.base import Registry
from bridge.agents.kimi import KimiAdapter
from bridge.auth import password_record
from bridge.httpd import GatewayServer
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)
SID = 'session_fake-0001'
UPDATED = '2026-01-02T03:04:05Z'
UPDATED_MS = int(datetime(2026, 1, 2, 3, 4, 5, tzinfo=timezone.utc).timestamp() * 1000)

CONFIG_OPTIONS = [
    {'id': 'model', 'name': 'Model', 'type': 'select', 'category': 'model',
     'currentValue': 'kimi-code/k3-256k',
     'options': [{'value': 'kimi-code/k3-256k', 'name': 'K3 256K'},
                 {'value': 'kimi-code/k2', 'name': 'K2'}]},
    {'id': 'thinking', 'name': 'Thinking', 'type': 'select', 'category': 'thought_level',
     'currentValue': 'high',
     'options': [{'value': 'low', 'name': 'Low'}, {'value': 'high', 'name': 'High'},
                 {'value': 'max', 'name': 'Max'}]},
    {'id': 'mode', 'name': 'Mode', 'type': 'select', 'category': 'mode',
     'currentValue': 'default',
     'options': [{'value': 'default', 'name': 'Default'}, {'value': 'plan', 'name': 'Plan'}]},
]
MODES = {'currentModeId': 'default',
         'availableModes': [{'id': 'default', 'name': 'Default'},
                            {'id': 'plan', 'name': 'Plan'}]}


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


class AcpAdapterTests(unittest.TestCase):
    def make_bridge(self, fake=None, adapter_cls=KimiAdapter):
        fake = fake or FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = adapter_cls('fake-acp', data_dir=Path(tmp.name) / 'data',
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

    def wait_prompt_done(self, bridge):
        handle = bridge.adapter._handles.get(SID)
        return self.wait_for(lambda: handle is None or not handle.prompt_running)

    def view(self, bridge, **kwargs):
        return bridge.view(SID, **kwargs)

    def test_attach_replays_history_into_view(self):
        fake = FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        fake.load_updates = [
            chunk('user_message_chunk', 'hi'),
            chunk('agent_message_chunk', 'hello'),
            {'sessionUpdate': 'tool_call', 'toolCallId': 't1', 'title': 'Run', 'kind': 'execute',
             'status': 'in_progress', 'rawInput': {'command': 'ls -la'}},
            {'sessionUpdate': 'tool_call_update', 'toolCallId': 't1', 'status': 'completed',
             'content': [{'type': 'content', 'content': {'type': 'text', 'text': 'total 0'}}]},
            chunk('user_message_chunk', 'again'),
            chunk('agent_thought_chunk', 'thinking'),
            chunk('agent_message_chunk', 'done'),
        ]
        _, _, bridge = self.make_bridge(fake)
        view = self.view(bridge)
        self.assertTrue(view['connected'])
        self.assertEqual(view['agent'], 'kimi')
        self.assertEqual(view['capabilities'], [])
        self.assertEqual(view['title'], 'Old')
        self.assertEqual(view['cwd'], '/work')
        self.assertEqual(view['model'], 'kimi-code/k3-256k')
        self.assertEqual(view['effort'], 'high')
        self.assertEqual(view['collaborationMode'], 'default')
        self.assertEqual(view['status'], 'idle')
        self.assertTrue(view['historyComplete'])
        self.assertEqual(len(view['turns']), 2)
        first, second = view['turns']
        self.assertEqual(first['status'], 'completed')
        self.assertEqual(second['status'], 'completed')
        self.assertEqual([m['role'] for m in first['messages']], ['user', 'assistant', 'activity'])
        tool = first['messages'][2]
        self.assertEqual(tool['kind'], 'toolCall')
        self.assertEqual(tool['text'], 'ls -la')
        self.assertEqual(tool['output'], 'total 0')
        self.assertEqual(tool['status'], 'completed')
        self.assertEqual([m['kind'] for m in second['messages']],
                         ['userMessage', 'agentThought', 'agentMessage'])
        self.assertEqual(second['messages'][2]['text'], 'done')

    def test_list_decorates_rows_with_agent_and_project(self):
        _, _, bridge = self.make_bridge()
        rows = bridge.list()
        self.assertEqual(len(rows), 1)
        row = rows[0]
        self.assertEqual(row['id'], SID)
        self.assertEqual(row['title'], 'Old')
        self.assertEqual(row['agent'], 'kimi')
        self.assertEqual(row['host'], 'local')
        self.assertEqual(row['recency'], UPDATED_MS)
        self.assertEqual(row['projectKey'], 'local|/work')
        self.assertEqual(row['projectName'], 'work')
        self.assertEqual(row['capabilities'], [])
        self.assertEqual(bridge.list(query='nomatch'), [])
        self.assertEqual(bridge.list(query='old'), rows)

    def test_send_dispatches_prompt_and_folds_chunks(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'answer')]
        self.view(bridge)
        result = bridge.send(SID, 'hello', str(uuid.uuid4()))
        self.assertEqual(result['status'], 'accepted')
        prompts = [r for r in fake.requests if r['method'] == 'session/prompt']
        self.assertEqual(len(prompts), 1)
        self.assertEqual(prompts[0]['params']['sessionId'], SID)
        self.assertEqual(prompts[0]['params']['prompt'][0], {'type': 'text', 'text': 'hello'})
        self.assertTrue(self.wait_for(lambda: self.view(bridge)['status'] == 'idle'
                                      and self.view(bridge)['turns'][-1]['status'] == 'completed'))
        view = self.view(bridge)
        last = view['turns'][-1]
        self.assertEqual(last['messages'][0]['text'], 'hello')
        self.assertEqual(last['messages'][-1]['text'], 'answer')
        self.assertEqual(view['status'], 'idle')

    def test_duplicate_submission_is_not_replayed(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'answer')]
        self.view(bridge)
        submission = str(uuid.uuid4())
        first = bridge.send(SID, 'hello', submission)
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        again = bridge.send(SID, 'hello', submission)
        self.assertEqual(again['status'], first['status'])
        self.assertTrue(again['duplicate'])
        prompts = [r for r in fake.requests if r['method'] == 'session/prompt']
        self.assertEqual(len(prompts), 1)
        with self.assertRaises(ValueError):
            bridge.send(SID, 'different text', submission)

    def test_permission_card_roundtrip(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [
            ('permission', {'toolCall': {'toolCallId': 't1', 'title': 'Run ls', 'kind': 'execute',
                                         'status': 'in_progress', 'rawInput': {'command': 'ls -la'}},
                            'options': [{'optionId': 'allow', 'name': 'Allow once', 'kind': 'allow_once'},
                                        {'optionId': 'deny', 'name': 'Deny', 'kind': 'reject_once'}]}),
            chunk('agent_message_chunk', 'done'),
        ]
        self.view(bridge)
        bridge.send(SID, 'run it', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['requests']))
        request = bridge.view(SID)['requests'][0]
        self.assertEqual(request['method'], 'acp/requestPermission')
        self.assertTrue(request['supported'])
        self.assertEqual(request['params']['title'], 'Run ls')
        self.assertEqual(request['params']['command'], 'ls -la')
        self.assertEqual([o['id'] for o in request['params']['options']], ['allow', 'deny'])
        with self.assertRaises(ValueError):
            bridge.respond(SID, request['id'], {'optionId': 'bogus'})
        result = bridge.respond(SID, request['id'], {'optionId': 'allow'})
        self.assertEqual(result, {'ok': True})
        self.assertTrue(self.wait_for(lambda: fake.permission_outcome is not None))
        self.assertEqual(fake.permission_outcome, {'outcome': 'selected', 'optionId': 'allow'})
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertTrue(self.wait_prompt_done(bridge))
        view = bridge.view(SID)
        self.assertEqual(view['requests'], [])
        self.assertEqual(view['turns'][-1]['status'], 'completed')
        self.assertEqual(view['turns'][-1]['messages'][-1]['text'], 'done')

    def test_interrupt_cancels_running_turn(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'partial'), ('await_cancel',)]
        self.view(bridge)
        bridge.send(SID, 'hello', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'active'))
        result = bridge.interrupt(SID)
        self.assertEqual(result, {'stopped': True})
        self.assertTrue(self.wait_for(lambda: fake.cancelled))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertTrue(self.wait_prompt_done(bridge))
        view = bridge.view(SID)
        self.assertEqual(view['turns'][-1]['status'], 'interrupted')
        with self.assertRaises(ValueError):
            bridge.interrupt(SID)

    def test_send_while_active_rejected_and_queue_dispatches_after_idle(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [('await_cancel',)]
        self.view(bridge)
        bridge.send(SID, 'first', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'active'))
        with self.assertRaises(ValueError):
            bridge.send(SID, 'second', str(uuid.uuid4()))
        queued_id = str(uuid.uuid4())
        self.assertEqual(bridge.send(SID, 'later', queued_id, 'queue')['status'], 'queued')
        bridge.interrupt(SID)
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertTrue(self.wait_prompt_done(bridge))
        fake.prompt_updates = [chunk('agent_message_chunk', 'queued answer')]
        session = bridge.live[SID]
        key = SID + ':' + queued_id
        bridge._send_queued(session, key, bridge.submissions[key])
        self.assertEqual(bridge.submissions[key]['status'], 'accepted')
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        prompts = [r for r in fake.requests if r['method'] == 'session/prompt']
        self.assertEqual(len(prompts), 2)
        self.assertEqual(prompts[-1]['params']['prompt'][0]['text'], 'later')

    def test_process_death_disconnects_session(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        self.assertIn(SID, adapter._handles)
        fake.die()
        self.assertTrue(self.wait_for(lambda: not bridge.live[SID].connected))
        session = bridge.live[SID]
        self.assertEqual(session.error, 'Agent 进程已退出，请重新连接')

    def test_detach_closes_agent_session_and_process(self):
        fake, adapter, bridge = self.make_bridge()
        self.view(bridge)
        handle = adapter._handles[SID]
        adapter.detach(bridge.live[SID])
        self.assertEqual(adapter._handles, {})
        self.assertEqual(fake.closed_sessions, [SID])
        self.assertIsNotNone(handle.process._proc.returncode)

    def test_update_settings_maps_config_options(self):
        fake, _, bridge = self.make_bridge()
        self.view(bridge)
        result = bridge.settings(SID, 'kimi-code/k2', 'max')
        self.assertEqual(result, {'applied': True, 'confirmed': True,
                                  'model': 'kimi-code/k2', 'effort': 'max'})
        self.assertEqual(fake.config_values, {'model': 'kimi-code/k2', 'thinking': 'max'})
        view = bridge.view(SID)
        self.assertEqual(view['model'], 'kimi-code/k2')
        self.assertEqual(view['effort'], 'max')
        with self.assertRaises(ValueError):
            bridge.settings(SID, 'kimi-code/unknown', 'high')
        with self.assertRaises(ValueError):
            bridge.settings(SID, 'kimi-code/k2', 'high', fast_mode=True)

    def test_catalog_and_skills(self):
        _, _, bridge = self.make_bridge()
        self.view(bridge)
        models = bridge.catalog(SID, kind='models')
        self.assertEqual(models['kind'], 'models')
        self.assertEqual([m['id'] for m in models['models']], ['kimi-code/k3-256k', 'kimi-code/k2'])
        self.assertEqual(models['currentModel'], 'kimi-code/k3-256k')
        self.assertEqual(models['currentEffort'], 'high')
        self.assertIn('high', models['models'][0]['efforts'])
        self.assertFalse(models['fastMode']['allowed'])
        skills = bridge.catalog(SID, kind='skills')
        self.assertEqual(skills, {'kind': 'skills', 'skills': [], 'errors': []})
        with self.assertRaises(ValueError):
            bridge.send(SID, 'hi', str(uuid.uuid4()), skills=['review'])

    def test_plan_mode_switch_uses_set_mode(self):
        fake, _, bridge = self.make_bridge()
        fake.prompt_updates = [chunk('agent_message_chunk', 'planned')]
        self.view(bridge)
        bridge.send(SID, 'plan this', str(uuid.uuid4()), work_mode='plan')
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['status'] == 'idle'))
        self.assertEqual(fake.mode_values, ['plan'])
        self.assertEqual(bridge.view(SID)['collaborationMode'], 'plan')

    def test_kimi_adapter_requires_executable(self):
        with patch('bridge.agents.kimi.discover_executable', return_value=None):
            with self.assertRaises(AcpUnavailable) as ctx:
                KimiAdapter()
        self.assertEqual(str(ctx.exception), '未找到 kimi 命令，请先安装 Kimi Code CLI')

    def test_run_agents_rejects_unknown_kind(self):
        import run
        with patch('sys.argv', ['run.py', '--agents', 'bogus']):
            with self.assertRaises(SystemExit) as ctx:
                run.main()
        self.assertEqual(ctx.exception.code, 2)

    def test_agent_kinds_parsing(self):
        import run
        self.assertEqual(run.agent_kinds('codex,kimi'), ['codex', 'kimi'])
        self.assertEqual(run.agent_kinds(' kimi , mcode ,,'), ['kimi', 'mcode'])
        self.assertEqual(run.agent_kinds('codex,claude'), ['codex', 'claude'])
        self.assertEqual(run.agent_kinds('codex,dsh'), ['codex', 'dsh'])
        for bad in ('', ',,', 'bogus', 'codex,zcode'):
            with self.assertRaises(ValueError):
                run.agent_kinds(bad)

    def test_agents_default_reads_environment(self):
        import run
        with patch.dict('os.environ', {'MOBILE_BRIDGE_AGENTS': 'bogus'}):
            with patch('sys.argv', ['run.py']):
                with self.assertRaises(SystemExit) as ctx:
                    run.main()
        self.assertEqual(ctx.exception.code, 2)


class AcpHttpTests(unittest.TestCase):
    """Registry routing across two ACP bridges served by one GatewayServer."""

    SID_A = 'session_alpha_0001'
    SID_K = 'session_kimi_00001'

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        fake_a = FakeAcpAgent(list_rows=[{'sessionId': self.SID_A, 'cwd': '/alpha',
                                          'title': 'Alpha', 'updatedAt': UPDATED}])
        fake_k = FakeAcpAgent(list_rows=[{'sessionId': self.SID_K, 'cwd': '/kimi',
                                          'title': 'Kimi chat', 'updatedAt': UPDATED}])
        self.fake_a, self.fake_k = fake_a, fake_k
        adapter_a = AcpAdapter('fake-acp', data_dir=root / 'a', spawn=fake_a.spawn_factory())
        adapter_k = KimiAdapter('fake-acp', data_dir=root / 'k', spawn=fake_k.spawn_factory())
        self.bridge_a = Bridge(root, root / 'a', adapter=adapter_a)
        self.bridge_k = Bridge(root, root / 'k', adapter=adapter_k)
        self.addCleanup(self.bridge_a.close)
        self.addCleanup(self.bridge_k.close)
        registry = Registry()
        registry.register('codex', self.bridge_a)
        registry.register('kimi', self.bridge_k)
        config = {'auth': {'mode': 'password', 'username': 'admin',
                           **password_record('correct password test')}, 'origins': []}
        self.server = GatewayServer(('127.0.0.1', 0), self.bridge_a, config, ROOT / 'web',
                                    registry=registry)
        self.port = self.server.server_address[1]
        self.origin = 'http://127.0.0.1:' + str(self.port)
        self.server.origins.add(self.origin)
        self.server.hosts.add('127.0.0.1:' + str(self.port))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def request(self, method, path, body=None):
        conn = http.client.HTTPConnection('127.0.0.1', self.port, timeout=5)
        if not hasattr(self, '_auth'):
            conn.request('POST', '/api/login',
                         body=json.dumps({'username': 'admin', 'password': 'correct password test'}),
                         headers={'Origin': self.origin, 'Content-Type': 'application/json'})
            response = conn.getresponse()
            payload = json.loads(response.read())
            self._auth = {'Cookie': response.getheader('Set-Cookie').split(';')[0],
                          'X-CSRF-Token': payload['csrf']}
        headers = {'Origin': self.origin, 'Content-Type': 'application/json', **self._auth}
        conn.request(method, path, body=json.dumps(body) if body is not None else None,
                     headers=headers)
        response = conn.getresponse()
        status, data = response.status, response.read()
        conn.close()
        return status, json.loads(data)

    def test_agent_query_selects_bridge_and_merges_rows(self):
        status, body = self.request('GET', '/api/sessions')
        self.assertEqual(status, 200)
        by_agent = {row['agent']: row['id'] for row in body['sessions']}
        self.assertEqual(by_agent, {'acp': self.SID_A, 'kimi': self.SID_K})
        status, body = self.request('GET', '/api/sessions?agent=kimi')
        self.assertEqual(status, 200)
        self.assertEqual([row['id'] for row in body['sessions']], [self.SID_K])
        self.assertEqual(body['sessions'][0]['agent'], 'kimi')
        status, body = self.request('GET', '/api/sessions?agent=bogus')
        self.assertEqual(status, 400)
        self.assertEqual(body['error'], '未知的 Agent 类型')

    def test_session_route_falls_back_to_listed_lookup(self):
        self.request('GET', '/api/sessions')  # Populate the listed sets.
        status, view = self.request('GET', '/api/sessions/' + self.SID_K)
        self.assertEqual(status, 200)
        self.assertEqual(view['id'], self.SID_K)
        self.assertEqual(view['agent'], 'kimi')
        status, _ = self.request('GET', '/api/sessions/' + self.SID_K + '?agent=bogus')
        self.assertEqual(status, 400)

    def test_projects_merge_across_kinds(self):
        status, body = self.request('GET', '/api/projects')
        self.assertEqual(status, 200)
        cwds = {project['cwd'] for project in body['projects']}
        self.assertEqual(cwds, {'/alpha', '/kimi'})

    def test_session_route_probes_unlisted_acp_session(self):
        # No GET /api/sessions first: a direct link after a restart must still
        # route to the kimi bridge instead of failing Codex UUID validation.
        self.assertNotIn(self.SID_K, self.bridge_k.listed)
        status, view = self.request('GET', '/api/sessions/' + self.SID_K)
        self.assertEqual(status, 200)
        self.assertEqual(view['id'], self.SID_K)
        self.assertEqual(view['agent'], 'kimi')
        self.assertIn(self.SID_K, self.bridge_k.listed)

    def test_view_lists_enabled_agents(self):
        status, view = self.request('GET', '/api/sessions/' + self.SID_K)
        self.assertEqual(status, 200)
        self.assertEqual(view['agents'], [{'kind': 'codex', 'label': 'ACP Agent'},
                                          {'kind': 'kimi', 'label': 'Kimi'}])

    def test_migrate_creates_target_session_and_injects_transcript(self):
        self.fake_k.list_rows.append({'sessionId': 'session_kimi_alpha', 'cwd': '/alpha',
                                      'title': 'A', 'updatedAt': UPDATED})
        status, body = self.request('POST', '/api/sessions/' + self.SID_A + '/migrate',
                                    {'agent': 'kimi', 'id': str(uuid.uuid4())})
        self.assertEqual(status, 200)
        self.assertEqual(body['agent'], 'kimi')
        self.assertEqual(body['status'], 'sent')
        self.assertTrue(body['sent'])
        prompts = [r for r in self.fake_k.requests if r['method'] == 'session/prompt']
        self.assertEqual(len(prompts), 1)
        text = prompts[0]['params']['prompt'][0]['text']
        self.assertIn('【从 ACP Agent 迁移的会话】', text)
        self.assertIn('目录：/alpha', text)
        status, body = self.request('POST', '/api/sessions/' + self.SID_A + '/migrate',
                                    {'agent': 'bogus', 'id': str(uuid.uuid4())})
        self.assertEqual(status, 400)
        self.assertEqual(body['error'], '未知的 Agent 类型')


class RegistryProbeTests(unittest.TestCase):
    """bridge_for probes agent-native stores when the listed sets are cold."""

    def make_registry(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        fake = FakeAcpAgent(list_rows=[{'sessionId': SID, 'cwd': '/work',
                                        'title': 'Old', 'updatedAt': UPDATED}])
        adapter = KimiAdapter('fake-acp', data_dir=root / 'data', spawn=fake.spawn_factory())
        bridge = Bridge(root, root / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        codex = types.SimpleNamespace(listed=set())
        registry = Registry()
        registry.register('codex', codex)
        registry.register('kimi', bridge)
        return registry, bridge, codex

    def test_probe_finds_unlisted_session_and_caches_it(self):
        registry, bridge, _ = self.make_registry()
        self.assertIs(registry.bridge_for(SID), bridge)
        self.assertIn(SID, bridge.listed)

    def test_probe_misses_return_none_without_touching_codex(self):
        registry, bridge, codex = self.make_registry()
        self.assertIsNone(registry.bridge_for('session_missing_0000'))
        self.assertIsNone(registry.bridge_for(str(uuid.uuid4())))
        self.assertEqual(bridge.listed, set())
        self.assertEqual(codex.listed, set())


class AcpCreateChatTests(unittest.TestCase):
    """create_chat must mark new ACP sessions as listed for later routing."""

    def test_create_chat_marks_session_listed_and_stays_idempotent(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        fake = FakeAcpAgent(list_rows=[{'sessionId': SID, 'cwd': '/work',
                                        'title': 'Old', 'updatedAt': UPDATED}])
        adapter = KimiAdapter('fake-acp', data_dir=root / 'data', spawn=fake.spawn_factory())
        bridge = Bridge(root, root / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        project = adapter.projects()[0]
        request_id = str(uuid.uuid4())
        result = bridge.create_chat(project['key'], 'New chat', request_id)
        self.assertEqual(result['id'], 'fake-1')
        self.assertIn('fake-1', bridge.listed)
        again = bridge.create_chat(project['key'], 'New chat', request_id)
        self.assertEqual(again['id'], 'fake-1')
        self.assertEqual(fake.session_count, 1)


class AcpNotificationTests(unittest.TestCase):
    """Notifications accept non-UUID ACP ids and route them via the registry."""

    def setUp(self):
        from bridge.notifications import Notifications, save_settings
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        save_settings(root / 'n', {'enabled': True, 'topic': 'test', 'token': 'secret-token'})
        self.fake = FakeAcpAgent(list_rows=[{'sessionId': SID, 'cwd': '/work',
                                            'title': 'Old', 'updatedAt': UPDATED}])
        adapter = KimiAdapter('fake-acp', data_dir=root / 'k', spawn=self.fake.spawn_factory())
        self.bridge_k = Bridge(root, root / 'k', adapter=adapter)
        self.addCleanup(self.bridge_k.close)

        class CodexSource:
            listed = set()
            def __init__(self):
                def session_meta(tid):
                    raise KeyError(tid)
                self.adapter = type('Adapter', (), {'label': 'Codex',
                                                    'session_meta': staticmethod(session_meta)})()
            def for_host(self, host):
                return self
            def session(self, thread, **kwargs):
                raise AssertionError('codex sessions must not be resumed')
        self.codex = CodexSource()
        registry = Registry()
        registry.register('codex', self.codex)
        registry.register('kimi', self.bridge_k)
        self.manager = Notifications(self.codex, root / 'n', registry=registry)
        self.addCleanup(self.manager.close)

    def wait_for(self, predicate, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.02)
        return False

    def test_watch_and_policy_accept_acp_session_ids(self):
        row = self.manager.watch(SID, 'local', True)
        self.assertTrue(row['watching'])
        self.assertEqual([r['id'] for r in self.manager.watches()], [SID])
        effective = self.manager.policy(SID, 'local', {'requests': 'on', 'completion': 'off'})
        self.assertEqual(effective['requests'], 'on')
        self.assertIn(SID, self.bridge_k.listed)
        with self.assertRaises(KeyError):
            self.manager.policy('session_missing_0000', 'local', {'requests': 'on', 'completion': 'off'})

    def test_invalid_ids_are_still_rejected(self):
        for bad in ('bad|id', 'bad id', '', 'x' * 129):
            with self.assertRaises(ValueError):
                self.manager.watch(bad, 'local', True)
            with self.assertRaises(ValueError):
                self.manager.policy(bad, 'local')

    def test_notification_heading_uses_agent_label(self):
        self.assertEqual(self.manager._label_for(SID, 'local'), 'Kimi')
        self.assertEqual(self.manager._label_for(str(uuid.uuid4()), 'local'), 'Codex')

    def test_resume_watched_reattaches_acp_sessions_and_skips_codex(self):
        from bridge.notifications import write_json
        sid2 = 'session_second_001'
        self.fake.list_rows.append({'sessionId': sid2, 'cwd': '/work',
                                    'title': 'Second', 'updatedAt': UPDATED})
        self.manager.data_dir.mkdir(parents=True, exist_ok=True)
        write_json(self.manager.data_dir/'notification-watches.json', [
            {'id': SID, 'host': 'local'},
            {'id': str(uuid.uuid4()), 'host': 'local'},       # Codex: skipped.
            {'id': sid2, 'host': 'bogus-host'},               # Fails, must not block.
            {'id': sid2, 'host': 'local'},
        ])
        with self.assertLogs('bridge.notifications', level='ERROR'):
            self.manager.resume_watched()
        for sid in (SID, sid2):
            session = self.bridge_k.live.get(sid)
            self.assertIsNotNone(session)
            self.assertTrue(session.watched)
            self.assertIs(self.manager.attached[('local', sid)], session)
            self.assertTrue(self.wait_for(lambda: session.connected))
        self.assertNotIn(('bogus-host', sid2), self.manager.attached)

    def test_resume_watched_without_registry_is_a_noop(self):
        from bridge.notifications import Notifications
        manager = Notifications(self.codex, self.manager.data_dir)
        self.addCleanup(manager.close)
        manager.resume_watched()  # Single-agent installs have nothing to resume.
        self.assertEqual(manager.attached, {})


if __name__ == '__main__':
    unittest.main()
