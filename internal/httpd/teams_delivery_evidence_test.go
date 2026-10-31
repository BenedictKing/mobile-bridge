package httpd

// teams_delivery_evidence_test.go 覆盖交付证据投影（docs/team-overall-design.md
// §8 阶段三）：Agent 完成度、Review 裁决、验证闸门与结果摘要四类证据在
// Run 详情的 delivery 块中分别落位。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

type deliveryEvidenceView struct {
	RunID string `json:"runId"`
	Agent struct {
		Completed int `json:"completed"`
		Total     int `json:"total"`
	} `json:"agent"`
	ResultSummary struct {
		Text      string `json:"text"`
		Truncated bool   `json:"truncated"`
	} `json:"resultSummary"`
	Review *struct {
		TaskID  string `json:"taskId"`
		Verdict string `json:"verdict"`
	} `json:"review"`
	Verify struct {
		Configured bool   `json:"configured"`
		ExitCode   int    `json:"exitCode"`
		Status     string `json:"status"`
	} `json:"verify"`
	PendingQuestions int      `json:"pendingQuestions"`
	TaskIds          []string `json:"taskIds"`
	Workspaces       []struct {
		WorkspaceID string `json:"workspaceId"`
		TaskID      string `json:"taskId"`
	} `json:"workspaces"`
}

func fetchDeliveryEvidence(t *testing.T, env *teamTestEnv, teamID, runID string) deliveryEvidenceView {
	t.Helper()
	response := env.request(http.MethodGet, "/api/teams/"+teamID+"/runs/"+runID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("读取 Run 详情失败: %d %s", response.Code, response.Body.String())
	}
	var detail struct {
		Delivery deliveryEvidenceView `json:"delivery"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatalf("Run 详情解析失败: %v\n%s", err, response.Body.String())
	}
	return detail.Delivery
}

func TestRunDetailDeliveryEvidenceDimensions(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	// 团队补评审成员（ReviewerID 指向 role=reviewer 成员）。
	teamRow := env.store.Team(teamID)
	teamRow.ReviewerID = "rev"
	teamRow.Members = append(teamRow.Members, team.TeamMember{
		ID: "rev", SessionID: "sess-rev", AgentKind: "zcode", Role: team.RoleReviewer, Enabled: true,
	})
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{
		ID: "run-evidence", TeamID: teamID, Goal: "交付证据", Status: team.RunCompleted,
		VerifyCommand: []string{"make", "check"},
		Verify:        &team.VerifyResult{ExitCode: 0},
		Tasks: []team.Task{
			{ID: "impl", MemberID: "lead", Status: team.TaskCompleted, Result: "接口完成", Ordinal: 0},
			{ID: "rev", MemberID: "rev", Status: team.TaskCompleted, Result: "审查通过", Ordinal: 1,
				ResultDetail: &team.TaskResult{SessionID: "sess-rev", Result: "审查通过", ReviewVerdict: team.ReviewVerdictPassed}},
			{ID: "iso", MemberID: "lead", Status: team.TaskCompleted, Result: "分支产出", Ordinal: 2,
				WorkspaceMode: team.WorkspaceIsolatedWorktree, WorkspaceID: "ws-1", BaseRevision: "abc123"},
		},
	}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	evidence := fetchDeliveryEvidence(t, env, teamID, run.ID)
	if evidence.RunID != run.ID {
		t.Fatalf("runId 应指向本 Run: %+v", evidence)
	}
	if evidence.Agent.Completed != 3 || evidence.Agent.Total != 3 {
		t.Fatalf("Agent 完成度应 3/3: %+v", evidence.Agent)
	}
	if evidence.Review == nil || evidence.Review.TaskID != "rev" || evidence.Review.Verdict != "passed" {
		t.Fatalf("Review 裁决应通过且指向 rev: %+v", evidence.Review)
	}
	if !evidence.Verify.Configured || evidence.Verify.Status != "passed" || evidence.Verify.ExitCode != 0 {
		t.Fatalf("验证闸门应已通过: %+v", evidence.Verify)
	}
	if !strings.Contains(evidence.ResultSummary.Text, "接口完成") || !strings.Contains(evidence.ResultSummary.Text, "审查通过") {
		t.Fatalf("结果摘要应含各任务结果头: %q", evidence.ResultSummary.Text)
	}
	if evidence.ResultSummary.Truncated {
		t.Fatal("短结果不应截断")
	}
	if len(evidence.TaskIds) != 3 || evidence.PendingQuestions != 0 {
		t.Fatalf("任务清单/问题数不符: %+v", evidence)
	}
	if len(evidence.Workspaces) != 1 || evidence.Workspaces[0].WorkspaceID != "ws-1" || evidence.Workspaces[0].TaskID != "iso" {
		t.Fatalf("worktree 引用不符: %+v", evidence.Workspaces)
	}
}

func TestRunDetailDeliveryEvidenceFailuresAndAbsence(t *testing.T) {
	env := newTeamTestEnv(t, "queued", true)
	teamID, _ := env.createTeam(t)
	// 团队无评审成员：delivery 不带 review 键；失败任务/闸门分别落 failed。
	teamRow := env.store.Team(teamID)
	teamRow.ReviewerID = "rev"
	teamRow.Members = append(teamRow.Members, team.TeamMember{
		ID: "rev", SessionID: "sess-rev", AgentKind: "zcode", Role: team.RoleReviewer, Enabled: true,
	})
	if err := env.store.SaveTeam(teamRow); err != nil {
		t.Fatal(err)
	}
	run := &team.Run{
		ID: "run-ev-fail", TeamID: teamID, Goal: "失败形态", Status: team.RunCompleted,
		VerifyCommand: []string{"make", "check"},
		Verify:        &team.VerifyResult{ExitCode: 2},
		Tasks: []team.Task{
			{ID: "impl", MemberID: "lead", Status: team.TaskCompleted, Result: "实现完成", Ordinal: 0},
			{ID: "rev", MemberID: "rev", Status: team.TaskFailed, Error: "发现问题", Ordinal: 1},
		},
	}
	if err := env.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	evidence := fetchDeliveryEvidence(t, env, teamID, run.ID)
	// 评审任务执行失败不等于审查不通过：裁决未知。
	if evidence.Review == nil || evidence.Review.Verdict != team.ReviewVerdictUnknown {
		t.Fatalf("执行失败的评审任务裁决应为 unknown: %+v", evidence.Review)
	}
	if evidence.Verify.Status != "failed" || evidence.Verify.ExitCode != 2 {
		t.Fatalf("非零退出码应落 failed: %+v", evidence.Verify)
	}
	if evidence.Agent.Completed != 1 || evidence.Agent.Total != 2 {
		t.Fatalf("完成度应 1/2: %+v", evidence.Agent)
	}
	if strings.Contains(evidence.ResultSummary.Text, "rev：") {
		t.Fatalf("失败任务不进结果摘要: %q", evidence.ResultSummary.Text)
	}

	// Lead 结果只有动作信封：不进摘要（协议 JSON 不是给人读的结果）；正文与
	// 信封混排时只留正文。
	envelope := "<team-actions>\n{\"actions\":[{\"type\":\"delegate\",\"member\":\"rev\",\"task\":\"审查\"}]}\n</team-actions>"
	actionRun := &team.Run{
		ID: "run-ev-actions", TeamID: teamID, Goal: "动作信封", Status: team.RunCompleted,
		Tasks: []team.Task{
			{ID: "lead", MemberID: "lead", Status: team.TaskCompleted, Result: envelope, Ordinal: 0},
			{ID: "lead-d1-0", MemberID: "lead", Status: team.TaskCompleted, Result: "汇总如下\n" + envelope, Ordinal: 1},
		},
	}
	if err := env.store.SaveRun(actionRun); err != nil {
		t.Fatal(err)
	}
	actionEvidence := fetchDeliveryEvidence(t, env, teamID, actionRun.ID)
	if strings.Contains(actionEvidence.ResultSummary.Text, "team-actions") || strings.Contains(actionEvidence.ResultSummary.Text, "lead：") {
		t.Fatalf("动作信封不应进入结果摘要: %q", actionEvidence.ResultSummary.Text)
	}
	if actionEvidence.ResultSummary.Text != "lead-d1-0：汇总如下" {
		t.Fatalf("混排结果应只留正文: %q", actionEvidence.ResultSummary.Text)
	}
	if actionEvidence.ResultSummary.Truncated {
		t.Fatal("短结果不应标截断")
	}

	// Markdown 结果折成纯文本：强调符、反引号、标题与列表符号不进摘要，标识符里的下划线保留。
	markdownRun := &team.Run{
		ID: "run-ev-markdown", TeamID: teamID, Goal: "Markdown", Status: team.RunCompleted,
		Tasks: []team.Task{{ID: "impl", MemberID: "lead", Status: team.TaskCompleted, Ordinal: 0,
			Result: "## 结论\n**结论：TEAM_OK。**\n- 改动：`web/app.js`\n- 遗留：无"}},
	}
	if err := env.store.SaveRun(markdownRun); err != nil {
		t.Fatal(err)
	}
	if got := fetchDeliveryEvidence(t, env, teamID, markdownRun.ID).ResultSummary.Text; got != "impl：结论 结论：TEAM_OK。 改动：web/app.js 遗留：无" {
		t.Fatalf("摘要应是去 Markdown 标记的单行纯文本: %q", got)
	}

	// 单任务结果超过 120 字：逐任务头被截，摘要必须标 truncated。
	longRun := &team.Run{
		ID: "run-ev-long", TeamID: teamID, Goal: "长结果", Status: team.RunCompleted,
		Tasks: []team.Task{{ID: "impl", MemberID: "lead", Status: team.TaskCompleted, Result: strings.Repeat("长", 130), Ordinal: 0}},
	}
	if err := env.store.SaveRun(longRun); err != nil {
		t.Fatal(err)
	}
	if long := fetchDeliveryEvidence(t, env, teamID, longRun.ID); !long.ResultSummary.Truncated {
		t.Fatalf("逐任务头被截时摘要应标 truncated: %+v", long.ResultSummary)
	}

	// 评审任务 completed：无裁决标记 → unknown（Agent 完成不替代 Review 通过）；
	// 显式 failed 裁决 → failed。
	for _, tc := range []struct {
		id, verdict, want string
	}{
		{"run-ev-noverdict", "", team.ReviewVerdictUnknown},
		{"run-ev-rejected", team.ReviewVerdictFailed, team.ReviewVerdictFailed},
	} {
		verdictRun := &team.Run{
			ID: tc.id, TeamID: teamID, Goal: "裁决", Status: team.RunCompleted,
			Tasks: []team.Task{{ID: "rev", MemberID: "rev", Status: team.TaskCompleted, Result: "看过了", Ordinal: 0,
				ResultDetail: &team.TaskResult{Result: "看过了", ReviewVerdict: tc.verdict}}},
		}
		if err := env.store.SaveRun(verdictRun); err != nil {
			t.Fatal(err)
		}
		got := fetchDeliveryEvidence(t, env, teamID, tc.id)
		if got.Review == nil || got.Review.Verdict != tc.want {
			t.Fatalf("%s 裁决应为 %s: %+v", tc.id, tc.want, got.Review)
		}
	}

	// 无评审成员的团队：不带 review 键；未配置闸门：verify 只有 configured=false。
	bareTeam := "00000000-0000-4000-8000-0000000000aa"
	other := env.store.Team(teamID)
	other.ID = bareTeam
	other.Version = 0
	other.ReviewerID = ""
	if err := env.store.SaveTeam(other); err != nil {
		t.Fatal(err)
	}
	bareRun := &team.Run{
		ID: "run-ev-bare", TeamID: bareTeam, Goal: "裸交付", Status: team.RunCompleted,
		Tasks: []team.Task{{ID: "solo", MemberID: "lead", Status: team.TaskCompleted, Result: "完成", Ordinal: 0}},
	}
	if err := env.store.SaveRun(bareRun); err != nil {
		t.Fatal(err)
	}
	bare := fetchDeliveryEvidence(t, env, bareTeam, bareRun.ID)
	if bare.Review != nil {
		t.Fatalf("无评审成员不应带 review 键: %+v", bare.Review)
	}
	if bare.Verify.Configured || bare.Verify.Status != "" {
		t.Fatalf("未配置闸门只有 configured=false: %+v", bare.Verify)
	}
	// 截断：结果引用标记 truncated 时摘要置 truncated。
	bareRun2 := &team.Run{
		ID: "run-ev-trunc", TeamID: bareTeam, Goal: "截断", Status: team.RunCompleted,
		Tasks: []team.Task{{ID: "big", MemberID: "lead", Status: team.TaskCompleted, Result: strings.Repeat("长", 200), Ordinal: 0,
			ResultDetail: &team.TaskResult{Result: strings.Repeat("长", 200), Truncated: true}}},
	}
	if err := env.store.SaveRun(bareRun2); err != nil {
		t.Fatal(err)
	}
	trunc := fetchDeliveryEvidence(t, env, bareTeam, bareRun2.ID)
	if !trunc.ResultSummary.Truncated {
		t.Fatal("结果引用被截断时摘要应置 truncated")
	}
	if len([]rune(trunc.ResultSummary.Text)) > 1200 {
		t.Fatalf("摘要应有界: %d 字", len([]rune(trunc.ResultSummary.Text)))
	}
}
