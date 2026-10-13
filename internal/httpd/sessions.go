// Package httpd: session routes (timeline/detail/changes/poll/events) and
// the SSE stream writer, mirroring bridge/httpd.py's THREAD_ROUTE handling.
// The scripted fixtures match conformance/http/run.py's SCRIPTED_* payloads
// byte-for-byte (key order included).
package httpd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

var threadRoute = regexp.MustCompile(
	`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})(?:/(events|send|stop|history|respond|reconnect|queue|catalog|settings|permissions|poll|timeline|changes|detail|notifications|uploads|message-action|rename|migrate|input))?$`)
var uploadRoute = regexp.MustCompile(`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/uploads/([0-9a-f-]{36})/(preview|thumb)$`)

// ScriptedSession is the conformance stand-in for service.LiveSession: an
// epoch, a sequence and a predicate-based wait. Production wiring replaces it
// in P6; the scripted state never changes, so waits with an already-true
// predicate return immediately (matching Python's condition.wait_for).
type ScriptedSession struct {
	Epoch    string
	Sequence int
}

// WaitFor mirrors condition.wait_for(pred, timeout): immediate when the
// predicate already holds, otherwise it polls until the timeout and reports
// the predicate one last time.
func (s *ScriptedSession) WaitFor(predicate func() bool, timeout time.Duration) bool {
	if predicate() {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if predicate() {
			return true
		}
	}
	return predicate()
}

// SessionBridge extends Bridge with the session-route surface.
type SessionBridge interface {
	Bridge
	ForHost(host string) (SessionBridge, error)
	Session(threadID string) *ScriptedSession
	View(threadID string) any
	TimelineRead(threadID, mode string) any
	Closed() <-chan struct{}
}

func scriptedPage() *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("key", "k-1")
	row.Set("order", jsoncompat.Num(0))
	row.Set("text", "页行 中文")
	meta := jsoncompat.NewObject()
	meta.Set("sequence", jsoncompat.Num(7))
	meta.Set("requests", &jsoncompat.Array{})
	meta.Set("latestUserTurnId", nil)
	page := jsoncompat.NewObject()
	page.Set("epoch", "scripted-epoch")
	page.Set("sequence", jsoncompat.Num(7))
	page.Set("rows", &jsoncompat.Array{Items: []any{row}})
	page.Set("before", "scripted-epoch.k-0")
	page.Set("hasMore", false)
	page.Set("meta", meta)
	return page
}

func scriptedDetail() *jsoncompat.Object {
	detail := jsoncompat.NewObject()
	detail.Set("key", "k-1")
	detail.Set("version", "v1")
	detail.Set("text", "明细内容")
	detail.Set("offset", jsoncompat.Num(0))
	detail.Set("next", nil)
	return detail
}

func scriptedChanges() *jsoncompat.Object {
	changes := jsoncompat.NewObject()
	changes.Set("epoch", "scripted-epoch")
	changes.Set("sequence", jsoncompat.Num(7))
	changes.Set("rows", &jsoncompat.Array{})
	changes.Set("meta", jsoncompat.NewObject())
	return changes
}

func scriptedView() *jsoncompat.Object {
	view := jsoncompat.NewObject()
	view.Set("id", "sess-1")
	view.Set("title", "脚本会话")
	view.Set("sequence", jsoncompat.Num(7))
	view.Set("turns", &jsoncompat.Array{})
	return view
}

// scriptedSessionBridge mirrors conformance/http/run.py's EmptyBridge.
type scriptedSessionBridge struct{ closed chan struct{} }

func newScriptedSessionBridge() *scriptedSessionBridge {
	return &scriptedSessionBridge{closed: make(chan struct{})}
}

func (b *scriptedSessionBridge) SessionsList() any {
	sessions := jsoncompat.NewObject()
	sessions.Set("sessions", &jsoncompat.Array{})
	sessions.Set("unavailableHosts", &jsoncompat.Array{})
	return sessions
}

func (b *scriptedSessionBridge) AccountsPublic() any { return stubBridge{}.AccountsPublic() }

func (b *scriptedSessionBridge) ForHost(host string) (SessionBridge, error) {
	if host != "local" {
		return nil, fmt.Errorf("KeyError: %s", host)
	}
	return b, nil
}

func (b *scriptedSessionBridge) Session(threadID string) *ScriptedSession {
	return &ScriptedSession{Epoch: "scripted-epoch", Sequence: 7}
}

func (b *scriptedSessionBridge) View(threadID string) any { return scriptedView() }
func (b *scriptedSessionBridge) TimelineRead(threadID, mode string) any {
	switch mode {
	case "detail":
		return scriptedDetail()
	case "changes":
		return scriptedChanges()
	}
	return scriptedPage()
}

func (b *scriptedSessionBridge) Closed() <-chan struct{} { return b.closed }

// withTimelineAgents mirrors httpd.with_timeline_agents: without a registry
// the result passes through untouched.
func (h *handler) withTimelineAgents(result any) any { return result }

func (h *handler) writeRaw(text string) {
	_, _ = h.writer.Write([]byte(text))
	if flusher, ok := h.writer.(http.Flusher); ok {
		flusher.Flush()
	}
}

// sseFrame mirrors httpd.sse_frame.
func (h *handler) sseFrame(event string, payload any, cursor string) {
	head := ""
	if cursor != "" {
		head = "id: " + cursor + "\n"
	}
	h.writeRaw(head + "event: " + event + "\ndata: " +
		string(jsoncompat.EncodeCompact(payload)) + "\n\n")
}

// handleSessionRoutes dispatches THREAD_ROUTE actions after the auth gate.
// Returns false when the path is not a session route.
func (h *handler) handleSessionRoutes(bridge SessionBridge, write bool, path string,
	query map[string][]string) bool {
	if write && h.server.Uploads != nil {
		if match := threadRoute.FindStringSubmatch(path); match != nil && match[2] == "uploads" {
			id := queryValue(query, "id", "")
			name := queryValue(query, "name", "")
			data, err := readBodyLimit(h.request, uploads.MaxFile)
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return true
			}
			result, putErr := h.server.Uploads.Put(match[1], id, name, data)
			if putErr != nil {
				h.fail(http.StatusBadRequest, errorBody(putErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		}
	}
	match := threadRoute.FindStringSubmatch(path)
	if match == nil {
		return false
	}
	threadID, action := match[1], match[2]
	if h.server.ACP != nil && strings.HasPrefix(threadID, "sess_") {
		if h.handleACPRoute(threadID, action, write, query) {
			return true
		}
	}
	if write && h.server.Codex != nil {
		if action == "uploads" && h.server.Uploads != nil {
			id := queryValue(query, "id", "")
			name := queryValue(query, "name", "")
			data, err := readBodyLimit(h.request, uploads.MaxFile)
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return true
			}
			result, putErr := h.server.Uploads.Put(threadID, id, name, data)
			if putErr != nil {
				h.fail(http.StatusBadRequest, errorBody(putErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		}
		body, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return true
		}
		switch action {
		case "send":
			text, _ := body.(*jsoncompat.Object).MustGet("text").(string)
			if text == "" {
				h.fail(http.StatusBadRequest, errorBody("消息内容不能为空"))
				return true
			}
			id, _ := body.(*jsoncompat.Object).MustGet("id").(string)
			result, callErr := h.server.Codex.StartTurn(threadID, id, text)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		case "stop":
			result, callErr := h.server.Codex.Interrupt(threadID)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		case "settings":
			modelName, _ := body.(*jsoncompat.Object).MustGet("model").(string)
			effort, _ := body.(*jsoncompat.Object).MustGet("effort").(string)
			var fastMode *bool
			if value, exists := body.(*jsoncompat.Object).Get("fastMode"); exists {
				flag, valid := value.(bool)
				if !valid {
					h.fail(http.StatusBadRequest, errorBody("Fast 模式开关无效"))
					return true
				}
				fastMode = &flag
			}
			result, callErr := h.server.Codex.UpdateSettings(threadID, modelName, effort, fastMode)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		case "respond", "input":
			requestID := ""
			response := jsoncompat.NewObject()
			if action == "respond" {
				requestID, _ = body.(*jsoncompat.Object).MustGet("requestId").(string)
				if raw, ok := body.(*jsoncompat.Object).MustGet("response").(*jsoncompat.Object); ok {
					response = raw
				}
			} else {
				requestID, _ = body.(*jsoncompat.Object).MustGet("id").(string)
				response.Set("answers", body.(*jsoncompat.Object).MustGet("answers"))
			}
			result, callErr := h.server.Codex.Respond(threadID, requestID, response)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		}
	}
	if !write && h.server.Uploads != nil {
		if strings.HasSuffix(path, "/preview") || strings.HasSuffix(path, "/thumb") {
			// Preview/thumb routes are handled below by their fixed path parser.
		}
	}
	if !write && action == "" && h.server.Codex != nil {
		if view, ok := h.liveView(threadID); ok {
			h.output(http.StatusOK, view, contentTypeJSON, "")
			return true
		}
	}
	if !write && h.server.Codex != nil && (action == "timeline" || action == "changes" || action == "detail") {
		if result, handled, err := h.liveTimeline(threadID, action, query); handled {
			if err != nil {
				h.fail(http.StatusConflict, errorBody(err.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		}
	}
	switch {
	case !write && action == "history" && h.server.Store != nil:
		result, err := h.server.Store.HistoryFor(threadID, nil)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return true
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case !write && h.server.Store != nil && (action == "timeline" || action == "changes" || action == "detail"):
		result, err := h.historicalTimeline(threadID, action, query)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return true
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case !write && action == "timeline":
		h.output(http.StatusOK, h.withTimelineAgents(bridge.TimelineRead(threadID, "page")),
			contentTypeJSON, "")
	case !write && action == "detail":
		h.output(http.StatusOK, bridge.TimelineRead(threadID, "detail"), contentTypeJSON, "")
	case !write && action == "changes":
		h.sessionRouteChanges(bridge, threadID, query)
	case !write && action == "poll":
		h.sessionRoutePoll(bridge, threadID, query)
	case !write && action == "events":
		h.stream(bridge, threadID, query)
	default:
		h.fail(http.StatusNotFound, errorBody("接口不存在"))
	}
	return true
}

func (h *handler) handleACPRoute(threadID, action string, write bool, query map[string][]string) bool {
	session, err := h.server.ACP.Open(threadID, queryValue(query, "cwd", "."), 10*time.Second)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return true
	}
	if !write {
		if action == "" {
			h.output(http.StatusOK, session.Snapshot(), contentTypeJSON, "")
			return true
		}
		if action == "timeline" {
			view := session.Snapshot()
			view.Set("sequence", jsoncompat.Num(1))
			h.server.historyMu.Lock()
			projection := h.server.historyTL[threadID]
			if projection == nil {
				projection = timeline.New(nil)
				h.server.historyTL[threadID] = projection
			}
			projection.Update(view)
			result := projection.Page(intQuery(query, "limit", 20), queryValue(query, "before", ""))
			projection.Meta().Set("host", "local")
			projection.Meta().Set("agent", "acp")
			projection.Meta().Set("hostLabel", "ACP Agent")
			h.server.historyMu.Unlock()
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return true
		}
		return false
	}
	body, bodyErr := h.readJSON()
	if bodyErr != nil {
		h.fail(http.StatusBadRequest, errorBody(bodyErr.Error()))
		return true
	}
	object := body.(*jsoncompat.Object)
	switch action {
	case "send":
		text, _ := object.MustGet("text").(string)
		if text == "" {
			h.fail(http.StatusBadRequest, errorBody("消息内容不能为空"))
			return true
		}
		if callErr := session.Prompt(text, 3600*time.Second); callErr != nil {
			h.fail(http.StatusConflict, errorBody(callErr.Error()))
		} else {
			h.output(http.StatusOK, jsoncompat.NewObject(), contentTypeJSON, "")
		}
	case "stop":
		if callErr := session.Cancel(); callErr != nil {
			h.fail(http.StatusConflict, errorBody(callErr.Error()))
		} else {
			h.output(http.StatusOK, jsoncompat.NewObject(), contentTypeJSON, "")
		}
	case "respond", "input":
		requestID, _ := object.MustGet("requestId").(string)
		response := jsoncompat.NewObject()
		if action == "input" {
			requestID, _ = object.MustGet("id").(string)
			response.Set("answers", object.MustGet("answers"))
		} else if raw, ok := object.MustGet("response").(*jsoncompat.Object); ok {
			response = raw
		}
		if callErr := session.Respond(requestID, response); callErr != nil {
			h.fail(http.StatusConflict, errorBody(callErr.Error()))
		} else {
			h.output(http.StatusOK, jsoncompat.NewObject(), contentTypeJSON, "")
		}
	default:
		return false
	}
	return true
}

func readBodyLimit(request *http.Request, limit int) ([]byte, error) {
	if request.ContentLength <= 0 || request.ContentLength > int64(limit) {
		return nil, errors.New("请求体大小无效")
	}
	data, err := io.ReadAll(io.LimitReader(request.Body, int64(limit)+1))
	if err != nil || len(data) == 0 || len(data) > limit {
		return nil, errors.New("请求体大小无效")
	}
	return data, nil
}

func (h *handler) handleUploadHTTP(write bool, path string, query map[string][]string) bool {
	if h.server.Uploads == nil {
		return false
	}
	match := uploadRoute.FindStringSubmatch(path)
	if match == nil {
		return false
	}
	threadID, id, action := match[1], match[2], match[3]
	if action == "thumb" && write {
		data, err := readBodyLimit(h.request, uploads.MaxThumb)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return true
		}
		width, errW := strconv.Atoi(queryValue(query, "width", "0"))
		height, errH := strconv.Atoi(queryValue(query, "height", "0"))
		if errW != nil || errH != nil {
			h.fail(http.StatusBadRequest, errorBody("缩略图尺寸无效"))
			return true
		}
		result, setErr := h.server.Uploads.SetThumb(threadID, id, data, width, height)
		if setErr != nil {
			h.fail(http.StatusBadRequest, errorBody(setErr.Error()))
		} else {
			h.output(http.StatusOK, result, contentTypeJSON, "")
		}
		return true
	}
	if write {
		return false
	}
	row, variant, err := h.server.Uploads.Preview(threadID, id, queryValue(query, "variant", "thumb"))
	if err != nil {
		h.fail(http.StatusNotFound, errorBody(err.Error()))
		return true
	}
	pathName, _ := row.MustGet("previewPath").(string)
	data, err := os.ReadFile(pathName)
	if err != nil {
		h.fail(http.StatusNotFound, errorBody("附件已不可用，请重新上传"))
		return true
	}
	mime, _ := row.MustGet("previewMime").(string)
	etag := `"` + jsoncompat.PyStr(row.MustGet("previewSha256")) + `"`
	if h.request.Header.Get("If-None-Match") == etag {
		h.writer.Header().Set("ETag", etag)
		h.writer.Header().Set("X-Preview-Variant", variant)
		h.writer.WriteHeader(http.StatusNotModified)
		return true
	}
	h.writer.Header().Set("Cache-Control", "private, no-store")
	h.writer.Header().Set("Content-Type", mime)
	h.writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
	h.writer.Header().Set("ETag", etag)
	h.writer.Header().Set("X-Preview-Variant", variant)
	h.writer.WriteHeader(http.StatusOK)
	_, _ = h.writer.Write(data)
	return true
}

func (h *handler) liveView(threadID string) (*jsoncompat.Object, bool) {
	if h.server.Codex == nil {
		return nil, false
	}
	_, _ = h.server.Codex.Watch(threadID, 1500*time.Millisecond)
	state, sequence, connected, connectionError := h.server.Codex.Snapshot(threadID)
	if state == nil {
		return nil, false
	}
	view := model.NormalizeState(state, connected)
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("connectionError", connectionError)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	return view, true
}

func (h *handler) liveTimeline(threadID, action string, query map[string][]string) (any, bool, error) {
	view, ok := h.liveView(threadID)
	if !ok {
		return nil, false, nil
	}
	h.server.historyMu.Lock()
	defer h.server.historyMu.Unlock()
	projection := h.server.historyTL[threadID]
	if projection == nil {
		projection = timeline.New(nil)
		h.server.historyTL[threadID] = projection
	}
	projection.Update(view)
	meta := projection.Meta()
	meta.Set("host", "local")
	meta.Set("agent", "codex")
	meta.Set("hostLabel", "此电脑")
	meta.Set("capabilities", &jsoncompat.Array{})
	meta.Set("goalRuntimeAvailable", false)
	if action == "detail" {
		var version any
		if value := queryValue(query, "version", ""); value != "" {
			version = value
		}
		result, err := projection.Detail(queryValue(query, "key", ""), intQuery(query, "offset", 0), version)
		return result, true, err
	}
	if action == "changes" {
		return projection.Changes(queryValue(query, "after", "-1"), queryValue(query, "epoch", ""), queryValue(query, "start", "")), true, nil
	}
	return projection.Page(intQuery(query, "limit", 20), queryValue(query, "before", "")), true, nil
}

// historicalTimeline projects a saved rollout through the same Go timeline
// implementation used for live sessions. The projection is retained per
// thread so cursors remain valid across HTTP requests.
func (h *handler) historicalTimeline(threadID, action string, query map[string][]string) (any, error) {
	history, err := h.server.Store.HistoryFor(threadID, nil)
	if err != nil {
		return nil, err
	}
	state := jsoncompat.NewObject()
	for _, key := range []string{"id", "title", "cwd", "latestModel", "modelProvider", "turns"} {
		if value, ok := history.Get(key); ok {
			state.Set(key, value)
		}
	}
	state.Set("turnsPagination", history.MustGet("turnsPagination"))
	var sequence any = jsoncompat.Num(1)
	if value, ok := history.Get("updated_at_ms"); ok {
		sequence = value
	} else if value, ok := history.Get("updated_at"); ok {
		sequence = value
	}
	state.Set("sequence", sequence)
	view := model.NormalizeState(state, false)
	view.Set("sequence", sequence)

	h.server.historyMu.Lock()
	defer h.server.historyMu.Unlock()
	projection := h.server.historyTL[threadID]
	if projection == nil {
		projection = timeline.New(nil)
		h.server.historyTL[threadID] = projection
	}
	projection.Update(view)
	meta := projection.Meta()
	meta.Set("host", "local")
	meta.Set("agent", "codex")
	meta.Set("hostLabel", "此电脑")
	meta.Set("capabilities", &jsoncompat.Array{})
	meta.Set("goalRuntimeAvailable", false)
	if action == "detail" {
		version := any(nil)
		if raw := queryValue(query, "version", ""); raw != "" {
			version = raw
		}
		return projection.Detail(queryValue(query, "key", ""), intQuery(query, "offset", 0), version)
	}
	if action == "changes" {
		return projection.Changes(queryValue(query, "after", "-1"), queryValue(query, "epoch", ""), queryValue(query, "start", "")), nil
	}
	return projection.Page(intQuery(query, "limit", 20), queryValue(query, "before", "")), nil
}

func intQuery(query map[string][]string, key string, fallback int) int {
	value, err := strconv.Atoi(queryValue(query, key, strconv.Itoa(fallback)))
	if err != nil {
		return fallback
	}
	return value
}

func queryValue(query map[string][]string, key, fallback string) string {
	if values, ok := query[key]; ok && len(values) > 0 {
		return values[0]
	}
	return fallback
}

func (h *handler) sessionRouteChanges(bridge SessionBridge, threadID string,
	query map[string][]string) {
	after, _ := strconv.Atoi(queryValue(query, "after", "-1"))
	epoch := queryValue(query, "epoch", "")
	start := queryValue(query, "start", "")
	session := bridge.Session(threadID)
	session.WaitFor(func() bool {
		return session.Sequence != after || epoch != session.Epoch
	}, 12*time.Second)
	if h.session(false) == nil {
		return
	}
	h.output(http.StatusOK, h.withTimelineAgents(bridge.TimelineRead(threadID, "changes")),
		contentTypeJSON, "")
	_ = start
}

func (h *handler) sessionRoutePoll(bridge SessionBridge, threadID string,
	query map[string][]string) {
	after, _ := strconv.Atoi(queryValue(query, "after", "-1"))
	session := bridge.Session(threadID)
	changed := session.WaitFor(func() bool { return session.Sequence != after }, 12*time.Second)
	if h.session(false) == nil {
		return
	}
	result := jsoncompat.NewObject()
	if changed {
		result.Set("state", bridge.View(threadID))
	} else {
		result.Set("state", nil)
	}
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

// stream mirrors httpd.stream: the SSE endpoint behind /events.
func (h *handler) stream(bridge SessionBridge, threadID string, query map[string][]string) {
	incremental := queryValue(query, "incremental", "") == "1"
	epoch, sequence := "", -1
	if incremental {
		since := h.request.Header.Get("Last-Event-ID")
		if since == "" {
			since = queryValue(query, "since", "")
		}
		if head, tail, found := strings.Cut(since, "."); found && head != "" && isDigits(tail) {
			epoch = head
			sequence, _ = strconv.Atoi(tail)
		}
	}
	session := bridge.Session(threadID)
	header := h.writer.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("X-Accel-Buffering", "no")
	header.Set("Connection", "close")
	h.writer.WriteHeader(http.StatusOK)
	h.closeConnection = true
	if incremental {
		h.streamIncremental(bridge, threadID, session, epoch, sequence)
		return
	}
	sequence = -1
	for {
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.writeRaw("event: logout\ndata: {}\n\n")
			return
		}
		updated := session.WaitFor(func() bool { return session.Sequence != sequence },
			12*time.Second)
		sequence = session.Sequence
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.writeRaw("event: logout\ndata: {}\n\n")
			return
		}
		if updated {
			payload := string(jsoncompat.EncodeCompact(bridge.View(threadID)))
			h.writeRaw("id: " + strconv.Itoa(sequence) + "\nevent: state\ndata: " + payload + "\n\n")
		} else {
			h.writeRaw(": heartbeat\n\n")
		}
		select {
		case <-bridge.Closed():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (h *handler) streamIncremental(bridge SessionBridge, threadID string,
	session *ScriptedSession, epoch string, sequence int) {
	welcome := jsoncompat.NewObject()
	welcome.Set("epoch", session.Epoch)
	welcome.Set("sequence", jsoncompat.Num(session.Sequence))
	h.sseFrame("ready", welcome, "")
	for {
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.sseFrame("logout", &jsoncompat.Object{}, "")
			return
		}
		changed := session.WaitFor(func() bool {
			return session.Sequence != sequence || session.Epoch != epoch
		}, 12*time.Second)
		if h.server.Auth.Get(h.cookieToken()) == nil {
			h.sseFrame("logout", &jsoncompat.Object{}, "")
			return
		}
		if changed {
			payload, _ := h.withTimelineAgents(
				bridge.TimelineRead(threadID, "changes")).(*jsoncompat.Object)
			epochValue, _ := payload.MustGet("epoch").(string)
			sequenceNumber, _ := payload.MustGet("sequence").(json.Number)
			epoch = epochValue
			sequence, _ = strconv.Atoi(sequenceNumber.String())
			event := "changes"
			if _, reset := payload.Get("reset"); reset {
				event = "reset"
			}
			h.sseFrame(event, payload, epoch+"."+strconv.Itoa(sequence))
		} else {
			h.writeRaw(": heartbeat\n\n")
		}
		select {
		case <-bridge.Closed():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func isDigits(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
