package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/google/uuid"
)

const maxOfflineBytes = 8 << 20

// Keep concurrent disk reads and projection work bounded, even across devices.
var offlineHistorySlots = make(chan struct{}, 2)

type offlineHistory struct {
	Version    int                   `json:"version"`
	DeviceID   string                `json:"deviceId"`
	Host       string                `json:"host"`
	Agent      string                `json:"agent"`
	ID         string                `json:"id"`
	Title      string                `json:"title"`
	CapturedAt int64                 `json:"capturedAt"`
	Through    string                `json:"throughTurn"`
	Rows       []timeline.OfflineRow `json:"rows"`
}

func (h *handler) handleOfflineHistory() {
	if h.request.Method != http.MethodGet {
		h.writer.Header().Set("Allow", "GET")
		h.fail(http.StatusMethodNotAllowed, errorBody("此接口只读"))
		return
	}
	session := h.sessionValue(false)
	if session == nil {
		return
	}
	trusted, _ := session.MustGet("trustedDevice").(bool)
	verifier, _ := session.MustGet("routeRenewal").(string)
	if !trusted || verifier == "" || !auth.MobileClient(h.request.UserAgent()) {
		h.fail(http.StatusForbidden, errorBody("请先扫码配对电脑"))
		return
	}
	query := h.request.URL.Query()
	id := query.Get("id")
	thread, err := uuid.Parse(id)
	if err != nil || thread.String() != id || query.Get("host") != "local" || query.Get("agent") != "codex" {
		h.fail(http.StatusBadRequest, errorBody("离线保存仅支持本机 Codex 聊天"))
		return
	}
	if h.server.Store == nil || h.server.hiddenSession(id) {
		h.fail(http.StatusNotFound, errorBody("找不到可保存的历史记录"))
		return
	}
	if _, remote := readAppHosts(h.server.Store.Home).assignments[id]; remote {
		h.fail(http.StatusBadRequest, errorBody("离线保存仅支持本机 Codex 聊天"))
		return
	}
	select {
	case offlineHistorySlots <- struct{}{}:
		defer func() { <-offlineHistorySlots }()
	default:
		h.fail(http.StatusTooManyRequests, errorBody("离线保存繁忙，请稍后重试"))
		return
	}
	ctx := h.request.Context()
	saved, through, err := h.server.Store.OfflineHistory(ctx, id)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOfflineTooLarge):
			h.fail(http.StatusRequestEntityTooLarge, errorBody(err.Error()))
		case errors.Is(err, store.ErrOfflineBoundary), errors.Is(err, store.ErrOfflineChanged):
			h.fail(http.StatusConflict, errorBody(err.Error()))
		default:
			h.fail(http.StatusNotFound, errorBody("找不到可保存的历史记录"))
		}
		return
	}
	if turns, ok := saved.MustGet("turns").(*jsoncompat.Array); ok {
		complete := turns.Items[:0]
		for _, value := range turns.Items {
			if turn, ok := value.(*jsoncompat.Object); ok && turn.MustGet("status") != "inProgress" {
				complete = append(complete, turn)
			}
		}
		turns.Items = complete
	}
	view := buildHistoryView(saved)
	projection := timeline.New(nil)
	projection.Update(view)
	rows, withinBudget := projection.OfflineRows(4000, maxOfflineBytes, 512<<10)
	if !withinBudget {
		h.fail(http.StatusRequestEntityTooLarge, errorBody(store.ErrOfflineTooLarge.Error()))
		return
	}
	if len(rows) == 0 {
		h.fail(http.StatusConflict, errorBody(store.ErrOfflineBoundary.Error()))
		return
	}
	title, _ := view.MustGet("title").(string)
	titleRunes := []rune(title)
	result := offlineHistory{
		Version: 1, DeviceID: h.server.Auth.DeviceID(), Host: "local", Agent: "codex", ID: id,
		Title: string(titleRunes[:min(len(titleRunes), 256)]), CapturedAt: time.Now().Unix(), Through: through, Rows: rows,
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) > maxOfflineBytes {
		h.fail(http.StatusRequestEntityTooLarge, errorBody(store.ErrOfflineTooLarge.Error()))
		return
	}
	if ctx.Err() != nil || h.sessionValue(false) == nil {
		return
	}
	if h.server.hiddenSession(id) {
		h.fail(http.StatusNotFound, errorBody("找不到可保存的历史记录"))
		return
	}
	digest := sha256.Sum256(body)
	h.writer.Header().Set("X-Bridge-Offline-SHA256", hex.EncodeToString(digest[:]))
	h.output(http.StatusOK, body, contentTypeJSON, "")
}
