package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestActivityAndJevRoutesUseAuthenticatedGoState(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Jev = jev.New(t.TempDir() + "/jev.json")

	activity := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/activity", strings.NewReader(`{"hosts":{"local":["thread-1"]}}`))
	activity.Host = "127.0.0.1:8790"
	activity.Header.Set("Origin", "http://127.0.0.1:8790")
	activity.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	activity.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, activity)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sessions"`) {
		t.Fatalf("activity status=%d body=%s", response.Code, response.Body.String())
	}

	read := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/jev", nil)
	read.Host = "127.0.0.1:8790"
	read.Header.Set("Origin", "http://127.0.0.1:8790")
	read.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response = httptest.NewRecorder()
	server.ServeHTTP(response, read)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"configured": false`) {
		t.Fatalf("jev read status=%d body=%s", response.Code, response.Body.String())
	}

	update := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/jev", strings.NewReader(`{"baseUrl":"","model":"","autoApprove":false,"autoContinue":true}`))
	update.Host = "127.0.0.1:8790"
	update.Header.Set("Origin", "http://127.0.0.1:8790")
	update.Header.Set("X-CSRF-Token", session.MustGet("csrf").(string))
	update.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response = httptest.NewRecorder()
	server.ServeHTTP(response, update)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"autoContinue": true`) {
		t.Fatalf("jev update status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestACPActivityIncludesTurnAndPendingRequests(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(featureAgent(t, dir))
	defer manager.Close()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	request := func(target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		req.Host = "127.0.0.1:8790"
		req.Header.Set("Origin", "http://127.0.0.1:8790")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	created := request("/api/sessions", `{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"活动状态","id":"7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	if sent := request("/api/sessions/km-feature/send", `{"text":"完成一轮"}`); sent.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sent.Code, sent.Body.String())
	}
	time.Sleep(120 * time.Millisecond)
	live := manager.Get("km-feature")
	if live == nil {
		t.Fatal("missing live ACP session")
	}
	live.View.Requests = append(live.View.Requests, jsoncompat.NewObject())
	response := request("/api/activity", `{"hosts":{"local":["km-feature"]}}`)
	body := response.Body.String()
	for _, needle := range []string{`"turnId":`, `"turnStatus": "completed"`, `"pendingRequests": 1`} {
		if !strings.Contains(body, needle) {
			t.Fatalf("activity missing %s: %s", needle, body)
		}
	}
}
