package team

// swarm.go 复用 Run journal 保存执行前契约、候选和检查事实；不以模型自评签发目标达成。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/swarmeval"
	"github.com/google/uuid"
)

const EventSwarmUpdated = "swarm_updated"

var ErrSwarmInvalid = errors.New("Swarm 请求无效")

type SwarmCriterion struct {
	ID        string   `json:"id"`
	Text      string   `json:"text"`
	CheckKind string   `json:"checkKind"`
	Required  bool     `json:"required"`
	IssueIDs  []string `json:"issueIds,omitempty"`
}
type SwarmEvalSpec struct {
	ID             string   `json:"id"`
	CriterionIDs   []string `json:"criterionIds"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	WorkspaceID    string   `json:"workspaceId,omitempty"`
}
type SwarmTaskBinding struct {
	TaskID            string   `json:"taskId"`
	CriterionIDs      []string `json:"criterionIds"`
	AllowedWritePaths []string `json:"allowedWritePaths"`
	ProtectedPaths    []string `json:"protectedPaths,omitempty"`
	EvalSpecIDs       []string `json:"evalSpecIds"`
	InputTaskIDs      []string `json:"inputTaskIds,omitempty"`
}
type SwarmContract struct {
	Revision                 uint64             `json:"revision"`
	OriginalGoal             string             `json:"originalGoal"`
	Criteria                 []SwarmCriterion   `json:"criteria"`
	EvalSpecs                []SwarmEvalSpec    `json:"evalSpecs"`
	Tasks                    []SwarmTaskBinding `json:"tasks"`
	RequireIndependentReview bool               `json:"requireIndependentReview"`
	MaxRepairs               int                `json:"maxRepairs"`
	MaxEvals                 int                `json:"maxEvals"`
	IntegrationWorkspaceID   string             `json:"integrationWorkspaceId,omitempty"`
}
type SwarmProposal struct {
	ID       string        `json:"id"`
	Contract SwarmContract `json:"contract"`
	Source   string        `json:"source"`
}
type SwarmArtifact struct {
	ID           string   `json:"id"`
	TaskID       string   `json:"taskId,omitempty"`
	Attempt      int      `json:"attempt"`
	Kind         string   `json:"kind"`
	WorkspaceID  string   `json:"workspaceId,omitempty"`
	ContentHash  string   `json:"contentHash"`
	BaseRevision string   `json:"baseRevision"`
	ChangedPaths []string `json:"changedPaths"`
	Inputs       []string `json:"inputs,omitempty"`
	Validity     string   `json:"validity"`
	Isolation    string   `json:"isolation"`
	ReviewTaskID string   `json:"reviewTaskId,omitempty"`
}
type SwarmEval struct {
	ID              string    `json:"id"`
	TaskID          string    `json:"taskId,omitempty"`
	ArtifactID      string    `json:"artifactId"`
	EvalSpecID      string    `json:"evalSpecId"`
	Attempt         int       `json:"attempt"`
	Status          string    `json:"status"`
	Validity        string    `json:"validity"`
	Reason          string    `json:"reason,omitempty"`
	ExitCode        int       `json:"exitCode"`
	OutputHead      string    `json:"outputHead,omitempty"`
	Truncated       bool      `json:"truncated,omitempty"`
	LogHash         string    `json:"logHash,omitempty"`
	EnvironmentHash string    `json:"environmentHash,omitempty"`
	StartedAt       time.Time `json:"startedAt,omitempty"`
	FinishedAt      time.Time `json:"finishedAt,omitempty"`
}
type SwarmFinding struct {
	ID           string `json:"id"`
	CriterionID  string `json:"criterionId"`
	TaskID       string `json:"taskId,omitempty"`
	ArtifactID   string `json:"artifactId"`
	Severity     string `json:"severity"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	Signature    string `json:"signature"`
	RepairTaskID string `json:"repairTaskId,omitempty"`
}
type SwarmState struct {
	Policy               *TeamPolicy        `json:"policy,omitempty"`
	Deadline             time.Time          `json:"deadline,omitempty"`
	ScopeHash            string             `json:"scopeHash,omitempty"`
	VerifiedEvidenceHash string             `json:"verifiedEvidenceHash,omitempty"`
	TaskInputs           map[string]string  `json:"taskInputs,omitempty"`
	Sequence             uint64             `json:"sequence,omitempty"`
	RepairBindings       []SwarmTaskBinding `json:"repairBindings,omitempty"`
	SchemaVersion        int                `json:"schemaVersion"`
	ChainID              string             `json:"chainId"`
	Status               string             `json:"status"`
	Reason               string             `json:"reason,omitempty"`
	Proposal             *SwarmProposal     `json:"proposal,omitempty"`
	Contract             *SwarmContract     `json:"contract,omitempty"`
	AcceptedAt           time.Time          `json:"acceptedAt,omitempty"`
	ContractHash         string             `json:"contractHash,omitempty"`
	Artifacts            []SwarmArtifact    `json:"artifacts"`
	Evals                []SwarmEval        `json:"evals"`
	Findings             []SwarmFinding     `json:"findings"`
	Operations           map[string]string  `json:"operations,omitempty"`
}
type SwarmRequest struct {
	ArtifactID    string        `json:"artifactId,omitempty"`
	ID            string        `json:"id"`
	Version       uint64        `json:"version"`
	SchemaVersion int           `json:"schemaVersion"`
	Contract      SwarmContract `json:"contract"`
	TaskID        string        `json:"taskId,omitempty"`
	EvalSpecID    string        `json:"evalSpecId,omitempty"`
	ArtifactIDs   []string      `json:"artifactIds,omitempty"`
}

func cloneSwarm(value *SwarmState) *SwarmState {
	if value == nil {
		return nil
	}
	raw, _ := json.Marshal(value)
	var copy SwarmState
	_ = json.Unmarshal(raw, &copy)
	if copy.Operations == nil {
		copy.Operations = map[string]string{}
	}
	return &copy
}
func swarmInvalid(format string, args ...any) error {
	return fmt.Errorf("%w：%s", ErrSwarmInvalid, fmt.Sprintf(format, args...))
}
func swarmEvent(run *Run, actor string) *Event {
	return newEvent(run.ID, EventSwarmUpdated, actor, map[string]any{"swarm": goValue(run.Swarm)})
}
func decodeSwarm(value any) *SwarmState {
	raw, err := json.Marshal(treeToGo(value))
	if err != nil {
		return nil
	}
	var state SwarmState
	if json.Unmarshal(raw, &state) != nil || state.SchemaVersion != 1 {
		return nil
	}
	if state.Operations == nil {
		state.Operations = map[string]string{}
	}
	return &state
}
func newSwarm(runID string) *SwarmState {
	return &SwarmState{SchemaVersion: 1, ChainID: runID, Status: "awaiting_contract", Artifacts: []SwarmArtifact{}, Evals: []SwarmEval{}, Findings: []SwarmFinding{}, Operations: map[string]string{}}
}

// 全部相对路径按字面量匹配，不能通过 glob、路径穿越或 git pathspec 扩大范围。
func validSwarmPaths(paths []string) bool {
	if len(paths) == 0 || len(paths) > 64 {
		return false
	}
	for _, p := range paths {
		if p == "." {
			continue
		}
		clean := strings.TrimSuffix(p, "/")
		if clean == "" || path.Clean(clean) != clean || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsAny(clean, "\\:*?[]\x00\r\n") || clean == ".git" || strings.HasPrefix(clean, ".git/") {
			return false
		}
	}
	return true
}
func pathAllowed(name string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSuffix(p, "/")
		if p == "." || name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}
func validateSwarmContract(run *Run, c *SwarmContract) error {
	if RunGoalScope(run).Text != run.Goal {
		return swarmInvalid("原始要求已追加，请创建修订后冻结新契约")
	}
	if len(c.Criteria) < 1 || len(c.Criteria) > 20 || len(c.EvalSpecs) > 12 || len(c.Tasks) > 48 {
		return swarmInvalid("完成条件、检查或任务数量超限")
	}
	c.OriginalGoal = run.Goal
	criteria := map[string]SwarmCriterion{}
	for _, v := range c.Criteria {
		if len(v.IssueIDs) > 20 {
			return swarmInvalid("事项引用数量超限")
		}
		if v.Required && v.CheckKind == "review" && !c.RequireIndependentReview {
			return swarmInvalid("必需审查条件需要启用独立审查")
		}
		if !ValidMemberID(v.ID) || strings.TrimSpace(v.Text) == "" || len([]rune(v.Text)) > 10000 {
			return swarmInvalid("完成条件标识或正文无效")
		}
		if _, ok := criteria[v.ID]; ok {
			return swarmInvalid("完成条件重复")
		}
		switch v.CheckKind {
		case "deterministic", "review", "human":
		default:
			return swarmInvalid("不支持的检查类型")
		}
		criteria[v.ID] = v
	}
	if original, ok := criteria["original"]; !ok || original.Text != run.Goal || !original.Required {
		return swarmInvalid("必须保留完整原始要求 original，且为必需条件")
	}
	covered := map[string]bool{}
	specs := map[string]bool{}
	for i := range c.EvalSpecs {
		v := &c.EvalSpecs[i]
		if !ValidMemberID(v.ID) || specs[v.ID] || len(v.CriterionIDs) == 0 {
			return swarmInvalid("检查标识重复或缺少条件引用")
		}
		if err := ValidateVerifyCommand(v.Command); err != nil {
			return swarmInvalid("%v", err)
		}
		if v.TimeoutSeconds == 0 {
			v.TimeoutSeconds = 600
		}
		if v.TimeoutSeconds < 1 || v.TimeoutSeconds > 600 {
			return swarmInvalid("评测超时须为 1–600 秒")
		}
		if v.WorkspaceID != "" && !ValidMemberID(v.WorkspaceID) {
			return swarmInvalid("工作区标识无效")
		}
		specs[v.ID] = true
		for _, id := range v.CriterionIDs {
			if _, ok := criteria[id]; !ok {
				return swarmInvalid("检查引用未知条件 %s", id)
			}
			covered[id] = true
		}
	}
	for _, v := range c.Criteria {
		if v.Required && v.CheckKind == "deterministic" && !covered[v.ID] {
			return swarmInvalid("确定性条件 %s 缺少检查", v.ID)
		}
	}
	seen := map[string]bool{}
	for _, v := range c.Tasks {
		if len(v.CriterionIDs) > 20 || len(v.EvalSpecIDs) > 12 || len(v.InputTaskIDs) > 48 {
			return swarmInvalid("任务引用数量超限")
		}
		if v.TaskID != "*" && !ValidMemberID(v.TaskID) || seen[v.TaskID] || !validSwarmPaths(v.AllowedWritePaths) {
			return swarmInvalid("任务绑定或写入范围无效")
		}
		seen[v.TaskID] = true
		if len(v.ProtectedPaths) > 0 && !validSwarmPaths(v.ProtectedPaths) {
			return swarmInvalid("受保护路径无效")
		}
		for _, id := range v.CriterionIDs {
			if _, ok := criteria[id]; !ok {
				return swarmInvalid("任务引用未知条件")
			}
		}
		for _, id := range v.EvalSpecIDs {
			if !specs[id] {
				return swarmInvalid("任务引用未知检查")
			}
		}
		for _, id := range v.InputTaskIDs {
			if !ValidMemberID(id) || id == v.TaskID {
				return swarmInvalid("输入产物引用无效")
			}
		}
	}
	if c.MaxRepairs < 0 || c.MaxRepairs > 2 {
		return swarmInvalid("自动修复上限为 0–2")
	}
	if c.MaxEvals == 0 {
		c.MaxEvals = 12
	}
	if c.MaxEvals < 1 || c.MaxEvals > 64 {
		return swarmInvalid("评测上限为 1–64")
	}
	if c.IntegrationWorkspaceID != "" {
		return swarmInvalid("集成当前使用受管理的独立候选目录，无需设置工作区标识")
	}
	return nil
}

func (s *Service) swarmCommand(runID, action, target string, request SwarmRequest, mutate func(*Run) error) (*Run, error) {
	if _, err := uuid.Parse(request.ID); err != nil || request.SchemaVersion != 1 {
		return nil, swarmInvalid("需要操作 UUID 与 schemaVersion=1")
	}
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(runID)
	if run == nil {
		return nil, ErrNotFound
	}
	if err := s.syncSwarmLocked(run); err != nil {
		return nil, err
	}
	run = s.store.Run(runID)
	if run.Swarm == nil {
		return nil, swarmInvalid("此运行未启用 Swarm")
	}
	if run.Swarm.Operations == nil {
		run.Swarm.Operations = map[string]string{}
	}
	content := request
	content.Version = 0
	digest := goalHash([]any{action, target, content})
	if previous, ok := run.Swarm.Operations[request.ID]; ok {
		if previous != digest {
			return nil, ErrRunContentMismatch
		}
		return run, nil
	}
	if run.Version != request.Version {
		return nil, ErrVersionConflict
	}
	if run.Status == RunCancelled || run.Status == RunOutcomeUnknown {
		return nil, ErrRunFinished
	}
	if err := mutate(run); err != nil {
		return nil, err
	}
	if run.Swarm.Operations == nil {
		run.Swarm.Operations = map[string]string{}
	}
	if len(run.Swarm.Operations) >= 256 {
		return nil, swarmInvalid("操作记录已达上限，请建立修订")
	}
	run.Swarm.Operations[request.ID] = digest
	if err := s.saveSwarmLocked(run, "user"); err != nil {
		return nil, err
	}
	s.wakeLocked(state)
	if !terminalRun(run.Status) {
		s.startLoopLocked(state)
	}
	return s.store.Run(runID), nil
}

// saveSwarmLocked 追加完整且有界的状态事实，先 journal 后快照；写入失败可由相同操作对账。
func (s *Service) saveSwarmLocked(run *Run, actor string) error {
	event := swarmEvent(run, actor)
	if err := s.journal.Append(event); err != nil {
		return err
	}
	run.Swarm.Sequence = event.Sequence
	s.emit(event)
	run.UpdatedAt = time.Now()
	return s.store.SaveRun(run)
}

// journal 已接受但快照未写成时先恢复事实，禁止重发同一外部操作。
func (s *Service) syncSwarmLocked(run *Run) error {
	if run.Swarm == nil {
		return nil
	}
	events, err := s.journal.Replay()
	if err != nil {
		return err
	}
	changed := false
	for _, event := range events {
		if event.RunID != run.ID {
			continue
		}
		if event.Type == EventTaskPlanned && event.Sequence > run.Swarm.Sequence {
			if _, ok := event.Payload["swarmState"]; ok {
				applyJournalEventToRun(run, event)
				changed = true
				continue
			}
		}
		if event.Type == EventSwarmUpdated && event.Sequence > run.Swarm.Sequence {
			if v := decodeSwarm(event.Payload["swarm"]); v != nil {
				v.Sequence = event.Sequence
				run.Swarm = v
				changed = true
			}
		}
		if event.Type == EventTaskPlanned && findTaskIn(run, payloadString(event.Payload, "taskId")) == nil {
			if _, ok := event.Payload["swarmTask"]; ok {
				applyJournalEventToRun(run, event)
				changed = true
			}
		}
	}
	if changed {
		return s.store.SaveRun(run)
	}
	return nil
}
func (s *Service) ProposeSwarmContract(runID string, request SwarmRequest) (*Run, error) {
	return s.swarmCommand(runID, "propose", "", request, func(run *Run) error {
		if run.Swarm.Contract != nil {
			return swarmInvalid("契约已冻结，请建立修订")
		}
		if err := validateSwarmContract(run, &request.Contract); err != nil {
			return err
		}
		if err := s.validateSwarmScopes(run, request.Contract); err != nil {
			return err
		}
		request.Contract.Revision = 1
		run.Swarm.Proposal = &SwarmProposal{ID: request.ID, Contract: request.Contract, Source: "user"}
		run.Swarm.Status = "awaiting_confirmation"
		run.Swarm.Reason = "请核对完成条件及检查规格"
		return nil
	})
}
func (s *Service) AcceptSwarmContract(runID, proposalID string, request SwarmRequest) (*Run, error) {
	return s.swarmCommand(runID, "accept", proposalID, request, func(run *Run) error {
		if run.Swarm.Contract != nil {
			return swarmInvalid("契约已冻结")
		}
		if run.Swarm.Proposal == nil || run.Swarm.Proposal.ID != proposalID {
			return swarmInvalid("契约提案不存在")
		}
		c := run.Swarm.Proposal.Contract
		if err := validateSwarmContract(run, &c); err != nil {
			return err
		}
		if err := s.validateSwarmScopes(run, c); err != nil {
			return err
		}
		run.Swarm.Contract = &c
		run.Swarm.ScopeHash = RunGoalScope(run).Hash
		run.Swarm.ContractHash = goalHash(c)
		run.Swarm.AcceptedAt = time.Now()
		run.Swarm.Status = "running"
		run.Swarm.Reason = ""
		return nil
	})
}
func swarmBinding(run *Run, task *Task) *SwarmTaskBinding {
	if run.Swarm == nil || run.Swarm.Contract == nil {
		return nil
	}
	var fallback *SwarmTaskBinding
	for i := range run.Swarm.RepairBindings {
		if run.Swarm.RepairBindings[i].TaskID == task.ID {
			return &run.Swarm.RepairBindings[i]
		}
	}
	for i := range run.Swarm.Contract.Tasks {
		v := &run.Swarm.Contract.Tasks[i]
		if v.TaskID == task.ID {
			return v
		}
		if v.TaskID == "*" {
			fallback = v
		}
	}
	return fallback
}
func swarmWorker(team *Team, task *Task) bool {
	return task != nil && !task.Reduction && !task.IssueClassificationOnly && !IsReviewTask(team, task) && task.ExecutionContext != ExecutionContextLeadControl && task.WriteLock != TaskWriteLockNone
}
func swarmArtifact(run *Run, id string) *SwarmArtifact {
	if run.Swarm != nil {
		for i := range run.Swarm.Artifacts {
			if run.Swarm.Artifacts[i].ID == id {
				return &run.Swarm.Artifacts[i]
			}
		}
	}
	return nil
}
func swarmTaskArtifact(run *Run, taskID string) *SwarmArtifact {
	if run.Swarm != nil {
		visited := map[string]bool{}
		for !visited[taskID] {
			visited[taskID] = true
			next := ""
			for _, f := range run.Swarm.Findings {
				if f.TaskID == taskID && f.RepairTaskID != "" {
					next = f.RepairTaskID
				}
			}
			if next == "" {
				break
			}
			taskID = next
		}
		for i := len(run.Swarm.Artifacts) - 1; i >= 0; i-- {
			a := &run.Swarm.Artifacts[i]
			if a.TaskID == taskID {
				return a
			}
		}
	}
	return nil
}
func swarmEvalSpec(run *Run, id string) *SwarmEvalSpec {
	if run.Swarm != nil && run.Swarm.Contract != nil {
		for i := range run.Swarm.Contract.EvalSpecs {
			if run.Swarm.Contract.EvalSpecs[i].ID == id {
				return &run.Swarm.Contract.EvalSpecs[i]
			}
		}
	}
	return nil
}

func swarmDirectArtifact(run *Run, taskID string) *SwarmArtifact {
	if run.Swarm != nil {
		for i := len(run.Swarm.Artifacts) - 1; i >= 0; i-- {
			a := &run.Swarm.Artifacts[i]
			if a.TaskID == taskID {
				return a
			}
		}
	}
	return nil
}
func latestSwarmEval(run *Run, artifactID, specID string) *SwarmEval {
	for i := len(run.Swarm.Evals) - 1; i >= 0; i-- {
		v := &run.Swarm.Evals[i]
		if v.ArtifactID == artifactID && v.EvalSpecID == specID {
			return v
		}
	}
	return nil
}
func (s *Service) RequestSwarmEval(runID, target string, request SwarmRequest) (*Run, error) {
	s.swarmBudgetMu.Lock()
	defer s.swarmBudgetMu.Unlock()
	result, err := s.swarmCommand(runID, "evaluate", target, request, func(run *Run) error {
		if run.Status == RunPaused || run.Status == RunWaitingInput {
			return swarmInvalid("暂停或等待处理期间不能发起评测")
		}
		if run.Status == RunCompleted {
			for _, v := range run.Swarm.Evals {
				if v.Status == "pending" || v.Status == "running" {
					return swarmInvalid("此运行已有待执行或进行中的复验，请等待结算后重试")
				}
			}
		}
		if run.Swarm.Contract == nil {
			return swarmInvalid("请先接受契约")
		}
		if len(run.Swarm.Evals) >= 64 {
			return swarmInvalid("评测记录已达上限")
		}
		if target != "" {
			found := false
			for _, v := range run.Swarm.Evals {
				if v.ID == target {
					if v.Status == "outcome_unknown" {
						return swarmInvalid("上一次评测结果待核查，禁止自动重跑")
					}
					found = true
					request.TaskID = v.TaskID
					request.EvalSpecID = v.EvalSpecID
					break
				}
			}
			if !found {
				return swarmInvalid("评测记录不存在")
			}
		}
		if request.TaskID == "" {
			for i := len(run.Swarm.Artifacts) - 1; i >= 0; i-- {
				if run.Swarm.Artifacts[i].Kind == "integration" {
					request.TaskID = "integration"
					break
				}
			}
		}
		a := swarmTaskArtifact(run, request.TaskID)
		if request.ArtifactID != "" {
			a = swarmArtifact(run, request.ArtifactID)
		}
		if a == nil {
			return swarmInvalid("尚无该任务候选，请等待任务完成")
		}
		if request.EvalSpecID == "" && len(run.Swarm.Contract.EvalSpecs) > 0 {
			request.EvalSpecID = run.Swarm.Contract.EvalSpecs[0].ID
		}
		if swarmEvalSpec(run, request.EvalSpecID) == nil {
			return swarmInvalid("检查规格不存在")
		}
		if v := latestSwarmEval(run, a.ID, request.EvalSpecID); v != nil && (v.Status == "pending" || v.Status == "running") {
			return swarmInvalid("同一检查正在执行")
		}
		if s.swarmEvalCount(run) >= run.Swarm.Contract.MaxEvals {
			return swarmInvalid("执行链评测预算已用尽")
		}
		n := 1
		for _, v := range run.Swarm.Evals {
			if v.ArtifactID == a.ID && v.EvalSpecID == request.EvalSpecID {
				n++
			}
		}
		run.Swarm.Evals = append(run.Swarm.Evals, SwarmEval{ID: request.ID, TaskID: a.TaskID, ArtifactID: a.ID, EvalSpecID: request.EvalSpecID, Attempt: n, Status: "pending", Validity: "unknown", ExitCode: -1})
		run.Swarm.Status = "evaluating"
		return nil
	})
	if err == nil && result.Status == RunCompleted && s.SwarmRunner != nil {
		go func() {
			for _, v := range result.Swarm.Evals {
				if v.Status == "pending" {
					s.executeSwarmEval(s.stateFor(runID), v)
				}
			}
		}()
	}
	return result, err
}
func (s *Service) swarmEvalCount(run *Run) int {
	n := 0
	for _, r := range s.store.RunsByTeam(run.TeamID) {
		if r.Swarm != nil && r.Swarm.ChainID == run.Swarm.ChainID {
			n += len(r.Swarm.Evals)
		}
	}
	return n
}

// SwarmBriefing 的契约、检查和上下文都进入既有 SentText/inputHash，并可追溯原派发。
func SwarmBriefing(run *Run, task *Task) string {
	if run == nil || run.Swarm == nil {
		return ""
	}
	v := run.Swarm
	if v.Contract == nil {
		return "\n[Swarm contract]\n先提出可核验的完成条件、检查规格和任务绑定；原始要求不可缩小。完成规划后在 <team-swarm-contract> 中输出 JSON，字段 criteria（含 original 原文、checkKind、required）、evalSpecs（固定 command argv）、tasks（taskId 或 *、criterionIds、allowedWritePaths、evalSpecIds）、maxRepairs（0–2）、maxEvals。契约需用户接受才可执行写入。不得把模型自评分当作成功。\n"
	}
	raw, _ := json.Marshal(v.Contract)
	text := "\n[Swarm frozen contract]\n" + string(raw) + "\n契约哈希：" + v.ContractHash + "。不得改验收规格或门槛；按绑定范围产出，报告实际检查和未知项。\n"
	if task != nil && task.Reduction {
		text += "网关评测状态：" + v.Status + "；" + v.Reason + "。执行结束不等于目标达成；待确认条件必须明确列出。\n"
	}
	return text
}

// SwarmReady 是额外门禁，原队列、资源与审批约束仍由现有调度器执行。
func SwarmReady(run *Run, team *Team, task *Task) bool {
	if run.Swarm == nil {
		return true
	}
	if run.Swarm.Contract != nil && run.Swarm.ContractHash != goalHash(run.Swarm.Contract) {
		return false
	}
	if run.Swarm.ScopeHash != "" && run.Swarm.ScopeHash != RunGoalScope(run).Hash && task.ExecutionContext != ExecutionContextLeadControl {
		return false
	}
	if !run.Swarm.Deadline.IsZero() && time.Now().After(run.Swarm.Deadline) {
		return task.ExecutionContext == ExecutionContextLeadControl
	}
	if task.ExecutionContext == ExecutionContextLeadControl || task.IssueClassificationOnly {
		return true
	}
	if run.Swarm.Contract == nil {
		return task.WriteLock == TaskWriteLockNone && !task.Reduction && !IsReviewTask(team, task)
	}
	if swarmWorker(team, task) && swarmBinding(run, task) == nil {
		return false
	}
	for _, id := range task.DependsOn {
		dep := findTaskIn(run, id)
		if swarmWorker(team, dep) {
			a := swarmTaskArtifact(run, id)
			if a == nil || !swarmArtifactPassed(run, a) {
				return false
			}
		}
	}
	if b := swarmBinding(run, task); b != nil {
		for _, id := range b.InputTaskIDs {
			a := swarmTaskArtifact(run, id)
			if a == nil || !swarmArtifactPassed(run, a) {
				return false
			}
		}
	}
	return true
}
func swarmArtifactPassed(run *Run, a *SwarmArtifact) bool {
	if a == nil || a.Validity != "fresh" || run.Swarm.Contract == nil {
		return false
	}
	ids := []string{}
	if t := findTaskIn(run, a.TaskID); t != nil {
		if b := swarmBinding(run, t); b != nil {
			ids = b.EvalSpecIDs
		}
	} else {
		for _, spec := range run.Swarm.Contract.EvalSpecs {
			ids = append(ids, spec.ID)
		}
	}
	for _, id := range ids {
		v := latestSwarmEval(run, a.ID, id)
		if v == nil || v.Status != "passed" || v.Validity != "fresh" {
			return false
		}
	}
	if run.Swarm.Contract.RequireIndependentReview {
		t := findTaskIn(run, a.ReviewTaskID)
		if t == nil || t.Status != TaskCompleted || t.ResultDetail == nil || t.ResultDetail.ReviewVerdict != "passed" || t.ReviewVerdictStale {
			return false
		}
		for _, author := range swarmArtifactAuthors(run, a) {
			if author.MemberID == t.MemberID || author.DispatchSessionID != "" && author.DispatchSessionID == t.DispatchSessionID {
				return false
			}
		}
	}
	return true
}

func swarmArtifactAuthors(run *Run, a *SwarmArtifact) []*Task {
	out := []*Task{}
	if a == nil {
		return out
	}
	if a.Kind == "integration" {
		for _, id := range a.Inputs {
			out = append(out, swarmArtifactAuthors(run, swarmArtifact(run, id))...)
		}
	} else if t := findTaskIn(run, a.TaskID); t != nil {
		out = append(out, t)
	}
	return out
}

func SwarmReviewIndependent(run *Run, artifactID string) bool {
	a := swarmArtifact(run, artifactID)
	if a == nil {
		return false
	}
	t := findTaskIn(run, a.ReviewTaskID)
	if t == nil {
		return false
	}
	for _, author := range swarmArtifactAuthors(run, a) {
		if author.MemberID == t.MemberID || author.DispatchSessionID != "" && author.DispatchSessionID == t.DispatchSessionID {
			return false
		}
	}
	return true
}

func swarmArtifactRef(task *Task, a *swarmeval.Artifact) SwarmArtifact {
	return SwarmArtifact{ID: a.ID, TaskID: task.ID, Attempt: task.Attempt, Kind: "task", WorkspaceID: task.WorkspaceID, ContentHash: a.ContentHash, BaseRevision: a.BaseRevision, ChangedPaths: a.ChangedPaths, Inputs: a.Inputs, Validity: "fresh", Isolation: a.Isolation}
}

func (s *Service) CancelSwarmEval(runID string) {
	s.mu.Lock()
	cancels := []context.CancelFunc{}
	for _, cancel := range s.swarmCancels[runID] {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func swarmEvidenceIdentity(run *Run) any {
	if run.Swarm == nil {
		return nil
	}
	return struct {
		ContractHash string
		Artifacts    []SwarmArtifact
		Evals        []SwarmEval
		Findings     []SwarmFinding
	}{run.Swarm.ContractHash, run.Swarm.Artifacts, run.Swarm.Evals, run.Swarm.Findings}
}

// 冻结的运行检查是既有 GoalContract 条件的执行规格；用户仍通过原目标确认接口判定。
func withSwarmCriteria(run *Run, scope GoalScope, contract GoalContract) GoalContract {
	if run.Swarm == nil || run.Swarm.Contract == nil {
		return contract
	}
	seen := map[string]bool{}
	for _, c := range contract.Criteria {
		seen[c.ID] = true
	}
	for _, c := range run.Swarm.Contract.Criteria {
		if c.ID == "original" || !c.Required || seen[c.ID] {
			continue
		}
		if len(c.IssueIDs) > 0 {
			matches := false
			for _, id := range c.IssueIDs {
				if id == scope.ID {
					matches = true
				}
			}
			if !matches {
				continue
			}
		}
		contract.Criteria = append(contract.Criteria, GoalCriterion{ID: c.ID, Text: c.Text, State: "unknown"})
		seen[c.ID] = true
	}
	return contract
}

func (s *Service) validateSwarmScopes(run *Run, contract SwarmContract) error {
	for _, criterion := range contract.Criteria {
		for _, id := range criterion.IssueIDs {
			if s.inputs == nil || s.inputs.GetIssue(run.TeamID, id) == nil {
				return swarmInvalid("完成条件引用未知事项")
			}
		}
	}
	return nil
}

func (s *Service) validateSwarmEvidence(run *Run) error {
	if !swarmCandidatePassed(run) {
		return swarmInvalid("必要候选检查尚未有效通过")
	}
	if len(run.Swarm.Artifacts) == 0 {
		return nil
	}
	if s.SwarmRunner == nil {
		return swarmInvalid("缺少候选执行器，无法核验检查")
	}
	for _, a := range run.Swarm.Artifacts {
		if a.Kind != "integration" {
			continue
		}
		loaded, err := s.SwarmRunner.LoadArtifact(context.Background(), a.ID)
		if err != nil || loaded.ContentHash != a.ContentHash {
			return swarmInvalid("候选内容已变化或缺失")
		}
		for _, spec := range run.Swarm.Contract.EvalSpecs {
			v := latestSwarmEval(run, a.ID, spec.ID)
			if v == nil {
				return swarmInvalid("缺少必要检查")
			}
			result, err := s.SwarmRunner.LoadResult(v.ID)
			if err != nil || result.ArtifactHash != a.ContentHash || result.LogHash != v.LogHash || result.Status != "passed" {
				return swarmInvalid("评测结果已变化或缺失")
			}
		}
	}
	return nil
}

func swarmCompletionHash(run *Run) string {
	return goalHash(struct {
		Evidence any
		Tasks    []Task
	}{swarmEvidenceIdentity(run), run.Tasks})
}
func (s *Service) SwarmBudget(run *Run) map[string]int {
	repairs, evals := 0, 0
	if run.Swarm == nil {
		return nil
	}
	for _, r := range s.store.RunsByTeam(run.TeamID) {
		if r.Swarm == nil || r.Swarm.ChainID != run.Swarm.ChainID {
			continue
		}
		evals += len(r.Swarm.Evals)
		for _, f := range r.Swarm.Findings {
			if f.RepairTaskID != "" {
				repairs++
			}
		}
	}
	maxRepairs, maxEvals := 2, 12
	if run.Swarm.Contract != nil {
		maxRepairs = run.Swarm.Contract.MaxRepairs
		maxEvals = run.Swarm.Contract.MaxEvals
	}
	return map[string]int{"repairsUsed": repairs, "evalsUsed": evals, "maxRepairs": maxRepairs, "maxEvals": maxEvals}
}
func (s *Service) swarmContext(runID string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	id := uuid.NewString()
	s.mu.Lock()
	if s.swarmCancels == nil {
		s.swarmCancels = map[string]map[string]context.CancelFunc{}
	}
	if s.swarmCancels[runID] == nil {
		s.swarmCancels[runID] = map[string]context.CancelFunc{}
	}
	s.swarmCancels[runID][id] = cancel
	s.mu.Unlock()
	return ctx, func() {
		cancel()
		s.mu.Lock()
		delete(s.swarmCancels[runID], id)
		if len(s.swarmCancels[runID]) == 0 {
			delete(s.swarmCancels, runID)
		}
		s.mu.Unlock()
	}
}
