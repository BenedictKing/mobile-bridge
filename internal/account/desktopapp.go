// Desktop App lifecycle for account switching (bridge/desktop_app.py's
// DesktopApp port). Only the selected GUI executable is ever quit or relaunched;
// the bundled Codex CLI runtime this gateway drives over stdio stays untouched.
// Every real process operation goes through ProcessController so tests can
// fake the desktop App without spawning or killing anything.
package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/transport"
)

// quitTimeout mirrors desktop_app.py's stop deadline: the GUI gets 25 seconds
// to exit after the gentle quit; the gateway never force-kills it. A variable
// so the switch tests can shrink the wait.
var quitTimeout = 25 * time.Second

// quitAskTimeout bounds the osascript "tell application to quit" call itself.
const quitAskTimeout = 25 * time.Second

// ProcessController abstracts every desktop App process side effect. The
// production implementation is osProcesses; tests inject a fake.
type ProcessController interface {
	// Processes lists the pids whose executable image equals the path.
	Processes(executable string) ([]int, error)
	// QuitGUI asks the macOS application bundle to quit gracefully
	// (osascript "tell application ... to quit").
	QuitGUI(bundlePath string, timeout time.Duration) error
	// CloseMainWindow asks one Windows GUI process to close (WM_CLOSE).
	CloseMainWindow(pid int) error
	// Terminate sends SIGTERM to one process (Linux).
	Terminate(pid int) error
	// Launch starts the desktop App detached, with CODEX_HOME set.
	Launch(executable, home string) error
	// DialIPC probes the desktop App's local IPC endpoint; an error means
	// no live desktop instance is accepting connections.
	DialIPC(endpoint string, timeout time.Duration) error
}

// DesktopApp points at one validated GUI executable and the CODEX_HOME it
// must be (re)launched with.
type DesktopApp struct {
	Executable string
	Home       string
	proc       ProcessController
}

func newDesktopApp(executable, home string, proc ProcessController) *DesktopApp {
	if proc == nil {
		proc = osProcesses{}
	}
	return &DesktopApp{Executable: executable, Home: home, proc: proc}
}

// DiscoverDesktopApp mirrors DesktopApp.discover: on macOS the runtime binary
// usually lives inside the Codex.app bundle, so walk its ancestors for the
// bundle and read CFBundleExecutable from Info.plist; on Windows/Linux fall
// back to well-known GUI names in the runtime's first four ancestors.
func DiscoverDesktopApp(runtimePath string) string {
	if runtimePath == "" {
		return ""
	}
	resolved, err := filepath.Abs(runtimePath)
	if err == nil {
		if link, linkErr := filepath.EvalSymlinks(resolved); linkErr == nil {
			resolved = link
		}
	}
	parents := ancestors(resolved)
	for _, parent := range parents {
		if filepath.Ext(parent) != ".app" {
			continue
		}
		name := bundleExecutable(parent)
		if name == "" {
			return ""
		}
		candidate := filepath.Join(parent, "Contents", "MacOS", name)
		if isRegularFile(candidate) {
			return candidate
		}
		return ""
	}
	if runtime.GOOS == "darwin" {
		return ""
	}
	names := []string{"Codex.exe", "ChatGPT.exe"}
	if runtime.GOOS == "linux" {
		names = []string{"chatgpt", "ChatGPT", "codex-desktop", "codex"}
	}
	for index, parent := range parents {
		if index >= 4 {
			break
		}
		for _, name := range names {
			candidate := filepath.Join(parent, name)
			if !isRegularFile(candidate) {
				continue
			}
			// Never hand back the runtime binary itself (Python's
			// `p.resolve() != runtime` guard).
			if samePath(candidate, runtimePath) {
				continue
			}
			return candidate
		}
	}
	return ""
}

// Validate mirrors DesktopApp.validate: an installed, executable, non-script
// binary that is not the gateway's own CLI runtime.
func (d *DesktopApp) Validate(runtimePath string) error {
	info, err := os.Stat(d.Executable)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0) {
		return errors.New("请选择已安装的 Codex / ChatGPT 桌面程序")
	}
	if samePath(d.Executable, runtimePath) {
		return errors.New("桌面程序不能选择内置 Codex 命令行运行时")
	}
	file, err := os.Open(d.Executable)
	if err == nil {
		header := make([]byte, 2)
		read, _ := file.Read(header)
		_ = file.Close()
		if read == 2 && header[0] == '#' && header[1] == '!' {
			// Launch scripts cannot be matched reliably to the resulting GUI process.
			return errors.New("请选择桌面程序的实际可执行文件，而不是启动脚本")
		}
	}
	return nil
}

// Processes lists the live pids of this desktop executable.
func (d *DesktopApp) Processes() ([]int, error) { return d.proc.Processes(d.Executable) }

// Stop mirrors DesktopApp.stop: a gentle quit (osascript on macOS,
// CloseMainWindow on Windows, SIGTERM on Linux) followed by a bounded wait;
// the process is never force-killed.
func (d *DesktopApp) Stop() error {
	pids, err := d.Processes()
	if err != nil {
		return err
	}
	if len(pids) > 0 && runtime.GOOS == "darwin" {
		bundle := ""
		for _, parent := range ancestors(d.Executable) {
			if filepath.Ext(parent) == ".app" {
				bundle = parent
				break
			}
		}
		if bundle == "" {
			return errors.New("macOS 桌面程序必须位于应用程序包中")
		}
		if err := d.proc.QuitGUI(bundle, quitAskTimeout); err != nil {
			return err
		}
	}
	for _, pid := range pids {
		if runtime.GOOS == "darwin" {
			// The AppleScript quit already targeted every bundle process.
			continue
		}
		still, err := d.proc.Processes(d.Executable)
		if err != nil || !containsInt(still, pid) {
			continue
		}
		if runtime.GOOS == "windows" {
			if err := d.proc.CloseMainWindow(pid); err != nil {
				return err
			}
		} else if err := d.proc.Terminate(pid); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(quitTimeout)
	for {
		live, err := d.Processes()
		if err != nil {
			return err
		}
		if len(live) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("桌面程序尚未退出，请在电脑上关闭后重试；尚未强制结束进程")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Start mirrors DesktopApp.start: a detached GUI process with CODEX_HOME
// pinned to the account home.
func (d *DesktopApp) Start() error { return d.proc.Launch(d.Executable, d.Home) }

// WaitStopped mirrors Accounts.wait_stopped: the desktop IPC endpoint must
// stop accepting connections (the GUI is really gone) before files change.
func (d *DesktopApp) WaitStopped(endpoint string, timeout time.Duration) error {
	if endpoint == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := d.proc.DialIPC(endpoint, 500*time.Millisecond); err != nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("仍有桌面实例占用连接，请在电脑端关闭后重试")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// WaitReady mirrors Accounts.wait_ready: processes must be alive, the IPC
// endpoint reachable, and verify (a config/account RPC probe) must pass
// before the deadline.
func (d *DesktopApp) WaitReady(endpoint string, verify func() error, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		pids, err := d.Processes()
		if err != nil {
			return err
		}
		if len(pids) > 0 {
			reachable := true
			if endpoint != "" {
				reachable = d.proc.DialIPC(endpoint, 500*time.Millisecond) == nil
			}
			if reachable {
				if verify == nil {
					return nil
				}
				if verifyErr := verify(); verifyErr == nil {
					return nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return errors.New("未能确认桌面连接和目标账号，请检查电脑端")
		}
		time.Sleep(poll)
	}
}

// osProcesses is the production controller: real ps//proc/powershell probes,
// osascript quits and detached launches.
type osProcesses struct{}

func (osProcesses) Processes(executable string) ([]int, error) {
	switch runtime.GOOS {
	case "windows":
		return windowsProcesses(executable)
	case "linux":
		return linuxProcesses(executable)
	default:
		return darwinProcesses(executable)
	}
}

func (osProcesses) QuitGUI(bundlePath string, timeout time.Duration) error {
	script := "on run argv\n tell application (item 1 of argv) to quit\nend run"
	command := exec.Command("osascript", "-e", script, bundlePath)
	command.Stdout = nil
	command.Stderr = nil
	done := make(chan error, 1)
	if err := command.Start(); err != nil {
		return fmt.Errorf("无法发送退出指令，请检查系统权限：%s", err.Error())
	}
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New("桌面程序未能响应退出指令，请在电脑端手动关闭后重试")
		}
		return nil
	case <-time.After(timeout):
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return errors.New("桌面程序未能响应退出指令，请在电脑端手动关闭后重试")
	}
}

func (osProcesses) CloseMainWindow(pid int) error {
	script := "$p=Get-Process -Id ([int]$env:CMB_GUI_PID) -ErrorAction Stop; if (-not $p.CloseMainWindow()) {exit 2}"
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "CMB_GUI_PID="+strconv.Itoa(pid))
	command.Stdout = nil
	command.Stderr = nil
	if err := command.Start(); err != nil {
		return errors.New("桌面程序未能响应关闭请求，请在电脑端手动关闭后重试")
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return errors.New("桌面程序未能响应关闭请求，请在电脑端手动关闭后重试")
		}
		return nil
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
		return errors.New("桌面程序未能响应关闭请求，请在电脑端手动关闭后重试")
	}
}

func (osProcesses) Terminate(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil // already gone
	}
	return process.Signal(syscallSIGTERM)
}

func (osProcesses) Launch(executable, home string) error {
	command := exec.Command(executable)
	command.Dir = home
	command.Env = append(os.Environ(), "CODEX_HOME="+home)
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil
	if runtime.GOOS != "windows" {
		command.SysProcAttr = sysProcAttrNewSession()
	}
	if err := command.Start(); err != nil {
		return errors.New("无法启动 Codex 桌面程序，请检查电脑端")
	}
	// Reap the child whenever it exits so a quit does not leave a zombie.
	go func() { _, _ = command.Process.Wait() }()
	return nil
}

func (osProcesses) DialIPC(endpoint string, timeout time.Duration) error {
	conn, err := transport.ConnectStream(endpoint, timeout)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

// darwinProcesses mirrors the `ps -u uid -o pid=,comm=` probe.
func darwinProcesses(executable string) ([]int, error) {
	output, err := exec.Command("ps", "-u", strconv.Itoa(os.Getuid()), "-o", "pid=,comm=").Output()
	if err != nil {
		return nil, errors.New("无法确认桌面程序进程，请检查电脑端")
	}
	target := resolvePath(executable)
	result := []int{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) != 2 {
			continue
		}
		pid, convErr := strconv.Atoi(fields[0])
		if convErr != nil {
			continue
		}
		if resolvePath(fields[1]) == target {
			result = append(result, pid)
		}
	}
	return result, nil
}

// linuxProcesses mirrors the /proc/<pid>/exe probe, own-user only.
func linuxProcesses(executable string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, errors.New("无法确认桌面程序进程，请检查电脑端")
	}
	target := resolvePath(executable)
	result := []int{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, convErr := strconv.Atoi(entry.Name())
		if convErr != nil {
			continue
		}
		info, statErr := os.Stat("/proc/" + entry.Name())
		if statErr != nil || uidOf(info) != os.Getuid() {
			continue
		}
		link, linkErr := os.Readlink("/proc/" + entry.Name() + "/exe")
		if linkErr != nil || resolvePath(link) != target {
			continue
		}
		result = append(result, pid)
	}
	return result, nil
}

// windowsProcesses mirrors the Win32_Process ExecutablePath probe.
func windowsProcesses(executable string) ([]int, error) {
	script := ("[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new(); " +
		"Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath} | " +
		"Select-Object ProcessId,ExecutablePath | ConvertTo-Json -Compress")
	command := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	output, err := command.Output()
	if err != nil {
		return nil, errors.New("无法确认桌面程序进程，请检查电脑端")
	}
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return []int{}, nil
	}
	var rows []map[string]any
	if trimmed[0] != '[' {
		// ConvertTo-Json emits a bare object for a single match.
		single := map[string]any{}
		if json.Unmarshal(trimmed, &single) == nil && single != nil {
			rows = append(rows, single)
		}
	} else if json.Unmarshal(trimmed, &rows) != nil {
		rows = nil
	}
	target := normalizeWindowsPath(executable)
	result := []int{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		path, _ := row["ExecutablePath"].(string)
		pidNumber, _ := row["ProcessId"].(float64)
		if normalizeWindowsPath(path) != target {
			continue
		}
		result = append(result, int(pidNumber))
	}
	return result, nil
}

// --- shared helpers -------------------------------------------------------

func ancestors(path string) []string {
	cleaned := filepath.Clean(path)
	parent, _ := filepath.Split(cleaned)
	if parent == "" || parent == cleaned {
		return nil
	}
	parent = strings.TrimRight(parent, string(filepath.Separator))
	if parent == "" {
		return nil
	}
	return append(ancestors(parent), parent)
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func samePath(left, right string) bool { return resolvePath(left) == resolvePath(right) }

// normalizeWindowsPath mirrors os.path.normcase for the Windows process probe:
// case-insensitive comparison over backslash separators.
func normalizeWindowsPath(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "/", "\\"))
}

func containsInt(values []int, needle int) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// bundleExecutable reads CFBundleExecutable from an .app bundle's Info.plist.
// XML plists are scanned directly; binary plists go through plutil on macOS.
func bundleExecutable(bundle string) string {
	plist := filepath.Join(bundle, "Contents", "Info.plist")
	raw, err := os.ReadFile(plist)
	if err != nil {
		return ""
	}
	if name := plistString(raw, "CFBundleExecutable"); name != "" {
		return name
	}
	if bytes.HasPrefix(raw, []byte("bplist00")) && runtime.GOOS == "darwin" {
		out, convErr := exec.Command("plutil", "-convert", "xml1", "-o", "-", plist).Output()
		if convErr == nil {
			return plistString(out, "CFBundleExecutable")
		}
	}
	return ""
}

// plistString extracts <key>NAME</key><string>VALUE</string> from an XML
// plist (plistlib's job in Python); malformed input yields "".
func plistString(raw []byte, key string) string {
	needle := "<key>" + key + "</key>"
	index := bytes.Index(raw, []byte(needle))
	if index < 0 {
		return ""
	}
	rest := raw[index+len(needle):]
	open := bytes.Index(rest, []byte("<string>"))
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	close := bytes.Index(rest, []byte("</string>"))
	if close < 0 {
		return ""
	}
	value := string(rest[:close])
	replacer := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", "\"", "&apos;", "'", "&amp;", "&")
	return replacer.Replace(value)
}
