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
