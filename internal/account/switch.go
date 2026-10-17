// Account switching state machine (bridge/accounts.py's switch/_apply_switch
// port): the phone picks a saved account, the gateway snapshots the Codex home,
// rewrites config/auth for the target account, quits and relaunches the desktop
// App, waits for health and rolls everything back on failure. Phases persist in
// switch.json so an interrupted switch surfaces as 'interrupted' after a crash.
package account

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/transport"
)

// rpcCallTimeout mirrors ManagedRPC's 30s per-call budget during switching.
const rpcCallTimeout = 30 * time.Second

// Blocker describes one live desktop task or uncertain submission that forbids
// a switch (Python blockers()).
type Blocker struct {
	ID     string
	Host   string
	Title  string
	Reason string
}

// ConflictError marks switch-state conflicts (another switch running or a
// pending recovery); the HTTP layer maps it to 409.
type ConflictError struct{ message string }

func (e *ConflictError) Error() string { return e.message }

// Phases between which a restart is in flight; anything here blocks chat
// operations and a stale value normalizes to 'interrupted' on load.
var switchActivePhases = map[string]bool{
	"preparing": true, "stopping": true, "applying": true,
	"starting": true, "verifying": true, "restoring": true,
}

// Phases from which a new switch may start (Python check_ready).
var switchReadyPhases = map[string]bool{
	"idle": true, "complete": true, "failed": true, "restored": true,
}

var (
	accountIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	requestIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// SetProcesses swaps the desktop App process controller (test seam; nil
// restores the real OS implementation).
func (m *Manager) SetProcesses(proc ProcessController) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.proc = proc
}

// SetSwitchTimeouts overrides the switch waits (test seam; defaults mirror
// Python's 8s stop-wait, 35s health wait and 500ms poll).
func (m *Manager) SetSwitchTimeouts(stopWait, readyWait, poll time.Duration) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.stopWait, m.readyWait, m.poll = stopWait, readyWait, poll
}

// CheckReady mirrors check_ready: chat mutations are refused while a switch is
// in flight or awaiting manual recovery.
func (m *Manager) CheckReady() error {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	if state := m.loadStateLocked(); !switchReadyPhases[phaseOf(state)] {
		return &ConflictError{"账号正在切换或等待恢复，请稍后再操作聊天"}
	}
	return nil
}

// Switch validates one switch request, records the idempotency ledger and the
// 'preparing' phase, then drives the restart on a background goroutine. The
// response carries the fresh public status either way (Python switch()).
// idle, when set, refuses to start while desktop tasks or submissions block
// the restart (Python idle()).
func (m *Manager) Switch(value *jsoncompat.Object, idle func() error) (*jsoncompat.Object, error) {
	if value == nil {
		value = jsoncompat.NewObject()
	}
	if value.MustGet("confirmed") != true || value.MustGet("tasksConfirmed") != true {
		return nil, errors.New("请确认所有桌面任务已结束，并同意重启 Codex 桌面应用")
	}
	requestID, _ := value.MustGet("requestId").(string)
	if !requestIDPattern.MatchString(requestID) {
		return nil, errors.New("切换请求标识无效")
	}
	targetID, _ := value.MustGet("id").(string)

	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	row, err := m.rowLocked(targetID)
	if err != nil {
		return nil, err
	}
	ledger := readObject(m.ledgerPath())
	if ledger == nil {
		ledger = jsoncompat.NewObject()
	}
	if previous, exists := ledger.Get(requestID); exists {
		if pyString(previous) != targetID {
			return nil, errors.New("同一请求不能切换到不同账号")
		}
		return m.publicLocked(), nil
	}
	state := m.loadStateLocked()
	if !switchReadyPhases[phaseOf(state)] {
		return nil, &ConflictError{"账号正在切换或等待恢复，请稍后再操作聊天"}
	}
	if pyString(indexValue(m.readIndexFile(), "activeId")) == targetID {
		// Already the live account: nothing to restart for.
		return m.publicLocked(), nil
	}
	if idle != nil {
		if err := idle(); err != nil {
			return nil, err
		}
	}
	executable := m.resolveExecutable()
	if executable == "" {
		return nil, errors.New("请先在桌面端指定 Codex 桌面程序路径")
	}
	app := newDesktopApp(executable, m.Home, m.proc)
	if err := app.Validate(m.Executable); err != nil {
		return nil, err
	}
	ledger.Set(requestID, targetID)
	if err := privateWrite(m.ledgerPath(), jsoncompat.EncodeCompact(ledger)); err != nil {
		return nil, err
	}
	state.Set("phase", "preparing")
	state.Set("requestId", requestID)
	state.Set("targetId", targetID)
	state.Set("error", nil)
	if err := privateWrite(m.statePath(), jsoncompat.EncodeCompact(state)); err != nil {
		return nil, err
	}
	rowCopy, _ := jsoncompat.Clone(row).(*jsoncompat.Object)
	go m.runSwitch(rowCopy, app)
	return m.publicLocked(), nil
}

// runSwitch mirrors _apply_switch: snapshot -> prepare -> rollback record ->
// stop -> apply -> start -> verify -> activate, with a full restore on any
// failure after the desktop quit.
func (m *Manager) runSwitch(row *jsoncompat.Object, app *DesktopApp) {
	defer func() { _ = os.RemoveAll(m.preparedPath()) }()
	before, err := m.snapshotFiles()
	if err != nil {
		m.failSwitch()
		return
	}
	m.retainCurrent(before)
	folder, err := m.prepare(row, before)
	if err != nil {
		m.failSwitch()
		return
	}
	backup := jsoncompat.NewObject()
	backup.Set("files", encodeSnapshot(before))
	backup.Set("activeId", indexValue(m.readIndexFile(), "activeId"))
	backup.Set("desktopExecutable", app.Executable)
	if err := privateWrite(m.rollbackPath(), jsoncompat.EncodeCompact(backup)); err != nil {
		m.failSwitch()
		return
	}
	m.setPhase("stopping", "")
	if err := app.Stop(); err != nil {
		// Nothing was applied yet, so a failed quit only fails the switch.
		m.failSwitch()
		return
	}
	// The GUI may refresh tokens or persist settings while exiting.
	afterStop, snapErr := m.snapshotFiles()
	changed := snapErr == nil && snapshotDiffers(afterStop, before)
	if changed {
		before = afterStop
		backup.Set("files", encodeSnapshot(before))
		if err := privateWrite(m.rollbackPath(), jsoncompat.EncodeCompact(backup)); err != nil {
			m.restore(app, backup, before)
			return
		}
		m.retainCurrent(before)
	}
	if err := app.WaitStopped(m.endpoint(), m.stopWait); err != nil {
		m.restore(app, backup, before)
		return
	}
	if changed {
		if folder, err = m.prepare(row, before); err != nil {
			m.restore(app, backup, before)
			return
		}
	}
	m.setPhase("applying", "")
	if err := m.applyFiles(folder); err != nil {
		m.restore(app, backup, before)
		return
	}
	m.setPhase("starting", "")
	if err := app.Start(); err != nil {
		m.restore(app, backup, before)
		return
	}
	m.setPhase("verifying", "")
	if err := app.WaitReady(m.endpoint(), m.verifyReady(row), m.readyWait, m.poll); err != nil {
		m.restore(app, backup, before)
		return
	}
	_ = os.RemoveAll(m.preparedPath())
	m.switchMu.Lock()
	m.markActiveLocked(pyString(row.MustGet("id")))
	m.setPhaseLocked("complete", "")
	m.switchMu.Unlock()
	_ = os.Remove(m.rollbackPath())
}

// restore mirrors the rollback branch: put the previous account back and
// relaunch, or park in 'interrupted' for a manual desktop-side recovery.
func (m *Manager) restore(app *DesktopApp, backup *jsoncompat.Object, before map[string][]byte) {
	m.setPhase("restoring", "")
	restoreErr := func() error {
		if err := app.Stop(); err != nil {
			return err
		}
		if err := m.restoreFiles(before); err != nil {
			return err
		}
		if err := app.Start(); err != nil {
			return err
		}
		return app.WaitReady(m.endpoint(), nil, m.readyWait, m.poll)
	}()
	if restoreErr != nil {
		m.setPhase("interrupted", "自动恢复未完成，请在桌面端恢复原接入")
		return
	}
	m.switchMu.Lock()
	m.markActiveLocked(pyString(backup.MustGet("activeId")))
	m.setPhaseLocked("restored", "切换未完成，已恢复原接入；请检查桌面程序或重新登录")
	m.switchMu.Unlock()
	_ = os.Remove(m.rollbackPath())
}

// failSwitch mirrors the no-rollback failure branch: the message depends on
// how far the switch got (Python reads state phase 'stopping').
func (m *Manager) failSwitch() {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	message := "切换未执行，请检查账号登录、配置限制和桌面程序路径"
	if phaseOf(m.state) == "stopping" {
		message = "桌面程序未正常退出，请检查系统权限或手动关闭后重试"
	}
	m.setPhaseLocked("failed", message)
}

// --- persisted state ------------------------------------------------------

// loadStateLocked returns the in-memory switch state. The first read loads
// switch.json and normalizes a stale in-flight phase to 'interrupted' (Python
// __init__; later reads must see the live phases this process is driving).
func (m *Manager) loadStateLocked() *jsoncompat.Object {
	if m.state != nil {
		return m.state
	}
	m.state = readObject(m.statePath())
	if m.state == nil {
		m.state = jsoncompat.NewObject()
		m.state.Set("phase", "idle")
	}
	if phase := phaseOf(m.state); switchActivePhases[phase] {
		m.state.Set("phase", "interrupted")
		m.state.Set("error", "上次切换中断，请在桌面端恢复原接入")
		_ = privateWrite(m.statePath(), jsoncompat.EncodeCompact(m.state))
	}
	return m.state
}

func (m *Manager) setPhase(phase, message string) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	m.setPhaseLocked(phase, message)
}

func (m *Manager) setPhaseLocked(phase, message string) {
	if m.state == nil {
		m.state = jsoncompat.NewObject()
	}
	m.state.Set("phase", phase)
	if message == "" {
		m.state.Set("error", nil)
	} else {
		m.state.Set("error", message)
	}
	_ = privateWrite(m.statePath(), jsoncompat.EncodeCompact(m.state))
}

func phaseOf(state *jsoncompat.Object) string {
	if state == nil {
		return "idle"
	}
	phase, _ := state.MustGet("phase").(string)
	if phase == "" {
		return "idle"
	}
	return phase
}

// --- index and account rows -----------------------------------------------

func (m *Manager) accountRoot() string  { return filepath.Join(m.DataDir, "accounts") }
func (m *Manager) indexPath() string    { return filepath.Join(m.accountRoot(), "index.json") }
func (m *Manager) statePath() string    { return filepath.Join(m.accountRoot(), "switch.json") }
func (m *Manager) ledgerPath() string   { return filepath.Join(m.accountRoot(), "requests.json") }
func (m *Manager) rollbackPath() string { return filepath.Join(m.accountRoot(), "rollback.json") }
func (m *Manager) preparedPath() string { return filepath.Join(m.accountRoot(), "prepared") }

func (m *Manager) readIndexFile() *jsoncompat.Object {
	index := readObject(m.indexPath())
	if index == nil {
		index = jsoncompat.NewObject()
	}
	return index
}

// rowLocked mirrors row(): validate the id shape, then find the index entry.
func (m *Manager) rowLocked(identifier string) (*jsoncompat.Object, error) {
	if !accountIDPattern.MatchString(identifier) {
		return nil, errors.New("账号标识无效")
	}
	index := m.readIndexFile()
	accounts, _ := index.MustGet("accounts").(*jsoncompat.Array)
	for _, value := range orArray(accounts).Items {
		row, _ := value.(*jsoncompat.Object)
		if row != nil && pyString(row.MustGet("id")) == identifier {
			return row, nil
		}
	}
	return nil, errors.New("账号不存在，请刷新列表")
}

// markActiveLocked mirrors mark_active: activeId plus a config.toml hash stamp
// so later reads can detect external edits.
func (m *Manager) markActiveLocked(identifier string) {
	index := m.readIndexFile()
	if identifier == "" {
		index.Set("activeId", nil)
		index.Set("activeStamp", nil)
	} else {
		index.Set("activeId", identifier)
		stamp := jsoncompat.NewObject()
		configBytes, readErr := os.ReadFile(m.homeFile("config.toml"))
		if readErr != nil {
			// The config was just applied and verified; hash whatever the
			// desktop may have written, else the stamp simply degrades.
			configBytes = nil
		}
		summary := sha256.Sum256(configBytes)
		stamp.Set("config", hex.EncodeToString(summary[:]))
		index.Set("activeStamp", stamp)
	}
	_ = privateWrite(m.indexPath(), jsoncompat.EncodeCompact(index))
}

func indexValue(index *jsoncompat.Object, key string) any {
	if index == nil {
		return nil
	}
	return index.MustGet(key)
}

// resolveExecutable mirrors the desktopExecutable index field falling back to
// discovery from the CLI runtime path.
func (m *Manager) resolveExecutable() string {
	index := m.readIndexFile()
	if executable, _ := index.MustGet("desktopExecutable").(string); executable != "" {
		return executable
	}
	return DiscoverDesktopApp(m.Executable)
}

// --- Codex home file snapshots ---------------------------------------------

func (m *Manager) homeFile(name string) string { return filepath.Join(m.Home, name) }

// snapshotFiles mirrors snapshot_files: raw bytes of config.toml/auth.json,
// refusing symlinks and tolerating missing files (nil).
func (m *Manager) snapshotFiles() (map[string][]byte, error) {
	result := map[string][]byte{}
	for _, name := range []string{"config.toml", "auth.json"} {
		path := m.homeFile(name)
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				result[name] = nil
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("切换不支持符号链接形式的认证或配置文件")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		result[name] = data
	}
	return result, nil
}

func snapshotDiffers(left, right map[string][]byte) bool {
	for _, name := range []string{"config.toml", "auth.json"} {
		if !bytes.Equal(left[name], right[name]) {
			return true
		}
	}
	return false
}

// restoreFiles mirrors restore_files.
func (m *Manager) restoreFiles(snapshot map[string][]byte) error {
	for name, data := range snapshot {
		if name != "config.toml" && name != "auth.json" {
			return errors.New("恢复文件无效")
		}
		if data == nil {
			_ = os.Remove(m.homeFile(name))
			continue
		}
		if err := privateWrite(m.homeFile(name), data); err != nil {
			return err
		}
	}
	return nil
}

// retainCurrent mirrors retain_current: refreshed tokens seen while quitting
// are written back into the active account's own directory.
func (m *Manager) retainCurrent(snapshot map[string][]byte) {
	m.switchMu.Lock()
	defer m.switchMu.Unlock()
	currentID := pyString(indexValue(m.readIndexFile(), "activeId"))
	if currentID == "" || snapshot["auth.json"] == nil {
		return
	}
	row, err := m.rowLocked(currentID)
	if err != nil || pyString(row.MustGet("kind")) != "chatgpt" {
		return
	}
	auth, err := jsoncompat.Unmarshal(snapshot["auth.json"])
	if err != nil {
		return
	}
	authObject, _ := auth.(*jsoncompat.Object)
	owner := tokenOwnerOf(authObject)
	rowOwner, _ := row.MustGet("tokenOwner").(*jsoncompat.Array)
	if owner == nil || rowOwner == nil || string(jsoncompat.EncodeCompact(owner)) != string(jsoncompat.EncodeCompact(rowOwner)) {
		return
	}
	_ = privateWrite(filepath.Join(m.accountRoot(), currentID, "auth.json"), snapshot["auth.json"])
}

// tokenOwnerOf mirrors token_owner: [account_id, id_token sub, id_token email]
// or nil when the record is unusable.
func tokenOwnerOf(auth *jsoncompat.Object) *jsoncompat.Array {
	if auth == nil {
		return nil
	}
	tokens, _ := auth.MustGet("tokens").(*jsoncompat.Object)
	if tokens == nil {
		return nil
	}
	encoded, _ := tokens.MustGet("id_token").(string)
	parts := strings.Split(encoded, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	decoded, err := jsoncompat.Unmarshal(payload)
	if err != nil {
		return nil
	}
	claims, _ := decoded.(*jsoncompat.Object)
	if claims == nil {
		return nil
	}
	return &jsoncompat.Array{Items: []any{
		tokens.MustGet("account_id"), claims.MustGet("sub"), claims.MustGet("email"),
	}}
}

// encodeSnapshot base64s snapshot bytes into the rollback record shape
// (Python stores {name: base64 | None}).
func encodeSnapshot(snapshot map[string][]byte) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, name := range []string{"config.toml", "auth.json"} {
		if snapshot[name] == nil {
			result.Set(name, nil)
			continue
		}
		result.Set(name, base64.StdEncoding.EncodeToString(snapshot[name]))
	}
	return result
}

// --- prepared folder (target account config) -------------------------------

type configEdit struct {
	keyPath string
	value   any
}

// prepare mirrors prepare(): build DataDir/accounts/prepared with the target
// account's config.toml/auth.json, then drive config/batchWrite through a
// short-lived app-server rooted at that folder so overlay limits surface
// before anything is applied.
func (m *Manager) prepare(row *jsoncompat.Object, before map[string][]byte) (string, error) {
	folder := m.preparedPath()
	if err := os.RemoveAll(folder); err != nil {
		return "", err
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	if before["config.toml"] != nil {
		if err := privateWrite(filepath.Join(folder, "config.toml"), before["config.toml"]); err != nil {
			return "", err
		}
	}
	kind := pyString(row.MustGet("kind"))
	changes := []configEdit{
		{"cli_auth_credentials_store", "file"},
		{"model_provider", "openai"},
		{"model_providers.bridge_api", nil},
		{"openai_base_url", nil},
	}
	if kind == "chatgpt" {
		auth, err := os.ReadFile(filepath.Join(m.accountRoot(), pyString(row.MustGet("id")), "auth.json"))
		if err != nil {
			return "", errors.New("保存的账号需要重新登录")
		}
		if err := privateWrite(filepath.Join(folder, "auth.json"), auth); err != nil {
			return "", err
		}
		changes = append(changes, configEdit{"model_providers.openai", nil})
	} else {
		// Python reads api.json with a {} default; a missing key file simply
		// yields an empty secret and fails later at the account verification.
		secret := ""
		if api := readObject(filepath.Join(m.accountRoot(), pyString(row.MustGet("id")), "api.json")); api != nil {
			secret, _ = api.MustGet("key").(string)
		}
		auth := jsoncompat.NewObject()
		auth.Set("auth_mode", "apikey")
		auth.Set("OPENAI_API_KEY", secret)
		if err := privateWrite(filepath.Join(folder, "auth.json"), jsoncompat.EncodeCompact(auth)); err != nil {
			return "", err
		}
		changes = append(changes,
			configEdit{"model_provider", "bridge_api"},
			configEdit{"model", row.MustGet("model")},
			configEdit{"service_tier", nil},
		)
		provider := jsoncompat.NewObject()
		provider.Set("name", "Bridge API")
		provider.Set("base_url", row.MustGet("baseUrl"))
		provider.Set("wire_api", "responses")
		provider.Set("requires_openai_auth", true)
		provider.Set("supports_websockets", false)
		changes = append(changes,
			configEdit{"model_providers.bridge_api", provider},
			// The desktop may explicitly select openai, including when resuming
			// an existing chat; both provider IDs must use the endpoint above.
			configEdit{"model_providers.openai", nil},
			configEdit{"openai_base_url", row.MustGet("baseUrl")},
		)
	}

	rpc, err := startRPCIn(folder, m.Executable)
	if err != nil {
		return "", err
	}
	config, err := rpc.requestDeadline("config/read", map[string]any{"includeLayers": false}, rpcCallTimeout)
	if err != nil {
		rpc.Close()
		return "", errors.New("接入配置无法读取，请检查账号登录和桌面程序路径")
	}
	if nested := object(config, "config"); nested != nil {
		config = nested
	}
	if pyString(config.MustGet("forced_login_method")) != "" {
		forced := "api"
		if kind == "chatgpt" {
			forced = "chatgpt"
		}
		changes = append(changes, configEdit{"forced_login_method", forced})
	}
	if pyString(config.MustGet("profile")) != "" {
		rpc.Close()
		return "", errors.New("当前配置启用了 profile，请先在桌面切回默认配置")
	}
	if kind == "chatgpt" {
		// Read the native catalog again after changing provider below.
		changes = append(changes, configEdit{"model", nil})
	}
	edits := &jsoncompat.Array{}
	for _, change := range changes {
		edit := jsoncompat.NewObject()
		edit.Set("keyPath", change.keyPath)
		edit.Set("value", change.value)
		edit.Set("mergeStrategy", "replace")
		edits.Items = append(edits.Items, edit)
	}
	result, err := rpc.requestDeadline("config/batchWrite", map[string]any{"edits": edits}, rpcCallTimeout)
	rpc.Close()
	if err != nil {
		return "", errors.New("运行时拒绝账号或配置操作，请检查登录及工作区限制")
	}
	if pyString(result.MustGet("status")) != "ok" {
		return "", errors.New("接入配置被其他配置层覆盖，无法安全切换")
	}
	if kind == "chatgpt" {
		verify, err := startRPCIn(folder, m.Executable)
		if err != nil {
			return "", err
		}
		defer verify.Close()
		auth, err := verify.requestDeadline("account/read", map[string]any{"refreshToken": false}, rpcCallTimeout)
		if err != nil {
			return "", errors.New("保存的账号需要重新登录")
		}
		account := object(auth, "account")
		if account == nil || pyString(account.MustGet("type")) != "chatgpt" ||
			pyString(account.MustGet("email")) != pyString(row.MustGet("email")) {
			return "", errors.New("保存的账号需要重新登录")
		}
	}
	return folder, nil
}

// applyFiles mirrors the applying phase: move the prepared files into the
// Codex home atomically enough for the relaunch.
func (m *Manager) applyFiles(folder string) error {
	config, err := os.ReadFile(filepath.Join(folder, "config.toml"))
	if err != nil {
		return err
	}
	if err := privateWrite(m.homeFile("config.toml"), config); err != nil {
		return err
	}
	auth, err := os.ReadFile(filepath.Join(folder, "auth.json"))
	if err != nil {
		_ = os.Remove(m.homeFile("auth.json"))
		return nil
	}
	return privateWrite(m.homeFile("auth.json"), auth)
}

// verifyReady builds the wait_ready account probe: provider, identity and for
// API accounts the endpoint/model must all reflect the target row.
func (m *Manager) verifyReady(row *jsoncompat.Object) func() error {
	return func() error {
		probe, err := startRPC(m.Home, m.Executable)
		if err != nil {
			return err
		}
		defer probe.Close()
		config, err := probe.requestDeadline("config/read", map[string]any{"includeLayers": false}, rpcCallTimeout)
		if err != nil {
			return err
		}
		if nested := object(config, "config"); nested != nil {
			config = nested
		}
		provider := pyString(config.MustGet("model_provider"))
		if provider == "" {
			provider = "openai"
		}
		auth, err := probe.requestDeadline("account/read", map[string]any{"refreshToken": false}, rpcCallTimeout)
		if err != nil {
			return err
		}
		account := object(auth, "account")
		if pyString(row.MustGet("kind")) == "chatgpt" {
			if provider != "openai" {
				return errors.New("模型提供商未生效")
			}
			if account == nil || pyString(account.MustGet("type")) != "chatgpt" ||
				pyString(account.MustGet("email")) != pyString(row.MustGet("email")) {
				return errors.New("账号身份未生效")
			}
			tokens := readObject(m.homeFile("auth.json"))
			var tokenOwnerValue *jsoncompat.Array
			if tokens != nil {
				tokenOwnerValue = tokenOwnerOf(tokens)
			}
			rowOwner, _ := row.MustGet("tokenOwner").(*jsoncompat.Array)
			if tokenOwnerValue == nil || rowOwner == nil ||
				string(jsoncompat.EncodeCompact(tokenOwnerValue)) != string(jsoncompat.EncodeCompact(rowOwner)) {
				return errors.New("账号身份未生效")
			}
			return nil
		}
		if provider != "bridge_api" {
			return errors.New("模型提供商未生效")
		}
		if account == nil || pyString(account.MustGet("type")) != "apiKey" {
			return errors.New("API 接入配置未生效")
		}
		providers, _ := config.MustGet("model_providers").(*jsoncompat.Object)
		var bridgeAPI *jsoncompat.Object
		if providers != nil {
			bridgeAPI, _ = providers.MustGet("bridge_api").(*jsoncompat.Object)
		}
		baseURL := pyString(row.MustGet("baseUrl"))
		if bridgeAPI == nil || pyString(bridgeAPI.MustGet("base_url")) != baseURL ||
			pyString(config.MustGet("openai_base_url")) != baseURL ||
			pyString(config.MustGet("model")) != pyString(row.MustGet("model")) {
			return errors.New("API 接入配置未生效")
		}
		return nil
	}
}

// endpoint resolves the desktop IPC endpoint the same way main.go wires the
// codex bridge: explicit override first, then the OS default for the home.
func (m *Manager) endpoint() string {
	if path := os.Getenv("GATEWAY_IPC_PATH"); path != "" {
		return path
	}
	if m.Home == "" {
		return ""
	}
	return transport.CurrentOSEndpoint(m.Home)
}

// --- small shared helpers --------------------------------------------------

func pyString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

func orArray(array *jsoncompat.Array) *jsoncompat.Array {
	if array == nil {
		return &jsoncompat.Array{}
	}
	return array
}

// privateWrite mirrors private_bytes: owner-only temp file, fsync, atomic
// rename, refusing symlink targets.
func privateWrite(path string, content []byte) error {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("账号文件不能是符号链接")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".account-")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(content); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
