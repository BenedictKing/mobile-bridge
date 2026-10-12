"""定时会话发现：列表版本信号与各 bridge 的本机发现读取。"""
import http.client
import json
import threading
import time
import unittest
import uuid
from pathlib import Path
from types import SimpleNamespace

from bridge.agents.base import AgentAdapter
from bridge.discovery import ListDiscovery
from bridge.httpd import GatewayServer
from bridge.service import Bridge

ROOT = Path(__file__).resolve().parent.parent
THREAD = str(uuid.uuid4())


class DiscoveryAdapter(AgentAdapter):
    kind = 'fake'
    label = 'Fake Agent'
    accounts = None

    def __init__(self, rows):
        super().__init__()
        self.rows = rows
        self.refreshes = 0

    def refresh_list(self):
        self.refreshes += 1

    def list_sessions(self, label, limit=100, offset=0, query='', archived=False):
        return self.rows[:limit]


def row(id, title='聊天', project='local|p', recency=1000):
    return {'id': id, 'title': title, 'projectKey': project, 'recency': recency}


class ListDiscoveryTests(unittest.TestCase):
    def setUp(self):
        self.adapter = DiscoveryAdapter([row('one'), row('two', project='local|q', recency=2000)])
        import tempfile
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.bridge = Bridge(Path(self.tmp.name) / 'home', Path(self.tmp.name) / 'data', adapter=self.adapter)
        self.addCleanup(self.bridge.close)
        self.discovery = ListDiscovery({'fake': self.bridge})

    def test_first_refresh_versions_and_feeds_routing_set(self):
        self.assertEqual(self.discovery.refresh(), [
            ('fake', 'one', '聊天', 'local|p'), ('fake', 'two', '聊天', 'local|q')])
        self.assertEqual(self.discovery.revision, 1)
        self.assertEqual(self.bridge.listed, {'one', 'two'})
        self.assertEqual(self.adapter.refreshes, 1)

    def test_identity_title_or_project_changes_bump_revision(self):
        self.discovery.refresh()
        self.adapter.rows.append(row('three'))
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 2)
        self.assertIn('three', self.bridge.listed)
        self.adapter.rows[0]['title'] = '改名'
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 3)
        self.adapter.rows[1]['projectKey'] = 'local|moved'
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 4)

    def test_recency_only_changes_do_not_reload_the_list(self):
        self.discovery.refresh()
        self.adapter.rows[0]['recency'] = time.time() * 1000
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 1)

    def test_disappearing_sessions_bump_revision(self):
        self.discovery.refresh()
        self.adapter.rows.clear()
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 2)

    def test_one_failing_agent_does_not_block_the_others(self):
        broken = SimpleNamespace(refresh_discovery=self._fail, adapter=SimpleNamespace(refresh_list=self._fail))
        self.discovery.bridges['broken'] = broken
        self.discovery.refresh()
        self.assertEqual(self.discovery.revision, 1)

    @staticmethod
    def _fail():
        raise OSError('agent offline')

    def test_refresh_only_runs_while_watched(self):
        self.discovery.INTERVAL = 0.02
        self.discovery.IDLE_AFTER = 0.5
        self.discovery._watched = time.monotonic() - 1
        self.discovery.start()
        self.addCleanup(self.discovery.close)
        time.sleep(0.15)
        self.assertEqual(self.adapter.refreshes, 0)
        self.discovery.touch()
        self.assertTrue(self.discovery._watched_recently())
        time.sleep(0.15)
        # 线程节拍不保证精确次数，但被观看后必须开始刷新。
        self.assertGreaterEqual(self.adapter.refreshes, 1)


class ActivityRevisionTests(unittest.TestCase):
    def setUp(self):
        class EmptyBridge:
            host_errors = []
            closed = threading.Event()

            def for_host(self, host):
                if host != 'local':
                    raise KeyError(host)
                return self

            def list(self, **kwargs):
                return []

        config = {'auth': {'mode': 'none'}, 'origins': []}
        self.discovery = ListDiscovery({})
        self.server = GatewayServer(('127.0.0.1', 0), EmptyBridge(), config, ROOT / 'web',
                                    discovery=self.discovery)
        self.port = self.server.server_address[1]
        self.origin = 'http://127.0.0.1:' + str(self.port)
        self.server.origins.add(self.origin)
        self.server.hosts.add('127.0.0.1:' + str(self.port))
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)

    def request(self, method, path, body=None, auth=None):
        conn = http.client.HTTPConnection('127.0.0.1', self.port, timeout=3)
        headers = {'Origin': self.origin, 'Content-Type': 'application/json'}
        if auth:
            headers.update(auth)
        conn.request(method, path, body=json.dumps(body) if body is not None else None, headers=headers)
        response = conn.getresponse()
        data = response.read()
        cookie = response.getheader('Set-Cookie')
        conn.close()
        return response.status, json.loads(data), cookie

    def authorize(self):
        status, body, cookie = self.request('POST', '/api/login', {'username': '', 'password': ''})
        self.assertEqual(status, 200)
        return {'Cookie': cookie.split(';')[0], 'X-CSRF-Token': body['csrf']}

    def test_activity_carries_revision_and_keeps_discovery_awake(self):
        auth = self.authorize()
        self.discovery._watched = time.monotonic() - 10 * 60
        status, body, _ = self.request('POST', '/api/activity', {'hosts': {}}, auth)
        self.assertEqual(status, 200)
        self.assertEqual(body['sessions'], [])
        self.assertEqual(body['listRevision'], 0)
        self.assertTrue(self.discovery._watched_recently())

    def test_activity_without_discovery_omits_revision(self):
        auth = self.authorize()
        self.server.discovery = None
        status, body, _ = self.request('POST', '/api/activity', {'hosts': {}}, auth)
        self.assertEqual(status, 200)
        self.assertNotIn('listRevision', body)


if __name__ == '__main__':
    unittest.main()
