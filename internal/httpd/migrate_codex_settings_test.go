package httpd

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/ipc"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type codexMigrationDesktop struct {
	mu         sync.Mutex
	operations []*jsoncompat.Object
}

func (d *codexMigrationDesktop) add(operation *jsoncompat.Object) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.operations = append(d.operations, operation)
}

func (d *codexMigrationDesktop) byMethod(method string) *jsoncompat.Object {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, operation := range d.operations {
		if operation.MustGet("method") == method {
			return operation
		}
	}
	return nil
}

func codexMigrationAppServer(t *testing.T, threadID string) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
THREAD_ID = "` + threadID + `"
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"ok":True}
 elif method == "config/read": result={"config":{"model":"gpt-6-astra","model_provider":"openai","model_reasoning_effort":"xhigh"}}
 elif method == "account/read": result={"account":{"type":"chatgpt"},"requiresOpenaiAuth":True}
 elif method == "configRequirements/read": result={"requirements":{}}
 elif method == "model/list": result={"data":[{"model":"gpt-6-astra","displayName":"GPT 6 Astra","supportedReasoningEfforts":[{"reasoningEffort":"medium"},{"reasoningEffort":"xhigh"}],"defaultReasoningEffort":"medium"}],"nextCursor":None}
 elif method == "thread/start": result={"thread":{"id":THREAD_ID}}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "codex-app-server")
	if err := os.WriteFile(path, []byte("#!"+python+"\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexMigrationOwner(t *testing.T) (string, *codexMigrationDesktop) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix IPC fixture")
	}
	path := filepath.Join(os.TempDir(), "mb-mig-"+time.Now().Format("150405.000000000")+".sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	desktop := &codexMigrationDesktop{}
	var connection net.Conn
	var connectionMu sync.Mutex
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		connectionMu.Lock()
		connection = conn
		connectionMu.Unlock()
		defer conn.Close()
		for {
			value, err := ipc.DecodeFrame(conn)
			if err != nil {
				return
			}
			request, _ := value.(*jsoncompat.Object)
			if request == nil || request.MustGet("type") != "request" {
				continue
			}
			method := request.MustGet("method")
			result := jsoncompat.NewObject()
			result.Set("ok", true)
			response := jsoncompat.NewObject()
			response.Set("type", "response")
			response.Set("requestId", request.MustGet("requestId"))
			response.Set("resultType", "success")
			response.Set("result", result)
			if method == "initialize" {
				result.Set("clientId", "mobile-test")
			} else {
				desktop.add(request)
				if method == "thread-owner-discovery" {
					response.Set("handledByClientId", "desktop-owner")
				}
			}
			frame, _ := ipc.EncodeFrame(response)
			if _, err := conn.Write(frame); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		connectionMu.Lock()
		if connection != nil {
			connection.Close()
		}
		connectionMu.Unlock()
		<-done
	})
	return path, desktop
}

func TestMigrateCodexDefaultsPersistAndApplyBeforeSend(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix open and IPC fixture")
	}
	cwd := t.TempDir()
	const threadID = "9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	bridge := codex.New(t.TempDir(), "")
	bridge.Executable = codexMigrationAppServer(t, threadID)
	desktopPath, desktop := codexMigrationOwner(t)
	bridge.IPC.Path = desktopPath
	defer bridge.Close()

	sourceArgv, _, sourcePrompts := migrationFakeAgent(t, `[]`, "source")
	source := acpruntime.NewCommand(sourceArgv)
	source.Label = "Source"
	defer source.Close()
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"source": source}
	server.Codex = bridge
	server.Creations = mustLoadCreations(t, filepath.Join(t.TempDir(), "creations.json"))

	openDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(openDir, "open"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", openDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	request := func(method, target, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
		req.Host = "127.0.0.1:8790"
		req.Header.Set("Origin", "http://127.0.0.1:8790")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, req)
		return response
	}
	created := request(http.MethodPost, "/api/sessions",
		`{"agent":"source","cwd":`+jsonText(cwd)+`,"title":"源聊天","id":"8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if created.Code != http.StatusOK {
		t.Fatalf("create source status=%d body=%s", created.Code, created.Body.String())
	}
	if sent := request(http.MethodPost, "/api/sessions/source-0/send", `{"text":"你好世界"}`); sent.Code != http.StatusOK {
		t.Fatalf("send source status=%d body=%s", sent.Code, sent.Body.String())
	}
	for deadline := time.Now().Add(2 * time.Second); counterValue(t, sourcePrompts) != 1; {
		if time.Now().After(deadline) {
			t.Fatal("source prompt did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}

	const requestID = "a1aaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	migrate := func(phase string) *httptest.ResponseRecorder {
		extra := ""
		if phase != "" {
			extra = `,"phase":"` + phase + `"`
		}
		return request(http.MethodPost, "/api/sessions/source-0/migrate",
			`{"agent":"codex","scope":"main","id":"`+requestID+`"`+extra+`}`)
	}
	prepared := migrate("prepare")
	if prepared.Code != http.StatusOK || !strings.Contains(prepared.Body.String(), `"currentModel": "gpt-6-astra"`) ||
		!strings.Contains(prepared.Body.String(), `"currentEffort": "xhigh"`) {
		t.Fatalf("prepare status=%d body=%s", prepared.Code, prepared.Body.String())
	}
	confirmed := migrate("")
	if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), `"state": "opening"`) {
		t.Fatalf("confirm status=%d body=%s", confirmed.Code, confirmed.Body.String())
	}
	entry := server.Creations.lookup(requestID)
	if entry == nil || entry.ID != threadID || entry.Model != "gpt-6-astra" || entry.Effort != "xhigh" {
		t.Fatalf("saved defaults entry=%+v", entry)
	}
	bridge.Executable = filepath.Join(t.TempDir(), "missing-codex")
	state := jsoncompat.NewObject()
	state.Set("id", threadID)
	state.Set("turns", &jsoncompat.Array{})
	bridge.Publish(threadID, state)
	resumed := request(http.MethodPost, "/api/sessions/"+threadID+"/reconnect", `{"activate":true}`)
	if resumed.Code != http.StatusOK {
		t.Fatalf("reconnect status=%d body=%s", resumed.Code, resumed.Body.String())
	}
	settingsOperation := desktop.byMethod("thread-follower-update-thread-settings")
	if settingsOperation == nil {
		t.Fatal("default migration did not apply Codex settings")
	}
	settings := settingsOperation.MustGet("params").(*jsoncompat.Object).MustGet("threadSettings").(*jsoncompat.Object)
	if stringField(settings, "model") != "gpt-6-astra" || stringField(settings, "effort") != "xhigh" {
		t.Fatalf("applied settings=%s", jsoncompat.EncodePythonDefault(settings))
	}
	if desktop.byMethod("thread-follower-start-turn") == nil {
		t.Fatal("migration transcript was not sent")
	}
	desktop.mu.Lock()
	methods := []string{}
	for _, operation := range desktop.operations {
		methods = append(methods, jsoncompat.PyStr(operation.MustGet("method")))
	}
	desktop.mu.Unlock()
	if strings.Join(methods, ",") != "thread-owner-discovery,thread-follower-update-thread-settings,thread-follower-start-turn" {
		t.Fatalf("desktop operations=%v", methods)
	}
	if entry = server.Creations.lookup(requestID); entry == nil || !entry.Sent {
		t.Fatalf("migration was not marked sent: %+v", entry)
	}
}
