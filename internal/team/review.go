package team

// review.go 是只读 Review 的证据面（docs/team-overall-design.md §3.3/§8
// 阶段三）：评审成员的结果必须带显式裁决，Agent 的「完成了」不能替代
// Review 通过——任务 completed 只说明回合正常结束，裁决另行记账。
//   - ReviewerMemberID 解析评审成员（ReviewerID 优先，回退首个启用的
//     role=reviewer 成员）；
//   - 评审任务默认只读（WriteLock=none，§6 「Review 使用只读锁」），显式
//     声明 exclusive 的保持原值（用户/Lead 的明确意图优先）；review=false
//     把默认评审成员的任务明确标为普通执行，不注入审查协议；
//   - BuildReviewBriefing 在派发文本前注入裁决协议，ExtractReviewVerdict
//     只认回复最后一个非空行的裁决标记（转述协议的中间行不误判）。

import (
	"fmt"
	"strconv"
	"strings"
)

// 评审裁决取值：passed/failed 来自评审回复末行标记；unknown 表示回合结束
// 但未给出可识别的裁决（不得按通过处理）。
const (
	ReviewVerdictPassed  = "passed"
	ReviewVerdictFailed  = "failed"
	ReviewVerdictUnknown = "unknown"

	reviewVerdictOpenTag  = "<team-review>"
	reviewVerdictCloseTag = "</team-review>"
)

// ReviewerMemberID 返回团队的评审成员 ID；没有评审维度返回空串。
func (t *Team) ReviewerMemberID() string {
	if t == nil {
		return ""
	}
	if t.ReviewerID != "" {
		return t.ReviewerID
	}
	for i := range t.Members {
		if t.Members[i].Enabled && t.Members[i].Role == RoleReviewer {
			return t.Members[i].ID
		}
	}
	return ""
}

func reviewFlag(value bool) *bool { return &value }

// IsReviewTask 判定任务是否承担评审维度。review=false 是执行任务的显式标记；
// 旧任务缺省 review 时保持原语义——默认评审成员承担的都是审查任务。
func IsReviewTask(team *Team, task *Task) bool {
	if team == nil || task == nil || task.Reduction || strings.HasPrefix(task.ID, actionRepairPrefix) ||
		task.Review != nil && !*task.Review {
		return false
	}
	reviewer := team.ReviewerMemberID()
	return reviewer != "" && reviewer != team.LeadID && task.MemberID == reviewer
}

// ReviewIndependence 报告审查者是否独立于全部被审任务；引用缺失时不作判断。
func ReviewIndependence(run *Run, teamRow *Team, task *Task) *bool {
	if !IsReviewTask(teamRow, task) || len(task.ReviewOf) == 0 {
		return nil
	}
	independent := true
	for _, taskID := range task.ReviewOf {
		reviewed := findTaskIn(run, taskID)
		if reviewed == nil {
			return nil
		}
		if reviewed.MemberID == task.MemberID {
			independent = false
		}
	}
	return &independent
}

// planInputIsReview 判定计划条目是否承担审查语义（IsReviewTask 的创建期
// 口径）：review=true 显式；未声明时默认评审成员承接的条目按审查兼容。
func planInputIsReview(team *Team, input PlanTaskInput) bool {
	if input.Review != nil {
		return *input.Review
	}
	reviewer := team.ReviewerMemberID()
	return reviewer != "" && reviewer != team.LeadID && input.MemberID == reviewer
}

// defaultReviewWriteLock 归一评审任务的缺省写锁：未声明时只读。
func defaultReviewWriteLock(team *Team, memberID, writeLock string, review ...*bool) string {
	if writeLock != "" {
		return writeLock
	}
	execution := len(review) > 0 && review[0] != nil && !*review[0]
	if reviewer := team.ReviewerMemberID(); !execution && reviewer != "" && reviewer != team.LeadID && memberID == reviewer {
		return TaskWriteLockNone
	}
	// 方案顾问只出方案，缺省同样只读。
	if memberID != team.LeadID && team.memberRole(memberID) == RoleAdvisor {
		return TaskWriteLockNone
	}
	return ""
}

// BuildReviewBriefing 返回评审任务的裁决协议；非评审任务返回空串。内容只
// 依赖常量，保证派发文本与 inputHash 确定。
func BuildReviewBriefing(team *Team, task *Task) string {
	if !IsReviewTask(team, task) {
		return ""
	}
	// 分级口径：只有可复现的阻塞问题才判不通过，建议不触发返工——否则审查会
	// 在边缘情况上无限往复（每轮总能再挑出几条）。
	briefing := "[Team review]\n你是本团队的评审成员，本任务只做审查，不要修改文件。" +
		"把发现的问题分两节列出：「阻塞问题」只收能写出具体触发场景（什么输入或状态 → 产生什么错误结果）、" +
		"且影响正确性、数据、安全或验收条件的问题，每条附文件位置与场景；" +
		"风格、命名、可选优化、难以触发的极端情况和仅凭推测的风险一律列为「建议」。" +
		"不要为了显得严格把建议升级为阻塞问题。" +
		"审查结束后，请在回复的最后一行单独输出裁决：没有阻塞问题时写 " +
		reviewVerdictOpenTag + ReviewVerdictPassed + reviewVerdictCloseTag +
		"（建议照常列出，不构成返工理由），有阻塞问题时写 " + reviewVerdictOpenTag + ReviewVerdictFailed + reviewVerdictCloseTag +
		"。\n"
	// 版本绑定说明（阶段 5，§6）：声明 reviewOf 的审查告知被审对象与绑定
	// 语义——裁决基于当前工作区实际内容；之后的再次写入会使本裁决过期。
	if task != nil && len(task.ReviewOf) > 0 {
		briefing += "本任务审查以下实现任务的结果与当前工作区的实际改动：" +
			strings.Join(task.ReviewOf, "、") +
			"。请以你实际查看的当前工作区内容为准；本裁决绑定当前工作区版本，之后发生的改动会使它过期，需重新审查。\n"
	}
	return briefing
}

// ExtractReviewVerdict 从评审回复全文解析裁决：只看最后一个非空行，且该行
// 去空白后必须恰好是一个裁决标记；其余形态一律 unknown。
func ExtractReviewVerdict(fullText string) string {
	lines := strings.Split(strings.TrimSpace(fullText), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !strings.HasPrefix(last, reviewVerdictOpenTag) || !strings.HasSuffix(last, reviewVerdictCloseTag) {
		return ReviewVerdictUnknown
	}
	value := strings.TrimSpace(last[len(reviewVerdictOpenTag) : len(last)-len(reviewVerdictCloseTag)])
	switch strings.ToLower(value) {
	case ReviewVerdictPassed:
		return ReviewVerdictPassed
	case ReviewVerdictFailed:
		return ReviewVerdictFailed
	}
	return ReviewVerdictUnknown
}

// ---- 审查格式修复与交付事实简报（docs/team-demand-queue-plan.md §6/§8 阶段 0）----
//
// 2026-11-07 的真实 Run 暴露两类交付风险：Reviewer 回合结束但末行没有裁决
// 标记，交付投影为 unknown 而 Lead 摘要却写「独立审查未发现问题」。这里补两
// 道机械防线：unknown 给 Reviewer 一次有界修复机会（revfix-*，只重试一次，
// 次数与结果随 task_planned/task_completed 入账）；汇总任务派发前注入结构化
// 交付事实，Lead 只能撰写说明，不能把 unknown 表述为通过。

const (
	// reviewRepairPrefix 是审查格式修复任务 ID 前缀（revfix-{taskID}-{n}）。
	reviewRepairPrefix = "revfix-"
	// maxReviewRepairs 是 unknown 裁决的有界修复次数：只重试一次，仍 unknown
	// 不再派单，交付保持「未取得裁决」。
	maxReviewRepairs = 1
)

// reviewRepairPrompt 是修复任务的确定性提示词（不含易变字段，同输入恒等，
// InputHash 语义与 RepairPrompt 同根）。
const reviewRepairPrompt = "你上一条审查回复的最后一行没有可识别的裁决标记，编排器无法确认审查结论。请补充给出最终裁决：通过写 <team-review>passed</team-review>，不通过写 <team-review>failed</team-review>，并在裁决行之前用一两句话说明依据。除说明与裁决外不要输出其他内容。"

// reviewRepairPlannedKey 是修复任务 task_planned 事实的幂等键（actionRepair
// PlannedKey 同款拼装）。
func reviewRepairPlannedKey(teamID, runID, repairTaskID string) string {
	return fmt.Sprintf("team:%s:run:%s:revfix:%s", teamID, runID, repairTaskID)
}

// countReviewRepairs 统计原审查任务已有的修复回合数（revfix-{taskID}- 前缀
// 且余段为纯整数——countActionRepairs 同款口径）。修复任务随快照持久化，
// 计数跨重启存活。
func countReviewRepairs(run *Run, reviewTaskID string) int {
	if run == nil {
		return 0
	}
	prefix := reviewRepairPrefix + reviewTaskID + "-"
	count := 0
	for i := range run.Tasks {
		rest := strings.TrimPrefix(run.Tasks[i].ID, prefix)
		if rest == run.Tasks[i].ID {
			continue
		}
		if _, err := strconv.Atoi(rest); err == nil {
			count++
		}
	}
	return count
}

// buildReviewRepairTask 在原审查任务刚落 completed 且裁决 unknown 时构造
// 修复任务（成员仍是该 Reviewer、只读、依赖原审查任务——依赖已终态，立即可
// 派发）。修复任务本身不再派生修复（前缀守卫），防 revfix-revfix-* 无限链。
// 已达修复上限、原任务不承担审查语义、ID 超长/占用或 Run 任务数满时返回
// false（确定性收场为 unknown，不循环派单）。
func buildReviewRepairTask(run *Run, team *Team, reviewTaskID string) (Task, bool) {
	if swarmReviewArtifact(run, reviewTaskID) != nil {
		return Task{}, false
	}
	if run == nil || team == nil || strings.HasPrefix(reviewTaskID, reviewRepairPrefix) {
		return Task{}, false
	}
	review := findTaskIn(run, reviewTaskID)
	if review == nil || !IsReviewTask(team, review) {
		return Task{}, false
	}
	attempt := countReviewRepairs(run, reviewTaskID) + 1
	if attempt > maxReviewRepairs {
		return Task{}, false
	}
	if limit := team.Policy.MaxTasksPerRun; limit > 0 && len(run.Tasks) >= limit {
		return Task{}, false
	}
	id := fmt.Sprintf("%s%s-%d", reviewRepairPrefix, reviewTaskID, attempt)
	if !ValidMemberID(id) || findTaskIn(run, id) != nil {
		return Task{}, false
	}
	ordinal := 0
	for i := range run.Tasks {
		if run.Tasks[i].Ordinal > ordinal {
			ordinal = run.Tasks[i].Ordinal
		}
	}
	task := Task{
		ID: id, MemberID: review.MemberID, Prompt: reviewRepairPrompt,
		DependsOn: []string{reviewTaskID}, WriteLock: TaskWriteLockNone,
		Review: reviewFlag(true), Ordinal: ordinal + 1, Status: TaskPlanned,
		InstructionsHead: InstructionsHead(reviewRepairPrompt), WorkspaceMode: WorkspaceShared,
	}
	if task.MemberID == team.LeadID {
		task.ExecutionContext = ExecutionContextLeadRun
	}
	if review.WorkflowID != "" {
		task.WorkflowID, task.ExecutionThreadID = review.WorkflowID, review.ExecutionThreadID
		task.WorkspaceID, task.WorkspaceMode = review.WorkspaceID, review.WorkspaceMode
	}
	task.InputHash = InputHashFor(&task, PolicyDigest(team.Policy), "")
	return task, true
}

// BuildDeliveryBriefing 为汇总（Reduction）任务注入结构化交付事实（§6
// 「Lead 只撰写说明，不能覆盖网关的 unknown/stale/failed」）：逐审查任务
// 报出编排器记账的机器裁决，并明确 unknown 的表述义务；没有审查任务时强制
// 摘要写明未安排独立审查。内容只依赖 Run 快照（零 I/O、无易变字段），同
// 快照恒等，进入派发期 inputHash。
func BuildDeliveryBriefing(team *Team, run *Run) string {
	if team == nil || run == nil {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("[Team delivery]\n以下是编排器记账的交付事实，你的汇总必须与其一致，不能改写机器结论：\n")
	found := false
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if !IsReviewTask(team, task) {
			continue
		}
		found = true
		verdict := ReviewVerdictUnknown
		if task.Status == TaskCompleted && task.ResultDetail != nil && task.ResultDetail.ReviewVerdict != "" {
			verdict = task.ResultDetail.ReviewVerdict
		}
		switch verdict {
		case ReviewVerdictPassed:
			fmt.Fprintf(&builder, "- 审查任务 %s：已完成，机器裁决=已通过。\n", task.ID)
		case ReviewVerdictFailed:
			fmt.Fprintf(&builder, "- 审查任务 %s：已完成，机器裁决=未通过。\n", task.ID)
		default:
			fmt.Fprintf(&builder, "- 审查任务 %s（状态 %s）：机器裁决=未取得裁决。摘要只能表述为「未取得裁决」，不得写成已通过或未通过。\n", task.ID, task.Status)
		}
		if note := readOnlyWriteNote(team, task); note != "" {
			builder.WriteString("  " + note + "\n")
		}
	}
	if !found {
		builder.WriteString("- 本 Run 未安排独立审查任务。摘要必须明确写「未安排独立审查」，不得声称已经过审查。\n")
	}
	builder.WriteString("你的自然语言总结不能覆盖以上机器状态。\n")
	return builder.String()
}
