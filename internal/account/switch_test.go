// Switch state machine tests: every desktop App side effect runs through a
// fake ProcessController and every app-server probe through a scripted
// runtime, so no real Codex desktop process is ever touched.
package account

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const testAccountID = "0123456789abcdef0123456789abcdef"
const otherAccountID = "fedcba9876543210fedcba9876543210"
const missingAccountID = "11111111111111111111111111111111"
const testRequestID = "63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5b"

// fakeProcesses records desktop lifecycle calls; running mimics the GUI
// process, dialable mimics the desktop IPC endpoint.
type fakeProcesses struct {
	mu        sync.Mutex
	running   bool
	dialable  bool
	quitErr   error
	launchErr error
	quitBlock chan struct{}
	quits     int
	launches  int
}

func newFakeProcesses() *fakeProcesses {
	return &fakeProcesses{running: true, dialable: true}
}

func (f *fakeProcesses) Processes(string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return []int{4242}, nil
	}
	return nil, nil
}

func (f *fakeProcesses) QuitGUI(string, time.Duration) error {
	f.mu.Lock()
	block := f.quitBlock
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quits++
	if f.quitErr != nil {
		return f.quitErr
	}
	f.running = false
	f.dialable = false
	return nil
}

func (f *fakeProcesses) CloseMainWindow(int) error { return nil }

func (f *fakeProcesses) Terminate(int) error { return nil }

func (f *fakeProcesses) Launch(string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launches++
	if f.launchErr != nil {
		return f.launchErr
	}
	f.running = true
	f.dialable = true
	return nil
}

func (f *fakeProcesses) DialIPC(string, time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dialable {
		return nil
	}
	return errors.New("connection refused")
}

func (f *fakeProcesses) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quits, f.launches
}

// fakeRuntime writes the scripted app-server the same way TestReadUsesRestrictedAccountRPC
// does; its replies are keyed on CODEX_HOME and CMB_FAKE_MODE.
const fakeRuntimeScript = `#!/usr/bin/env python3
import json,sys,os
home=os.environ.get('CODEX_HOME','')
mode=os.environ.get('CMB_FAKE_MODE','api')
for line in sys.stdin:
 message=json.loads(line); method=message.get('method'); ident=message.get('id'); result={}
 if method=='initialize': result={}
 elif method=='config/read':
  if home.endswith('prepared'):
   result={'config':{'model_provider':'openai'}}
  elif mode=='api':
   result={'config':{'model_provider':'bridge_api','model':'gpt-test','openai_base_url':'https://api.example.com/v1','model_providers':{'bridge_api':{'base_url':'https://api.example.com/v1'}}}}
  elif mode=='failverify':
   result={'config':{'model_provider':'openai'}}
  else:
   result={'config':{'model_provider':'openai'}}
 elif method=='config/batchWrite': result={'status':'ok'}
 elif method=='account/read':
  if home.endswith('prepared'):
   result={'account':{'type':'chatgpt','email':'user@example.com'}}
  elif mode=='api':
   result={'account':{'type':'apiKey'}}
  else:
   result={'account':{'type':'chatgpt','email':'user@example.com'}}
 else: result={}
 print(json.dumps({'id':ident,'result':result}),flush=True)
`

type switchFixture struct {
	home     string
	manager  *Manager
	proc     *fakeProcesses
	guiPath  string
	row      *jsoncompat.Object
	switchID string
}

func newSwitchFixture(t *testing.T, mode string) *switchFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake desktop binary uses a unix lookup")
	}
	t.Setenv("CMB_FAKE_MODE", mode)
	// A real executable file that is not a script and never runs (the
	// controller is fake): a copy of true(1).
	source, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) available for the fake GUI binary")
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	dataDir := t.TempDir()
	// The GUI must live inside an .app bundle (macOS Stop quits the bundle).
	macos := filepath.Join(dataDir, "Codex.app", "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o700); err != nil {
		t.Fatal(err)
	}
	guiPath := filepath.Join(macos, "Codex")
	if err := os.WriteFile(guiPath, data, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(t.TempDir(), "codex-runtime")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeRuntime := filepath.Join(t.TempDir(), "runtime.py")
	if err := os.WriteFile(fakeRuntime, []byte(fakeRuntimeScript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model_provider = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	index := jsoncompat.NewObject()
	accounts := &jsoncompat.Array{}
	row := jsoncompat.NewObject()
	row.Set("id", testAccountID)
	row.Set("kind", "api")
	row.Set("name", "示例 API")
	row.Set("baseUrl", "https://api.example.com/v1")
	row.Set("model", "gpt-test")
	accounts.Items = append(accounts.Items, row)
	other := jsoncompat.NewObject()
	other.Set("id", otherAccountID)
	other.Set("kind", "api")
	other.Set("name", "另一个 API")
	other.Set("baseUrl", "https://other.example.com/v1")
	other.Set("model", "other-model")
	accounts.Items = append(accounts.Items, other)
	index.Set("accounts", accounts)
	index.Set("activeId", nil)
	index.Set("desktopExecutable", guiPath)
	root := filepath.Join(dataDir, "accounts")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.json"), jsoncompat.EncodeCompact(index), 0o600); err != nil {
		t.Fatal(err)
	}
	api := jsoncompat.NewObject()
	api.Set("key", "secret-key")
	if err := os.MkdirAll(filepath.Join(root, testAccountID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, testAccountID, "api.json"), jsoncompat.EncodeCompact(api), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewWithData(home, dataDir, fakeRuntime)
	proc := newFakeProcesses()
	manager.SetProcesses(proc)
	manager.SetSwitchTimeouts(300*time.Millisecond, 400*time.Millisecond, 5*time.Millisecond)
	// Wait loops inside DesktopApp.Stop honor the same short budget.
	prevQuit := quitTimeout
	quitTimeout = 100 * time.Millisecond
	t.Cleanup(func() { quitTimeout = prevQuit })
	return &switchFixture{home: home, manager: manager, proc: proc, guiPath: guiPath, row: row, switchID: testAccountID}
}

func (f *switchFixture) request(t *testing.T, id, requestID string) *jsoncompat.Object {
	t.Helper()
	body := jsoncompat.NewObject()
	body.Set("action", "switch")
	body.Set("id", id)
	body.Set("requestId", requestID)
	body.Set("confirmed", true)
	body.Set("tasksConfirmed", true)
	return body
}

func waitForPhase(t *testing.T, manager *Manager, phase string) *jsoncompat.Object {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, _ := manager.AccountsPublic().MustGet("switch").(*jsoncompat.Object)
		if state != nil && state.MustGet("phase") == phase {
			return state
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("phase %s not reached: %s", phase, jsoncompat.EncodeCompact(manager.AccountsPublic()))
	return nil
}

func TestSwitchSuccessAppliesAccountAndActivates(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	before := readFile(t, filepath.Join(fixture.home, "config.toml"))
	public, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if public.MustGet("canSwitch") != false {
		t.Fatalf("canSwitch should be false while switching: %s", jsoncompat.EncodeCompact(public))
	}
	state := waitForPhase(t, fixture.manager, "complete")
	if state.MustGet("targetId") != testAccountID || state.MustGet("error") != nil {
		t.Fatalf("unexpected state: %s", jsoncompat.EncodeCompact(state))
	}
	index := readObject(filepath.Join(fixture.manager.DataDir, "accounts", "index.json"))
	if index.MustGet("activeId") != testAccountID {
		t.Fatalf("activeId not updated: %s", jsoncompat.EncodeCompact(index))
	}
	if _, hasStamp := index.Get("activeStamp"); !hasStamp {
		t.Fatal("activeStamp missing")
	}
	// The prepared config (original bytes here) landed in the home, the API
	// auth was synthesized, and no rollback/prepared leftovers remain.
	if readFile(t, filepath.Join(fixture.home, "config.toml")) != before {
		t.Fatal("home config.toml unexpectedly rewritten")
	}
	auth := readFile(t, filepath.Join(fixture.home, "auth.json"))
	if !strings.Contains(auth, "apikey") || !strings.Contains(auth, "secret-key") {
		t.Fatalf("auth.json not applied: %s", auth)
	}
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	if _, err := os.Stat(filepath.Join(root, "rollback.json")); !os.IsNotExist(err) {
		t.Fatal("rollback.json should be removed after success")
	}
	if _, err := os.Stat(filepath.Join(root, "prepared")); !os.IsNotExist(err) {
		t.Fatal("prepared folder should be removed after success")
	}
	ledger := readObject(filepath.Join(root, "requests.json"))
	if ledger.MustGet(testRequestID) != testAccountID {
		t.Fatalf("ledger missing request: %s", jsoncompat.EncodeCompact(ledger))
	}
	quits, launches := fixture.proc.counts()
	if quits != 1 || launches != 1 {
		t.Fatalf("unexpected lifecycle counts quits=%d launches=%d", quits, launches)
	}
	if public := fixture.manager.AccountsPublic(); public.MustGet("canSwitch") != true {
		t.Fatalf("canSwitch should recover after complete: %s", jsoncompat.EncodeCompact(public))
	}
}

func TestSwitchQuitsAgainWhenDesktopNotHealthy(t *testing.T) {
	// waitReady never passes: the home probe keeps reporting the openai
	// provider, so the flow must restore the previous account.
	fixture := newSwitchFixture(t, "failverify")
	_, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	state := waitForPhase(t, fixture.manager, "restored")
	if !strings.Contains(pyString(state.MustGet("error")), "已恢复原接入") {
		t.Fatalf("unexpected restored error: %s", jsoncompat.EncodeCompact(state))
	}
	// The home is back to the snapshot (no auth.json existed before).
	if _, err := os.Stat(filepath.Join(fixture.home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("auth.json should have been restored away")
	}
	index := readObject(filepath.Join(fixture.manager.DataDir, "accounts", "index.json"))
	if index.MustGet("activeId") != nil {
		t.Fatalf("activeId should be restored: %s", jsoncompat.EncodeCompact(index))
	}
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	if _, err := os.Stat(filepath.Join(root, "rollback.json")); !os.IsNotExist(err) {
		t.Fatal("rollback.json should be removed after restore")
	}
	quits, launches := fixture.proc.counts()
	if quits != 2 || launches != 2 {
		t.Fatalf("rollback should quit and launch again: quits=%d launches=%d", quits, launches)
	}
}

func TestSwitchFailedQuitLeavesFilesUntouched(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	fixture.proc.mu.Lock()
	fixture.proc.quitErr = errors.New("osascript failed")
	fixture.proc.mu.Unlock()
	_, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil)
	if err != nil {
		t.Fatalf("switch request itself should succeed: %v", err)
	}
	state := waitForPhase(t, fixture.manager, "failed")
	if !strings.Contains(pyString(state.MustGet("error")), "桌面程序未正常退出") {
		t.Fatalf("unexpected failure error: %s", jsoncompat.EncodeCompact(state))
	}
	if readFile(t, filepath.Join(fixture.home, "config.toml")) != "model_provider = \"openai\"\n" {
		t.Fatal("home config.toml must stay untouched after a failed quit")
	}
	if _, err := os.Stat(filepath.Join(fixture.home, "auth.json")); !os.IsNotExist(err) {
		t.Fatal("auth.json must stay untouched after a failed quit")
	}
	quits, launches := fixture.proc.counts()
	if quits != 1 || launches != 0 {
		t.Fatalf("no rollback expected: quits=%d launches=%d", quits, launches)
	}
}

func TestSwitchRejectsConcurrentSwitchAndReplaysLedger(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	fixture.proc.mu.Lock()
	fixture.proc.quitBlock = make(chan struct{})
	fixture.proc.mu.Unlock()
	if _, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil); err != nil {
		t.Fatalf("first switch: %v", err)
	}
	waitForPhase(t, fixture.manager, "stopping")
	second := fixture.request(t, testAccountID, "63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5c")
	if _, err := fixture.manager.Switch(second, nil); err == nil {
		t.Fatal("second switch must be refused")
	} else if _, ok := err.(*ConflictError); !ok {
		t.Fatalf("second switch must be a conflict: %v", err)
	}
	fixture.proc.mu.Lock()
	close(fixture.proc.quitBlock)
	fixture.proc.mu.Unlock()
	waitForPhase(t, fixture.manager, "complete")
	// The same request id replays idempotently; a different target with the
	// same id is refused.
	if _, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil); err != nil {
		t.Fatalf("ledger replay should succeed: %v", err)
	}
	other := jsoncompat.NewObject()
	other.Set("id", otherAccountID)
	other.Set("requestId", testRequestID)
	other.Set("confirmed", true)
	other.Set("tasksConfirmed", true)
	if _, err := fixture.manager.Switch(other, nil); err == nil || !strings.Contains(err.Error(), "同一请求不能切换到不同账号") {
		t.Fatalf("mismatched replay must fail: %v", err)
	}
	quits, launches := fixture.proc.counts()
	if quits != 1 || launches != 1 {
		t.Fatalf("replay must not restart the desktop: quits=%d launches=%d", quits, launches)
	}
}

func TestSwitchValidationErrors(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	cases := []struct {
		name    string
		body    func() *jsoncompat.Object
		message string
	}{
		{"unconfirmed", func() *jsoncompat.Object {
			body := fixture.request(t, testAccountID, testRequestID)
			body.Set("confirmed", false)
			return body
		}, "请确认"},
		{"bad-request-id", func() *jsoncompat.Object {
			return fixture.request(t, testAccountID, "not-a-uuid")
		}, "切换请求标识无效"},
		{"bad-account-id", func() *jsoncompat.Object {
			return fixture.request(t, "../escape", testRequestID)
		}, "账号标识无效"},
		{"unknown-account", func() *jsoncompat.Object {
			return fixture.request(t, missingAccountID, testRequestID)
		}, "账号不存在"},
	}
	for _, testCase := range cases {
		if _, err := fixture.manager.Switch(testCase.body(), nil); err == nil || !strings.Contains(err.Error(), testCase.message) {
			t.Fatalf("%s: unexpected error %v", testCase.name, err)
		}
	}
	// A nil idle gate means "no blockers"; a failing one must refuse.
	blocking := func() error { return errors.New("请先结束任务或处理待发送消息：任务一") }
	if _, err := fixture.manager.Switch(fixture.request(t, testAccountID, "63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5d"), blocking); err == nil {
		t.Fatal("blocked switch must fail")
	}
	// Without a desktop executable there is nothing to restart with.
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	index := readObject(filepath.Join(root, "index.json"))
	index.Set("desktopExecutable", "")
	if err := os.WriteFile(filepath.Join(root, "index.json"), jsoncompat.EncodeCompact(index), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Switch(fixture.request(t, testAccountID, "63f8a9b2-1c4d-4e5f-8a9b-0c1d2e3f4a5e"), nil); err == nil || !strings.Contains(err.Error(), "桌面程序路径") {
		t.Fatalf("missing executable must fail: %v", err)
	}
}

func TestSwitchAlreadyActiveIsNoOp(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	index := readObject(filepath.Join(root, "index.json"))
	index.Set("activeId", testAccountID)
	if err := os.WriteFile(filepath.Join(root, "index.json"), jsoncompat.EncodeCompact(index), 0o600); err != nil {
		t.Fatal(err)
	}
	public, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if public.MustGet("activeId") != testAccountID {
		t.Fatalf("unexpected public: %s", jsoncompat.EncodeCompact(public))
	}
	if quits, launches := fixture.proc.counts(); quits != 0 || launches != 0 {
		t.Fatalf("no-op switch must not touch the desktop: quits=%d launches=%d", quits, launches)
	}
}

func TestStaleActivePhaseNormalizesToInterrupted(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	stale := jsoncompat.NewObject()
	stale.Set("phase", "verifying")
	stale.Set("requestId", testRequestID)
	stale.Set("targetId", testAccountID)
	stale.Set("error", nil)
	if err := os.WriteFile(filepath.Join(root, "switch.json"), jsoncompat.EncodeCompact(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	public := fixture.manager.AccountsPublic()
	state, _ := public.MustGet("switch").(*jsoncompat.Object)
	if state.MustGet("phase") != "interrupted" {
		t.Fatalf("stale phase must normalize: %s", jsoncompat.EncodeCompact(state))
	}
	if public.MustGet("canSwitch") != false {
		t.Fatal("canSwitch must be false while interrupted")
	}
	persisted := readObject(filepath.Join(root, "switch.json"))
	if persisted.MustGet("phase") != "interrupted" {
		t.Fatalf("normalization must persist: %s", jsoncompat.EncodeCompact(persisted))
	}
	if err := fixture.manager.CheckReady(); err == nil {
		t.Fatal("CheckReady must refuse while interrupted")
	}
}

func TestConsumeRefusedWhileSwitching(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	fixture.proc.mu.Lock()
	fixture.proc.quitBlock = make(chan struct{})
	fixture.proc.mu.Unlock()
	if _, err := fixture.manager.Switch(fixture.request(t, testAccountID, testRequestID), nil); err != nil {
		t.Fatalf("switch: %v", err)
	}
	waitForPhase(t, fixture.manager, "stopping")
	err := fixture.manager.CheckReady()
	if err == nil || !strings.Contains(err.Error(), "账号正在切换") {
		t.Fatalf("CheckReady must refuse while switching: %v", err)
	}
	fixture.proc.mu.Lock()
	close(fixture.proc.quitBlock)
	fixture.proc.mu.Unlock()
	waitForPhase(t, fixture.manager, "complete")
}

func TestDiscoverDesktopAppFindsBundleExecutable(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("bundle discovery is macOS-specific")
	}
	bundle := t.TempDir()
	macos := filepath.Join(bundle, "Codex.app", "Contents", "MacOS")
	resources := filepath.Join(bundle, "Codex.app", "Contents", "Resources")
	if err := os.MkdirAll(macos, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resources, 0o700); err != nil {
		t.Fatal(err)
	}
	gui := filepath.Join(macos, "Codex")
	if err := os.WriteFile(gui, []byte{0xcf, 0xfa}, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\"><dict><key>CFBundleExecutable</key><string>Codex</string></dict></plist>"
	if err := os.WriteFile(filepath.Join(bundle, "Codex.app", "Contents", "Info.plist"), []byte(plist), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimePath := filepath.Join(resources, "codex")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverDesktopApp(runtimePath); got != resolvePath(gui) {
		t.Fatalf("discover = %q, want %q", got, resolvePath(gui))
	}
	// A bundle without the advertised executable yields nothing.
	if err := os.Remove(gui); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverDesktopApp(runtimePath); got != "" {
		t.Fatalf("discover = %q, want empty", got)
	}
	// No bundle ancestor at all yields nothing on macOS.
	if got := DiscoverDesktopApp(filepath.Join(t.TempDir(), "codex")); got != "" {
		t.Fatalf("discover = %q, want empty", got)
	}
	if DiscoverDesktopApp("") != "" {
		t.Fatal("empty runtime must discover nothing")
	}
}

func TestDesktopAppValidateRejectsScriptsAndRuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	dir := t.TempDir()
	runtimePath := filepath.Join(dir, "codex")
	if err := os.WriteFile(runtimePath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "launcher")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "Codex")
	if err := os.WriteFile(binary, []byte{0xcf, 0xfa}, 0o755); err != nil {
		t.Fatal(err)
	}
	muted := filepath.Join(dir, "muted")
	if err := os.WriteFile(muted, []byte{0xcf, 0xfa}, 0o644); err != nil {
		t.Fatal(err)
	}
	app := newDesktopApp(script, dir, nil)
	if err := app.Validate(runtimePath); err == nil || !strings.Contains(err.Error(), "启动脚本") {
		t.Fatalf("script must be rejected: %v", err)
	}
	app = newDesktopApp(runtimePath, dir, nil)
	if err := app.Validate(runtimePath); err == nil || !strings.Contains(err.Error(), "运行时") {
		t.Fatalf("runtime must be rejected: %v", err)
	}
	app = newDesktopApp(muted, dir, nil)
	if err := app.Validate(runtimePath); err == nil {
		t.Fatal("non-executable must be rejected")
	}
	app = newDesktopApp(binary, dir, nil)
	if err := app.Validate(runtimePath); err != nil {
		t.Fatalf("real binary must validate: %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Recover replays rollback.json after an interrupted switch: files return to
// the pre-switch bytes, the previous account becomes active and the rollback
// record is removed.
func TestRecoverRestoresInterruptedSwitch(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	original := []byte("model_provider = \"openai\"\n")
	stale := jsoncompat.NewObject()
	stale.Set("phase", "verifying")
	stale.Set("requestId", testRequestID)
	stale.Set("targetId", testAccountID)
	stale.Set("error", nil)
	if err := os.WriteFile(filepath.Join(root, "switch.json"), jsoncompat.EncodeCompact(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	rollback := jsoncompat.NewObject()
	rollback.Set("files", encodeSnapshot(map[string][]byte{"config.toml": original}))
	rollback.Set("activeId", otherAccountID)
	rollback.Set("desktopExecutable", fixture.guiPath)
	if err := os.WriteFile(filepath.Join(root, "rollback.json"), jsoncompat.EncodeCompact(rollback), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.home, "config.toml"), []byte("model_provider = \"switched\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.AccountsPublic().MustGet("switch").(*jsoncompat.Object); err != true {
		t.Fatal("panel state missing")
	}

	if _, err := fixture.manager.Recover(jsoncompat.NewObject()); err == nil ||
		!strings.Contains(err.Error(), "请确认恢复原接入") {
		t.Fatalf("unconfirmed recover must fail: %v", err)
	}
	result, err := fixture.manager.Recover(func() *jsoncompat.Object {
		body := jsoncompat.NewObject()
		body.Set("confirmed", true)
		return body
	}())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if result == nil {
		t.Fatal("recover must return the panel status")
	}
	waitForPhase(t, fixture.manager, "restored")
	if current := readFile(t, filepath.Join(fixture.home, "config.toml")); string(current) != string(original) {
		t.Fatalf("config.toml not restored: %q", current)
	}
	if active := pyString(indexValue(fixture.manager.readIndexFile(), "activeId")); active != otherAccountID {
		t.Fatalf("activeId must return to the backup account: %q", active)
	}
	if _, err := os.Stat(filepath.Join(root, "rollback.json")); !os.IsNotExist(err) {
		t.Fatal("rollback record must be removed after a successful recover")
	}
}

// Guards: nothing to recover outside 'interrupted'; a missing rollback record
// means nothing was applied and the switch parks in 'failed'.
func TestRecoverGuards(t *testing.T) {
	confirmed := jsoncompat.NewObject()
	confirmed.Set("confirmed", true)
	if _, err := newSwitchFixture(t, "api").manager.Recover(confirmed); err == nil ||
		!strings.Contains(err.Error(), "当前不需要恢复") {
		t.Fatalf("idle recover must be refused: %v", err)
	}

	// A fresh manager must read the stale switch state before Recover: the
	// in-memory state only loads once, so the record is written first.
	fixture := newSwitchFixture(t, "api")
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	stale := jsoncompat.NewObject()
	stale.Set("phase", "applying")
	stale.Set("requestId", testRequestID)
	stale.Set("targetId", testAccountID)
	stale.Set("error", nil)
	if err := os.WriteFile(filepath.Join(root, "switch.json"), jsoncompat.EncodeCompact(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if phase := fixture.manager.AccountsPublic().MustGet("switch").(*jsoncompat.Object).MustGet("phase"); phase != "interrupted" {
		t.Fatalf("stale phase must normalize to interrupted, got %v", phase)
	}
	if _, err := fixture.manager.Recover(confirmed); err != nil {
		t.Fatalf("recover without rollback must not error: %v", err)
	}
	state := waitForPhase(t, fixture.manager, "failed")
	if message, _ := state.MustGet("error").(string); !strings.Contains(message, "未修改接入") {
		t.Fatalf("unexpected failure message: %q", message)
	}
}

// A rollback record whose desktop executable cannot launch parks the recover
// back in 'interrupted' with the manual-recovery hint.
func TestRecoverFailureParksInterrupted(t *testing.T) {
	fixture := newSwitchFixture(t, "api")
	root := filepath.Join(fixture.manager.DataDir, "accounts")
	stale := jsoncompat.NewObject()
	stale.Set("phase", "starting")
	stale.Set("requestId", testRequestID)
	stale.Set("targetId", testAccountID)
	stale.Set("error", nil)
	if err := os.WriteFile(filepath.Join(root, "switch.json"), jsoncompat.EncodeCompact(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "not-an-app")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rollback := jsoncompat.NewObject()
	rollback.Set("files", encodeSnapshot(map[string][]byte{"config.toml": []byte("x")}))
	rollback.Set("activeId", otherAccountID)
	rollback.Set("desktopExecutable", script)
	if err := os.WriteFile(filepath.Join(root, "rollback.json"), jsoncompat.EncodeCompact(rollback), 0o600); err != nil {
		t.Fatal(err)
	}
	confirmed := jsoncompat.NewObject()
	confirmed.Set("confirmed", true)
	if _, err := fixture.manager.Recover(confirmed); err != nil {
		t.Fatalf("recover: %v", err)
	}
	state := waitForPhase(t, fixture.manager, "interrupted")
	if message, _ := state.MustGet("error").(string); !strings.Contains(message, "恢复未完成") {
		t.Fatalf("unexpected interrupted message: %q", message)
	}
}
