package httpd

import (
	"database/sql"
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
	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	_ "modernc.org/sqlite"
)

// fakeGoalRuntime speaks just enough app-server for the goal sidecar and
// keeps goals_1.sqlite authoritative, like the desktop does.
const fakeGoalRuntimeScript = `#!/usr/bin/env python3
import json, os, sqlite3, sys
home = os.environ.get("CODEX_HOME", "")
db_path = os.path.join(home, "goals_1.sqlite")
def ensure():
    conn = sqlite3.connect(db_path)
    conn.execute("CREATE TABLE IF NOT EXISTS thread_goals (thread_id TEXT PRIMARY KEY, objective TEXT, status TEXT, goal_id TEXT, token_budget INTEGER, tokens_used INTEGER, time_used_seconds INTEGER, created_at_ms INTEGER, updated_at_ms INTEGER)")
    conn.commit()
    return conn
def row(conn, thread_id):
    return conn.execute("SELECT objective, status, goal_id, created_at_ms FROM thread_goals WHERE thread_id = ?", (thread_id,)).fetchone()
for line in sys.stdin:
    message = json.loads(line)
    method, ident, params = message.get("method"), message.get("id"), message.get("params") or {}
    result = {}
    conn = ensure()
    if method == "initialize":
        result = {}
    elif method == "thread/goal/set":
        conn.execute("INSERT INTO thread_goals (thread_id, objective, status, goal_id, created_at_ms, updated_at_ms) VALUES (?, ?, ?, ?, strftime('%s','now')*1000, strftime('%s','now')*1000) ON CONFLICT(thread_id) DO UPDATE SET objective=excluded.objective, status=excluded.status, updated_at_ms=excluded.updated_at_ms",
                     (params["threadId"], params.get("objective", ""), params.get("status", "active"), "g-1"))
        conn.commit()
        result = {"goal": {"objective": params.get("objective", ""), "status": params.get("status", "active"), "goalId": "g-1"}}
    elif method == "thread/goal/clear":
        conn.execute("DELETE FROM thread_goals WHERE thread_id = ?", (params["threadId"],))
        conn.commit()
        result = {}
    elif method == "thread/goal/get":
        found = row(conn, params["threadId"])
        if found:
            result = {"goal": {"objective": found[0], "status": found[1], "goalId": found[2] or "g-1"}}
        else:
            result = {}
    conn.close()
    print(json.dumps({"jsonrpc": "2.0", "id": ident, "result": result}), flush=True)
`

type goalFixture struct {
	server *Server
	home   string
}

func newGoalFixture(t *testing.T) *goalFixture {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	home := t.TempDir()
	dataDir := t.TempDir()
	runtimePath := filepath.Join(t.TempDir(), "goal-runtime.py")
	if err := os.WriteFile(runtimePath, []byte(fakeGoalRuntimeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "goals_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY, objective TEXT, status TEXT, goal_id TEXT, token_budget INTEGER, tokens_used INTEGER, time_used_seconds INTEGER, created_at_ms INTEGER, updated_at_ms INTEGER)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8794, "instance")
	server.Goal = goal.New(home, runtimePath)
	_ = python // shebang makes the script directly executable
	server.GoalCmds = NewGoalLedger(filepath.Join(dataDir, "goal-commands.json"))
	server.Queue = LoadSubmissions(filepath.Join(dataDir, "submissions.json"))
	return &goalFixture{server: server, home: home}
}

// Create → confirm → activation enqueued; duplicate create replays confirmed;
// pause/resume/cancel ride the same machine; the overlay carries every field
// modes.js renders and hides activation ids from the visible queue.
func TestGoalCommandChain(t *testing.T) {
	fixture := newGoalFixture(t)
	const threadID = "11111111-2222-4333-8444-555555555555"
	transport := fixture.server.goalTransportFor(threadID)
	if transport == nil {
		t.Fatal("codex sidecar transport missing")
	}

	outcome, err := fixture.server.runGoalCommand(transport, threadID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "create", "整理仓库", "", "zh-CN")
	if err != nil || outcome == nil || !outcome.confirmed {
		t.Fatalf("create: err=%v outcome=%+v", err, outcome)
	}
	created, _ := outcome.response.MustGet("result").(*jsoncompat.Object)
	if created == nil || created.MustGet("status") != "active" {
		t.Fatalf("create result wrong: %s", jsoncompat.EncodeCompact(outcome.response))
	}
	activation := fixture.server.sendGoalActivation(threadID, outcome, "create", "zh-CN")
	if activation == nil {
		t.Fatal("confirmed create must fan out an activation turn")
	}
	activationID, _ := activation.MustGet("id").(string)
	if activationID == "" {
		t.Fatal("activation id missing")
	}
	if entry := fixture.server.Queue.get(threadID, activationID); entry == nil ||
		entry.Text != "目标已创建。请立即开始执行当前目标，不要重复创建目标。" {
		t.Fatalf("activation not queued: %+v", entry)
	}

	// A second create with a new request id plans duplicate against the
	// native row and replays as confirmed+duplicate.
	again, err := fixture.server.runGoalCommand(transport, threadID, "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee", "create", "整理仓库", "", "")
	if err != nil || again == nil || !again.confirmed || !again.duplicate {
		t.Fatalf("duplicate create: err=%v outcome=%+v", err, again)
	}

	// Control verbs: pause → resume (activation again) → cancel (queued
	// activations retired).
	paused, err := fixture.server.runGoalCommand(transport, threadID, "cccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee", "pause", "整理仓库", "paused", "")
	if err != nil || !paused.confirmed {
		t.Fatalf("pause: err=%v", err)
	}
	resumed, err := fixture.server.runGoalCommand(transport, threadID, "dddddddd-bbbb-4ccc-8ddd-eeeeeeeeeeee", "resume", "整理仓库", "active", "")
	if err != nil || !resumed.confirmed {
		t.Fatalf("resume: err=%v", err)
	}
	if activation := fixture.server.sendGoalActivation(threadID, resumed, "resume", ""); activation == nil {
		t.Fatal("resume must fan out an activation turn")
	}
	cancelled, err := fixture.server.runGoalCommand(transport, threadID, "eeeeeeee-bbbb-4ccc-8ddd-eeeeeeeeeeee", "cancel", "整理仓库", "", "")
	if err != nil || !cancelled.confirmed {
		t.Fatalf("cancel: err=%v", err)
	}
	fixture.server.cancelGoalActivationQueues(threadID)
	for _, command := range fixture.server.GoalCmds.forThread(threadID) {
		_ = command
	}
	if native := goal.ReadNativeGoal(fixture.home, threadID); native != nil {
		t.Fatalf("goal must be gone after cancel: %s", jsoncompat.EncodeCompact(native))
	}

	// Overlay: every field the phone renders is present and typed.
	view := jsoncompat.NewObject()
	view.Set("cwd", "/work")
	fixture.server.applyGoalOverlay(threadID, view)
	if view.MustGet("goalRuntimeAvailable") != true {
		t.Fatal("goalRuntimeAvailable missing")
	}
	if ids, _ := view.MustGet("goalActivationIds").(*jsoncompat.Array); ids == nil || len(ids.Items) < 2 {
		t.Fatalf("goalActivationIds wrong: %v", ids)
	}
	if submission := view.MustGet("goalSubmission"); submission != nil {
		t.Fatalf("no pending submission after confirmed cancel: %v", submission)
	}
	// The visible queue hides activation ids.
	if visible := fixture.server.queueMetaExcludingActivations(threadID); len(visible.Items) != 0 {
		t.Fatalf("activation turns must stay out of the queue cards: %s", jsoncompat.EncodeCompact(visible))
	}
}

// The send route's workMode goal branch and the /goal control routes speak
// the command machine through HTTP.
func TestGoalRoutesRideCommandMachine(t *testing.T) {
	fixture := newGoalFixture(t)
	const threadID = "21111111-2222-4333-8444-555555555555"
	token, session, err := fixture.server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	post := func(target, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8794"+target, strings.NewReader(body))
		request.Host = "127.0.0.1:8794"
		request.Header.Set("Origin", "http://127.0.0.1:8794")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", csrf)
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		fixture.server.ServeHTTP(response, request)
		return response
	}
	// Goals are created through the send chain (runGoalCommand); the panel
	// routes only control an existing goal.
	routeTransport := fixture.server.goalTransportFor(threadID)
	if routeTransport == nil {
		t.Fatal("codex sidecar transport missing")
	}
	if outcome, err := fixture.server.runGoalCommand(routeTransport, threadID, "ffffffff-bbbb-4ccc-8ddd-eeeeeeeeeeee", "create", "写周报", "", ""); err != nil || !outcome.confirmed {
		t.Fatalf("create: %v", err)
	}
	paused := post("/api/sessions/"+threadID+"/goal/status", `{"status":"paused"}`)
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"status": "paused"`) {
		t.Fatalf("goal pause route: code=%d body=%s", paused.Code, paused.Body.String())
	}
	resumed := post("/api/sessions/"+threadID+"/goal/status", `{"status":"active"}`)
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), `"status": "active"`) {
		t.Fatalf("goal resume route: code=%d body=%s", resumed.Code, resumed.Body.String())
	}
	if native := goal.ReadNativeGoal(fixture.home, threadID); native == nil ||
		native.MustGet("objective") != "写周报" {
		t.Fatalf("native goal missing after route: %s", jsoncompat.EncodeCompact(native))
	}
	cancelled := post("/api/sessions/"+threadID+"/goal/cancel", `{"objective":"写周报"}`)
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status": "cancelled"`) {
		t.Fatalf("goal cancel: code=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	if native := goal.ReadNativeGoal(fixture.home, threadID); native != nil {
		t.Fatalf("goal must clear: %s", jsoncompat.EncodeCompact(native))
	}
}

// mcodeGoalAgent is the fake MiniMax ACP agent: an in-process goal store
// behind mcode/session/goal/*, prompts recorded so the tests can prove no
// activation turn ever rides a goal command, and a startup marker so the
// read-never-spawns guarantee is observable.
func mcodeGoalAgent(t *testing.T, dir string) []string {
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
record("started.txt", "yes")
GOALS = {}
for line in sys.stdin:
 m = json.loads(line); method = m.get("method"); ident = m.get("id"); params = m.get("params") or {}
 if method == "initialize":
  result = {"protocolVersion": 1, "agentCapabilities": {}, "authMethods": []}
 elif method == "session/list":
  result = {"sessions": []}
 elif method == "session/new":
  result = {"sessionId": "mc-goal", "modes": {"currentModeId": "default", "availableModes": [{"id": "default"}]}, "configOptions": []}
 elif method == "session/load":
  result = {}
 elif method == "session/prompt":
  blocks = params.get("prompt") or []
  text = next((b.get("text", "") for b in blocks if isinstance(b, dict)), "")
  record("prompts.txt", text)
  result = {"stopReason": "end_turn"}
 elif method == "mcode/session/goal/get":
  goal = GOALS.get(params.get("sessionId"))
  result = {"goal": goal} if goal else {}
 elif method == "mcode/session/goal/create":
  goal = {"objective": params.get("objective", ""), "status": "active", "goalId": "g-mc"}
  GOALS[params.get("sessionId")] = goal
  result = {"goal": dict(goal)}
 elif method == "mcode/session/goal/patch":
  goal = GOALS.get(params.get("sessionId")) or {}
  if "status" in params:
   goal["status"] = params["status"]
  if "objective" in params:
   goal["objective"] = params["objective"]
  GOALS[params.get("sessionId")] = goal
  result = {"goal": dict(goal)}
 elif method == "mcode/session/goal/clear":
  GOALS.pop(params.get("sessionId"), None)
  result = {}
 else:
  result = {}
 print(json.dumps({"jsonrpc": "2.0", "id": ident, "result": result}), flush=True)
`
	path := filepath.Join(t.TempDir(), "mcode-agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// The mcode transport rides the same command machine over ACP RPCs: send
// with workMode goal confirms through the ledger, no activation turn is ever
// enqueued (mcode's goal/create starts the turn itself), the view overlay
// carries the panel fields with the goals capability advertised, and the
// panel controls work pause → resume → cancel with cancel confirmed by
// absence.
func TestMcodeGoalChain(t *testing.T) {
	dir := t.TempDir()
	dataDir := t.TempDir()
	manager := acpruntime.NewCommand(mcodeGoalAgent(t, dir))
	manager.Label = "MiniMax"
	manager.Capabilities = []string{"goals", "steer", "fork"}
	manager.SteerMethod = "mcode/session/steer"
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"mcode": manager}
	server.GoalCmds = NewGoalLedger(filepath.Join(dataDir, "goal-commands.json"))
	server.Queue = LoadSubmissions(filepath.Join(dataDir, "submissions.json"))
	// server.Goal stays nil on purpose: the mcode transport must never need
	// the codex sidecar.

	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httpRequest := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		httpRequest.Host = "127.0.0.1:8790"
		httpRequest.Header.Set("Origin", "http://127.0.0.1:8790")
		if body != "" {
			httpRequest.Header.Set("Content-Type", "application/json")
			httpRequest.Header.Set("X-CSRF-Token", csrf)
		}
		httpRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httpRequest)
		return response
	}

	created := request(http.MethodPost, "/api/sessions",
		`{"agent":"mcode","cwd":`+jsonText(dir)+`,"title":"目标会话","id":"3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK || !strings.Contains(created.Body.String(), `"id": "mc-goal"`) {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}

	send := request(http.MethodPost, "/api/sessions/mc-goal/send",
		`{"text":"整理仓库","workMode":"goal","id":"3bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee","uiLocale":"zh-CN"}`)
	if send.Code != http.StatusOK || !strings.Contains(send.Body.String(), `"confirmed": true`) {
		t.Fatalf("goal send status=%d body=%s", send.Code, send.Body.String())
	}
	if strings.Contains(send.Body.String(), "activation") {
		t.Fatalf("mcode must not fan out an activation turn: %s", send.Body.String())
	}
	time.Sleep(150 * time.Millisecond)
	if recorded(t, dir, "prompts.txt") != "" {
		t.Fatalf("goal create must not enqueue a turn, prompt recorded=%q", recorded(t, dir, "prompts.txt"))
	}
	confirmed := false
	for _, command := range server.GoalCmds.forThread("mc-goal") {
		if state, _ := command.MustGet("state").(string); state == "confirmed" {
			confirmed = true
			break
		}
	}
	if !confirmed {
		t.Fatalf("no confirmed ledger entry: %s", jsoncompat.EncodeCompact(server.GoalCmds.forThread("mc-goal")))
	}

	// The live view overlay: goal row from the agent RPC, runtime flag, the
	// capability bit now that the transport exists.
	view := server.acpViewState("mcode", manager, manager.Get("mc-goal"))
	if view.MustGet("goalRuntimeAvailable") != true {
		t.Fatalf("goalRuntimeAvailable missing: %s", jsoncompat.EncodeCompact(view))
	}
	if goalValue, ok := view.MustGet("goal").(*jsoncompat.Object); !ok ||
		goalValue.MustGet("objective") != "整理仓库" || goalValue.MustGet("status") != "active" {
		t.Fatalf("view goal wrong: %s", jsoncompat.EncodeCompact(view.MustGet("goal")))
	}
	declared := false
	if caps, ok := view.MustGet("capabilities").(*jsoncompat.Array); ok {
		for _, item := range caps.Items {
			if item == "goals" {
				declared = true
			}
		}
	}
	if !declared {
		t.Fatalf("goals capability missing: %s", jsoncompat.EncodeCompact(view.MustGet("capabilities")))
	}

	// Timeline meta carries the overlay to the phone.
	timeline := request(http.MethodGet, "/api/sessions/mc-goal/timeline?limit=5", "")
	for _, needle := range []string{`"goalRuntimeAvailable": true`, `"objective": "整理仓库"`, `"goalActivationIds": []`} {
		if !strings.Contains(timeline.Body.String(), needle) {
			t.Fatalf("timeline meta missing %s: %s", needle, timeline.Body.String())
		}
	}

	// A second create plans duplicate against the agent's goal; a different
	// objective is refused by the decision matrix.
	duplicate := request(http.MethodPost, "/api/sessions/mc-goal/send",
		`{"text":"整理仓库","workMode":"goal","id":"3ccccccc-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"duplicate": true`) {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	conflict := request(http.MethodPost, "/api/sessions/mc-goal/send",
		`{"text":"别的目标","workMode":"goal","id":"3ddddddd-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "此聊天已有未完成目标") {
		t.Fatalf("conflicting create status=%d body=%s", conflict.Code, conflict.Body.String())
	}

	// Panel controls ride the command machine through the agent RPC.
	paused := request(http.MethodPost, "/api/sessions/mc-goal/goal/status", `{"status":"paused"}`)
	if paused.Code != http.StatusOK || !strings.Contains(paused.Body.String(), `"status": "paused"`) {
		t.Fatalf("pause status=%d body=%s", paused.Code, paused.Body.String())
	}
	resumed := request(http.MethodPost, "/api/sessions/mc-goal/goal/status", `{"status":"active"}`)
	if resumed.Code != http.StatusOK || !strings.Contains(resumed.Body.String(), `"status": "active"`) {
		t.Fatalf("resume status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	cancelled := request(http.MethodPost, "/api/sessions/mc-goal/goal/cancel", `{"objective":"整理仓库"}`)
	if cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status": "cancelled"`) {
		t.Fatalf("cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())
	}
	// Absence confirms the cancel and the fresh view reads nil; no turn was
	// ever prompted for any of it.
	view = server.acpViewState("mcode", manager, manager.Get("mc-goal"))
	if view.MustGet("goal") != nil {
		t.Fatalf("goal must be gone after cancel: %s", jsoncompat.EncodeCompact(view.MustGet("goal")))
	}
	if recorded(t, dir, "prompts.txt") != "" {
		t.Fatalf("panel controls must not enqueue turns: %q", recorded(t, dir, "prompts.txt"))
	}
}

// Reading never brings the agent up (mcode.py's goal_read contract): without
// a live session the transport reads nil and no process spawns; with no
// transport at all the overlay leaves the view untouched.
func TestMcodeGoalReadNeverSpawns(t *testing.T) {
	dir := t.TempDir()
	manager := acpruntime.NewCommand(mcodeGoalAgent(t, dir))
	defer manager.Close()

	transport := &mcodeGoalTransport{manager: manager}
	if goalValue := transport.Read("mc-never-opened"); goalValue != nil {
		t.Fatalf("absent session must read nil: %s", jsoncompat.EncodeCompact(goalValue))
	}
	if marker := recorded(t, dir, "started.txt"); marker != "" {
		t.Fatalf("goal read must not spawn the agent: %q", marker)
	}

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"mcode": manager}
	if server.goalTransportFor("mc-never-opened") != nil {
		t.Fatal("unknown thread must not resolve a transport without the sidecar")
	}
	view := jsoncompat.NewObject()
	view.Set("cwd", dir)
	server.applyGoalOverlay("mc-never-opened", view)
	if _, exists := view.Get("goalRuntimeAvailable"); exists {
		t.Fatalf("overlay must skip threads without a transport: %s", jsoncompat.EncodeCompact(view))
	}
}
