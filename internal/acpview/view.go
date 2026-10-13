// Package acpview: the session view state machine mirroring AcpSessionView.
package acpview

import (
	"fmt"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// View mirrors AcpSessionView. now supplies begin_turn's startedAt ms.
type View struct {
	SessionID string
	Title     any
	Cwd       any
	Model     any
	Thinking  any
	Mode      any
	Status    string
	Turns     []*jsoncompat.Object
	Requests  []*jsoncompat.Object
	Commands  []*jsoncompat.Object
	Now       func() float64

	ordinal   int
	toolIndex map[string]*jsoncompat.Object
}

func NewView(sessionID string, title, cwd any, now func() float64) *View {
	return &View{SessionID: sessionID, Title: title, Cwd: cwd, Status: "idle",
		Now: now, toolIndex: map[string]*jsoncompat.Object{}}
}

func (v *View) nextID(prefix string) string {
	v.ordinal++
	return fmt.Sprintf("%s-%d", prefix, v.ordinal)
}

func (v *View) newTurn(id string, startedAt any) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", "inProgress")
	turn.Set("startedAt", startedAt)
	turn.Set("messages", &jsoncompat.Array{})
	turn.Set("actionable", false)
	turn.Set("error", nil)
	turn.Set("diff", nil)
	v.Turns = append(v.Turns, turn)
	return turn
}

func (v *View) currentTurn() *jsoncompat.Object {
	if len(v.Turns) == 0 {
		return nil
	}
	return v.Turns[len(v.Turns)-1]
}

func (v *View) assistantTurn() *jsoncompat.Object {
	if turn := v.currentTurn(); turn != nil {
		return turn
	}
	return v.newTurn(v.nextID("turn"), nil)
}

func messages(turn *jsoncompat.Object) *jsoncompat.Array {
	list, _ := turn.MustGet("messages").(*jsoncompat.Array)
	if list == nil {
		list = &jsoncompat.Array{}
		turn.Set("messages", list)
	}
	return list
}

func (v *View) assistantMessage(turn *jsoncompat.Object, kind string, role any, extra func(*jsoncompat.Object)) *jsoncompat.Object {
	list := messages(turn)
	if len(list.Items) > 0 {
		if last := obj(list.Items[len(list.Items)-1]); last != nil {
			if lastKind, _ := last.MustGet("kind").(string); lastKind == kind {
				return last
			}
		}
	}
	message := jsoncompat.NewObject()
	message.Set("id", v.nextID(kind))
	message.Set("kind", kind)
	message.Set("text", "")
	message.Set("role", role)
	if extra != nil {
		extra(message)
	}
	list.Items = append(list.Items, message)
	return message
}

// Snapshot mirrors AcpSessionView.snapshot.
func (v *View) Snapshot() *jsoncompat.Object {
	title := v.Title
	if !jsoncompat.Truthy(title) {
		title = "未命名聊天"
	}
	turns := &jsoncompat.Array{}
	for _, turn := range v.Turns {
		turns.Items = append(turns.Items, jsoncompat.Clone(turn))
	}
	requests := &jsoncompat.Array{}
	for _, request := range v.Requests {
		requests.Items = append(requests.Items, jsoncompat.Clone(request))
	}
	snapshot := jsoncompat.NewObject()
	snapshot.Set("id", v.SessionID)
	snapshot.Set("title", title)
	snapshot.Set("cwd", v.Cwd)
	snapshot.Set("model", v.Model)
	snapshot.Set("latestModel", v.Model)
	snapshot.Set("provider", nil)
	snapshot.Set("effort", v.Thinking)
	snapshot.Set("connected", true)
	snapshot.Set("status", v.Status)
	snapshot.Set("collaborationMode", v.Mode)
	snapshot.Set("goal", nil)
	snapshot.Set("turns", turns)
	snapshot.Set("requests", requests)
	snapshot.Set("historyComplete", true)
	return snapshot
}

// BeginTurn mirrors begin_turn.
func (v *View) BeginTurn(text string) {
	turn := v.newTurn(v.nextID("turn"), jsoncompat.Num(int(v.Now()*1000)))
	list := messages(turn)
	message := jsoncompat.NewObject()
	message.Set("id", v.nextID("user"))
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", text)
	list.Items = append(list.Items, message)
	v.Status = "active"
}

// OnUpdate mirrors on_update (swallowing per-update failures like Python).
func (v *View) OnUpdate(update *jsoncompat.Object) {
	kind, _ := update.MustGet("sessionUpdate").(string)
	v.apply(kind, update)
}

// EndTurn mirrors end_turn.
func (v *View) EndTurn(stopReason string) {
	if turn := v.currentTurn(); turn != nil {
		if status, _ := turn.MustGet("status").(string); status == "inProgress" {
			if stopReason == "cancelled" {
				turn.Set("status", "interrupted")
			} else {
				turn.Set("status", "completed")
			}
			turn.Set("stopReason", stopReason)
		}
	}
	v.Status = "idle"
	v.Requests = nil
}

// FailTurn mirrors fail_turn.
func (v *View) FailTurn(message any) {
	if turn := v.currentTurn(); turn != nil {
		if status, _ := turn.MustGet("status").(string); status == "inProgress" {
			turn.Set("status", "failed")
			entry := jsoncompat.NewObject()
			entry.Set("id", v.nextID("error"))
			entry.Set("kind", "error")
			entry.Set("role", "error")
			entry.Set("text", jsoncompat.PyStr(message))
			messages(turn).Items = append(messages(turn).Items, entry)
		}
	}
	v.Status = "idle"
	v.Requests = nil
}

// FinishReplay mirrors finish_replay.
func (v *View) FinishReplay() {
	for _, turn := range v.Turns {
		if status, _ := turn.MustGet("status").(string); status == "inProgress" {
			turn.Set("status", "completed")
		}
	}
	v.Status = "idle"
}

// AddPermission mirrors add_permission.
func (v *View) AddPermission(requestID string, params *jsoncompat.Object) {
	toolCall := obj(truthyOr(params.MustGet("toolCall"), any(jsoncompat.NewObject())))
	options := &jsoncompat.Array{}
	if list := arr(truthyOr(params.MustGet("options"), any(&jsoncompat.Array{}))); list != nil {
		for _, item := range list.Items {
			if option := obj(item); option != nil {
				entry := jsoncompat.NewObject()
				entry.Set("id", option.MustGet("optionId"))
				entry.Set("label", option.MustGet("name"))
				entry.Set("kind", option.MustGet("kind"))
				options.Items = append(options.Items, entry)
			}
		}
	}
	entryParams := jsoncompat.NewObject()
	entryParams.Set("title", truthyOr(toolCall.MustGet("title"), "权限请求"))
	entryParams.Set("command", CommandSummary(toolCall.MustGet("rawInput")))
	entryParams.Set("options", options)
	entry := jsoncompat.NewObject()
	entry.Set("id", requestID)
	entry.Set("method", "acp/requestPermission")
	entry.Set("supported", true)
	entry.Set("params", entryParams)
	v.replaceRequest(requestID, entry)
}

// AddInput mirrors add_input.
func (v *View) AddInput(requestID string, params *jsoncompat.Object) {
	schema := params.MustGet("requestedSchema")
	schemaObject, _ := schema.(*jsoncompat.Object)
	supported := FormSupported(schemaObject)
	entryParams := jsoncompat.NewObject()
	if supported {
		entryParams.Set("message", truthyOr(params.MustGet("message"), ""))
	} else {
		entryParams.Set("message", "此问题需要在桌面回答")
	}
	entry := jsoncompat.NewObject()
	entry.Set("id", requestID)
	entry.Set("method", "acp/requestInput")
	entry.Set("supported", supported)
	if supported {
		entryParams.Set("schema", schema)
	}
	entry.Set("params", entryParams)
	v.replaceRequest(requestID, entry)
}

func (v *View) replaceRequest(requestID string, entry *jsoncompat.Object) {
	kept := v.Requests[:0:0]
	for _, request := range v.Requests {
		if id, _ := request.MustGet("id").(string); id != requestID {
			kept = append(kept, request)
		}
	}
	v.Requests = append(kept, entry)
}

// RemovePermission mirrors remove_permission.
func (v *View) RemovePermission(requestID string) {
	kept := v.Requests[:0:0]
	for _, request := range v.Requests {
		if id, _ := request.MustGet("id").(string); id != requestID {
			kept = append(kept, request)
		}
	}
	v.Requests = kept
}

// ApplyConfig mirrors apply_config.
func (v *View) ApplyConfig(options *jsoncompat.Array, modes *jsoncompat.Object) {
	for _, optionValue := range orArray(options).Items {
		v.applyConfigOption(obj(optionValue))
	}
	if current := getDefault(modes, "currentModeId", nil); jsoncompat.Truthy(current) {
		v.Mode = current
	}
}

func orArray(list *jsoncompat.Array) *jsoncompat.Array {
	if list == nil {
		return &jsoncompat.Array{}
	}
	return list
}

// SetMode mirrors set_mode.
func (v *View) SetMode(modeID any) { v.Mode = modeID }

func (v *View) applyConfigOption(option *jsoncompat.Object) {
	if option == nil {
		return
	}
	value := option.MustGet("currentValue")
	if value == nil {
		return
	}
	slot := truthyOr(option.MustGet("category"), option.MustGet("id"))
	slotText, _ := slot.(string)
	switch slotText {
	case "model":
		v.Model = value
	case "thought_level", "thinking":
		v.Thinking = value
	case "mode":
		v.Mode = value
	}
}

func (v *View) apply(kind string, update *jsoncompat.Object) {
	switch kind {
	case "user_message_chunk":
		turn := v.currentTurn()
		if turn != nil {
			for _, item := range messages(turn).Items {
				if message := obj(item); message != nil {
					if role, _ := message.MustGet("role").(string); role != "user" {
						if status, _ := turn.MustGet("status").(string); status == "inProgress" {
							turn.Set("status", "completed")
						}
						turn = nil
						break
					}
				}
			}
		}
		if turn == nil {
			turn = v.newTurn(v.nextID("turn"), nil)
		}
		message := jsoncompat.NewObject()
		message.Set("id", v.nextID("user"))
		message.Set("kind", "userMessage")
		message.Set("role", "user")
		message.Set("text", Text(update.MustGet("content")))
		messages(turn).Items = append(messages(turn).Items, message)
	case "agent_message_chunk":
		if text := Text(update.MustGet("content")); text != "" {
			message := v.assistantMessage(v.assistantTurn(), "agentMessage",
				"assistant", func(m *jsoncompat.Object) { m.Set("phase", "final") })
			previous, _ := message.MustGet("text").(string)
			message.Set("text", previous+text)
		}
	case "agent_thought_chunk":
		if text := Text(update.MustGet("content")); text != "" {
			message := v.assistantMessage(v.assistantTurn(), "agentThought",
				"activity", func(m *jsoncompat.Object) { m.Set("title", "思考摘要") })
			previous, _ := message.MustGet("text").(string)
			message.Set("text", previous+text)
		}
	case "tool_call":
		titleInput, _ := update.MustGet("title").(string)
		metadata := SubagentMeta(update.MustGet("rawInput"), titleInput)
		message := jsoncompat.NewObject()
		message.Set("id", v.nextID("tool"))
		message.Set("kind", "toolCall")
		message.Set("role", "activity")
		message.Set("title", truthyOr(update.MustGet("title"),
			truthyOr(update.MustGet("kind"), "工具调用")))
		message.Set("text", CommandSummary(update.MustGet("rawInput")))
		message.Set("output", FlattenOutput(update.MustGet("content")))
		message.Set("status", statusMap(getDefault(update, "status", nil)))
		if metadata.Len() > 0 {
			for _, key := range metadata.Keys() {
				value, _ := metadata.Get(key)
				message.Set(key, value)
			}
			message.Set("kind", "subagent")
		}
		messages(v.assistantTurn()).Items = append(messages(v.assistantTurn()).Items, message)
		if callID, ok := update.MustGet("toolCallId").(string); ok && callID != "" {
			v.toolIndex[callID] = message
		}
	case "tool_call_update":
		callID, _ := update.MustGet("toolCallId").(string)
		message := v.toolIndex[callID]
		if message == nil {
			return
		}
		if status := getDefault(update, "status", nil); jsoncompat.Truthy(status) {
			message.Set("status", statusMap(status))
		}
		if title := getDefault(update, "title", nil); jsoncompat.Truthy(title) {
			message.Set("title", title)
		}
		fallbackTitle, _ := truthyOr(message.MustGet("title"), "").(string)
		metadata := SubagentMeta(update.MustGet("rawInput"),
			stringValue(truthyOr(update.MustGet("title"), fallbackTitle)))
		for _, key := range metadata.Keys() {
			value, _ := metadata.Get(key)
			message.Set(key, value)
		}
		if id, ok := message.MustGet("subagentId").(string); ok && id != "" {
			message.Set("kind", "subagent")
		}
		if command := CommandSummary(update.MustGet("rawInput")); command != "" {
			message.Set("text", command)
		}
		if output := FlattenOutput(update.MustGet("content")); output != "" {
			message.Set("output", output)
		}
	case "plan":
		entries := &jsoncompat.Array{}
		if list := arr(truthyOr(update.MustGet("entries"), any(&jsoncompat.Array{}))); list != nil {
			for _, item := range list.Items {
				entry := jsoncompat.NewObject()
				if record := obj(item); record != nil {
					entry.Set("content", jsoncompat.PyStr(getDefault(record, "content", "")))
					status := "pending"
					if value := truthyOr(record.MustGet("status"), nil); value != nil {
						status = jsoncompat.PyStr(value)
					}
					entry.Set("status", status)
				} else {
					entry.Set("content", jsoncompat.PyStr(item))
					entry.Set("status", "pending")
				}
				entries.Items = append(entries.Items, entry)
			}
		}
		turn := v.assistantTurn()
		var plan *jsoncompat.Object
		for _, item := range messages(turn).Items {
			if message := obj(item); message != nil {
				if messageKind, _ := message.MustGet("kind").(string); messageKind == "plan" {
					plan = message
					break
				}
			}
		}
		if plan == nil {
			plan = jsoncompat.NewObject()
			plan.Set("id", v.nextID("plan"))
			plan.Set("kind", "plan")
			plan.Set("role", "activity")
			plan.Set("title", "计划")
			messages(turn).Items = append(messages(turn).Items, plan)
		}
		plan.Set("entries", entries)
		texts := []string{}
		allCompleted := len(entries.Items) > 0
		for _, item := range entries.Items {
			entry := obj(item)
			status, _ := entry.MustGet("status").(string)
			content, _ := entry.MustGet("content").(string)
			mark, ok := planMarks[status]
			if !ok {
				mark = "○"
			}
			texts = append(texts, mark+" "+content)
			if status != "completed" {
				allCompleted = false
			}
		}
		plan.Set("text", joinLines(texts))
		if allCompleted {
			plan.Set("status", "completed")
		} else {
			plan.Set("status", nil)
		}
	case "available_commands_update":
		v.Commands = nil
		if list := arr(truthyOr(update.MustGet("availableCommands"),
			any(&jsoncompat.Array{}))); list != nil {
			for _, item := range list.Items {
				if command := obj(item); command != nil {
					if name, ok := command.MustGet("name").(string); ok && name != "" {
						v.Commands = append(v.Commands, command)
					}
				}
			}
		}
	case "current_mode_update":
		if mode := getDefault(update, "currentModeId", nil); jsoncompat.Truthy(mode) {
			v.Mode = mode
		}
	case "config_option_update":
		if options := arr(update.MustGet("configOptions")); options != nil {
			for _, optionValue := range options.Items {
				v.applyConfigOption(obj(optionValue))
			}
		} else if option := obj(update.MustGet("configOption")); option != nil {
			v.applyConfigOption(option)
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func statusMap(status any) any {
	if text, ok := status.(string); ok {
		if mapped, exists := toolStatus[text]; exists {
			return mapped
		}
	}
	return status
}

func joinLines(parts []string) string {
	result := ""
	for i, part := range parts {
		if i > 0 {
			result += "\n"
		}
		result += part
	}
	return result
}
