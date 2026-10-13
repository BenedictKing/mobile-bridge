package main

import (
	"embed"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed frontend
var assets embed.FS

type GatewayService struct {
	gateway string
	mu      sync.Mutex
	process *exec.Cmd
	logFile *os.File
	done    chan error
}

func NewGatewayService() *GatewayService { return &GatewayService{} }

func (s *GatewayService) Info() map[string]any {
	return map[string]any{"gateway": resolveGateway(s.gateway), "dataDir": resolveDataDir()}
}

func (s *GatewayService) Snapshot() (map[string]any, error) {
	return runAction(s.gateway, "snapshot", nil)
}
func (s *GatewayService) Save(value map[string]any) (map[string]any, error) {
	return runAction(s.gateway, "save", value)
}
func (s *GatewayService) Start() (map[string]any, error) {
	s.mu.Lock()
	if s.process != nil && s.process.ProcessState == nil {
		pid := s.process.Process.Pid
		s.mu.Unlock()
		return map[string]any{"started": false, "pid": pid, "message": "网关已在运行"}, nil
	}
	s.mu.Unlock()
	if snapshot, err := runAction(s.gateway, "snapshot", nil); err == nil {
		if runtime, ok := snapshot["runtime"].(map[string]any); ok {
			if running, _ := runtime["running"].(bool); running {
				return map[string]any{"started": false, "message": "已连接正在运行的网关", "runtime": runtime}, nil
			}
		}
	}
	return s.startProcess()
}

func (s *GatewayService) Stop() (map[string]any, error) {
	result, err := runAction(s.gateway, "stop", nil)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	process := s.process
	s.process = nil
	logFile := s.logFile
	s.logFile = nil
	done := s.done
	s.done = nil
	s.mu.Unlock()
	if process != nil && process.ProcessState == nil {
		select {
		case <-done:
		case <-time.After(8 * time.Second):
			return nil, errors.New("网关停止超时，请检查本机日志")
		}
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	return result, nil
}

func (s *GatewayService) startProcess() (map[string]any, error) {
	executable := resolveGateway(s.gateway)
	dataDir := resolveDataDir()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "gateway.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	process := exec.Command(executable, "serve", "--data-dir", dataDir)
	process.Env = append(os.Environ(), "GATEWAY_DATA_DIR="+dataDir)
	process.Stdout = logFile
	process.Stderr = logFile
	if err := process.Start(); err != nil {
		_ = logFile.Close()
		return nil, err
	}
	s.mu.Lock()
	s.process = process
	s.logFile = logFile
	done := make(chan error, 1)
	s.done = done
	s.mu.Unlock()
	go func() {
		done <- process.Wait()
		s.mu.Lock()
		if s.process == process {
			s.process = nil
			s.logFile = nil
			s.done = nil
		}
		s.mu.Unlock()
		_ = logFile.Close()
	}()
	return map[string]any{"started": true, "pid": process.Process.Pid, "message": "正在启动网关"}, nil
}

func main() {
	service := NewGatewayService()
	app := application.New(application.Options{
		Name:        "Mobile Bridge",
		Description: "Codex 手机网关 Go 桌面控制器",
		Services:    []application.Service{application.NewService(service)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac:         application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: false},
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{Title: "Mobile Bridge", Width: 1100, Height: 760, URL: "/"})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
