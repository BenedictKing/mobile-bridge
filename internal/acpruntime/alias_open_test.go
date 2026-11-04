package acpruntime

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// lazyPlaceholder 模拟 zcode 懒会话的占位 UUID；sessBackend 是首条 prompt
// 物化后的真实后端 id；sessBroken 单独模拟血缘已损坏的真实会话。
const (
	lazyPlaceholder = "5aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	sessBackend     = "sess_backend_materialized"
	sessBroken      = "sess_broken_lineage"
)

// lazyAliasAgent 接受任意 sessionId 的 session/load 与 prompt 并把收到的
// id 落盘（loads.txt / prompts.txt / spawns.txt）；只有 sessBroken 以血缘
// 错误拒绝——覆盖“解析成功”“无映射透传”“真实会话损坏”三种形态。
const lazyAliasAgent = `import json,os,sys
DIR = os.environ.get("LAZY_DIR", ".")
def record(name, value):
    with open(os.path.join(DIR, name), "a") as fh:
        fh.write(value + "\n")
def reply(ident, result):
    print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}), flush=True)
def deny(ident, message):
    print(json.dumps({"jsonrpc":"2.0","id":ident,"error":{"code":-32000,"message":message}}), flush=True)
for line in sys.stdin:
    m=json.loads(line); method=m.get("method"); ident=m.get("id")
    if method == "initialize":
        record("spawns.txt", "init")
        reply(ident, {"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]})
        continue
    if method == "session/list":
        reply(ident, {"sessions":[{"sessionId":"` + sessBackend + `","title":"真实会话","cwd":DIR}]})
        continue
    if method == "session/load":
        sid=(m.get("params") or {}).get("sessionId","")
        record("loads.txt", sid)
        if sid == "` + sessBroken + `":
            deny(ident, "project.session-lineage-invalid: session lineage chain is broken")
            continue
        reply(ident, {"sessionId":sid})
        continue
    if method == "session/prompt":
        sid=(m.get("params") or {}).get("sessionId","")
        record("prompts.txt", sid)
        reply(ident, {"stopReason":"end_turn"})
        continue
    reply(ident, {})
`

func lazyCanonicalFactory(dir string) func(string) string {
	return func(id string) string {
		raw, err := os.ReadFile(filepath.Join(dir, "acp-lazy-sessions.json"))
		if err != nil {
			return ""
		}
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			return ""
		}
		object, _ := value.(*jsoncompat.Object)
		if object == nil {
			return ""
		}
		entry, _ := object.MustGet(id).(*jsoncompat.Object)
		if entry == nil {
			return ""
		}
		sid, _ := entry.MustGet("zcodeSid").(string)
		return sid
	}
}

func writeLazyMap(t *testing.T, dir string) {
	t.Helper()
	raw := `{"` + lazyPlaceholder + `":{"zcodeSid":"` + sessBackend + `"}}`
	if err := os.WriteFile(filepath.Join(dir, "acp-lazy-sessions.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}

func recordedLines(t *testing.T, dir, name string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.SplitN(string(raw), "\n", -1) {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func newLazyManager(t *testing.T, dir string, withMapping bool) *Manager {
	t.Helper()
	t.Setenv("LAZY_DIR", dir)
	manager := NewCommand(writeFakeAgent(t, lazyAliasAgent))
	manager.Kind = "zcode"
	manager.Created = LoadCreated(filepath.Join(dir, "created-sessions.json"))
	manager.Canonical = lazyCanonicalFactory(dir)
	if withMapping {
		writeLazyMap(t, dir)
	}
	return manager
}

// 重启后懒占位已被映射到真实 sess_*：Open 必须用真实 id 做 session/load，
// 后续 prompt 也发往真实 id，且占位与真实 id 共享同一句柄。
func TestOpenResolvesLazyPlaceholderToBackend(t *testing.T) {
	dir := t.TempDir()
	manager := newLazyManager(t, dir, true)
	defer manager.Close()

	loaded, err := manager.Open(lazyPlaceholder, dir, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != sessBackend {
		t.Fatalf("handle id = %s, want %s", loaded.ID, sessBackend)
	}
	if lines := recordedLines(t, dir, "loads.txt"); len(lines) != 1 || lines[0] != sessBackend {
		t.Fatalf("session/load ids = %v, want exactly [%s]", lines, sessBackend)
	}
	if err := loaded.Prompt("继续", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if lines := recordedLines(t, dir, "prompts.txt"); len(lines) == 1 && lines[0] == sessBackend {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("prompt ids = %v, want exactly [%s]", recordedLines(t, dir, "prompts.txt"), sessBackend)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if manager.Get(lazyPlaceholder) != loaded || manager.Get(sessBackend) != loaded {
		t.Fatal("placeholder and backend id must share one handle")
	}
}

// 尚未物化（无映射）的占位会话保持既有恢复策略：session/load 原样使用
// 请求里的 id，不强行改写也不失败。
func TestUnmappedPlaceholderOpensWithRequestedID(t *testing.T) {
	dir := t.TempDir()
	manager := newLazyManager(t, dir, false)
	defer manager.Close()

	loaded, err := manager.Open(lazyPlaceholder, dir, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != lazyPlaceholder {
		t.Fatalf("handle id = %s, want the placeholder itself", loaded.ID)
	}
	if lines := recordedLines(t, dir, "loads.txt"); len(lines) != 1 || lines[0] != lazyPlaceholder {
		t.Fatalf("session/load ids = %v, want exactly [%s]", lines, lazyPlaceholder)
	}
}

// 占位与真实 id 并发打开只能有一个有效句柄、一个 wrapper 进程。
func TestPlaceholderAndBackendConcurrentOpenShareOneHandle(t *testing.T) {
	dir := t.TempDir()
	manager := newLazyManager(t, dir, true)
	defer manager.Close()

	var wg sync.WaitGroup
	handles := make([]*Session, 2)
	for i, id := range []string{lazyPlaceholder, sessBackend} {
		wg.Add(1)
		go func(slot int, sessionID string) {
			defer wg.Done()
			opened, err := manager.Open(sessionID, dir, 5*time.Second)
			if err != nil {
				t.Error(err)
				return
			}
			handles[slot] = opened
		}(i, id)
	}
	wg.Wait()
	if handles[0] == nil || handles[0] != handles[1] {
		t.Fatalf("concurrent opens produced distinct handles: %p vs %p", handles[0], handles[1])
	}
	if lines := recordedLines(t, dir, "spawns.txt"); len(lines) != 1 {
		t.Fatalf("agent spawns = %d, want 1", len(lines))
	}
	if lines := recordedLines(t, dir, "loads.txt"); len(lines) != 1 || lines[0] != sessBackend {
		t.Fatalf("session/load ids = %v, want exactly [%s]", lines, sessBackend)
	}
}

// Create 存活期间句柄仍在占位键下（其 wrapper 仍是会话所有者）；此后用
// 真实 id 打开必须复用该句柄，而不是再拉起第二个 wrapper。
func TestBackendOpenReusesLiveCreatedPlaceholderHandle(t *testing.T) {
	dir := t.TempDir()
	manager := newLazyManager(t, dir, true)
	defer manager.Close()

	live := &Session{ID: lazyPlaceholder, Cwd: dir,
		View: acpview.NewView(lazyPlaceholder, nil, dir, func() float64 { return 1 })}
	manager.mu.Lock()
	manager.sessions[lazyPlaceholder] = live
	manager.mu.Unlock()
	manager.Created.Remember(manager.Kind, lazyPlaceholder, dir, "团队 · 主控", 1700000000000)

	opened, err := manager.Open(sessBackend, dir, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if opened != live {
		t.Fatalf("backend open spawned a second handle: %p vs %p", opened, live)
	}
	if lines := recordedLines(t, dir, "spawns.txt"); len(lines) != 0 {
		t.Fatalf("backend open spawned %d wrappers, want 0", len(lines))
	}
}

// 按占位 id 删除会话必须清掉真实 id 键下的句柄，否则下次打开又把已删除
// 的会话拉起来。
func TestDropPlaceholderRemovesBackendHandle(t *testing.T) {
	dir := t.TempDir()
	manager := newLazyManager(t, dir, true)
	defer manager.Close()

	if _, err := manager.Open(lazyPlaceholder, dir, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	manager.Drop(lazyPlaceholder)
	if manager.Get(lazyPlaceholder) != nil || manager.Get(sessBackend) != nil {
		t.Fatal("drop via placeholder left the backend-keyed handle alive")
	}
}
