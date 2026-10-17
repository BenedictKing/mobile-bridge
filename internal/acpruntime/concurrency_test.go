// Concurrency gate for the session view boundary: the notification reader
// goroutine folds session/update payloads (reassigning the advertised command
// list, rewriting the modes object) while HTTP-shaped readers walk the
// catalog, the skill list and switch modes. `go test -race` pins the locked
// snapshots these reads go through.
package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// chattyAgent answers the handshake, advertises modes/configOptions, answers
// set_mode, and continuously emits available_commands_update notifications
// from a background thread once the session is loaded.
func chattyAgent(t *testing.T) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys,threading
lock = threading.Lock()
def spam():
    n = 0
    while True:
        n += 1
        names = [{"name": "skill:%d" % n, "description": "技能"}] * 3
        notice = {"jsonrpc": "2.0", "method": "session/update",
                  "params": {"update": {"sessionUpdate": "available_commands_update",
                                        "availableCommands": names}}}
        with lock:
            print(json.dumps(notice), flush=True)
threading.Thread(target=spam, daemon=True).start()
for line in sys.stdin:
    m = json.loads(line); method = m.get("method"); ident = m.get("id")
    result = {}
    if method == "initialize":
        result = {"protocolVersion": 1, "agentCapabilities": {}, "agentInfo": {"name": "fake"}, "authMethods": []}
    elif method == "session/load":
        result = {"sessionId": "sess-chatty",
                  "modes": {"currentModeId": "default",
                            "availableModes": [{"id": "default"}, {"id": "plan"}]},
                  "configOptions": [{"id": "model", "category": "model", "currentValue": "gpt",
                                     "options": [{"value": "gpt", "name": "GPT"}]},
                                    {"id": "thought_level", "category": "thought_level",
                                     "currentValue": "high",
                                     "options": [{"value": "high", "name": "High"}]}]}
    elif method == "session/set_mode":
        result = {}
    with lock:
        print(json.dumps({"jsonrpc": "2.0", "id": ident, "result": result}), flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func TestCatalogReadsRaceNotificationStream(t *testing.T) {
	manager := NewCommand(chattyAgent(t))
	defer manager.Close()
	session, err := manager.Open("sess-chatty", "/work", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(700 * time.Millisecond)
	var readers sync.WaitGroup
	for id := 0; id < 3; id++ {
		readers.Add(1)
		go func(id int) {
			defer readers.Done()
			for time.Now().Before(deadline) {
				rows := manager.CommandSkills(session)
				for _, row := range rows {
					if _, ok := row.MustGet("name").(string); !ok {
						t.Errorf("skill row without a name: %v", row)
					}
				}
				catalog := manager.Catalog(session, "models", "", nil)
				if current := catalog.MustGet("currentModel"); current == nil {
					t.Errorf("catalog lost currentModel: %v", catalog)
				}
				if err := session.SetMode([]string{"default", "plan"}[id%2]); err != nil {
					t.Errorf("SetMode failed: %v", err)
				}
			}
		}(id)
	}
	readers.Wait()

	// The spam thread keeps mutating the advertised command list; a final
	// read must still produce a coherent snapshot.
	if rows := manager.CommandSkills(session); rows == nil {
		t.Fatal("nil skill rows")
	}
	if snapshot := session.Snapshot(); snapshot.MustGet("status") == nil {
		t.Fatal("snapshot lost its status")
	}
	_ = jsoncompat.NewObject
}
