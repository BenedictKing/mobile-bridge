package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
)

func TestVendorAssetsAreServedBeforeAuthentication(t *testing.T) {
	web := fstest.MapFS{"web/vendor/katex/katex.min.css": &fstest.MapFile{Data: []byte(".katex{}")}}
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), web, 8790, "instance")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/vendor/katex/katex.min.css", nil)
	request.Host = "127.0.0.1:8790"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/css; charset=utf-8" || response.Body.String() != ".katex{}" {
		t.Fatalf("asset status=%d type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	traversal := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/vendor/../config.json", nil)
	traversal.Host = "127.0.0.1:8790"
	response = httptest.NewRecorder()
	server.ServeHTTP(response, traversal)
	if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "config") {
		t.Fatalf("vendor traversal unexpectedly served: status=%d body=%s", response.Code, response.Body.String())
	}
}
