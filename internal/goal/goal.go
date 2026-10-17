package goal

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Manager exposes only native goal metadata RPCs.
type Manager struct {
	Home, Executable string
	mu               sync.Mutex
}

func New(home, executable string) *Manager { return &Manager{Home: home, Executable: executable} }

func (m *Manager) Get(threadID string) (*jsoncompat.Object, error) {
	result, err := m.request("thread/goal/get", map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}
	return NormalizeRPCGoal(result), nil
}
func (m *Manager) Set(threadID, objective, status string) (*jsoncompat.Object, error) {
	if status != "active" && status != "paused" {
		return nil, errors.New("目标状态无效")
	}
	params := map[string]any{"threadId": threadID, "status": status}
	if objective != "" {
		params["objective"] = objective
	}
	result, err := m.request("thread/goal/set", params)
	if err != nil {
		return nil, err
	}
	return NormalizeRPCGoal(result), nil
}
func (m *Manager) Clear(threadID string) error {
	_, err := m.request("thread/goal/clear", map[string]any{"threadId": threadID})
	return err
}

func (m *Manager) request(method string, params any) (*jsoncompat.Object, error) {
	if m.Executable == "" {
		return nil, errors.New("找不到桌面 App 的 Codex 运行时")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := exec.Command(m.Executable, "app-server", "--listen", "stdio://")
	cmd.Dir = filepath.Clean(m.Home)
	cmd.Env = append(os.Environ(), "CODEX_HOME="+m.Home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("无法连接 Codex 原生目标服务")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("无法连接 Codex 原生目标服务")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, errors.New("无法启动 Codex 原生目标服务")
	}
	defer func() { _ = stdin.Close(); _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	scanner := bufio.NewScanner(stdout)
	counter := 0
	send := func(name string, value any) (int, error) {
		counter++
		payload := map[string]any{"id": counter, "method": name, "params": value}
		data, _ := json.Marshal(payload)
		if _, err := stdin.Write(append(data, '\n')); err != nil {
			return 0, err
		}
		return counter, nil
	}
	initializeID, err := send("initialize", map[string]any{"clientInfo": map[string]any{"name": "codex_mobile_goal", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err != nil {
		return nil, err
	}
	if err := waitResponse(scanner, initializeID, 15*time.Second); err != nil {
		return nil, err
	}
	_, _ = stdin.Write([]byte("{\"method\":\"initialized\"}\n"))
	id, err := send(method, params)
	if err != nil {
		return nil, err
	}
	return responseObject(scanner, id, 15*time.Second)
}

func waitResponse(scanner *bufio.Scanner, id int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		if number(value["id"]) != id {
			continue
		}
		if value["error"] != nil {
			return errors.New("Codex 原生目标操作未完成，请检查电脑端 Codex")
		}
		return nil
	}
	return errors.New("Codex 原生目标操作超时")
}
func responseObject(scanner *bufio.Scanner, id int, timeout time.Duration) (*jsoncompat.Object, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(scanner.Bytes(), &value) != nil {
			continue
		}
		if number(value["id"]) != id {
			continue
		}
		if value["error"] != nil {
			return nil, errors.New("Codex 原生目标操作未完成，请检查电脑端 Codex")
		}
		raw, _ := json.Marshal(value["result"])
		parsed, _ := jsoncompat.Unmarshal(raw)
		result, _ := parsed.(*jsoncompat.Object)
		if result == nil {
			return jsoncompat.NewObject(), nil
		}
		return result, nil
	}
	return nil, errors.New("Codex 原生目标操作超时")
}
func number(value any) int {
	if n, ok := value.(float64); ok {
		return int(n)
	}
	return -1
}

// NormalizeRPCGoal folds a goal RPC result (either {"goal": {...}} or a bare
// goal row) into the panel shape. The row only counts when objective and
// status are both set, so an empty result reads as "no goal".
func NormalizeRPCGoal(value *jsoncompat.Object) *jsoncompat.Object {
	if value == nil {
		return nil
	}
	if nested, ok := value.MustGet("goal").(*jsoncompat.Object); ok {
		value = nested
	}
	objective, _ := value.MustGet("objective").(string)
	status, _ := value.MustGet("status").(string)
	if objective == "" || status == "" {
		return nil
	}
	value.Set("objective", objective)
	value.Set("status", status)
	return value
}
