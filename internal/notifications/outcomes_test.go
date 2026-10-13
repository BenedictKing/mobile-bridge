package notifications

import (
	"testing"
	"time"
)

func TestOutcomeLedgerPersistsAndReconciles(t *testing.T) {
	dir := t.TempDir()
	ledger := NewOutcomeLedger(dir)
	at := time.Unix(100, 0)
	if err := ledger.Record("local", "thread", "turn", "Chat", at); err != nil {
		t.Fatal(err)
	}
	loaded := NewOutcomeLedger(dir)
	if loaded.Snapshot().Len() != 1 {
		t.Fatal("outcome was not persisted")
	}
	if err := loaded.Reconcile(at.Add(8*24*time.Hour), nil, nil); err != nil {
		t.Fatal(err)
	}
	if loaded.Snapshot().Len() != 0 {
		t.Fatal("expired outcome was not removed")
	}
	if err := loaded.Record("local", "thread", "turn2", "Chat", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := loaded.Reconcile(time.Now(), func(host, thread, event string) bool { return event == "turn2" }, nil); err != nil {
		t.Fatal(err)
	}
	if loaded.Snapshot().Len() != 0 {
		t.Fatal("delivered outcome was not removed")
	}
}

func TestOutcomeWorkerStopsCleanly(t *testing.T) {
	worker := NewOutcomeLedger(t.TempDir()).StartWorker(time.Millisecond, nil, nil)
	worker.Close()
}
