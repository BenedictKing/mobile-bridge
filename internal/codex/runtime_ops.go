package codex

// Short-lived app-server operations beyond creation (bridge/create.py's
// runtime pattern): thread/delete for session deletion and thread/fork for
// branch creation. The desktop App keeps owning execution; these helpers
// never call turn/start.

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// runtimeExecutable resolves the Codex binary the same way create/rename do.
func (b *Bridge) runtimeExecutable() (string, error) {
	if b.Home == "" {
		return "", errors.New("找不到 Codex 数据目录")
	}
	executable := b.Executable
	if executable == "" {
		executable = os.Getenv("GATEWAY_CODEX_BIN")
	}
	if executable == "" {
		executable, _ = exec.LookPath("codex")
	}
	if executable == "" {
		return "", errors.New("找不到 Codex 运行时，请在电脑端运行配置中指定路径")
	}
	return executable, nil
}

// runtimeOperation starts one short-lived app-server, runs the operation and
// always tears the process down before the desktop can take over (create.go's
// protocol, shared so rename/delete/fork stay one code path).
func (b *Bridge) runtimeOperation(dir string, operation func(request func(string, any) (*jsoncompat.Object, error)) error) error {
	executable, err := b.runtimeExecutable()
	if err != nil {
		return err
	}
	if dir == "" {
		dir = b.Home
	}
	command := exec.Command(executable, "app-server", "--listen", "stdio://")
	command.Dir = dir
	command.Env = append(os.Environ(), "CODEX_HOME="+b.Home)
	stdin, err := command.StdinPipe()
	if err != nil {
		return errors.New("无法连接 Codex 运行时")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return errors.New("无法连接 Codex 运行时")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return errors.New("无法启动 Codex 运行时")
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	request := func(method string, params any) (*jsoncompat.Object, error) {
		return appRequestTimeout(stdin, scanner, method, params, 60*time.Second)
	}
	if _, err := request("initialize", map[string]any{"clientInfo": map[string]any{"name": "mobile_bridge_go", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return err
	}
	if _, err := stdin.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		return err
	}
	return operation(request)
}

// DeleteThread removes a saved thread through the short-lived app-server
// (thread/delete). The rollout is deleted by Codex itself — the gateway never
// touches ~/.codex files directly.
func (b *Bridge) DeleteThread(threadID string) error {
	return b.runtimeOperation(b.Home, func(request func(string, any) (*jsoncompat.Object, error)) error {
		_, err := request("thread/delete", map[string]any{"threadId": threadID})
		return err
	})
}

// ForkThread copies a thread up to one turn (bridge/create.py fork_copy
// port): the schema gate first (older servers silently ignore unknown fields,
// which must never copy later turns or resume a goal), then thread/fork with
// lastTurnId + deferGoalContinuation, a title, and the source settings.
func (b *Bridge) ForkThread(sourceID, turnID, cwd, title string, settings map[string]any) (string, error) {
	if err := b.forkSupported(); err != nil {
		return "", err
	}
	child := ""
	err := b.runtimeOperation(cwd, func(request func(string, any) (*jsoncompat.Object, error)) error {
		params := map[string]any{"threadId": sourceID, "lastTurnId": turnID,
			"deferGoalContinuation": true, "ephemeral": false}
		for key, value := range settings {
			params[key] = value
		}
		if cwd != "" {
			params["cwd"] = cwd
		}
		result, callErr := request("thread/fork", params)
		if callErr != nil {
			return callErr
		}
		thread, _ := result.MustGet("thread").(*jsoncompat.Object)
		if thread == nil {
			thread = result
		}
		id, _ := thread.MustGet("id").(string)
		if id == "" {
			return errors.New("分支结果无效，请检查桌面聊天列表")
		}
		if id == sourceID {
			return errors.New("分支结果无效，请检查桌面聊天列表")
		}
		// The fork position must be confirmed: a server that ignored
		// lastTurnId would silently copy later turns.
		turns, _ := thread.MustGet("turns").(*jsoncompat.Array)
		if turns == nil || len(turns.Items) == 0 {
			return errors.New("分支位置未确认，请检查桌面聊天列表，不要重复创建")
		}
		last, _ := turns.Items[len(turns.Items)-1].(*jsoncompat.Object)
		lastID := ""
		if last != nil {
			lastID, _ = last.MustGet("id").(string)
			if lastID == "" {
				lastID, _ = last.MustGet("turnId").(string)
			}
		}
		if turnID != "" && lastID != turnID {
			return errors.New("分支位置未确认，请检查桌面聊天列表，不要重复创建")
		}
		if title != "" {
			if _, nameErr := request("thread/name/set", map[string]any{"threadId": id, "name": title}); nameErr != nil {
				return nameErr
			}
		}
		child = id
		return nil
	})
	if err != nil {
		return "", err
	}
	return child, nil
}

// forkSupported checks the bundled runtime's schema, not a guessed version:
// ThreadForkParams must expose lastTurnId and deferGoalContinuation (Python's
// ForkUnavailable gate).
func (b *Bridge) forkSupported() error {
	executable, err := b.runtimeExecutable()
	if err != nil {
		return err
	}
	folder, err := os.MkdirTemp("", "fork-schema-")
	if err != nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	defer func() { _ = os.RemoveAll(folder) }()
	command := exec.Command(executable, "app-server", "generate-json-schema", "--experimental", "--out", folder)
	command.Env = append(os.Environ(), "CODEX_HOME="+b.Home)
	if runErr := command.Run(); runErr != nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	raw, err := os.ReadFile(filepath.Join(folder, "v2", "ThreadForkParams.json"))
	if err != nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	schema, _ := value.(*jsoncompat.Object)
	if schema == nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	properties, _ := schema.MustGet("properties").(*jsoncompat.Object)
	if properties == nil {
		return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
	}
	for _, key := range []string{"lastTurnId", "deferGoalContinuation"} {
		if _, ok := properties.Get(key); !ok {
			return errors.New("此 Codex 版本暂不支持安全分支，请更新电脑 Codex App")
		}
	}
	return nil
}
