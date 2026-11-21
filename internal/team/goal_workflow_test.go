package team

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type workflowProvisioner struct {
	mu    sync.Mutex
	inner fakeExecutionSessionProvisioner
}

func (p *workflowProvisioner) EnsureExecutionSession(teamID, runID string, member *TeamMember, cwd string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inner.EnsureExecutionSession(teamID, runID, member, cwd)
}

func newGoalWorkflowRun(t *testing.T) (*dispatchHarness, *InputLedger, *Run) {
	t.Helper()
	h := newDispatchHarness(t)
	h.team.Policy.AutoDispatch = true
	h.team.Policy.MaxTasksPerRun = 20
	for _, member := range h.team.Members {
		h.resolver.setSession(member.SessionID, newFakeSession(member.SessionID, "/tmp/"+member.ID))
	}
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	h.service.ExecutionSessions = &workflowProvisioner{inner: fakeExecutionSessionProvisioner{resolver: h.resolver, sessions: map[string]string{}}}
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "inputs.json"), h.journal)
	h.service.SetInputs(ledger)
	const id, goal = "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "完整实现两条独立交付，并验证全部目标"
	if _, _, err := ledger.AcceptWithOptions(h.team.ID, id, goal, nil, "policy", 64, InputAcceptOptions{PlanningVersion: 1, Intent: InputIntentTask}); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreateConversationRun(h.team, id, goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, ledger, run
}

func workflowReply(plan *planningDefinition, feedback *planningFeedback, actions ...Action) string {
	payload := map[string]any{"actions": actions}
	if len(actions) == 0 {
		payload["actions"] = []Action{}
	}
	if plan != nil {
		payload["plan"] = plan
	}
	if feedback != nil {
		payload["feedback"] = feedback
	}
	raw, _ := json.Marshal(payload)
	return "<team-actions>" + string(raw) + "</team-actions>"
}

func workflowAction(source, workflow string) Action {
	return Action{Type: ActionTypeDelegate, SourceTaskID: source, Round: 1, Member: "dev", Task: "完成并验证" + workflow + "交付", WorkflowID: workflow, WriteLock: TaskWriteLockNone, Review: reviewFlag(false)}
}

func goalTestPlan() *planningDefinition {
	return &planningDefinition{Criteria: []PlanningCriterion{{ID: "all", Text: "两条独立交付全部实现并验证"}}, Workflows: []PlanningWorkflow{{ID: "a", Title: "交付 A"}, {ID: "b", Title: "交付 B"}}}
}

func startGoalBranches(t *testing.T, h *dispatchHarness, run *Run, plan *planningDefinition, actions ...Action) {
	t.Helper()
	root := h.waitForNthSubmission(t, "s-lead", 1, "主控规划")
	issue := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"create","newIssueTitle":"完整目标","source":"full"}]}</team-issues>`, run.ID)
	h.completeSubmission("s-lead", root.submissionID, root.text, issue+workflowReply(plan, nil, actions...))
}

func waitGoalTaskEntry(t *testing.T, h *dispatchHarness, runID, taskID string) (Task, fakeQueueEntry) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run := h.store.Run(runID)
		if task := findTaskIn(run, taskID); task != nil && task.SubmissionID != "" {
			for _, entry := range h.queue.entriesFor(task.DispatchSessionID) {
				if entry.submissionID == task.SubmissionID {
					return *task, entry
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	current := h.store.Run(runID)
	t.Fatalf("任务 %s 未入队，run=%s planning=%+v actions=%+v", taskID, current.Status, current.Planning, current.PendingActions)
	return Task{}, fakeQueueEntry{}
}

func completeGoalEntry(h *dispatchHarness, task Task, entry fakeQueueEntry, text string) {
	h.completeSubmission(task.DispatchSessionID, entry.submissionID, entry.text, text)
}

func waitGoalStatus(t *testing.T, h *dispatchHarness, runID, status string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if run := h.store.Run(runID); run.Status == status {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("期望 %s，实际 %+v", status, h.store.Run(runID))
	return nil
}

func TestGoalWorkflowParallelThenContinueUntilAccepted(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"), workflowAction("lead", "b"))
	a, entryA := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	b, entryB := waitGoalTaskEntry(t, h, run.ID, "lead-d1-1")
	if a.DispatchSessionID == b.DispatchSessionID || a.ExecutionThreadID == b.ExecutionThreadID {
		t.Fatal("不同 workflow 必须使用独立会话与线程")
	}
	if terminalTask(h.taskOf(run.ID, a.ID).Status) || terminalTask(h.taskOf(run.ID, b.ID).Status) {
		t.Fatal("两个分支应在任何一个完成前同时在途")
	}
	if strings.Contains(entryA.text, "Goal workflow planning") || strings.Contains(entryB.text, "Goal workflow planning") {
		t.Fatal("执行成员不能收到主控规划协议")
	}
	completeGoalEntry(h, a, entryA, "A 已实现，验证通过")
	completeGoalEntry(h, b, entryB, "")
	check, entryCheck := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	if !strings.Contains(entryCheck.text, "resultPresent=false") || !strings.Contains(entryCheck.text, run.Goal) {
		t.Fatal("评估必须包含完整目标与空结果事实")
	}
	feedback := &planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet", EvidenceTaskIDs: []string{a.ID}, Reason: "B 没有交付证据"}}, Remaining: []string{"补齐并验证 B"}}
	completeGoalEntry(h, check, entryCheck, workflowReply(nil, feedback, workflowAction(check.ID, "b")))
	repair, entryRepair := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1-d1-0")
	if repair.DispatchSessionID != b.DispatchSessionID {
		t.Fatal("同 workflow 续做应复用原执行会话")
	}
	if h.store.Run(run.ID).Status == RunCompleted {
		t.Fatal("未达标不能按 completed 任务数收场")
	}
	completeGoalEntry(h, repair, entryRepair, "B 已补齐，验证通过")
	check2, entryCheck2 := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-2")
	feedback = &planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{a.ID, repair.ID}, Reason: "两条交付已有验证证据"}}}
	completeGoalEntry(h, check2, entryCheck2, workflowReply(nil, feedback))
	fresh := waitGoalStatus(t, h, run.ID, RunCompleted)
	if fresh.Planning.Status != "complete" || fresh.Planning.Iteration != 2 {
		t.Fatalf("应完成两次目标评估：%+v", fresh.Planning)
	}
	if result := FinalGoalResult(fresh); result == nil || !strings.Contains(result.Text, "两条交付已有验证证据") {
		t.Fatal("纯机器反馈也必须生成可读验收依据")
	}
}

func TestGoalWorkflowBlockedResumesWithNewEvaluation(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "A 已完成，B 需要外部凭证")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	feedback := &planningFeedback{Status: "blocked", Checks: []PlanningCheck{{ID: "all", State: "unmet", Reason: "B 缺少外部凭证"}}, Remaining: []string{"完成 B"}, Blockers: []string{"需要用户提供 B 凭证"}}
	completeGoalEntry(h, check, checkEntry, workflowReply(nil, feedback))
	fresh := waitGoalStatus(t, h, run.ID, RunWaitingInput)
	if EvaluateGoal(fresh, h.team, GoalScope{ID: "run", Text: run.Goal}).CanConfirm {
		t.Fatal("阻塞且未完成目标不能确认达成")
	}
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatal(err)
	}
	check2, _ := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-2")
	if !check2.GoalFeedback || h.store.Run(run.ID).Status == RunCompleted {
		t.Fatal("resume 必须重新评估，不能把全终态任务直接折叠成完成")
	}
}

func TestGoalWorkflowEmptyFeedbackRepairKeepsEvaluationProtocol(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "部分交付")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	completeGoalEntry(h, check, checkEntry, "目标没完成，我会继续")
	repair, repairEntry := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-1")
	if !repair.GoalFeedback || !strings.Contains(repairEntry.text, "feedback=") || !strings.Contains(repairEntry.text, run.Goal) {
		t.Fatal("评估协议纠错必须保留目标、状态和 feedback schema")
	}
	if h.store.Run(run.ID).Planning.Status == "complete" {
		t.Fatal("普通进度回复不得通过目标验收")
	}
}

func TestGoalWorkflowPlanAndEvidenceGuards(t *testing.T) {
	team := testTeam()
	state := &GoalPlanningState{Version: 1, MaxIterations: 3}
	if err := applyPlanningDefinition(state, *goalTestPlan(), team.Policy); err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "run", TeamID: team.ID, Planning: state, Tasks: []Task{
		{ID: "lead", MemberID: team.LeadID, Status: TaskCompleted, ExecutionContext: ExecutionContextLeadControl, Result: "计划已制定"},
		{ID: "work", MemberID: "dev", Status: TaskCompleted, Result: "产物已生成，检查通过"},
		{ID: "empty", MemberID: "dev", Status: TaskCompleted},
	}}
	for _, evidence := range []string{"missing", "empty", "lead"} {
		feedback := planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{evidence}}}}
		if err := applyPlanningFeedback(cloneGoalPlanning(state), run, feedback, nil); err == nil {
			t.Fatalf("无效证据 %s 不能声明完成", evidence)
		}
	}
	plan := *goalTestPlan()
	plan.Criteria = []PlanningCriterion{{ID: "all", Text: "只完成 A"}}
	if err := applyPlanningDefinition(cloneGoalPlanning(state), plan, team.Policy); err == nil {
		t.Fatal("不能缩小原始验收条件")
	}
	plan = *goalTestPlan()
	plan.Criteria = append(plan.Criteria, PlanningCriterion{ID: "extra", Text: "聊天补充的新条件"})
	if err := applyPlanningDefinition(cloneGoalPlanning(state), plan, team.Policy); err != nil {
		t.Fatalf("用户补充条件应可追加：%v", err)
	}
	feedback := planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet"}}, Remaining: []string{"继续实现"}}
	if err := applyPlanningFeedback(cloneGoalPlanning(state), run, feedback, nil); err == nil {
		t.Fatal("continue 必须有实际派发")
	}
	feedback.Status = "complete"
	feedback.Checks[0] = PlanningCheck{ID: "all", State: "met", EvidenceTaskIDs: []string{"work"}}
	if err := applyPlanningFeedback(cloneGoalPlanning(state), run, feedback, nil); err == nil {
		t.Fatal("仍有缺口不能 complete")
	}
	feedback.Remaining = nil
	if err := applyPlanningFeedback(cloneGoalPlanning(state), run, feedback, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGoalWorkflowNoProgressDoesNotResetWhenGapsAreRephrased(t *testing.T) {
	state := &GoalPlanningState{Criteria: []PlanningCriterion{{ID: "all", Text: "完整交付"}}, Checks: []PlanningCheck{{ID: "all", State: "unmet"}}}
	run := &Run{Tasks: []Task{{ID: "partial", Status: TaskCompleted, Result: "部分成果"}}}
	for i := 1; i <= 2; i++ {
		feedback := planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet"}}, Remaining: []string{fmt.Sprintf("换个说法的缺口 %d", i)}}
		if err := applyPlanningFeedback(state, run, feedback, []Action{workflowAction("lead", "a")}); err != nil {
			t.Fatal(err)
		}
	}
	if state.NoProgress != 2 {
		t.Fatal("仅改写缺口文字不能算进展")
	}
	feedback := planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet", EvidenceTaskIDs: []string{"partial"}}}, Remaining: []string{"还有一步"}}
	if err := applyPlanningFeedback(state, run, feedback, []Action{workflowAction("lead", "a")}); err != nil || state.NoProgress != 0 {
		t.Fatalf("新增有效证据应计为进展：%+v %v", state, err)
	}
}

type workflowWorkspaceAdapter struct {
	stubWorkspaceAdapter
	paths map[string]string
}

func (a *workflowWorkspaceAdapter) Resolve(id string) (string, error) {
	if path := a.paths[id]; path != "" {
		return path, nil
	}
	return "", fmt.Errorf("工作区未登记：%s", id)
}

func TestGoalWorkflowWritersUseRegisteredSeparateWorkspaces(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	h.service.Workspaces = &workflowWorkspaceAdapter{paths: map[string]string{"wa": "/tmp/workflow-a", "wb": "/tmp/workflow-b"}}
	plan := goalTestPlan()
	plan.Workflows[0].WorkspaceID = "wa"
	plan.Workflows[1].WorkspaceID = "wb"
	actionA, actionB := workflowAction("lead", "a"), workflowAction("lead", "b")
	actionA.WriteLock, actionB.WriteLock = TaskWriteLockExclusive, TaskWriteLockExclusive
	startGoalBranches(t, h, run, plan, actionA, actionB)
	a, _ := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	b, _ := waitGoalTaskEntry(t, h, run.ID, "lead-d1-1")
	if h.service.locks.Holder("/tmp/workflow-a") != a.ID || h.service.locks.Holder("/tmp/workflow-b") != b.ID {
		t.Fatal("独立工作区写任务应各自持锁并同时在途")
	}
	if a.WorkspaceMode != WorkspaceIsolatedWorktree || b.WorkspaceMode != WorkspaceIsolatedWorktree {
		t.Fatal("动作必须继承 workflow 登记工作区")
	}
}

func TestGoalWorkflowSchedulerRespectsThreadLocksDependenciesAndCap(t *testing.T) {
	policy := DefaultPolicy()
	makeTask := func(id, workflow, lock string) Task {
		return Task{ID: id, MemberID: "dev", WorkflowID: workflow, ExecutionThreadID: workflowThreadID("run", "dev", workflow), WriteLock: lock, Status: TaskPlanned}
	}
	a, b := makeTask("a", "a", TaskWriteLockExclusive), makeTask("b", "b", TaskWriteLockExclusive)
	run := &Run{Tasks: []Task{a, b}}
	locks := NewLockRegistry()
	cwds := map[string]string{"a": "/tmp/shared", "b": "/tmp/shared"}
	if selected := ReadyTasks(run, policy, 0, nil, cwds, locks); len(selected) != 1 {
		t.Fatalf("同目录写不得并行：%v", selected)
	}
	run.Tasks[1].WriteLock = TaskWriteLockNone
	if selected := ReadyTasks(run, policy, 0, nil, cwds, locks); len(selected) != 2 {
		t.Fatalf("独立只读可与写并行：%v", selected)
	}
	run.Tasks[1].WorkflowID, run.Tasks[1].ExecutionThreadID = a.WorkflowID, a.ExecutionThreadID
	if selected := ReadyTasks(run, policy, 0, nil, cwds, locks); len(selected) != 1 {
		t.Fatalf("同 workflow 的同成员会话不得同时派发：%v", selected)
	}
	run.Tasks[1] = b
	cwds["b"] = "/tmp/isolated"
	run.Tasks[1].DependsOn = []string{"a"}
	if selected := ReadyTasks(run, policy, 0, nil, cwds, locks); len(selected) != 1 {
		t.Fatalf("真实依赖尚未完成不得派发：%v", selected)
	}
	run.Tasks[1].DependsOn = nil
	if selected := ReadyTasks(run, policy, policy.MaxParallelTasks-1, nil, cwds, locks); len(selected) != 1 {
		t.Fatalf("并发容量必须保留：%v", selected)
	}
}

func TestGoalWorkflowJournalRebuildPreservesPlanningThreadsAndLoop(t *testing.T) {
	h, ledger, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"), workflowAction("lead", "b"))
	a, entryA := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	b, entryB := waitGoalTaskEntry(t, h, run.ID, "lead-d1-1")
	completeGoalEntry(h, a, entryA, "A 已完成")
	completeGoalEntry(h, b, entryB, "B 有部分成果")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	feedback := &planningFeedback{Status: "blocked", Checks: []PlanningCheck{{ID: "all", State: "unmet", Reason: "需要凭证"}}, Blockers: []string{"用户提供凭证"}}
	completeGoalEntry(h, check, checkEntry, workflowReply(nil, feedback))
	waitGoalStatus(t, h, run.ID, RunWaitingInput)
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := LoadStore(filepath.Join(t.TempDir(), "teams.json"), filepath.Join(t.TempDir(), "runs.json"))
	teamCopy := h.team.Clone()
	teamCopy.Version = 0
	if err := rebuilt.SaveTeam(teamCopy); err != nil {
		t.Fatal(err)
	}
	rebuilt.runsCorrupt = true
	rebuilt.RebuildRunsFromJournal(events)
	restored := rebuilt.Run(run.ID)
	if restored == nil || restored.Planning == nil || restored.Planning.Status != "blocked" || restored.Status != RunWaitingInput {
		t.Fatalf("目标状态必须从事件重建：%+v", restored)
	}
	for _, original := range []Task{a, b, check} {
		task := findTaskIn(restored, original.ID)
		if task == nil || task.WorkflowID != original.WorkflowID || task.GoalFeedback != original.GoalFeedback || task.ExecutionThreadID != original.ExecutionThreadID || task.Prompt != original.Prompt || task.WriteLock != original.WriteLock {
			t.Fatalf("重建丢失 workflow 任务协议：%+v", task)
		}
	}
	clone := restored.Clone()
	clone.Planning.Criteria[0].Text = "changed"
	if restored.Planning.Criteria[0].Text == "changed" {
		t.Fatal("规划状态必须深拷贝")
	}
	if ledger.Get(h.team.ID, run.ID).PlanningVersion != 1 {
		t.Fatal("输入必须保存规划版本")
	}
}

func TestGoalWorkflowChatSupplementLinksIssueAndExistingWorkflow(t *testing.T) {
	h, ledger, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"), workflowAction("lead", "b"))
	_, _ = waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	b, entryB := waitGoalTaskEntry(t, h, run.ID, "lead-d1-1")
	const inputID, text = "chat-supplement", "给 B 再增加导出验收条件"
	const taskID = "input-" + inputID
	if _, _, err := ledger.AcceptWithOptions(h.team.ID, inputID, text, nil, "policy", 64, InputAcceptOptions{PlanningVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.service.FollowUpConversationRun(h.team, run.ID, taskID, text, nil); err != nil {
		t.Fatal(err)
	}
	control, controlEntry := waitGoalTaskEntry(t, h, run.ID, taskID)
	if !strings.Contains(controlEntry.text, "交付 B") {
		t.Fatal("聊天补充必须看到已有 workflow 规划")
	}
	issue := ledger.ListIssues(h.team.ID)[0]
	association := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"link","issueId":%q,"source":"full"}]}</team-issues>`, inputID, issue.ID)
	plan := goalTestPlan()
	plan.Criteria = append(plan.Criteria, PlanningCriterion{ID: "export", Text: "B 支持导出并通过检查"})
	completeGoalEntry(h, control, controlEntry, association+workflowReply(plan, nil, workflowAction(taskID, "b")))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if task := findTaskIn(h.store.Run(run.ID), taskID+"-d1-0"); task != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	target := findTaskIn(h.store.Run(run.ID), taskID+"-d1-0")
	if target == nil || target.WorkflowID != "b" || len(h.store.Run(run.ID).Planning.Criteria) != 2 {
		t.Fatal("聊天补充必须落到 B 的实际任务，并追加验收条件")
	}
	if input := ledger.Get(h.team.ID, inputID); len(input.IssueLinks) != 1 || input.IssueLinks[0].IssueID != issue.ID {
		t.Fatal("聊天补充必须自动关联原事项")
	}
	completeGoalEntry(h, b, entryB, "B 原交付完成")
	followup, _ := waitGoalTaskEntry(t, h, run.ID, target.ID)
	if followup.DispatchSessionID != b.DispatchSessionID {
		t.Fatal("补充任务应在原 workflow 会话执行")
	}
}

func TestGoalWorkflowVerificationFailureReturnsToRepairLoop(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "交付已生成")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	if err := h.service.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		fresh.Status = RunReducing
		fresh.Verify = &VerifyResult{ExitCode: 7, OutputHead: "导出检查失败"}
		return nil, true
	}); err != nil {
		t.Fatal(err)
	}
	h.service.applyVerifyVerdictLocked(state, run.ID)
	state.mu.Unlock()
	if h.store.Run(run.ID).Status != RunDispatching {
		t.Fatal("失败检查应回到评估修复流程")
	}
	feedback := &planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet", Reason: "导出检查失败"}}, Remaining: []string{"修复导出"}}
	completeGoalEntry(h, check, checkEntry, workflowReply(nil, feedback, workflowAction(check.ID, "a")))
	_, _ = waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1-d1-0")
	if h.store.Run(run.ID).Verify != nil {
		t.Fatal("修复后必须重新跑验证，不能复用失败检查")
	}
}

func TestGoalWorkflowLimitAndUserStopGuards(t *testing.T) {
	for _, test := range []struct {
		status           string
		iteration, tasks int
		expect           string
	}{
		{RunDispatching, 3, 1, RunWaitingInput}, {RunDispatching, 0, 12, RunWaitingInput},
		{RunPaused, 3, 1, RunPaused}, {RunCancelled, 3, 1, RunCancelled},
	} {
		h := newDispatchHarness(t)
		h.team.Policy.AutoDispatch = true
		if err := h.store.SaveTeam(h.team); err != nil {
			t.Fatal(err)
		}
		if test.tasks == 12 {
			test.tasks = h.team.Policy.MaxTasksPerRun
		}
		planning := &GoalPlanningState{Version: 1, MaxIterations: 3, Iteration: test.iteration, Status: "continue", Criteria: goalTestPlan().Criteria, Workflows: goalTestPlan().Workflows}
		run := &Run{ID: "limits", TeamID: h.team.ID, Status: test.status, Planning: planning, AutoPlan: true}
		for i := 0; i < test.tasks; i++ {
			run.Tasks = append(run.Tasks, Task{ID: fmt.Sprintf("done-%d", i), Status: TaskCompleted, MemberID: "dev", Result: "阶段成果"})
		}
		if err := h.store.SaveRun(run); err != nil {
			t.Fatal(err)
		}
		state := h.service.stateFor(run.ID)
		state.mu.Lock()
		h.service.ensureGoalFeedbackLocked(state, run)
		state.mu.Unlock()
		if fresh := h.store.Run(run.ID); fresh.Status != test.expect || len(fresh.Tasks) != test.tasks {
			t.Fatalf("上限和用户停止必须保留：%+v", fresh)
		}
	}
}

func TestGoalWorkflowReassignmentPreservesBranchAndChangesThreadOwner(t *testing.T) {
	target := Task{ID: "work", MemberID: "dev", WorkflowID: "a", ExecutionThreadID: workflowThreadID("run", "dev", "a"), Status: TaskFailed}
	run := &Run{ID: "run", TeamID: "team", Tasks: []Task{target}}
	action := workflowAction("lead-triage-1", "a")
	action.Member = "lead"
	action.ActionKey = ActionKey(run.TeamID, run.ID, action.SourceTaskID, 1, 0)
	events := applyRetriage(run, &run.Tasks[0], &action, "lead")
	if run.Tasks[0].ExecutionThreadID != workflowThreadID(run.ID, "lead", "a") || run.Tasks[0].WorkflowID != "a" {
		t.Fatal("改派必须保留分支，并更换线程所有者")
	}
	replayed := &Run{Tasks: []Task{target}}
	applyJournalEventToRun(replayed, events[0])
	if replayed.Tasks[0].ExecutionThreadID != run.Tasks[0].ExecutionThreadID || replayed.Tasks[0].MemberID != "lead" {
		t.Fatal("改派线程必须通过事件重建")
	}
}

func TestGoalWorkflowLongEvaluationFitsDispatchBudget(t *testing.T) {
	team := testTeam()
	team.Policy.MaxContextChars = 30000
	upstream := Task{ID: "upstream", MemberID: "dev", Status: TaskCompleted, Result: strings.Repeat("有效成果", 10000)}
	check := Task{ID: "lead-goal-check-1", MemberID: team.LeadID, ExecutionContext: ExecutionContextLeadControl, GoalFeedback: true, Reduction: true, Prompt: goalFeedbackInstruction, DependsOn: []string{upstream.ID}}
	run := &Run{ID: "run", TeamID: team.ID, AutoPlan: true, Goal: "原目标开头" + strings.Repeat("完整交付", 2000) + "原目标末尾", Planning: &GoalPlanningState{Criteria: goalTestPlan().Criteria, Workflows: goalTestPlan().Workflows}, Tasks: []Task{upstream, check}}
	message := composeTaskDispatchMessage(team, run, &check, team.Policy).text
	if len(message) > promptHardLimit || !strings.Contains(message, "原目标开头") || !strings.Contains(message, "原目标末尾") {
		t.Fatalf("评估协议应预留容量并保留完整目标，得到 %d 字节", len(message))
	}
}

func TestGoalWorkflowBudgetResumeDoesNotSkipGoalEvaluation(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.AutoDispatch = true
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "budget-goal", TeamID: h.team.ID, AutoPlan: true, Status: RunWaitingInput, StopReason: StopReasonBudgetExhausted, Planning: &GoalPlanningState{Version: 1, MaxIterations: 3, Status: "running", Criteria: goalTestPlan().Criteria, Workflows: goalTestPlan().Workflows}, Tasks: []Task{{ID: "work", MemberID: "dev", Status: TaskCompleted, Result: "阶段成果"}}}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Resume(run.ID); err != nil {
		t.Fatal(err)
	}
	check, _ := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	if !check.GoalFeedback || h.store.Run(run.ID).Status == RunCompleted {
		t.Fatal("预算继续必须完成目标评估，不能跳过验收")
	}
}

func TestGoalWorkflowSameTaskIDsAcrossRunsCannotShareWriteLock(t *testing.T) {
	h := newDispatchHarness(t)
	makeRun := func(id string) *Run {
		run := &Run{ID: id, TeamID: h.team.ID, Status: RunDispatching, Tasks: []Task{{ID: "lead-d1-0", MemberID: "dev", Status: TaskPlanned, Prompt: "写入成果", WriteLock: TaskWriteLockExclusive}}}
		if err := h.store.SaveRun(run); err != nil {
			t.Fatal(err)
		}
		return h.store.Run(id)
	}
	a, b := makeRun("run-a"), makeRun("run-b")
	dispatch := func(run *Run) *taskDispatch {
		state := h.service.stateFor(run.ID)
		state.mu.Lock()
		value, err := h.service.accountDispatchLocked(state, h.team, h.store.Run(run.ID), run.Tasks[0].ID, "/tmp/shared", nil)
		state.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	if dispatch(a) == nil || dispatch(b) != nil {
		t.Fatal("不同 Run 的同名任务不能被视为同一锁持有者")
	}
	if _, err := h.service.Cancel(a.ID); err != nil {
		t.Fatal(err)
	}
	if dispatch(b) == nil {
		t.Fatal("取消首个 Run 必须释放正确的锁归属")
	}
}
