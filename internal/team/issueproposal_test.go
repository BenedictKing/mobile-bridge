package team

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIssueProposalPersistsBubbleLinksAndReplays(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "team-inputs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	snapshot := filepath.Join(dir, "team-inputs.json")
	ledger := LoadInputLedger(snapshot, journal)
	input, duplicate, err := ledger.AcceptWithIssueRouting("team-1", "input-1", "团队导航统一；保留筛选", nil, "policy", 64, true)
	if err != nil || duplicate {
		t.Fatalf("accept: input=%+v duplicate=%v err=%v", input, duplicate, err)
	}
	if input.AssociationStatus != IssueAssociationPending {
		t.Fatalf("new input association status = %q", input.AssociationStatus)
	}
	segments := []IssueProposalSegment{
		{Action: IssueSegmentCreate, NewIssueTitle: " 统一 团队导航 ", SourceStart: 0, SourceEnd: 6},
		{Action: IssueSegmentIgnore, SourceStart: 6, SourceEnd: 7},
		{Action: IssueSegmentClarify, Reason: "需要确定保留筛选的范围", SourceStart: 7, SourceEnd: 11},
	}
	proposal := &IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: segments}
	proposalID := issueProposalID("team-1", input.ID, input.Revision)
	linked, duplicate, err := ledger.ApplyIssueProposal("team-1", proposalID, proposal)
	if err != nil || duplicate {
		t.Fatalf("apply proposal: input=%+v duplicate=%v err=%v", linked, duplicate, err)
	}
	if linked.AssociationStatus != IssueAssociationClarification || len(linked.IssueLinks) != 1 {
		t.Fatalf("association result = status %q links %+v", linked.AssociationStatus, linked.IssueLinks)
	}
	issues := ledger.ListIssues("team-1")
	if len(issues) != 1 || issues[0].Title != "统一 团队导航" || len(issues[0].Links) != 1 {
		t.Fatalf("issues = %+v", issues)
	}
	if _, duplicate, err := ledger.ApplyIssueProposal("team-1", proposalID, proposal); err != nil || !duplicate {
		t.Fatalf("proposal replay: duplicate=%v err=%v", duplicate, err)
	}

	restored := LoadInputLedger(snapshot, journal)
	restoredInput := restored.Get("team-1", input.ID)
	restoredIssues := restored.ListIssues("team-1")
	if restoredInput == nil || restoredInput.AssociationStatus != IssueAssociationClarification ||
		len(restoredInput.IssueLinks) != 1 || len(restoredIssues) != 1 || len(restoredIssues[0].Links) != 1 {
		t.Fatalf("snapshot+journal replay lost issue association: input=%+v issues=%+v", restoredInput, restoredIssues)
	}
	if briefing := restored.IssueAssociationBriefing("team-1", "input-2"); briefing != "" {
		t.Fatalf("briefing unexpectedly returned for absent input: %q", briefing)
	}
}

func TestIssueProposalRejectsGapsOverlapAndUnknownIssue(t *testing.T) {
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "team-inputs.json"), nil)
	input, _, err := ledger.AcceptWithIssueRouting("team-1", "input-1", "修复导航并保留筛选", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		segments []IssueProposalSegment
	}{
		{name: "gap", segments: []IssueProposalSegment{{Action: IssueSegmentCreate, NewIssueTitle: "导航", SourceStart: 0, SourceEnd: 4}}},
		{name: "overlap", segments: []IssueProposalSegment{
			{Action: IssueSegmentCreate, NewIssueTitle: "导航", SourceStart: 0, SourceEnd: 4},
			{Action: IssueSegmentClarify, Reason: "范围不明", SourceStart: 3, SourceEnd: 10},
		}},
		{name: "unknown issue", segments: []IssueProposalSegment{{Action: IssueSegmentLink, IssueID: "issue-missing", SourceStart: 0, SourceEnd: 10}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proposal := &IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: test.segments}
			if _, _, err := ledger.ApplyIssueProposal("team-1", "proposal-"+test.name, proposal); err == nil {
				t.Fatal("invalid Issue proposal was accepted")
			}
		})
	}
	if got := ledger.Get("team-1", input.ID).AssociationStatus; got != IssueAssociationPending {
		t.Fatalf("invalid proposal changed association status to %q", got)
	}
}

func TestLegacyInputRemainsUnclassified(t *testing.T) {
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "team-inputs.json"), nil)
	input, _, err := ledger.Accept("team-1", "legacy-input", "旧客户端输入", nil, "policy", 64)
	if err != nil {
		t.Fatal(err)
	}
	if input.IssueRouting || input.AssociationStatus != IssueAssociationUnclassified {
		t.Fatalf("legacy input was marked for automatic classification: %+v", input)
	}
	if briefing := ledger.IssueAssociationBriefing("team-1", input.ID); briefing != "" {
		t.Fatalf("legacy input received a Lead classification briefing: %q", briefing)
	}

	snapshot := filepath.Join(t.TempDir(), "team-inputs.json")
	legacySnapshot := `{"legacy-input":{"id":"legacy-input","teamId":"team-1","queueStatus":"queued","associationStatus":"pending"}}`
	if err := os.WriteFile(snapshot, []byte(legacySnapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	restored := LoadInputLedger(snapshot, nil).Get("team-1", "legacy-input")
	if restored == nil || restored.AssociationStatus != IssueAssociationUnclassified {
		t.Fatalf("legacy snapshot pending state should not imply an active classifier: %+v", restored)
	}
}

func TestIssueAssociationFailurePersistsAndRevisionCanRetry(t *testing.T) {
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "team-inputs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	snapshot := filepath.Join(dir, "team-inputs.json")
	ledger := LoadInputLedger(snapshot, journal)
	input, _, err := ledger.AcceptWithIssueRouting("team-1", "input-failed", "原始请求", nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	failed, duplicate, err := ledger.FailIssueAssociation("team-1", input.ID, "提案重试已达上限")
	if err != nil || duplicate || failed.AssociationStatus != IssueAssociationFailed || failed.AssociationReason != "提案重试已达上限" {
		t.Fatalf("failed classification: input=%+v duplicate=%v err=%v", failed, duplicate, err)
	}
	if _, duplicate, err := ledger.FailIssueAssociation("team-1", input.ID, "另一个原因"); err != nil || !duplicate {
		t.Fatalf("failed classification should be idempotent: duplicate=%v err=%v", duplicate, err)
	}

	restored := LoadInputLedger(snapshot, journal)
	restoredInput := restored.Get("team-1", input.ID)
	if restoredInput == nil || restoredInput.AssociationStatus != IssueAssociationFailed ||
		restoredInput.AssociationReason != "提案重试已达上限" || restored.IssueAssociationBriefing("team-1", input.ID) != "" {
		t.Fatalf("failed classification was not restored: %+v", restoredInput)
	}
	revised, _, err := restored.Revise("team-1", input.ID, "revise-after-failure", "补充后的请求", nil, restoredInput.Version)
	if err != nil || revised.AssociationStatus != IssueAssociationPending || revised.AssociationReason != "" {
		t.Fatalf("revised input should re-enter classification: input=%+v err=%v", revised, err)
	}
}

func TestHistoricalInputImportIsStableAndNeverQueuedForExecution(t *testing.T) {
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "team-inputs.json"), nil)
	id := HistoricalInputID("team-1", "run-1", "input-user-1")
	input, duplicate, err := ledger.ImportHistoricalInput("team-1", id, "修复团队画布", "run-1", "input-user-1", time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC))
	if err != nil || duplicate {
		t.Fatalf("import: input=%+v duplicate=%v err=%v", input, duplicate, err)
	}
	if !input.Historical || !input.IssueRouting || input.QueueStatus != InputRouted ||
		input.TargetRunID != "run-1" || len(input.RunIDs) != 1 || input.RunIDs[0] != "run-1" ||
		input.AssociationStatus != IssueAssociationPending {
		t.Fatalf("historical source must be classifiable without entering execution routing: %+v", input)
	}
	if _, duplicate, err := ledger.ImportHistoricalInput("team-1", id, "修复团队画布", "run-1", "input-user-1", input.AcceptedAt); err != nil || !duplicate {
		t.Fatalf("repeat import should be idempotent: duplicate=%v err=%v", duplicate, err)
	}
	if briefing := ledger.IssueAssociationBriefing("team-1", id); !strings.Contains(briefing, "离线整理") || !strings.Contains(briefing, "不要重新执行") {
		t.Fatalf("historical briefing should prohibit re-execution: %q", briefing)
	}
}

func TestExtractIssueProposalSupportsTeamActionsAndRejectsMultipleBlocks(t *testing.T) {
	valid := "<team-issues>{\"inputId\":\"input-1\",\"revision\":1,\"segments\":[{\"action\":\"ignore\",\"sourceStart\":0,\"sourceEnd\":4}]}</team-issues>\n" +
		"<team-actions>{\"actions\":[]}</team-actions>"
	proposal, violations := ExtractIssueProposal(valid)
	if len(violations) != 0 || proposal == nil || proposal.InputID != "input-1" || len(proposal.Segments) != 1 {
		t.Fatalf("proposal=%+v violations=%v", proposal, violations)
	}
	_, violations = ExtractIssueProposal(valid + "\n<team-issues>{}</team-issues>")
	if len(violations) == 0 || !strings.Contains(violations[0], "只能输出一个") {
		t.Fatalf("multiple issue blocks were not rejected: %v", violations)
	}
}
