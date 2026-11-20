package codex

import (
	"bufio"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	_ "modernc.org/sqlite"
)

func TestCreateConversationDirectRolloutResumesWithOfficialRuntime(t *testing.T) {
	if os.Getenv("GATEWAY_CODEX_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_CODEX_INTEGRATION=1 to use the installed Codex runtime")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip(err)
	}
	for _, test := range []struct{ name, config string }{
		{name: "default permissions"},
		{name: "workspace write without approvals", config: "sandbox_mode = \"workspace-write\"\napproval_policy = \"never\"\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, cwd := t.TempDir(), t.TempDir()
			if test.config != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			bridge := New(home, "")
			bridge.Executable = executable
			defer bridge.Close()
			officialID, err := bridge.createConversationWithRuntime(cwd, "官方对照")
			if err != nil {
				t.Fatal(err)
			}
			id, err := store.NewSessionStore(home).CreateThread(cwd, "直接恢复")
			if err != nil {
				t.Fatal(err)
			}
			official := resumeCreatedConversation(t, executable, home, cwd, officialID)
			thread := resumeCreatedConversation(t, executable, home, cwd, id)
			if got, _ := thread.MustGet("id").(string); got != id {
				t.Fatalf("resumed id=%q, want %q", got, id)
			}
			resolvedCwd, _ := filepath.EvalSymlinks(cwd)
			if got, _ := thread.MustGet("cwd").(string); got != resolvedCwd {
				t.Fatalf("resumed cwd=%q, want %q", got, resolvedCwd)
			}
			for _, key := range []string{"approvalPolicy", "sandbox"} {
				want, got := string(jsoncompat.EncodeCompact(official.MustGet(key))), string(jsoncompat.EncodeCompact(thread.MustGet(key)))
				if got != want {
					t.Fatalf("%s=%s, want official %s", key, got, want)
				}
			}
			row, err := store.NewSessionStore(home).Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if row.MustGet("name") != "直接恢复" || row.MustGet("model_provider") != "openai" {
				t.Fatalf("metadata changed after resume: %v", row)
			}
		})
	}
}

func resumeCreatedConversation(t *testing.T, executable, home, cwd, id string) *jsoncompat.Object {
	t.Helper()
	command := exec.Command(executable, "app-server", "--listen", "stdio://")
	command.Dir = cwd
	command.Env = append(os.Environ(), "CODEX_HOME="+home)
	command.Stderr = io.Discard
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	request := func(method string, params any) (*jsoncompat.Object, error) {
		return appRequestTimeout(stdin, scanner, method, params, 30*time.Second)
	}
	if _, err := request("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "mobile_bridge_integration", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		t.Fatal(err)
	}
	result, err := request("thread/resume", map[string]any{"threadId": id})
	if err != nil {
		t.Fatal(err)
	}
	thread, _ := result.MustGet("thread").(*jsoncompat.Object)
	if thread == nil {
		t.Fatalf("missing resumed thread: %s", jsoncompat.EncodeCompact(result))
	}
	return thread
}

func TestCreateConversationUsesLocalStoreBeforeRuntime(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE threads (
		id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		source TEXT NOT NULL, originator TEXT, model_provider TEXT NOT NULL,
		cwd TEXT NOT NULL, title TEXT NOT NULL, name TEXT,
		sandbox_policy TEXT NOT NULL, approval_mode TEXT NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	bridge := New(home, "")
	bridge.Executable = filepath.Join(t.TempDir(), "codex-must-not-start")
	defer bridge.Close()

	id, err := bridge.CreateConversation(cwd, "直接创建")
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.NewSessionStore(home).Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.MustGet("name") != "直接创建" || row.MustGet("cwd") != cwd || row.MustGet("model_provider") != "openai" {
		t.Fatalf("unexpected created row: %v", row)
	}
}
