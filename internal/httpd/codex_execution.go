package httpd

import (
	"errors"
	"net/http"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

func (s *Server) codexExecutionTarget(id string) string {
	if s.Codex != nil && s.Codex.IsCLI(id) {
		return "cli"
	}
	return s.Creations.executionForTarget(id)
}

func (s *Server) annotateCodexExecution(id string, view *jsoncompat.Object) {
	view.Set("executionTarget", s.codexExecutionTarget(id))
	view.Set("executionSwitchSupported", codex.CLISupported())
	owner := ""
	if s.Codex != nil {
		owner = s.Codex.ExecutionOwner(id)
	}
	view.Set("executionOwner", owner)
}

func defaultCodexExecution() string {
	if codex.CLISupported() {
		return "cli"
	}
	return "desktop"
}

func codexHandoffBusy(state *jsoncompat.Object) bool {
	if state == nil {
		return false
	}
	view := model.NormalizeState(state, false)
	return view.MustGet("status") == "active" || len(model.PendingRequests(state).Items) != 0
}

func (s *Server) switchCodexExecution(id, target string) error {
	if s.Codex == nil || s.Store == nil || s.Creations == nil {
		return errors.New("当前网关不支持 Codex 执行交接")
	}
	if s.hiddenSession(id) {
		return errors.New("此聊天已删除")
	}
	limit := 1
	history, err := s.Store.HistoryFor(id, &limit)
	if err != nil {
		return err
	}
	cwd := stringField(history, "cwd")
	if cwd == "" {
		return errors.New("找不到此聊天的工作目录，未交接")
	}
	live, _, _, _ := s.Codex.Snapshot(id)
	if codexHandoffBusy(history) || codexHandoffBusy(live) {
		return errors.New("会话仍在执行或等待审批，暂不能交接")
	}
	if s.Queue != nil {
		for _, raw := range s.Queue.meta(id).Items {
			if row, ok := raw.(*jsoncompat.Object); ok && row.MustGet("status") != SubFailed {
				return errors.New("聊天还有待发送或结果待确认的消息，请处理后再交接")
			}
		}
	}
	if s.Creations.migrationForTarget(id) != nil {
		return errors.New("聊天还有未完成或结果待确认的迁移，请处理后再交接")
	}
	persist := func() error { return s.Creations.setTargetExecution(id, cwd, target) }
	if target == "cli" {
		if s.Codex.IsCLI(id) {
			_, err = s.watchCodexForWrite(id)
			return err
		}
		return s.Codex.ClaimCLI(id, cwd, persist)
	}
	if s.Codex.IsCLI(id) {
		if err := s.Codex.HandOffCLI(id, cwd, persist); err != nil {
			return err
		}
	} else if err := persist(); err != nil {
		return err
	}
	if err := codex.OpenInDesktop(id); err != nil {
		return errors.New("执行归属已切换到桌面，打开 App 失败；请手动打开此聊天后重新连接")
	}
	return nil
}

func (h *handler) handleCodexExecution(id string, write bool) {
	if write {
		raw, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		target := stringField(raw.(*jsoncompat.Object), "target")
		if target != "cli" && target != "desktop" {
			h.fail(http.StatusBadRequest, errorBody("请选择桌面 App 或网关 CLI 执行"))
			return
		}
		if err := h.server.switchCodexExecution(id, target); err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
	}
	result := jsoncompat.NewObject()
	h.server.annotateCodexExecution(id, result)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}
