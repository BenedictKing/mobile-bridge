package team

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestGoalWorkflowFeedbackRepairSubmitsComplete(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "完整交付已实现并验证")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	completeGoalEntry(h, check, checkEntry, "缺少反馈协议")
	repair, repairEntry := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-1")
	completeGoalEntry(h, repair, repairEntry, "仍缺少反馈协议")
	repair2, repairEntry2 := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-2")
	for _, id := range []string{a.ID, check.ID, repair.ID} {
		if !containsString(repair2.DependsOn, id) {
			t.Fatalf("连续纠错丢失原评估快照或纠错事实 %s: %v", id, repair2.DependsOn)
		}
	}
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
	restored := findTaskIn(rebuilt.Run(run.ID), repair2.ID)
	if restored == nil || !reflect.DeepEqual(restored.DependsOn, repair2.DependsOn) {
		t.Fatalf("journal 重建丢失纠错快照: %+v", restored)
	}
	feedback := &planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{a.ID}}}}
	for _, event := range events {
		id := payloadString(event.Payload, "taskId")
		if event.Type == EventTaskPlanned && payloadBool(event.Payload, "goalFeedback") && actionRepairRoot(id) != id {
			delete(event.Payload, "dependsOn")
		}
	}
	legacyStore := LoadStore(filepath.Join(t.TempDir(), "teams.json"), filepath.Join(t.TempDir(), "runs.json"))
	teamCopy.Version = 0
	if err := legacyStore.SaveTeam(teamCopy); err != nil {
		t.Fatal(err)
	}
	legacyStore.runsCorrupt = true
	legacyStore.RebuildRunsFromJournal(events)
	legacyRun := legacyStore.Run(run.ID)
	legacyTask := findTaskIn(legacyRun, repair2.ID)
	if legacyTask == nil || len(legacyTask.DependsOn) != 0 {
		t.Fatal("旧 journal 应恢复无依赖的纠错事实")
	}
	legacyView := goalFeedbackWithSnapshot(legacyRun, legacyTask)
	if _, _, violations := parsePlanningReply(workflowReply(nil, feedback), h.team, legacyRun, legacyView, h.team.Policy); len(violations) > 0 {
		t.Fatalf("旧 journal 的连续纠错必须沿原评估快照恢复: %v", violations)
	}
	completeGoalEntry(h, repair2, repairEntry2, workflowReply(nil, feedback))
	fresh := waitGoalStatus(t, h, run.ID, RunCompleted)
	if fresh.Planning.Status != "complete" || fresh.Planning.Iteration != 1 {
		t.Fatalf("有效纠错必须提交验收且不消耗新评估轮次: %+v", fresh.Planning)
	}
}

func TestGoalWorkflowFeedbackRepairSubmitsContinue(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "阶段交付")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	completeGoalEntry(h, check, checkEntry, "缺少反馈协议")
	repair, repairEntry := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-1")
	feedback := &planningFeedback{Status: "continue", Checks: []PlanningCheck{{ID: "all", State: "unmet", EvidenceTaskIDs: []string{a.ID}}}, Remaining: []string{"补齐交付"}}
	completeGoalEntry(h, repair, repairEntry, workflowReply(nil, feedback, workflowAction(repair.ID, "a")))
	work, workEntry := waitGoalTaskEntry(t, h, run.ID, repair.ID+"-d1-0")
	completeGoalEntry(h, work, workEntry, "完整交付并验证")
	check2, checkEntry2 := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-2")
	feedback = &planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{work.ID}}}}
	completeGoalEntry(h, check2, checkEntry2, workflowReply(nil, feedback))
	if fresh := waitGoalStatus(t, h, run.ID, RunCompleted); fresh.Planning.Iteration != 2 {
		t.Fatalf("纠错 continue 必须实际派发且仅计一轮评估: %+v", fresh.Planning)
	}
}

func TestGoalWorkflowFeedbackRepairRejectsNewExecution(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "legacy"}[legacy], func(t *testing.T) {
			h, _, run := newGoalWorkflowRun(t)
			startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
			a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
			completeGoalEntry(h, a, entry, "原交付完成")
			check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
			completeGoalEntry(h, check, checkEntry, "缺少反馈协议")
			repair, repairEntry := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-1")
			if legacy {
				clearGoalRepairSnapshot(t, h, run.ID, repair.ID)
			}
			if _, _, err := h.service.FollowUpConversationRun(h.team, run.ID, "new-execution", "补充实际工作", nil, a.ID); err != nil {
				t.Fatal(err)
			}
			work, workEntry := waitGoalTaskEntry(t, h, run.ID, "new-execution")
			feedback := &planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{a.ID}}}}
			completeGoalEntry(h, repair, repairEntry, workflowReply(nil, feedback))
			completeGoalEntry(h, work, workEntry, "补充工作完成")
			check2, _ := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-2")
			fresh := h.store.Run(run.ID)
			if fresh.Planning.Status == "complete" || findTaskIn(fresh, "actfix-lead-goal-check-1-2") != nil || !containsString(check2.DependsOn, work.ID) {
				t.Fatalf("旧纠错不得关闭目标或吸收新增执行任务: %+v", fresh.Planning)
			}
		})
	}
}

func clearGoalRepairSnapshot(t *testing.T, h *dispatchHarness, runID, taskID string) {
	t.Helper()
	state := h.service.stateFor(runID)
	state.mu.Lock()
	err := h.service.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		findTaskIn(fresh, taskID).DependsOn = nil
		return nil, true
	})
	state.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestGoalWorkflowLegacyFeedbackRepairSubmitsComplete(t *testing.T) {
	h, _, run := newGoalWorkflowRun(t)
	startGoalBranches(t, h, run, goalTestPlan(), workflowAction("lead", "a"))
	a, entry := waitGoalTaskEntry(t, h, run.ID, "lead-d1-0")
	completeGoalEntry(h, a, entry, "完整交付并验证")
	check, checkEntry := waitGoalTaskEntry(t, h, run.ID, "lead-goal-check-1")
	completeGoalEntry(h, check, checkEntry, "缺少反馈协议")
	repair, repairEntry := waitGoalTaskEntry(t, h, run.ID, "actfix-lead-goal-check-1-1")
	clearGoalRepairSnapshot(t, h, run.ID, repair.ID)
	feedback := &planningFeedback{Status: "complete", Checks: []PlanningCheck{{ID: "all", State: "met", EvidenceTaskIDs: []string{a.ID}}}}
	completeGoalEntry(h, repair, repairEntry, workflowReply(nil, feedback))
	if fresh := waitGoalStatus(t, h, run.ID, RunCompleted); fresh.Planning.Iteration != 1 {
		t.Fatalf("旧版本纠错必须沿原快照提交，不消耗新评估轮次: %+v", fresh.Planning)
	}
}
