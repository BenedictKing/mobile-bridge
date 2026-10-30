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
	"crypto/tls"
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
	"github.com/BenedictKing/mobile-bridge/internal/accpool"
	"github.com/BenedictKing/mobile-bridge/internal/acpagents"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/claude"
	"github.com/BenedictKing/mobile-bridge/internal/codebuddy"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/conventions"
	"github.com/BenedictKing/mobile-bridge/internal/dsh"
	"github.com/BenedictKing/mobile-bridge/internal/goal"
	"github.com/BenedictKing/mobile-bridge/internal/httpd"
	"github.com/BenedictKing/mobile-bridge/internal/jev"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/kimi"
	"github.com/BenedictKing/mobile-bridge/internal/lifecycle"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
	"github.com/BenedictKing/mobile-bridge/internal/store"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
	"github.com/BenedictKing/mobile-bridge/internal/team"
	"github.com/BenedictKing/mobile-bridge/internal/tunnel"
	"github.com/BenedictKing/mobile-bridge/internal/uploads"
	"github.com/BenedictKing/mobile-bridge/internal/zcode"
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

type directSettings struct {
	publicOrigin string
	directOrigin string
	port         int
	certificate  tls.Certificate
}

func directOrigin(value string) (string, int, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", 0, errors.New("必须是有效的 HTTPS origin")
	}
	port := 443
	if value := parsed.Port(); value != "" {
		port, err = strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return "", 0, errors.New("端口无效")
		}
	}
	return "https://" + parsed.Host, port, nil
}

func loadDirectSettings(dataDir string, preferences *jsoncompat.Object) (*directSettings, error) {
	value, exists := preferences.Get("direct")
	if !exists || value == nil {
		return nil, nil
	}
	root, ok := value.(*jsoncompat.Object)
	if !ok {
		return nil, errors.New("direct 必须是对象")
	}
	publicValue, _ := root.MustGet("publicUrl").(string)
	directValue, _ := root.MustGet("url").(string)
	publicOrigin, _, err := directOrigin(publicValue)
	if err != nil {
		return nil, fmt.Errorf("direct.publicUrl: %w", err)
	}
	directOriginValue, port, err := directOrigin(directValue)
	if err != nil {
		return nil, fmt.Errorf("direct.url: %w", err)
	}
	if publicOrigin == directOriginValue {
		return nil, errors.New("direct.url 必须与 publicUrl 不同")
	}
	certFile, _ := root.MustGet("certFile").(string)
	keyFile, _ := root.MustGet("keyFile").(string)
	if certFile == "" || keyFile == "" {
		return nil, errors.New("direct.certFile 与 direct.keyFile 不能为空")
	}
	if !filepath.IsAbs(certFile) {
		certFile = filepath.Join(dataDir, certFile)
	}
	if !filepath.IsAbs(keyFile) {
		keyFile = filepath.Join(dataDir, keyFile)
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载直连证书: %w", err)
	}
	return &directSettings{publicOrigin: publicOrigin, directOrigin: directOriginValue, port: port, certificate: certificate}, nil
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
				for _, key := range []string{"autoStart", "port", "lan", "lanAddresses", "localAccess", "tunnel", "cloudflared", "codexHome", "ipcPath", "codexBin", "connections", "direct"} {
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
	preferences := desktopPreferences(dataDir)
	addresses := gatewayListenAddresses(dataDir)
	listeners := make([]net.Listener, 0, len(addresses)*2)
	if gatewayDynamicLAN(dataDir) {
		listener, listenErr := net.Listen("tcp", ":"+strconv.Itoa(port))
		if listenErr != nil {
			log.Fatalf("listen :%d: %v", port, listenErr)
		}
		listeners = append(listeners, listener)
		loopback := false
		for _, address := range addresses {
			server.AddListenHost(address, address == "127.0.0.1" || address == "::1" || address == "localhost")
			loopback = loopback || address == "127.0.0.1" || address == "::1" || address == "localhost"
		}
		if !loopback {
			for _, address := range []string{"127.0.0.1", "localhost", "::1"} {
				hostPort := net.JoinHostPort(address, strconv.Itoa(port))
				delete(server.Hosts, hostPort)
				delete(server.LocalHosts, hostPort)
				delete(server.Origins, "http://"+hostPort)
			}
		}
		server.AllowHost = func(host string) bool { return gatewayCurrentHost(dataDir, port, host) }
		server.AllowOrigin = func(origin string) bool {
			return strings.HasPrefix(origin, "http://") && gatewayCurrentHost(dataDir, port, strings.TrimPrefix(origin, "http://"))
		}
	} else {
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
	}
	server.NativeCandidates = func() []string { return gatewayNativeCandidates(dataDir, port) }
	direct, directErr := loadDirectSettings(dataDir, preferences)
	if directErr != nil {
		log.Printf("局域网直连已禁用: %v", directErr)
	}
	if direct != nil {
		directListeners := make([]net.Listener, 0, len(addresses))
		for _, address := range addresses {
			listener, listenErr := net.Listen("tcp", net.JoinHostPort(address, strconv.Itoa(direct.port)))
			if listenErr != nil {
				directErr = listenErr
				break
			}
			directListeners = append(directListeners, listener)
		}
		if directErr != nil {
			for _, opened := range directListeners {
				_ = opened.Close()
			}
			log.Printf("局域网直连已禁用: %v", directErr)
		} else if err := server.AddDirect(direct.publicOrigin, direct.directOrigin); err != nil {
			for _, opened := range directListeners {
				_ = opened.Close()
			}
			log.Printf("局域网直连已禁用: %v", err)
		} else {
			tlsConfig := &tls.Config{Certificates: []tls.Certificate{direct.certificate}, MinVersion: tls.VersionTLS12}
			for _, listener := range directListeners {
				listeners = append(listeners, tls.NewListener(listener, tlsConfig))
			}
			log.Printf("局域网直连: %s", direct.directOrigin)
		}
	}
	server.Uploads = uploads.New(dataDir)
	server.NotificationDir = dataDir
	server.Jev = jev.New(filepath.Join(dataDir, "jev.json"))
	server.Conventions = conventions.New(filepath.Join(dataDir, "conventions.json"))
	var quickTunnel *tunnel.Quick
	sshForwards := []*tunnel.SSHForward{}
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
		if server.Jev != nil {
			server.Codex.AutoApprove = jevApprovalHook(server.Jev, "codex")
		}
	}
	server.Account = account.NewWithData(codexHome, dataDir, os.Getenv("GATEWAY_CODEX_BIN"))
	// ACP account pools (kimi first): snapshots imported from zcode-switcher,
	// quota polled in the background for whichever account occupies the
	// official slot. GATEWAY_ZCAS_DIR points at the zcode-switcher data dir.
	server.Pool = accpool.New(dataDir, os.Getenv("GATEWAY_ZCAS_DIR"))
	poolAuto := accpool.NewAutoSwitcher(server.Pool)
	poolAuto.Notify = func(title, body string) {
		config := notifications.ReadSettings(dataDir)
		publisher := notifications.NewPublisher(nil)
		for _, channel := range notifications.Channels(config).Keys() {
			if err := publisher.Publish(config, channel, title, body, ""); err != nil {
				log.Printf("账号池通知发送失败（%s）: %v", channel, err)
			}
		}
	}
	poolAuto.Sessions = func(kind string) accpool.SessionManager {
		if manager := server.ACPAgents[accpool.AgentKind(kind)]; manager != nil {
			return manager
		}
		return nil
	}
	poolPoller := accpool.NewPoller(server.Pool)
	poolPoller.Threshold = poolAuto.Threshold
	poolPoller.OnExhausted = func(kind string) {
		if _, err := poolAuto.HandleQuotaExhaustion(context.Background(), kind, ""); err != nil {
			log.Printf("账号池: %s 预防性切换失败: %v", kind, err)
		}
	}
	poolPoller.OnActiveChange = func(kind, fromID, toID string) {
		// An external tool (zcode-switcher) moved the slot. The kimi CLI
		// processes behind open phone sessions cached the previous account's
		// OAuth state and would fail with "Stored token rejected"; reload the
		// idle ones so they pick up the new credentials.
		log.Printf("账号池: %s 槽位账号变化 %s -> %s", kind, fromID, toID)
		if kind != "kimi" {
			return
		}
		manager := server.ACPAgents[accpool.AgentKind(kind)]
		if manager == nil {
			return
		}
		for id, cwd := range manager.IdleSessions() {
			if err := manager.Reattach(id, cwd, 30*time.Second); err != nil {
				log.Printf("账号池: kimi 槽位切换后重挂会话 %s 失败: %v", id, err)
			}
		}
	}
	poolPoller.Start()
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
	home := ""
	if userHome, homeErr := os.UserHomeDir(); homeErr == nil {
		home = userHome
	}
	profiles := acpagents.Discover(os.Getenv, home)
	profileByKind := map[string]*acpagents.Profile{}
	for _, profile := range profiles {
		profileByKind[profile.Kind] = &profile
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
			var manager *acpruntime.Manager
			if argv := acpagents.SplitArgv(executable); len(argv) > 1 {
				manager = acpruntime.NewCommand(argv)
			} else {
				manager = acpruntime.New(executable)
			}
			// Env managers keep the env's launch command but inherit the
			// kind's dialect constants (effort ladder, steer RPC, mode
			// mapping) — without them an env-configured dsh still refuses
			// every send and zcode/opencode lose their mode ids.
			if profile := profileByKind[kind]; profile != nil {
				manager.Label = profile.Label
				manager.Capabilities = profile.Capabilities
				manager.EffortMap = profile.EffortMap
				manager.InitializeTimeout = profile.InitializeTimeout
				manager.SteerMethod = profile.SteerMethod
				manager.ModeMap = profile.ModeMap
				manager.ModeOptionID = profile.ModeOptionID
				manager.LazySessions = profile.LazySessions
			}
			server.ACPAgents[kind] = manager
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
	autoKinds := map[string]bool{}
	if agentKinds == nil {
		for _, profile := range profiles {
			autoKinds[profile.Kind] = true
		}
	} else {
		for _, kind := range agentKinds {
			autoKinds[kind] = true
		}
	}
	enabled := []string{"codex"}
	for _, profile := range profiles {
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
		manager.ModeOptionID = profile.ModeOptionID
		manager.Canonical = profile.Canonical
		manager.LazySessions = profile.LazySessions
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
	// ZCode's ACP wrapper only streams turns it runs itself. The desktop app
	// persists every turn to a shared SQLite store; following it keeps the
	// phone's live view in step with desktop-driven work. Scripted parity
	// mode never touches the machine.
	// DSH sessions the desktop app owns have no title in dsh-acp's list and a
	// creation-time updatedAt; the app's projection cache and transcript
	// mtime carry the truth. Read-only, never touches the app.
	if manager := server.ACPAgents["dsh"]; manager != nil && !scriptedMode {
		dshHome := acpagents.DshHome(os.Getenv, home)
		reader := dsh.NewTranscriptReader(dshHome)
		manager.NativeMeta = dsh.MetaReader(dshHome)
		// Follow the transcript even when an ACP handle opened successfully:
		// the desktop app can keep driving the same session outside that
		// handle. A session the app owns outright falls back to the same
		// reader through NativeTranscript (read-only view).
		manager.Native = reader.Load
		manager.NativeReset = reader.Reset
		manager.NativeTranscript = reader.Load
		manager.NativeInterval = 1500 * time.Millisecond
		manager.NativeSettle = func(session *acpruntime.Session) {
			server.QueueSettleACP("dsh", session)
		}
	}
	if manager := server.ACPAgents["zcode"]; manager != nil && !scriptedMode {
		if source, err := zcode.OpenSource(acpagents.ZcodeDBPath(os.Getenv, home)); err == nil {
			source.SetMediaDir(filepath.Join(dataDir, "agent-media"))
			manager.Native = source.Load
			manager.NativeReset = source.Reset
			manager.NativeClose = source.Close
			manager.NativeMeta = source.Meta
			manager.NativeSettle = func(session *acpruntime.Session) {
				server.QueueSettleACP("zcode", session)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			log.Printf("zcode native store: %v", err)
		}
	}
	if manager := server.ACPAgents["kimi"]; manager != nil && !scriptedMode {
		kimiHome := os.Getenv("KIMI_CODE_HOME")
		if kimiHome == "" {
			kimiHome = filepath.Join(home, ".kimi-code")
		}
		manager.NativeList = func() []*jsoncompat.Object {
			return kimi.ListSessions(kimiHome)
		}
		retries := kimi.NewRetrySource(kimiHome)
		manager.RetryStatus = retries.Status
	}
	if manager := server.ACPAgents["workbuddy"]; manager != nil && !scriptedMode {
		// codebuddy --acp implements session/load but not session/list
		// (-32601), so the CLI's own transcript store is the list source.
		manager.NativeList = func() []*jsoncompat.Object {
			return codebuddy.ListSessions(home)
		}
	}
	if manager := server.ACPAgents["claude"]; manager != nil && !scriptedMode {
		// claude-agent-acp's session/list reports each transcript's mtime as
		// updatedAt, and resuming a session (the startup preload does, for
		// every recent chat) appends timestamp-less cost-state records, so a
		// restart restamped the preloaded chats as just-used. The transcript's
		// last timestamped record is the real last interaction. Read-only,
		// never touches the CLI.
		manager.NativeMeta = claude.MetaReader(claude.ProjectsRoot(os.Getenv, home))
	}
	if len(server.ACPAgents) > 0 {
		// Per-agent ledgers shared by every manager of the same kind. Agents
		// only report usage inside a live turn and only list a session after
		// its first turn, so these files are what keep a reopened chat's gauge
		// populated and a created-but-unused chat resolvable after a restart.
		contextUsage := acpruntime.LoadUsage(filepath.Join(dataDir, "context-usage.json"))
		createdSessions := acpruntime.LoadCreated(filepath.Join(dataDir, "created-sessions.json"))
		for kind, manager := range server.ACPAgents {
			if manager != nil {
				manager.Usage = contextUsage
				manager.Created = createdSessions
				manager.Kind = kind
			}
			if manager != nil && server.Jev != nil {
				manager.AutoApprove = jevApprovalHook(server.Jev, kind)
				manager.AutoContinue = jevContinueHook(server.Jev, kind)
			}
			if manager != nil {
				// Queued sends dispatch the moment a turn settles: chain the
				// queue hook onto whatever auto-continue hook is installed.
				prior := manager.AutoContinue
				managedKind := kind
				quotaHook := poolQuotaHook(server.Pool, poolAuto, managedKind, manager)
				manager.AutoContinue = func(session *acpruntime.Session) {
					// Quota failover owns the settle edge when it fired: the
					// resumed turn's next settle re-enters the chain.
					if quotaHook(session) {
						return
					}
					if prior != nil {
						prior(session)
					}
					server.QueueSettleACP(managedKind, session)
				}
				// Inline image blocks stream as base64; persisting them under the
				// data dir gives the phone a previewable path per session.
				manager.MediaDir = filepath.Join(dataDir, "agent-media")
			}
		}
		sort.Strings(enabled)
		log.Printf("agents enabled: %s", strings.Join(enabled, ", "))
		// Idle ACP sessions hold one child process each; sweep them like the
		// Python maintain loop's 300s eviction once nobody is reading them.
		// （§12 阶段四：Server 化补停止通道，关停序列可优雅退出。）
		server.StartIdleSweeper()
	}
	server.Creations = httpd.LoadCreations(filepath.Join(dataDir, "creations.json"))
	// Durable feature ledgers: queued sends, hidden (deleted) sessions and
	// fork origins survive restarts like Python's submissions/actions files.
	server.Queue = httpd.LoadSubmissions(filepath.Join(dataDir, "submissions.json"))
	server.GoalCmds = httpd.NewGoalLedger(filepath.Join(dataDir, "goal-commands.json"))
	server.Hidden = httpd.LoadHiddenSessions(filepath.Join(dataDir, "hidden-sessions.json"))
	server.Forks = httpd.LoadForks(filepath.Join(dataDir, "forks.json"))
	// Team 编排（docs/team-orchestration-plan.md §8.3 启动顺序）：账本与
	// journal 先装配，事件投影从 journal 预热后再对账恢复；恢复期产生的
	// journal 事件经 OnEvent 继续进入投影。全部注入在任何 ServeHTTP 之前
	//（Server 字段构造后直赋、无同步）。
	teamJournal, teamJournalErr := team.OpenJournal(filepath.Join(dataDir, "team-journal.jsonl"))
	if teamJournalErr != nil {
		log.Fatalf("team journal: %v", teamJournalErr)
	}
	teamStore := team.LoadStore(filepath.Join(dataDir, "teams.json"), filepath.Join(dataDir, "team-runs.json"))
	if teamStore.RunsLedgerCorrupt() {
		// §8.1：team-runs.json 只是可重建的查询快照，journal 才是事实
		// 来源。损坏原件已在 LoadStore 内改名留存，此处按 journal 事实
		// 重建可恢复的 Run 记录。
		if events, replayErr := teamJournal.Replay(); replayErr != nil {
			log.Printf("team 运行账本损坏且 journal 重放失败，Run 记录按空账本启动: %v", replayErr)
		} else {
			teamStore.RebuildRunsFromJournal(events)
		}
	}
	server.TeamStore = teamStore
	// TeamJournal 供 /stats 端点全量重放 journal（§12 阶段四：内存投影每
	// Run 只留 500 条，长 Run 的早期事件只在磁盘）；与 TeamStore 同源装配。
	server.TeamJournal = teamJournal
	server.TeamFeeds = httpd.NewTeamFeedRegistry(teamJournal, teamStore)
	teamQueue, teamResolver := httpd.NewTeamAdapters(server)
	server.Teams = team.NewService(teamStore, teamJournal, teamQueue, teamResolver)
	server.Teams.Transcripts = httpd.NewTeamTranscriptSource(server)
	// worktree 注册表（§6.4 + §16 默认决策 6）：注册/列表端点与 Service 的
	// Resolve/HeadRevision/Diff/Cleanup 共用同一实例（一份注册表文件）。
	teamWorkspaces := httpd.NewTeamWorkspaceRegistry(filepath.Join(dataDir, "team-workspaces.json"))
	server.TeamWorkspaceRegistry = teamWorkspaces
	server.Teams.Workspaces = teamWorkspaces
	server.Teams.OnEvent = server.TeamFeeds.Ingest
	// Run 通知（编排计划 §12 阶段四工作项 3/5，§6.3/§9）：完成/失败/取消/
	// 停止/waiting_input 事件经 ntfy/Bark 推送（click deep link），stall 只
	// 通知不停止。OnRunStatus 在 run 状态事件落账点同步回调（notify.go 的
	// commitRun 收口）——先装配再 RecoverAll：恢复期对账产生的 waiting_input
	// 迁移（recovery.go）同样入账，重启补发由下方水位扫描兜底。
	runNotifyLedger := notifications.NewRunNotificationLedger(dataDir)
	server.Teams.OnRunStatus = func(runID, kind string) {
		recordTeamRunNotification(runNotifyLedger, teamStore, runID, kind, time.Now())
	}
	if err := server.TeamFeeds.PrimeFromJournal(); err != nil {
		log.Printf("team 事件投影预热失败: %v", err)
	}
	if err := server.Teams.RecoverAll(); err != nil {
		log.Printf("team 运行恢复失败: %v", err)
	}
	// 启动补发扫描（评审缺失项④：水位定案）：优雅关停时账本记下 watermark，
	// 重启后只考虑 UpdatedAt≥watermark−5min 的 Run（停机期间完结的 Run 覆盖
	// 在内）；送达痕迹（7 天内）仍在的跳过——两道合并后扫描集合有界
	// （team-runs.json 不裁剪，无水位会随历史 Run 无界轰炸，评审问题⑤）。
	// 首启（从未优雅关停、水位为零）不回扫历史。stall 不参与补发（运行期
	// 事实，补发只会轰炸）。
	backfillTeamRunNotifications(runNotifyLedger, teamStore, time.Now())
	// 补发 worker（仿 outcomeWorker 的 StartPublisher 形态）：每分钟对未达
	// 通知重试，带每键指数退避（stall 类下限 20 分钟）；Record 即时唤醒，
	// 首次尝试不必等整周期。启动即 reconcile 一次，上方扫描补记的行随首轮
	// 立即发出。
	runNotifyWorker := runNotifyLedger.StartWorker(time.Minute, notifications.NewPublisher(nil))
	// 定时触发（编排计划 §12 阶段四工作项 1）：team-schedules.json 账本 +
	// 按 tick 扫描的触发器。装配在 RecoverAll 之后——先让既有 Run 回到可
	// 确认状态，到期判定才不会撞上恢复前的假非终态；启动首轮 sweep 兼作
	// PendingTriggerID 的 journal 回扫对账（崩溃/重启不重复触发）。CreateRun
	// 恒传新 runID（每次触发都是全新 Run）、autoPlan=false 与
	// scheduledFrom=triggerID（打标可追溯，触发幂等由 journal 裁决）。
	teamSchedules := team.LoadScheduleStore(filepath.Join(dataDir, "team-schedules.json"))
	// TeamSchedules 供 /api/teams/:id/schedule 配置端点读写（§12 阶段四
	// 工作项 1 的 HTTP 配置面）；与触发器共用同一份账本——单实例、单事实
	// 来源，HTTP 写与调度器读天然互见（账本自身的锁串行化）。
	server.TeamSchedules = teamSchedules
	teamScheduler := team.NewScheduler(teamSchedules, teamStore, teamJournal,
		func(scheduledTeam *team.Team, goal, mode, collect string, verifyCommand []string, plan []team.PlanTaskInput, scheduledFrom string) (*team.Run, error) {
			return server.Teams.CreateRun(scheduledTeam, "", goal, mode, collect, verifyCommand, plan, false, scheduledFrom)
		})
	teamScheduler.Start()
	// 运行中状态刷新（编排计划 §12 阶段四工作项 2）：周期对全部 waiting_input
	// Run 依次 RefreshRun——收养在途任务的 watcher（重启后重新有人观察）+
	// outcome_unknown 任务只读重探（探到确定事实才单向纠正，绝不重发、不改
	// run 状态）。装配在 RecoverAll 之后；间隔默认 60s，env
	// GATEWAY_TEAM_RECONCILE_SECONDS 可调（正整数秒）。
	server.Teams.StartReconciler(teamReconcileIntervalFromEnv(os.Getenv))
	server.StartQueueWorker()
	controller, controlErr := lifecycle.NewController(dataDir, os.Getpid(), instanceID)
	if controlErr != nil {
		log.Fatalf("control file: %v", controlErr)
	}
	outcomeWorker := notifications.NewOutcomeLedger(dataDir).StartPublisher(time.Minute, notifications.NewPublisher(nil), nil)
	server.StartDiscovery()
	go server.PreloadRecentSessions()
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
	// 后台循环按序优雅退出（§12 阶段四：此前 queue worker 与 idle sweeper 是
	// 裸 ticker goroutine，进程关停只能靠整体退出）：HTTP 已停 → 队列 worker
	// → 空闲 sweeper → team 对账 reconciler → 定时触发 → 会话发现；其余
	// 收尾随后。
	server.StopQueueWorker()
	server.StopIdleSweeper()
	server.Teams.StopReconciler()
	// 定时触发器优雅退出（ctx 取消 + 等待循环返回，仿 accpool Poller）：
	// sweep 中断处即停，进行中的触发留标记，重启后按 journal 回扫收尾。
	teamScheduler.Stop()
	server.StopDiscovery()
	outcomeWorker.Close()
	// Run 通知收尾：worker 先优雅退出，再把水位盖到关停时刻——重启后的
	// 补发扫描只看水位窗口（覆盖停机期间完结的 Run）。崩溃无优雅关停时
	// 水位陈旧、扫描窗口变宽，由送达痕迹（7 天）兜底去重。
	runNotifyWorker.Close()
	if err := runNotifyLedger.SetWatermark(time.Now()); err != nil {
		log.Printf("team run 通知水位更新失败: %v", err)
	}
	poolPoller.Stop()
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

func gatewayDynamicLAN(dataDir string) bool {
	if raw, err := os.ReadFile(filepath.Join(dataDir, "desktop.json")); err == nil {
		if value, decodeErr := jsoncompat.Unmarshal(raw); decodeErr == nil {
			if root, ok := value.(*jsoncompat.Object); ok {
				if lan, ok := root.MustGet("lan").(bool); ok && !lan {
					return false
				}
				if values, ok := root.MustGet("lanAddresses").(*jsoncompat.Array); ok && len(values.Items) > 0 {
					return false
				}
			}
		}
	}
	return true
}

func gatewayCurrentHost(dataDir string, port int, host string) bool {
	address, portValue, err := net.SplitHostPort(host)
	if err != nil || portValue != strconv.Itoa(port) {
		return false
	}
	for _, current := range gatewayListenAddresses(dataDir) {
		if address == current {
			return true
		}
	}
	return false
}

func gatewayNativeCandidates(dataDir string, port int) []string {
	candidates := []string{}
	for _, address := range gatewayListenAddresses(dataDir) {
		ip := net.ParseIP(address)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		candidates = append(candidates, "http://"+net.JoinHostPort(address, strconv.Itoa(port)))
	}
	return candidates
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

// teamReconcileIntervalFromEnv 解析 team 对账刷新循环的间隔（编排计划 §12
// 阶段四工作项 2）：env GATEWAY_TEAM_RECONCILE_SECONDS 取正整数秒，未设置/
// 非法/非正一律回 team.DefaultReconcileInterval（默认 60s；非法值不打断启动，
// 回默认即可观察可修）。
func teamReconcileIntervalFromEnv(getenv func(string) string) time.Duration {
	if raw := getenv("GATEWAY_TEAM_RECONCILE_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return team.DefaultReconcileInterval
}

// recordTeamRunNotification 是 OnRunStatus 回调的主体（§12 阶段四工作项
// 3/5）：从 store 补读 Run（事件落账点之后，快照已是终局形态），经
// team.RunNotificationText 组装标题（goal 截 60 rune+任务计数+结果摘要）并
// 写入 Run 通知账本——键=run+kind 幂等去重，实际发送与退避在账本 worker。
// 回调在 Run 的 state.mu 下同步执行，只做读 store+写账本；Run 已被删除
// （随 Team 级联）时静默跳过。
func recordTeamRunNotification(ledger *notifications.RunNotificationLedger, store *team.Store, runID, kind string, at time.Time) {
	run := store.Run(runID)
	if run == nil {
		return
	}
	title, body := team.RunNotificationText(run, kind)
	if title == "" {
		return
	}
	if err := ledger.Record(runID, run.TeamID, kind, title, body, at); err != nil {
		log.Printf("team run %s 通知记账失败（%s，下轮补发扫描兜底）: %v", runID, kind, err)
	}
}

// backfillTeamRunNotifications 是水位制启动补发扫描（评审缺失项④）：把
// team-runs.json 快照里 UpdatedAt 落在水位窗口（≥watermark−5min）内的终态/
// waiting_input Run 投影成补发候选（文案组装复用 RunNotificationText），
// 交给 ledger.Backfill 裁决——送达痕迹仍在的跳过、首启无水位不回扫。
// PrimeFromJournal 只喂 TeamFeed 不重放 OnEvent，运行期挂点对停机期间完结
// 的 Run 无能为力，靠本扫描自建「未通知」索引（勘察结论）。
func backfillTeamRunNotifications(ledger *notifications.RunNotificationLedger, store *team.Store, now time.Time) {
	candidates := []notifications.RunBackfillCandidate{}
	for _, run := range store.Runs() {
		kind := team.RunNotificationKindOfStatus(run.Status)
		if kind == "" {
			continue
		}
		title, body := team.RunNotificationText(run, kind)
		if title == "" {
			continue
		}
		candidates = append(candidates, notifications.RunBackfillCandidate{
			RunID:     run.ID,
			TeamID:    run.TeamID,
			Kind:      kind,
			UpdatedAt: run.UpdatedAt,
			Title:     title,
			Body:      body,
		})
	}
	if recorded := ledger.Backfill(now, notifications.RunBackfillMargin, candidates); recorded > 0 {
		log.Printf("team run 通知补发扫描：%d 条待发通知已入账", recorded)
	}
}
