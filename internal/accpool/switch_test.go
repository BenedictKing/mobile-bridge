package accpool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubTokenServer points the kimi token endpoint at a stub that answers a
// fixed token pair.
func stubTokenServer(t *testing.T, accessToken, refreshToken string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("unexpected grant: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{
			"access_token": accessToken, "refresh_token": refreshToken,
			"token_type": "Bearer", "expires_in": 3600,
		})
		_, _ = w.Write(body)
	}))
	restore := kimiTokenURL
	kimiTokenURL = server.URL
	t.Cleanup(func() { kimiTokenURL = restore })
	return server
}

type fakeSM struct {
	live       map[string]string
	busy       []string
	failOn     map[string]bool
	reattached []string
}

func (f *fakeSM) LiveSessions() map[string]string            { return f.live }
func (f *fakeSM) BusyReasonsExcept(map[string]bool) []string { return f.busy }
func (f *fakeSM) Reattach(id, cwd string, _ time.Duration) error {
	if f.failOn[id] {
		return errors.New("session/load 被拒绝")
	}
	f.reattached = append(f.reattached, id)
	return nil
}

// setupSwitchFixture creates a pool with accounts u_a (occupying the slot)
// and u_b, both with tokens far from expiry.
func setupSwitchFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	for _, acc := range []*Account{
		{ID: "u_a", Label: "账号A", UserID: "a", AccessToken: "at-a", RefreshToken: "rt-a",
			ExpiresAt: m.now().Unix() + 86400, ExpiresIn: 86400, Status: StatusActive},
		{ID: "u_b", Label: "账号B", UserID: "b", AccessToken: "at-b", RefreshToken: "rt-b",
			ExpiresAt: m.now().Unix() + 86400, ExpiresIn: 86400, Status: StatusActive},
	} {
		if err := m.saveAccount("kimi", acc); err != nil {
			t.Fatal(err)
		}
	}
	writeSlot(t, home, &kimiCredentials{AccessToken: "at-a", RefreshToken: "rt-a", TokenType: "Bearer"})
	return m, home
}

func writeSlot(t *testing.T, home string, creds *kimiCredentials) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readSlot(t *testing.T, home string) *kimiCredentials {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "credentials", "kimi-code.json"))
	if err != nil {
		return nil
	}
	var creds kimiCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	return &creds
}

func TestSwitchSuccess(t *testing.T) {
	m, home := setupSwitchFixture(t)
	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp/proj"}}
	if err := m.Switch(context.Background(), "kimi", "u_b", "manual", sessions, nil); err != nil {
		t.Fatalf("switch: %v", err)
	}
	slot := readSlot(t, home)
	if slot.AccessToken != "at-b" || slot.RefreshToken != "rt-b" {
		t.Fatalf("slot not switched: %+v", slot)
	}
	if len(sessions.reattached) != 1 || sessions.reattached[0] != "sess-1" {
		t.Fatalf("reattached: %v", sessions.reattached)
	}
	if got := m.activeID("kimi"); got != "u_b" {
		t.Fatalf("active id: %q", got)
	}
	// The CLI provider section must exist for the CLI to read the slot.
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil || !strings.Contains(string(raw), `[providers."managed:kimi-code"]`) {
		t.Fatalf("config.toml: %v %q", err, raw)
	}
	history := m.History("kimi", 10)
	if len(history) != 1 || !history[0].OK || history[0].From != "u_a" || history[0].To != "u_b" {
		t.Fatalf("history: %+v", history)
	}
	target, _ := m.loadAccount("kimi", "u_b")
	if target.LastUsedAt != m.now().UnixMilli() {
		t.Fatalf("lastUsedAt not bumped: %+v", target)
	}
}

func TestSwitchReadsBackRotatedTokens(t *testing.T) {
	m, home := setupSwitchFixture(t)
	// The CLI rotated A's tokens on its own; the slot no longer matches the
	// snapshot. Switching away must fold the rotated pair into A's snapshot.
	writeSlot(t, home, &kimiCredentials{AccessToken: "at-a-rotated", RefreshToken: "rt-a", TokenType: "Bearer", ExpiresAt: 42})
	if err := m.Switch(context.Background(), "kimi", "u_b", "manual", nil, nil); err != nil {
		t.Fatalf("switch: %v", err)
	}
	a, err := m.loadAccount("kimi", "u_a")
	if err != nil {
		t.Fatal(err)
	}
	if a.AccessToken != "at-a-rotated" || a.ExpiresAt != 42 {
		t.Fatalf("read-back failed: %+v", a)
	}
}

func TestSwitchBusyGate(t *testing.T) {
	m, home := setupSwitchFixture(t)
	sessions := &fakeSM{busy: []string{"sess-1: 回合进行中"}}
	err := m.Switch(context.Background(), "kimi", "u_b", "manual", sessions, nil)
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("expected BusyError, got %v", err)
	}
	if slot := readSlot(t, home); slot.AccessToken != "at-a" {
		t.Fatalf("slot must be untouched: %+v", slot)
	}
	if history := m.History("kimi", 10); len(history) != 1 || history[0].OK {
		t.Fatalf("history should record the refusal: %+v", history)
	}
}

func TestSwitchRollbackOnReattachFailure(t *testing.T) {
	m, home := setupSwitchFixture(t)
	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp"}, failOn: map[string]bool{"sess-1": true}}
	err := m.Switch(context.Background(), "kimi", "u_b", "manual", sessions, nil)
	if err == nil || !strings.Contains(err.Error(), "已恢复原账号") {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if slot := readSlot(t, home); slot.AccessToken != "at-a" || slot.RefreshToken != "rt-a" {
		t.Fatalf("slot not restored: %+v", slot)
	}
	if got := m.activeID("kimi"); got != "u_a" {
		t.Fatalf("active id after rollback: %q", got)
	}
	if history := m.History("kimi", 10); len(history) != 1 || history[0].OK || history[0].Error == "" {
		t.Fatalf("history: %+v", history)
	}
}

func TestSwitchRejectsSameAndDeadAccounts(t *testing.T) {
	m, _ := setupSwitchFixture(t)
	if err := m.Switch(context.Background(), "kimi", "u_a", "manual", nil, nil); err == nil {
		t.Fatal("switching to the current account must fail")
	}
	dead, _ := m.loadAccount("kimi", "u_b")
	dead.Status = StatusReauthRequired
	if err := m.saveAccount("kimi", dead); err != nil {
		t.Fatal(err)
	}
	if err := m.Switch(context.Background(), "kimi", "u_b", "manual", nil, nil); err == nil {
		t.Fatal("switching to a reauth_required account must fail")
	}
}

func TestSwitchRefreshesExpiringTarget(t *testing.T) {
	m, home := setupSwitchFixture(t)
	server := stubTokenServer(t, "at-b-new", "rt-b-new")
	defer server.Close()
	target, _ := m.loadAccount("kimi", "u_b")
	target.ExpiresAt = m.now().Unix() + 60 // inside the refresh lead
	if err := m.saveAccount("kimi", target); err != nil {
		t.Fatal(err)
	}
	if err := m.Switch(context.Background(), "kimi", "u_b", "manual", nil, nil); err != nil {
		t.Fatalf("switch: %v", err)
	}
	slot := readSlot(t, home)
	if slot.AccessToken != "at-b-new" || slot.RefreshToken != "rt-b-new" {
		t.Fatalf("expiring target must be refreshed before applying: %+v", slot)
	}
}

func TestHistoryOrdering(t *testing.T) {
	m, _ := setupSwitchFixture(t)
	if err := m.Switch(context.Background(), "kimi", "u_b", "manual", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Switch(context.Background(), "kimi", "u_a", "manual", nil, nil); err != nil {
		t.Fatal(err)
	}
	history := m.History("kimi", 10)
	if len(history) != 2 || history[0].To != "u_a" || history[1].To != "u_b" {
		t.Fatalf("history must be newest-first: %+v", history)
	}
}
