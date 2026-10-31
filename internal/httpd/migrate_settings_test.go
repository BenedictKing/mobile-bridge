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
)

// settingsFakeAgent advertises a model and a thought_level option on
// session/new and appends every session/set_config_option and session/prompt
// to one ordered log, so tests can assert the settings land before the
// transcript.
func settingsFakeAgent(t *testing.T) (argv []string, creates, log string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	creates, log = filepath.Join(dir, "creates.txt"), filepath.Join(dir, "log.txt")
	script := `import json,sys
CREATES = "` + creates + `"
LOG = "` + log + `"
OPTIONS = [
 {"id":"model","category":"model","name":"Model","type":"select","currentValue":"m1",
  "options":[{"value":"m1","name":"Model One"},{"value":"m2","name":"Model Two"}]},
 {"id":"thought","category":"thought_level","name":"Thinking","type":"select","currentValue":"low",
  "options":[{"value":"low","name":"Low"},{"value":"high","name":"High"}]}]
def note(line):
 open(LOG, "a").write(line + "\n")
n = 0
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[]}
 elif method == "session/new":
  open(CREATES, "a").write("x")
  result={"sessionId":"target-%d" % n,"modes":{"currentModeId":"build"},"configOptions":OPTIONS}; n += 1
 elif method == "session/set_config_option":
  note("set %s=%s" % (params.get("configId"), params.get("value")))
  result={}
 elif method == "session/prompt": note("prompt"); result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}, creates, log
}

func logLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(raw)), " ", "_"))
}

func TestMigrateTargetSettings(t *testing.T) {
	cwd := t.TempDir()
	sourceArgv, _, _ := migrationFakeAgent(t, `[]`, "source")
	targetArgv, targetCreates, targetLog := settingsFakeAgent(t)
	source := acpruntime.NewCommand(sourceArgv)
	source.Label = "Source"
	target := acpruntime.NewCommand(targetArgv)
	target.Label = "Target"
	target.EffortMap = map[string]string{"low": "low", "high": "high"}
	defer source.Close()
	defer target.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"source": source, "target": target}
	server.Creations = LoadCreations(filepath.Join(t.TempDir(), "creations.json"))

	request := func(target, body string) *httptest.ResponseRecorder {
		t.Helper()
		httptestRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		httptestRequest.Host = "127.0.0.1:8790"
		httptestRequest.Header.Set("Origin", "http://127.0.0.1:8790")
		httptestRequest.Header.Set("Content-Type", "application/json")
		httptestRequest.Header.Set("X-CSRF-Token", csrf)
		httptestRequest.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptestRequest)
		return response
	}
	create := request("/api/sessions",
		`{"agent":"source","cwd":`+jsonText(cwd)+`,"title":"源聊天","id":"1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK || !strings.Contains(create.Body.String(), `"source-0"`) {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	if send := request("/api/sessions/source-0/send", `{"text":"你好世界"}`); send.Code != http.StatusOK {
		t.Fatalf("send status=%d body=%s", send.Code, send.Body.String())
	}
	time.Sleep(100 * time.Millisecond)

	const id = "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	migrate := func(extra string) *httptest.ResponseRecorder {
		return request("/api/sessions/source-0/migrate",
			`{"agent":"target","scope":"main","id":"`+id+`"`+extra+`}`)
	}

	// Prepare creates the target and returns its real catalog; nothing is sent.
	prepared := migrate(`,"phase":"prepare"`)
	for _, needle := range []string{`"status": "prepared"`, `"sent": false`, `"target-0"`,
		`"Model One"`, `"Model Two"`, `"currentModel": "m1"`} {
		if !strings.Contains(prepared.Body.String(), needle) {
			t.Fatalf("prepare missing %s: status=%d body=%s", needle, prepared.Code, prepared.Body.String())
		}
	}
	if len(logLines(t, targetLog)) != 0 {
		t.Fatalf("prepare must not prompt or configure: %v", logLines(t, targetLog))
	}

	// Preparing again reuses the same session instead of creating another.
	if again := migrate(`,"phase":"prepare"`); !strings.Contains(again.Body.String(), `"target-0"`) ||
		len(counterFile(t, targetCreates)) != 1 {
		t.Fatalf("re-prepare duplicated: %s creates=%d", again.Body.String(), len(counterFile(t, targetCreates)))
	}

	// A model the agent does not advertise stops the migration before any
	// prompt, and the entry stays retryable.
	rejected := migrate(`,"model":"nope","effort":"low"`)
	if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "不支持所选模型") {
		t.Fatalf("rejected status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	if len(logLines(t, targetLog)) != 0 {
		t.Fatalf("rejected settings must not prompt: %v", logLines(t, targetLog))
	}

	// Confirming applies the settings first, then injects the transcript.
	sent := migrate(`,"model":"m2","effort":"high"`)
	if sent.Code != http.StatusOK || !strings.Contains(sent.Body.String(), `"sent": true`) ||
		!strings.Contains(sent.Body.String(), `"target-0"`) {
		t.Fatalf("confirm status=%d body=%s", sent.Code, sent.Body.String())
	}
	if got := strings.Join(logLines(t, targetLog), ","); got != "set_model=m2,set_thought=high,prompt" {
		t.Fatalf("order = %s", got)
	}
	if len(counterFile(t, targetCreates)) != 1 {
		t.Fatalf("confirm created another session")
	}

	// A retry of the confirmed request neither reconfigures nor re-prompts.
	migrate(`,"model":"m2","effort":"high"`)
	if got := strings.Join(logLines(t, targetLog), ","); got != "set_model=m2,set_thought=high,prompt" {
		t.Fatalf("retry changed log: %s", got)
	}

	// Abandoning a prepared migration forgets it; a sent one is untouched.
	const abandonID = "3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	abandonBody := func(phase string) string {
		return `{"agent":"target","scope":"main","id":"` + abandonID + `","phase":"` + phase + `"}`
	}
	request("/api/sessions/source-0/migrate", abandonBody("prepare"))
	abandoned := request("/api/sessions/source-0/migrate", abandonBody("abandon"))
	if !strings.Contains(abandoned.Body.String(), `"abandoned"`) {
		t.Fatalf("abandon body=%s", abandoned.Body.String())
	}
	if again := request("/api/sessions/source-0/migrate", abandonBody("abandon")); !strings.Contains(again.Body.String(), `"none"`) {
		t.Fatalf("second abandon body=%s", again.Body.String())
	}
	if kept := request("/api/sessions/source-0/migrate",
		`{"agent":"target","scope":"main","id":"`+id+`","phase":"abandon"}`); !strings.Contains(kept.Body.String(), `"none"`) {
		t.Fatalf("abandon of a sent migration body=%s", kept.Body.String())
	}

	// Validation: effort needs a model, phase must be known.
	for _, bad := range []struct{ body, needle string }{
		{`,"effort":"high"`, "请先选择模型"},
		{`,"phase":"wat"`, "未知的迁移阶段"},
	} {
		response := migrate(bad.body)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), bad.needle) {
			t.Fatalf("bad %s: status=%d body=%s", bad.body, response.Code, response.Body.String())
		}
	}
}

// counterFile returns the bytes written by the fake agent's create marker.
func counterFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, _ := os.ReadFile(path)
	return raw
}
