package accpool

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// login_test.go asserts kimi answers 暂不支持网关内登录 unless a test
// registers a fake driver, and registers/deletes fakes around each case. The
// real driver from init() would hit the network in TestLoginUnknownKind, so
// drop the registry entry for the package run; the tests below exercise
// startKimiLogin directly.
func TestMain(m *testing.M) {
	delete(loginDrivers, "kimi")
	os.Exit(m.Run())
}

// stubKimiAuthServer points the kimi endpoint package variables at a stub
// server and restores them on cleanup.
func stubKimiEndpoints(t *testing.T, server *httptest.Server) {
	t.Helper()
	oldAuth, oldToken, oldAPI := kimiDeviceAuthURL, kimiTokenURL, kimiAPIBase
	kimiDeviceAuthURL = server.URL + "/api/oauth/device_authorization"
	kimiTokenURL = server.URL + "/api/oauth/token"
	kimiAPIBase = server.URL + "/coding/v1"
	t.Cleanup(func() {
		kimiDeviceAuthURL, kimiTokenURL, kimiAPIBase = oldAuth, oldToken, oldAPI
	})
}

func TestKimiDeviceLoginFullFlow(t *testing.T) {
	var mu sync.Mutex
	var tokenCalls int
	var tokenForms []map[string]string
	var tokenDeviceIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			if r.Method != http.MethodPost {
				t.Errorf("device_authorization method = %s", r.Method)
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			if got := r.PostForm.Get("client_id"); got != kimiClientID {
				t.Errorf("device_authorization client_id = %q", got)
			}
			assertKimiDeviceHeaders(t, r)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"device_code":"dc-123","user_code":"ABCD-EFGH","verification_uri":"https://auth.kimi.com/activate","verification_uri_complete":"https://auth.kimi.com/activate?user_code=ABCD-EFGH","interval":1,"expires_in":900}`)
		case "/api/oauth/token":
			mu.Lock()
			tokenCalls++
			call := tokenCalls
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			tokenForms = append(tokenForms, map[string]string{
				"grant_type":  r.PostForm.Get("grant_type"),
				"device_code": r.PostForm.Get("device_code"),
				"client_id":   r.PostForm.Get("client_id"),
			})
			tokenDeviceIDs = append(tokenDeviceIDs, r.Header.Get("X-Msh-Device-Id"))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			switch call {
			case 1:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
			case 2:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"slow_down"}`)
			default:
				fmt.Fprint(w, `{"access_token":"at-1","refresh_token":"rt-1","token_type":"Bearer","scope":"coding","expires_in":7200}`)
			}
		case "/coding/v1/me":
			if auth := r.Header.Get("Authorization"); auth != "Bearer at-1" {
				t.Errorf("me Authorization = %q", auth)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"user_id":"u-42","nickname":"小明","email":"ming@example.com","avatar":"http://a","user_level_name":"Pro","region":"cn"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	challenge, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "ABCD-EFGH" {
		t.Fatalf("UserCode = %q", challenge.UserCode)
	}
	if challenge.URL != "https://auth.kimi.com/activate?user_code=ABCD-EFGH" {
		t.Fatalf("URL should prefer verification_uri_complete, got %q", challenge.URL)
	}
	if challenge.ExpiresIn != 900 {
		t.Fatalf("ExpiresIn = %d", challenge.ExpiresIn)
	}

	account, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if tokenCalls != 3 {
		t.Fatalf("token calls = %d, want 3", tokenCalls)
	}
	for i, form := range tokenForms {
		if form["grant_type"] != kimiDeviceGrantType {
			t.Errorf("poll %d grant_type = %q", i, form["grant_type"])
		}
		if form["device_code"] != "dc-123" {
			t.Errorf("poll %d device_code = %q", i, form["device_code"])
		}
		if form["client_id"] != kimiClientID {
			t.Errorf("poll %d client_id = %q", i, form["client_id"])
		}
		if tokenDeviceIDs[i] == "" {
			t.Errorf("poll %d missing X-Msh-Device-Id", i)
		}
		if i > 0 && tokenDeviceIDs[i] != tokenDeviceIDs[0] {
			t.Errorf("poll %d device id %q != first %q", i, tokenDeviceIDs[i], tokenDeviceIDs[0])
		}
	}
	if account.AccessToken != "at-1" || account.RefreshToken != "rt-1" {
		t.Fatalf("tokens = %+v", account)
	}
	if account.TokenType != "Bearer" || account.Scope != "coding" {
		t.Fatalf("token meta = %+v", account)
	}
	if account.ExpiresIn != 7200 {
		t.Fatalf("ExpiresIn = %d", account.ExpiresIn)
	}
	if want := time.Now().Unix() + 7200; account.ExpiresAt < want-5 || account.ExpiresAt > want+5 {
		t.Fatalf("ExpiresAt = %d, want ~%d (seconds)", account.ExpiresAt, want)
	}
	if account.DeviceID == "" || account.DeviceID != tokenDeviceIDs[0] {
		t.Fatalf("DeviceID = %q, polls saw %q", account.DeviceID, tokenDeviceIDs[0])
	}
	if account.UserID != "u-42" || account.Nickname != "小明" || account.Email != "ming@example.com" ||
		account.Avatar != "http://a" || account.LevelName != "Pro" || account.Region != "cn" {
		t.Fatalf("identity = %+v", account)
	}
	if n := len(mgr.list("kimi")); n != 0 {
		t.Fatalf("login wait must not persist accounts, pool has %d", n)
	}
}

// assertKimiDeviceHeaders checks the full X-Msh-* device identity header set.
func assertKimiDeviceHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	checks := map[string]string{
		"X-Msh-Platform":     "mobile-bridge",
		"X-Msh-Version":      kimiClientVersion,
		"X-Msh-Device-Name":  "",
		"X-Msh-Device-Model": "",
		"X-Msh-Os-Version":   "",
		"X-Msh-Device-Id":    "",
	}
	for header, want := range checks {
		got := r.Header.Get(header)
		if got == "" {
			t.Errorf("missing header %s", header)
		} else if want != "" && got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestKimiDeviceLoginAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":1,"expires_in":900}`)
		case "/api/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"access_denied"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || err.Error() != "授权已被拒绝" {
		t.Fatalf("err = %v", err)
	}
}

func TestKimiDeviceLoginExpiredToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":1,"expires_in":900}`)
		case "/api/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"expired_token"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || err.Error() != "验证码已过期，请重试" {
		t.Fatalf("err = %v", err)
	}
}

func TestKimiDeviceAuthNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, _, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err == nil || !strings.Contains(err.Error(), "Kimi device flow 发起失败（HTTP 500）") {
		t.Fatalf("err = %v", err)
	}
}

func TestKimiDeviceLoginCtxCancel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":30,"expires_in":900}`)
		case "/api/oauth/token":
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"authorization_pending"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	// 轮询进行中取消：第一次请求落地后取消，wait 应立即返回。
	ctx2, cancel2 := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		account, err := wait(ctx2)
		if account != nil {
			done <- fmt.Errorf("wait returned account on cancel")
			return
		}
		done <- err
	}()
	time.Sleep(1500 * time.Millisecond)
	cancel2()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after cancel")
	}
}

func TestKimiDeviceLoginTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no HTTP expected on expired deadline, got %s", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	// 已过期的 deadline：第一轮循环即超时，不应发任何请求。
	deadline := time.Now().Add(-time.Second)
	_, err := pollKimiDeviceLogin(context.Background(), "dc", "dev", 1, deadline)
	if err == nil || err.Error() != "登录超时（验证码已过期），请重试" {
		t.Fatalf("err = %v", err)
	}
}

func TestKimiDeviceLoginTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":1,"expires_in":900}`)
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	// 只让 token 端点不可达：起一个服务器拿到 URL 后立即关闭。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/api/oauth/token"
	dead.Close()
	kimiTokenURL = deadURL

	mgr := loginTestManager(t)
	_, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "轮询登录状态失败（网络）") {
		t.Fatalf("err = %v", err)
	}
}

func TestKimiDeviceIDPersistedAndReused(t *testing.T) {
	mgr := loginTestManager(t)

	first, err := ensureKimiDeviceID(mgr)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("device id %q is not a UUID: %v", first, err)
	}
	path := filepath.Join(mgr.storeDir("kimi"), kimiDeviceIDFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("device-id mode = %o", info.Mode().Perm())
	}

	second, err := ensureKimiDeviceID(mgr)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("device id not reused: %q vs %q", first, second)
	}

	// 预置文件时 startKimiLogin 必须复用它（header 断言在 stub 里做）。
	var sawDeviceID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDeviceID = r.Header.Get("X-Msh-Device-Id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":1,"expires_in":900}`)
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	if _, _, err := startKimiLogin(context.Background(), mgr, LoginOptions{}); err != nil {
		t.Fatal(err)
	}
	if sawDeviceID != first {
		t.Fatalf("stub saw device id %q, want persisted %q", sawDeviceID, first)
	}
}

// TestKimiDeviceAuthMissingFields 覆盖 device_authorization 响应缺字段的失败。
func TestKimiDeviceAuthMissingFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"device_code":"dc"}`)
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, _, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err == nil || !strings.Contains(err.Error(), "缺少 device_code/user_code") {
		t.Fatalf("err = %v", err)
	}
}

// TestKimiDeviceLoginBadJSON 覆盖 token 端点返回非 JSON 异常文本的失败分支。
func TestKimiDeviceLoginBadJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/oauth/device_authorization":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"device_code":"dc","user_code":"CODE","verification_uri":"https://auth.kimi.com/activate","interval":1,"expires_in":900}`)
		case "/api/oauth/token":
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, "upstream exploded")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	stubKimiEndpoints(t, server)

	mgr := loginTestManager(t)
	_, wait, err := startKimiLogin(context.Background(), mgr, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || !strings.Contains(err.Error(), "Kimi token 端点返回异常（HTTP 502）") {
		t.Fatalf("err = %v", err)
	}
}
