package acpruntime

import "time"

// LiveSessions maps open, controllable session ids to their cwd. The account-pool
// switcher re-attaches these after swapping credentials. Read-only histories
// hold no writable process and attach under the current account on next use.
func (m *Manager) LiveSessions() map[string]string {
	m.mu.Lock()
	sessions := make(map[string]*Session, len(m.sessions))
	for id, session := range m.sessions {
		sessions[id] = session
	}
	m.mu.Unlock()
	out := map[string]string{}
	for id, session := range sessions {
		if !session.IsClosed() && !session.ReadOnly() {
			out[id] = session.Cwd
		}
	}
	return out
}

// BusyReasons lists why the manager cannot be switched right now: turns in
// flight and unanswered permission/question prompts. Switching mid-turn would
// kill the process the turn is streaming from.
func (m *Manager) BusyReasons() []string {
	return m.BusyReasonsExcept(nil)
}

// BusyReasonsExcept is BusyReasons with sessions to ignore — the quota
// failover path excludes the session whose turn just failed (its leftover
// pending requests are dead letters, not a reason to keep the dead account).
func (m *Manager) BusyReasonsExcept(ignore map[string]bool) []string {
	m.mu.Lock()
	sessions := make(map[string]*Session, len(m.sessions))
	for id, session := range m.sessions {
		sessions[id] = session
	}
	m.mu.Unlock()
	var reasons []string
	for id, session := range sessions {
		if m.ignoredForSwitch(id, ignore) || session.IsClosed() {
			continue
		}
		if session.PromptRunning() {
			reasons = append(reasons, id+": 回合进行中")
		}
		if session.hasPending() {
			reasons = append(reasons, id+": 有待确认的审批或提问")
		}
	}
	return reasons
}

// ignoredForSwitch matches both directions of a lazy session alias: the failed
// turn may already use the backend id while an older handle still blocks under
// its placeholder (and vice versa).
func (m *Manager) ignoredForSwitch(id string, ignore map[string]bool) bool {
	if ignore[id] {
		return true
	}
	if canonical := m.canonicalUnlocked(id); canonical != "" && ignore[canonical] {
		return true
	}
	for candidate := range ignore {
		if m.canonicalUnlocked(candidate) == id {
			return true
		}
	}
	return false
}

func (s *Session) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// IdleSessions is LiveSessions minus sessions with a running turn or a
// pending prompt — the set safe to reload when the underlying credentials
// changed underneath a live process (kimi slot drift heal). Busy sessions
// are left to their own failure path.
func (m *Manager) IdleSessions() map[string]string {
	m.mu.Lock()
	sessions := make(map[string]*Session, len(m.sessions))
	sessions = m.sessions
	m.mu.Unlock()
	out := map[string]string{}
	for id, session := range sessions {
		if session.IsClosed() || session.PromptRunning() || session.hasPending() {
			continue
		}
		out[id] = session.Cwd
	}
	return out
}

// Reattach reopens one session under the agent's current credentials
// (Reconcile builds the fresh handle before dropping the old one, so readers
// never observe a half-loaded session).
func (m *Manager) Reattach(id, cwd string, timeout time.Duration) error {
	_, err := m.Reconcile(id, cwd, timeout)
	return err
}

// CloseSession tears down a session whose old-credentials process cannot be
// kept after an account switch. A later open creates a fresh handle.
func (m *Manager) CloseSession(id string) {
	key := m.openKey(id)
	m.mu.Lock()
	session := m.sessions[key]
	if session == nil {
		session = m.liveAliasLocked(key)
	}
	m.mu.Unlock()
	if session != nil {
		session.Close()
	}
}
