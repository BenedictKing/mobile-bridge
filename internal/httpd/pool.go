package httpd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// poolJSONValue converts a plain struct (LoginSession, LoginChallenge) into
// an ordered jsoncompat tree by round-tripping through JSON.
func poolJSONValue(value any) any {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	parsed, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return nil
	}
	return parsed
}

// handlePool serves the ACP account-pool routes:
//
//	GET  /api/pool                 overview of every supported agent (+ a
//	                               read-only Codex row when its account
//	                               manager is enabled)
//	GET  /api/pool/{kind}          one agent's accounts (redacted) + activeId
//	POST /api/pool/{kind}/refresh  refresh quotas for every pooled account
//	POST /api/pool/{kind}/import   import zcode-switcher snapshots + capture
//	                               the current official slot
//	POST /api/pool/{kind}/capture  capture the current official slot into the
//	                               pool (upsert: import new / update existing)
//	POST /api/pool/{kind}/remove   delete one pooled snapshot ({id, confirmed})
//	POST /api/pool/{kind}/login    start an in-gateway login ({provider?,
//	                               reauthId?}) → {login, challenge}
//	GET  /api/pool/{kind}/login    current/last login session status
//	POST /api/pool/{kind}/login/cancel  cancel the in-flight login
//
// kind "codex" is a read-only view over the existing Codex account manager
// (its heavy switch state machine stays the only Codex switching path).
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
		overview := pool.Overview()
		if row := h.codexOverviewRow(); row != nil {
			agents, _ := overview.MustGet("agents").(*jsoncompat.Array)
			agents.Items = append(agents.Items, row)
		}
		h.output(http.StatusOK, overview, contentTypeJSON, "")
		return
	}
	kind := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if kind == "codex" {
		h.handlePoolCodex(write, action)
		return
	}
	if !write {
		if action == "login" {
			session, err := pool.LoginStatus(kind)
			if err != nil {
				h.fail(http.StatusNotFound, errorBody(err.Error()))
				return
			}
			out := jsoncompat.NewObject()
			out.Set("login", poolJSONValue(session))
			h.output(http.StatusOK, out, contentTypeJSON, "")
			return
		}
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
	case "login":
		if len(parts) > 2 && parts[2] == "cancel" {
			session, err := pool.CancelLogin(kind)
			if err != nil {
				h.fail(http.StatusNotFound, errorBody(err.Error()))
				return
			}
			out := jsoncompat.NewObject()
			out.Set("login", poolJSONValue(session))
			h.output(http.StatusOK, out, contentTypeJSON, "")
			return
		}
		provider, _ := body.MustGet("provider").(string)
		reauthID, _ := body.MustGet("reauthId").(string)
		session, challenge, err := pool.StartLogin(h.request.Context(), kind, accpool.LoginOptions{
			Provider: provider,
			ReauthID: reauthID,
		})
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		out := jsoncompat.NewObject()
		out.Set("login", poolJSONValue(session))
		out.Set("challenge", poolJSONValue(challenge))
		h.output(http.StatusOK, out, contentTypeJSON, "")
	case "switch":
		id, _ := body.MustGet("id").(string)
		if id == "" {
			h.fail(http.StatusBadRequest, errorBody("缺少目标账号 id"))
			return
		}
		if confirmed, _ := body.MustGet("confirmed").(bool); !confirmed {
			h.fail(http.StatusBadRequest, errorBody("请先确认切换"))
			return
		}
		// The ACP runtime manager doubles as the switcher's session gate and
		// re-attacher; when the agent kind is not enabled the credential swap
		// still runs (it governs CLI usage) with no sessions to re-attach.
		// Pool kinds map to agent kinds (minimax pool -> mcode agent).
		var sessions accpool.SessionManager
		if manager := h.server.ACPAgents[accpool.AgentKind(kind)]; manager != nil {
			sessions = manager
		}
		if err := pool.Switch(h.request.Context(), kind, id, "manual", sessions, nil); err != nil {
			var busy *accpool.BusyError
			if errors.As(err, &busy) {
				h.fail(http.StatusConflict, errorBody(err.Error()))
			} else {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
			}
			return
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	case "settings":
		enabled, ok := body.MustGet("autoSwitch").(bool)
		if !ok {
			h.fail(http.StatusBadRequest, errorBody("缺少 autoSwitch 开关值"))
			return
		}
		if err := pool.SetAutoSwitch(kind, enabled); err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		h.output(http.StatusOK, detail, contentTypeJSON, "")
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
	case "capture":
		acc, created, err := pool.CaptureCurrent(h.request.Context(), kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		captured := jsoncompat.NewObject()
		captured.Set("id", acc.ID)
		captured.Set("created", created)
		// WorkBuddy envelope-state credentials cannot call APIs; the phone
		// notice tells the user a QR re-login is the way to plain tokens.
		captured.Set("envelope", accpool.IsEnvelopeAccount(acc))
		detail.Set("captured", captured)
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	case "checkin":
		id, _ := body.MustGet("id").(string)
		if id == "" {
			h.fail(http.StatusBadRequest, errorBody("缺少目标账号 id"))
			return
		}
		acc, message, err := pool.Checkin(h.request.Context(), kind, id)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		// A successful check-in changes the balance; re-read the quota so the
		// panel shows the new credits without a second tap.
		_, _ = pool.RefreshQuota(h.request.Context(), kind, acc.ID)
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		checkin := jsoncompat.NewObject()
		checkin.Set("id", acc.ID)
		checkin.Set("message", message)
		detail.Set("checkin", checkin)
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	case "provision":
		id, _ := body.MustGet("id").(string)
		region, _ := body.MustGet("region").(string)
		if id == "" || region == "" {
			h.fail(http.StatusBadRequest, errorBody("缺少目标账号 id 或 region"))
			return
		}
		acc, message, err := pool.Provision(h.request.Context(), kind, id, region)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		_, _ = pool.RefreshQuota(h.request.Context(), kind, acc.ID)
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		provision := jsoncompat.NewObject()
		provision.Set("id", acc.ID)
		provision.Set("message", message)
		detail.Set("provision", provision)
		h.output(http.StatusOK, detail, contentTypeJSON, "")
	case "remove":
		id, _ := body.MustGet("id").(string)
		if id == "" {
			h.fail(http.StatusBadRequest, errorBody("缺少目标账号 id"))
			return
		}
		if confirmed, _ := body.MustGet("confirmed").(bool); !confirmed {
			h.fail(http.StatusBadRequest, errorBody("请先确认删除"))
			return
		}
		if err := pool.Remove(kind, id); err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		detail, err := pool.Detail(kind)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
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

// codexOverviewRow summarizes the Codex account manager for the pool overview;
// nil when Codex account management is not enabled.
func (h *handler) codexOverviewRow() *jsoncompat.Object {
	if h.server.Account == nil {
		return nil
	}
	public := h.server.Account.AccountsPublic()
	accounts, _ := public.MustGet("accounts").(*jsoncompat.Array)
	count := int64(0)
	if accounts != nil {
		count = int64(len(accounts.Items))
	}
	row := jsoncompat.NewObject()
	row.Set("kind", "codex")
	row.Set("activeId", public.MustGet("activeId"))
	row.Set("accountCount", count)
	row.Set("readOnly", true)
	return row
}

// handlePoolCodex is the read-only Codex branch: detail for the unified view,
// every write redirected to the dedicated Codex switch flow.
func (h *handler) handlePoolCodex(write bool, action string) {
	if h.server.Account == nil {
		h.fail(http.StatusConflict, errorBody("当前网关未启用 Codex 账号管理"))
		return
	}
	if write {
		h.fail(http.StatusBadRequest, errorBody("Codex 账号请使用上方「账号与接入」面板的切换流程"))
		return
	}
	if action != "" {
		h.fail(http.StatusNotFound, errorBody("未知的账号池操作"))
		return
	}
	public := h.server.Account.AccountsPublic()
	accounts, _ := public.MustGet("accounts").(*jsoncompat.Array)
	if accounts == nil {
		accounts = &jsoncompat.Array{}
	}
	detail := jsoncompat.NewObject()
	detail.Set("kind", "codex")
	detail.Set("readOnly", true)
	detail.Set("activeId", public.MustGet("activeId"))
	detail.Set("autoSwitch", false)
	detail.Set("accounts", accounts)
	detail.Set("history", &jsoncompat.Array{})
	h.output(http.StatusOK, detail, contentTypeJSON, "")
}
