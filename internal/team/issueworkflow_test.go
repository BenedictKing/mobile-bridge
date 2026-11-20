package team

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestIssueProposalFullSourceResolvesAndPersistsUnicodeRange(t *testing.T) {
	ledger, snapshot, journalPath := newInputLedgerFixture(t)
	text := "📽️ 实施计划\n[文档](plan.md)  按计划实现"
	input, _, err := ledger.AcceptWithIssueRouting("team-1", "full-input", text, nil, "policy", 64, true)
	if err != nil {
		t.Fatal(err)
	}
	proposal, violations := ExtractIssueProposal(`<team-issues>{"inputId":"full-input","revision":1,"segments":[{"action":"create","newIssueTitle":"实现计划","source":"full"}]}</team-issues>`)
	if len(violations) != 0 {
		t.Fatal(violations)
	}
	linked, _, err := ledger.ApplyIssueProposal("team-1", "full-proposal", proposal)
	if err != nil {
		t.Fatal(err)
	}
	if len(linked.IssueLinks) != 1 || linked.IssueLinks[0].SourceStart != 0 || linked.IssueLinks[0].SourceEnd != len([]rune(text)) || linked.IssueLinks[0].SourceText != text {
		t.Fatalf("整段来源必须由网关解析为精确 Unicode 区间: %+v", linked.IssueLinks)
	}
	journal, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	restored := LoadInputLedger(snapshot, journal).Get("team-1", input.ID)
	if restored == nil || len(restored.IssueLinks) != 1 || restored.IssueLinks[0].SourceText != text || restored.IssueLinks[0].SourceEnd != len([]rune(text)) || !restored.IssueLinks[0].LinkedAt.Equal(linked.IssueLinks[0].LinkedAt) {
		t.Fatalf("重放丢失解析后的精确来源: %+v", restored)
	}
}

func TestIssueProposalFullSourceRejectsAmbiguousRanges(t *testing.T) {
	for _, segments := range []string{
		`[{"action":"create","newIssueTitle":"计划","source":"unknown"}]`,
		`[{"action":"create","newIssueTitle":"计划","source":"full","sourceEnd":2}]`,
		`[{"action":"create","newIssueTitle":"计划","source":"full"},{"action":"ignore","sourceStart":0,"sourceEnd":1}]`,
	} {
		ledger := LoadInputLedger(filepath.Join(t.TempDir(), "inputs.json"), nil)
		_, _, err := ledger.AcceptWithIssueRouting("team-1", "input", "甲乙", nil, "policy", 64, true)
		if err != nil {
			t.Fatal(err)
		}
		proposal, _ := ExtractIssueProposal(fmt.Sprintf(`<team-issues>{"inputId":"input","revision":1,"segments":%s}</team-issues>`, segments))
		if _, _, err := ledger.ApplyIssueProposal("team-1", "proposal", proposal); err == nil {
			t.Fatalf("不得静默扩大或忽略冲突来源: %s", segments)
		}
	}
}

func TestIssueProposalNumericCoverageReportsWholeMissingTail(t *testing.T) {
	input := &TeamInput{ID: "input", Revision: 1, Text: "📽️ 文档链接  按计划实现"}
	end := len([]rune(input.Text))
	proposal := &IssueProposal{InputID: input.ID, Revision: input.Revision, Segments: []IssueProposalSegment{
		{Action: IssueSegmentCreate, NewIssueTitle: "计划", SourceEnd: end - 4},
	}}
	_, _, err := validateIssueProposal(proposal, input, "team-1", nil)
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("[%d,%d)", end-4, end)) || !strings.Contains(err.Error(), "计划实现") || !strings.Contains(err.Error(), "source=full") {
		t.Fatalf("纠错必须报告完整遗漏范围与无需计数的格式: %v", err)
	}
	proposal.Segments[0].SourceEnd = end
	if _, _, err := validateIssueProposal(proposal, input, "team-1", nil); err != nil {
		t.Fatalf("既有精确数字区间仍应受支持: %v", err)
	}
}

func newIssueWorkflowRun(t *testing.T, goal string) (*dispatchHarness, *InputLedger, *Run) {
	t.Helper()
	h := newDispatchHarness(t)
	h.team.Policy.AutoDispatch = true
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "inputs.json"), h.journal)
	h.service.SetInputs(ledger)
	const inputID = "issue-workflow"
	if _, _, err := ledger.AcceptWithIssueRouting(h.team.ID, inputID, goal, nil, "policy", 64, true); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreateConversationRun(h.team, inputID, goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h, ledger, run
}

func issueWorkflowReply(runID string, sourceEnd int, actions string) string {
	return fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"create","newIssueTitle":"按计划实现","sourceStart":0,"sourceEnd":%d}]}</team-issues>`, runID, sourceEnd) + leadActionReply(actions)
}

func TestIssueWorkflowRepairExhaustionFailsInsteadOfCompleting(t *testing.T) {
	goal := strings.Repeat("文档说明。", 118) + "按计划实现"
	h, ledger, run := newIssueWorkflowRun(t, goal)
	for attempt := 0; attempt <= maxActionRepairs; attempt++ {
		entry := h.waitForNthSubmission(t, "s-lead", attempt+1, "Issue 归类与纠错")
		if attempt > 0 && strings.Contains(entry.text, "整段回复只包含这一个动作块") {
			t.Fatal("Issue 纠错不能同时要求只输出动作块与必须输出 Issue 块")
		}
		h.completeSubmission("s-lead", entry.submissionID, entry.text, issueWorkflowReply(run.ID, len([]rune(goal))-4+attempt, `{"actions":[]}`))
	}
	waitFor(t, "编排失败终局", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunFailed || !strings.Contains(fresh.StopReason, "Issue 提案") {
		t.Fatalf("归类纠错耗尽不得落成功终局: status=%s reason=%s", fresh.Status, fresh.StopReason)
	}
	last := findTaskIn(fresh, "actfix-lead-3")
	if last == nil || last.Status != TaskFailed || last.ResultDetail == nil {
		t.Fatalf("最后一次协议失败必须保留失败状态与原始回复: %+v", last)
	}
	if input := ledger.Get(h.team.ID, run.ID); input.AssociationStatus != IssueAssociationFailed {
		t.Fatalf("输入必须保留归类失败事实: %+v", input)
	}
	if len(fresh.PendingActions) != 0 || len(h.eventsOfType(run.ID, EventRunCompleted)) != 0 {
		t.Fatal("未通过校验的提案不得执行或生成 run_completed")
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	restored := &Run{ID: run.ID}
	for _, event := range events {
		if event.RunID == run.ID {
			applyJournalEventToRun(restored, event)
		}
	}
	replayed := findTaskIn(restored, last.ID)
	if restored.Status != RunFailed || restored.StopReason != fresh.StopReason || replayed == nil || replayed.Status != TaskFailed || !reflect.DeepEqual(replayed.ResultDetail, last.ResultDetail) {
		t.Fatalf("Journal 重建必须保留失败终局、原因和原始回复: %+v", restored)
	}
}

func TestIssueWorkflowRepairsThenRunsPlanImplementVerifyAndDelivery(t *testing.T) {
	goal := "按文档实施六镜头 MVP，并验证交付结果 📽️"
	h, ledger, run := newIssueWorkflowRun(t, goal)
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev"))
	h.service.ExecutionSessions = &fakeExecutionSessionProvisioner{resolver: h.resolver, sessions: map[string]string{}}
	t.Cleanup(func() {
		if t.Failed() {
			fresh := h.store.Run(run.ID)
			t.Logf("workflow status=%s reason=%s", fresh.Status, fresh.StopReason)
			for _, task := range fresh.Tasks {
				t.Logf("task=%s status=%s error=%s deps=%v", task.ID, task.Status, task.Error, task.DependsOn)
			}
			for _, action := range fresh.PendingActions {
				t.Logf("action key=%s status=%s reason=%s", action.Key, action.Status, action.Reason)
			}
		}
	})
	const actions = `{"actions":[` +
		`{"type":"delegate","sourceTaskId":"lead","round":1,"member":"lead","key":"plan","phase":"plan","review":false,"writeLock":"none","task":"只读核对计划与验收标准"},` +
		`{"type":"delegate","sourceTaskId":"lead","round":2,"member":"dev","key":"implement","phase":"implement","dependsOn":["plan"],"review":false,"task":"根据计划实现并运行测试"},` +
		`{"type":"delegate","sourceTaskId":"lead","round":3,"member":"lead","key":"verify","dependsOn":["implement"],"review":false,"writeLock":"none","task":"只读核验实际实现和测试证据"}]}`
	root := h.waitForNthSubmission(t, "s-lead", 1, "首次主控")
	if !strings.Contains(root.text, `"source":"full"`) {
		t.Fatal("初始主控必须提供由网关解析的全文归类格式")
	}
	h.completeSubmission("s-lead", root.submissionID, root.text, issueWorkflowReply(run.ID, len([]rune(goal))-1, actions))
	repair := h.waitForNthSubmission(t, "s-lead", 2, "双块纠错")
	if !strings.Contains(repair.text, "同时先输出一个 team-issues 块") || !strings.Contains(repair.text, fmt.Sprintf("原文共 %d 个 Unicode rune", len([]rune(goal)))) {
		t.Fatalf("纠错必须说明双块协议与精确全文范围: %s", repair.text)
	}
	fullProposal := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"create","newIssueTitle":"实现六镜头 MVP","source":"full"}]}</team-issues>`, run.ID)
	h.completeSubmission("s-lead", repair.submissionID, repair.text, fullProposal+leadActionReply(actions))
	executionID := "s-lead-execution-" + run.ID
	plan := h.waitForNthSubmission(t, executionID, 1, "Lead 独立执行计划核对")
	if len(h.queue.entriesFor("s-dev")) != 0 {
		t.Fatal("实现任务不得早于计划核对完成")
	}
	h.completeSubmission(executionID, plan.submissionID, plan.text, "计划核对完成：实施六镜头 MVP，验收命令为 go test ./...")
	implement := h.waitForNthSubmission(t, "s-dev", 1, "成员执行实现任务")
	if !strings.Contains(implement.text, "计划核对完成") || len(h.queue.entriesFor(executionID)) != 1 {
		t.Fatal("实现必须取得计划结果，核验不得早于实现完成")
	}
	h.completeSubmission("s-dev", implement.submissionID, implement.text, "已实现六镜头 MVP；测试通过；提交 abc123")
	verify := h.waitForNthSubmission(t, executionID, 2, "Lead 独立执行交付核验")
	if !strings.Contains(verify.text, "已实现六镜头 MVP") {
		t.Fatal("核验任务必须取得真实实现结果")
	}
	h.completeSubmission(executionID, verify.submissionID, verify.text, "核验完成：实现与测试证据符合计划")
	delivery := h.waitForNthSubmission(t, "s-lead", 3, "最终交付回到 Lead 控制会话")
	if !strings.Contains(delivery.text, "核验完成") || !strings.Contains(delivery.text, "已实现六镜头 MVP") {
		t.Fatal("最终交付必须包含实现与核验结果")
	}
	h.completeSubmission("s-lead", delivery.submissionID, delivery.text, "六镜头 MVP 已交付；实现和核验完成，测试通过。")
	waitFor(t, "完整 workflow 终局", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
	fresh := h.store.Run(run.ID)
	if fresh.Status != RunCompleted || len(fresh.Tasks) != 6 || len(fresh.PendingActions) != 3 {
		t.Fatalf("纠错后应完整执行且不重复派发: %+v", fresh)
	}
	for _, action := range fresh.PendingActions {
		if action.Status != ActionStatusExecuted {
			t.Fatalf("提案必须变成实际执行事实: %+v", action)
		}
	}
	input := ledger.Get(h.team.ID, run.ID)
	if input.AssociationStatus != IssueAssociationLinked || len(input.IssueLinks) != 1 || input.IssueLinks[0].SourceText != goal {
		t.Fatalf("原始目标必须完整关联: %+v", input)
	}
	if reduction := findTaskIn(fresh, "lead-auto-reduce"); reduction == nil || reduction.Status != TaskCompleted || !strings.Contains(reduction.Result, "已交付") {
		t.Fatalf("完成终局前必须产生用户可读的最终交付: %+v", reduction)
	}
}
