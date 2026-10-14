package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func tunnelServer(t *testing.T, mode string) *Server {
	t.Helper()
	web := fstest.MapFS{"web/index.html": &fstest.MapFile{Data: []byte("ok")}}
	server := New(auth.NewAuth(auth.Config{Mode: mode, Username: "admin"}), web, 8790, "instance")
	server.Hosts["bridge.example"] = true
	server.Origins["https://bridge.example"] = true
	server.SecureHosts["bridge.example"] = true
	return server
}

func TestSecureHostCookieCarriesSecureAttribute(t *testing.T) {
	server := tunnelServer(t, "none")
	request := httptest.NewRequest(http.MethodPost, "https://bridge.example/api/login", strings.NewReader(`{"username":"admin","password":""}`))
	request.Host = "bridge.example"
	request.Header.Set("Origin", "https://bridge.example")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", response.Code, response.Body.String())
	}
	cookie := response.Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "; Secure") {
		t.Fatalf("tunnel cookie missing Secure: %q", cookie)
	}

	local := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/login", strings.NewReader(`{"username":"admin","password":""}`))
	local.Host = "127.0.0.1:8790"
	local.Header.Set("Origin", "http://127.0.0.1:8790")
	local.Header.Set("Content-Type", "application/json")
	localResponse := httptest.NewRecorder()
	server.ServeHTTP(localResponse, local)
	if localResponse.Code != http.StatusOK {
		t.Fatalf("local login status=%d", localResponse.Code)
	}
	if strings.Contains(localResponse.Header().Get("Set-Cookie"), "; Secure") {
		t.Fatalf("loopback cookie must not be Secure: %q", localResponse.Header().Get("Set-Cookie"))
	}
}

func TestForwardedClientIPKeysFailureAccounting(t *testing.T) {
	server := tunnelServer(t, "password")
	request := httptest.NewRequest(http.MethodPost, "https://bridge.example/api/login", strings.NewReader(`{"username":"admin","password":"wrong"}`))
	request.Host = "bridge.example"
	request.RemoteAddr = "127.0.0.1:55555"
	request.Header.Set("Origin", "https://bridge.example")
	request.Header.Set("X-Forwarded-For", "203.0.113.5")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	status := body["loginStatus"].(map[string]any)
	if status["attemptsRemaining"].(float64) != 4 {
		t.Fatalf("forwarded ip accounting: %v", status)
	}

	// The loopback peer itself stays clean: local access is not blocked by
	// remote failures through the tunnel.
	if remaining := server.Auth.LoginStatus("127.0.0.1").MustGet("attemptsRemaining"); remaining != jsoncompat.Num(5) {
		t.Fatalf("peer was polluted: %v", remaining)
	}
}
