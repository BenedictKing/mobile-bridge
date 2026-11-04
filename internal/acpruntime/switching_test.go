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

	live := m.LiveSessions()
	if len(live) != 3 || live["s-idle"] != "/tmp/a" {
		t.Fatalf("live sessions: %+v", live)
	}
	if _, ok := live["s-closed"]; ok {
		t.Fatal("closed session must not be listed")
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
