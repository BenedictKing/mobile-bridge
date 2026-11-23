package team

import (
	"strings"
	"testing"
)

func advisorTeam() *Team {
	team := testTeam()
	team.Members = append(team.Members, TeamMember{ID: "adv", SessionID: "s-adv", AgentKind: "gemini", Role: RoleAdvisor, Enabled: true})
	return team
}

// TestAdvisorTaskGetsReadOnlyBriefingAndUserOriginal 顾问任务注入只读协议与
// 用户原话（含追加输入），缺省只读；普通成员任务原文原样送达。
func TestAdvisorTaskGetsReadOnlyBriefingAndUserOriginal(t *testing.T) {
	team := advisorTeam()
	run := &Run{ID: "r", TeamID: team.ID, AutoPlan: true, Goal: "重做登录页", Tasks: []Task{
		{ID: "input-1", MemberID: "lead", ExecutionContext: ExecutionContextLeadControl, Prompt: "要支持扫码登录"},
		{ID: "adv-1", MemberID: "adv", Prompt: "给两套方案"},
		{ID: "dev-1", MemberID: "dev", Prompt: "实现方案 A"},
	}}
	advisor, dev := &run.Tasks[1], &run.Tasks[2]
	if !IsAdvisorTask(team, advisor) || IsAdvisorTask(team, dev) {
		t.Fatal("只有 role=advisor 成员的任务是顾问任务")
	}
	text := composeTaskDispatchMessage(team, run, advisor, team.Policy).text
	for _, want := range []string{"[Team advisor]", "不要修改文件", "推荐选项放在第一位", "[User original]", "重做登录页", "要支持扫码登录", "给两套方案"} {
		if !strings.Contains(text, want) {
			t.Fatalf("顾问派发文本缺少 %q:\n%s", want, text)
		}
	}
	if got := composeTaskDispatchMessage(team, run, dev, team.Policy).text; strings.Contains(got, "[Team advisor]") || strings.Contains(got, "[User original]") {
		t.Fatalf("普通成员任务不得注入顾问协议:\n%s", got)
	}
	if lock := defaultReviewWriteLock(team, "adv", ""); lock != TaskWriteLockNone {
		t.Fatalf("顾问任务缺省应只读，实际 %q", lock)
	}
	if lock := defaultReviewWriteLock(team, "dev", ""); lock != "" {
		t.Fatalf("普通成员不应被改为只读，实际 %q", lock)
	}
}

// TestLeadBriefingTeachesAdvisorOnlyWhenPresent 有顾问时 Lead 才收到顾问用法与汇总模板。
func TestLeadBriefingTeachesAdvisorOnlyWhenPresent(t *testing.T) {
	run := &Run{AutoPlan: true}
	task := &Task{ID: "lead", MemberID: "lead"}
	if got := BuildLeadBriefing(advisorTeam(), run, task, DefaultPolicy()); !strings.Contains(got, "role=advisor 的成员是只读方案顾问") ||
		!strings.Contains(got, "需用户拍板") || !strings.Contains(got, "role=advisor") {
		t.Fatalf("Lead 简报缺少顾问规则:\n%s", got)
	}
	if got := BuildLeadBriefing(testTeam(), run, task, DefaultPolicy()); strings.Contains(got, "只读方案顾问") {
		t.Fatal("没有顾问时不应注入顾问规则")
	}
}

// TestReadOnlyWriteNoteSurfacesInDeliveryAndEvaluationFacts 审查任务记录到
// 文件编辑时，交付事实与目标评估事实都要警示，且评估事实带上机器裁决。
func TestReadOnlyWriteNoteSurfacesInDeliveryAndEvaluationFacts(t *testing.T) {
	team := testTeam()
	review := Task{ID: "rev-1", MemberID: "rev", WorkflowID: "a", Status: TaskCompleted, Result: "发现问题",
		ResultDetail: &TaskResult{ReviewVerdict: ReviewVerdictPassed, FileSummary: "internal/x.go"}}
	run := &Run{ID: "r", AutoPlan: true, Tasks: []Task{review}}
	if got := BuildDeliveryBriefing(team, run); !strings.Contains(got, "该只读任务记录到文件编辑（internal/x.go）") {
		t.Fatalf("交付事实应警示审查任务写入:\n%s", got)
	}
	run.Planning = &GoalPlanningState{Criteria: []PlanningCriterion{{ID: "all", Text: "完成"}}, MaxIterations: 3}
	feedback := &Task{ID: "lead-goal-check-1", MemberID: "lead", GoalFeedback: true, Reduction: true, ExecutionContext: ExecutionContextLeadControl}
	got := goalPlanningBriefing(team, run, feedback)
	if !strings.Contains(got, "reviewVerdict=passed") || !strings.Contains(got, "该只读任务记录到文件编辑") {
		t.Fatalf("评估事实应带裁决与只读警示:\n%s", got)
	}
	run.Tasks[0].ResultDetail.FileSummary = ""
	if got := BuildDeliveryBriefing(team, run); strings.Contains(got, "记录到文件编辑") {
		t.Fatal("没有文件编辑时不应警示")
	}
}
