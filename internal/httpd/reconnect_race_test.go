package httpd

import (
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
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// streamAgent answers prompts with a slow streamed turn, so a reconnect that
// arrives mid-turn either keeps the live handle (turn survives) or kills it.
func streamAgent(t *testing.T) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys,threading,time
def note(update):
 print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"update":update}}),flush=True)
def run():
 note({"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"text","text":"流式中…"}})
 time.sleep(1.2)
 note({"sessionUpdate":"agent_message_chunk","messageId":"m-1","content":{"type":"text","text":"完成"}})
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method in ("session/new","session/load"):
  result={"sessionId":"km-race","modes":{"currentModeId":"default","availableModes":[{"id":"default"}]}}
 elif method == "session/prompt":
  result={"stopReason":"end_turn"}
  threading.Thread(target=run,daemon=True).start()
 elif method == "session/list":
  result={"sessions":[]}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// The phone opens a chat by firing reconnect-activate; for a session with a
// live streaming turn that used to Reconcile — spawning a fresh child process
// and closing the streaming one — so the turn (a fork's transcript prompt, or
// an ordinary reply the user switched away from and back to) vanished.
func TestACPReconnectKeepsInFlightTurn(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(streamAgent(t))
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8793, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}

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

	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":`+jsonText(dir)+`,"title":"重连竞态","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	send := request(http.MethodPost, "/api/sessions/km-race/send",
		`{"text":"跑","id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	// The frontend opens the chat the moment the fork/send returns: this
	// reconnect used to land while the turn was still streaming.
	reconnect := request(http.MethodPost, "/api/sessions/km-race/reconnect", `{"activate":true}`)
	if reconnect.Code != http.StatusOK {
		t.Fatalf("reconnect status=%d body=%s", reconnect.Code, reconnect.Body.String())
	}
	time.Sleep(2 * time.Second)

	page := request(http.MethodGet, "/api/sessions/km-race/timeline?limit=20", "")
	value, _ := jsoncompat.Unmarshal(page.Body.Bytes())
	payload, _ := value.(*jsoncompat.Object)
	if payload == nil {
		t.Fatalf("timeline not json: %s", page.Body.String())
	}
	body := string(jsoncompat.EncodeCompact(payload))
	if !strings.Contains(body, "跑") || !strings.Contains(body, "完成") {
		t.Fatalf("in-flight turn lost across reconnect: %s", page.Body.String())
	}
}
