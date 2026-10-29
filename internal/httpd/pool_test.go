package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// newPoolRouteServer builds a server with the account pool and a Codex
// account manager holding one saved account.
func newPoolRouteServer(t *testing.T) *Server {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	t.Setenv("ZCODE_HOME", t.TempDir())
	root := filepath.Join(dataDir, "accounts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	index := jsoncompat.NewObject()
	accounts := &jsoncompat.Array{}
	row := jsoncompat.NewObject()
	row.Set("id", "acc-1")
	row.Set("kind", "chatgpt")
	row.Set("name", "主账号")
	row.Set("email", "a@example.com")
	accounts.Items = append(accounts.Items, row)
	index.Set("accounts", accounts)
	index.Set("activeId", "acc-1")
	if err := os.WriteFile(filepath.Join(root, "index.json"), []byte(jsoncompat.EncodeCompact(index)), 0o600); err != nil {
		t.Fatal(err)
	}
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Account = account.NewWithData(home, dataDir, "")
	server.Pool = accpool.New(dataDir, "")
	return server
}

func poolRoute(t *testing.T, server *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, "http://127.0.0.1:8790"+path, reader)
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	token, session, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	if method == http.MethodPost {
		csrf, _ := session.MustGet("csrf").(string)
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestPoolOverviewIncludesReadOnlyCodex(t *testing.T) {
	server := newPoolRouteServer(t)
	response := poolRoute(t, server, http.MethodGet, "/api/pool", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	overview := jsonObject(t, response.Body.String())
	agents, _ := overview.MustGet("agents").(*jsoncompat.Array)
	if agents == nil {
		t.Fatalf("agents missing: %s", response.Body.String())
	}
	byKind := map[string]*jsoncompat.Object{}
	for _, value := range agents.Items {
		if row, ok := value.(*jsoncompat.Object); ok {
			if kind, _ := row.MustGet("kind").(string); kind != "" {
				byKind[kind] = row
			}
		}
	}
	for _, kind := range []string{"kimi", "zcode", "codex"} {
		if byKind[kind] == nil {
			t.Fatalf("overview missing %s: %s", kind, response.Body.String())
		}
	}
	codex := byKind["codex"]
	if readOnly, _ := codex.MustGet("readOnly").(bool); !readOnly {
		t.Fatalf("codex row must be read-only: %s", jsoncompat.EncodeCompact(codex))
	}
	if active, _ := codex.MustGet("activeId").(string); active != "acc-1" {
		t.Fatalf("codex activeId: %s", jsoncompat.EncodeCompact(codex))
	}
}

func TestPoolCodexDetailIsReadOnly(t *testing.T) {
	server := newPoolRouteServer(t)
	response := poolRoute(t, server, http.MethodGet, "/api/pool/codex", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	detail := jsonObject(t, response.Body.String())
	if readOnly, _ := detail.MustGet("readOnly").(bool); !readOnly {
		t.Fatalf("detail must be read-only: %s", response.Body.String())
	}
	accounts, _ := detail.MustGet("accounts").(*jsoncompat.Array)
	if accounts == nil || len(accounts.Items) != 1 {
		t.Fatalf("accounts: %s", response.Body.String())
	}

	// Writes are redirected to the dedicated Codex switch flow.
	for _, action := range []string{"switch", "settings", "refresh", "import", "capture", "remove"} {
		response = poolRoute(t, server, http.MethodPost, "/api/pool/codex/"+action, `{}`)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "Codex") {
			t.Fatalf("%s: status=%d body=%s", action, response.Code, response.Body.String())
		}
	}
}

func TestPoolCodexRequiresAccountManager(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Pool = accpool.New(t.TempDir(), "")
	response := poolRoute(t, server, http.MethodGet, "/api/pool/codex", "")
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	// The overview simply omits the codex row when the manager is absent.
	response = poolRoute(t, server, http.MethodGet, "/api/pool", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "codex") {
		t.Fatalf("overview must omit codex: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPoolRemoveRoute(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	t.Setenv("ZCODE_HOME", t.TempDir())
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "instance")
	server.Pool = accpool.New(dataDir, "")
	storeDir := filepath.Join(dataDir, "pool", "kimi")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "u_test1.json"), []byte(`{"id":"u_test1","refreshToken":"rt"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Missing id and missing confirmation are both rejected.
	response := poolRoute(t, server, http.MethodPost, "/api/pool/kimi/remove", `{}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("missing id: status=%d body=%s", response.Code, response.Body.String())
	}
	response = poolRoute(t, server, http.MethodPost, "/api/pool/kimi/remove", `{"id":"u_test1"}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed: status=%d body=%s", response.Code, response.Body.String())
	}

	// Confirmed remove drops the snapshot and returns the fresh detail.
	response = poolRoute(t, server, http.MethodPost, "/api/pool/kimi/remove", `{"id":"u_test1","confirmed":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("remove: status=%d body=%s", response.Code, response.Body.String())
	}
	detail := jsonObject(t, response.Body.String())
	accounts, _ := detail.MustGet("accounts").(*jsoncompat.Array)
	if accounts == nil || len(accounts.Items) != 0 {
		t.Fatalf("accounts after remove: %s", response.Body.String())
	}

	// Removing it again reports the missing snapshot.
	response = poolRoute(t, server, http.MethodPost, "/api/pool/kimi/remove", `{"id":"u_test1","confirmed":true}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("re-remove: status=%d body=%s", response.Code, response.Body.String())
	}
}

// WorkBuddy 快照里的积分汇总是 float64，而 jsoncompat 只编码整数：视图层没
// 取整时 encode 会 panic，连接被直接掐断，网页只能看到「Failed to fetch」。
func TestPoolWorkbuddyDetailRendersCredits(t *testing.T) {
	server := newPoolRouteServer(t)
	t.Setenv("WORKBUDDY_HOME", t.TempDir())
	storeDir := filepath.Join(server.Pool.DataDir, "pool", "workbuddy")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	account := `{"id":"w_test1","label":"测试账号","userId":"test1","accessToken":"at","refreshToken":"rt",` +
		`"variant":"cn","status":"active","createdAt":1793075725907,"lastUsedAt":1793075725907,` +
		`"quota":{"credits":{"totalCapacity":1100.5,"totalRemaining":400.25,"expiringSoonRemaining":12.5,` +
		`"resources":[{"packageCode":"pkg","packageName":"基础包","total":1000,"remaining":400,"used":600}]}}}`
	if err := os.WriteFile(filepath.Join(storeDir, "w_test1.json"), []byte(account), 0o600); err != nil {
		t.Fatal(err)
	}

	response := poolRoute(t, server, http.MethodGet, "/api/pool/workbuddy", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	detail := jsonObject(t, response.Body.String())
	accounts, _ := detail.MustGet("accounts").(*jsoncompat.Array)
	if accounts == nil || len(accounts.Items) != 1 {
		t.Fatalf("accounts: %s", response.Body.String())
	}
	row, _ := accounts.Items[0].(*jsoncompat.Object)
	quota, _ := row.MustGet("quota").(*jsoncompat.Object)
	credits, _ := quota.MustGet("credits").(*jsoncompat.Object)
	// 积分取整后写进响应，不是 1100.5 这样的浮点字面量。
	if got := jsoncompat.EncodeCompact(credits.MustGet("totalCapacity")); string(got) != "1101" {
		t.Fatalf("totalCapacity must be rounded to 1101: %s", got)
	}
	if got := jsoncompat.EncodeCompact(credits.MustGet("totalRemaining")); string(got) != "400" {
		t.Fatalf("totalRemaining must be rounded to 400: %s", got)
	}
	if !strings.Contains(response.Body.String(), "基础包") {
		t.Fatalf("resources missing: %s", response.Body.String())
	}
}

func TestPoolCaptureRouteWithoutCredentials(t *testing.T) {
	server := newPoolRouteServer(t)
	// KIMI_CODE_HOME points at an empty temp dir: no local login to capture.
	response := poolRoute(t, server, http.MethodPost, "/api/pool/kimi/capture", `{}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "凭据") {
		t.Fatalf("capture: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestPoolLoginRoutes(t *testing.T) {
	server := newPoolRouteServer(t)
	// Provider drivers hit the real network; stub kimi's with a flow that
	// stays pending until cancelled.
	restore := accpool.RegisterLoginDriverForTest("kimi", func(ctx context.Context, m *accpool.Manager, opts accpool.LoginOptions) (*accpool.LoginChallenge, func(ctx context.Context) (*accpool.Account, error), error) {
		return &accpool.LoginChallenge{UserCode: "ABCD-1234", URL: "https://example.com/device", ExpiresIn: 600},
			func(ctx context.Context) (*accpool.Account, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}, nil
	})
	defer restore()

	// Start → pending session plus the user-facing challenge.
	response := poolRoute(t, server, http.MethodPost, "/api/pool/kimi/login", `{}`)
	if response.Code != http.StatusOK {
		t.Fatalf("start: status=%d body=%s", response.Code, response.Body.String())
	}
	started := jsonObject(t, response.Body.String())
	if login, _ := started.MustGet("login").(*jsoncompat.Object); login == nil {
		t.Fatalf("start: login missing: %s", response.Body.String())
	} else if status, _ := login.MustGet("status").(string); status != "pending" {
		t.Fatalf("start: status=%s", status)
	}
	if challenge, _ := started.MustGet("challenge").(*jsoncompat.Object); challenge == nil {
		t.Fatalf("start: challenge missing: %s", response.Body.String())
	} else if code, _ := challenge.MustGet("userCode").(string); code != "ABCD-1234" {
		t.Fatalf("start: userCode=%s", code)
	}

	// Status polls the in-flight session.
	response = poolRoute(t, server, http.MethodGet, "/api/pool/kimi/login", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pending") {
		t.Fatalf("status: %d body=%s", response.Code, response.Body.String())
	}

	// Cancel → cancelled, and the cancelled snapshot stays readable.
	response = poolRoute(t, server, http.MethodPost, "/api/pool/kimi/login/cancel", `{}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "cancelled") {
		t.Fatalf("cancel: %d body=%s", response.Code, response.Body.String())
	}
	response = poolRoute(t, server, http.MethodGet, "/api/pool/kimi/login", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "cancelled") {
		t.Fatalf("status after cancel: %d body=%s", response.Code, response.Body.String())
	}

	// Codex stays read-only: login writes redirect to the switch flow.
	response = poolRoute(t, server, http.MethodPost, "/api/pool/codex/login", `{}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Codex") {
		t.Fatalf("codex login: status=%d body=%s", response.Code, response.Body.String())
	}
}
