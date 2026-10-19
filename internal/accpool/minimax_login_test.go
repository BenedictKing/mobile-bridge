package accpool

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// minimaxLoginRecorder collects incoming stub requests for assertions.
type minimaxLoginRecorder struct {
	mu       sync.Mutex
	requests []minimaxLoginRequest
}

type minimaxLoginRequest struct {
	Path   string
	Form   url.Values
	Header http.Header
}

func (r *minimaxLoginRecorder) record(req *http.Request) minimaxLoginRequest {
	_ = req.ParseForm()
	entry := minimaxLoginRequest{Path: req.URL.Path, Form: req.PostForm, Header: req.Header.Clone()}
	r.mu.Lock()
	r.requests = append(r.requests, entry)
	r.mu.Unlock()
	return entry
}

func (r *minimaxLoginRecorder) take() []minimaxLoginRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]minimaxLoginRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

// stubMinimaxLoginOrigins points the account/matrix origins at the given
// per-region stub servers (the minimax_login tests only need those two maps).
func stubMinimaxLoginOrigins(t *testing.T, cn, en string) {
	t.Helper()
	restoreAccount := minimaxAccountOrigins
	restoreMatrix := minimaxMatrixOrigins
	minimaxAccountOrigins = map[string]string{"cn": cn, "en": en}
	minimaxMatrixOrigins = map[string]string{"cn": cn, "en": en}
	t.Cleanup(func() {
		minimaxAccountOrigins = restoreAccount
		minimaxMatrixOrigins = restoreMatrix
	})
}

// shrinkMinimaxSlowDown keeps slow_down tests from sleeping the production 5s.
func shrinkMinimaxSlowDown(t *testing.T) {
	t.Helper()
	restore := minimaxSlowDownStep
	minimaxSlowDownStep = 50 * time.Millisecond
	t.Cleanup(func() { minimaxSlowDownStep = restore })
}

// minimaxLoginStub serves the three device-flow endpoints; each handler can
// be nil (404).
type minimaxLoginStub struct {
	rec          *minimaxLoginRecorder
	onDeviceCode func(http.ResponseWriter)
	onToken      func(http.ResponseWriter)
	onUserInfo   func(http.ResponseWriter)
}

func (s *minimaxLoginStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/oauth2/device/code":
			s.rec.record(req)
			if s.onDeviceCode != nil {
				s.onDeviceCode(w)
				return
			}
		case "/oauth2/token":
			s.rec.record(req)
			if s.onToken != nil {
				s.onToken(w)
				return
			}
		case "/v1/api/user/info":
			s.rec.record(req)
			if s.onUserInfo != nil {
				s.onUserInfo(w)
				return
			}
		}
		http.NotFound(w, req)
	})
}

// popJSONBody returns a handler serving the queued JSON bodies in order,
// repeating the last one once the queue is drained.
func popJSONBody(bodies ...string) func(http.ResponseWriter) {
	queue := append([]string(nil), bodies...)
	var mu sync.Mutex
	return func(w http.ResponseWriter) {
		mu.Lock()
		body := queue[0]
		if len(queue) > 1 {
			queue = queue[1:]
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

const minimaxTestUserInfo = `{"data":{"userInfo":{"realUserID":"u-max-1","userName":"Max","userEmail":"max@example.com"}}}`

func serveUserInfoWithAuthCheck(t *testing.T, rec *minimaxLoginRecorder, token string) func(http.ResponseWriter) {
	t.Helper()
	return func(w http.ResponseWriter) {
		requests := rec.take()
		last := requests[len(requests)-1]
		if got := last.Header.Get("authorization"); got != "Bearer "+token {
			t.Errorf("user/info authorization = %q", got)
		}
		for _, header := range []string{"yy", "x-timestamp", "x-signature"} {
			if last.Header.Get(header) == "" {
				t.Errorf("user/info missing signature header %q", header)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(minimaxTestUserInfo))
	}
}

// TestMinimaxLoginStandardVariant drives the RFC 8628 shape end to end:
// pending → slow_down → success, asserting the PKCE S256 form parameters and
// that the returned candidate is seconds-dated and not persisted.
func TestMinimaxLoginStandardVariant(t *testing.T) {
	m := newTestManager(t)
	shrinkMinimaxSlowDown(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"ABCD-EFGH",
			"verification_uri":"https://account.minimax.cn/device",
			"verification_uri_complete":"https://account.minimax.cn/device?user_code=ABCD-EFGH",
			"expires_in":900,"interval":1}`),
		onToken: popJSONBody(
			`{"status":"pending"}`,
			`{"status":"slow_down"}`,
			`{"access_token":"at-new","refresh_token":"rt-new","expires_in":7200,"token_type":"Bearer"}`,
		),
	}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubMinimaxLoginOrigins(t, server.URL, server.URL)
	stub.onUserInfo = serveUserInfoWithAuthCheck(t, rec, "at-new")

	challenge, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{Provider: "cn"})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "ABCD-EFGH" {
		t.Fatalf("userCode = %q", challenge.UserCode)
	}
	if challenge.URL != "https://account.minimax.cn/device?user_code=ABCD-EFGH" {
		t.Fatalf("url = %q", challenge.URL)
	}
	if challenge.ExpiresIn != 900 {
		t.Fatalf("expiresIn = %d", challenge.ExpiresIn)
	}

	started := time.Now().Unix()
	account, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "at-new" || account.RefreshToken != "rt-new" {
		t.Fatalf("tokens: %+v", account)
	}
	if account.TokenType != "Bearer" || account.Scope != minimaxScopes {
		t.Fatalf("token meta: %+v", account)
	}
	if account.ExpiresIn != 7200 {
		t.Fatalf("expiresIn = %d", account.ExpiresIn)
	}
	if account.ExpiresAt < started+7200 || account.ExpiresAt > started+7210 {
		t.Fatalf("expiresAt must be Unix seconds: %d (started %d)", account.ExpiresAt, started)
	}
	if account.Region != "cn" {
		t.Fatalf("region = %q", account.Region)
	}
	if account.UserID != "u-max-1" || account.Nickname != "Max" || account.Email != "max@example.com" {
		t.Fatalf("identity: %+v", account)
	}
	if n := len(m.list("minimax")); n != 0 {
		t.Fatalf("candidate must not be persisted by the driver, pool has %d", n)
	}

	requests := rec.take()
	if len(requests) != 5 { // device/code + 3 polls + user/info
		t.Fatalf("requests = %d", len(requests))
	}
	start := requests[0]
	if start.Path != "/oauth2/device/code" {
		t.Fatalf("start path = %q", start.Path)
	}
	if got := start.Form.Get("client_id"); got != "mcode-public" {
		t.Fatalf("client_id = %q", got)
	}
	if got := start.Form.Get("scope"); got != "agent.default" {
		t.Fatalf("scope = %q", got)
	}
	if got := start.Form.Get("audience"); got != "agent-backend" {
		t.Fatalf("audience = %q", got)
	}
	if got := start.Form.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q", got)
	}
	challengeB64 := start.Form.Get("code_challenge")
	if challengeB64 == "" {
		t.Fatal("code_challenge empty")
	}
	for i, poll := range requests[1:4] {
		if poll.Path != "/oauth2/token" {
			t.Fatalf("poll %d path = %q", i, poll.Path)
		}
		if got := poll.Form.Get("grant_type"); got != minimaxDeviceGrantType {
			t.Fatalf("poll %d grant_type = %q", i, got)
		}
		if got := poll.Form.Get("device_code"); got != "dc-1" {
			t.Fatalf("poll %d device_code = %q", i, got)
		}
		if got := poll.Form.Get("user_code"); got != "" {
			t.Fatalf("poll %d must not send user_code, got %q", i, got)
		}
	}
	verifier := requests[1].Form.Get("code_verifier")
	if verifier == "" {
		t.Fatal("code_verifier empty")
	}
	sum := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != challengeB64 {
		t.Fatalf("code_challenge != S256(verifier): %q vs %q", challengeB64, got)
	}
	if requests[4].Path != "/v1/api/user/info" {
		t.Fatalf("identity path = %q", requests[4].Path)
	}
}

// TestMinimaxLoginAccountVariant drives the account shape (no device_code,
// expired_in, millisecond interval, poll with user_code) on the en region.
func TestMinimaxLoginAccountVariant(t *testing.T) {
	m := newTestManager(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"user_code":"WXYZ-9876",
			"verification_uri":"https://account.minimax.io/device",
			"expired_in":600,"interval":100}`),
		onToken: popJSONBody(
			`{"status":"pending"}`,
			`{"access_token":"at-en","refresh_token":"rt-en","expires_in":3600}`,
		),
	}
	enServer := httptest.NewServer(stub.handler())
	defer enServer.Close()
	cnServer := httptest.NewServer(http.NotFoundHandler())
	defer cnServer.Close()
	stubMinimaxLoginOrigins(t, cnServer.URL, enServer.URL)
	stub.onUserInfo = serveUserInfoWithAuthCheck(t, rec, "at-en")

	challenge, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{Provider: "en"})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.UserCode != "WXYZ-9876" || challenge.ExpiresIn != 600 {
		t.Fatalf("challenge: %+v", challenge)
	}

	account, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "at-en" || account.Region != "en" {
		t.Fatalf("candidate: %+v", account)
	}
	requests := rec.take()
	if len(requests) != 4 {
		t.Fatalf("requests = %d", len(requests))
	}
	poll := requests[1]
	if got := poll.Form.Get("user_code"); got != "WXYZ-9876" {
		t.Fatalf("poll user_code = %q", got)
	}
	if got := poll.Form.Get("device_code"); got != "" {
		t.Fatalf("account variant must not send device_code, got %q", got)
	}
	if got := poll.Form.Get("code_verifier"); got == "" {
		t.Fatal("code_verifier empty")
	}
}

// TestMinimaxLoginRegionRouting asserts the en provider hits the en origins
// and an unknown/empty provider falls back to cn.
func TestMinimaxLoginRegionRouting(t *testing.T) {
	newServer := func(t *testing.T, rec *minimaxLoginRecorder) *httptest.Server {
		t.Helper()
		stub := &minimaxLoginStub{
			rec: rec,
			onDeviceCode: popJSONBody(`{"device_code":"dc","user_code":"CODE",
				"verification_uri":"https://x/device","expires_in":900,"interval":1}`),
		}
		server := httptest.NewServer(stub.handler())
		t.Cleanup(server.Close)
		return server
	}
	cnRec, enRec := &minimaxLoginRecorder{}, &minimaxLoginRecorder{}
	cnServer := newServer(t, cnRec)
	enServer := newServer(t, enRec)
	restoreAccount := minimaxAccountOrigins
	minimaxAccountOrigins = map[string]string{"cn": cnServer.URL, "en": enServer.URL}
	t.Cleanup(func() { minimaxAccountOrigins = restoreAccount })

	m := newTestManager(t)
	for _, provider := range []string{"en"} {
		if _, _, err := startMinimaxLogin(context.Background(), m, LoginOptions{Provider: provider}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(enRec.take()); n != 1 {
		t.Fatalf("en server got %d requests, want 1", n)
	}
	if n := len(cnRec.take()); n != 0 {
		t.Fatalf("cn server must not be hit for provider=en, got %d", n)
	}

	// 空 / 非法 Provider 回退 cn。
	for _, provider := range []string{"", "xx"} {
		if _, _, err := startMinimaxLogin(context.Background(), m, LoginOptions{Provider: provider}); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(cnRec.take()); n != 2 {
		t.Fatalf("cn server got %d requests, want 2", n)
	}
	if n := len(enRec.take()); n != 1 {
		t.Fatalf("en server got %d requests, want 1", n)
	}
}

// TestMinimaxLoginDenied covers both denial shapes: the status field and the
// standard error field.
func TestMinimaxLoginDenied(t *testing.T) {
	cases := []struct {
		name  string
		token string
	}{
		{"status field", `{"status":"denied"}`},
		{"error field", `{"error":"access_denied"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t)
			rec := &minimaxLoginRecorder{}
			stub := &minimaxLoginStub{
				rec: rec,
				onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"CODE",
					"verification_uri":"https://x/device","expires_in":900,"interval":1}`),
				onToken: func(w http.ResponseWriter) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.token))
				},
			}
			server := httptest.NewServer(stub.handler())
			defer server.Close()
			stubMinimaxLoginOrigins(t, server.URL, server.URL)

			_, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := wait(context.Background()); err == nil || err.Error() != "授权已被拒绝" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// TestMinimaxLoginUnknownError asserts the generic error-field failure shape.
func TestMinimaxLoginUnknownError(t *testing.T) {
	m := newTestManager(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"CODE",
			"verification_uri":"https://x/device","expires_in":900,"interval":1}`),
		onToken: func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad verifier"}`))
		},
	}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubMinimaxLoginOrigins(t, server.URL, server.URL)

	_, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || err.Error() != "MiniMax 登录失败: invalid_grant（bad verifier）" {
		t.Fatalf("err = %v", err)
	}
}

// TestMinimaxLoginTimeout: the wait loop gives up once the challenge expires.
func TestMinimaxLoginTimeout(t *testing.T) {
	m := newTestManager(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"CODE",
			"verification_uri":"https://x/device","expires_in":1,"interval":1}`),
		onToken: popJSONBody(`{"status":"pending"}`),
	}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubMinimaxLoginOrigins(t, server.URL, server.URL)

	challenge, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.ExpiresIn != 1 {
		t.Fatalf("expiresIn = %d", challenge.ExpiresIn)
	}
	before := time.Now()
	if _, err := wait(context.Background()); err == nil || err.Error() != "登录超时（验证码已过期），请重试" {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(before); elapsed < time.Second {
		t.Fatalf("timeout returned too early: %s", elapsed)
	}
}

// TestMinimaxLoginContextCancel: cancelling the wait context surfaces ctx.Err.
func TestMinimaxLoginContextCancel(t *testing.T) {
	m := newTestManager(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"CODE",
			"verification_uri":"https://x/device","expires_in":900,"interval":1}`),
		onToken: popJSONBody(`{"status":"pending"}`),
	}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubMinimaxLoginOrigins(t, server.URL, server.URL)

	_, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()
	if _, err := wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// TestMinimaxLoginTransportErrors: three consecutive network failures abort
// the poll with a Chinese error.
func TestMinimaxLoginTransportErrors(t *testing.T) {
	m := newTestManager(t)
	rec := &minimaxLoginRecorder{}
	stub := &minimaxLoginStub{
		rec: rec,
		onDeviceCode: popJSONBody(`{"device_code":"dc-1","user_code":"CODE",
			"verification_uri":"https://x/device","expires_in":900,"interval":1}`),
		onToken: func(w http.ResponseWriter) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("response writer cannot hijack")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		},
	}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubMinimaxLoginOrigins(t, server.URL, server.URL)

	_, wait, err := startMinimaxLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || !strings.Contains(err.Error(), "轮询登录状态失败（网络）") {
		t.Fatalf("err = %v", err)
	}
	polls := 0
	for _, request := range rec.take() {
		if request.Path == "/oauth2/token" {
			polls++
		}
	}
	if polls != minimaxMaxTransportErrors {
		t.Fatalf("poll attempts = %d, want %d", polls, minimaxMaxTransportErrors)
	}
}
