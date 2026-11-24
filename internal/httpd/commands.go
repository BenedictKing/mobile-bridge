package httpd

import (
	"context"
	"errors"
	"net/http"

	"github.com/BenedictKing/mobile-bridge/internal/command"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// Existing aliases of an attached ACP chat must serialize on its live handle.
func (s *Server) commandThreadKey(threadID string) string {
	// Lock identity must never cold-probe or attach a provider. Before attach,
	// the route id is the identity; existing aliases share their cached handle.
	for _, manager := range s.acpAgentSet() {
		if manager != nil {
			if session := manager.Peek(threadID); session != nil {
				return "thread:" + session.ID
			}
		}
	}
	return "thread:" + threadID
}

func (s *Server) lockThreadCommand(ctx context.Context, threadID string) (func(), error) {
	return s.threadCommands.Lock(ctx, s.commandThreadKey(threadID))
}

func isThreadCommand(action string) bool {
	switch action {
	case "send", "stop", "respond", "input", "settings", "queue", "reconnect", "execution", "message-action", "rename", "permissions", "goal", "delete":
		return true
	}
	return false
}

func (h *handler) runStop(threadID string, body *jsoncompat.Object, activeTurnID string, stop func(string) (any, error)) {
	id := stringField(body, "commandId")
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			h.fail(http.StatusBadRequest, errorBody("命令标识无效"))
			return
		}
	}
	expected := stringField(body, "expectedTurnId")
	payload := map[string]any{"expectedTurnId": expected}
	scope := "stop:" + h.server.commandThreadKey(threadID)
	if raw, err, found := h.server.Commands.Replay(id, scope, payload); found {
		h.outputControlResult(raw, err)
		return
	}
	if expected == "" {
		expected = activeTurnID
	}
	if id == "" {
		id = uuid.NewString()
	}
	raw, err := h.server.Commands.Run(command.Intent{ID: id, Scope: scope, Target: scope + ":" + expected, Payload: payload}, func() (any, error) {
		if expected == "" {
			return nil, command.Rejected(errors.New("当前没有可停止的任务"))
		}
		if expected != activeTurnID {
			return nil, command.Rejected(errors.New("任务已切换，未停止新的任务；请刷新聊天"))
		}
		return stop(expected)
	})
	h.outputControlResult(raw, err)
}

func (h *handler) queueBlockedDirect(threadID string, payload submissionPayload) bool {
	if h.server.Queue == nil {
		return false
	}
	resolution := h.server.resolveSubmissionThread(threadID)
	if payload.mode == "steer" {
		var view *jsoncompat.Object
		if resolution.session != nil {
			view = resolution.session.Snapshot()
		} else if h.server.Codex != nil {
			view, _, _, _ = h.server.Codex.Snapshot(threadID)
		}
		if h.server.Commands.HasTarget("stop:" + h.server.commandThreadKey(threadID) + ":" + activeViewTurn(view)) {
			h.fail(http.StatusConflict, errorBody("当前任务已收到停止请求，请使用排队发送"))
			return true
		}
		return false
	}
	if payload.mode != "send" {
		return false
	}
	blocked := h.server.threadActive(threadID, resolution)
	beforeSelf := true
	for _, entry := range h.server.Queue.threadEntries(threadID) {
		if entry.ID == payload.id {
			beforeSelf = false
			continue
		}
		if entry.Status == SubUnknown || (beforeSelf && entry.Status == SubQueued) {
			blocked = true
		}
	}
	if !blocked {
		return false
	}
	if err := h.server.Queue.setStatus(threadID, payload.id, SubQueued); err != nil {
		h.fail(http.StatusInternalServerError, errorBody(err.Error()))
		return true
	}
	result := jsoncompat.NewObject()
	result.Set("id", payload.id)
	result.Set("status", SubQueued)
	h.output(http.StatusOK, result, contentTypeJSON, "")
	return true
}

func (h *handler) acceptDirectSubmission(threadID, id string) bool {
	if h.server.Queue == nil {
		return true
	}
	if err := h.server.Queue.setStatus(threadID, id, SubAccepted); err != nil {
		h.fail(http.StatusInternalServerError, errorBody("消息已发送，但接受状态落账失败，结果按未知处理: "+err.Error()))
		return false
	}
	return true
}

func (h *handler) outputControlResult(raw []byte, err error) {
	if err != nil {
		h.failControl(err)
		return
	}
	result, decodeErr := jsoncompat.Unmarshal(raw)
	if decodeErr != nil {
		h.fail(http.StatusInternalServerError, errorBody(decodeErr.Error()))
		return
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) failControl(err error) {
	body := errorBody(err.Error())
	status := http.StatusConflict
	switch {
	case errors.Is(err, command.ErrUnknown):
		body.Set("code", "outcome_unknown")
	case errors.Is(err, command.ErrStore):
		body.Set("code", "command_store_unavailable")
		status = http.StatusServiceUnavailable
	case command.IsRejected(err), errors.Is(err, command.ErrConflict):
		body.Set("code", "command_rejected")
	}
	h.fail(status, body)
}

func activeViewTurn(view *jsoncompat.Object) string {
	if view == nil {
		return ""
	}
	turns, _ := view.MustGet("turns").(*jsoncompat.Array)
	if turns == nil {
		return ""
	}
	for i := len(turns.Items) - 1; i >= 0; i-- {
		turn, _ := turns.Items[i].(*jsoncompat.Object)
		if turn != nil && turn.MustGet("status") == "inProgress" {
			if id := stringField(turn, "id"); id != "" {
				return id
			}
			return stringField(turn, "turnId")
		}
	}
	return ""
}
