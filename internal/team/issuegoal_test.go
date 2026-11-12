package team

import (
	"errors"
	"testing"
	"time"
)

func TestIssueGoalPersistsReplaysAndDeletesWithIssue(t *testing.T) {
	ledger, snapshot, journalPath := newInputLedgerFixture(t)
	input, _, err := ledger.AcceptWithIssueRouting("team-1", "input-goal", "完成事项", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ledger.ApplyIssueProposal("team-1", "goal-proposal", &IssueProposal{InputID: input.ID, Revision: 1, Segments: []IssueProposalSegment{{
		Action: IssueSegmentCreate, NewIssueTitle: "事项", SourceStart: 0, SourceEnd: 4,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	issue := ledger.ListIssues("team-1")[0]
	scope := NewGoalScope(issue.ID, issue.Title)
	contract := DefaultGoalContract(scope)
	contract.Criteria[0].State = "met"
	record := &GoalRecord{Scope: scope, Contract: contract, EvidenceHash: "run-evidence", WorkspaceHead: "head-1", WorkspaceDiff: "diff-1",
		AssessedBy: "user", AssessedAt: time.Now(), Reason: "已确认", Operations: map[string]string{"op-1": "fingerprint-1"}}
	if err := ledger.ApplyIssueGoal("team-1", "run-1", record); err != nil {
		t.Fatal(err)
	}
	if err := ledger.ApplyIssueGoal("team-1", "run-1", record); err != nil {
		t.Fatalf("相同记录重放应幂等: %v", err)
	}
	conflict := cloneGoalRecord(record)
	conflict.Operations["op-1"] = "different"
	if err := ledger.ApplyIssueGoal("team-1", "run-1", conflict); !errors.Is(err, ErrInputContentMismatch) {
		t.Fatalf("同事项同操作不同内容应冲突: %v", err)
	}

	reopened, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored := LoadInputLedger(snapshot, reopened)
	got := restored.IssueGoal("team-1", issue.ID)
	if got == nil || got.EvidenceHash != "run-evidence" || got.WorkspaceHead != "head-1" || got.Contract.Criteria[0].State != "met" {
		t.Fatalf("快照+journal 重放丢失事项目标: %+v", got)
	}
	if err := restored.MarkRoutingIntent("team-1", input.ID, "new_task", "run-1", "lead", ""); err != nil {
		t.Fatal(err)
	}
	if err := restored.MarkRouted("team-1", input.ID, "new_task", "run-1", "lead"); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.DeleteIssue("team-1", issue.ID); err != nil {
		t.Fatal(err)
	}
	if restored.IssueGoal("team-1", issue.ID) != nil {
		t.Fatal("删除事项应同时删除其目标投影")
	}
}
