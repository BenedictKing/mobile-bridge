package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
)

func TestNativeCandidatesRequireAuthenticatedNativeRequest(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	server.NativeCandidates = func() []string { return []string{"http://192.0.2.10:8790"} }
	token, _, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	request := httptestNativeRequest("http://127.0.0.1:8790/api/auth", token, true)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"nativeDirect": ["http://192.0.2.10:8790"]`) {
		t.Fatalf("native candidates status=%d body=%s", response.Code, response.Body.String())
	}
	plain := httptestNativeRequest("http://127.0.0.1:8790/api/auth", token, false)
	plainResponse := httptest.NewRecorder()
	server.ServeHTTP(plainResponse, plain)
	if strings.Contains(plainResponse.Body.String(), "nativeDirect") {
		t.Fatal("browser auth response contains native candidates")
	}
	anonymous := httptestNativeRequest("http://127.0.0.1:8790/api/auth", "", true)
	anonymousResponse := httptest.NewRecorder()
	server.ServeHTTP(anonymousResponse, anonymous)
	if strings.Contains(anonymousResponse.Body.String(), "nativeDirect") {
		t.Fatal("anonymous auth response contains native candidates")
	}
}

func TestDynamicHostAndOriginCallbacksRemainScoped(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	server.AllowHost = func(host string) bool { return host == "192.0.2.10:8790" }
	server.AllowOrigin = func(origin string) bool { return origin == "http://192.0.2.10:8790" }
	token, _, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	allowed := httptestNativeRequest("http://192.0.2.10:8790/api/auth", token, false)
	allowed.Header.Set("Origin", "http://192.0.2.10:8790")
	allowedResponse := httptest.NewRecorder()
	server.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("dynamic address status=%d body=%s", allowedResponse.Code, allowedResponse.Body.String())
	}
	rejected := httptestNativeRequest("http://192.0.2.11:8790/api/auth", token, false)
	rejectedResponse := httptest.NewRecorder()
	server.ServeHTTP(rejectedResponse, rejected)
	if rejectedResponse.Code != http.StatusForbidden {
		t.Fatalf("unexpected host status=%d", rejectedResponse.Code)
	}
}

func httptestNativeRequest(target, token string, native bool) *http.Request {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	}
	if native {
		request.Header.Set("X-Mobile-Bridge-Native", "1")
	}
	return request
}
