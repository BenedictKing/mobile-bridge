package httpd

import (
	"compress/gzip"
	"encoding/json"
	"io"
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

func TestEmbeddedProductionAssetsAreHashedAndCompressed(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "password"}), mobilebridge.WebFS, 8790, "instance")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("index status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
	}
	references := regexp.MustCompile(`<(?:script|link)\b[^>]*(?:src|href)="([^"]+)"`).FindAllSubmatch(response.Body.Bytes(), -1)
	if len(references) == 0 {
		t.Fatal("production shell has no assets")
	}
	hashed := regexp.MustCompile(`^(.+)\.[0-9a-f]{12}(\.(?:js|css|png|webmanifest))$`)
	rawBytes, productionBytes := 0, 0
	checkedScripts := 0
	for _, reference := range references {
		target := string(reference[1])
		t.Run(target, func(t *testing.T) {
			assetURL, err := url.Parse(target)
			if err != nil || assetURL.IsAbs() || assetURL.RawQuery != "" {
				t.Fatalf("unexpected production asset %q", target)
			}
			matches := hashed.FindStringSubmatch(assetURL.Path)
			if matches == nil {
				t.Fatalf("asset is not content hashed: %s", target)
			}
			originalPath := matches[1] + matches[2]
			raw, err := mobilebridge.WebFS.ReadFile("web" + originalPath)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+target, nil)
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != staticImmutableCache {
				t.Fatalf("asset status=%d cache=%q", response.Code, response.Header().Get("Cache-Control"))
			}
			rawBytes += len(raw)
			productionBytes += response.Body.Len()
			if response.Body.Len() > len(raw) {
				t.Fatalf("production asset grew from %d to %d bytes", len(raw), response.Body.Len())
			}
			if strings.HasSuffix(originalPath, ".js") {
				checkedScripts++
			}
			etag := response.Header().Get("ETag")
			if etag == "" {
				t.Fatal("asset has no ETag")
			}
			conditional := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+target, nil)
			conditional.Header.Set("If-None-Match", etag)
			notModified := httptest.NewRecorder()
			server.ServeHTTP(notModified, conditional)
			if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
				t.Fatalf("conditional status=%d body=%d", notModified.Code, notModified.Body.Len())
			}
			compressed := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+target, nil)
			compressed.Header.Set("Accept-Encoding", "gzip")
			gzipped := httptest.NewRecorder()
			server.ServeHTTP(gzipped, compressed)
			if gzipped.Header().Get("Content-Encoding") == "gzip" {
				reader, err := gzip.NewReader(gzipped.Body)
				if err != nil {
					t.Fatal(err)
				}
				unpacked, err := io.ReadAll(reader)
				if err != nil {
					t.Fatal(err)
				}
				if string(unpacked) != response.Body.String() {
					t.Fatal("gzip content differs from production asset")
				}
			}
		})
	}
	if checkedScripts < 20 || productionBytes >= rawBytes {
		t.Fatalf("insufficient production coverage: scripts=%d raw=%d production=%d", checkedScripts, rawBytes, productionBytes)
	}
	t.Logf("production shell assets: raw=%d production=%d", rawBytes, productionBytes)
}

func TestEmbeddedProductionManifestMapsDeferredAssets(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "password"}), mobilebridge.WebFS, 8790, "instance")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/", nil))
	match := regexp.MustCompile(`<script id="bridge-assets" type="application/json">([^<]+)</script>`).FindSubmatch(response.Body.Bytes())
	if match == nil {
		t.Fatal("production shell has no deferred asset manifest")
	}
	var manifest map[string]string
	if err := json.Unmarshal(match[1], &manifest); err != nil {
		t.Fatal(err)
	}
	deferred := []string{
		"/account.js", "/accounts.js", "/pool.js", "/markdown.js", "/diff.js", "/team-canvas.js", "/team-swarm.js",
		"/vendor/markdown-it.min.js", "/vendor/katex/katex.min.js", "/vendor/katex/katex.min.css",
		"/vendor/texmath.js", "/vendor/dagre.min.js",
	}
	if len(manifest) != len(deferred) {
		t.Errorf("manifest contains %d assets, want %d: %v", len(manifest), len(deferred), manifest)
	}
	hashed := regexp.MustCompile(`^(.+)\.[0-9a-f]{12}(\.(?:js|css))$`)
	for _, original := range deferred {
		production := manifest[original]
		if !hashed.MatchString(production) {
			t.Errorf("deferred asset %q has unhashed production URL %q", original, production)
			continue
		}
		if strings.Contains(response.Body.String(), `src="`+original+`"`) || strings.Contains(response.Body.String(), `href="`+original+`"`) {
			t.Errorf("deferred asset %q is still eagerly referenced", original)
		}
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790"+production, nil)
		assetResponse := httptest.NewRecorder()
		server.ServeHTTP(assetResponse, request)
		if assetResponse.Code != http.StatusOK || assetResponse.Header().Get("Cache-Control") != staticImmutableCache || assetResponse.Body.Len() == 0 {
			t.Errorf("deferred asset %q status=%d cache=%q bytes=%d", production, assetResponse.Code, assetResponse.Header().Get("Cache-Control"), assetResponse.Body.Len())
		}
	}
}

func TestStaticAssetAssemblyFailureIsExplicit(t *testing.T) {
	web := fstest.MapFS{
		"web/index.html": &fstest.MapFile{Data: []byte(`<script src="/app.js"></script>`)},
		"web/app.js":     &fstest.MapFile{Data: []byte(`const {`)},
	}
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), web, 8790, "instance")
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/", nil)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "静态资源装配失败") {
		t.Fatalf("broken assets status=%d body=%q", response.Code, response.Body.String())
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
