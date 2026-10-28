package httpd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func preloadRow(kind, id, cwd, project string) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("agent", kind)
	row.Set("id", id)
	row.Set("cwd", cwd)
	row.Set("projectKey", project)
	return row
}

func TestSelectPreloadSessions(t *testing.T) {
	rows := &jsoncompat.Array{}
	rows.Items = append(rows.Items, preloadRow("codex", "desktop", "/desktop", "local|/desktop"))
	for round := 0; round < 4; round++ {
		for project := 0; project < 8; project++ {
			id := fmt.Sprintf("p%d-s%d", project, round)
			cwd := fmt.Sprintf("/p%d", project)
			rows.Items = append(rows.Items, preloadRow("kimi", id, cwd, "local|"+cwd))
		}
	}
	selected := selectPreloadSessions(rows)
	if len(selected) != 20 {
		t.Fatalf("selected=%d want=20", len(selected))
	}
	seen := map[string]int{}
	for _, target := range selected {
		seen[target.id]++
		if target.kind != "kimi" || target.id == "desktop" {
			t.Fatalf("unexpected target: %+v", target)
		}
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("%s selected %d times", id, count)
		}
	}
	for project := 0; project < 5; project++ {
		for round := 0; round < 3; round++ {
			id := fmt.Sprintf("p%d-s%d", project, round)
			if seen[id] != 1 {
				t.Errorf("recent project target %s missing", id)
			}
		}
	}
	for _, id := range []string{"p5-s2", "p6-s2", "p7-s2", "p0-s3"} {
		if seen[id] != 0 {
			t.Errorf("%s should not be selected", id)
		}
	}
}

func TestPreloadRecentSessionsContinuesAfterFailure(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	loads := filepath.Join(dir, "loads.log")
	script := `import json,sys
log = "` + loads + `"
sessions = [
 {"sessionId":"good-new","title":"new","cwd":"` + dir + `","updatedAt":3},
 {"sessionId":"broken","title":"broken","cwd":"` + dir + `","updatedAt":2},
 {"sessionId":"good-old","title":"old","cwd":"` + dir + `","updatedAt":1},
]
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  print(json.dumps({"jsonrpc":"2.0","id":ident,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}}),flush=True)
  continue
 if method == "session/list": result={"sessions":sessions}
 elif method == "session/load":
  sid=m.get("params",{}).get("sessionId")
  with open(log,"a") as f: f.write(sid+"\n")
  if sid == "broken":
   print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32001,"message":"broken"}}),flush=True)
   continue
  result={"sessionId":sid}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	manager.InitializeTimeout = 3 * time.Second
	defer manager.Close()
	server := &Server{ACPAgents: map[string]*acpruntime.Manager{"kimi": manager}}
	server.PreloadRecentSessions()
	if manager.Get("good-new") == nil || manager.Get("good-old") == nil {
		t.Fatal("valid sessions were not preloaded")
	}
	if manager.Get("broken") != nil {
		t.Fatal("failed session must not stay live")
	}
	if _, err := manager.Open("good-new", dir, time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(loads)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if fmt.Sprint(lines) != "[good-new broken good-old]" {
		t.Fatalf("session/load calls=%v", lines)
	}
}
