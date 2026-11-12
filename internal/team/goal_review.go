package team

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// GoalReviewRequest 只记录用户的条件与判断，不派发任务，不修改或回滚工作区。
type GoalReviewRequest struct {
	ID           string            `json:"id"`
	Version      uint64            `json:"version"`
	GoalRevision uint64            `json:"goalRevision"`
	ScopeHash    string            `json:"scopeHash"`
	EvidenceHash string            `json:"evidenceHash"`
	Action       string            `json:"action"`
	Contract     *GoalContract     `json:"contract,omitempty"`
	States       map[string]string `json:"states,omitempty"`
	Reason       string            `json:"reason"`
}

var ErrGoalReviewInvalid = fmt.Errorf("目标评估参数无效")

func goalInvalid(message string) error { return fmt.Errorf("%w：%s", ErrGoalReviewInvalid, message) }

func (s *Service) issueGoalRecord(run *Run, scope GoalScope) (*GoalRecord, bool) {
	if s.inputs == nil || scope.ID == "run" {
		return nil, false
	}
	issue := s.inputs.GetIssue(run.TeamID, scope.ID)
	if issue == nil {
		return nil, false
	}
	return s.inputs.IssueGoal(run.TeamID, scope.ID), true
}

func evaluateGoalRecord(run *Run, teamRow *Team, scope GoalScope, record *GoalRecord, issueScoped bool) GoalOutcome {
	if issueScoped {
		return EvaluateIssueGoal(run, teamRow, scope, record)
	}
	return EvaluateGoal(run, teamRow, scope)
}

func (s *Service) syncIssueGoal(run *Run, scope GoalScope, issueScoped bool) error {
	if !issueScoped {
		return nil
	}
	record := run.Goals[scope.ID]
	if record == nil {
		return nil
	}
	return s.inputs.ApplyIssueGoal(run.TeamID, run.ID, record)
}

func (s *Service) ReviewGoal(runID string, scope GoalScope, request GoalReviewRequest) (*Run, error) {
	if _, err := uuid.Parse(request.ID); err != nil {
		return nil, goalInvalid("操作标识无效")
	}
	if request.ScopeHash != scope.Hash {
		return nil, ErrVersionConflict
	}
	if len([]rune(request.Reason)) > 2000 {
		return nil, goalInvalid("说明不能超过 2000 字")
	}
	if request.Action == "define" {
		if request.Contract == nil {
			return nil, goalInvalid("缺少完成条件")
		}
		if err := validateGoalContract(*request.Contract, scope); err != nil {
			return nil, goalInvalid(err.Error())
		}
	} else if request.Action != "assess" && request.Action != "close" {
		return nil, goalInvalid("不支持的目标操作")
	}
	fingerprint := goalHash(struct {
		Scope   GoalScope
		Request GoalReviewRequest
	}{scope, request})
	observed := s.store.Run(runID)
	if observed == nil {
		return nil, ErrNotFound
	}
	issueRecord, issueScoped := s.issueGoalRecord(observed, scope)
	for _, record := range observed.Goals {
		if record == nil {
			continue
		}
		if previous, exists := record.Operations[request.ID]; exists {
			if previous != fingerprint {
				return nil, goalInvalid("同一操作标识不能用于不同内容")
			}
			if err := s.syncIssueGoal(observed, scope, issueScoped); err != nil {
				return nil, err
			}
			return observed, nil
		}
	}
	// 慢 I/O 在 Run 锁外完成；锁内仍按证据哈希和版本复查，不能把过期检查当作当前证据。
	if request.Action == "assess" {
		allMet := len(request.States) > 0
		for _, value := range request.States {
			allMet = allMet && value == "met"
		}
		if allMet {
			observed := s.store.Run(runID)
			if observed == nil {
				return nil, ErrNotFound
			}
			outcome := evaluateGoalRecord(observed, s.store.Team(observed.TeamID), scope, issueRecord, issueScoped)
			if outcome.CanConfirm {
				if err := s.checkGoalWorkspaceEvidence(observed, outcome.Contract); err != nil {
					return nil, err
				}
			}
		}
	}
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	var decisionErr error
	err := s.commitRun(runID, func(run *Run) ([]*Event, bool) {
		decisionErr = nil
		for _, record := range run.Goals {
			if record == nil {
				continue
			}
			if previous, ok := record.Operations[request.ID]; ok {
				if previous != fingerprint {
					decisionErr = goalInvalid("同一操作标识不能用于不同内容")
				}
				return nil, false
			}
		}
		if run.Version != request.Version || request.EvidenceHash != GoalEvidenceHash(run) {
			decisionErr = ErrVersionConflict
			return nil, false
		}
		if run.Status == RunCancelled && request.Action != "close" {
			decisionErr = goalInvalid("本次执行已取消，无需评估结果")
			return nil, false
		}
		current := run.Goals[scope.ID]
		if issueScoped && issueRecord != nil {
			current = goalRecordForScope(issueRecord, scope)
		}
		record := cloneGoalRecord(current)
		if record == nil || record.Scope.Hash != scope.Hash {
			record = &GoalRecord{Scope: scope, Contract: DefaultGoalContract(scope)}
			if current != nil {
				record.Operations = cloneGoalRecord(current).Operations
			}
		}
		if record.Contract.Revision != request.GoalRevision {
			decisionErr = ErrVersionConflict
			return nil, false
		}
		record.Contract = automaticGoalContract(record.Contract)
		switch request.Action {
		case "define":
			if (len(run.VerifyCommand) > 0 || run.Verify != nil) && request.Contract.Verify.Requirement == "not_required" {
				decisionErr = goalInvalid("本轮已配置验证闸门，必须保留验证要求")
				return nil, false
			}
			if len(goalReviewTasks(run, s.store.Team(run.TeamID))) > 0 && request.Contract.Review.Requirement == "not_required" {
				decisionErr = goalInvalid("本轮已安排审查，必须保留审查要求")
				return nil, false
			}
			if current != nil && request.Contract != nil {
				if record.Contract.Verify.Requirement == "required" && request.Contract.Verify.Requirement != "required" || record.Contract.Review.Requirement == "required" && request.Contract.Review.Requirement != "required" {
					decisionErr = goalInvalid("已约定的必要检查不能降级；请通过修订变更目标")
					return nil, false
				}
				// 已约定条件只能追加；不能事后删掉未实现的要求。
				for _, old := range record.Contract.Criteria {
					found := false
					for _, added := range request.Contract.Criteria {
						if added.ID == old.ID && added.Text == old.Text {
							found = true
							break
						}
					}
					if !found {
						decisionErr = goalInvalid("已有条件不能删除或缩小；请通过后续修订变更目标")
						return nil, false
					}
				}
			}
			record.Contract = *request.Contract
			record.Contract.Criteria = append([]GoalCriterion(nil), request.Contract.Criteria...)
			record.Contract.Revision = request.GoalRevision + 1
			for i := range record.Contract.Criteria {
				record.Contract.Criteria[i].State = "unknown"
				record.Contract.Criteria[i].EvidenceRefs = nil
			}
			record.AssessedBy, record.EvidenceHash, record.Closed = "", "", false
			record.WorkspaceHead, record.WorkspaceDiff = "", ""
		case "assess":
			if len(request.States) != len(record.Contract.Criteria) {
				decisionErr = goalInvalid("请逐项核对全部完成条件")
				return nil, false
			}
			allMet := true
			for i := range record.Contract.Criteria {
				value := request.States[record.Contract.Criteria[i].ID]
				if value != "met" && value != "unmet" && value != "unknown" {
					decisionErr = goalInvalid("条件结论应为满足、不满足或待确认")
					return nil, false
				}
				record.Contract.Criteria[i].State = value
				record.Contract.Criteria[i].EvidenceRefs = goalEvidenceRefs(run)
				allMet = allMet && value == "met"
			}
			if allMet && !evaluateGoalRecord(run, s.store.Team(run.TeamID), scope, record, issueScoped).CanConfirm {
				decisionErr = goalInvalid("结果或必要检查尚未就绪，不能确认目标达成")
				return nil, false
			}
			reason := strings.TrimSpace(request.Reason)
			if reason == "" && !allMet {
				decisionErr = goalInvalid("请说明结果还有哪些问题")
				return nil, false
			}
			if reason == "" {
				reason = "用户确认结果符合要求"
			}
			record.AssessedBy, record.AssessedAt, record.EvidenceHash, record.Reason, record.Closed = "user", time.Now(), GoalEvidenceHash(run), reason, false
			record.WorkspaceHead, record.WorkspaceDiff = goalWorkspaceBinding(run, s.store.Team(run.TeamID), record.Contract)
		case "close":
			if !issueScoped && !terminalRun(run.Status) {
				decisionErr = goalInvalid("请先停止执行，再关闭目标追踪")
				return nil, false
			}
			if strings.TrimSpace(request.Reason) == "" {
				decisionErr = goalInvalid("请记录关闭原因")
				return nil, false
			}
			record.Closed, record.Reason, record.EvidenceHash = true, request.Reason, GoalEvidenceHash(run)
		}
		if record.Operations == nil {
			record.Operations = map[string]string{}
		}
		record.Operations[request.ID] = fingerprint
		if run.Goals == nil {
			run.Goals = map[string]*GoalRecord{}
		}
		run.Goals[scope.ID] = record
		event := newEvent(run.ID, EventGoalReviewed, "user", map[string]any{"goal": goalPayload(record)})
		event.IdempotencyKey = "run:" + run.ID + ":goal-review:" + request.ID
		return []*Event{event}, true
	})
	if err != nil {
		return nil, err
	}
	if decisionErr != nil {
		return nil, decisionErr
	}
	fresh := s.store.Run(runID)
	if err := s.syncIssueGoal(fresh, scope, issueScoped); err != nil {
		return nil, err
	}
	return fresh, nil
}

func goalWorkspaceBinding(run *Run, teamRow *Team, contract GoalContract) (string, string) {
	if contract.Verify.Requirement == "required" && run.Verify != nil {
		return run.Verify.HeadRevision, run.Verify.DiffHash
	}
	if contract.Review.Requirement == "required" {
		for _, task := range goalReviewTasks(run, teamRow) {
			if task.HeadRevision != "" || task.DiffHash != "" {
				return task.HeadRevision, task.DiffHash
			}
		}
	}
	return "", ""
}

func (s *Service) goalWorkspaceEvidenceError(run *Run, contract GoalContract) (string, string, error) {
	checksRequired := contract.Verify.Requirement == "required" || contract.Review.Requirement == "required"
	if !checksRequired {
		return "", "", nil
	}
	teamRow := s.store.Team(run.TeamID)
	if s.Fingerprints == nil || teamRow == nil || teamRow.ProjectCwd == "" {
		return "", "", goalInvalid("无法核对检查结果所对应的当前工作区版本")
	}
	head, diff, err := s.Fingerprints.Fingerprint(teamRow.ProjectCwd)
	if err != nil {
		return "", "", goalInvalid("读取当前工作区版本失败，请重新验证")
	}
	if contract.Verify.Requirement == "required" && (run.Verify == nil || run.Verify.HeadRevision != head || run.Verify.DiffHash != diff) {
		return head, diff, goalInvalid("验证结果对应的工作区版本已变化，请重新验证")
	}
	if contract.Review.Requirement == "required" {
		for _, task := range goalReviewTasks(run, teamRow) {
			if task.HeadRevision != head || task.DiffHash != diff {
				return head, diff, goalInvalid("审查结果对应的工作区版本已变化，请重新审查")
			}
		}
	}
	return head, diff, nil
}

func (s *Service) checkGoalWorkspaceEvidence(run *Run, contract GoalContract) error {
	_, _, err := s.goalWorkspaceEvidenceError(run, contract)
	return err
}

func (s *Service) GoalOutcome(run *Run, scope GoalScope) GoalOutcome {
	return s.goalOutcome(run, scope, nil, false)
}

func (s *Service) IssueGoalOutcome(run *Run, scope GoalScope, record *GoalRecord) GoalOutcome {
	return s.goalOutcome(run, scope, record, true)
}

// GoalOutcome 保留用户结论；未确认结果仍按当前工作区复核必要检查。
func (s *Service) goalOutcome(run *Run, scope GoalScope, issueRecord *GoalRecord, issueScoped bool) GoalOutcome {
	teamRow := s.store.Team(run.TeamID)
	out := evaluateGoalRecord(run, teamRow, scope, issueRecord, issueScoped)
	checksRequired := out.Contract.Verify.Requirement == "required" || out.Contract.Review.Requirement == "required"
	if !checksRequired {
		return out
	}
	if out.AssessedBy == "user" {
		head, diff, err := s.goalWorkspaceEvidenceError(run, out.Contract)
		if err != nil {
			change := &GoalWorkspaceChange{CurrentHead: head, CurrentDiff: diff, Reason: err.Error()}
			record := issueRecord
			if record == nil {
				record = run.Goals[scope.ID]
			}
			if record != nil {
				change.ConfirmedHead, change.ConfirmedDiff = record.WorkspaceHead, record.WorkspaceDiff
			}
			out.WorkspaceChange = change
		}
		return out
	}
	if out.CanConfirm {
		if err := s.checkGoalWorkspaceEvidence(run, out.Contract); err != nil {
			out.Status, out.Label, out.Reason, out.CanConfirm = "pending_verification", GoalOutcomeLabels["pending_verification"], err.Error(), false
		}
	}
	return out
}
