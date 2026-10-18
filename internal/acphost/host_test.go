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

func TestHostRoutesSessionUpdatesToOwner(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 params=m.get("params") or {}
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/new":
  sid="native-"+params.get("cwd","x")
  print(json.dumps({"jsonrpc":"2.0","id":ident,"result":{"sessionId":sid}}),flush=True)
  print(json.dumps({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":sid,"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"chunk-"+sid}}}}),flush=True)
  continue
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(os.TempDir(), "mb-ah-"+strconv.Itoa(os.Getpid())+"-route.sock")
	_ = os.Remove(socket)
	t.Cleanup(func() { _ = os.Remove(socket) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	host := New(socket, []string{python, "-u", path})
	go func() { _ = host.Run(ctx) }()

	dial := func() *acp.Process {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			process, err := acp.StartSocket(socket)
			if err == nil {
				return process
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("connect host: timeout")
		return nil
	}
	connA, connB := dial(), dial()
	defer connA.Close()
	defer connB.Close()
	updatesA, updatesB := make(chan string, 4), make(chan string, 4)
	watch := func(message *jsoncompat.Object, sink chan string) {
		params, _ := message.MustGet("params").(*jsoncompat.Object)
		if params == nil {
			return
		}
		if sid, _ := params.MustGet("sessionId").(string); sid != "" {
			sink <- sid
		}
	}
	connA.OnNotification = func(message *jsoncompat.Object) { watch(message, updatesA) }
	connB.OnNotification = func(message *jsoncompat.Object) { watch(message, updatesB) }
	if _, err := connA.Initialize(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := connB.Initialize(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	create := func(process *acp.Process, cwd string) {
		params := jsoncompat.NewObject()
		params.Set("cwd", cwd)
		result, err := process.Request("session/new", params, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if sid, _ := result.(*jsoncompat.Object).MustGet("sessionId").(string); sid != "native-"+cwd {
			t.Fatalf("unexpected session/new result: %v", result)
		}
	}
	create(connA, "a")
	create(connB, "b")
	for sink, want := range map[chan string]string{updatesA: "native-a", updatesB: "native-b"} {
		select {
		case got := <-sink:
			if got != want {
				t.Fatalf("owner received %s, want %s", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("owner never received %s", want)
		}
	}
	// Neither connection may observe the sibling session's stream.
	for sink, name := range map[chan string]string{updatesA: "a", updatesB: "b"} {
		select {
		case stray := <-sink:
			t.Fatalf("connection %s received foreign update %s", name, stray)
		case <-time.After(300 * time.Millisecond):
		}
	}
}
