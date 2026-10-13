package tunnel

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// SSHForward supervises the safe, non-interactive reverse-forward command.
// Authentication remains delegated to the user's OpenSSH config/agent.
type SSHForward struct {
	Target     string
	RemotePort int
	LocalPort  int
	DataDir    string
	ID         string

	mu      sync.Mutex
	process *exec.Cmd
	stop    context.CancelFunc
}

func (f *SSHForward) Start() error {
	if f.Target == "" || f.RemotePort < 1 || f.RemotePort > 65535 {
		return errors.New("SSH 转发配置无效")
	}
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("未找到 OpenSSH 客户端")
	}
	args := []string{"-v", "-N", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ForkAfterAuthentication=no", "-o", "PermitLocalCommand=no", "-o", "ForwardAgent=no", "-o", "UpdateHostKeys=no", "-o", "AddKeysToAgent=no", "-R", "127.0.0.1:" + strconv.Itoa(f.RemotePort) + ":127.0.0.1:" + strconv.Itoa(f.LocalPort), f.Target}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, ssh, args...)
	command.Stdin = nil
	stderr, err := command.StderrPipe()
	if err != nil {
		cancel()
		return err
	}
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		return err
	}
	f.mu.Lock()
	f.process, f.stop = command, cancel
	f.mu.Unlock()
	f.writeStatus("connecting", "正在连接自有服务器…")
	go f.watch(command, stderr)
	return nil
}

func (f *SSHForward) watch(command *exec.Cmd, stderr io.ReadCloser) {
	logPath := filepath.Join(f.DataDir, "ssh-tunnel-"+f.ID+".log")
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if logFile != nil {
		defer logFile.Close()
	}
	scanner := bufio.NewScanner(stderr)
	connected := false
	for scanner.Scan() {
		line := scanner.Text()
		if logFile != nil {
			_, _ = logFile.WriteString(time.Now().Format(time.RFC3339) + " " + line + "\n")
		}
		if strings.Contains(line, "remote forward success for:") {
			connected = true
			f.writeStatus("connected", "SSH 转发已建立；请检测固定 HTTPS 入口")
		}
	}
	_, _ = command.Process.Wait()
	f.mu.Lock()
	if f.process == command {
		f.process, f.stop = nil, nil
	}
	f.mu.Unlock()
	if !connected {
		f.writeStatus("retrying", "SSH 连接失败或已中断，正在重试；详情见 SSH 日志")
	} else {
		f.writeStatus("stopped", "SSH 转发已停止")
	}
}

func (f *SSHForward) Close() {
	f.mu.Lock()
	stop, process := f.stop, f.process
	f.stop, f.process = nil, nil
	f.mu.Unlock()
	if stop != nil {
		stop()
	}
	if process != nil {
		_ = process.Process.Kill()
	}
	f.writeStatus("stopped", "SSH 转发已停止")
}

func (f *SSHForward) writeStatus(state, message string) {
	value := jsoncompat.NewObject()
	value.Set("pid", jsoncompat.Num(os.Getpid()))
	value.Set("state", state)
	value.Set("message", message)
	_ = os.MkdirAll(f.DataDir, 0o700)
	_ = os.WriteFile(filepath.Join(f.DataDir, "ssh-status-"+f.ID+".json"), append(jsoncompat.EncodePythonIndent2(value), '\n'), 0o600)
}
