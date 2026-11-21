package codex

import (
	"errors"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

func (b *Bridge) callCLI(id, method string, params *jsoncompat.Object, timeout time.Duration) (*jsoncompat.Object, error) {
	b.mu.Lock()
	c, cwd, closing := b.cli, b.cliThreads[id], b.closing
	b.mu.Unlock()
	if closing {
		return nil, errors.New("网关正在关闭")
	}
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	if method == "thread-follower-load-complete-history" {
		return c.readThread(id, cwd, timeout)
	}
	defer c.touch(id)
	if _, err := b.watchCLILocked(id, c, cwd); err != nil {
		return nil, err
	}
	request := object(map[string]any{"threadId": id})
	operation := ""
	switch method {
	case "thread-follower-start-turn":
		start, _ := params.MustGet("turnStart").(*jsoncompat.Object)
		if start == nil {
			return nil, errors.New("缺少 Codex CLI 回合参数")
		}
		input, _ := start.MustGet("request").(*jsoncompat.Object)
		if input == nil {
			return nil, errors.New("缺少 Codex CLI 输入")
		}
		request, _ = jsoncompat.Clone(input).(*jsoncompat.Object)
		if context, ok := start.MustGet("context").(*jsoncompat.Object); ok {
			if files := cliArray(context.MustGet("fileAttachments")); files != nil && len(files.Items) > 0 {
				lines := []string{"# Files mentioned by the user:"}
				for _, raw := range files.Items {
					if file, ok := raw.(*jsoncompat.Object); ok {
						lines = append(lines, "- "+cliText(file.MustGet("path")))
					}
				}
				inputs := cliArray(request.MustGet("input"))
				inputs.Items = append([]any{object(map[string]any{"type": "text", "text": strings.Join(lines, "\n")})}, inputs.Items...)
			}
		}
		operation = "turn/start"
	case "thread-follower-steer-turn":
		request.Set("input", params.MustGet("input"))
		request.Set("clientUserMessageId", params.MustGet("clientUserMessageId"))
		request.Set("expectedTurnId", b.activeCLITurn(id))
		operation = "turn/steer"
	case "thread-follower-interrupt-turn":
		request.Set("turnId", b.activeCLITurn(id))
		operation = "turn/interrupt"
	case "thread-follower-update-thread-settings":
		settings, _ := params.MustGet("threadSettings").(*jsoncompat.Object)
		if settings == nil {
			return nil, errors.New("缺少 Codex CLI 模型设置")
		}
		for _, key := range []string{"model", "effort", "serviceTier"} {
			if value, ok := settings.Get(key); ok {
				if key == "effort" && value == "" {
					continue
				}
				request.Set(key, value)
			}
		}
		result, err := c.request("thread/settings/update", request, timeout)
		if err == nil {
			b.mu.Lock()
			session := b.sessions[id]
			state := session.State.(*jsoncompat.Object)
			for source, target := range map[string]string{"model": "latestModel", "effort": "latestReasoningEffort"} {
				if value, ok := settings.Get(source); ok {
					if source == "effort" && value == "" {
						continue
					}
					state.Set(target, value)
				}
			}
			state.Set("latestThreadSettings", jsoncompat.Clone(settings))
			c.changedLocked(session)
			b.mu.Unlock()
		}
		return result, err
	default:
		return nil, errors.New("当前 Codex CLI 接管暂不支持此桌面操作")
	}
	result, err := c.request(operation, request, timeout)
	if err == nil && operation == "turn/start" {
		if turn, ok := result.MustGet("turn").(*jsoncompat.Object); ok {
			b.mu.Lock()
			session := b.sessions[id]
			state := session.State.(*jsoncompat.Object)
			current := mergeCLITurn(state, turn)
			current.Set("params", jsoncompat.Clone(request))
			if current.MustGet("status") == "inProgress" {
				state.Set("threadRuntimeStatus", object(map[string]any{"type": "active"}))
			}
			c.changedLocked(session)
			b.mu.Unlock()
		}
	}
	return result, err
}

func (b *Bridge) activeCLITurn(id string) string {
	state, _, _, _ := b.Snapshot(id)
	if state != nil {
		turns := model.OrderedTurns(state)
		for index := len(turns.Items) - 1; index >= 0; index-- {
			turn, _ := turns.Items[index].(*jsoncompat.Object)
			if turn != nil && turn.MustGet("status") == "inProgress" {
				return cliText(turn.MustGet("turnId"))
			}
		}
	}
	return ""
}

func (b *Bridge) respondCLI(id, requestID string, response *jsoncompat.Object) (*jsoncompat.Object, error) {
	c := b.cli
	c.attachMu.Lock()
	defer c.attachMu.Unlock()
	defer c.touch(id)
	key := id + "\x00" + requestID
	c.mu.Lock()
	pending, process := c.pending[key], c.process
	if pending == nil || process == nil {
		c.mu.Unlock()
		return nil, errors.New("请求已过期，请刷新聊天")
	}
	if model.NormalizeRequest(pending).MustGet("supported") != true {
		c.mu.Unlock()
		return nil, errors.New("当前 Codex CLI 请求需要在原生客户端处理")
	}
	delete(c.pending, key) // A transport failure is not permission to replay a reply.
	c.mu.Unlock()
	err := process.Respond(pending.MustGet("id"), response)
	b.mu.Lock()
	session := b.sessions[id]
	if state, ok := session.State.(*jsoncompat.Object); ok {
		requests := cliArray(state.MustGet("requests"))
		kept := &jsoncompat.Array{}
		if requests != nil {
			for _, raw := range requests.Items {
				entry, _ := raw.(*jsoncompat.Object)
				if entry != nil && jsoncompat.PyStr(entry.MustGet("id")) != requestID {
					kept.Items = append(kept.Items, raw)
				}
			}
		}
		state.Set("requests", kept)
		c.changedLocked(session)
	}
	b.mu.Unlock()
	if err != nil {
		return nil, errors.New("Codex CLI 回应结果待确认，未自动重发: " + err.Error())
	}
	return object(map[string]any{"ok": true}), nil
}
