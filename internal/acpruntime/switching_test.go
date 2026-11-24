package acpruntime

import (
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func newStubSession(id, cwd string) *Session {
	return &Session{ID: id, Cwd: cwd, active: true,
		pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{},
		pendingKinds: map[string]string{}, changed: make(chan struct{}),
		touched: time.Now(), lastActivity: time.Now()}
}

func TestLiveSessionsAndBusyReasons(t *testing.T) {
	m := New("")
	m.sessions["s-idle"] = newStubSession("s-idle", "/tmp/a")
	running := newStubSession("s-running", "/tmp/b")
	running.promptRunning = true
	m.sessions["s-running"] = running
	approval := newStubSession("s-approval", "/tmp/c")
	approval.pending["req-1"] = jsoncompat.NewObject()
	m.sessions["s-approval"] = approval
	closed := newStubSession("s-closed", "/tmp/d")
	closed.closed = true
	m.sessions["s-closed"] = closed
	history := newStubSession("s-history", "/tmp/history")
	history.readOnly = true
	m.sessions[history.ID] = history
	desktop := newStubSession("s-desktop", "/tmp/desktop")
	desktop.readOnly = true
	desktop.desktopOwned = true
	m.sessions[desktop.ID] = desktop

	live := m.LiveSessions()
	if len(live) != 3 || live["s-idle"] != "/tmp/a" {
		t.Fatalf("live sessions: %+v", live)
	}
	if _, ok := live["s-closed"]; ok {
		t.Fatal("closed session must not be listed")
	}
	if history.IsClosed() || desktop.IsClosed() || m.Peek(history.ID) != history || m.Peek(desktop.ID) != desktop {
		t.Fatal("account switch live set must preserve readable history handles")
	}
	reasons := m.BusyReasons()
	if len(reasons) != 2 {
		t.Fatalf("busy reasons: %+v", reasons)
	}
	joined := reasons[0] + reasons[1]
	if !strings.Contains(joined, "s-running") || !strings.Contains(joined, "s-approval") {
		t.Fatalf("busy reasons: %+v", reasons)
	}
}

func TestCloseSessionRemovesHandleFromLiveSet(t *testing.T) {
	m := New("")
	session := newStubSession("s-broken", "/tmp/a")
	m.sessions[session.ID] = session
	m.CloseSession(session.ID)
	if !session.IsClosed() {
		t.Fatal("session process handle must be closed")
	}
	if live := m.LiveSessions(); len(live) != 0 {
		t.Fatalf("closed session must leave the live set: %+v", live)
	}
}

func TestPeekLeavesIdleClockUntouched(t *testing.T) {
	m := New("")
	session := newStubSession("s-idle", "/tmp/a")
	session.touched = time.Now().Add(-time.Hour)
	m.sessions[session.ID] = session
	if got := m.Peek(session.ID); got != session {
		t.Fatal("peek did not return the live session")
	}
	if idle := session.IdleFor(); idle < 30*time.Minute {
		t.Fatalf("peek refreshed the idle clock: %v", idle)
	}
}

func TestBusyReasonsExceptIgnoresEitherSessionAlias(t *testing.T) {
	for _, test := range []struct {
		name      string
		sessionID string
		ignoreID  string
	}{
		{name: "placeholder blocked after canonical failure", sessionID: "lazy-1", ignoreID: "sess-real"},
		{name: "canonical blocked after placeholder failure", sessionID: "sess-real", ignoreID: "lazy-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := New("")
			m.Canonical = func(id string) string {
				if id == "lazy-1" {
					return "sess-real"
				}
				return ""
			}
			busy := newStubSession(test.sessionID, "/tmp/a")
			busy.promptRunning = true
			busy.pending["req-1"] = jsoncompat.NewObject()
			m.sessions[test.sessionID] = busy
			if reasons := m.BusyReasonsExcept(map[string]bool{test.ignoreID: true}); len(reasons) != 0 {
				t.Fatalf("alias was not ignored: %+v", reasons)
			}
			if reasons := m.BusyReasons(); len(reasons) != 2 {
				t.Fatalf("busy state fixture is not active: %+v", reasons)
			}
		})
	}
}
