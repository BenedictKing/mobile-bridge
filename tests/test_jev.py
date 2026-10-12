"""Jev advisory triage: pure mapping helpers plus the Bridge annotation path."""
import http.client
import json
import os
import tempfile
import threading
import time
import unittest
import uuid
from pathlib import Path
from unittest.mock import patch

from bridge.agents.kimi import KimiAdapter
from bridge.auth import password_record
from bridge.httpd import GatewayServer
from bridge.jev import (JevAssessor, JevSettings, RISK_LABELS, assessable,
                        auto_response, build_payload, effective_field, fingerprint,
                        init_shared_settings, normalize_answer,
                        parse_recommendation, shared_settings)
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent
from tests.test_acp_adapter import CONFIG_OPTIONS, MODES, SID, UPDATED, chunk

ROOT = Path(__file__).resolve().parents[1]


def acp_permission(options=(('allow', 'Allow once', 'allow_once'),
                            ('always', 'Always allow', 'allow_always'),
                            ('deny', 'Deny', 'reject_once'))):
    return {'id': 'req-1', 'method': 'acp/requestPermission', 'supported': True,
            'params': {'title': 'Run ls', 'command': 'ls -la',
                       'options': [{'id': oid, 'label': label, 'kind': kind}
                                   for oid, label, kind in options]}}


def codex_command(decisions=('accept', 'decline', 'cancel')):
    return {'id': 'req-2', 'method': 'item/commandExecution/requestApproval', 'supported': True,
            'params': {'command': 'rm -rf /tmp/scratch', 'cwd': '/work',
                       'availableDecisions': list(decisions)}}


class JevMappingTests(unittest.TestCase):
    def test_auto_response_requires_safe_high_confidence_assessment(self):
        safe = {'source': 'jev', 'risk': 1, 'reversible': 0.9,
                'recommend': 'accept_once', 'confidence': 0.9}
        self.assertEqual(auto_response(codex_command(('accept', 'decline')), safe),
                         {'decision': 'accept'})
        self.assertIsNone(auto_response(codex_command(), {**safe, 'risk': 2}))
        self.assertIsNone(auto_response(codex_command(), {**safe, 'reversible': 0.79}))
        self.assertIsNone(auto_response(codex_command(), {**safe, 'confidence': 0.79}))
        self.assertIsNone(auto_response(codex_command(), {**safe, 'recommend': 'accept_always'}))

    def test_auto_response_maps_acp_and_computer_use(self):
        safe = {'source': 'jev', 'risk': 0, 'reversible': 1.0,
                'recommend': 'accept_once', 'confidence': 1.0}
        self.assertEqual(auto_response(acp_permission(), safe), {'optionId': 'allow'})
        computer = {'method': 'mcpServer/elicitation/request', 'supported': True,
                    'params': {'computerUse': {'app': 'Safari', 'persistModes': ['session']}}}
        self.assertEqual(auto_response(computer, {**safe, 'recommend': 'accept_session'}),
                         {'action': 'accept', 'persist': 'session'})
    def test_assessable_covers_permission_confirmations_only(self):
        self.assertTrue(assessable(acp_permission()))
        self.assertTrue(assessable(codex_command()))
        self.assertTrue(assessable({'method': 'item/fileChange/requestApproval', 'supported': True, 'params': {}}))
        self.assertTrue(assessable({'method': 'item/permissions/requestApproval', 'supported': True, 'params': {}}))
        elicitation = {'method': 'mcpServer/elicitation/request', 'supported': True,
                       'params': {'computerUse': {'app': 'Safari', 'persistModes': ['session']}}}
        self.assertTrue(assessable(elicitation))
        self.assertFalse(assessable({'method': 'mcpServer/elicitation/request', 'supported': True,
                                     'params': {'requestedSchema': {'type': 'object'}}}))
        self.assertFalse(assessable({'method': 'item/tool/requestUserInput', 'supported': True, 'params': {}}))
        self.assertFalse(assessable({'method': 'item/plan/requestImplementation', 'supported': True, 'params': {}}))
        self.assertFalse(assessable({'method': 'acp/requestPermission', 'supported': False, 'params': {}}))

    def test_fingerprint_tracks_payload_not_identity(self):
        first, second = acp_permission(), acp_permission()
        second['id'] = 'req-other'
        self.assertEqual(fingerprint(first), fingerprint(second))
        second['params']['command'] = 'sudo rm -rf /'
        self.assertNotEqual(fingerprint(first), fingerprint(second))
        self.assertEqual(len(fingerprint(first)), 32)

    def test_payload_shape_and_available_options(self):
        payload = build_payload(acp_permission(), {'agent': 'kimi', 'cwd': '/work'})
        self.assertEqual(payload['model'], 'jev-latest')
        self.assertEqual(payload['state']['command'], 'ls -la')
        self.assertEqual(payload['state']['agent'], 'kimi')
        self.assertEqual(sorted(payload['questions']), ['recommend', 'reversible', 'risk'])
        self.assertEqual(payload['questions']['risk']['type'], 'score')
        self.assertEqual(len(payload['questions']['risk']['criteria']), 4)
        self.assertEqual(payload['questions']['reversible']['type'], 'noul')
        # accept_session is absent from the card and must stay out of the choice.
        self.assertEqual(sorted(payload['questions']['recommend']['criteria']),
                         ['accept_always', 'accept_once', 'decline', 'review'])

    def test_available_decisions_from_native_fields(self):
        payload = build_payload(codex_command(), {})
        self.assertEqual(sorted(payload['questions']['recommend']['criteria']),
                         ['accept_once', 'decline', 'review'])
        # Cards without decision metadata still offer the primary pair.
        bare = codex_command()
        del bare['params']['availableDecisions']
        payload = build_payload(bare, {})
        self.assertEqual(sorted(payload['questions']['recommend']['criteria']),
                         ['accept_once', 'decline', 'review'])

    def test_normalize_answer_rounds_and_clamps(self):
        payload = {'answers': {
            'risk': {'type': 'score', 'score': 2.4, 'confidence': 0.9},
            'reversible': {'type': 'noul', 'noul': 0.18},
            'recommend': {'type': 'choice', 'choice': 'accept_once',
                          'probabilities': {'accept_once': 0.8, 'decline': 0.2}, 'confidence': 0.75},
        }}
        assessment = normalize_answer(payload)
        self.assertEqual(assessment['source'], 'jev')
        self.assertEqual(assessment['risk'], 2)
        self.assertEqual(assessment['riskLabel'], RISK_LABELS[2])
        self.assertEqual(assessment['reversible'], 0.18)
        self.assertEqual(assessment['recommend'], 'accept_once')
        self.assertEqual(assessment['confidence'], 0.75)
        self.assertEqual(assessment['options'], {'accept_once': 0.8, 'decline': 0.2})

    def test_normalize_answer_rejects_malformed(self):
        self.assertIsNone(normalize_answer({}))
        self.assertIsNone(normalize_answer({'answers': {'risk': {}, 'recommend': {}}}))
        bad_choice = {'answers': {'risk': {'score': 1},
                                  'recommend': {'choice': 'accept_forever'}}}
        self.assertIsNone(normalize_answer(bad_choice))
        clamped = normalize_answer({'answers': {'risk': {'score': 9},
                                                'recommend': {'choice': 'decline'}}})
        self.assertEqual(clamped['risk'], 3)

    def test_parse_recommendation_targets_native_ids(self):
        high = {'source': 'jev', 'recommend': 'decline', 'risk': 3, 'confidence': 0.9}
        self.assertEqual(parse_recommendation(acp_permission(), high), 'deny')
        once = {'source': 'jev', 'recommend': 'accept_once', 'risk': 1, 'confidence': 0.8}
        self.assertEqual(parse_recommendation(acp_permission(), once), 'allow')
        always = {'source': 'jev', 'recommend': 'accept_always', 'risk': 0, 'confidence': 0.7}
        self.assertEqual(parse_recommendation(acp_permission(), always), 'always')
        self.assertEqual(parse_recommendation(codex_command(), once), 'accept')
        self.assertEqual(parse_recommendation(codex_command(), high), 'decline')
        self.assertIsNone(parse_recommendation(codex_command(), {'source': 'local'}))
        self.assertIsNone(parse_recommendation(codex_command(),
                                               {'source': 'jev', 'recommend': 'review'}))
        elicitation = {'method': 'mcpServer/elicitation/request', 'supported': True,
                       'params': {'computerUse': {'app': 'Safari', 'persistModes': ['session', 'always']}}}
        self.assertEqual(parse_recommendation(elicitation, once), 'once')
        self.assertEqual(parse_recommendation(elicitation, always), 'always')
        self.assertEqual(parse_recommendation(elicitation,
                                              {'source': 'jev', 'recommend': 'accept_session'}), 'session')


class JevAssessorTests(unittest.TestCase):
    def test_disabled_without_api_key(self):
        assessor = JevAssessor()
        self.assertFalse(assessor.enabled)
        self.assertFalse(assessor.start('k', acp_permission(), {}, lambda a: None))

    def test_assessment_runs_once_and_caches(self):
        assessor = JevAssessor(api_key='test-key')
        seen = []
        done = threading.Event()

        def on_done(assessment):
            seen.append(assessment)
            done.set()

        def fake_call(payload):
            self.assertEqual(payload['model'], 'jev-latest')
            self.assertTrue(payload['state'])
            return {'answers': {
                'risk': {'score': 1.2},
                'reversible': {'noul': 0.9},
                'recommend': {'choice': 'accept_once', 'confidence': 0.66},
            }}

        assessor._call = fake_call
        request = acp_permission()
        self.assertTrue(assessor.start(fingerprint(request), request, {}, on_done))
        # A second start while in flight is ignored; the key dedupes callers.
        self.assertFalse(assessor.start(fingerprint(request), request, {}, on_done))
        self.assertTrue(done.wait(5))
        self.assertEqual(len(seen), 1)
        self.assertEqual(seen[0]['recommend'], 'accept_once')
        self.assertEqual(assessor.cached(fingerprint(request)), seen[0])

    def test_failure_caches_none_without_retry_loop(self):
        assessor = JevAssessor(api_key='test-key')
        done = threading.Event()

        def on_done(assessment):
            self.assertIsNone(assessment)
            done.set()

        def broken_call(payload):
            raise OSError('Jev endpoint returned HTTP 429')

        assessor._call = broken_call
        request = codex_command()
        assessor.start(fingerprint(request), request, {}, on_done)
        self.assertTrue(done.wait(5))
        self.assertIsNone(assessor.cached(fingerprint(request)))
        self.assertFalse(assessor.start(fingerprint(request), request, {}, on_done))


class BridgeAssessmentTests(unittest.TestCase):
    def make_bridge(self, canned):
        fake = FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        adapter = KimiAdapter('fake-acp', data_dir=Path(tmp.name) / 'data',
                              spawn=fake.spawn_factory())
        bridge = Bridge(Path(tmp.name), Path(tmp.name) / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        bridge.assessor = JevAssessor(api_key='test-key')
        bridge.assessor._call = canned
        return fake, bridge

    def wait_for(self, predicate, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if predicate():
                return True
            time.sleep(0.02)
        return False

    def test_assessment_reaches_the_view_and_guides_the_choice(self):
        canned = {'answers': {
            'risk': {'score': 2.6},
            'reversible': {'noul': 0.1},
            'recommend': {'choice': 'decline', 'confidence': 0.9},
        }}
        fake, bridge = self.make_bridge(lambda payload: canned)
        fake.prompt_updates = [
            ('permission', {'toolCall': {'toolCallId': 't1', 'title': 'Wipe scratch',
                                         'kind': 'execute', 'status': 'in_progress',
                                         'rawInput': {'command': 'rm -rf /tmp/scratch'}},
                            'options': [{'optionId': 'allow', 'name': 'Allow once', 'kind': 'allow_once'},
                                        {'optionId': 'deny', 'name': 'Deny', 'kind': 'reject_once'}]}),
            chunk('agent_message_chunk', 'done'),
        ]
        bridge.view(SID)
        bridge.send(SID, 'run it', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['requests']))
        assessed = self.wait_for(lambda: bridge.view(SID)['requests'][0].get('assessment'))
        self.assertTrue(assessed)
        request = bridge.view(SID)['requests'][0]
        self.assertEqual(request['assessment']['risk'], 3)
        self.assertEqual(request['assessment']['riskLabel'], 'high')
        self.assertEqual(request['recommend'], 'deny')
        # The manual answer path is untouched: Jev never responds on its own.
        with self.assertRaises(ValueError):
            bridge.respond(SID, request['id'], {'optionId': 'bogus'})
        self.assertEqual(bridge.respond(SID, request['id'], {'optionId': 'deny'})['ok'], True)

    def test_riskiest_request_surfaced_first(self):
        _, bridge = self.make_bridge(lambda payload: {'answers': {}})
        safe = acp_permission()
        safe['params']['command'] = 'ls -la'
        risky = codex_command()
        bridge.assessor._cache.update({
            fingerprint(safe): {'source': 'jev', 'risk': 0, 'riskLabel': 'routine',
                                'recommend': 'accept_once', 'confidence': 0.9},
            fingerprint(risky): {'source': 'jev', 'risk': 3, 'riskLabel': 'high',
                                 'recommend': 'decline', 'confidence': 0.5},
        })
        session = bridge.session(SID, background=True)

        class Stub:
            condition = session.condition
            state = session.state

        view = bridge._annotate_assessments(Stub(), {'requests': [safe, risky]})
        ordered = view['requests']
        self.assertEqual(ordered[0]['method'], 'item/commandExecution/requestApproval')
        self.assertEqual(ordered[0]['recommend'], 'decline')
        self.assertEqual(ordered[1]['recommend'], 'allow')
        self.assertEqual(ordered[1]['assessment']['riskLabel'], 'routine')

    def test_safe_assessment_auto_responds_when_enabled(self):
        _, bridge = self.make_bridge(lambda payload: {'answers': {}})
        settings_path = Path(bridge.data_dir) / 'jev.json'
        init_shared_settings(settings_path)
        shared_settings().update({'autoApprove': True, 'baseUrl': '', 'model': ''})
        request = codex_command(('accept', 'decline'))
        request['id'] = 'safe-request'
        bridge.assessor._cache[fingerprint(request)] = {
            'source': 'jev', 'risk': 1, 'riskLabel': 'low',
            'reversible': 0.95, 'recommend': 'accept_once', 'confidence': 0.95,
        }
        session = bridge.session(SID, background=True)
        called = []
        done = threading.Event()
        bridge.respond = lambda thread_id, request_id, response: (called.append(
            (thread_id, request_id, response)), done.set(), {'status': 'accepted'})[-1]

        class Stub:
            condition = session.condition
            state = session.state
            id = SID

        bridge._annotate_assessments(Stub(), {'requests': [request]})
        self.assertTrue(done.wait(5))
        self.assertEqual(called, [(SID, 'safe-request', {'decision': 'accept'})])

    def test_disabled_assessor_keeps_plain_requests(self):
        fake, bridge = self.make_bridge(lambda payload: {'answers': {}})
        bridge.assessor = JevAssessor()
        fake.prompt_updates = [
            ('permission', {'toolCall': {'toolCallId': 't1', 'title': 'Run ls', 'kind': 'execute',
                                         'status': 'in_progress', 'rawInput': {'command': 'ls -la'}},
                            'options': [{'optionId': 'allow', 'name': 'Allow once', 'kind': 'allow_once'},
                                        {'optionId': 'deny', 'name': 'Deny', 'kind': 'reject_once'}]}),
            chunk('agent_message_chunk', 'done'),
        ]
        bridge.view(SID)
        bridge.send(SID, 'run it', str(uuid.uuid4()))
        self.assertTrue(self.wait_for(lambda: bridge.view(SID)['requests']))
        request = bridge.view(SID)['requests'][0]
        self.assertNotIn('assessment', request)
        self.assertNotIn('recommend', request)


class JevSettingsTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        self.path = Path(tmp.name) / 'jev.json'
        env = patch.dict(os.environ, {'TYPESAFE_API_KEY': 'env-key-123',
                                      'TYPESAFE_BASE_URL': 'https://env.example',
                                      'JEV_MODEL': 'env-model'})
        env.start()
        self.addCleanup(env.stop)
        init_shared_settings(self.path)

    def test_file_overrides_and_masks_env(self):
        store = shared_settings()
        self.assertEqual(store.read()['apiKey'], 'env-key-123')
        result = store.update({'apiKey': 'tsk-file-key-9999', 'baseUrl': '', 'model': ''})
        self.assertTrue(result['configured'])
        self.assertIn('…', result['apiKeyMasked'])
        self.assertNotIn('tsk-file-key-9999', json.dumps(result))
        self.assertEqual(store.read(), {'apiKey': 'tsk-file-key-9999',
                                        'baseUrl': 'https://api.typesafe.ai',
                                        'model': 'jev-latest', 'autoApprove': False})
        self.assertEqual(self.path.stat().st_mode & 0o777, 0o600)

    def test_unrelated_save_keeps_env_fallback(self):
        store = shared_settings()
        # Saving only the model must not copy or suppress the env-provided key.
        store.update({'model': 'jev-1.13.0'})
        self.assertEqual(store.read()['apiKey'], 'env-key-123')
        self.assertEqual(store.read()['model'], 'jev-1.13.0')
        self.assertNotIn('apiKey', json.loads(self.path.read_text()))

    def test_clear_disables_even_with_env(self):
        store = shared_settings()
        store.update({'apiKey': 'tsk-file-key-9999', 'baseUrl': '', 'model': ''})
        store.update({'clearApiKey': True, 'baseUrl': '', 'model': ''})
        self.assertFalse(store.public()['configured'])
        self.assertEqual(store.read()['apiKey'], '')
        self.assertEqual(effective_field('apiKey'), '')

    def test_blank_key_submission_keeps_saved_value(self):
        store = shared_settings()
        store.update({'apiKey': 'tsk-file-key-9999', 'baseUrl': '', 'model': ''})
        store.update({'apiKey': '   ', 'baseUrl': '', 'model': ''})
        self.assertEqual(store.read()['apiKey'], 'tsk-file-key-9999')

    def test_validation_rejects_bad_submissions(self):
        store = shared_settings()
        for bad in ({'apiKey': 'short'}, {'apiKey': 'has space inside'},
                    {'apiKey': 42}, {'baseUrl': 'ftp://x', 'model': ''},
                    {'baseUrl': 'http://insecure.example', 'model': ''},
                    {'baseUrl': 'https://ok.example', 'model': 'bad name'},
                    {'model': None}):
            with self.assertRaises(ValueError):
                store.update(bad)
        self.assertFalse(self.path.exists())
        # Loopback HTTP stays allowed for local testing.
        store.update({'baseUrl': 'http://127.0.0.1:9000', 'model': ''})
        self.assertEqual(store.read()['baseUrl'], 'http://127.0.0.1:9000')

    def test_reload_persists_across_instances(self):
        shared_settings().update({'apiKey': 'tsk-file-key-9999', 'baseUrl': 'https://a.example', 'model': 'm1'})
        reloaded = JevSettings(self.path)
        self.assertEqual(reloaded.read()['baseUrl'], 'https://a.example')


class LiveSettingsTests(unittest.TestCase):
    def test_bridge_assessor_follows_settings_without_restart(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        init_shared_settings(Path(tmp.name) / 'jev.json')
        bridge = Bridge(Path(tmp.name), Path(tmp.name) / 'data')
        self.addCleanup(bridge.close)
        self.assertFalse(bridge.assessor.enabled)
        shared_settings().update({'apiKey': 'tsk-live-key-42', 'baseUrl': '', 'model': ''})
        self.assertTrue(bridge.assessor.enabled)
        seen = {}
        bridge.assessor._call = lambda payload: seen.update(payload) or {'answers': {
            'risk': {'score': 1}, 'reversible': {'noul': 1},
            'recommend': {'choice': 'accept_once', 'confidence': 0.5}}}
        request = acp_permission()
        done = threading.Event()
        self.assertTrue(bridge.assessor.start(fingerprint(request), request, {},
                                              lambda _assessment: done.set()))
        self.assertTrue(done.wait(5))
        self.assertEqual(seen['model'], 'jev-latest')
        self.assertEqual(seen['state']['command'], 'ls -la')


class JevEndpointTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        init_shared_settings(Path(tmp.name) / 'jev.json')

        class EmptyBridge:
            host_errors = []
            def for_host(self, host):
                if host != 'local':
                    raise KeyError(host)
                return self
            closed = threading.Event()
            def list(self, **kwargs):
                return []
        record = password_record('correct password test')
        self.server = GatewayServer(('127.0.0.1', 0), EmptyBridge(),
                                    {'auth': {'mode': 'password', 'username': 'admin', **record}, 'origins': []},
                                    ROOT / 'web')
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.port = self.server.server_address[1]
        self.origin = 'http://127.0.0.1:' + str(self.port)
        self.server.origins.add(self.origin)
        self.server.hosts.add('127.0.0.1:' + str(self.port))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def request(self, method, path, body=None, headers=None):
        conn = http.client.HTTPConnection('127.0.0.1', self.port, timeout=3)
        base = {'Origin': self.origin, 'Content-Type': 'application/json'}
        base.update(headers or {})
        conn.request(method, path, body=json.dumps(body) if body is not None else None, headers=base)
        response = conn.getresponse()
        status, response_headers, data = response.status, dict(response.getheaders()), response.read()
        conn.close()
        return status, response_headers, json.loads(data)

    def test_settings_roundtrip_over_http(self):
        status, headers, body = self.request('POST', '/api/login', {'username': 'admin', 'password': 'correct password test'})
        self.assertEqual(status, 200)
        auth = {'Cookie': headers['Set-Cookie'].split(';')[0], 'X-CSRF-Token': body['csrf']}
        status, _headers, body = self.request('GET', '/api/jev', headers=auth)
        self.assertEqual(status, 200)
        self.assertFalse(body['configured'])
        self.assertEqual(body['baseUrl'], 'https://api.typesafe.ai')
        self.assertEqual(body['model'], 'jev-latest')
        # Writes without a CSRF token stay rejected even with the cookie.
        self.assertEqual(self.request('POST', '/api/jev', {'apiKey': 'tsk-http-key-77'},
                                      {'Cookie': auth['Cookie']})[0], 403)
        status, _headers, body = self.request('POST', '/api/jev', {'apiKey': 'tsk-http-key-77', 'baseUrl': '', 'model': ''}, auth)
        self.assertEqual(status, 200)
        self.assertTrue(body['configured'])
        self.assertIn('…', body['apiKeyMasked'])
        status, _headers, body = self.request('GET', '/api/jev', headers=auth)
        self.assertTrue(body['configured'])
        status, _headers, body = self.request('POST', '/api/jev', {'baseUrl': 'not a url', 'model': ''}, auth)
        self.assertEqual(status, 400)


def tearDownModule():
    # Leave the shared store pointing at an empty file so later test modules
    # with permission flows never inherit this module's keys.
    (ROOT / '.tmp').mkdir(exist_ok=True)
    init_shared_settings(ROOT / '.tmp' / 'jev-test-reset.json')


if __name__ == '__main__':
    unittest.main()
