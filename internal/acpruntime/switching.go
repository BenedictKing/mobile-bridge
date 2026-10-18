package acpruntime

import "time"

// LiveSessions maps every open session id to its cwd. The account-pool
// switcher re-attaches exactly these after swapping the agent's credentials;
// sessions that are not open simply spawn fresh processes on next use.
func (m *Manager) LiveSessions() map[string]string {
	m.mu.Lock()
	sessions := make(map[string]*Session, len(m.sessions))
	for id, session := range m.sessions {
		sessions[id] = session
	}
	m.mu.Unlock()
	out := map[string]string{}
	for id, session := range sessions {
		if !session.IsClosed() {
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
		if ignore[id] || session.IsClosed() {
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

func (s *Session) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) > 0
}

// Reattach reopens one session under the agent's current credentials
// (Reconcile builds the fresh handle before dropping the old one, so readers
// never observe a half-loaded session).
func (m *Manager) Reattach(id, cwd string, timeout time.Duration) error {
	_, err := m.Reconcile(id, cwd, timeout)
	return err
}
