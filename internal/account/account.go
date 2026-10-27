// Package account reads the Codex app-server account surface without
// exposing credentials or allowing arbitrary RPC forwarding.
package account

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
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

type Manager struct {
	Home, Executable, DataDir string
	mu                        sync.Mutex
	// Switch state machine (switch.go): switchMu serializes Switch entries,
	// phase writes and the persisted state object; proc injects the desktop
	// App process controller (nil means the real OS controller); the waits
	// mirror Python's 8s stop-wait, 35s health wait and 500ms poll.
	switchMu  sync.Mutex
	state     *jsoncompat.Object
	proc      ProcessController
	stopWait  time.Duration
	readyWait time.Duration
	poll      time.Duration
}

func defaultSwitchWaits() (stop, ready, poll time.Duration) {
	return 8 * time.Second, 35 * time.Second, 500 * time.Millisecond
}

func (m *Manager) AccountsPublic() *jsoncompat.Object {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	return m.publicLocked()
}

// publicLocked builds the phone's account panel status: index rows, the live
// switch phase and a real canSwitch (a validated desktop executable and a
// switch state that accepts new work).
func (m *Manager) publicLocked() *jsoncompat.Object {
	state := m.loadStateLocked()
	result := jsoncompat.NewObject()
	result.Set("accounts", &jsoncompat.Array{})
	result.Set("activeId", nil)
	index := readObject(filepath.Join(m.DataDir, "accounts", "index.json"))
	if index != nil {
		if accounts, ok := index.MustGet("accounts").(*jsoncompat.Array); ok {
			result.Set("accounts", accounts)
		}
		result.Set("activeId", index.MustGet("activeId"))
	}
	switchState, _ := jsoncompat.Clone(state).(*jsoncompat.Object)
	result.Set("switch", switchState)
	executable := m.resolveExecutable()
	result.Set("desktopExecutable", executable)
	canSwitch := false
	if switchReadyPhases[phaseOf(state)] && executable != "" {
		canSwitch = newDesktopApp(executable, m.Home, m.proc).Validate(m.Executable) == nil
	}
	result.Set("canSwitch", canSwitch)
	return result
}

func (m *Manager) AccountDetails(id string) *jsoncompat.Object {
	public := m.AccountsPublic()
	accounts, _ := public.MustGet("accounts").(*jsoncompat.Array)
	for _, value := range accounts.Items {
		row, _ := value.(*jsoncompat.Object)
		if row != nil && row.MustGet("id") == id {
			return row
		}
	}
	return nil
}

func New(home, executable string) *Manager {
	stopWait, readyWait, poll := defaultSwitchWaits()
	return &Manager{Home: home, DataDir: filepath.Join(home, "mobile-bridge"), Executable: executable,
		stopWait: stopWait, readyWait: readyWait, poll: poll}
}
func NewWithData(home, dataDir, executable string) *Manager {
	stopWait, readyWait, poll := defaultSwitchWaits()
	return &Manager{Home: home, DataDir: dataDir, Executable: executable,
		stopWait: stopWait, readyWait: readyWait, poll: poll}
}

func (m *Manager) Read(refresh bool) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("visible", false)
	result.Set("loginType", "unknown")
	if m.Executable == "" {
		result.Set("loginType", "unavailable")
		result.Set("error", "找不到桌面 App 的 Codex 运行时")
		return result
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	rpc, err := startRPC(m.Home, m.Executable)
	if err != nil {
		result.Set("loginType", "unavailable")
		result.Set("error", err.Error())
		return result
	}
	defer rpc.Close()
	config, err := rpc.Request("config/read", map[string]any{"includeLayers": false})
	if err != nil {
		result.Set("error", err.Error())
		return result
	}
	if nested := object(config, "config"); nested != nil {
		config = nested
	}
	if isCustomProvider(config) {
		result.Set("loginType", "api")
		return result
	}
	auth, err := rpc.Request("account/read", map[string]any{"refreshToken": refresh})
	if err != nil {
		result.Set("error", err.Error())
		return result
	}
	row := object(auth, "account")
	if row == nil {
		result.Set("loginType", "signedOut")
		return result
	}
	kind, _ := row.MustGet("type").(string)
	if kind == "apiKey" {
		result.Set("loginType", "api")
		return result
	}
	if kind != "chatgpt" {
		result.Set("loginType", "unknown")
		return result
	}
	identity := jsoncompat.EncodeCompact(&jsoncompat.Array{Items: []any{row, auth.MustGet("workspaceRouting")}})
	sum := sha256.Sum256(identity)
	result.Set("visible", true)
	result.Set("loginType", "chatgpt")
	result.Set("accountKey", hex.EncodeToString(sum[:]))
	result.Set("email", row.MustGet("email"))
	result.Set("planType", row.MustGet("planType"))
	result.Set("limits", &jsoncompat.Array{})
	result.Set("resetCredits", nil)
	result.Set("error", nil)
	if desktop := object(config, "desktop"); desktop != nil && desktop.MustGet("agent-usage-reset-enabled") == true {
		result.Set("canReset", true)
	} else {
		result.Set("canReset", false)
	}
	if limits, limitErr := rpc.Request("account/rateLimits/read", nil); limitErr == nil {
		result.Set("limits", normalizeLimits(limits))
		result.Set("resetCredits", limits.MustGet("rateLimitResetCredits"))
	}
	result.Set("updatedAt", time.Now().Unix())
	return result
}

func (m *Manager) Consume(value *jsoncompat.Object) (*jsoncompat.Object, error) {
	// The reset card is a chat-adjacent mutation: refuse it while a switch is
	// in flight or awaiting recovery (Python operation()'s check_ready gate).
	if err := m.CheckReady(); err != nil {
		return nil, err
	}
	confirmed, _ := value.MustGet("confirmed").(bool)
	if !confirmed {
		return nil, errors.New("请先确认使用重置卡")
	}
	requestID, _ := value.MustGet("requestId").(string)
	accountKey, _ := value.MustGet("accountKey").(string)
	creditID, _ := value.MustGet("creditId").(string)
	if requestID == "" {
		return nil, errors.New("重置请求标识无效")
	}
	if len(accountKey) != 64 {
		return nil, errors.New("请刷新账号信息后重试")
	}
	status := m.Read(true)
	if status.MustGet("visible") != true || status.MustGet("accountKey") != accountKey {
		return nil, errors.New("电脑端账号已变化，请刷新后重新确认")
	}
	if status.MustGet("canReset") != true {
		return nil, errors.New("请先在 Codex 桌面设置中允许使用额度重置")
	}
	m.mu.Lock()
	rpc, err := startRPC(m.Home, m.Executable)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	params := map[string]any{"idempotencyKey": requestID}
	if creditID != "" {
		params["creditId"] = creditID
	}
	result, err := rpc.Request("account/rateLimitResetCredit/consume", params)
	rpc.Close()
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	outcome, _ := result.MustGet("outcome").(string)
	switch outcome {
	case "reset", "alreadyRedeemed", "nothingToReset", "noCredit":
	default:
		return nil, errors.New("重置结果尚未确认，请刷新并重试")
	}
	response := jsoncompat.NewObject()
	response.Set("outcome", outcome)
	response.Set("account", m.Read(true))
	return response, nil
}

type rpc struct {
	cmd     *exec.Cmd
	in      io.WriteCloser
	scanner *bufio.Scanner
	next    int
}

func startRPC(home, executable string) (*rpc, error) { return startRPCIn(home, executable) }

// startRPCIn roots one short-lived app-server at dir (CODEX_HOME=dir), which
// is also how the switch flow probes a prepared account folder.
func startRPCIn(dir, executable string) (*rpc, error) {
	cmd := exec.Command(executable, "app-server", "--listen", "stdio://")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CODEX_HOME="+dir)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("无法连接 Codex 账号服务")
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.New("无法连接 Codex 账号服务")
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, errors.New("无法启动 Codex 账号服务")
	}
	r := &rpc{cmd: cmd, in: in, scanner: bufio.NewScanner(out)}
	if _, err := r.Request("initialize", map[string]any{"clientInfo": map[string]any{"name": "codex_mobile_go", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		r.Close()
		return nil, err
	}
	_ = r.notify("initialized", nil)
	return r, nil
}

func (r *rpc) Request(method string, params any) (*jsoncompat.Object, error) {
	return r.requestDeadline(method, params, 15*time.Second)
}

// requestDeadline carries the switch flow's 30s budget (Python ManagedRPC).
func (r *rpc) requestDeadline(method string, params any, timeout time.Duration) (*jsoncompat.Object, error) {
	r.next++
	id := r.next
	msg := map[string]any{"id": id, "method": method, "params": params}
	if err := r.write(msg); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) && r.scanner.Scan() {
		var value map[string]any
		if json.Unmarshal(r.scanner.Bytes(), &value) != nil {
			continue
		}
		if intValue(value["id"]) != id {
			continue
		}
		if value["error"] != nil {
			return nil, errors.New("Codex 账号操作未完成，请检查电脑端登录状态")
		}
		raw, _ := json.Marshal(value["result"])
		decoded, _ := jsoncompat.Unmarshal(raw)
		if object, ok := decoded.(*jsoncompat.Object); ok {
			return object, nil
		}
		return jsoncompat.NewObject(), nil
	}
	return nil, errors.New("Codex 账号服务超时或已断开")
}
func (r *rpc) notify(method string, params any) error {
	return r.write(map[string]any{"method": method, "params": params})
}
func (r *rpc) write(value any) error {
	data, _ := json.Marshal(value)
	data = append(data, '\n')
	_, err := r.in.Write(data)
	return err
}
func (r *rpc) Close() { _ = r.in.Close(); _ = r.cmd.Process.Kill(); _, _ = r.cmd.Process.Wait() }
func intValue(value any) int {
	switch n := value.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return -1
	}
}
func object(root *jsoncompat.Object, key string) *jsoncompat.Object {
	if root == nil {
		return nil
	}
	value, _ := root.MustGet(key).(*jsoncompat.Object)
	return value
}
func isCustomProvider(config *jsoncompat.Object) bool {
	if config == nil {
		return false
	}
	provider, _ := config.MustGet("model_provider").(string)
	return provider != "" && provider != "openai"
}
func normalizeLimits(raw *jsoncompat.Object) *jsoncompat.Array {
	result := &jsoncompat.Array{}
	buckets := object(raw, "rateLimitsByLimitId")
	if buckets == nil {
		return result
	}
	for _, id := range buckets.Keys() {
		bucket := object(buckets, id)
		if bucket == nil {
			continue
		}
		row := jsoncompat.NewObject()
		row.Set("id", id)
		row.Set("name", bucket.MustGet("limitName"))
		windows := &jsoncompat.Array{}
		for _, kind := range []string{"primary", "secondary"} {
			value := object(bucket, kind)
			if value == nil {
				continue
			}
			window := jsoncompat.NewObject()
			window.Set("kind", kind)
			window.Set("remainingPercent", remaining(value.MustGet("usedPercent")))
			window.Set("windowDurationMins", value.MustGet("windowDurationMins"))
			window.Set("resetsAt", value.MustGet("resetsAt"))
			windows.Items = append(windows.Items, window)
		}
		row.Set("windows", windows)
		result.Items = append(result.Items, row)
	}
	return result
}
func remaining(value any) any {
	n, ok := value.(json.Number)
	if !ok {
		return nil
	}
	f, err := n.Float64()
	if err != nil {
		return nil
	}
	if f < 0 {
		f = 0
	}
	if f > 100 {
		f = 100
	}
	return 100 - f
}

func readObject(path string) *jsoncompat.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return nil
	}
	result, _ := value.(*jsoncompat.Object)
	return result
}
