package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
)

func directTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	if err := server.AddDirect("https://bridge.example", "https://lan.bridge.example:8788"); err != nil {
		t.Fatal(err)
	}
	token, session, err := authenticator.NewSession("127.0.0.1", "", true)
	if err != nil {
		t.Fatal(err)
	}
	return server, token, session.MustGet("csrf").(string)
}

func directRequest(t *testing.T, server *Server, method, target, origin, token, csrf string, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestDirectGrantExchangeAndSingleUse(t *testing.T) {
	server, token, csrf := directTestServer(t)
	authResponse := directRequest(t, server, http.MethodGet, "https://bridge.example/api/auth", "https://bridge.example", token, "", "")
	if authResponse.Code != http.StatusOK || !strings.Contains(authResponse.Body.String(), `"direct": "https://lan.bridge.example:8788"`) {
		t.Fatalf("direct candidate missing: status=%d body=%s", authResponse.Code, authResponse.Body.String())
	}
	grantResponse := directRequest(t, server, http.MethodPost, "https://bridge.example/api/direct/grant", "https://bridge.example", token, csrf, `{}`)
	if grantResponse.Code != http.StatusOK {
		t.Fatalf("grant status=%d body=%s", grantResponse.Code, grantResponse.Body.String())
	}
	var grant struct{ Token string }
	if err := json.Unmarshal(grantResponse.Body.Bytes(), &grant); err != nil || len(grant.Token) != 43 {
		t.Fatalf("grant=%q err=%v", grant.Token, err)
	}
	exchange := func() *httptest.ResponseRecorder {
		return directRequest(t, server, http.MethodPost, "https://lan.bridge.example:8788/api/direct/exchange", "https://bridge.example", "", "", `{"token":"`+grant.Token+`"}`)
	}
	exchanged := exchange()
	if exchanged.Code != http.StatusOK {
		t.Fatalf("exchange status=%d body=%s", exchanged.Code, exchanged.Body.String())
	}
	cookies := exchanged.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteNoneMode || cookies[0].Domain != "" {
		t.Fatalf("unexpected direct cookie: %+v", cookies)
	}
	if got := exchanged.Header().Get("Access-Control-Allow-Origin"); got != "https://bridge.example" {
		t.Fatalf("CORS origin=%q", got)
	}
	if got := exchanged.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Fatalf("CORS credentials=%q", got)
	}
	directAuth := directRequest(t, server, http.MethodGet, "https://lan.bridge.example:8788/api/auth", "https://bridge.example", cookies[0].Value, "", "")
	if directAuth.Code != http.StatusOK || !strings.Contains(directAuth.Body.String(), `"authenticated": true`) || strings.Contains(directAuth.Body.String(), `"direct"`) {
		t.Fatalf("direct auth status=%d body=%s", directAuth.Code, directAuth.Body.String())
	}
	if duplicate := exchange(); duplicate.Code != http.StatusForbidden {
		t.Fatalf("single use status=%d", duplicate.Code)
	}
}

func TestDirectRequiresAuthenticatedGrantAndExactOrigin(t *testing.T) {
	server, _, _ := directTestServer(t)
	anonymous := directRequest(t, server, http.MethodPost, "https://bridge.example/api/direct/grant", "https://bridge.example", "", "", `{}`)
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous grant status=%d", anonymous.Code)
	}
	token, _ := server.directGrants.issue("https://bridge.example", "https://lan.bridge.example:8788")
	wrongOrigin := directRequest(t, server, http.MethodPost, "https://lan.bridge.example:8788/api/direct/exchange", "https://attacker.example", "", "", `{"token":"`+token+`"}`)
	if wrongOrigin.Code != http.StatusForbidden || wrongOrigin.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("wrong origin status=%d headers=%v", wrongOrigin.Code, wrongOrigin.Header())
	}
}

func TestDirectPreflightAndContentSecurityPolicy(t *testing.T) {
	server, _, _ := directTestServer(t)
	request := httptest.NewRequest(http.MethodOptions, "https://lan.bridge.example:8788/api/sessions", nil)
	request.Header.Set("Origin", "https://bridge.example")
	request.Header.Set("Access-Control-Request-Method", http.MethodPost)
	request.Header.Set("Access-Control-Request-Headers", "Content-Type, X-CSRF-Token")
	request.Header.Set("Access-Control-Request-Private-Network", "true")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Private-Network") != "true" {
		t.Fatalf("preflight status=%d headers=%v", response.Code, response.Header())
	}
	page := directRequest(t, server, http.MethodGet, "https://bridge.example/", "", "", "", "")
	policy := page.Header().Get("Content-Security-Policy")
	if !strings.Contains(policy, "connect-src 'self' https://lan.bridge.example:8788") || !strings.Contains(policy, "img-src 'self' data: https://lan.bridge.example:8788") {
		t.Fatalf("CSP=%q", policy)
	}
}

func TestDirectGrantExpiryAndOriginBinding(t *testing.T) {
	store := newDirectGrantStore()
	now := time.Now()
	store.now = func() time.Time { return now }
	token, err := store.issue("https://bridge.example", "https://lan.bridge.example:8788")
	if err != nil {
		t.Fatal(err)
	}
	if store.exchange(token, "https://other.example", "https://lan.bridge.example:8788", "192.0.2.1") {
		t.Fatal("origin mismatch accepted")
	}
	now = now.Add(directGrantTTL + time.Second)
	if store.exchange(token, "https://bridge.example", "https://lan.bridge.example:8788", "192.0.2.1") {
		t.Fatal("expired grant accepted")
	}
}
