package httpd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpgoal"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestGatewayGoalTwoTurnsAndIdempotentCreate(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	cwd, dataDir := t.TempDir(), t.TempDir()
	promptFile := filepath.Join(dataDir, "prompts.txt")
	script := `import json,sys
path=` + fmt.Sprintf("%q", promptFile) + `
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method=="session/new": result={"sessionId":"goal-sess","modes":{"currentModeId":"default"},"configOptions":[]}
 elif method=="session/list": result={"sessions":[]}
 elif method=="session/prompt":
  text=m["params"]["prompt"][0]["text"]
  with open(path,"a") as out: out.write(text.replace("\n"," ")+"\n")
  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"goal-sess","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"step done"}}}}),flush=True)
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	agentPath := filepath.Join(dataDir, "agent.py")
	if err := os.WriteFile(agentPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", agentPath})
	manager.Kind = "zcode"
	defer manager.Close()
	var assessments atomic.Int32
	jevServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if assessments.Add(1) == 1 {
			_, _ = fmt.Fprint(w, `{"answers":{"complete":{"choice":"no"},"continue":{"choice":"continue"},"stop_kind":{"choice":"finished"},"confidence":{"noul":0.9}}}`)
		} else {
			_, _ = fmt.Fprint(w, `{"answers":{"complete":{"choice":"yes"},"continue":{"choice":"stop"},"stop_kind":{"choice":"finished"},"confidence":{"noul":0.9}}}`)
		}
	}))
	defer jevServer.Close()
	settings := jev.New(filepath.Join(dataDir, "jev.json"))
	config := jsoncompat.NewObject()
	config.Set("apiKey", "test-key-123456")
	config.Set("baseUrl", jevServer.URL)
	if _, err := settings.Update(config); err != nil {
		t.Fatal(err)
	}
	server, token, csrf := newFeatureServer(t, dataDir)
	server.Jev = settings
	server.ACPGoals, err = acpgoal.Open(filepath.Join(dataDir, "acp-goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	manager.AutoContinue = func(session *acpruntime.Session) { server.GoalSettleACP("zcode", session) }
	created := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", `{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"Goal test","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	body := `{"text":"整理仓库","mode":"send","workMode":"goal","id":"3bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`
	sent := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/goal-sess/send", body)
	if sent.Code != http.StatusOK {
		t.Fatalf("goal send=%d %s", sent.Code, sent.Body.String())
	}
	deadline := time.After(5 * time.Second)
	for {
		goal, ok := server.ACPGoals.ForThread("zcode", "goal-sess")
		if ok && goal.Status == acpgoal.StatusCompleted {
			if goal.AutoStepsUsed != 1 || assessments.Load() != 2 {
				t.Fatalf("goal=%+v assessments=%d", goal, assessments.Load())
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("goal did not complete: %+v", goal)
		case <-time.After(10 * time.Millisecond):
		}
	}
	repeat := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/goal-sess/send", body)
	if repeat.Code != http.StatusOK || !strings.Contains(repeat.Body.String(), `"duplicate": true`) {
		t.Fatalf("repeat=%d %s", repeat.Code, repeat.Body.String())
	}
	raw, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 2 {
		t.Fatalf("prompts=%q", raw)
	}
}

func TestGatewayGoalWithoutJevWaitsForManualReview(t *testing.T) {
	cwd, dataDir := t.TempDir(), t.TempDir()
	manager := acpruntime.NewCommand(fakeACPAgent(t, cwd))
	manager.Kind = "zcode"
	defer manager.Close()
	server, token, csrf := newFeatureServer(t, dataDir)
	var err error
	server.ACPGoals, err = acpgoal.Open(filepath.Join(dataDir, "acp-goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	manager.AutoContinue = func(session *acpruntime.Session) { server.GoalSettleACP("zcode", session) }
	created := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions", `{"agent":"zcode","cwd":`+jsonText(cwd)+`,"title":"Review test","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	emptyStatus := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/uuid-new/goal/status", "")
	if emptyStatus.Code != http.StatusOK || !strings.Contains(emptyStatus.Body.String(), `"goal": null`) {
		t.Fatalf("empty status=%d %s", emptyStatus.Code, emptyStatus.Body.String())
	}
	sent := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/uuid-new/send", `{"text":"人工目标","mode":"send","workMode":"goal","id":"3bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if sent.Code != http.StatusOK {
		t.Fatalf("send=%d %s", sent.Code, sent.Body.String())
	}
	deadline := time.After(5 * time.Second)
	var goal acpgoal.Goal
	for {
		goal, _ = server.ACPGoals.ForThread("zcode", "uuid-new")
		if goal.Status == acpgoal.StatusWaitingReview {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("goal did not pause for review: %+v", goal)
		case <-time.After(10 * time.Millisecond):
		}
	}
	status := featureRequest(server, token, csrf, http.MethodGet, "/api/sessions/uuid-new/goal/status", "")
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"status": "waiting_review"`) {
		t.Fatalf("status=%d %s", status.Code, status.Body.String())
	}
	stale := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/uuid-new/goal/complete", `{"id":"5bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee","expectedState":"waiting_review","revision":1}`)
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revision=%d %s", stale.Code, stale.Body.String())
	}
	body := fmt.Sprintf(`{"id":"4bbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee","expectedState":"waiting_review","revision":%d}`, goal.Revision)
	completed := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/uuid-new/goal/complete", body)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), `"status": "completed"`) {
		t.Fatalf("complete=%d %s", completed.Code, completed.Body.String())
	}
	reloaded, err := acpgoal.Open(filepath.Join(dataDir, "acp-goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	final, _ := reloaded.ForThread("zcode", "uuid-new")
	if final.Status != acpgoal.StatusCompleted || final.AutoStepsUsed != 0 {
		t.Fatalf("reloaded=%+v", final)
	}
}

func TestGatewayGoalResolvesLazySessionAlias(t *testing.T) {
	dataDir := t.TempDir()
	goalStore, err := acpgoal.Open(filepath.Join(dataDir, "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := goalStore.Create("g", "op", "zcode", "lazy-1", "目标", 2); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{"unused"})
	manager.Kind = "zcode"
	manager.Created = acpruntime.LoadCreated(filepath.Join(dataDir, "created.json"))
	manager.Created.Remember("zcode", "lazy-1", "/work", "Goal", 1)
	manager.Canonical = func(id string) string {
		if id == "lazy-1" {
			return "sess-real"
		}
		return ""
	}
	server := &Server{ACPGoals: goalStore, ACPAgents: map[string]*acpruntime.Manager{"zcode": manager}}
	goal, ok := server.gatewayGoalFor("zcode", "sess-real")
	if !ok || goal.GoalID != "g" {
		t.Fatalf("alias goal=%+v found=%v", goal, ok)
	}
}
