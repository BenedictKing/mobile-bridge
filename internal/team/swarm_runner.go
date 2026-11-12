package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/swarmeval"
	"github.com/google/uuid"
)

// 一轮只执行一个候选/检查，慢 I/O 始终在 state.mu 外；实际副作用先登记意图。
func (s *Service) passSwarm(state *runState) bool {
	state.swarmMu.Lock()
	defer state.swarmMu.Unlock()
	state.mu.Lock()
	initial := s.store.Run(state.runID)
	if initial != nil && initial.Swarm != nil {
		if err := s.syncSwarmLocked(initial); err != nil {
			state.mu.Unlock()
			return true
		}
	}
	state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil || run.Swarm == nil || terminalRun(run.Status) || run.Status == RunPaused || run.Status == RunWaitingInput {
		return false
	}
	team := s.store.Team(run.TeamID)
	if team == nil {
		return false
	}
	if run.Swarm.Policy != nil {
		team = team.Clone()
		team.Policy = *run.Swarm.Policy
	}
	if !run.Swarm.Deadline.IsZero() && time.Now().After(run.Swarm.Deadline) {
		s.swarmReason(state, "budget_exhausted", "执行链墙钟期限已到，停止新评测与自动修复")
		return false
	}
	if run.Swarm.Contract == nil {
		if run.Swarm.Proposal == nil {
			for _, task := range run.Tasks {
				if task.Status != TaskCompleted || task.MemberID != team.LeadID {
					continue
				}
				text := task.Result
				if task.ResultDetail != nil && text == "" {
					text = task.ResultDetail.Result
				}
				start := strings.Index(text, "<team-swarm-contract>")
				end := strings.Index(text, "</team-swarm-contract>")
				if start < 0 || end < start {
					continue
				}
				raw := text[start+len("<team-swarm-contract>") : end]
				var c SwarmContract
				if len(raw) > 32768 || json.Unmarshal([]byte(raw), &c) != nil {
					continue
				}
				if err := validateSwarmContract(run, &c); err != nil {
					s.swarmReason(state, "awaiting_contract", err.Error())
					return false
				}
				c.Revision = 1
				state.mu.Lock()
				fresh := s.store.Run(run.ID)
				if fresh.Swarm.Contract == nil && fresh.Swarm.Proposal == nil {
					fresh.Swarm.Proposal = &SwarmProposal{ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(run.ID+task.ID+goalHash(c))).String(), Contract: c, Source: "lead"}
					fresh.Swarm.Status = "awaiting_confirmation"
					fresh.Swarm.Reason = "请核对负责人提出的完成条件与检查规格"
					_ = s.saveSwarmLocked(fresh, "lead")
				}
				state.mu.Unlock()
				break
			}
		}
		return false
	}
	if run.Swarm.ContractHash != goalHash(run.Swarm.Contract) {
		s.swarmReason(state, "blocked", "冻结契约身份不匹配")
		return false
	}
	if run.Swarm.ScopeHash != "" && run.Swarm.ScopeHash != RunGoalScope(run).Hash {
		s.swarmReason(state, "blocked", "原始要求已追加，请通过修订更新契约；旧证据只适用于旧目标")
		return false
	}
	if s.SwarmRunner == nil {
		s.swarmReason(state, "blocked", "候选评测执行器未装配")
		return false
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if (task.Status == TaskPlanned || task.Status == TaskReady) && swarmWorker(team, task) && swarmBinding(run, task) == nil {
			s.swarmReason(state, "blocked", "实现任务缺少冻结契约绑定："+task.ID+"；请创建修订补齐任务范围")
		}
	}
	for key, ids := range run.Swarm.Operations {
		if strings.HasPrefix(key, "integrate_") && ids != "" {
			id := strings.TrimPrefix(key, "integrate_")
			state.mu.Lock()
			fresh := s.store.Run(run.ID)
			delete(fresh.Swarm.Operations, key)
			fresh.Swarm.Operations[id] = "integrating"
			err := s.saveSwarmLocked(fresh, "runner")
			state.mu.Unlock()
			if err == nil {
				s.integrateSwarm(state, strings.Split(ids, ","), id)
			}
			return true
		}
	}
	for _, value := range run.Swarm.Evals {
		if value.Status == "pending" || value.Status == "running" {
			s.executeSwarmEval(state, value)
			return true
		}
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if !swarmWorker(team, task) || task.Status != TaskCompleted || swarmDirectArtifact(run, task.ID) != nil {
			continue
		}
		if s.captureSwarmTask(state, run, task) {
			return true
		}
	}
	for i := range run.Swarm.Artifacts {
		a := &run.Swarm.Artifacts[i]
		if a.Validity != "fresh" {
			continue
		}
		ids := []string{}
		if task := findTaskIn(run, a.TaskID); task != nil {
			if b := swarmBinding(run, task); b != nil {
				ids = b.EvalSpecIDs
			}
		} else if a.Kind == "integration" {
			for _, sp := range run.Swarm.Contract.EvalSpecs {
				ids = append(ids, sp.ID)
			}
		}
		for _, id := range ids {
			if latestSwarmEval(run, a.ID, id) == nil {
				if s.queueSwarmEval(state, run, a, id) {
					return true
				}
			}
		}
		if swarmChecksPassed(run, a) && run.Swarm.Contract.RequireIndependentReview && a.ReviewTaskID == "" {
			if s.planSwarmReview(state, run, a) {
				return true
			}
		}
		if a.ReviewTaskID != "" {
			if review := findTaskIn(run, a.ReviewTaskID); review != nil && terminalTask(review.Status) {
				verdict := "unknown"
				if review.ResultDetail != nil {
					if value := review.ResultDetail.ReviewVerdict; value != "" {
						verdict = value
					}
				}
				if review.Status == TaskCompleted && verdict == "unknown" {
					if s.planSwarmReviewFormat(state, run, a, review) {
						return true
					}
					s.swarmReason(state, "blocked", "独立审查未取得有效裁决")
					continue
				}
				if review.Status != TaskCompleted {
					s.swarmReason(state, "blocked", "独立审查执行未成功："+review.Error)
					continue
				}
				if verdict == "failed" {
					before := len(run.Swarm.Findings)
					s.swarmFinding(state, run, a, "review", review.Result)
					if len(s.store.Run(run.ID).Swarm.Findings) > before {
						return true
					}
				}
			}
		}
	}
	if s.planSwarmRepair(state, run) {
		return true
	}
	if s.autoIntegrateSwarm(state, run) {
		return true
	}
	if swarmCandidatePassed(run) {
		if err := s.validateSwarmEvidence(run); err != nil {
			state.mu.Lock()
			fresh := s.store.Run(run.ID)
			for i := range fresh.Swarm.Artifacts {
				if fresh.Swarm.Artifacts[i].Kind == "integration" {
					fresh.Swarm.Artifacts[i].Validity = "unknown"
				}
			}
			fresh.Swarm.Status = "blocked"
			fresh.Swarm.Reason = err.Error()
			_ = s.saveSwarmLocked(fresh, "runner")
			state.mu.Unlock()
			return true
		}
		state.mu.Lock()
		fresh := s.store.Run(run.ID)
		if fresh != nil && swarmCompletionHash(fresh) == swarmCompletionHash(run) && fresh.Swarm.VerifiedEvidenceHash != swarmCompletionHash(fresh) {
			fresh.Swarm.VerifiedEvidenceHash = swarmCompletionHash(fresh)
			_ = s.saveSwarmLocked(fresh, "runner")
		}
		state.mu.Unlock()
		s.swarmReason(state, "pending_confirmation", "候选检查已结算；本地快照不是操作系统沙箱，目标仍需用户确认")
	}
	return false
}
func (s *Service) swarmReason(state *runState, status, reason string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil || run.Swarm == nil || terminalRun(run.Status) {
		return
	}
	if run.Swarm.Status == status && run.Swarm.Reason == reason {
		return
	}
	run.Swarm.Status = status
	run.Swarm.Reason = truncateRunes(reason, 1000)
	_ = s.saveSwarmLocked(run, "runner")
}
func (s *Service) captureSwarmTask(state *runState, run *Run, task *Task) bool {
	b := swarmBinding(run, task)
	if b == nil {
		s.swarmReason(state, "blocked", "写入任务缺少契约绑定："+task.ID)
		return false
	}
	cwd, err := s.swarmTaskCwd(run, task)
	if err != nil {
		s.swarmReason(state, "blocked", err.Error())
		return false
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("swarm-artifact:"+run.ID+":"+task.ID+fmt.Sprint(task.Attempt))).String()
	state.mu.Lock()
	fresh := s.store.Run(run.ID)
	if swarmDirectArtifact(fresh, task.ID) != nil || fresh.Swarm.Operations[id] == "captured" {
		state.mu.Unlock()
		return false
	}
	if fresh.Swarm.Operations[id] == "capture_failed" {
		state.mu.Unlock()
		return false
	}
	if fresh.Swarm.Operations[id] == "capturing" {
		state.mu.Unlock()
		s.swarmReason(state, "blocked", "候选创建结果待核查："+id)
		return false
	}
	fresh.Swarm.Operations[id] = "capturing"
	err = s.saveSwarmLocked(fresh, "runner")
	state.mu.Unlock()
	if err != nil {
		return true
	}
	ctx, cancel := s.swarmContext(run.ID)
	defer cancel()
	a, err := s.SwarmRunner.Capture(ctx, id+"_source", cwd, task.BaseRevision)
	if err == nil {
		a, err = s.SwarmRunner.Detach(ctx, id, a)
	}
	var changes []string
	var changeErr, checkErr error
	if err == nil {
		changes, changeErr = s.swarmActualChanges(ctx, run, task, a)
		checkErr = s.swarmProtectedChecks(ctx, run, task, a)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh = s.store.Run(run.ID)
	if fresh == nil || fresh.Swarm == nil {
		return true
	}
	if err != nil {
		fresh.Swarm.Operations[id] = "capture_failed"
		fresh.Swarm.Status = "blocked"
		fresh.Swarm.Reason = truncateRunes(err.Error(), 1000)
		_ = s.saveSwarmLocked(fresh, "runner")
		return true
	}
	ref := swarmArtifactRef(task, a)
	if changeErr == nil {
		ref.ChangedPaths = changes
	} else {
		ref.Validity = "unknown"
		fresh.Swarm.Status = "blocked"
		fresh.Swarm.Reason = changeErr.Error()
	}
	if checkErr != nil {
		ref.Validity = "unknown"
		fresh.Swarm.Status = "blocked"
		fresh.Swarm.Reason = checkErr.Error()
	}
	for _, p := range ref.ChangedPaths {
		if !pathAllowed(p, b.AllowedWritePaths) || pathAllowed(p, b.ProtectedPaths) {
			ref.Validity = "unknown"
			fresh.Swarm.Status = "blocked"
			fresh.Swarm.Reason = "产物写入越过冻结边界：" + p
			break
		}
	}
	if terminalRun(fresh.Status) || fresh.Status == RunPaused {
		ref.Validity = "unknown"
	}
	fresh.Swarm.Artifacts = append(fresh.Swarm.Artifacts, ref)
	fresh.Swarm.Operations[id] = "captured"
	_ = s.saveSwarmLocked(fresh, "runner")
	return true
}
func (s *Service) swarmTaskCwd(run *Run, task *Task) (string, error) {
	cwd := ""
	if task.WorkspaceID != "" {
		if s.Workspaces == nil {
			return "", ErrWorkspacesUnavailable
		}
		var err error
		cwd, err = s.Workspaces.Resolve(task.WorkspaceID)
		if err != nil {
			return "", err
		}
	} else {
		member := memberOf(s.store.Team(run.TeamID), task.MemberID)
		if member == nil {
			return "", fmt.Errorf("成员不存在")
		}
		cwd = s.resolver.Cwd(taskProviderSessionID(run, task, member))
	}
	if cwd == "" {
		return "", fmt.Errorf("无法确定候选工作区")
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	return real, nil
}
func swarmChecksPassed(run *Run, a *SwarmArtifact) bool {
	if a.Validity != "fresh" {
		return false
	}
	ids := []string{}
	if task := findTaskIn(run, a.TaskID); task != nil {
		b := swarmBinding(run, task)
		if b == nil {
			return false
		}
		ids = b.EvalSpecIDs
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
	return true
}
func (s *Service) queueSwarmEval(state *runState, run *Run, a *SwarmArtifact, specID string) bool {
	s.swarmBudgetMu.Lock()
	defer s.swarmBudgetMu.Unlock()
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	if fresh == nil || terminalRun(fresh.Status) {
		return false
	}
	if s.swarmEvalCount(fresh) >= fresh.Swarm.Contract.MaxEvals {
		if fresh.Swarm.Status != "budget_exhausted" {
			fresh.Swarm.Status = "budget_exhausted"
			fresh.Swarm.Reason = "执行链评测预算已用尽"
			_ = s.saveSwarmLocked(fresh, "runner")
		}
		return false
	}
	if latestSwarmEval(fresh, a.ID, specID) != nil {
		return false
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(a.ID+":"+specID+":1")).String()
	fresh.Swarm.Evals = append(fresh.Swarm.Evals, SwarmEval{ID: id, TaskID: a.TaskID, ArtifactID: a.ID, EvalSpecID: specID, Attempt: 1, Status: "pending", Validity: "unknown", ExitCode: -1})
	fresh.Swarm.Status = "evaluating"
	_ = s.saveSwarmLocked(fresh, "runner")
	return true
}
func (s *Service) executeSwarmEval(state *runState, value SwarmEval) {
	s.mu.Lock()
	if s.swarmEvalBusy == nil {
		s.swarmEvalBusy = map[string]bool{}
	}
	if s.swarmEvalBusy[state.runID] {
		s.mu.Unlock()
		return
	}
	s.swarmEvalBusy[state.runID] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.swarmEvalBusy, state.runID); s.mu.Unlock() }()
	ctx, cancel := s.swarmContext(state.runID)
	defer cancel()
	state.mu.Lock()
	run := s.store.Run(state.runID)
	if run == nil || run.Status == RunCancelled || run.Status == RunOutcomeUnknown || run.Status == RunPaused {
		state.mu.Unlock()
		return
	}
	for _, v := range run.Swarm.Evals {
		if v.ID == value.ID && v.Status != "pending" && v.Status != "running" {
			state.mu.Unlock()
			return
		}
	}
	spec := swarmEvalSpec(run, value.EvalSpecID)
	if spec == nil {
		state.mu.Unlock()
		return
	}
	for i := range run.Swarm.Evals {
		if run.Swarm.Evals[i].ID == value.ID {
			run.Swarm.Evals[i].Status = "running"
			run.Swarm.Evals[i].StartedAt = time.Now()
		}
	}
	err := s.saveSwarmLocked(run, "runner")
	state.mu.Unlock()
	if err != nil {
		return
	}
	a, err := s.SwarmRunner.LoadArtifact(ctx, value.ArtifactID)
	var result *swarmeval.Result
	if err == nil {
		result, err = s.SwarmRunner.Evaluate(ctx, value.ID, a, swarmeval.Spec{Command: spec.Command, Timeout: time.Duration(spec.TimeoutSeconds) * time.Second, PrepareDependencies: true})
	}
	state.mu.Lock()
	fresh := s.store.Run(run.ID)
	if fresh == nil {
		state.mu.Unlock()
		return
	}
	for i := range fresh.Swarm.Evals {
		v := &fresh.Swarm.Evals[i]
		if v.ID != value.ID {
			continue
		}
		v.FinishedAt = time.Now()
		if err != nil {
			v.Status = "inconclusive"
			if errors.Is(err, swarmeval.ErrConflict) {
				v.Status = "outcome_unknown"
			}
			v.Validity = "unknown"
			v.Reason = truncateRunes(err.Error(), 1000)
		} else {
			v.Status = result.Status
			v.Validity = "fresh"
			v.ExitCode = result.ExitCode
			v.OutputHead = truncateRunes(result.OutputHead, 2048)
			v.Truncated = result.Truncated || len(v.OutputHead) < len(result.OutputHead)
			v.Reason = result.Reason
			v.LogHash = result.LogHash
			v.EnvironmentHash = result.EnvironmentHash
			v.StartedAt = result.StartedAt
			v.FinishedAt = result.FinishedAt
		}
		if fresh.Status == RunCancelled || fresh.Status == RunOutcomeUnknown || fresh.Status == RunPaused {
			v.Validity = "unknown"
		}
		break
	}
	_ = s.saveSwarmLocked(fresh, "runner")
	state.mu.Unlock()
	current := s.store.Run(run.ID)
	if current != nil && current.Status == RunCompleted && swarmCandidatePassed(current) {
		if err := s.validateSwarmEvidence(current); err == nil {
			state.mu.Lock()
			fresh := s.store.Run(run.ID)
			if swarmCompletionHash(fresh) == swarmCompletionHash(current) {
				fresh.Swarm.VerifiedEvidenceHash = swarmCompletionHash(fresh)
				_ = s.saveSwarmLocked(fresh, "runner")
			}
			state.mu.Unlock()
		}
	}
	v := latestSwarmEval(current, value.ArtifactID, value.EvalSpecID)
	if v != nil && v.Status == "failed" {
		s.swarmFinding(state, current, swarmArtifact(current, value.ArtifactID), value.EvalSpecID, v.OutputHead)
	} else if v != nil && v.Status != "passed" {
		s.swarmReason(state, "blocked", "评测未能取得有效结论："+v.Reason)
	}
}
func (s *Service) swarmFinding(state *runState, run *Run, a *SwarmArtifact, specID, message string) {
	if a == nil {
		return
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(a.ID+specID)).String()
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	for _, f := range fresh.Swarm.Findings {
		if f.ID == id {
			return
		}
	}
	criterion := "original"
	if spec := swarmEvalSpec(fresh, specID); spec != nil && len(spec.CriterionIDs) > 0 {
		criterion = spec.CriterionIDs[0]
	}
	signature := goalHash([]string{criterion, specID, strings.TrimSpace(truncateRunes(message, 500))})
	fresh.Swarm.Findings = append(fresh.Swarm.Findings, SwarmFinding{ID: id, CriterionID: criterion, TaskID: a.TaskID, ArtifactID: a.ID, Severity: "blocking", Status: "open", Reason: truncateRunes(message, 2000), Signature: signature})
	fresh.Swarm.Status = "repair_needed"
	fresh.Swarm.Reason = "必要检查发现问题"
	_ = s.saveSwarmLocked(fresh, "runner")
}
func (s *Service) planSwarmReview(state *runState, run *Run, a *SwarmArtifact) bool {
	team := s.store.Team(run.TeamID)
	reviewer := memberOf(team, team.ReviewerMemberID())
	independent := reviewer != nil && reviewer.Enabled
	if independent {
		for _, source := range swarmArtifactAuthors(run, a) {
			if source.MemberID == reviewer.ID || taskProviderSessionID(run, source, memberOf(team, source.MemberID)) == reviewer.SessionID {
				independent = false
			}
		}
	}
	if !independent {
		s.swarmReason(state, "blocked", "必要审查缺少未参与实现的独立成员")
		return false
	}
	if s.ExecutionSessions == nil {
		s.swarmReason(state, "blocked", "独立审查需要专用执行会话能力")
		return false
	}
	if len(run.Tasks) >= team.Policy.MaxTasksPerRun && team.Policy.MaxTasksPerRun > 0 {
		s.swarmReason(state, "blocked", "任务容量不足以安排必要审查，请创建修订")
		return false
	}
	artifact, err := s.SwarmRunner.LoadArtifact(context.Background(), a.ID)
	if err != nil {
		s.swarmReason(state, "blocked", err.Error())
		return false
	}
	id := "swrev-" + a.ID
	task := Task{ID: id, MemberID: reviewer.ID, Prompt: "独立审查冻结候选目录 " + artifact.Directory + "。只读检查原始目标与契约条件，报告逐项结论、证据及可复现缺陷，不要修改文件。不要使用实现者的自评分。最后一行必须为 <team-review>passed</team-review> 或 <team-review>failed</team-review>。候选内容身份：" + a.ContentHash, Phase: "review", WriteLock: TaskWriteLockNone, Review: reviewFlag(true), Status: TaskPlanned, Attempt: 1, Ordinal: len(run.Tasks), ExecutionThreadID: "th-" + id}
	task.InstructionsHead = InstructionsHead(task.Prompt)
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	ref := swarmArtifact(fresh, a.ID)
	if ref == nil || ref.ReviewTaskID != "" || terminalRun(fresh.Status) {
		return false
	}
	ref.ReviewTaskID = id
	fresh.Tasks = append(fresh.Tasks, task)
	event := newEvent(run.ID, EventTaskPlanned, "runner", map[string]any{"taskId": id, "memberId": task.MemberID, "prompt": task.Prompt, "instructionsHead": task.InstructionsHead, "phase": task.Phase, "writeLock": task.WriteLock, "review": true, "swarmTask": goValue(task)})
	event.Payload["swarmState"] = goValue(fresh.Swarm)
	if err := s.journal.Append(event); err != nil {
		return true
	}
	s.emit(event)
	_ = s.saveSwarmLocked(fresh, "runner")
	return true
}
func swarmReviewArtifact(run *Run, taskID string) *SwarmArtifact {
	if run.Swarm != nil {
		for i := range run.Swarm.Artifacts {
			if run.Swarm.Artifacts[i].ReviewTaskID == taskID {
				return &run.Swarm.Artifacts[i]
			}
		}
	}
	return nil
}

func (s *Service) planSwarmReviewFormat(state *runState, run *Run, a *SwarmArtifact, review *Task) bool {
	if strings.HasPrefix(review.ID, "swfmt-") {
		return false
	}
	team := s.store.Team(run.TeamID)
	if team.Policy.MaxTasksPerRun > 0 && len(run.Tasks) >= team.Policy.MaxTasksPerRun {
		return false
	}
	task := *review.Clone()
	task.ID = "swfmt-" + a.ID
	task.Prompt = "你上一轮审查未给出可识别裁决。请保留真实审查结论，补充逐项依据，最后一行只能写 <team-review>passed</team-review> 或 <team-review>failed</team-review>。只读候选，不修改代码。\n上一轮输出：" + truncateRunes(review.Result, 4000)
	task.Status = TaskPlanned
	task.Result = ""
	task.ResultDetail = nil
	task.SubmissionID = ""
	task.DispatchSessionID = ""
	task.SentText = ""
	task.ExecutionThreadID = "th-" + task.ID
	task.Ordinal = len(run.Tasks)
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	task.InstructionsHead = InstructionsHead(task.Prompt)
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	ref := swarmArtifact(fresh, a.ID)
	if ref == nil || ref.ReviewTaskID != review.ID || terminalRun(fresh.Status) {
		return false
	}
	ref.ReviewTaskID = task.ID
	fresh.Tasks = append(fresh.Tasks, task)
	event := newEvent(run.ID, EventTaskPlanned, "runner", map[string]any{"taskId": task.ID, "memberId": task.MemberID, "swarmTask": goValue(task)})
	event.Payload["swarmState"] = goValue(fresh.Swarm)
	if err := s.journal.Append(event); err == nil {
		s.emit(event)
		_ = s.saveSwarmLocked(fresh, "runner")
	}
	return true
}

func (s *Service) planSwarmRepair(state *runState, run *Run) bool {
	s.swarmBudgetMu.Lock()
	defer s.swarmBudgetMu.Unlock()
	for _, finding := range run.Swarm.Findings {
		if finding.Status != "open" || finding.RepairTaskID != "" || finding.TaskID == "integration" {
			continue
		}
		used, same := 0, 0
		for _, r := range s.store.RunsByTeam(run.TeamID) {
			if r.Swarm == nil || r.Swarm.ChainID != run.Swarm.ChainID {
				continue
			}
			for _, f := range r.Swarm.Findings {
				if f.RepairTaskID != "" && f.CriterionID == finding.CriterionID {
					used++
				}
				if f.Signature == finding.Signature {
					same++
				}
			}
		}
		if used >= run.Swarm.Contract.MaxRepairs || same >= 2 {
			s.swarmReason(state, "blocked", "同一条件修复上限或重复失败停靠："+finding.CriterionID)
			continue
		}
		source := findTaskIn(run, finding.TaskID)
		if source == nil {
			continue
		}
		team := s.store.Team(run.TeamID)
		if team.Policy.MaxTasksPerRun > 0 && len(run.Tasks) >= team.Policy.MaxTasksPerRun {
			s.swarmReason(state, "blocked", "修复所需任务容量不足")
			continue
		}
		task := *source.Clone()
		task.ID = "swfix-" + finding.ID
		task.Status = TaskPlanned
		task.Prompt = "修复独立检查发现的问题，不得降低验收规格。\n条件：" + finding.CriterionID + "\n证据：" + finding.Reason + "\n原任务：" + source.Prompt
		task.InstructionsHead = InstructionsHead(task.Prompt)
		task.Attempt = 1
		task.Ordinal = len(run.Tasks)
		task.Result = ""
		task.ResultDetail = nil
		task.SubmissionID = ""
		task.SentText = ""
		task.Error = ""
		task.StopReason = ""
		task.CachedFrom = ""
		task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
		state.mu.Lock()
		fresh := s.store.Run(run.ID)
		if terminalRun(fresh.Status) {
			state.mu.Unlock()
			return false
		}
		if findTaskIn(fresh, task.ID) != nil {
			state.mu.Unlock()
			return false
		}
		eligible := false
		for _, current := range fresh.Swarm.Findings {
			if current.ID == finding.ID && current.Status == "open" && current.RepairTaskID == "" {
				eligible = true
			}
		}
		if !eligible {
			state.mu.Unlock()
			return false
		}
		for i := range fresh.Swarm.Findings {
			if fresh.Swarm.Findings[i].ID == finding.ID {
				fresh.Swarm.Findings[i].RepairTaskID = task.ID
				fresh.Swarm.Findings[i].Status = "repairing"
			}
		}
		fresh.Tasks = append(fresh.Tasks, task)
		binding := swarmBinding(run, source)
		if binding != nil {
			b := *binding
			b.TaskID = task.ID
			fresh.Swarm.RepairBindings = append(fresh.Swarm.RepairBindings, b)
		}
		event := newEvent(run.ID, EventTaskPlanned, "runner", map[string]any{"taskId": task.ID, "memberId": task.MemberID, "swarmTask": goValue(task), "prompt": task.Prompt, "instructionsHead": task.InstructionsHead})
		event.Payload["swarmState"] = goValue(fresh.Swarm)
		if err := s.journal.Append(event); err == nil {
			s.emit(event)
			_ = s.saveSwarmLocked(fresh, "runner")
		}
		state.mu.Unlock()
		return true
	}
	return false
}

func (s *Service) autoIntegrateSwarm(state *runState, run *Run) bool {
	inputs := swarmFinalArtifacts(run, s.store.Team(run.TeamID))
	if len(inputs) == 0 {
		return false
	}
	for _, a := range run.Swarm.Artifacts {
		if a.Kind == "integration" {
			return false
		}
	}
	for _, task := range run.Tasks {
		if swarmWorker(s.store.Team(run.TeamID), &task) && !terminalTask(task.Status) {
			return false
		}
	}
	ids := []string{}
	for _, a := range inputs {
		if !swarmArtifactPassed(run, a) {
			return false
		}
		ids = append(ids, a.ID)
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(run.ID+":integration:"+strings.Join(ids, ","))).String()
	state.mu.Lock()
	fresh := s.store.Run(run.ID)
	if fresh.Swarm.Operations[id] != "" {
		state.mu.Unlock()
		return false
	}
	fresh.Swarm.Operations[id] = "integrating"
	err := s.saveSwarmLocked(fresh, "runner")
	state.mu.Unlock()
	if err != nil {
		return true
	}
	s.integrateSwarm(state, ids, id)
	return true
}
func swarmFinalArtifacts(run *Run, team *Team) []*SwarmArtifact {
	replaced := map[string]bool{}
	for _, f := range run.Swarm.Findings {
		if f.RepairTaskID != "" {
			replaced[f.TaskID] = true
		}
	}
	out := []*SwarmArtifact{}
	for i := range run.Tasks {
		t := &run.Tasks[i]
		if !swarmWorker(team, t) || replaced[t.ID] {
			continue
		}
		a := swarmTaskArtifact(run, t.ID)
		if a == nil {
			return nil
		}
		out = append(out, a)
	}
	return out
}
func (s *Service) integrateSwarm(state *runState, ids []string, id string) {
	ctx, cancel := s.swarmContext(state.runID)
	defer cancel()
	inputs := []*swarmeval.Artifact{}
	var result *swarmeval.Artifact
	var err error
	for _, aid := range ids {
		var a *swarmeval.Artifact
		a, err = s.SwarmRunner.LoadArtifact(ctx, aid)
		if err != nil {
			break
		}
		inputs = append(inputs, a)
	}
	if err == nil {
		result, err = s.SwarmRunner.Integrate(ctx, id, inputs)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil {
		return
	}
	if err != nil {
		run.Swarm.Status = "blocked"
		run.Swarm.Reason = "集成未通过：" + truncateRunes(err.Error(), 1000)
		run.Swarm.Operations[id] = "integration_failed"
	} else {
		a := swarmArtifactRef(&Task{ID: "integration"}, result)
		a.Kind = "integration"
		a.Inputs = ids
		run.Swarm.Artifacts = append(run.Swarm.Artifacts, a)
		run.Swarm.Operations[id] = "integrated"
		run.Swarm.Status = "evaluating"
	}
	_ = s.saveSwarmLocked(run, "runner")
}
func (s *Service) RequestSwarmIntegration(runID string, request SwarmRequest) (*Run, error) {
	return s.swarmCommand(runID, "integrate", "", request, func(run *Run) error {
		if terminalRun(run.Status) {
			return swarmInvalid("执行已结束，请创建修订后集成新候选")
		}
		if run.Swarm.Contract == nil || s.SwarmRunner == nil {
			return swarmInvalid("契约或评测执行器未就绪")
		}
		if len(request.ArtifactIDs) < 1 || len(request.ArtifactIDs) > 16 {
			return swarmInvalid("集成需要 1–16 个候选")
		}
		required := swarmFinalArtifacts(run, s.store.Team(run.TeamID))
		if len(required) != len(request.ArtifactIDs) {
			return swarmInvalid("必须集成全部当前有效的最终任务候选，不能遗漏或重复输入")
		}
		selected := map[string]bool{}
		for _, id := range request.ArtifactIDs {
			if selected[id] {
				return swarmInvalid("集成候选重复")
			}
			selected[id] = true
		}
		for _, a := range required {
			if !selected[a.ID] {
				return swarmInvalid("集成遗漏最终任务候选")
			}
		}
		for _, id := range request.ArtifactIDs {
			a := swarmArtifact(run, id)
			if a == nil || !swarmArtifactPassed(run, a) {
				return swarmInvalid("候选尚未通过必要检查")
			}
		}
		// 接口只登记意图，调度循环负责副作用；幂等请求不会创建第二份候选。
		run.Swarm.Operations["integrate_"+request.ID] = strings.Join(request.ArtifactIDs, ",")
		return nil
	})
}
func swarmCandidatePassed(run *Run) bool {
	if run.Swarm == nil {
		return true
	}
	if run.Swarm.Contract == nil {
		return false
	}
	if run.Swarm.ContractHash != goalHash(run.Swarm.Contract) {
		return false
	}
	if run.Swarm.ScopeHash != "" && run.Swarm.ScopeHash != RunGoalScope(run).Hash {
		return false
	}
	for i := len(run.Swarm.Artifacts) - 1; i >= 0; i-- {
		a := &run.Swarm.Artifacts[i]
		if a.Kind == "integration" {
			required := map[string]bool{}
			for _, task := range run.Tasks {
				if task.WriteLock == TaskWriteLockNone || task.Reduction || task.IssueClassificationOnly || task.ExecutionContext == ExecutionContextLeadControl || swarmReviewArtifact(run, task.ID) != nil {
					continue
				}
				leaf := swarmTaskArtifact(run, task.ID)
				if leaf == nil {
					return false
				}
				required[leaf.ID] = true
			}
			if len(required) != len(a.Inputs) {
				return false
			}
			for _, id := range a.Inputs {
				if !required[id] {
					return false
				}
			}
			return swarmArtifactPassed(run, a)
		}
	}
	// 纯咨询可以没有代码候选，保留原用户确认而不强加构建。
	if len(run.Swarm.Contract.EvalSpecs) > 0 || len(run.Swarm.Artifacts) > 0 {
		return false
	}
	if run.Swarm.Contract.RequireIndependentReview {
		return false
	}
	for _, task := range run.Tasks {
		if task.ExecutionContext != ExecutionContextLeadControl && !task.IssueClassificationOnly && !task.Reduction && task.WriteLock != TaskWriteLockNone {
			return false
		}
	}
	return true
}

func swarmRunPassed(run *Run) bool {
	if !swarmCandidatePassed(run) {
		return false
	}
	if run.Swarm == nil || len(run.Swarm.Artifacts) == 0 {
		return true
	}
	return run.Swarm.VerifiedEvidenceHash == swarmCompletionHash(run)
}
