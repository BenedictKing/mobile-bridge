// Command gateway is the Go gateway (plan doc P3). It serves the embedded
// web assets and the HTTP API pipeline with business routes stubbed; the
// conformance/http golden is the shared acceptance gate with the Python
// gateway.
//
// Environment:
//
//	GATEWAY_PORT          listen port (default 8790)
//	GATEWAY_AUTH          JSON: {"username","salt","hash","iterations"} (pbkdf2 record)
//	GATEWAY_INSTANCE_ID   deterministic instance id for tests
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	gateway "github.com/BenedictKing/mobile-bridge"
	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/httpd"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/lifecycle"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

func main() {
	if len(os.Args) > 1 && isDesktopAction(os.Args[1]) {
		desktopAction(os.Args[1], os.Args[2:])
		return
	}
	runGateway(os.Args[1:])
}

func isDesktopAction(action string) bool {
	switch action {
	case "snapshot", "snapshot-stream", "start", "stop", "save", "serve":
		return true
	}
	return false
}

func desktopDataDir(args []string) string {
	for i, value := range args {
		if value == "--data-dir" && i+1 < len(args) {
			return args[i+1]
		}
	}
	if value := os.Getenv("GATEWAY_DATA_DIR"); value != "" {
		return value
	}
	return ".local"
}

func desktopAction(action string, args []string) {
	dataDir := desktopDataDir(args)
	switch action {
	case "serve":
		runGateway(args)
	case "snapshot":
		emitWorker(workerSnapshot(dataDir))
	case "save":
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 1_000_000))
		if err != nil {
			emitError(err)
			return
		}
		value, err := jsoncompat.Unmarshal(data)
		if err != nil {
			emitError(err)
			return
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			emitError(err)
			return
		}
		if err := os.WriteFile(filepath.Join(dataDir, "desktop.json"), append(jsoncompat.EncodePythonIndent2(value), '\n'), 0o600); err != nil {
			emitError(err)
			return
		}
		if body, ok := value.(*jsoncompat.Object); ok {
			config := jsoncompat.NewObject()
			if authValue, exists := body.Get("auth"); exists {
				config.Set("auth", authValue)
			}
			if origins, exists := body.Get("origins"); exists {
				config.Set("origins", origins)
			}
			_ = os.WriteFile(filepath.Join(dataDir, "config.json"), append(jsoncompat.EncodePythonIndent2(config), '\n'), 0o600)
		}
		emitWorker(jsoncompat.NewObject())
	case "snapshot-stream":
		for scanner := bufio.NewScanner(os.Stdin); scanner.Scan(); {
			if strings.TrimSpace(scanner.Text()) == "{\"action\":\"snapshot\"}" {
				emitWorker(workerSnapshot(dataDir))
			}
		}
	case "start":
		executable, _ := os.Executable()
		command := exec.Command(executable, "serve", "--data-dir", dataDir)
		command.Env = append(os.Environ(), "GATEWAY_DATA_DIR="+dataDir)
		if err := command.Start(); err != nil {
			emitError(err)
			return
		}
		result := jsoncompat.NewObject()
		result.Set("started", true)
		result.Set("pid", jsoncompat.Num(command.Process.Pid))
		result.Set("message", "正在启动网关")
		emitWorker(result)
	case "stop":
		controller := readControl(dataDir)
		if controller == nil {
			emitError(errors.New("No gateway control record; use Ctrl+C in its terminal if it is running."))
			return
		}
		_ = os.WriteFile(filepath.Join(dataDir, "gateway.stop"), lifecycle.DumpASCII(controller), 0o600)
		result := jsoncompat.NewObject()
		result.Set("message", "网关已停止")
		emitWorker(result)
	}
}

func emitWorker(result *jsoncompat.Object) {
	data := jsoncompat.NewObject()
	data.Set("ok", true)
	data.Set("result", result)
	_, _ = os.Stdout.Write(append(jsoncompat.EncodePythonDefault(data), '\n'))
}
func emitError(err error) {
	data := jsoncompat.NewObject()
	data.Set("ok", false)
	data.Set("error", err.Error())
	_, _ = os.Stdout.Write(append(jsoncompat.EncodePythonDefault(data), '\n'))
}
func gatewayRuntime(dataDir string) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	record := readControl(dataDir)
	if record != nil {
		result.Set("running", true)
		result.Set("pid", record.MustGet("pid"))
		result.Set("instanceId", record.MustGet("instanceId"))
	} else {
		result.Set("running", false)
		result.Set("pid", nil)
		result.Set("instanceId", nil)
	}
	result.Set("portOccupied", false)
	result.Set("supportsNotifications", true)
	return result
}

func workerSnapshot(dataDir string) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	preferences := jsoncompat.NewObject()
	if raw, err := os.ReadFile(filepath.Join(dataDir, "desktop.json")); err == nil {
		if value, err := jsoncompat.Unmarshal(raw); err == nil {
			if root, ok := value.(*jsoncompat.Object); ok {
				if value, ok := root.Get("preferences"); ok {
					preferences, _ = value.(*jsoncompat.Object)
				}
			}
		}
	}
	authValue := jsoncompat.NewObject()
	if raw, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		if value, err := jsoncompat.Unmarshal(raw); err == nil {
			if root, ok := value.(*jsoncompat.Object); ok {
				if value, ok := root.Get("auth"); ok {
					authValue, _ = value.(*jsoncompat.Object)
				}
			}
		}
	}
	result.Set("preferences", preferences)
	result.Set("auth", authValue)
	result.Set("notifications", notifications.ReadSettings(dataDir))
	result.Set("runtime", gatewayRuntime(dataDir))
	result.Set("urls", &jsoncompat.Array{})
	result.Set("externalStatus", jsoncompat.NewObject())
	result.Set("cloudflared", jsoncompat.NewObject())
	result.Set("quickTunnel", jsoncompat.NewObject())
	result.Set("watches", &jsoncompat.Array{})
	return result
}
func readControl(dataDir string) *jsoncompat.Object {
	raw, err := os.ReadFile(filepath.Join(dataDir, "gateway-control.json"))
	if err != nil {
		return nil
	}
	return lifecycle.ParseRecord(string(raw)).(*jsoncompat.Object)
}

func applyDesktopDefaults(dataDir string) {
	if os.Getenv("GATEWAY_PORT") == "" {
		if raw, err := os.ReadFile(filepath.Join(dataDir, "desktop.json")); err == nil {
			if value, err := jsoncompat.Unmarshal(raw); err == nil {
				if root, ok := value.(*jsoncompat.Object); ok {
					if prefs, ok := root.MustGet("preferences").(*jsoncompat.Object); ok {
						if port, ok := prefs.MustGet("port").(json.Number); ok {
							_ = os.Setenv("GATEWAY_PORT", port.String())
						}
					}
				}
			}
		}
	}
	if os.Getenv("GATEWAY_AUTH") == "" {
		if raw, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
			if value, err := jsoncompat.Unmarshal(raw); err == nil {
				if root, ok := value.(*jsoncompat.Object); ok {
					if authValue, ok := root.MustGet("auth").(*jsoncompat.Object); ok {
						_ = os.Setenv("GATEWAY_AUTH", string(jsoncompat.EncodeCompact(authValue)))
					}
				}
			}
		}
	}
}

func runGateway(args []string) {
	for i, value := range args {
		if i+1 >= len(args) {
			continue
		}
		switch value {
		case "--data-dir":
			_ = os.Setenv("GATEWAY_DATA_DIR", args[i+1])
		case "--port":
			_ = os.Setenv("GATEWAY_PORT", args[i+1])
		case "--codex-home":
			_ = os.Setenv("CODEX_HOME", args[i+1])
		case "--ipc-path":
			_ = os.Setenv("GATEWAY_IPC_PATH", args[i+1])
		case "--codex-bin":
			_ = os.Setenv("GATEWAY_CODEX_BIN", args[i+1])
		}
	}
	dataDir := desktopDataDir(args)
	applyDesktopDefaults(dataDir)
	port := 8787
	if value := os.Getenv("GATEWAY_PORT"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			log.Fatalf("GATEWAY_PORT: %v", err)
		}
		port = parsed
	}
	config := auth.Config{Mode: "password", Username: "admin", Iterations: 600000}
	if value := os.Getenv("GATEWAY_AUTH"); value != "" {
		var record struct {
			Mode       string `json:"mode"`
			Username   string `json:"username"`
			Salt       string `json:"salt"`
			Hash       string `json:"hash"`
			Iterations int    `json:"iterations"`
		}
		if err := json.Unmarshal([]byte(value), &record); err != nil {
			log.Fatalf("GATEWAY_AUTH: %v", err)
		}
		if record.Mode != "" {
			config.Mode = record.Mode
		}
		config.Username = record.Username
		config.Salt = record.Salt
		config.Hash = record.Hash
		config.Iterations = record.Iterations
	}
	instanceID := ""
	if value := os.Getenv("GATEWAY_INSTANCE_ID"); value != "" {
		instanceID = value
	} else {
		buffer := make([]byte, 16)
		if _, err := rand.Read(buffer); err != nil {
			log.Fatal(err)
		}
		instanceID = hex.EncodeToString(buffer)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	server := httpd.New(auth.NewAuth(config), gateway.WebFS, port, instanceID)
	server.Uploads = uploads.New(dataDir)
	server.NotificationDir = dataDir
	server.Jev = jev.New(filepath.Join(dataDir, "jev.json"))
	if ledger := notifications.NewOutcomeLedger(dataDir); ledger != nil {
		_ = ledger.Reconcile(time.Now(), nil, nil)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil {
			codexHome = filepath.Join(home, ".codex")
		}
	}
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_STORE") != "1" {
		server.Store = store.NewSessionStore(codexHome)
	}
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_IPC") != "1" {
		server.Codex = codex.New(codexHome, os.Getenv("GATEWAY_IPC_PATH"))
	}
	server.Account = account.NewWithData(codexHome, dataDir, os.Getenv("GATEWAY_CODEX_BIN"))
	server.Goal = goal.New(codexHome, os.Getenv("GATEWAY_CODEX_BIN"))
	if executable := os.Getenv("GATEWAY_ACP_BIN"); executable != "" {
		server.ACP = acpruntime.New(executable)
		server.ACPAgents = map[string]*acpruntime.Manager{"acp": server.ACP}
	}
	if raw := os.Getenv("GATEWAY_ACP_BINS"); raw != "" {
		var bins map[string]string
		if err := json.Unmarshal([]byte(raw), &bins); err != nil {
			log.Fatalf("GATEWAY_ACP_BINS: %v", err)
		}
		if server.ACPAgents == nil {
			server.ACPAgents = map[string]*acpruntime.Manager{}
		}
		for kind, executable := range bins {
			if strings.TrimSpace(kind) != "" && executable != "" {
				server.ACPAgents[kind] = acpruntime.New(executable)
			}
		}
		if server.ACP == nil {
			if manager := server.ACPAgents["acp"]; manager != nil {
				server.ACP = manager
			}
		}
	}
	controller, controlErr := lifecycle.NewController(dataDir, os.Getpid(), instanceID)
	if controlErr != nil {
		log.Fatalf("control file: %v", controlErr)
	}
	outcomeWorker := notifications.NewOutcomeLedger(dataDir).StartPublisher(time.Minute, notifications.NewPublisher(nil), nil)
	httpServer := &http.Server{Handler: server}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if controller.StopRequested() {
					_ = httpServer.Shutdown(context.Background())
					return
				}
			case <-shutdown:
				_ = httpServer.Shutdown(context.Background())
				return
			}
		}
	}()
	log.Printf("gateway listening on http://%s", listener.Addr())
	err = httpServer.Serve(listener)
	outcomeWorker.Close()
	controller.Close()
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
