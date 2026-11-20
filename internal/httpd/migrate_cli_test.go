//go:build !windows

package httpd

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"golang.org/x/net/websocket"
)

func TestMigrationCLIFallbackAndDurableRecovery(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "accepted"
		if unknown {
			name = "unknown-send"
		}
		t.Run(name, func(t *testing.T) {
			env := newConcurrencyHarness(t)
			t.Cleanup(env.bridge.Close)
			cwd := t.TempDir()
			argv, _, _ := migrationFakeAgent(t, `[{"sessionId":"sess-old","title":"迁移源会话","cwd":`+jsonText(cwd)+`,"updatedAt":1}]`, "kimi")
			manager := acpruntime.NewCommand(argv)
			t.Cleanup(manager.Close)
			env.server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
			ledgerPath := filepath.Join(t.TempDir(), "creations.json")
			env.server.Creations = mustLoadCreations(t, ledgerPath)
			targetID := "87bfb3ac-7a8a-4c11-a444-a6d24e51f211"
			env.bridge.Executable = codexMigrationAppServer(t, targetID)
			dir, err := os.MkdirTemp("", "mb-migrate-cli-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			env.bridge.CLIPath = filepath.Join(dir, "cli.sock")
			listener, err := net.Listen("unix", env.bridge.CLIPath)
			if err != nil {
				t.Fatal(err)
			}
			var sends atomic.Int32
			var settings atomic.Int32
			var resumes atomic.Int32
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
						params, _ := message.MustGet("params").(*jsoncompat.Object)
						result := jsoncompat.NewObject()
						thread := jsoncompat.NewObject()
						thread.Set("id", targetID)
						thread.Set("cwd", cwd)
						thread.Set("modelProvider", "openai")
						thread.Set("turns", &jsoncompat.Array{})
						switch message.MustGet("method") {
						case "initialize":
							result.Set("codexHome", env.bridge.Home)
						case "thread/read":
							result.Set("thread", thread)
						case "thread/resume":
							resumes.Add(1)
							result.Set("thread", thread)
							result.Set("model", "chosen-model")
						case "thread/settings/update":
							if params.MustGet("model") == "chosen-model" && params.MustGet("effort") == "xhigh" {
								settings.Add(1)
							}
						case "turn/start":
							sends.Add(1)
							if params.MustGet("clientUserMessageId") == nil {
								t.Error("missing stable submission id")
							}
							if unknown {
								return
							}
							turn := jsoncompat.NewObject()
							turn.Set("id", "turn-migrated")
							turn.Set("status", "completed")
							turn.Set("items", &jsoncompat.Array{})
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
			body := `{"id":"e168152b-7b6f-4ed0-ae4f-fd24f7241f8b","agent":"codex","scope":"main","model":"chosen-model","effort":"xhigh"}`
			first := env.request("POST", "/api/sessions/sess-old/migrate", body)
			want := http.StatusOK
			if unknown {
				want = http.StatusConflict
			}
			if first.Code != want {
				t.Fatalf("migration failed: %d %s", first.Code, first.Body.String())
			}
			if !unknown {
				result, _ := jsoncompat.Unmarshal(first.Body.Bytes())
				if result.(*jsoncompat.Object).MustGet("sent") != true {
					t.Fatal("CLI migration must send immediately")
				}
			}
			entry := env.server.Creations.lookup("e168152b-7b6f-4ed0-ae4f-fd24f7241f8b")
			if entry.ID != targetID || entry.Execution != "cli" || entry.Sent == unknown {
				t.Fatalf("wrong persisted execution: %+v", entry)
			}
			second := env.request("POST", "/api/sessions/sess-old/migrate", body)
			if second.Code != want || sends.Load() != 1 || settings.Load() != 1 {
				t.Fatalf("retry duplicated migration: status=%d sends=%d settings=%d", second.Code, sends.Load(), settings.Load())
			}
			env.bridge.Close()
			next := codex.New(env.bridge.Home, "unused-desktop")
			next.CLIPath = env.bridge.CLIPath
			t.Cleanup(next.Close)
			env.server.Codex = next
			env.server.Creations = mustLoadCreations(t, ledgerPath)
			env.server.RegisterCodexCLIThreads()
			reconnected := env.request("POST", "/api/sessions/"+targetID+"/reconnect", `{"activate":true}`)
			if reconnected.Code != http.StatusOK || sends.Load() != 1 || resumes.Load() != 2 {
				t.Fatalf("restart did not resume the same runtime: %d sends=%d resumes=%d %s", reconnected.Code, sends.Load(), resumes.Load(), reconnected.Body.String())
			}
		})
	}
}
