// Package httpd is the Go port of bridge/httpd.py's request pipeline:
// host/origin validation, the auth + CSRF gate, static assets, JSON output
// with Python-default separators, and the error-mapping contract. Business
// routes are stubbed for the P3 conformance gate; the P0 differential golden
// (conformance/http) is the acceptance test for both implementations.
package httpd

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/pairing"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/timeline"
)

const contentTypeJSON = "application/json; charset=utf-8"

// Bridge supplies the business responses behind the routes (stubbed in P3).
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
	Auth       *auth.Auth
	WebFS      fs.FS
	InstanceID string
	Port       int
	LocalHosts map[string]bool // "127.0.0.1:port", "localhost:port"
	Hosts      map[string]bool
	Origins    map[string]bool
	Bridge     SessionBridge
	Pairing    *pairing.Pairing
	Store      *store.SessionStore
	historyMu  sync.Mutex
	historyTL  map[string]*timeline.Timeline
}

func New(authenticator *auth.Auth, webFS fs.FS, port int, instanceID string) *Server {
	server := &Server{Auth: authenticator, WebFS: webFS, Port: port, InstanceID: instanceID,
		LocalHosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Hosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Origins: map[string]bool{"http://127.0.0.1:" + strconv.Itoa(port): true,
			"http://localhost:" + strconv.Itoa(port): true},
		Bridge: newScriptedSessionBridge(), Pairing: pairing.New(nil), historyTL: map[string]*timeline.Timeline{}}
	return server
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

var staticRoutes = map[string]string{ // path -> file name under web/ (bridge/httpd.py STATIC)
	"/": "index.html", "/i18n.js": "i18n.js", "/app.js": "app.js", "/modes.js": "modes.js",
	"/attachments.js": "attachments.js", "/activity.js": "activity.js",
	"/fast-mode.js": "fast-mode.js", "/accounts.js": "accounts.js",
	"/timeline.js": "timeline.js", "/markdown.js": "markdown.js",
	"/transport.js": "transport.js", "/presentation.js": "presentation.js",
	"/message-actions.js": "message-actions.js", "/migrate.js": "migrate.js",
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
		if name, ok := staticRoutes[path]; ok {
			if data, err := fs.ReadFile(h.server.WebFS, "web/"+name); err == nil {
				contentType := "text/html; charset=utf-8"
				if strings.HasSuffix(name, ".js") {
					contentType = "text/javascript; charset=utf-8"
				}
				h.output(http.StatusOK, data, contentType, "")
				return
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
		if h.server.Store != nil {
			result, err := h.sessionList(h.request.URL.Query())
			if err != nil {
				h.fail(http.StatusConflict, errorBody(err.Error()))
				return
			}
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		h.output(http.StatusOK, h.server.Bridge.SessionsList(), contentTypeJSON, "")
	case !write && path == "/api/accounts":
		if h.session(write) == nil {
			return
		}
		h.output(http.StatusOK, h.server.Bridge.AccountsPublic(), contentTypeJSON, "")
	default:
		// The auth gate runs before every unmatched path (Python's
		// authorized() precedes all remaining routing).
		if h.session(write) == nil {
			return
		}
		query := map[string][]string(h.request.URL.Query())
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
	rows, err := h.server.Store.List(text, 100, offset, archived)
	if err != nil {
		return nil, err
	}
	sessions := &jsoncompat.Array{}
	for _, row := range rows {
		if _, ok := row.Get("host"); !ok {
			row.Set("host", "local")
		}
		if _, ok := row.Get("agent"); !ok {
			row.Set("agent", "codex")
		}
		sessions.Items = append(sessions.Items, row)
	}
	result := jsoncompat.NewObject()
	result.Set("sessions", sessions)
	result.Set("unavailableHosts", &jsoncompat.Array{})
	return result, nil
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
