package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
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
