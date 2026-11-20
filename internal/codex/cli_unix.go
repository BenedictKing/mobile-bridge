//go:build !windows

package codex

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
	"golang.org/x/sys/unix"
)

type cliStream struct {
	connection *websocket.Conn
	buffer     *bytes.Reader
}

func (s *cliStream) Read(data []byte) (int, error) {
	if s.buffer == nil || s.buffer.Len() == 0 {
		var message []byte
		if err := websocket.Message.Receive(s.connection, &message); err != nil {
			return 0, err
		}
		s.buffer = bytes.NewReader(append(message, '\n'))
	}
	return s.buffer.Read(data)
}

func (s *cliStream) Write(data []byte) (int, error) {
	if err := websocket.Message.Send(s.connection, strings.TrimSpace(string(data))); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (s *cliStream) Close() error { return s.connection.Close() }

func dialCLI(path string) (io.ReadWriteCloser, error) {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	config, _ := websocket.NewConfig("ws://localhost/", "http://localhost/")
	ws, err := websocket.NewClient(config, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	ws.MaxPayloadBytes = 64 << 20
	return &cliStream{connection: ws}, nil
}

// A dedicated official app-server owns the Unix socket and survives gateway
// restarts. No launchd/system configuration or desktop process is changed.
func connectCLITransport(executable, home, path string) (io.ReadWriteCloser, error) {
	if !filepath.IsAbs(path) || len(path) > 100 {
		return nil, errors.New("Codex CLI socket 路径必须为绝对路径且不超过 100 字节")
	}
	if stream, err := dialCLI(path); err == nil {
		return stream, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
			return nil, fmt.Errorf("Codex CLI 启动锁不可用: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN)
	if stream, err := dialCLI(path); err == nil {
		return stream, nil
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return nil, err
	}
	log, err := os.OpenFile(path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	command := exec.Command(executable, "app-server", "--listen", "unix://"+path)
	command.Dir = home
	command.Env = append(os.Environ(), "CODEX_HOME="+home)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path+".pid", []byte(strconv.Itoa(command.Process.Pid)), 0o600); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, err
	}
	_ = command.Process.Release()
	for time.Now().Before(deadline) {
		if stream, err := dialCLI(path); err == nil {
			return stream, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, errors.New("Codex CLI 运行时启动超时，请检查 socket 日志")
}
