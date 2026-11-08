package team

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func acceptRouteProposalInput(t *testing.T, h *actionsExecHarness, inputID, text string) {
	t.Helper()
	if _, _, err := h.service.inputs.Accept(h.team.ID, inputID, text, nil, PolicyDigest(h.team.Policy), 64); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, run := range h.store.RunsByTeam(h.team.ID) {
			_, _ = h.service.Cancel(run.ID)
		}
	})
}

func proposalItem(id, text string, start int, member, review string) WorkItem {
	return WorkItem{ID: id, RequirementText: text, SourceStart: start, SourceEnd: start + len([]rune(text)),
		MemberID: member, ReviewPolicy: review}
}

func TestRouteProposalBuildsWorkItemDAGAndReviewBinding(t *testing.T) {
	h := newRouterHarness(t)
	text := "实现 A\n实现 B\n补充 A"
	acceptRouteProposalInput(t, h, "input-proposal", text)
	items := []WorkItem{
		proposalItem("impl-a", "实现 A", 0, "dev", "required"),
		proposalItem("impl-b", "实现 B", 5, "dev", "off"),
		proposalItem("follow-a", "补充 A", 10, "dev", "best_effort"),
	}
	items[2].DependsOnItemIDs = []string{"impl-a"}
	opID := uuid.NewString()
	input, duplicate, err := h.service.ProposeInputRoute(h.team.ID, "input-proposal", opID, items, 1)
	if err != nil || duplicate || input.RouteStatus != "planned" || len(input.WorkItems) != 3 {
		t.Fatalf("提案落账异常: %+v duplicate=%v err=%v", input, duplicate, err)
	}
	waitFor(t, "planned 路由确认", func() bool {
		return h.service.inputs.Get(h.team.ID, "input-proposal").QueueStatus == InputRouted
	})
	input = h.service.inputs.Get(h.team.ID, "input-proposal")
	if len(input.RunIDs) != 1 || input.WorkItems[0].SourceInputID != "input-proposal" ||
		input.WorkItems[0].SourceRevision != 1 || len(input.WorkItems[0].TaskIDs) != 2 {
		t.Fatalf("工作项来源与审查链接异常: %+v", input)
	}
	run := h.store.Run("input-proposal")
	if run == nil || len(run.Tasks) != 5 {
		t.Fatalf("三项要求与两个审查应落成 5 个 DAG 任务: %+v", run)
	}
	if review := findTaskIn(run, "review-impl-a"); review == nil || len(review.ReviewOf) != 1 ||
		review.ReviewOf[0] != "impl-a" || !containsString(review.DependsOn, "impl-a") {
		t.Fatalf("required 审查必须绑定实现版本: %+v", review)
	}
	if follow := findTaskIn(run, "follow-a"); follow == nil || !containsString(follow.DependsOn, "impl-a") {
		t.Fatalf("依赖工作项不得抢跑: %+v", follow)
	}
	replay, duplicate, err := h.service.ProposeInputRoute(h.team.ID, "input-proposal", opID, items, 1)
	if err != nil || !duplicate || len(replay.WorkItems) != 3 {
		t.Fatalf("提案重放应幂等: %+v duplicate=%v err=%v", replay, duplicate, err)
	}
}

func TestRouteProposalRejectsMissingOverlapAndUnsafeReview(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		text  string
		items []WorkItem
	}{
		{"漏项", "实现 A\n实现 B", []WorkItem{proposalItem("only-a", "实现 A", 0, "dev", "off")}},
		{"重叠", "实现 A", []WorkItem{
			proposalItem("a1", "实现", 0, "dev", "off"),
			proposalItem("a2", "现 A", 1, "dev", "off"),
		}},
		{"来源篡改", "实现 A", []WorkItem{{ID: "a", RequirementText: "别的正文", SourceStart: 0, SourceEnd: 4, MemberID: "dev"}}},
		{"依赖前向引用", "A\nB", []WorkItem{
			proposalItem("a", "A", 0, "dev", "off"),
			proposalItem("b", "B", 2, "dev", "off"),
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newRouterHarness(t)
			acceptRouteProposalInput(t, h, "input-invalid", testCase.text)
			if testCase.name == "依赖前向引用" {
				testCase.items[0].DependsOnItemIDs = []string{"b"}
			}
			_, _, err := h.service.ProposeInputRoute(h.team.ID, "input-invalid", uuid.NewString(), testCase.items, 1)
			if !errors.Is(err, ErrRouteProposal) {
				t.Fatalf("无效提案应拒绝: %v", err)
			}
			if got := len(h.store.RunsByTeam(h.team.ID)); got != 0 {
				t.Fatalf("无效提案不得创建 Run: %d", got)
			}
		})
	}
}

func TestRouteProposalSplitsIndependentBatches(t *testing.T) {
	h := newRouterHarness(t)
	h.team.Policy.MaxTasksPerRun = 2
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	text := "任务 A\n任务 B\n任务 C"
	acceptRouteProposalInput(t, h, "input-batches", text)
	items := []WorkItem{
		proposalItem("task-a", "任务 A", 0, "dev", "off"),
		proposalItem("task-b", "任务 B", 5, "dev", "off"),
		proposalItem("task-c", "任务 C", 10, "dev", "off"),
	}
	_, _, err := h.service.ProposeInputRoute(h.team.ID, "input-batches", uuid.NewString(), items, 1)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "拆批路由确认", func() bool {
		return h.service.inputs.Get(h.team.ID, "input-batches").QueueStatus == InputRouted
	})
	input := h.service.inputs.Get(h.team.ID, "input-batches")
	if len(input.RunIDs) != 2 || input.WorkItems[2].RunID != "input-batches-batch-2" {
		t.Fatalf("独立要求应拆成两个 Run: %+v", input)
	}
	if run := h.store.Run("input-batches-batch-2"); run == nil || len(run.Tasks) != 1 {
		t.Fatalf("第二批 Run 异常: %+v", run)
	}
	if err := h.service.materializePlannedRoute(h.team, input); err != nil {
		t.Fatalf("已创建批次重放应幂等: %v", err)
	}
	if got := len(h.store.Run("input-batches").Tasks) + len(h.store.Run("input-batches-batch-2").Tasks); got != 3 {
		t.Fatalf("重放不得重复任务: %d", got)
	}
}

func TestRouteProposalRequiredNeedsIndependentReviewer(t *testing.T) {
	h := newRouterHarness(t)
	for i := range h.team.Members {
		if h.team.Members[i].ID == "rev" {
			h.team.Members[i].Enabled = false
		}
	}
	if err := h.store.SaveTeam(h.team); err != nil {
		t.Fatal(err)
	}
	acceptRouteProposalInput(t, h, "input-no-reviewer", "实现 A")
	_, _, err := h.service.ProposeInputRoute(h.team.ID, "input-no-reviewer", uuid.NewString(),
		[]WorkItem{proposalItem("impl-a", "实现 A", 0, "dev", "required")}, 1)
	if !errors.Is(err, ErrRouteProposal) || !strings.Contains(err.Error(), "独立评审") {
		t.Fatalf("required 不得在无独立评审时静默通过: %v", err)
	}
}
