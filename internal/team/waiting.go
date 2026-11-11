package team

import "strings"

type WaitingAction struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	ReasonCode string `json:"reasonCode"`
	Actor      string `json:"actor"`
	Reason     string `json:"reason"`
}

func RunWaitingAction(run *Run) WaitingAction {
	action := WaitingAction{Kind: "inspect", Label: "查看阻塞原因", ReasonCode: "blocked", Actor: "user", Reason: run.StopReason}
	set := func(kind, label, code string) WaitingAction {
		action.Kind, action.Label, action.ReasonCode = kind, label, code
		return action
	}
	for _, task := range run.Tasks {
		if task.Status == TaskOutcomeUnknown {
			return set("recheck", "核查原会话结果", "outcome_unknown")
		}
	}
	for _, question := range run.PendingQuestions {
		if question.Status == QuestionStatusOpen {
			return set("answer", "回答成员提出的问题", "question")
		}
	}
	if strings.HasPrefix(run.StopReason, "budget_exhausted") {
		return set("resume", "追加预算后继续", "budget")
	}
	if strings.HasPrefix(run.StopReason, "verify_failed") {
		return set("verify", "查看验证失败并重新验证", "verification_failed")
	}
	for _, task := range run.Tasks {
		if task.Status != TaskStopped {
			continue
		}
		switch task.Outcome {
		case "auth":
			return set("configure", "处理成员认证后继续", "authentication")
		case "quota":
			return set("configure", "处理成员额度后继续", "quota")
		case "not_configured":
			return set("configure", "配置成员后继续", "configuration")
		}
	}
	return action
}
