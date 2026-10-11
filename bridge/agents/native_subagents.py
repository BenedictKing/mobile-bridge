"""On-disk sub-agent discovery for ACP agents that archive children as their
own sessions instead of embedding them in the parent transcript.

Each store speaks one agent's native layout and projects both a child roster
(`children`) and a readable history (`history`) in the generic item vocabulary
`model.normalize_item` already renders. Stores are read-only and fail soft:
missing or unreadable files simply yield no children, because a foreign on-disk
layout must never break the live ACP session it decorates.

dsh archives sub-sessions as zstd-compressed event logs and the gateway runs
on a stdlib-only Python without zstd support, so it stays undiscoverable here.
"""
import json
import logging
import threading
from pathlib import Path

LOG = logging.getLogger(__name__)

# One child transcript is bounded the same way as any other projection source:
# real transcripts reach tens of megabytes, but the card only needs the story.
MAX_TRANSCRIPT_BYTES = 8 * 1024 * 1024
MAX_EVENTS = 4000
MAX_CHILDREN = 32


def _read_json(path):
    try:
        return json.loads(path.read_text(encoding='utf-8'))
    except (OSError, ValueError):
        return None


def _iter_lines(path, byte_cap=MAX_TRANSCRIPT_BYTES):
    """Stream JSONL with a hard byte budget; partial last lines are dropped."""
    try:
        with path.open('rb') as handle:
            consumed = 0
            for raw in handle:
                consumed += len(raw)
                if consumed > byte_cap:
                    LOG.debug("Stopped reading %s at byte cap", path)
                    return
                try:
                    yield json.loads(raw)
                except ValueError:
                    continue
    except OSError:
        return


def _text(content):
    """Flatten message content blocks the way text_content does."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [block.get('text', '') for block in content
                 if isinstance(block, dict) and block.get('type') in ('text', 'input_text', 'output_text')]
        return '\n'.join(part for part in parts if part)
    return ''


def _iso_ms(value):
    if isinstance(value, (int, float)):
        return int(value if value < 10 ** 12 else value)
    try:
        from datetime import datetime, timezone
        parsed = datetime.fromisoformat(str(value).replace('Z', '+00:00'))
        return int(parsed.timestamp() * 1000)
    except (TypeError, ValueError):
        return 0


def _tool_item(call_id, name, arguments):
    text = arguments if isinstance(arguments, str) else json.dumps(arguments, ensure_ascii=False)
    return {'type': 'toolCall', 'id': str(call_id), 'title': str(name or '工具调用'), 'text': text}


class _BaseStore:
    def __init__(self, root):
        self.root = Path(root)
        self._lock = threading.Lock()
        self._roster_cache = {}
        self._history_cache = {}

    def _cached(self, cache, key, signature, build):
        with self._lock:
            hit = cache.get(key)
            if hit is not None and hit[0] == signature:
                return hit[1]
        value = build()
        with self._lock:
            if len(cache) > 128:
                cache.clear()
            cache[key] = (signature, value)
        return value

    @staticmethod
    def _signature(path):
        try:
            stat = path.stat()
            return (stat.st_mtime_ns, stat.st_size)
        except OSError:
            return None

    def _all_roster_rows(self):
        """History lookups arrive right after a roster scan; transcript paths
        ride along on the roster rows."""
        with self._lock:
            return [row for cached in self._roster_cache.values() for row in cached[1]]


class ZcodeSubagentStore(_BaseStore):
    """zcode: ~/.zcode/v2/acp-lazy-sessions.json maps the ACP session id to the
    backend sess_* id; ~/.zcode/cli/agents/<sid>/agent_*/ holds one directory
    per spawned child with metadata.json and transcript.jsonl."""

    def _zcode_sid(self, acp_session_id):
        mapping = _read_json(self.root / 'v2' / 'acp-lazy-sessions.json') or {}
        entry = mapping.get(acp_session_id)
        if isinstance(entry, dict) and entry.get('zcodeSid'):
            return entry['zcodeSid']
        # Sessions created before the lazy map existed already use backend ids.
        return acp_session_id if acp_session_id.startswith('sess_') else None

    def children(self, parent_id):
        sid = self._zcode_sid(parent_id)
        if not sid:
            return []
        agents_dir = self.root / 'cli' / 'agents' / sid
        def build():
            rows = []
            for meta_path in sorted(agents_dir.glob('agent_*/metadata.json'))[:MAX_CHILDREN]:
                meta = _read_json(meta_path)
                if not isinstance(meta, dict) or not meta.get('childSessionId'):
                    continue
                rows.append({
                    'id': meta['childSessionId'],
                    'name': meta.get('description') or meta.get('prompt') or meta.get('childSessionId'),
                    'agent_role': meta.get('profileId') or '',
                    'spawn_status': meta.get('status') or '',
                    'updated_at_ms': _iso_ms(meta.get('updatedAt') or meta.get('completedAt')),
                    'first_user_message': (meta.get('prompt') or '')[:4000],
                    'transcript': str(meta_path.parent / 'transcript.jsonl'),
                })
            return rows
        return self._cached(self._roster_cache, sid, self._signature(agents_dir), build)

    def history(self, child_id):
        row = next((row for row in self._all_roster_rows() if row['id'] == child_id), None)
        if not row:
            return {}
        path = Path(row['transcript'])
        def build():
            turns, items = [], []
            pending = {}
            events = 0
            for event in _iter_lines(path):
                events += 1
                if events > MAX_EVENTS:
                    break
                kind = event.get('type')
                payload = event.get('payload') or {}
                if kind == 'turn_started':
                    if items:
                        turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
                    items = []
                    prompt = payload.get('input')
                    text = prompt if isinstance(prompt, str) else _text(prompt)
                    if text:
                        items.append({'type': 'userMessage', 'content': [{'type': 'text', 'text': text[:16000]}]})
                elif kind == 'tool_call_scheduled':
                    item = _tool_item(payload.get('toolCallId'), payload.get('toolName'), payload.get('input'))
                    pending[item['id']] = item
                    items.append(item)
                elif kind == 'model_complete':
                    text = payload.get('content')
                    if isinstance(text, str) and text.strip():
                        items.append({'type': 'agentMessage', 'text': text[:16000]})
                elif kind == 'turn_complete':
                    failed = payload.get('resultType') not in (None, 'success', 'completed')
                    response = str(payload.get('response') or '').strip()
                    if response:
                        items.append({'type': 'agentMessage', 'text': response[:16000]})
                    turns.append({'turnId': len(turns), 'status': 'failed' if failed else 'completed', 'items': items})
                    items = []
            if items:
                turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
            return {'id': row['id'], 'title': row.get('name'), 'turns': turns}
        return self._cached(self._history_cache, row['id'], self._signature(path), build)


class KimiSubagentStore(_BaseStore):
    """kimi: ~/.kimi-code/session_index.jsonl locates the session directory;
    state.json lists spawned agents and agents/<id>/wire.jsonl records each
    child's execution as native loop events."""

    def _session_dir(self, session_id):
        index_path = self.root / 'session_index.jsonl'
        for entry in _iter_lines(index_path, byte_cap=1024 * 1024):
            if entry.get('sessionId') == session_id and entry.get('sessionDir'):
                return Path(entry['sessionDir'])
        return None

    def children(self, parent_id):
        session_dir = self._session_dir(parent_id)
        if session_dir is None:
            return []
        def build():
            state = _read_json(session_dir / 'state.json')
            if not isinstance(state, dict):
                return []
            spawn_types, statuses = self._spawn_types(session_dir)
            rows = []
            for agent_id, agent in (state.get('agents') or {}).items():
                if not isinstance(agent, dict) or agent.get('type') != 'sub':
                    continue
                wire = session_dir / 'agents' / agent_id / 'wire.jsonl'
                rows.append({
                    # 'agent-0' repeats in every session; the parent prefix
                    # keeps child ids unique across the roster cache.
                    'id': f'{parent_id}/{agent_id}',
                    'name': agent_id,
                    'agent_role': spawn_types.get(agent_id, ''),
                    'spawn_status': statuses.get(agent_id, 'completed'),
                    'updated_at_ms': _iso_ms(state.get('updatedAt')),
                    'first_user_message': (state.get('lastPrompt') or '')[:4000],
                    'transcript': str(wire),
                })
                if len(rows) >= MAX_CHILDREN:
                    break
            return rows
        return self._cached(self._roster_cache, parent_id, self._signature(session_dir / 'state.json'), build)

    def _spawn_types(self, session_dir):
        """Child profile names ride on task.started events (info.kind=agent) in
        the main agent's wire; task.terminated closes them out."""
        types, statuses = {}, {}
        for event in _iter_lines(session_dir / 'agents' / 'main' / 'wire.jsonl', byte_cap=4 * 1024 * 1024):
            if not isinstance(event, dict):
                continue
            info = event.get('info') if isinstance(event.get('info'), dict) else {}
            if info.get('kind') != 'agent':
                continue
            agent_id = info.get('agentId')
            if not agent_id:
                continue
            if info.get('subagentType'):
                types.setdefault(agent_id, str(info['subagentType']))
            if event.get('type') == 'task.terminated' and info.get('status'):
                statuses[agent_id] = str(info['status'])
        return types, statuses

    def history(self, child_id):
        row = next((row for row in self._all_roster_rows() if row['id'] == child_id), None)
        if not row:
            return {}
        path = Path(row['transcript'])
        def build():
            turns, items = [], []
            pending = {}
            for event in _iter_lines(path):
                kind = event.get('type')
                if kind == 'turn.prompt':
                    if items:
                        turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
                    items = []
                    text = _text(event.get('input'))
                    if text:
                        items.append({'type': 'userMessage', 'content': [{'type': 'text', 'text': text[:16000]}]})
                elif kind == 'context.append_message':
                    message = event.get('message') or {}
                    text = _text(message.get('content'))
                    if not text:
                        continue
                    if message.get('role') == 'user':
                        items.append({'type': 'userMessage', 'content': [{'type': 'text', 'text': text[:16000]}]})
                    else:
                        items.append({'type': 'agentMessage', 'text': text[:16000]})
                elif kind == 'context.append_loop_event':
                    loop = event.get('event') or {}
                    loop_kind = loop.get('type')
                    if loop_kind == 'tool.call':
                        item = _tool_item(loop.get('toolCallId'), loop.get('name'), loop.get('args'))
                        pending[item['id']] = item
                        items.append(item)
                    elif loop_kind == 'tool.result':
                        item = pending.get(str(loop.get('toolCallId')))
                        result = loop.get('result')
                        if item is not None:
                            item['output'] = (result if isinstance(result, str)
                                              else json.dumps(result, ensure_ascii=False))[:16000]
                elif kind == 'turn.ended':
                    failed = event.get('reason') not in (None, 'completed', 'stop')
                    turns.append({'turnId': len(turns), 'status': 'failed' if failed else 'completed', 'items': items})
                    items = []
            if items:
                turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
            return {'id': child_id, 'title': row.get('name'), 'turns': turns}
        # Child ids like 'agent-0' repeat across sessions; the transcript path
        # is the real identity.
        return self._cached(self._history_cache, str(path), self._signature(path), build)


class ClaudeSubagentStore(_BaseStore):
    """claude: ~/.claude/projects/<slug>/<sessionId>/subagents/agent-*.jsonl;
    the directory name is the parent session id, so discovery is one glob."""

    def _subagents_dir(self, parent_id):
        if not str(parent_id).strip():
            return None
        for directory in self.root.glob('projects/*'):
            candidate = directory / str(parent_id) / 'subagents'
            if candidate.is_dir():
                return candidate
        return None

    def children(self, parent_id):
        directory = self._subagents_dir(parent_id)
        if directory is None:
            return []
        def build():
            rows = []
            for path in sorted(directory.glob('agent-*.jsonl'))[:MAX_CHILDREN]:
                first, last_stamp = None, None
                for event in _iter_lines(path, byte_cap=256 * 1024):
                    if first is None:
                        first = event
                    if event.get('timestamp'):
                        last_stamp = event['timestamp']
                if first is None:
                    continue
                agent_id = first.get('agentId') or path.stem
                role = str(agent_id)
                if role.startswith('agent-'):
                    role = role[len('agent-'):].rsplit('-', 1)[0] or role
                rows.append({
                    'id': str(path),
                    'name': str(agent_id),
                    'agent_role': role,
                    'spawn_status': 'completed',
                    'updated_at_ms': _iso_ms(last_stamp or first.get('timestamp')),
                    'first_user_message': (_text(first.get('message', {}).get('content')) if isinstance(first.get('message'), dict) else '')[:4000],
                    'transcript': str(path),
                })
            return rows
        return self._cached(self._roster_cache, str(parent_id), self._signature(directory), build)

    def history(self, child_id):
        path = Path(child_id)
        def build():
            turns, items = [], []
            pending = {}
            for event in _iter_lines(path):
                kind = event.get('type')
                message = event.get('message') if isinstance(event.get('message'), dict) else {}
                content = message.get('content')
                if kind == 'user':
                    if isinstance(content, str):
                        if content.strip() and not event.get('isMeta'):
                            if items:
                                turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
                            items = [{'type': 'userMessage', 'content': [{'type': 'text', 'text': content[:16000]}]}]
                    else:
                        for block in content or []:
                            if not isinstance(block, dict):
                                continue
                            if block.get('type') == 'text' and block.get('text', '').strip() and not event.get('isMeta'):
                                items.append({'type': 'userMessage', 'content': [{'type': 'text', 'text': block['text'][:16000]}]})
                            elif block.get('type') == 'tool_result':
                                item = pending.get(str(block.get('tool_use_id')))
                                if item is not None:
                                    item['output'] = _text(block.get('content'))[:16000]
                elif kind == 'assistant':
                    for block in content or []:
                        if not isinstance(block, dict):
                            continue
                        if block.get('type') == 'text' and block.get('text', '').strip():
                            items.append({'type': 'agentMessage', 'text': block['text'][:16000]})
                        elif block.get('type') == 'tool_use':
                            item = _tool_item(block.get('id'), block.get('name'), block.get('input'))
                            pending[item['id']] = item
                            items.append(item)
            if items:
                turns.append({'turnId': len(turns), 'status': 'completed', 'items': items})
            return {'id': path.stem, 'title': path.stem, 'turns': turns}
        return self._cached(self._history_cache, str(child_id), self._signature(path), build)
