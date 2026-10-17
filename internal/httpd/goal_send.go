// Goal-mode send chain (the last unported Python surface): phone-initiated
// native goals. Mirrors service.py's _goal_command / _send_goal_activation: a
// durable command ledger plans every intent against the authoritative goal
// state, the write rides the owning transport, and a confirmed create/resume
// fans out a bridge-sent activation turn whose request id both hides its user
// row from the timeline and keeps it out of the visible queue. Two transports
// implement the read/write wire: codex reads goals_1.sqlite and writes the
// app-server sidecar RPC; mcode speaks its custom mcode/session/goal/* ACP
// methods on the session process (reads never spawn, its goal/create kicks
// the turn off by itself so no activation turn may ride along).
package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
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

// --- transports ---------------------------------------------------------------

// goalTransport abstracts the read/write wires behind the command machine
// (Python's adapter goal methods): where the authoritative row lives and how
// intents reach it.
type goalTransport interface {
	// Read returns the authoritative goal row (normalized) or nil. It must
	// never spawn a process: absent/unready handles read as no goal.
	Read(threadID string) *jsoncompat.Object
	// Absent proves the thread has no goal ("no goal" vs "unreadable").
	Absent(threadID string) bool
	// Write applies one intent (create/pause/resume/edit/cancel). Error
	// classification decides failed vs unknown — see goalWriteUnknown.
	Write(threadID, action, objective, status string) error
	// NeedsActivation reports whether a confirmed create/resume must fan out
	// the bridge activation turn (mcode's goal/create starts the turn itself;
	// a bridge turn would double-start the goal).
	NeedsActivation() bool
	// Available reports whether the transport can operate at all.
	Available() bool
}

// codexGoalTransport reads goals_1.sqlite directly (view reads never spin an
// app-server) and writes through the goal sidecar RPC — the exact wiring the
// codex send chain has always used.
type codexGoalTransport struct{ goal *goal.Manager }

func (t codexGoalTransport) Read(threadID string) *jsoncompat.Object {
	return goal.ReadNativeGoal(t.goal.Home, threadID)
}

func (t codexGoalTransport) Absent(threadID string) bool { return goal.Absent(t.goal.Home, threadID) }

func (t codexGoalTransport) Write(threadID, action, objective, status string) error {
	if action == "cancel" {
		return t.goal.Clear(threadID)
	}
	if status == "" || action == "create" {
		status = "active"
	}
	_, err := t.goal.Set(threadID, objective, status)
	return err
}

func (t codexGoalTransport) NeedsActivation() bool { return true }

func (t codexGoalTransport) Available() bool { return t.goal != nil && t.goal.Executable != "" }

// mcodeGoalTransport speaks mcode's custom goal methods on the session
// process (bridge/agents/mcode.py): reads touch only an already-live handle
// and return nil otherwise, writes attach the session on demand.
type mcodeGoalTransport struct{ manager *acpruntime.Manager }

func (t *mcodeGoalTransport) Read(threadID string) *jsoncompat.Object {
	session := t.manager.Get(threadID)
	if session == nil || session.IsClosed() {
		return nil
	}
	result, err := session.Request("mcode/session/goal/get", goalSessionParams(threadID), 5*time.Second)
	if err != nil {
		return nil
	}
	return goal.NormalizeRPCGoal(result)
}

func (t *mcodeGoalTransport) Absent(threadID string) bool { return t.Read(threadID) == nil }

func (t *mcodeGoalTransport) Write(threadID, action, objective, status string) error {
	// Writes may activate: the goal RPCs need the session loaded in a live
	// process (mcode.py's _goal_handle → bridge.activate).
	cwd := t.manager.CwdFor(threadID)
	if cwd == "" {
		return errors.New("找不到这个会话的工作目录，请稍后刷新列表")
	}
	session, err := t.manager.Open(threadID, cwd, t.manager.InitializeTimeout)
	if err != nil {
		return err
	}
	method, extra := "", ""
	switch action {
	case "cancel":
		method = "mcode/session/goal/clear"
	case "create":
		method, extra = "mcode/session/goal/create", objective
	case "edit":
		method, extra = "mcode/session/goal/patch", objective
	case "pause", "resume":
		method = "mcode/session/goal/patch"
	default:
		return errors.New("目标操作无效")
	}
	params := goalSessionParams(threadID)
	if action == "pause" || action == "resume" {
		params.Set("status", status)
	} else if extra != "" {
		params.Set("objective", extra)
	}
	_, err = session.Request(method, params, 30*time.Second)
	return err
}

// goalSessionParams mirrors mcode.py's {"sessionId": thread_id, **extra}.
func goalSessionParams(threadID string) *jsoncompat.Object {
	params := jsoncompat.NewObject()
	params.Set("sessionId", threadID)
	return params
}

func (t *mcodeGoalTransport) NeedsActivation() bool { return false }

func (t *mcodeGoalTransport) Available() bool { return t.manager != nil }

// goalWriteUnknown splits the failure classes Python's GoalUnavailable vs
// GoalError produce: a typed ACP timeout/disconnect never proved the write
// reached the agent (unknown), an agent-side AcpError did (failed). Codex
// sidecar errors keep the string classification.
func goalWriteUnknown(err error) bool {
	var acpErr *acp.Error
	if errors.As(err, &acpErr) {
		return acpErr.Kind == acp.KindTimeout || acpErr.Kind == acp.KindUnavailable
	}
	return strings.Contains(err.Error(), "超时") || strings.Contains(err.Error(), "timeout")
}

// acpGoalOwner finds the ACP manager owning threadID without ever spawning:
// a live session wins, then the cached session/list roster — resolveACP
// minus the cold list probe, so goal reads never bring an agent up.
func (s *Server) acpGoalOwner(threadID string) (string, *acpruntime.Manager, bool) {
	agents := s.acpAgentSet()
	if len(agents) == 0 || threadID == "" {
		return "", nil, false
	}
	kinds := make([]string, 0, len(agents))
	for kind := range agents {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		if manager := agents[kind]; manager != nil && manager.Get(threadID) != nil {
			return kind, manager, true
		}
	}
	for _, kind := range kinds {
		if manager := agents[kind]; manager != nil && manager.Knows(threadID) {
			return kind, manager, true
		}
	}
	return "", nil, false
}

// acpGoalTransport builds the ACP goal transport (mcode's session-process
// RPC methods) for a manager whose kind is mcode or that declares the goals
// capability; nil means the agent has no goal surface.
func (s *Server) acpGoalTransport(kind string, manager *acpruntime.Manager) goalTransport {
	if manager == nil {
		return nil
	}
	if kind != "mcode" {
		declared := false
		for _, capability := range manager.Capabilities {
			if capability == "goals" {
				declared = true
				break
			}
		}
		if !declared {
			return nil
		}
	}
	return &mcodeGoalTransport{manager: manager}
}

// goalTransportFor picks the transport owning threadID: the owning ACP
// manager's goal RPC when it carries a goal surface, else the codex sidecar.
func (s *Server) goalTransportFor(threadID string) goalTransport {
	if kind, manager, owned := s.acpGoalOwner(threadID); owned {
		return s.acpGoalTransport(kind, manager)
	}
	if s.Goal != nil {
		return codexGoalTransport{goal: s.Goal}
	}
	return nil
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
// authoritative goal state, transport write, re-read, confirm. The returned
// response is what both the send route and /goal/* hand the phone
// ({status, confirmed, result, duplicate?, transitionId?}).
func (s *Server) runGoalCommand(transport goalTransport, threadID, requestID, action, objective, status, uiLocale string) (*goalCommandOutcome, error) {
	key := threadID + ":" + requestID
	if s.GoalCmds == nil || transport == nil {
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

	native := transport.Read(threadID)
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

	// Transport write. Create/resume target active; edit keeps paused.
	writeErr := transport.Write(threadID, action, strings.TrimSpace(objective), expectedStatus)
	if writeErr != nil {
		unknown := goalWriteUnknown(writeErr)
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

	fresh := transport.Read(threadID)
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
// hides the row from both the timeline and the visible queue. Transports
// whose agent starts the turn itself (mcode) never fan one out.
func (s *Server) sendGoalActivation(threadID string, outcome *goalCommandOutcome, action, uiLocale string) *jsoncompat.Object {
	if transport := s.goalTransportFor(threadID); transport == nil || !transport.NeedsActivation() {
		return nil
	}
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
// Only transports that fan activation turns out ever produce ids.
func (s *Server) goalActivationIDs(threadID string) []string {
	if s.GoalCmds == nil {
		return nil
	}
	if transport := s.goalTransportFor(threadID); transport == nil || !transport.NeedsActivation() {
		return []string{}
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
	transport := s.goalTransportFor(threadID)
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
		if transport == nil || transport.Absent(threadID) {
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
	transport := s.goalTransportFor(threadID)
	if transport == nil {
		return
	}
	s.applyGoalOverlayWith(transport, threadID, view)
}

// applyGoalOverlayWith is the transport-parameterized overlay, shared by the
// codex view paths and the ACP (mcode) view builder.
func (s *Server) applyGoalOverlayWith(transport goalTransport, threadID string, view *jsoncompat.Object) {
	runtime := transport.Available()
	view.Set("goalRuntimeAvailable", runtime)
	// nil unless the authoritative store proves a live row; a stale desktop
	// snapshot behind a confirmed create is suppressed by the absence check
	// in goalSubmissionFor. The absent value is an untyped nil — a typed nil
	// *Object would panic Clone/encode in the timeline projection.
	var native any
	if runtime {
		if fresh := transport.Read(threadID); fresh != nil {
			native = fresh
		}
	}
	view.Set("goal", native)
	ids := &jsoncompat.Array{}
	if transport.NeedsActivation() {
		for _, id := range s.goalActivationIDs(threadID) {
			ids.Items = append(ids.Items, id)
		}
	}
	view.Set("goalActivationIds", ids)
	if submission := s.goalSubmissionFor(threadID); submission != nil {
		view.Set("goalSubmission", submission)
	} else {
		view.Set("goalSubmission", nil)
	}
}

func (s *Server) goalAbsentWithConfirmedCreate(threadID string) bool {
	if s.GoalCmds == nil {
		return false
	}
	transport := s.goalTransportFor(threadID)
	if transport == nil {
		return false
	}
	for _, command := range s.GoalCmds.forThread(threadID) {
		if action, _ := command.MustGet("action").(string); action != "create" {
			continue
		}
		if state, _ := command.MustGet("state").(string); state == "confirmed" {
			return transport.Absent(threadID)
		}
	}
	return false
}

// handleGoalCreate is the send route's workMode=="goal" branch, shared by the
// codex sidecar and the mcode ACP transport.
func (h *handler) handleGoalCreate(transport goalTransport, threadID string, payload submissionPayload) {
	if transport == nil {
		h.fail(http.StatusConflict, errorBody("目标模式暂不可用"))
		return
	}
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
	outcome, err := h.server.runGoalCommand(transport, threadID, payload.id, "create", text, "", payload.uiLocale)
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
