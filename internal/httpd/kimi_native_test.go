package httpd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/kimi"
)

func TestACPViewStateRefreshesKimiNativeNotifications(t *testing.T) {
	home := t.TempDir()
	wire := filepath.Join(home, "sessions", "workspace", "sess-1", "agents", "main", "wire.jsonl")
	if err := os.MkdirAll(filepath.Dir(wire), 0o700); err != nil {
		t.Fatal(err)
	}
	initial := `{"type":"turn.prompt","input":"原始问题","origin":{"kind":"user"}}
{"type":"turn.ended","reason":"completed"}
`
	if err := os.WriteFile(wire, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	reader := kimi.NewTranscriptReader(home)
	manager := acpruntime.NewCommand(nil)
	manager.Kind = "kimi"
	manager.Native = reader.Load
	manager.NativeTranscript = reader.Load
	manager.NativeReset = reader.Reset
	manager.NativeInterval = time.Hour
	defer manager.Close()
	session, err := manager.OpenHistory("sess-1", "/w")
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{}
	appendWire := func(content string) {
		t.Helper()
		file, err := os.OpenFile(wire, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if _, err := file.WriteString(content); err != nil {
			t.Fatal(err)
		}
	}
	appendWire(`{"type":"turn.prompt","input":"<notification>后台任务继续</notification>","origin":{"kind":"task"}}
{"type":"context.append_loop_event","event":{"type":"content.part","part":{"type":"text","text":"正在执行后台通知"}}}
`)
	view := server.acpViewState("kimi", manager, session)
	if view.MustGet("status") != "active" || !strings.Contains(string(jsoncompat.EncodeCompact(view)), "正在执行后台通知") {
		t.Fatalf("native notification did not reach the HTTP view: %s", jsoncompat.EncodeCompact(view))
	}
	appendWire(`{"type":"turn.ended","reason":"completed"}
`)
	view = server.acpViewState("kimi", manager, session)
	if view.MustGet("status") != "idle" {
		t.Fatalf("settled native notification kept the HTTP view busy: %s", jsoncompat.EncodeCompact(view))
	}
}
