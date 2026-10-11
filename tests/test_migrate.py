"""Cross-agent session migration: transcript building, routing, idempotence."""
import hashlib
import sqlite3
import tempfile
import types
import unittest
import uuid
from pathlib import Path

from bridge.agents.acp_adapter import AcpAdapter
from bridge.agents.base import AgentAdapter
from bridge.agents.kimi import KimiAdapter
from bridge.ipc import IPCError
from bridge.service import (Bridge, MIGRATION_ENTRY_CHARS, MIGRATION_MAX_CHARS,
                            MIGRATION_MAX_MESSAGES, MIGRATION_SCOPE_BUDGETS,
                            migration_message, migration_scope, migration_transcript)
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

    def test_scope_full_raises_caps(self):
        count = MIGRATION_MAX_MESSAGES + 80
        view = self.view([{'role': 'user', 'text': f'message-{i}'} for i in range(count)])
        entries, truncated = migration_transcript(view, scope='full')
        self.assertFalse(truncated)
        self.assertEqual(len(entries), count)
        self.assertEqual(entries[0], ('user', 'message-0'))
        # The per-entry cap also grows: a 5k message survives full but is cut in main.
        long = 'y' * (MIGRATION_ENTRY_CHARS + 1000)
        full_entries, _ = migration_transcript(self.view([{'role': 'user', 'text': long}]), scope='full')
        self.assertFalse(full_entries[0][1].endswith('…'))
        main_entries, _ = migration_transcript(self.view([{'role': 'user', 'text': long}]))
        self.assertTrue(main_entries[0][1].endswith('…'))

    def test_scope_latest_keeps_final_question_and_replies(self):
        view = self.view([
            {'role': 'user', 'text': '旧提问'},
            {'role': 'assistant', 'text': '旧回答'},
            {'role': 'activity', 'title': '执行命令', 'text': 'ls'},
            {'role': 'user', 'text': '最新提问'},
            {'role': 'assistant', 'text': '回答一'},
            {'role': 'assistant', 'text': '回答二（计划）'},
        ])
        entries, truncated = migration_transcript(view, scope='latest')
        self.assertFalse(truncated)
        self.assertEqual(entries, [('user', '最新提问'), ('assistant', '回答一'), ('assistant', '回答二（计划）')])
        # Without any user message, keep only the final assistant reply.
        entries, _ = migration_transcript(self.view([{'role': 'assistant', 'text': '仅回答'}]), scope='latest')
        self.assertEqual(entries, [('assistant', '仅回答')])
        self.assertEqual(migration_transcript(self.view([]), scope='latest'), ([], False))

    def test_scope_latest_message_template(self):
        view = {'title': 'Old chat', 'cwd': '/work',
                'turns': [{'id': 't', 'messages': [{'role': 'user', 'text': '最新提问'},
                                                   {'role': 'assistant', 'text': '最新回答'}]}]}
        zh = migration_message('Kimi', view, scope='latest')
        self.assertIn('【来自 Kimi 的最新问答】', zh)
        self.assertIn('用户：最新提问', zh)
        self.assertIn('助手：最新回答', zh)
        self.assertIn('最近一次提问与回答，请在此基础上继续。', zh)
        en = migration_message('Kimi', view, 'en', scope='latest')
        self.assertIn('[Latest exchange from Kimi]', en)
        self.assertIn('latest question and answer from Kimi', en)

    def test_invalid_scope_rejected(self):
        self.assertEqual(migration_scope(None), 'main')
        for bad in ('everything', '', 3):
            with self.assertRaises(ValueError):
                migration_scope(bad)
        with self.assertRaises(ValueError):
            migration_transcript(self.view([]), scope='everything')
        self.assertIn('main', MIGRATION_SCOPE_BUDGETS)


class MigrateBridgeTests(unittest.TestCase):
    """Bridge.migrate across fake ACP agents and a codex desktop fixture."""

    def make_acp_bridge(self, fake, adapter_cls=KimiAdapter, name='kimi', host='local'):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        adapter = adapter_cls('fake-acp', data_dir=root / 'data', spawn=fake.spawn_factory())
        if host != 'local':
            adapter.hosts = types.SimpleNamespace(
                hosts=lambda: {host: {'alias': host, 'displayName': host}})
        bridge = Bridge(root, root / 'data', host=host, adapter=adapter)
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

    def test_explicit_project_key_and_auto_creation(self):
        _, source, target_fake, target = self.make_acp_pair(target_cwd='/elsewhere')
        project = target.adapter.projects()[0]
        result = source.migrate(SID_SOURCE, target, project_key=project['key'], request_id=str(uuid.uuid4()))
        self.assertEqual(result['project'], project['key'])
        self.assertFalse(result['projectCreated'])
        # The target has no matching directory: the source cwd is created on it directly.
        result = source.migrate(SID_SOURCE, target, request_id=str(uuid.uuid4()))
        self.assertEqual(result['project'], 'acp:' + hashlib.sha256(b'/work').hexdigest()[:12])
        self.assertTrue(result['projectCreated'])
        self.assertEqual(result['status'], 'sent')
        created = [r for r in target_fake.requests if r['method'] == 'session/new'][-1]
        self.assertEqual(created['params']['cwd'], '/work')
        with self.assertRaises(ValueError):
            source.migrate(SID_SOURCE, target, project_key='bogus', request_id=str(uuid.uuid4()))
        with self.assertRaises(KeyError):
            source.migrate('session_missing_0000', target, request_id=str(uuid.uuid4()))
        with self.assertRaises(ValueError):
            source.migrate(SID_SOURCE, target, request_id='not-a-uuid')
        # Remote-host sources keep the explicit-project requirement: their cwd
        # is a remote path that must not become a local project on the target.
        remote = self.make_acp_bridge(FakeAcpAgent(
            list_rows=[{'sessionId': SID_SOURCE, 'cwd': '/home/user/proj', 'title': '远端', 'updatedAt': UPDATED}]),
            host='myserver')
        with self.assertRaises(ValueError) as ctx:
            remote.migrate(SID_SOURCE, target, request_id=str(uuid.uuid4()))
        self.assertIn('相同目录的项目', str(ctx.exception))

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

    def test_latest_scope_copies_final_exchange_only(self):
        source_fake, source, target_fake, target = self.make_acp_pair()
        source_fake.load_updates = [chunk('user_message_chunk', '第一问'),
                                    chunk('agent_message_chunk', '旧回答'),
                                    chunk('user_message_chunk', '最新提问'),
                                    chunk('agent_message_chunk', '最新回答，含计划')]
        result = source.migrate(SID_SOURCE, target, request_id=str(uuid.uuid4()), scope='latest')
        self.assertEqual(result['status'], 'sent')
        text = self.prompts(target_fake)[0]['params']['prompt'][0]['text']
        self.assertIn('【来自 Kimi 的最新问答】', text)
        self.assertIn('用户：最新提问', text)
        self.assertIn('助手：最新回答，含计划', text)
        self.assertNotIn('第一问', text)
        self.assertNotIn('旧回答', text)

    def test_invalid_scope_rejected_at_bridge(self):
        _, source, _, target = self.make_acp_pair()
        with self.assertRaises(ValueError):
            source.migrate(SID_SOURCE, target, request_id=str(uuid.uuid4()), scope='everything')


def raw_turn(index):
    return {'turnId': f't{index}', 'status': 'completed', 'items': [
        {'id': f'u{index}', 'type': 'userMessage', 'content': [{'type': 'text', 'text': f'问题 {index}'}]},
        {'id': f'a{index}', 'type': 'agentMessage', 'text': f'回答 {index}'}]}


class PagingAdapter(AgentAdapter):
    """Codex-like source whose saved rollout pages in from the tail."""

    kind = 'stub'
    label = 'Stub'
    # Attributes the bridge delegates to its adapter; unused by this stub.
    goal_transport = None
    accounts = None

    def __init__(self, turns):
        self.turns = turns
        self.reads = []

    def attach(self, session, timeout=1.5):
        pass  # The desktop never connects; saved history serves reads.

    def session_meta(self, thread_id):
        return {'id': thread_id, 'title': 'Paged', 'cwd': '/work'}

    def saved_history(self, session):
        self.reads.append(session.history_limit)
        complete = session.history_limit is None
        return {'id': session.id, 'title': 'Paged', 'cwd': '/work',
                'turns': self.turns if complete else self.turns[-1:],
                'turnsPagination': {'hasLoadedOldest': complete}}


class MigrateFullScopeTests(unittest.TestCase):
    """A full-scope migration must read past the saved-history first page."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(self.temp.cleanup)
        root = Path(self.temp.name)
        self.adapter = PagingAdapter([raw_turn(i) for i in range(25)])
        self.source = Bridge(root, root / 'data', adapter=self.adapter)
        self.addCleanup(self.source.close)
        target_fake = FakeAcpAgent(
            list_rows=[{'sessionId': 'session_target_001', 'cwd': '/work', 'title': 'T', 'updatedAt': UPDATED}])
        target_tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(target_tmp.cleanup)
        target_root = Path(target_tmp.name)
        self.target_fake = target_fake
        self.target = Bridge(target_root, target_root / 'data',
                             adapter=KimiAdapter('fake-acp', data_dir=target_root / 'data',
                                                 spawn=target_fake.spawn_factory()))
        self.addCleanup(self.target.close)

    def prompt_text(self):
        return next(r for r in self.target_fake.requests if r['method'] == 'session/prompt'
                    )['params']['prompt'][0]['text']

    def test_full_scope_reads_complete_saved_history(self):
        result = self.source.migrate('thread_full_001', self.target, request_id=str(uuid.uuid4()), scope='full')
        self.assertEqual(result['status'], 'sent')
        # The expansion re-read saved history without a turn limit.
        self.assertEqual(self.adapter.reads, [20, None])
        text = self.prompt_text()
        self.assertIn('问题 0', text)
        self.assertIn('回答 24', text)

    def test_main_scope_keeps_first_page_only(self):
        result = self.source.migrate('thread_full_001', self.target, request_id=str(uuid.uuid4()))
        self.assertEqual(result['status'], 'sent')
        self.assertEqual(self.adapter.reads, [20])  # No expansion outside full scope.
        text = self.prompt_text()
        self.assertNotIn('问题 0', text)
        self.assertIn('问题 24', text)


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
