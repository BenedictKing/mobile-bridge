package planstore

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	SchemaVersion   = 1
	MaxContentBytes = 2 * 1024 * 1024
	MaxPlanBytes    = 8 * 1024 * 1024
	MaxRevisions    = 100
	// 反馈上限与前端一致：引用截断到 4000 字符外加截断标记、意见 20000 字符、单次 50 条批注。
	MaxAnnotations    = 50
	MaxQuoteRunes     = 4096
	MaxFeedbackRunes  = 20000
	MaxFeedbackPerRev = 20
	maxRequestIDBytes = 512
)

var ErrConflict = errors.New("plan revision conflict")

type Revision struct {
	Version   int        `json:"version"`
	Hash      string     `json:"hash"`
	Content   string     `json:"content"`
	CreatedAt time.Time  `json:"createdAt"`
	Feedback  []Feedback `json:"feedback,omitempty"`
}

// Feedback 是用户对某一版本提交的修订意见：Text 为全局意见，Annotations 为
// 绑定原文选区的结构化批注。它只是辅助历史，不参与审批或幂等判断。
type Feedback struct {
	RequestID   string       `json:"requestId"`
	Text        string       `json:"text,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
	CreatedAt   time.Time    `json:"createdAt"`
}

type Annotation struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Quote string `json:"quote"`
	Body  string `json:"body,omitempty"`
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

// RecordFeedback 把一次修订意见追加到指定版本。版本与哈希由调用方从服务端
// pending 请求取得；版本已被裁剪、从未落盘或哈希不符时返回错误而不臆造版本。
// 同一 RequestID 只记录一次，命令重放不会产生重复反馈。
func (s *Store) RecordFeedback(agent, sessionID, planID string, version int, hash string, feedback Feedback) error {
	if err := ValidateFeedback(feedback); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := planKey(agent, sessionID, planID)
	plan, ok := s.plans[key]
	if !ok {
		return errors.New("plan not recorded")
	}
	plan = clonePlan(plan)
	for index := range plan.Revisions {
		revision := &plan.Revisions[index]
		if revision.Version != version {
			continue
		}
		if revision.Hash != hash {
			return ErrConflict
		}
		for _, existing := range revision.Feedback {
			if existing.RequestID == feedback.RequestID {
				return nil
			}
		}
		feedback.CreatedAt = s.now().UTC()
		feedback.Annotations = append([]Annotation(nil), feedback.Annotations...)
		revision.Feedback = append(revision.Feedback, feedback)
		if extra := len(revision.Feedback) - MaxFeedbackPerRev; extra > 0 {
			revision.Feedback = revision.Feedback[extra:]
		}
		plan.UpdatedAt = feedback.CreatedAt
		trim(&plan)
		// 只剩一个版本仍超限时 trim 不再裁剪；落盘会让下次加载校验失败。
		if planBytes(plan) > MaxPlanBytes {
			return errors.New("plan feedback exceeds plan size limit")
		}
		s.plans[key] = plan
		s.dirty = true
		return s.saveLocked()
	}
	return errors.New("plan revision not recorded")
}

var annotationTypes = map[string]bool{"comment": true, "delete": true}

// ValidateFeedback 校验反馈结构；调用方可在送达 Agent 之前用它拒绝非法输入。
func ValidateFeedback(feedback Feedback) error {
	invalid := errors.New("invalid plan feedback")
	if feedback.RequestID == "" || len(feedback.RequestID) > maxRequestIDBytes || utf8.RuneCountInString(feedback.Text) > MaxFeedbackRunes || len(feedback.Annotations) > MaxAnnotations {
		return invalid
	}
	if strings.TrimSpace(feedback.Text) == "" && len(feedback.Annotations) == 0 {
		return invalid
	}
	seen := map[string]bool{}
	for _, annotation := range feedback.Annotations {
		if !annotationID.MatchString(annotation.ID) || seen[annotation.ID] || !annotationTypes[annotation.Type] {
			return invalid
		}
		seen[annotation.ID] = true
		if strings.TrimSpace(annotation.Quote) == "" || utf8.RuneCountInString(annotation.Quote) > MaxQuoteRunes || utf8.RuneCountInString(annotation.Body) > MaxFeedbackRunes {
			return invalid
		}
		if annotation.Type == "comment" && strings.TrimSpace(annotation.Body) == "" {
			return invalid
		}
	}
	return nil
}

var annotationID = regexp.MustCompile(`^A[1-9][0-9]{0,2}$`)

func trim(plan *Plan) {
	for len(plan.Revisions) > 1 && (len(plan.Revisions) > MaxRevisions || planBytes(*plan) > MaxPlanBytes) {
		plan.Revisions = plan.Revisions[1:]
	}
}

func planBytes(plan Plan) int {
	total := 0
	for _, revision := range plan.Revisions {
		total += len(revision.Content)
		for _, feedback := range revision.Feedback {
			total += len(feedback.Text)
			for _, annotation := range feedback.Annotations {
				total += len(annotation.Quote) + len(annotation.Body)
			}
		}
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
		if len(revision.Feedback) > MaxFeedbackPerRev {
			return fmt.Errorf("invalid plan feedback %q version %d", plan.PlanID, revision.Version)
		}
		for _, feedback := range revision.Feedback {
			if ValidateFeedback(feedback) != nil || feedback.CreatedAt.IsZero() {
				return fmt.Errorf("invalid plan feedback %q version %d", plan.PlanID, revision.Version)
			}
		}
		previous = revision.Version
	}
	return nil
}

func clonePlan(plan Plan) Plan {
	plan.Revisions = append([]Revision(nil), plan.Revisions...)
	for index := range plan.Revisions {
		feedback := append([]Feedback(nil), plan.Revisions[index].Feedback...)
		for item := range feedback {
			feedback[item].Annotations = append([]Annotation(nil), feedback[item].Annotations...)
		}
		plan.Revisions[index].Feedback = feedback
	}
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
