package acpruntime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func writeFakeAgent(t *testing.T, script string) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// usageSilentAgent loads a session but never reports context usage — the state a
// reopened chat is in right after a gateway restart.
const usageSilentAgent = `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`

func TestOpenRestoresRecordedContextUsage(t *testing.T) {
	command := writeFakeAgent(t, usageSilentAgent)
	ledgerPath := filepath.Join(t.TempDir(), "context-usage.json")
	LoadUsage(ledgerPath).Record("fake", "sess-existing", 48066, 960000)

	manager := NewCommand(command)
	manager.Usage = LoadUsage(ledgerPath)
	manager.Kind = "fake"
	defer manager.Close()
	session, err := manager.Open("sess-existing", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := session.Snapshot().MustGet("usage").(*jsoncompat.Object)
	if !ok {
		t.Fatalf("reopened session lost its usage: %v", session.Snapshot())
	}
	if usage.MustGet("used").(json.Number).String() != "48066" ||
		usage.MustGet("size").(json.Number).String() != "960000" {
		t.Fatalf("usage = %v", usage)
	}
	// Restored counts are a replay, not the live context: the phone labels
	// them so a compaction in between cannot masquerade as current usage.
	if usage.MustGet("stale") != true {
		t.Fatalf("restored usage is not marked stale: %v", usage)
	}
}

func TestUsageUpdatePersistsAcrossRestart(t *testing.T) {
	reportingAgent := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/load":
  result={"sessionId":m.get("params",{}).get("sessionId")}
  print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":m.get("params",{}).get("sessionId"),"update":{"sessionUpdate":"usage_update","used":13000,"size":960000}}}),flush=True)
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	ledgerPath := filepath.Join(t.TempDir(), "context-usage.json")
	manager := NewCommand(writeFakeAgent(t, reportingAgent))
	manager.Usage = LoadUsage(ledgerPath)
	manager.Kind = "fake"
	session, err := manager.Open("sess-existing", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if usage, ok := session.Snapshot().MustGet("usage").(*jsoncompat.Object); ok {
			if usage.MustGet("used").(json.Number).String() == "13000" {
				// A live report is authoritative and never marked stale.
				if _, stale := usage.Get("stale"); stale {
					t.Fatalf("live usage marked stale: %v", usage)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage never arrived: %v", session.Snapshot())
		}
		time.Sleep(20 * time.Millisecond)
	}
	manager.Close()

	recorded := LoadUsage(ledgerPath).Get("fake", "sess-existing")
	if recorded == nil || recorded.MustGet("used").(json.Number).String() != "13000" ||
		recorded.MustGet("size").(json.Number).String() != "960000" {
		t.Fatalf("usage was not persisted: %v", recorded)
	}

	// Restart: a fresh manager, an agent that does not replay usage, and the
	// same ledger — the reopened chat still shows the last known count.
	restarted := NewCommand(writeFakeAgent(t, usageSilentAgent))
	restarted.Usage = LoadUsage(ledgerPath)
	restarted.Kind = "fake"
	defer restarted.Close()
	reopened, err := restarted.Open("sess-existing", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	usage, ok := reopened.Snapshot().MustGet("usage").(*jsoncompat.Object)
	if !ok || usage.MustGet("used").(json.Number).String() != "13000" {
		t.Fatalf("usage lost across restart: %v", reopened.Snapshot())
	}
}

func TestUsageStoreIgnoresIncompleteReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context-usage.json")
	store := LoadUsage(path)
	store.Record("fake", "sess", 120, nil)
	store.Record("fake", "sess", 120, 0)
	store.Record("fake", "sess", -1, 960000)
	if usage := store.Get("fake", "sess"); usage != nil {
		t.Fatalf("incomplete report was stored: %v", usage)
	}
	store.Record("fake", "sess", 120, 960000)
	store.Forget("fake", "sess")
	if usage := store.Get("fake", "sess"); usage != nil {
		t.Fatalf("forgotten session still has usage: %v", usage)
	}
}
