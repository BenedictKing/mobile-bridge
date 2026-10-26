// Package model is the Go port of bridge/model.py: normalizing desktop state
// into the phone-facing view (normalize_state), JSON-patch application, and
// request/approval normalization. Byte parity with Python — including
// quirks like the always-set requestId ("None" when no client id exists) —
// is enforced against conformance/model goldens.
package model

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

var fileDrivePattern = regexp.MustCompile(`^/[A-Za-z]:[\\/].+$`)
var connectorSlugPattern = regexp.MustCompile(`[^a-z0-9]+`)

func obj(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

func arr(value any) *jsoncompat.Array {
	array, _ := value.(*jsoncompat.Array)
	return array
}

func orEmptyArray(value any) *jsoncompat.Array {
	if array := arr(value); array != nil {
		return array
	}
	return &jsoncompat.Array{}
}

// valueEqual compares parsed JSON values structurally.
func valueEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	return string(jsoncompat.EncodeCompact(a)) == string(jsoncompat.EncodeCompact(b))
}

func valueKey(value any) string { return string(jsoncompat.EncodeCompact(value)) }

// getDefault mirrors dict.get(key, default): the default only applies when
// the key is missing entirely (an explicit null is returned as-is).
func getDefault(object *jsoncompat.Object, key string, fallback any) any {
	if value, ok := object.Get(key); ok {
		return value
	}
	return fallback
}

func textPartType(kind string) bool {
	switch kind {
	case "text", "input_text", "output_text", "inputText":
		return true
	}
	return false
}

// TextContent mirrors text_content: plain strings pass through; lists join
// their text-typed parts with newlines.
func TextContent(content any) string {
	if text, ok := content.(string); ok {
		return text
	}
	if list := arr(content); list != nil {
		parts := []string{}
		for _, item := range list.Items {
			part := obj(item)
			if part == nil {
				continue
			}
			kind, _ := part.MustGet("type").(string)
			if !textPartType(kind) {
				continue
			}
			if value, ok := part.Get("text"); ok {
				parts = append(parts, jsoncompat.PyStr(value))
			} else {
				parts = append(parts, "")
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

const (
	questionPrefix = "<send_user_message_question_reply>"
	questionSuffix = "</send_user_message_question_reply>"
)

// QuestionReplies mirrors question_replies: strict envelope + JSON list of
// objects, else nothing.
func QuestionReplies(text string) []*jsoncompat.Object {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, questionPrefix) || !strings.HasSuffix(trimmed, questionSuffix) {
		return nil
	}
	inner := trimmed[len(questionPrefix) : len(trimmed)-len(questionSuffix)]
	value, err := jsoncompat.Unmarshal([]byte(inner))
	if err != nil {
		return nil
	}
	list := arr(value)
	if list == nil {
		return nil
	}
	replies := []*jsoncompat.Object{}
	for _, item := range list.Items {
		if entry := obj(item); entry == nil {
			return nil
		}
		replies = append(replies, obj(item))
	}
	return replies
}

const filesMarker = "# Files mentioned by the user:"

// UserDisplayText mirrors user_display_text: strip the desktop's
// "files mentioned" plumbing, keep the real request.
func UserDisplayText(text string) string {
	text = textutil.StripInternalReminders(text)
	if !strings.Contains(text, filesMarker) {
		return text
	}
	const requestMarker = "\n## My request:\n"
	header := strings.Index(text, filesMarker)
	boundary := strings.LastIndex(text, requestMarker)
	if header < 0 || boundary <= header {
		return text
	}
	request := strings.Trim(text[boundary+len(requestMarker):], "\r\n")
	if request == "" {
		return text
	}
	return request
}

// RequestID mirrors the truthy or-chain over the client id fields.
func RequestID(item *jsoncompat.Object) any {
	for _, key := range [...]string{"clientId", "clientUserMessageId", "client_id"} {
		if value, ok := item.Get(key); ok && jsoncompat.Truthy(value) {
			return value
		}
	}
	return nil
}

// ItemsArray mirrors items_array: a list passes through; a canonical-history
// object resolves through islands/entitiesByKey; other dicts use "items".
func ItemsArray(items any) *jsoncompat.Array {
	if list := arr(items); list != nil {
		return list
	}
	if container := obj(items); container != nil {
		if entities := obj(container.MustGet("entitiesByKey")); entities != nil && entities.Len() > 0 {
			resolved := &jsoncompat.Array{}
			for _, islandValue := range orEmptyArray(container.MustGet("islands")).Items {
				island := obj(islandValue)
				if island == nil {
					continue
				}
				for _, entryValue := range orEmptyArray(island.MustGet("entries")).Items {
					entry := obj(entryValue)
					if entry == nil {
						continue
					}
					if key, ok := entry.MustGet("value").(string); ok {
						if entity, exists := entities.Get(key); exists {
							resolved.Items = append(resolved.Items, entity)
						}
					}
				}
			}
			return resolved
		}
		return orEmptyArray(container.MustGet("items"))
	}
	return &jsoncompat.Array{}
}

// OrderedTurns mirrors ordered_turns: canonical turnHistory islands resolve
// entities with dedupe; anything else falls back to the plain turns list.
// A nil state (no snapshot yet) yields no turns.
func OrderedTurns(state *jsoncompat.Object) *jsoncompat.Array {
	if state == nil {
		return &jsoncompat.Array{}
	}
	history := obj(state.MustGet("turnHistory"))
	if history == nil {
		return orEmptyArray(state.MustGet("turns"))
	}
	if kind, _ := history.MustGet("kind").(string); kind != "canonical" {
		return orEmptyArray(state.MustGet("turns"))
	}
	record := obj(history.MustGet("history"))
	if record == nil {
		return &jsoncompat.Array{}
	}
	entities := obj(record.MustGet("entitiesByKey"))
	turns := &jsoncompat.Array{}
	seen := map[string]bool{}
	for _, islandValue := range orEmptyArray(record.MustGet("islands")).Items {
		island := obj(islandValue)
		if island == nil {
			continue
		}
		for _, entryValue := range orEmptyArray(island.MustGet("entries")).Items {
			entry := obj(entryValue)
			if entry == nil {
				continue
			}
			if key, ok := entry.MustGet("value").(string); ok {
				if entity, exists := entities.Get(key); exists && !seen[key] {
					seen[key] = true
					turns.Items = append(turns.Items, entity)
				}
			}
		}
	}
	return turns
}

// ApplyPatches mirrors apply_patches: add/remove/replace with JSON-pointer
// paths (also accepted as pre-split arrays), ~0/~1 unescaping and "-" append.
func ApplyPatches(state any, patches *jsoncompat.Array) (any, error) {
	for _, patchValue := range patches.Items {
		patch := obj(patchValue)
		if patch == nil {
			return nil, errors.New("TypeError: patch is not an object")
		}
		var path []string
		if raw, ok := patch.Get("path"); ok {
			if text, isString := raw.(string); isString {
				for _, segment := range strings.Split(text, "/")[1:] {
					segment = strings.ReplaceAll(segment, "~1", "/")
					segment = strings.ReplaceAll(segment, "~0", "~")
					path = append(path, segment)
				}
			} else if list := arr(raw); list != nil {
				for _, segmentValue := range list.Items {
					segment := ""
					switch typed := segmentValue.(type) {
					case string:
						segment = typed
					case json.Number:
						segment = typed.String()
					case bool:
						if typed {
							segment = "1"
						} else {
							segment = "0"
						}
					}
					path = append(path, segment)
				}
			}
		}
		operation, _ := patch.MustGet("op").(string)
		value := jsoncompat.Clone(patch.MustGet("value"))
		if len(path) == 0 {
			if operation != "replace" {
				return nil, errors.New("ValueError: Unsupported root patch")
			}
			state = value
			continue
		}
		target := state
		for _, key := range path[:len(path)-1] {
			if list, isList := target.(*jsoncompat.Array); isList {
				index, err := strconv.Atoi(key)
				if err != nil || index < 0 || index >= len(list.Items) {
					return nil, errors.New("ValueError: bad list index " + key)
				}
				target = list.Items[index]
			} else if object := obj(target); object != nil {
				next, exists := object.Get(key)
				if !exists {
					return nil, errors.New("KeyError: '" + key + "'")
				}
				target = next
			} else {
				return nil, errors.New("TypeError: cannot walk path")
			}
		}
		last := path[len(path)-1]
		if list, isList := target.(*jsoncompat.Array); isList {
			index := len(list.Items)
			if last != "-" {
				parsed, err := strconv.Atoi(last)
				if err != nil || parsed < 0 || parsed > len(list.Items) {
					return nil, errors.New("IndexError: list index out of range")
				}
				index = parsed
			}
			switch operation {
			case "add":
				list.Items = append(list.Items, nil)
				copy(list.Items[index+1:], list.Items[index:])
				list.Items[index] = value
			case "remove":
				if index >= len(list.Items) {
					return nil, errors.New("IndexError: list index out of range")
				}
				list.Items = append(list.Items[:index], list.Items[index+1:]...)
			case "replace":
				if index >= len(list.Items) {
					return nil, errors.New("IndexError: list index out of range")
				}
				list.Items[index] = value
			default:
				return nil, errors.New("ValueError: Unsupported list patch")
			}
		} else if object := obj(target); object != nil {
			switch operation {
			case "remove":
				object.Delete(last)
			case "replace", "add":
				object.Set(last, value)
			default:
				return nil, errors.New("ValueError: Unsupported patch")
			}
		}
	}
	return state, nil
}

// pyBaseName mirrors PurePosixPath(...).name for attachment display.
func pyBaseName(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed[strings.LastIndex(trimmed, "/")+1:]
}

// NormalizeItem mirrors normalize_item: one desktop item to one row.
func NormalizeItem(item *jsoncompat.Object) *jsoncompat.Object {
	kindValue := any("unknown")
	if value, ok := item.Get("type"); ok {
		kindValue = value
	}
	kind, _ := kindValue.(string)
	row := jsoncompat.NewObject()
	row.Set("id", item.MustGet("id"))
	row.Set("kind", kindValue)
	row.Set("status", item.MustGet("status"))
	switch kind {
	case "userMessage", "steeringUserMessage":
		if kind == "steeringUserMessage" {
			merged := jsoncompat.NewObject()
			for _, key := range item.Keys() {
				value, _ := item.Get(key)
				merged.Set(key, value)
			}
			merged.Set("content", getDefault(item, "input", &jsoncompat.Array{}))
			item = merged
		}
		row.Set("requestId", jsoncompat.PyStr(RequestID(item)))
		text := UserDisplayText(TextContent(getDefault(item, "content", &jsoncompat.Array{})))
		if replies := QuestionReplies(text); len(replies) > 0 {
			parts := []string{}
			for _, reply := range replies {
				parts = append(parts, jsoncompat.PyStr(getDefault(reply, "question", ""))+"\n"+
					jsoncompat.PyStr(getDefault(reply, "answer", "")))
			}
			text = strings.Join(parts, "\n\n")
		}
		row.Set("role", "user")
		row.Set("text", text)
	case "agentMessage", "assistantMessage":
		row.Set("role", "assistant")
		row.Set("text", textutil.StripInternalReminders(jsoncompat.PyStr(getDefault(item, "text", ""))))
		row.Set("phase", item.MustGet("phase"))
	case "imageView", "ImageView":
		raw, _ := getDefault(item, "path", "").(string)
		path := raw
		if parsed, err := url.Parse(raw); err == nil && parsed.Scheme == "file" &&
			(parsed.Host == "" || parsed.Host == "localhost") {
			if filePath, err := url.PathUnescape(parsed.Path); err == nil {
				if fileDrivePattern.MatchString(filePath) {
					filePath = filePath[1:]
				}
				path = filePath
			}
		}
		row.Set("role", "activity")
		row.Set("title", "图片预览")
		row.Set("text", "")
		row.Set("phase", "commentary")
		if path != "" {
			attachment := jsoncompat.NewObject()
			attachment.Set("type", "localImage")
			attachment.Set("path", path)
			attachment.Set("name", pyBaseName(path))
			row.Set("attachments", &jsoncompat.Array{Items: []any{attachment}})
		}
	case "reasoning":
		row.Set("role", "activity")
		row.Set("title", "思考摘要")
		summary := []string{}
		for _, part := range orEmptyArray(item.MustGet("summary")).Items {
			text, _ := part.(string)
			summary = append(summary, text)
		}
		row.Set("text", strings.Join(summary, "\n"))
	case "commandExecution":
		row.Set("role", "activity")
		row.Set("title", "执行命令")
		row.Set("text", getDefault(item, "command", ""))
		row.Set("output", getDefault(item, "aggregatedOutput", ""))
		row.Set("exitCode", item.MustGet("exitCode"))
	case "fileChange":
		parts := []string{}
		for _, changeValue := range orEmptyArray(item.MustGet("changes")).Items {
			change := obj(changeValue)
			if change == nil {
				continue
			}
			parts = append(parts, jsoncompat.PyStr(getDefault(change, "path", ""))+"\n"+
				jsoncompat.PyStr(getDefault(change, "diff", "")))
		}
		row.Set("role", "activity")
		row.Set("title", "文件变更")
		row.Set("text", strings.Join(parts, "\n\n"))
	case "mcpToolCall", "dynamicToolCall":
		titles := []string{}
		if server := item.MustGet("server"); jsoncompat.Truthy(server) {
			titles = append(titles, jsoncompat.PyStr(server))
		}
		tool := getDefault(item, "tool", item.MustGet("toolName"))
		if jsoncompat.Truthy(tool) {
			titles = append(titles, jsoncompat.PyStr(tool))
		}
		row.Set("role", "activity")
		row.Set("title", strings.Join(titles, " · "))
		row.Set("text", string(jsoncompat.EncodePythonIndent2(getDefault(item, "arguments",
			&jsoncompat.Object{}))))
		output := item.MustGet("result")
		if _, exists := item.Get("result"); !exists {
			if contentItems, ok := item.Get("contentItems"); ok {
				output = contentItems
			} else if failure, ok := item.Get("error"); ok {
				output = failure
			} else {
				output = ""
			}
		}
		row.Set("output", string(jsoncompat.EncodePythonIndent2(output)))
	case "toolCall", "tool_call", "subagent":
		title := item.MustGet("title")
		if !jsoncompat.Truthy(title) {
			if kind == "subagent" {
				title = "子 Agent"
			} else {
				title = "工具调用"
			}
		}
		row.Set("role", "activity")
		row.Set("title", title)
		row.Set("text", getDefault(item, "text", ""))
		row.Set("output", getDefault(item, "output", ""))
		if kind == "subagent" || jsoncompat.Truthy(item.MustGet("subagentId")) ||
			jsoncompat.Truthy(item.MustGet("actualSubagentType")) {
			row.Set("kind", "subagent")
		}
		for _, key := range [...]string{"subagentId", "subagentType",
			"subagentDescription", "subagentPrompt", "stopReason"} {
			if value := item.MustGet(key); jsoncompat.Truthy(value) {
				row.Set(key, value)
			}
		}
	case "error":
		row.Set("role", "error")
		row.Set("text", getDefault(item, "message", "执行失败"))
	case "planImplementation":
		row.Set("role", "assistant")
		row.Set("title", "计划")
		row.Set("text", getDefault(item, "planContent", ""))
	default:
		row.Set("role", "activity")
		row.Set("title", kindValue)
		row.Set("text", string(jsoncompat.EncodePythonIndent2(item)))
	}
	// Preserve non-text attachment descriptors from content (after steering merge).
	attachments := &jsoncompat.Array{}
	if contentList := arr(item.MustGet("content")); contentList != nil {
		for _, partValue := range contentList.Items {
			part := obj(partValue)
			if part == nil {
				continue
			}
			if partType, _ := part.MustGet("type").(string); partType != "text" &&
				partType != "input_text" && partType != "output_text" {
				attachments.Items = append(attachments.Items, part)
			}
		}
	}
	if len(attachments.Items) > 0 {
		row.Set("attachments", attachments)
	}
	return row
}
