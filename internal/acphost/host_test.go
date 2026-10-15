package acphost

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acp"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestHostKeepsAgentAcrossGatewayReconnect(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"hosted-fake"},"authMethods":[]}
 elif method == "session/load":
  print(json.dumps({"jsonrpc":"2.0","id":"reverse-1","method":"session/request_permission","params":{}}),flush=True)
  _=sys.stdin.readline()
  result={"sessionId":m.get("params",{}).get("sessionId"),"hosted":True}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(os.TempDir(), "mb-ah-"+strconv.Itoa(os.Getpid())+".sock")
	_ = os.Remove(socket)
	t.Cleanup(func() { _ = os.Remove(socket) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := New(socket, []string{python, "-u", path})
	hostErr := make(chan error, 1)
	go func() { hostErr <- host.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	var first *acp.Process
	for first == nil && time.Now().Before(deadline) {
		first, err = acp.StartSocket(socket)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if first == nil {
		t.Fatalf("connect host: %v", err)
	}
	if _, err := first.Initialize(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := acp.StartSocket(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Initialize(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	params := jsoncompat.NewObject()
	params.Set("sessionId", "sess-reconnected")
	result, err := second.Request("session/load", params, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.(*jsoncompat.Object).MustGet("hosted") != true {
		t.Fatalf("unexpected session/load result: %v", result)
	}
	cancel()
	select {
	case <-hostErr:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not stop")
	}
}
