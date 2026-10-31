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
// not approve come back annotated so the phone shows the verdict.
func jevApprovalHook(settings *jev.Settings, agent string) acpruntime.ApprovalHook {
	return func(sessionID, requestID, method string, params *jsoncompat.Object) *acpruntime.ApprovalVerdict {
		decision := jevEvaluate(settings, agent, sessionID, requestID, method, params)
		verdict := &acpruntime.ApprovalVerdict{Response: decision.Response}
		if annotation := jev.Annotate(method, params, decision); annotation != nil {
			verdict.Assessment = annotation.Assessment
			verdict.RecommendOption = annotation.RecommendOption
		}
		return verdict
	}
}

// jevCodexApprovalHook is the Codex bridge's response-only variant: Codex
// re-normalizes its desktop snapshot on every read, so there is no pending
// card to annotate yet.
func jevCodexApprovalHook(settings *jev.Settings) func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object {
	return func(sessionID, requestID, method string, params *jsoncompat.Object) *jsoncompat.Object {
		return jevEvaluate(settings, "codex", sessionID, requestID, method, params).Response
	}
}

func jevEvaluate(settings *jev.Settings, agent, sessionID, requestID, method string, params *jsoncompat.Object) jev.Decision {
	decision := settings.EvaluateSession(agent, sessionID, requestID, method, params)
	response := "-"
	if decision.Response != nil {
		response = string(jsoncompat.EncodeCompact(decision.Response))
	}
	log.Printf("Jev 审批: agent=%s session=%s request=%s method=%s 结果=%s 和=%.2f 阈值=%.2f 可逆=%.2f 置信=%.2f 建议=%s 回应=%s 请求=%q",
		agent, sessionID, requestID, method, decision.Reason,
		decision.NegativeSum, decision.Threshold, decision.Reversible, decision.Confidence, decision.Recommend,
		response, jev.SubjectPrefix(params))
	return decision
}

// jevContinueHook resumes a settled turn when the persisted auto-continue
// policy and (with a key) the Jev completeness verdict both say yes. It runs
// on the turn-settle goroutine, so the 4s assessment budget is acceptable.
func jevContinueHook(settings *jev.Settings, agent string) func(*acpruntime.Session) {
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
		if jev.QuotaStop(jsoncompat.PyStr(context.MustGet("stopReason")), jsoncompat.PyStr(context.MustGet("error"))) {
			log.Printf("Jev 自动继续跳过（疑似额度不足）: agent=%s session=%s turn=%s",
				agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")))
			return
		}
		var assessment *jev.ContinueAssessment
		if settings.ContinueKeyPresent() {
			assessment = settings.ContinueAssess(context)
		}
		send, reason := jev.DecideContinue(context, assessment)
		if !send {
			log.Printf("Jev 自动继续未触发（%s）: agent=%s session=%s turn=%s",
				reason, agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")))
			return
		}
		if session.IdleFor() > watchedWithin {
			return
		}
		if err := session.AutoContinuePrompt("继续", time.Hour); err != nil {
			log.Printf("Jev 自动继续发送失败: agent=%s session=%s turn=%s error=%s",
				agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")), err.Error())
			return
		}
		log.Printf("Jev 自动继续已发送: agent=%s session=%s turn=%s 原因=%s",
			agent, session.ID, jsoncompat.PyStr(context.MustGet("turnId")), reason)
	}
}
