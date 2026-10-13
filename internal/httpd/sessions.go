// Package httpd: session routes (timeline/detail/changes/poll/events) and
// the SSE stream writer, mirroring bridge/httpd.py's THREAD_ROUTE handling.
// The scripted fixtures match conformance/http/run.py's SCRIPTED_* payloads
// byte-for-byte (key order included).
package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

var threadRoute = regexp.MustCompile(
	`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})(?:/(events|send|stop|history|respond|reconnect|queue|catalog|settings|permissions|poll|timeline|changes|detail|notifications|uploads|message-action|rename|migrate|input))?$`)

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
	match := threadRoute.FindStringSubmatch(path)
	if match == nil {
		return false
	}
	threadID, action := match[1], match[2]
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
