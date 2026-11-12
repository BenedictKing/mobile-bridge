package team

import (
	"reflect"
	"testing"
)

func TestIssueTaskAttributionSeparatesBranchesAndSharedEvidence(t *testing.T) {
	issueA, issueB := &Issue{ID: "issue-a", TeamID: "team"}, &Issue{ID: "issue-b", TeamID: "team"}
	input := &TeamInput{ID: "input", TeamID: "team", Revision: 1, RunIDs: []string{"run"}, IssueLinks: []IssueInputLink{
		{IssueID: issueA.ID, InputID: "input", Revision: 1, SourceStart: 0, SourceEnd: 4},
		{IssueID: issueB.ID, InputID: "input", Revision: 1, SourceStart: 4, SourceEnd: 8},
	}, WorkItems: []WorkItem{
		{ID: "item-a", RunID: "run", SourceRevision: 1, SourceStart: 0, SourceEnd: 4, TaskIDs: []string{"root-a"}},
		{ID: "item-b", RunID: "run", SourceRevision: 1, SourceStart: 4, SourceEnd: 8, TaskIDs: []string{"root-b"}},
	}}
	run := &Run{ID: "run", TeamID: "team", Tasks: []Task{
		{ID: "lead", ExecutionContext: ExecutionContextLeadControl},
		{ID: "root-a"},
		{ID: "root-a-d1-0"},
		{ID: "review-a", ReviewOf: []string{"root-a-d1-0"}},
		{ID: "root-b"},
		{ID: "root-b-d1-0"},
		{ID: "review-b", ReviewOf: []string{"root-b-d1-0"}},
		{ID: "review-shared", ReviewOf: []string{"root-a-d1-0", "root-b-d1-0"}},
		{ID: "reduce", Reduction: true},
	}}
	attribution := IssueTaskAttribution(issueA, run, []*TeamInput{input})
	if !attribution.Known || !reflect.DeepEqual(attribution.TaskIDs, []string{"root-a", "root-a-d1-0", "review-a"}) {
		t.Fatalf("事项 A 归属不符: %+v", attribution)
	}
	if !reflect.DeepEqual(attribution.SharedTaskIDs, []string{"lead", "review-shared", "reduce"}) {
		t.Fatalf("共享任务归属不符: %+v", attribution.SharedTaskIDs)
	}

	run.PendingActions = []Action{{SourceTaskID: "root-b-d1-0"}, {SourceTaskID: "root-a-d1-0"}}
	run.PendingQuestions = []PendingQuestion{{TaskID: "root-b-d1-0"}, {TaskID: "root-a-d1-0"}, {TaskID: "lead"}}
	scoped, _ := RunForIssue(issueA, run, []*TeamInput{input})
	var ids []string
	for _, task := range scoped.Tasks {
		ids = append(ids, task.ID)
	}
	if !reflect.DeepEqual(ids, []string{"lead", "root-a", "root-a-d1-0", "review-a", "review-shared", "reduce"}) {
		t.Fatalf("scoped Run 混入其他事项或丢失共享证据: %+v", ids)
	}
	if len(scoped.PendingActions) != 1 || scoped.PendingActions[0].SourceTaskID != "root-a-d1-0" ||
		len(scoped.PendingQuestions) != 2 || scoped.PendingQuestions[0].TaskID != "root-a-d1-0" {
		t.Fatalf("动作与问题未按归属过滤: %+v %+v", scoped.PendingActions, scoped.PendingQuestions)
	}
}

func TestIssueTaskAttributionFallbackAndAmbiguity(t *testing.T) {
	issue := &Issue{ID: "issue-a", TeamID: "team"}
	run := &Run{ID: "run", TeamID: "team", Tasks: []Task{{ID: "lead", ExecutionContext: ExecutionContextLeadControl}, {ID: "lead-d1-0"}}}
	single := &TeamInput{ID: "single", TeamID: "team", Revision: 1, RunIDs: []string{"run"},
		IssueLinks: []IssueInputLink{{IssueID: issue.ID, InputID: "single", Revision: 1}},
		WorkItems:  []WorkItem{{ID: "work", RunID: "run", TaskIDs: []string{"lead"}}}}
	attribution := IssueTaskAttribution(issue, run, []*TeamInput{single})
	if !attribution.Known || !reflect.DeepEqual(attribution.TaskIDs, []string{"lead", "lead-d1-0"}) {
		t.Fatalf("单事项机械路由应回溯派生任务: %+v", attribution)
	}
	ambiguous := single.Clone()
	ambiguous.IssueLinks = append(ambiguous.IssueLinks, IssueInputLink{IssueID: "issue-b", InputID: "single", Revision: 1})
	attribution = IssueTaskAttribution(issue, run, []*TeamInput{ambiguous})
	if attribution.Known || len(attribution.TaskIDs) != 0 || !reflect.DeepEqual(attribution.SharedTaskIDs, []string{"lead", "lead-d1-0"}) {
		t.Fatalf("共享原话的无区间任务不得重复计入事项: %+v", attribution)
	}
}

func TestIssueGoalEvidenceIgnoresOtherIssueBranches(t *testing.T) {
	teamRow := reviewTestTeam()
	issueA, issueB := &Issue{ID: "issue-a", TeamID: teamRow.ID}, &Issue{ID: "issue-b", TeamID: teamRow.ID}
	input := &TeamInput{ID: "input", TeamID: teamRow.ID, Revision: 1, RunIDs: []string{"run"}, IssueLinks: []IssueInputLink{
		{IssueID: issueA.ID, InputID: "input", Revision: 1, SourceStart: 0, SourceEnd: 4},
		{IssueID: issueB.ID, InputID: "input", Revision: 1, SourceStart: 4, SourceEnd: 8},
	}, WorkItems: []WorkItem{
		{ID: "item-a", RunID: "run", SourceRevision: 1, SourceStart: 0, SourceEnd: 4, TaskIDs: []string{"impl-a"}},
		{ID: "item-b", RunID: "run", SourceRevision: 1, SourceStart: 4, SourceEnd: 8, TaskIDs: []string{"impl-b"}},
	}}
	run := &Run{ID: "run", TeamID: teamRow.ID, Status: RunCompleted, Tasks: []Task{
		{ID: "impl-a", MemberID: "dev", Status: TaskCompleted, Result: "事项 A 结果"},
		{ID: "review-a", MemberID: "rev", Review: reviewFlag(true), ReviewOf: []string{"impl-a"}, Status: TaskCompleted,
			HeadRevision: "head", DiffHash: "diff", ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictPassed}},
		{ID: "impl-b", MemberID: "dev", Status: TaskCompleted, Result: "事项 B 结果"},
		{ID: "review-b", MemberID: "rev", Review: reviewFlag(true), ReviewOf: []string{"impl-b"}, Status: TaskCompleted,
			HeadRevision: "head", DiffHash: "diff", ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictFailed}},
	}}
	scopeA := NewGoalScope(issueA.ID, "事项 A")
	contractA := DefaultGoalContract(scopeA)
	contractA.Review.Requirement = "required"
	outA := EvaluateIssueGoal(issueA, run, []*TeamInput{input}, teamRow, scopeA, &GoalRecord{Scope: scopeA, Contract: contractA})
	if !outA.CanConfirm || outA.Contract.Review.Status != "passed" || outA.Result == nil || outA.Result.Text != "事项 A 结果" || outA.Result.IncludesShared {
		t.Fatalf("事项 B 的失败审查或结果不应影响 A: %+v", outA)
	}
	scopeB := NewGoalScope(issueB.ID, "事项 B")
	contractB := DefaultGoalContract(scopeB)
	contractB.Review.Requirement = "required"
	outB := EvaluateIssueGoal(issueB, run, []*TeamInput{input}, teamRow, scopeB, &GoalRecord{Scope: scopeB, Contract: contractB})
	if outB.CanConfirm || outB.Contract.Review.Status != "failed" {
		t.Fatalf("事项 B 应保留自己的失败审查: %+v", outB)
	}
	scopedA, _ := RunForIssue(issueA, run, []*TeamInput{input})
	before := GoalEvidenceHash(scopedA)
	run.Tasks[2].Result = "事项 B 新结果"
	scopedA, _ = RunForIssue(issueA, run, []*TeamInput{input})
	if after := GoalEvidenceHash(scopedA); after != before {
		t.Fatalf("其他事项分支变化不应改变 A 的证据哈希: %s -> %s", before, after)
	}

	sharedRun := run.Clone()
	sharedRun.Tasks = append(sharedRun.Tasks, Task{ID: "shared-result", ExecutionContext: ExecutionContextLeadControl,
		Reduction: true, Status: TaskCompleted, Result: "共享汇总"})
	outShared := EvaluateIssueGoal(issueA, sharedRun, []*TeamInput{input}, teamRow, scopeA, &GoalRecord{Scope: scopeA, Contract: contractA})
	if outShared.Result == nil || !outShared.Result.IncludesShared {
		t.Fatalf("回退到共享输出时应显式标注: %+v", outShared.Result)
	}
}
