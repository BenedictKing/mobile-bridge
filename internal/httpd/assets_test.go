package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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

func TestLoginShellAssetsAreServedBeforeAuthentication(t *testing.T) {
	files := map[string]string{
		"web/account.js":           "window.account=true;",
		"web/account.css":          "body{}",
		"web/style.css":            "body{}",
		"web/presentation.css":     "body{}",
		"web/manifest.webmanifest": `{"name":"Mobile / Bridge"}`,
		"web/icon.png":             "\x89PNG",
	}
	web := fstest.MapFS{}
	for name, data := range files {
		web[name] = &fstest.MapFile{Data: []byte(data)}
	}
	server := New(auth.NewAuth(auth.Config{Mode: "password"}), web, 8790, "instance")
	expected := map[string]string{
		"/account.js":           "text/javascript; charset=utf-8",
		"/account.css":          "text/css; charset=utf-8",
		"/style.css":            "text/css; charset=utf-8",
		"/presentation.css":     "text/css; charset=utf-8",
		"/manifest.webmanifest": "application/manifest+json",
		"/icon.png":             "image/png",
	}
	for path, contentType := range expected {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+path, nil)
		request.Host = "127.0.0.1:8790"
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != contentType {
			t.Fatalf("%s status=%d type=%q", path, response.Code, response.Header().Get("Content-Type"))
		}
	}
}

func TestScriptedSessionLiveSequenceWakesWaits(t *testing.T) {
	current := 3
	session := &ScriptedSession{Epoch: "go-x", Sequence: current, Live: func() (string, int, bool) {
		return "go-x", current, true
	}}
	done := make(chan bool, 1)
	go func() {
		changed := session.WaitFor(func() bool { return session.Sequence != 3 }, 5*time.Second)
		done <- changed
	}()
	time.Sleep(300 * time.Millisecond)
	current = 4
	select {
	case changed := <-done:
		if !changed || session.Sequence != 4 {
			t.Fatalf("live wait: changed=%v sequence=%d", changed, session.Sequence)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live wait never woke")
	}
}
