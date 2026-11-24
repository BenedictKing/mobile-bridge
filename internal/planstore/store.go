package planstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	SchemaVersion   = 1
	MaxContentBytes = 2 * 1024 * 1024
	MaxPlanBytes    = 8 * 1024 * 1024
	MaxRevisions    = 100
)

var ErrConflict = errors.New("plan revision conflict")

type Revision struct {
	Version   int       `json:"version"`
	Hash      string    `json:"hash"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"createdAt"`
}

type Plan struct {
	Agent     string     `json:"agent"`
	SessionID string     `json:"sessionId"`
	PlanID    string     `json:"planId"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	Revisions []Revision `json:"revisions"`
}

type fileData struct {
	SchemaVersion int    `json:"schemaVersion"`
	Plans         []Plan `json:"plans"`
}

type Store struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	plans map[string]Plan
	dirty bool
}

func Open(path string) (*Store, error) {
	store := &Store{path: path, now: time.Now, plans: map[string]Plan{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	var data fileData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("load plan store: %w", err)
	}
	if data.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("unsupported plan store schema version %d", data.SchemaVersion)
	}
	for _, plan := range data.Plans {
		if err := validatePlan(plan); err != nil {
			return nil, err
		}
		key := planKey(plan.Agent, plan.SessionID, plan.PlanID)
		if _, exists := store.plans[key]; exists {
			return nil, fmt.Errorf("duplicate plan store entry %q", plan.PlanID)
		}
		store.plans[key] = clonePlan(plan)
	}
	return store, nil
}

func Hash(content string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func (s *Store) Get(agent, sessionID, planID string) (Plan, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, ok := s.plans[planKey(agent, sessionID, planID)]
	return clonePlan(plan), ok
}

func (s *Store) Record(agent, sessionID, planID string, version int, hash, content string) error {
	if agent == "" || sessionID == "" || planID == "" || len(planID) > 512 || version < 1 || len(content) > MaxContentBytes || hash != Hash(content) {
		return errors.New("invalid plan revision")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(agent, sessionID, planID)
	plan := s.plans[key]
	if len(plan.Revisions) > 0 {
		latest := plan.Revisions[len(plan.Revisions)-1]
		if version < latest.Version || version == latest.Version && hash != latest.Hash {
			return ErrConflict
		}
		if version == latest.Version {
			if s.dirty {
				return s.saveLocked()
			}
			return nil
		}
	}
	now := s.now().UTC()
	if plan.Agent == "" {
		plan = Plan{Agent: agent, SessionID: sessionID, PlanID: planID, CreatedAt: now}
	}
	plan.UpdatedAt = now
	plan.Revisions = append(plan.Revisions, Revision{Version: version, Hash: hash, Content: content, CreatedAt: now})
	trim(&plan)
	s.plans[key] = plan
	s.dirty = true
	return s.saveLocked()
}

func trim(plan *Plan) {
	for len(plan.Revisions) > 1 && (len(plan.Revisions) > MaxRevisions || planBytes(*plan) > MaxPlanBytes) {
		plan.Revisions = plan.Revisions[1:]
	}
}

func planBytes(plan Plan) int {
	total := 0
	for _, revision := range plan.Revisions {
		total += len(revision.Content)
	}
	return total
}

func validatePlan(plan Plan) error {
	if plan.Agent == "" || plan.SessionID == "" || plan.PlanID == "" || len(plan.PlanID) > 512 || plan.CreatedAt.IsZero() || plan.UpdatedAt.IsZero() || len(plan.Revisions) == 0 || len(plan.Revisions) > MaxRevisions || planBytes(plan) > MaxPlanBytes {
		return fmt.Errorf("invalid plan store entry %q", plan.PlanID)
	}
	previous := 0
	for _, revision := range plan.Revisions {
		if revision.Version <= previous || revision.CreatedAt.IsZero() || len(revision.Content) > MaxContentBytes || revision.Hash != Hash(revision.Content) {
			return fmt.Errorf("invalid plan revision %q version %d", plan.PlanID, revision.Version)
		}
		previous = revision.Version
	}
	return nil
}

func clonePlan(plan Plan) Plan {
	plan.Revisions = append([]Revision(nil), plan.Revisions...)
	return plan
}

func planKey(agent, sessionID, planID string) string {
	return agent + "\x00" + sessionID + "\x00" + planID
}

func (s *Store) saveLocked() error {
	data := fileData{SchemaVersion: SchemaVersion, Plans: make([]Plan, 0, len(s.plans))}
	for _, plan := range s.plans {
		data.Plans = append(data.Plans, plan)
	}
	sort.Slice(data.Plans, func(i, j int) bool {
		return planKey(data.Plans[i].Agent, data.Plans[i].SessionID, data.Plans[i].PlanID) < planKey(data.Plans[j].Agent, data.Plans[j].SessionID, data.Plans[j].PlanID)
	})
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".acp-plans-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	s.dirty = false
	return nil
}
