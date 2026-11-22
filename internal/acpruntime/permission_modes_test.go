package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// fakeModeAgent starts a scripted ACP agent advertising the given modes and
// config options. Every session/set_mode and session/set_config_option call
// is appended to the returned log so tests can assert what reached the wire.
func fakeModeAgent(t *testing.T, kind, modes, options string) (*Session, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `import json,sys
LOG=` + "'" + logPath + "'" + `
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-modes","modes":json.loads('` + modes + `'),"configOptions":json.loads('` + options + `')}
 elif method == "session/set_mode":
  open(LOG,"a").write("set_mode "+params.get("modeId","")+"\n"); result={}
 elif method == "session/set_config_option":
  open(LOG,"a").write("set_config_option "+params.get("configId","")+"="+str(params.get("value",""))+"\n"); result={}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(dir, "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.Kind = kind
	t.Cleanup(manager.Close)
	session, err := manager.Create("seed", "/work", kind+" 会话", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return session, logPath
}

func wireCalls(t *testing.T, logPath string) []string {
	t.Helper()
	raw, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(strings.ReplaceAll(string(raw), " ", "_"))
}

const claudeModes = `{"currentModeId":"auto","availableModes":[{"id":"default","name":"Manual"},{"id":"acceptEdits","name":"Accept edits"},{"id":"plan","name":"Plan"},{"id":"auto","name":"Auto"},{"id":"bypassPermissions","name":"Bypass permissions"}]}`

func currentPermission(session *Session) any { return session.Permissions().MustGet("current") }

func TestDefaultWorkModeKeepsAgentAutoMode(t *testing.T) {
	session, log := fakeModeAgent(t, "claude", claudeModes, `[]`)
	if err := session.ApplyWorkMode("default", nil); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); len(calls) != 0 {
		t.Fatalf("default work mode outside plan must not touch the agent: %v", calls)
	}
	if current := currentPermission(session); current != "auto" {
		t.Fatalf("permission after send = %v, want auto", current)
	}
}

func TestLeavingPlanRestoresPriorPermission(t *testing.T) {
	session, log := fakeModeAgent(t, "claude", claudeModes, `[]`)
	if err := session.ApplyWorkMode("plan", nil); err != nil {
		t.Fatal(err)
	}
	if mode := session.Snapshot().MustGet("collaborationMode"); mode != "plan" {
		t.Fatalf("collaboration mode = %v, want plan", mode)
	}
	state := session.Permissions()
	if state.MustGet("planActive") != true || state.MustGet("current") != "auto" {
		t.Fatalf("plan should report the preset it will restore: %s", jsoncompat.EncodeCompact(state))
	}
	if err := session.ApplyWorkMode("default", nil); err != nil {
		t.Fatal(err)
	}
	calls := wireCalls(t, log)
	if strings.Join(calls, ",") != "set_mode_plan,set_mode_auto" {
		t.Fatalf("wire calls = %v, want plan then auto", calls)
	}
}

func TestPermissionPickedDuringPlanAppliesOnExit(t *testing.T) {
	session, log := fakeModeAgent(t, "claude", claudeModes, `[]`)
	if err := session.ApplyWorkMode("plan", nil); err != nil {
		t.Fatal(err)
	}
	if err := session.SetPermission("acceptEdits"); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); strings.Join(calls, ",") != "set_mode_plan" {
		t.Fatalf("picking a preset in plan must not leave plan: %v", calls)
	}
	if err := session.ApplyWorkMode("default", nil); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); calls[len(calls)-1] != "set_mode_acceptEdits" {
		t.Fatalf("exit plan should apply the picked preset: %v", calls)
	}
}

func TestAgentEnteredPlanRemembersPriorPermission(t *testing.T) {
	session, log := fakeModeAgent(t, "claude", claudeModes, `[]`)
	update := jsoncompat.NewObject()
	update.Set("sessionUpdate", "current_mode_update")
	update.Set("currentModeId", "plan")
	params := jsoncompat.NewObject()
	params.Set("sessionId", session.ID)
	params.Set("update", update)
	message := jsoncompat.NewObject()
	message.Set("method", "session/update")
	message.Set("params", params)
	session.onNotification(message)
	if err := session.ApplyWorkMode("default", nil); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); strings.Join(calls, ",") != "set_mode_auto" {
		t.Fatalf("leaving agent-entered plan should restore auto: %v", calls)
	}
}

func TestKimiPermissionLabelsOverrideSwappedDescriptions(t *testing.T) {
	modes := `{"currentModeId":"default","availableModes":[{"id":"default","name":"Default"},{"id":"plan","name":"Plan"},{"id":"auto","name":"Auto","description":"Auto-approve safe operations."},{"id":"yolo","name":"YOLO","description":"Auto-approve everything."}]}`
	session, log := fakeModeAgent(t, "kimi", modes, `[]`)
	state := session.Permissions()
	options, _ := state.MustGet("options").(*jsoncompat.Array)
	if state.MustGet("supported") != true || len(options.Items) != 3 {
		t.Fatalf("kimi presets = %s, want default/auto/yolo", jsoncompat.EncodeCompact(state))
	}
	names := map[string]string{}
	for _, item := range options.Items {
		option := item.(*jsoncompat.Object)
		names[option.MustGet("value").(string)] = option.MustGet("name").(string)
	}
	if names["yolo"] != "按需询问（规则）" || names["auto"] != "从不询问" {
		t.Fatalf("kimi labels = %v", names)
	}
	if err := session.SetPermission("yolo"); err != nil {
		t.Fatal(err)
	}
	if err := session.SetPermission("plan"); err == nil {
		t.Fatal("plan accepted as a permission preset")
	}
	if calls := wireCalls(t, log); strings.Join(calls, ",") != "set_mode_yolo" {
		t.Fatalf("wire calls = %v", calls)
	}
}

func TestMcodePermissionUsesConfigOption(t *testing.T) {
	modes := `{"currentModeId":"default","availableModes":[{"id":"default","name":"Default"},{"id":"plan","name":"Plan"}]}`
	options := `[{"id":"permissionMode","name":"Permission mode","category":"_permission","currentValue":"default","options":[{"value":"default","name":"Ask"},{"value":"auto","name":"Auto"},{"value":"bypassPermissions","name":"Full access"}]}]`
	session, log := fakeModeAgent(t, "mcode", modes, options)
	if current := currentPermission(session); current != "default" {
		t.Fatalf("initial permission = %v", current)
	}
	if err := session.SetPermission("auto"); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); strings.Join(calls, ",") != "set_config_option_permissionMode=auto" {
		t.Fatalf("wire calls = %v", calls)
	}
	if current := currentPermission(session); current != "auto" {
		t.Fatalf("permission after switch = %v", current)
	}
	if err := session.ApplyWorkMode("default", nil); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); len(calls) != 1 {
		t.Fatalf("default send must not touch mcode's permission option: %v", calls)
	}
}

func TestZcodeLeavingPlanRestoresGuarded(t *testing.T) {
	modes := `{"currentModeId":"guarded","availableModes":[{"id":"plan","name":"Plan"},{"id":"build","name":"Build"},{"id":"edit","name":"Edit"},{"id":"guarded","name":"Guarded"},{"id":"yolo","name":"Yolo"}]}`
	session, log := fakeModeAgent(t, "zcode", modes, `[]`)
	modeMap := map[string]string{"plan": "plan", "default": "yolo"}
	if err := session.ApplyWorkMode("default", modeMap); err != nil {
		t.Fatal(err)
	}
	if err := session.ApplyWorkMode("plan", modeMap); err != nil {
		t.Fatal(err)
	}
	if err := session.ApplyWorkMode("default", modeMap); err != nil {
		t.Fatal(err)
	}
	if calls := wireCalls(t, log); strings.Join(calls, ",") != "set_mode_plan,set_mode_guarded" {
		t.Fatalf("zcode wire calls = %v, want guarded kept and restored", calls)
	}
}
