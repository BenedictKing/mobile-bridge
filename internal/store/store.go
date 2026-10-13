// Package store is the Go port of bridge/store.py's read-only session store:
// SQLite discovery, the backward chunked reader (_recent_lines), and the
// JSONL → turns/items parser (history). Parity with Python is enforced against
// conformance/store goldens and fixture tests.
package store

import (
	"bytes"
	"encoding/json"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func obj(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

func getDefault(object *jsoncompat.Object, key string, fallback any) any {
	if value, ok := object.Get(key); ok {
		return value
	}
	return fallback
}

// splitLinesKeepEnds splits on \n / \r\n / \r keeping terminators, matching
// bytes.splitlines(keepends=True) for the terminators rollout files use.
func splitLinesKeepEnds(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' || data[i] == '\r' {
			end := i + 1
			if data[i] == '\r' && end < len(data) && data[end] == '\n' {
				end++
				i++
			}
			lines = append(lines, data[start:end])
			start = end
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// RecentLines mirrors SessionStore._recent_lines: read complete turns
// backwards in 64 KiB chunks without parsing the older rollout prefix. A nil
// turnLimit returns everything.
func RecentLines(data []byte, turnLimit *int) ([]byte, bool) {
	if turnLimit == nil {
		return data, true
	}
	position := len(data)
	var parts [][]byte
	pending := []byte{}
	starts := 0
	for position > 0 {
		size := min(position, 65536)
		position -= size
		chunk := append(append([]byte{}, data[position:position+size]...), pending...)
		lines := splitLinesKeepEnds(chunk)
		if position > 0 {
			pending = lines[0]
			lines = lines[1:]
		} else {
			pending = nil
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			parts = append(parts, line)
			if !bytes.Contains(line, []byte(`"task_started"`)) {
				continue
			}
			value, err := jsoncompat.Unmarshal(bytes.TrimRight(line, "\r\n"))
			if err != nil {
				continue
			}
			record := obj(value)
			if record == nil {
				continue
			}
			payload := obj(getDefault(record, "payload", &jsoncompat.Object{}))
			recordType, _ := record.MustGet("type").(string)
			payloadType := ""
			if payload != nil {
				payloadType, _ = payload.MustGet("type").(string)
			}
			if recordType == "event_msg" && payloadType == "task_started" {
				starts++
				if starts >= *turnLimit {
					return joinReversed(parts), position == 0 && len(parts) == len(lines)
				}
			}
		}
	}
	return joinReversed(parts), true
}

func joinReversed(parts [][]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	joined := make([]byte, 0, total)
	for i := len(parts) - 1; i >= 0; i-- {
		joined = append(joined, parts[i]...)
	}
	return joined
}

// History mirrors SessionStore.history's parsing: rollout lines to saved
// turns/items plus the result header. meta carries the thread row.
func History(threadID string, meta *jsoncompat.Object, raw []byte, turnLimit *int) *jsoncompat.Object {
	data, complete := RecentLines(raw, turnLimit)
	turns := &jsoncompat.Array{}
	var current *jsoncompat.Object
	var items *jsoncompat.Array
	historyTurn := func() {
		current = jsoncompat.NewObject()
		current.Set("turnId", "history")
		current.Set("status", "completed")
		items = &jsoncompat.Array{}
		current.Set("items", items)
		turns.Items = append(turns.Items, current)
	}
	for _, line := range splitLinesKeepEnds(data) {
		value, err := jsoncompat.Unmarshal(bytes.TrimRight(line, "\r\n"))
		if err != nil {
			continue
		}
		record := obj(value)
		if record == nil {
			continue
		}
		recordType, _ := record.MustGet("type").(string)
		payload := obj(getDefault(record, "payload", &jsoncompat.Object{}))
		payloadType := ""
		if payload != nil {
			payloadType, _ = payload.MustGet("type").(string)
		}
		switch {
		case recordType == "event_msg" && payloadType == "task_started":
			current = jsoncompat.NewObject()
			current.Set("turnId", payload.MustGet("turn_id"))
			current.Set("status", "inProgress")
			items = &jsoncompat.Array{}
			current.Set("items", items)
			turns.Items = append(turns.Items, current)
		case recordType == "event_msg" && (payloadType == "task_complete" || payloadType == "turn_aborted"):
			if current != nil {
				if payloadType == "task_complete" {
					current.Set("status", "completed")
				} else {
					current.Set("status", "interrupted")
				}
			}
		case recordType == "event_msg" && payloadType == "item_completed":
			item := obj(getDefault(payload, "item", &jsoncompat.Object{}))
			if item == nil {
				continue
			}
			if kind, _ := item.MustGet("type").(string); kind != "ImageView" && kind != "imageView" {
				continue
			}
			if current == nil {
				historyTurn()
			}
			id := any(strconv.Itoa(len(items.Items)))
			if value, ok := item.Get("id"); ok {
				id = value
			}
			row := jsoncompat.NewObject()
			row.Set("id", id)
			row.Set("type", "ImageView")
			row.Set("path", getDefault(item, "path", ""))
			items.Items = append(items.Items, row)
		case recordType == "response_item":
			if current == nil {
				historyTurn()
			}
			role, _ := payload.MustGet("role").(string)
			switch {
			case payloadType == "message" && (role == "user" || role == "assistant"):
				row := jsoncompat.NewObject()
				row.Set("id", strconv.Itoa(len(items.Items)))
				if role == "user" {
					row.Set("type", "userMessage")
					row.Set("content", getDefault(payload, "content", &jsoncompat.Array{}))
				} else {
					texts := []string{}
					for _, partValue := range orDefault(payload, "content").Items {
						if part := obj(partValue); part != nil {
							if text, ok := part.Get("text"); ok {
								texts = append(texts, jsoncompat.PyStr(text))
							} else {
								texts = append(texts, "")
							}
						}
					}
					row.Set("type", "agentMessage")
					row.Set("text", joinStrings(texts))
					row.Set("phase", payload.MustGet("phase"))
				}
				items.Items = append(items.Items, row)
			case payloadType == "agent_message" || payloadType == "agentMessage":
				row := jsoncompat.NewObject()
				id := any(strconv.Itoa(len(items.Items)))
				if value, ok := payload.Get("id"); ok {
					id = value
				}
				row.Set("id", id)
				row.Set("type", "agentMessage")
				row.Set("text", getDefault(payload, "text", ""))
				items.Items = append(items.Items, row)
			case payloadType == "function_call" || payloadType == "custom_tool_call" ||
				payloadType == "function_call_output" || payloadType == "custom_tool_call_output":
				row := jsoncompat.NewObject()
				id := any(strconv.Itoa(len(items.Items)))
				if value, ok := payload.Get("call_id"); ok {
					id = value
				}
				row.Set("id", id)
				row.Set("type", "storedToolEvent")
				// Python spreads **payload afterwards, replacing matching
				// keys in place and appending the rest in payload order.
				for _, key := range payload.Keys() {
					value, _ := payload.Get(key)
					row.Set(key, value)
				}
				items.Items = append(items.Items, row)
			}
		}
	}
	result := jsoncompat.NewObject()
	result.Set("id", threadID)
	title := meta.MustGet("name")
	if !jsoncompat.Truthy(title) {
		title = meta.MustGet("title")
	}
	result.Set("title", title)
	result.Set("cwd", meta.MustGet("cwd"))
	result.Set("latestModel", meta.MustGet("model"))
	result.Set("modelProvider", meta.MustGet("model_provider"))
	result.Set("turns", turns)
	pagination := jsoncompat.NewObject()
	pagination.Set("hasLoadedOldest", complete)
	result.Set("turnsPagination", pagination)
	result.Set("requests", &jsoncompat.Array{})
	status := jsoncompat.NewObject()
	status.Set("type", "notLoaded")
	result.Set("threadRuntimeStatus", status)
	return result
}

func orDefault(container *jsoncompat.Object, key string) *jsoncompat.Array {
	if list, ok := container.MustGet(key).(*jsoncompat.Array); ok {
		return list
	}
	return &jsoncompat.Array{}
}

func joinStrings(parts []string) string {
	var buffer bytes.Buffer
	for i, part := range parts {
		if i > 0 {
			buffer.WriteByte('\n')
		}
		buffer.WriteString(part)
	}
	return buffer.String()
}

var _ = json.Marshal
