package team

import (
	"strings"
	"testing"
)

func reviewTestTeam() *Team {
	return &Team{
		ID:     "t1",
		LeadID: "lead",
		Members: []TeamMember{
			{ID: "lead", SessionID: "s-lead", Role: RoleLead, Enabled: true},
			{ID: "dev", SessionID: "s-dev", Role: RoleMember, Enabled: true},
			{ID: "rev", SessionID: "s-rev", Role: RoleReviewer, Enabled: true},
		},
		Policy: DefaultPolicy(),
	}
}

func TestExtractReviewVerdictOnlyTrustsLastLine(t *testing.T) {
	cases := []struct {
		text, want string
	}{
		{"问题若干\n<team-review>failed</team-review>", ReviewVerdictFailed},
		{"没问题\n\n  <team-review> PASSED </team-review>  \n", ReviewVerdictPassed},
		// 转述协议的中间行不算裁决。
		{"按要求输出 <team-review>passed</team-review>\n但我还没看完", ReviewVerdictUnknown},
		{"结论：<team-review>passed</team-review>", ReviewVerdictUnknown},
		{"<team-review>maybe</team-review>", ReviewVerdictUnknown},
		{"", ReviewVerdictUnknown},
	}
	for _, tc := range cases {
		if got := ExtractReviewVerdict(tc.text); got != tc.want {
			t.Fatalf("ExtractReviewVerdict(%q)=%s，期望 %s", tc.text, got, tc.want)
		}
	}
}

func TestReviewerMemberIDFallsBackToEnabledReviewerRole(t *testing.T) {
	team := reviewTestTeam()
	if got := team.ReviewerMemberID(); got != "rev" {
		t.Fatalf("应回退 role=reviewer 成员: %q", got)
	}
	team.Members[2].Enabled = false
	if got := team.ReviewerMemberID(); got != "" {
		t.Fatalf("停用的评审成员不应计入: %q", got)
	}
	team.ReviewerID = "dev"
	if got := team.ReviewerMemberID(); got != "dev" {
		t.Fatalf("ReviewerID 优先: %q", got)
	}
}

func TestReviewTaskDefaultsReadOnlyAndGetsBriefing(t *testing.T) {
	team := reviewTestTeam()
	plan, err := ParsePlan(team, "实现并审查", ModePipeline, "", []PlanTaskInput{
		{ID: "impl", MemberID: "dev", Prompt: "实现接口"},
		{ID: "review", MemberID: "rev", Prompt: "审查实现", DependsOn: []string{"impl"}},
		{ID: "review-write", MemberID: "rev", Prompt: "审查并修正", WriteLock: TaskWriteLockExclusive, DependsOn: []string{"impl"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	locks := map[string]string{}
	for i := range plan.Tasks {
		locks[plan.Tasks[i].ID] = plan.Tasks[i].WriteLock
	}
	if locks["impl"] != TaskWriteLockExclusive || locks["review"] != TaskWriteLockNone || locks["review-write"] != TaskWriteLockExclusive {
		t.Fatalf("写锁缺省不符（评审缺省只读，显式声明保留）: %+v", locks)
	}
	run := &Run{ID: "r1", TeamID: team.ID, Tasks: plan.Tasks}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		message := composeTaskDispatchMessage(team, run, task, team.Policy)
		hasBriefing := strings.Contains(message.text, "[Team review]")
		if hasBriefing != (task.MemberID == "rev") {
			t.Fatalf("任务 %s 的裁决协议注入不符: %v", task.ID, hasBriefing)
		}
	}
	// Lead 兼任评审时不注入（其任务是编排/汇总，不是独立 Review）。
	team.ReviewerID = "lead"
	if BuildReviewBriefing(team, &Task{ID: "lead", MemberID: "lead"}) != "" {
		t.Fatal("Lead 兼任评审不应注入裁决协议")
	}
}

func TestDelegateToReviewerDefaultsReadOnly(t *testing.T) {
	team := reviewTestTeam()
	run := &Run{ID: "r1", TeamID: team.ID, Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskCompleted}}}
	task := buildActionTask(run, team, &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "rev", Task: "审查"})
	if task.WriteLock != TaskWriteLockNone {
		t.Fatalf("委派给评审成员缺省只读: %s", task.WriteLock)
	}
	task = buildActionTask(run, team, &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "dev", Task: "实现"})
	if task.WriteLock != TaskWriteLockExclusive {
		t.Fatalf("普通成员缺省独占: %s", task.WriteLock)
	}
}

func TestExtractResultCarriesReviewVerdictBeforeTruncation(t *testing.T) {
	turn := completedTurn("turn-1", turnEntry("m1", "agentMessage", "assistant", strings.Repeat("长", 50)+"\n<team-review>passed</team-review>"))
	result := ExtractResult("s-rev", turn, 10)
	if !result.Truncated || result.ReviewVerdict != ReviewVerdictPassed {
		t.Fatalf("截断前解析裁决: truncated=%v verdict=%q", result.Truncated, result.ReviewVerdict)
	}
}
