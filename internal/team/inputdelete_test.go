package team

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestDeleteInputPersistsAndCannotBeReimported(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "inputs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	path := filepath.Join(dir, "inputs.json")
	ledger := LoadInputLedger(path, journal)
	input, _, err := ledger.ImportHistoricalInput("team", "orphan", "继续", "run", "lead", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.DeleteInput("other", input.ID, input.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-team: %v", err)
	}
	if err := ledger.DeleteInput("team", input.ID, input.Version+1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("version: %v", err)
	}
	if err := ledger.DeleteInput("team", input.ID, input.Version); err != nil {
		t.Fatal(err)
	}
	for _, restored := range []*InputLedger{ledger, LoadInputLedger(path, journal), LoadInputLedger(path, nil), LoadInputLedger(filepath.Join(dir, "missing.json"), journal)} {
		if restored.Get("team", input.ID) != nil || len(restored.ListAll("team")) != 0 {
			t.Fatal("deleted input reappeared")
		}
		if row, duplicate, err := restored.ImportHistoricalInput("team", input.ID, "继续", "run", "lead", time.Now()); err != nil || !duplicate || row != nil {
			t.Fatalf("reimport: %+v %v %v", row, duplicate, err)
		}
		if _, _, err := restored.Accept("team", input.ID, "继续", nil, "policy", 64); !errors.Is(err, ErrNotFound) {
			t.Fatalf("send replay resurrected deleted input: %v", err)
		}
	}
}

func TestDeleteInputRejectsQueuedAndLinkedMessages(t *testing.T) {
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "inputs.json"), nil)
	input, _, err := ledger.Accept("team", "queued", "继续", nil, "policy", 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.DeleteInput("team", input.ID, input.Version); !errors.Is(err, ErrInputNotQueued) {
		t.Fatalf("queued: %v", err)
	}
	input, _, err = ledger.ImportHistoricalInput("team", "linked", "修复", "run", "lead", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ledger.ApplyIssueProposal("team", "proposal", &IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: []IssueProposalSegment{{Action: IssueSegmentCreate, NewIssueTitle: "修复", SourceStart: 0, SourceEnd: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	input = ledger.Get("team", input.ID)
	if err := ledger.DeleteInput("team", input.ID, input.Version); !errors.Is(err, ErrInputNotQueued) {
		t.Fatalf("linked: %v", err)
	}
}
