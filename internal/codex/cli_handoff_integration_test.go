//go:build !windows

package codex

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func isolatedOfficialCLI(t *testing.T, home, cwd string) *Bridge {
	t.Helper()
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "mbcli-handoff-")
	if err != nil {
		t.Fatal(err)
	}
	b := New(home, "unused-desktop")
	b.Executable, b.CLIPath = executable, filepath.Join(dir, "cli.sock")
	b.RegisterCLI("placeholder", cwd)
	t.Cleanup(func() {
		b.Close()
		if raw, err := os.ReadFile(b.CLIPath + ".pid"); err == nil {
			if pid, err := strconv.Atoi(string(raw)); err == nil {
				if process, err := os.FindProcess(pid); err == nil {
					_ = process.Kill()
				}
			}
		}
		_ = os.RemoveAll(dir)
	})
	if err := b.cli.connect(); err != nil {
		t.Fatal(err)
	}
	return b
}

// Two independent official processes share only an isolated Codex home. The
// second process must fail before release and acquire the same writer after it;
// this verifies the OS lock handoff rather than only the projected idle status.
func TestCLIWriterHandoffOfficialRuntime(t *testing.T) {
	if os.Getenv("GATEWAY_CODEX_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_CODEX_INTEGRATION=1 to verify official writer handoff")
	}
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("thread_unload_delay_secs = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := isolatedOfficialCLI(t, home, cwd)
	// Initialize the native index without sending a model request.
	if _, err := first.cli.request("thread/start", object(map[string]any{"cwd": cwd, "ephemeral": true}), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	id, err := first.CreateConversation(cwd, "writer handoff fixture")
	if err != nil {
		t.Fatal(err)
	}
	first.RegisterCLI(id, cwd)
	if _, err := first.WatchForWrite(id, time.Second); err != nil {
		t.Fatal(err)
	}
	second := isolatedOfficialCLI(t, home, cwd)
	second.RegisterCLI(id, cwd)
	if _, err := second.WatchForWrite(id, time.Second); err == nil || !strings.Contains(err.Error(), "另一个 Codex 应用占用") {
		t.Fatalf("second process acquired an active writer: %v", err)
	}
	first.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
	_, _, connected, connectionError := first.Snapshot(id)
	if connected || connectionError != "" {
		t.Fatalf("official unsubscribe failed: connected=%v error=%s", connected, connectionError)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		read, err := first.cli.request("thread/read", object(map[string]any{"threadId": id, "includeTurns": false}), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		thread, _ := read.MustGet("thread").(*jsoncompat.Object)
		if thread != nil && cliStatus(thread.MustGet("status")) == "notLoaded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("official runtime did not unload the unsubscribed thread")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := second.WatchForWrite(id, time.Second); err != nil {
		t.Fatalf("second process could not acquire the released writer: %v", err)
	}
	if _, err := first.Watch(id, time.Second); err != nil {
		t.Fatalf("history read attempted to retake the second process's writer: %v", err)
	}
	_, _, connected, _ = first.Snapshot(id)
	if connected {
		t.Fatal("history read became a second execution owner")
	}
}

func TestExplicitHandoffOfficialRuntime(t *testing.T) {
	if os.Getenv("GATEWAY_CODEX_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_CODEX_INTEGRATION=1 to verify explicit handoff")
	}
	home, cwd := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("thread_unload_delay_secs = 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first := isolatedOfficialCLI(t, home, cwd)
	if _, err := first.cli.request("thread/start", object(map[string]any{"cwd": cwd, "ephemeral": true}), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	id, err := first.CreateConversation(cwd, "explicit handoff fixture")
	if err != nil {
		t.Fatal(err)
	}
	first.RegisterCLI(id, cwd)
	if _, err := first.WatchForWrite(id, time.Second); err != nil {
		t.Fatal(err)
	}
	second := isolatedOfficialCLI(t, home, cwd)
	persisted := false
	if err := second.ClaimCLI(id, cwd, func() error { persisted = true; return nil }); err == nil || !strings.Contains(err.Error(), "另一个 Codex 应用占用") || persisted || second.IsCLI(id) {
		t.Fatalf("conflicting handoff changed routing: %v persisted=%v", err, persisted)
	}
	if err := first.HandOffCLI(id, cwd, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if first.IsCLI(id) || first.ExecutionOwner(id) != "" {
		t.Fatal("handoff retained first runtime ownership")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = second.ClaimCLI(id, cwd, func() error { persisted = true; return nil })
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("released thread could not be claimed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	view, _, connected, _ := second.Snapshot(id)
	if !persisted || !connected || view.MustGet("id") != id || !cliPathEqual(cliText(view.MustGet("cwd")), cwd) {
		t.Fatal("handoff lost the original conversation identity")
	}
}
