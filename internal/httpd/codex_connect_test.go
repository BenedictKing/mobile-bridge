//go:build !windows

package httpd

import (
	"database/sql"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/ipc"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"golang.org/x/net/websocket"
)

type ordinaryCLIFixture struct {
	env            *concurrencyHarness
	cwd            string
	resumes        atomic.Int32
	sends          atomic.Int32
	active         atomic.Bool
	loaded         atomic.Bool
	writerBusy     atomic.Bool
	background     atomic.Bool
	busyDescendant atomic.Bool
	unsubscribes   atomic.Int32
	unknown        bool
}

func newOrdinaryCLIFixture(t *testing.T, active, unknown bool) *ordinaryCLIFixture {
	t.Helper()
	env := newConcurrencyHarness(t)
	t.Cleanup(env.bridge.Close)
	f := &ordinaryCLIFixture{env: env, cwd: t.TempDir(), unknown: unknown}
	f.active.Store(active)
	env.server.Creations = mustLoadCreations(t, filepath.Join(t.TempDir(), "creations.json"))
	env.server.Queue = mustLoadSubmissions(t, filepath.Join(t.TempDir(), "submissions.json"))
	path := filepath.Join(env.bridge.Home, "sessions", "ordinary.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_complete\"}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(env.bridge.Home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, title TEXT, cwd TEXT, originator TEXT, source TEXT, rollout_path TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO threads VALUES (?, '普通聊天', ?, 'Codex Desktop', 'desktop', ?)`, env.threadID, f.cwd, path); err != nil {
		t.Fatal(err)
	}
	env.server.Store = store.NewSessionStore(env.bridge.Home)
	f.startRuntime(t)
	return f
}

func (f *ordinaryCLIFixture) startRuntime(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "mb-ordinary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f.env.bridge.CLIPath = filepath.Join(dir, "cli.sock")
	listener, err := net.Listen("unix", f.env.bridge.CLIPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: websocket.Server{
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
		Handler: func(connection *websocket.Conn) {
			defer connection.Close()
			for {
				var raw []byte
				if websocket.Message.Receive(connection, &raw) != nil {
					return
				}
				value, err := jsoncompat.Unmarshal(raw)
				if err != nil {
					return
				}
				message := value.(*jsoncompat.Object)
				if message.MustGet("id") == nil {
					continue
				}
				result := jsoncompat.NewObject()
				thread := jsoncompat.NewObject()
				thread.Set("id", f.env.threadID)
				thread.Set("cwd", f.cwd)
				thread.Set("modelProvider", "openai")
				status := jsoncompat.NewObject()
				status.Set("type", "notLoaded")
				if f.loaded.Load() {
					status.Set("type", "idle")
				}
				thread.Set("status", status)
				turn := jsoncompat.NewObject()
				turn.Set("id", "old-turn")
				turn.Set("status", "completed")
				if f.active.Load() {
					turn.Set("status", "inProgress")
				}
				item := jsoncompat.NewObject()
				item.Set("id", "old-answer")
				item.Set("type", "agentMessage")
				item.Set("text", "原对话的回答")
				turn.Set("items", &jsoncompat.Array{Items: []any{item}})
				thread.Set("turns", &jsoncompat.Array{Items: []any{turn}})
				switch message.MustGet("method") {
				case "initialize":
					result.Set("codexHome", f.env.bridge.Home)
				case "thread/read":
					result.Set("thread", thread)
				case "thread/resume":
					f.resumes.Add(1)
					if f.writerBusy.Load() {
						response := jsoncompat.NewObject()
						response.Set("id", message.MustGet("id"))
						errorMessage := jsoncompat.NewObject()
						errorMessage.Set("code", jsoncompat.Num(-32000))
						errorMessage.Set("message", "already has an active writer")
						response.Set("error", errorMessage)
						_ = websocket.Message.Send(connection, string(jsoncompat.EncodeCompact(response)))
						continue
					}
					f.loaded.Store(true)
					status.Set("type", "idle")
					result.Set("thread", thread)
					result.Set("model", "original-model")
					result.Set("reasoningEffort", "xhigh")
				case "thread/unsubscribe":
					f.unsubscribes.Add(1)
					f.loaded.Store(false)
					result.Set("status", "unsubscribed")
				case "thread/backgroundTerminals/list", "thread/list":
					rows := &jsoncompat.Array{}
					if message.MustGet("method") == "thread/backgroundTerminals/list" && f.background.Load() {
						rows.Items = append(rows.Items, jsoncompat.NewObject())
					}
					if message.MustGet("method") == "thread/list" && f.busyDescendant.Load() {
						child := jsoncompat.NewObject()
						child.Set("id", "child")
						childStatus := jsoncompat.NewObject()
						childStatus.Set("type", "active")
						child.Set("status", childStatus)
						rows.Items = append(rows.Items, child)
					}
					result.Set("data", rows)
				case "turn/start":
					f.sends.Add(1)
					if f.unknown {
						return
					}
					turn.Set("id", "new-turn")
					result.Set("turn", turn)
				}
				response := jsoncompat.NewObject()
				response.Set("id", message.MustGet("id"))
				response.Set("result", result)
				if websocket.Message.Send(connection, string(jsoncompat.EncodeCompact(response))) != nil {
					return
				}
			}
		},
	}}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
}

func TestOrdinaryCodexCLIExplicitHandoffAndRecovery(t *testing.T) {
	for _, reconnectFirst := range []bool{false, true} {
		name := "send"
		if reconnectFirst {
			name = "reconnect"
		}
		t.Run(name, func(t *testing.T) {
			f := newOrdinaryCLIFixture(t, false, false)
			env := f.env
			base := "/api/sessions/" + env.threadID
			read := env.request("GET", base+"/timeline", "")
			if read.Code != http.StatusOK || env.bridge.IsCLI(env.threadID) || f.resumes.Load() != 0 {
				t.Fatalf("history acquired an execution owner: %d %s", read.Code, read.Body.String())
			}
			handoff := env.request("POST", base+"/execution", `{"target":"cli"}`)
			if handoff.Code != http.StatusOK || f.sends.Load() != 0 {
				t.Fatalf("handoff failed or sent a message: %d %s", handoff.Code, handoff.Body.String())
			}
			if reconnectFirst {
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						response := env.request("POST", base+"/reconnect", `{"activate":true}`)
						if response.Code != http.StatusOK {
							t.Errorf("reconnect failed: %d %s", response.Code, response.Body.String())
						}
					})
				}
				wg.Wait()
			}
			body := `{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"继续原聊天"}`
			for range 2 {
				response := env.request("POST", base+"/send", body)
				if response.Code != http.StatusOK {
					t.Fatalf("send failed: %d %s", response.Code, response.Body.String())
				}
			}
			if f.sends.Load() != 1 || f.resumes.Load() != 1 {
				t.Fatalf("duplicated execution: sends=%d resumes=%d", f.sends.Load(), f.resumes.Load())
			}
			ledgerPath := env.server.Creations.path
			env.bridge.Close()
			next := codex.New(env.bridge.Home, "unused-desktop")
			next.CLIPath = env.bridge.CLIPath
			t.Cleanup(next.Close)
			env.server.Codex = next
			env.server.Creations = mustLoadCreations(t, ledgerPath)
			env.server.RegisterCodexCLIThreads()
			if f.resumes.Load() != 1 {
				t.Fatal("startup eagerly resumed a thread")
			}
			response := env.request("POST", base+"/reconnect", `{"activate":true}`)
			if response.Code != http.StatusOK || f.resumes.Load() != 2 || f.sends.Load() != 1 {
				t.Fatalf("recovery changed execution: %d %s", response.Code, response.Body.String())
			}
			page := env.request("GET", base+"/timeline", "")
			value, _ := jsoncompat.Unmarshal(page.Body.Bytes())
			meta := value.(*jsoncompat.Object).MustGet("meta").(*jsoncompat.Object)
			if meta.MustGet("executionOwner") != "cli" {
				t.Fatalf("missing CLI ownership: %s", page.Body.String())
			}
			if !strings.Contains(page.Body.String(), "原对话的回答") {
				t.Fatalf("takeover lost the original conversation: %s", page.Body.String())
			}
		})
	}
}

func TestOrdinaryCodexCLIRefusesUnfinishedHistory(t *testing.T) {
	f := newOrdinaryCLIFixture(t, true, false)
	base := "/api/sessions/" + f.env.threadID
	response := f.env.request("POST", base+"/execution", `{"target":"cli"}`)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "历史仍显示执行中") {
		t.Fatalf("unfinished history was taken over: %d %s", response.Code, response.Body.String())
	}
	response = f.env.request("POST", base+"/send", `{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"不要重复执行"}`)
	if response.Code != http.StatusConflict || f.resumes.Load() != 0 || f.sends.Load() != 0 {
		t.Fatalf("unfinished thread was resumed or sent: %d %s", response.Code, response.Body.String())
	}
	entry := f.env.server.Queue.get(f.env.threadID, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if entry.Status != SubFailed {
		t.Fatalf("a refused, never-dispatched message must not be marked unknown: %+v", entry)
	}
	// Once the original runtime has finished, retrying the same phone draft
	// must send it once, rather than return a failed duplicate as success.
	f.active.Store(false)
	response = f.env.request("POST", base+"/execution", `{"target":"cli"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("handoff after settlement failed: %s", response.Body.String())
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			response := f.env.request("POST", base+"/send", `{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"不要重复执行"}`)
			if response.Code != http.StatusOK {
				t.Errorf("retry failed: %d %s", response.Code, response.Body.String())
			}
		})
	}
	wg.Wait()
	if f.sends.Load() != 1 || f.resumes.Load() != 1 {
		t.Fatalf("failed-draft retry changed execution: sends=%d resumes=%d", f.sends.Load(), f.resumes.Load())
	}
}

func TestOrdinaryCodexCLIUnknownSendIsNotReplayed(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, true)
	base := "/api/sessions/" + f.env.threadID
	if response := f.env.request("POST", base+"/execution", `{"target":"cli"}`); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	body := `{"id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","text":"发送一次"}`
	first := f.env.request("POST", base+"/send", body)
	if first.Code != http.StatusConflict || f.sends.Load() != 1 {
		t.Fatalf("expected an uncertain send: %d %s", first.Code, first.Body.String())
	}
	f.env.bridge.Close()
	next := codex.New(f.env.bridge.Home, "unused-desktop")
	next.CLIPath = f.env.bridge.CLIPath
	t.Cleanup(next.Close)
	f.env.server.Codex = next
	f.env.server.Creations = mustLoadCreations(t, f.env.server.Creations.path)
	f.env.server.Queue = mustLoadSubmissions(t, f.env.server.Queue.path)
	f.env.server.RegisterCodexCLIThreads()
	second := f.env.request("POST", base+"/send", body)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "unknown") || f.sends.Load() != 1 {
		t.Fatalf("uncertain send was replayed: %d %s", second.Code, second.Body.String())
	}
}

func TestOrdinaryCodexCLIDoesNotTakeOverBrokenDesktopHandshake(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	path := filepath.Join(filepath.Dir(f.env.bridge.CLIPath), "desktop.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	f.env.bridge.IPC.Path = path
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		value, err := ipc.DecodeFrame(connection)
		if err != nil {
			return
		}
		response := jsoncompat.NewObject()
		response.Set("type", "response")
		response.Set("requestId", value.(*jsoncompat.Object).MustGet("requestId"))
		response.Set("resultType", "success")
		response.Set("result", jsoncompat.NewObject())
		frame, _ := ipc.EncodeFrame(response)
		_, _ = connection.Write(frame)
	}()
	response := f.env.request("POST", "/api/sessions/"+f.env.threadID+"/reconnect", `{"activate":true}`)
	if response.Code != http.StatusConflict || f.env.bridge.IsCLI(f.env.threadID) || f.resumes.Load() != 0 {
		t.Fatalf("desktop protocol failure incorrectly selected CLI: %d %s", response.Code, response.Body.String())
	}
}

func TestOrdinaryCodexCLILedgerFailureDoesNotAttach(t *testing.T) {
	f := newOrdinaryCLIFixture(t, false, false)
	f.env.server.Creations.path = filepath.Join(t.TempDir(), "missing", "creations.json")
	response := f.env.request("POST", "/api/sessions/"+f.env.threadID+"/execution", `{"target":"cli"}`)
	if response.Code != http.StatusConflict || f.env.bridge.IsCLI(f.env.threadID) || f.resumes.Load() != 1 || f.unsubscribes.Load() != 1 {
		t.Fatalf("failed persistence left an unrecorded owner: %d %s", response.Code, response.Body.String())
	}
}
