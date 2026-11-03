package accpool

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultSwitchThreshold    = 0.99
	defaultLongTermThreshold  = 0.99
	defaultResetCardThreshold = 0.98
)

// ThresholdSettings stores used/limit ratios. The HTTP view converts them to
// percentages; ResetCard is only used by zcode's five-hour reset cards.
type ThresholdSettings struct {
	Switch    float64 `json:"switch"`
	LongTerm  float64 `json:"longTerm"`
	ResetCard float64 `json:"resetCard"`
}

func DefaultThresholdSettings() ThresholdSettings {
	return ThresholdSettings{Switch: defaultSwitchThreshold, LongTerm: defaultLongTermThreshold, ResetCard: defaultResetCardThreshold}
}

func (s *ThresholdSettings) defaults() {
	fallback := DefaultThresholdSettings()
	if s.Switch <= 0 || s.Switch > 1 {
		s.Switch = fallback.Switch
	}
	if s.LongTerm <= 0 || s.LongTerm > 1 {
		s.LongTerm = fallback.LongTerm
	}
	if s.ResetCard <= 0 || s.ResetCard > 1 {
		s.ResetCard = fallback.ResetCard
	}
}

func validateThresholdSettings(s ThresholdSettings) error {
	for _, value := range []float64{s.Switch, s.LongTerm, s.ResetCard} {
		if value <= 0 || value > 1 {
			return errors.New("阈值必须大于 0% 且不超过 100%")
		}
	}
	return nil
}

// poolSettings is <dataDir>/pool/settings.json: per-agent auto-switch toggles
// and threshold ratios. Missing entries use the defaults above.
type poolSettings struct {
	AutoSwitch map[string]bool              `json:"autoSwitch"`
	Thresholds map[string]ThresholdSettings `json:"thresholds,omitempty"`
}

var settingsMu sync.Mutex

func newPoolSettings() *poolSettings {
	return &poolSettings{AutoSwitch: map[string]bool{}, Thresholds: map[string]ThresholdSettings{}}
}

func (m *Manager) settingsPath() string {
	return filepath.Join(m.DataDir, "pool", "settings.json")
}

func (m *Manager) loadSettings() *poolSettings {
	settings := newPoolSettings()
	raw, err := os.ReadFile(m.settingsPath())
	if err != nil || json.Unmarshal(raw, settings) != nil {
		return newPoolSettings()
	}
	if settings.AutoSwitch == nil {
		settings.AutoSwitch = map[string]bool{}
	}
	if settings.Thresholds == nil {
		settings.Thresholds = map[string]ThresholdSettings{}
	}
	return settings
}

func (m *Manager) saveSettingsLocked(settings *poolSettings) error {
	raw, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.settingsPath(), append(raw, '\n'), 0o600)
}

// AutoSwitchEnabled reports whether automatic quota failover is on for the
// agent kind. GATEWAY_POOL_AUTOSWITCH=0 is the global kill switch.
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
	return m.saveSettingsLocked(settings)
}

// Thresholds returns the latest per-kind settings on every call so HTTP edits
// take effect without restarting the gateway.
func (m *Manager) Thresholds(kind string) ThresholdSettings {
	settingsMu.Lock()
	defer settingsMu.Unlock()
	thresholds := m.loadSettings().Thresholds[kind]
	thresholds.defaults()
	return thresholds
}

func (m *Manager) SetThresholds(kind string, thresholds ThresholdSettings) error {
	if !m.supports(kind) {
		return errUnsupported(kind)
	}
	if err := validateThresholdSettings(thresholds); err != nil {
		return err
	}
	settingsMu.Lock()
	defer settingsMu.Unlock()
	settings := m.loadSettings()
	settings.Thresholds[kind] = thresholds
	return m.saveSettingsLocked(settings)
}

func errUnsupported(kind string) error {
	return &unsupportedKindError{kind: kind}
}

type unsupportedKindError struct{ kind string }

func (e *unsupportedKindError) Error() string { return "暂不支持的 Agent 类型: " + e.kind }
