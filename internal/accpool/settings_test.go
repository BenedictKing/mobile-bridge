package accpool

import "testing"

func TestThresholdSettingsPersist(t *testing.T) {
	m := newTestManager(t)
	if got := m.Thresholds("kimi"); got != DefaultThresholdSettings() {
		t.Fatalf("defaults: %+v", got)
	}
	custom := ThresholdSettings{Switch: 0.975, LongTerm: 0.985, ResetCard: 0.96}
	if err := m.SetThresholds("kimi", custom); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAutoSwitch("kimi", false); err != nil {
		t.Fatal(err)
	}

	reloaded := New(m.DataDir, "")
	if got := reloaded.Thresholds("kimi"); got != custom {
		t.Fatalf("persisted thresholds: %+v", got)
	}
	if got := reloaded.Thresholds("zcode"); got != DefaultThresholdSettings() {
		t.Fatalf("other kind must keep defaults: %+v", got)
	}
	if reloaded.AutoSwitchEnabled("kimi") {
		t.Fatal("auto-switch toggle must survive threshold updates")
	}
}

func TestThresholdSettingsValidation(t *testing.T) {
	m := newTestManager(t)
	for _, invalid := range []ThresholdSettings{
		{Switch: 0, LongTerm: 0.99, ResetCard: 0.98},
		{Switch: 0.99, LongTerm: 1.01, ResetCard: 0.98},
		{Switch: 0.99, LongTerm: 0.99, ResetCard: -0.1},
	} {
		if err := m.SetThresholds("kimi", invalid); err == nil {
			t.Fatalf("invalid thresholds accepted: %+v", invalid)
		}
	}
	if got := m.Thresholds("kimi"); got != DefaultThresholdSettings() {
		t.Fatalf("invalid update changed settings: %+v", got)
	}
}
