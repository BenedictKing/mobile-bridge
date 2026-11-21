package acpruntime

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/command"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func replyReceiptSession(t *testing.T, ledger *command.Ledger) *Session {
	t.Helper()
	s := &Session{ID: "session", Commands: ledger, kind: "fake", View: acpview.NewView("session", nil, "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{})}
	params := jsoncompat.NewObject()
	option := jsoncompat.NewObject()
	option.Set("optionId", "allow")
	params.Set("options", &jsoncompat.Array{Items: []any{option}})
	s.pending["request"] = params
	s.pendingIDs["request"] = "native"
	s.pendingKinds["request"] = "permission"
	s.View.AddPermission("request", params)
	return s
}

func TestResponseReceiptSurvivesHandleAndGatewayRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.json")
	ledger, err := command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := replyReceiptSession(t, ledger)
	calls := 0
	s.respondToAgent = func(any, any) error { calls++; return nil }
	response := jsoncompat.NewObject()
	response.Set("optionId", "allow")
	if err := s.Respond("request", response, "human-command"); err != nil {
		t.Fatal(err)
	}
	ledger, err = command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// The native callback is gone. Its stable command still replays the receipt.
	restarted := &Session{ID: "session", kind: "fake", Commands: ledger, closed: true}
	if err := restarted.Respond("request", response, "human-command"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestUncertainResponseReceiptPreventsCrossRestartWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "receipts.json")
	ledger, err := command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s := replyReceiptSession(t, ledger)
	calls := 0
	s.respondToAgent = func(any, any) error { calls++; return &acp.Error{Message: "partial write", Kind: acp.KindWriteUnknown} }
	response := jsoncompat.NewObject()
	response.Set("optionId", "allow")
	if err := s.Respond("request", response, "human-command"); !errors.Is(err, command.ErrUnknown) {
		t.Fatal(err)
	}
	ledger, err = command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := replyReceiptSession(t, ledger)
	restarted.respondToAgent = func(any, any) error { calls++; return nil }
	if err := restarted.Respond("request", response, "human-command"); !errors.Is(err, command.ErrUnknown) || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestReceiptCannotReplayAContradictoryDecision(t *testing.T) {
	ledger, err := command.Open(filepath.Join(t.TempDir(), "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := replyReceiptSession(t, ledger)
	s.respondToAgent = func(any, any) error { return nil }
	response := jsoncompat.NewObject()
	response.Set("optionId", "allow")
	if err := s.Respond("request", response, "one"); err != nil {
		t.Fatal(err)
	}
	response.Set("cancel", true)
	if err := s.Respond("request", response, "one"); !errors.Is(err, command.ErrConflict) {
		t.Fatal(err)
	}
}

func TestResponseReplayDoesNotRearmHumanBudgets(t *testing.T) {
	ledger, err := command.Open(filepath.Join(t.TempDir(), "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := replyReceiptSession(t, ledger)
	s.respondToAgent = func(any, any) error { return nil }
	s.autoContinueRuns, s.switchResumeRuns = 2, 2
	response := jsoncompat.NewObject()
	response.Set("optionId", "allow")
	if err := s.Respond("request", response, "human"); err != nil {
		t.Fatal(err)
	}
	if s.autoContinueRuns != 0 || s.switchResumeRuns != 0 {
		t.Fatal("new human response did not rearm budgets")
	}
	s.autoContinueRuns, s.switchResumeRuns = 3, 3
	if err := s.Respond("request", response, "human"); err != nil {
		t.Fatal(err)
	}
	if s.autoContinueRuns != 3 || s.switchResumeRuns != 3 {
		t.Fatal("receipt replay rearmed budgets")
	}
	auto := replyReceiptSession(t, ledger)
	auto.autoContinueRuns = 3
	auto.respondToAgent = func(any, any) error { return nil }
	if err := auto.Respond("request", response); err != nil {
		t.Fatal(err)
	}
	if auto.autoContinueRuns != 3 {
		t.Fatal("automatic response counted as a human action")
	}
}
