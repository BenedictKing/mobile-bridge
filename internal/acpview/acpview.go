// Package acpview is the Go port of bridge/agents/acp_view.py: the ACP
// session view that folds session/update payloads into the normalized view
// contract, plus elicitation-form validation and payload flattening helpers.
// Parity with Python — including snapshot key order and error strings — is
// enforced against conformance/acpview goldens.
package acpview

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

const (
	maxFormFields = 12
	maxFormChars  = 20000
	maxFormItems  = 20
)

var toolStatus = map[string]string{"pending": "inProgress", "in_progress": "inProgress",
	"completed": "completed", "failed": "failed"}

var planMarks = map[string]string{"completed": "✓", "in_progress": "▸", "pending": "○"}

func obj(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

func arr(value any) *jsoncompat.Array {
	array, _ := value.(*jsoncompat.Array)
	return array
}

func getDefault(object *jsoncompat.Object, key string, fallback any) any {
	if object == nil {
		return fallback
	}
	if value, ok := object.Get(key); ok {
		return value
	}
	return fallback
}

// truthyOr mirrors `a or b` for JSON values.
func truthyOr(value, fallback any) any {
	if jsoncompat.Truthy(value) {
		return value
	}
	return fallback
}

func runePrefix(text string, limit int) string {
	count := 0
	for range text {
		count++
	}
	if count <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit])
}

// FormSupported mirrors acp_form_supported: the tiny wrapper vocabulary the
// phone can render.
func FormSupported(schema *jsoncompat.Object) bool {
	if schema == nil {
		return false
	}
	if kind, ok := truthyOr(schema.MustGet("type"), "object").(string); !ok || kind != "object" {
		return false
	}
	properties := obj(schema.MustGet("properties"))
	if properties == nil || properties.Len() < 1 || properties.Len() > maxFormFields {
		return false
	}
	if required, ok := schema.Get("required"); ok && required != nil {
		list := arr(required)
		if list == nil {
			return false
		}
		for _, item := range list.Items {
			key, isString := item.(string)
			if !isString {
				// Python set(required) <= set(properties) tolerates any
				// hashable member; non-strings simply never match.
				continue
			}
			if _, exists := properties.Get(key); !exists {
				return false
			}
		}
	}
	if runeCount(string(jsoncompat.EncodePythonDefault(schema))) > 65536 {
		return false
	}
	allowedFields := map[string]bool{"type": true, "title": true, "description": true,
		"enum": true, "oneOf": true, "anyOf": true, "items": true, "default": true,
		"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
		"minItems": true, "maxItems": true, "format": true, "pattern": true, "_meta": true}
	for _, name := range properties.Keys() {
		field := obj(properties.MustGet(name))
		if field == nil {
			return false
		}
		for _, key := range field.Keys() {
			if !allowedFields[key] {
				return false
			}
		}
		kind, _ := truthyOr(field.MustGet("type"), "string").(string)
		switch kind {
		case "string", "number", "integer", "boolean", "array":
		default:
			return false
		}
		if kind == "array" {
			items := obj(field.MustGet("items"))
			if items == nil {
				return false
			}
			for _, key := range items.Keys() {
				if key != "anyOf" && key != "enum" && key != "oneOf" {
					return false
				}
			}
		}
	}
	return true
}

func runeCount(text string) int {
	count := 0
	for range text {
		count++
	}
	return count
}

// enumValues mirrors _enum_values.
func enumValues(field *jsoncompat.Object) *jsoncompat.Array {
	if values := arr(field.MustGet("enum")); values != nil && len(values.Items) > 0 {
		picked := &jsoncompat.Array{}
		for _, value := range values.Items {
			if text, ok := value.(string); ok {
				picked.Items = append(picked.Items, text)
			}
		}
		return picked
	}
	variants := arr(truthyOr(field.MustGet("oneOf"), field.MustGet("anyOf")))
	if variants != nil && len(variants.Items) > 0 {
		picked := &jsoncompat.Array{}
		for _, variant := range variants.Items {
			if entry := obj(variant); entry != nil {
				if value, ok := entry.MustGet("const").(string); ok {
					picked.Items = append(picked.Items, value)
				}
			}
		}
		return picked
	}
	return nil
}

func hasChoice(choices *jsoncompat.Array, value string) bool {
	if choices == nil {
		return false
	}
	for _, item := range choices.Items {
		if text, ok := item.(string); ok && text == value {
			return true
		}
	}
	return false
}

// ValidateForm mirrors validate_acp_form: normalized content or a
// phone-friendly error.
func ValidateForm(schema *jsoncompat.Object, content any) (*jsoncompat.Object, error) {
	contentObject := obj(content)
	if contentObject == nil {
		return nil, errors.New("请填写表单")
	}
	if schema == nil {
		schema = jsoncompat.NewObject()
	}
	properties := obj(schema.MustGet("properties"))
	if properties == nil || properties.Len() == 0 {
		return nil, errors.New("此表单没有可填写的字段")
	}
	for _, key := range contentObject.Keys() {
		if _, exists := properties.Get(key); !exists {
			return nil, errors.New("回答包含未知字段")
		}
	}
	var required []string
	if list := arr(truthyOr(schema.MustGet("required"), &jsoncompat.Array{})); list != nil {
		for _, item := range list.Items {
			if key, ok := item.(string); ok {
				required = append(required, key)
			}
		}
	}
	for _, key := range required {
		if _, exists := contentObject.Get(key); !exists {
			return nil, errors.New("请填写所有必填项")
		}
	}
	label := func(field *jsoncompat.Object, key string) string {
		return jsoncompat.PyStr(truthyOr(field.MustGet("title"), key))
	}
	result := jsoncompat.NewObject()
	for _, key := range contentObject.Keys() {
		value, _ := contentObject.Get(key)
		field := obj(truthyOr(properties.MustGet(key), any(jsoncompat.NewObject())))
		kind, _ := truthyOr(field.MustGet("type"), "string").(string)
		var choices *jsoncompat.Array
		if kind == "string" || kind == "array" {
			choices = enumValues(field)
		}
		if kind == "array" {
			items := obj(truthyOr(field.MustGet("items"), any(jsoncompat.NewObject())))
			if choices == nil {
				choices = enumValues(items)
			}
			minItems := 1
			if value, ok := field.Get("minItems"); ok && value != nil {
				minItems = int(parseFloatValue(value))
			}
			maxItems := maxFormItems
			if value, ok := field.Get("maxItems"); ok && value != nil {
				maxItems = min(maxItems, int(parseFloatValue(value)))
			}
			list := arr(value)
			if list == nil || len(list.Items) < minItems {
				return nil, errors.New("请至少选择一项：" + label(field, key))
			}
			allStrings := true
			for _, item := range list.Items {
				if _, ok := item.(string); !ok {
					allStrings = false
					break
				}
			}
			if len(list.Items) > maxItems || !allStrings {
				return nil, errors.New("选择数量超出限制：" + label(field, key))
			}
			if choices != nil {
				for _, item := range list.Items {
					if text, _ := item.(string); !hasChoice(choices, text) {
						return nil, errors.New("包含无效选项：" + label(field, key))
					}
				}
			}
			result.Set(key, value)
			continue
		}
		if kind == "boolean" {
			if _, ok := value.(bool); !ok {
				return nil, errors.New("请勾选或留空：" + label(field, key))
			}
			result.Set(key, value)
			continue
		}
		if kind == "number" || kind == "integer" {
			number, isNumber := value.(json.Number)
			isBool := false
			if _, isBoolValue := value.(bool); isBoolValue {
				isBool = true
			}
			if !isNumber || isBool {
				return nil, errors.New("请填写数字：" + label(field, key))
			}
			parsed := parseFloat(number)
			if kind == "integer" && parsed != float64(int64(parsed)) {
				return nil, errors.New("请填写整数：" + label(field, key))
			}
			if minimum, ok := field.Get("minimum"); ok && minimum != nil {
				if parsed < parseFloatValue(minimum) {
					return nil, errors.New("数值超出范围：" + label(field, key))
				}
			}
			if maximum, ok := field.Get("maximum"); ok && maximum != nil {
				if parsed > parseFloatValue(maximum) {
					return nil, errors.New("数值超出范围：" + label(field, key))
				}
			}
			result.Set(key, value)
			continue
		}
		text, isString := value.(string)
		if !isString {
			return nil, errors.New("请填写文本：" + label(field, key))
		}
		minLength := 0
		if value, ok := field.Get("minLength"); ok && value != nil {
			minLength = int(parseFloatValue(value))
		}
		maxLength := maxFormChars
		if value, ok := field.Get("maxLength"); ok && value != nil {
			maxLength = int(parseFloatValue(value))
		}
		if length := runeCount(text); length < minLength || length > maxLength {
			return nil, errors.New("文本长度超出限制：" + label(field, key))
		}
		if _, hasEnum := field.Get("enum"); choices != nil && hasEnum && !hasChoice(choices, text) {
			return nil, errors.New("请选择有效的选项：" + label(field, key))
		}
		if text == "" {
			for _, requiredKey := range required {
				if requiredKey == key {
					return nil, errors.New("请填写所有必填项")
				}
			}
			continue
		}
		result.Set(key, value)
	}
	return result, nil
}

func parseFloat(number json.Number) float64 { return parseFloatValue(number) }

func parseFloatValue(value any) float64 {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	parsed, err := number.Float64()
	if err != nil {
		return 0
	}
	return parsed
}

// Normalize mirrors acp_normalize.
func Normalize(state *jsoncompat.Object, connected bool) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, key := range state.Keys() {
		value, _ := state.Get(key)
		result.Set(key, value)
	}
	result.Set("connected", connected)
	return result
}

// Text mirrors _text.
func Text(content any) string {
	switch typed := content.(type) {
	case string:
		return textutil.StripInternalReminders(typed)
	case *jsoncompat.Array:
		parts := make([]string, 0, len(typed.Items))
		for _, item := range typed.Items {
			if text := Text(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case *jsoncompat.Object:
		if text, ok := typed.MustGet("text").(string); ok && text != "" {
			return textutil.StripInternalReminders(text)
		}
		// Kimi migration records may wrap ACP blocks in content/message/data.
		for _, key := range []string{"content", "message", "data"} {
			if value, ok := typed.Get(key); ok {
				if text := Text(value); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

var outputKeys = [...]string{"text", "output", "message", "summary", "content",
	"result", "transcript", "events", "messages"}

// FlattenOutput mirrors _output: flatten tool output, including nested agent
// transcripts, with a sorted-JSON fallback.
func FlattenOutput(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case *jsoncompat.Object:
		parts := []string{}
		for _, key := range outputKeys {
			if _, ok := typed.Get(key); ok {
				if flattened := FlattenOutput(typed.MustGet(key)); flattened != "" {
					parts = append(parts, flattened)
				}
			}
		}
		if joined := strings.Join(parts, "\n"); joined != "" {
			return joined
		}
		return runePrefix(string(jsoncompat.EncodePythonDefaultSorted(typed)), 4000)
	case *jsoncompat.Array:
		parts := []string{}
		for _, item := range typed.Items {
			if flattened := FlattenOutput(item); flattened != "" {
				parts = append(parts, flattened)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

var commandPreferred = [...]string{"command", "cmd", "script", "shellCommand", "path", "file_path"}
var commandNested = [...]string{"input", "arguments", "args", "params", "payload", "toolCall"}

// CommandSummary mirrors _command_summary.
func CommandSummary(rawInput any) string {
	switch typed := rawInput.(type) {
	case string:
		return typed
	case *jsoncompat.Object:
		for _, key := range commandPreferred {
			if value, ok := typed.Get(key); ok {
				if text, isString := value.(string); isString && strings.TrimSpace(text) != "" {
					return text
				}
			}
		}
		for _, key := range commandNested {
			if value, ok := typed.Get(key); ok {
				if summary := CommandSummary(value); summary != "" {
					return summary
				}
			}
		}
		return runePrefix(string(jsoncompat.EncodePythonDefaultSorted(typed)), 2000)
	default:
		return ""
	}
}

// QuestionSummary renders an AskUserQuestion rawInput (questions with labelled
// options) as a readable question-plus-choices block; empty for every other
// tool input, which keeps the CommandSummary path untouched.
func QuestionSummary(rawInput any) string {
	root := obj(rawInput)
	if root == nil {
		return ""
	}
	questions := arr(root.MustGet("questions"))
	if questions == nil || len(questions.Items) == 0 {
		return ""
	}
	var body strings.Builder
	for _, value := range questions.Items {
		question := obj(value)
		if question == nil {
			continue
		}
		title := strings.TrimSpace(jsoncompat.PyStr(truthyOr(
			question.MustGet("question"), question.MustGet("header"))))
		if title == "" {
			continue
		}
		if body.Len() > 0 {
			body.WriteString("\n\n")
		}
		body.WriteString(title)
		if list := arr(question.MustGet("options")); list != nil {
			for _, optionValue := range list.Items {
				option := obj(optionValue)
				if option == nil {
					continue
				}
				if label := strings.TrimSpace(jsoncompat.PyStr(option.MustGet("label"))); label != "" {
					body.WriteString("\n· " + label)
				}
			}
		}
	}
	return body.String()
}

var subagentKeys = [...]struct{ source, target string }{
	{"agent_id", "subagentId"}, {"agentId", "subagentId"}, {"subagent_id", "subagentId"},
	{"actual_subagent_type", "subagentType"}, {"actualSubagentType", "subagentType"},
	{"subagent_type", "subagentType"}, {"description", "subagentDescription"},
	{"prompt", "subagentPrompt"}, {"stop_reason", "stopReason"}, {"stopReason", "stopReason"},
}

// SubagentMeta mirrors _subagent_meta: BFS over nested payloads, first
// discovery wins per output field.
func SubagentMeta(rawInput any, title string) *jsoncompat.Object {
	found := jsoncompat.NewObject()
	pending := []*jsoncompat.Object{}
	if root := obj(rawInput); root != nil {
		pending = append(pending, root)
	}
	for len(pending) > 0 {
		current := pending[0]
		pending = pending[1:]
		for _, mapping := range subagentKeys {
			if value, ok := current.Get(mapping.source); ok {
				if scalarValue(value) && strings.TrimSpace(jsoncompat.PyStr(value)) != "" {
					if _, taken := found.Get(mapping.target); !taken {
						found.Set(mapping.target, jsoncompat.PyStr(value))
					}
				}
			}
		}
		for _, key := range commandNested {
			if value, ok := current.Get(key); ok {
				if nested := obj(value); nested != nil {
					pending = append(pending, nested)
				}
			}
		}
	}
	lowered := strings.ToLower(title)
	if found.Len() > 0 || strings.Contains(lowered, "agent") || strings.Contains(lowered, "子 agent") {
		return found
	}
	return jsoncompat.NewObject()
}

func scalarValue(value any) bool {
	switch value.(type) {
	case string, json.Number, bool:
		return true
	}
	return false
}
