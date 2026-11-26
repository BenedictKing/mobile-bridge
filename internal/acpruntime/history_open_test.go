package acpruntime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func startMarkerManager(t *testing.T) (*Manager, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	marker := filepath.Join(t.TempDir(), "agent-started")
	t.Setenv("ACP_START_MARKER", marker)
	manager := NewCommand([]string{python, "-u", "-c",
		`import os; open(os.environ["ACP_START_MARKER"], "w").write("started")`})
	t.Cleanup(manager.Close)
	return manager, marker
}

func assertAgentNotStarted(t *testing.T, marker string) {
	t.Helper()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent was started: stat err=%v", err)
	}
}

func historySnapshot() *acpview.NativeSnapshot {
	return &acpview.NativeSnapshot{Title: "原生历史", Turns: []*jsoncompat.Object{
		nativeTurn("turn-history", "completed", "历史问题"),
	}}
}

func TestOpenHistoryNeverStartsAgent(t *testing.T) {
	manager, marker := startMarkerManager(t)
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		return historySnapshot(), nil
	}
	session, err := manager.OpenHistory("session-history", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if !session.ReadOnly() || session.View.Title != "原生历史" {
		t.Fatalf("session readOnly=%v title=%q", session.ReadOnly(), session.View.Title)
	}
	assertAgentNotStarted(t, marker)
}

func TestOpenHistoryResetsUnchangedSnapshotCache(t *testing.T) {
	manager := NewCommand(nil)
	t.Cleanup(manager.Close)
	fresh := true
	manager.NativeReset = func(string) { fresh = true }
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		if !fresh {
			return nil, nil
		}
		fresh = false
		return historySnapshot(), nil
	}
	first, err := manager.OpenHistory("session-history", "/work")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := manager.OpenHistory("session-history", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if second.View.Title != "原生历史" {
		t.Fatalf("reopened title=%q", second.View.Title)
	}
}

func TestOpenOwnerProbeAvoidsStartingAgent(t *testing.T) {
	manager, marker := startMarkerManager(t)
	manager.OwnerProbe = func(string) (owned, known bool) { return true, true }
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		return historySnapshot(), nil
	}
	session, err := manager.Open("session-owned", "/work", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !session.ReadOnly() {
		t.Fatal("owned session must stay read-only")
	}
	assertAgentNotStarted(t, marker)
}

func TestReconcileOwnerProbeAvoidsStartingAgent(t *testing.T) {
	manager, marker := startMarkerManager(t)
	manager.OwnerProbe = func(string) (owned, known bool) { return true, true }
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		return historySnapshot(), nil
	}
	session, err := manager.Reconcile("session-owned", "/work", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !session.ReadOnly() {
		t.Fatal("owned session must stay read-only after explicit attach")
	}
	assertAgentNotStarted(t, marker)
}

func TestNativeExclusiveListNeverStartsAgent(t *testing.T) {
	manager, marker := startMarkerManager(t)
	manager.NativeListExclusive = true
	manager.NativeList = func() []*jsoncompat.Object {
		row := jsoncompat.NewObject()
		row.Set("sessionId", "session-history")
		row.Set("cwd", "/work")
		return []*jsoncompat.Object{row}
	}
	rows, err := manager.List(time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	assertAgentNotStarted(t, marker)
}

func TestStaleCreatedOnlyForUnpromptedCreatedChats(t *testing.T) {
	manager := NewCommand(nil)
	t.Cleanup(manager.Close)
	manager.Kind = "claude"
	manager.Created = LoadCreated(filepath.Join(t.TempDir(), "created-sessions.json"))
	manager.Created.Remember("claude", "sess-empty", "/work", "空聊天", 1)
	manager.Created.Remember("claude", "sess-used", "/work", "用过", 1)
	cached := map[string]bool{}
	manager.NativeReset = func(id string) { delete(cached, id) }
	manager.NativeTranscript = func(id string) (*acpview.NativeSnapshot, error) {
		if id != "sess-used" || cached[id] {
			return nil, nil
		}
		cached[id] = true
		return historySnapshot(), nil
	}
	if title, stale := manager.StaleCreated("sess-empty"); !stale || title != "空聊天" {
		t.Fatalf("empty created chat: title=%q stale=%v", title, stale)
	}
	// An unchanged-cache nil must not read as "no transcript".
	cached["sess-used"] = true
	if _, stale := manager.StaleCreated("sess-used"); stale {
		t.Fatal("chat with a transcript reported stale")
	}
	if _, stale := manager.StaleCreated("sess-unknown"); stale {
		t.Fatal("chat not created by the gateway reported stale")
	}
	manager.mu.Lock()
	manager.sessions["sess-empty"] = &Session{ID: "sess-empty"}
	manager.mu.Unlock()
	if _, stale := manager.StaleCreated("sess-empty"); stale {
		t.Fatal("live created chat reported stale")
	}
}
