package acpgoal

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreCreateTransitionAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acp-goals.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Unix(100, 0) }
	created, err := store.Create("goal-1", "op-1", "kimi", "thread-1", "整理仓库", 5)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != StatusDispatching || created.Revision != 1 {
		t.Fatalf("created=%+v", created)
	}
	replay, err := store.Create("goal-1", "op-1", "kimi", "thread-1", "整理仓库", 5)
	if err != nil || replay.GoalID != created.GoalID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err := store.MarkDispatched("goal-1", created.ActiveDispatchID, "turn-1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Transition("goal-1", "op-2", StatusActive, 99, StatusPaused, "user pause"); err != ErrConflict {
		t.Fatalf("revision conflict=%v", err)
	}
	paused, err := store.Transition("goal-1", "op-2", StatusActive, 2, StatusPaused, "user pause")
	if err != nil || paused.Status != StatusPaused || paused.Revision != 3 {
		t.Fatalf("paused=%+v err=%v", paused, err)
	}
	if _, err := store.Transition("goal-1", "op-3", StatusPaused, 3, StatusCompleted, "manual complete"); err != ErrInvalidTransition {
		t.Fatalf("invalid transition=%v", err)
	}
	resumed, err := store.Transition("goal-1", "op-4", StatusPaused, 3, StatusDispatching, "manual one step")
	if err != nil || resumed.Status != StatusDispatching {
		t.Fatalf("resumed=%+v err=%v", resumed, err)
	}
	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("goal-1")
	if !ok || got.Status != StatusOutcomeUnknown || got.Revision != 4 {
		t.Fatalf("reloaded=%+v ok=%v", got, ok)
	}
	mode := os.FileMode(0o777)
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else {
		mode = info.Mode().Perm()
	}
	if mode != 0o600 {
		t.Fatalf("mode=%o", mode)
	}
}

func TestStoreOperationIDConflictAndTerminalStates(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create("goal-1", "op-1", "kimi", "thread-1", "目标一", 2); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Create("goal-2", "op-1", "kimi", "thread-2", "目标二", 2); err != ErrOperationConflict {
		t.Fatalf("operation conflict=%v", err)
	}
	if _, err = store.Create("goal-2", "op-other", "kimi", "thread-1", "目标二", 2); err != ErrConflict {
		t.Fatalf("parallel goal conflict=%v", err)
	}
	if _, err = store.MarkDispatched("goal-1", "goal-1:0", "turn-1", true); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Transition("goal-1", "op-2", StatusActive, 2, StatusPaused, "different input"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Transition("goal-1", "op-2", StatusActive, 2, StatusCancelled, "user cancel"); err != ErrOperationConflict {
		t.Fatalf("transition operation conflict=%v", err)
	}
	if _, err = store.Transition("goal-1", "op-3", StatusPaused, 3, StatusCancelled, "user cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Transition("goal-1", "op-4", StatusCancelled, 4, StatusActive, "retry"); err != ErrInvalidTransition {
		t.Fatalf("terminal transition=%v", err)
	}
}

func TestEditPreservesOldObjective(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create("g", "create", "kimi", "thread", "旧目标", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDispatched("g", created.ActiveDispatchID, "turn", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("g", "pause", StatusActive, 2, StatusPaused, "pause"); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Edit("g", "edit", 3, "新目标")
	if err != nil || updated.Status != StatusPaused || updated.AutoStepsLimit != 1 || len(updated.ObjectiveHistory) != 1 || updated.ObjectiveHistory[0].Objective != "旧目标" {
		t.Fatalf("edit=%+v err=%v", updated, err)
	}
	if _, err := s.Edit("g", "edit", 3, "其他目标"); err != ErrOperationConflict {
		t.Fatalf("edit operation conflict=%v", err)
	}
}

func TestAutoStepLimitStopsDispatch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	goal, err := s.Create("g", "create", "kimi", "thread", "目标", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDispatched("g", goal.ActiveDispatchID, "t1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEvaluation("g", "t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEvaluation("g", "t1", StatusDispatching, "jev_continue"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDispatched("g", "g:1", "t2", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEvaluation("g", "t2"); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.ResolveEvaluation("g", "t2", StatusDispatching, "jev_continue")
	if err != nil || stopped.Status != StatusWaitingReview || stopped.AutoStepsUsed != 1 {
		t.Fatalf("limit=%+v err=%v", stopped, err)
	}
}

func TestTransitionWriteFailureRollsBackMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("g", "create", "kimi", "thread", "目标", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MarkDispatched("g", "g:0", "turn", true); err != nil {
		t.Fatal(err)
	}
	store.path = filepath.Join(path, "cannot-write.json")
	if _, err := store.Transition("g", "pause", StatusActive, 2, StatusPaused, "pause"); err == nil {
		t.Fatal("write unexpectedly succeeded")
	}
	got, _ := store.Get("g")
	if got.Status != StatusActive || got.Revision != 2 {
		t.Fatalf("failed write changed memory: %+v", got)
	}
	store.path = path
	if _, err := store.Transition("g", "pause", StatusActive, 2, StatusPaused, "pause"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverDispatchRequiresReconciliation(t *testing.T) {
	goal := RecoverDispatch(Goal{Status: StatusDispatching, ActiveDispatchID: "step-1"})
	if goal.Status != StatusOutcomeUnknown || goal.ActiveDispatchID != "step-1" {
		t.Fatalf("recovered=%+v", goal)
	}
}

func TestOpenParksActiveGoalWithoutResending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	goal, err := s.Create("g", "create", "kimi", "thread", "目标", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDispatched("g", goal.ActiveDispatchID, "t1", true); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	parked, _ := reloaded.ForThread("kimi", "thread")
	if parked.Status != StatusWaitingReview || parked.ActiveTurnID != "t1" {
		t.Fatalf("restart=%+v", parked)
	}
	if _, err := reloaded.ClaimEvaluation("g", "t1"); err != ErrConflict {
		t.Fatalf("restart evaluated without review: %v", err)
	}
}

func TestPauseDuringDispatchIsAppliedAfterAcceptance(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	goal, err := s.Create("g", "create", "kimi", "thread", "目标", 2)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Transition("g", "pause", StatusDispatching, 1, StatusPaused, "user pause")
	if err != nil || pending.Status != StatusDispatching || pending.PendingControl != StatusPaused {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	settled, err := s.MarkDispatched("g", goal.ActiveDispatchID, "turn", true)
	if err != nil || settled.Status != StatusPaused || settled.ActiveTurnID != "turn" {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
}

func TestPausedEvaluationCanResumeOnce(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	goal, err := s.Create("g", "create", "kimi", "thread", "目标", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkDispatched("g", goal.ActiveDispatchID, "turn", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEvaluation("g", "turn"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition("g", "pause", StatusEvaluating, 3, StatusPaused, "pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEvaluation("g", "turn", StatusDispatching, "late Jev"); err != ErrConflict {
		t.Fatalf("late verdict applied: %v", err)
	}
	if _, err := s.Transition("g", "resume", StatusPaused, 4, StatusActive, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimEvaluation("g", "turn"); err != nil {
		t.Fatalf("resume evaluation: %v", err)
	}
}

func TestStoreCorruptFileIsNotReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("corrupt store loaded without error")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "not json" {
		t.Fatalf("corrupt store changed: %q", raw)
	}
}

func TestStoreRejectsLostOperationHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals.json")
	data := `{"schemaVersion":1,"goals":{"g":{"schemaVersion":1,"goalId":"g","agent":"kimi","threadId":"t","objective":"x","revision":1,"status":"dispatching","autoStepsLimit":2}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("operation history was silently rebuilt")
	}
}
