package accpool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestParseKimiUsages(t *testing.T) {
	payload := map[string]any{
		"usage": map[string]any{"limit": 1000.0, "resetTime": "2026-10-25T00:00:00Z"},
		"limits": []any{
			map[string]any{
				"window": map[string]any{"timeUnit": "TIME_UNIT_MINUTE", "duration": 300.0},
				"detail": map[string]any{"used": 42.0, "limit": 100.0, "resetTime": "2026-10-18T12:00:00Z"},
			},
			map[string]any{
				"name":   "legacy-flat",
				"used":   "7",
				"limit":  "10",
				"window": map[string]any{"timeUnit": "TIME_UNIT_HOUR", "duration": 5.0},
			},
		},
		"boosterWallet": map[string]any{
			"balance":            map[string]any{"type": "BOOSTER", "amount": 123456789.0, "amountLeft": 23456789.0},
			"monthlyChargeLimit": map[string]any{"currency": "CNY"},
		},
	}
	quota := parseKimiUsages(payload)
	if quota.Weekly == nil || quota.Weekly.Used != 0 || quota.Weekly.Limit != 1000 {
		t.Fatalf("weekly: %+v", quota.Weekly)
	}
	if quota.Weekly.ResetAt != "2026-10-25T00:00:00Z" {
		t.Fatalf("weekly resetAt: %q", quota.Weekly.ResetAt)
	}
	if len(quota.Limits) != 2 {
		t.Fatalf("limits: %+v", quota.Limits)
	}
	window := quota.Limits[0]
	if window.WindowUnit != "minute" || window.WindowDuration == nil || *window.WindowDuration != 300 {
		t.Fatalf("window: %+v", window)
	}
	if window.Used != 42 || window.Limit != 100 {
		t.Fatalf("window detail: %+v", window)
	}
	legacy := quota.Limits[1]
	if legacy.Name != "legacy-flat" || legacy.Used != 7 || legacy.WindowUnit != "hour" {
		t.Fatalf("legacy window: %+v", legacy)
	}
	if quota.Booster == nil || quota.Booster.TotalCents == nil || *quota.Booster.TotalCents != 123 {
		t.Fatalf("booster: %+v", quota.Booster)
	}
	if *quota.Booster.BalanceCents != 23 || quota.Booster.Currency != "CNY" {
		t.Fatalf("booster detail: %+v", quota.Booster)
	}
}

func TestParseKimiUsagesEmpty(t *testing.T) {
	quota := parseKimiUsages(nil)
	if quota.Weekly != nil || len(quota.Limits) != 0 || quota.Booster != nil {
		t.Fatalf("empty payload should yield empty quota: %+v", quota)
	}
}

func TestNeedsKimiRefresh(t *testing.T) {
	now := time.Unix(1000000, 0)
	if needsKimiRefresh(0, 0, now) {
		t.Fatal("no expiry means no refresh")
	}
	if !needsKimiRefresh(now.Unix()+200, 3600, now) {
		t.Fatal("within 300s lead should refresh")
	}
	if !needsKimiRefresh(now.Unix()+3000, 7200, now) {
		t.Fatal("within expires_in/2 lead should refresh")
	}
	if needsKimiRefresh(now.Unix()+4000, 7200, now) {
		t.Fatal("beyond lead should not refresh")
	}
}

func TestKimiGetBackoffAndAuth(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"used":1,"limit":2}}`))
	}))
	defer server.Close()
	payload, err := kimiGet(context.Background(), server.URL, "token")
	if err != nil {
		t.Fatalf("kimiGet: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 attempts, got %d", calls.Load())
	}
	if int64Or(payload["usage"].(map[string]any)["used"], 0) != 1 {
		t.Fatalf("payload: %+v", payload)
	}
}

func TestKimiGetAuthFailed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
	}))
	defer server.Close()
	_, err := kimiGet(context.Background(), server.URL, "token")
	if !isAuthFailed(err) {
		t.Fatalf("expected auth failure, got %v", err)
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := New(t.TempDir(), "")
	m.now = func() time.Time { return time.Unix(1700000000, 0) }
	return m
}

func TestUpsertDedupePriority(t *testing.T) {
	m := newTestManager(t)
	first := &Account{UserID: "u1", Email: "a@example.com", AccessToken: "at1", RefreshToken: "rt1"}
	acc, created, err := m.upsert("kimi", first)
	if err != nil || !created || acc.ID != "u_u1" {
		t.Fatalf("first upsert: created=%v id=%q err=%v", created, acc.ID, err)
	}
	// Same refresh token, different user id claim: must merge into u_u1.
	second := &Account{UserID: "other", Email: "b@example.com", AccessToken: "at2", RefreshToken: "rt1"}
	merged, created, err := m.upsert("kimi", second)
	if err != nil || created || merged.ID != "u_u1" {
		t.Fatalf("dedupe by refresh token failed: created=%v id=%q err=%v", created, merged.ID, err)
	}
	if merged.AccessToken != "at2" || merged.UserID != "other" || merged.Email != "b@example.com" {
		t.Fatalf("merge fields: %+v", merged)
	}
	// No user id and no refresh token: email match.
	third := &Account{Email: "b@example.com", AccessToken: "at3"}
	merged, created, err = m.upsert("kimi", third)
	if err != nil || created || merged.ID != "u_u1" || merged.AccessToken != "at3" {
		t.Fatalf("dedupe by email failed: created=%v acc=%+v err=%v", created, merged, err)
	}
	if got := len(m.list("kimi")); got != 1 {
		t.Fatalf("expected 1 account, got %d", got)
	}
}

func TestUpsertHashID(t *testing.T) {
	m := newTestManager(t)
	acc, created, err := m.upsert("kimi", &Account{RefreshToken: "some-refresh-token"})
	if err != nil || !created {
		t.Fatalf("upsert: %v", err)
	}
	if !strings.HasPrefix(acc.ID, "k_") || len(acc.ID) != 10 {
		t.Fatalf("hash id: %q", acc.ID)
	}
}

func TestRemove(t *testing.T) {
	m := newTestManager(t)
	acc, _, err := m.upsert("kimi", &Account{UserID: "u1", RefreshToken: "rt1"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := m.Remove("kimi", acc.ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := len(m.list("kimi")); got != 0 {
		t.Fatalf("expected 0 accounts after remove, got %d", got)
	}
	if err := m.Remove("kimi", acc.ID); err == nil {
		t.Fatal("removing a missing snapshot must fail")
	}
	if err := m.Remove("kimi", "../escape"); err == nil {
		t.Fatal("unsafe id must be rejected")
	}
	if err := m.Remove("codex", acc.ID); err == nil {
		t.Fatal("unsupported kind must be rejected")
	}
}

func TestImport(t *testing.T) {
	m := newTestManager(t)
	source := t.TempDir()
	storeDir := filepath.Join(source, "accounts-kimi")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := Account{ID: "u_abc", Label: "账号A", RefreshToken: "rt", AccessToken: "at"}
	raw, _ := json.Marshal(snapshot)
	if err := os.WriteFile(filepath.Join(storeDir, "u_abc.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// An entry without refreshToken must be rejected, garbage skipped.
	bad, _ := json.Marshal(Account{ID: "u_bad", AccessToken: "at-only"})
	_ = os.WriteFile(filepath.Join(storeDir, "u_bad.json"), bad, 0o600)
	_ = os.WriteFile(filepath.Join(storeDir, "junk.json"), []byte("{"), 0o600)

	imported, skipped, err := m.Import("kimi", source)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(imported) != 1 || imported[0] != "u_abc" {
		t.Fatalf("imported: %v", imported)
	}
	if len(skipped) != 2 {
		t.Fatalf("skipped: %v", skipped)
	}
	// Second import skips the existing snapshot.
	imported, skipped, err = m.Import("kimi", source)
	if err != nil || len(imported) != 0 {
		t.Fatalf("re-import: imported=%v err=%v", imported, err)
	}
	found := false
	for _, row := range skipped {
		if row.ID == "u_abc" && row.Reason == "已存在" {
			found = true
		}
	}
	if !found {
		t.Fatalf("re-import skipped: %v", skipped)
	}
}

func TestImportFloatQuotaSnapshot(t *testing.T) {
	m := newTestManager(t)
	source := t.TempDir()
	storeDir := filepath.Join(source, "accounts-kimi")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// zcode-switcher serializes whole numbers as floats (65.0); a strict
	// int64 decode used to reject the whole snapshot as 不是有效快照.
	raw := []byte(`{
		"id": "u_float", "label": "浮点额度", "refreshToken": "rt", "accessToken": "at",
		"quota": {
			"weekly": {"used": 65.0, "limit": 100.0, "resetAt": "2026-08-24T16:13:43Z"},
			"limits": [{"name": null, "windowUnit": "minute", "windowDuration": 300.0, "used": 1.0, "limit": 100.0, "resetAt": "2026-08-23T05:13:43Z"}],
			"booster": {"totalCents": 5000.0, "balanceCents": 1826.0, "currency": "CNY"},
			"levelName": "Allegretto", "region": "REGION_CN"
		}
	}`)
	if err := os.WriteFile(filepath.Join(storeDir, "u_float.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	imported, skipped, err := m.Import("kimi", source)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(imported) != 1 || imported[0] != "u_float" || len(skipped) != 0 {
		t.Fatalf("imported=%v skipped=%v", imported, skipped)
	}
	acc, err := m.loadAccount("kimi", "u_float")
	if err != nil {
		t.Fatalf("loadAccount: %v", err)
	}
	if acc.Quota == nil || acc.Quota.Weekly == nil || acc.Quota.Weekly.Used != 65 {
		t.Fatalf("weekly quota: %+v", acc.Quota)
	}
	if len(acc.Quota.Limits) != 1 || acc.Quota.Limits[0].Used != 1 || acc.Quota.Limits[0].Limit != 100 {
		t.Fatalf("limits: %+v", acc.Quota.Limits)
	}
	if d := acc.Quota.Limits[0].WindowDuration; d == nil || *d != 300 {
		t.Fatalf("windowDuration: %v", d)
	}
	if b := acc.Quota.Booster; b == nil || b.TotalCents == nil || *b.TotalCents != 5000 || b.BalanceCents == nil || *b.BalanceCents != 1826 {
		t.Fatalf("booster: %+v", acc.Quota.Booster)
	}
}

func TestActiveIDMatchesSlot(t *testing.T) {
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	if _, _, err := m.upsert("kimi", &Account{UserID: "u1", AccessToken: "at1", RefreshToken: "rt1"}); err != nil {
		t.Fatal(err)
	}
	if got := m.activeID("kimi"); got != "" {
		t.Fatalf("no slot file should mean no active account, got %q", got)
	}
	creds := kimiCredentials{AccessToken: "at1", RefreshToken: "rt1", TokenType: "Bearer"}
	raw, _ := json.Marshal(creds)
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.activeID("kimi"); got != "u_u1" {
		t.Fatalf("active id: %q", got)
	}
	// CLI rotated the access token but kept the refresh token.
	creds.AccessToken = "at-rotated"
	raw, _ = json.Marshal(creds)
	_ = os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), raw, 0o600)
	if got := m.activeID("kimi"); got != "u_u1" {
		t.Fatalf("active id after rotation: %q", got)
	}
}

// testKimiJWT builds an unsigned kimi-style access token carrying user_id.
func testKimiJWT(t *testing.T, userID string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, err := json.Marshal(map[string]string{"user_id": userID, "sub": userID, "type": "access"})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestKimiTokenUserID(t *testing.T) {
	if got := kimiTokenUserID(testKimiJWT(t, "uid-a")); got != "uid-a" {
		t.Fatalf("jwt user_id: %q", got)
	}
	if got := kimiTokenUserID("not-a-jwt"); got != "" {
		t.Fatalf("garbage token: %q", got)
	}
	// user_id missing → sub fallback.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"uid-b"}`))
	if got := kimiTokenUserID(header + "." + payload + ".x"); got != "uid-b" {
		t.Fatalf("sub fallback: %q", got)
	}
}

func TestActiveIDMatchesSlotUserID(t *testing.T) {
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)
	// The pool copy's token chain was rotated away long ago; only the slot
	// token's user_id claim still ties the slot to this account.
	if _, _, err := m.upsert("kimi", &Account{UserID: "uid-a", AccessToken: "at-pool", RefreshToken: "rt-pool"}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	slot, _ := json.Marshal(kimiCredentials{AccessToken: testKimiJWT(t, "uid-a"), RefreshToken: "rt-rotated", TokenType: "Bearer"})
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), slot, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.activeID("kimi"); got != "u_uid-a" {
		t.Fatalf("active id via user_id claim: %q", got)
	}
	// Non-JWT slot token falls back to the exact token match.
	fallback, _ := json.Marshal(kimiCredentials{AccessToken: "at-pool", RefreshToken: "rt-pool", TokenType: "Bearer"})
	_ = os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), fallback, 0o600)
	if got := m.activeID("kimi"); got != "u_uid-a" {
		t.Fatalf("active id via token fallback: %q", got)
	}
}

func TestPollerFiresOnActiveChange(t *testing.T) {
	m, home := setupSwitchFixture(t)
	t.Setenv("KIMI_CODE_HOME", home)
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"x","nickname":"n"}`))
	})
	mux.HandleFunc("/usages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restoreAPI, restoreToken := kimiAPIBase, kimiTokenURL
	kimiAPIBase, kimiTokenURL = server.URL, server.URL+"/token"
	defer func() { kimiAPIBase, kimiTokenURL = restoreAPI, restoreToken }()

	var fired []string
	p := &Poller{Manager: m, OnActiveChange: func(kind, fromID, toID string) {
		fired = append(fired, kind+":"+fromID+"->"+toID)
	}}
	// Slot starts on u_a (exact-token fallback matches at-a).
	p.tick(context.Background())
	if len(fired) != 0 {
		t.Fatalf("first observation must not fire: %v", fired)
	}
	// zcode-switcher moves the slot to account b.
	writeSlot(t, home, &kimiCredentials{AccessToken: testKimiJWT(t, "b"), RefreshToken: "rt-sw", TokenType: "Bearer"})
	p.tick(context.Background())
	if len(fired) != 1 || fired[0] != "kimi:u_a->u_b" {
		t.Fatalf("drift must fire once: %v", fired)
	}
	// Same account on the next tick stays quiet.
	p.tick(context.Background())
	if len(fired) != 1 {
		t.Fatalf("no drift, no fire: %v", fired)
	}
}

func TestPollerAllIntervalEnv(t *testing.T) {
	if p := NewPoller(newTestManager(t)); p.AllInterval != defaultPollAllInterval {
		t.Fatalf("default all interval: %v", p.AllInterval)
	}
	t.Setenv("GATEWAY_POOL_POLL_ALL_SECONDS", "120")
	if p := NewPoller(newTestManager(t)); p.AllInterval != 2*time.Minute {
		t.Fatalf("custom all interval: %v", p.AllInterval)
	}
	t.Setenv("GATEWAY_POOL_POLL_ALL_SECONDS", "-1")
	if p := NewPoller(newTestManager(t)); p.AllInterval > 0 {
		t.Fatalf("negative all interval should disable: %v", p.AllInterval)
	}
}

func TestPollerTickAllRefreshesStaleOnly(t *testing.T) {
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)

	var usages atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"stale","nickname":"陈旧"}`))
	})
	mux.HandleFunc("/usages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-stale" {
			t.Errorf("usages auth: %q", r.Header.Get("Authorization"))
		}
		usages.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"used":9,"limit":100},"limits":[{"window":{"timeUnit":"TIME_UNIT_MINUTE","duration":300},"detail":{"used":10,"limit":100}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restoreAPI, restoreToken := kimiAPIBase, kimiTokenURL
	kimiAPIBase, kimiTokenURL = server.URL, server.URL+"/token"
	defer func() { kimiAPIBase, kimiTokenURL = restoreAPI, restoreToken }()

	future := m.now().Add(time.Hour).Unix()
	if _, _, err := m.upsert("kimi", &Account{UserID: "stale", AccessToken: "at-stale", RefreshToken: "rt-stale", ExpiresAt: future, ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.upsert("kimi", &Account{UserID: "fresh", AccessToken: "at-fresh", RefreshToken: "rt-fresh", ExpiresAt: future, ExpiresIn: 3600, QuotaUpdatedAt: m.now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}

	// Disabled sweep is a no-op even with a stale account present.
	(&Poller{Manager: m, AllInterval: -1}).tickAll(context.Background())
	if usages.Load() != 0 {
		t.Fatalf("disabled sweep should not fetch, got %d calls", usages.Load())
	}

	(&Poller{Manager: m, AllInterval: time.Hour}).tickAll(context.Background())
	if usages.Load() != 1 {
		t.Fatalf("usages calls: %d", usages.Load())
	}
	acc, err := m.loadAccount("kimi", "u_stale")
	if err != nil {
		t.Fatal(err)
	}
	if acc.Quota == nil || acc.QuotaUpdatedAt != m.now().UnixMilli() || acc.QuotaError != "" {
		t.Fatalf("stale account after sweep: quota=%+v err=%q at=%d", acc.Quota, acc.QuotaError, acc.QuotaUpdatedAt)
	}
	fresh, err := m.loadAccount("kimi", "u_fresh")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Quota != nil {
		t.Fatalf("fresh account should be skipped: %+v", fresh.Quota)
	}
}

func TestRefreshQuotaFlow(t *testing.T) {
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("KIMI_CODE_HOME", home)

	var tokenCalls, meCalls, usageCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Errorf("form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-old" {
			t.Errorf("unexpected refresh form: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/me", func(w http.ResponseWriter, r *http.Request) {
		meCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer at-new" {
			t.Errorf("me auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"u1","nickname":"昵称","email":"a@example.com"}`))
	})
	mux.HandleFunc("/usages", func(w http.ResponseWriter, r *http.Request) {
		usageCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"used":5,"limit":100,"resetTime":"2026-10-25T00:00:00Z"},"limits":[{"window":{"timeUnit":"TIME_UNIT_MINUTE","duration":300},"detail":{"used":50,"limit":100,"resetTime":"2026-10-18T12:00:00Z"}}]}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	restoreAPI, restoreToken := kimiAPIBase, kimiTokenURL
	kimiAPIBase, kimiTokenURL = server.URL, server.URL+"/token"
	defer func() { kimiAPIBase, kimiTokenURL = restoreAPI, restoreToken }()

	// The account occupies the official slot and its token is past the
	// refresh lead, so RefreshQuota must renew it and write the slot back.
	if err := os.MkdirAll(filepath.Join(home, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	slot, _ := json.Marshal(kimiCredentials{AccessToken: "at-old", RefreshToken: "rt-old", TokenType: "Bearer"})
	if err := os.WriteFile(filepath.Join(home, "credentials", "kimi-code.json"), slot, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.upsert("kimi", &Account{
		UserID: "u1", AccessToken: "at-old", RefreshToken: "rt-old",
		ExpiresAt: m.now().Unix() + 60, ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := m.RefreshQuota(context.Background(), "kimi", "u_u1")
	if err != nil {
		t.Fatalf("RefreshQuota: %v", err)
	}
	if acc.AccessToken != "at-new" || acc.RefreshToken != "rt-new" {
		t.Fatalf("token not refreshed: %+v", acc)
	}
	if acc.Email != "a@example.com" || acc.Nickname != "昵称" {
		t.Fatalf("identity not merged: %+v", acc)
	}
	if acc.Quota == nil || acc.Quota.Weekly == nil || acc.Quota.Weekly.Limit != 100 {
		t.Fatalf("quota: %+v", acc.Quota)
	}
	if len(acc.Quota.Limits) != 1 || acc.Quota.Limits[0].Used != 50 {
		t.Fatalf("quota limits: %+v", acc.Quota.Limits)
	}
	if acc.QuotaUpdatedAt == 0 {
		t.Fatal("quotaUpdatedAt not set")
	}
	if tokenCalls.Load() != 1 || meCalls.Load() != 1 || usageCalls.Load() != 1 {
		t.Fatalf("calls: token=%d me=%d usages=%d", tokenCalls.Load(), meCalls.Load(), usageCalls.Load())
	}
	// The refreshed pair must be written back to the official slot.
	raw, err := os.ReadFile(filepath.Join(home, "credentials", "kimi-code.json"))
	if err != nil {
		t.Fatal(err)
	}
	var written kimiCredentials
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if written.AccessToken != "at-new" || written.RefreshToken != "rt-new" {
		t.Fatalf("slot not updated: %+v", written)
	}
}

func TestRefreshQuotaAuthFailed(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KIMI_CODE_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	restoreAPI, restoreToken := kimiAPIBase, kimiTokenURL
	kimiAPIBase, kimiTokenURL = server.URL, server.URL
	defer func() { kimiAPIBase, kimiTokenURL = restoreAPI, restoreToken }()

	if _, _, err := m.upsert("kimi", &Account{
		UserID: "u1", AccessToken: "at", RefreshToken: "rt",
		ExpiresAt: m.now().Unix() + 60, ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.RefreshQuota(context.Background(), "kimi", "u_u1")
	if err == nil {
		t.Fatal("expected reauth error")
	}
	acc, loadErr := m.loadAccount("kimi", "u_u1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if acc.Status != StatusReauthRequired {
		t.Fatalf("status: %q", acc.Status)
	}
}

func TestViewRedactsTokens(t *testing.T) {
	m := newTestManager(t)
	acc := &Account{ID: "u_x", Label: "x", AccessToken: "secret-at", RefreshToken: "secret-rt", ExpiresAt: 123}
	row := m.view(acc, "u_x")
	encoded := jsoncompat.EncodeCompact(row)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("view leaks credentials: %s", encoded)
	}
	if row.MustGet("active") != true || row.MustGet("hasRefreshToken") != true {
		t.Fatalf("view: %s", encoded)
	}
	if row.MustGet("tokenExpiresAt") != int64(123) {
		t.Fatalf("tokenExpiresAt: %v", row.MustGet("tokenExpiresAt"))
	}
}

func TestPollerDisabled(t *testing.T) {
	t.Setenv("GATEWAY_POOL_POLL_SECONDS", "-1")
	p := NewPoller(newTestManager(t))
	p.Start() // must be a no-op, and Stop must not block
	p.Stop()
}

func TestPollerIntervalEnv(t *testing.T) {
	t.Setenv("GATEWAY_POOL_POLL_SECONDS", "17")
	p := NewPoller(newTestManager(t))
	if p.Interval != 17*time.Second {
		t.Fatalf("interval: %v", p.Interval)
	}
	t.Setenv("GATEWAY_POOL_POLL_SECONDS", "not-a-number")
	if NewPoller(newTestManager(t)).Interval != defaultPollInterval {
		t.Fatal("invalid env should fall back to the default")
	}
}

func TestSafeID(t *testing.T) {
	if _, err := safeID("u_abc-DEF.123"); err != nil {
		t.Fatalf("valid id rejected: %v", err)
	}
	for _, bad := range []string{"", "ab", "../escape", "with space", "x;y"} {
		if _, err := safeID(bad); err == nil {
			t.Fatalf("invalid id accepted: %q", bad)
		}
	}
}
