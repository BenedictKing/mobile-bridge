package acpruntime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// deadTransport 启动一个立即退出的进程并等到它的读循环落定（closed=true），
// 得到一个 Submit 必失败、但从未触发本会话 OnClose 的传输句柄。
func deadTransport(t *testing.T) *acp.Process {
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

// TestPromptSubmitFailurePropagates 复现审计缺陷：session/prompt Submit 失败
// （进程已死但会话句柄未标记 closed）时，Prompt 必须把错误返回给调用方，
// 否则 HTTP 与队列会把从未发出的消息记为 accepted。
func TestPromptSubmitFailurePropagates(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new": result={"sessionId":"sess-created"}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	defer manager.Close()
	session, err := manager.Create("request-id", "/work", "新建聊天", 3*time.Second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// 换入已死亡的传输：会话层 closed 仍为 false（OnClose 从未对本会话触发），
	// 前置检查放行，错误只能在 Submit 处暴露——与线上进程竞态退出同形态。
	live := session.Process
	session.Process = deadTransport(t)
	defer live.Close()

	promptErr := session.Prompt("你好", time.Second)
	if promptErr == nil {
		t.Fatal("Submit 失败时 Prompt 必须返回错误，而不是记为已发送")
	}
	if !strings.Contains(promptErr.Error(), "Agent 进程已退出") {
		t.Fatalf("错误应为传输不可用：%v", promptErr)
	}
	// 本地回合已失败落账，且运行位复位（后续消息不会被「任务正在运行」卡死）。
	snapshot := session.Snapshot()
	turns, _ := snapshot.MustGet("turns").(*jsoncompat.Array)
	if turns == nil || len(turns.Items) == 0 {
		t.Fatal("失败回合应留在视图里")
	}
	turn, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
	if status, _ := turn.MustGet("status").(string); status != "failed" {
		t.Fatalf("回合状态=%s，应为 failed", status)
	}
	if session.promptRunning {
		t.Fatal("Submit 失败后 promptRunning 必须复位")
	}
	if done := session.promptDone; done != nil {
		select {
		case <-done:
		default:
			t.Fatal("Submit 失败后 promptDone 必须关闭")
		}
	}
}
