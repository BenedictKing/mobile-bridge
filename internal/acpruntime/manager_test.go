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

func TestMergePendingKeepsCreatedSessionWithClearedTitle(t *testing.T) {
	manager := NewCommand(nil)
	manager.sessions["sess-created"] = &Session{
		ID: "sess-created", Cwd: "/work", CreatedAtMs: 1,
		View: acpview.NewView("sess-created", nil, "/work", func() float64 { return 1 }),
	}
	rows := manager.MergePending(nil)
	if len(rows) != 1 || rows[0].MustGet("title") != "未命名聊天" {
		t.Fatalf("created session with cleared title disappeared: %v", rows)
	}
}

func TestManagerFetchesAllSessionListPages(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list":
  cursor=m.get("params",{}).get("cursor")
  if cursor is None: result={"sessions":[{"sessionId":"sess-1","cwd":"/work"},{"sessionId":"sess-2","cwd":"/work"}],"nextCursor":"page-2"}
  elif cursor == "page-2": result={"sessions":[{"sessionId":"sess-2","cwd":"/work"},{"sessionId":"sess-3","cwd":"/work"}],"nextCursor":"page-2"}
  else: raise Exception("unexpected cursor "+str(cursor))
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 3 {
		t.Fatalf("paginated rows=%v err=%v", rows, err)
	}
	for index, id := range []string{"sess-1", "sess-2", "sess-3"} {
		if rows[index].MustGet("sessionId") != id {
			t.Fatalf("row %d = %v", index, rows[index])
		}
	}
	manager.Close()
}

func TestManagerTreatsMissingSessionListAsUnsupported(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "session/list":
  print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32601,"message":"Method not found: session/list"}}),flush=True)
  continue
 result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 0 {
		t.Fatalf("list rows=%v err=%v", rows, err)
	}
	if !manager.HasListCache() {
		t.Fatal("unsupported session/list should still cache an empty snapshot")
	}
	manager.Close()
}

// Agents that answer -32601 but carry an on-disk store (workbuddy's
// `codebuddy --acp`) list through NativeList instead.
func TestManagerFallsBackToNativeListWhenSessionListUnsupported(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "session/list":
  print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32601,"message":"Method not found: session/list"}}),flush=True)
  continue
 result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.NativeList = func() []*jsoncompat.Object {
		row := jsoncompat.NewObject()
		row.Set("sessionId", "native-1")
		row.Set("title", "原生会话")
		row.Set("cwd", "/work/native")
		row.Set("updatedAt", jsoncompat.Num(1792283000000/1000*1000))
		return []*jsoncompat.Object{row}
	}
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%v err=%v", rows, err)
	}
	if id, _ := rows[0].MustGet("sessionId").(string); id != "native-1" {
		t.Fatalf("row=%v", rows[0])
	}
	if !manager.Knows("native-1") {
		t.Fatal("native row not known to the manager")
	}
	if cwd := manager.CwdFor("native-1"); cwd != "/work/native" {
		t.Fatalf("cwd=%q", cwd)
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

func TestManagerReconcileSwapsLoadedHandle(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	old, err := manager.Open("sess-reconcile", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := manager.Reconcile("sess-reconcile", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if old == fresh {
		t.Fatal("reconcile reused the old session handle")
	}
	if got := manager.Get("sess-reconcile"); got != fresh {
		t.Fatal("manager did not publish the new session atomically")
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if !closed {
		t.Fatal("old session handle was not closed")
	}
}

// Concurrent Open calls for the same id must single-flight: through a shared
// Agent Host every open is a new connection that re-registers the sessionId
// routing, so parallel opens steal the survivor's update stream and leave the
// opened chat permanently empty (kimi sessions rendered blank).
func TestManagerOpenSingleFlightsConcurrentLoad(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	countFile := filepath.Join(dir, "loads")
	script := `import json,sys,time
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load":
  open(sys.argv[1],"a").write("x")
  time.sleep(0.2)
  result={"sessionId":m.get("params",{}).get("sessionId")}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path, countFile})
	defer manager.Close()
	const callers = 8
	sessions := make(chan *Session, callers)
	errs := make(chan error, callers)
	for range callers {
		go func() {
			session, err := manager.Open("sess-shared", "/work", 3*time.Second)
			if err != nil {
				errs <- err
				return
			}
			sessions <- session
		}()
	}
	var first *Session
	for range callers {
		select {
		case err := <-errs:
			t.Fatalf("concurrent open failed: %v", err)
		case session := <-sessions:
			if first == nil {
				first = session
			} else if session != first {
				t.Fatal("concurrent opens returned different session handles")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent open timed out")
		}
	}
	data, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatal(err)
	}
	if loads := len(data); loads != 1 {
		t.Fatalf("session/load ran %d times, want 1", loads)
	}
}

func TestManagerReopensAfterAgentExit(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
 if method == "session/load": sys.exit(0)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	first, err := manager.Open("sess-exit", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !first.IsClosed() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !first.IsClosed() {
		t.Fatal("agent exit did not invalidate the session")
	}
	second, err := manager.Open("sess-exit", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Open reused the exited session")
	}
}

// An opencode-style agent (no legacy modes object, "mode" advertised as a
// config option) must switch modes through session/set_config_option.
func TestSetModeFallsBackToModeConfigOption(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	options := `[{"id":"mode","category":"mode","currentValue":"build","options":[{"value":"build","name":"Build"},{"value":"plan","name":"Plan"}]}]`
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-mode","configOptions":` + options + `}
 elif method == "session/set_config_option":
  option=json.loads('` + options + `')[0]
  option["currentValue"]=m.get("params",{}).get("value")
  result={"configOptions":[option]}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	session, err := manager.Create("seed", "/work", "模式会话", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "build" {
		t.Fatalf("initial mode=%v", mode)
	}
	if err := session.SetMode("plan"); err != nil {
		t.Fatalf("set mode via config option: %v", err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "plan" {
		t.Fatalf("mode after switch=%v", mode)
	}
	if err := session.SetMode("nope"); err == nil {
		t.Fatal("unadvertised mode accepted")
	}
}

// dsh-shaped agent: the modes object and the "mode" config option are
// permission presets; the collaboration surface is the collaboration_mode
// config option, and session/set_mode must never be called.
func TestSetModeRoutesToDialectModeOption(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	options := `[{"id":"mode","category":"mode","currentValue":"workspace-write","options":[{"value":"read-only"},{"value":"workspace-write"},{"value":"danger-full-access"}]},` +
		`{"id":"collaboration_mode","currentValue":"default","options":[{"value":"default"},{"value":"plan"}]}]`
	modes := `{"currentModeId":"workspace-write","availableModes":[{"id":"read-only"},{"id":"workspace-write"},{"id":"danger-full-access"}]}`
	script := `import json,sys
OPTIONS=json.loads('` + options + `')
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-dsh","modes":json.loads('` + modes + `'),"configOptions":OPTIONS}
 elif method == "session/set_config_option":
  if m.get("params",{}).get("configId") != "collaboration_mode":
   result={"error":{"code":-32602,"message":"unexpected configId"}}
  else:
   option={"id":"collaboration_mode","currentValue":m.get("params",{}).get("value"),"options":[{"value":"default"},{"value":"plan"}]}
   result={"configOptions":[option]}
 elif method == "session/set_mode":
  result={"error":{"code":-32601,"message":"session/set_mode must not be called"}}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.ModeOptionID = "collaboration_mode"
	defer manager.Close()
	session, err := manager.Create("seed", "/work", "dsh 会话", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "default" {
		t.Fatalf("initial collaboration mode=%v (permission preset leaked?)", mode)
	}
	// Already current: succeeds without touching the agent.
	if err := session.SetMode("default"); err != nil {
		t.Fatalf("no-op set: %v", err)
	}
	if err := session.SetMode("plan"); err != nil {
		t.Fatalf("switch to plan: %v", err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "plan" {
		t.Fatalf("collaboration mode after switch=%v", mode)
	}
	if err := session.SetMode("default"); err != nil {
		t.Fatalf("switch back to default: %v", err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "default" {
		t.Fatalf("collaboration mode after switch back=%v", mode)
	}
	if err := session.SetMode("danger-full-access"); err == nil {
		t.Fatal("permission preset id accepted as a collaboration mode")
	}
}

// dsh only advertises collaboration_mode while its runtime carries a plan
// command; without the option the original refusal stands even though the
// permission-preset modes object exists.
func TestSetModeDialectOptionMissingRefused(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	options := `[{"id":"mode","category":"mode","currentValue":"workspace-write","options":[{"value":"read-only"},{"value":"workspace-write"},{"value":"danger-full-access"}]}]`
	modes := `{"currentModeId":"workspace-write","availableModes":[{"id":"read-only"},{"id":"workspace-write"},{"id":"danger-full-access"}]}`
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-dsh2","modes":json.loads('` + modes + `'),"configOptions":json.loads('` + options + `')}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.ModeOptionID = "collaboration_mode"
	defer manager.Close()
	session, err := manager.Create("seed", "/work", "dsh 会话", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.SetMode("default"); err == nil || err.Error() != "当前 Agent 不支持此工作模式" {
		t.Fatalf("missing collaboration_mode must refuse, got %v", err)
	}
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

func TestSessionTracksSessionInfoTitle(t *testing.T) {
	session := &Session{ID: "sess-1", Title: "旧标题", View: acpview.NewView("sess-1", "旧标题", "/work", func() float64 { return 1 }), changed: make(chan struct{})}
	message := jsoncompat.NewObject()
	message.Set("method", "session/update")
	params := jsoncompat.NewObject()
	params.Set("sessionUpdate", "session_info_update")
	params.Set("title", "自动标题")
	message.Set("params", params)
	session.onNotification(message)
	if session.Title != "自动标题" || session.Snapshot().MustGet("title") != "自动标题" {
		t.Fatalf("title was not synchronized: %q", session.Title)
	}
	params.Set("title", nil)
	session.onNotification(message)
	if session.Title != "" || session.Snapshot().MustGet("title") != "未命名聊天" {
		t.Fatalf("title was not cleared: %q", session.Title)
	}
}

func TestSessionCancelsOnlyRequestedReverseRequest(t *testing.T) {
	session := &Session{ID: "sess-1", View: acpview.NewView("sess-1", "Demo", "/work", func() float64 { return 1 }), changed: make(chan struct{})}
	addRequest := func(id any) {
		request := jsoncompat.NewObject()
		request.Set("id", id)
		request.Set("method", "session/request_permission")
		params := jsoncompat.NewObject()
		params.Set("toolCall", jsoncompat.NewObject())
		params.Set("options", &jsoncompat.Array{})
		request.Set("params", params)
		session.onRequest(request, func(any) {})
	}
	addRequest(jsoncompat.Num(7))
	addRequest("permission-keep")
	if len(session.Snapshot().MustGet("requests").(*jsoncompat.Array).Items) != 2 {
		t.Fatal("expected two pending requests")
	}
	cancel := jsoncompat.NewObject()
	cancel.Set("method", "$/cancel_request")
	params := jsoncompat.NewObject()
	params.Set("id", "missing")
	cancel.Set("params", params)
	sequence := session.Snapshot().MustGet("sequence")
	session.onNotification(cancel)
	if session.Snapshot().MustGet("sequence") != sequence {
		t.Fatal("unknown cancellation must not emit a change")
	}
	params.Set("id", jsoncompat.Num(7))
	session.onNotification(cancel)
	requests := session.Snapshot().MustGet("requests").(*jsoncompat.Array).Items
	if len(requests) != 1 || requests[0].(*jsoncompat.Object).MustGet("id") != "permission-keep" {
		t.Fatalf("wrong request was cancelled: %v", requests)
	}
	if _, exists := session.pendingIDs["permission-keep"]; !exists {
		t.Fatal("unrelated request is no longer answerable")
	}
}

func TestSessionAcceptsMcodeSteeredResult(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-mcode"}
 elif method == "mcode/session/steer":
  assert m["params"]["text"] == "继续补充"
  result={"turnId":"turn-1","mode":"steered"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	session, err := manager.Create("seed", "/work", "MiniMax", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	injected, err := session.Steer("mcode/session/steer", "继续补充")
	if err != nil || !injected {
		t.Fatalf("mcode steer injected=%v err=%v", injected, err)
	}
}

func TestBackgroundQuestionSurvivesPromptCompletion(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	answerLog := filepath.Join(t.TempDir(), "answers.log")
	script := `import json,sys
log = open(` + pythonRepr(answerLog) + `, "a")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method")
 if method is None and m.get("id") == "ask-background":
  log.write(json.dumps(m.get("result"))+"\n"); log.flush()
  continue
 ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-background"}
 elif method == "session/prompt":
  print(json.dumps({"jsonrpc":"2.0","id":"ask-background","method":"session/request_permission","params":{"sessionId":"sess-background","toolCall":{"toolCallId":"tool-q","title":"AskUserQuestion"},"options":[{"optionId":"q0_opt_0","name":"继续","kind":"allow_once"}]}}),flush=True)
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	session, err := manager.Create("seed", "/work", "后台提问", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Prompt("开始", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot := session.Snapshot()
		requests := snapshot.MustGet("requests").(*jsoncompat.Array).Items
		if snapshot.MustGet("status") == "idle" && len(requests) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background question did not survive prompt completion: %s", jsoncompat.EncodeCompact(snapshot))
		}
		time.Sleep(20 * time.Millisecond)
	}
	response := jsoncompat.NewObject()
	response.Set("optionId", "q0_opt_0")
	if err := session.Respond("ask-background", response); err != nil {
		t.Fatal(err)
	}
	for {
		raw, readErr := os.ReadFile(answerLog)
		if readErr == nil && len(raw) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background answer was not delivered: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRespondWireTranslatesPermissionReplies(t *testing.T) {
	params := jsoncompat.NewObject()
	options := &jsoncompat.Array{}
	for _, id := range []string{"q0_opt_0", "q0_opt_1", "q0_skip"} {
		option := jsoncompat.NewObject()
		option.Set("optionId", id)
		option.Set("kind", "allow_once")
		options.Items = append(options.Items, option)
	}
	params.Set("options", options)

	reply := func(pairs ...any) *jsoncompat.Object {
		object := jsoncompat.NewObject()
		for i := 0; i+1 < len(pairs); i += 2 {
			object.Set(pairs[i].(string), pairs[i+1])
		}
		return object
	}

	wire, err := respondWire("permission", params, reply("optionId", "q0_opt_1"))
	if err != nil {
		t.Fatalf("valid selection rejected: %v", err)
	}
	outcome, _ := wire.MustGet("outcome").(*jsoncompat.Object)
	if outcome == nil || outcome.MustGet("outcome") != "selected" || outcome.MustGet("optionId") != "q0_opt_1" {
		t.Fatalf("selection was not wrapped as ACP outcome: %s", jsoncompat.EncodeCompact(wire))
	}

	wire, err = respondWire("permission", params, reply("cancel", true))
	if err != nil {
		t.Fatalf("cancel rejected: %v", err)
	}
	outcome, _ = wire.MustGet("outcome").(*jsoncompat.Object)
	if outcome == nil || outcome.MustGet("outcome") != "cancelled" {
		t.Fatalf("cancel was not wrapped as ACP outcome: %s", jsoncompat.EncodeCompact(wire))
	}

	if _, err = respondWire("permission", params, reply("optionId", "unknown")); err == nil ||
		err.Error() != "请选择有效的选项" {
		t.Fatalf("unknown option must be refused, got %v", err)
	}
	if _, err = respondWire("permission", params, reply("decision", "accept")); err == nil ||
		err.Error() != "请求回应格式不正确" {
		t.Fatalf("extra keys must be refused, got %v", err)
	}

	accepted := reply("action", "accept")
	accepted.Set("content", reply("q0", "选项 A"))
	wire, err = respondWire("input", params, accepted)
	if err != nil || wire != accepted {
		t.Fatalf("elicitation reply must pass through verbatim: %v %v", wire, err)
	}
	if _, err = respondWire("input", params, reply("action", "maybe")); err == nil {
		t.Fatal("unknown elicitation action must be refused")
	}
}

func TestSessionDropsSiblingSessionTraffic(t *testing.T) {
	session := &Session{ID: "sess-1", View: acpview.NewView("sess-1", "Demo", "/work", func() float64 { return 1 }), pending: map[string]*jsoncompat.Object{}, pendingIDs: map[string]any{}, pendingKinds: map[string]string{}, changed: make(chan struct{})}
	chunk := func(sid string) *jsoncompat.Object {
		update := jsoncompat.NewObject()
		update.Set("method", "session/update")
		params := jsoncompat.NewObject()
		if sid != "" {
			params.Set("sessionId", sid)
		}
		params.Set("sessionUpdate", "agent_message_chunk")
		content := jsoncompat.NewObject()
		content.Set("text", "hello")
		params.Set("content", content)
		update.Set("params", params)
		return update
	}
	// A shared agent host broadcasts every child's notifications to all
	// gateway connections; updates addressed to a sibling must not fold.
	session.onNotification(chunk("sess-2"))
	if turns := session.Snapshot().MustGet("turns").(*jsoncompat.Array); len(turns.Items) != 0 {
		t.Fatal("sibling session update folded into this view")
	}
	session.onNotification(chunk("sess-1"))
	if turns := session.Snapshot().MustGet("turns").(*jsoncompat.Array); len(turns.Items) != 1 {
		t.Fatal("own session update was dropped")
	}
	// A reverse request for another session is answered null and never shown.
	responded := false
	request := jsoncompat.NewObject()
	request.Set("id", "permission-foreign")
	request.Set("method", "session/request_permission")
	params := jsoncompat.NewObject()
	params.Set("sessionId", "sess-2")
	request.Set("params", params)
	session.onRequest(request, func(any) { responded = true })
	if !responded {
		t.Fatal("foreign request left unanswered")
	}
	if len(session.Snapshot().MustGet("requests").(*jsoncompat.Array).Items) != 0 {
		t.Fatal("foreign permission request surfaced in this chat")
	}
}

func TestOwnsUpdateAcceptsCanonicalBackendID(t *testing.T) {
	session := &Session{ID: "placeholder-1", canonical: func(id string) string {
		if id == "placeholder-1" {
			return "sess-backend"
		}
		return ""
	}}
	params := jsoncompat.NewObject()
	params.Set("sessionId", "sess-backend")
	if !session.ownsUpdate(params) {
		t.Fatal("canonical backend id rejected")
	}
	params.Set("sessionId", "sess-other")
	if session.ownsUpdate(params) {
		t.Fatal("unrelated session id accepted")
	}
}
