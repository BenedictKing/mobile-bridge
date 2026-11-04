package httpd

import (
	"os"
	"path/filepath"
	"testing"
)

func mustLoadCreations(t *testing.T, path string) *creations {
	t.Helper()
	ledger, err := LoadCreations(path)
	if err != nil {
		t.Fatal(err)
	}
	return ledger
}

func TestCreationsRejectsCorruptHistoryAndFailedClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creations.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ledger, err := LoadCreations(path); err == nil || ledger != nil {
		t.Fatalf("corrupt history must fail startup: ledger=%v err=%v", ledger, err)
	}
	ledger := mustLoadCreations(t, filepath.Join(t.TempDir(), "missing", "creations.json"))
	if _, claimed, err := ledger.begin("request-1", &creationEntry{Agent: "kimi"}); err == nil || claimed {
		t.Fatalf("failed durable claim: claimed=%v err=%v", claimed, err)
	}
	if ledger.lookup("request-1") != nil {
		t.Fatal("failed claim must roll back in memory")
	}
}

func TestCreationsFailedCompletionRetainsClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creations.json")
	ledger := mustLoadCreations(t, path)
	if _, claimed, err := ledger.begin("request-1", &creationEntry{Agent: "kimi"}); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	ledger.path = filepath.Join(t.TempDir(), "missing", "creations.json")
	if err := ledger.complete("request-1", "sess-created"); err == nil {
		t.Fatal("completion persistence failure must surface")
	}
	if entry := ledger.lookup("request-1"); entry == nil || entry.ID != "" {
		t.Fatalf("unknown result must retain id-less claim: %+v", entry)
	}
	reloaded := mustLoadCreations(t, path)
	if entry := reloaded.lookup("request-1"); entry == nil || entry.ID != "" {
		t.Fatalf("durable claim lost: %+v", entry)
	}
}

func TestCreationsDispatchClaimSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "creations.json")
	ledger := mustLoadCreations(t, path)
	if _, claimed, err := ledger.begin("request-1", &creationEntry{Agent: "kimi", ID: "sess-1"}); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if claimed, err := ledger.beginDispatch("request-1"); err != nil || !claimed {
		t.Fatalf("dispatch: claimed=%v err=%v", claimed, err)
	}
	reloaded := mustLoadCreations(t, path)
	if claimed, err := reloaded.beginDispatch("request-1"); err != nil || claimed {
		t.Fatalf("dispatch replay: claimed=%v err=%v", claimed, err)
	}
	reloaded.path = filepath.Join(t.TempDir(), "missing", "creations.json")
	if err := reloaded.markSent("request-1"); err == nil {
		t.Fatal("sent state must report persistence failure")
	}
	if entry := reloaded.lookup("request-1"); entry == nil || entry.State != "dispatching" || entry.Sent {
		t.Fatalf("failed completion reopened dispatch: %+v", entry)
	}
	if err := reloaded.markWaiting("request-1", "late timeout"); err == nil {
		t.Fatal("later state update must report persistence failure")
	}
	if entry := reloaded.lookup("request-1"); entry == nil || entry.State != "dispatching" {
		t.Fatalf("late update overwrote dispatch claim: %+v", entry)
	}
	reloaded.path = path
	if err := reloaded.markWaiting("request-1", "late timeout"); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.markPrepared("request-1"); err != nil {
		t.Fatal(err)
	}
	if entry := reloaded.lookup("request-1"); entry == nil || entry.State != "dispatching" {
		t.Fatalf("late state update reopened dispatch: %+v", entry)
	}
}
