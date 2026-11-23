package team

// advisor.go 是方案顾问角色与成员任务约定：
//   - role=advisor 的成员只出独立方案或意见，任务缺省只读（WriteLock=none）；
//   - 顾问任务由网关自动附上用户原话，避免只看到 Lead 转述而失去独立性；
//   - 提问约定（先做完不依赖答案的部分，提问带推荐项与后果）随顾问协议注入，
//     普通成员由 Lead 在委派 task 中写明。
// 只读目前是提示与写锁层面的约束；实际写入由交付事实警示（readOnlyWriteNote），
// 不切换 Agent 的 plan 模式——Claude 在 plan 模式收尾常调用 ExitPlanMode，
// 会生成待批准请求并卡住团队任务。

import (
	"fmt"
	"strings"
)

// RoleAdvisor 是只读方案顾问角色。
const RoleAdvisor = "advisor"

const (
	advisorOriginalLimit = 4000
	memberQuestionRule   = "需要用户决定时，先完成不依赖答案的部分再提问；提问时把推荐选项放在第一位并标注（推荐），写明各选项的后果。"
)

// memberRole 返回成员角色；成员不存在时返回空串。
func (t *Team) memberRole(memberID string) string {
	if t == nil {
		return ""
	}
	for i := range t.Members {
		if t.Members[i].ID == memberID {
			return t.Members[i].Role
		}
	}
	return ""
}

// HasAdvisor 报告团队是否有启用的方案顾问。
func (t *Team) HasAdvisor() bool {
	if t == nil {
		return false
	}
	for i := range t.Members {
		if t.Members[i].Enabled && t.Members[i].Role == RoleAdvisor && t.Members[i].ID != t.LeadID {
			return true
		}
	}
	return false
}

// IsAdvisorTask 判定任务是否由方案顾问承担（汇总、修复与审查任务除外）。
func IsAdvisorTask(team *Team, task *Task) bool {
	if team == nil || task == nil || task.Reduction || task.MemberID == team.LeadID ||
		strings.HasPrefix(task.ID, actionRepairPrefix) || strings.HasPrefix(task.ID, reviewRepairPrefix) || IsReviewTask(team, task) {
		return false
	}
	return team.memberRole(task.MemberID) == RoleAdvisor
}

// BuildMemberBriefing 返回顾问任务的只读协议与用户原话；其他任务返回空串——
// 普通成员（含广播与显式计划）的任务原文须原样送达，提问约定由 Lead 写进 task。
// 内容只依赖 Run 快照，保证派发文本与 inputHash 确定。
func BuildMemberBriefing(team *Team, run *Run, task *Task) string {
	if IsAdvisorTask(team, task) {
		briefing := "[Team advisor]\n你是本团队的方案顾问，本任务只提供独立方案或意见，不要修改文件，也不要执行会改变状态的命令。" +
			"请基于下方用户原话独立判断，不要只附和任务描述里的既有思路；结论写清推荐方案、理由、主要风险与备选。" + memberQuestionRule + "\n"
		if run != nil {
			briefing += "[User original]\n" + truncateRunes(RunGoalScopeText(run), advisorOriginalLimit) + "\n"
		}
		return briefing
	}
	return ""
}

// readOnlyWriteNote 为只读任务（审查/顾问）记录到文件编辑时返回警示；FileSummary
// 含绝对路径，可能包括临时草稿，因此只作交付事实提示，不改写裁决。
func readOnlyWriteNote(team *Team, task *Task) string {
	if task == nil || task.ResultDetail == nil || strings.TrimSpace(task.ResultDetail.FileSummary) == "" ||
		!IsReviewTask(team, task) && !IsAdvisorTask(team, task) {
		return ""
	}
	return fmt.Sprintf("注意：该只读任务记录到文件编辑（%s），独立性存疑，需核实这些改动。", truncateRunes(task.ResultDetail.FileSummary, 300))
}
