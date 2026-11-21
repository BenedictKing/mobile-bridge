//go:build !windows

package codex

import (
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"golang.org/x/net/websocket"
)

type cliFixture struct {
	mu          sync.Mutex
	calls       []*jsoncompat.Object
	connections []*websocket.Conn
	thread      *jsoncompat.Object
	home        string
	onCall      func(*jsoncompat.Object) *jsoncompat.Object
}

func newCLIFixture(t *testing.T) (*Bridge, *cliFixture) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mbcli-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cli.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	b := New(t.TempDir(), "unused-desktop")
	b.CLIPath = path
	cwd := t.TempDir()
	f := &cliFixture{home: b.Home, thread: object(map[string]any{
		"id": "cli-thread", "cwd": cwd, "name": "迁移会话", "modelProvider": "ccx",
		"status": object(map[string]any{"type": "idle"}), "turns": &jsoncompat.Array{},
	})}
	server := &http.Server{Handler: websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(connection *websocket.Conn) {
			f.mu.Lock()
			f.connections = append(f.connections, connection)
			f.mu.Unlock()
			defer connection.Close()
			for {
				var data []byte
				if websocket.Message.Receive(connection, &data) != nil {
					return
				}
				value, err := jsoncompat.Unmarshal(data)
				if err != nil {
					return
				}
				message := value.(*jsoncompat.Object)
				if message.MustGet("id") == nil {
					continue
				}
				f.mu.Lock()
				f.calls = append(f.calls, message)
				onCall := f.onCall
				f.mu.Unlock()
				if message.MustGet("method") == nil {
					continue
				}
				if onCall != nil {
					if response := onCall(message); response != nil {
						f.send(connection, response)
						continue
					}
				}
				result := jsoncompat.NewObject()
				switch message.MustGet("method") {
				case "initialize":
					result.Set("codexHome", f.home)
				case "thread/read":
					result.Set("thread", f.thread)
				case "thread/resume":
					result.Set("thread", f.thread)
					result.Set("model", "gpt-test")
					result.Set("reasoningEffort", "high")
				case "thread/list", "thread/backgroundTerminals/list":
					result.Set("data", &jsoncompat.Array{})
				case "thread/unsubscribe":
					result.Set("status", "unsubscribed")
				case "turn/start":
					turn := object(map[string]any{"id": "cli-turn", "status": "inProgress", "items": &jsoncompat.Array{}})
					item := object(map[string]any{"id": "reply", "type": "agentMessage", "text": ""})
					f.send(connection, object(map[string]any{"method": "turn/started", "params": object(map[string]any{"threadId": "cli-thread", "turn": turn})}))
					f.send(connection, object(map[string]any{"method": "item/started", "params": object(map[string]any{"threadId": "cli-thread", "turnId": "cli-turn", "item": item})}))
					f.send(connection, object(map[string]any{"method": "item/agentMessage/delta", "params": object(map[string]any{"threadId": "cli-thread", "turnId": "cli-turn", "itemId": "reply", "delta": "实时回复"})}))
					completed := object(map[string]any{"id": "cli-turn", "status": "completed", "items": &jsoncompat.Array{}})
					f.send(connection, object(map[string]any{"method": "turn/completed", "params": object(map[string]any{"threadId": "cli-thread", "turn": completed})}))
					result.Set("turn", turn) // The stale RPC reply arrives after completion.
				}
				f.send(connection, object(map[string]any{"id": message.MustGet("id"), "result": result}))
			}
		},
	}}
	go server.Serve(listener)
	b.RegisterCLI("cli-thread", cwd)
	t.Cleanup(func() { b.Close(); server.Close(); listener.Close(); f.disconnect(); os.RemoveAll(dir) })
	return b, f
}

func (f *cliFixture) send(connection *websocket.Conn, message *jsoncompat.Object) {
	_ = websocket.Message.Send(connection, string(jsoncompat.EncodeCompact(message)))
}

func (f *cliFixture) disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, connection := range f.connections {
		connection.Close()
	}
}

func (f *cliFixture) operations(method string) []*jsoncompat.Object {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := []*jsoncompat.Object{}
	for _, call := range f.calls {
		if call.MustGet("method") == method {
			result = append(result, call)
		}
	}
	return result
}

func TestCLIMigrationSettingsStreamingAndReconnect(t *testing.T) {
	b, f := newCLIFixture(t)
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpdateSettings("cli-thread", "chosen-model", "xhigh", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.StartTurnWithOptions("cli-thread", "same-submission", "迁移正文", "send", "plan", nil, nil); err != nil {
		t.Fatal(err)
	}
	start := f.operations("turn/start")[0].MustGet("params").(*jsoncompat.Object)
	if start.MustGet("clientUserMessageId") != "same-submission" || start.MustGet("threadId") != "cli-thread" {
		t.Fatal("submission identity changed")
	}
	collaboration := start.MustGet("collaborationMode").(*jsoncompat.Object)
	if collaboration.MustGet("mode") != "plan" || collaboration.MustGet("settings").(*jsoncompat.Object).MustGet("model") != "chosen-model" {
		t.Fatal("chosen settings were lost")
	}
	state, _, connected, _ := b.Snapshot("cli-thread")
	view := model.NormalizeState(state, connected)
	if !connected || view.MustGet("status") != "idle" || !strings.Contains(string(jsoncompat.EncodeCompact(view)), "实时回复") {
		t.Fatalf("stream did not settle: %s", jsoncompat.EncodeCompact(view))
	}
	f.disconnect()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, _, ready, _ := b.Snapshot("cli-thread")
		if !ready {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.operations("thread/resume")) != 2 || len(f.operations("turn/start")) != 1 {
		t.Fatal("reconnect must resume the same runtime without replaying a turn")
	}
}

func TestCLIRejectsMismatchedCwdBeforeResume(t *testing.T) {
	b, f := newCLIFixture(t)
	b.RegisterCLI("cli-thread", t.TempDir())
	if _, err := b.WatchForWrite("cli-thread", time.Second); err == nil {
		t.Fatal("mismatched cwd accepted")
	}
	if len(f.operations("thread/resume")) != 0 {
		t.Fatal("identity check must precede resume")
	}
}

func TestCLIApprovalUsesTheOriginalReverseRequest(t *testing.T) {
	b, fixture := newCLIFixture(t)
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	b.cli.onRequest(object(map[string]any{
		"id": jsoncompat.Num(99), "method": "item/commandExecution/requestApproval",
		"params": object(map[string]any{"threadId": "cli-thread", "command": "go test ./..."}),
	}))
	state, _, _, _ := b.Snapshot("cli-thread")
	if len(model.PendingRequests(state).Items) != 1 {
		t.Fatal("approval is not visible")
	}
	if _, err := b.Respond("cli-thread", "99", object(map[string]any{"decision": "accept"})); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Respond("cli-thread", "99", object(map[string]any{"decision": "accept"})); err == nil {
		t.Fatal("approval reply must not be repeated")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fixture.mu.Lock()
		found := false
		for _, call := range fixture.calls {
			if call.MustGet("id") == jsoncompat.Num(99) && call.MustGet("result") != nil {
				found = true
			}
		}
		fixture.mu.Unlock()
		if found {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("reply did not preserve the native reverse request id")
}

func TestCLISharedOfficialRuntime(t *testing.T) {
	if os.Getenv("GATEWAY_CODEX_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_CODEX_INTEGRATION=1 to verify the installed official runtime")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "mbcli-official-")
	if err != nil {
		t.Fatal(err)
	}
	b := New(t.TempDir(), "unused-desktop")
	b.Executable = executable
	b.CLIPath = filepath.Join(dir, "cli.sock")
	t.Cleanup(func() {
		b.Close()
		if raw, err := os.ReadFile(b.CLIPath + ".pid"); err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					process.Kill()
				}
			}
		}
		os.RemoveAll(dir)
	})
	cwd := t.TempDir()
	b.RegisterCLI("placeholder", cwd)
	if err := b.cli.connect(); err != nil {
		t.Fatal(err)
	}
	created, err := b.cli.request("thread/start", object(map[string]any{"cwd": cwd, "ephemeral": false}), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = created // Initialize the real index schema, then use production's empty rollout writer.
	id, err := b.CreateConversation(cwd, "CLI migration fixture")
	if err != nil {
		t.Fatal(err)
	}
	b.RegisterCLI(id, cwd)
	if _, err := b.WatchForWrite(id, time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.UpdateSettings(id, "gpt-6.1-sol", "xhigh", nil); err != nil {
		t.Fatal(err)
	}
	// Only the gateway connection closes. A new bridge reconnects to the same
	// official runtime and observes the thread's sticky model settings.
	b.Close()
	next := New(b.Home, "unused-desktop")
	next.Executable, next.CLIPath = executable, b.CLIPath
	next.RegisterCLI(id, cwd)
	t.Cleanup(next.Close)
	if _, err := next.WatchForWrite(id, time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, connected, _ := next.Snapshot(id)
	if !connected || state.MustGet("latestModel") != "gpt-6.1-sol" || state.MustGet("latestReasoningEffort") != "xhigh" {
		t.Fatalf("official runtime did not preserve settings: %s", jsoncompat.EncodeCompact(state))
	}
}
