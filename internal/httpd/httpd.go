// Package httpd is the Go port of bridge/httpd.py's request pipeline:
// host/origin validation, the auth + CSRF gate, embedded assets, production
// session routes, JSON output with Python-default separators, and error
// mapping. The HTTP differential golden is the compatibility gate.
package httpd

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/pairing"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
)

const contentTypeJSON = "application/json; charset=utf-8"

// Bridge supplies the business responses behind the routes.
type Bridge interface {
	SessionsList() any
	AccountsPublic() any
}

type stubBridge struct{}

func (stubBridge) SessionsList() any {
	sessions := jsoncompat.NewObject()
	sessions.Set("sessions", &jsoncompat.Array{})
	sessions.Set("unavailableHosts", &jsoncompat.Array{})
	return sessions
}

func (stubBridge) AccountsPublic() any {
	public := jsoncompat.NewObject()
	public.Set("accounts", &jsoncompat.Array{})
	public.Set("activeId", nil)
	switchState := jsoncompat.NewObject()
	switchState.Set("phase", "idle")
	public.Set("switch", switchState)
	public.Set("canSwitch", true)
	return public
}

// Server mirrors GatewayServer's request-facing behavior.
type Server struct {
	Auth            *auth.Auth
	WebFS           fs.FS
	InstanceID      string
	Port            int
	LocalHosts      map[string]bool // "127.0.0.1:port", "localhost:port"
	Hosts           map[string]bool
	Origins         map[string]bool
	Bridge          SessionBridge
	Pairing         *pairing.Pairing
	Store           *store.SessionStore
	Codex           *codex.Bridge
	Uploads         *uploads.Manager
	ACP             *acpruntime.Manager
	ACPAgents       map[string]*acpruntime.Manager
	NotificationDir string
	Jev             *jev.Settings
	Account         *account.Manager
	Goal            *goal.Manager
	historyMu       sync.Mutex
	historyTL       map[string]*timeline.Timeline
}

func New(authenticator *auth.Auth, webFS fs.FS, port int, instanceID string) *Server {
	server := &Server{Auth: authenticator, WebFS: webFS, Port: port, InstanceID: instanceID,
		LocalHosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Hosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Origins: map[string]bool{"http://127.0.0.1:" + strconv.Itoa(port): true,
			"http://localhost:" + strconv.Itoa(port): true},
		Pairing: pairing.New(nil), historyTL: map[string]*timeline.Timeline{}}
	server.Bridge = server
	return server
}

// AddListenHost extends the local gateway allowlist for a listener created by
// the process. Keeping this explicit prevents an arbitrary Host header from
// becoming an accepted origin when LAN mode is enabled.
func (s *Server) AddListenHost(host string, local bool) {
	hostPort := net.JoinHostPort(host, strconv.Itoa(s.Port))
	s.Hosts[hostPort] = true
	s.Origins["http://"+hostPort] = true
	if local {
		s.LocalHosts[hostPort] = true
	}
}

func (s *Server) ACPManager(kind string) *acpruntime.Manager {
	if kind != "" && s.ACPAgents != nil {
		return s.ACPAgents[kind]
	}
	return s.ACP
}

var acceptsGzip = regexp.MustCompile(`(?:^|,)\s*gzip\s*(?:;\s*q=([01](?:\.\d+)?))?\s*(?:,|$)`)

// permissionError / output mirror the handler contract.
type handler struct {
	server          *Server
	writer          http.ResponseWriter
	request         *http.Request
	closeConnection bool
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h := &handler{server: s, writer: writer, request: request}
	h.handleMethod(request.Method == http.MethodPost)
	if h.closeConnection {
		writer.Header().Set("Connection", "close")
	}
}

// output mirrors BaseHTTPRequestHandler.output: serialize with json.dumps
// defaults (", "/": "), shared security headers, optional gzip.
func (h *handler) output(status int, data any, contentType string, cookie string) {
	var body []byte
	if raw, ok := data.([]byte); ok {
		body = raw
	} else {
		body = jsoncompat.EncodePythonDefault(data)
	}
	compressible := strings.HasPrefix(contentType, "text/") ||
		strings.HasPrefix(contentType, "application/json") ||
		contentType == "image/svg+xml"
	header := h.writer.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Content-Type", contentType)
	header.Set("Vary", "Accept-Encoding")
	if compressible && len(body) >= 1024 && gzipAccepted(h.request) {
		compressed := gzipBytes(body)
		if len(compressed) < len(body) {
			body = compressed
			header.Set("Content-Encoding", "gzip")
		}
	}
	header.Set("Content-Length", strconv.Itoa(len(body)))
	if cookie != "" {
		header.Add("Set-Cookie", cookie)
	}
	h.writer.WriteHeader(status)
	_, _ = h.writer.Write(body)
}

func gzipAccepted(request *http.Request) bool {
	match := acceptsGzip.FindStringSubmatch(request.Header.Get("Accept-Encoding"))
	if match == nil {
		return false
	}
	if match[1] == "" {
		return true
	}
	quality, err := strconv.ParseFloat(match[1], 64)
	return err == nil && quality > 0
}

func gzipBytes(data []byte) []byte {
	var buffer sliceWriter
	compressor, _ := gzip.NewWriterLevel(&buffer, 5)
	_, _ = compressor.Write(data)
	_ = compressor.Close()
	return buffer.data
}

type sliceWriter struct{ data []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

// errorBody builds an ordered {"error": ...} body (Python dict literal order).
func errorBody(message string) *jsoncompat.Object {
	body := jsoncompat.NewObject()
	body.Set("error", message)
	return body
}

func (h *handler) fail(status int, body *jsoncompat.Object) {
	h.closeConnection = true
	h.output(status, body, contentTypeJSON, "")
}

func unauthenticatedBody() *jsoncompat.Object {
	body := jsoncompat.NewObject()
	body.Set("error", "请登录")
	body.Set("code", "unauthenticated")
	return body
}

func (h *handler) cookieToken() string {
	cookie, err := h.request.Cookie(auth.CookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (h *handler) session(write bool) *jsoncompat.Object {
	session := h.server.Auth.Get(h.cookieToken())
	if session == nil {
		h.fail(http.StatusUnauthorized, unauthenticatedBody())
		return nil
	}
	if write {
		expected, _ := session.MustGet("csrf").(string)
		if h.request.Header.Get("X-CSRF-Token") != expected {
			h.fail(http.StatusForbidden, errorBody("登录验证已失效，请刷新页面"))
			return nil
		}
	}
	return session
}

func (h *handler) checkRequest(write bool) bool {
	host := h.request.Host
	if !h.server.Hosts[host] {
		h.fail(http.StatusForbidden, errorBody("此访问地址未在网关配置中允许"))
		return false
	}
	origin := h.request.Header.Get("Origin")
	if (origin != "" && !h.server.Origins[origin]) || (write && origin == "") {
		h.fail(http.StatusForbidden, errorBody("不允许跨站请求"))
		return false
	}
	if h.request.Header.Get("Sec-Fetch-Site") == "cross-site" {
		h.fail(http.StatusForbidden, errorBody("不允许跨站请求"))
		return false
	}
	return true
}

type staticRoute struct {
	file        string
	contentType string
}

// staticRoutes mirrors bridge/httpd.py's STATIC table exactly, content types
// included: the pre-login shell needs every one of these without a session.
var staticRoutes = map[string]staticRoute{
	"/":                           {"index.html", "text/html; charset=utf-8"},
	"/i18n.js":                    {"i18n.js", "text/javascript; charset=utf-8"},
	"/app.js":                     {"app.js", "text/javascript; charset=utf-8"},
	"/modes.js":                   {"modes.js", "text/javascript; charset=utf-8"},
	"/attachments.js":             {"attachments.js", "text/javascript; charset=utf-8"},
	"/activity.js":                {"activity.js", "text/javascript; charset=utf-8"},
	"/fast-mode.js":               {"fast-mode.js", "text/javascript; charset=utf-8"},
	"/accounts.js":                {"accounts.js", "text/javascript; charset=utf-8"},
	"/migrate.js":                 {"migrate.js", "text/javascript; charset=utf-8"},
	"/account.js":                 {"account.js", "text/javascript; charset=utf-8"},
	"/account.css":                {"account.css", "text/css; charset=utf-8"},
	"/presentation.js":            {"presentation.js", "text/javascript; charset=utf-8"},
	"/presentation.css":           {"presentation.css", "text/css; charset=utf-8"},
	"/markdown.js":                {"markdown.js", "text/javascript; charset=utf-8"},
	"/message-actions.js":         {"message-actions.js", "text/javascript; charset=utf-8"},
	"/timeline.js":                {"timeline.js", "text/javascript; charset=utf-8"},
	"/transport.js":               {"transport.js", "text/javascript; charset=utf-8"},
	"/vendor/markdown-it.min.js":  {"vendor/markdown-it.min.js", "text/javascript; charset=utf-8"},
	"/vendor/texmath.js":          {"vendor/texmath.js", "text/javascript; charset=utf-8"},
	"/vendor/katex/katex.min.js":  {"vendor/katex/katex.min.js", "text/javascript; charset=utf-8"},
	"/vendor/katex/katex.min.css": {"vendor/katex/katex.min.css", "text/css; charset=utf-8"},
	"/style.css":                  {"style.css", "text/css; charset=utf-8"},
	"/manifest.webmanifest":       {"manifest.webmanifest", "application/manifest+json"},
	"/icon.png":                   {"icon.png", "image/png"},
}

func (h *handler) readJSON() (any, error) {
	body, err := io.ReadAll(io.LimitReader(h.request.Body, 512001))
	if err != nil {
		return nil, errJSON("需要 JSON 请求")
	}
	if len(body) == 0 || len(body) > 512000 {
		return nil, errJSON("请求过大或为空")
	}
	value, err := jsoncompat.Unmarshal(body)
	if err != nil {
		return nil, errJSON("请求内容格式不正确")
	}
	if _, isObject := value.(*jsoncompat.Object); !isObject {
		return nil, errJSON("请求内容必须为对象")
	}
	return value, nil
}

type errJSON string

func (e errJSON) Error() string { return string(e) }

func (h *handler) handleMethod(write bool) {
	if !write && h.request.URL.Path == "/api/health" {
		if h.request.Header.Get("Origin") != "" ||
			h.request.Header.Get("Sec-Fetch-Site") == "cross-site" ||
			!h.server.LocalHosts[h.request.Host] {
			h.fail(http.StatusForbidden, errorBody("仅允许本机状态检查"))
			return
		}
		body := jsoncompat.NewObject()
		body.Set("service", "codex-mobile-bridge")
		body.Set("instanceId", h.server.InstanceID)
		body.Set("notifications", false)
		h.output(http.StatusOK, body, contentTypeJSON, "")
		return
	}
	if !h.checkRequest(write) {
		return
	}
	path := h.request.URL.Path
	if !write {
		if route, ok := staticRoutes[path]; ok {
			if data, err := fs.ReadFile(h.server.WebFS, "web/"+route.file); err == nil {
				h.output(http.StatusOK, data, route.contentType, "")
				return
			}
		}
		if strings.HasPrefix(path, "/vendor/") {
			name := strings.TrimPrefix(path, "/")
			if name == pathpkg.Clean(name) && !strings.Contains(name, "..") {
				if data, err := fs.ReadFile(h.server.WebFS, "web/"+name); err == nil {
					contentType := "application/octet-stream"
					switch filepath.Ext(name) {
					case ".js":
						contentType = "text/javascript; charset=utf-8"
					case ".css":
						contentType = "text/css; charset=utf-8"
					case ".woff", ".woff2":
						contentType = "font/" + strings.TrimPrefix(filepath.Ext(name), ".")
					case ".ttf":
						contentType = "font/ttf"
					}
					h.output(http.StatusOK, data, contentType, "")
					return
				}
			}
		}
	}
	switch {
	case !write && path == "/api/auth":
		h.handleAuth()
	case write && path == "/api/login":
		h.handleLogin()
	case write && path == "/api/pair":
		h.handlePair()
	case write && path == "/api/logout":
		if h.session(write) == nil {
			return
		}
		h.server.Auth.Logout(h.cookieToken())
		result := jsoncompat.NewObject()
		result.Set("ok", true)
		h.output(http.StatusOK, result, contentTypeJSON, h.server.Auth.CookieValue(""))
	case !write && path == "/api/sessions":
		if h.session(write) == nil {
			return
		}
		result, err := h.sessionList(h.request.URL.Query())
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case write && path == "/api/sessions":
		if h.session(write) == nil {
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		body := value.(*jsoncompat.Object)
		agent, _ := body.MustGet("agent").(string)
		manager := h.server.ACPManager(agent)
		if manager == nil {
			if h.server.Codex == nil || (agent != "" && agent != "codex") {
				h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型或当前网关未启用创建服务"))
				return
			}
			cwd, _ := body.MustGet("cwd").(string)
			if cwd == "" {
				cwd, _ = body.MustGet("project").(string)
			}
			title, _ := body.MustGet("title").(string)
			if cwd == "" || strings.TrimSpace(title) == "" {
				h.fail(http.StatusBadRequest, errorBody("请选择项目并填写聊天名称"))
				return
			}
			id, createErr := h.server.Codex.CreateConversation(cwd, strings.TrimSpace(title))
			if createErr != nil {
				h.fail(http.StatusConflict, errorBody(createErr.Error()))
				return
			}
			result := jsoncompat.NewObject()
			result.Set("id", id)
			result.Set("title", strings.TrimSpace(title))
			result.Set("cwd", cwd)
			result.Set("host", "local")
			result.Set("agent", "codex")
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		cwd, _ := body.MustGet("cwd").(string)
		if cwd == "" {
			cwd, _ = body.MustGet("project").(string)
		}
		if cwd == "" {
			cwd = "."
		}
		id, _ := body.MustGet("id").(string)
		if id == "" {
			id = "sess_" + uuid.NewString()
		}
		title, _ := body.MustGet("title").(string)
		session, createErr := manager.Create(id, cwd, 60*time.Second)
		if createErr != nil {
			h.fail(http.StatusConflict, errorBody(createErr.Error()))
			return
		}
		result := jsoncompat.NewObject()
		result.Set("id", session.ID)
		result.Set("title", title)
		result.Set("cwd", cwd)
		result.Set("agent", "acp")
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case write && path == "/api/activity":
		if h.session(write) == nil {
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		groups, ok := value.(*jsoncompat.Object).MustGet("hosts").(*jsoncompat.Object)
		if !ok || groups.Len() > 20 {
			h.fail(http.StatusBadRequest, errorBody("会话状态请求无效"))
			return
		}
		rows := &jsoncompat.Array{}
		for _, host := range groups.Keys() {
			if host != "local" {
				continue
			}
			identifiers, ok := groups.MustGet(host).(*jsoncompat.Array)
			if !ok || len(identifiers.Items) > 500 {
				h.fail(http.StatusBadRequest, errorBody("会话状态请求无效"))
				return
			}
			ids := make([]string, 0, len(identifiers.Items))
			for _, item := range identifiers.Items {
				id, valid := item.(string)
				if !valid {
					h.fail(http.StatusBadRequest, errorBody("会话状态请求无效"))
					return
				}
				ids = append(ids, id)
			}
			if h.server.Codex != nil {
				for _, row := range h.server.Codex.Activity(ids) {
					rows.Items = append(rows.Items, row)
				}
			}
			for kind, manager := range h.server.ACPAgents {
				if manager == nil {
					continue
				}
				for _, id := range ids {
					if !strings.HasPrefix(id, "sess_") {
						continue
					}
					session := manager.Get(id)
					if session == nil {
						continue
					}
					row := jsoncompat.NewObject()
					row.Set("id", id)
					row.Set("host", "local")
					row.Set("agent", kind)
					row.Set("connected", true)
					row.Set("status", "idle")
					snapshot := session.Snapshot()
					if snapshot.MustGet("status") == "active" {
						row.Set("status", "active")
					}
					rows.Items = append(rows.Items, row)
				}
			}
		}
		result := jsoncompat.NewObject()
		result.Set("sessions", rows)
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case !write && path == "/api/projects":
		if h.session(write) == nil {
			return
		}
		h.output(http.StatusOK, h.projectList(), contentTypeJSON, "")
	case !write && path == "/api/accounts":
		if h.session(write) == nil {
			return
		}
		if h.server.Account != nil {
			h.output(http.StatusOK, h.server.Account.AccountsPublic(), contentTypeJSON, "")
		} else {
			h.output(http.StatusOK, h.server.Bridge.AccountsPublic(), contentTypeJSON, "")
		}
	case write && path == "/api/accounts/details":
		if h.session(write) == nil {
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		id, _ := value.(*jsoncompat.Object).MustGet("id").(string)
		if h.server.Account == nil {
			h.fail(http.StatusConflict, errorBody("请启动支持账号管理的新版网关"))
			return
		}
		row := h.server.Account.AccountDetails(id)
		if row == nil {
			h.fail(http.StatusNotFound, errorBody("账号不存在，请刷新列表"))
			return
		}
		h.output(http.StatusAccepted, row, contentTypeJSON, "")
	case write && path == "/api/accounts/ignore-submission":
		if h.session(write) == nil {
			return
		}
		if h.server.Account == nil {
			h.fail(http.StatusConflict, errorBody("请启动支持账号管理的新版网关"))
			return
		}
		h.output(http.StatusOK, h.server.Account.AccountsPublic(), contentTypeJSON, "")
	case write && path == "/api/accounts/switch":
		if h.session(write) == nil {
			return
		}
		h.fail(http.StatusConflict, errorBody("账号切换需要桌面端协调，请在电脑端操作"))
	case !write && path == "/api/account":
		if h.session(write) == nil {
			return
		}
		if h.server.Account == nil {
			h.output(http.StatusOK, stubAccount(), contentTypeJSON, "")
		} else {
			h.output(http.StatusOK, h.server.Account.Read(h.request.URL.Query().Get("cached") != "1"), contentTypeJSON, "")
		}
	case write && path == "/api/account/reset":
		if h.session(write) == nil {
			return
		}
		if h.server.Account == nil {
			h.fail(http.StatusConflict, errorBody("请启动支持账号管理的新版网关"))
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		result, consumeErr := h.server.Account.Consume(value.(*jsoncompat.Object))
		if consumeErr != nil {
			h.fail(http.StatusConflict, errorBody(consumeErr.Error()))
			return
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case path == "/api/notifications/defaults":
		if h.session(write) == nil {
			return
		}
		if h.server.NotificationDir == "" {
			h.fail(http.StatusBadRequest, errorBody("当前网关未启用通知服务"))
			return
		}
		if write {
			value, err := h.readJSON()
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return
			}
			result, saveErr := notifications.SaveSettings(h.server.NotificationDir, value.(*jsoncompat.Object))
			if saveErr != nil {
				h.fail(http.StatusBadRequest, errorBody(saveErr.Error()))
				return
			}
			h.output(http.StatusOK, result, contentTypeJSON, "")
		} else {
			h.output(http.StatusOK, notifications.ReadSettings(h.server.NotificationDir), contentTypeJSON, "")
		}
	case path == "/api/jev":
		if h.session(write) == nil {
			return
		}
		if h.server.Jev == nil {
			h.fail(http.StatusConflict, errorBody("当前网关未启用 Jev 设置"))
			return
		}
		if write {
			value, err := h.readJSON()
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return
			}
			result, updateErr := h.server.Jev.Update(value.(*jsoncompat.Object))
			if updateErr != nil {
				h.fail(http.StatusBadRequest, errorBody(updateErr.Error()))
				return
			}
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		h.output(http.StatusOK, h.server.Jev.Public(), contentTypeJSON, "")
	default:
		// The auth gate runs before every unmatched path (Python's
		// authorized() precedes all remaining routing).
		if h.session(write) == nil {
			return
		}
		query := map[string][]string(h.request.URL.Query())
		if h.handleGoalHTTP(write, path) {
			return
		}
		if h.handleUploadHTTP(write, path, query) {
			return
		}
		bridge, err := h.server.Bridge.ForHost(queryValue(query, "host", "local"))
		if err != nil {
			h.fail(http.StatusNotFound, errorBody("找不到这个会话"))
			return
		}
		if h.handleSessionRoutes(bridge, write, path, query) {
			return
		}
		h.fail(http.StatusNotFound, errorBody("页面不存在"))
	}
}

func (h *handler) sessionList(query map[string][]string) (*jsoncompat.Object, error) {
	text := queryValue(query, "q", "")
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:200])
	}
	offset, err := strconv.Atoi(queryValue(query, "offset", "0"))
	if err != nil || offset < 0 {
		return nil, errors.New("分页参数无效")
	}
	archived := queryValue(query, "archived", "false") == "true"
	sessions := &jsoncompat.Array{}
	agent := queryValue(query, "agent", "")
	if h.server.Store != nil && (agent == "" || agent == "codex") {
		rows, err := h.server.Store.List(text, 100, offset, archived)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, ok := row.Get("host"); !ok {
				row.Set("host", "local")
			}
			if _, ok := row.Get("agent"); !ok {
				row.Set("agent", "codex")
			}
			sessions.Items = append(sessions.Items, row)
		}
	}
	unavailable := &jsoncompat.Array{}
	acpAgents := h.server.ACPAgents
	if len(acpAgents) == 0 && h.server.ACP != nil {
		acpAgents = map[string]*acpruntime.Manager{"acp": h.server.ACP}
	}
	if !archived {
		for kind, manager := range acpAgents {
			if agent != "" && agent != kind {
				continue
			}
			rows, err := manager.List(15 * time.Second)
			if err != nil {
				unavailable.Items = append(unavailable.Items, err.Error())
			} else {
				for _, row := range rows {
					id, _ := row.MustGet("sessionId").(string)
					title, _ := row.MustGet("title").(string)
					cwd, _ := row.MustGet("cwd").(string)
					if id == "" || (text != "" && !strings.Contains(strings.ToLower(title+" "+cwd), strings.ToLower(text))) {
						continue
					}
					item := jsoncompat.NewObject()
					item.Set("id", id)
					item.Set("title", title)
					item.Set("name", title)
					item.Set("cwd", cwd)
					item.Set("host", "local")
					item.Set("hostLabel", "此电脑")
					item.Set("agent", kind)
					sessions.Items = append(sessions.Items, item)
				}
			}
		}
	}
	result := jsoncompat.NewObject()
	result.Set("sessions", sessions)
	result.Set("unavailableHosts", unavailable)
	return result, nil
}

func (h *handler) projectList() *jsoncompat.Object {
	projects := &jsoncompat.Array{}
	seen := map[string]bool{}
	if h.server.Store != nil {
		if rows, err := h.server.Store.List("", 200, 0, false); err == nil {
			for _, row := range rows {
				cwd, _ := row.MustGet("cwd").(string)
				if cwd == "" {
					continue
				}
				key := "codex:" + hashProject(cwd)
				if seen[key] {
					continue
				}
				seen[key] = true
				item := jsoncompat.NewObject()
				item.Set("key", key)
				item.Set("host", "local")
				item.Set("hostLabel", "此电脑")
				item.Set("name", filepath.Base(filepath.Clean(cwd)))
				item.Set("cwd", cwd)
				item.Set("agent", "codex")
				projects.Items = append(projects.Items, item)
			}
		}
	}
	acpAgents := h.server.ACPAgents
	if len(acpAgents) == 0 && h.server.ACP != nil {
		acpAgents = map[string]*acpruntime.Manager{"acp": h.server.ACP}
	}
	for kind, manager := range acpAgents {
		if rows, err := manager.List(15 * time.Second); err == nil {
			for _, row := range rows {
				cwd, _ := row.MustGet("cwd").(string)
				if cwd == "" {
					continue
				}
				key := "acp:" + hashProject(cwd)
				if seen[key] {
					continue
				}
				seen[key] = true
				item := jsoncompat.NewObject()
				item.Set("key", key)
				item.Set("host", "local")
				item.Set("hostLabel", "此电脑")
				item.Set("name", filepath.Base(filepath.Clean(cwd)))
				item.Set("cwd", cwd)
				item.Set("agent", kind)
				projects.Items = append(projects.Items, item)
			}
		}
	}
	result := jsoncompat.NewObject()
	result.Set("projects", projects)
	result.Set("agents", &jsoncompat.Array{})
	return result
}

func stubAccount() *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("visible", false)
	result.Set("loginType", "unknown")
	return result
}

func hashProject(cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	return hex.EncodeToString(sum[:])[:12]
}

// handlePair exchanges a one-time QR grant for a normal authenticated session.
// The grant registry is populated by the desktop control path; keeping the
// exchange here preserves the Python gateway's origin binding and cookie
// semantics without giving the phone a second authentication mechanism.
func (h *handler) handlePair() {
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	host := h.request.Host
	origin := "http://" + host
	if h.request.Header.Get("Origin") == "https://"+host {
		origin = "https://" + host
	}
	if h.request.Header.Get("Origin") != origin {
		h.fail(http.StatusForbidden, errorBody("不允许的请求来源"))
		return
	}
	token, _ := value.(*jsoncompat.Object).MustGet("token").(string)
	if h.server.Pairing == nil {
		h.fail(http.StatusForbidden, errorBody("二维码已失效或已使用，请在电脑上刷新二维码，或使用账号密码登录"))
		return
	}
	issued, err := h.server.Pairing.Exchange(token, origin, clientIP(h.request), float64(time.Now().Unix()), func(address string) any {
		sessionToken, session, issueErr := h.server.Auth.NewSession(address, h.request.UserAgent(), true)
		if issueErr != nil {
			return issueErr
		}
		return []any{sessionToken, session}
	})
	if err != nil {
		h.fail(http.StatusForbidden, errorBody(err.Error()))
		return
	}
	parts, ok := issued.([]any)
	if !ok || len(parts) != 2 {
		h.fail(http.StatusForbidden, errorBody("二维码已失效或已使用，请在电脑上刷新二维码，或使用账号密码登录"))
		return
	}
	sessionToken, _ := parts[0].(string)
	session, _ := parts[1].(*jsoncompat.Object)
	if session == nil {
		h.fail(http.StatusForbidden, errorBody("二维码已失效或已使用，请在电脑上刷新二维码，或使用账号密码登录"))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("csrf", session.MustGet("csrf"))
	h.output(http.StatusOK, result, contentTypeJSON, h.server.Auth.CookieValue(sessionToken))
}

func (h *handler) handleAuth() {
	token := h.cookieToken()
	session := h.server.Auth.Get(token)
	address := clientIP(h.request)
	body := jsoncompat.NewObject()
	body.Set("authenticated", session != nil)
	if session != nil {
		body.Set("csrf", session.MustGet("csrf"))
	} else {
		body.Set("csrf", nil)
	}
	body.Set("loginStatus", h.server.Auth.LoginStatus(address))
	body.Set("instanceId", h.server.InstanceID)
	body.Set("notifications", false)
	body.Set("passwordless", h.server.Auth.Config.Mode == "none")
	transport := "sse"
	if strings.HasSuffix(h.request.Host, ".trycloudflare.com") {
		transport = "poll"
	}
	body.Set("transport", transport)
	cookie := ""
	if session != nil {
		cookie = h.server.Auth.CookieValue(token)
	}
	h.output(http.StatusOK, body, contentTypeJSON, cookie)
}

func (h *handler) handleLogin() {
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	body := value.(*jsoncompat.Object)
	username, _ := body.MustGet("username").(string)
	password, _ := body.MustGet("password").(string)
	if len(username) > 200 || len(password) > 1000 {
		h.fail(http.StatusBadRequest, errorBody("账号或密码格式不正确"))
		return
	}
	var remember any
	if raw, ok := body.Get("remember"); ok {
		remember = raw
	}
	token, session, loginErr := h.server.Auth.Login(username, password, clientIP(h.request), remember)
	if loginErr != nil {
		rejected := loginErr.(*auth.LoginRejected)
		status := jsoncompat.NewObject()
		status.Set("error", rejected.Message)
		status.Set("loginStatus", rejected.Status)
		h.fail(http.StatusForbidden, status)
		return
	}
	// A fresh login replaces the caller's previous session.
	h.server.Auth.Logout(h.cookieToken())
	result := jsoncompat.NewObject()
	result.Set("csrf", session.MustGet("csrf"))
	h.output(http.StatusOK, result, contentTypeJSON, h.server.Auth.CookieValue(token))
}

func clientIP(request *http.Request) string {
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		return host
	}
	return request.RemoteAddr
}

var _ = json.Marshal
