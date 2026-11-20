package team

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestConversationAmendCarriesUnfinishedAncestorGoal(t *testing.T) {
	h := newDispatchHarness(t)
	const originalGoal = "根据 plan.md 按计划实现六镜头 MVP"
	root := &Run{ID: "original", TeamID: h.team.ID, Goal: originalGoal, AutoPlan: true, Status: RunCompleted,
		Tasks: []Task{{ID: "lead", MemberID: h.team.LeadID, Status: TaskCompleted, ExecutionContext: ExecutionContextLeadControl,
			Result: `<team-issues>{"segments":[{"action":"ignore"}]}</team-issues>`}}}
	middle := &Run{ID: "status-update", TeamID: h.team.ID, ParentRunID: root.ID, Goal: "实施计划文档已提交", AutoPlan: true, Status: RunCompleted}
	for _, row := range []*Run{root, middle} {
		if err := h.store.SaveRun(row); err != nil {
			t.Fatal(err)
		}
	}
	run, err := h.service.CreateConversationAmendRun(h.team, middle.ID, "continuation", "已完善并落盘实施计划", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, sent := h.waitForSubmission(t, "s-lead", "续轮主控上下文")
	if !strings.Contains(sent, originalGoal) || !strings.Contains(sent, middle.Goal) || !strings.Contains(sent, "completed 只表示") || strings.Contains(sent, `<team-issues>{"segments"`) {
		t.Fatalf("续轮必须保留原目标和结束/交付区别，不能搬运机器动作块: %s", sent)
	}
	if task := h.taskOf(run.ID, "lead"); h.service.sentTextFor(h.store.Run(run.ID), &task, h.team.Policy) != sent {
		t.Fatal("续轮上下文必须随首次派发固定，不能在重放时漂移")
	}
}

func TestTaskIntentPersistsAndCannotBecomeIgnoreOrAcknowledgement(t *testing.T) {
	ledger, snapshot, journalPath := newInputLedgerFixture(t)
	const text = "计划文档已经提交"
	input, _, err := ledger.AcceptWithIssueRouting("team-1", "task-input", text, nil, "policy", 64, false, InputIntentTask)
	if err != nil || input.Intent != InputIntentTask || !input.IssueRouting || input.Text != text {
		t.Fatalf("任务意图必须保留并启用分类，不改写原文: %+v %v", input, err)
	}
	if _, duplicate, err := ledger.AcceptWithIssueRouting("team-1", input.ID, text, nil, "policy", 64, true, InputIntentTask); err != nil || !duplicate {
		t.Fatalf("相同任务意图重试必须幂等: duplicate=%v err=%v", duplicate, err)
	}
	if _, _, err := ledger.AcceptWithIssueRouting("team-1", input.ID, text, nil, "policy", 64, true); !errors.Is(err, ErrInputContentMismatch) {
		t.Fatalf("同一消息 ID 不能被降级成普通沟通: %v", err)
	}
	proposal := &IssueProposal{InputID: input.ID, Revision: 1, Segments: []IssueProposalSegment{{Action: IssueSegmentIgnore, Source: "full"}}}
	if _, _, err := ledger.ApplyIssueProposal("team-1", "ignore-task", proposal); err == nil {
		t.Fatal("显式派发任务不能整条 ignore")
	}
	proposal.Segments[0] = IssueProposalSegment{Action: IssueSegmentClarify, Reason: "需要确认要实现的目标", Source: "full"}
	if _, _, err := ledger.ApplyIssueProposal("team-1", "clarify-task", proposal); err != nil {
		t.Fatalf("缺少目标时必须允许澄清: %v", err)
	}
	journal, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	restored := LoadInputLedger(snapshot, journal)
	if row := restored.Get("team-1", input.ID); row == nil || row.Intent != InputIntentTask || row.AssociationStatus != IssueAssociationClarification {
		t.Fatalf("任务意图必须跨重放保留: %+v", row)
	}
	if briefing := restored.IssueAssociationBriefing("team-1", input.ID); !strings.Contains(briefing, "明确要求团队执行") {
		t.Fatal("完成归类后仍必须保留执行意图简报")
	}
}

func TestExplicitTaskReportRepairsIgnoreThenDelegatesAndDelivers(t *testing.T) {
	h := newDispatchHarness(t)
	h.team.Policy.AutoDispatch = true
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	h.resolver.setSession("s-dev", newFakeSession("s-dev", "/tmp/dev"))
	ledger := LoadInputLedger(filepath.Join(t.TempDir(), "inputs.json"), h.journal)
	h.service.SetInputs(ledger)
	const id, text = "report-task", "已完善并落盘实施计划，详情见 plan.md"
	if _, _, err := ledger.AcceptWithIssueRouting(h.team.ID, id, text, nil, "policy", 64, true, InputIntentTask); err != nil {
		t.Fatal(err)
	}
	run, err := h.service.CreateConversationRun(h.team, id, text, nil)
	if err != nil {
		t.Fatal(err)
	}
	root := h.waitForNthSubmission(t, "s-lead", 1, "状态式正文的任务派发")
	if !strings.Contains(root.text, "明确要求团队执行") {
		t.Fatal("任务派发必须明确执行意图")
	}
	ignored := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"ignore","source":"full"}]}</team-issues>`, run.ID)
	h.completeSubmission("s-lead", root.submissionID, root.text, ignored+"已收到。")
	repair := h.waitForNthSubmission(t, "s-lead", 2, "禁止将任务退化为收到")
	if !strings.Contains(repair.text, "不能将整条任务标为 ignore") {
		t.Fatalf("必须反馈执行意图违反原因: %s", repair.text)
	}
	proposal := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"create","newIssueTitle":"落实计划","source":"full"}]}</team-issues>`, run.ID)
	h.completeSubmission("s-lead", repair.submissionID, repair.text, proposal+"已收到。")
	second := h.waitForNthSubmission(t, "s-lead", 3, "建项后仍必须派发工作")
	if !strings.Contains(second.text, "尚未委派实际工作") || !strings.Contains(second.text, "明确要求团队执行") {
		t.Fatalf("只建项和确认收到不能满足任务派发: %s", second.text)
	}
	h.completeSubmission("s-lead", second.submissionID, second.text, leadActionReply(`{"actions":[{"type":"summarize","sourceTaskId":"actfix-lead-2","round":1}]}`))
	third := h.waitForNthSubmission(t, "s-lead", 4, "不能用提前汇总代替执行")
	if !strings.Contains(third.text, "尚未委派实际工作") {
		t.Fatalf("提前 summarize 不能满足执行意图: %s", third.text)
	}
	h.completeSubmission("s-lead", third.submissionID, third.text, leadActionReply(`{"actions":[{"type":"delegate","sourceTaskId":"actfix-lead-3","round":1,"member":"dev","task":"通读 plan.md，落实实现并验证"}]}`))
	worker := h.waitForNthSubmission(t, "s-dev", 1, "实际实现任务")
	h.completeSubmission("s-dev", worker.submissionID, worker.text, "实现完成，测试通过，产物可核验。")
	delivery := h.waitForNthSubmission(t, "s-lead", 5, "面向用户的最终交付")
	h.completeSubmission("s-lead", delivery.submissionID, delivery.text, "已按计划实现，产物与验证结果如下。")
	waitFor(t, "派发任务终局", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
	if fresh := h.store.Run(run.ID); fresh.Status != RunCompleted || len(fresh.PendingActions) != 1 || findTaskIn(fresh, "lead-auto-reduce").Result == "" {
		t.Fatalf("必须有实际执行与可读交付: %+v", fresh)
	}
}

func TestContinuationBudgetPreservesTrailingInstructionAndLeadWorkGetsDelivery(t *testing.T) {
	text := "原目标：" + strings.Repeat("详细说明", 1000) + "最后必须按计划实现"
	if bounded := boundedContinuationText(text, 100); len([]rune(bounded)) > 100 || !strings.HasSuffix(bounded, "最后必须按计划实现") || !strings.Contains(bounded, "已截断") {
		t.Fatalf("截断必须保留目标尾部并标注缺失: %s", bounded)
	}
	h := newActionsExecHarness(t)
	run := execRunFixture()
	run.Tasks = append(run.Tasks, Task{ID: "lead-work", MemberID: h.team.LeadID, ExecutionContext: ExecutionContextLeadRun, Status: TaskCompleted})
	state := h.saveExecRun(t, run)
	state.mu.Lock()
	created := h.service.ensureAutoPlanReductionLocked(state, run)
	state.mu.Unlock()
	if !created || findTaskIn(h.store.Run(run.ID), "lead-auto-reduce") == nil {
		t.Fatal("Lead 自己完成实质工作后也必须回控制会话交付")
	}
}

func TestIssueOnlyIgnoreRequiresReadableReplyOrDelegation(t *testing.T) {
	h, ledger, run := newIssueWorkflowRun(t, "计划文档已经提交")
	root := h.waitForNthSubmission(t, "s-lead", 1, "用户消息")
	ignored := fmt.Sprintf(`<team-issues>{"inputId":%q,"revision":1,"segments":[{"action":"ignore","source":"full"}]}</team-issues>`, run.ID)
	h.completeSubmission("s-lead", root.submissionID, root.text, ignored)
	waitFor(t, "主控答复校验", func() bool {
		fresh := h.store.Run(run.ID)
		return terminalRun(fresh.Status) || findTaskIn(fresh, "actfix-lead-1") != nil
	})
	if terminalRun(h.store.Run(run.ID).Status) {
		t.Fatal("只返回 ignore 归类块，没有回复用户，不能直接完成")
	}
	repair := h.waitForNthSubmission(t, "s-lead", 2, "可读答复修复")
	if !strings.Contains(repair.text, "可读回复") {
		t.Fatalf("纠错必须说明缺少用户回复: %s", repair.text)
	}
	h.completeSubmission("s-lead", repair.submissionID, repair.text, "已收到计划文档更新；目前尚未产生实现交付。")
	waitFor(t, "可读沟通回复终局", func() bool { return terminalRun(h.store.Run(run.ID).Status) })
	if fresh := h.store.Run(run.ID); fresh.Status != RunCompleted || findTaskIn(fresh, "actfix-lead-1").Result != "已收到计划文档更新；目前尚未产生实现交付。" {
		t.Fatalf("允许无需执行的可读沟通回复: %+v", fresh)
	}
	if input := ledger.Get(h.team.ID, run.ID); input.AssociationStatus != IssueAssociationIgnored {
		t.Fatalf("补回复不得重复创建或改写已成功的归类: %+v", input)
	}
}
