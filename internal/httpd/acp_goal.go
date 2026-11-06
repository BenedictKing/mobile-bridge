package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpgoal"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

func (s *Server) gatewayGoalFor(kind, threadID string) (acpgoal.Goal, bool) {
	if s.ACPGoals == nil || kind == "mcode" {
		return acpgoal.Goal{}, false
	}
	if goal, ok := s.ACPGoals.ForThread(kind, threadID); ok {
		return goal, true
	}
	manager := s.ACPManager(kind)
	if manager == nil {
		return acpgoal.Goal{}, false
	}
	if live := manager.Peek(threadID); live != nil && live.ID != threadID {
		if goal, ok := s.ACPGoals.ForThread(kind, live.ID); ok {
			return goal, true
		}
	}
	for _, alias := range manager.Aliases(threadID) {
		if goal, ok := s.ACPGoals.ForThread(kind, alias); ok {
			return goal, true
		}
	}
	return acpgoal.Goal{}, false
}

func gatewayGoalView(goal acpgoal.Goal) *jsoncompat.Object {
	value := jsoncompat.NewObject()
	value.Set("provider", "gateway")
	value.Set("goalId", goal.GoalID)
	value.Set("objective", goal.Objective)
	value.Set("status", goal.Status)
	value.Set("revision", jsoncompat.Num(int(goal.Revision)))
	value.Set("autoStepsUsed", jsoncompat.Num(goal.AutoStepsUsed))
	value.Set("autoStepsLimit", jsoncompat.Num(goal.AutoStepsLimit))
	value.Set("stopReason", goal.StopReason)
	value.Set("pendingControl", goal.PendingControl)
	return value
}

func goalTip(view *jsoncompat.Object) (string, bool) {
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil || len(turns.Items) == 0 {
		return "", false
	}
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if turn == nil {
		return "", false
	}
	id, _ := turn.MustGet("id").(string)
	status, _ := turn.MustGet("status").(string)
	return id, status == "completed" || status == "failed" || status == "interrupted"
}

func (s *Server) dispatchGatewayGoal(session *acpruntime.Session, goal acpgoal.Goal, prompt string) (acpgoal.Goal, error) {
	if err := session.GoalPrompt(prompt, time.Hour); err != nil {
		// A failed ACP write can still have reached the agent. Do not retry it.
		_, _ = s.ACPGoals.MarkDispatched(goal.GoalID, goal.ActiveDispatchID, "", false)
		return acpgoal.Goal{}, err
	}
	turnID, _ := goalTip(session.Snapshot())
	accepted, err := s.ACPGoals.MarkDispatched(goal.GoalID, goal.ActiveDispatchID, turnID, true)
	if err != nil {
		return acpgoal.Goal{}, fmt.Errorf("Goal prompt sent but ledger confirmation failed: %w", err)
	}
	// An agent can settle before the accepted state is written.
	if id, settled := goalTip(session.Snapshot()); settled && id == turnID {
		go s.GoalSettleACP(goal.Agent, session)
	}
	return accepted, nil
}

func (h *handler) handleGatewayGoalCreate(kind, threadID string, session *acpruntime.Session, payload submissionPayload) {
	if h.server.ACPGoals == nil {
		h.fail(http.StatusServiceUnavailable, errorBody("网关目标账本不可用"))
		return
	}
	if payload.mode != "send" || len(payload.attachments) != 0 || len(payload.skills) != 0 {
		h.fail(http.StatusBadRequest, errorBody("目标只支持无附件的直接发送"))
		return
	}
	objective := strings.TrimSpace(payload.text)
	if objective == "" || len([]rune(objective)) > 4000 || payload.id == "" {
		h.fail(http.StatusBadRequest, errorBody("请输入 1–4000 字目标并提供操作 ID"))
		return
	}
	if status, _ := session.Snapshot().MustGet("status").(string); status == "active" {
		h.fail(http.StatusConflict, errorBody("当前任务尚未结束，不能创建目标"))
		return
	}
	stableThreadID := session.ID
	if previous, ok := h.server.gatewayGoalFor(kind, threadID); ok {
		stableThreadID = previous.ThreadID
	}
	goalID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("acp-goal:"+kind+":"+stableThreadID+":"+payload.id)).String()
	goal, claimed, err := h.server.ACPGoals.CreateOnce(goalID, payload.id, kind, stableThreadID, objective, 5)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	if claimed {
		goal, err = h.server.dispatchGatewayGoal(session, goal, objective)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
	} else if current, ok := h.server.ACPGoals.Get(goalID); ok {
		goal = current
	}
	result := jsoncompat.NewObject()
	result.Set("status", "accepted")
	result.Set("confirmed", goal.Status != acpgoal.StatusOutcomeUnknown && goal.Status != acpgoal.StatusDispatching)
	result.Set("duplicate", !claimed)
	result.Set("id", payload.id)
	result.Set("result", gatewayGoalView(goal))
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// GoalSettleACP consumes only the turn whose id was committed with a Goal
// dispatch. The bool means this Goal owns the settle edge and suppresses the
// generic auto-continue/queue hooks.
func (s *Server) GoalSettleACP(kind string, session *acpruntime.Session) bool {
	goal, ok := s.gatewayGoalFor(kind, session.ID)
	if !ok || goal.Status == acpgoal.StatusCompleted || goal.Status == acpgoal.StatusCancelled {
		return false
	}
	if goal.Status != acpgoal.StatusActive && goal.Status != acpgoal.StatusWaitingInput {
		return true
	}
	snapshot := session.Snapshot()
	if goal.Status == acpgoal.StatusWaitingInput {
		if requests, _ := snapshot.MustGet("requests").(*jsoncompat.Array); requests != nil && len(requests.Items) > 0 {
			return true
		}
	}
	turnID, settled := goalTip(snapshot)
	if !settled {
		return true
	}
	if goal.ActiveTurnID != turnID {
		_, _ = s.ACPGoals.Transition(goal.GoalID, goal.GoalID+":manual-intervention:"+turnID, goal.Status, goal.Revision, acpgoal.StatusWaitingReview, "manual turn requires review")
		return true
	}
	claimed, err := s.ACPGoals.ClaimEvaluation(goal.GoalID, turnID)
	if err != nil {
		return true
	}
	context, valid := jev.ContinueContext(kind, snapshot)
	if !valid {
		_, _ = s.ACPGoals.ResolveEvaluation(goal.GoalID, turnID, acpgoal.StatusWaitingReview, "turn snapshot unavailable")
		return true
	}
	goalContext := jev.GoalContext{
		Objective: claimed.Objective, Agent: kind, GoalID: claimed.GoalID, Revision: claimed.Revision, Step: claimed.AutoStepsUsed,
		TurnID: turnID, UserPrompt: jsoncompat.PyStr(context.MustGet("userRequest")),
		AssistantTail: jsoncompat.PyStr(context.MustGet("assistantTail")), StopReason: jsoncompat.PyStr(context.MustGet("stopReason")),
		Error: jsoncompat.PyStr(context.MustGet("error")),
	}
	if requests, _ := snapshot.MustGet("requests").(*jsoncompat.Array); requests != nil && len(requests.Items) > 0 {
		goalContext.PendingRequests = true
	}
	var assessment *jev.GoalAssessment
	if s.Jev != nil && !goalContext.PendingRequests {
		assessment, _ = s.Jev.GoalAssess(goalContext)
	}
	next, reason := jev.DecideGoal(goalContext, assessment)
	decision, _ := json.Marshal(struct {
		Reason     string              `json:"reason"`
		Assessment *jev.GoalAssessment `json:"assessment,omitempty"`
	}{reason, assessment})
	resolved, err := s.ACPGoals.ResolveEvaluation(goal.GoalID, turnID, next, reason, decision)
	if err != nil || resolved.Status != acpgoal.StatusDispatching {
		return true
	}
	prompt := "目标：" + resolved.Objective + "\n已确认的上一轮进展：" + goalContext.AssistantTail + "\n请继续完成目标，并说明本轮结果。"
	_, _ = s.dispatchGatewayGoal(session, resolved, prompt)
	return true
}

func (h *handler) handleGatewayGoalControl(threadID, action string, body *jsoncompat.Object) bool {
	if h.server.ACPGoals == nil {
		return false
	}
	kind, manager, owned := h.server.acpGoalOwner(threadID)
	if !owned || manager == nil || kind == "mcode" {
		return false
	}
	goal, ok := h.server.gatewayGoalFor(kind, threadID)
	if !ok {
		h.fail(http.StatusConflict, errorBody("此会话没有网关托管目标"))
		return true
	}
	id, _ := body.MustGet("id").(string)
	expected, _ := body.MustGet("expectedState").(string)
	revision := int64(0)
	if number, ok := body.MustGet("revision").(interface{ Int64() (int64, error) }); ok {
		revision, _ = number.Int64()
	}
	if id == "" || expected == "" || revision < 1 {
		h.fail(http.StatusBadRequest, errorBody("目标操作需要 id、expectedState 和 revision"))
		return true
	}
	next := ""
	switch action {
	case "cancel":
		next = acpgoal.StatusCancelled
	case "complete":
		if expected != acpgoal.StatusWaitingReview {
			h.fail(http.StatusConflict, errorBody("仅等待复核的目标可标记完成"))
			return true
		}
		next = acpgoal.StatusCompleted
	case "status":
		status, _ := body.MustGet("status").(string)
		if status == "paused" {
			next = acpgoal.StatusPaused
		} else if status == "active" {
			next = acpgoal.StatusActive
		}
	case "continue":
		next = acpgoal.StatusDispatching
	case "review":
		if expected != acpgoal.StatusOutcomeUnknown || body.MustGet("verified") != true {
			h.fail(http.StatusBadRequest, errorBody("请先核实上一轮结果，再确认进入人工复核"))
			return true
		}
		next = acpgoal.StatusWaitingReview
	}
	if action == "edit" {
		objective, _ := body.MustGet("objective").(string)
		if expected != acpgoal.StatusPaused {
			h.fail(http.StatusConflict, errorBody("请先暂停目标"))
			return true
		}
		updated, err := h.server.ACPGoals.Edit(goal.GoalID, id, revision, objective)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return true
		}
		result := jsoncompat.NewObject()
		result.Set("goal", gatewayGoalView(updated))
		result.Set("confirmed", true)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
	}
	if next == "" {
		h.fail(http.StatusBadRequest, errorBody("未知目标操作"))
		return true
	}
	var session *acpruntime.Session
	if next == acpgoal.StatusDispatching || next == acpgoal.StatusActive {
		var attachErr error
		session, attachErr = manager.Attach(threadID, manager.CwdFor(threadID), manager.InitializeTimeout)
		if attachErr != nil || session == nil || session.ReadOnly() {
			h.fail(http.StatusConflict, errorBody("无法接入 Agent，请稍后重试"))
			return true
		}
		if next == acpgoal.StatusDispatching {
			if status, _ := session.Snapshot().MustGet("status").(string); status == "active" {
				h.fail(http.StatusConflict, errorBody("当前任务尚未结束，不能人工继续"))
				return true
			}
		}
	}
	updated, err := h.server.ACPGoals.Transition(goal.GoalID, id, expected, revision, next, action)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return true
	}
	if next == acpgoal.StatusDispatching && updated.Status == acpgoal.StatusDispatching {
		updated, err = h.server.dispatchGatewayGoal(session, updated, "目标："+updated.Objective+"\n请继续完成目标，并说明本轮结果。")
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return true
		}
	}
	if next == acpgoal.StatusActive && session != nil {
		go h.server.GoalSettleACP(kind, session)
	}
	result := jsoncompat.NewObject()
	result.Set("goal", gatewayGoalView(updated))
	result.Set("confirmed", updated.Status != acpgoal.StatusOutcomeUnknown)
	h.output(http.StatusOK, result, contentTypeJSON, "")
	return true
}
