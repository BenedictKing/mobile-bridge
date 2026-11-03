package httpd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// capabilitiesAgent 是带可选命令广播的假 Agent：withCommands 在 session/load
// 应答后追加一条 available_commands_update 通知（真 Agent 的命令列表同样经
// session/update 通知到达）。
func capabilitiesAgent(t *testing.T, withCommands, builtinsOnly bool) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	commands := `[{"name":"deploy","description":"部署"}]`
	if builtinsOnly {
		commands = `[{"name":"compact","description":"内置"}]`
	}
	notice := ""
	if withCommands {
		notice = `  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"sess-cap","update":{"sessionUpdate":"available_commands_update","availableCommands":` + commands + `}}},ensure_ascii=False),flush=True)
`
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method=="initialize":
  result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method=="session/load":
  print(json.dumps({"jsonrpc":"2.0","id":ident,"result":{"sessionId":"sess-cap"}},ensure_ascii=False),flush=True)
` + notice + `  continue
 elif method=="session/new":
  result={"sessionId":"sess-cap"}
 else:
  result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result},ensure_ascii=False),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

func effectiveCapabilities(t *testing.T, server *Server, manager *acpruntime.Manager) []string {
	t.Helper()
	session, err := manager.Open("sess-cap", "/work", 3*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	view := server.acpViewState("kimi", manager, session)
	list, _ := view.MustGet("capabilities").(*jsoncompat.Array)
	if list == nil {
		t.Fatal("视图缺少 capabilities")
	}
	names := []string{}
	for _, item := range list.Items {
		text, _ := item.(string)
		names = append(names, text)
	}
	return names
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestACPViewCapabilitiesConvergeOnRuntimeEvidence 审计 §3 的收敛：profile
// 只是先验，steer 需要运行时方法证据（SteerMethod），skills 需要会话真实
// 广播过可选命令。「profile 声明支持但运行时拒绝」「旧会话不广告该能力」
// 两个降级形态都必须在 UI 层隐藏入口，而不是等发送失败。
func TestACPViewCapabilitiesConvergeOnRuntimeEvidence(t *testing.T) {
	// 降级一：声明 steer 但无 steering RPC、无命令广播 → 只剩网关合成的 fork。
	server, _, _ := newFeatureServer(t, t.TempDir())
	bare := acpruntime.NewCommand(capabilitiesAgent(t, false, false))
	bare.Capabilities = []string{"steer", "skills", "fork"}
	t.Cleanup(func() { bare.Close() })
	if got := effectiveCapabilities(t, server, bare); !equalStrings(got, []string{"fork"}) {
		t.Fatalf("无运行时证据应为 [fork]，got=%v", got)
	}

	// 正例：方法与命令证据齐备 → 三项全亮。
	equipped := acpruntime.NewCommand(capabilitiesAgent(t, true, false))
	equipped.Capabilities = []string{"steer", "skills", "fork"}
	equipped.SteerMethod = "_session/steering"
	t.Cleanup(func() { equipped.Close() })
	if got := effectiveCapabilities(t, server, equipped); !equalStrings(got, []string{"steer", "skills", "fork"}) {
		t.Fatalf("证据齐备应为全量，got=%v", got)
	}

	// 降级二：会话只广播内置命令（compact 等不可作为 Skill 激活）→ skills
	// 仍隐藏，选择器不会是空列表陷阱。
	builtinOnly := acpruntime.NewCommand(capabilitiesAgent(t, true, true))
	builtinOnly.Capabilities = []string{"steer", "skills", "fork"}
	builtinOnly.SteerMethod = "_session/steering"
	t.Cleanup(func() { builtinOnly.Close() })
	if got := effectiveCapabilities(t, server, builtinOnly); !equalStrings(got, []string{"steer", "fork"}) {
		t.Fatalf("仅内置命令应为 [steer fork]，got=%v", got)
	}
}

// TestACPViewCapabilitiesGoalsStillGateOnTransport 既有 goals 门控回归：
// 未声明 goals 的 Agent 不出现 goals（transport 为 nil 路径）。
func TestACPViewCapabilitiesGoalsStillGateOnTransport(t *testing.T) {
	server, _, _ := newFeatureServer(t, t.TempDir())
	manager := acpruntime.NewCommand(capabilitiesAgent(t, true, false))
	manager.Capabilities = []string{"skills", "fork"}
	t.Cleanup(func() { manager.Close() })
	if got := effectiveCapabilities(t, server, manager); !equalStrings(got, []string{"skills", "fork"}) {
		t.Fatalf("goals 未声明不得出现，got=%v", got)
	}
}
