package accpool

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubMinimaxAPI points every MiniMax origin at one stub server (origin maps
// keyed by region; restore afterwards).
func stubMinimaxAPI(t *testing.T, server string) {
	restoreAccount := minimaxAccountOrigins
	restoreMatrix := minimaxMatrixOrigins
	restoreOpen := minimaxOpenOrigins
	minimaxAccountOrigins = map[string]string{"cn": server, "en": server}
	minimaxMatrixOrigins = map[string]string{"cn": server, "en": server}
	minimaxOpenOrigins = map[string]string{"cn": server, "en": server}
	t.Cleanup(func() {
		minimaxAccountOrigins = restoreAccount
		minimaxMatrixOrigins = restoreMatrix
		minimaxOpenOrigins = restoreOpen
	})
}

func setupMinimaxFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("MINIMAX_DATA_DIR", home)
	return m, home
}

func minimaxFixtureAccount(id, token string) *Account {
	return &Account{
		ID:           id,
		Label:        "账号" + id,
		UserID:       strings.TrimPrefix(id, "u_"),
		Email:        id + "@example.com",
		Nickname:     "昵称" + id,
		Region:       "cn",
		AccessToken:  "at-" + token,
		RefreshToken: "rt-" + token,
		TokenType:    "Bearer",
		ExpiresAt:    1700000000 + 36000, // far future: no refresh during switch
		ExpiresIn:    3600,
		Status:       StatusActive,
	}
}

// --- snapshot parsing (seconds expiresAt) ---------------------------------------

func TestMinimaxImportSnapshot(t *testing.T) {
	m := newTestManager(t)
	source := t.TempDir()
	dir := filepath.Join(source, "accounts-minimax")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{
		"id": "u_123", "label": "MiniMax账号", "email": "m@example.com",
		"nickname": "Max", "subUserName": "子账号",
		"region": "cn", "accessToken": "at-x", "refreshToken": "rt-x",
		"expiresAt": 1700000600, "expiresIn": 3600,
		"quota": {
			"windows": [{"key": "fiveHour", "remainingPercent": 60}],
			"membership": {"hasTokenPlan": false, "opGroupId": "", "tier": "", "creditBalance": null}
		},
		"quotaUpdatedAt": 1700000000000
	}`)
	if err := os.WriteFile(filepath.Join(dir, "u_123.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// A snapshot without refreshToken must be rejected like kimi's.
	bad, _ := json.Marshal(Account{ID: "u_bad", AccessToken: "at-only"})
	_ = os.WriteFile(filepath.Join(dir, "u_bad.json"), bad, 0o600)

	imported, skipped, err := m.Import("minimax", source)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(imported) != 1 || imported[0] != "u_123" {
		t.Fatalf("imported: %v", imported)
	}
	if len(skipped) != 1 || skipped[0].ID != "u_bad" {
		t.Fatalf("skipped: %+v", skipped)
	}
	acc, err := m.loadAccount("minimax", "u_123")
	if err != nil {
		t.Fatal(err)
	}
	// expiresAt stays Unix SECONDS, like kimi's slot credentials.
	if acc.ExpiresAt != 1700000600 {
		t.Fatalf("expiresAt must be seconds: %d", acc.ExpiresAt)
	}
	if acc.SubUserName != "子账号" || acc.Region != "cn" {
		t.Fatalf("identity: %+v", acc)
	}
	if acc.Quota == nil || acc.Quota.Membership == nil {
		t.Fatalf("membership lost: %+v", acc.Quota)
	}
	if acc.Quota.Membership.HasTokenPlan == nil || *acc.Quota.Membership.HasTokenPlan {
		t.Fatalf("hasTokenPlan must import as explicit false: %+v", acc.Quota.Membership)
	}
	// The round-trip save must keep the snapshot valid and kimi fields absent.
	if err := m.saveAccount("minimax", acc); err != nil {
		t.Fatal(err)
	}
	again, err := m.loadAccount("minimax", "u_123")
	if err != nil || again.ExpiresAt != 1700000600 {
		t.Fatalf("round trip: %v %+v", err, again)
	}
}

// --- record key / fingerprint ----------------------------------------------------

func TestMinimaxRecordKey(t *testing.T) {
	_, home := setupMinimaxFixture(t)
	authHome := filepath.Join(home, "auth")
	sum := sha256.Sum256([]byte(authHome + "\x00mcode-public"))
	want := "com.minimax.mcode.oauth.prod.cn\x00" + base64.RawURLEncoding.EncodeToString(sum[:])
	if got := minimaxRecordKey("cn"); got != want {
		t.Fatalf("recordKey:\n got %q\nwant %q", got, want)
	}
}

func TestMinimaxProjectionFingerprint(t *testing.T) {
	acc := &Account{
		AccessToken: "at-b",
		UserID:      "u-b",
		Email:       "b@example.com",
		Nickname:    "贝贝",
	}
	// Fixed storage.ts key order: accessToken, realUserID, userEmail,
	// userName, subUserName (empty values dropped).
	const wire = `{"accessToken":"at-b","realUserID":"u-b","userEmail":"b@example.com","userName":"贝贝"}`
	if got := minimaxProjectionAuthJSON(acc); got != wire {
		t.Fatalf("projection auth:\n got %s\nwant %s", got, wire)
	}
	sum := sha256.Sum256([]byte(wire))
	want := fmt.Sprintf("sha256:%x", sum)
	if got := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(minimaxProjectionAuthJSON(acc)))); got != want {
		t.Fatalf("fingerprint: %s", got)
	}
}

// --- remains normalization ---------------------------------------------------------

func remainsPayload(model map[string]any) map[string]any {
	return map[string]any{"base_resp": map[string]any{"status_code": 0}, "model_remains": []any{model}}
}

func TestMinimaxRemainsPercentToWindows(t *testing.T) {
	payload := remainsPayload(map[string]any{
		"model_name": "m2",
		"current_interval_remaining_percent": 4.0,
		"end_time":                           1700003600000.0,
		"current_weekly_remaining_percent":   80.0,
		"weekly_end_time":                    1700611200000.0,
	})
	membership := &minimaxMembership{OpGroupID: "g-1", Tier: "Pro"}
	quota := parseMinimaxRemains(payload, membership)
	if len(quota.Limits) != 1 {
		t.Fatalf("limits: %+v", quota.Limits)
	}
	window := quota.Limits[0]
	// Remaining percent inverts into used/limit: used = 100 - 4.
	if window.Used != 96 || window.Limit != 100 {
		t.Fatalf("five-hour window: %+v", window)
	}
	if window.WindowUnit != "minute" || window.WindowDuration == nil || *window.WindowDuration != 300 {
		t.Fatalf("window shape: %+v", window)
	}
	if window.ResetAt == "" {
		t.Fatal("resetAt missing")
	}
	if quota.Weekly == nil || quota.Weekly.Used != 20 || quota.Weekly.Limit != 100 {
		t.Fatalf("weekly window: %+v", quota.Weekly)
	}
	if quota.LevelName != "Pro" || quota.Membership == nil || quota.Membership.OpGroupID != "g-1" {
		t.Fatalf("membership/tier: %+v", quota)
	}
	// An over-95% window reads exhausted for the auto-switch gate.
	if got := ExhaustedWindows(quota, 0.95); len(got) != 1 || got[0] != window {
		t.Fatalf("exhausted: %+v", got)
	}
}

func TestMinimaxRemainsUnlimitedAndFallback(t *testing.T) {
	// status == 3 (unlimited) drops the window from the quota entirely.
	unlimited := remainsPayload(map[string]any{
		"current_interval_status":            3.0,
		"current_interval_remaining_percent": 0.0,
		"current_weekly_remaining_percent":   55.0,
	})
	quota := parseMinimaxRemains(unlimited, nil)
	if len(quota.Limits) != 0 {
		t.Fatalf("unlimited five-hour must be dropped: %+v", quota.Limits)
	}
	if quota.Weekly == nil || quota.Weekly.Used != 45 {
		t.Fatalf("weekly: %+v", quota.Weekly)
	}

	// No percent fields: usage_count/total_count converts like api.rs.
	fallback := remainsPayload(map[string]any{
		"current_interval_total_count": 200.0,
		"current_interval_usage_count": 150.0,
	})
	quota = parseMinimaxRemains(fallback, nil)
	if len(quota.Limits) != 1 || quota.Limits[0].Used != 25 || quota.Limits[0].Limit != 100 {
		t.Fatalf("usage_count fallback: %+v", quota.Limits)
	}

	// base_resp.status_code != 0: no windows at all.
	bad := map[string]any{"base_resp": map[string]any{"status_code": 1000}, "model_remains": []any{map[string]any{}}}
	if quota := parseMinimaxRemains(bad, nil); len(quota.Limits) != 0 || quota.Weekly != nil {
		t.Fatalf("bad status_code must yield empty quota: %+v", quota)
	}
}

func TestMinimaxNeedsRefresh(t *testing.T) {
	now := time.Unix(1700000000, 0)
	if needsMinimaxRefresh(0, 0, now) {
		t.Fatal("no expiry means no refresh")
	}
	if !needsMinimaxRefresh(now.Unix()+200, 3600, now) {
		t.Fatal("within 300s lead should refresh")
	}
	if !needsMinimaxRefresh(now.Unix()+3000, 7200, now) {
		t.Fatal("within expires_in/2 lead should refresh")
	}
	if needsMinimaxRefresh(now.Unix()+4000, 7200, now) {
		t.Fatal("beyond lead should not refresh")
	}
}

// --- refresh quota flow (httptest) -------------------------------------------------

// minimaxQuotaMux stubs the full MiniMax API family and counts hits.
type minimaxQuotaMux struct {
	tokenHits    int
	infoHits     int
	extraHits    int
	remainsHits  int
	tokenHandler func(w http.ResponseWriter, r *http.Request)
}

func (stub *minimaxQuotaMux) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/token"):
			stub.tokenHits++
			if stub.tokenHandler != nil {
				stub.tokenHandler(w, r)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("form: %v", err)
			}
			for key, want := range map[string]string{
				"grant_type": "refresh_token", "refresh_token": "rt-old",
				"client_id": "mcode-public", "scope": "agent.default", "audience": "agent-backend",
			} {
				if got := r.Form.Get(key); got != want {
					t.Errorf("token form %s: got %q want %q", key, got, want)
				}
			}
			_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`))
		case strings.HasPrefix(r.URL.Path, "/v1/api/user/info"):
			stub.infoHits++
			if auth := r.Header.Get("authorization"); auth != "Bearer at-new" {
				t.Errorf("user/info auth: %q", auth)
			}
			if r.Header.Get("yy") == "" || r.Header.Get("x-timestamp") == "" || r.Header.Get("x-signature") == "" {
				t.Error("matrix signature headers missing")
			}
			if r.Method != http.MethodGet {
				t.Errorf("user/info must be GET, got %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"data":{"userInfo":{"realUserID":"u-1","userName":"马克斯","userEmail":"m@example.com","subUserName":"子账号"}}}`))
		case strings.HasSuffix(r.URL.Path, "/get_user_extra_info"):
			stub.extraHits++
			if r.Method != http.MethodPost {
				t.Errorf("extra_info must be POST, got %s", r.Method)
			}
			_, _ = w.Write([]byte(`{"workspaces":[{"workspace_type":0,"workspace_id":"ws-1","has_token_plan":true,"op_group_id":"grp-1","token_plan_tier":"Pro","op_credit_summary":{"total_remaining_amount":"1234"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/coding_plan/remains"):
			stub.remainsHits++
			if got := r.Header.Get("x-group-id"); got != "grp-1" {
				t.Errorf("x-group-id: %q", got)
			}
			_, _ = w.Write([]byte(`{"base_resp":{"status_code":0},"model_remains":[{"model_name":"m2","current_interval_remaining_percent":37.0,"end_time":1700003600000,"current_weekly_remaining_percent":80.0,"weekly_end_time":1700611200000}]}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestMinimaxRefreshQuotaFlow(t *testing.T) {
	m, _ := setupMinimaxFixture(t)
	stub := &minimaxQuotaMux{}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	stubMinimaxAPI(t, server.URL)

	// The account occupies the slot; its token is inside the refresh lead.
	current := minimaxFixtureAccount("u_a", "old")
	current.ExpiresAt = 1700000000 + 60
	current.ExpiresIn = 3600
	if err := m.writeMinimaxSlot(current, true); err != nil {
		t.Fatal(err)
	}
	slotBefore := readMinimaxJSON(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	epochBefore := stringValue(minimaxSlotRecord(slotBefore, "cn"), "loginEpoch")

	if _, _, err := m.upsert("minimax", &Account{
		UserID: "1", AccessToken: "at-old", RefreshToken: "rt-old",
		Region: "cn", ExpiresAt: 1700000000 + 60, ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}

	acc, err := m.RefreshQuota(context.Background(), "minimax", "u_1")
	if err != nil {
		t.Fatalf("RefreshQuota: %v", err)
	}
	if acc.AccessToken != "at-new" || acc.RefreshToken != "rt-new" {
		t.Fatalf("token not refreshed: %+v", acc)
	}
	if acc.Nickname != "马克斯" || acc.Email != "m@example.com" || acc.SubUserName != "子账号" {
		t.Fatalf("identity: %+v", acc)
	}
	if acc.Quota == nil || len(acc.Quota.Limits) != 1 || acc.Quota.Limits[0].Used != 63 {
		t.Fatalf("quota limits: %+v", acc.Quota)
	}
	if acc.Quota.Weekly == nil || acc.Quota.Weekly.Used != 20 {
		t.Fatalf("quota weekly: %+v", acc.Quota.Weekly)
	}
	if acc.Quota.LevelName != "Pro" || acc.Quota.Membership == nil ||
		acc.Quota.Membership.OpGroupID != "grp-1" || acc.Quota.Membership.CreditBalance != "1234" {
		t.Fatalf("membership: %+v", acc.Quota)
	}
	if acc.QuotaUpdatedAt == 0 || acc.QuotaError != "" {
		t.Fatalf("bookkeeping: %+v", acc)
	}
	if stub.tokenHits != 1 || stub.infoHits != 1 || stub.extraHits != 1 || stub.remainsHits != 1 {
		t.Fatalf("hits: token=%d info=%d extra=%d remains=%d", stub.tokenHits, stub.infoHits, stub.extraHits, stub.remainsHits)
	}
	// The rotated pair must be written back to the official slot, keeping the
	// loginEpoch (pure renewal, not an account switch).
	slotAfter := readMinimaxJSON(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	record := minimaxSlotRecord(slotAfter, "cn")
	if stringValue(record, "accessToken") != "at-new" || stringValue(record, "refreshToken") != "rt-new" {
		t.Fatalf("slot not written back: %+v", record)
	}
	if stringValue(record, "loginEpoch") != epochBefore {
		t.Fatalf("renewal must keep the loginEpoch: %q -> %q", epochBefore, stringValue(record, "loginEpoch"))
	}
	if int64Or(record["expiresAtMs"], 0) != acc.ExpiresAt*1000 {
		t.Fatalf("expiresAtMs must be seconds*1000: %+v", record)
	}
}

func TestMinimaxRefreshQuotaReauthOnInvalidGrant(t *testing.T) {
	m, _ := setupMinimaxFixture(t)
	stub := &minimaxQuotaMux{tokenHandler: func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}}
	server := httptest.NewServer(stub.handler(t))
	defer server.Close()
	stubMinimaxAPI(t, server.URL)

	if _, _, err := m.upsert("minimax", &Account{
		UserID: "1", AccessToken: "at", RefreshToken: "rt",
		Region: "cn", ExpiresAt: 1700000000 + 60, ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.RefreshQuota(context.Background(), "minimax", "u_1")
	if err == nil || !strings.Contains(err.Error(), "reauth") {
		t.Fatalf("expected reauth error, got %v", err)
	}
	acc, loadErr := m.loadAccount("minimax", "u_1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if acc.Status != StatusReauthRequired {
		t.Fatalf("status: %q", acc.Status)
	}
	if availableForSwitch("minimax", acc, 0.95) {
		t.Fatal("reauth account must not be switchable")
	}
}

func TestMinimaxRefreshQuotaNoCodingPlan(t *testing.T) {
	m, _ := setupMinimaxFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/oauth2/token"):
			_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`))
		case strings.HasPrefix(r.URL.Path, "/v1/api/user/info"):
			_, _ = w.Write([]byte(`{"data":{"userInfo":{"realUserID":"u-1"}}}`))
		case strings.HasSuffix(r.URL.Path, "/get_user_extra_info"):
			// Personal workspace without a Coding Plan: no op_group_id.
			_, _ = w.Write([]byte(`{"workspaces":[{"workspace_type":0,"workspace_id":"ws-1","has_token_plan":false}]}`))
		default:
			t.Errorf("coding_plan/remains must not be called without an opGroupId: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	stubMinimaxAPI(t, server.URL)

	if _, _, err := m.upsert("minimax", &Account{
		UserID: "1", AccessToken: "at", RefreshToken: "rt",
		Region: "cn", ExpiresAt: 1700000000 + 60, ExpiresIn: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	acc, err := m.RefreshQuota(context.Background(), "minimax", "u_1")
	if err != nil {
		t.Fatalf("RefreshQuota: %v", err)
	}
	if acc.Quota == nil || acc.Quota.Membership == nil || acc.Quota.Membership.HasTokenPlan == nil || *acc.Quota.Membership.HasTokenPlan {
		t.Fatalf("hasTokenPlan=false must be preserved: %+v", acc.Quota)
	}
}

// --- capture current ---------------------------------------------------------------

func TestMinimaxCaptureCurrent(t *testing.T) {
	m, _ := setupMinimaxFixture(t)
	current := minimaxFixtureAccount("u_a", "slot")
	if err := m.writeMinimaxSlot(current, true); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"userInfo":{"realUserID":"a","userName":"槽位账号","userEmail":"slot@example.com"}}}`))
	}))
	defer server.Close()
	stubMinimaxAPI(t, server.URL)

	acc, created, err := m.CaptureCurrent(context.Background(), "minimax")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !created || acc.ID != "u_a" {
		t.Fatalf("capture id: created=%v acc=%+v", created, acc)
	}
	if acc.Nickname != "槽位账号" || acc.Email != "slot@example.com" || acc.Region != "cn" {
		t.Fatalf("captured identity: %+v", acc)
	}
	if got := m.activeID("minimax"); got != "u_a" {
		t.Fatalf("active id: %q", got)
	}
	// Capturing again merges into the same account.
	_, created, err = m.CaptureCurrent(context.Background(), "minimax")
	if err != nil || created {
		t.Fatalf("re-capture: created=%v err=%v", created, err)
	}
}

// --- switch ---------------------------------------------------------------------

func TestMinimaxSwitchWritesOfficialFiles(t *testing.T) {
	m, home := setupMinimaxFixture(t)
	outgoing := minimaxFixtureAccount("u_a", "old")
	if err := m.writeMinimaxSlot(outgoing, true); err != nil {
		t.Fatal(err)
	}
	target := minimaxFixtureAccount("u_b", "new")
	if err := m.saveAccount("minimax", target); err != nil {
		t.Fatal(err)
	}
	// A stale verified-identity cache must be removed by the switch.
	scopeDir := minimaxCliAuthScopeDir("cn")
	if err := os.MkdirAll(scopeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, "account-identity.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp/proj"}}
	if err := m.Switch(context.Background(), "minimax", "u_b", "manual", sessions, nil); err != nil {
		t.Fatalf("switch: %v", err)
	}

	// auth.json: new record, generation bumped, new loginEpoch.
	authRaw, err := os.ReadFile(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var authPayload struct {
		SchemaVersion int64 `json:"schemaVersion"`
	}
	_ = json.Unmarshal(authRaw, &authPayload)
	payload := readMinimaxJSON(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	record := minimaxSlotRecord(payload, "cn")
	if record == nil {
		t.Fatal("record missing after switch")
	}
	if stringValue(record, "accessToken") != "at-new" || stringValue(record, "refreshToken") != "rt-new" {
		t.Fatalf("record tokens: %+v", record)
	}
	if int64Or(record["generation"], 0) != 2 {
		t.Fatalf("generation must be previous+1: %+v", record)
	}
	if stringValue(record, "loginEpoch") == "" {
		t.Fatal("loginEpoch missing")
	}
	if !strings.Contains(string(authRaw), `"agent.default"`) || !strings.Contains(string(authRaw), `"agent-backend"`) {
		t.Fatalf("record constants missing:\n%s", authRaw)
	}

	// auth-state.json: same generation, authenticated.
	state := readMinimaxJSON(minimaxAuthStatePath("cn"))
	if state == nil || stringValue(state, "status") != "authenticated" ||
		int64Or(state["generation"], 0) != 2 || stringValue(state, "region") != "cn" {
		t.Fatalf("auth-state: %+v", state)
	}

	// cli-auth projection + root projection carry the same auth body.
	projection := readMinimaxJSON(filepath.Join(scopeDir, "local-runtime.auth.json"))
	rootProjection := readMinimaxJSON(minimaxRootProjectionPath())
	if projection == nil || rootProjection == nil {
		t.Fatal("projections missing")
	}
	auth := objectValue(projection, "auth")
	if stringValue(auth, "accessToken") != "at-new" || stringValue(auth, "realUserID") != "b" ||
		stringValue(auth, "userEmail") != "u_b@example.com" {
		t.Fatalf("projection auth: %+v", auth)
	}
	if fmt.Sprint(rootProjection["auth"]) != fmt.Sprint(projection["auth"]) {
		t.Fatalf("root projection mismatch: %+v vs %+v", rootProjection["auth"], projection["auth"])
	}
	scope := readMinimaxJSON(filepath.Join(scopeDir, "cli-auth.scope.json"))
	if scope == nil || stringValue(scope, "region") != "cn" || stringValue(scope, "buildEnv") != "prod" {
		t.Fatalf("scope: %+v", scope)
	}
	shared := readMinimaxJSON(minimaxSharedProjectionPath())
	if shared == nil || !strings.HasPrefix(stringValue(shared, "authFingerprint"), "sha256:") {
		t.Fatalf("shared projection: %+v", shared)
	}
	// The fingerprint matches the projection auth body by the Rust algorithm.
	sum := sha256.Sum256([]byte(minimaxProjectionAuthJSON(target)))
	if want := fmt.Sprintf("sha256:%x", sum); stringValue(shared, "authFingerprint") != want {
		t.Fatalf("fingerprint: got %s want %s", stringValue(shared, "authFingerprint"), want)
	}

	// Region preference + identity cache removal.
	pref := readMinimaxJSON(minimaxRegionPrefPath())
	if pref == nil || stringValue(objectValue(pref, "regions"), "prod") != "cn" {
		t.Fatalf("region pref: %+v", pref)
	}
	if _, err := os.Stat(filepath.Join(scopeDir, "account-identity.json")); !os.IsNotExist(err) {
		t.Fatal("account-identity.json must be removed")
	}

	// Permission bits: auth chain 0700, slot files 0600.
	info, err := os.Stat(filepath.Join(home, "auth"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("auth dir mode: %o", info.Mode().Perm())
	}
	fileInfo, err := os.Stat(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("auth.json mode: %o", fileInfo.Mode().Perm())
	}

	// Bookkeeping.
	if got := m.activeID("minimax"); got != "u_b" {
		t.Fatalf("active id: %q", got)
	}
	if len(sessions.reattached) != 1 || sessions.reattached[0] != "sess-1" {
		t.Fatalf("reattached: %v", sessions.reattached)
	}
	history := m.History("minimax", 10)
	if len(history) != 1 || !history[0].OK || history[0].To != "u_b" {
		t.Fatalf("history: %+v", history)
	}
}

func TestMinimaxSwitchRollbackOnReattachFailure(t *testing.T) {
	m, home := setupMinimaxFixture(t)
	outgoing := minimaxFixtureAccount("u_a", "old")
	if err := m.saveAccount("minimax", outgoing); err != nil {
		t.Fatal(err)
	}
	if err := m.writeMinimaxSlot(outgoing, true); err != nil {
		t.Fatal(err)
	}
	authBefore, _ := os.ReadFile(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	target := minimaxFixtureAccount("u_b", "new")
	if err := m.saveAccount("minimax", target); err != nil {
		t.Fatal(err)
	}

	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp"}, failOn: map[string]bool{"sess-1": true}}
	err := m.Switch(context.Background(), "minimax", "u_b", "manual", sessions, nil)
	if err == nil || !strings.Contains(err.Error(), "已恢复原账号") {
		t.Fatalf("expected rollback error, got %v", err)
	}
	authAfter, _ := os.ReadFile(filepath.Join(minimaxNSHome("cn"), "auth.json"))
	if string(authAfter) != string(authBefore) {
		t.Fatalf("slot not restored:\n%s\nvs\n%s", authAfter, authBefore)
	}
	if got := m.activeID("minimax"); got != "u_a" {
		t.Fatalf("active id after rollback: %q", got)
	}
	homeInfo, err := os.Stat(home)
	if err != nil || !homeInfo.IsDir() {
		t.Fatalf("data dir gone: %v", err)
	}
}

func TestMinimaxSwitchRejectsSameAccount(t *testing.T) {
	m, _ := setupMinimaxFixture(t)
	outgoing := minimaxFixtureAccount("u_a", "old")
	if err := m.saveAccount("minimax", outgoing); err != nil {
		t.Fatal(err)
	}
	if err := m.writeMinimaxSlot(outgoing, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Switch(context.Background(), "minimax", "u_a", "manual", nil, nil); err == nil ||
		!strings.Contains(err.Error(), "已是当前账号") {
		t.Fatalf("expected same-account refusal, got %v", err)
	}
}

// --- kind mapping ------------------------------------------------------------------

func TestPoolAgentKindMapping(t *testing.T) {
	if AgentKind("minimax") != "mcode" || AgentKind("kimi") != "kimi" || AgentKind("zcode") != "zcode" {
		t.Fatal("AgentKind mapping wrong")
	}
	if PoolKind("mcode") != "minimax" || PoolKind("kimi") != "kimi" || PoolKind("zcode") != "zcode" {
		t.Fatal("PoolKind mapping wrong")
	}
}
