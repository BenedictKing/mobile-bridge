package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestActionPlanGateAllowsBatchPlanAndFeedsRepair(t *testing.T) {
	team := testTeam()
	run := batchDepsRunFixture()
	run.TeamID = team.ID
	missing := actionJSON(`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"实现","phase":"implement"}`)
	payload, _ := decodeActionPayload(missing)
	_, violations := ValidateActions(payload, team, run, actionsPolicyFixture())
	if len(violations) == 0 || !strings.Contains(strings.Join(violations, "；"), "phase=plan") {
		t.Fatalf("缺少计划依赖应被门禁拒绝: %+v", violations)
	}
	if prompt := RepairPrompt(violations, 1); !strings.Contains(prompt, "phase=plan") {
		t.Fatalf("门禁原因应进入 Lead 修复提示: %s", prompt)
	}

	inner := actionJSON(
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"先制定计划","key":"design","phase":"plan","writeLock":"none"}`,
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"dev","task":"按计划实现","dependsOn":["design"],"phase":"implement"}`,
	)
	payload, _ = decodeActionPayload(inner)
	actions, violations := ValidateActions(payload, team, run, actionsPolicyFixture())
	if len(violations) != 0 {
		t.Fatalf("同批 plan → implement 应通过: %+v", violations)
	}
	byKey, _ := validateActionBatchKeys(team, run, actions)
	built := buildActionTaskWithKeys(run, team, &actions[1], byKey)
	if built.Phase != TaskPhaseImplement || len(built.DependsOn) == 0 {
		t.Fatalf("Phase 或计划依赖未写入任务: %+v", built)
	}

	payload, _ = decodeActionPayload(missing)
	policy := actionsPolicyFixture()
	policy.AllowImplementationWithoutPlan = true
	if _, violations = ValidateActions(payload, team, run, policy); len(violations) != 0 {
		t.Fatalf("显式豁免应允许省略计划依赖: %+v", violations)
	}
	if got := NormalizeTaskPhase(strings.Repeat("x", 80)); len([]rune(got)) != 64 {
		t.Fatalf("Phase 应截断到 64 字: %d", len([]rune(got)))
	}
}

func TestParsePlanPhaseGateAndPersistence(t *testing.T) {
	team := testTeam()
	if _, err := ParsePlan(team, "实现", ModePipeline, "", []PlanTaskInput{{ID: "impl", MemberID: "dev", Prompt: "实现", Phase: "implement"}}); err == nil || !strings.Contains(err.Error(), "phase=plan") {
		t.Fatalf("显式计划缺少 plan 依赖应拒绝: %v", err)
	}
	plan, err := ParsePlan(team, "实现", ModePipeline, "", []PlanTaskInput{
		{ID: "design", MemberID: "dev", Prompt: "计划", Phase: "plan", WriteLock: TaskWriteLockNone},
		{ID: "impl", MemberID: "dev", Prompt: "实现", Phase: "implement", DependsOn: []string{"design"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tasks[0].Phase != TaskPhasePlan || plan.Tasks[1].Phase != TaskPhaseImplement {
		t.Fatalf("显式计划 Phase 未规范化保留: %+v", plan.Tasks)
	}
	withoutPhase := append([]Task(nil), plan.Tasks...)
	withoutPhase[1].Phase = ""
	if PlanHash(plan.Mode, plan.CollectPolicy, plan.Goal, withoutPhase) == plan.PlanHash {
		t.Fatal("不同 Phase 不应共享计划或缓存哈希")
	}
	raw, err := json.Marshal(plan.Tasks[1])
	if err != nil {
		t.Fatal(err)
	}
	var restored Task
	if err := json.Unmarshal(raw, &restored); err != nil || restored.Phase != TaskPhaseImplement {
		t.Fatalf("快照往返丢失 Phase: %+v err=%v", restored, err)
	}
	run := &Run{}
	applyJournalEventToRun(run, newEvent("run", EventTaskPlanned, "runner", map[string]any{"taskId": "impl", "phase": "implement"}))
	if len(run.Tasks) != 1 || run.Tasks[0].Phase != TaskPhaseImplement {
		t.Fatalf("journal 重建丢失 Phase: %+v", run.Tasks)
	}
}
