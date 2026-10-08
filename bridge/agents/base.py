"""Agent adapter seam: capability flags, the base adapter, and the registry."""
import re

from ..ipc import IPCError


class Capability:
    """String capability flags advertised in session list rows and views."""
    STEER = 'steer'
    GOALS = 'goals'
    FORK = 'fork'
    RENAME = 'rename'
    SKILLS = 'skills'
    FAST_MODE = 'fastMode'
    ACCOUNTS = 'accounts'
    EDIT_MESSAGE = 'editMessage'
    FULL_HISTORY = 'fullHistory'
    NOTIFICATION_SCAN = 'notificationScan'
    SERVICE_TIER = 'serviceTier'


class AgentAdapter:
    """Protocol seam between the generic Bridge orchestration and one agent.

    The bridge keeps ledgers, locks and validation; every agent-specific
    protocol detail lives behind these methods. Defaults either raise
    NotImplementedError or return a safe neutral value.
    """

    kind = 'base'
    label = 'Agent'
    capabilities = frozenset()

    def __init__(self):
        self.bridge = None

    def bind(self, bridge):
        """Attach the owning bridge after construction (avoids a cycle)."""
        self.bridge = bridge

    def validate_id(self, thread_id):
        """Accept only a conservative, path-safe session id charset."""
        if not isinstance(thread_id, str) or not re.fullmatch(r'[A-Za-z0-9._-]{1,128}', thread_id):
            raise ValueError('会话标识无效')

    @property
    def home(self):
        """Local data home used for artifact resolution, if any."""
        return None

    def upload_transport(self):
        """Optional upload_file callback for attachments on another host."""
        return None

    def handle_event(self, message):
        """Consume one agent event; no live stream exists by default."""

    def handle_disconnect(self):
        """Invalidate live state after the agent stream drops."""

    def adopt_session(self, session):
        """Install agent-specific state on one new LiveSession."""

    def detach(self, session):
        """Release the live owner of one session being evicted or closed."""

    def is_active(self, session):
        """Whether the session currently runs a turn."""
        return False

    def require_ready(self, session):
        """Raise when the session cannot serve an explicit operation."""
        if not session.connected:
            raise IPCError(session.error or '会话尚未连接，请重新打开')

    def check_provider(self, session):
        """Raise when the session's provider conflicts with the active account."""

    def projects(self):
        """Return saved project rows usable as create_chat targets."""
        raise NotImplementedError

    def attach(self, session, timeout=1.5):
        """Connect the session to its live owner and wait for a snapshot."""
        raise NotImplementedError

    def call(self, session, method, params, timeout=30):
        """Issue one agent request on behalf of a live session."""
        raise NotImplementedError

    def dispatch_turn(self, session, submission_id, entry, files):
        """Start or steer one turn; returns the native response."""
        raise NotImplementedError

    def update_settings(self, session, model, effort, fast_mode=None):
        """Validate against the catalog and apply model settings."""
        raise NotImplementedError

    def catalog(self, session, refresh=False, kind=None, query='', offset=0, limit=200, ids=None):
        """Read the model/skill catalog for this session."""
        raise NotImplementedError

    def resolve_skills(self, session, skills):
        """Validate selected skills against the session workspace."""
        raise NotImplementedError

    def rename(self, session, thread_id, title):
        """Persist a new session title on the agent side."""
        raise NotImplementedError

    def create_session(self, project, title):
        """Create an empty persisted session; returns its thread id."""
        raise NotImplementedError

    def open_created(self, thread_id, host):
        """Best-effort handover of a created session; returns success."""
        raise NotImplementedError

    def fork_settings(self, state):
        """Derive fork settings from a source session snapshot."""
        raise NotImplementedError

    def fork_thread(self, thread_id, args):
        """Copy a session up to one turn; returns the child thread id."""
        raise NotImplementedError

    def edit_last_user_turn(self, target, params):
        """Replace the latest user message in place."""
        raise NotImplementedError

    def respond_to_request(self, session, pending, response):
        """Answer a pending approval/question request."""
        raise NotImplementedError

    def interrupt(self, session):
        """Stop the running turn."""
        raise NotImplementedError

    def full_history(self, session):
        """Load the complete session history."""
        raise NotImplementedError

    def activate_owner(self, session):
        """Bring up the owner of an unloaded session."""
        raise NotImplementedError

    def goal_call(self, method, *args):
        """Invoke one native-goal RPC."""
        raise NotImplementedError

    def goal_read(self, thread_id):
        """Read the native goal row, when the agent has one."""
        return None

    def goal_absent(self, thread_id):
        """Whether readable native storage proves the goal is absent."""
        return False

    def goal_runtime_available(self):
        """Whether the native-goal runtime can serve this bridge."""
        return False

    def session_meta(self, thread_id):
        """Read one session's stored metadata; raises KeyError if unknown."""
        raise NotImplementedError

    def saved_history(self, session):
        """Read persisted history without activating the session."""
        raise NotImplementedError

    def list_sessions(self, label, limit=100, offset=0, query='', archived=False):
        """Return decorated session list rows for this adapter's host."""
        raise NotImplementedError

    def recencies(self, identifiers):
        """Return stored recency timestamps keyed by session id."""
        raise NotImplementedError

    def notification_changes(self, cursor):
        """Return session metadata changed since the discovery cursor."""
        raise NotImplementedError

    def close(self):
        """Release every transport owned by this adapter."""


class Registry:
    """Minimal kind → bridge lookup; 'codex' remains the implicit default."""

    def __init__(self):
        self._bridges = {}

    def register(self, kind, bridge):
        self._bridges[kind] = bridge

    def default(self):
        return self.for_agent('codex')

    def for_agent(self, kind):
        return self._bridges[kind]

    def bridge_for(self, session_id):
        """Return the bridge that listed this session id, if any did."""
        for bridge in self._bridges.values():
            if session_id in bridge.listed:
                return bridge
        return None

    def kinds(self):
        return sorted(self._bridges)
