// Command gateway is the Go gateway. It serves the embedded web assets and
// the production HTTP/API pipeline; the conformance/http golden remains the
// shared compatibility gate with the legacy Python oracle.
//
// Environment:
//
//	GATEWAY_PORT          listen port (default 8787)
//	GATEWAY_AUTH          JSON: {"username","salt","hash","iterations"} (pbkdf2 record)
//	GATEWAY_INSTANCE_ID   deterministic instance id for tests
//	GATEWAY_ACP_HOST_SOCKET   optional Unix socket for the generic external ACP Host
//	GATEWAY_ACP_HOST_SOCKETS  JSON map of agent kind to external ACP Host socket
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	gateway "github.com/BenedictKing/mobile-bridge"
	"github.com/BenedictKing/mobile-bridge/internal/account"
	"github.com/BenedictKing/mobile-bridge/internal/acpagents"
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
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
	"github.com/BenedictKing/mobile-bridge/internal/tunnel"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
)

func main() {
	help, action, rest, err := classifyArgs(os.Args[1:])
	if help {
		writeUsage(os.Stdout)
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		writeUsage(os.Stderr)
		os.Exit(2)
	}
	if action != "" {
		desktopAction(action, rest)
		return
	}
	runGateway(rest)
}

// classifyArgs turns the command line into a dispatch decision. Help requests
// print usage and exit 0; an unrecognized leading word or flag is an error —
// unknown arguments must never fall through into silently serving.
func classifyArgs(args []string) (help bool, action string, rest []string, err error) {
	for _, value := range args {
		if value == "--help" || value == "-h" || value == "help" {
			return true, "", nil, nil
		}
	}
	if len(args) == 0 {
		return false, "", nil, nil
	}
	if !isDesktopAction(args[0]) {
		return false, "", nil, fmt.Errorf("未知子命令或参数：%s（运行网关请用 serve，--help 查看用法）", args[0])
	}
	return false, args[0], args[1:], nil
}

func writeUsage(w io.Writer) {
	fmt.Fprint(w, `用法：codex-mobile-gateway <子命令> [参数]

子命令：
  serve            运行网关（默认数据目录 .local，默认端口 8787）
  start            后台启动网关进程
  stop             通过控制文件请求网关停止
  snapshot         输出桌面状态快照 JSON
  snapshot-stream  按 stdin 指令持续输出快照
  save             从 stdin 读取并保存桌面配置
  help             显示本帮助（同 --help / -h）

serve 参数：
  --data-dir DIR   数据目录（GATEWAY_DATA_DIR）
  --port N         监听端口（GATEWAY_PORT，默认 8787）
  --codex-home DIR Codex 主目录（CODEX_HOME）
  --ipc-path PATH  桌面 IPC 端点（GATEWAY_IPC_PATH）
  --codex-bin PATH Codex 可执行文件（GATEWAY_CODEX_BIN）

ACP Host 可选环境变量：
  GATEWAY_ACP_HOST_SOCKET   通用 ACP Host Unix socket
  GATEWAY_ACP_HOST_SOCKETS  按 Agent 映射的 JSON socket 配置
`)
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

// trustedOrigins mirrors run.py's origin assembly for server deployments:
// the stored config.json origins plus the publicUrl of every enabled
// server/nas/cloudflare connection. config.json carries server-deployment
// connections (run.py --lan); desktop.json carries desktop-managed ones.
// subagentStoreFor wires the on-disk child-agent readers (Python's
// native_subagents) for the agents that archive spawned children as their
// own sessions. dsh stays undiscoverable (zstd event logs).
func subagentStoreFor(kind, home string) acpruntime.Subagents {
	var store subagents.Store
	switch kind {
	case "zcode":
		store = subagents.NewZcodeStore(filepath.Join(home, ".zcode"))
	case "kimi":
		store = subagents.NewKimiStore(filepath.Join(home, ".kimi-code"))
	case "claude":
		store = subagents.NewClaudeStore(filepath.Join(home, ".claude"))
	default:
		return nil
	}
	if store == nil {
		return nil
	}
	return subagents.NewFlattener(store)
}

func trustedOrigins(dataDir string, preferences *jsoncompat.Object) []string {
	seen := map[string]bool{}
	var origins []string
	add := func(value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		origins = append(origins, value)
	}
	addEntry := func(item any) {
		entry, _ := item.(*jsoncompat.Object)
		if entry == nil || entry.MustGet("enabled") != true {
			return
		}
		mode, _ := entry.MustGet("accessMode").(string)
		if mode != "server" && mode != "nas" && mode != "cloudflare" {
			return
		}
		value, _ := entry.MustGet("publicUrl").(string)
		add(value)
	}
	if raw, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		if value, err := jsoncompat.Unmarshal(raw); err == nil {
			if root, ok := value.(*jsoncompat.Object); ok {
				if items, ok := root.MustGet("origins").(*jsoncompat.Array); ok {
					for _, item := range items.Items {
						if value, _ := item.(string); value != "" {
							add(value)
						}
					}
				}
				if items, ok := root.MustGet("connections").(*jsoncompat.Array); ok {
					for _, item := range items.Items {
						addEntry(item)
					}
				}
			}
		}
	}
	if items, ok := preferences.MustGet("connections").(*jsoncompat.Array); ok {
		for _, item := range items.Items {
			addEntry(item)
		}
	}
	return origins
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

// serveFlagEnvs maps the serve CLI flags to the environment variables
// runGateway actually reads.
var serveFlagEnvs = map[string]string{
	"--data-dir":   "GATEWAY_DATA_DIR",
	"--port":       "GATEWAY_PORT",
	"--codex-home": "CODEX_HOME",
	"--ipc-path":   "GATEWAY_IPC_PATH",
	"--codex-bin":  "GATEWAY_CODEX_BIN",
}

func applyServeFlags(args []string) error {
	for i := 0; i < len(args); i++ {
		value := args[i]
		if !strings.HasPrefix(value, "--") {
			continue
		}
		env, ok := serveFlagEnvs[value]
		if !ok {
			return fmt.Errorf("未知参数：%s（--help 查看用法）", value)
		}
		if i+1 >= len(args) {
			return fmt.Errorf("参数 %s 缺少取值", value)
		}
		i++
		_ = os.Setenv(env, args[i])
	}
	return nil
}

func runGateway(args []string) {
	if err := applyServeFlags(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		writeUsage(os.Stderr)
		os.Exit(2)
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

	authenticator := auth.NewAuth(config)
	authenticator.EnablePersistence(dataDir)
	server := httpd.New(authenticator, gateway.WebFS, port, instanceID)
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
	sshForwards := []*tunnel.SSHForward{}
	preferences := desktopPreferences(dataDir)
	for _, origin := range trustedOrigins(dataDir, preferences) {
		parsed, parseErr := url.Parse(origin)
		if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
			log.Printf("忽略无效外网地址 %q: %v", origin, parseErr)
			continue
		}
		server.Origins[origin] = true
		server.Hosts[parsed.Host] = true
		if parsed.Scheme == "https" {
			server.SecureHosts[parsed.Host] = true
		}
	}
	if enabled, _ := preferences.MustGet("tunnel").(bool); enabled {
		cloudflared, _ := preferences.MustGet("cloudflared").(string)
		quickTunnel = &tunnel.Quick{Executable: cloudflared, DataDir: dataDir, Port: port}
		if tunnelErr := quickTunnel.Start(); tunnelErr != nil {
			log.Printf("quick tunnel unavailable: %v", tunnelErr)
		}
	}
	if connections, ok := preferences.MustGet("connections").(*jsoncompat.Array); ok {
		for _, item := range connections.Items {
			entry, _ := item.(*jsoncompat.Object)
			if entry == nil || entry.MustGet("enabled") != true || entry.MustGet("accessMode") != "server" {
				continue
			}
			target, _ := entry.MustGet("sshTarget").(string)
			id, _ := entry.MustGet("id").(string)
			remotePort := 0
			if value, ok := entry.MustGet("sshRemotePort").(json.Number); ok {
				remotePort, _ = strconv.Atoi(value.String())
			}
			forward := &tunnel.SSHForward{Target: target, RemotePort: remotePort, LocalPort: port, DataDir: dataDir, ID: id}
			if forwardErr := forward.Start(); forwardErr != nil {
				log.Printf("SSH forward %s unavailable: %v", id, forwardErr)
				continue
			}
			sshForwards = append(sshForwards, forward)
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
	server.CodexHome = codexHome
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_STORE") != "1" {
		server.Store = store.NewSessionStore(codexHome)
	}
	if codexHome != "" && os.Getenv("GATEWAY_DISABLE_IPC") != "1" {
		server.Codex = codex.New(codexHome, os.Getenv("GATEWAY_IPC_PATH"))
		server.Codex.Executable = os.Getenv("GATEWAY_CODEX_BIN")
	}
	server.Account = account.NewWithData(codexHome, dataDir, os.Getenv("GATEWAY_CODEX_BIN"))
	server.Goal = goal.New(codexHome, os.Getenv("GATEWAY_CODEX_BIN"))
	// ACP agent wiring. Explicit env (GATEWAY_ACP_BIN / GATEWAY_ACP_BINS)
	// wins; otherwise the gateway auto-detects locally installed agents the
	// way the Python gateway's --agents auto did, so a plain
	// `gateway serve` keeps exposing zcode/claude/… sessions on the phone.
	// The scripted parity mode (both disables set) must stay deterministic:
	// never probe the machine from the conformance harness.
	scriptedMode := os.Getenv("GATEWAY_DISABLE_STORE") == "1" && os.Getenv("GATEWAY_DISABLE_IPC") == "1"
	agentsValue := os.Getenv("GATEWAY_AGENTS")
	if agentsValue == "" {
		agentsValue = os.Getenv("MOBILE_BRIDGE_AGENTS")
	}
	if agentsValue == "" {
		agentsValue = "auto"
	}
	agentKinds, agentKindsErr := acpagents.ParseKinds(agentsValue)
	if agentKindsErr != nil {
		log.Fatalf("GATEWAY_AGENTS: %v", agentKindsErr)
	}
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
			if strings.TrimSpace(kind) == "" || executable == "" {
				continue
			}
			// Multi-word values are full argv (wrappers that speak ACP
			// directly); single tokens keep the legacy "acp" subcommand.
			if argv := acpagents.SplitArgv(executable); len(argv) > 1 {
				server.ACPAgents[kind] = acpruntime.NewCommand(argv)
			} else {
				server.ACPAgents[kind] = acpruntime.New(executable)
			}
		}
		if server.ACP == nil {
			if manager := server.ACPAgents["acp"]; manager != nil {
				server.ACP = manager
			}
		}
	}
	hostSockets := map[string]string{}
	if raw := os.Getenv("GATEWAY_ACP_HOST_SOCKETS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &hostSockets); err != nil {
			log.Fatalf("GATEWAY_ACP_HOST_SOCKETS: %v", err)
		}
	}
	if socket := os.Getenv("GATEWAY_ACP_HOST_SOCKET"); socket != "" {
		hostSockets["acp"] = socket
	}
	for kind, socket := range hostSockets {
		if manager := server.ACPAgents[kind]; manager != nil {
			manager.HostSocket = socket
		}
	}
	home := ""
	if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
		home = userHome
	}
	autoKinds := map[string]bool{}
	if agentKinds == nil {
		for _, profile := range acpagents.Discover(os.Getenv, home) {
			autoKinds[profile.Kind] = true
		}
	} else {
		for _, kind := range agentKinds {
			autoKinds[kind] = true
		}
	}
	enabled := []string{"codex"}
	for _, profile := range acpagents.Discover(os.Getenv, home) {
		if scriptedMode || !autoKinds[profile.Kind] || profile.Command == nil {
			continue
		}
		if server.ACPAgents[profile.Kind] != nil {
			// An explicit env manager keeps its own launch command.
			continue
		}
		manager := acpruntime.NewCommand(profile.Command)
		manager.HostSocket = hostSockets[profile.Kind]
		manager.Label = profile.Label
		manager.Capabilities = profile.Capabilities
		manager.EffortMap = profile.EffortMap
		manager.InitializeTimeout = profile.InitializeTimeout
		manager.SteerMethod = profile.SteerMethod
		manager.ModeMap = profile.ModeMap
		manager.Canonical = profile.Canonical
		manager.Subagents = subagentStoreFor(profile.Kind, home)
		if server.ACPAgents == nil {
			server.ACPAgents = map[string]*acpruntime.Manager{}
		}
		server.ACPAgents[profile.Kind] = manager
		enabled = append(enabled, profile.Kind)
		if profile.Kind == "zcode" {
			// Real launches (auto-discovered, not env-forced argv) keep the
			// coding-plan key slot healed, mirroring zcode.py's
			// ensure_and_watch(); explicit argv must not touch real state.
			go acpagents.EnsureAndWatch(os.Getenv, home, log.Printf)
		}
	}
	if len(server.ACPAgents) > 0 {
		sort.Strings(enabled)
		log.Printf("agents enabled: %s", strings.Join(enabled, ", "))
		// Idle ACP sessions hold one child process each; sweep them like the
		// Python maintain loop's 300s eviction once nobody is reading them.
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				for _, manager := range server.ACPAgents {
					if manager != nil {
						manager.SweepIdle(5 * time.Minute)
					}
				}
			}
		}()
	}
	server.Creations = httpd.LoadCreations(filepath.Join(dataDir, "creations.json"))
	controller, controlErr := lifecycle.NewController(dataDir, os.Getpid(), instanceID)
	if controlErr != nil {
		log.Fatalf("control file: %v", controlErr)
	}
	outcomeWorker := notifications.NewOutcomeLedger(dataDir).StartPublisher(time.Minute, notifications.NewPublisher(nil), nil)
	server.StartDiscovery()
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
	server.StopDiscovery()
	if quickTunnel != nil {
		quickTunnel.Close()
	}
	for _, forward := range sshForwards {
		forward.Close()
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
