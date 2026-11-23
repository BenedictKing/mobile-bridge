package team

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/textutil"
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

// 派发文本经会话标题清洗（textutil.TitleFromText）后应是真实任务：钉住
// textutil 里复制的信封字面量与本包常量同步。
func TestDispatchTextTitleIsAssignedTask(t *testing.T) {
	team := reviewTestTeam()
	run := &Run{ID: "r1", TeamID: team.ID, AutoPlan: true, Tasks: []Task{
		{ID: "lead", MemberID: "lead", Prompt: "实现头像上传"},
		{ID: "review", MemberID: "rev", Prompt: "审查头像上传"},
		{ID: "impl", MemberID: "dev", Prompt: "写接口", DependsOn: []string{"lead"}},
	}}
	run.Tasks[0].Status, run.Tasks[0].Result = TaskCompleted, "规划完成"
	for i, want := range []string{"实现头像上传", "审查头像上传", "写接口"} {
		text := composeTaskDispatchMessage(team, run, &run.Tasks[i], team.Policy).text
		if got := textutil.TitleFromText(text); got != want {
			t.Fatalf("任务 %s 的派发文本标题应为 %q，得到 %q\n%s", run.Tasks[i].ID, want, got, text)
		}
		if strings.Count(text, assignedTaskHeader) != 1 {
			t.Fatalf("任务 %s 的派发文本应恰有一个 [Assigned task] 头:\n%s", run.Tasks[i].ID, text)
		}
	}
}

func TestReviewerExecutionAndReviewAreDistinct(t *testing.T) {
	team := reviewTestTeam()
	team.ReviewerID = "rev"
	plan, err := ParsePlan(team, "实现并审查", ModePipeline, "", []PlanTaskInput{
		{ID: "impl", MemberID: "rev", Prompt: "实现接口", Review: reviewFlag(false)},
		{ID: "review", MemberID: "rev", Prompt: "独立审查", DependsOn: []string{"impl"}, Review: reviewFlag(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := &Run{ID: "r1", TeamID: team.ID, Tasks: plan.Tasks}
	execution := &run.Tasks[0]
	review := &run.Tasks[1]
	if execution.WriteLock != TaskWriteLockExclusive || review.WriteLock != TaskWriteLockNone {
		t.Fatalf("执行/审查写锁混淆: execution=%s review=%s", execution.WriteLock, review.WriteLock)
	}
	if IsReviewTask(team, execution) || !IsReviewTask(team, review) {
		t.Fatal("review 标记应区分执行任务与审查任务")
	}
	if got := composeTaskDispatchMessage(team, run, execution, team.Policy).text; strings.Contains(got, "[Team review]") {
		t.Fatalf("执行任务不得注入审查协议:\n%s", got)
	}
	if got := composeTaskDispatchMessage(team, run, review, team.Policy).text; !strings.Contains(got, "[Team review]") ||
		!strings.Contains(got, "「阻塞问题」只收能写出具体触发场景") || !strings.Contains(got, "没有阻塞问题时写 <team-review>passed</team-review>") {
		t.Fatalf("审查任务缺少分级裁决协议:\n%s", got)
	}
	action := &Action{Type: ActionTypeDelegate, SourceTaskID: "lead", Round: 1, Member: "rev", Task: "实现", Review: reviewFlag(false)}
	if task := buildActionTask(run, team, action); task.WriteLock != TaskWriteLockExclusive || IsReviewTask(team, &task) {
		t.Fatalf("review=false 的动作应生成普通执行任务: %+v", task)
	}
}

func TestReviewFlagValidationAndActionParsing(t *testing.T) {
	team := reviewTestTeam()
	team.ReviewerID = "rev"
	if _, err := ParsePlan(team, "错误审查", ModeSingle, "", []PlanTaskInput{
		{ID: "review", MemberID: "dev", Prompt: "审查", Review: reviewFlag(true)},
	}); err == nil || !strings.Contains(err.Error(), "只能指派给独立的默认评审成员") {
		t.Fatalf("非默认评审的 review=true 应拒绝: %v", err)
	}
	run := &Run{ID: "r1", TeamID: team.ID, Tasks: []Task{{ID: "lead", MemberID: "lead", Status: TaskCompleted}}}
	actions, violations := ParseLeadActions(`<team-actions>{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"rev","task":"实现","review":false}]}</team-actions>`, team, run, team.Policy)
	if len(violations) != 0 || len(actions) != 1 || actions[0].Review == nil || *actions[0].Review {
		t.Fatalf("review=false 解析失败: actions=%+v violations=%v", actions, violations)
	}
	_, violations = ParseLeadActions(`<team-actions>{"actions":[{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"审查","review":true}]}</team-actions>`, team, run, team.Policy)
	if len(violations) == 0 {
		t.Fatal("非默认评审的 review=true 动作应拒绝")
	}
}

func TestReviewFlagSurvivesSnapshotCloneAndJournalRebuild(t *testing.T) {
	original := &Task{ID: "impl", MemberID: "rev", Review: reviewFlag(false)}
	raw, err := original.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded Task
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Review == nil || *decoded.Review {
		t.Fatalf("快照丢失 review=false: %s", raw)
	}
	clone := decoded.Clone()
	*clone.Review = true
	if *decoded.Review {
		t.Fatal("Clone 不应共享 review 指针")
	}
	run := &Run{}
	applyJournalEventToRun(run, newEvent("r1", EventTaskPlanned, "runner", map[string]any{
		"taskId": "impl", "memberId": "rev", "review": false,
	}))
	if len(run.Tasks) != 1 || run.Tasks[0].Review == nil || *run.Tasks[0].Review {
		t.Fatalf("journal 重建丢失 review=false: %+v", run.Tasks)
	}
}
