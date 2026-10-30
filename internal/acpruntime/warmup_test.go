package acpruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// lazyAgent is the shape of zcode/workbuddy: session/new only reserves an id,
// the session reaches the agent's own store on the first prompt, and
// session/load refuses an id it never materialized. Both facts are persisted
// in dir so a second manager (a restart) sees them.
func lazyAgent(dir string) string {
	return fmt.Sprintf(`import json,os,sys
DIR = %q
def read(name):
 try:
  with open(os.path.join(DIR,name)) as fh:
   return [line.strip() for line in fh if line.strip()]
 except FileNotFoundError:
  return []
def append(name, value):
 with open(os.path.join(DIR,name),"a") as fh:
  fh.write(value + "\n")
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id"); params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"placeholder-1"}
 elif method == "session/list":
  result={"sessions":[{"sessionId":sid,"title":"团队 · 成员","cwd":"/work","updatedAt":1} for sid in read("materialized.txt")]}
 elif method == "session/load":
  if params.get("sessionId") not in read("materialized.txt"):
   print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32603,"message":"Internal error"}}),flush=True)
   continue
  result={"sessionId":params.get("sessionId")}
 elif method == "session/prompt":
  text=""
  for block in params.get("prompt") or []:
   if isinstance(block,dict) and block.get("text"):
    text=block["text"]; break
  append("prompts.txt", params.get("sessionId","") + "\t" + text)
  append("materialized.txt", params.get("sessionId",""))
  result={"stopReason":"end_turn"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`, dir)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	lines := []string{}
	for _, line := range splitLines(string(raw)) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func splitLines(text string) []string {
	lines := []string{}
	current := ""
	for _, char := range text {
		if char == '\n' {
			lines = append(lines, current)
			current = ""
			continue
		}
		current += string(char)
	}
	if current != "" {
		lines = append(lines, current)
	}
	return lines
}

// waitForFile polls until path has at least want lines: the agent writes from
// another process, so the assertion cannot run on a fixed schedule.
func waitForFile(t *testing.T, path string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lines := readLines(t, path); len(lines) >= want {
			return lines
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reached %d lines", filepath.Base(path), want)
	return nil
}

// TestWarmupMaterializesLazySession is the bug from the field: a member slot
// created but never messaged cannot be loaded after a restart, because the
// agent only writes the session on its first prompt.
func TestWarmupMaterializesLazySession(t *testing.T) {
	dir := t.TempDir()
	manager := NewCommand(writeFakeAgent(t, lazyAgent(dir)))
	manager.Kind = "fake"
	created, err := manager.Create("req-1", "/work", "团队 · 成员", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "placeholder-1" {
		t.Fatalf("created id = %s", created.ID)
	}
	// Before the warm-up the agent has nothing to load: this is exactly what
	// the gateway hit after the idle sweep closed the creating process. A
	// second manager stands in for that restart — the live handle would answer
	// Open from memory.
	probe := NewCommand(writeFakeAgent(t, lazyAgent(dir)))
	if _, openErr := probe.Open("placeholder-1", "/work", 3*time.Second); openErr == nil {
		t.Fatal("a never-prompted session must not be loadable")
	}
	probe.Close()
	if err := created.Warmup(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, created)

	prompts := waitForFile(t, filepath.Join(dir, "prompts.txt"), 1)
	if prompts[0] != "placeholder-1\t"+WarmupPrompt {
		t.Fatalf("warm-up prompt = %q", prompts[0])
	}
	manager.RefreshList()
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 1 || rows[0].MustGet("sessionId") != "placeholder-1" {
		t.Fatalf("warmed session rows = %v err=%v", rows, err)
	}
	manager.Close()

	// A restart now finds a materialized session instead of a dead placeholder.
	restarted := NewCommand(writeFakeAgent(t, lazyAgent(dir)))
	defer restarted.Close()
	if _, err := restarted.Open("placeholder-1", "/work", 3*time.Second); err != nil {
		t.Fatalf("warmed session must reload after a restart: %v", err)
	}
}

// TestWarmupSkipsAutoContinue keeps the synthetic turn out of the resume
// chain: a warm-up nobody asked about must not trigger 继续.
func TestWarmupSkipsAutoContinue(t *testing.T) {
	dir := t.TempDir()
	manager := NewCommand(writeFakeAgent(t, lazyAgent(dir)))
	defer manager.Close()
	var resumed int32
	manager.AutoContinue = func(*Session) { atomic.AddInt32(&resumed, 1) }
	created, err := manager.Create("req-2", "/work", "团队 · 成员", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Warmup(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, created)
	// The hook runs in its own goroutine after the turn settles.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&resumed); got != 0 {
		t.Fatalf("warm-up reached the auto-continue hook %d times", got)
	}
	// A human turn still fires it: the suppression is scoped to the warm-up.
	if err := created.Prompt("继续", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	waitSettled(t, created)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&resumed) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&resumed); got != 1 {
		t.Fatalf("human turn did not reach the hook (hits=%d)", got)
	}
}

// TestManagerAliasesCoverPlaceholderAndBackend guards the delete path: the
// same chat is listed under the placeholder before its first turn and under
// the backend id afterwards, so both have to be treated as one.
func TestManagerAliasesCoverPlaceholderAndBackend(t *testing.T) {
	store := LoadCreated(filepath.Join(t.TempDir(), "created-sessions.json"))
	store.Remember("fake", "placeholder-1", "/work", "团队 · 成员", 1)
	manager := NewCommand(nil)
	manager.Kind = "fake"
	manager.Created = store
	manager.Canonical = func(id string) string {
		if id == "placeholder-1" {
			return "sess_backend"
		}
		return ""
	}
	if got := manager.Aliases("placeholder-1"); len(got) != 1 || got[0] != "sess_backend" {
		t.Fatalf("placeholder aliases = %v", got)
	}
	if got := manager.Aliases("sess_backend"); len(got) != 1 || got[0] != "placeholder-1" {
		t.Fatalf("backend aliases = %v", got)
	}
	if got := manager.Aliases("unrelated"); len(got) != 0 {
		t.Fatalf("unrelated aliases = %v", got)
	}
}
