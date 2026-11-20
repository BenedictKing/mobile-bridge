package team

import (
	"fmt"
	"strings"
)

// continuationBriefing 为控制回合提供有界血缘摘要，不复制或执行旧动作信封。
// 它与其他 Lead 简报一起进入派发哈希，并由 Task.SentText 固定首次派发内容。
func (s *Service) continuationBriefing(run *Run, task *Task, policy TeamPolicy) string {
	if run == nil || task == nil || run.ParentRunID == "" || task.IssueClassificationOnly || task.Reduction || task.ExecutionContext == ExecutionContextLeadRun || policy.MaxContextChars <= 0 {
		return ""
	}
	var ancestors []*Run
	seen := map[string]bool{run.ID: true}
	for id := run.ParentRunID; id != "" && len(ancestors) < 8 && !seen[id]; {
		seen[id] = true
		parent := s.store.Run(id)
		if parent == nil || parent.TeamID != run.TeamID {
			break
		}
		ancestors = append(ancestors, parent)
		id = parent.ParentRunID
	}
	if len(ancestors) == 0 {
		return ""
	}
	budget := policy.MaxContextChars
	if budget > 6000 {
		budget = 6000
	}
	var builder strings.Builder
	builder.WriteString("\n[Team continuation]\n这是既有团队对话的续轮，以下是前轮目标与实际执行记录。completed 只表示执行轮结束，不能据此认定目标已实现；请核对可读交付、执行与验证证据。资料补充或状态说明不自动撤销未完成目标；最新用户指令优先，明确停止、改目标或提出独立任务时按新指令处理。只对本轮原文做 Issue 归类，历史原文仅作上下文。\n")
	for index := len(ancestors) - 1; index >= 0; index-- {
		parent := ancestors[index]
		completed, total := 0, 0
		lastReply := ""
		for _, oldTask := range parent.Tasks {
			if !oldTask.IssueClassificationOnly && !oldTask.Reduction && (oldTask.ExecutionContext == ExecutionContextLeadRun || oldTask.MemberID != task.MemberID || !parent.AutoPlan) {
				total++
				if oldTask.Status == TaskCompleted {
					completed++
				}
			}
			if oldTask.Status == TaskCompleted {
				text := oldTask.Result
				if oldTask.ResultDetail != nil {
					text = oldTask.ResultDetail.Result
				}
				if readable := ResultDisplayText(text); readable != "" {
					lastReply = readable
				}
			}
		}
		if lastReply == "" {
			lastReply = "无可读交付或回复"
		}
		fmt.Fprintf(&builder, "\n前轮 runId=%s status=%s 实际执行任务=%d/%d\n原目标：%s\n最近可读结果：%s\n",
			parent.ID, parent.Status, completed, total, boundedContinuationText(parent.Goal, 1400), boundedContinuationText(lastReply, 400))
		if parent.StopReason != "" {
			fmt.Fprintf(&builder, "结束原因：%s\n", boundedContinuationText(parent.StopReason, 240))
		}
	}
	return boundedContinuationText(builder.String(), budget)
}

// 保留头尾，避免长目标末尾的执行指令被单纯截取开头丢掉。
func boundedContinuationText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	const note = "\n[血缘上下文已截断；请勿推断缺失内容]\n"
	remaining := limit - len([]rune(note))
	if remaining <= 0 {
		return string([]rune(note)[:limit])
	}
	head := (remaining + 1) / 2
	return string(runes[:head]) + note + string(runes[len(runes)-(remaining-head):])
}
