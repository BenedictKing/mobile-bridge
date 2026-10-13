package codex

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// CreateConversation uses the short-lived Codex app-server protocol for the
// one operation the desktop owner does not expose through follower IPC.
func (b *Bridge) CreateConversation(cwd, title string) (string, error) {
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
	if cwd == "" {
		cwd = b.Home
	}
	if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
		return "", errors.New("项目目录不存在，请先在电脑 App 中检查项目")
	}
	command := exec.Command(executable, "app-server", "--listen", "stdio://")
	command.Dir = cwd
	command.Env = append(os.Environ(), "CODEX_HOME="+b.Home)
	stdin, err := command.StdinPipe()
	if err != nil {
		return "", errors.New("无法连接 Codex 创建服务")
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", errors.New("无法连接 Codex 创建服务")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return "", errors.New("无法启动 Codex 创建服务")
	}
	defer func() {
		_ = stdin.Close()
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	request := func(method string, params any) (*jsoncompat.Object, error) {
		return appRequest(stdin, scanner, method, params)
	}
	if _, err := request("initialize", map[string]any{"clientInfo": map[string]any{"name": "mobile_bridge_go", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}); err != nil {
		return "", err
	}
	if _, err := stdin.Write([]byte("{\"method\":\"initialized\"}\n")); err != nil {
		return "", err
	}
	created, err := request("thread/start", map[string]any{"cwd": cwd, "ephemeral": false})
	if err != nil {
		return "", err
	}
	thread, _ := created.MustGet("thread").(*jsoncompat.Object)
	if thread == nil {
		thread = created
	}
	id, _ := thread.MustGet("id").(string)
	if id == "" {
		return "", errors.New("Codex 创建结果缺少会话 ID")
	}
	if title != "" {
		if _, err := request("thread/name/set", map[string]any{"threadId": id, "name": title}); err != nil {
			return "", err
		}
	}
	return id, nil
}

var appRequestID atomic.Uint64

func appRequest(stdin io.Writer, scanner *bufio.Scanner, method string, params any) (*jsoncompat.Object, error) {
	requestID := appRequestID.Add(1)
	payload := map[string]any{"id": requestID, "method": method, "params": params}
	data, _ := json.Marshal(payload)
	if _, err := stdin.Write(append(data, '\n')); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && scanner.Scan() {
		var response map[string]any
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			continue
		}
		if jsoncompat.PyStr(response["id"]) != strconv.FormatUint(requestID, 10) {
			continue
		}
		if response["error"] != nil {
			return nil, errors.New("Codex 创建操作未完成，请检查电脑端登录状态")
		}
		raw, _ := json.Marshal(response["result"])
		value, err := jsoncompat.Unmarshal(raw)
		if err != nil {
			return nil, err
		}
		object, _ := value.(*jsoncompat.Object)
		if object == nil {
			return jsoncompat.NewObject(), nil
		}
		return object, nil
	}
	return nil, errors.New("Codex 创建服务超时或已断开")
}
