package httpd

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
)

func TestCodexDeleteResponseAndFallback(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	for _, testCase := range []struct {
		name     string
		fails    bool
		mode     string
		warning  bool
		isHidden bool
	}{
		{name: "native delete", mode: "deleted"},
		{name: "legacy runtime falls back to hidden", fails: true, mode: "hidden", warning: true, isHidden: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			script := `import json,sys
FAIL = ` + map[bool]string{true: "True", false: "False"}[testCase.fails] + `
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize": result={"ok":True}
 elif method=="thread/delete" and FAIL:
  print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32601,"message":"thread/delete unavailable"}}),flush=True)
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
			executable := filepath.Join(t.TempDir(), "app-server")
			if err := os.WriteFile(executable, []byte("#!"+python+"\n"+script), 0o700); err != nil {
				t.Fatal(err)
			}
			bridge := codex.New(t.TempDir(), "unused-endpoint")
			bridge.Executable = executable
			server, token, csrf := newFeatureServer(t, t.TempDir())
			server.Codex = bridge
			threadID := "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			response := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/"+threadID+"/delete", `{}`)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"mode": "`+testCase.mode+`"`) {
				t.Fatalf("delete status=%d body=%s", response.Code, response.Body.String())
			}
			if strings.Contains(response.Body.String(), `"warning"`) != testCase.warning {
				t.Fatalf("warning mismatch: %s", response.Body.String())
			}
			if server.Hidden.Hidden(threadID) != testCase.isHidden {
				t.Fatalf("hidden=%v want %v", server.Hidden.Hidden(threadID), testCase.isHidden)
			}
		})
	}
}
