package accpool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubWorkbuddyAPI points both variants at one stub server.
func stubWorkbuddyAPI(t *testing.T, server string) {
	t.Helper()
	restore := workbuddyEndpoints
	workbuddyEndpoints = map[string]string{"cn": server, "ai": server}
	t.Cleanup(func() { workbuddyEndpoints = restore })
}

// setupWorkbuddyFixture gives a test manager plus a WORKBUDDY_HOME sandbox,
// with the desktop process hooks stubbed to "not running".
func setupWorkbuddyFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newTestManager(t)
	home := t.TempDir()
	t.Setenv("WORKBUDDY_HOME", home)
	restoreRunning := workbuddyAppRunning
	workbuddyAppRunning = func(string) bool { return false }
	t.Cleanup(func() { workbuddyAppRunning = restoreRunning })
	return m, home
}

// workbuddyNowMS is the fixture clock (newTestManager's 1700000000s) in ms.
const workbuddyNowMS = int64(1700000000000)

func workbuddyFixtureAccount(id, uid, token string) *Account {
	return &Account{
		ID:           id,
		Label:        "账号" + id,
		UserID:       uid,
		Nickname:     "昵称" + id,
		Email:        id + "@example.com",
		Variant:      "cn",
		AccessToken:  "at-" + token,
		RefreshToken: "rt-" + token,
		TokenType:    "Bearer",
		Domain:       "codebuddy.cn",
		EnterpriseID: "ent-1",
		ExpiresAt:    workbuddyNowMS + 10*24*3600*1000, // 10 days out: no lazy refresh
		AuthRaw:      `{"auth":{"scope":"openid"}}`,
		ProfileRaw:   `{"email":"` + id + `@example.com"}`,
		Status:       StatusActive,
	}
}

// --- snapshot parsing --------------------------------------------------------------

func TestWorkbuddyImportSnapshot(t *testing.T) {
	m := newTestManager(t)
	source := t.TempDir()
	dir := filepath.Join(source, "accounts-workbuddy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A WorkBuddy 5.6 snapshot: accessToken is an encrypted envelope OBJECT,
	// expiresAt is Unix MILLISECONDS, authRaw/profileRaw ride along.
	raw := []byte(`{
		"id": "w_u1", "label": "工作号", "uid": "u1", "nickname": "buddy",
		"email": "b@example.com", "variant": "cn", "enterpriseId": "ent-9",
		"accessToken": {"$wbEncrypted": 1, "envelope": " sealed "},
		"refreshToken": "rt-plain-123", "tokenType": "Bearer",
		"domain": "codebuddy.cn", "expiresAt": 1700007200000,
		"authRaw": {"auth": {"scope": "openid"}}, "profileRaw": {"uid": "u1"}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "w_u1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// No refreshToken: rejected like the other kinds.
	bad, _ := json.Marshal(map[string]any{"id": "w_bad", "accessToken": "at-only"})
	_ = os.WriteFile(filepath.Join(dir, "w_bad.json"), bad, 0o600)

	imported, skipped, err := m.Import("workbuddy", source)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(imported) != 1 || imported[0] != "w_u1" {
		t.Fatalf("imported: %v", imported)
	}
	if len(skipped) != 1 || skipped[0].ID != "w_bad" || skipped[0].Reason != "缺少 refreshToken" {
		t.Fatalf("skipped: %+v", skipped)
	}
	acc, err := m.loadAccount("workbuddy", "w_u1")
	if err != nil {
		t.Fatal(err)
	}
	// Envelope survives as compact JSON text; expiry stays milliseconds.
	if !workbuddyEnvelope(acc.AccessToken) {
		t.Fatalf("envelope lost: %q", acc.AccessToken)
	}
	if acc.ExpiresAt != 1700007200000 {
		t.Fatalf("expiresAt must be milliseconds: %d", acc.ExpiresAt)
	}
	if acc.EnterpriseID != "ent-9" || acc.Variant != "cn" || acc.Domain != "codebuddy.cn" {
		t.Fatalf("identity: %+v", acc)
	}
	if !strings.Contains(acc.AuthRaw, `"openid"`) || !strings.Contains(acc.ProfileRaw, `"u1"`) {
		t.Fatalf("raw blobs: authRaw=%q profileRaw=%q", acc.AuthRaw, acc.ProfileRaw)
	}
	// Second import skips the existing snapshot.
	imported, skipped, err = m.Import("workbuddy", source)
	if err != nil || len(imported) != 0 {
		t.Fatalf("re-import: %v %v", imported, err)
	}
	found := false
	for _, row := range skipped {
		if row.ID == "w_u1" && row.Reason == "已存在" {
			found = true
		}
	}
	if !found {
		t.Fatalf("re-import skipped: %+v", skipped)
	}
}

func TestWorkbuddyEnvelopeAndNormTs(t *testing.T) {
	if workbuddyEnvelope("plain-token") || !workbuddyEnvelope(`{"$wbEncrypted":1,"envelope":"x"}`) {
		t.Fatal("envelope detection wrong")
	}
	if got := workbuddyPlainToken(`{"$wbEncrypted":1}`); got != "" {
		t.Fatalf("envelope must not be a plain token: %q", got)
	}
	// Seconds normalize to milliseconds; milliseconds pass through.
	if got := workbuddyNormTsMS(float64(1700000000)); got != 1700000000000 {
		t.Fatalf("seconds: %d", got)
	}
	if got := workbuddyNormTsMS("1700007200000"); got != 1700007200000 {
		t.Fatalf("millis string: %d", got)
	}
	if got := workbuddyNormTsMS(nil); got != 0 {
		t.Fatalf("nil: %d", got)
	}
}

// --- credit normalization ------------------------------------------------------------

func TestWorkbuddyResourceSummary(t *testing.T) {
	now := time.Unix(1700000000, 0)
	nowMS := now.UnixMilli()
	// Precise keys + DeductionEndTime placeholder 400 days after CycleEndTime
	// → placeholder ignored, cycle end wins.
	row := map[string]any{
		"PackageCode":                "TCACA_code_002_AkiJS3ZHF5",
		"CycleCapacitySizePrecise":   "1000.5",
		"CycleCapacityRemainPrecise": 250.0,
		"CycleCapacityUsedPrecise":   750.0,
		"CycleEndTime":               float64(nowMS + 30*24*3600*1000),
		"DeductionEndTime":           float64(nowMS + 400*24*3600*1000),
	}
	resource := workbuddyResourceSummary(row, nowMS)
	if resource.Total != 1001 || resource.Remaining != 250 || resource.Used != 750 {
		t.Fatalf("resource: %+v", resource)
	}
	if resource.PackageCode != "TCACA_code_002_AkiJS3ZHF5" {
		t.Fatalf("package code: %+v", resource)
	}
	if resource.ExpireAt != nowMS+30*24*3600*1000 {
		t.Fatalf("placeholder expiry must fall back to cycle end: %d", resource.ExpireAt)
	}
	if resource.Expired || resource.ExpiringSoon {
		t.Fatalf("flags: %+v", resource)
	}

	// A 2049-style placeholder >730 days out counts as long-valid (no expiry).
	far := map[string]any{
		"CycleCapacitySize": 100.0, "CycleCapacityRemain": 10.0,
		"CycleEndTime": float64(nowMS + 800*24*3600*1000),
	}
	if resource := workbuddyResourceSummary(far, nowMS); resource.ExpireAt != 0 {
		t.Fatalf("far future must be long-valid: %+v", resource)
	}

	// Expiring inside the 7-day highlight window.
	soon := map[string]any{
		"CycleCapacitySize": 100.0, "CycleCapacityRemain": 10.0,
		"ExpiredTime": float64(nowMS + 2*24*3600*1000),
	}
	resource = workbuddyResourceSummary(soon, nowMS)
	if !resource.ExpiringSoon || resource.Expired {
		t.Fatalf("expiringSoon flag: %+v", resource)
	}

	// SlicePeriodUsageDetails fallback for the capacity numbers.
	slice := map[string]any{
		"SlicePeriodUsageDetails": []any{map[string]any{
			"SlicePeriodCapacitySizePrecise":   50.0,
			"SlicePeriodCapacityRemainPrecise": 5.0,
		}},
		"CycleEndTime": float64(nowMS + 30*24*3600*1000),
	}
	resource = workbuddyResourceSummary(slice, nowMS)
	if resource.Total != 50 || resource.Remaining != 5 || resource.Used != 45 {
		t.Fatalf("slice fallback: %+v", resource)
	}
}

// workbuddyBillingStub serves the cn three-path family and records requests.
type workbuddyBillingStub struct {
	summaryBody, paidBody, freeBody, legacyBody string
	legacyHits                                  int
	t                                           *testing.T
	ai                                          bool
	emptyThree                                  bool
	uid                                         string
}

func (stub *workbuddyBillingStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			stub.t.Errorf("billing must be POST, got %s", r.Method)
		}
		for key, want := range map[string]string{
			"X-Client-Platform": "web",
			"Authorization":     "Bearer at-fixture",
		} {
			if got := r.Header.Get(key); got != want {
				stub.t.Errorf("header %s: got %q want %q", key, got, want)
			}
		}
		if !strings.HasSuffix(r.Header.Get("Referer"), "/profile/plans-usage") {
			stub.t.Errorf("Referer: %q", r.Header.Get("Referer"))
		}
		if got := r.Header.Get("X-User-Id"); got != stub.uid {
			stub.t.Errorf("X-User-Id: %q (want %q)", got, stub.uid)
		}
		if got := r.Header.Get("X-Enterprise-Id"); got != "ent-1" {
			stub.t.Errorf("X-Enterprise-Id: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "get-user-resource-summary"):
			stub.summaryBody = string(body)
			if stub.emptyThree {
				_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[{"PackageCode":"acct-1","CycleCapacitySizePrecise":800,"CycleCapacityRemainPrecise":200,"CycleCapacityUsedPrecise":600,"CycleEndTime":1700200000000}]}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource-paid-packages"):
			stub.paidBody = string(body)
			if stub.emptyThree {
				_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"PackageName":"Pro包","CycleCapacitySizePrecise":200,"CycleCapacityRemainPrecise":50,"CycleCapacityUsedPrecise":150,"CycleEndTime":1700300000000}]}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource-free-packages"):
			stub.freeBody = string(body)
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource"):
			stub.legacyHits++
			stub.legacyBody = string(body)
			if stub.ai && !strings.HasPrefix(r.URL.Path, "/v2") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"code":404,"message":"route missing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[{"PackageCode":"legacy-1","CycleCapacitySizePrecise":100,"CycleCapacityRemainPrecise":10,"CycleCapacityUsedPrecise":90,"CycleEndTime":1700400000000}]}}`))
		default:
			stub.t.Errorf("unexpected billing path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestWorkbuddyCreditsThreePaths(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	stub := &workbuddyBillingStub{t: t, uid: "uid-1"}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	if _, _, err := m.upsert("workbuddy", workbuddyFixtureAccount("w_uid1", "uid-1", "fixture")); err != nil {
		t.Fatal(err)
	}
	acc, err := m.RefreshQuota(context.Background(), "workbuddy", "w_uid1")
	if err != nil {
		t.Fatalf("RefreshQuota: %v", err)
	}
	if acc.Quota == nil || acc.Quota.Credits == nil {
		t.Fatalf("credits missing: %+v", acc.Quota)
	}
	credits := acc.Quota.Credits
	// 800+200 capacity, 200+50 remaining.
	if credits.TotalCapacity != 1000 || credits.TotalRemaining != 250 {
		t.Fatalf("summary: %+v", credits)
	}
	if len(credits.Resources) != 2 {
		t.Fatalf("resources: %+v", credits.Resources)
	}
	if stub.legacyHits != 0 {
		t.Fatalf("legacy fallback must not fire when the three paths return data: %d", stub.legacyHits)
	}
	// Paid request carries the fixed package code table.
	if !strings.Contains(stub.paidBody, "TCACA_code_002_AkiJS3ZHF5") || !strings.Contains(stub.paidBody, "NeedRenewInfo") {
		t.Fatalf("paid body: %s", stub.paidBody)
	}
	if !strings.Contains(stub.freeBody, "TCACA_code_008_cfWoLwvjU4") {
		t.Fatalf("free body: %s", stub.freeBody)
	}
	// Synthetic single window: used = capacity - remaining.
	if len(acc.Quota.Limits) != 1 {
		t.Fatalf("limits: %+v", acc.Quota.Limits)
	}
	window := acc.Quota.Limits[0]
	if window.Name != "积分" || window.Used != 750 || window.Limit != 1000 {
		t.Fatalf("window: %+v", window)
	}
	if window.ResetAt == "" {
		t.Fatal("resetAt must carry the soonest expiry")
	}
	if acc.QuotaUpdatedAt == 0 {
		t.Fatal("quotaUpdatedAt not set")
	}
}

func TestWorkbuddyCreditsLegacyFallbacks(t *testing.T) {
	// cn: three paths all empty → legacy get-user-resource.
	m, _ := setupWorkbuddyFixture(t)
	stub := &workbuddyBillingStub{t: t, uid: "uid-1", emptyThree: true}
	server := httptest.NewServer(stub.handler())
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)
	if _, _, err := m.upsert("workbuddy", workbuddyFixtureAccount("w_uid1", "uid-1", "fixture")); err != nil {
		t.Fatal(err)
	}
	acc, err := m.RefreshQuota(context.Background(), "workbuddy", "w_uid1")
	if err != nil {
		t.Fatalf("RefreshQuota: %v", err)
	}
	if stub.legacyHits != 1 {
		t.Fatalf("legacy hits: %d", stub.legacyHits)
	}
	if !strings.Contains(stub.legacyBody, "p_tcaca") {
		t.Fatalf("legacy body: %s", stub.legacyBody)
	}
	if acc.Quota == nil || acc.Quota.Credits == nil || acc.Quota.Credits.TotalCapacity != 100 {
		t.Fatalf("quota: %+v", acc.Quota)
	}

	// ai: the /v2-stripped path 404s, the /v2 fallback answers.
	m2, _ := setupWorkbuddyFixture(t)
	stub2 := &workbuddyBillingStub{t: t, uid: "uid-2", ai: true}
	server2 := httptest.NewServer(stub2.handler())
	defer server2.Close()
	stubWorkbuddyAPI(t, server2.URL)
	aiAccount := workbuddyFixtureAccount("w_uid2", "uid-2", "fixture")
	aiAccount.Variant = "ai"
	aiAccount.Domain = "x.workbuddy.ai"
	if _, _, err := m2.upsert("workbuddy", aiAccount); err != nil {
		t.Fatal(err)
	}
	acc2, err := m2.RefreshQuota(context.Background(), "workbuddy", "w_uid2")
	if err != nil {
		t.Fatalf("ai RefreshQuota: %v", err)
	}
	if stub2.legacyHits != 2 {
		t.Fatalf("ai legacy hits (404 + fallback): %d", stub2.legacyHits)
	}
	if acc2.Quota == nil || acc2.Quota.Credits == nil || acc2.Quota.Credits.TotalCapacity != 100 {
		t.Fatalf("ai quota: %+v", acc2.Quota)
	}
}

// --- token refresh -----------------------------------------------------------------------

func TestWorkbuddyRefreshTokenHeaders(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	var gotHeaders map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = map[string]string{
			"Authorization":         r.Header.Get("Authorization"),
			"X-Refresh-Token":       r.Header.Get("X-Refresh-Token"),
			"X-Auth-Refresh-Source": r.Header.Get("X-Auth-Refresh-Source"),
			"X-User-Id":             r.Header.Get("X-User-Id"),
			"X-Enterprise-Id":       r.Header.Get("X-Enterprise-Id"),
			"X-Tenant-Id":           r.Header.Get("X-Tenant-Id"),
			"X-Domain":              r.Header.Get("X-Domain"),
		}
		body, _ := io.ReadAll(r.Body)
		if strings.TrimSpace(string(body)) != "{}" {
			t.Errorf("refresh body must be {}, got %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"at-rotated","refreshToken":"rt-rotated","expiresIn":7200,"refreshExpiresIn":86400}}`))
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_uid1", "uid-1", "old")
	if err := refreshWorkbuddyToken(context.Background(), acc, m.now()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := map[string]string{
		"Authorization":         "Bearer at-old",
		"X-Refresh-Token":       "rt-old",
		"X-Auth-Refresh-Source": "plugin",
		"X-User-Id":             "uid-1",
		"X-Enterprise-Id":       "ent-1",
		"X-Tenant-Id":           "ent-1",
		"X-Domain":              "codebuddy.cn",
	}
	for key, value := range want {
		if gotHeaders[key] != value {
			t.Fatalf("header %s: got %q want %q", key, gotHeaders[key], value)
		}
	}
	if acc.AccessToken != "at-rotated" || acc.RefreshToken != "rt-rotated" {
		t.Fatalf("rotated tokens: %+v", acc)
	}
	// expiresIn seconds → millisecond expiries relative to the fixture clock.
	if acc.ExpiresAt != workbuddyNowMS+7200*1000 || acc.RefreshExpiresAt != workbuddyNowMS+86400*1000 {
		t.Fatalf("expiries: at=%d rt=%d", acc.ExpiresAt, acc.RefreshExpiresAt)
	}
	if acc.Status != StatusActive {
		t.Fatalf("status: %q", acc.Status)
	}
}

func TestWorkbuddyRefreshTokenUnauthorized(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":401,"message":"token 已过期"}`))
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_uid1", "uid-1", "old")
	err := refreshWorkbuddyToken(context.Background(), acc, m.now())
	if !isAuthFailed(err) {
		t.Fatalf("expected auth failure, got %v", err)
	}
	if acc.Status != StatusReauthRequired {
		t.Fatalf("status: %+v", acc)
	}
}

func TestWorkbuddyEnvelopeQuotaShortCircuits(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("envelope account must not hit the API")
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_uid1", "uid-1", "old")
	acc.AccessToken = `{"$wbEncrypted":1,"envelope":"sealed"}`
	if _, _, err := m.upsert("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	updated, err := m.RefreshQuota(context.Background(), "workbuddy", "w_uid1")
	if err == nil {
		t.Fatal("envelope quota refresh must fail")
	}
	if updated.QuotaError == "" {
		t.Fatalf("quotaError not recorded: %+v", updated)
	}
	if availableForSwitch("workbuddy", updated, 0.95) {
		t.Fatal("envelope account must not be a switch candidate after the failed refresh")
	}
}

// --- switch ---------------------------------------------------------------------------------

func workbuddyWriteSlot(t *testing.T, acc *Account) {
	t.Helper()
	if err := writeWorkbuddyAuthFile(acc, workbuddyVariantFromAccount(acc), time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestWorkbuddySwitchWritesSlotAndCLI(t *testing.T) {
	m, home := setupWorkbuddyFixture(t)
	// helper validation runs through this stub during the CLI takeover.
	restoreHelper := workbuddyRunHelper
	workbuddyRunHelper = func() (string, error) { return "Bearer at-new\n", nil }
	t.Cleanup(func() { workbuddyRunHelper = restoreHelper })

	outgoing := workbuddyFixtureAccount("w_uold", "u-old", "old")
	if err := m.saveAccount("workbuddy", outgoing); err != nil {
		t.Fatal(err)
	}
	workbuddyWriteSlot(t, outgoing)
	target := workbuddyFixtureAccount("w_unew", "u-new", "new")
	if err := m.saveAccount("workbuddy", target); err != nil {
		t.Fatal(err)
	}
	// A logged-out marker must be cleared by the switch.
	marker := workbuddyAuthFilePath("cn") + ".logged-out"
	if err := os.WriteFile(marker, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp/proj"}}
	if err := m.Switch(context.Background(), "workbuddy", "w_unew", "manual", sessions, nil); err != nil {
		t.Fatalf("switch: %v", err)
	}

	// Desktop slot: four segments, target tokens, both uids in allAccounts.
	root := readWorkbuddyAuthFile("cn")
	if root == nil {
		t.Fatal("slot missing")
	}
	auth := objectValue(root, "auth")
	if workbuddyPlainToken(workbuddySecretText(auth, "accessToken")) != "at-new" {
		t.Fatalf("slot token: %+v", auth)
	}
	if int64Or(auth["expiresAt"], 0) != target.ExpiresAt {
		t.Fatalf("slot expiry must be milliseconds: %+v", auth["expiresAt"])
	}
	if int64Or(auth["lastRefreshTime"], 0) == 0 {
		t.Fatalf("lastRefreshTime missing: %+v", auth)
	}
	accountObj := objectValue(root, "account")
	if stringValue(accountObj, "uid") != "u-new" || stringValue(accountObj, "type") != "personal" {
		t.Fatalf("account segment: %+v", accountObj)
	}
	all, _ := root["allAccounts"].([]any)
	if len(all) != 2 {
		t.Fatalf("allAccounts must merge by uid: %+v", root["allAccounts"])
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("logged-out marker must be removed")
	}

	// Marker + backup.
	markerRaw, err := os.ReadFile(m.workbuddyCurrentMarkerPath())
	if err != nil || strings.TrimSpace(string(markerRaw)) != "w_unew" {
		t.Fatalf("current marker: %q %v", markerRaw, err)
	}
	backups, err := os.ReadDir(filepath.Join(m.storeDir("workbuddy"), "backups"))
	if err != nil || len(backups) == 0 {
		t.Fatalf("backup missing: %v", err)
	}

	// CLI side: state.json, settings.json (apiKeyHelper + env), caches.
	state := readMinimaxJSON(workbuddyCLIStatePath())
	if state == nil || stringValue(state, "activeAccountId") != "w_unew" {
		t.Fatalf("cli state: %+v", state)
	}
	settings := readMinimaxJSON(workbuddySettingsPath())
	if settings == nil || settings["apiKeyHelper"] != workbuddyHelperPath() {
		t.Fatalf("settings apiKeyHelper: %+v", settings)
	}
	env := objectValue(settings, "env")
	if env["CODEBUDDY_INTERNET_ENVIRONMENT"] != "internal" {
		t.Fatalf("cn env: %+v", env)
	}
	if _, present := env["CODEBUDDY_BASE_URL"]; present {
		t.Fatalf("cn env must not carry CODEBUDDY_BASE_URL: %+v", env)
	}
	cacheDir := filepath.Join(home, ".codebuddy", "local_storage")
	envCache, _ := os.ReadFile(filepath.Join(cacheDir, workbuddyEnvCacheFile))
	if strings.TrimSpace(string(envCache)) != `"internal"` {
		t.Fatalf("env cache: %q", envCache)
	}
	endpointCache, _ := os.ReadFile(filepath.Join(cacheDir, workbuddyEndpointCacheFile))
	if strings.TrimSpace(string(endpointCache)) != `"https://copilot.tencent.com"` {
		t.Fatalf("endpoint cache: %q", endpointCache)
	}

	// History: the switch itself + the CLI takeover note.
	history := m.History("workbuddy", 10)
	if len(history) != 2 || !history[0].OK || history[0].To != "w_unew" {
		t.Fatalf("history: %+v", history)
	}
	if history[1].Trigger != "cli-takeover" || !history[1].OK {
		t.Fatalf("takeover history: %+v", history[1])
	}
	if got := m.activeID("workbuddy"); got != "w_unew" {
		t.Fatalf("active id: %q", got)
	}
}

func TestWorkbuddySwitchRollbackOnReattachFailure(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	restoreHelper := workbuddyRunHelper
	workbuddyRunHelper = func() (string, error) { return "Bearer at-new\n", nil }
	t.Cleanup(func() { workbuddyRunHelper = restoreHelper })

	outgoing := workbuddyFixtureAccount("w_uold", "u-old", "old")
	if err := m.saveAccount("workbuddy", outgoing); err != nil {
		t.Fatal(err)
	}
	workbuddyWriteSlot(t, outgoing)
	slotBefore, _ := os.ReadFile(workbuddyAuthFilePath("cn"))
	target := workbuddyFixtureAccount("w_unew", "u-new", "new")
	if err := m.saveAccount("workbuddy", target); err != nil {
		t.Fatal(err)
	}

	sessions := &fakeSM{live: map[string]string{"sess-1": "/tmp"}, failOn: map[string]bool{"sess-1": true}}
	err := m.Switch(context.Background(), "workbuddy", "w_unew", "manual", sessions, nil)
	if err == nil || !strings.Contains(err.Error(), "已恢复原账号") {
		t.Fatalf("expected rollback error, got %v", err)
	}
	slotAfter, _ := os.ReadFile(workbuddyAuthFilePath("cn"))
	if string(slotAfter) != string(slotBefore) {
		t.Fatalf("slot not restored:\n%s\nvs\n%s", slotAfter, slotBefore)
	}
	if got := m.activeID("workbuddy"); got != "w_uold" {
		t.Fatalf("active id after rollback: %q", got)
	}
}

func TestWorkbuddyAutoSwitchGateLiveSession(t *testing.T) {
	m, home := setupWorkbuddyFixture(t)
	// Desktop app is running for this test.
	workbuddyAppRunning = func(string) bool { return true }
	// A fresh CLI session heartbeat (120s window).
	sessionsDir := workbuddyCLISessionsDir()
	if err := os.MkdirAll(sessionsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	session := map[string]any{"lastHeartbeat": workbuddyNowMS - 30_000}
	raw, _ := json.Marshal(session)
	if err := os.WriteFile(filepath.Join(sessionsDir, "s1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = home

	outgoing := workbuddyFixtureAccount("w_uold", "u-old", "old")
	if err := m.saveAccount("workbuddy", outgoing); err != nil {
		t.Fatal(err)
	}
	workbuddyWriteSlot(t, outgoing)
	target := workbuddyFixtureAccount("w_unew", "u-new", "new")
	if err := m.saveAccount("workbuddy", target); err != nil {
		t.Fatal(err)
	}

	err := m.Switch(context.Background(), "workbuddy", "w_unew", "auto-quota", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "自动切换已跳过") {
		t.Fatalf("expected conservative gate refusal, got %v", err)
	}
	if got := m.activeID("workbuddy"); got != "w_uold" {
		t.Fatalf("slot must be untouched: %q", got)
	}
}

// --- capture / active id -----------------------------------------------------------------------

func TestWorkbuddyCaptureAndActiveID(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	slotAccount := workbuddyFixtureAccount("w_uold", "u-old", "old")
	workbuddyWriteSlot(t, slotAccount)

	if got := m.activeID("workbuddy"); got != "" {
		t.Fatalf("no pooled account yet: %q", got)
	}
	acc, created, err := m.CaptureCurrent(context.Background(), "workbuddy")
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if !created || acc.ID != "w_u-old" {
		t.Fatalf("capture: created=%v acc=%+v", created, acc)
	}
	if acc.Variant != "cn" || acc.UserID != "u-old" || acc.AccessToken != "at-old" {
		t.Fatalf("captured: %+v", acc)
	}
	if got := m.activeID("workbuddy"); got != "w_u-old" {
		t.Fatalf("active id: %q", got)
	}
	// Recapturing merges instead of duplicating.
	_, created, err = m.CaptureCurrent(context.Background(), "workbuddy")
	if err != nil || created {
		t.Fatalf("re-capture: created=%v err=%v", created, err)
	}
	if got := len(m.list("workbuddy")); got != 1 {
		t.Fatalf("accounts: %d", got)
	}
}

func TestWorkbuddyEnvelopeProtection(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	now := time.Unix(1700000000, 0)
	// Existing record holds an unexpired PLAIN token.
	existing := workbuddyFixtureAccount("w_uold", "u-old", "plain")
	if _, _, err := m.upsert("workbuddy", existing); err != nil {
		t.Fatal(err)
	}
	// A capture arrives with an envelope token for the same uid: must NOT
	// overwrite the plain credential.
	candidate := workbuddyFixtureAccount("w_uold2", "u-old", "sealed")
	candidate.AccessToken = `{"$wbEncrypted":1,"envelope":"x"}`
	merged, updated, err := m.upsertWorkbuddy(candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	if updated || workbuddyPlainToken(merged.AccessToken) != "at-plain" {
		t.Fatalf("envelope protection: updated=%v acc=%+v", updated, merged)
	}
}
