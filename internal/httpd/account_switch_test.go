// Route coverage for POST /api/accounts/switch: the account package's own
// tests exercise the state machine; here the HTTP contract matters — 200 with
// the fresh panel status, 409 while a switch is in flight, 400 for an
// invalid target, 409 when the gateway predates account management.
package httpd

import (
	"errors"
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

	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const switchTestAccountID = "0123456789abcdef0123456789abcdef"
const switchTestRequestID = "63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5b"

const switchFakeRuntime = `#!/usr/bin/env python3
import json,sys,os
home=os.environ.get('CODEX_HOME','')
for line in sys.stdin:
 message=json.loads(line); method=message.get('method'); ident=message.get('id'); result={}
 if method=='initialize': result={}
 elif method=='config/read':
  if home.endswith('prepared'): result={'config':{'model_provider':'openai'}}
  else: result={'config':{'model_provider':'bridge_api','model':'gpt-test','openai_base_url':'https://api.example.com/v1','model_providers':{'bridge_api':{'base_url':'https://api.example.com/v1'}}}}
 elif method=='config/batchWrite': result={'status':'ok'}
 elif method=='account/read':
  if home.endswith('prepared'): result={'account':{'type':'chatgpt','email':'user@example.com'}}
  else: result={'account':{'type':'apiKey'}}
 else: result={}
 print(json.dumps({'id':ident,'result':result}),flush=True)
`

type switchFakeProc struct {
	mu        sync.Mutex
	running   bool
	dialable  bool
	quitBlock chan struct{}
}

func (f *switchFakeProc) Processes(string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return []int{7}, nil
	}
	return nil, nil
}

func (f *switchFakeProc) QuitGUI(string, time.Duration) error {
	f.mu.Lock()
	block := f.quitBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	f.dialable = false
	return nil
}

func (f *switchFakeProc) CloseMainWindow(int) error { return nil }
func (f *switchFakeProc) Terminate(int) error       { return nil }
func (f *switchFakeProc) Launch(string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = true
	f.dialable = true
	return nil
}
func (f *switchFakeProc) DialIPC(string, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialable {
		return nil
	}
	return errors.New("refused")
}

func newSwitchRouteServer(t *testing.T) (*Server, string, *switchFakeProc) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake desktop binary uses a unix lookup")
	}
	source, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) for the fake GUI binary")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dataDir := t.TempDir()
	macos := filepath.Join(dataDir, "Codex.app", "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o700); err != nil {
		t.Fatal(err)
	}
	gui := filepath.Join(macos, "Codex")
	if err := os.WriteFile(gui, data, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRuntime := filepath.Join(t.TempDir(), "runtime.py")
	if err := os.WriteFile(fakeRuntime, []byte(switchFakeRuntime), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dataDir, "accounts")
	if err := os.MkdirAll(filepath.Join(root, switchTestAccountID), 0o700); err != nil {
		t.Fatal(err)
	}
	index := jsoncompat.NewObject()
	accounts := &jsoncompat.Array{}
	row := jsoncompat.NewObject()
	row.Set("id", switchTestAccountID)
	row.Set("kind", "api")
	row.Set("name", "示例 API")
	row.Set("baseUrl", "https://api.example.com/v1")
	row.Set("model", "gpt-test")
	accounts.Items = append(accounts.Items, row)
	index.Set("accounts", accounts)
	index.Set("activeId", nil)
	index.Set("desktopExecutable", gui)
	if err := os.WriteFile(filepath.Join(root, "index.json"), jsoncompat.EncodeCompact(index), 0o600); err != nil {
		t.Fatal(err)
	}
	api := jsoncompat.NewObject()
	api.Set("key", "secret-key")
	if err := os.WriteFile(filepath.Join(root, switchTestAccountID, "api.json"), jsoncompat.EncodeCompact(api), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := account.NewWithData(home, dataDir, fakeRuntime)
	proc := &switchFakeProc{running: true, dialable: true}
	manager.SetProcesses(proc)
	manager.SetSwitchTimeouts(300*time.Millisecond, 400*time.Millisecond, 5*time.Millisecond)

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Account = manager
	return server, gui, proc
}

func switchRoutePost(t *testing.T, server *Server, token, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/accounts/switch", strings.NewReader(body))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}

func TestSwitchRouteSuccessConflictAndBadRequest(t *testing.T) {
	server, _, proc := newSwitchRouteServer(t)
	authenticator := server.Auth
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)

	// An unknown target is a 400 with the panel message.
	response := switchRoutePost(t, server, token, csrf,
		`{"action":"switch","id":"11111111111111111111111111111111","requestId":"`+switchTestRequestID+`","confirmed":true,"tasksConfirmed":true}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "账号不存在") {
		t.Fatalf("unknown target: status=%d body=%s", response.Code, response.Body.String())
	}

	// Start one switch and hold the desktop quit, so a second request lands
	// while phase is stopping.
	proc.mu.Lock()
	proc.quitBlock = make(chan struct{})
	proc.mu.Unlock()
	response = switchRoutePost(t, server, token, csrf,
		`{"action":"switch","id":"`+switchTestAccountID+`","requestId":"`+switchTestRequestID+`","confirmed":true,"tasksConfirmed":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("first switch: status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"switch"`) {
		t.Fatalf("switch response must carry the panel status: %s", response.Body.String())
	}
	waitForSwitchPhase(t, server, token, "stopping")
	response = switchRoutePost(t, server, token, csrf,
		`{"action":"switch","id":"`+switchTestAccountID+`","requestId":"63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5c","confirmed":true,"tasksConfirmed":true}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "账号正在切换") {
		t.Fatalf("second switch: status=%d body=%s", response.Code, response.Body.String())
	}
	proc.mu.Lock()
	close(proc.quitBlock)
	proc.mu.Unlock()
	waitForSwitchPhase(t, server, token, "complete")

	// A missing confirm is a 400 before anything else happens.
	response = switchRoutePost(t, server, token, csrf,
		`{"action":"switch","id":"`+switchTestAccountID+`","requestId":"63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5d","confirmed":false,"tasksConfirmed":true}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed switch: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSwitchRouteNeedsAccountManager(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	server := New(authenticator, fsys{}, 8790, "instance")
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8790/api/accounts/switch", strings.NewReader(`{}`))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "请在电脑端操作") {
		t.Fatalf("legacy gateway: status=%d body=%s", response.Code, response.Body.String())
	}
}

func waitForSwitchPhase(t *testing.T, server *Server, token, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	last := ""
	for time.Now().Before(deadline) {
		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/accounts", nil)
		request.Host = "127.0.0.1:8790"
		request.Header.Set("Origin", "http://127.0.0.1:8790")
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		last = response.Body.String()
		if response.Code != http.StatusOK {
			continue
		}
		if value, err := jsoncompat.Unmarshal([]byte(last)); err == nil {
			if public, ok := value.(*jsoncompat.Object); ok {
				if state, ok := public.MustGet("switch").(*jsoncompat.Object); ok && state.MustGet("phase") == phase {
					return
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase %s not reached via /api/accounts: %s", phase, last)
}

// The panel's 恢复原接入 control rides the switch route with action
// "recover": an interrupted switch with a rollback record returns the panel
// status (200) and drives the restore; anything else is a 400.
func TestSwitchRouteDispatchesRecover(t *testing.T) {
	server, gui, _ := newSwitchRouteServer(t)
	authenticator := server.Auth
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)

	// Park the manager in interrupted with a rollback record before its
	// state is first read, then recover through the route.
	accountsRoot := filepath.Join(server.Account.DataDir, "accounts")
	stale := jsoncompat.NewObject()
	stale.Set("phase", "verifying")
	stale.Set("requestId", switchTestRequestID)
	stale.Set("targetId", switchTestAccountID)
	stale.Set("error", nil)
	if err := os.WriteFile(filepath.Join(accountsRoot, "switch.json"), jsoncompat.EncodeCompact(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback := jsoncompat.NewObject()
	rollback.Set("files", jsoncompat.NewObject())
	rollback.Set("activeId", nil)
	rollback.Set("desktopExecutable", gui)
	if err := os.WriteFile(filepath.Join(accountsRoot, "rollback.json"), jsoncompat.EncodeCompact(rollback), 0o600); err != nil {
		t.Fatal(err)
	}
	response := switchRoutePost(t, server, token, csrf, `{"action":"recover","confirmed":true}`)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"switch"`) {
		t.Fatalf("recover: status=%d body=%s", response.Code, response.Body.String())
	}
	waitForSwitchPhase(t, server, token, "restored")

	// Once restored there is nothing left to recover.
	response = switchRoutePost(t, server, token, csrf, `{"action":"recover","confirmed":true}`)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "当前不需要恢复") {
		t.Fatalf("stale recover: status=%d body=%s", response.Code, response.Body.String())
	}
}
