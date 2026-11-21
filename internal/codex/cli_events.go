package codex

import (
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func cliArray(value any) *jsoncompat.Array { list, _ := value.(*jsoncompat.Array); return list }

func cliTurn(turn *jsoncompat.Object) *jsoncompat.Object {
	copy, _ := jsoncompat.Clone(turn).(*jsoncompat.Object)
	copy.Set("turnId", turn.MustGet("id"))
	return copy
}

func (c *cliRuntime) applyThread(thread, settings *jsoncompat.Object, connected bool) {
	id := jsoncompat.PyStr(thread.MustGet("id"))
	session := c.bridge.session(id)
	c.bridge.mu.Lock()
	defer c.bridge.mu.Unlock()
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		state = jsoncompat.NewObject()
		session.State = state
	}
	for _, key := range []string{"id", "cwd", "modelProvider"} {
		state.Set(key, thread.MustGet(key))
	}
	title := thread.MustGet("name")
	if title == nil || title == "" {
		title = thread.MustGet("preview")
	}
	if title != nil && title != "" {
		state.Set("title", title)
	}
	state.Set("threadRuntimeStatus", thread.MustGet("status"))
	if turns := cliArray(thread.MustGet("turns")); turns != nil {
		for _, raw := range turns.Items {
			if turn, ok := raw.(*jsoncompat.Object); ok {
				mergeCLITurn(state, turn)
			}
		}
	}
	for source, target := range map[string]string{"model": "latestModel", "reasoningEffort": "latestReasoningEffort", "collaborationMode": "latestCollaborationMode"} {
		value := thread.MustGet(source)
		if settings != nil && settings.MustGet(source) != nil {
			value = settings.MustGet(source)
		}
		if value != nil {
			state.Set(target, value)
		}
	}
	session.Owner, session.Connected, session.Error = "", connected, ""
	if connected {
		session.Owner = "cli"
		state.Set("writerReleased", false)
	}
	c.changedLocked(session)
}

func mergeCLITurn(state, incoming *jsoncompat.Object) *jsoncompat.Object {
	turns := cliArray(state.MustGet("turns"))
	if turns == nil {
		turns = &jsoncompat.Array{}
		state.Set("turns", turns)
	}
	id := incoming.MustGet("id")
	for _, raw := range turns.Items {
		turn, _ := raw.(*jsoncompat.Object)
		if turn != nil && turn.MustGet("turnId") == id {
			for _, key := range incoming.Keys() {
				if key == "status" && incoming.MustGet(key) == "inProgress" && turn.MustGet("status") != nil && turn.MustGet("status") != "inProgress" {
					continue
				}
				if key != "items" {
					turn.Set(key, incoming.MustGet(key))
				}
			}
			if items := cliArray(incoming.MustGet("items")); items != nil {
				for _, raw := range items.Items {
					if item, ok := raw.(*jsoncompat.Object); ok {
						mergeCLIItem(turn, item)
					}
				}
			}
			return turn
		}
	}
	turn := cliTurn(incoming)
	if cliArray(turn.MustGet("items")) == nil {
		turn.Set("items", &jsoncompat.Array{})
	}
	turns.Items = append(turns.Items, turn)
	return turn
}

func mergeCLIItem(turn, incoming *jsoncompat.Object) *jsoncompat.Object {
	items := cliArray(turn.MustGet("items"))
	if items == nil {
		items = &jsoncompat.Array{}
		turn.Set("items", items)
	}
	for _, raw := range items.Items {
		item, _ := raw.(*jsoncompat.Object)
		if item != nil && item.MustGet("id") == incoming.MustGet("id") {
			for _, key := range incoming.Keys() {
				item.Set(key, incoming.MustGet(key))
			}
			return item
		}
	}
	item, _ := jsoncompat.Clone(incoming).(*jsoncompat.Object)
	items.Items = append(items.Items, item)
	return item
}

func (c *cliRuntime) onEvent(message *jsoncompat.Object) {
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		return
	}
	method := jsoncompat.PyStr(message.MustGet("method"))
	if method == "thread/started" {
		// Resume responses are identity-checked before publishing their snapshot.
		return
	}
	id := jsoncompat.PyStr(params.MustGet("threadId"))
	if !c.bridge.IsCLI(id) {
		return
	}
	session := c.bridge.session(id)
	c.bridge.mu.Lock()
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		state = object(map[string]any{"id": id})
		session.State = state
	}
	switch method {
	case "thread/closed":
		state.Set("threadRuntimeStatus", object(map[string]any{"type": "notLoaded"}))
		c.detachedLocked(session)
	case "thread/status/changed":
		state.Set("threadRuntimeStatus", params.MustGet("status"))
		if cliStatus(params.MustGet("status")) == "notLoaded" {
			c.detachedLocked(session)
		}
	case "serverRequest/resolved":
		requestID := jsoncompat.PyStr(params.MustGet("requestId"))
		c.mu.Lock()
		delete(c.pending, id+"\x00"+requestID)
		c.mu.Unlock()
		if requests := cliArray(state.MustGet("requests")); requests != nil {
			remaining := &jsoncompat.Array{}
			for _, raw := range requests.Items {
				request, _ := raw.(*jsoncompat.Object)
				if request == nil || jsoncompat.PyStr(request.MustGet("id")) != requestID {
					remaining.Items = append(remaining.Items, raw)
				}
			}
			state.Set("requests", remaining)
		}
	case "thread/tokenUsage/updated":
		state.Set("latestTokenUsageInfo", params.MustGet("tokenUsage"))
	case "turn/started", "turn/completed":
		if turn, ok := params.MustGet("turn").(*jsoncompat.Object); ok {
			mergeCLITurn(state, turn)
		}
		status := "idle"
		if method == "turn/started" {
			status = "active"
		}
		state.Set("threadRuntimeStatus", object(map[string]any{"type": status}))
	case "item/started", "item/completed":
		if item, ok := params.MustGet("item").(*jsoncompat.Object); ok {
			turn := mergeCLITurn(state, object(map[string]any{"id": params.MustGet("turnId")}))
			mergeCLIItem(turn, item)
		}
	default:
		if method != "item/agentMessage/delta" && method != "item/plan/delta" && method != "item/commandExecution/outputDelta" && method != "item/reasoning/summaryTextDelta" && method != "item/reasoning/textDelta" {
			c.bridge.mu.Unlock()
			return
		}
		turn := mergeCLITurn(state, object(map[string]any{"id": params.MustGet("turnId")}))
		item := mergeCLIItem(turn, object(map[string]any{"id": params.MustGet("itemId")}))
		delta := jsoncompat.PyStr(params.MustGet("delta"))
		switch method {
		case "item/agentMessage/delta":
			item.Set("type", "agentMessage")
			item.Set("text", cliText(item.MustGet("text"))+delta)
		case "item/plan/delta":
			item.Set("type", "plan")
			item.Set("text", cliText(item.MustGet("text"))+delta)
		case "item/commandExecution/outputDelta":
			item.Set("aggregatedOutput", cliText(item.MustGet("aggregatedOutput"))+delta)
		case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
			item.Set("type", "reasoning")
			key := "summary"
			if method == "item/reasoning/textDelta" {
				key = "content"
			}
			parts := cliArray(item.MustGet(key))
			if parts == nil {
				parts = &jsoncompat.Array{Items: []any{""}}
				item.Set(key, parts)
			}
			if len(parts.Items) == 0 {
				parts.Items = append(parts.Items, "")
			}
			parts.Items[len(parts.Items)-1] = cliText(parts.Items[len(parts.Items)-1]) + delta
		}
	}
	if _, subscribed := c.lastUsed[id]; subscribed {
		c.lastUsed[id] = time.Now()
	}
	c.changedLocked(session)
	c.bridge.mu.Unlock()
}

func cliText(value any) string { text, _ := value.(string); return text }

func (c *cliRuntime) onRequest(message *jsoncompat.Object) {
	params, _ := message.MustGet("params").(*jsoncompat.Object)
	if params == nil {
		c.rejectRequest(message)
		return
	}
	id := cliText(params.MustGet("threadId"))
	if !c.bridge.IsCLI(id) {
		c.rejectRequest(message)
		return
	}
	requestID := jsoncompat.PyStr(message.MustGet("id"))
	c.mu.Lock()
	c.pending[id+"\x00"+requestID] = message
	c.mu.Unlock()
	session := c.bridge.session(id)
	c.bridge.mu.Lock()
	state, _ := session.State.(*jsoncompat.Object)
	if state == nil {
		state = object(map[string]any{"id": id})
		session.State = state
	}
	requests := cliArray(state.MustGet("requests"))
	if requests == nil {
		requests = &jsoncompat.Array{}
		state.Set("requests", requests)
	}
	requests.Items = append(requests.Items, jsoncompat.Clone(message))
	if _, subscribed := c.lastUsed[id]; subscribed {
		c.lastUsed[id] = time.Now()
	}
	c.changedLocked(session)
	c.bridge.mu.Unlock()
	go c.bridge.autoApprovePending(id)
}

func (c *cliRuntime) rejectRequest(message *jsoncompat.Object) {
	c.mu.Lock()
	process := c.process
	c.mu.Unlock()
	if process != nil {
		_ = process.RespondError(message.MustGet("id"), -32601, "此请求不属于已接入的 Codex CLI 会话")
	}
}
