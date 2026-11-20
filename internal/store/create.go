package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

var ErrDirectCreateUnsupported = errors.New("Codex 本地存储不支持直接创建")

type threadColumnSchema struct {
	notNull    bool
	hasDefault bool
}

type threadCreateValue struct {
	name  string
	value any
}

type directCreateSettings struct {
	provider      string
	sandboxPolicy string
	approvalMode  string
}

type emptyRolloutLine struct {
	Timestamp string           `json:"timestamp"`
	Type      string           `json:"type"`
	Payload   emptyRolloutMeta `json:"payload"`
}

type emptyRolloutMeta struct {
	SessionID        string `json:"session_id"`
	ID               string `json:"id"`
	Timestamp        string `json:"timestamp"`
	Cwd              string `json:"cwd"`
	Originator       string `json:"originator"`
	CLIVersion       string `json:"cli_version"`
	Source           string `json:"source"`
	ModelProvider    string `json:"model_provider"`
	BaseInstructions any    `json:"base_instructions"`
	HistoryMode      string `json:"history_mode"`
}

// CreateThread persists only a new, empty legacy rollout and its index row.
// Simple root settings are mirrored; layered config uses the runtime fallback.
func (s *SessionStore) CreateThread(cwd, title string) (string, error) {
	database, err := s.databasePath()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDirectCreateUnsupported, err)
	}
	db, err := sql.Open("sqlite", "file:"+database+"?mode=rw&_pragma=busy_timeout(3000)")
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDirectCreateUnsupported, err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrDirectCreateUnsupported, err)
	}
	schema, err := readThreadColumnSchema(db)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDirectCreateUnsupported, err)
	}
	settings, err := readDirectCreateSettings(s.Home, cwd)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDirectCreateUnsupported, err)
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	rollout := filepath.Join(s.Home, "sessions", now.Format("2006"), now.Format("01"), now.Format("02"),
		"rollout-"+now.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	values := threadCreateValues(id, rollout, cwd, title, settings, now)
	provided := make(map[string]bool, len(values))
	for _, value := range values {
		provided[value.name] = true
	}
	for _, required := range []string{"id", "rollout_path", "created_at", "updated_at", "source", "model_provider", "cwd", "title", "sandbox_policy", "approval_mode"} {
		if _, ok := schema[required]; !ok {
			return "", fmt.Errorf("%w: threads 缺少 %s", ErrDirectCreateUnsupported, required)
		}
	}
	for name, definition := range schema {
		if definition.notNull && !definition.hasDefault && !provided[name] {
			return "", fmt.Errorf("%w: threads 含有未知必填列 %s", ErrDirectCreateUnsupported, name)
		}
	}

	if err := writeEmptyRollout(rollout, id, cwd, settings.provider, now); err != nil {
		return "", err
	}
	columns := []string{}
	arguments := []any{}
	placeholders := []string{}
	for _, value := range values {
		if _, ok := schema[value.name]; !ok {
			continue
		}
		columns = append(columns, value.name)
		arguments = append(arguments, value.value)
		placeholders = append(placeholders, "?")
	}
	_, err = db.Exec("INSERT INTO threads ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")", arguments...)
	if err != nil {
		if cleanupErr := os.Remove(rollout); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
			err = errors.Join(err, cleanupErr)
		}
		return "", fmt.Errorf("无法写入 Codex 会话索引: %w", err)
	}
	return id, nil
}

func readThreadColumnSchema(db *sql.DB) (map[string]threadColumnSchema, error) {
	rows, err := db.Query("PRAGMA table_info(threads)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schema := map[string]threadColumnSchema{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		schema[name] = threadColumnSchema{notNull: notNull != 0, hasDefault: defaultValue != nil}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(schema) == 0 {
		return nil, errors.New("threads 表不存在")
	}
	return schema, nil
}

func readDirectCreateSettings(home, cwd string) (directCreateSettings, error) {
	settings := directCreateSettings{provider: "openai", sandboxPolicy: "read-only", approvalMode: "on-request"}
	if err := rejectDirectProjectConfig(home, cwd); err != nil {
		return directCreateSettings{}, err
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return settings, nil
	}
	if err != nil {
		return directCreateSettings{}, err
	}
	root, seen := true, map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			header, _, _ := strings.Cut(trimmed, "#")
			table := strings.Trim(strings.TrimSpace(header), "[] \t")
			if complexDirectCreateKey(table) {
				return directCreateSettings{}, fmt.Errorf("Codex 配置含有需运行时解析的表 %s", table)
			}
			root = false
			continue
		}
		if !root {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if complexDirectCreateKey(key) {
			return directCreateSettings{}, fmt.Errorf("Codex 配置含有需运行时解析的键 %s", key)
		}
		switch key {
		case "model_provider", "profile", "sandbox_mode", "approval_policy":
		default:
			continue
		}
		if seen[key] {
			return directCreateSettings{}, fmt.Errorf("config.toml 含有重复 %s", key)
		}
		seen[key] = true
		parsed, parseErr := parseRootConfigString(value)
		if parseErr != nil {
			return directCreateSettings{}, fmt.Errorf("解析 %s 失败: %w", key, parseErr)
		}
		switch key {
		case "profile":
			if parsed != "" {
				return directCreateSettings{}, errors.New("当前 Codex 配置启用了 profile")
			}
		case "model_provider":
			if parsed == "" {
				return directCreateSettings{}, errors.New("model_provider 不能为空")
			}
			settings.provider = parsed
		case "sandbox_mode":
			switch parsed {
			case "read-only", "workspace-write", "danger-full-access":
				settings.sandboxPolicy = parsed
			default:
				return directCreateSettings{}, fmt.Errorf("未知 sandbox_mode %q", parsed)
			}
		case "approval_policy":
			if parsed == "" {
				return directCreateSettings{}, errors.New("approval_policy 不能为空")
			}
			settings.approvalMode = parsed
		}
	}
	return settings, nil
}

func complexDirectCreateKey(key string) bool {
	for _, prefix := range []string{"permissions", "default_permissions", "permission_profile", "sandbox_workspace_write", "history", "memories"} {
		if key == prefix || strings.HasPrefix(key, prefix+".") {
			return true
		}
	}
	return false
}

func rejectDirectProjectConfig(home, cwd string) error {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	homeConfig, err := filepath.Abs(filepath.Join(home, "config.toml"))
	if err != nil {
		return err
	}
	for {
		path := filepath.Join(dir, ".codex", "config.toml")
		if filepath.Clean(path) != filepath.Clean(homeConfig) {
			if _, statErr := os.Stat(path); statErr == nil {
				return fmt.Errorf("项目配置 %s 需要官方运行时解析", path)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func parseRootConfigString(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 || (raw[0] != '"' && raw[0] != '\'') {
		return "", errors.New("值必须是单行字符串")
	}
	quote := raw[0]
	for index := 1; index < len(raw); index++ {
		if quote == '"' && raw[index] == '\\' {
			index++
			continue
		}
		if raw[index] != quote {
			continue
		}
		rest := strings.TrimSpace(raw[index+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", errors.New("字符串后含有无法识别的内容")
		}
		literal := raw[:index+1]
		if quote == '\'' {
			return literal[1 : len(literal)-1], nil
		}
		value, err := strconv.Unquote(literal)
		if err != nil {
			return "", err
		}
		return value, nil
	}
	return "", errors.New("字符串未闭合")
}

func threadCreateValues(id, rollout, cwd, title string, settings directCreateSettings, now time.Time) []threadCreateValue {
	seconds, millis := now.Unix(), now.UnixMilli()
	return []threadCreateValue{
		{"id", id}, {"rollout_path", rollout},
		{"created_at", seconds}, {"updated_at", seconds}, {"recency_at", seconds},
		{"created_at_ms", millis}, {"updated_at_ms", millis}, {"recency_at_ms", millis},
		{"source", "vscode"}, {"originator", "mobile_bridge_go"},
		{"history_mode", "legacy"}, {"thread_source", "user"},
		{"model_provider", settings.provider}, {"cwd", cwd}, {"cli_version", "mobile-bridge"},
		{"title", title}, {"name", title}, {"preview", ""},
		{"sandbox_policy", settings.sandboxPolicy}, {"approval_mode", settings.approvalMode},
		{"tokens_used", 0}, {"has_user_event", 0}, {"first_user_message", ""},
		{"archived", 0}, {"is_pinned", 0}, {"memory_mode", "enabled"},
	}
}

func writeEmptyRollout(path, id, cwd, provider string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("无法创建 Codex 会话目录: %w", err)
	}
	timestamp := now.Format("2006-01-02T15:04:05.000Z")
	line := emptyRolloutLine{Timestamp: timestamp, Type: "session_meta", Payload: emptyRolloutMeta{
		SessionID: id, ID: id, Timestamp: timestamp, Cwd: cwd, Originator: "mobile_bridge_go",
		CLIVersion: "mobile-bridge", Source: "vscode", ModelProvider: provider, HistoryMode: "legacy",
	}}
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("无法创建 Codex 会话记录: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(path)
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		cleanup()
		return fmt.Errorf("无法写入 Codex 会话记录: %w", err)
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("无法保存 Codex 会话记录: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("无法关闭 Codex 会话记录: %w", err)
	}
	return nil
}
