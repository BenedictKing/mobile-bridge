package httpd

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
)

func TestDeepSeekPermissionRoutes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
MODES={"currentModeId":"workspace-write","availableModes":[{"id":"read-only","name":"Read only"},{"id":"workspace-write","name":"Workspace"}]}
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new": result={"sessionId":"dsh-permissions","modes":MODES,"configOptions":[]}
 elif method == "session/set_mode":
  MODES["currentModeId"]=params.get("modeId")
  result={}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	manager.Kind = "dsh"
	defer manager.Close()
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"dsh": manager}

	created := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions",
		`{"agent":"dsh","cwd":`+jsonText(t.TempDir())+`,"title":"权限会话","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	read := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/dsh-permissions/permissions", "")
	if read.Code != http.StatusOK || !strings.Contains(read.Body.String(), `"supported": true`) ||
		!strings.Contains(read.Body.String(), `"current": "workspace-write"`) {
		t.Fatalf("permissions read status=%d body=%s", read.Code, read.Body.String())
	}
	updated := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/dsh-permissions/permissions", `{"mode":"read-only"}`)
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"current": "read-only"`) {
		t.Fatalf("permissions update status=%d body=%s", updated.Code, updated.Body.String())
	}
	rejected := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/dsh-permissions/permissions", `{"mode":"plan"}`)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("unadvertised permission status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}
