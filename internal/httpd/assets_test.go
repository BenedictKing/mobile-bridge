package httpd

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	mobilebridge "github.com/BenedictKing/mobile-bridge"
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
		"web/team-canvas.js":       "window.teamCanvas=true;",
		"web/team-live-panel.js":   "window.teamLivePanel=true;",
		"web/icons.js":             "const BridgeIcons={};",
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
		"/team-canvas.js":       "text/javascript; charset=utf-8",
		"/team-live-panel.js":   "text/javascript; charset=utf-8",
		"/icons.js":             "text/javascript; charset=utf-8",
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

func TestEmbeddedLoginShellReferencesAreServed(t *testing.T) {
	index, err := mobilebridge.WebFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	server := New(auth.NewAuth(auth.Config{Mode: "password"}), mobilebridge.WebFS, 8790, "instance")
	references := regexp.MustCompile(`<(?:script|link)\b[^>]*(?:src|href)="([^"]+)"`).FindAllSubmatch(index, -1)
	if len(references) == 0 {
		t.Fatal("login shell must reference its assets")
	}
	for _, reference := range references {
		target := string(reference[1])
		t.Run(target, func(t *testing.T) {
			assetURL, err := url.Parse(target)
			if err != nil || assetURL.IsAbs() || !strings.HasPrefix(assetURL.Path, "/") {
				t.Fatalf("unexpected shell asset %q", target)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+target, nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			expected, err := mobilebridge.WebFS.ReadFile("web" + assetURL.Path)
			if err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusOK || response.Body.String() != string(expected) {
				t.Fatalf("shell asset %s status=%d, must serve embedded bytes before login", target, response.Code)
			}
		})
	}
}

func TestScriptedSessionLiveSequenceWakesWaits(t *testing.T) {
	var current atomic.Int64
	current.Store(3)
	session := &ScriptedSession{Epoch: "go-x", Sequence: 3, Live: func() (string, int, bool) {
		return "go-x", int(current.Load()), true
	}}
	done := make(chan bool, 1)
	go func() {
		changed := session.WaitFor(func() bool { return session.Sequence != 3 }, 5*time.Second)
		done <- changed
	}()
	time.Sleep(300 * time.Millisecond)
	current.Store(4)
	select {
	case changed := <-done:
		if !changed || session.Sequence != 4 {
			t.Fatalf("live wait: changed=%v sequence=%d", changed, session.Sequence)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("live wait never woke")
	}
}
