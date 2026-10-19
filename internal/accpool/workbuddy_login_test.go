package accpool

// Gateway scan-login tests for the workbuddy driver: an httptest stub stands
// in for the two per-variant endpoints (workbuddyEndpoints is repointed at
// it), the poll interval / overall budget are shrunk, and the candidate is
// asserted without touching the pool or the desktop slot.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// workbuddyScanStub records the scan-login HTTP exchange and lets each test
// script the token endpoint per poll round.
type workbuddyScanStub struct {
	mu       sync.Mutex
	platform string
	tokenURL string // last token request URL (query included)
	tokens   int    // token endpoint hits
	stateErr int    // HTTP status for the state request (0 = 200)
	profile  map[string]any
	// onToken returns the HTTP status + body for the n-th token poll.
	onToken func(call int) (int, map[string]any)
}

func (s *workbuddyScanStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			s.platform = r.URL.Query().Get("platform")
			if s.stateErr != 0 {
				writeWorkbuddyStubJSON(w, s.stateErr, map[string]any{"code": s.stateErr, "message": "state failed"})
				return
			}
			writeWorkbuddyStubJSON(w, 200, map[string]any{"code": 0, "data": map[string]any{
				"state": "state-1", "authUrl": "https://example.com/scan?state=state-1",
			}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			s.tokens++
			s.tokenURL = r.URL.String()
			status, body := 500, map[string]any{"code": 500, "message": "boom"}
			if s.onToken != nil {
				status, body = s.onToken(s.tokens)
			}
			writeWorkbuddyStubJSON(w, status, body)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			if s.profile == nil {
				writeWorkbuddyStubJSON(w, 500, map[string]any{"code": 500, "message": "no profile"})
				return
			}
			writeWorkbuddyStubJSON(w, 200, map[string]any{"code": 0, "data": s.profile})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			writeWorkbuddyStubJSON(w, 404, map[string]any{"code": 404})
		}
	}
}

func (s *workbuddyScanStub) tokenCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens
}

func writeWorkbuddyStubJSON(w http.ResponseWriter, status int, body map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	raw, _ := json.Marshal(body)
	_, _ = w.Write(raw)
}

// workbuddyScanTestEnv points both variant endpoints at a fresh stub, shrinks
// the poll cadence, and returns the manager + stub.
func workbuddyScanTestEnv(t *testing.T) (*Manager, *workbuddyScanStub) {
	t.Helper()
	stub := &workbuddyScanStub{
		profile: map[string]any{
			"uid": "u1", "nickname": "小明", "email": "a@b.c",
			"enterpriseId": "e1", "enterpriseName": "Acme",
		},
	}
	server := httptest.NewServer(stub.handler(t))
	t.Cleanup(server.Close)
	savedEndpoints := workbuddyEndpoints
	workbuddyEndpoints = map[string]string{"cn": server.URL, "ai": server.URL}
	t.Cleanup(func() { workbuddyEndpoints = savedEndpoints })
	savedPoll := workbuddyScanPollInterval
	workbuddyScanPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { workbuddyScanPollInterval = savedPoll })
	return loginTestManager(t), stub
}

func TestWorkbuddyScanLoginSuccess(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	nowMS := m.now().UnixMilli()
	stub.onToken = func(call int) (int, map[string]any) {
		if call < 3 {
			return 200, map[string]any{"code": 4001, "message": "not scanned"} // 未扫：继续
		}
		return 200, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-1", "refreshToken": "rt-1", "tokenType": "Bearer",
			"domain": "codebuddy.cn", "expiresAt": nowMS + 3600_000, "refreshExpiresIn": 7200,
		}}
	}

	challenge, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.URL != "https://example.com/scan?state=state-1" || challenge.ExpiresIn != 600 {
		t.Fatalf("challenge = %+v", challenge)
	}
	stub.mu.Lock()
	if stub.platform != "workbuddy" {
		t.Fatalf("platform = %q", stub.platform)
	}
	stub.mu.Unlock()

	candidate, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls := stub.tokenCalls(); calls != 3 {
		t.Fatalf("token polls = %d, want 3", calls)
	}
	if candidate.ID != "w_u1" || candidate.UserID != "u1" || candidate.Nickname != "小明" || candidate.Email != "a@b.c" ||
		candidate.EnterpriseID != "e1" || candidate.EnterpriseName != "Acme" {
		t.Fatalf("identity fields = %+v", candidate)
	}
	if candidate.Variant != "cn" || candidate.AccessToken != "at-1" || candidate.RefreshToken != "rt-1" ||
		candidate.TokenType != "Bearer" || candidate.Domain != "codebuddy.cn" {
		t.Fatalf("credential fields = %+v", candidate)
	}
	if candidate.ExpiresAt != nowMS+3600_000 {
		t.Fatalf("ExpiresAt = %d", candidate.ExpiresAt)
	}
	if candidate.RefreshExpiresAt != nowMS+7200_000 { // expiresIn 秒 → 毫秒
		t.Fatalf("RefreshExpiresAt = %d", candidate.RefreshExpiresAt)
	}
	// candidate 不落盘：池里和桌面槽位都不许有东西。
	if n := len(m.list("workbuddy")); n != 0 {
		t.Fatalf("login wait persisted %d accounts", n)
	}
}

func TestWorkbuddyScanLoginAIVariant(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	stub.onToken = func(call int) (int, map[string]any) {
		return 200, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-ai", "refreshToken": "rt-ai",
			"domain": "tencent.workbuddy.ai", "expiresIn": 3600,
		}}
	}
	challenge, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{Provider: "ai"})
	if err != nil {
		t.Fatal(err)
	}
	stub.mu.Lock()
	if stub.platform != "workbuddy-ai" {
		t.Fatalf("platform = %q", stub.platform)
	}
	stub.mu.Unlock()
	candidate, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Variant != "ai" || candidate.AccessToken != "at-ai" {
		t.Fatalf("candidate = %+v", candidate)
	}
	if challenge.ExpiresIn != 600 {
		t.Fatalf("challenge = %+v", challenge)
	}
}

func TestWorkbuddyScanLoginRejectsWrongVariant(t *testing.T) {
	m, _ := workbuddyScanTestEnv(t)
	if _, _, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{Provider: "us"}); err == nil {
		t.Fatal("invalid variant must fail at start")
	}
}

func TestWorkbuddyScanLoginDomainMismatch(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	stub.onToken = func(call int) (int, map[string]any) {
		return 200, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-x", "domain": "evil.workbuddy.ai",
		}}
	}
	_, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "档位不符") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkbuddyScanLoginStartFailure(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	stub.stateErr = 500
	if _, _, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{}); err == nil {
		t.Fatal("state request failure must fail the start")
	}
}

func TestWorkbuddyScanLoginHTTPErrorTimesOut(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	m.now = time.Now // 真实时钟：deadline 才会推进
	savedWait := workbuddyScanMaxWait
	workbuddyScanMaxWait = 60 * time.Millisecond
	t.Cleanup(func() { workbuddyScanMaxWait = savedWait })
	stub.onToken = func(call int) (int, map[string]any) {
		return 500, map[string]any{"code": 500, "message": "boom"} // 永远未扫
	}
	_, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("err = %v", err)
	}
	if stub.tokenCalls() == 0 {
		t.Fatal("never polled the token endpoint")
	}
}

func TestWorkbuddyScanLoginCtxCancel(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	stub.onToken = func(call int) (int, map[string]any) {
		return 200, map[string]any{"code": 4001} // 一直未扫
	}
	_, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = wait(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkbuddyScanLoginProfileBestEffort(t *testing.T) {
	m, stub := workbuddyScanTestEnv(t)
	stub.profile = nil // profile 接口挂掉：登录仍应成功，只是没有身份字段
	stub.onToken = func(call int) (int, map[string]any) {
		return 200, map[string]any{"code": 0, "data": map[string]any{
			"accessToken": "at-1", "refreshToken": "rt-1", "domain": "codebuddy.cn",
		}}
	}
	_, wait, err := startWorkbuddyLogin(context.Background(), m, LoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if candidate.UserID != "" || candidate.ID != "" {
		t.Fatalf("identity should be empty without profile: %+v", candidate)
	}
	if candidate.AccessToken != "at-1" || candidate.RefreshToken != "rt-1" {
		t.Fatalf("tokens lost: %+v", candidate)
	}
}
