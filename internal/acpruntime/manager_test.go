package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestManagerRunsSessionLifecycleAgainstFakeAgent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[{"sessionId":"sess-existing","title":"Existing","cwd":"/work","updatedAt":1}]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new": result={"sessionId":"sess-created"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%v err=%v", rows, err)
	}
	loaded, err := manager.Open("sess-existing", "/work", 3*time.Second)
	if err != nil || loaded.ID != "sess-existing" {
		t.Fatalf("load=%v err=%v", loaded, err)
	}
	created, err := manager.Create("request-id", "/work", "新建聊天", 3*time.Second)
	if err != nil || created.ID != "sess-created" {
		t.Fatalf("create=%v err=%v", created, err)
	}
	// The created session must be visible before the agent lists it.
	merged := manager.MergePending(nil)
	if len(merged) != 1 {
		t.Fatalf("pending rows=%v", merged)
	}
	if merged[0].MustGet("sessionId") != "sess-created" || merged[0].MustGet("title") != "新建聊天" {
		t.Fatalf("pending row=%v", merged[0])
	}
	if !manager.Knows("sess-created") {
		t.Fatal("created session not known to the manager")
	}
	// Merging against the listed rows keeps both: the listed session and the
	// created one the (stateless) fake never lists back.
	covered := manager.MergePending(rows)
	if len(covered) != 2 {
		t.Fatalf("covered rows=%v", covered)
	}
	manager.Close()
}

func TestManagerResolvesLazyPlaceholderThroughCanonical(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[{"sessionId":"sess-real","title":"Materialized","cwd":"/work","updatedAt":2}]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 elif method == "session/new": result={"sessionId":"uuid-placeholder"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.Canonical = func(id string) string {
		if id == "uuid-placeholder" {
			return "sess-real"
		}
		return ""
	}
	if _, err := manager.Create("seed", "/work", "占位会话", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	rows, err := manager.List(3 * time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The placeholder is covered by its materialized backend id.
	merged := manager.MergePending(rows)
	if len(merged) != 1 || merged[0].MustGet("sessionId") != "sess-real" {
		t.Fatalf("merged rows=%v", merged)
	}
	if !manager.Knows("uuid-placeholder") {
		t.Fatal("placeholder not known through canonical id")
	}
	if cwd := manager.CwdFor("uuid-placeholder"); cwd != "/work" {
		t.Fatalf("canonical cwd=%q", cwd)
	}
	manager.Close()
}

func TestSessionFoldsUpdatesAndRequests(t *testing.T) {
	session := &Session{ID: "sess-1", View: acpview.NewView("sess-1", "Demo", "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, changed: make(chan struct{})}
	update := jsoncompat.NewObject()
	update.Set("method", "session/update")
	params := jsoncompat.NewObject()
	params.Set("sessionUpdate", "agent_message_chunk")
	content := jsoncompat.NewObject()
	content.Set("text", "hello")
	params.Set("content", content)
	update.Set("params", params)
	session.onNotification(update)
	snapshot := session.Snapshot()
	turns := snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) != 1 || !jsoncompat.Truthy(turns.Items[0]) {
		t.Fatalf("update did not create turn: %s", jsoncompat.EncodeCompact(snapshot))
	}
	request := jsoncompat.NewObject()
	request.Set("id", "permission-1")
	request.Set("method", "session/request_permission")
	permissionParams := jsoncompat.NewObject()
	toolCall := jsoncompat.NewObject()
	toolCall.Set("title", "Run command")
	toolCall.Set("rawInput", "ls")
	permissionParams.Set("toolCall", toolCall)
	permissionParams.Set("options", &jsoncompat.Array{})
	request.Set("params", permissionParams)
	session.onRequest(request, func(any) {})
	if len(session.Snapshot().MustGet("requests").(*jsoncompat.Array).Items) != 1 {
		t.Fatal("permission request was not exposed")
	}
}
