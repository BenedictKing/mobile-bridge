package goal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sidecar is persistent like Python's GoalRPC: many calls ride one
// initialized process, a dead connection is discarded, and the next idempotent
// read heals onto a fresh process. The fake also refuses methods that arrive
// before initialize, proving the handshake ordering.
func TestGoalSidecarPersistenceAndHealing(t *testing.T) {
	dir := t.TempDir()
	spawnLog := filepath.Join(dir, "spawns.txt")
	script := `#!/usr/bin/env python3
import json, os, sys
spawn_log = "` + spawnLog + `"
with open(spawn_log, "a") as fh:
    fh.write("spawn\n")
initialized = False
for line in sys.stdin:
    m = json.loads(line)
    ident, method, params = m.get("id"), m.get("method"), m.get("params") or {}
    if method == "initialize":
        initialized = True
        result = {}
    elif not initialized:
        print(json.dumps({"id": ident, "error": {"message": "not initialized"}}), flush=True)
        continue
    elif method == "thread/goal/get":
        if params.get("die"):
            sys.exit(0)
        result = {"goal": {"objective": "ship", "status": "active", "createdAt": 1}}
    elif method == "thread/goal/set":
        result = {"goal": {"objective": params.get("objective", "ship"),
                           "status": params.get("status", "active"), "createdAt": 1}}
    elif method == "thread/goal/clear":
        result = {}
    else:
        result = {}
    print(json.dumps({"id": ident, "result": result}), flush=True)
`
	runtimePath := filepath.Join(dir, "runtime.py")
	if err := os.WriteFile(runtimePath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := New(t.TempDir(), runtimePath)

	// Three calls, one process.
	for index := 0; index < 3; index++ {
		if current, err := manager.Get("thread-1"); err != nil ||
			current.MustGet("objective") != "ship" {
			t.Fatalf("call %d: get=%v err=%v", index, current, err)
		}
	}
	if _, err := manager.Set("thread-1", "ship safely", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Clear("thread-1"); err != nil {
		t.Fatal(err)
	}
	if spawns := strings.Count(readFileString(t, spawnLog), "spawn"); spawns != 1 {
		t.Fatalf("five calls must ride one sidecar process, spawns=%d", spawns)
	}

	// Kill the sidecar through the fake: the failing read drops the
	// connection; the same-request retry re-sends the die flag (it dies too,
	// spawns 2+3) and the request surfaces an error. The next ordinary read
	// heals onto a fresh process and succeeds.
	if _, err := manager.requestForTest("thread/goal/get", map[string]any{"threadId": "thread-1", "die": true}); err == nil {
		t.Fatal("dying sidecar must surface an error")
	}
	if current, err := manager.Get("thread-1"); err != nil || current.MustGet("objective") != "ship" {
		t.Fatalf("healed read: get=%v err=%v", current, err)
	}
	if spawns := strings.Count(readFileString(t, spawnLog), "spawn"); spawns != 3 {
		t.Fatalf("dead sidecar must be dropped and replaced, spawns=%d", spawns)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
