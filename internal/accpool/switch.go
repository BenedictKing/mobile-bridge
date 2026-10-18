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

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// SessionManager is the slice of acpruntime.Manager the switcher needs:
// which sessions are live, whether any turn or approval is in flight, and how
// to re-attach one session under fresh credentials.
type SessionManager interface {
	LiveSessions() map[string]string
	BusyReasons() []string
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
// kimiManager.use plus the ACP-specific process restart:
//
//	gate → read back the current slot into its snapshot → refresh the target
//	token when due → atomically replace the slot (+ ensure the CLI provider
//	section) → Reconcile every live session → on any reattach failure,
//	restore the previous slot and re-attach back.
//
// Every step is recorded in history.jsonl under the pool directory.
func (m *Manager) Switch(ctx context.Context, kind, targetID, trigger string, sessions SessionManager) error {
	if kind != "kimi" {
		return fmt.Errorf("暂不支持的 Agent 类型: %s", kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()

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
		if reasons := sessions.BusyReasons(); len(reasons) > 0 {
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

	// 2. Renew the target token when due. An auth failure means the snapshot
	// is dead weight: mark it and refuse the switch.
	if target.RefreshToken != "" && needsKimiRefresh(target.ExpiresAt, target.ExpiresIn, m.now()) {
		tokens, refreshErr := refreshKimiToken(ctx, target.RefreshToken, target.DeviceID)
		if refreshErr != nil {
			if isAuthFailed(refreshErr) {
				target.Status = StatusReauthRequired
				target.StatusReason = refreshErr.Error()
				_ = m.saveAccount(kind, target)
				return fail(fmt.Errorf("目标账号 Token 已失效，请重新登录（reauth）"))
			}
			// Transient refresh failure: fall through with the old token, the
			// CLI renews on its own at startup.
		} else {
			applyTokenSet(target, tokens)
			target.Status = StatusActive
			target.StatusReason = ""
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
