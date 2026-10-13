package model

import (
	"encoding/json"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// requestFields mirrors the method → param whitelist, preserving the tuple
// order that normalize_request uses when rebuilding params.
var requestFields = []struct {
	Method string
	Fields []string
}{
	{"item/commandExecution/requestApproval", []string{"command", "cwd", "reason", "availableDecisions", "networkApprovalContext"}},
	{"item/fileChange/requestApproval", []string{"reason", "grantRoot", "changes"}},
	{"item/permissions/requestApproval", []string{"reason", "permissions"}},
	{"item/tool/requestUserInput", []string{"questions"}},
	{"tool/requestUserInput", []string{"questions"}},
	{"item/plan/requestImplementation", []string{"turnId", "planContent"}},
	{"mcpServer/elicitation/request", []string{"serverName", "message", "mode", "requestedSchema"}},
	{"acp/requestPermission", []string{"title", "command", "options"}},
	{"acp/requestInput", []string{"message", "schema"}},
}

func fieldsFor(method string) []string {
	for _, entry := range requestFields {
		if entry.Method == method {
			return entry.Fields
		}
	}
	return nil
}

// NormalizeState mirrors normalize_state: turns → ordered messages, stored
// tool-call pairing, opening injection, and the view metadata header.
func NormalizeState(state *jsoncompat.Object, connected bool) *jsoncompat.Object {
	turns := OrderedTurns(state)
	result := &jsoncompat.Array{}
	for ordinal, turnValue := range turns.Items {
		turn := obj(turnValue)
		if turn == nil {
			turn = jsoncompat.NewObject()
		}
		items := ItemsArray(getDefault(turn, "items", &jsoncompat.Array{}))
		messages := &jsoncompat.Array{}
		calls := map[string]*jsoncompat.Object{}
		for _, itemValue := range items.Items {
			item := obj(itemValue)
			if item == nil {
				continue
			}
			kind, _ := item.MustGet("type").(string)
			if kind == "function_call" || kind == "custom_tool_call" {
				callID := getDefault(item, "call_id", item.MustGet("id"))
				title := item.MustGet("name")
				if !jsoncompat.Truthy(title) {
					title = "工具调用"
				}
				arguments := item.MustGet("arguments")
				if _, exists := item.Get("arguments"); !exists {
					if input, ok := item.Get("input"); ok {
						arguments = input
					} else {
						arguments = ""
					}
				}
				message := jsoncompat.NewObject()
				message.Set("id", callID)
				message.Set("kind", "storedToolEvent")
				message.Set("role", "activity")
				message.Set("title", title)
				message.Set("text", jsoncompat.PyStr(arguments))
				calls[valueKey(callID)] = message
				messages.Items = append(messages.Items, message)
			} else if (kind == "function_call_output" || kind == "custom_tool_call_output") &&
				item.MustGet("call_id") != nil {
				if message, matched := calls[valueKey(item.MustGet("call_id"))]; matched {
					output := getDefault(item, "output", "")
					if _, isString := output.(string); !isString {
						output = string(jsoncompat.EncodePythonDefault(output))
					}
					message.Set("output", output)
				} else {
					messages.Items = append(messages.Items, NormalizeItem(item))
				}
			} else {
				messages.Items = append(messages.Items, NormalizeItem(item))
			}
		}
		hasUser := false
		for _, message := range messages.Items {
			if role, _ := obj(message).MustGet("role").(string); role == "user" {
				hasUser = true
				break
			}
		}
		if !hasUser {
			params := obj(getDefault(turn, "params", &jsoncompat.Object{}))
			var opening any = ""
			if params != nil {
				opening = getDefault(params, "input", &jsoncompat.Array{})
			}
			if openingText := TextContent(opening); openingText != "" {
				turnID := any(jsoncompat.Num(ordinal))
				if value, ok := turn.Get("turnId"); ok {
					turnID = value
				}
				openingRow := jsoncompat.NewObject()
				openingRow.Set("id", "opening-"+jsoncompat.PyStr(turnID))
				openingRow.Set("role", "user")
				openingRow.Set("kind", "userMessage")
				openingRow.Set("text", openingText)
				messages.Items = append([]any{openingRow}, messages.Items...)
			}
		}
		turnRow := jsoncompat.NewObject()
		turnIDValue := turn.MustGet("turnId")
		if !jsoncompat.Truthy(turnIDValue) {
			turnIDValue = jsoncompat.PyStr(jsoncompat.Num(ordinal))
		}
		turnRow.Set("id", turnIDValue)
		turnRow.Set("status", turn.MustGet("status"))
		turnRow.Set("startedAt", turn.MustGet("turnStartedAtMs"))
		turnRow.Set("messages", messages)
		turnRow.Set("actionable", jsoncompat.Truthy(turn.MustGet("turnId")) &&
			turnIDText(turn) != "history")
		turnRow.Set("error", turn.MustGet("error"))
		turnRow.Set("diff", turn.MustGet("diff"))
		result.Items = append(result.Items, turnRow)
	}
	var history *jsoncompat.Object
	if container := obj(state.MustGet("turnHistory")); container != nil {
		history = obj(container.MustGet("history"))
	}
	var historyComplete any = true
	if history != nil {
		if complete, ok := history.Get("isComplete"); ok {
			historyComplete = complete
		} else {
			historyComplete = paginationFallback(state)
		}
	} else {
		historyComplete = paginationFallback(state)
	}
	settings := obj(state.MustGet("latestThreadSettings"))
	if settings == nil {
		settings = jsoncompat.NewObject()
	}
	tier := settings
	if _, ok := settings.Get("serviceTier"); !ok {
		tier = jsoncompat.NewObject()
		if len(turns.Items) > 0 {
			if params := obj(obj(turns.Items[len(turns.Items)-1]).MustGet("params")); params != nil {
				tier = params
			}
		}
	}
	view := jsoncompat.NewObject()
	id := state.MustGet("id")
	if !jsoncompat.Truthy(id) {
		id = state.MustGet("sessionId")
	}
	view.Set("id", id)
	title := state.MustGet("title")
	if !jsoncompat.Truthy(title) {
		title = "未命名聊天"
	}
	view.Set("title", title)
	view.Set("cwd", state.MustGet("cwd"))
	view.Set("model", state.MustGet("latestModel"))
	view.Set("provider", state.MustGet("modelProvider"))
	effort := state.MustGet("latestReasoningEffort")
	if !jsoncompat.Truthy(effort) {
		effortSettings := obj(state.MustGet("latestThreadSettings"))
		if effortSettings == nil {
			effortSettings = jsoncompat.NewObject()
		}
		effort = effortSettings.MustGet("effort")
	}
	view.Set("effort", effort)
	view.Set("connected", connected)
	runtime := obj(state.MustGet("threadRuntimeStatus"))
	status := any("idle")
	if runtime != nil {
		if value, ok := runtime.Get("type"); ok {
			status = value
		} else {
			status = "idle"
		}
	}
	view.Set("status", status)
	collaboration := obj(state.MustGet("latestCollaborationMode"))
	if collaboration == nil {
		collaboration = jsoncompat.NewObject()
	}
	view.Set("collaborationMode", collaboration.MustGet("mode"))
	if serviceTier, ok := tier.Get("serviceTier"); ok {
		view.Set("serviceTier", serviceTier)
	}
	goal := state.MustGet("threadGoal")
	if !jsoncompat.Truthy(goal) {
		goal = state.MustGet("completedThreadGoal")
	}
	view.Set("goal", jsoncompat.Clone(goal))
	view.Set("turns", result)
	view.Set("requests", PendingRequests(state))
	view.Set("historyComplete", historyComplete)
	return view
}

func paginationFallback(state *jsoncompat.Object) any {
	if pagination := obj(state.MustGet("turnsPagination")); pagination != nil {
		if loaded, ok := pagination.Get("hasLoadedOldest"); ok {
			return loaded
		}
	}
	return true
}

func turnIDText(turn *jsoncompat.Object) string {
	text, _ := turn.MustGet("turnId").(string)
	return text
}

// PendingRequests mirrors pending_requests: normalize each request, enrich
// approvals from related turn items, then append async questions.
func PendingRequests(state *jsoncompat.Object) *jsoncompat.Array {
	turns := OrderedTurns(state)
	requests := &jsoncompat.Array{}
	for _, requestValue := range orEmptyArray(state.MustGet("requests")).Items {
		request := obj(requestValue)
		if request == nil {
			continue
		}
		normalized := NormalizeRequest(request)
		params := obj(getDefault(request, "params", &jsoncompat.Object{}))
		var itemID any
		if params != nil {
			itemID = params.MustGet("itemId")
		}
		var related *jsoncompat.Object
		if jsoncompat.Truthy(itemID) {
			for _, turnValue := range turns.Items {
				for _, itemValue := range ItemsArray(getDefault(obj(turnValue),
					"items", &jsoncompat.Array{})).Items {
					if item := obj(itemValue); item != nil &&
						valueEqual(item.MustGet("id"), itemID) {
						related = item
						break
					}
				}
				if related != nil {
					break
				}
			}
		}
		supported, _ := normalized.MustGet("supported").(bool)
		if related != nil && supported {
			method, _ := request.MustGet("method").(string)
			normalizedParams := obj(normalized.MustGet("params"))
			if method == "item/fileChange/requestApproval" {
				normalizedParams.Set("changes", jsoncompat.Clone(getDefault(related,
					"changes", &jsoncompat.Array{})))
			} else if method == "item/commandExecution/requestApproval" &&
				!jsoncompat.Truthy(normalizedParams.MustGet("command")) {
				normalizedParams.Set("command", getDefault(related, "command", ""))
			}
		}
		requests.Items = append(requests.Items, normalized)
	}
	async := AsyncRequests(state)
	requests.Items = append(requests.Items, async.Items...)
	return requests
}

// NormalizeRequest mirrors normalize_request: per-method whitelists, the
// desktop-only identity-verification guard and the elicitation form gate.
func NormalizeRequest(request *jsoncompat.Object) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("id", request.MustGet("id"))
	result.Set("method", request.MustGet("method"))
	params := obj(getDefault(request, "params", &jsoncompat.Object{}))
	if params == nil {
		params = jsoncompat.NewObject()
	}
	method, _ := request.MustGet("method").(string)
	resultParams := jsoncompat.NewObject()
	verification := strings.Contains(
		string(jsoncompat.EncodePythonDefaultASCII(getDefault(params, "_meta",
			&jsoncompat.Object{}))), "openai/userVerification")
	if _, hasKey := params.Get("openai/userVerification"); hasKey {
		verification = true
	}
	supported := fieldsFor(method) != nil && !verification
	if method == "mcpServer/elicitation/request" {
		mode := getDefault(params, "mode", "form")
		if mode != "form" || !FormSupported(obj(getDefault(params, "requestedSchema",
			&jsoncompat.Object{}))) {
			supported = false
		}
	}
	if supported {
		for _, field := range fieldsFor(method) {
			if value, ok := params.Get(field); ok {
				resultParams.Set(field, jsoncompat.Clone(value))
			}
		}
		if method == "mcpServer/elicitation/request" {
			if computer := ComputerUseApproval(params); computer != nil {
				resultParams.Set("computerUse", computer)
			}
		}
	} else {
		resultParams.Set("message", "请在桌面 App 处理此请求")
	}
	result.Set("params", resultParams)
	result.Set("supported", supported)
	return result
}

var formRootKeys = map[string]bool{"type": true, "properties": true, "required": true,
	"title": true, "description": true, "$schema": true, "additionalProperties": true}

var formFieldKeys = map[string]bool{"type": true, "title": true, "description": true,
	"enum": true, "enumNames": true, "default": true, "minLength": true,
	"maxLength": true, "minimum": true, "maximum": true}

// FormSupported mirrors form_supported's allowlist checks.
func FormSupported(schema *jsoncompat.Object) bool {
	if schema == nil {
		return false
	}
	if kind, _ := schema.MustGet("type").(string); kind != "object" {
		return false
	}
	for _, key := range schema.Keys() {
		if !formRootKeys[key] {
			return false
		}
	}
	properties := obj(schema.MustGet("properties"))
	if properties == nil {
		return true
	}
	for _, fieldValue := range properties.Keys() {
		field := obj(properties.MustGet(fieldValue))
		if field == nil {
			return false
		}
		for _, key := range field.Keys() {
			if !formFieldKeys[key] {
				return false
			}
		}
		fieldType := "string"
		if value, ok := field.Get("type"); ok {
			fieldType, _ = value.(string)
		}
		switch fieldType {
		case "string", "number", "integer", "boolean":
		default:
			return false
		}
	}
	return true
}

// ComputerUseApproval mirrors computer_use_approval: only the scope choices
// present in the live desktop approval.
func ComputerUseApproval(params *jsoncompat.Object) *jsoncompat.Object {
	meta := obj(params.MustGet("_meta"))
	if meta == nil || !jsoncompat.Truthy(params.MustGet("_meta")) {
		meta = jsoncompat.NewObject()
	}
	if kind, _ := meta.MustGet("codex_approval_kind").(string); kind != "mcp_tool_call" {
		return nil
	}
	connector := strings.Trim(connectorSlugPattern.ReplaceAllString(
		strings.ToLower(jsoncompat.PyStr(getDefault(meta, "connector_id", ""))), "-"), "-")
	tool := obj(meta.MustGet("tool_params"))
	if !jsoncompat.Truthy(meta.MustGet("tool_params")) {
		tool = jsoncompat.NewObject()
	}
	var app any
	if tool != nil {
		app = tool.MustGet("app")
	}
	schema := obj(params.MustGet("requestedSchema"))
	if !jsoncompat.Truthy(params.MustGet("requestedSchema")) {
		schema = jsoncompat.NewObject()
	}
	mode := getDefault(params, "mode", "form")
	appText, appIsString := app.(string)
	if mode != "form" || !(connector == "computer-use" ||
		strings.HasPrefix(connector, "computer-use-")) || !appIsString ||
		strings.TrimSpace(appText) == "" || jsoncompat.Truthy(schema.MustGet("properties")) ||
		jsoncompat.Truthy(schema.MustGet("required")) {
		return nil
	}
	modes := &jsoncompat.Array{}
	if persist := meta.MustGet("persist"); persist != nil {
		if text, isString := persist.(string); isString {
			modes.Items = []any{text}
		} else if list := arr(persist); list != nil {
			modes = list
		}
	}
	persistModes := &jsoncompat.Array{}
	for _, allowed := range [...]string{"session", "always"} {
		for _, mode := range modes.Items {
			if text, _ := mode.(string); text == allowed {
				persistModes.Items = append(persistModes.Items, allowed)
				break
			}
		}
	}
	computer := jsoncompat.NewObject()
	computer.Set("app", app)
	computer.Set("persistModes", persistModes)
	return computer
}

// AsyncRequests mirrors async_requests: unanswered agent questions on the
// last in-flight turn become bridge input requests.
func AsyncRequests(state *jsoncompat.Object) *jsoncompat.Array {
	result := &jsoncompat.Array{}
	turns := OrderedTurns(state)
	for _, turnValue := range turns.Items[max(0, len(turns.Items)-1):] {
		turn := obj(turnValue)
		if turn == nil {
			continue
		}
		status, _ := turn.MustGet("status").(string)
		if status != "inProgress" && status != "completed" {
			continue
		}
		items := ItemsArray(getDefault(turn, "items", &jsoncompat.Array{}))
		answered := map[string]bool{}
		for _, itemValue := range items.Items {
			item := obj(itemValue)
			if item == nil {
				continue
			}
			kind, _ := item.MustGet("type").(string)
			itemStatus, _ := item.MustGet("status").(string)
			if kind == "userMessage" ||
				(kind == "steeringUserMessage" && itemStatus == "accepted") {
				content := getDefault(item, "content", getDefault(item, "input",
					&jsoncompat.Array{}))
				for _, reply := range QuestionReplies(TextContent(content)) {
					// Python stores the raw (string) ids; the async key is the
					// json.dumps text, matched by plain string equality.
					if value, ok := reply.Get("questionItemId"); ok {
						if text, isString := value.(string); isString {
							answered[text] = true
						}
					}
				}
			}
		}
		for _, itemValue := range items.Items {
			item := obj(itemValue)
			if item == nil {
				continue
			}
			if kind, _ := item.MustGet("type").(string); kind != "agentMessage" ||
				!jsoncompat.Truthy(item.MustGet("questions")) {
				continue
			}
			questions := &jsoncompat.Array{}
			for index, questionValue := range orEmptyArray(item.MustGet("questions")).Items {
				question := obj(questionValue)
				if question == nil {
					continue
				}
				key := valueKey(&jsoncompat.Array{Items: []any{
					"request_user_input_async", item.MustGet("id"), jsoncompat.Num(index)}})
				if answered[key] {
					continue
				}
				entry := jsoncompat.NewObject()
				entry.Set("id", key)
				entry.Set("question", question.MustGet("title"))
				options := &jsoncompat.Array{}
				if list := arr(question.MustGet("options")); list != nil {
					for _, option := range list.Items {
						label := jsoncompat.NewObject()
						label.Set("label", option)
						options.Items = append(options.Items, label)
					}
				}
				entry.Set("options", options)
				questions.Items = append(questions.Items, entry)
			}
			if len(questions.Items) > 0 {
				itemID, _ := item.MustGet("id").(string)
				async := jsoncompat.NewObject()
				async.Set("id", "async:"+jsoncompat.PyStr(turn.MustGet("turnId"))+":"+itemID)
				async.Set("method", "bridge/requestUserInputAsync")
				async.Set("supported", true)
				asyncParams := jsoncompat.NewObject()
				asyncParams.Set("questions", questions)
				async.Set("params", asyncParams)
				result.Items = append(result.Items, async)
			}
		}
	}
	return result
}

var _ = json.Marshal
