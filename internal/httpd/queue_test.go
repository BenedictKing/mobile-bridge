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
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// queueAgent is a python ACP agent whose prompts are recorded; a prompt
// starting with "hold" keeps the turn active for ~1.5s so the test can queue
// a message mid-run and watch the settle hook dispatch it.
func queueAgent(t *testing.T, dir string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys,time
DIR = "` + dir + `"
def record(name, value):
 with open(os.path.join(DIR, name), "a") as fh:
  fh.write(value + "\n")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new": result={"sessionId":"km-queue","modes":{"currentModeId":"default","availableModes":[{"id":"default"}]},"configOptions":[]}
 elif method == "session/prompt":
  blocks=(m.get("params") or {}).get("prompt") or []
  text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
  record("prompts.txt", text)
  if text.startswith("hold"):
   time.sleep(1.5)
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func newFeatureServer(t *testing.T, dataDir string) (*Server, string, string) {
	t.Helper()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8791, "instance")
	server.Queue = LoadSubmissions(filepath.Join(dataDir, "submissions.json"))
	server.Hidden = LoadHiddenSessions(filepath.Join(dataDir, "hidden-sessions.json"))
	server.Forks = LoadForks(filepath.Join(dataDir, "forks.json"))
	return server, token, csrf
}

func featureRequest(server *Server, token, csrf, method, target, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://127.0.0.1:8791"+target, strings.NewReader(body))
	request.Host = "127.0.0.1:8791"
	request.Header.Set("Origin", "http://127.0.0.1:8791")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", csrf)
	}
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestSubmissionsLedgerTransitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "submissions.json")
	ledger := LoadSubmissions(path)
	entry, duplicate, err := ledger.enqueue("thread-1", "6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"你好", "queue", "", nil, nil, nil)
	if err != nil || duplicate || entry.Status != SubQueued {
		t.Fatalf("enqueue: entry=%+v duplicate=%v err=%v", entry, duplicate, err)
	}
	if _, duplicate, err := ledger.enqueue("thread-1", entry.ID, "你好", "queue", "", nil, nil, nil); err != nil || !duplicate {
		t.Fatalf("duplicate expected: duplicate=%v err=%v", duplicate, err)
	}
	if _, _, err := ledger.enqueue("thread-1", entry.ID, "别的", "queue", "", nil, nil, nil); err == nil {
		t.Fatal("conflicting content must be rejected")
	}
	// Uncertain outcomes climb the attempt ladder; the tenth fails.
	for i := 0; i < 9; i++ {
		ledger.setStatus("thread-1", entry.ID, SubUnknown)
		ledger.requeueEntry("thread-1", entry.ID, "超时")
		if status := ledger.get("thread-1", entry.ID).Status; status != SubQueued {
			t.Fatalf("attempt %d: status=%s", i, status)
		}
	}
	ledger.setStatus("thread-1", entry.ID, SubUnknown)
	ledger.requeueEntry("thread-1", entry.ID, "超时")
	failed := ledger.get("thread-1", entry.ID)
	if failed.Status != SubFailed || failed.LastQueueError != "超时" || failed.QueueAttempts != 10 {
		t.Fatalf("ladder: %+v", failed)
	}
	// Editing a failed entry requeues it and clears the ladder.
	edited, err := ledger.edit("thread-1", entry.ID, "改后的消息")
	if err != nil || edited.Status != SubQueued || edited.QueueAttempts != 0 || edited.LastQueueError != "" {
		t.Fatalf("edit: %+v err=%v", edited, err)
	}
	if _, err := ledger.edit("thread-1", entry.ID, "  "); err == nil {
		t.Fatal("blank edit must be rejected")
	}
	// Cancel terminal for queued entries; meta only shows live statuses.
	if err := ledger.cancel("thread-1", entry.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if err := ledger.cancel("thread-1", entry.ID); err == nil {
		t.Fatal("second cancel must report the entry gone")
	}
	unknownID := "7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	ledger.enqueue("thread-1", unknownID, "不确定", "send", "", nil, nil, nil)
	if err := ledger.ignore("thread-1", unknownID); err != nil {
		t.Fatalf("ignore unknown: %v", err)
	}
	if err := ledger.ignore("thread-1", unknownID); err != nil {
		t.Fatalf("ignore twice: %v", err)
	}
	if err := ledger.ignore("thread-1", "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); err == nil {
		t.Fatal("ignoring a missing entry must fail")
	}
	ledger.enqueue("thread-1", "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "带附件", "queue", "",
		nil, []string{"up-1"}, []string{"截图.png"})
	rows := ledger.meta("thread-1")
	if len(rows.Items) != 1 {
		t.Fatalf("meta rows=%s", jsoncompat.EncodeCompact(rows))
	}
	row, _ := rows.Items[0].(*jsoncompat.Object)
	encoded := string(jsoncompat.EncodeCompact(row))
	for _, needle := range []string{`"id":"9aaaaaaa`, `"text":"带附件"`, `"status":"queued"`, `"editable":true`, `"attachments":["截图.png"]`} {
		if !strings.Contains(encoded, needle) {
			t.Fatalf("meta missing %s in %s", needle, encoded)
		}
	}
	// Persistence round-trip.
	reloaded := LoadSubmissions(path)
	if reloaded.get("thread-1", unknownID) == nil || reloaded.get("thread-1", unknownID).Status != SubIgnored {
		t.Fatalf("reload lost entries: %s", jsoncompat.EncodeCompact(reloaded.meta("thread-1")))
	}
	// The queued "带附件" entry keeps its thread dispatchable after a reload.
	if got := reloaded.queuedThreads(); len(got) != 1 || got[0] != "thread-1" {
		t.Fatalf("queuedThreads=%v", got)
	}
}

func TestTimelineReadFallbackKeepsQueueMeta(t *testing.T) {
	server, _, _ := newFeatureServer(t, t.TempDir())
	_, _, err := server.Queue.enqueue("cold-thread", "5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"切回后仍应显示", "queue", "", nil, nil, nil)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	encoded := string(jsoncompat.EncodeCompact(server.TimelineRead("cold-thread", "page")))
	if !strings.Contains(encoded, `"text":"切回后仍应显示"`) || !strings.Contains(encoded, `"status":"queued"`) {
		t.Fatalf("cold fallback lost queue meta=%s", encoded)
	}
}

func TestACPQueueDispatchOnSettle(t *testing.T) {
	dir := t.TempDir()
	agentDir := t.TempDir()
	manager := acpruntime.NewCommand(queueAgent(t, agentDir))
	manager.Label = "Kimi"
	defer manager.Close()

	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	// Wire the settle hook the way main.go chains it.
	manager.AutoContinue = func(session *acpruntime.Session) {
		server.QueueSettleACP("kimi", session)
	}

	request := func(method, target, body string) *httptest.ResponseRecorder {
		return featureRequest(server, token, csrf, method, target, body)
	}
	create := request(http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":"`+dir+`","title":"排队会话","id":"1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), "km-queue") {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}

	// A running turn + a queued send: the queue card shows up in meta.
	if send := request(http.MethodPost, "/api/sessions/km-queue/send", `{"text":"hold","mode":"send","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`); send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	queued := request(http.MethodPost, "/api/sessions/km-queue/send",
		`{"text":"排队消息","mode":"queue","id":"3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if queued.Code != http.StatusOK || !strings.Contains(queued.Body.String(), `"status": "queued"`) {
		t.Fatalf("queue send status=%d body=%s", queued.Code, queued.Body.String())
	}
	meta := request(http.MethodGet, "/api/sessions/km-queue/timeline?limit=5", "")
	if meta.Code != http.StatusOK || !strings.Contains(meta.Body.String(), `"text": "排队消息"`) ||
		!strings.Contains(meta.Body.String(), `"status": "queued"`) {
		t.Fatalf("timeline meta=%s", meta.Body.String())
	}
	fallback := string(jsoncompat.EncodeCompact(server.TimelineRead("km-queue", "page")))
	if !strings.Contains(fallback, `"text":"排队消息"`) || !strings.Contains(fallback, `"status":"queued"`) {
		t.Fatalf("fallback timeline lost queue meta=%s", fallback)
	}

	// The turn settles and the settle hook dispatches the queued message.
	deadline := time.Now().Add(8 * time.Second)
	for {
		if strings.Contains(recordedFile(agentDir, "prompts.txt"), "排队消息") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued message never dispatched; prompts=%s", recordedFile(agentDir, "prompts.txt"))
		}
		time.Sleep(120 * time.Millisecond)
	}
	// Accepted entries leave the queue cards (the dispatched turn itself is
	// a timeline row now).
	meta = request(http.MethodGet, "/api/sessions/km-queue/timeline?limit=5", "")
	if !strings.Contains(meta.Body.String(), `"submissions": []`) {
		t.Fatalf("accepted entry still in meta: %s", meta.Body.String())
	}

	// Wait for the second turn to settle, then detach the settle hook so the
	// action tests below run without the automatic dispatch racing them.
	deadline = time.Now().Add(8 * time.Second)
	for {
		if snapshot := manager.Get("km-queue"); snapshot != nil && snapshot.Snapshot().MustGet("status") != "active" &&
			strings.Contains(recordedFile(agentDir, "prompts.txt"), "排队消息\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second turn never settled; prompts=%s", recordedFile(agentDir, "prompts.txt"))
		}
		time.Sleep(120 * time.Millisecond)
	}
	manager.AutoContinue = nil

	// queue actions: edit then delete.
	second := request(http.MethodPost, "/api/sessions/km-queue/send",
		`{"text":"待编辑","mode":"queue","id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second queue status=%d", second.Code)
	}
	edited := request(http.MethodPost, "/api/sessions/km-queue/queue",
		`{"id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","action":"edit","text":"编辑后"}`)
	if edited.Code != http.StatusOK || !strings.Contains(edited.Body.String(), `"text": "编辑后"`) {
		t.Fatalf("edit status=%d body=%s", edited.Code, edited.Body.String())
	}
	removed := request(http.MethodPost, "/api/sessions/km-queue/queue",
		`{"id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","action":"delete"}`)
	if removed.Code != http.StatusOK || !strings.Contains(removed.Body.String(), "cancelled") {
		t.Fatalf("delete status=%d body=%s", removed.Code, removed.Body.String())
	}
	gone := request(http.MethodPost, "/api/sessions/km-queue/queue",
		`{"id":"4aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","action":"send"}`)
	if gone.Code != http.StatusConflict || !strings.Contains(gone.Body.String(), "已离开队列") {
		t.Fatalf("send-now gone status=%d body=%s", gone.Code, gone.Body.String())
	}

	// The fork-origin ledger reaches timeline meta (来源会话 link).
	server.Forks.Record("km-queue", forkOrigin{ID: "source-thread", Host: "local", Title: "来源会话"})
	meta = request(http.MethodGet, "/api/sessions/km-queue/timeline?limit=5", "")
	if !strings.Contains(meta.Body.String(), `"forkedFrom": {"id": "source-thread", "host": "local", "title": "来源会话"}`) {
		t.Fatalf("forkedFrom meta missing: %s", meta.Body.String())
	}

	// A duplicate send replays the ledger instead of prompting again.
	duplicate := request(http.MethodPost, "/api/sessions/km-queue/send",
		`{"text":"排队消息","mode":"queue","id":"3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"duplicate": true`) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	// Ignore-submission wires the uncertain-outcome hide.
	server.Queue.enqueue("km-queue", "5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "不确定", "send", "", nil, nil, nil)
	ignore := request(http.MethodPost, "/api/accounts/ignore-submission",
		`{"threadId":"km-queue","submissionId":"5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","host":"local"}`)
	if ignore.Code != http.StatusOK {
		t.Fatalf("ignore status=%d body=%s", ignore.Code, ignore.Body.String())
	}
	if entry := server.Queue.get("km-queue", "5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); entry == nil || entry.Status != SubIgnored {
		t.Fatalf("ignore did not land: %+v", entry)
	}
}

// TestSessionDeleteHides covers the ACP hide path end to end: list, activity
// dots, the detail view and persistence of the hide list.
func TestSessionDeleteHides(t *testing.T) {
	dir := t.TempDir()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list":
  result={"sessions":[{"sessionId":"km-gone","title":"要删除的会话","cwd":"` + dir + `"}]}
 elif method == "session/new": result={"sessionId":"km-new"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", path})
	defer manager.Close()

	dataDir := t.TempDir()
	server, token, csrf := newFeatureServer(t, dataDir)
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}

	request := func(method, target, body string) *httptest.ResponseRecorder {
		return featureRequest(server, token, csrf, method, target, body)
	}
	if list := request(http.MethodGet, "/api/sessions", ""); list.Code != http.StatusOK ||
		!strings.Contains(list.Body.String(), "km-gone") {
		t.Fatalf("list before delete=%d %s", list.Code, list.Body.String())
	}
	deleted := request(http.MethodPost, "/api/sessions/km-gone/delete", `{}`)
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"mode": "hidden"`) {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if list := request(http.MethodGet, "/api/sessions", ""); strings.Contains(list.Body.String(), "km-gone") {
		t.Fatalf("hidden session still listed: %s", list.Body.String())
	}
	activity := request(http.MethodPost, "/api/activity", `{"hosts":{"local":["km-gone"]}}`)
	if strings.Contains(activity.Body.String(), "km-gone") {
		t.Fatalf("hidden session still in activity: %s", activity.Body.String())
	}
	if view := request(http.MethodGet, "/api/sessions/km-gone", ""); view.Code != http.StatusNotFound {
		t.Fatalf("hidden view status=%d body=%s", view.Code, view.Body.String())
	}
	// The hide list persists across reloads.
	if reloaded := LoadHiddenSessions(filepath.Join(dataDir, "hidden-sessions.json")); !reloaded.Hidden("km-gone") {
		t.Fatal("hide list did not persist")
	}
	// A queued entry of the deleted thread is dropped with it.
	server.Queue.enqueue("km-gone", "6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "未派发", "queue", "", nil, nil, nil)
	if again := request(http.MethodPost, "/api/sessions/km-gone/delete", `{}`); again.Code != http.StatusOK {
		t.Fatalf("second delete status=%d", again.Code)
	}
	if entry := server.Queue.get("km-gone", "6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); entry != nil {
		t.Fatalf("queued entry survived delete: %+v", entry)
	}
}

// TestCodexDeleteAndForkRuntime covers the short-lived app-server operations
// against a fake codex binary: thread/delete succeeds, and thread/fork is
// gated by the ThreadForkParams schema check.
func TestCodexDeleteAndForkRuntime(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,os,sys
args=sys.argv[1:]
if "generate-json-schema" in args:
 out=args[args.index("--out")+1]
 os.makedirs(os.path.join(out,"v2"),exist_ok=True)
 with open(os.path.join(out,"v2","ThreadForkParams.json"),"w") as fh:
  json.dump({"properties":{"threadId":{},"lastTurnId":{},"deferGoalContinuation":{}}},fh)
 sys.exit(0)
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize": result={"ok":True}
 elif method=="thread/delete": result={}
 elif method=="thread/fork":
  params=m.get("params") or {}
  result={"thread":{"id":"7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","turns":[{"id":params.get("lastTurnId")}]},
          "modelProvider":params.get("modelProvider")}
 elif method=="thread/name/set": result={"ok":True}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	executable := filepath.Join(t.TempDir(), "app-server")
	if err := os.WriteFile(executable, []byte("#!"+python+"\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("chmod", "+x", executable).Run(); err != nil {
		t.Fatal(err)
	}
	bridge := codex.New(t.TempDir(), "unused-endpoint")
	bridge.Executable = executable

	if err := bridge.DeleteThread("8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	child, err := bridge.ForkThread("9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "turn-1", ".", "分支", map[string]any{"modelProvider": "openai"})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child != "7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" {
		t.Fatalf("fork child=%s", child)
	}
	// A runtime without the fork schema fields refuses to fork.
	limited := filepath.Join(t.TempDir(), "limited-codex")
	if err := os.WriteFile(limited, []byte("#!"+python+"\nimport sys\nsys.exit(0)\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("chmod", "+x", limited).Run(); err != nil {
		t.Fatal(err)
	}
	bridge.Executable = limited
	if _, err := bridge.ForkThread("9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "turn-1", ".", "分支", nil); err == nil ||
		!strings.Contains(err.Error(), "暂不支持安全分支") {
		t.Fatalf("schema gate: %v", err)
	}
}

func recordedFile(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return string(raw)
}

var _ = json.Marshal

// A panicking dispatch pass must never take the gateway down: the background
// worker recovers and the next tick retries (a nil Queue used to be
// process-fatal from the ticker goroutine).
func TestSafeDispatchQueuedPassRecovers(t *testing.T) {
	server := &Server{}
	server.safeDispatchQueuedPass() // s.Queue is nil: panics inside, recovers
}
