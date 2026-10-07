import threading
import time
import unittest
from pathlib import Path

from bridge.agents.acp import (AcpAuthRequired, AcpError, AcpProcess, AcpTimeout,
                               AcpUnavailable, discover_executable)
from tests.fake_acp import FakeAcpAgent

ROOT = Path(__file__).resolve().parents[1]


class AcpTransportTests(unittest.TestCase):
    def spawn(self, fake=None, **kwargs):
        fake = fake or FakeAcpAgent()
        proc = AcpProcess(['fake-acp', 'acp'], spawn=fake.spawn_factory(), **kwargs)
        self.addCleanup(proc.close)
        return fake, proc

    def test_initialize_captures_capabilities_agent_info_and_auth_methods(self):
        fake, proc = self.spawn()
        result = proc.initialize()
        self.assertEqual(result['protocolVersion'], 1)
        self.assertTrue(proc.capabilities['loadSession'])
        self.assertIn('list', proc.capabilities['sessionCapabilities'])
        self.assertTrue(proc.capabilities['promptCapabilities']['image'])
        self.assertEqual(proc.agent_info, {'name': 'fake-acp', 'version': '0.0.1'})
        self.assertEqual(proc.auth_methods, [])
        self.assertEqual(fake.spawned_argv, ['fake-acp', 'acp'])
        self.assertEqual(fake.requests[0]['id'], 0)
        self.assertEqual(fake.requests[0]['method'], 'initialize')
        self.assertEqual(fake.requests[0]['params'], {
            'protocolVersion': 1,
            'clientCapabilities': {'fs': {'readTextFile': False, 'writeTextFile': False},
                                   'terminal': False},
        })

    def test_request_roundtrip_and_unknown_method_keeps_error_code(self):
        fake, proc = self.spawn()
        proc.initialize()
        created = proc.request('session/new', {'cwd': '/tmp', 'mcpServers': []})
        self.assertEqual(created['sessionId'], 'fake-1')
        self.assertEqual(created['configOptions'], fake.config_options)
        self.assertEqual(created['modes'], fake.modes)
        again = proc.request('session/new', {'cwd': '/tmp', 'mcpServers': []})
        self.assertEqual(again['sessionId'], 'fake-2')
        with self.assertRaises(AcpError) as ctx:
            proc.request('no/such-method', {})
        self.assertEqual(ctx.exception.code, -32601)
        self.assertEqual(str(ctx.exception), 'Method not found')
        self.assertNotIsInstance(ctx.exception, AcpAuthRequired)

    def test_timeout_raises_acp_timeout(self):
        fake, proc = self.spawn()
        proc.initialize()
        fake.hang()
        with self.assertRaises(AcpTimeout):
            proc.request('session/list', {}, timeout=0.3)

    def test_notifications_reach_the_handler_in_order(self):
        fake = FakeAcpAgent()
        fake.load_updates = [
            {'sessionUpdate': 'user_message_chunk', 'content': {'type': 'text', 'text': 'hi'}},
            {'sessionUpdate': 'agent_message_chunk', 'content': {'type': 'text', 'text': 'hello'}},
        ]
        seen = []
        _, proc = self.spawn(fake, on_notification=seen.append)
        proc.initialize()
        proc.request('session/load', {'sessionId': 'fake-1', 'cwd': '/tmp', 'mcpServers': []})
        self.assertEqual([m['method'] for m in seen], ['session/update', 'session/update'])
        self.assertEqual([m['params']['update'] for m in seen], fake.load_updates)
        self.assertEqual([m['params']['sessionId'] for m in seen], ['fake-1', 'fake-1'])

    def test_reverse_request_roundtrip_records_permission_outcome(self):
        fake = FakeAcpAgent()
        fake.prompt_updates = [
            ('permission', {'options': [{'optionId': 'allow', 'kind': 'allow_once'}]}),
            {'sessionUpdate': 'agent_message_chunk', 'content': {'type': 'text', 'text': 'done'}},
        ]
        notifications, reverse = [], []
        def on_request(message, respond):
            reverse.append(message)
            respond({'outcome': {'outcome': 'selected', 'optionId': 'allow'}})
        _, proc = self.spawn(fake, on_notification=notifications.append, on_request=on_request)
        proc.initialize()
        result = proc.request('session/prompt', {'sessionId': 'fake-1', 'prompt': []})
        self.assertEqual(result['stopReason'], 'end_turn')
        self.assertEqual([m['method'] for m in reverse], ['session/request_permission'])
        self.assertEqual(reverse[0]['params']['sessionId'], 'fake-1')
        self.assertEqual(fake.permission_outcome, {'outcome': 'selected', 'optionId': 'allow'})
        self.assertEqual(len(notifications), 1)

    def test_process_death_fails_pending_requests_and_reports_exit_once(self):
        exits = []
        fake, proc = self.spawn(on_exit=exits.append)
        proc.initialize()
        fake.hang()
        outcome = []
        def blocked():
            try:
                proc.request('session/list', {}, timeout=10)
            except AcpUnavailable as exc:
                outcome.append(str(exc))
        thread = threading.Thread(target=blocked)
        thread.start()
        time.sleep(0.2)
        fake.die()
        thread.join(5)
        self.assertFalse(thread.is_alive())
        self.assertEqual(outcome, ['Agent 进程已退出；操作结果未知'])
        with self.assertRaises(AcpUnavailable):
            proc.request('session/list', {}, timeout=0.2)
        proc.close()
        self.assertEqual(len(exits), 1)
        self.assertEqual(exits[0], 1)

    def test_non_json_lines_are_dropped_and_the_protocol_continues(self):
        fake, proc = self.spawn()
        fake.emit_raw(b'garbage banner line\n')
        fake.emit_raw(b'{"jsonrpc": "2.0", "broken"\n')
        proc.initialize()
        self.assertEqual(proc.agent_info['name'], 'fake-acp')
        created = proc.request('session/new', {})
        self.assertEqual(created['sessionId'], 'fake-1')

    def test_close_is_idempotent_and_requests_fail_afterwards(self):
        fake, proc = self.spawn()
        proc.initialize()
        proc.close()
        proc.close()
        with self.assertRaises(AcpUnavailable):
            proc.request('session/list', {}, timeout=0.2)

    def test_auth_required_error_maps_to_acp_auth_required(self):
        fake = FakeAcpAgent()
        fake.errors['initialize'] = {'code': -32000, 'message': 'Authentication required',
                                     'data': {'reason': 'token-expired'}}
        _, proc = self.spawn(fake)
        with self.assertRaises(AcpAuthRequired) as ctx:
            proc.initialize()
        self.assertEqual(ctx.exception.code, -32000)
        self.assertEqual(ctx.exception.data, {'reason': 'token-expired'})
        self.assertEqual(str(ctx.exception), 'Authentication required')

    def test_discover_executable_prefers_env_then_home_then_path(self):
        import os
        from unittest.mock import patch
        with patch.dict(os.environ, {'FAKE_AGENT_BIN': '/opt/custom/agent'}):
            self.assertEqual(discover_executable('FAKE_AGENT_BIN', '.fake/bin', 'agent'), '/opt/custom/agent')
        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop('FAKE_AGENT_BIN', None)
            with patch('pathlib.Path.is_file', return_value=True):
                found = discover_executable('FAKE_AGENT_BIN', '.fake/bin', 'agent')
            self.assertTrue(found.endswith('/.fake/bin/agent'))
            with patch('pathlib.Path.is_file', return_value=False), \
                 patch('shutil.which', return_value=None):
                self.assertIsNone(discover_executable('FAKE_AGENT_BIN', '.fake/bin', 'agent'))


if __name__ == '__main__':
    unittest.main()
