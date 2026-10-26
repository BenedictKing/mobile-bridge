package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// silentAgent answers everything except session/prompt, which it never
// replies to; received cancels are appended to a log for assertions.
func silentAgent(t *testing.T) (argv []string, cancelLog string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	log := filepath.Join(t.TempDir(), "cancels.log")
	cancelLog = log
	script := `import json,sys
log = open(` + pythonRepr(log) + `, "a")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method")
 if "id" not in m:
  if method == "session/cancel":
   log.write(m.get("params",{}).get("sessionId","") + "\n"); log.flush()
  continue
 ident=m.get("id")
 if method == "session/prompt":
  continue  # never answers: the idle timeout must fire
 result={"sessionId":"sess-idle"} if method=="session/load" else {}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}, log
}

func pythonRepr(value string) string {
	return "'" + value + "'"
}

func TestPromptIdleTimeoutCancelsAndFailsTurn(t *testing.T) {
	argv, cancelLog := silentAgent(t)
	manager := NewCommand(argv)
	defer manager.Close()
	session, err := manager.Open("sess-idle", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.activitySlice = 20 * time.Millisecond
	session.mu.Unlock()
	if err := session.Prompt("hello", 60*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if session.Snapshot().MustGet("status") == "idle" && !session.PromptRunning() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	snapshot := session.Snapshot()
	if snapshot.MustGet("status") != "idle" || session.PromptRunning() {
		t.Fatalf("session state=%v running=%v", snapshot.MustGet("status"), session.PromptRunning())
	}
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if len(turns.Items) == 0 {
		t.Fatal("prompt turn missing")
	}
	lastTurn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if lastTurn.MustGet("status") != "failed" {
		t.Fatalf("turn status=%v", lastTurn.MustGet("status"))
	}
	failure := false
	if messages, ok := lastTurn.MustGet("messages").(*jsoncompat.Array); ok {
		for _, item := range messages.Items {
			if message, ok := item.(*jsoncompat.Object); ok {
				if message.MustGet("kind") == "error" {
					failure = true
				}
			}
		}
	}
	if !failure {
		t.Fatalf("turn carries no error entry: %s", string(jsoncompat.EncodeCompact(lastTurn)))
	}
	deadline = time.Now().Add(3 * time.Second)
	var raw []byte
	var readErr error
	for {
		raw, readErr = os.ReadFile(cancelLog)
		if readErr == nil && len(raw) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never received cancel: %v %q", readErr, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A follow-up prompt is accepted again (the first was cleaned up).
	session.mu.Lock()
	session.activitySlice = 20 * time.Millisecond
	session.mu.Unlock()
	if err := session.Prompt("again", time.Hour); err != nil {
		t.Fatalf("follow-up prompt rejected: %v", err)
	}
}

func TestSweepIdleClosesUntouchedSessions(t *testing.T) {
	argv, _ := silentAgent(t)
	manager := NewCommand(argv)
	defer manager.Close()
	fresh, err := manager.Open("sess-fresh", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stale, err := manager.Open("sess-stale", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	stale.mu.Lock()
	stale.touched = time.Now().Add(-10 * time.Minute)
	stale.mu.Unlock()
	if closed := manager.SweepIdle(5 * time.Minute); closed != 1 {
		t.Fatalf("sweep closed=%d", closed)
	}
	if manager.Get("sess-stale") != nil {
		t.Fatal("stale session survived the sweep")
	}
	if manager.Get("sess-fresh") == nil {
		t.Fatal("fresh session was swept")
	}
	_ = fresh
}
