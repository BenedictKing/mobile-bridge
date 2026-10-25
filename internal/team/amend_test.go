package team

// amend_test 是修订 Run 与多轮缓存导入（§10.2 amend 段、§6.4.1，阶段三，
// §13.1 team/recovery 行的 amend 缓存失效项）的单元测试：前缀规则表驱动
//（哈希匹配导入/依赖变化断链/插入新写任务关缓存/同 ID 异哈希显式拒绝/
// 父任务非 completed 不导入/成员变更）、创建幂等 duplicate、父 Run journal
// 行数零增（读文件断言）与父快照版本不变、cache_hit payload 有界、全缓存
// Run 零派发直接终局、部分缓存续跑（下游经既有 BuildTaskContext 注入导入
// 摘要——不搬运 transcript）、ReconcileRun 重放保终态（缺 cache_hit 事件的
// 对照组）、坏 team-runs.json 的 journal 重建复原 imports。等待全部走
// deadline 轮询（waitFor 惯用法），不用固定 sleep 判正确性。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const amendGoal = "实现用户头像上传接口"

// amendParentRun 从同一套计划输入构造父 Run：completed 集合内的任务落
// completed + ResultDetail，其余保持 planned。快照 InputHash 模拟派发期
// 覆写形态（与创建期哈希必然不同）——ImportPrefix 必须按确定性字段重算
// 创建期哈希匹配，绝不能比对快照哈希（两级哈希分离）。
func amendParentRun(t *testing.T, team *Team, runID, goal string, inputs []PlanTaskInput, completed ...string) *Run {
	t.Helper()
	plan, err := ParsePlan(team, goal, ModePipeline, "", inputs)
	if err != nil {
		t.Fatalf("父计划解析失败: %v", err)
	}
	run := &Run{
		ID: runID, TeamID: team.ID, Goal: plan.Goal, Mode: plan.Mode,
		CollectPolicy: plan.CollectPolicy, Status: RunDispatching, Round: 1,
		PlanHash: plan.PlanHash, Tasks: plan.Tasks,
	}
	done := map[string]bool{}
	for _, id := range completed {
		done[id] = true
	}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if !done[task.ID] {
			continue
		}
		result := task.ID + " 的结论。"
		task.Status = TaskCompleted
		task.Attempt = 1
		task.SubmissionID = TaskSubmissionID(team.ID, runID, task.ID, 1)
		task.InputHash = "dispatch-time:" + task.ID // 派发期覆写形态
		task.SentText = "已派发全文：" + task.ID
		task.Result = result
		task.ResultDetail = &TaskResult{
			SessionID: "s-" + task.MemberID, TurnID: "turn-" + task.ID,
			Result: result, ResultChars: len([]rune(result)), StopReason: "end_turn",
		}
	}
	return run
}

// amendEventsOf 重放 journal 并返回归属 runID 的事件（按落账序）。
func amendEventsOf(t *testing.T, journal *Journal, runID string) []*Event {
	t.Helper()
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	owned := []*Event{}
	for _, event := range events {
		if event.RunID == runID {
			owned = append(owned, event)
		}
	}
	return owned
}

// amendCountFileLines 直接读 journal 文件统计归属 runID 的事件行数（「父
// Run journal 零增」的读文件断言，不经过 Replay 的修复路径）。
func amendCountFileLines(t *testing.T, journal *Journal, runID string) int {
	t.Helper()
	raw, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatalf("journal 文件读取失败: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		event := &Event{}
		if err := event.UnmarshalJSON([]byte(line)); err != nil {
			t.Fatalf("journal 行解析失败: %v", err)
		}
		if event.RunID == runID {
			count++
		}
	}
	return count
}

// ---- ImportPrefix 前缀规则（纯函数，表驱动） ----

func TestImportPrefixRules(t *testing.T) {
	team := testTeam()
	// 修订版计划变体：实现任务换文案（同 ID 异哈希）。
	revisedImplement := append([]PlanTaskInput{}, pipelinePlan()...)
	revisedImplement[1].Prompt = "实现接口并兼容旧客户端"
	// 修订版计划变体：首个任务换文案（前缀起点即断链）。
	revisedDesign := append([]PlanTaskInput{}, pipelinePlan()...)
	revisedDesign[0].Prompt = "重新分析需求并拆分任务"
	// 修订版计划变体：在 design 与 implement 之间插入新的 exclusive 任务。
	insertedWrite := append([]PlanTaskInput{}, pipelinePlan()...)
	insertedWrite = append(insertedWrite[:1],
		append([]PlanTaskInput{{
			ID: "hotfix", MemberID: "dev", Prompt: "先修复构建",
			DependsOn: []string{"design"}, WriteLock: TaskWriteLockExclusive,
		}}, insertedWrite[1:]...)...)
	// 修订版计划变体：review 增补对 design 的直接依赖（依赖集合变化）。
	relinkedReview := append([]PlanTaskInput{}, pipelinePlan()...)
	relinkedReview[2].DependsOn = []string{"design", "implement"}
	// 修订版计划变体：implement 换成员（同 ID 异成员，视同任务修订）。
	swappedMember := append([]PlanTaskInput{}, pipelinePlan()...)
	swappedMember[1].MemberID = "rev"

	cases := []struct {
		name    string
		parent  *Run
		amend   []PlanTaskInput
		wantIDs []string
	}{
		{
			name:    "哈希匹配且父任务完成：连续前缀导入",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement"),
			amend:   pipelinePlan(),
			wantIDs: []string{"design", "implement"},
		},
		{
			name:    "父任务未完成：前缀在其处断链",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design"),
			amend:   pipelinePlan(),
			wantIDs: []string{"design"},
		},
		{
			name:    "依赖变化断链：上游修订后其后任务不复用",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement"),
			amend:   revisedImplement,
			wantIDs: []string{"design"},
		},
		{
			name:    "同 ID 异哈希显式拒绝复用：前缀起点断链零导入",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement"),
			amend:   revisedDesign,
			wantIDs: nil,
		},
		{
			name:    "插入新写任务后缓存关闭：其后任务不复用",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement", "review"),
			amend:   insertedWrite,
			wantIDs: []string{"design"},
		},
		{
			name:    "下游依赖集合变化断链：仅导入上游前缀",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement", "review"),
			amend:   relinkedReview,
			wantIDs: []string{"design", "implement"},
		},
		{
			name:    "同 ID 异成员视同任务修订",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement"),
			amend:   swappedMember,
			wantIDs: []string{"design"},
		},
		{
			name:    "父任务全部完成：整计划导入",
			parent:  amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement", "review"),
			amend:   pipelinePlan(),
			wantIDs: []string{"design", "implement", "review"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := ParsePlan(team, amendGoal, ModePipeline, "", tc.amend)
			if err != nil {
				t.Fatalf("修订计划解析失败: %v", err)
			}
			imports := ImportPrefix(tc.parent, plan, team.Policy)
			gotIDs := make([]string, 0, len(imports))
			for i := range imports {
				gotIDs = append(gotIDs, imports[i].TaskID)
			}
			if len(gotIDs) != len(tc.wantIDs) {
				t.Fatalf("导入前缀应为 %v，实际 %v", tc.wantIDs, gotIDs)
			}
			for i := range gotIDs {
				if gotIDs[i] != tc.wantIDs[i] {
					t.Fatalf("导入前缀应为 %v，实际 %v", tc.wantIDs, gotIDs)
				}
			}
			for i := range imports {
				entry := &imports[i]
				parentTask := func() *Task {
					for j := range tc.parent.Tasks {
						if tc.parent.Tasks[j].ID == entry.TaskID {
							return &tc.parent.Tasks[j]
						}
					}
					return nil
				}()
				if parentTask == nil {
					t.Fatalf("导入 %s 无对应父任务", entry.TaskID)
				}
				if entry.ParentTaskID != parentTask.ID {
					t.Fatalf("导入 %s 的父任务应为 %s", entry.TaskID, parentTask.ID)
				}
				if entry.ResultDetail == nil || entry.ResultDetail.Result != parentTask.ResultDetail.Result ||
					entry.ResultDetail.SessionID != parentTask.ResultDetail.SessionID ||
					entry.ResultDetail.TurnID != parentTask.ResultDetail.TurnID {
					t.Fatalf("导入 %s 应深拷贝父结果引用: %+v", entry.TaskID, entry.ResultDetail)
				}
				// 匹配键是创建期规范化哈希：既不等于父快照的派发期哈希，
				// 也必须与子计划任务的创建期哈希一致。
				if entry.InputHash == parentTask.InputHash {
					t.Fatalf("导入 %s 的匹配哈希不得采用父快照的派发期哈希", entry.TaskID)
				}
				if entry.InputHash != InputHashFor(parentTask, PolicyDigest(team.Policy), "") {
					t.Fatalf("导入 %s 的匹配哈希应为父任务创建期哈希", entry.TaskID)
				}
				// 深拷贝隔离：改写导入件不得波及父 Run。
				entry.ResultDetail.Result = "篡改"
				if parentTask.ResultDetail.Result == "篡改" {
					t.Fatalf("导入 %s 与父任务共享 ResultDetail 内层指针", entry.TaskID)
				}
			}
		})
	}
}

// TestImportPrefixRejectsMismatchedHashWithoutReuse 是 §8.2「已有不同
// inputHash：拒绝复用」的行为面断言：同 ID 同成员但内容修订的任务绝不进
// imports（按全新任务执行的语义由 CreateAmendRun 落实——不记 cache_hit、
// 不写 CachedFrom）。
func TestImportPrefixRejectsMismatchedHashWithoutReuse(t *testing.T) {
	team := testTeam()
	parent := amendParentRun(t, team, "run-p", amendGoal, pipelinePlan(), "design", "implement")
	revised := append([]PlanTaskInput{}, pipelinePlan()...)
	revised[1].Prompt = "实现接口并兼容旧客户端"
	plan, err := ParsePlan(team, amendGoal, ModePipeline, "", revised)
	if err != nil {
		t.Fatalf("修订计划解析失败: %v", err)
	}
	imports := ImportPrefix(parent, plan, team.Policy)
	for _, entry := range imports {
		if entry.TaskID == "implement" {
			t.Fatal("同 ID 异哈希的 implement 绝不能进入导入列表")
		}
	}
}

// ---- CreateAmendRun：导入、续跑与父 Run 不变量 ----

func TestCreateAmendRunPartialImportContinuesFresh(t *testing.T) {
	h := newDispatchHarness(t)
	parent := amendParentRun(t, h.team, "run-parent", amendGoal, pipelinePlan(), "design", "implement")
	if err := h.store.SaveRun(parent); err != nil {
		t.Fatalf("父 Run 保存失败: %v", err)
	}

	amend, duplicate, err := h.service.CreateAmendRun(h.team, "run-parent", "run-amend",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil || duplicate {
		t.Fatalf("amend 应成功且非重放: duplicate=%v err=%v", duplicate, err)
	}
	if amend.ID != "run-amend" || amend.ParentRunID != "run-parent" {
		t.Fatalf("amend 头字段异常: %+v", amend)
	}
	if amend.PlanHash != parent.PlanHash {
		t.Fatalf("同计划修订的 planHash 应与父一致: %s vs %s", amend.PlanHash, parent.PlanHash)
	}
	if len(amend.Tasks) != 3 {
		t.Fatalf("任务数应为 3，实际 %d", len(amend.Tasks))
	}
	// design/implement 创建批次内直接快照 completed：CachedFrom 标记来源、
	// 深拷贝结果引用、绝不携带派发痕迹（SubmissionID/SentText 留空）。
	for i, want := range []string{"design", "implement"} {
		task := amend.Tasks[i]
		if task.ID != want || task.Status != TaskCompleted || task.CachedFrom != want {
			t.Fatalf("任务 %s 应为导入的 completed（CachedFrom=%s）: %+v", want, want, task)
		}
		if task.SubmissionID != "" || task.SentText != "" || task.Attempt != 0 {
			t.Fatalf("缓存任务不得携带派发痕迹: %+v", task)
		}
		if task.ResultDetail == nil || task.ResultDetail.TurnID != "turn-"+want ||
			task.ResultDetail.Result != want+" 的结论。" {
			t.Fatalf("任务 %s 应深拷贝父结果引用: %+v", want, task.ResultDetail)
		}
	}
	review := amend.Tasks[2]
	if review.ID != "review" || review.Status != TaskPlanned || review.CachedFrom != "" {
		t.Fatalf("review 应按全新任务执行: %+v", review)
	}

	// 创建批次事件序（CreateAmendRun 返回前同步落账）：run_created → 每任务
	// task_planned，命中任务紧跟 task_cache_hit（§6.4.1 规则 4：事件一个不少）。
	events := amendEventsOf(t, h.journal, "run-amend")
	wantPrefix := []string{
		EventRunCreated,
		EventTaskPlanned, EventTaskCacheHit, // design
		EventTaskPlanned, EventTaskCacheHit, // implement
		EventTaskPlanned, // review
	}
	if len(events) < len(wantPrefix) {
		t.Fatalf("创建批次事件不足: %v", eventTypesOf(events))
	}
	for i, want := range wantPrefix {
		if events[i].Type != want {
			t.Fatalf("创建批次第 %d 个事件应为 %s，实际 %s（全序 %v）",
				i, want, events[i].Type, eventTypesOf(events))
		}
	}
	if got := payloadString(events[0].Payload, "parentRunId"); got != "run-parent" {
		t.Fatalf("run_created 应携带 parentRunId，实际 %q", got)
	}
	// cache_hit payload 契约（§10.3 有界 payload）。
	var designHit *Event
	for _, event := range events {
		if event.Type == EventTaskCacheHit && payloadString(event.Payload, "taskId") == "design" {
			designHit = event
		}
	}
	if designHit == nil {
		t.Fatal("design 应有 task_cache_hit 事件")
	}
	for key, want := range map[string]string{
		"taskId":       "design",
		"parentRunId":  "run-parent",
		"parentTaskId": "design",
		"inputHash":    amend.Tasks[0].InputHash,
		"sessionId":    "s-lead",
		"turnId":       "turn-design",
		"result":       "design 的结论。",
	} {
		if got := payloadString(designHit.Payload, key); got != want {
			t.Fatalf("cache_hit payload[%s] 应为 %q，实际 %q", key, want, got)
		}
	}
	if got := payloadInt(designHit.Payload, "resultChars"); got != len([]rune("design 的结论。")) {
		t.Fatalf("cache_hit payload[resultChars] 应为 %d，实际 %d", len([]rune("design 的结论。")), got)
	}
	if truncated, _ := designHit.Payload["truncated"].(bool); truncated {
		t.Fatal("未触上限不应标记 truncated")
	}

	// 缓存任务绝不重派发：只有 review（s-rev）收到派发，design/implement
	// 成员会话零入账。
	_, sentText := h.waitForSubmission(t, "s-rev", "review 任务")
	for _, sessionID := range []string{"s-lead", "s-dev"} {
		if entries := h.queue.entriesFor(sessionID); len(entries) != 0 {
			t.Fatalf("缓存任务不得重派发，%s 收到入账: %+v", sessionID, entries)
		}
	}
	// 不搬运 transcript（§6.4.1 规则 5）：下游上下文走既有 BuildTaskContext，
	// 导入结果以有界摘要注入（review 只依赖 implement）。
	for _, want := range []string{
		"[Team context]", "来源成员：dev", "来源任务：implement", "implement 的结论。", "[Assigned task]",
	} {
		if !strings.Contains(sentText, want) {
			t.Fatalf("review 派发文本应含 %q:\n%s", want, sentText)
		}
	}

	// review 完成后整 Run 终局（导入任务 + 新执行任务同批 fold）。
	entry, _ := h.queue.firstEntry("s-rev")
	h.completeSubmission("s-rev", entry.submissionID, sentText, "测试已补齐。")
	waitFor(t, "amend run 完成", func() bool { return h.store.Run("run-amend").Status == RunCompleted })

	// 父 Run 快照不变：任务状态原样、版本不因 amend 前进。
	fresh := h.store.Run("run-parent")
	if fresh.Version != parent.Version {
		t.Fatalf("父 Run 版本不得被 amend 推进: %d vs %d", fresh.Version, parent.Version)
	}
	if fresh.Tasks[0].Status != TaskCompleted || fresh.Tasks[1].Status != TaskCompleted ||
		fresh.Tasks[2].Status != TaskPlanned {
		t.Fatalf("父 Run 任务状态不得被改动: %+v", fresh.Tasks)
	}
}

// eventTypesOf 提取事件类型序（失败信息用）。
func eventTypesOf(events []*Event) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

// TestCreateAmendRunFullCacheCompletesWithoutDispatch：全部任务命中缓存时
// 零派发直接终局——「命中即本 Run 新节点实例入账」的完整形态（事件齐全、
// 终局照走，只是没有任何外部发送）。
func TestCreateAmendRunFullCacheCompletesWithoutDispatch(t *testing.T) {
	h := newDispatchHarness(t)
	parent := amendParentRun(t, h.team, "run-full", amendGoal, pipelinePlan(), "design", "implement", "review")
	if err := h.store.SaveRun(parent); err != nil {
		t.Fatalf("父 Run 保存失败: %v", err)
	}

	amend, _, err := h.service.CreateAmendRun(h.team, "run-full", "run-amend-full",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil {
		t.Fatalf("amend 应成功: %v", err)
	}
	waitFor(t, "全缓存 run 完成", func() bool { return h.store.Run(amend.ID).Status == RunCompleted })
	for i, want := range []string{"design", "implement", "review"} {
		task := h.taskOf(amend.ID, want)
		if task.Status != TaskCompleted || task.CachedFrom != want {
			t.Fatalf("任务 %s（第 %d 个）应为缓存导入的 completed: %+v", want, i, task)
		}
	}
	for _, sessionID := range []string{"s-lead", "s-dev", "s-rev"} {
		if entries := h.queue.entriesFor(sessionID); len(entries) != 0 {
			t.Fatalf("全缓存 run 不得有任何派发，%s 收到入账: %+v", sessionID, entries)
		}
	}
	types := h.runEventTypes(amend.ID)
	found := map[string]int{}
	for _, eventType := range types {
		found[eventType]++
	}
	if found[EventTaskCacheHit] != 3 || found[EventTaskPlanned] != 3 || found[EventRunCompleted] != 1 {
		t.Fatalf("全缓存 run 的事件账应为 3×cache_hit + 3×planned + 1×终局，实际 %v", types)
	}
}

// TestCreateAmendRunKeepsParentJournalZeroGrowth：amend 绝不写父 Run 的
// journal（§10.2 硬边界，读文件断言），父快照版本不变；被取消的下游任务
// 断链后按全新任务重派。
func TestCreateAmendRunKeepsParentJournalZeroGrowth(t *testing.T) {
	h := newDispatchHarness(t)
	parent := h.createRun(t, ModePipeline, pipelinePlan())
	// 推进真实父 Run：design 完成 → implement 已派发 → 取消（implement/
	// review 落 cancelled，design completed）。
	designSubmission, designText := h.waitForSubmission(t, "s-lead", "design 任务")
	h.completeSubmission("s-lead", designSubmission, designText, "分析完成。")
	h.waitForSubmission(t, "s-dev", "implement 任务")
	if _, err := h.service.Cancel(parent.ID); err != nil {
		t.Fatalf("父 Run 取消失败: %v", err)
	}
	parentVersion := h.store.Run(parent.ID).Version
	linesBefore := amendCountFileLines(t, h.journal, parent.ID)
	if linesBefore == 0 {
		t.Fatal("前置失败：父 Run 应已有 journal 事实")
	}

	amend, _, err := h.service.CreateAmendRun(h.team, parent.ID, "run-amend2",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil {
		t.Fatalf("amend 应成功: %v", err)
	}
	if amend.ParentRunID != parent.ID {
		t.Fatalf("amend 应记录父 Run: %+v", amend)
	}
	// 父 Run journal 行数零增（读文件）。
	if linesAfter := amendCountFileLines(t, h.journal, parent.ID); linesAfter != linesBefore {
		t.Fatalf("amend 不得写父 Run 的 journal：%d 行 → %d 行", linesBefore, linesAfter)
	}
	if fresh := h.store.Run(parent.ID); fresh.Version != parentVersion {
		t.Fatalf("父 Run 版本不得被 amend 推进: %d vs %d", fresh.Version, parentVersion)
	}
	// design 命中缓存（不重跑——s-lead 只有父 Run 自己的一条入账）；被取消
	// 的 implement/review 断链后按全新任务重派。
	if task := h.taskOf(amend.ID, "design"); task.Status != TaskCompleted || task.CachedFrom != "design" {
		t.Fatalf("design 应命中缓存: %+v", task)
	}
	waitFor(t, "implement 重新派发", func() bool { return len(h.queue.entriesFor("s-dev")) == 2 })
	if entries := h.queue.entriesFor("s-lead"); len(entries) != 1 {
		t.Fatalf("design 不得重派（s-lead 应只有父 Run 自己的入账）: %+v", entries)
	}
}

// ---- 创建幂等 ----

func TestCreateAmendRunIdempotencyAndParentChecks(t *testing.T) {
	h := newDispatchHarness(t)
	parent := amendParentRun(t, h.team, "run-parent", amendGoal, pipelinePlan(), "design")
	if err := h.store.SaveRun(parent); err != nil {
		t.Fatalf("父 Run 保存失败: %v", err)
	}
	// 同 id 同内容重放：duplicate=true、零新事件。
	first, duplicate, err := h.service.CreateAmendRun(h.team, "run-parent", "run-amend-idem",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil || duplicate {
		t.Fatalf("首次创建应成功且非重放: duplicate=%v err=%v", duplicate, err)
	}
	second, duplicate, err := h.service.CreateAmendRun(h.team, "run-parent", "run-amend-idem",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil || !duplicate || second.ID != first.ID {
		t.Fatalf("同 id 同内容重放应返回 duplicate: duplicate=%v id=%s err=%v", duplicate, second.ID, err)
	}
	if got := len(amendEventsOf(t, h.journal, "run-amend-idem")); got != 5 {
		t.Fatalf("重放不得追加事件（run_created + 3×task_planned + design 的 cache_hit 共 5 条），实际 %d 条", got)
	}
	// 异内容（目标修订）：拒绝。
	if _, _, err := h.service.CreateAmendRun(h.team, "run-parent", "run-amend-idem",
		"另一个目标", ModePipeline, "", nil, pipelinePlan(), false); !errors.Is(err, ErrRunContentMismatch) {
		t.Fatalf("同 id 异目标应报内容不一致，实际 %v", err)
	}
	// 换父 Run（parentRunID 属于请求身份）：拒绝。
	other := amendParentRun(t, h.team, "run-parent-2", amendGoal, pipelinePlan(), "design")
	if err := h.store.SaveRun(other); err != nil {
		t.Fatalf("第二父 Run 保存失败: %v", err)
	}
	if _, _, err := h.service.CreateAmendRun(h.team, "run-parent-2", "run-amend-idem",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false); !errors.Is(err, ErrRunContentMismatch) {
		t.Fatalf("同 id 换父 Run 应报内容不一致，实际 %v", err)
	}
	// 父 Run 不存在：NotFound；复用父 Run 标识：拒绝。
	if _, _, err := h.service.CreateAmendRun(h.team, "run-nope", "run-x",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("父 Run 不存在应报 NotFound，实际 %v", err)
	}
	if _, _, err := h.service.CreateAmendRun(h.team, "run-parent", "run-parent",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false); err == nil {
		t.Fatal("修订 Run 不得复用父 Run 的标识")
	}
	// 跨团队父 Run：拒绝。
	foreignTeam := testTeam()
	foreignTeam.ID = "team-2"
	foreign := amendParentRun(t, foreignTeam, "run-foreign", amendGoal, pipelinePlan(), "design")
	if err := h.store.SaveRun(foreign); err != nil {
		t.Fatalf("跨团队父 Run 保存失败: %v", err)
	}
	if _, _, err := h.service.CreateAmendRun(h.team, "run-foreign", "run-y",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false); err == nil ||
		errors.Is(err, ErrRunContentMismatch) {
		t.Fatalf("跨团队父 Run 应被拒绝，实际 %v", err)
	}
}

// ---- cache_hit payload 有界 ----

func TestCreateAmendRunCacheHitPayloadBounded(t *testing.T) {
	h := newDispatchHarness(t)
	parent := amendParentRun(t, h.team, "run-bounded", amendGoal, pipelinePlan(), "design")
	limit := h.team.Policy.MaxResultChars
	full := strings.Repeat("析", limit) // 父任务触上限截断后的有界正文
	parent.Tasks[0].Result = full
	parent.Tasks[0].ResultDetail.Result = full
	parent.Tasks[0].ResultDetail.ResultChars = limit * 2 // 截断前全量 rune
	parent.Tasks[0].ResultDetail.Truncated = true
	if err := h.store.SaveRun(parent); err != nil {
		t.Fatalf("父 Run 保存失败: %v", err)
	}

	if _, _, err := h.service.CreateAmendRun(h.team, "run-bounded", "run-amend-bounded",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false); err != nil {
		t.Fatalf("amend 应成功: %v", err)
	}
	for _, event := range amendEventsOf(t, h.journal, "run-amend-bounded") {
		if event.Type != EventTaskCacheHit {
			continue
		}
		if got := payloadString(event.Payload, "result"); len([]rune(got)) > limit {
			t.Fatalf("cache_hit payload 的 result 应有界（≤%d rune），实际 %d", limit, len([]rune(got)))
		}
		if got := payloadInt(event.Payload, "resultChars"); got != limit*2 {
			t.Fatalf("cache_hit payload[resultChars] 应保留截断前全量 %d，实际 %d", limit*2, got)
		}
		if truncated, _ := event.Payload["truncated"].(bool); !truncated {
			t.Fatal("父结果触上限时 cache_hit 应携带 truncated 标记")
		}
	}
}

// ---- 恢复与重建 ----

// TestReconcileRunCacheHitKeepsCompleted：cache_hit 事件是缓存任务的
// journal 终态佐证——重放保 completed；缺了它会被规则 5 折成
// outcome_unknown + run waiting_input（对照组证明 case 是承重的）。
func TestReconcileRunCacheHitKeepsCompleted(t *testing.T) {
	run := &Run{
		ID: "run-amend-r", TeamID: "team-1", Status: RunDispatching, Round: 1,
		PlanHash: "hash", Tasks: []Task{
			{
				ID: "design", MemberID: "lead", Ordinal: 0, Status: TaskCompleted,
				CachedFrom: "design", InputHash: "ih-design",
				ResultDetail: &TaskResult{SessionID: "s-lead", TurnID: "turn-design", Result: "分析完成。"},
			},
			{ID: "review", MemberID: "rev", Ordinal: 1, Status: TaskPlanned},
		},
	}
	events := []*Event{
		journalEvent("run-amend-r", EventRunCreated, 1, map[string]any{
			"teamId": "team-1", "goal": amendGoal, "mode": ModePipeline,
			"planHash": "hash", "taskCount": jsoncompat.Num(2), "parentRunId": "run-parent",
		}),
		journalEvent("run-amend-r", EventTaskPlanned, 2, map[string]any{
			"taskId": "design", "memberId": "lead", "instructionsHead": "分析",
		}),
		journalEvent("run-amend-r", EventTaskCacheHit, 3, map[string]any{
			"taskId": "design", "parentRunId": "run-parent", "parentTaskId": "design",
			"inputHash": "ih-design", "sessionId": "s-lead", "turnId": "turn-design",
			"result": "分析完成。", "resultChars": jsoncompat.Num(5), "truncated": false,
		}),
		journalEvent("run-amend-r", EventTaskPlanned, 4, map[string]any{
			"taskId": "review", "memberId": "rev", "instructionsHead": "检查",
		}),
	}
	transitions := ReconcileRun(run, events, newFakeProbe())
	if len(transitions) != 0 {
		t.Fatalf("缓存任务有 cache_hit 佐证时对账应零迁移: %+v", transitions)
	}
	// 对照组：剥掉 cache_hit 事实，快照 completed 失去 journal 佐证 →
	// outcome_unknown + run waiting_input。
	stripped := []*Event{events[0], events[1], events[3]}
	transitions = ReconcileRun(run, stripped, newFakeProbe())
	sawUnknown, sawWaiting := false, false
	for _, transition := range transitions {
		if transition.Kind == TransitionTaskStatus && transition.TaskID == "design" &&
			transition.Status == TaskOutcomeUnknown {
			sawUnknown = true
		}
		if transition.Kind == TransitionRunStatus && transition.Status == RunWaitingInput {
			sawWaiting = true
		}
	}
	if !sawUnknown || !sawWaiting {
		t.Fatalf("缺 cache_hit 佐证时应折 outcome_unknown 并停 waiting_input: %+v", transitions)
	}
}

// TestRebuildRunsRestoresAmendImports：team-runs.json 整文件损坏后按
// journal 重建，amend 的血缘（parentRunId）与缓存导入事实（completed +
// CachedFrom + 结果引用）必须复原。
func TestRebuildRunsRestoresAmendImports(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(sampleTeam("t-amend")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runsPath, []byte("不是 JSON{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if !store.RunsLedgerCorrupt() {
		t.Fatal("整文件损坏应置 RunsLedgerCorrupt")
	}
	store.RebuildRunsFromJournal([]*Event{
		{ID: "e1", RunID: "r-amend", Type: EventRunCreated, Source: "user", Sequence: 1,
			Payload: map[string]any{
				"teamId": "t-amend", "goal": amendGoal, "mode": ModePipeline,
				"planHash": "hash-1", "taskCount": jsoncompat.Num(2), "parentRunId": "r-parent",
			}, CreatedAt: testTime},
		{ID: "e2", RunID: "r-amend", Type: EventTaskPlanned, Source: "user", Sequence: 2,
			Payload: map[string]any{"taskId": "design", "memberId": "lead", "instructionsHead": "分析"}, CreatedAt: testTime},
		{ID: "e3", RunID: "r-amend", Type: EventTaskCacheHit, Source: "user", Sequence: 3,
			Payload: map[string]any{
				"taskId": "design", "parentRunId": "r-parent", "parentTaskId": "design",
				"inputHash": "ih-1", "sessionId": "codex-1", "turnId": "turn-1",
				"result": "分析完成。", "resultChars": jsoncompat.Num(5), "truncated": false,
			}, CreatedAt: testTime},
		{ID: "e4", RunID: "r-amend", Type: EventTaskPlanned, Source: "user", Sequence: 4,
			Payload: map[string]any{"taskId": "review", "memberId": "reviewer", "instructionsHead": "检查"}, CreatedAt: testTime},
	})
	run := store.Run("r-amend")
	if run == nil {
		t.Fatal("重建后 r-amend 应存在")
	}
	if run.ParentRunID != "r-parent" {
		t.Fatalf("重建应复原修订血缘 parentRunId，实际 %q", run.ParentRunID)
	}
	if len(run.Tasks) != 2 {
		t.Fatalf("重建任务数应为 2，实际 %d", len(run.Tasks))
	}
	design := run.Tasks[0]
	if design.ID != "design" || design.Status != TaskCompleted || design.CachedFrom != "design" {
		t.Fatalf("重建应复原缓存导入的 completed（CachedFrom=design）: %+v", design)
	}
	if design.InputHash != "ih-1" {
		t.Fatalf("重建应复原创建期 inputHash，实际 %q", design.InputHash)
	}
	if design.ResultDetail == nil || design.ResultDetail.Result != "分析完成。" ||
		design.ResultDetail.TurnID != "turn-1" || design.ResultDetail.ResultChars != 5 {
		t.Fatalf("重建应复原导入结果引用: %+v", design.ResultDetail)
	}
	if run.Tasks[1].ID != "review" || run.Tasks[1].Status != TaskPlanned {
		t.Fatalf("未命中任务按 task_planned 重建: %+v", run.Tasks[1])
	}
}

// TestAmendFieldsPersistAcrossReload：ParentRunID/CachedFrom 进
// runKnownFields/taskKnownFields 后经 team-runs.json 落盘重载不丢（漏登
// knownFields 会把它们落进 extra、结构体字段恒零值）。
func TestAmendFieldsPersistAcrossReload(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(sampleTeam("t-amend")); err != nil {
		t.Fatal(err)
	}
	run := &Run{
		ID: "r-persist", TeamID: "t-amend", Goal: amendGoal, Status: RunCompleted,
		Round: 1, PlanHash: "hash", ParentRunID: "r-parent",
		Tasks: []Task{{
			ID: "design", MemberID: "lead", Ordinal: 0, Status: TaskCompleted,
			CachedFrom: "design", InputHash: "ih-1",
			ResultDetail: &TaskResult{SessionID: "codex-1", TurnID: "turn-1", Result: "分析完成。"},
		}},
	}
	if err := seed.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	reopened := LoadStore(teamsPath, runsPath)
	got := reopened.Run("r-persist")
	if got == nil || got.ParentRunID != "r-parent" {
		t.Fatalf("重载后 ParentRunID 应存活: %+v", got)
	}
	if len(got.Tasks) != 1 || got.Tasks[0].CachedFrom != "design" {
		t.Fatalf("重载后 CachedFrom 应存活: %+v", got.Tasks)
	}
}

// TestCreateAmendRunAutoPlanDerivation 空计划修订（single 模式合成 Lead 任务）
// 必须落 AutoPlan=true 并随 run_created payload 落账（§5.1/§10.2：与
// handleRunCreate 的「无 plan 键即 autoPlan」推导一致——「让 Lead 再规划
// 一轮」的典型修订场景拿得到 team-actions 解析）；携带显式计划则 false；
// autoPlan 与显式计划互斥（同 CreateRun）；创建幂等的身份比对含 AutoPlan。
func TestCreateAmendRunAutoPlanDerivation(t *testing.T) {
	h := newDispatchHarness(t)
	parent := amendParentRun(t, h.team, "run-ap", amendGoal, pipelinePlan(), "design")
	if err := h.store.SaveRun(parent); err != nil {
		t.Fatalf("父 Run 保存失败: %v", err)
	}

	// 空计划 + autoPlan：Run 落 AutoPlan，run_created payload 携带 autoPlan=true
	//（team-runs.json 损坏后的 journal 重建据此恢复解析闸门）。
	amend, _, err := h.service.CreateAmendRun(h.team, "run-ap", "run-amend-ap",
		"再规划一轮", ModeSingle, "", nil, nil, true)
	if err != nil {
		t.Fatalf("空计划 autoPlan 修订应成功: %v", err)
	}
	if !amend.AutoPlan {
		t.Fatalf("空计划修订应落 AutoPlan=true: %+v", amend)
	}
	if len(amend.Tasks) != 1 || amend.Tasks[0].MemberID != "lead" {
		t.Fatalf("single 空计划应合成 Lead 任务: %+v", amend.Tasks)
	}
	var created *Event
	for _, event := range amendEventsOf(t, h.journal, amend.ID) {
		if event.Type == EventRunCreated {
			created = event
		}
	}
	if created == nil || !payloadBool(created.Payload, "autoPlan") {
		t.Fatalf("run_created payload 应携带 autoPlan=true: %+v", created)
	}

	// 同 id 重放换 autoPlan（同形空计划、语义不同）：身份比对拒绝。
	if _, _, err := h.service.CreateAmendRun(h.team, "run-ap", "run-amend-ap",
		"再规划一轮", ModeSingle, "", nil, nil, false); !errors.Is(err, ErrRunContentMismatch) {
		t.Fatalf("autoPlan 属创建身份，同 id 异语义应报内容不一致，实际 %v", err)
	}

	// 显式计划：AutoPlan=false（显式 Plan 优先）。
	explicit, _, err := h.service.CreateAmendRun(h.team, "run-ap", "run-amend-explicit",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), false)
	if err != nil {
		t.Fatalf("显式计划修订应成功: %v", err)
	}
	if explicit.AutoPlan {
		t.Fatalf("显式计划修订不得落 AutoPlan: %+v", explicit)
	}

	// 互斥：autoPlan 与显式计划同时携带直接拒绝（CreateRun 同款）。
	if _, _, err := h.service.CreateAmendRun(h.team, "run-ap", "run-amend-mixed",
		amendGoal, ModePipeline, "", nil, pipelinePlan(), true); err == nil ||
		!strings.Contains(err.Error(), "自动规划的 Run 不能携带显式计划") {
		t.Fatalf("autoPlan + 显式计划应被拒绝，实际 %v", err)
	}
}
