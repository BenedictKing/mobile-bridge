package acpgoal

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion        = 1
	StatusActive         = "active"
	StatusDispatching    = "dispatching"
	StatusEvaluating     = "evaluating"
	StatusWaitingInput   = "waiting_input"
	StatusWaitingReview  = "waiting_review"
	StatusPaused         = "paused"
	StatusBlocked        = "blocked"
	StatusOutcomeUnknown = "outcome_unknown"
	StatusCompleted      = "completed"
	StatusCancelled      = "cancelled"
)

var (
	ErrConflict          = errors.New("goal revision or state conflict")
	ErrOperationConflict = errors.New("goal operation id was already used with different input")
	ErrInvalidTransition = errors.New("invalid goal state transition")
)

type Goal struct {
	SchemaVersion     int             `json:"schemaVersion"`
	GoalID            string          `json:"goalId"`
	Agent             string          `json:"agent"`
	ThreadID          string          `json:"threadId"`
	Objective         string          `json:"objective"`
	ObjectiveHistory  []GoalRevision  `json:"objectiveHistory,omitempty"`
	Revision          int64           `json:"revision"`
	Status            string          `json:"status"`
	AutoStepsUsed     int             `json:"autoStepsUsed"`
	AutoStepsLimit    int             `json:"autoStepsLimit"`
	LastEvaluatedTurn string          `json:"lastEvaluatedTurnId,omitempty"`
	ActiveTurnID      string          `json:"activeTurnId,omitempty"`
	LastDecision      json.RawMessage `json:"lastDecision,omitempty"`
	ActiveDispatchID  string          `json:"activeDispatchId,omitempty"`
	StopReason        string          `json:"stopReason,omitempty"`
	PendingControl    string          `json:"pendingControl,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	UpdatedAt         time.Time       `json:"updatedAt"`
}

type GoalRevision struct {
	Revision  int64     `json:"revision"`
	Objective string    `json:"objective"`
	ChangedAt time.Time `json:"changedAt"`
}

type operation struct {
	ID     string          `json:"id"`
	Action string          `json:"action"`
	Input  json.RawMessage `json:"input"`
	Result Goal            `json:"result"`
}

type fileData struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Goals         map[string]Goal      `json:"goals"`
	Operations    map[string]operation `json:"operations"`
}

type Store struct {
	mu         sync.Mutex
	path       string
	now        func() time.Time
	goals      map[string]Goal
	operations map[string]operation
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, now: time.Now, goals: map[string]Goal{}, operations: map[string]operation{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("load goal store: %w", err)
	}
	if data.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported goal store schema version %d", data.SchemaVersion)
	}
	if data.Goals == nil {
		data.Goals = map[string]Goal{}
	}
	if data.Operations == nil {
		if len(data.Goals) > 0 {
			return nil, errors.New("goal store is missing operation history")
		}
		data.Operations = map[string]operation{}
	}
	if len(data.Goals) > 0 && len(data.Operations) == 0 {
		return nil, errors.New("goal store has no operation history")
	}
	for id, operation := range data.Operations {
		if id != operation.ID || len(operation.Input) == 0 || operation.Result.GoalID == "" {
			return nil, fmt.Errorf("invalid goal operation %q", id)
		}
	}
	for id, goal := range data.Goals {
		if id != goal.GoalID || goal.SchemaVersion != SchemaVersion || !validStatus(goal.Status) || goal.Revision < 1 {
			return nil, fmt.Errorf("invalid goal store entry %q", id)
		}
	}
	s.goals, s.operations = data.Goals, data.Operations
	changed := false
	for id, goal := range s.goals {
		wasChanged := false
		if goal.Status == StatusDispatching {
			goal.Status, goal.StopReason = StatusOutcomeUnknown, "gateway restarted during prompt dispatch"
			wasChanged = true
		} else if goal.Status == StatusActive {
			goal.Status, goal.StopReason = StatusWaitingReview, "gateway restarted; verify agent turn"
			wasChanged = true
		} else if goal.Status == StatusEvaluating {
			goal.Status, goal.StopReason = StatusWaitingReview, "gateway restarted during Goal assessment"
			wasChanged = true
		}
		if wasChanged {
			s.goals[id] = goal
			changed = true
		}
	}
	if changed {
		if err := s.saveLocked(); err != nil {
			return nil, fmt.Errorf("recover goal store: %w", err)
		}
	}
	return s, nil
}

func (s *Store) Get(goalID string) (Goal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	goal, ok := s.goals[goalID]
	return goal, ok
}

func (s *Store) Create(goalID, operationID, agent, threadID, objective string, limit int) (Goal, error) {
	goal, _, err := s.CreateOnce(goalID, operationID, agent, threadID, objective, limit)
	return goal, err
}

// CreateOnce reports whether this caller won the durable first-dispatch claim.
func (s *Store) CreateOnce(goalID, operationID, agent, threadID, objective string, limit int) (Goal, bool, error) {
	if goalID == "" || operationID == "" || agent == "" || threadID == "" || strings.TrimSpace(objective) == "" || len([]rune(objective)) > 4000 || limit <= 0 {
		return Goal{}, false, errors.New("invalid goal creation input")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	input, _ := json.Marshal(struct {
		GoalID, Agent, ThreadID, Objective string
		Limit                              int
	}{goalID, agent, threadID, objective, limit})
	if previous, ok := s.operations[operationID]; ok {
		if previous.Action != "create" || !equalJSON(previous.Input, input) {
			return Goal{}, false, ErrOperationConflict
		}
		return previous.Result, false, nil
	}
	if _, exists := s.goals[goalID]; exists {
		return Goal{}, false, ErrConflict
	}
	for _, existing := range s.goals {
		if existing.Agent == agent && existing.ThreadID == threadID && existing.Status != StatusCompleted && existing.Status != StatusCancelled {
			return Goal{}, false, ErrConflict
		}
	}
	now := s.now().UTC()
	goal := Goal{SchemaVersion: SchemaVersion, GoalID: goalID, Agent: agent, ThreadID: threadID, Objective: objective, Revision: 1, Status: StatusDispatching, ActiveDispatchID: goalID + ":0", AutoStepsLimit: limit, CreatedAt: now, UpdatedAt: now}
	s.goals[goalID] = goal
	s.operations[operationID] = operation{ID: operationID, Action: "create", Input: input, Result: goal}
	if err := s.saveLocked(); err != nil {
		delete(s.goals, goalID)
		delete(s.operations, operationID)
		return Goal{}, false, err
	}
	return goal, true, nil
}

func (s *Store) ForThread(agent, threadID string) (Goal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest Goal
	var found bool
	for _, goal := range s.goals {
		if goal.Agent != agent || goal.ThreadID != threadID {
			continue
		}
		if goal.Status != StatusCompleted && goal.Status != StatusCancelled {
			return goal, true
		}
		if !found || goal.UpdatedAt.After(latest.UpdatedAt) {
			latest, found = goal, true
		}
	}
	return latest, found
}

// MarkDispatched commits the ACP write result. An in-flight control request
// wins over the normal active state once the send result is known.
func (s *Store) MarkDispatched(goalID, dispatchID, turnID string, accepted bool) (Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.goals[goalID]
	if !ok || old.Status != StatusDispatching || old.ActiveDispatchID != dispatchID {
		return Goal{}, ErrConflict
	}
	goal := old
	if accepted {
		goal.Status, goal.ActiveTurnID = StatusActive, turnID
		if goal.PendingControl != "" {
			goal.Status = goal.PendingControl
		}
		goal.PendingControl = ""
	} else {
		goal.Status, goal.StopReason = StatusOutcomeUnknown, "dispatch outcome unknown"
	}
	goal.Revision++
	goal.UpdatedAt = s.now().UTC()
	if err := s.replaceLocked(old, goal); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

func (s *Store) ClaimEvaluation(goalID, turnID string) (Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.goals[goalID]
	if !ok || (old.Status != StatusActive && old.Status != StatusWaitingInput) || old.ActiveTurnID != turnID || (old.Status == StatusActive && old.LastEvaluatedTurn == turnID) {
		return Goal{}, ErrConflict
	}
	goal := old
	goal.Status, goal.LastEvaluatedTurn = StatusEvaluating, turnID
	goal.Revision++
	goal.UpdatedAt = s.now().UTC()
	if err := s.replaceLocked(old, goal); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

func (s *Store) ResolveEvaluation(goalID, turnID, next, reason string, decision ...json.RawMessage) (Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.goals[goalID]
	if !ok || old.Status != StatusEvaluating || old.LastEvaluatedTurn != turnID || !allowed(old.Status, next) {
		return Goal{}, ErrConflict
	}
	goal := old
	if next == StatusDispatching {
		if goal.AutoStepsUsed >= goal.AutoStepsLimit {
			next, reason = StatusWaitingReview, "auto step limit reached"
		} else {
			goal.AutoStepsUsed++
			goal.ActiveDispatchID = fmt.Sprintf("%s:%d", goal.GoalID, goal.AutoStepsUsed)
		}
	}
	goal.Status, goal.StopReason = next, reason
	if len(decision) > 0 {
		goal.LastDecision = append(json.RawMessage(nil), decision[0]...)
	}
	goal.Revision++
	goal.UpdatedAt = s.now().UTC()
	if err := s.replaceLocked(old, goal); err != nil {
		return Goal{}, err
	}
	return goal, nil
}

func (s *Store) replaceLocked(old, goal Goal) error {
	s.goals[goal.GoalID] = goal
	if err := s.saveLocked(); err != nil {
		s.goals[goal.GoalID] = old
		return err
	}
	return nil
}

func (s *Store) Edit(goalID, operationID string, expectedRevision int64, objective string) (Goal, error) {
	if operationID == "" || strings.TrimSpace(objective) == "" || len([]rune(objective)) > 4000 {
		return Goal{}, errors.New("invalid goal edit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	input, _ := json.Marshal(struct {
		GoalID, Objective string
		Revision          int64
	}{goalID, objective, expectedRevision})
	if previous, ok := s.operations[operationID]; ok {
		if previous.Action != "edit" || !bytes.Equal(previous.Input, input) {
			return Goal{}, ErrOperationConflict
		}
		return previous.Result, nil
	}
	old, ok := s.goals[goalID]
	if !ok || old.Status != StatusPaused || old.Revision != expectedRevision {
		return Goal{}, ErrConflict
	}
	goal := old
	goal.ObjectiveHistory = append(append([]GoalRevision{}, old.ObjectiveHistory...), GoalRevision{Revision: old.Revision, Objective: old.Objective, ChangedAt: s.now().UTC()})
	goal.Objective, goal.Revision, goal.UpdatedAt = objective, goal.Revision+1, s.now().UTC()
	s.goals[goalID] = goal
	s.operations[operationID] = operation{ID: operationID, Action: "edit", Input: input, Result: goal}
	if err := s.saveLocked(); err != nil {
		s.goals[goalID] = old
		delete(s.operations, operationID)
		return Goal{}, err
	}
	return goal, nil
}

func (s *Store) Transition(goalID, operationID, expectedState string, expectedRevision int64, nextState, reason string) (Goal, error) {
	if operationID == "" || !validStatus(nextState) {
		return Goal{}, errors.New("invalid goal transition input")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.operations[operationID]; ok {
		input, _ := json.Marshal(struct {
			GoalID, ExpectedState, NextState, Reason string
			ExpectedRevision                         int64
		}{goalID, expectedState, nextState, reason, expectedRevision})
		if previous.Action != "transition" || !bytes.Equal(previous.Input, input) {
			return Goal{}, ErrOperationConflict
		}
		return previous.Result, nil
	}
	previousGoal, ok := s.goals[goalID]
	if !ok || previousGoal.Status != expectedState || previousGoal.Revision != expectedRevision {
		return Goal{}, ErrConflict
	}
	if !allowed(previousGoal.Status, nextState) {
		return Goal{}, ErrInvalidTransition
	}
	goal := previousGoal
	if goal.Status == StatusDispatching && (nextState == StatusPaused || nextState == StatusCancelled) {
		goal.PendingControl = nextState
	} else {
		goal.Status = nextState
	}
	if nextState == StatusDispatching {
		goal.ActiveDispatchID = fmt.Sprintf("%s:manual:%d", goal.GoalID, goal.Revision+1)
	}
	if nextState == StatusPaused && (previousGoal.Status == StatusEvaluating || previousGoal.Status == StatusWaitingReview) {
		goal.LastEvaluatedTurn = ""
	}
	goal.StopReason, goal.Revision, goal.UpdatedAt = reason, goal.Revision+1, s.now().UTC()
	input, _ := json.Marshal(struct {
		GoalID, ExpectedState, NextState, Reason string
		ExpectedRevision                         int64
	}{goalID, expectedState, nextState, reason, expectedRevision})
	s.goals[goalID] = goal
	s.operations[operationID] = operation{ID: operationID, Action: "transition", Input: input, Result: goal}
	if err := s.saveLocked(); err != nil {
		s.goals[goalID] = previousGoal
		delete(s.operations, operationID)
		return Goal{}, err
	}
	return goal, nil
}

// RecoverDispatch never assumes an ACP prompt was not delivered after a crash.
// The caller must resolve this state against the authoritative session before
// permitting another prompt.
func RecoverDispatch(goal Goal) Goal {
	if goal.Status == StatusDispatching {
		goal.Status = StatusOutcomeUnknown
		goal.StopReason = "dispatch outcome requires reconciliation"
	}
	return goal
}

func (s *Store) saveLocked() error {
	data, err := json.MarshalIndent(fileData{SchemaVersion: SchemaVersion, Goals: s.goals, Operations: s.operations}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp-" + randomSuffix()
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
func equalJSON(a, b []byte) bool { return bytes.Equal(a, b) }

func validStatus(status string) bool {
	switch status {
	case StatusActive, StatusDispatching, StatusEvaluating, StatusWaitingInput, StatusWaitingReview, StatusPaused, StatusBlocked, StatusOutcomeUnknown, StatusCompleted, StatusCancelled:
		return true
	}
	return false
}
func allowed(from, to string) bool {
	if from == StatusCompleted || from == StatusCancelled {
		return false
	}
	if to == StatusCompleted {
		return from == StatusWaitingReview || from == StatusEvaluating
	}
	if to == StatusCancelled {
		return true
	}
	switch from {
	case StatusActive:
		return to == StatusEvaluating || to == StatusWaitingInput || to == StatusWaitingReview || to == StatusPaused || to == StatusBlocked
	case StatusDispatching:
		return to == StatusActive || to == StatusPaused || to == StatusCancelled || to == StatusOutcomeUnknown || to == StatusBlocked
	case StatusEvaluating:
		return to == StatusDispatching || to == StatusWaitingReview || to == StatusWaitingInput || to == StatusPaused || to == StatusCancelled || to == StatusBlocked
	case StatusWaitingInput:
		return to == StatusEvaluating || to == StatusPaused || to == StatusCancelled
	case StatusWaitingReview:
		return to == StatusDispatching || to == StatusEvaluating || to == StatusPaused || to == StatusCancelled
	case StatusPaused:
		return to == StatusActive || to == StatusEvaluating || to == StatusDispatching || to == StatusCancelled
	case StatusBlocked:
		return to == StatusDispatching || to == StatusEvaluating || to == StatusPaused || to == StatusCancelled
	case StatusOutcomeUnknown:
		return to == StatusWaitingReview || to == StatusPaused || to == StatusCancelled
	}
	return false
}
