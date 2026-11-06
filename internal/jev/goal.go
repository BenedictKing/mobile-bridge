package jev

import (
	"errors"
	"math"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const goalMinConfidence = 0.80

type GoalContext struct {
	Objective       string
	Agent           string
	GoalID          string
	Revision        int64
	Step            int
	TurnID          string
	UserPrompt      string
	AssistantTail   string
	StopReason      string
	Error           string
	PendingRequests bool
}

type GoalAssessment struct {
	Complete   bool
	Continue   string
	StopKind   string
	Confidence float64
}

func (c GoalContext) state() *jsoncompat.Object {
	state := jsoncompat.NewObject()
	state.Set("objective", goalLimit(c.Objective, 4000))
	state.Set("agent", c.Agent)
	state.Set("goalId", c.GoalID)
	state.Set("revision", jsoncompat.Num(int(c.Revision)))
	state.Set("step", jsoncompat.Num(c.Step))
	state.Set("turnId", c.TurnID)
	state.Set("userPrompt", goalLimit(c.UserPrompt, 4000))
	state.Set("assistantTail", goalLimit(c.AssistantTail, 6000))
	state.Set("stopReason", goalLimit(c.StopReason, 200))
	state.Set("error", goalLimit(c.Error, 1500))
	state.Set("pendingRequests", c.PendingRequests)
	return state
}

func goalLimit(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// GoalAssess is intentionally independent of the interruption-specific
// ContinueAssess path. A nil verdict always requires a human review.
func (s *Settings) GoalAssess(context GoalContext) (*GoalAssessment, error) {
	s.mu.Lock()
	config := s.effectiveLocked()
	s.mu.Unlock()
	key, _ := config.MustGet("apiKey").(string)
	if key == "" {
		return nil, errors.New("Jev API key is not configured")
	}
	payload := jsoncompat.NewObject()
	payload.Set("model", config.MustGet("model"))
	payload.Set("state", context.state())
	payload.Set("questions", goalQuestions())
	value, err := s.post(config, key, payload)
	if err != nil {
		return nil, err
	}
	root, _ := value.(*jsoncompat.Object)
	if root == nil {
		return nil, errors.New("invalid Goal assessment")
	}
	answers, _ := root.MustGet("answers").(*jsoncompat.Object)
	if answers == nil {
		return nil, errors.New("missing Goal answers")
	}
	choice := func(name string) string {
		row, _ := answers.MustGet(name).(*jsoncompat.Object)
		if row == nil {
			return ""
		}
		value, _ := row.MustGet("choice").(string)
		return value
	}
	complete, next, kind := choice("complete"), choice("continue"), choice("stop_kind")
	confidenceAnswer, _ := answers.MustGet("confidence").(*jsoncompat.Object)
	if confidenceAnswer == nil {
		return nil, errors.New("missing Goal confidence")
	}
	confidence := number(confidenceAnswer.MustGet("noul"), math.NaN())
	if (complete != "yes" && complete != "no") || (next != "continue" && next != "stop" && next != "review") ||
		(kind != "finished" && kind != "interrupted" && kind != "quota" && kind != "awaiting_user" && kind != "refusal" && kind != "other") ||
		math.IsNaN(confidence) || math.IsInf(confidence, 0) || confidence < 0 || confidence > 1 ||
		(complete == "yes" && next == "continue") || (complete == "yes" && kind != "finished") {
		return nil, errors.New("invalid or contradictory Goal assessment")
	}
	return &GoalAssessment{Complete: complete == "yes", Continue: next, StopKind: kind, Confidence: confidence}, nil
}

// DecideGoal never automatically advances after an error, an interruption,
// a user stop, or an unresolved reverse request.
func DecideGoal(context GoalContext, assessment *GoalAssessment) (string, string) {
	if context.PendingRequests {
		return "waiting_input", "pending_input"
	}
	if QuotaStop(context.StopReason, context.Error) {
		return "blocked", "quota"
	}
	if context.StopReason == "cancelled" || context.StopReason == "refusal" || context.Error != "" {
		return "blocked", "agent_stop"
	}
	if context.StopReason != "end_turn" {
		return "waiting_review", "unexpected_stop"
	}
	if assessment == nil {
		return "waiting_review", "jev_unavailable"
	}
	if assessment.StopKind == "quota" || assessment.StopKind == "refusal" || assessment.StopKind == "awaiting_user" {
		return "blocked", assessment.StopKind
	}
	if assessment.Complete && assessment.Continue == "stop" && assessment.StopKind == "finished" && assessment.Confidence >= goalMinConfidence {
		return "completed", "jev_complete"
	}
	if !assessment.Complete && assessment.Continue == "continue" && assessment.StopKind == "finished" && assessment.Confidence >= goalMinConfidence {
		return "dispatching", "jev_continue"
	}
	return "waiting_review", "jev_review"
}

func goalQuestions() *jsoncompat.Object {
	question := func(instructions string, criteria map[string]string) *jsoncompat.Object {
		q := jsoncompat.NewObject()
		q.Set("type", "choice")
		q.Set("instructions", instructions)
		choices := jsoncompat.NewObject()
		for key, value := range criteria {
			choices.Set(key, value)
		}
		q.Set("criteria", choices)
		return q
	}
	questions := jsoncompat.NewObject()
	questions.Set("complete", question("Is the original objective fully achieved?", map[string]string{"yes": "All requested work is complete", "no": "Work remains"}))
	questions.Set("continue", question("Should the gateway send exactly one more prompt for this objective?", map[string]string{"continue": "Incomplete and safe to continue", "stop": "Complete or blocked", "review": "Needs human decision"}))
	questions.Set("stop_kind", question("Why did the agent stop?", map[string]string{"finished": "Normal end of turn", "interrupted": "Unexpected interruption", "quota": "Quota or billing limit", "awaiting_user": "Needs user input", "refusal": "Agent refusal", "other": "Unclear"}))
	confidence := jsoncompat.NewObject()
	confidence.Set("type", "noul")
	confidence.Set("instructions", "Confidence in this assessment, from zero to one")
	questions.Set("confidence", confidence)
	return questions
}
