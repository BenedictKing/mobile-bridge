package team

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunDeliveryAcceptIsIdempotentAndTerminal(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture()
	state := h.saveExecRun(t, run)
	if err := h.journal.Append(newEvent(run.ID, EventRunCreated, "user", map[string]any{
		"teamId": run.TeamID, "goal": run.Goal, "mode": run.Mode, "autoPlan": true, "taskCount": 1,
	})); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	h.service.finishRunLocked(state, run, RunCompleted, "")
	state.mu.Unlock()
	completed := h.store.Run(run.ID)
	if completed.Status != RunCompleted || completed.DeliveryStatus != DeliveryPendingReview {
		t.Fatalf("成功执行后应进入待验收: %+v", completed)
	}
	if _, _, err := h.service.SetDeliveryStatus(run.ID, DeliveryAccepted, completed.Version+10); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本应冲突: %v", err)
	}
	accepted, duplicate, err := h.service.SetDeliveryStatus(run.ID, DeliveryAccepted, completed.Version)
	if err != nil || duplicate || accepted.DeliveryStatus != DeliveryAccepted {
		t.Fatalf("接受成果失败: duplicate=%v run=%+v err=%v", duplicate, accepted, err)
	}
	if _, duplicate, err = h.service.SetDeliveryStatus(run.ID, DeliveryAccepted, completed.Version); err != nil || !duplicate {
		t.Fatalf("同状态重放应返回 duplicate: duplicate=%v err=%v", duplicate, err)
	}
	if _, _, err = h.service.SetDeliveryStatus(run.ID, DeliveryDiscarded, accepted.Version); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("已接受成果不得再丢弃: %v", err)
	}

	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	teamsPath, runsPath := filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json")
	seedTeam := h.team.Clone()
	seedTeam.Version = 0
	if err := LoadStore(teamsPath, runsPath).SaveTeam(seedTeam); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := LoadStore(teamsPath, runsPath)
	fresh.RebuildRunsFromJournal(events)
	if rebuilt := fresh.Run(run.ID); rebuilt == nil || rebuilt.DeliveryStatus != DeliveryAccepted {
		t.Fatalf("Journal 重建丢失接受状态: %+v", rebuilt)
	}
}

func TestRunDeliveryDiscardRequiresPendingReview(t *testing.T) {
	h := newActionsExecHarness(t)
	run := execRunFixture()
	run.DeliveryStatus = DeliveryPendingReview
	run.Status = RunCompleted
	h.saveExecRun(t, run)
	discarded, duplicate, err := h.service.SetDeliveryStatus(run.ID, DeliveryDiscarded, run.Version)
	if err != nil || duplicate || discarded.DeliveryStatus != DeliveryDiscarded || discarded.Status != RunCompleted {
		t.Fatalf("丢弃成果失败: duplicate=%v run=%+v err=%v", duplicate, discarded, err)
	}

	running := execRunFixture()
	running.ID = "run-still-running"
	h.saveExecRun(t, running)
	if _, _, err := h.service.SetDeliveryStatus(running.ID, DeliveryAccepted, running.Version); !errors.Is(err, ErrRunFinished) {
		t.Fatalf("未完成 Run 不得接受: %v", err)
	}
}
