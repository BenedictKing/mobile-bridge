package httpd

import (
	"net/http"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// handlePool serves the ACP account-pool routes:
//
//	GET  /api/pool                 overview of every supported agent
//	GET  /api/pool/{kind}          one agent's accounts (redacted) + activeId
//	POST /api/pool/{kind}/refresh  refresh quotas for every pooled account
//	POST /api/pool/{kind}/import   import zcode-switcher snapshots + capture
//	                               the current official slot
func (h *handler) handlePool(write bool) {
	pool := h.server.Pool
	if pool == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用账号池"))
		return
	}
	rest := strings.Trim(strings.TrimPrefix(h.request.URL.Path, "/api/pool"), "/")
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		if write {
			h.fail(http.StatusNotFound, errorBody("未知的账号池操作"))
			return
		}
		h.output(http.StatusOK, pool.Overview(), contentTypeJSON, "")
		return
	}
	kind := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if !write {
		if action != "" {
			h.fail(http.StatusNotFound, errorBody("未知的账号池操作"))
			return
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		h.output(http.StatusOK, detail, contentTypeJSON, "")
		return
	}
	// Body is optional for both write actions; an empty body is an empty
	// object (readJSON itself rejects empty payloads).
	body := jsoncompat.NewObject()
	if h.request.Body != nil && h.request.ContentLength != 0 {
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		body = value.(*jsoncompat.Object)
	}
	switch action {
	case "refresh":
		ctx := h.request.Context()
		failures := pool.RefreshAllQuotas(ctx, kind)
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		if len(failures) > 0 {
			errs := jsoncompat.NewObject()
			for id, failure := range failures {
				errs.Set(id, failure.Error())
			}
			detail.Set("refreshErrors", errs)
		}
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	case "import":
		source, _ := body.MustGet("source").(string)
		imported, skipped, err := pool.Import(kind, source)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		captured := false
		captureErr := ""
		if _, _, capErr := pool.CaptureCurrent(h.request.Context(), kind); capErr == nil {
			captured = true
		} else {
			captureErr = capErr.Error()
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		importedList := &jsoncompat.Array{}
		for _, id := range imported {
			importedList.Items = append(importedList.Items, id)
		}
		skippedList := &jsoncompat.Array{}
		for _, row := range skipped {
			entry := jsoncompat.NewObject()
			entry.Set("id", row.ID)
			entry.Set("reason", row.Reason)
			skippedList.Items = append(skippedList.Items, entry)
		}
		detail.Set("imported", importedList)
		detail.Set("skipped", skippedList)
		detail.Set("captured", captured)
		if captureErr != "" {
			detail.Set("captureError", captureErr)
		}
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	default:
		h.fail(http.StatusNotFound, errorBody("未知的账号池操作"))
	}
}
