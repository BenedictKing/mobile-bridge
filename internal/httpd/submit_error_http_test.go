package httpd

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// deadTransportACP 启动一个立即退出的进程并等到读循环落定：Submit 必失败，
// 但它从未触发目标会话的 OnClose，会话层 closed 仍为 false。
func deadTransportACP(t *testing.T) *acp.Process {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	dead, err := acp.Start([]string{python, "-u", "-c", "import sys; sys.exit(0)"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, submitErr := dead.Submit("ping", jsoncompat.NewObject()); submitErr != nil {
			return dead
		}
		if time.Now().After(deadline) {
			t.Fatal("假传输进程迟迟未退出")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// submitFailureServer 装配 kimi 假 Agent 的功能服务器，并返回已换入死亡传输
// 的会话：前置检查放行，错误只能在 session/prompt Submit 处暴露。
func submitFailureServer(t *testing.T) (*Server, string, string, *acpruntime.Manager, *acpruntime.Session) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-suberr"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	agentPath := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(agentPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := acpruntime.NewCommand([]string{python, "-u", agentPath})
	manager.Label = "Kimi"
	server, token, csrf := newFeatureServer(t, t.TempDir())
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	create := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions",
		`{"agent":"kimi","cwd":"`+t.TempDir()+`","title":"提交失败会话","id":"1aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	session := manager.Get("sess-suberr")
	if session == nil {
		t.Fatal("会话未建立")
	}
	live := session.Process
	t.Cleanup(func() { live.Close(); manager.Close() })
	session.Process = deadTransportACP(t)
	return server, token, csrf, manager, session
}

// TestDirectSendSubmitFailureIsNotAccepted 复现审计缺陷的 HTTP 侧：Submit 失败
// 时直接发送必须以失败回应，账本条目绝不能落 accepted。
func TestDirectSendSubmitFailureIsNotAccepted(t *testing.T) {
	server, token, csrf, _, _ := submitFailureServer(t)
	send := featureRequest(server, token, csrf, http.MethodPost, "/api/sessions/sess-suberr/send",
		`{"text":"这条不该记为已发送","mode":"send","id":"2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if send.Code != http.StatusConflict {
		t.Fatalf("Submit 失败必须返回冲突，got=%d body=%s", send.Code, send.Body.String())
	}
	if !strings.Contains(send.Body.String(), "Agent 进程已退出") {
		t.Fatalf("错误应为传输不可用：%s", send.Body.String())
	}
	entry := server.Queue.get("sess-suberr", "2aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if entry == nil {
		t.Fatal("直接发送应先记录账本条目")
	}
	if entry.Status == SubAccepted {
		t.Fatalf("从未发出的消息被记为 accepted：%+v", entry)
	}
}

// TestQueueDispatchSubmitFailureStaysUnknown 复现审计缺陷的队列侧：Submit 失败
// 的派发结果必须是 unknown（不自动重试、不落 accepted）。
func TestQueueDispatchSubmitFailureStaysUnknown(t *testing.T) {
	server, _, _, manager, session := submitFailureServer(t)
	if _, _, err := server.Queue.enqueue("sess-suberr", "3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"排队消息", "queue", "", nil, nil, nil); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	entry, err := server.Queue.editableEntry("sess-suberr", "3aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatalf("editableEntry: %v", err)
	}
	outcome, message := server.dispatchSubmission("sess-suberr",
		acpResolution{kind: "kimi", manager: manager, session: session}, entry)
	if outcome != outcomeUnknown {
		t.Fatalf("Submit 失败的派发结果=%s（message=%q），必须为 unknown", outcome, message)
	}
	if entry.Status == SubAccepted {
		t.Fatalf("从未发出的消息被记为 accepted：%+v", entry)
	}
}
