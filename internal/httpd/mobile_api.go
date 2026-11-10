// Companion mobile API surface (httpd.py's /api/mobile/* routes port):
// the inbox feed with long-polling, read receipts, native push registration
// and the health shell page. Requests share the gateway's session/CSRF/Origin
// conventions; a slow device never blocks another (each long-poll holds only
// its own goroutine and the feed wakeups are broadcast).
package httpd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
)

var (
	afterPattern      = regexp.MustCompile(`^[0-9]{1,16}$`)
	waitPattern       = regexp.MustCompile(`^[0-9]{1,2}$`)
	revisionPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
	streamIDValidator = func(value string) bool {
		return len(value) >= 1 && len(value) <= 128 && !strings.ContainsFunc(value, func(r rune) bool { return r < 32 })
	}
)

// mobileEnabled reads the inbox switch from notification settings.
func (h *handler) mobileEnabled() bool {
	if h.server.NotificationDir == "" {
		return false
	}
	return notifications.MobileInboxEnabled(h.server.NotificationDir)
}

// mobilePolicyFilter builds the per-thread allowed filter from the existing
// notification policy store (global requests/completion defaults with
// per-chat overrides) — the same semantics the outbound channels use.
func (h *handler) mobilePolicyFilter() func(event *notifications.MobileEvent) bool {
	if h.server.NotificationDir == "" {
		return nil
	}
	manager := notifications.NewPolicyManager(h.server.NotificationDir)
	policies := manager.Snapshot()
	effective := func(host, threadID string) map[string]bool {
		result := map[string]bool{}
		if requests, ok := policies.MustGet("requests").(bool); ok {
			result["request"] = requests
		} else {
			result["request"] = true
		}
		if completion, ok := policies.MustGet("completion").(bool); ok {
			result["completion"] = completion
		}
		if chats, ok := policies.MustGet("chats").(*jsoncompat.Object); ok {
			if chat, ok := chats.MustGet(host + "|" + threadID).(*jsoncompat.Object); ok {
				if requests, ok := chat.MustGet("requests").(bool); ok {
					result["request"] = requests
				}
				if completion, ok := chat.MustGet("completion").(bool); ok {
					result["completion"] = completion
				}
			}
		}
		return result
	}
	cache := map[string]map[string]bool{}
	return func(event *notifications.MobileEvent) bool {
		key := event.Host + "|" + event.ThreadID
		policy, cached := cache[key]
		if !cached {
			policy = effective(event.Host, event.ThreadID)
			cache[key] = policy
		}
		if event.Kind == "completion" {
			return policy["completion"]
		}
		return policy["request"]
	}
}

// mobileClients lists the provider roster with per-provider unread counts
// (upstream builds it from desktop instances; this gateway lists the codex
// plus the enabled ACP agents).
func (h *handler) mobileClients(snapshot *notifications.MobileSnapshot) []map[string]any {
	ids := []string{"codex"}
	// agentRoster already mirrors the enabled set; keep the order stable.
	for _, item := range h.server.agentRoster().Items {
		agent, _ := item.(*jsoncompat.Object)
		if agent == nil {
			continue
		}
		if kind, _ := agent.MustGet("kind").(string); kind != "" && kind != "codex" {
			ids = append(ids, kind)
		}
	}
	clients := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		clients = append(clients, map[string]any{"id": id, "connected": nil, "enabled": true,
			"unread": snapshot.Unread[id]})
	}
	return clients
}

// snapshotState renders the feed snapshot into the response shape and the
// stable revision token (sha256 over the Python-default JSON of the state
// minus events, sorted keys — matching upstream's revision contract).
func (h *handler) snapshotState(snapshot *notifications.MobileSnapshot, enabled bool, clients []map[string]any) (state map[string]any, revision string) {
	unread := map[string]any{}
	for provider, count := range snapshot.Unread {
		unread[provider] = count
	}
	// The revision encoder walks map[string]any; Seen's map[string]int64 must
	// be copied over like unread or the render panics on its first read.
	seen := map[string]any{}
	for provider, value := range snapshot.Seen {
		seen[provider] = value
	}
	state = map[string]any{
		"clients":  clients,
		"cursor":   snapshot.Cursor,
		"enabled":  enabled,
		"seen":     seen,
		"streamId": snapshot.StreamID,
		"summary":  snapshot.Summary,
		"unread":   unread,
	}
	encoded := pythonSortedJSON(state)
	sum := sha256.Sum256([]byte(encoded))
	return state, hex.EncodeToString(sum[:])[:32]
}

// pythonSortedJSON encodes a map as Python's json.dumps(sort_keys=True) with
// the default separators (", ", ": ").
func pythonSortedJSON(value map[string]any) string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	object := jsoncompat.NewObject()
	for _, key := range keys {
		object.Set(key, pythonSortedValue(value[key]))
	}
	return string(jsoncompat.EncodePythonDefault(object))
}

func pythonSortedValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		for i := 1; i < len(keys); i++ {
			for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
				keys[j], keys[j-1] = keys[j-1], keys[j]
			}
		}
		object := jsoncompat.NewObject()
		for _, key := range keys {
			object.Set(key, pythonSortedValue(typed[key]))
		}
		return object
	case []map[string]any:
		array := &jsoncompat.Array{}
		for _, item := range typed {
			array.Items = append(array.Items, pythonSortedValue(item))
		}
		return array
	case []any:
		array := &jsoncompat.Array{}
		for _, item := range typed {
			array.Items = append(array.Items, pythonSortedValue(item))
		}
		return array
	default:
		return value
	}
}

// handleMobileEvents serves GET /api/mobile/events: validated parameters, one
// passive snapshot per phone, optional long-poll (wait ≤ 60s) woken by any
// feed change, and a post-wait authorization recheck (a device logged out
// during the wait gets 401, never a stale snapshot).
func (h *handler) handleMobileEvents() {
	query := h.request.URL.Query()
	afterText := query.Get("after")
	if afterText == "" {
		afterText = "0"
	}
	if !afterPattern.MatchString(afterText) {
		h.fail(http.StatusBadRequest, errorBody("通知游标格式不正确"))
		return
	}
	after, _ := strconv.ParseInt(afterText, 10, 64)
	waitText := query.Get("wait")
	if waitText == "" {
		waitText = "0"
	}
	if !waitPattern.MatchString(waitText) {
		h.fail(http.StatusBadRequest, errorBody("通知等待参数无效"))
		return
	}
	wait, _ := strconv.Atoi(waitText)
	if wait > 60 {
		h.fail(http.StatusBadRequest, errorBody("通知等待参数无效"))
		return
	}
	stream := query.Get("streamId")
	if stream != "" && !streamIDValidator(stream) {
		h.fail(http.StatusBadRequest, errorBody("通知等待参数无效"))
		return
	}
	revision := query.Get("revision")
	if revision != "" && !revisionPattern.MatchString(revision) {
		h.fail(http.StatusBadRequest, errorBody("通知等待参数无效"))
		return
	}
	if h.server.Mobile == nil {
		h.fail(http.StatusConflict, errorBody("请启动支持手机收件箱的新版网关"))
		return
	}
	feed := h.server.Mobile
	enabled := h.mobileEnabled()
	allowed := h.mobilePolicyFilter()
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		version := feed.Version()
		snapshot := feed.Read(after, allowed)
		clients := h.mobileClients(snapshot)
		state, token := h.snapshotState(snapshot, enabled, clients)
		remaining := time.Until(deadline)
		if remaining <= 0 || feed.Closed() || !enabled || stream != snapshot.StreamID ||
			after != snapshot.Cursor || revision != token {
			h.outputMobileSnapshot(snapshot, state, token, time.Duration(wait)*time.Second-remaining)
			return
		}
		// External preference changes live outside the feed; bound each wait
		// slice so settings flips surface without a new event.
		slice := 15 * time.Second
		if remaining < slice {
			slice = remaining
		}
		feed.Wait(version, slice)
		// Authorized after the wait: revocation during a long-poll must not
		// leak a fresh snapshot. sessionValue already renders the 401.
		if h.sessionValue(false) == nil {
			return
		}
		enabled = h.mobileEnabled()
		allowed = h.mobilePolicyFilter()
	}
}

// outputMobileSnapshot renders the final response body: the state plus the
// ageMs-decorated events and the transport metadata.
func (h *handler) outputMobileSnapshot(snapshot *notifications.MobileSnapshot, state map[string]any, revision string, waited time.Duration) {
	now := time.Now()
	events := make([]*notifications.MobileEventView, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		view := *event
		view.StreamID = snapshot.StreamID
		events = append(events, &view)
	}
	body := jsoncompat.NewObject()
	body.Set("streamId", snapshot.StreamID)
	body.Set("cursor", snapshot.Cursor)
	unread := jsoncompat.NewObject()
	for provider, count := range snapshot.Unread {
		unread.Set(provider, jsoncompat.Num64(count))
	}
	body.Set("unread", unread)
	seen := jsoncompat.NewObject()
	for provider, value := range snapshot.Seen {
		seen.Set(provider, jsoncompat.Num64(value))
	}
	body.Set("seen", seen)
	summary := &jsoncompat.Array{}
	for _, item := range snapshot.Summary {
		entry := jsoncompat.NewObject()
		entry.Set("provider", item["provider"])
		entry.Set("kind", item["kind"])
		entry.Set("sequence", jsoncompat.Num64(item["sequence"].(int64)))
		summary.Items = append(summary.Items, entry)
	}
	body.Set("summary", summary)
	eventArray := &jsoncompat.Array{}
	for _, event := range events {
		entry := jsoncompat.NewObject()
		entry.Set("id", event.ID)
		entry.Set("sequence", jsoncompat.Num64(event.Sequence))
		entry.Set("threadId", event.ThreadID)
		entry.Set("host", event.Host)
		entry.Set("trace", event.Trace)
		entry.Set("kind", event.Kind)
		entry.Set("title", event.Title)
		entry.Set("body", event.Body)
		entry.Set("createdAt", json.Number(strconv.FormatFloat(event.CreatedAt, 'f', -1, 64)))
		entry.Set("provider", event.Provider)
		entry.Set("clientId", event.ClientID)
		entry.Set("unread", event.Unread)
		age := now.Sub(time.Unix(int64(event.CreatedAt), 0))
		if age < 0 {
			age = 0
		}
		entry.Set("ageMs", jsoncompat.Num64(age.Milliseconds()))
		entry.Set("streamId", event.StreamID)
		eventArray.Items = append(eventArray.Items, entry)
	}
	body.Set("events", eventArray)
	body.Set("enabled", state["enabled"])
	clients := &jsoncompat.Array{}
	for _, client := range state["clients"].([]map[string]any) {
		entry := jsoncompat.NewObject()
		entry.Set("id", client["id"])
		if client["connected"] == nil {
			entry.Set("connected", nil)
		} else {
			entry.Set("connected", client["connected"])
		}
		entry.Set("enabled", client["enabled"])
		if count, ok := client["unread"].(int64); ok {
			entry.Set("unread", jsoncompat.Num64(count))
		} else {
			entry.Set("unread", jsoncompat.Num(0))
		}
		clients.Items = append(clients.Items, entry)
	}
	body.Set("clients", clients)
	body.Set("longPoll", jsoncompat.Num(60))
	body.Set("revision", revision)
	timing := jsoncompat.NewObject()
	waitedMs := waited.Milliseconds()
	if waitedMs < 0 {
		waitedMs = 0
	}
	timing.Set("waitMs", jsoncompat.Num64(waitedMs))
	body.Set("timing", timing)
	h.output(http.StatusOK, body, contentTypeJSON, "")
}

// handleMobileEventsRead serves POST /api/mobile/events/read: a read receipt
// for one provider (or all) that cannot rewind what another device read.
func (h *handler) handleMobileEventsRead() {
	if h.server.Mobile == nil {
		h.fail(http.StatusConflict, errorBody("请启动支持手机收件箱的新版网关"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body, _ := value.(*jsoncompat.Object)
	if body == nil || len(body.Keys()) != 3 {
		h.fail(http.StatusBadRequest, errorBody("通知已读参数无效"))
		return
	}
	provider, _ := body.MustGet("provider").(string)
	stream, _ := body.MustGet("streamId").(string)
	throughNumber, ok := body.MustGet("through").(json.Number)
	if !ok {
		h.fail(http.StatusBadRequest, errorBody("通知已读参数无效"))
		return
	}
	through, err := throughNumber.Int64()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody("通知已读参数无效"))
		return
	}
	if ackErr := h.server.Mobile.Acknowledge(provider, stream, through); ackErr != nil {
		h.fail(http.StatusBadRequest, errorBody(ackErr.Error()))
		return
	}
	h.outputMobileSnapshot(h.server.Mobile.Read(0, h.mobilePolicyFilter()), h.snapshotForEnabled(), "", 0)
}

func (h *handler) snapshotForEnabled() map[string]any {
	snapshot := h.server.Mobile.Read(0, h.mobilePolicyFilter())
	state, _ := h.snapshotState(snapshot, h.mobileEnabled(), h.mobileClients(snapshot))
	return state
}

// handleMobilePush serves GET (provider status) and POST (registration) for
// /api/mobile/push. Registration never carries provider credentials; it
// requires password protection and a trusted origin like upstream.
func (h *handler) handleMobilePush(write bool) {
	if h.server.MobilePush == nil {
		h.fail(http.StatusConflict, errorBody("请启动支持手机推送的新版网关"))
		return
	}
	if !write {
		status := jsoncompat.NewObject()
		for key, value := range h.server.MobilePush.Status() {
			status.Set(key, value)
		}
		h.output(http.StatusOK, status, contentTypeJSON, "")
		return
	}
	if h.server.Auth.Config.Mode == "none" {
		h.fail(http.StatusForbidden, errorBody("请启用密码保护后注册系统推送"))
		return
	}
	origin := h.request.Header.Get("Origin")
	if !h.server.Origins[origin] && (h.server.AllowOrigin == nil || !h.server.AllowOrigin(origin)) {
		h.fail(http.StatusForbidden, errorBody("不允许的推送注册来源"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body, _ := value.(*jsoncompat.Object)
	payload := map[string]any{}
	for _, key := range body.Keys() {
		entry, _ := body.Get(key)
		payload[key] = entry
	}
	token, sessionKey, _ := h.routeSessionKey()
	_ = token
	result, registerErr := h.server.MobilePush.Register(payload, sessionKey, origin)
	if registerErr != nil {
		h.fail(http.StatusBadRequest, errorBody(registerErr.Error()))
		return
	}
	response := jsoncompat.NewObject()
	for key, entry := range result {
		response.Set(key, entry)
	}
	h.output(http.StatusOK, response, contentTypeJSON, "")
}

// sessionValue resolves the request session without the CSRF branch (the
// long-poll recheck must not demand a token on a GET).
func (h *handler) sessionValue(write bool) *jsoncompat.Object {
	token, sessionKey, viaRoute := h.routeSessionKey()
	if token == "" {
		h.fail(http.StatusUnauthorized, unauthenticatedBody())
		return nil
	}
	var session *jsoncompat.Object
	if viaRoute {
		if sessionKey == "" {
			h.fail(http.StatusUnauthorized, unauthenticatedBody())
			return nil
		}
		session = h.server.Auth.GetSessionByKey(sessionKey, h.client(), h.request.UserAgent())
	} else {
		session = h.server.Auth.GetForClient(token, h.client(), h.request.UserAgent())
	}
	if session == nil {
		h.fail(http.StatusUnauthorized, unauthenticatedBody())
		return nil
	}
	if write {
		expected, _ := session.MustGet("csrf").(string)
		if !constantTimeEqual(h.request.Header.Get("X-CSRF-Token"), expected) {
			h.fail(http.StatusForbidden, errorBody("登录验证已失效，请刷新页面"))
			return nil
		}
	}
	return session
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var diff byte
	for index := 0; index < len(left); index++ {
		diff |= left[index] ^ right[index]
	}
	return diff == 0
}

// handleConnectionProve serves the anonymous GET /api/connection/prove.
func (h *handler) handleConnectionProve() {
	address, _ := h.client().MustGet("ip").(string)
	if !h.server.Auth.Permitted(address) {
		h.fail(http.StatusForbidden, errorBody("Connection discovery unavailable"))
		return
	}
	query := h.request.URL.Query()
	status, result, err := h.prove(query.Get("id"), query.Get("challenge"), h.target(), query.Get("device"))
	if err != nil || result == nil {
		h.fail(status, errorBody(err.Error()))
		return
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleConnectionRoutes serves GET/POST /api/connection/routes for paired
// phone user agents.
func (h *handler) handleConnectionRoutes(write bool) {
	if !auth.MobileClient(h.request.UserAgent()) {
		h.fail(http.StatusForbidden, errorBody("需要手机网关绑定"))
		return
	}
	token, sessionKey, viaRoute := h.routeSessionKey()
	if token == "" || (viaRoute && sessionKey == "") {
		h.fail(http.StatusUnauthorized, unauthenticatedBody())
		return
	}
	var session *jsoncompat.Object
	if viaRoute {
		session = h.server.Auth.GetSessionByKey(sessionKey, h.client(), h.request.UserAgent())
	} else {
		session = h.server.Auth.GetForClient(token, h.client(), h.request.UserAgent())
	}
	if session == nil {
		h.fail(http.StatusUnauthorized, unauthenticatedBody())
		return
	}
	if !write {
		routes := h.mobileRoutes(sessionKey, token, viaRoute)
		if routes == nil {
			h.fail(http.StatusForbidden, errorBody("需要手机网关绑定"))
			return
		}
		h.output(http.StatusOK, routes, contentTypeJSON, "")
		return
	}
	expected, _ := session.MustGet("csrf").(string)
	if !constantTimeEqual(h.request.Header.Get("X-CSRF-Token"), expected) {
		h.fail(http.StatusForbidden, errorBody("登录验证已失效，请刷新页面"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body, _ := value.(*jsoncompat.Object)
	if body == nil {
		h.fail(http.StatusBadRequest, errorBody("无效的信道绑定"))
		return
	}
	status, renewErr := h.renewRoutes(sessionKey, token, body)
	if renewErr != nil {
		h.fail(status, errorBody(renewErr.Error()))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("bound", true)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// handleMobileChannel serves the bare shell page a companion loads in its
// verified secondary view (the transport code is injected by the app).
func (h *handler) handleMobileChannel() {
	h.output(http.StatusOK, []byte("<!doctype html><meta charset=utf-8><title>Bridge</title>"), "text/html; charset=utf-8", "")
}

// handleHealth serves the local-only instance probe.
func (h *handler) handleHealth() {
	address, _ := h.client().MustGet("ip").(string)
	if address != "127.0.0.1" && address != "::1" {
		h.fail(http.StatusForbidden, errorBody("仅允许本机状态检查"))
		return
	}
	body := jsoncompat.NewObject()
	body.Set("service", "mobile-bridge")
	body.Set("instanceId", h.server.InstanceID)
	body.Set("notifications", h.server.Mobile != nil)
	h.output(http.StatusOK, body, contentTypeJSON, "")
}
