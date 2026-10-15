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
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpadapter"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
)

var threadRoute = regexp.MustCompile(
	`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})(?:/(events|send|stop|history|respond|reconnect|queue|catalog|settings|permissions|poll|timeline|changes|detail|notifications|uploads|message-action|rename|migrate|input))?$`)
var uploadRoute = regexp.MustCompile(`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/uploads/([0-9a-f-]{36})/(preview|thumb)$`)
var fileRoute = regexp.MustCompile(`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/files/([a-f0-9]{64})$`)
var goalRoute = regexp.MustCompile(`^/api/sessions/([A-Za-z0-9][A-Za-z0-9._-]{4,127})/goal/(cancel|edit|status)$`)

// ScriptedSession is the conformance stand-in for service.LiveSession: an
// epoch, a sequence and a predicate-based wait. Production wiring injects a
// live follower sequence through Live so waits wake on desktop patches; the
// scripted state never changes on its own (matching Python's
// condition.wait_for semantics for static fixtures).
type ScriptedSession struct {
	Epoch    string
	Sequence int
	Live     func() (string, int, bool) // optional: (epoch, sequence) from the live projection
}

func (s *ScriptedSession) refresh() {
	if s.Live == nil {
		return
	}
	if epoch, sequence, ok := s.Live(); ok {
		s.Epoch, s.Sequence = epoch, sequence
	}
}

// WaitFor mirrors condition.wait_for(pred, timeout): immediate when the
// predicate already holds, otherwise it polls until the timeout and reports
// the predicate one last time.
func (s *ScriptedSession) WaitFor(predicate func() bool, timeout time.Duration) bool {
	s.refresh()
	if predicate() {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		s.refresh()
		if predicate() {
			return true
		}
	}
	s.refresh()
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

// NewScriptedBridge is reserved for the deterministic HTTP differential
// harness; production servers use Server as their bridge.
func NewScriptedBridge() SessionBridge { return newScriptedSessionBridge() }

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

// withTimelineAgents mirrors httpd.with_timeline_agents: expose the agent
// roster in timeline meta so live views keep it. Without configured ACP
// agents the result passes through untouched (golden parity).
func (h *handler) withTimelineAgents(result any) any {
	if len(h.server.acpAgentSet()) == 0 {
		return result
	}
	object, ok := jsoncompat.Clone(result).(*jsoncompat.Object)
	if !ok {
		return result
	}
	meta, ok := object.MustGet("meta").(*jsoncompat.Object)
	if !ok {
		return result
	}
	meta.Set("agents", h.server.agentRoster())
	return object
}

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
	if action == "notifications" && h.server.NotificationDir != "" {
		var value *jsoncompat.Object
		if write {
			raw, err := h.readJSON()
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return true
			}
			value = raw.(*jsoncompat.Object)
		}
		result, err := notifications.NewPolicyManager(h.server.NotificationDir).Chat(queryValue(query, "host", "local"), threadID, value)
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
		} else {
			h.output(http.StatusOK, result, contentTypeJSON, "")
		}
		return true
	}
	// Cross-agent migration owns its routing: it reads the source view (any
	// agent) and dispatches to an explicit target, so it must not fall into
	// the ownership-based ACP dispatch below.
	if write && action == "migrate" {
		h.handleMigrate(threadID)
		return true
	}
	// ACP dispatch mirrors Python's registry routing: an explicit agent param
	// forces that agent (unknown kinds are rejected), otherwise the thread is
	// routed by ownership — live sessions, cached list rows and lazy
	// placeholders alike, no sess_ prefix requirement.
	acpKind, acpManager := "", (*acpruntime.Manager)(nil)
	if agentParam := queryValue(query, "agent", ""); agentParam != "" {
		if agentParam != "codex" {
			agents := h.server.acpAgentSet()
			acpManager = agents[agentParam]
			if acpManager == nil {
				h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型"))
				return true
			}
			acpKind = agentParam
		}
	} else if resolution := h.server.resolveACP(threadID); resolution.manager != nil {
		acpKind, acpManager = resolution.kind, resolution.manager
	}
	if acpManager != nil {
		if h.handleACPRoute(acpKind, acpManager, threadID, action, write, query) {
			return true
		}
	} // Catalog and permission panels are capability-driven. ACP agents
	// answer through their manager above; codex reads its runtime catalog.
	// A gateway with neither surfaces an explicit empty response so the
	// mobile client keeps the controls hidden instead of 404ing.
	if !write && action == "catalog" {
		h.handleCodexCatalog(threadID, query)
		return true
	}
	if !write && action == "permissions" {
		result := jsoncompat.NewObject()
		result.Set("supported", false)
		result.Set("options", &jsoncompat.Array{})
		result.Set("current", nil)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
	}
	if write && action == "permissions" {
		h.fail(http.StatusConflict, errorBody("当前 Agent 不支持移动端权限模式设置"))
		return true
	}
	if !write && action == "input" {
		result := jsoncompat.NewObject()
		result.Set("waiting", false)
		result.Set("inputs", &jsoncompat.Array{})
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
	}
	if write && action == "reconnect" && h.server.Codex != nil {
		body, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return true
		}
		if activate, _ := body.(*jsoncompat.Object).MustGet("activate").(bool); activate {
			if _, watchErr := h.server.Codex.Watch(threadID, 1500*time.Millisecond); watchErr != nil {
				h.fail(http.StatusConflict, errorBody(watchErr.Error()))
				return true
			}
			if h.server.Creations != nil {
				if requestID, pending := h.server.Creations.pendingForTarget(threadID); pending != nil {
					submissionID := uuid.NewSHA1(uuid.NameSpaceURL, []byte("mobile-bridge:migrate:"+requestID)).String()
					if _, retryErr := h.server.Codex.StartTurnWithOptions(threadID, submissionID, pending.Message, "send", "", nil, nil); retryErr == nil {
						h.server.Creations.markSent(requestID)
					} else {
						h.server.Creations.markWaiting(requestID, retryErr.Error())
					}
				}
			}
		}
		result := jsoncompat.NewObject()
		result.Set("ok", true)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
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
		case "history":
			result, callErr := h.server.Codex.Call(threadID, "thread-follower-load-complete-history", jsoncompat.NewObject(), 180*time.Second)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		case "send":
			text, _ := body.(*jsoncompat.Object).MustGet("text").(string)
			if text == "" {
				h.fail(http.StatusBadRequest, errorBody("消息内容不能为空"))
				return true
			}
			id, _ := body.(*jsoncompat.Object).MustGet("id").(string)
			mode, _ := body.(*jsoncompat.Object).MustGet("mode").(string)
			workMode, _ := body.(*jsoncompat.Object).MustGet("workMode").(string)
			var attachments []codex.AttachmentInput
			if raw, ok := body.(*jsoncompat.Object).MustGet("attachments").(*jsoncompat.Array); ok {
				for _, value := range raw.Items {
					entry, ok := value.(*jsoncompat.Object)
					if !ok {
						h.fail(http.StatusBadRequest, errorBody("附件记录无效"))
						return true
					}
					attachmentID, _ := entry.MustGet("id").(string)
					file, getErr := h.server.Uploads.Get(threadID, attachmentID)
					if getErr != nil {
						h.fail(http.StatusBadRequest, errorBody(getErr.Error()))
						return true
					}
					path, _ := file.MustGet("localPath").(string)
					name, _ := file.MustGet("name").(string)
					image, _ := file.MustGet("image").(string)
					attachments = append(attachments, codex.AttachmentInput{Name: name, Path: path, Image: image != ""})
				}
			}
			var skills []codex.SkillInput
			if raw := bodyList(body, "skills"); len(raw) > 0 {
				resolved, skillErr := h.server.Codex.Catalog().ValidateSkills(h.codexCatalogCwd(threadID), raw, false)
				if skillErr != nil {
					h.fail(http.StatusBadRequest, errorBody(skillErr.Error()))
					return true
				}
				for _, entry := range resolved {
					skills = append(skills, codex.SkillInput{Name: entry["name"], Path: entry["path"]})
				}
			}
			result, callErr := h.server.Codex.StartTurnWithOptions(threadID, id, text, mode, workMode, skills, attachments)
			if callErr != nil {
				h.fail(http.StatusConflict, errorBody(callErr.Error()))
			} else {
				h.output(http.StatusOK, result, contentTypeJSON, "")
			}
			return true
		case "rename":
			title, _ := body.(*jsoncompat.Object).MustGet("title").(string)
			if err := validRenameTitle(title); err != "" {
				h.fail(http.StatusBadRequest, errorBody(err))
				return true
			}
			if _, parseErr := uuid.Parse(threadID); parseErr != nil {
				h.fail(http.StatusBadRequest, errorBody("会话 ID 无效"))
				return true
			}
			title = strings.TrimSpace(title)
			if renameErr := h.server.Codex.RenameConversation(threadID, title); renameErr != nil {
				h.fail(http.StatusConflict, errorBody(renameErr.Error()))
				return true
			}
			result := jsoncompat.NewObject()
			result.Set("id", threadID)
			result.Set("host", "local")
			result.Set("title", title)
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return true
		case "message-action":
			h.handleCodexMessageAction(threadID, acpKind, body.(*jsoncompat.Object))
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
	if !write && action == "" {
		if view, _, ok := h.liveView(threadID); ok {
			if len(h.server.acpAgentSet()) > 0 {
				view.Set("agents", h.server.agentRoster())
			}
			h.output(http.StatusOK, view, contentTypeJSON, "")
			return true
		}
	}
	if !write && (action == "timeline" || action == "changes" || action == "detail") {
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

// handleACPRoute serves the ACP-specific write surface (send/stop/respond/
// input/settings) plus the model catalog. Reads (view/timeline/changes/poll/
// events) return false here and flow through the same live-projection paths
// codex uses, so cursors and SSE semantics stay uniform across agents.
func (h *handler) handleACPRoute(kind string, manager *acpruntime.Manager, threadID, action string, write bool, query map[string][]string) bool {
	if write && action == "reconnect" {
		cwd := queryValue(query, "cwd", manager.CwdFor(threadID))
		if cwd == "" {
			cwd = "."
		}
		if _, err := manager.Reconcile(threadID, cwd, manager.InitializeTimeout); err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return true
		}
		result := jsoncompat.NewObject()
		result.Set("ok", true)
		h.output(http.StatusOK, result, contentTypeJSON, "")
		return true
	}
	session, err := manager.Open(threadID, queryValue(query, "cwd", "."), manager.InitializeTimeout)
	if err != nil {
		h.fail(http.StatusConflict, errorBody(err.Error()))
		return true
	}
	if !write {
		if action == "catalog" {
			result := manager.Catalog(session, queryValue(query, "kind", "models"),
				queryValue(query, "q", ""), queryIDs(query))
			pageCatalogSkills(result, intQuery(query, "offset", 0), intQuery(query, "limit", 200))
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
		mode, _ := object.MustGet("mode").(string)
		workMode, _ := object.MustGet("workMode").(string)
		if workMode == "goal" {
			h.fail(http.StatusBadRequest, errorBody("当前 Agent 不支持目标模式"))
			return true
		}
		var attachmentFiles []acpadapter.FileSpec
		if raw, ok := object.MustGet("attachments").(*jsoncompat.Array); ok {
			for _, value := range raw.Items {
				attachmentID := ""
				if entry, ok := value.(*jsoncompat.Object); ok {
					attachmentID, _ = entry.MustGet("id").(string)
				} else if legacyID, ok := value.(string); ok {
					attachmentID = legacyID
				}
				if attachmentID == "" {
					h.fail(http.StatusBadRequest, errorBody("附件记录无效"))
					return true
				}
				file, getErr := h.server.Uploads.Get(threadID, attachmentID)
				if getErr != nil {
					h.fail(http.StatusBadRequest, errorBody(getErr.Error()))
					return true
				}
				path, _ := file.MustGet("localPath").(string)
				name, _ := file.MustGet("name").(string)
				image, _ := file.MustGet("image").(string)
				attachmentFiles = append(attachmentFiles, acpadapter.FileSpec{Name: name, Path: path, Image: image != ""})
			}
		}
		blocks, blockErr := acpadapter.AttachmentBlocks(session.Process.Capabilities, attachmentFiles)
		if blockErr != nil {
			h.fail(http.StatusBadRequest, errorBody(blockErr.Error()))
			return true
		}
		// ACP has no native skill block: kimi activates a skill from a
		// leading slash command in the first text block.
		if raw := bodyList(body, "skills"); len(raw) > 0 {
			name, skillErr := manager.ResolveCommandSkill(session, raw)
			if skillErr != nil {
				h.fail(http.StatusBadRequest, errorBody(skillErr.Error()))
				return true
			}
			if strings.TrimSpace(text) != "" {
				text = "/" + name + " " + text
			} else {
				text = "/" + name
			}
		}
		if mode == "steer" {
			if manager.SteerMethod == "" {
				h.fail(http.StatusBadRequest, errorBody("当前 Agent 不支持补充任务，请使用排队发送"))
				return true
			}
			injected, steerErr := session.Steer(manager.SteerMethod, text)
			if steerErr != nil {
				h.fail(http.StatusConflict, errorBody(steerErr.Error()))
				return true
			}
			if injected {
				result := jsoncompat.NewObject()
				result.Set("accepted", true)
				result.Set("steered", true)
				h.output(http.StatusOK, result, contentTypeJSON, "")
				return true
			}
			// promptRequired: the turn ended in the race window; send normally.
		}
		if workMode == "plan" || workMode == "default" {
			modeID := workMode
			if mapped, ok := manager.ModeMap[workMode]; ok {
				modeID = mapped
			}
			if modeErr := session.SetMode(modeID); modeErr != nil {
				h.fail(http.StatusBadRequest, errorBody(modeErr.Error()))
				return true
			}
		}
		var extra []*jsoncompat.Object
		for _, value := range blocks.Items {
			if block, ok := value.(*jsoncompat.Object); ok {
				extra = append(extra, block)
			}
		}
		if callErr := session.PromptWithBlocks(text, extra, 3600*time.Second); callErr != nil {
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
	case "settings":
		modelName, _ := object.MustGet("model").(string)
		effort, _ := object.MustGet("effort").(string)
		var fastMode *bool
		if value, exists := object.Get("fastMode"); exists {
			flag, valid := value.(bool)
			if !valid {
				h.fail(http.StatusBadRequest, errorBody("Fast 模式开关无效"))
				return true
			}
			fastMode = &flag
		}
		if callErr := manager.UpdateSettings(session, modelName, effort, fastMode); callErr != nil {
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

func (h *handler) handleGoalHTTP(write bool, path string) bool {
	if !write {
		return false
	}
	match := goalRoute.FindStringSubmatch(path)
	if match == nil {
		return false
	}
	if h.server.Goal == nil {
		h.fail(http.StatusConflict, errorBody("请启动支持目标模式的新版网关"))
		return true
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return true
	}
	body := value.(*jsoncompat.Object)
	threadID, action := match[1], match[2]
	switch action {
	case "cancel":
		if err := h.server.Goal.Clear(threadID); err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
		} else {
			h.output(http.StatusOK, jsoncompat.NewObject(), contentTypeJSON, "")
		}
	case "edit":
		objective, _ := body.MustGet("objective").(string)
		result, callErr := h.server.Goal.Set(threadID, objective, "paused")
		if callErr != nil {
			h.fail(http.StatusConflict, errorBody(callErr.Error()))
		} else {
			h.output(http.StatusOK, result, contentTypeJSON, "")
		}
	case "status":
		status, _ := body.MustGet("status").(string)
		result, callErr := h.server.Goal.Set(threadID, "", status)
		if callErr != nil {
			h.fail(http.StatusConflict, errorBody(callErr.Error()))
		} else {
			h.output(http.StatusOK, result, contentTypeJSON, "")
		}
	}
	return true
}

func (h *handler) handleUploadHTTP(write bool, path string, query map[string][]string) bool {
	if h.server.Uploads == nil {
		return false
	}
	if !write {
		if match := fileRoute.FindStringSubmatch(path); match != nil {
			row, err := h.server.Uploads.Artifact(match[1], match[2])
			if err != nil {
				h.fail(http.StatusNotFound, errorBody(err.Error()))
				return true
			}
			localPath, _ := row.MustGet("localPath").(string)
			data, err := os.ReadFile(localPath)
			if err != nil {
				h.fail(http.StatusNotFound, errorBody("附件已不可用，请重新上传"))
				return true
			}
			mimeType := "application/octet-stream"
			if image, _ := row.MustGet("image").(string); image != "" {
				mimeType = image
			}
			name, _ := row.MustGet("name").(string)
			disposition := "attachment"
			if mimeType != "application/octet-stream" {
				disposition = "inline"
			}
			h.writer.Header().Set("Content-Type", mimeType)
			h.writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
			h.writer.Header().Set("Content-Disposition", disposition+"; filename*=UTF-8''"+url.PathEscape(name))
			h.writer.WriteHeader(http.StatusOK)
			_, _ = h.writer.Write(data)
			return true
		}
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

// liveView resolves the live chat-screen state for one thread, ACP threads
// first, then codex followers. kind is the owning ACP agent ("" for codex).
func (h *handler) liveView(threadID string) (*jsoncompat.Object, string, bool) {
	if resolution := h.server.resolveACP(threadID); resolution.manager != nil {
		if resolution.session == nil {
			return nil, resolution.kind, false
		}
		return h.server.acpViewState(resolution.kind, resolution.manager, resolution.session), resolution.kind, true
	}
	if h.server.Codex == nil {
		return nil, "", false
	}
	_, _ = h.server.Codex.Watch(threadID, 1500*time.Millisecond)
	state, sequence, connected, connectionError := h.server.Codex.Snapshot(threadID)
	if state == nil {
		return nil, "", false
	}
	view := model.NormalizeState(state, connected)
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("connectionError", connectionError)
	view.Set("connecting", false)
	view.Set("activating", false)
	view.Set("loadingHistory", false)
	view.Set("host", "local")
	view.Set("hostLabel", "此电脑")
	view.Set("agent", "codex")
	view.Set("capabilities", h.server.codexCapabilities())
	h.server.codexSubagentView(threadID, view)
	return view, "", true
}

// liveTimeline projects a live view (codex follower or ACP snapshot) through
// the shared per-thread projection so HTTP cursors stay valid.
func (h *handler) liveTimeline(threadID, action string, query map[string][]string) (any, bool, error) {
	view, kind, ok := h.liveView(threadID)
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
	projection.SetMeta("host", "local")
	if kind != "" {
		projection.SetMeta("agent", kind)
		projection.SetMeta("hostLabel", "此电脑")
	} else {
		projection.SetMeta("agent", "codex")
		projection.SetMeta("hostLabel", "此电脑")
	}
	// Views carry their own capabilities (codex set, per-agent ACP set);
	// only a view that shipped without any gets the empty default.
	if _, ok := projection.MetaSnapshot().Get("capabilities"); !ok {
		projection.SetMeta("capabilities", &jsoncompat.Array{})
	}
	// Live timelines keep the agent roster in meta (with_timeline_agents):
	// the chat screen renders from timeline pages/changes, and the migrate
	// control loses its target list without it.
	if len(h.server.acpAgentSet()) > 0 {
		projection.SetMeta("agents", h.server.agentRoster())
	}
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
	view.Set("host", "local")
	view.Set("hostLabel", "此电脑")
	view.Set("agent", "codex")
	view.Set("capabilities", h.server.codexCapabilities())
	h.server.codexSubagentView(threadID, view)

	h.server.historyMu.Lock()
	defer h.server.historyMu.Unlock()
	projection := h.server.historyTL[threadID]
	if projection == nil {
		projection = timeline.New(nil)
		h.server.historyTL[threadID] = projection
	}
	projection.Update(view)
	projection.SetMeta("host", "local")
	projection.SetMeta("agent", "codex")
	projection.SetMeta("hostLabel", "此电脑")
	if _, ok := projection.MetaSnapshot().Get("capabilities"); !ok {
		projection.SetMeta("capabilities", &jsoncompat.Array{})
	}
	// Saved-rollout timelines carry the roster too (with_timeline_agents
	// applied to every timeline_read in Python).
	if len(h.server.acpAgentSet()) > 0 {
		projection.SetMeta("agents", h.server.agentRoster())
	}
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
			// The same persistent projection the HTTP routes read from,
			// fed the connection's own cursor (stream_incremental parity).
			// The scripted conformance bridge has no live projection; it
			// keeps serving through TimelineRead like it always did.
			var payload *jsoncompat.Object
			payloadAny, handled, err := h.liveTimeline(threadID, "changes", map[string][]string{
				"after": {strconv.Itoa(sequence)},
				"epoch": {epoch},
				"start": {""},
			})
			if handled {
				payload, _ = payloadAny.(*jsoncompat.Object)
				if err != nil {
					payload = nil
				}
			} else {
				payload, _ = h.withTimelineAgents(
					bridge.TimelineRead(threadID, "changes")).(*jsoncompat.Object)
			}
			if payload == nil {
				h.writeRaw(": heartbeat\n\n")
			} else {
				epochValue, _ := payload.MustGet("epoch").(string)
				sequenceNumber, _ := payload.MustGet("sequence").(json.Number)
				parsed, _ := strconv.Atoi(sequenceNumber.String())
				event := "changes"
				if _, reset := payload.Get("reset"); reset {
					event = "reset"
				}
				epoch, sequence = epochValue, parsed
				h.sseFrame(event, payload, epoch+"."+strconv.Itoa(sequence))
			}
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
