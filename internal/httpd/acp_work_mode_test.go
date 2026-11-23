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
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type acpWorkModeFixture struct {
	server           *Server
	session          *acpruntime.Session
	token, csrf, log string
}

func newACPWorkModeFixture(t *testing.T, kind, modes, rejectMode string) *acpWorkModeFixture {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := `import json,sys
LOG='` + log + `'
MODES=json.loads('` + modes + `')
REJECT='` + rejectMode + `'
def record(value):
 with open(LOG,"a") as stream: stream.write(value+"\n")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method in ["session/new","session/load"]: result={"sessionId":"work-mode-session","modes":MODES,"configOptions":[{"id":"model","category":"model","currentValue":"model-a"}]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/set_mode":
  mode=params.get("modeId",""); record("mode:"+mode)
  if mode == REJECT:
   print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32602,"message":"permission mode rejected"}}),flush=True); continue
  result={}
 elif method == "test/native_exit":
  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"work-mode-session","update":{"sessionUpdate":"current_mode_update","currentModeId":"auto"}}}),flush=True)
  result={}
 elif method == "session/prompt": record("prompt"); result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	manager.Kind = kind
	t.Cleanup(manager.Close)
	session, err := manager.Create("seed", dir, "工作模式测试", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	server, token, csrf := newFeatureServer(t, dir)
	server.ACPAgents = map[string]*acpruntime.Manager{kind: manager}
	t.Cleanup(server.CloseStreams)
	return &acpWorkModeFixture{server: server, session: session, token: token, csrf: csrf, log: log}
}

func (f *acpWorkModeFixture) send(mode string) *httptest.ResponseRecorder {
	return featureRequest(f.server, f.token, f.csrf, http.MethodPost,
		"/api/sessions/"+f.session.ID+"/send",
		`{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"hello","mode":"`+mode+`","workMode":"default"}`)
}

func (f *acpWorkModeFixture) expectCalls(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err := os.ReadFile(f.log)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(raw)) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("wire calls=%q, want %q", raw, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestACPWithoutWorkModesSupportsDirectAndQueuedSend(t *testing.T) {
	for _, kind := range []string{"acpagent", "acpagent-review"} {
		for _, mode := range []string{"send", "queue"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				fixture := newACPWorkModeFixture(t, kind, `null`, "")
				response := fixture.send(mode)
				if response.Code != http.StatusOK {
					t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
				}
				if mode == "queue" {
					fixture.server.dispatchQueuedThread(fixture.session.ID, "", nil)
				}
				fixture.expectCalls(t, "prompt")
				entry := fixture.server.Queue.get(fixture.session.ID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
				if entry == nil || entry.Status != SubAccepted {
					t.Fatalf("submission not accepted: %+v", entry)
				}
			})
		}
	}
}

func TestDeferredPermissionAppliesBeforeDirectAndQueuedPrompt(t *testing.T) {
	const modes = `{"currentModeId":"auto","availableModes":[{"id":"default"},{"id":"plan"},{"id":"auto"},{"id":"acceptEdits"}]}`
	for _, mode := range []string{"send", "queue"} {
		for _, rejectMode := range []string{"", "acceptEdits"} {
			t.Run(mode+"/reject="+rejectMode, func(t *testing.T) {
				fixture := newACPWorkModeFixture(t, "claude", modes, rejectMode)
				if err := fixture.session.ApplyWorkMode("plan", nil); err != nil {
					t.Fatal(err)
				}
				if err := fixture.session.SetPermission("acceptEdits"); err != nil {
					t.Fatal(err)
				}
				if _, err := fixture.session.Request("test/native_exit", jsoncompat.NewObject(), 3*time.Second); err != nil {
					t.Fatal(err)
				}
				response := fixture.send(mode)
				wantStatus := http.StatusOK
				if rejectMode != "" && mode == "send" {
					wantStatus = http.StatusBadRequest
				}
				if response.Code != wantStatus {
					t.Fatalf("send status=%d body=%s", response.Code, response.Body.String())
				}
				if mode == "queue" {
					fixture.server.dispatchQueuedThread(fixture.session.ID, "", nil)
				}
				wantCalls := "mode:plan\nmode:acceptEdits"
				if rejectMode == "" {
					wantCalls += "\nprompt"
				}
				fixture.expectCalls(t, wantCalls)
				if rejectMode != "" && mode == "queue" {
					entry := fixture.server.Queue.get(fixture.session.ID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
					if entry == nil || entry.Status != SubFailed || !strings.Contains(entry.Error, "permission mode rejected") {
						t.Fatalf("queue did not surface permission failure: %+v", entry)
					}
				}
			})
		}
	}
}
