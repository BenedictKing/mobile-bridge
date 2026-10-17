package httpd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/conventions"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// blocksAgent records every session/prompt and steering block list verbatim
// so tests can assert the exact block layout the send route assembled.
func blocksAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys
DIR = "` + dir + `"
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new": result={"sessionId":"km-conv"}
 elif method == "session/prompt":
  blocks=(m.get("params") or {}).get("prompt") or []
  record("prompts.txt", json.dumps(blocks,ensure_ascii=False))
  result={"stopReason":"end_turn"}
 elif method == "_session/steering":
  blocks=(m.get("params") or {}).get("prompt") or []
  record("steers.txt", json.dumps(blocks,ensure_ascii=False))
  result={"outcome":"injected"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func TestConventionsAPI(t *testing.T) {
	dataDir := t.TempDir()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8793, "instance")
	server.Conventions = conventions.New(filepath.Join(dataDir, "conventions.json"))

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(method, "http://127.0.0.1:8793"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8793"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8793")
		if body != "" {
			httptestRequest.Header.Set("Content-Type", "application/json")
			httptestRequest.Header.Set("X-CSRF-Token", csrf)
		}
		if token != "" {
			httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}

	// Unauthenticated reads stay behind the auth gate.
	anonymous := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8793, "instance")
	anonymous.Conventions = server.Conventions
	unauthenticated := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8793/api/conventions", nil)
	unauthenticated.Host = "127.0.0.1:8793"
	anonymousResponse := httptest.NewRecorder()
	anonymous.ServeHTTP(anonymousResponse, unauthenticated)
	if anonymousResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", anonymousResponse.Code)
	}

	// Default is off.
	defaults := request(http.MethodGet, "/api/conventions", "")
	if defaults.Code != http.StatusOK || !strings.Contains(defaults.Body.String(), `"artifactPaths": false`) {
		t.Fatalf("defaults status=%d body=%s", defaults.Code, defaults.Body.String())
	}

	// POST persists; a fresh instance sees the saved value.
	saved := request(http.MethodPost, "/api/conventions", `{"artifactPaths":true}`)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"artifactPaths": true`) {
		t.Fatalf("save status=%d body=%s", saved.Code, saved.Body.String())
	}
	if reloaded := conventions.New(filepath.Join(dataDir, "conventions.json")); !reloaded.ArtifactPaths() {
		t.Fatal("POST did not persist conventions.json")
	}
	if again := request(http.MethodGet, "/api/conventions", ""); !strings.Contains(again.Body.String(), `"artifactPaths": true`) {
		t.Fatalf("reload status=%d body=%s", again.Code, again.Body.String())
	}

	// Invalid bodies are 400: wrong type, or not an object at all.
	if bad := request(http.MethodPost, "/api/conventions", `{"artifactPaths":"yes"}`); bad.Code != http.StatusBadRequest {
		t.Fatalf("string switch status=%d body=%s", bad.Code, bad.Body.String())
	}
	if array := request(http.MethodPost, "/api/conventions", `[1]`); array.Code != http.StatusBadRequest {
		t.Fatalf("array body status=%d body=%s", array.Code, array.Body.String())
	}
	// Rejected writes leave the stored value alone.
	if still := request(http.MethodGet, "/api/conventions", ""); !strings.Contains(still.Body.String(), `"artifactPaths": true`) {
		t.Fatalf("failed write corrupted state: %s", still.Body.String())
	}

	// Without server wiring the route degrades to the standard conflict.
	unwired := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8793, "instance")
	unwiredRequest := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8793/api/conventions", nil)
	unwiredRequest.Host = "127.0.0.1:8793"
	unwiredResponse := httptest.NewRecorder()
	unwired.ServeHTTP(unwiredResponse, unwiredRequest)
	if unwiredResponse.Code != http.StatusUnauthorized {
		// Still behind auth even when unwired.
		t.Fatalf("unwired anonymous status=%d", unwiredResponse.Code)
	}
}

// TestContentSecurityPolicyFramesSelf covers both CSP sites (every output()
// response via /api/health, and the shared constant the SSE stream uses):
// frame-src 'self' admits the same-origin /files iframe previews.
func TestContentSecurityPolicyFramesSelf(t *testing.T) {
	if !strings.Contains(contentSecurityPolicy, "frame-src 'self'") {
		t.Fatalf("shared CSP lost frame-src: %s", contentSecurityPolicy)
	}
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8793, "instance")
	health := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8793/api/health", nil)
	health.Host = "127.0.0.1:8793"
	response := httptest.NewRecorder()
	server.ServeHTTP(response, health)
	if response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}
	if csp := response.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self'") {
		t.Fatalf("response CSP lost frame-src: %s", csp)
	}
}

// TestACPArtifactConventionInjection walks the send route with a fake agent:
// enabled turns prepend the convention as the first extra block while the
// user text stays its own leading block (and the timeline user row keeps the
// raw text), disabled turns carry no convention, steer never injects.
func TestACPArtifactConventionInjection(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(blocksAgent(t, dir))
	manager.Capabilities = []string{"steer"}
	manager.SteerMethod = "_session/steering"
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8793, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	server.Conventions = conventions.New(filepath.Join(t.TempDir(), "conventions.json"))

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(method, "http://127.0.0.1:8793"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8793"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8793")
		if body != "" {
			httptestRequest.Header.Set("Content-Type", "application/json")
			httptestRequest.Header.Set("X-CSRF-Token", csrf)
		}
		httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}
	blocks := func(name string) []any {
		t.Helper()
		time.Sleep(150 * time.Millisecond)
		raw := recorded(t, dir, name)
		if raw == "" {
			return nil
		}
		line := strings.TrimSpace(strings.Split(raw, "\n")[len(strings.Split(strings.TrimSpace(raw), "\n"))-1])
		var parsed []any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("%s not json (%q): %v", name, line, err)
		}
		return parsed
	}
	blockText := func(t *testing.T, block any) string {
		t.Helper()
		object, ok := block.(map[string]any)
		if !ok {
			t.Fatalf("block not an object: %v", block)
		}
		if object["type"] != "text" {
			t.Fatalf("block type=%v", object["type"])
		}
		text, _ := object["text"].(string)
		return text
	}

	if created := request(http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"约定会话","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`); created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	// Off by default: a plain send is exactly one text block.
	if sent := request(http.MethodPost, "/api/sessions/km-conv/send", `{"text":"你好"}`); sent.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sent.Code, sent.Body.String())
	}
	offBlocks := blocks("prompts.txt")
	if len(offBlocks) != 1 || blockText(t, offBlocks[0]) != "你好" {
		t.Fatalf("off-by-default blocks=%v", offBlocks)
	}

	// On: the convention rides as the first extra block, right after the
	// untouched user text block.
	if saved := request(http.MethodPost, "/api/conventions", `{"artifactPaths":true}`); saved.Code != http.StatusOK {
		t.Fatalf("enable status=%d body=%s", saved.Code, saved.Body.String())
	}
	if sent := request(http.MethodPost, "/api/sessions/km-conv/send", `{"text":"第二问"}`); sent.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", sent.Code, sent.Body.String())
	}
	onBlocks := blocks("prompts.txt")
	if len(onBlocks) != 2 || blockText(t, onBlocks[0]) != "第二问" || blockText(t, onBlocks[1]) != conventions.ArtifactPathHint {
		t.Fatalf("enabled blocks=%v", onBlocks)
	}

	// The timeline user row must show the raw text only, never the hint.
	page := request(http.MethodGet, "/api/sessions/km-conv/timeline?limit=20", "")
	if page.Code != http.StatusOK {
		t.Fatalf("timeline status=%d", page.Code)
	}
	var userTexts []string
	if payload, err := jsoncompat.Unmarshal(page.Body.Bytes()); err == nil {
		if rows, ok := payload.(*jsoncompat.Object).MustGet("rows").(*jsoncompat.Array); ok {
			for _, item := range rows.Items {
				row, _ := item.(*jsoncompat.Object)
				if row != nil && row.MustGet("role") == "user" {
					text, _ := row.MustGet("text").(string)
					userTexts = append(userTexts, text)
				}
			}
		}
	}
	if len(userTexts) != 2 || userTexts[0] != "你好" || userTexts[1] != "第二问" {
		t.Fatalf("user rows=%v", userTexts)
	}
	for _, text := range userTexts {
		if strings.Contains(text, conventions.ArtifactPathHint) {
			t.Fatalf("convention leaked into the user row: %q", text)
		}
	}

	// Steer supplements never carry the convention.
	if steered := request(http.MethodPost, "/api/sessions/km-conv/send", `{"text":"补充一下","mode":"steer"}`); steered.Code != http.StatusOK ||
		!strings.Contains(steered.Body.String(), `"steered": true`) {
		t.Fatalf("steer status=%d body=%s", steered.Code, steered.Body.String())
	}
	steerBlocks := blocks("steers.txt")
	if len(steerBlocks) != 1 || blockText(t, steerBlocks[0]) != "补充一下" {
		t.Fatalf("steer blocks=%v", steerBlocks)
	}
	if promptCount := len(strings.Split(strings.TrimSpace(recorded(t, dir, "prompts.txt")), "\n")); promptCount != 2 {
		t.Fatalf("steer fell through to a prompt: %d recorded", promptCount)
	}
}
