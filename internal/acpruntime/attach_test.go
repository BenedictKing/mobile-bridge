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

func nativeSnapshotForAttach(title string) *acpview.NativeSnapshot {
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-history")
	turn.Set("status", "completed")
	message := jsoncompat.NewObject()
	message.Set("id", "user-history")
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", "历史里的问题")
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	return &acpview.NativeSnapshot{Title: title, Turns: []*jsoncompat.Object{turn}}
}

// A read opens as pure history; the first mutation attaches in place, so the
// phone can intervene in any listed chat without a manual upgrade step.
func TestAttachUpgradesHistoryViewToLive(t *testing.T) {
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
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		return nativeSnapshotForAttach("可接入历史"), nil
	}
	defer manager.Close()

	history, err := manager.OpenHistory("session-attach", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if !history.ReadOnly() || history.DesktopOwned() {
		t.Fatalf("history readOnly=%v desktopOwned=%v", history.ReadOnly(), history.DesktopOwned())
	}

	live, err := manager.Attach("session-attach", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if live.ReadOnly() {
		t.Fatal("attach left the session read-only")
	}
	if live.Process == nil || live.IsClosed() {
		t.Fatal("attach produced no live handle")
	}
	if !history.IsClosed() {
		t.Fatal("the replaced history view kept running after the upgrade")
	}
	again, err := manager.Attach("session-attach", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if again != live {
		t.Fatal("second attach did not reuse the live handle")
	}
	live.Close()
	history.Close()
}

// A desktop-owned view is the one case attach refuses to touch: the wrapper
// would reject session/load again, so mutations stay unavailable.
func TestAttachLeavesDesktopOwnedViewReadOnly(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load":
  print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32603,"message":"Internal error: cannot restore session: session is already owned by an active write handle"}}),flush=True)
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) {
		return nativeSnapshotForAttach("桌面占用"), nil
	}
	defer manager.Close()

	owned, err := manager.Open("session-owned-2", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !owned.ReadOnly() || !owned.DesktopOwned() {
		t.Fatalf("owned readOnly=%v desktopOwned=%v", owned.ReadOnly(), owned.DesktopOwned())
	}
	after, err := manager.Attach("session-owned-2", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ReadOnly() {
		t.Fatal("attach upgraded a desktop-owned session")
	}
	after.Close()
}
