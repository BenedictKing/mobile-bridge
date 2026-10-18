package accpool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// poolSettings is <dataDir>/pool/settings.json: per-agent auto-switch toggles.
// A missing entry means enabled — automatic failover is the feature's point;
// GATEWAY_POOL_AUTOSWITCH=0 is the global kill switch.
type poolSettings struct {
	AutoSwitch map[string]bool `json:"autoSwitch"`
}

var settingsMu sync.Mutex

func (m *Manager) settingsPath() string {
	return filepath.Join(m.DataDir, "pool", "settings.json")
}

func (m *Manager) loadSettings() *poolSettings {
	settings := &poolSettings{AutoSwitch: map[string]bool{}}
	raw, err := os.ReadFile(m.settingsPath())
	if err != nil {
		return settings
	}
	if json.Unmarshal(raw, settings) != nil || settings.AutoSwitch == nil {
		return &poolSettings{AutoSwitch: map[string]bool{}}
	}
	return settings
}

// AutoSwitchEnabled reports whether automatic quota failover is on for the
// agent kind.
func (m *Manager) AutoSwitchEnabled(kind string) bool {
	if os.Getenv("GATEWAY_POOL_AUTOSWITCH") == "0" {
		return false
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	enabled, present := m.loadSettings().AutoSwitch[kind]
	return !present || enabled
}

// SetAutoSwitch persists the per-kind toggle.
func (m *Manager) SetAutoSwitch(kind string, enabled bool) error {
	if !m.supports(kind) {
		return errUnsupported(kind)
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	settings := m.loadSettings()
	settings.AutoSwitch[kind] = enabled
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.settingsPath(), append(raw, '\n'), 0o600)
}

func errUnsupported(kind string) error {
	return &unsupportedKindError{kind: kind}
}

type unsupportedKindError struct{ kind string }

func (e *unsupportedKindError) Error() string { return "暂不支持的 Agent 类型: " + e.kind }
