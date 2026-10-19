package accpool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubKimiAPI serves /me, /usages and /token, keying the quota response on
// the Bearer token so each account gets its own usage fixture.
func stubKimiAPI(t *testing.T, usages map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/me":
			_, _ = w.Write([]byte(`{"user_id":"u","email":"u@example.com"}`))
		case "/usages":
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			body, ok := usages[token]
			if !ok {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(body))
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"at-x","refresh_token":"rt-x","expires_in":3600}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	restoreAPI, restoreToken := kimiAPIBase, kimiTokenURL
	kimiAPIBase, kimiTokenURL = server.URL, server.URL+"/token"
	t.Cleanup(func() { kimiAPIBase, kimiTokenURL = restoreAPI, restoreToken })
	return server
}

func usagesJSON(windowUsed, windowLimit int64, resetAt string) string {
	detail := `{"used":` + strconv.FormatInt(windowUsed, 10) + `,"limit":` + strconv.FormatInt(windowLimit, 10)
	if resetAt != "" {
		detail += `,"resetTime":"` + resetAt + `"`
	}
	return `{"usage":{"used":1,"limit":1000,"resetTime":"2099-01-01T00:00:00Z"},` +
		`"limits":[{"window":{"timeUnit":"TIME_UNIT_MINUTE","duration":300},"detail":` + detail + `}}]}`
}

func TestExhaustedWindows(t *testing.T) {
	quota := parseKimiUsages(map[string]any{
		"usage": map[string]any{"used": 500.0, "limit": 1000.0},
		"limits": []any{
			map[string]any{"window": map[string]any{"timeUnit": "TIME_UNIT_MINUTE", "duration": 300.0},
				"detail": map[string]any{"used": 96.0, "limit": 100.0}},
			map[string]any{"window": map[string]any{"timeUnit": "TIME_UNIT_DAY", "duration": 1.0},
				"detail": map[string]any{"used": 10.0, "limit": 100.0}},
		},
	})
	if got := ExhaustedWindows(quota, 0.95); len(got) != 1 {
		t.Fatalf("threshold 0.95: %d windows", len(got))
	}
	if got := ExhaustedWindows(quota, 1.0); len(got) != 0 {
		t.Fatalf("threshold 1.0: %d windows", len(got))
	}
	if got := ExhaustedWindows(nil, 0.95); got != nil {
		t.Fatalf("nil quota: %v", got)
	}
	// Auxiliary side quotas (zcode MCP 用量) never count as exhausted, even
	// over 100% — AI availability is judged by the primary windows only.
	aux := parseKimiUsages(map[string]any{
		"usage": map[string]any{"used": 10.0, "limit": 100.0},
	})
	aux.Limits = []*WindowQuota{{Name: "MCP 用量", Used: 101, Limit: 100, Auxiliary: true}}
	if got := ExhaustedWindows(aux, 0.95); len(got) != 0 {
		t.Fatalf("auxiliary window must not exhaust: %v", got)
	}
}

// setupAutoFixture: slot holds u_a (exhausted 5h window), u_b healthy, u_c
// also exhausted. Returns the manager, the auto switcher and sent
// notifications.
func setupAutoFixture(t *testing.T, resetAt string) (*Manager, *AutoSwitcher, *[]string) {
	t.Helper()
	m, home := setupSwitchFixture(t)
	if _, _, err := m.upsert("kimi", &Account{ID: "u_c", Label: "账号C", UserID: "c",
		AccessToken: "at-c", RefreshToken: "rt-c",
		ExpiresAt: m.now().Unix() + 86400, ExpiresIn: 86400}); err != nil {
		t.Fatal(err)
	}
	stubKimiAPI(t, map[string]string{
		"at-a": usagesJSON(100, 100, resetAt),
		"at-b": usagesJSON(10, 100, ""),
		"at-c": usagesJSON(99, 100, ""),
	})
	_ = home
	notes := &[]string{}
	auto := NewAutoSwitcher(m)
	auto.now = m.now
	auto.Notify = func(title, body string) { *notes = append(*notes, title+"|"+body) }
	auto.Sessions = func(kind string) SessionManager { return &fakeSM{} }
	return m, auto, notes
}

func TestAutoSwitchOnExhaustion(t *testing.T) {
	m, auto, notes := setupAutoFixture(t, "")
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "sess-1")
	if err != nil || !switched {
		t.Fatalf("expected switch, got switched=%v err=%v", switched, err)
	}
	if got := m.activeID("kimi"); got != "u_b" {
		t.Fatalf("active id: %q", got)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "已自动切换") {
		t.Fatalf("notify: %v", *notes)
	}
	history := m.History("kimi", 10)
	if len(history) != 1 || history[0].Trigger != "auto-quota" || !history[0].OK {
		t.Fatalf("history: %+v", history)
	}
}

func TestAutoSwitchSkippedWhenTransient(t *testing.T) {
	_, auto, notes := setupAutoFixture(t, "")
	// The confirm read says the active account is fine: no switch.
	stubKimiAPI(t, map[string]string{"at-a": usagesJSON(10, 100, "")})
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "sess-1")
	if err != nil || switched {
		t.Fatalf("transient failure must not switch: switched=%v err=%v", switched, err)
	}
	if len(*notes) != 0 {
		t.Fatalf("no notification expected: %v", *notes)
	}
}

func TestAutoSwitchCooldownGate(t *testing.T) {
	_, auto, _ := setupAutoFixture(t, "")
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "")
	if err != nil || !switched {
		t.Fatalf("first switch: %v %v", switched, err)
	}
	// History now holds an auto switch seconds old: the cooldown gate must
	// refuse a second one.
	if gate := auto.gate("kimi", "u_b", nil); gate != "切换冷却中" {
		t.Fatalf("cooldown gate: %q", gate)
	}
}

func TestAutoSwitchResetGraceGate(t *testing.T) {
	// Window resets 3 minutes after the fake now: waiting beats switching.
	resetAt := time.Unix(1700000000, 0).Add(3 * time.Minute).UTC().Format(time.RFC3339)
	_, auto, notes := setupAutoFixture(t, resetAt)
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "")
	if err != nil || switched {
		t.Fatalf("reset grace must block switching: switched=%v err=%v", switched, err)
	}
	if len(*notes) != 0 {
		t.Fatalf("no notification expected: %v", *notes)
	}
}

func TestAutoSwitchNoTarget(t *testing.T) {
	m, auto, notes := setupAutoFixture(t, "")
	// Only exhausted or dead accounts remain: u_b goes reauth_required, u_c
	// is exhausted per the stub.
	poolB, err := m.loadAccount("kimi", "u_b")
	if err != nil {
		t.Fatal(err)
	}
	poolB.Status = StatusReauthRequired
	if err := m.saveAccount("kimi", poolB); err != nil {
		t.Fatal(err)
	}
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "")
	if err != nil || switched {
		t.Fatalf("no target must not switch: switched=%v err=%v", switched, err)
	}
	if len(*notes) != 1 || !strings.Contains((*notes)[0], "无可用账号") {
		t.Fatalf("notify: %v", *notes)
	}
}

func TestAutoSwitchDisabled(t *testing.T) {
	m, auto, _ := setupAutoFixture(t, "")
	if err := m.SetAutoSwitch("kimi", false); err != nil {
		t.Fatal(err)
	}
	switched, err := auto.HandleQuotaExhaustion(context.Background(), "kimi", "")
	if err != nil || switched {
		t.Fatalf("disabled auto-switch must not switch: switched=%v err=%v", switched, err)
	}
	if m.AutoSwitchEnabled("kimi") {
		t.Fatal("setting not persisted")
	}
	if err := m.SetAutoSwitch("kimi", true); err != nil {
		t.Fatal(err)
	}
	if !m.AutoSwitchEnabled("kimi") {
		t.Fatal("re-enable failed")
	}
}

func TestChooseTargetPrefersHeadroom(t *testing.T) {
	m, _, _ := setupAutoFixture(t, "")
	target := m.ChooseTarget(context.Background(), "kimi", "u_a", 0.95)
	if target == nil || target.ID != "u_b" {
		t.Fatalf("target: %+v", target)
	}
}
