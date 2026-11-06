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

import "strings"

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

// defaultReviewWriteLock 归一评审任务的缺省写锁：未声明时只读。
func defaultReviewWriteLock(team *Team, memberID, writeLock string, review ...*bool) string {
	if writeLock != "" {
		return writeLock
	}
	execution := len(review) > 0 && review[0] != nil && !*review[0]
	if reviewer := team.ReviewerMemberID(); !execution && reviewer != "" && reviewer != team.LeadID && memberID == reviewer {
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
	return "[Team review]\n你是本团队的评审成员，本任务只做审查，不要修改文件。" +
		"审查结束后，请在回复的最后一行单独输出裁决：通过写 " +
		reviewVerdictOpenTag + ReviewVerdictPassed + reviewVerdictCloseTag +
		"，不通过写 " + reviewVerdictOpenTag + ReviewVerdictFailed + reviewVerdictCloseTag +
		"，并在前文列出问题。\n"
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
