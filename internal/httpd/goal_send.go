// Goal-mode send chain (the last unported Python surface): phone-initiated
// native goals for codex chats. Mirrors service.py's _goal_command /
// _send_goal_activation: a durable command ledger plans every intent against
// the authoritative sqlite goal state, the write rides the goal sidecar RPC,
// and a confirmed create/resume fans out a bridge-sent activation turn whose
// request id both hides its user row from the timeline and keeps it out of
// the visible queue.
package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// goalLedger is goal_commands.json: "thread:submission" -> command record.
type goalLedger struct {
	mu    sync.Mutex
	path  string
	data  map[string]*jsoncompat.Object
	order []string
}

// NewGoalLedger loads (lazily) the durable goal command ledger at path.
func NewGoalLedger(path string) *goalLedger {
	return &goalLedger{path: path, data: map[string]*jsoncompat.Object{}}
}

func (g *goalLedger) load() {
	if g.data != nil || g.path == "" {
		return
	}
	g.data = map[string]*jsoncompat.Object{}
	raw, _ := os.ReadFile(g.path)
	value, _ := jsoncompat.Unmarshal(raw)
	object, _ := value.(*jsoncompat.Object)
	if object != nil {
		for _, key := range object.Keys() {
			if entry, ok := object.MustGet(key).(*jsoncompat.Object); ok {
				g.data[key] = entry
				g.order = append(g.order, key)
			}
		}
	}
}

func (g *goalLedger) saveLocked() {
	if g.path == "" {
		return
	}
	object := jsoncompat.NewObject()
	for _, key := range g.order {
		if entry, exists := g.data[key]; exists {
			object.Set(key, entry)
		}
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, jsoncompat.EncodePythonIndent2(object), 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, g.path)
}

func (g *goalLedger) record(key string, entry *jsoncompat.Object) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.load()
	if _, exists := g.data[key]; !exists {
		g.order = append(g.order, key)
	}
	g.data[key] = entry
	g.saveLocked()
}

func (g *goalLedger) get(key string) *jsoncompat.Object {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.load()
	return g.data[key]
}

// forThread lists the thread's command records, newest first, each stamped
// with its ledger key so submissions can cite the original request id.
func (g *goalLedger) forThread(threadID string) []*jsoncompat.Object {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.load()
	prefix := threadID + ":"
	found := []*jsoncompat.Object{}
	for i := len(g.order) - 1; i >= 0; i-- {
		key := g.order[i]
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if entry, exists := g.data[key]; exists {
			clone, _ := jsoncompat.Clone(entry).(*jsoncompat.Object)
			if clone == nil {
				continue
			}
			clone.Set("key", key)
			found = append(found, clone)
		}
	}
	return found
}

// --- planning (plan_goal_command) -------------------------------------------

type goalDecision int

const (
	goalExecute goalDecision = iota
	goalDuplicate
	goalInvalid
)

func planGoalCommand(action, objective string, current *jsoncompat.Object) (goalDecision, string, string) {
	objective = strings.TrimSpace(objective)
	currentObjective, currentStatus := "", ""
	if current != nil {
		currentObjective, _ = current.MustGet("objective").(string)
		currentStatus, _ = current.MustGet("status").(string)
	}
	sameObjective := currentObjective != "" && currentObjective == objective
	switch action {
	case "create":
		if current == nil || currentStatus == "complete" {
			return goalExecute, "", "active"
		}
		if sameObjective && currentStatus == "active" {
			return goalDuplicate, "", "active"
		}
		return goalInvalid, "此聊天已有未完成目标，请先处理当前目标", ""
	case "cancel":
		if current == nil {
			return goalDuplicate, "", "cancelled"
		}
		if !sameObjective {
			return goalInvalid, "目标已发生变化，请刷新后重试", ""
		}
		return goalExecute, "", "cancelled"
	case "edit":
		if current == nil || currentStatus != "paused" {
			return goalInvalid, "请先暂停目标，再修改目标内容", ""
		}
		if sameObjective {
			return goalDuplicate, "", "paused"
		}
		return goalExecute, "", "paused"
	case "pause", "resume":
		expected, predecessor := "paused", "active"
		if action == "resume" {
			expected, predecessor = "active", "paused"
		}
		if current == nil {
			return goalInvalid, "当前聊天没有可操作的目标", ""
		}
		if !sameObjective {
			return goalInvalid, "目标已发生变化，请刷新后重试", ""
		}
		if currentStatus == expected {
			return goalDuplicate, "", expected
		}
		if currentStatus == predecessor ||
			(action == "pause" && (currentStatus == "blocked" || currentStatus == "usageLimited" || currentStatus == "budgetLimited")) {
			return goalExecute, "", expected
		}
		if action == "pause" {
			return goalInvalid, "目标当前状态不支持暂停", ""
		}
		return goalInvalid, "目标当前状态不支持恢复", ""
	}
	return goalInvalid, "目标操作无效", ""
}

// --- activation (goal_activation) -------------------------------------------

var goalActivationTexts = map[string]map[string]string{
	"zh-CN": {
		"create": "目标已创建。请立即开始执行当前目标，不要重复创建目标。",
		"resume": "目标已恢复。请继续执行当前目标。",
	},
	"en-US": {
		"create": "Goal created. Start working on it now. Do not create it again.",
		"resume": "Goal resumed. Continue working on it.",
	},
}

func normalizeGoalLocale(value string) string {
	lowered := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lowered, "en") {
		return "en-US"
	}
	return "zh-CN"
}

// goalActivationID derives the stable request id of a goal's activation turn.
func goalActivationID(threadID string, goalValue *jsoncompat.Object, action, transitionID string) string {
	identity := goal.IdentityOf(goalValue)
	seed := string(jsoncompat.EncodeCompact(identity))
	if transitionID == "" {
		if objective, _ := goalValue.MustGet("objective").(string); seed == "{}" && objective != "" {
			seed = "objective:" + objective
		}
	}
	var namespace uuid.UUID
	if parsed, err := uuid.Parse(threadID); err == nil {
		namespace = parsed
	} else {
		namespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:agent:"+threadID))
	}
	return uuid.NewSHA1(namespace, []byte("goal-activation:v1:"+action+":"+(transitionIDOf(transitionID, seed)))).String()
}

func transitionIDOf(transitionID, seed string) string {
	if transitionID != "" {
		return transitionID
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// --- the command state machine ----------------------------------------------

type goalCommandOutcome struct {
	response  *jsoncompat.Object
	confirmed bool
	duplicate bool
}

// runGoalCommand mirrors _goal_command: ledger replay, plan against the
// authoritative sqlite state, sidecar write, re-read, confirm. The returned
// response is what both the send route and /goal/* hand the phone
// ({status, confirmed, result, duplicate?, transitionId?}).
func (s *Server) runGoalCommand(threadID, requestID, action, objective, status, uiLocale string) (*goalCommandOutcome, error) {
	key := threadID + ":" + requestID
	if s.GoalCmds == nil || s.Goal == nil {
		return nil, errors.New("目标模式暂不可用")
	}
	if prior := s.GoalCmds.get(key); prior != nil {
		state, _ := prior.MustGet("state").(string)
		response, _ := prior.MustGet("response").(*jsoncompat.Object)
		switch state {
		case "confirmed":
			result := cloneOrEmpty(response)
			result.Set("duplicate", true)
			return &goalCommandOutcome{response: result, confirmed: true, duplicate: true}, nil
		case "failed":
			message, _ := prior.MustGet("error").(string)
			return nil, errors.New(firstNonEmpty(message, "目标操作失败"))
		}
		result := cloneOrEmpty(response)
		result.Set("status", "unknown")
		result.Set("confirmed", false)
		result.Set("duplicate", true)
		return &goalCommandOutcome{response: result}, nil
	}

	native := goal.ReadNativeGoal(s.Goal.Home, threadID)
	decision, message, expectedStatus := planGoalCommand(action, objective, native)
	if decision == goalInvalid {
		s.GoalCmds.record(key, failedGoalCommand(action, objective, message, expectedStatus))
		return nil, errors.New(message)
	}
	if decision == goalDuplicate {
		// The authoritative state already proves the command (Python's
		// duplicate branch): confirm without writing.
		command := jsoncompat.NewObject()
		command.Set("action", action)
		command.Set("objective", strings.TrimSpace(objective))
		command.Set("workMode", "goal")
		command.Set("uiLocale", normalizeGoalLocale(uiLocale))
		command.Set("state", "confirmed")
		command.Set("sent", true)
		command.Set("at", json.Number(strconv.FormatFloat(float64(time.Now().UnixMilli())/1000, 'f', 3, 64)))
		command.Set("confirmedAt", command.MustGet("at"))
		response := jsoncompat.NewObject()
		response.Set("status", expectedStatus)
		response.Set("confirmed", true)
		if native != nil {
			response.Set("result", native)
		}
		command.Set("response", cloneOrEmpty(response))
		s.GoalCmds.record(key, command)
		return &goalCommandOutcome{response: response, confirmed: true, duplicate: true}, nil
	}
	command := jsoncompat.NewObject()
	command.Set("action", action)
	command.Set("objective", strings.TrimSpace(objective))
	command.Set("workMode", "goal")
	command.Set("uiLocale", normalizeGoalLocale(uiLocale))
	command.Set("state", "sent")
	command.Set("sent", true)
	command.Set("at", json.Number(strconv.FormatFloat(float64(time.Now().UnixMilli())/1000, 'f', 3, 64)))
	s.GoalCmds.record(key, command)

	// Sidecar write. Create/resume target active; edit keeps paused.
	writeErr := error(nil)
	switch action {
	case "cancel":
		writeErr = s.Goal.Clear(threadID)
	default:
		writeStatus := expectedStatus
		if action == "create" {
			writeStatus = "active"
		}
		_, writeErr = s.Goal.Set(threadID, strings.TrimSpace(objective), writeStatus)
	}
	if writeErr != nil {
		unknown := strings.Contains(writeErr.Error(), "超时") || strings.Contains(writeErr.Error(), "timeout")
		state := "failed"
		status := firstNonEmpty(expectedStatus, "cancelled")
		if unknown {
			state = "unknown"
			status = "unknown"
		}
		command.Set("state", state)
		command.Set("error", writeErr.Error())
		response := jsoncompat.NewObject()
		response.Set("status", status)
		response.Set("confirmed", false)
		response.Set("error", writeErr.Error())
		command.Set("response", response)
		s.GoalCmds.record(key, command)
		return nil, writeErr
	}

	fresh := goal.ReadNativeGoal(s.Goal.Home, threadID)
	// Cancel confirms by absence; every other verb by the matching row.
	confirmed := expectedStatus == "cancelled" && fresh == nil ||
		expectedStatus != "cancelled" && fresh != nil && fresh.MustGet("status") == expectedStatus
	response := jsoncompat.NewObject()
	response.Set("status", expectedStatus)
	response.Set("confirmed", confirmed)
	if !confirmed {
		// The write went out; the sqlite row has not caught up (or failed).
		command.Set("state", "unknown")
		response.Set("status", "unknown")
		response.Set("error", "目标操作已发送，但状态尚未确认")
		command.Set("response", response)
		s.GoalCmds.record(key, command)
		return &goalCommandOutcome{response: response}, errors.New("目标操作已发送，但状态尚未确认")
	}
	if expectedStatus == "cancelled" {
		fresh = nil
	}
	command.Set("state", "confirmed")
	command.Set("confirmedAt", json.Number(strconv.FormatFloat(float64(time.Now().UnixMilli())/1000, 'f', 3, 64)))
	// Store an absent result as a missing key, not a typed nil *Object:
	// Clone/encode treat the latter as a real object and dereference it.
	if fresh != nil {
		response.Set("result", fresh)
	}
	response.Set("transitionId", requestID)
	command.Set("response", cloneOrEmpty(response))
	s.GoalCmds.record(key, command)
	return &goalCommandOutcome{response: response, confirmed: true}, nil
}

func failedGoalCommand(action, objective, message, expectedStatus string) *jsoncompat.Object {
	command := jsoncompat.NewObject()
	command.Set("action", action)
	command.Set("objective", strings.TrimSpace(objective))
	command.Set("workMode", "goal")
	command.Set("state", "failed")
	command.Set("error", message)
	response := jsoncompat.NewObject()
	response.Set("status", firstNonEmpty(expectedStatus, "cancelled"))
	response.Set("confirmed", false)
	response.Set("error", message)
	command.Set("response", response)
	return command
}

func cloneOrEmpty(value *jsoncompat.Object) *jsoncompat.Object {
	if clone, ok := jsoncompat.Clone(value).(*jsoncompat.Object); ok && clone != nil {
		return clone
	}
	return jsoncompat.NewObject()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// sendGoalActivation mirrors _send_goal_activation: only confirmed
// create/resume with an active goal fans out the activation turn. The turn
// rides the queue ledger so an in-flight reply finishes first; the request id
// hides the row from both the timeline and the visible queue.
func (s *Server) sendGoalActivation(threadID string, outcome *goalCommandOutcome, action, uiLocale string) *jsoncompat.Object {
	if outcome == nil || !outcome.confirmed || action != "create" && action != "resume" {
		return nil
	}
	response := cloneOrEmpty(outcome.response)
	goalValue, _ := response.MustGet("result").(*jsoncompat.Object)
	if goalValue == nil {
		return nil
	}
	if status, _ := goalValue.MustGet("status").(string); status != "active" {
		return nil
	}
	if outcome.duplicate && response.MustGet("transitionId") == nil {
		return nil
	}
	locale := normalizeGoalLocale(uiLocale)
	if value, _ := response.MustGet("uiLocale").(string); value != "" {
		locale = normalizeGoalLocale(value)
	}
	transition, _ := response.MustGet("transitionId").(string)
	activationID := goalActivationID(threadID, goalValue, action, transition)
	text := goalActivationTexts[locale][action]
	if s.Queue == nil {
		return nil
	}
	entry, _, err := s.Queue.enqueue(threadID, activationID, text, "queue", "", nil, nil, nil)
	if err != nil {
		return nil
	}
	activation := jsoncompat.NewObject()
	activation.Set("id", activationID)
	activation.Set("status", entry.Status)
	return activation
}

// goalActivationIDs lists the activation request ids whose user rows the
// timeline projection hides (timeline.Update consumes view.goalActivationIds).
func (s *Server) goalActivationIDs(threadID string) []string {
	if s.GoalCmds == nil {
		return nil
	}
	ids := []string{}
	for _, command := range s.GoalCmds.forThread(threadID) {
		action, _ := command.MustGet("action").(string)
		if action != "create" && action != "resume" {
			continue
		}
		if state, _ := command.MustGet("state").(string); state != "confirmed" {
			continue
		}
		response, _ := command.MustGet("response").(*jsoncompat.Object)
		if response == nil || response.MustGet("confirmed") != true {
			continue
		}
		goalValue, _ := response.MustGet("result").(*jsoncompat.Object)
		if goalValue == nil {
			continue
		}
		transition, _ := response.MustGet("transitionId").(string)
		ids = append(ids, goalActivationID(threadID, goalValue, action, transition))
	}
	return ids
}

// cancelGoalActivationQueues marks still-queued activation turns cancelled
// once their goal command is confirmed cancelled.
func (s *Server) cancelGoalActivationQueues(threadID string) {
	if s.Queue == nil {
		return
	}
	for _, activationID := range s.goalActivationIDs(threadID) {
		s.Queue.markCancelled(threadID, activationID)
	}
}

// goalSubmissionFor derives the panel's pending-goal card from the ledger:
// the newest create/resume/cancel command the phone has not seen confirmed.
func (s *Server) goalSubmissionFor(threadID string) *jsoncompat.Object {
	if s.GoalCmds == nil {
		return nil
	}
	commands := s.GoalCmds.forThread(threadID)
	if len(commands) == 0 {
		return nil
	}
	latest := commands[0]
	action, _ := latest.MustGet("action").(string)
	if action == "cancel" {
		return nil
	}
	state, _ := latest.MustGet("state").(string)
	status := ""
	switch state {
	case "pending", "sent":
		status = "pending"
	case "unknown":
		status = "unknown"
	case "confirmed":
		// Confirmed but the native row vanished: unconfirmed, not a dead end.
		if s.Goal != nil && goal.Absent(s.Goal.Home, threadID) {
			return nil
		}
		response, _ := latest.MustGet("response").(*jsoncompat.Object)
		if response == nil || response.MustGet("confirmed") != true {
			status = "unconfirmed"
		} else {
			return nil
		}
	default:
		return nil
	}
	submission := jsoncompat.NewObject()
	id := strings.TrimPrefix(latest.MustGet("key").(string), threadID+":")
	submission.Set("id", id)
	submission.Set("status", status)
	return submission
}

// applyGoalOverlay enriches a codex view (live or historical) with the goal
// fields modes.js renders. The view is request-private, safe to mutate.
func (s *Server) applyGoalOverlay(threadID string, view *jsoncompat.Object) {
	if s.Goal == nil {
		return
	}
	runtime := s.Goal.Executable != ""
	view.Set("goalRuntimeAvailable", runtime)
	// nil unless native storage proves a live row; a stale desktop snapshot
	// behind a confirmed create is suppressed by the absence check below.
	var native *jsoncompat.Object
	if runtime {
		if fresh := goal.ReadNativeGoal(s.Goal.Home, threadID); fresh != nil {
			native = fresh
		}
	}
	view.Set("goal", native)
	ids := &jsoncompat.Array{}
	for _, id := range s.goalActivationIDs(threadID) {
		ids.Items = append(ids.Items, id)
	}
	view.Set("goalActivationIds", ids)
	if submission := s.goalSubmissionFor(threadID); submission != nil {
		view.Set("goalSubmission", submission)
	} else {
		view.Set("goalSubmission", nil)
	}
}

func (s *Server) goalAbsentWithConfirmedCreate(threadID string) bool {
	for _, command := range s.GoalCmds.forThread(threadID) {
		if action, _ := command.MustGet("action").(string); action != "create" {
			continue
		}
		if state, _ := command.MustGet("state").(string); state == "confirmed" {
			return goal.Absent(s.Goal.Home, threadID)
		}
	}
	return false
}

// handleGoalCreate is the send route's workMode=="goal" branch for codex.
func (h *handler) handleGoalCreate(threadID string, payload submissionPayload) {
	if payload.mode != "send" {
		h.fail(http.StatusBadRequest, errorBody("目标需在当前任务结束后直接发送"))
		return
	}
	text := strings.TrimSpace(payload.text)
	if text == "" || len(text) > 4000 {
		h.fail(http.StatusBadRequest, errorBody("目标需在当前任务结束后直接发送，且不超过 4000 字"))
		return
	}
	if len(payload.attachments) > 0 {
		h.fail(http.StatusBadRequest, errorBody("目标模式暂不支持附件"))
		return
	}
	if len(payload.skills) > 0 {
		h.fail(http.StatusBadRequest, errorBody("目标模式只接受纯文本目标"))
		return
	}
	outcome, err := h.server.runGoalCommand(threadID, payload.id, "create", text, "", payload.uiLocale)
	if err != nil && outcome == nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return
	}
	result := jsoncompat.NewObject()
	status := "accepted"
	if outcome != nil && outcome.response != nil {
		if value, _ := outcome.response.MustGet("status").(string); value == "unknown" {
			status = "unknown"
		}
	}
	result.Set("status", status)
	result.Set("confirmed", outcome != nil && outcome.confirmed)
	result.Set("duplicate", outcome != nil && outcome.duplicate)
	result.Set("id", payload.id)
	if outcome != nil && outcome.response != nil {
		if goalValue, ok := outcome.response.MustGet("result").(*jsoncompat.Object); ok {
			result.Set("result", goalValue)
		}
	}
	if activation := h.server.sendGoalActivation(threadID, outcome, "create", payload.uiLocale); activation != nil {
		result.Set("activation", activation)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// queueMetaExcludingActivations is the visible queue minus goal activation
// turns (they are bridge-sent, not user compositions).
func (s *Server) queueMetaExcludingActivations(threadID string) *jsoncompat.Array {
	rows := s.Queue.meta(threadID)
	if rows == nil || len(rows.Items) == 0 {
		return rows
	}
	hidden := map[string]bool{}
	for _, id := range s.goalActivationIDs(threadID) {
		hidden[id] = true
	}
	if len(hidden) == 0 {
		return rows
	}
	visible := &jsoncompat.Array{}
	for _, item := range rows.Items {
		if row, ok := item.(*jsoncompat.Object); ok {
			if id, _ := row.MustGet("id").(string); hidden[id] {
				continue
			}
		}
		visible.Items = append(visible.Items, item)
	}
	return visible
}
