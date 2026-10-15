package jev

import (
	"regexp"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Auto-continue: after a live turn settles, the gateway may send one
// automatic "继续" so the agent keeps working on unfinished tasks. A turn
// that settled on an exhausted quota or billing must never be resumed — every
// "继续" would bounce off the same paywall — so a wide deterministic keyword
// scan runs before and below the Jev classification: a false positive costs
// one skipped nudge, a false negative costs an error loop.

const (
	continueMinConfidence = 0.8
	continueTailLimit     = 6000
	continueTextLimit     = 4000
	continueErrorLimit    = 1500
)

var quotaStopRE = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`额度不足|额度已|余额不足|余额耗尽|余额已|积分不足|欠费|充值|配额不足|配额已`,
	`|用量限制|用量已达|用量超|达到.{0,6}上限|请求过于频繁|频率限制|限流`,
	// Separators must be real (space/hyphen): usage_limit / RateLimiter are
	// code identifiers, not paywall messages.
	`|usage[\s-]+limit|rate[\s-]+limit|rate-limit|quota|insufficient\s+credit`,
	`|out\s+of\s+credit|credit\s+balance|no\s+credits?\b|exceeded\s+your`,
	`|limit\s+reached|too\s+many\s+requests|payment\s+required|billing`,
	`|subscription\s+limit|\b402\b|\b429\b`,
}, ""))

// QuotaStop reports whether any stop-context text signals quota or billing
// exhaustion.
func QuotaStop(texts ...string) bool {
	for _, text := range texts {
		if text != "" && quotaStopRE.MatchString(text) {
			return true
		}
	}
	return false
}

// ContinueAssessment is Jev's verdict on one settled turn: whether the task
// is incomplete, why the agent stopped, and whether resuming is advisable.
// Nil pointer fields mean "not answered"; a nil assessment means Jev was not
// consulted (no key) or the API failed.
type ContinueAssessment struct {
	Incomplete     *bool
	StopKind       string
	ShouldContinue *bool
	Confidence     float64
}

// ContinueEnabled reports the persisted auto-continue switch.
func (s *Settings) ContinueEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effectiveLocked().MustGet("autoContinue") == true
}

// ContinueKeyPresent reports whether a Jev key is configured; without one
// only the deterministic interruption rule resumes turns.
func (s *Settings) ContinueKeyPresent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, _ := s.effectiveLocked().MustGet("apiKey").(string)
	return key != ""
}

// ContinueAssess asks Jev whether the settled turn's task is complete and
// should be resumed; nil means "no answer" (no key, transport or shape
// failure) and callers fall back to the deterministic rule.
func (s *Settings) ContinueAssess(context *jsoncompat.Object) *ContinueAssessment {
	s.mu.Lock()
	config := s.effectiveLocked()
	s.mu.Unlock()
	key, _ := config.MustGet("apiKey").(string)
	if key == "" {
		return nil
	}
	payload := jsoncompat.NewObject()
	payload.Set("model", config.MustGet("model"))
	payload.Set("state", context)
	payload.Set("questions", continueQuestions())
	value, ok := s.post(config, key, payload)
	if !ok {
		return nil
	}
	result, _ := value.(*jsoncompat.Object)
	answers, _ := result.MustGet("answers").(*jsoncompat.Object)
	if answers == nil {
		return nil
	}
	complete, _ := answers.MustGet("complete").(*jsoncompat.Object)
	kind, _ := answers.MustGet("stop_kind").(*jsoncompat.Object)
	resume, _ := answers.MustGet("continue").(*jsoncompat.Object)
	choice := jsoncompat.PyStr(resume.MustGet("choice"))
	if choice != "continue" && choice != "stop" && choice != "review" {
		return nil
	}
	assessment := &ContinueAssessment{Confidence: number(resume.MustGet("confidence"), 0)}
	if noul := number(complete.MustGet("noul"), -1); noul >= 0 {
		incomplete := noul < 0.5
		assessment.Incomplete = &incomplete
	}
	if stopKind := jsoncompat.PyStr(kind.MustGet("choice")); stopKind != "" {
		assessment.StopKind = stopKind
	}
	should := choice == "continue"
	assessment.ShouldContinue = &should
	return assessment
}

// DecideContinue applies the continuation policy: the deterministic quota
// scan always wins over Jev; a user stop is never undone; without an
// assessment only an unexpectedly interrupted turn resumes.
func DecideContinue(context *jsoncompat.Object, assessment *ContinueAssessment) (bool, string) {
	stopReason := jsoncompat.PyStr(context.MustGet("stopReason"))
	message := jsoncompat.PyStr(context.MustGet("error"))
	tail := truncateTail(jsoncompat.PyStr(context.MustGet("assistantTail")), 800)
	if QuotaStop(stopReason, message, tail) {
		return false, "quota"
	}
	status := jsoncompat.PyStr(context.MustGet("turnStatus"))
	if status == "interrupted" || stopReason == "cancelled" {
		// The user stopped the turn; resuming would undo their decision.
		return false, "cancelled"
	}
	interrupted := status == "failed" || stopReason == "max_tokens" || stopReason == "max_turns"
	if assessment == nil {
		if interrupted {
			return true, "interrupted"
		}
		return false, "undecided"
	}
	switch assessment.StopKind {
	case "quota":
		return false, "quota"
	case "awaiting_user":
		return false, "awaiting_user"
	}
	if assessment.Incomplete == nil {
		return false, "unsure"
	}
	if !*assessment.Incomplete {
		return false, "complete"
	}
	if assessment.ShouldContinue == nil || *assessment.ShouldContinue != true {
		if assessment.ShouldContinue != nil {
			return false, "jev_stop"
		}
		return false, "jev_review"
	}
	if assessment.Confidence < continueMinConfidence {
		return false, "low_confidence"
	}
	return true, "jev_continue"
}

// truncateTail keeps the last limit runes of a conversation tail.
func truncateTail(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[len(runes)-limit:])
}

func continueQuestions() *jsoncompat.Object {
	complete := jsoncompat.NewObject()
	complete.Set("type", "noul")
	complete.Set("instructions", "Based on this conversation tail, is the user's task fully completed?")
	completeCriteria := jsoncompat.NewObject()
	completeCriteria.Set("true", "The agent finished the requested work: the final message states or shows the outcome, and nothing requested is left open.")
	completeCriteria.Set("false", "Work remains: the agent stopped midway, the task was interrupted, the plan is only partly done, or the final message defers or truncates work.")
	complete.Set("criteria", completeCriteria)
	stopKind := jsoncompat.NewObject()
	stopKind.Set("type", "choice")
	stopKind.Set("instructions", "Why did the agent stop after this turn?")
	stopKindCriteria := jsoncompat.NewObject()
	stopKindCriteria.Set("finished", "The agent chose to end the turn because the task is done.")
	stopKindCriteria.Set("interrupted", "The turn was cut off unexpectedly: a timeout, process exit, output/token limit, or transport drop.")
	stopKindCriteria.Set("quota", "The account ran out of quota, credits, or rate limit; the message mentions usage limits, billing, or asks to upgrade or recharge.")
	stopKindCriteria.Set("awaiting_user", "The agent asked the user a question or needs more input before it can proceed.")
	stopKindCriteria.Set("other", "None of the above clearly applies.")
	stopKind.Set("criteria", stopKindCriteria)
	resume := jsoncompat.NewObject()
	resume.Set("type", "choice")
	resume.Set("instructions", `Should the gateway automatically send one "继续" (continue) message so the agent keeps working on this task?`)
	resumeCriteria := jsoncompat.NewObject()
	resumeCriteria.Set("continue", "The task is incomplete, the agent is able to make progress, and resuming is safe.")
	resumeCriteria.Set("stop", "Do not resume: the task is complete, blocked on the user, out of quota, or resuming would be unsafe or pointless.")
	resumeCriteria.Set("review", "Not confident enough to decide either way.")
	resume.Set("criteria", resumeCriteria)
	questions := jsoncompat.NewObject()
	questions.Set("complete", complete)
	questions.Set("stop_kind", stopKind)
	questions.Set("continue", resume)
	return questions
}

// ContinueContext compresses one settled turn of a session view into the
// PII-light description Jev judges and DecideContinue reads. ok is false
// when the view has no settled turn to judge (no turns, still in progress,
// or no messages at all).
func ContinueContext(agent string, view *jsoncompat.Object) (context *jsoncompat.Object, ok bool) {
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil || len(turns.Items) == 0 {
		return nil, false
	}
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if turn == nil {
		return nil, false
	}
	if jsoncompat.PyStr(turn.MustGet("status")) == "inProgress" {
		return nil, false
	}
	messages, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if messages == nil || len(messages.Items) == 0 {
		return nil, false
	}
	userRequest := ""
	for index := len(messages.Items) - 1; index >= 0; index-- {
		message, _ := messages.Items[index].(*jsoncompat.Object)
		if message != nil && jsoncompat.PyStr(message.MustGet("role")) == "user" &&
			jsoncompat.PyStr(message.MustGet("text")) != "" {
			userRequest = jsoncompat.PyStr(message.MustGet("text"))
			break
		}
	}
	// The tail is newest-first while walking backwards, then joined back into
	// chronological order like the Python gateway did.
	tail := []string{}
	tailSize := 0
	for index := len(messages.Items) - 1; index >= 0; index-- {
		message, _ := messages.Items[index].(*jsoncompat.Object)
		if message == nil {
			continue
		}
		if jsoncompat.PyStr(message.MustGet("kind")) != "agentMessage" {
			continue
		}
		text := jsoncompat.PyStr(message.MustGet("text"))
		if text == "" {
			continue
		}
		tail = append(tail, text)
		tailSize += len(text)
		if tailSize >= continueTailLimit {
			break
		}
	}
	errors := []string{}
	for _, value := range messages.Items {
		message, _ := value.(*jsoncompat.Object)
		if message != nil && jsoncompat.PyStr(message.MustGet("kind")) == "error" &&
			jsoncompat.PyStr(message.MustGet("text")) != "" {
			errors = append(errors, jsoncompat.PyStr(message.MustGet("text")))
		}
	}
	if text := jsoncompat.PyStr(turn.MustGet("error")); text != "" {
		errors = append(errors, text)
	}
	context = jsoncompat.NewObject()
	context.Set("agent", agent)
	context.Set("cwd", view.MustGet("cwd"))
	context.Set("turnId", turn.MustGet("id"))
	context.Set("turnStatus", turn.MustGet("status"))
	context.Set("stopReason", turn.MustGet("stopReason"))
	context.Set("error", truncateTail(strings.Join(errors, "\n"), continueErrorLimit))
	context.Set("userRequest", truncateRunes(userRequest, continueTextLimit))
	chronological := make([]string, 0, len(tail))
	for index := len(tail) - 1; index >= 0; index-- {
		chronological = append(chronological, tail[index])
	}
	context.Set("assistantTail", truncateTail(strings.Join(chronological, "\n"), continueTailLimit))
	return context, true
}
