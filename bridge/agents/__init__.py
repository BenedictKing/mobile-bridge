"""Agent adapters: the bridge talks to agents only through this seam."""
from . import base, codex
from .base import AgentAdapter, Capability, Registry
from .codex import CodexAdapter

__all__ = ['AgentAdapter', 'Capability', 'CodexAdapter', 'Registry', 'base', 'codex']
