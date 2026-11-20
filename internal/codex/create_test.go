package codex

import (
	"bufio"
	"database/sql"
	"errors"
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

func codexIntegrationExecutable(t *testing.T) string {
	t.Helper()
	if os.Getenv("GATEWAY_CODEX_INTEGRATION") != "1" {
		t.Skip("set GATEWAY_CODEX_INTEGRATION=1 to use the installed Codex runtime")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Skip(err)
	}
	return executable
}

func TestCreateConversationDirectRolloutResumesWithOfficialRuntime(t *testing.T) {
	executable := codexIntegrationExecutable(t)
	for _, test := range []struct{ name, config, provider string }{
		{name: "default permissions"},
		{name: "workspace write without approvals", config: "sandbox_mode = \"workspace-write\"\napproval_policy = \"never\"\n"},
		{name: "quoted custom provider", provider: "review_provider", config: `"model_provider" = "review_provider"
[model_providers.review_provider]
name = "review"
base_url = "http://127.0.0.1:9/v1"
wire_api = "responses"
requires_openai_auth = false
`},
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
			result := resumeCreatedConversation(t, executable, home, cwd, id)
			thread := result.MustGet("thread").(*jsoncompat.Object)
			if got, _ := thread.MustGet("id").(string); got != id {
				t.Fatalf("resumed id=%q, want %q", got, id)
			}
			resolvedCwd, _ := filepath.EvalSymlinks(cwd)
			if got, _ := thread.MustGet("cwd").(string); got != resolvedCwd {
				t.Fatalf("resumed cwd=%q, want %q", got, resolvedCwd)
			}
			for _, key := range []string{"approvalPolicy", "sandbox", "modelProvider"} {
				want, got := string(jsoncompat.EncodeCompact(official.MustGet(key))), string(jsoncompat.EncodeCompact(result.MustGet(key)))
				if got != want {
					t.Fatalf("%s=%s, want official %s", key, got, want)
				}
			}
			row, err := store.NewSessionStore(home).Get(id)
			if err != nil {
				t.Fatal(err)
			}
			provider := test.provider
			if provider == "" {
				provider = "openai"
			}
			if row.MustGet("name") != "直接恢复" || row.MustGet("model_provider") != provider || result.MustGet("modelProvider") != provider {
				t.Fatalf("metadata changed after resume: %v", row)
			}
		})
	}
}

func TestCreateConversationMultipleDatabasesFallsBackWithTitle(t *testing.T) {
	executable := codexIntegrationExecutable(t)
	home, cwd := t.TempDir(), t.TempDir()
	bridge := New(home, "")
	bridge.Executable = executable
	defer bridge.Close()
	if _, err := bridge.createConversationWithRuntime(cwd, "官方对照"); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("native databases=%v error=%v", paths, err)
	}
	current, err := sql.Open("sqlite", paths[0])
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	oldPath := filepath.Join(home, "state_0.sqlite")
	if _, err := current.Exec("VACUUM INTO ?", oldPath); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(oldPath, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := store.NewSessionStore(home).CreateThread(cwd, "不能写入旧库"); !errors.Is(err, store.ErrDirectCreateUnsupported) {
		t.Fatalf("error=%v, want ErrDirectCreateUnsupported", err)
	}
	rollouts, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*.jsonl"))
	if err != nil || len(rollouts) != 1 {
		t.Fatalf("unsupported direct creation wrote rollout: %v error=%v", rollouts, err)
	}
	id, err := bridge.CreateConversation(cwd, "多库保留标题")
	if err != nil {
		t.Fatal(err)
	}
	result := resumeCreatedConversation(t, executable, home, cwd, id)
	thread := result.MustGet("thread").(*jsoncompat.Object)
	if thread.MustGet("id") != id || thread.MustGet("name") != "多库保留标题" {
		t.Fatalf("fallback lost thread metadata: %s", jsoncompat.EncodeCompact(thread))
	}
	var name string
	if err := current.QueryRow("SELECT name FROM threads WHERE id=?", id).Scan(&name); err != nil || name != "多库保留标题" {
		t.Fatalf("native name=%q error=%v", name, err)
	}
	old, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var count int
	if err := old.QueryRow("SELECT COUNT(*) FROM threads WHERE id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("fallback modified old database: count=%d error=%v", count, err)
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
	for _, key := range []string{"approvalPolicy", "sandbox", "modelProvider"} {
		if value, exists := result.Get(key); !exists || value == nil {
			t.Fatalf("missing resumed %s: %s", key, jsoncompat.EncodeCompact(result))
		}
	}
	return result
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
