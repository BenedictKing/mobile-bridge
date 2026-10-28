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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/conventions"
	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/pairing"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/google/uuid"
)

const contentTypeJSON = "application/json; charset=utf-8"

// contentSecurityPolicy is the shell CSP shared by every output() response
// and the SSE stream: frame-src 'self' admits the same-origin /files iframe
// previews while frame-ancestors 'none' still blocks third-party framing.
const contentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

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
	Auth             *auth.Auth
	Creations        *creations
	Queue            *submissions
	GoalCmds         *goalLedger
	Hidden           *hiddenSessions
	Forks            *forkLedger
	WebFS            fs.FS
	InstanceID       string
	Port             int
	LocalHosts       map[string]bool // "127.0.0.1:port", "localhost:port"
	Hosts            map[string]bool
	Origins          map[string]bool
	AllowHost        func(string) bool
	AllowOrigin      func(string) bool
	NativeCandidates func() []string
	Bridge           SessionBridge
	Pairing          *pairing.Pairing
	Store            *store.SessionStore
	Codex            *codex.Bridge
	CodexHome        string
	SecureHosts      map[string]bool // netloc of trusted HTTPS origins (secure_hosts)
	DirectByPublic   map[string]string
	PublicByDirect   map[string]string
	directGrants     *directGrantStore
	Uploads          *uploads.Manager
	ACP              *acpruntime.Manager
	ACPAgents        map[string]*acpruntime.Manager
	NotificationDir  string
	Jev              *jev.Settings
	Conventions      *conventions.Settings
	Account          *account.Manager
	Pool             *accpool.Manager
	Goal             *goal.Manager
	// Team 编排（docs/team-orchestration-plan.md §10）：服务、账本与每 Run
	// 事件投影由 main.go 装配（构造后直赋，必须在任何 ServeHTTP 之前完成）。
	Teams     *team.Service
	TeamStore team.TeamStore
	TeamFeeds *TeamFeedRegistry
	// TeamWorkspaceRegistry 是 /api/teams/workspaces 注册/列表端点的读写面
	//（§6.4 + §16 默认决策 6），与 Service.Workspaces 共用同一份
	// {dataDir}/team-workspaces.json；main.go 构造后直赋，未装配（nil）时
	// 端点报 409。
	TeamWorkspaceRegistry *TeamWorkspaceRegistry
	// TeamJournal 是 team-journal.jsonl 的只读出口（§12 阶段四：/stats 统计
	// 必须走 journal.Replay 全量事件——内存投影每 Run 只留最近 500 条，长
	// Run 的早期事件只在磁盘）。main.go 与 TeamStore 同源装配。
	TeamJournal *team.Journal
	// TeamSchedules 是 /api/teams/:id/schedule 配置端点的读写面（§12 阶段
	// 四工作项 1 的 HTTP 配置面），与定时触发器（team.Scheduler）共用同一份
	// {dataDir}/team-schedules.json；main.go 构造后直赋，未装配（nil）时
	// 端点报 409。
	TeamSchedules *team.ScheduleStore
	// teamSchedulePuts* 是 schedule PUT 的 requestId 幂等重放登记（照 Team
	// 创建先例：同 id 同内容 duplicate、异内容 400；schedule 的存储键是
	// teamID 而非 requestId，故在内存另记指纹，FIFO 封顶防无界增长）。
	teamSchedulePutsMu    sync.Mutex
	teamSchedulePuts      map[string]teamSchedulePutRecord
	teamSchedulePutsOrder []string
	// TeamChangesWait 是 changes 端点长轮询的等待上限（sessions.go 的
	// sessionRouteChanges 同款 12s）。仅测试用：生产保持零值走默认。
	TeamChangesWait time.Duration
	queueMu         sync.Mutex // serializes queue dispatch passes
	projectionsMu   sync.Mutex
	projections     map[string]*projectionSlot // per-thread timeline + lock
	artifactMu      sync.Mutex
	artifactCache   map[string]*artifactMemo
	desktopMu       sync.Mutex
	desktopPaths    map[string]map[string]string
	subagentOnce    sync.Once
	codexSubagents  *subagents.Flattener
	discovery       *listDiscovery
	// queueWorker / idleSweeper 是 Python maintain loop 的两半（3s 排队派发
	// 与 30s 空闲会话清扫）的启停句柄（§12 阶段四：补停止通道，关停序列
	// 可优雅退出）。main.go 在任何 ServeHTTP 之前 Start、HTTP 关停后 Stop，
	// 与 discovery 同款无锁装配惯例。
	queueWorker *maintainLoop
	idleSweeper *maintainLoop
}

func New(authenticator *auth.Auth, webFS fs.FS, port int, instanceID string) *Server {
	server := &Server{Auth: authenticator, WebFS: webFS, Port: port, InstanceID: instanceID,
		LocalHosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Hosts: map[string]bool{"127.0.0.1:" + strconv.Itoa(port): true,
			"localhost:" + strconv.Itoa(port): true},
		Origins: map[string]bool{"http://127.0.0.1:" + strconv.Itoa(port): true,
			"http://localhost:" + strconv.Itoa(port): true},
		SecureHosts:    map[string]bool{},
		DirectByPublic: map[string]string{}, PublicByDirect: map[string]string{},
		directGrants: newDirectGrantStore(),
		Pairing:      pairing.New(nil), projections: map[string]*projectionSlot{},
		artifactCache: map[string]*artifactMemo{}, desktopPaths: map[string]map[string]string{}}
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

// StartDiscovery 启动定时会话发现循环（listRevision 的来源）；重复调用无
// 副作用。发现只读本网关自己的原生来源，SSH 远端列表仍由显式请求驱动。
func (s *Server) StartDiscovery() {
	if s.discovery != nil {
		return
	}
	s.discovery = newListDiscovery(s)
	go s.discovery.run()
}

// StopDiscovery 停止定时会话发现循环。
func (s *Server) StopDiscovery() {
	if s.discovery != nil {
		s.discovery.close()
	}
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
	h.decorateDirectResponse()
	if h.handleDirectPreflight() {
		return
	}
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
	header.Set("Content-Security-Policy", h.contentSecurityPolicy())
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	header.Set("Content-Type", contentType)
	header.Add("Vary", "Accept-Encoding")
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
	session := h.server.Auth.GetForClient(h.cookieToken(), h.client(), h.request.UserAgent())
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
	if !h.server.Hosts[host] && (h.server.AllowHost == nil || !h.server.AllowHost(host)) {
		h.fail(http.StatusForbidden, errorBody("此访问地址未在网关配置中允许"))
		return false
	}
	origin := h.request.Header.Get("Origin")
	if (origin != "" && !h.server.Origins[origin] &&
		(h.server.AllowOrigin == nil || !h.server.AllowOrigin(origin))) || (write && origin == "") {
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
	"/pool.js":                    {"pool.js", "text/javascript; charset=utf-8"},
	"/team.js":                    {"team.js", "text/javascript; charset=utf-8"},
	"/migrate.js":                 {"migrate.js", "text/javascript; charset=utf-8"},
	"/account.js":                 {"account.js", "text/javascript; charset=utf-8"},
	"/account.css":                {"account.css", "text/css; charset=utf-8"},
	"/presentation.js":            {"presentation.js", "text/javascript; charset=utf-8"},
	"/presentation.css":           {"presentation.css", "text/css; charset=utf-8"},
	"/markdown.js":                {"markdown.js", "text/javascript; charset=utf-8"},
	"/image-viewer.js":            {"image-viewer.js", "text/javascript; charset=utf-8"},
	"/message-actions.js":         {"message-actions.js", "text/javascript; charset=utf-8"},
	"/timeline.js":                {"timeline.js", "text/javascript; charset=utf-8"},
	"/diff.js":                    {"diff.js", "text/javascript; charset=utf-8"},
	"/transport.js":               {"transport.js", "text/javascript; charset=utf-8"},
	"/connection.js":              {"connection.js", "text/javascript; charset=utf-8"},
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
	case write && path == "/api/direct/grant":
		if h.session(write) == nil {
			return
		}
		h.handleDirectGrant()
	case write && path == "/api/direct/exchange":
		h.handleDirectExchange()
	case write && path == "/api/logout":
		if h.session(write) == nil {
			return
		}
		h.server.Auth.Logout(h.cookieToken())
		result := jsoncompat.NewObject()
		result.Set("ok", true)
		h.output(http.StatusOK, result, contentTypeJSON, h.cookieValue(""))
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
		// Validation mirrors service.Bridge.create_chat: the request id is a
		// UUID, the title is 1–120 chars, the project must resolve to a real
		// directory (explicit cwd or a known project key).
		requestID, _ := body.MustGet("id").(string)
		if _, parseErr := uuid.Parse(requestID); parseErr != nil {
			h.fail(http.StatusBadRequest, errorBody("创建请求标识无效"))
			return
		}
		title, _ := body.MustGet("title").(string)
		title = strings.TrimSpace(title)
		if title == "" || len([]rune(title)) > 120 {
			h.fail(http.StatusBadRequest, errorBody("请输入 1–120 字的聊天名称"))
			return
		}
		project, _ := body.MustGet("project").(string)
		cwd, _ := body.MustGet("cwd").(string)
		if cwd == "" {
			cwd = h.resolveProjectCwd(body.MustGet("project"))
		}
		if cwd == "" {
			h.fail(http.StatusBadRequest, errorBody("请选择电脑 App 中已保存的项目"))
			return
		}
		if project == "" {
			project = cwd
		}
		kind := agent
		var manager *acpruntime.Manager
		if agent != "" && agent != "codex" {
			manager = h.server.acpAgentSet()[agent]
			if manager == nil {
				h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型或当前网关未启用创建服务"))
				return
			}
		} else if agent == "" && h.server.Codex == nil {
			// Legacy agent-less configuration: a single generic ACP manager
			// (GATEWAY_ACP_BIN) serves creation when codex is unavailable.
			manager = h.server.ACPManager("")
			kind = "acp"
		} else if h.server.Codex == nil {
			h.fail(http.StatusBadRequest, errorBody("未知的 Agent 类型或当前网关未启用创建服务"))
			return
		}
		// Idempotency ledger: the same phone request retried after a network
		// hiccup must return the same chat, never a duplicate.
		ledger := h.server.Creations
		if ledger != nil {
			if entry := ledger.lookup(requestID); entry != nil {
				if entry.Agent != agent || entry.Project != project || entry.Title != title {
					h.fail(http.StatusBadRequest, errorBody("同一创建请求不能用于不同内容"))
					return
				}
				if entry.ID == "" {
					h.fail(http.StatusConflict, errorBody("上次创建结果尚不确定，请先刷新聊天列表并检查电脑 App，避免重复创建"))
					return
				}
				h.output(http.StatusOK, h.createResponse(entry.ID, kind, title, cwd), contentTypeJSON, "")
				return
			}
			ledger.begin(requestID, &creationEntry{Agent: agent, Project: project, Title: title,
				Host: "local", At: ledger.now()})
		}
		created := ""
		if manager != nil {
			// ACP creation keeps the creating process as the session handle
			// (zcode's lazy placeholder can only be served by the process
			// that minted it).
			session, createErr := manager.Create("sess_"+uuid.NewString(), cwd, title, 60*time.Second)
			if createErr != nil {
				h.fail(http.StatusConflict, errorBody(createErr.Error()))
				return
			}
			created = session.ID
		} else {
			id, createErr := h.server.Codex.CreateConversation(cwd, title)
			if createErr != nil {
				h.fail(http.StatusConflict, errorBody(createErr.Error()))
				return
			}
			created = id
		}
		if ledger != nil {
			ledger.complete(requestID, created)
		}
		h.output(http.StatusOK, h.createResponse(created, kind, title, cwd), contentTypeJSON, "")
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
			// Split the ids by owning agent first (Python's registry split):
			// ACP-owned ids must not reach the Codex activity probe, whose
			// unknown-row shape would shadow the agent's real status.
			// Deleted (hidden) sessions never surface a dot again.
			visible := ids[:0]
			for _, id := range ids {
				if !h.server.hiddenSession(id) {
					visible = append(visible, id)
				}
			}
			ids = visible
			agents := h.server.acpAgentSet()
			owned, remaining := splitACPOwned(agents, ids)
			if h.server.Codex != nil {
				for _, row := range h.server.Codex.Activity(remaining) {
					rows.Items = append(rows.Items, row)
				}
			}
			for kind, manager := range agents {
				if manager == nil {
					continue
				}
				for _, id := range owned {
					if !manager.Knows(id) {
						continue
					}
					// Ownership follows the agent's roster, not an id prefix:
					// zcode placeholders are bare UUIDs until materialized.
					// Without a live handle the row reports offline (Python's
					// detached shape) so the phone clears stale dots instead
					// of resurrecting them.
					row := jsoncompat.NewObject()
					row.Set("id", id)
					row.Set("host", "local")
					row.Set("agent", kind)
					row.Set("connected", false)
					row.Set("status", "unknown")
					if session := manager.Get(id); session != nil {
						row.Set("connected", true)
						row.Set("status", "idle")
						snapshot := session.Snapshot()
						if snapshot.MustGet("status") == "active" {
							row.Set("status", "active")
						}
						if turns, ok := snapshot.MustGet("turns").(*jsoncompat.Array); ok && len(turns.Items) > 0 {
							if turn, ok := turns.Items[len(turns.Items)-1].(*jsoncompat.Object); ok {
								turnID, _ := turn.MustGet("id").(string)
								turnStatus, _ := turn.MustGet("status").(string)
								if turnID != "" {
									row.Set("turnId", turnID)
								}
								if turnStatus != "" {
									row.Set("turnStatus", turnStatus)
								}
							}
						}
						if requests, ok := snapshot.MustGet("requests").(*jsoncompat.Array); ok && len(requests.Items) > 0 {
							row.Set("pendingRequests", len(requests.Items))
						}
					}
					// Native-followed agents carry the real last-activity
					// time in their own store; include it so the list's
					// recency/time refreshes with the 5s activity poll
					// instead of waiting for a full list reload.
					if manager.NativeMeta != nil {
						if _, updated := manager.NativeMeta(id); updated > 0 {
							row.Set("recency", jsoncompat.Num(int(updated)))
						}
					}
					rows.Items = append(rows.Items, row)
				}
			}
		}
		result := jsoncompat.NewObject()
		result.Set("sessions", rows)
		if discovery := h.server.discovery; discovery != nil {
			discovery.touch()
			result.Set("listRevision", discovery.Revision())
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
	case !write && path == "/api/projects":
		if h.session(write) == nil {
			return
		}
		h.output(http.StatusOK, h.projectList(projectListLimit), contentTypeJSON, "")
	case !write && path == "/api/accounts":
		if h.session(write) == nil {
			return
		}
		if h.server.Account != nil {
			h.output(http.StatusOK, h.server.Account.AccountsPublic(), contentTypeJSON, "")
		} else {
			h.output(http.StatusOK, h.server.Bridge.AccountsPublic(), contentTypeJSON, "")
		}
	case !write && (path == "/api/pool" || strings.HasPrefix(path, "/api/pool/")):
		if h.session(write) == nil {
			return
		}
		h.handlePool(false)
	case write && strings.HasPrefix(path, "/api/pool/"):
		if h.session(write) == nil {
			return
		}
		h.handlePool(true)
	case path == "/api/teams" || strings.HasPrefix(path, "/api/teams/"):
		// Team 编排（§10）：PATCH/DELETE 是 web/team.js 契约使用的真实 HTTP
		// 方法，写判定与 CSRF 校验按「非 GET 即写」在 case 内重算（挂载点
		// 必须先于 default，否则被 Bridge.ForHost 拦成 404）。
		if h.session(h.request.Method != http.MethodGet) == nil {
			return
		}
		if h.server.Teams == nil || h.server.TeamStore == nil || h.server.TeamFeeds == nil {
			h.fail(http.StatusConflict, errorBody("请启动支持团队编排的新版网关"))
			return
		}
		h.handleTeams(h.request.Method)
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
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		body := value.(*jsoncompat.Object)
		// The phone hides an uncertain submission outcome from the queue
		// cards; the ledger owns that decision, the account panel only
		// carries the request.
		if h.server.Queue != nil {
			threadID, _ := body.MustGet("threadId").(string)
			submissionID, _ := body.MustGet("submissionId").(string)
			if err := h.server.Queue.ignore(threadID, submissionID); err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return
			}
		} else if h.server.Account == nil {
			h.fail(http.StatusConflict, errorBody("请启动支持账号管理的新版网关"))
			return
		}
		if h.server.Account != nil {
			h.output(http.StatusOK, h.server.Account.AccountsPublic(), contentTypeJSON, "")
		} else {
			h.output(http.StatusOK, h.server.Bridge.AccountsPublic(), contentTypeJSON, "")
		}
	case write && path == "/api/accounts/switch":
		if h.session(write) == nil {
			return
		}
		if h.server.Account == nil {
			h.fail(http.StatusConflict, errorBody("账号切换需要桌面端协调，请在电脑端操作"))
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		// The panel's 恢复原接入 control rides the same route with
		// action:"recover" (Python accounts.switch dispatches it the same
		// way): it replays rollback.json after an interrupted switch.
		if action, _ := value.(*jsoncompat.Object).MustGet("action").(string); action == "recover" {
			result, recoverErr := h.server.Account.Recover(value.(*jsoncompat.Object))
			if recoverErr != nil {
				h.fail(http.StatusBadRequest, errorBody(recoverErr.Error()))
				return
			}
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		// Manager.Switch validates the target, refuses a second concurrent
		// switch (409) or a blocked desktop (400), and drives the restart in
		// the background; the response is the fresh panel status either way.
		result, switchErr := h.server.Account.Switch(value.(*jsoncompat.Object), h.server.accountIdleGate())
		if switchErr != nil {
			var conflict *account.ConflictError
			if errors.As(switchErr, &conflict) {
				h.fail(http.StatusConflict, errorBody(switchErr.Error()))
			} else {
				h.fail(http.StatusBadRequest, errorBody(switchErr.Error()))
			}
			return
		}
		h.output(http.StatusOK, result, contentTypeJSON, "")
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
	case write && path == "/api/jev/trial":
		if h.session(write) == nil {
			return
		}
		if h.server.Jev == nil {
			h.fail(http.StatusConflict, errorBody("当前网关未启用 Jev 设置"))
			return
		}
		value, err := h.readJSON()
		if err != nil {
			h.fail(http.StatusBadRequest, errorBody(err.Error()))
			return
		}
		body := value.(*jsoncompat.Object)
		// Two shapes: a raw {method, params} pair, or a bare command the
		// gateway wraps into a simulated permission card (options included
		// so the response mapping matches the live chain).
		method := ""
		if raw, exists := body.Get("method"); exists {
			if text, isString := raw.(string); isString {
				method = strings.TrimSpace(text)
			}
		}
		var params *jsoncompat.Object
		if method != "" {
			params, _ = body.MustGet("params").(*jsoncompat.Object)
			if params == nil {
				h.fail(http.StatusBadRequest, errorBody("params 格式不正确"))
				return
			}
		} else if raw, exists := body.Get("command"); exists {
			text, isString := raw.(string)
			if !isString {
				h.fail(http.StatusBadRequest, errorBody("请提供试评命令或完整请求"))
				return
			}
			command := strings.TrimSpace(text)
			if command == "" {
				h.fail(http.StatusBadRequest, errorBody("试评命令不能为空"))
				return
			}
			method = "session/request_permission"
			params = jsoncompat.NewObject()
			params.Set("command", command)
			allow := jsoncompat.NewObject()
			allow.Set("optionId", "allow_once")
			allow.Set("kind", "allow_once")
			reject := jsoncompat.NewObject()
			reject.Set("optionId", "reject")
			reject.Set("kind", "reject_once")
			params.Set("options", &jsoncompat.Array{Items: []any{allow, reject}})
		} else {
			h.fail(http.StatusBadRequest, errorBody("请提供试评命令或完整请求"))
			return
		}
		decision, err := h.server.Jev.TrialEvaluate(method, params)
		if err != nil {
			h.fail(http.StatusConflict, errorBody(err.Error()))
			return
		}
		h.output(http.StatusOK, jev.DecisionPayload(decision), contentTypeJSON, "")
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
	case path == "/api/conventions":
		if h.session(write) == nil {
			return
		}
		if h.server.Conventions == nil {
			h.fail(http.StatusConflict, errorBody("当前网关未启用结果可见设置"))
			return
		}
		if write {
			value, err := h.readJSON()
			if err != nil {
				h.fail(http.StatusBadRequest, errorBody(err.Error()))
				return
			}
			result, updateErr := h.server.Conventions.Update(value.(*jsoncompat.Object))
			if updateErr != nil {
				h.fail(http.StatusBadRequest, errorBody(updateErr.Error()))
				return
			}
			h.output(http.StatusOK, result, contentTypeJSON, "")
			return
		}
		h.output(http.StatusOK, h.server.Conventions.Public(), contentTypeJSON, "")
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

// splitACPOwned partitions ids by ACP ownership: agent-owned ids and the
// remainder routed to codex.
func splitACPOwned(agents map[string]*acpruntime.Manager, ids []string) (owned, remaining []string) {
	ownedSet := map[string]bool{}
	for _, id := range ids {
		for _, manager := range agents {
			if manager != nil && manager.Knows(id) {
				ownedSet[id] = true
				break
			}
		}
	}
	owned = []string{}
	remaining = []string{}
	for _, id := range ids {
		if ownedSet[id] {
			owned = append(owned, id)
		} else {
			remaining = append(remaining, id)
		}
	}
	return owned, remaining
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
		rows, err := h.server.Store.List(text, 100+offset, 0, archived)
		if err != nil {
			return nil, err
		}
		projects := readLocalProjects(h.server.CodexHome)
		for _, row := range rows {
			if id, _ := row.MustGet("id").(string); h.server.hiddenSession(id) {
				continue
			}
			if _, ok := row.Get("host"); !ok {
				row.Set("host", "local")
			}
			if _, ok := row.Get("agent"); !ok {
				row.Set("agent", "codex")
			}
			decorateCodexRow(projects, row)
			row.Set("connected", false)
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
				entry := jsoncompat.NewObject()
				entry.Set("host", "local")
				entry.Set("label", "此电脑")
				entry.Set("error", err.Error())
				unavailable.Items = append(unavailable.Items, entry)
			} else {
				// Merge sessions created here that the agent's own list does
				// not cover yet (zcode's lazy placeholder before the first
				// prompt) so a fresh chat is visible immediately.
				rows = manager.MergePending(rows)
				capabilities := &jsoncompat.Array{}
				for _, capability := range manager.Capabilities {
					capabilities.Items = append(capabilities.Items, capability)
				}
				for _, row := range rows {
					id, _ := row.MustGet("sessionId").(string)
					title, _ := row.MustGet("title").(string)
					cwd, _ := row.MustGet("cwd").(string)
					if id == "" || h.server.hiddenSession(id) || (text != "" && !strings.Contains(strings.ToLower(title+" "+cwd), strings.ToLower(text))) {
						continue
					}
					if title == "" {
						title = "未命名聊天"
					}
					item := jsoncompat.NewObject()
					item.Set("id", id)
					item.Set("title", title)
					item.Set("name", title)
					item.Set("cwd", cwd)
					item.Set("host", "local")
					item.Set("hostLabel", "此电脑")
					item.Set("agent", kind)
					item.Set("capabilities", capabilities)
					projectName := projectBaseName(cwd)
					if projectName == "" {
						projectName = "未归类"
					}
					projectKey := cwd
					if projectKey == "" {
						projectKey = "unassigned"
					}
					item.Set("projectKey", "local|"+projectKey)
					item.Set("projectName", projectName)
					item.Set("recency", acpRecencyOf(row))
					item.Set("connected", false)
					sessions.Items = append(sessions.Items, item)
				}
			}
		}
	}
	sortSessionRows(sessions.Items)
	// Python slices the merged, sorted rows: one page of 100 from offset.
	if offset >= len(sessions.Items) {
		sessions.Items = nil
	} else {
		sessions.Items = sessions.Items[offset:]
		if len(sessions.Items) > 100 {
			sessions.Items = sessions.Items[:100]
		}
	}
	result := jsoncompat.NewObject()
	result.Set("sessions", sessions)
	result.Set("unavailableHosts", unavailable)
	return result, nil
}

// createResponse builds the POST /api/sessions reply: for codex the desktop
// App takes over execution via its codex:// scheme — without it the phone
// cannot attach to the new thread.
func (h *handler) createResponse(id, kind, title, cwd string) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("id", id)
	result.Set("title", title)
	result.Set("cwd", cwd)
	result.Set("host", "local")
	if kind == "" || kind == "codex" {
		result.Set("agent", "codex")
		if codex.OpenInDesktop(id) == nil {
			result.Set("opened", true)
			result.Set("message", "已创建，正在连接桌面 App")
		} else {
			result.Set("opened", false)
			result.Set("message", "聊天已创建，请在电脑 App 打开后重新连接")
		}
		return result
	}
	result.Set("agent", kind)
	result.Set("opened", true)
	result.Set("message", "已创建，正在连接桌面 App")
	return result
}

// resolveProjectCwd maps a /api/projects key ("codex:<hash>", "acp:<hash>")
// back to its working directory, mirroring Python's project resolution before
// create_chat. Unknown keys resolve to "" so callers reject them.
func (h *handler) resolveProjectCwd(key any) string {
	text, _ := key.(string)
	if text == "" {
		return ""
	}
	projects, _ := h.projectList(0).MustGet("projects").(*jsoncompat.Array)
	if projects == nil {
		return ""
	}
	for _, item := range projects.Items {
		project, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if candidate, _ := project.MustGet("key").(string); candidate == text {
			cwd, _ := project.MustGet("cwd").(string)
			return cwd
		}
	}
	return ""
}

// projectListLimit caps how many projects /api/projects returns. Internal
// callers (resolveProjectCwd, migration targets) pass 0 for the full list so
// projects that aged out of the recent window still resolve by key.
const projectListLimit = 30

type projectEntry struct {
	item    *jsoncompat.Object
	recency int64
}

// rowMillis reads the first available timestamp from a session list row as
// milliseconds; second-precision fields are scaled up.
func rowMillis(row *jsoncompat.Object, fields ...string) int64 {
	for _, field := range fields {
		number, ok := row.MustGet(field).(json.Number)
		if !ok {
			continue
		}
		value, err := number.Int64()
		if err != nil || value <= 0 {
			continue
		}
		if !strings.HasSuffix(field, "_ms") && field != "updatedAt" {
			value *= 1000
		}
		return value
	}
	return 0
}

func newProjectEntry(key, cwd, agent string, recency int64) *projectEntry {
	item := jsoncompat.NewObject()
	item.Set("key", key)
	item.Set("host", "local")
	item.Set("hostLabel", "此电脑")
	item.Set("name", filepath.Base(filepath.Clean(cwd)))
	item.Set("cwd", cwd)
	item.Set("agent", agent)
	return &projectEntry{item: item, recency: recency}
}

func (h *handler) projectList(limit int) *jsoncompat.Object {
	entries := []*projectEntry{}
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
				entries = append(entries, newProjectEntry(key, cwd, "codex", rowMillis(row, "updated_at_ms", "updated_at", "recency_at_ms", "recency_at")))
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
				entries = append(entries, newProjectEntry(key, cwd, kind, rowMillis(row, "updatedAt")))
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].recency > entries[j].recency })
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	projects := &jsoncompat.Array{}
	for _, entry := range entries {
		projects.Items = append(projects.Items, entry.item)
	}
	result := jsoncompat.NewObject()
	result.Set("projects", projects)
	// The new-chat agent picker is capability-driven: with no ACP agents
	// configured the roster stays empty (single-agent codex UX); otherwise it
	// mirrors Python's registry.kinds() — every enabled agent plus codex.
	result.Set("agents", h.server.agentRoster())
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
		sessionToken, session, issueErr := h.server.Auth.NewSessionWithClient(h.client(), h.request.UserAgent(), true)
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
	h.output(http.StatusOK, result, contentTypeJSON, h.cookieValue(sessionToken))
}

func (h *handler) handleAuth() {
	token := h.cookieToken()
	session := h.server.Auth.GetForClient(token, h.client(), h.request.UserAgent())
	address, _ := h.client().MustGet("ip").(string)
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
	if directOrigin := h.directCandidate(); directOrigin != "" {
		body.Set("direct", directOrigin)
	}
	if session != nil && h.request.Header.Get("X-Mobile-Bridge-Native") == "1" && h.server.NativeCandidates != nil {
		candidates := &jsoncompat.Array{}
		for _, candidate := range h.server.NativeCandidates() {
			candidates.Items = append(candidates.Items, candidate)
		}
		body.Set("nativeDirect", candidates)
	}
	cookie := ""
	if session != nil {
		cookie = h.cookieValue(token)
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
	token, session, loginErr := h.server.Auth.LoginClient(username, password, h.client(), h.request.UserAgent(), remember)
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
	h.output(http.StatusOK, result, contentTypeJSON, h.cookieValue(token))
}

func clientIP(request *http.Request) string {
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		return host
	}
	return request.RemoteAddr
}

// client mirrors httpd.py's client(): the resolved client record, trusting
// forwarded IPs only on a secure HTTPS-tunnel host via a trusted proxy.
func (h *handler) client() *jsoncompat.Object {
	secure := h.server.SecureHosts[h.request.Host]
	return h.server.Auth.Client(clientIP(h.request), h.request.Host,
		h.request.Header.Get("X-Forwarded-For"),
		h.request.Header.Get("CF-Connecting-IP"), secure)
}

// cookieValue mirrors httpd.py's cookie(): the Secure attribute rides along
// when the Host is an HTTPS tunnel origin. A direct session is intentionally
// cross-origin from the public page, so its HostOnly cookie uses SameSite=None.
func (h *handler) cookieValue(token string) string {
	value := h.server.Auth.CookieValueForHost(token, h.server.SecureHosts[h.request.Host])
	if h.directPublicOrigin() != "" {
		value = strings.Replace(value, "SameSite=Strict", "SameSite=None", 1)
	}
	return value
}

var _ = json.Marshal
