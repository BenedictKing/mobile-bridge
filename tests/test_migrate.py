"""Cross-agent session migration: transcript building, routing, idempotence."""
import sqlite3
import tempfile
import unittest
import uuid
from pathlib import Path

from bridge.agents.acp_adapter import AcpAdapter
from bridge.agents.kimi import KimiAdapter
from bridge.ipc import IPCError
from bridge.service import (Bridge, MIGRATION_ENTRY_CHARS, MIGRATION_MAX_CHARS,
                            MIGRATION_MAX_MESSAGES, migration_message, migration_transcript)
from tests.fake_acp import FakeAcpAgent
from tests.test_bridge import THREAD, DesktopFixture

ROOT = Path(__file__).resolve().parents[1]
UPDATED = '2026-01-02T03:04:05Z'
SID_SOURCE = 'session_source_001'


def chunk(kind, text):
    return {'sessionUpdate': kind, 'content': {'type': 'text', 'text': text}}


class TranscriptTests(unittest.TestCase):
    """Pure transcript builder: role flattening, caps, locales."""

    def view(self, messages):
        return {'title': 'T', 'cwd': '/work',
                'turns': [{'id': 't1', 'status': 'completed', 'messages': messages}]}

    def test_flattens_roles_and_compresses_activities(self):
        view = self.view([
            {'role': 'user', 'text': '部署一下'},
            {'role': 'activity', 'title': '执行命令', 'text': './deploy.sh --prod\n第二行', 'output': '大量输出'},
            {'role': 'assistant', 'text': '已完成'},
            {'role': 'error', 'text': '尾部报错'},
            {'role': 'activity', 'title': '', 'text': ''},
        ])
        entries, truncated = migration_transcript(view)
        self.assertFalse(truncated)
        self.assertEqual(entries, [('user', '部署一下'),
                                 ('activity', '执行命令：./deploy.sh --prod'),
                                 ('assistant', '已完成'),
                                 ('error', '尾部报错')])

    def test_message_cap_keeps_the_tail(self):
        view = self.view([{'role': 'user', 'text': f'message-{i}'} for i in range(MIGRATION_MAX_MESSAGES + 5)])
        entries, truncated = migration_transcript(view)
        self.assertTrue(truncated)
        self.assertEqual(len(entries), MIGRATION_MAX_MESSAGES)
        self.assertEqual(entries[0], ('user', 'message-5'))

    def test_char_cap_and_per_entry_cap(self):
        # The per-entry cap alone cannot push a short dialogue over the total
        # cap, so use several near-cap entries to force tail-dropping.
        view = self.view([{'role': 'assistant', 'text': f'{i}:' + 'x' * 3900} for i in range(10)])
        entries, truncated = migration_transcript(view)
        self.assertTrue(truncated)
        self.assertLessEqual(sum(len(text) + 8 for _, text in entries), MIGRATION_MAX_CHARS + 8)
        huge = 'x' * (MIGRATION_ENTRY_CHARS + 100)
        entries, truncated = migration_transcript(self.view([{'role': 'user', 'text': huge}]))
        self.assertFalse(truncated)
        self.assertTrue(entries[0][1].endswith('…'))
        self.assertEqual(len(entries[0][1]), MIGRATION_ENTRY_CHARS + 1)

    def test_message_template_locales(self):
        view = {'title': 'Old chat', 'cwd': '/work',
                'turns': [{'id': 't', 'messages': [{'role': 'user', 'text': 'hi'}]}]}
        zh = migration_message('Kimi', view)
        self.assertIn('【从 Kimi 迁移的会话】', zh)
        self.assertIn('标题：Old chat', zh)
        self.assertIn('目录：/work', zh)
        self.assertIn('用户：hi', zh)
        self.assertIn('请在此基础上继续。', zh)
        en = migration_message('Kimi', view, 'en')
        self.assertIn('[Session migrated from Kimi]', en)
        self.assertIn('User: hi', en)
        self.assertIn('Please continue from this context.', en)


class MigrateBridgeTests(unittest.TestCase):
    """Bridge.migrate across fake ACP agents and a codex desktop fixture."""

    def make_acp_bridge(self, fake, adapter_cls=KimiAdapter, name='kimi'):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        adapter = adapter_cls('fake-acp', data_dir=root / 'data', spawn=fake.spawn_factory())
        bridge = Bridge(root, root / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        return bridge

    def make_acp_pair(self, target_cwd='/work'):
        source_fake = FakeAcpAgent(
            list_rows=[{'sessionId': SID_SOURCE, 'cwd': '/work', 'title': '源会话', 'updatedAt': UPDATED}])
        source_fake.load_updates = [chunk('user_message_chunk', '帮我重构登录模块'),
                                    chunk('agent_message_chunk', '已完成初步重构')]
        target_fake = FakeAcpAgent(
            list_rows=[{'sessionId': 'session_target_001', 'cwd': target_cwd, 'title': 'T', 'updatedAt': UPDATED}])
        source = self.make_acp_bridge(source_fake)
        target = self.make_acp_bridge(target_fake, adapter_cls=AcpAdapter)
        return source_fake, source, target_fake, target

    def prompts(self, fake):
        return [r for r in fake.requests if r['method'] == 'session/prompt']

    def test_acp_to_acp_migrates_transcript_and_is_idempotent(self):
        source_fake, source, target_fake, target = self.make_acp_pair()
        request_id = str(uuid.uuid4())
        result = source.migrate(SID_SOURCE, target, request_id=request_id)
        self.assertEqual(result['agent'], 'acp')
        self.assertEqual(result['id'], 'fake-1')
        self.assertEqual(result['status'], 'sent')
        self.assertTrue(result['sent'])
        prompts = self.prompts(target_fake)
        self.assertEqual(len(prompts), 1)
        text = prompts[0]['params']['prompt'][0]['text']
        self.assertIn('【从 Kimi 迁移的会话】', text)
        self.assertIn('标题：源会话', text)
        self.assertIn('目录：/work', text)
        self.assertIn('用户：帮我重构登录模块', text)
        self.assertIn('助手：已完成初步重构', text)
        # The source agent never receives a prompt: read-only migration.
        self.assertEqual(self.prompts(source_fake), [])
        # A retry with the same request id neither recreates nor re-sends.
        again = source.migrate(SID_SOURCE, target, request_id=request_id)
        self.assertEqual(again['id'], 'fake-1')
        self.assertEqual(target_fake.session_count, 1)
        self.assertEqual(len(self.prompts(target_fake)), 1)

    def test_explicit_project_key_and_mismatch_errors(self):
        _, source, target_fake, target = self.make_acp_pair(target_cwd='/elsewhere')
        project = target.adapter.projects()[0]
        result = source.migrate(SID_SOURCE, target, project_key=project['key'], request_id=str(uuid.uuid4()))
        self.assertEqual(result['project'], project['key'])
        with self.assertRaises(ValueError) as ctx:
            source.migrate(SID_SOURCE, target, request_id=str(uuid.uuid4()))
        self.assertIn('相同目录的项目', str(ctx.exception))
        with self.assertRaises(ValueError):
            source.migrate(SID_SOURCE, target, project_key='bogus', request_id=str(uuid.uuid4()))
        with self.assertRaises(KeyError):
            source.migrate('session_missing_0000', target, request_id=str(uuid.uuid4()))
        with self.assertRaises(ValueError):
            source.migrate(SID_SOURCE, target, request_id='not-a-uuid')

    def test_send_failure_keeps_created_session_with_warning(self):
        _, source, _, _ = self.make_acp_pair()

        class FailingAdapter:
            kind = 'codex'
            label = 'Codex'
            def projects(self):
                return [{'key': 'k', 'cwd': '/work', 'host': 'local'}]

        class FailingTarget:
            adapter = FailingAdapter()
            def create_chat(self, project, title, request_id):
                return {'id': 'created-1', 'host': 'local'}
            def send(self, *args, **kwargs):
                raise IPCError('桌面连接中断')

        result = source.migrate(SID_SOURCE, FailingTarget(), request_id=str(uuid.uuid4()))
        self.assertEqual(result['status'], 'created')
        self.assertFalse(result['sent'])
        self.assertEqual(result['id'], 'created-1')
        self.assertEqual(result['warning'], '桌面连接中断')


class MigrateCodexSourceTests(unittest.TestCase):
    """A codex desktop chat migrates into an ACP agent without source writes."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.fixture = DesktopFixture(self.root)
        self.addCleanup(self.fixture.close)
        db = sqlite3.connect(str(self.root / 'state_5.sqlite'))
        db.execute('CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT, cwd TEXT, updated_at INT, archived INT, originator TEXT, source TEXT, rollout_path TEXT)')
        db.execute('INSERT INTO threads VALUES(?,?,?,?,?,?,?,?)', (THREAD, 'Desktop test', '/work', 1, 0, 'Codex Desktop', 'vscode', 'unused'))
        db.commit()
        db.close()
        self.fixture.state['cwd'] = '/work'
        self.fixture.state['turns'] = [{'turnId': 't1', 'status': 'completed', 'items': [
            {'id': 'u1', 'type': 'userMessage', 'content': [{'type': 'text', 'text': '优化构建速度'}]},
            {'id': 'c1', 'type': 'commandExecution', 'command': 'npm run build', 'status': 'completed', 'aggregatedOutput': 'ok'},
            {'id': 'a1', 'type': 'agentMessage', 'text': '已开启增量构建'},
        ]}]
        self.source = Bridge(self.root, self.root / 'data')
        self.addCleanup(self.source.close)
        self.source.ipc.path = self.fixture.path
        target_fake = FakeAcpAgent(
            list_rows=[{'sessionId': 'session_target_001', 'cwd': '/work', 'title': 'T', 'updatedAt': UPDATED}])
        self.target_fake = target_fake
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        target_root = Path(tmp.name)
        adapter = KimiAdapter('fake-acp', data_dir=target_root / 'data', spawn=target_fake.spawn_factory())
        self.target = Bridge(target_root, target_root / 'data', adapter=adapter)
        self.addCleanup(self.target.close)

    def test_codex_source_migrates_to_acp(self):
        result = self.source.migrate(THREAD, self.target, request_id=str(uuid.uuid4()), ui_locale='en')
        self.assertEqual(result['status'], 'sent')
        self.assertEqual(result['agent'], 'kimi')
        prompts = [r for r in self.target_fake.requests if r['method'] == 'session/prompt']
        self.assertEqual(len(prompts), 1)
        text = prompts[0]['params']['prompt'][0]['text']
        self.assertIn('[Session migrated from Codex]', text)
        self.assertIn('User: 优化构建速度', text)
        self.assertIn('Assistant: 已开启增量构建', text)
        self.assertIn('Activity: 执行命令：npm run build', text)
        # Read-only source: only follower/attach traffic, never a turn start.
        self.assertFalse(any(r['method'] == 'thread-follower-start-turn' for r in self.fixture.requests))


if __name__ == '__main__':
    unittest.main()
