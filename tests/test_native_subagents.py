"""Native sub-agent discovery: synthetic on-disk layouts per ACP agent."""
import json
import tempfile
import time
import unittest
import uuid
from pathlib import Path

from bridge.agents.kimi import KimiAdapter
from bridge.agents.native_subagents import (ClaudeSubagentStore, KimiSubagentStore,
                                            ZcodeSubagentStore)
from bridge.service import Bridge
from tests.fake_acp import FakeAcpAgent
from tests.test_acp_adapter import CONFIG_OPTIONS, MODES, SID, UPDATED

ROOT = Path(__file__).resolve().parents[1]


def write_lines(path, rows):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text('\n'.join(json.dumps(row, ensure_ascii=False) for row in rows) + '\n',
                    encoding='utf-8')


class TmpRoot(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)


class KimiStoreTests(TmpRoot):
    def build(self):
        session_dir = self.root / 'sess'
        write_lines(self.root / 'session_index.jsonl',
                    [{'sessionId': 'session_s1', 'sessionDir': str(session_dir), 'workDir': '/work'}])
        (session_dir / 'state.json').parent.mkdir(parents=True, exist_ok=True)
        (session_dir / 'state.json').write_text(json.dumps({
            'id': 'session_s1', 'title': 'parent', 'updatedAt': 1788275008531,
            'agents': {'main': {'type': 'main'},
                       'agent-0': {'type': 'sub', 'parentAgentId': 'main'},
                       'notes': 'not an agent dict'},
        }), encoding='utf-8')
        write_lines(session_dir / 'agents' / 'main' / 'wire.jsonl', [
            {'type': 'metadata', 'protocol_version': '1.5'},
            {'type': 'task.started', 'agentId': 'main',
             'info': {'kind': 'agent', 'agentId': 'agent-0', 'subagentType': 'explore',
                      'status': 'running', 'parentToolCallId': 'tool_spawn'}},
            {'type': 'task.terminated', 'agentId': 'main',
             'info': {'kind': 'agent', 'agentId': 'agent-0', 'status': 'completed'}},
        ])
        write_lines(session_dir / 'agents' / 'agent-0' / 'wire.jsonl', [
            {'type': 'metadata', 'protocol_version': '1.5'},
            {'type': 'turn.prompt', 'input': [{'type': 'text', 'text': 'find the bug'}], 'origin': {'kind': 'system_trigger'}},
            {'type': 'context.append_loop_event', 'agentId': 'agent-0',
             'event': {'type': 'tool.call', 'name': 'grep', 'toolCallId': 'tc1', 'args': {'q': 'TODO'}}},
            {'type': 'context.append_loop_event', 'agentId': 'agent-0',
             'event': {'type': 'tool.result', 'toolCallId': 'tc1', 'result': '3 matches'}},
            {'type': 'context.append_message', 'message': {'role': 'assistant',
                                                           'content': [{'type': 'text', 'text': 'found it'}]}},
            {'type': 'turn.ended', 'turnId': 0, 'reason': 'completed'},
        ])
        return KimiSubagentStore(self.root)

    def test_children_reads_state_and_spawn_type(self):
        children = self.build().children('session_s1')
        self.assertEqual(len(children), 1)
        child = children[0]
        # Child ids carry the parent prefix: 'agent-0' alone repeats in every
        # kimi session.
        self.assertEqual(child['id'], 'session_s1/agent-0')
        self.assertEqual(child['name'], 'agent-0')
        self.assertEqual(child['agent_role'], 'explore')
        self.assertEqual(child['updated_at_ms'], 1788275008531)

    def test_history_projects_prompt_tools_and_reply(self):
        store = self.build()
        children = store.children('session_s1')
        state = store.history(children[0]['id'])
        items = state['turns'][0]['items']
        self.assertEqual(items[0]['type'], 'userMessage')
        self.assertEqual(items[0]['content'][0]['text'], 'find the bug')
        tool = next(item for item in items if item['type'] == 'toolCall')
        self.assertEqual(tool['title'], 'grep')
        self.assertEqual(tool['output'], '3 matches')
        self.assertEqual(items[-1]['type'], 'agentMessage')
        self.assertEqual(items[-1]['text'], 'found it')

    def test_unknown_session_has_no_children(self):
        self.assertEqual(self.build().children('session_missing'), [])
        self.assertEqual(self.build().history('session_s1/agent-9'), {})

    def test_same_agent_name_in_two_sessions_stays_distinct(self):
        store = self.build()
        second = self.root / 'sess2'
        write_lines(self.root / 'session_index.jsonl',
                    [{'sessionId': 'session_s1', 'sessionDir': str(self.root / 'sess')},  # noqa: F601 - rebuilt below
                     {'sessionId': 'session_s2', 'sessionDir': str(second)}])
        (second).mkdir(parents=True, exist_ok=True)
        (second / 'state.json').write_text(json.dumps(
            {'agents': {'agent-0': {'type': 'sub'}}, 'updatedAt': 2}), encoding='utf-8')
        write_lines(second / 'agents' / 'agent-0' / 'wire.jsonl', [
            {'type': 'turn.prompt', 'input': [{'type': 'text', 'text': 'other session'}]},
            {'type': 'context.append_message', 'message': {'role': 'assistant',
                                                           'content': [{'type': 'text', 'text': 'other reply'}]}},
            {'type': 'turn.ended', 'reason': 'completed'},
        ])
        first_items = store.history(store.children('session_s1')[0]['id'])['turns'][0]['items']
        second_items = store.history(store.children('session_s2')[0]['id'])['turns'][0]['items']
        self.assertEqual(first_items[0]['content'][0]['text'], 'find the bug')
        self.assertEqual(second_items[0]['content'][0]['text'], 'other session')
        self.assertEqual(second_items[-1]['text'], 'other reply')


class ZcodeStoreTests(TmpRoot):
    def build(self):
        (self.root / 'v2').mkdir(parents=True)
        (self.root / 'v2' / 'acp-lazy-sessions.json').write_text(json.dumps(
            {'acp-uuid-1': {'cwd': '/work', 'zcodeSid': 'sess_backend1'}}), encoding='utf-8')
        agent_dir = self.root / 'cli' / 'agents' / 'sess_backend1' / 'agent_aa'
        (agent_dir).mkdir(parents=True)
        (agent_dir / 'metadata.json').write_text(json.dumps({
            'childSessionId': 'sess_subagent_aa', 'parentSessionId': 'sess_backend1',
            'description': '验收图表', 'profileId': 'judge', 'status': 'completed',
            'updatedAt': '2026-09-05T12:48:36.950Z', 'prompt': 'check the charts',
        }), encoding='utf-8')
        write_lines(agent_dir / 'transcript.jsonl', [
            {'type': 'turn_started', 'payload': {'input': 'check the charts'}},
            {'type': 'model_streaming', 'payload': {'delta': 'noise'}},
            {'type': 'tool_call_scheduled', 'payload': {'toolCallId': 'c1', 'toolName': 'Read', 'input': {'path': 'a.py'}}},
            {'type': 'model_complete', 'payload': {'content': ''}},
            {'type': 'model_complete', 'payload': {'content': 'charts look right'}},
            {'type': 'turn_complete', 'payload': {'resultType': 'success', 'response': 'done'}},
        ])
        return ZcodeSubagentStore(self.root)

    def test_children_maps_acp_id_to_backend_session(self):
        children = self.build().children('acp-uuid-1')
        self.assertEqual(len(children), 1)
        self.assertEqual(children[0]['id'], 'sess_subagent_aa')
        self.assertEqual(children[0]['agent_role'], 'judge')
        self.assertEqual(children[0]['name'], '验收图表')
        self.assertGreater(children[0]['updated_at_ms'], 0)

    def test_history_skips_streaming_noise(self):
        store = self.build()
        state = store.history(store.children('acp-uuid-1')[0]['id'])
        items = state['turns'][0]['items']
        kinds = [item['type'] for item in items]
        self.assertEqual(kinds, ['userMessage', 'toolCall', 'agentMessage', 'agentMessage'])
        self.assertEqual(items[1]['title'], 'Read')

    def test_unmapped_acp_session_yields_nothing(self):
        self.assertEqual(self.build().children('not-in-map'), [])


class ClaudeStoreTests(TmpRoot):
    def build(self):
        subagents = self.root / 'projects' / 'slug' / 'parent-1' / 'subagents'
        write_lines(subagents / 'agent-explore-ff89.jsonl', [
            {'type': 'user', 'isSidechain': True, 'agentId': 'agent-explore-ff89',
             'message': {'role': 'user', 'content': 'map the repo'}, 'timestamp': '2026-10-01T09:00:00Z'},
            {'type': 'assistant', 'message': {'role': 'assistant', 'content': [
                {'type': 'tool_use', 'id': 'tu1', 'name': 'Grep', 'input': {'q': 'TODO'}}]}},
            {'type': 'user', 'message': {'role': 'user', 'content': [
                {'type': 'tool_result', 'tool_use_id': 'tu1', 'content': 'nothing'}]}},
            {'type': 'assistant', 'message': {'role': 'assistant', 'content': [
                {'type': 'text', 'text': 'repo mapped'}]}},
        ])
        return ClaudeSubagentStore(self.root)

    def test_children_find_sidechain_files_by_parent_id(self):
        children = self.build().children('parent-1')
        self.assertEqual(len(children), 1)
        self.assertEqual(children[0]['agent_role'], 'explore')
        self.assertEqual(children[0]['first_user_message'], 'map the repo')
        self.assertGreater(children[0]['updated_at_ms'], 0)
        self.assertEqual(self.build().children('unknown-id'), [])

    def test_history_pairs_tool_results_with_calls(self):
        store = self.build()
        child = store.children('parent-1')[0]
        state = store.history(child['id'])
        items = state['turns'][0]['items']
        self.assertEqual(items[0]['type'], 'userMessage')
        tool = next(item for item in items if item['type'] == 'toolCall')
        self.assertEqual(tool['title'], 'Grep')
        self.assertEqual(tool['output'], 'nothing')
        self.assertEqual(items[-1]['text'], 'repo mapped')


class BridgeSubagentIntegration(TmpRoot):
    def test_kimi_children_render_in_the_timeline(self):
        session_dir = self.root / 'kimi-sess'
        write_lines(self.root / 'session_index.jsonl',
                    [{'sessionId': SID, 'sessionDir': str(session_dir), 'workDir': '/work'}])
        (session_dir / 'state.json').parent.mkdir(parents=True, exist_ok=True)
        (session_dir / 'state.json').write_text(json.dumps({
            'id': SID, 'title': 'parent', 'updatedAt': 1788275008531,
            'agents': {'agent-0': {'type': 'sub', 'parentAgentId': 'main'}}}), encoding='utf-8')
        write_lines(session_dir / 'agents' / 'agent-0' / 'wire.jsonl', [
            {'type': 'turn.prompt', 'input': [{'type': 'text', 'text': 'explore the store'}]},
            {'type': 'context.append_message', 'message': {'role': 'assistant',
                                                           'content': [{'type': 'text', 'text': 'done exploring'}]}},
            {'type': 'turn.ended', 'reason': 'completed'},
        ])
        fake = FakeAcpAgent(
            config_options=[dict(o, options=list(o['options'])) for o in CONFIG_OPTIONS],
            modes=dict(MODES),
            list_rows=[{'sessionId': SID, 'cwd': '/work', 'title': 'Old', 'updatedAt': UPDATED}])
        adapter = KimiAdapter('fake-acp', data_dir=self.root / 'data', spawn=fake.spawn_factory())
        adapter._subagent_store = KimiSubagentStore(self.root)
        bridge = Bridge(self.root, self.root / 'data', adapter=adapter)
        self.addCleanup(bridge.close)
        view = bridge.view(SID)
        rows = [message for turn in view['turns'] for message in turn['messages']
                if message.get('kind') == 'subagent']
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]['subagentId'], SID + '/agent-0')
        self.assertIn('explore the store', rows[0]['text'])
        self.assertIn('done exploring', rows[0]['text'])
        self.assertGreater(view['subagentRevision'], 0)


if __name__ == '__main__':
    unittest.main()
