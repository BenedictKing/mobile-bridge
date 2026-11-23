package main

// Jev runtime hooks: the audit log every approval decision lands in, and the
// auto-continue driver that resumes unfinished turns. Logs are single-line
// and greppable (make logs / jev); the API key never appears and command
// text is capped to a prefix so full commands stay out of the log.

import (
	"log"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// watchedWithin bounds how stale the last gateway-side read of a session may
// be before auto-continue stops resuming it: an open page or reminder poll
// touches the session every few seconds, so nobody-watching chats settle
// silently instead of burning through the resume budget unseen.
const watchedWithin = 2 * time.Minute

// jevApprovalHook wraps one Jev evaluation with the session approval cache,
// the decision journal and the decision audit log. Cards Jev assessed but did
// not approve come back annotated so the phone shows the verdict; a failed
// assessment with ladder rungs left asks the runtime to retry.
func jevApprovalHook(settings *jev.Settings, agent string) acpruntime.ApprovalHook {
	return func(sessionID, requestID, method string, params *jsoncompat.Object, attempt int) *acpruntime.ApprovalVerdict {
		decision := jevEvaluate(settings, agent, sessionID, requestID, method, params, attempt)
		verdict := &acpruntime.ApprovalVerdict{Response: decision.Response, Retry: decision.Retrying}
		if annotation := jev.Annotate(method, params, decision); annotation != nil {
			verdict.Assessment = annotation.Assessment
			verdict.RecommendOption = annotation.RecommendOption
		}
		return verdict
	}
}

// jevCodexApprovalHook is the Codex bridge's response-only variant: Codex
// re-normalizes its desktop snapshot on every read, so there is no pending
// card to annotate yet. A nil reply releases the claim, so the next snapshot
// change evaluates the card again.
func jevCodexApprovalHook(settings *jev.Settings) func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object {
	return func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object {
		return jevEvaluate(settings, "codex", sessionID, requestID, method, params, 0).Response
	}
}

func jevEvaluate(settings *jev.Settings, agent, sessionID, requestID, method string, params *jsoncompat.Object, attempt int) jev.Decision {
	decision := settings.EvaluateSessionAttempt(agent, sessionID, requestID, method, params, attempt)
	response := "-"
	if decision.Response != nil {
		response = string(jsoncompat.EncodeCompact(decision.Response))
	}
	failure := decision.Error
	if failure == "" {
		failure = "-"
	}
	log.Printf("Jev 审批: agent=%s session=%s request=%s method=%s 尝试=%d 结果=%s 错误=%s 和=%.2f 阈值=%.2f 可逆=%.2f 置信=%.2f 建议=%s 回应=%s 重试=%t 请求=%q",
		agent, sessionID, requestID, method, attempt+1, decision.Reason, failure,
		decision.NegativeSum, decision.Threshold, decision.Reversible, decision.Confidence, decision.Recommend,
		response, decision.Retrying, jev.SubjectPrefix(params))
	return decision
}

// jevContinueHook resumes a settled turn when the persisted auto-continue
// policy and (with a key) the Jev completeness verdict both say yes. It runs
// on the turn-settle goroutine, so the 4s assessment budget is acceptable.
func jevContinueHook(settings *jev.Settings, agent string) func(*acpruntime.Session) {
	return jevContinueHookMode(settings, agent, false)
}

// jevContinueAfterSwitchHook runs one explicit assessment after quota failover.
// The old turn's quota error is resolved context, not a reason to skip Jev.
func jevContinueAfterSwitchHook(settings *jev.Settings, agent string) func(*acpruntime.Session) {
	return jevContinueHookMode(settings, agent, true)
}

func jevContinueHookMode(settings *jev.Settings, agent string, afterSwitch bool) func(*acpruntime.Session) {
	return func(session *acpruntime.Session) {
		if settings == nil || !settings.ContinueEnabled() {
			return
		}
		if session.IdleFor() > watchedWithin {
			// Documented scope: chats someone is watching — an open page or
			// an active poll — not sessions merely list-polled.
			return
		}
		snapshot := session.Snapshot()
		if requests, _ := snapshot.MustGet("requests").(*jsoncompat.Array); requests != nil && len(requests.Items) > 0 {
			// Pending approvals or questions own the next step.
			return
		}
		context, ok := jev.ContinueContext(agent, snapshot)
		if !ok {
			return
		}
		if afterSwitch {
			context.Set("recoveryNote", "The previous account's quota was exhausted and the account has now been switched successfully. Assess whether the original task should continue on the new account.")
		} else if jev.QuotaStop(jsoncompat.PyStr(context.MustGet("stopReason")), jsoncompat.PyStr(context.MustGet("error"))) {
			log.Printf("Jev 自动继续跳过（疑似额度不足）: agent=%s session=%s turn=%s",
				agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")))
			return
		}
		var assessment *jev.ContinueAssessment
		if settings.ContinueKeyPresent() {
			assessment = settings.ContinueAssess(context)
		}
		decide := jev.DecideContinue
		if afterSwitch {
			decide = jev.DecideContinueAfterSwitch
		}
		send, reason := decide(context, assessment)
		if !send {
			log.Printf("Jev 自动继续未触发（%s）: agent=%s session=%s turn=%s",
				reason, agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")))
			return
		}
		if session.IdleFor() > watchedWithin {
			return
		}
		turnID := jsoncompat.PyStr(context.MustGet("turnId"))
		var err error
		if afterSwitch {
			err = session.ResumeAfterSwitchPromptIfCurrent(turnID, "继续", time.Hour)
		} else {
			err = session.AutoContinuePromptIfCurrent(turnID, "继续", time.Hour)
		}
		if err != nil {
			log.Printf("Jev 自动继续发送失败: agent=%s session=%s turn=%s error=%s",
				agent, session.ID, turnID, err.Error())
			return
		}
		log.Printf("Jev 自动继续已发送: agent=%s session=%s turn=%s 原因=%s",
			agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")), reason)
	}
}
