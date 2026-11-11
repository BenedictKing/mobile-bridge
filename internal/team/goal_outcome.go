package team

// 目标评估与执行生命周期独立；全部 Task 正常结束不能代替原始目标的确认。
import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const EventGoalReviewed = "goal_reviewed"

type GoalScope struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Hash string `json:"hash"`
}

type GoalCheck struct {
	Requirement string `json:"requirement"`
	Reason      string `json:"reason,omitempty"`
	Status      string `json:"status"`
}

type GoalCriterion struct {
	ID           string            `json:"id"`
	Text         string            `json:"text"`
	State        string            `json:"state"`
	EvidenceRefs []GoalEvidenceRef `json:"evidenceRefs,omitempty"`
}

type GoalEvidenceRef struct {
	Kind       string `json:"kind"`
	RunID      string `json:"runId"`
	TaskID     string `json:"taskId,omitempty"`
	Attempt    int    `json:"attempt,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	TurnID     string `json:"turnId,omitempty"`
	BundleHash string `json:"bundleHash"`
}

func goalEvidenceRefs(run *Run) []GoalEvidenceRef {
	hash := GoalEvidenceHash(run)
	refs := []GoalEvidenceRef{{Kind: "user_assessment", RunID: run.ID, BundleHash: hash}}
	if result := FinalGoalResult(run); result != nil && result.TaskID != "" {
		refs = append(refs, GoalEvidenceRef{Kind: "task_result", RunID: run.ID, TaskID: result.TaskID, Attempt: result.Attempt, SessionID: result.SessionID, TurnID: result.TurnID, BundleHash: hash})
	}
	return refs
}

type GoalContract struct {
	Revision        uint64          `json:"revision"`
	DeliverableKind string          `json:"deliverableKind"`
	Criteria        []GoalCriterion `json:"criteria"`
	Verify          GoalCheck       `json:"verify"`
	Review          GoalCheck       `json:"review"`
}

type GoalRecord struct {
	Scope        GoalScope         `json:"scope"`
	Contract     GoalContract      `json:"contract"`
	EvidenceHash string            `json:"evidenceHash,omitempty"`
	AssessedBy   string            `json:"assessedBy,omitempty"`
	AssessedAt   time.Time         `json:"assessedAt,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	Closed       bool              `json:"closed,omitempty"`
	Operations   map[string]string `json:"operations,omitempty"`
}

type GoalResultRef struct {
	TaskID       string `json:"taskId"`
	Attempt      int    `json:"attempt"`
	SubmissionID string `json:"submissionId,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	TurnID       string `json:"turnId,omitempty"`
	Text         string `json:"text"`
	Truncated    bool   `json:"truncated"`
}

type GoalOutcome struct {
	SchemaVersion   int            `json:"schemaVersion"`
	ExecutionStatus string         `json:"executionStatus"`
	Scope           GoalScope      `json:"scope"`
	Status          string         `json:"status"`
	Label           string         `json:"label"`
	Reason          string         `json:"reason"`
	Contract        GoalContract   `json:"contract"`
	EvidenceHash    string         `json:"evidenceHash"`
	Result          *GoalResultRef `json:"result,omitempty"`
	AssessedBy      string         `json:"assessedBy,omitempty"`
	AssessedAt      time.Time      `json:"assessedAt,omitempty"`
	CanConfirm      bool           `json:"canConfirm"`
	Closed          bool           `json:"closed,omitempty"`
}

var GoalOutcomeLabels = map[string]string{
	"unassessed": "尚未评估", "pending_verification": "待验证", "pending_confirmation": "结果待确认",
	"partial": "部分达成", "unsatisfied": "未达成", "satisfied": "目标已达成",
	"cancelled": "已取消",
}

func goalHash(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func NewGoalScope(id, text string) GoalScope {
	return GoalScope{ID: id, Text: text, Hash: goalHash([]string{id, text})}
}

// RunGoalScope 包含本轮追加的原始输入；后续反馈不能静默沿用旧确认。
func RunGoalScope(run *Run) GoalScope {
	texts := []string{run.Goal}
	for _, task := range run.Tasks {
		if strings.HasPrefix(task.ID, "input-") && task.ExecutionContext == ExecutionContextLeadControl {
			texts = append(texts, task.Prompt)
		}
	}
	return NewGoalScope("run", strings.Join(texts, "\n\n"))
}

// GoalEvidenceHash 不包含评估记录或 Run.Version，重复确认不改变证据身份。
func GoalEvidenceHash(run *Run) string {
	return goalHash(struct {
		Status    string
		Tasks     []Task
		Verify    *VerifyResult
		Command   []string
		Questions []PendingQuestion
		Actions   []Action
	}{run.ID + ":" + run.Status, run.Tasks, run.Verify, run.VerifyCommand, run.PendingQuestions, run.PendingActions})
}

func FinalGoalResult(run *Run) *GoalResultRef {
	var candidates []*Task
	var reduction *Task
	for i := range run.Tasks {
		task := &run.Tasks[i]
		text := task.Result
		if task.ResultDetail != nil && text == "" {
			text = task.ResultDetail.Result
		}
		isReview := task.Review != nil && *task.Review || task.ResultDetail != nil && task.ResultDetail.ReviewVerdict != ""
		if task.Status != TaskCompleted || task.IssueClassificationOnly || isReview || strings.TrimSpace(ResultDisplayText(text)) == "" {
			continue
		}
		candidates = append(candidates, task)
		if task.Reduction {
			reduction = task
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	selected := candidates[0]
	if reduction != nil && reduction == candidates[len(candidates)-1] {
		selected = reduction
	} else if len(candidates) > 1 {
		ref := &GoalResultRef{}
		texts := []string{}
		for _, task := range candidates {
			text := task.Result
			if task.ResultDetail != nil {
				if text == "" {
					text = task.ResultDetail.Result
				}
				ref.Truncated = ref.Truncated || task.ResultDetail.Truncated
			}
			texts = append(texts, task.ID+"："+ResultDisplayText(text))
		}
		ref.Text = strings.Join(texts, "\n\n")
		if len([]rune(ref.Text)) > 12000 {
			ref.Text = truncateRunes(ref.Text, 12000)
			ref.Truncated = true
		}
		return ref
	}
	ref := &GoalResultRef{TaskID: selected.ID, Attempt: selected.Attempt, SubmissionID: selected.SubmissionID, Text: ResultDisplayText(selected.Result)}
	if detail := selected.ResultDetail; detail != nil {
		ref.SessionID, ref.TurnID, ref.Truncated = detail.SessionID, detail.TurnID, detail.Truncated
		if ref.Text == "" {
			ref.Text = ResultDisplayText(detail.Result)
		}
	}
	return ref
}

func DefaultGoalContract(scope GoalScope) GoalContract {
	return GoalContract{Revision: 1, DeliverableKind: "result",
		Criteria: []GoalCriterion{{ID: "original", Text: scope.Text, State: "unknown"}},
		Verify:   GoalCheck{Requirement: "auto", Status: "not_configured"},
		Review:   GoalCheck{Requirement: "auto", Status: "not_configured"}}
}

// 旧记录中未填写的配置按自动处理；显式约定和原始完成条件保持不变。
func automaticGoalContract(contract GoalContract) GoalContract {
	if contract.DeliverableKind == "" || contract.DeliverableKind == "unspecified" {
		contract.DeliverableKind = "result"
	}
	if contract.Verify.Requirement == "" || contract.Verify.Requirement == "unspecified" {
		contract.Verify.Requirement = "auto"
	}
	if contract.Review.Requirement == "" || contract.Review.Requirement == "unspecified" {
		contract.Review.Requirement = "auto"
	}
	return contract
}

// 同一被审任务集合的新审查可取代旧审查；不同分支的失败不能被另一条通过覆盖。
func goalReviewTasks(run *Run, teamRow *Team) []*Task {
	var selected []*Task
	positions := map[string]int{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if !IsReviewTask(teamRow, task) {
			continue
		}
		refs := append([]string(nil), task.ReviewOf...)
		sort.Strings(refs)
		key := task.MemberID + "|" + strings.Join(refs, "|")
		if index, exists := positions[key]; exists {
			selected[index] = task
		} else {
			positions[key] = len(selected)
			selected = append(selected, task)
		}
	}
	return selected
}

func goalCheckEvidence(run *Run, teamRow *Team, contract GoalContract) (GoalCheck, GoalCheck) {
	verify, review := contract.Verify, contract.Review
	verify.Status, review.Status = "not_configured", "not_configured"
	if len(run.VerifyCommand) > 0 || run.Verify != nil {
		verify.Requirement = "required" // 既有验证约定不能通过确认界面降级。
		verify.Status = "pending"
	}
	if run.Verify != nil {
		verify.Status = "passed"
		if run.Verify.ExitCode != 0 {
			verify.Status = "failed"
		}
		if verify.Requirement == "required" && verify.Status == "passed" && (run.Verify.HeadRevision == "" || run.Verify.DiffHash == "") {
			verify.Status = "unknown"
		}
	}
	priority := map[string]int{"not_configured": 0, "passed": 1, "pending": 2, "unknown": 3, "stale": 4, "failed": 5}
	reviews := goalReviewTasks(run, teamRow)
	if len(reviews) > 0 {
		review.Requirement = "required"
	}
	for _, task := range reviews {
		status := "pending"
		if task.Status == TaskCompleted {
			status = "unknown"
			if task.ResultDetail != nil && task.ResultDetail.ReviewVerdict != "" {
				status = task.ResultDetail.ReviewVerdict
			}
		} else if terminalTask(task.Status) {
			status = "failed"
		}
		if task.ReviewVerdictStale {
			status = "stale"
		}
		if review.Requirement == "required" && status == "passed" && (task.HeadRevision == "" || task.DiffHash == "" || len(task.ReviewOf) == 0) {
			status = "unknown"
		}
		if priority[status] > priority[review.Status] {
			review.Status = status
		}
	}
	return verify, review
}

// EvaluateGoal 是所有 API 和界面共用的纯判定。缺记录的旧 Run 保守投影，绝不补造通过事实。
func EvaluateGoal(run *Run, teamRow *Team, scope GoalScope) GoalOutcome {
	out := GoalOutcome{SchemaVersion: 1, ExecutionStatus: run.Status, Scope: scope, Status: "unassessed", Reason: "尚无可查看的最终结果。",
		Contract: DefaultGoalContract(scope), EvidenceHash: GoalEvidenceHash(run), Result: FinalGoalResult(run)}
	record := run.Goals[scope.ID]
	if record != nil && record.Scope.Hash == scope.Hash {
		out.Contract = record.Contract
		out.Contract.Criteria = append([]GoalCriterion(nil), record.Contract.Criteria...)
	}
	out.Contract = automaticGoalContract(out.Contract)
	out.Contract.Verify, out.Contract.Review = goalCheckEvidence(run, teamRow, out.Contract)
	if run.Status == RunCancelled {
		out.Status, out.Label, out.Reason = "cancelled", GoalOutcomeLabels["cancelled"], "本次执行已取消，已有结果保留。"
		if run.StopReason != "" {
			out.Reason = run.StopReason
		}
		return out
	}
	blocked := run.Status != RunCompleted
	for _, task := range run.Tasks {
		if task.Status == TaskOutcomeUnknown {
			blocked = true
		}
	}
	for _, question := range run.PendingQuestions {
		if question.Status == QuestionStatusOpen {
			blocked = true
		}
	}
	if out.Result != nil && run.Status == RunCompleted {
		out.Status, out.Reason = "pending_confirmation", "本轮执行已结束，请查看最终结果并核对原始要求。"
	}
	for _, check := range []GoalCheck{out.Contract.Verify, out.Contract.Review} {
		if check.Requirement == "required" && check.Status != "passed" {
			out.Status, out.Reason = "pending_verification", "必要的验证或审查尚未有效通过。"
			blocked = true
		}
	}
	out.CanConfirm = !blocked && out.Result != nil
	if record != nil && record.Scope.Hash == scope.Hash && record.EvidenceHash == out.EvidenceHash {
		out.AssessedBy, out.AssessedAt, out.Closed = record.AssessedBy, record.AssessedAt, record.Closed
		if record.AssessedBy == "user" {
			met, unmet, unknown := 0, 0, 0
			for _, criterion := range record.Contract.Criteria {
				switch criterion.State {
				case "met":
					met++
				case "unmet":
					unmet++
				default:
					unknown++
				}
			}
			if met > 0 && unmet+unknown > 0 {
				out.Status, out.Reason = "partial", record.Reason
			} else if unmet > 0 {
				out.Status, out.Reason = "unsatisfied", record.Reason
			} else if met > 0 && unknown+unmet == 0 && out.CanConfirm {
				out.Status, out.Reason = "satisfied", record.Reason
			}
		}
	} else {
		for i := range out.Contract.Criteria {
			out.Contract.Criteria[i].State = "unknown"
			out.Contract.Criteria[i].EvidenceRefs = nil
		}
		if record != nil {
			out.Reason = "目标或执行证据已变化，请重新核对结果。"
		}
	}
	out.Label = GoalOutcomeLabels[out.Status]
	if out.Closed {
		out.Label = "已关闭"
		out.Reason = record.Reason
		out.CanConfirm = false
	}
	return out
}

func cloneGoalRecord(record *GoalRecord) *GoalRecord {
	if record == nil {
		return nil
	}
	raw, _ := json.Marshal(record)
	var copy GoalRecord
	_ = json.Unmarshal(raw, &copy)
	return &copy
}

func sortedGoalIDs(goals map[string]*GoalRecord) []string {
	ids := make([]string, 0, len(goals))
	for id := range goals {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func goalPayload(record *GoalRecord) map[string]any {
	raw, _ := json.Marshal(record)
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	return payload
}

func decodeGoalRecord(value any) *GoalRecord {
	raw, err := json.Marshal(treeToGo(value))
	if err != nil {
		return nil
	}
	var record GoalRecord
	if json.Unmarshal(raw, &record) != nil || record.Scope.ID == "" {
		return nil
	}
	return &record
}

func validateGoalContract(contract GoalContract, scope GoalScope) error {
	switch contract.DeliverableKind {
	case "result", "answer", "investigation", "change", "operation":
	default:
		return fmt.Errorf("请选择交付类型")
	}
	if len(contract.Criteria) < 1 || len(contract.Criteria) > 20 || contract.Criteria[0].ID != "original" || contract.Criteria[0].Text != scope.Text {
		return fmt.Errorf("完成条件必须保留完整原始要求，且不超过 20 项")
	}
	seen := map[string]bool{}
	for _, criterion := range contract.Criteria {
		if criterion.ID == "" || len(criterion.ID) > 100 || seen[criterion.ID] || strings.TrimSpace(criterion.Text) == "" || criterion.ID != "original" && len([]rune(criterion.Text)) > 10000 {
			return fmt.Errorf("完成条件标识应唯一，内容应为 1–10000 字")
		}
		seen[criterion.ID] = true
	}
	for _, check := range []GoalCheck{contract.Verify, contract.Review} {
		if len([]rune(check.Reason)) > 2000 {
			return fmt.Errorf("检查原因不能超过 2000 字")
		}
		if check.Requirement != "auto" && check.Requirement != "required" && check.Requirement != "not_required" {
			return fmt.Errorf("请明确验证和审查要求")
		}
		if check.Requirement == "not_required" && strings.TrimSpace(check.Reason) == "" {
			return fmt.Errorf("不需要检查时请说明原因")
		}
	}
	return nil
}
