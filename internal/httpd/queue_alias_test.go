package httpd

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// 懒会话占位 UUID 与物化后的真实 id（与 internal/acpruntime 的
// alias_open_test 保持同一形态，经 httpd 派发链路复现网关重启后的冷恢复）。
const (
	queuePlaceholder = "6aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	queueBackend     = "sess_queue_backend"
)

// lazyQueueAgent 把 session/load / session/prompt 收到的 sessionId 落盘。
// 默认只接受真实后端 id，占位 id 命中血缘错误（lineage），复现 zcode 冷恢复
// 失败；lineageBroken 时连真实 id 也一律血缘拒绝，复现解析后仍损坏的会话。
func lazyQueueAgent(t *testing.T, dir string, lineageBroken bool) []string {
	t.Helper()
	// acceptLoad 折进脚本：默认仅真实后端 id 放行；lineageBroken 时连真实
	// id 也血缘拒绝（复现别名解析后仍损坏的会话）。
	acceptLoad := "sid != \"" + queueBackend + "\""
	if lineageBroken {
		acceptLoad = "True"
	}
	script := `import json,os,sys
DIR = "` + dir + `"
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
        reply(ident, {"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]})
        continue
    if method == "session/list":
        reply(ident, {"sessions":[{"sessionId":"` + queueBackend + `","title":"真实会话","cwd":DIR}]})
        continue
    if method == "session/load":
        sid=(m.get("params") or {}).get("sessionId","")
        record("loads.txt", sid)
        if ` + acceptLoad + `:
            deny(ident, "project.session-lineage-invalid: session lineage chain is broken")
            continue
        reply(ident, {"sessionId":sid})
        continue
    if method == "session/prompt":
        sid=(m.get("params") or {}).get("sessionId","")
        blocks=(m.get("params") or {}).get("prompt") or []
        text=next((b.get("text","") for b in blocks if isinstance(b,dict)),"")
        record("prompts.txt", sid+" "+text)
        reply(ident, {"stopReason":"end_turn"})
        continue
    reply(ident, {})
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"/usr/bin/env", "python3", "-u", path}
}

// queueLazyCanonical 复刻 zcodeCanonicalFactory：读懒映射 JSON。
func queueLazyCanonical(dir string) func(string) string {
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

func newLazyQueueServer(t *testing.T, agentDir string, lineageBroken bool) (*Server, *acpruntime.Manager) {
	t.Helper()
	manager := acpruntime.NewCommand(lazyQueueAgent(t, agentDir, lineageBroken))
	manager.Kind = "zcode"
	manager.Created = acpruntime.LoadCreated(filepath.Join(agentDir, "created-sessions.json"))
	manager.Canonical = queueLazyCanonical(agentDir)
	server, _, _ := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}
	return server, manager
}

// 网关重启后团队/队列仍持有占位 id：派发必须把 session/load 与 prompt 都
// 发往映射出的真实 sess_*，一次成功，不触发血缘错误。
func TestACPQueueDispatchResolvesLazyPlaceholder(t *testing.T) {
	agentDir := t.TempDir()
	raw := `{"` + queuePlaceholder + `":{"zcodeSid":"` + queueBackend + `"}}`
	if err := os.WriteFile(filepath.Join(agentDir, "acp-lazy-sessions.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	server, manager := newLazyQueueServer(t, agentDir, false)
	defer manager.Close()

	const submission = "7aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := server.Queue.enqueue(queuePlaceholder, submission, "团队任务", "queue", "", nil, nil, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	server.dispatchQueuedThread(queuePlaceholder, "", nil)

	entry := server.Queue.get(queuePlaceholder, submission)
	if entry == nil || entry.Status != SubAccepted {
		t.Fatalf("entry = %+v, want accepted", entry)
	}
	if lines := recordedFileLines(agentDir, "loads.txt"); len(lines) == 0 || lines[0] != queueBackend {
		t.Fatalf("session/load ids = %v, want first load on %s", lines, queueBackend)
	}
	for _, line := range recordedFileLines(agentDir, "loads.txt") {
		if line != queueBackend {
			t.Fatalf("placeholder leaked into session/load: %v", recordedFileLines(agentDir, "loads.txt"))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if prompts := recordedFileLines(agentDir, "prompts.txt"); len(prompts) == 1 &&
			prompts[0] == queueBackend+" 团队任务" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("prompt ids = %v, want [%s 团队任务]", recordedFileLines(agentDir, "prompts.txt"), queueBackend)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 别名解析后真实会话仍血缘损坏时必须立即失败（进入团队分诊），不得爬完
// 10 级重试梯、不得反复拉起 Agent。
func TestACPQueueLineageInvalidFailsFast(t *testing.T) {
	agentDir := t.TempDir()
	server, manager := newLazyQueueServer(t, agentDir, true)
	defer manager.Close()

	// 列表里的真实会话本身就是坏的：占位解析无济于事。
	const broken = "sess_queue_backend"
	const submission = "8aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := server.Queue.enqueue(broken, submission, "团队任务", "queue", "", nil, nil, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	server.dispatchQueuedThread(broken, "", nil)

	entry := server.Queue.get(broken, submission)
	if entry == nil || entry.Status != SubFailed {
		t.Fatalf("entry = %+v, want failed at once", entry)
	}
	if !strings.Contains(entry.Error, "血缘") || !strings.Contains(entry.Error, "lineage") {
		t.Fatalf("error lost the lineage reason: %s", entry.Error)
	}
	if entry.QueueAttempts != 0 {
		t.Fatalf("deterministic failure climbed the ladder: %+v", entry)
	}
	// 再跑若干个派发轮回也不会重试：条目已终态，Agent 不再被拉起。
	for i := 0; i < 3; i++ {
		server.dispatchQueuedThread(broken, "", nil)
	}
	if loads := len(recordedFileLines(agentDir, "loads.txt")); loads > 2 {
		t.Fatalf("session/load attempts = %d, want at most 2 (no retry ladder)", loads)
	}
}

func TestACPReconnectReusesLiveLazyAlias(t *testing.T) {
	agentDir := t.TempDir()
	server, manager := newLazyQueueServer(t, agentDir, true)
	defer manager.Close()
	created, err := manager.Create(queuePlaceholder, agentDir, "仍在运行", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	mapping := `{"` + queuePlaceholder + `":{"zcodeSid":"` + queueBackend + `"}}`
	if err := os.WriteFile(filepath.Join(agentDir, "acp-lazy-sessions.json"), []byte(mapping), 0o600); err != nil {
		t.Fatal(err)
	}
	token, authSession, err := server.Auth.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := authSession.MustGet("csrf").(string)
	response := featureRequest(server, token, csrf, http.MethodPost,
		"/api/sessions/"+queueBackend+"/reconnect?cwd="+url.QueryEscape(agentDir), `{"activate":true}`)
	if response.Code != http.StatusOK {
		t.Fatalf("live alias must not be resumed again: status=%d body=%s", response.Code, response.Body.String())
	}
	if loads := recordedFileLines(agentDir, "loads.txt"); len(loads) != 0 {
		t.Fatalf("reconnect spawned another session/load: %v", loads)
	}
	if manager.Get(queuePlaceholder) != created || manager.Get(queueBackend) != created {
		t.Fatal("reconnect did not preserve the original live handle")
	}
}

func recordedFileLines(dir, name string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
