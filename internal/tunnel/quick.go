// Package tunnel owns the opt-in Quick Tunnel process used by the Go gateway.
// LAN service startup never depends on it: a tunnel failure is reflected in
// the local status files while the gateway remains reachable locally.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

var quickURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com\b`)

type Quick struct {
	Executable string
	DataDir    string
	Port       int

	mu      sync.Mutex
	process *exec.Cmd
	stop    context.CancelFunc
	URL     string
}

func (q *Quick) Start() error {
	if q.Executable == "" {
		return errors.New("缺少 cloudflared，请查看外网访问配置")
	}
	if info, err := os.Stat(q.Executable); err != nil || info.IsDir() {
		return errors.New("找不到可运行的 cloudflared")
	}
	q.mu.Lock()
	if q.process != nil && q.process.ProcessState == nil {
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()
	if err := os.MkdirAll(q.DataDir, 0o700); err != nil {
		return err
	}
	config := filepath.Join(q.DataDir, "cloudflared.yml")
	if err := os.WriteFile(config, []byte("{}\n"), 0o600); err != nil {
		return err
	}
	metrics := "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, q.Executable, "tunnel", "--config", config, "--no-autoupdate", "--url", "http://127.0.0.1:"+itoa(q.Port), "--protocol", "http2", "--metrics", metrics, "--grace-period", "2s")
	command.Stdin = nil
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return err
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		return err
	}
	q.mu.Lock()
	q.process, q.stop = command, cancel
	q.URL = ""
	q.mu.Unlock()
	q.writeStatus("connecting", "正在建立临时 HTTPS 连接，局域网可独立使用。")
	go q.read(command, stdout)
	return nil
}

func (q *Quick) read(command *exec.Cmd, output io.Reader) {
	logPath := filepath.Join(q.DataDir, "tunnel.log")
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logFile != nil {
		defer logFile.Close()
	}
	scanner := bufio.NewScanner(output)
	for scanner.Scan() {
		line := scanner.Text()
		if logFile != nil {
			_, _ = logFile.WriteString(line + "\n")
		}
		if match := quickURL.FindString(line); match != "" {
			if strings.HasPrefix(match, "https://api.") {
				continue
			}
			q.mu.Lock()
			q.URL = match
			q.mu.Unlock()
		}
		if strings.Contains(line, "Registered tunnel connection") {
			q.mu.Lock()
			url := q.URL
			q.mu.Unlock()
			if url != "" {
				_ = os.WriteFile(filepath.Join(q.DataDir, "外网地址.txt"), []byte(url+"\n\n账号和密码与局域网网关相同。\n"), 0o600)
			}
			q.writeStatus("ready", "临时 HTTPS 已连接。")
		}
	}
	q.mu.Lock()
	if q.process == command {
		q.process = nil
		q.stop = nil
	}
	q.mu.Unlock()
	q.writeStatus("stopped", "临时 HTTPS 已停止")
}

func (q *Quick) Close() {
	q.mu.Lock()
	stop, process := q.stop, q.process
	q.stop, q.process = nil, nil
	q.mu.Unlock()
	if stop != nil {
		stop()
	}
	if process != nil {
		wait := make(chan struct{})
		go func() { _, _ = process.Process.Wait(); close(wait) }()
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			_ = process.Process.Kill()
		}
	}
	q.writeStatus("stopped", "临时 HTTPS 已停止")
}

func (q *Quick) CurrentURL() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.URL
}

func (q *Quick) writeStatus(state, message string) {
	value := jsoncompat.NewObject()
	value.Set("pid", jsoncompat.Num(os.Getpid()))
	value.Set("state", state)
	value.Set("message", message)
	_ = os.MkdirAll(q.DataDir, 0o700)
	_ = os.WriteFile(filepath.Join(q.DataDir, "cloudflare-status.json"), append(jsoncompat.EncodePythonIndent2(value), '\n'), 0o600)
}

func itoa(value int) string {
	if value < 0 {
		return "0"
	}
	const digits = "0123456789"
	if value == 0 {
		return "0"
	}
	buf := make([]byte, 0, 8)
	for value > 0 {
		buf = append([]byte{digits[value%10]}, buf...)
		value /= 10
	}
	return string(buf)
}
