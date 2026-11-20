package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

const createThreadTestSchema = `CREATE TABLE threads (
	id TEXT PRIMARY KEY,
	rollout_path TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	recency_at INTEGER NOT NULL DEFAULT 0,
	created_at_ms INTEGER,
	updated_at_ms INTEGER,
	recency_at_ms INTEGER NOT NULL DEFAULT 0,
	source TEXT NOT NULL,
	originator TEXT,
	history_mode TEXT NOT NULL DEFAULT 'legacy',
	thread_source TEXT,
	model_provider TEXT NOT NULL,
	cwd TEXT NOT NULL,
	cli_version TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL,
	name TEXT,
	preview TEXT NOT NULL DEFAULT '',
	sandbox_policy TEXT NOT NULL,
	approval_mode TEXT NOT NULL,
	tokens_used INTEGER NOT NULL DEFAULT 0,
	has_user_event INTEGER NOT NULL DEFAULT 0,
	first_user_message TEXT NOT NULL DEFAULT '',
	archived INTEGER NOT NULL DEFAULT 0,
	is_pinned INTEGER NOT NULL DEFAULT 0,
	memory_mode TEXT NOT NULL DEFAULT 'enabled'
)`

func createThreadTestDB(t *testing.T, home, schema string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}

func TestCreateThreadWritesImmediatelyReadableConversation(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	createThreadTestDB(t, home, createThreadTestSchema).Close()
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("\"model_provider\" = \"bridge_api\" # 当前接入\n'sandbox_mode' = \"workspace-write\"\n\"approval_policy\" = \"never\""), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewSessionStore(home)

	id, err := store.CreateThread(cwd, "快速创建")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("invalid thread id %q: %v", id, err)
	}
	row, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.MustGet("name"); got != "快速创建" || row.MustGet("title") != "快速创建" {
		t.Fatalf("title was not persisted: %v", row)
	}
	if row.MustGet("cwd") != cwd || row.MustGet("originator") != "mobile_bridge_go" || row.MustGet("model_provider") != "bridge_api" ||
		row.MustGet("sandbox_policy") != "workspace-write" || row.MustGet("approval_mode") != "never" {
		t.Fatalf("unexpected metadata: %v", row)
	}
	path, _ := row.MustGet("rollout_path").(string)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(strings.TrimSpace(string(raw)), "\n") != 0 {
		t.Fatalf("empty thread must contain exactly one rollout line: %q", raw)
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	record := value.(*jsoncompat.Object)
	payload := record.MustGet("payload").(*jsoncompat.Object)
	if record.MustGet("type") != "session_meta" || payload.MustGet("id") != id || payload.MustGet("session_id") != id ||
		payload.MustGet("cwd") != cwd || payload.MustGet("source") != "vscode" || payload.MustGet("history_mode") != "legacy" {
		t.Fatalf("unexpected rollout: %s", raw)
	}
	if provider, exists := payload.Get("model_provider"); !exists || provider != "bridge_api" {
		t.Fatalf("provider must match the active Codex config: %#v", provider)
	}
	history, err := store.HistoryFor(id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if history.MustGet("title") != "快速创建" || history.MustGet("cwd") != cwd {
		t.Fatalf("created history is not readable: %s", jsoncompat.EncodeCompact(history))
	}
}

func TestReadDirectCreateSettings(t *testing.T) {
	tests := []struct {
		name, config, wantProvider, wantSandbox, wantApproval string
		wantError                                             bool
	}{
		{name: "default", wantProvider: "openai"},
		{name: "double quoted", config: `model_provider = "bridge_api" # active`, wantProvider: "bridge_api"},
		{name: "single quoted", config: `model_provider = 'custom_provider'`, wantProvider: "custom_provider"},
		{name: "double quoted key", config: `"model_provider" = "bridge_api"`, wantProvider: "bridge_api"},
		{name: "single quoted key", config: `'model_provider' = 'bridge_api'`, wantProvider: "bridge_api"},
		{name: "escaped key", config: `"model\u005fprovider" = "bridge_api"`, wantProvider: "bridge_api"},
		{name: "multiline provider", config: "model_provider = \"\"\"bridge_api\"\"\"", wantProvider: "bridge_api"},
		{name: "profile section does not override root", config: "model_provider = \"root\"\n[profiles.work]\nmodel_provider = \"other\"", wantProvider: "root"},
		{name: "simple permissions", config: "sandbox_mode = \"workspace-write\"\napproval_policy = \"never\"", wantProvider: "openai", wantSandbox: "workspace-write", wantApproval: "never"},
		{name: "active profile falls back", config: "profile = \"work\"\nmodel_provider = \"custom\"", wantError: true},
		{name: "quoted active profile falls back", config: `"profile" = "work"`, wantError: true},
		{name: "unquoted provider falls back", config: "model_provider = custom", wantError: true},
		{name: "empty provider falls back", config: `model_provider = ""`, wantError: true},
		{name: "complex permission key falls back", config: `default_permissions = {}`, wantError: true},
		{name: "complex permission table falls back", config: "[sandbox_workspace_write]\nnetwork_access = true", wantError: true},
		{name: "history settings fall back", config: "[history]\npersistence = \"none\"", wantError: true},
		{name: "memory settings fall back", config: "[memories] # custom memory behavior\ngenerate_memories = false", wantError: true},
		{name: "quoted memory table falls back", config: "[\"memories\"] # custom\ngenerate_memories = false", wantError: true},
		{name: "quoted dotted history falls back", config: `'history'.persistence = "none"`, wantError: true},
		{name: "unknown sandbox falls back", config: `sandbox_mode = "unknown"`, wantError: true},
		{name: "unknown approval falls back", config: `approval_policy = "unknown"`, wantError: true},
		{name: "duplicate quoted key falls back", config: "model_provider = \"first\"\n\"model_provider\" = \"second\"", wantError: true},
		{name: "malformed unrelated setting falls back", config: "model_provider = \"bridge_api\"\nmodel = [", wantError: true},
		{name: "non string provider falls back", config: `model_provider = 1`, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.config != "" {
				if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			settings, err := readDirectCreateSettings(home, home)
			want := directCreateSettings{provider: test.wantProvider, sandboxPolicy: test.wantSandbox, approvalMode: test.wantApproval}
			if !test.wantError {
				if want.sandboxPolicy == "" {
					want.sandboxPolicy = "read-only"
				}
				if want.approvalMode == "" {
					want.approvalMode = "on-request"
				}
			}
			if (err != nil) != test.wantError || settings != want {
				t.Fatalf("settings=%+v error=%v, want %+v error=%v", settings, err, want, test.wantError)
			}
		})
	}
}

func TestCreateThreadRejectsMultipleDatabasesBeforeWriting(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	current := createThreadTestDB(t, home, createThreadTestSchema)
	defer current.Close()
	oldPath := filepath.Join(home, "state_0.sqlite")
	old, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err := old.Exec(createThreadTestSchema); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(oldPath, future, future); err != nil {
		t.Fatal(err)
	}
	_, err = NewSessionStore(home).CreateThread(cwd, "不能写入旧库")
	if !errors.Is(err, ErrDirectCreateUnsupported) {
		t.Fatalf("error=%v, want ErrDirectCreateUnsupported", err)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ambiguous database wrote session data: %v", err)
	}
	for _, db := range []*sql.DB{current, old} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM threads").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("ambiguous database inserted %d rows", count)
		}
	}
}

func TestCreateThreadRejectsProjectConfigBeforeWriting(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	createThreadTestDB(t, home, createThreadTestSchema).Close()
	if err := os.MkdirAll(filepath.Join(cwd, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".codex", "config.toml"), []byte(`model_provider = "project"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewSessionStore(home).CreateThread(cwd, "项目配置回退")
	if !errors.Is(err, ErrDirectCreateUnsupported) {
		t.Fatalf("error=%v, want ErrDirectCreateUnsupported", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "sessions")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("project config fallback wrote session data: %v", statErr)
	}
}

func TestCreateThreadRejectsUnknownRequiredColumnBeforeWriting(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	schema := strings.Replace(createThreadTestSchema, "memory_mode TEXT NOT NULL DEFAULT 'enabled'",
		"memory_mode TEXT NOT NULL DEFAULT 'enabled', future_required TEXT NOT NULL", 1)
	createThreadTestDB(t, home, schema).Close()

	_, err := NewSessionStore(home).CreateThread(cwd, "不应落盘")
	if !errors.Is(err, ErrDirectCreateUnsupported) {
		t.Fatalf("error=%v, want ErrDirectCreateUnsupported", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "sessions")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsupported schema wrote session data: %v", statErr)
	}
}

func TestCreateThreadRemovesRolloutWhenIndexInsertFails(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	db := createThreadTestDB(t, home, createThreadTestSchema)
	if _, err := db.Exec(`CREATE TRIGGER reject_new_thread BEFORE INSERT ON threads
		BEGIN SELECT RAISE(ABORT, 'rejected'); END`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, err := NewSessionStore(home).CreateThread(cwd, "不能残留")
	if err == nil || errors.Is(err, ErrDirectCreateUnsupported) {
		t.Fatalf("unexpected error: %v", err)
	}
	matches, globErr := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "*.jsonl"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("orphan rollout files remain: %v", matches)
	}
}
