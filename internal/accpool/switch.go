package accpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpagents"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// SessionManager is the slice of acpruntime.Manager the switcher needs:
// which sessions are live, whether any turn or approval is in flight, and how
// to re-attach one session under fresh credentials.
type SessionManager interface {
	LiveSessions() map[string]string
	BusyReasonsExcept(ignore map[string]bool) []string
	Reattach(id, cwd string, timeout time.Duration) error
}

// BusyError means the switch was refused because work is in flight; the HTTP
// layer maps it to 409 like the Codex account switcher does.
type BusyError struct{ Reasons []string }

func (e *BusyError) Error() string {
	return "有会话正在进行，请等待结束后再切换：" + strings.Join(e.Reasons, "；")
}

// SwitchRecord is one history.jsonl line.
type SwitchRecord struct {
	At      int64  `json:"at"`
	Kind    string `json:"kind"`
	From    string `json:"from"`
	To      string `json:"to"`
	Trigger string `json:"trigger"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

const reattachTimeout = 60 * time.Second

// Switch moves the agent's official credential slot to the target account and
// re-attaches every live session. The sequence mirrors zcode-switcher's
// kimiManager.use / manager.use plus the ACP-specific process restart:
//
//	gate → read back the current slot into its snapshot → refresh the target
//	token when due → atomically replace the slot (+ ensure the CLI provider
//	section) → Reconcile every live session → on any reattach failure,
//	restore the previous slot and re-attach back.
//
// ignoreBusy exempts sessions from the busy gate (the quota failover path
// excludes the session whose turn just failed). Every step is recorded in
// history.jsonl under the pool directory.
func (m *Manager) Switch(ctx context.Context, kind, targetID, trigger string, sessions SessionManager, ignoreBusy map[string]bool) error {
	if !m.supports(kind) {
		return errUnsupported(kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if kind == "zcode" {
		return m.switchZcode(ctx, targetID, trigger, sessions, ignoreBusy)
	}
	if kind == "minimax" {
		return m.switchMinimax(ctx, targetID, trigger, sessions, ignoreBusy)
	}
	if kind == "workbuddy" {
		return m.switchWorkbuddy(ctx, targetID, trigger, sessions, ignoreBusy)
	}
	return m.switchKimi(ctx, targetID, trigger, sessions, ignoreBusy)
}

func (m *Manager) switchKimi(ctx context.Context, targetID, trigger string, sessions SessionManager, ignoreBusy map[string]bool) error {
	kind := "kimi"
	target, err := m.loadAccount(kind, targetID)
	if err != nil {
		return err
	}
	if target.Status == StatusReauthRequired || target.Status == StatusDisabled {
		return fmt.Errorf("账号 %s 不可用（%s），请先重新登录", target.Label, target.Status)
	}
	previous := readKimiCredentials()
	fromID := m.activeID(kind)
	if fromID == target.ID {
		return errors.New("该账号已是当前账号")
	}
	record := SwitchRecord{At: m.now().UnixMilli(), Kind: kind, From: fromID, To: target.ID, Trigger: trigger}

	fail := func(cause error) error {
		record.Error = cause.Error()
		m.appendHistory(&record)
		return cause
	}

	if sessions != nil {
		if reasons := sessions.BusyReasonsExcept(ignoreBusy); len(reasons) > 0 {
			return fail(&BusyError{Reasons: reasons})
		}
	}

	// 1. Read the slot back into the outgoing account's snapshot: the CLI
	// rotates refresh_token on its own, so a stale snapshot would be
	// permanently unusable.
	if previous != nil && fromID != "" {
		if outgoing, loadErr := m.loadAccount(kind, fromID); loadErr == nil &&
			(outgoing.AccessToken != previous.AccessToken || outgoing.RefreshToken != previous.RefreshToken) {
			outgoing.AccessToken = previous.AccessToken
			if previous.RefreshToken != "" {
				outgoing.RefreshToken = previous.RefreshToken
			}
			if previous.TokenType != "" {
				outgoing.TokenType = previous.TokenType
			}
			outgoing.Scope = previous.Scope
			if previous.ExpiresAt != 0 {
				outgoing.ExpiresAt = previous.ExpiresAt
			}
			if previous.ExpiresIn != 0 {
				outgoing.ExpiresIn = previous.ExpiresIn
			}
			if saveErr := m.saveAccount(kind, outgoing); saveErr != nil {
				return fail(fmt.Errorf("回读当前账号快照失败: %w", saveErr))
			}
		}
	}

	// 2. Always renew the target token before taking the slot: a snapshot can
	// be clock-valid yet server-side dead (its chain lost a rotation race
	// with the desktop CLI / zcode-switcher), and writing it would leave the
	// agent reporting "Stored token rejected". A fresh exchange both proves
	// the chain and mints a live access token.
	if target.RefreshToken != "" {
		tokens, refreshErr := refreshKimiToken(ctx, target.RefreshToken, target.DeviceID)
		if refreshErr == nil {
			applyTokenSet(target, tokens)
			target.Status = StatusActive
			target.StatusReason = ""
		} else if isAuthFailed(refreshErr) {
			target.Status = StatusReauthRequired
			target.StatusReason = refreshErr.Error()
			_ = m.saveAccount(kind, target)
			return fail(fmt.Errorf("目标账号 Token 已失效，请重新登录（reauth）"))
		} else if _, meErr := kimiGet(ctx, kimiAPIBase+"/me", target.AccessToken); meErr != nil {
			if isAuthFailed(meErr) {
				target.Status = StatusReauthRequired
				target.StatusReason = meErr.Error()
				_ = m.saveAccount(kind, target)
				return fail(fmt.Errorf("目标账号 Token 已失效，请重新登录（reauth）"))
			}
			// Transient network failure with an unverifiable stored token:
			// proceed like before — the CLI renews on its own at startup.
		}
	}

	// 3. Apply the slot and the CLI provider section.
	m.writeKimiSlot(target)
	if err := ensureKimiConfig(); err != nil {
		m.restoreKimiSlot(previous)
		return fail(err)
	}

	// 4. Re-attach live sessions; any failure restores the previous slot.
	if sessions != nil {
		failed := []string{}
		for id, cwd := range sessions.LiveSessions() {
			if reattachErr := sessions.Reattach(id, cwd, reattachTimeout); reattachErr != nil {
				failed = append(failed, id+": "+reattachErr.Error())
			}
		}
		if len(failed) > 0 {
			m.restoreKimiSlot(previous)
			for id, cwd := range sessions.LiveSessions() {
				_ = sessions.Reattach(id, cwd, reattachTimeout)
			}
			return fail(errors.New("会话重连失败，已恢复原账号：" + strings.Join(failed, "；")))
		}
	}

	target.LastUsedAt = m.now().UnixMilli()
	if err := m.saveAccount(kind, target); err != nil {
		return fail(err)
	}
	record.OK = true
	m.appendHistory(&record)
	return nil
}

// restoreKimiSlot puts the previous credentials back; nil means the slot was
// empty before the switch.
func (m *Manager) restoreKimiSlot(previous *kimiCredentials) {
	home := kimiHome()
	if home == "" {
		return
	}
	if previous == nil {
		_ = os.Remove(filepath.Join(home, "credentials", "kimi-code.json"))
		return
	}
	wire := *previous
	if wire.TokenType == "" {
		wire.TokenType = "Bearer"
	}
	raw, err := json.MarshalIndent(&wire, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWrite(filepath.Join(home, "credentials", "kimi-code.json"), append(raw, '\n'), 0o600)
}

// appendHistory appends one JSON line to pool/<kind>/history.jsonl.
func (m *Manager) appendHistory(record *SwitchRecord) {
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := filepath.Join(m.storeDir(record.Kind), "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(raw, '\n'))
}

// History returns the newest-first switch records, bounded to limit entries.
func (m *Manager) History(kind string, limit int) []*SwitchRecord {
	raw, err := os.ReadFile(filepath.Join(m.storeDir(kind), "history.jsonl"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	out := []*SwitchRecord{}
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		var record SwitchRecord
		if err := json.Unmarshal([]byte(lines[i]), &record); err == nil && record.Kind != "" {
			out = append(out, &record)
		}
	}
	return out
}

// historyView renders records for the API.
func historyView(records []*SwitchRecord) *jsoncompat.Array {
	out := &jsoncompat.Array{}
	for _, record := range records {
		row := jsoncompat.NewObject()
		row.Set("at", record.At)
		row.Set("kind", record.Kind)
		row.Set("from", record.From)
		row.Set("to", record.To)
		row.Set("trigger", record.Trigger)
		row.Set("ok", record.OK)
		row.Set("error", record.Error)
		out.Items = append(out.Items, row)
	}
	return out
}

// ensureKimiConfig appends the managed provider section to the CLI's
// config.toml when missing (kimiManager.ensureKimiConfig): the CLI refuses to
// read the credentials file without it. Existing content is never rewritten;
// the pre-change copy goes to config.toml.mobile-bridge-bak.
func ensureKimiConfig() error {
	home := kimiHome()
	if home == "" {
		return errors.New("无法定位 Kimi Code 目录")
	}
	file := filepath.Join(home, "config.toml")
	const sectionHeader = `[providers."managed:kimi-code"]`
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), sectionHeader) {
		return nil
	}
	template := "\n" + sectionHeader + "\n" +
		"type = \"kimi\"\n" +
		"base_url = \"" + kimiAPIBase + "\"\n" +
		"api_key = \"\"\n" +
		"\n[providers.\"managed:kimi-code\".oauth]\n" +
		"storage = \"file\"\n" +
		"key = \"oauth/kimi-code\"\n"
	content := strings.TrimRight(string(raw), " \t\r\n")
	if content != "" {
		_ = copyFile(file, file+".mobile-bridge-bak")
		content += "\n" + template
	} else {
		content = strings.TrimPrefix(template, "\n")
	}
	return atomicWrite(file, []byte(content), 0o600)
}

func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o600)
}

var zcodeValidateLogin = func(ctx context.Context, target *Account) error {
	credentials := parseJSONObject(target.Credentials)
	token := zcodeDecryptSafe(stringValue(credentials, "zcodejwttoken"))
	if !looksLikeToken(token) {
		return errors.New("目标账号快照缺少有效的 zcodeJWT")
	}
	if _, err := fetchZcodeBilling(ctx, zcodeBillingURL(zcodeBillingCurrentURL), token); err != nil {
		return fmt.Errorf("目标账号登录态校验失败: %w", err)
	}
	return nil
}

// switchZcode swaps the ~/.zcode/v2 login state. ZCode has no client refresh
// flow for zcodeJWT, so expired targets require reauthentication. The desktop
// app rewrites both slot files while running and is stopped before the swap.
func (m *Manager) switchZcode(ctx context.Context, targetID, trigger string, sessions SessionManager, ignoreBusy map[string]bool) error {
	kind := "zcode"
	target, err := m.loadAccount(kind, targetID)
	if err != nil {
		return err
	}
	if target.Status == StatusReauthRequired || target.Status == StatusDisabled {
		return fmt.Errorf("账号 %s 不可用（%s），请先重新登录", target.Label, target.Status)
	}
	if target.Credentials == "" || target.Config == "" {
		return fmt.Errorf("账号 %s 的快照不完整（缺少 credentials/config）", target.Label)
	}
	if err := zcodeValidateLogin(ctx, target); err != nil {
		return err
	}
	previous := readZcodeSlot()
	fromID := m.activeID(kind)
	if fromID == target.ID {
		return errors.New("该账号已是当前账号")
	}
	record := SwitchRecord{At: m.now().UnixMilli(), Kind: kind, From: fromID, To: target.ID, Trigger: trigger}

	fail := func(cause error) error {
		record.Error = cause.Error()
		m.appendHistory(&record)
		return cause
	}

	if sessions != nil {
		if reasons := sessions.BusyReasonsExcept(ignoreBusy); len(reasons) > 0 {
			return fail(&BusyError{Reasons: reasons})
		}
	}

	// The desktop app holds and rewrites the slot files; switching underneath
	// it is unreliable (zcode-switcher forces the same stop).
	desktopWasRunning := zcodeDesktopRunning()
	if desktopWasRunning && !zcodeDesktopStop(8*time.Second) {
		return fail(errors.New("关闭 ZCode 桌面端超时，已取消切换（避免登录态损坏）"))
	}

	// Re-read after shutdown so the outgoing snapshot includes the final write.
	previous = readZcodeSlot()
	if previous != nil && fromID != "" {
		if outgoing, loadErr := m.loadAccount(kind, fromID); loadErr == nil &&
			(outgoing.Credentials != previous.Credentials || outgoing.Config != previous.Config) {
			outgoing.Credentials = previous.Credentials
			outgoing.Config = previous.Config
			if saveErr := m.saveAccount(kind, outgoing); saveErr != nil {
				return fail(fmt.Errorf("回读当前账号快照失败: %w", saveErr))
			}
		}
	}

	if err := writeZcodeSlot(&zcodeSlot{Credentials: target.Credentials, Config: target.Config}); err != nil {
		restoreZcodeSlot(previous)
		return fail(err)
	}
	if active := m.activeID(kind); active != target.ID {
		restoreZcodeSlot(previous)
		if desktopWasRunning {
			zcodeDesktopRelaunch()
		}
		return fail(errors.New("写入后账号指纹核验失败，已恢复原账号"))
	}
	// The written snapshot may itself carry a dead coding-plan key (e.g. an
	// account whose plan key lives in the cloud and whose config slot holds
	// the desktop's OAuth JWT). Converge the slot before any backend
	// reattaches and caches the broken credential; the background watcher
	// would catch it too, but only after a window of failed turns.
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		acpagents.EnsureZcodeState(os.Getenv, home, nil)
	}

	if sessions != nil {
		failed := []string{}
		for id, cwd := range sessions.LiveSessions() {
			if reattachErr := sessions.Reattach(id, cwd, reattachTimeout); reattachErr != nil {
				failed = append(failed, id+": "+reattachErr.Error())
			}
		}
		if len(failed) > 0 {
			restoreZcodeSlot(previous)
			for id, cwd := range sessions.LiveSessions() {
				_ = sessions.Reattach(id, cwd, reattachTimeout)
			}
			if desktopWasRunning {
				zcodeDesktopRelaunch()
			}
			return fail(errors.New("会话重连失败，已恢复原账号：" + strings.Join(failed, "；")))
		}
	}

	if desktopWasRunning {
		zcodeDesktopRelaunch()
	}
	syncZcodeAccountSlot(target)
	target.LastUsedAt = m.now().UnixMilli()
	if err := m.saveAccount(kind, target); err != nil {
		return fail(err)
	}
	record.OK = true
	m.appendHistory(&record)
	return nil
}

// restoreZcodeSlot puts the previous slot files back; nil means the slot was
// empty before the switch.
func restoreZcodeSlot(previous *zcodeSlot) {
	if previous == nil {
		if path := zcodeCredentialsPath(); path != "" {
			_ = os.Remove(path)
		}
		if path := zcodeConfigPath(); path != "" {
			_ = os.Remove(path)
		}
		return
	}
	_ = writeZcodeSlot(previous)
}

// --- MiniMax (mod.rs use_account + write_official) ------------------------------

// minimaxSlotBackup captures the whole six-file slot set (both regions, so a
// cross-region switch restores whatever was there) for rollback; a nil entry
// means the file did not exist and must stay deleted on restore.
type minimaxSlotBackup struct {
	files map[string][]byte
}

func backupMinimaxSlot() *minimaxSlotBackup {
	backup := &minimaxSlotBackup{files: map[string][]byte{}}
	var paths []string
	for _, region := range minimaxRegions {
		paths = append(paths,
			filepath.Join(minimaxNSHome(region), "auth.json"),
			minimaxAuthStatePath(region),
			filepath.Join(minimaxCliAuthScopeDir(region), "local-runtime.auth.json"),
			filepath.Join(minimaxCliAuthScopeDir(region), "cli-auth.scope.json"),
		)
	}
	paths = append(paths,
		minimaxSharedProjectionPath(),
		minimaxRootProjectionPath(),
		minimaxRegionPrefPath(),
	)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			backup.files[path] = nil
			continue
		}
		content := make([]byte, len(raw))
		copy(content, raw)
		backup.files[path] = content
	}
	return backup
}

func (backup *minimaxSlotBackup) restore() {
	for path, content := range backup.files {
		if content == nil {
			_ = os.Remove(path)
			continue
		}
		_ = atomicWrite(path, content, 0o600)
	}
}

// switchMinimax swaps the ~/.minimax slot set. Compared to kimi: the slot is
// six files written with a fixed-key-order projection whose fingerprint mcode
// cross-checks, the generation/loginEpoch pair replaces the plain credentials
// file, and no process needs restarting (mcode re-reads auth.json per
// request and watches auth-state.json).
func (m *Manager) switchMinimax(ctx context.Context, targetID, trigger string, sessions SessionManager, ignoreBusy map[string]bool) error {
	kind := "minimax"
	target, err := m.loadAccount(kind, targetID)
	if err != nil {
		return err
	}
	if target.Status == StatusReauthRequired || target.Status == StatusDisabled {
		return fmt.Errorf("账号 %s 不可用（%s），请先重新登录", target.Label, target.Status)
	}
	if target.AccessToken == "" || target.RefreshToken == "" {
		return fmt.Errorf("账号 %s 的快照不完整（缺少 accessToken/refreshToken）", target.Label)
	}
	previous := backupMinimaxSlot()
	fromID := m.activeID(kind)
	if fromID == target.ID {
		return errors.New("该账号已是当前账号")
	}
	record := SwitchRecord{At: m.now().UnixMilli(), Kind: kind, From: fromID, To: target.ID, Trigger: trigger}

	fail := func(cause error) error {
		record.Error = cause.Error()
		m.appendHistory(&record)
		return cause
	}

	if sessions != nil {
		if reasons := sessions.BusyReasonsExcept(ignoreBusy); len(reasons) > 0 {
			return fail(&BusyError{Reasons: reasons})
		}
	}

	// 1. Read the slot back into the outgoing account's snapshot when its
	// tokens drifted (mcode rotates refresh_token on its own).
	if _, slotRecord, ok := scanMinimaxSlot(); ok {
		slotAccess, slotRefresh, slotExpires := minimaxSlotTokens(slotRecord)
		if slotAccess != "" {
			outgoing := m.findAccount(kind, func(a *Account) bool {
				return (slotRefresh != "" && a.RefreshToken != "" && a.RefreshToken == slotRefresh) ||
					(a.AccessToken != "" && a.AccessToken == slotAccess)
			})
			if outgoing != nil && outgoing.ID != target.ID && outgoing.AccessToken != slotAccess {
				outgoing.AccessToken = slotAccess
				if slotRefresh != "" {
					outgoing.RefreshToken = slotRefresh
				}
				if slotExpires > 0 {
					outgoing.ExpiresAt = slotExpires
				}
				if saveErr := m.saveAccount(kind, outgoing); saveErr != nil {
					return fail(fmt.Errorf("回读当前账号快照失败: %w", saveErr))
				}
			}
		}
	}

	// 2. Renew the target token when due. An auth failure marks the snapshot
	// dead and refuses; a transient failure falls through with the old token,
	// mcode renews on its own at startup (mod.rs use_account).
	if target.RefreshToken != "" && needsMinimaxRefresh(target.ExpiresAt, target.ExpiresIn, m.now()) {
		tokens, refreshErr := refreshMinimaxToken(ctx, target.Region, target.RefreshToken, m.now())
		if refreshErr != nil {
			if isAuthFailed(refreshErr) {
				target.Status = StatusReauthRequired
				target.StatusReason = refreshErr.Error()
				_ = m.saveAccount(kind, target)
				return fail(fmt.Errorf("目标账号 Token 已失效，请重新登录（reauth）"))
			}
		} else {
			applyTokenSet(target, tokens)
			target.Status = StatusActive
			target.StatusReason = ""
		}
	}

	// 3. Apply the six-file slot (a real account switch: new loginEpoch).
	if err := m.writeMinimaxSlot(target, true); err != nil {
		previous.restore()
		return fail(err)
	}

	// 4. Re-attach live sessions; any failure restores the previous slot.
	if sessions != nil {
		failed := []string{}
		for id, cwd := range sessions.LiveSessions() {
			if reattachErr := sessions.Reattach(id, cwd, reattachTimeout); reattachErr != nil {
				failed = append(failed, id+": "+reattachErr.Error())
			}
		}
		if len(failed) > 0 {
			previous.restore()
			for id, cwd := range sessions.LiveSessions() {
				_ = sessions.Reattach(id, cwd, reattachTimeout)
			}
			return fail(errors.New("会话重连失败，已恢复原账号：" + strings.Join(failed, "；")))
		}
	}

	target.LastUsedAt = m.now().UnixMilli()
	if err := m.saveAccount(kind, target); err != nil {
		return fail(err)
	}
	record.OK = true
	m.appendHistory(&record)
	return nil
}

// --- WorkBuddy (mod.rs use_account + cli.rs switch_active_account) ----------------

// switchWorkbuddy swaps the desktop app's four-segment slot and points the
// CodeBuddy CLI (apiKeyHelper mode) at the target. Sequence: refresh the
// token when stale → conservative auto-switch gate → graceful app shutdown →
// slot backup + write → relaunch when it was running → CLI state/env/cache →
// re-attach sessions (any failure after the write rolls slot + CLI state
// back).
func (m *Manager) switchWorkbuddy(ctx context.Context, targetID, trigger string, sessions SessionManager, ignoreBusy map[string]bool) error {
	kind := "workbuddy"
	target, err := m.loadAccount(kind, targetID)
	if err != nil {
		return err
	}
	if target.Status == StatusReauthRequired || target.Status == StatusDisabled {
		return fmt.Errorf("账号 %s 不可用（%s），请先重新登录", target.Label, target.Status)
	}
	if workbuddyPlainToken(target.AccessToken) == "" || target.RefreshToken == "" {
		return fmt.Errorf("账号 %s 的凭据不完整或处于加密信封态，无法切换（请扫码重新添加）", target.Label)
	}
	variant := workbuddyVariantFromAccount(target)
	fromID := m.activeID(kind)
	if fromID == target.ID {
		return errors.New("该账号已是当前账号")
	}
	record := SwitchRecord{At: m.now().UnixMilli(), Kind: kind, From: fromID, To: target.ID, Trigger: trigger}

	fail := func(cause error) error {
		record.Error = cause.Error()
		m.appendHistory(&record)
		return cause
	}

	if sessions != nil {
		if reasons := sessions.BusyReasonsExcept(ignoreBusy); len(reasons) > 0 {
			return fail(&BusyError{Reasons: reasons})
		}
	}

	// Conservative gate (stricter than zcode-switcher): the desktop app is
	// running AND the CLI holds live sessions — an auto switch would not take
	// effect on the live process and would break its key invariant, so it is
	// refused with a notification (the auto-switcher notifies on errors).
	if trigger == "auto-quota" && workbuddyAppRunning(variant) && workbuddyHasLiveSession(m.now()) {
		return fail(errors.New("WorkBuddy 运行中且 CodeBuddy CLI 有活跃会话，自动切换已跳过（重启 CLI 后新账号才会生效）"))
	}

	// 1. Renew the token inside the lazy 24h window; on failure keep the old
	// token (mod.rs use_account), only an auth failure refuses the switch.
	if needsWorkbuddyRefresh(target.ExpiresAt, m.now()) {
		if refreshErr := refreshWorkbuddyToken(ctx, target, m.now()); refreshErr != nil {
			if isAuthFailed(refreshErr) {
				_ = m.saveAccount(kind, target)
				return fail(fmt.Errorf("目标账号 Token 已失效，请重新登录（reauth）"))
			}
		} else {
			if saveErr := m.saveAccount(kind, target); saveErr != nil {
				return fail(saveErr)
			}
		}
	}

	// 2. Graceful desktop shutdown first (it rewrites the slot while
	// running); only an app that was running gets relaunched afterwards.
	wasRunning := workbuddyAppRunning(variant)
	if wasRunning {
		if closeErr := workbuddyCloseApp(variant, 20*time.Second); closeErr != nil {
			return fail(fmt.Errorf("关闭 WorkBuddy 超时，已取消切换（避免登录态损坏）：%w", closeErr))
		}
	}
	m.backupWorkbuddyAuthFile(variant, m.now())

	// In-memory copy of the previous slot + CLI state for rollback.
	slotPath := workbuddyAuthFilePath(variant)
	previousSlot, _ := os.ReadFile(slotPath)
	previousState, _ := os.ReadFile(workbuddyCLIStatePath())

	restore := func() {
		if previousSlot == nil {
			_ = os.Remove(slotPath)
		} else {
			_ = atomicWrite(slotPath, previousSlot, 0o600)
		}
		if previousState == nil {
			_ = os.Remove(workbuddyCLIStatePath())
		} else {
			_ = atomicWrite(workbuddyCLIStatePath(), previousState, 0o600)
		}
	}

	// 3. Write the four-segment slot.
	if err := writeWorkbuddyAuthFile(target, variant, m.now()); err != nil {
		restore()
		if wasRunning {
			_ = workbuddyLaunchApp(variant)
		}
		return fail(err)
	}

	// 4. Relaunch only when the app was running before the switch.
	if wasRunning {
		if launchErr := workbuddyLaunchApp(variant); launchErr != nil {
			restore()
			return fail(launchErr)
		}
	}

	// 5. CLI side: take over the apiKeyHelper on first use (journaled), then
	// point state.json at the target and sync the region env + caches.
	takeover, err := m.setupWorkbuddyHelper(m.now())
	if err != nil {
		restore()
		return fail(err)
	}
	if takeover {
		m.appendHistory(&SwitchRecord{
			At: m.now().UnixMilli(), Kind: kind, From: fromID, To: target.ID,
			Trigger: "cli-takeover", OK: true,
		})
	}
	if err := m.writeWorkbuddyCLIState(target, m.now()); err != nil {
		restore()
		return fail(err)
	}
	if err := m.applyWorkbuddyRegionEnv(variant); err != nil {
		restore()
		return fail(err)
	}

	// 6. Re-attach live sessions; any failure rolls slot + CLI state back.
	if sessions != nil {
		failed := []string{}
		for id, cwd := range sessions.LiveSessions() {
			if reattachErr := sessions.Reattach(id, cwd, reattachTimeout); reattachErr != nil {
				failed = append(failed, id+": "+reattachErr.Error())
			}
		}
		if len(failed) > 0 {
			restore()
			for id, cwd := range sessions.LiveSessions() {
				_ = sessions.Reattach(id, cwd, reattachTimeout)
			}
			return fail(errors.New("会话重连失败，已恢复原账号：" + strings.Join(failed, "；")))
		}
	}

	// 7. Marker + bookkeeping.
	target.LastUsedAt = m.now().UnixMilli()
	if err := m.saveAccount(kind, target); err != nil {
		return fail(err)
	}
	_ = atomicWrite(m.workbuddyCurrentMarkerPath(), []byte(target.ID+"\n"), 0o600)
	record.OK = true
	m.appendHistory(&record)
	return nil
}
