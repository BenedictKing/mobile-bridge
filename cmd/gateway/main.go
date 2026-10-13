// Command gateway is the Go gateway. It serves the embedded web assets and
// the production HTTP/API pipeline; the conformance/http golden remains the
// shared compatibility gate with the legacy Python oracle.
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
	"encoding/base64"
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
	"github.com/BenedictKing/mobile-bridge/internal/tunnel"
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
	ensureDesktopConfig(dataDir)
	result := jsoncompat.NewObject()
	preferences := desktopPreferences(dataDir)
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
					if saved, ok := value.(*jsoncompat.Object); ok {
						for _, key := range []string{"username", "mode", "sessionHours"} {
							if item, exists := saved.Get(key); exists {
								authValue.Set(key, item)
							}
						}
					}
				}
			}
		}
	}
	result.Set("preferences", preferences)
	result.Set("auth", authValue)
	result.Set("notifications", notifications.ReadSettings(dataDir))
	result.Set("runtime", gatewayRuntime(dataDir))
	urls := &jsoncompat.Array{}
	port := 8787
	if value, ok := preferences.MustGet("port").(json.Number); ok {
		if parsed, parseErr := strconv.Atoi(value.String()); parseErr == nil {
			port = parsed
		}
	}
	for _, address := range gatewayListenAddresses(dataDir) {
		if preferences.MustGet("localAccess") == false && (address == "127.0.0.1" || address == "::1") {
			continue
		}
		urls.Items = append(urls.Items, "http://"+net.JoinHostPort(address, strconv.Itoa(port))+"/")
	}
	result.Set("urls", urls)
	result.Set("externalStatus", jsoncompat.NewObject())
	result.Set("cloudflared", jsoncompat.NewObject())
	quickTunnel := jsoncompat.NewObject()
	if raw, err := os.ReadFile(filepath.Join(dataDir, "cloudflare-status.json")); err == nil {
		if value, decodeErr := jsoncompat.Unmarshal(raw); decodeErr == nil {
			if status, ok := value.(*jsoncompat.Object); ok {
				quickTunnel = status
			}
		}
	}
	if quickTunnel.MustGet("state") == "ready" {
		if raw, err := os.ReadFile(filepath.Join(dataDir, "外网地址.txt")); err == nil {
			if line := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0]); strings.HasPrefix(line, "https://") {
				urls.Items = append([]any{line + "/"}, urls.Items...)
			}
		}
	}
	result.Set("quickTunnel", quickTunnel)
	result.Set("watches", &jsoncompat.Array{})
	return result
}

func desktopPreferences(dataDir string) *jsoncompat.Object {
	preferences := jsoncompat.NewObject()
	preferences.Set("autoStart", false)
	preferences.Set("port", jsoncompat.Num(8787))
	preferences.Set("lan", true)
	preferences.Set("lanAddresses", nil)
	preferences.Set("localAccess", true)
	preferences.Set("tunnel", false)
	preferences.Set("cloudflared", "")
	home, _ := os.UserHomeDir()
	preferences.Set("codexHome", filepath.Join(home, ".codex"))
	preferences.Set("ipcPath", "")
	preferences.Set("codexBin", "")
	if raw, err := os.ReadFile(filepath.Join(dataDir, "desktop.json")); err == nil {
		if value, decodeErr := jsoncompat.Unmarshal(raw); decodeErr == nil {
			if saved, ok := value.(*jsoncompat.Object); ok {
				for _, key := range []string{"autoStart", "port", "lan", "lanAddresses", "localAccess", "tunnel", "cloudflared", "codexHome", "ipcPath", "codexBin", "connections"} {
					if item, exists := saved.Get(key); exists {
						preferences.Set(key, item)
					}
				}
			}
		}
	}
	return preferences
}
func readControl(dataDir string) *jsoncompat.Object {
	raw, err := os.ReadFile(filepath.Join(dataDir, "gateway-control.json"))
	if err != nil {
		return nil
	}
	return lifecycle.ParseRecord(string(raw)).(*jsoncompat.Object)
}

func applyDesktopDefaults(dataDir string) {
	ensureDesktopConfig(dataDir)
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

// ensureDesktopConfig creates the first-run password record used by the Go
// gateway. A fresh Wails installation must be usable without a Python helper
// or an Electron worker having to seed the data directory first.
func ensureDesktopConfig(dataDir string) {
	path := filepath.Join(dataDir, "config.json")
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		log.Fatalf("data directory: %v", err)
	}
	passwordBytes := make([]byte, 18)
	if _, err := rand.Read(passwordBytes); err != nil {
		log.Fatal(err)
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes)
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		log.Fatal(err)
	}
	authRecord := auth.PasswordRecord(password, salt)
	authRecord.Set("mode", "password")
	authRecord.Set("username", "admin")
	authRecord.Set("sessionHours", jsoncompat.Num(12))
	config := jsoncompat.NewObject()
	config.Set("auth", authRecord)
	config.Set("origins", &jsoncompat.Array{})
	if err := os.WriteFile(path, append(jsoncompat.EncodePythonIndent2(config), '\n'), 0o600); err != nil {
		log.Fatalf("config: %v", err)
	}
	credentials := "Codex App 手机网关\n账号：admin\n密码：" + password + "\n"
	_ = os.WriteFile(filepath.Join(dataDir, "首次登录.txt"), []byte(credentials), 0o600)
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

	server := httpd.New(auth.NewAuth(config), gateway.WebFS, port, instanceID)
	if os.Getenv("GATEWAY_DISABLE_STORE") == "1" && os.Getenv("GATEWAY_DISABLE_IPC") == "1" {
		server.Bridge = httpd.NewScriptedBridge()
	}
	addresses := gatewayListenAddresses(dataDir)
	listeners := make([]net.Listener, 0, len(addresses))
	for _, address := range addresses {
		listener, listenErr := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(port)))
		if listenErr != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			log.Fatalf("listen %s: %v", address, listenErr)
		}
		listeners = append(listeners, listener)
		server.AddListenHost(address, address == "127.0.0.1" || address == "::1" || address == "localhost")
	}
	server.Uploads = uploads.New(dataDir)
	server.NotificationDir = dataDir
	server.Jev = jev.New(filepath.Join(dataDir, "jev.json"))
	var quickTunnel *tunnel.Quick
	preferences := desktopPreferences(dataDir)
	if enabled, _ := preferences.MustGet("tunnel").(bool); enabled {
		cloudflared, _ := preferences.MustGet("cloudflared").(string)
		quickTunnel = &tunnel.Quick{Executable: cloudflared, DataDir: dataDir, Port: port}
		if tunnelErr := quickTunnel.Start(); tunnelErr != nil {
			log.Printf("quick tunnel unavailable: %v", tunnelErr)
		}
	}
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
		server.Codex.Executable = os.Getenv("GATEWAY_CODEX_BIN")
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
	serveErrors := make(chan error, len(listeners))
	for _, listener := range listeners {
		go func(opened net.Listener) {
			serveErrors <- httpServer.Serve(opened)
		}(listener)
	}
	log.Printf("gateway listening on %s", strings.Join(addresses, ","))
	err := <-serveErrors
	if err != nil && err != http.ErrServerClosed {
		_ = httpServer.Shutdown(context.Background())
	}
	outcomeWorker.Close()
	if quickTunnel != nil {
		quickTunnel.Close()
	}
	controller.Close()
	if err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// gatewayListenAddresses mirrors the desktop network defaults without
// depending on a Python helper. Explicit lanAddresses are honored exactly;
// when omitted, all non-loopback interface addresses are selected.
func gatewayListenAddresses(dataDir string) []string {
	lan := true
	localAccess := true
	var selected []string
	if raw, err := os.ReadFile(filepath.Join(dataDir, "desktop.json")); err == nil {
		if value, decodeErr := jsoncompat.Unmarshal(raw); decodeErr == nil {
			if root, ok := value.(*jsoncompat.Object); ok {
				if flag, ok := root.MustGet("lan").(bool); ok {
					lan = flag
				}
				if flag, ok := root.MustGet("localAccess").(bool); ok {
					localAccess = flag
				}
				if values, ok := root.MustGet("lanAddresses").(*jsoncompat.Array); ok {
					for _, item := range values.Items {
						if address, ok := item.(string); ok && net.ParseIP(address) != nil {
							selected = append(selected, address)
						}
					}
				}
			}
		}
	}
	if !lan {
		return []string{"127.0.0.1"}
	}
	if len(selected) == 0 {
		interfaces, _ := net.Interfaces()
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addresses, _ := iface.Addrs()
			for _, raw := range addresses {
				value := strings.Split(raw.String(), "/")[0]
				ip := net.ParseIP(value)
				if ip != nil && !ip.IsLinkLocalUnicast() {
					selected = append(selected, value)
				}
			}
		}
	}
	if localAccess {
		selected = append([]string{"127.0.0.1"}, selected...)
	}
	if len(selected) == 0 {
		selected = []string{"127.0.0.1"}
	}
	seen := map[string]bool{}
	result := make([]string, 0, len(selected))
	for _, address := range selected {
		if !seen[address] {
			seen[address] = true
			result = append(result, address)
		}
	}
	return result
}
