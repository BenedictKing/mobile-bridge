package team

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func goalRun() *Run {
	return &Run{ID: "goal-run", TeamID: "team-1", Goal: "说明现有过滤能力与限制", Status: RunCompleted,
		Tasks: []Task{{ID: "answer", MemberID: "dev", Status: TaskCompleted, Attempt: 1,
			Result: "已调查，当前只有逐 Key 白名单。", ResultDetail: &TaskResult{SessionID: "original", TurnID: "turn-1", Result: "已调查，当前只有逐 Key 白名单。"}}}}
}

func goalRequest(run *Run, scope GoalScope, action string) GoalReviewRequest {
	out := EvaluateGoal(run, testTeam(), scope)
	return GoalReviewRequest{ID: uuid.NewString(), Version: run.Version, GoalRevision: out.Contract.Revision,
		ScopeHash: scope.Hash, EvidenceHash: out.EvidenceHash, Action: action, Reason: "已查看结果并核对原始要求"}
}

func answerContract(scope GoalScope) *GoalContract {
	c := DefaultGoalContract(scope)
	c.DeliverableKind = "answer"
	c.Verify = GoalCheck{Requirement: "not_required", Reason: "仅咨询，不执行代码或配置"}
	c.Review = GoalCheck{Requirement: "not_required", Reason: "用户直接核对回答"}
	return &c
}

func TestCompletedExecutionNeverImpliesSatisfiedGoal(t *testing.T) {
	run := goalRun()
	scope := RunGoalScope(run)
	if out := EvaluateGoal(run, testTeam(), scope); out.Status != "pending_confirmation" || !out.CanConfirm {
		t.Fatalf("执行结束不能自行证明目标达成: %+v", out)
	}
	run.Tasks[0].Result = ""
	run.Tasks[0].ResultDetail = nil
	if out := EvaluateGoal(run, testTeam(), scope); out.Status != "unassessed" || out.Result != nil {
		t.Fatalf("空结果不能交付: %+v", out)
	}
}

func TestGoalCanBeConfirmedWithoutDefiningAnAcceptanceForm(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	request := goalRequest(run, scope, "assess")
	request.Reason = ""
	request.States = map[string]string{"original": "met"}
	fresh, err := h.service.ReviewGoal(run.ID, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	out := EvaluateGoal(fresh, h.team, scope)
	if out.Status != "satisfied" || out.AssessedBy != "user" || out.Contract.Criteria[0].Text != scope.Text {
		t.Fatalf("直接确认必须保留原始要求和用户判断: %+v", out)
	}
	if out.Contract.DeliverableKind != "result" || out.Contract.Verify.Requirement != "auto" || out.Contract.Review.Requirement != "auto" {
		t.Fatalf("默认配置应跟随执行事实，无需用户填写: %+v", out.Contract)
	}
	duplicate, err := h.service.ReviewGoal(run.ID, scope, request)
	if err != nil || duplicate.Version != fresh.Version || len(h.queue.calls) != 0 {
		t.Fatalf("直接确认仍须幂等，且不得派发任务: %v", err)
	}
	request.ID = uuid.NewString()
	if _, err := h.service.ReviewGoal(run.ID, scope, request); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧页面确认仍应拒绝: %v", err)
	}
}

func TestAutomaticGoalSettingsRetainLegacyCriteriaAndRequiredChecks(t *testing.T) {
	run := goalRun()
	scope := RunGoalScope(run)
	contract := DefaultGoalContract(scope)
	contract.DeliverableKind, contract.Verify.Requirement = "unspecified", "unspecified"
	contract.Review.Requirement = "required"
	contract.Criteria = append(contract.Criteria, GoalCriterion{ID: "extra", Text: "保留额外要求", State: "unknown"})
	run.Goals = map[string]*GoalRecord{scope.ID: {Scope: scope, Contract: contract}}
	out := EvaluateGoal(run, testTeam(), scope)
	if out.Contract.DeliverableKind != "result" || out.Contract.Verify.Requirement != "auto" || out.Contract.Review.Requirement != "required" || len(out.Contract.Criteria) != 2 || out.CanConfirm {
		t.Fatalf("兼容旧记录不能抹去明确约定或自动确认: %+v", out)
	}
}

func TestGoalReviewPersistsWithoutDispatchAndReplaysIdempotently(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	define := goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	run, err := h.service.ReviewGoal(run.ID, scope, define)
	if err != nil {
		t.Fatal(err)
	}
	assess := goalRequest(run, scope, "assess")
	assess.States = map[string]string{"original": "met"}
	fresh, err := h.service.ReviewGoal(run.ID, scope, assess)
	if err != nil {
		t.Fatal(err)
	}
	out := EvaluateGoal(fresh, h.team, scope)
	if out.Status != "satisfied" || fresh.Status != RunCompleted || out.Result.SessionID != "original" {
		t.Fatalf("目标判定或执行状态有误: %+v", out)
	}
	duplicate, err := h.service.ReviewGoal(run.ID, scope, assess)
	if err != nil || duplicate.Version != fresh.Version {
		t.Fatalf("重复确认不应重复记账: %+v %v", duplicate, err)
	}
	assess.Reason = "不同内容"
	if _, err = h.service.ReviewGoal(run.ID, scope, assess); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("操作 ID 冲突应拒绝: %v", err)
	}
	if len(h.queue.calls) != 0 {
		t.Fatal("目标确认不能派发任务")
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := goalRun()
	for _, event := range events {
		applyJournalEventToRun(rebuilt, event)
	}
	if EvaluateGoal(rebuilt, h.team, scope).Status != "satisfied" {
		t.Fatal("journal 重放丢失目标确认")
	}
	copy := fresh.Clone()
	copy.Goals[scope.ID].Contract.Criteria[0].Text = "改写"
	if fresh.Goals[scope.ID].Contract.Criteria[0].Text != scope.Text {
		t.Fatal("Clone 不能共享条件切片")
	}
	raw, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	var restored Run
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if EvaluateGoal(&restored, h.team, scope).Status != "satisfied" {
		t.Fatal("快照往返不能丢失确认与证据绑定")
	}
}

func TestGoalAssessmentRejectsMissingChecksAndStaleEvidence(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	_ = h.store.SaveRun(run)
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	define := goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	define.Contract.Verify.Requirement = "required"
	run, err := h.service.ReviewGoal(run.ID, scope, define)
	if err != nil {
		t.Fatal(err)
	}
	if out := EvaluateGoal(run, h.team, scope); out.Status != "pending_verification" || out.CanConfirm {
		t.Fatalf("未配置检查不能当作通过: %+v", out)
	}
	assess := goalRequest(run, scope, "assess")
	assess.States = map[string]string{"original": "met"}
	if _, err = h.service.ReviewGoal(run.ID, scope, assess); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("缺证据应拒绝全部达成: %v", err)
	}
	assess.States["original"] = "unmet"
	run, err = h.service.ReviewGoal(run.ID, scope, assess)
	if err != nil {
		t.Fatal(err)
	}
	if EvaluateGoal(run, h.team, scope).Status != "unsatisfied" {
		t.Fatal("检查未通过仍应允许记录不满足项")
	}
	stale := goalRequest(run, scope, "assess")
	stale.States = map[string]string{"original": "unmet"}
	run.Tasks[0].Result = "新结果"
	_ = h.store.SaveRun(run)
	if _, err = h.service.ReviewGoal(run.ID, scope, stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧页面应发生版本冲突: %v", err)
	}
	changed := h.store.Run(run.ID)
	if out := EvaluateGoal(changed, h.team, scope); out.AssessedBy != "" || out.Contract.Criteria[0].State != "unknown" {
		t.Fatalf("旧确认不能套用新结果: %+v", out)
	}
}

func TestAutomaticGoalChecksDoNotWaiveFailedOrMissingVerification(t *testing.T) {
	run := goalRun()
	run.VerifyCommand = []string{"make", "test"}
	for _, verify := range []*VerifyResult{nil, {ExitCode: 1}, {ExitCode: 0}} {
		run.Verify = verify
		out := EvaluateGoal(run, testTeam(), RunGoalScope(run))
		if out.CanConfirm || out.Status != "pending_verification" || out.Contract.Verify.Requirement != "required" {
			t.Fatalf("自动配置不能跳过未执行、失败或缺少版本身份的验证: %+v", out)
		}
	}
	run.VerifyCommand = nil
	run.Verify = &VerifyResult{ExitCode: 1}
	if out := EvaluateGoal(run, testTeam(), RunGoalScope(run)); out.CanConfirm || out.Contract.Verify.Status != "failed" {
		t.Fatalf("已有失败证据不能因命令缺失而被忽略: %+v", out)
	}
}

func TestVerificationEnvironmentFailureIsUnknown(t *testing.T) {
	run := goalRun()
	run.VerifyCommand = []string{"make", "test"}
	run.Verify = &VerifyResult{ExitCode: -1, OutputHead: "无法解析 Lead 会话的工作目录，验证命令未执行"}
	out := EvaluateGoal(run, testTeam(), RunGoalScope(run))
	if out.Contract.Verify.Status != "unknown" || out.Contract.Verify.Reason != run.Verify.OutputHead || out.CanConfirm {
		t.Fatalf("环境失败应保留原因并标记 unknown: %+v", out.Contract.Verify)
	}
	run.Verify.OutputHead = ""
	if check, _ := goalCheckEvidence(run, testTeam(), DefaultGoalContract(RunGoalScope(run))); check.Status != "unknown" || check.Reason == "" {
		t.Fatalf("无输出的环境失败也应说明检查方向: %+v", check)
	}
	run.Verify.ExitCode = 1
	if check, _ := goalCheckEvidence(run, testTeam(), DefaultGoalContract(RunGoalScope(run))); check.Status != "failed" {
		t.Fatalf("命令明确返回非零退出码仍是代码失败: %+v", check)
	}
}

func TestCancelledRunHasNoPendingGoalAssessment(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	run.Status, run.StopReason = RunCancelled, "用户取消运行"
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	out := EvaluateGoal(run, h.team, scope)
	if out.Status != "cancelled" || out.CanConfirm || out.Result == nil || out.ExecutionStatus != RunCancelled {
		t.Fatalf("取消只保留已有结果，不要求填写验收: %+v", out)
	}
	request := goalRequest(run, scope, "assess")
	request.States = map[string]string{"original": "unmet"}
	if _, err := h.service.ReviewGoal(run.ID, scope, request); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("取消的 Run 不应接受新的结果评估: %v", err)
	}
}

func TestGoalScopeAndPartialAssessmentRemainIndependent(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	_ = h.store.SaveRun(run)
	run = h.store.Run(run.ID)
	scope := NewGoalScope("issue-a", "实现分组白名单并验证")
	define := goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	define.Contract.Criteria = append(define.Contract.Criteria, GoalCriterion{ID: "analysis", Text: "完成调查"})
	run, err := h.service.ReviewGoal(run.ID, scope, define)
	if err != nil {
		t.Fatal(err)
	}
	request := goalRequest(run, scope, "assess")
	request.States = map[string]string{"original": "unmet", "analysis": "met"}
	run, err = h.service.ReviewGoal(run.ID, scope, request)
	if err != nil {
		t.Fatal(err)
	}
	if EvaluateGoal(run, h.team, scope).Status != "partial" {
		t.Fatal("只完成调查应为部分达成")
	}
	if EvaluateGoal(run, h.team, NewGoalScope("issue-b", "另一个要求")).Status == "satisfied" {
		t.Fatal("不能跨事项应用确认")
	}
	if out := EvaluateGoal(run, h.team, NewGoalScope("issue-a", "新修订目标")); out.Status == "partial" || out.AssessedBy != "" {
		t.Fatal("目标修订后旧评估应失效")
	}
	define = goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	if _, err = h.service.ReviewGoal(run.ID, scope, define); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("不能删去原有条件: %v", err)
	}
}

func TestGoalRequiredEvidenceTracksCurrentWorkspace(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.ProjectCwd = "/workspace"
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	fingerprint := &fakeFingerprint{head: "commit", diff: "diff"}
	h.service.Fingerprints = fingerprint
	run := goalRun()
	run.VerifyCommand = []string{"go", "test", "./..."}
	run.Verify = &VerifyResult{ExitCode: 0, HeadRevision: "commit", DiffHash: "diff"}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	define := goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	define.Contract.Verify = GoalCheck{Requirement: "required"}
	run, err := h.service.ReviewGoal(run.ID, scope, define)
	if err != nil {
		t.Fatal(err)
	}
	confirm := goalRequest(run, scope, "assess")
	confirm.States = map[string]string{"original": "met"}
	run, err = h.service.ReviewGoal(run.ID, scope, confirm)
	if err != nil {
		t.Fatal(err)
	}
	if h.service.GoalOutcome(run, scope).Status != "satisfied" {
		t.Fatal("匹配当前版本的证据应可确认")
	}
	fingerprint.diff = "external-edit"
	if out := h.service.GoalOutcome(run, scope); out.Status != "pending_verification" || out.CanConfirm {
		t.Fatalf("外部改动使旧证据失效: %+v", out)
	}
	confirm = goalRequest(run, scope, "assess")
	confirm.States = map[string]string{"original": "met"}
	if _, err = h.service.ReviewGoal(run.ID, scope, confirm); !errors.Is(err, ErrGoalReviewInvalid) {
		t.Fatalf("旧版本检查不能确认当前目标: %v", err)
	}
}

func TestGoalReviewFailureLeavesSnapshotAndExecutionUnchanged(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	request := goalRequest(run, scope, "define")
	request.Contract = answerContract(scope)
	if err := h.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.ReviewGoal(run.ID, scope, request); err == nil {
		t.Fatal("journal 写失败必须返回错误")
	}
	fresh := h.store.Run(run.ID)
	if fresh.Version != run.Version || len(fresh.Goals) != 0 || fresh.Status != RunCompleted {
		t.Fatal("未落账事实不能修改快照或重开执行")
	}
}

func TestGoalReviewKeepsFailureFromAnotherReviewBranch(t *testing.T) {
	run := goalRun()
	run.Tasks = append(run.Tasks,
		Task{ID: "review-a", MemberID: "rev", Review: reviewFlag(true), ReviewOf: []string{"a"}, Status: TaskCompleted, HeadRevision: "h", DiffHash: "d", ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictFailed}},
		Task{ID: "review-b", MemberID: "rev", Review: reviewFlag(true), ReviewOf: []string{"b"}, Status: TaskCompleted, HeadRevision: "h", DiffHash: "d", ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictPassed}})
	scope := RunGoalScope(run)
	contract := *answerContract(scope)
	contract.Review.Requirement = "required"
	run.Goals = map[string]*GoalRecord{scope.ID: {Scope: scope, Contract: contract}}
	if out := EvaluateGoal(run, testTeam(), scope); out.Contract.Review.Status != "failed" || out.CanConfirm {
		t.Fatalf("另一分支通过不能盖掉失败裁决: %+v", out)
	}
}

func TestWaitingActionsIdentifyTheirActualBlocker(t *testing.T) {
	cases := []struct {
		run        Run
		code, kind string
	}{
		{Run{Tasks: []Task{{Status: TaskOutcomeUnknown}}}, "outcome_unknown", "recheck"},
		{Run{Tasks: []Task{{Status: TaskStopped, Outcome: "quota"}}}, "quota", "configure"},
		{Run{StopReason: "verify_failed: exit 1"}, "verification_failed", "verify"},
		{Run{PendingQuestions: []PendingQuestion{{Status: QuestionStatusOpen}}}, "question", "answer"},
		{Run{}, "blocked", "inspect"},
	}
	for _, scenario := range cases {
		action := RunWaitingAction(&scenario.run)
		if action.ReasonCode != scenario.code || action.Kind != scenario.kind || action.Actor != "user" {
			t.Fatalf("等待原因误分类: %+v", action)
		}
	}
}

func TestConcurrentGoalConfirmationIsRecordedOnce(t *testing.T) {
	h := newDispatchHarness(t)
	run := goalRun()
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	run = h.store.Run(run.ID)
	scope := RunGoalScope(run)
	define := goalRequest(run, scope, "define")
	define.Contract = answerContract(scope)
	run, err := h.service.ReviewGoal(run.ID, scope, define)
	if err != nil {
		t.Fatal(err)
	}
	request := goalRequest(run, scope, "assess")
	request.States = map[string]string{"original": "met"}
	var wg sync.WaitGroup
	errorsFound := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := h.service.ReviewGoal(run.ID, scope, request); errorsFound <- err }()
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("条件定义与确认应各落账一次: %d", len(events))
	}
	if len(h.queue.calls) != 0 {
		t.Fatal("确认不能重发消息")
	}
}
