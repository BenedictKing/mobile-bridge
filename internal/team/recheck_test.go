package team

import "testing"

func TestRecheckUnknownRequiresMatchingCompletedTurn(t *testing.T) {
	for _, scenario := range []string{"completed", "unrelated", "running", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			h := newDispatchHarness(t)
			run := &Run{ID: "recheck", TeamID: h.team.ID, Status: RunOutcomeUnknown,
				Tasks: []Task{{ID: "dev-task", MemberID: "dev", Status: TaskOutcomeUnknown, SubmissionID: "sub", SentText: "original prompt"}}}
			if err := h.store.SaveRun(run); err != nil {
				t.Fatal(err)
			}
			text := "original prompt"
			if scenario == "unrelated" {
				text = "another task"
			}
			drive := turnCompleted(text, "verified result")
			if scenario == "running" {
				drive = turnInProgress(text)
			} else if scenario == "cancelled" {
				drive = turnInterrupted(text)
			}
			h.resolver.setSession("s-dev", newFakeSession("s-dev", "", drive))
			fresh, err := h.service.RecheckRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "completed" {
				if fresh.Status != RunWaitingInput || fresh.Tasks[0].Status != TaskCompleted || fresh.Tasks[0].Result != "verified result" {
					t.Fatalf("未正确恢复结果: %+v", fresh)
				}
			} else if fresh.Status != RunOutcomeUnknown || fresh.Tasks[0].Status != TaskOutcomeUnknown {
				t.Fatalf("无完成凭据不应改变状态: %+v", fresh)
			}
			if len(h.queue.calls) != 0 {
				t.Fatal("核查不得重发任务")
			}
		})
	}
}
