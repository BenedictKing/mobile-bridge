package accpool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
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
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			stub.t.Errorf("billing must be POST (or GET for the region probe), got %s", r.Method)
		}
		// 客户端形态：billing 域单段 UA，不带网页端的 X-Client-Platform，
		// Referer 是域名根而非网页中心路径。
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "WorkBuddy/") || strings.Contains(ua, "CLI/") {
			stub.t.Errorf("billing UA must be the single-segment client form: %q", ua)
		}
		if got := r.Header.Get("X-Client-Platform"); got != "" {
			stub.t.Errorf("billing must not carry the web platform header: %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-fixture" {
			stub.t.Errorf("Authorization: %q", got)
		}
		if referer := r.Header.Get("Referer"); strings.Contains(referer, "plans-usage") || !strings.HasSuffix(referer, "/") {
			stub.t.Errorf("Referer: %q", referer)
		}
		if got := r.Header.Get("X-User-Id"); got != stub.uid {
			stub.t.Errorf("X-User-Id: %q (want %q)", got, stub.uid)
		}
		if got := r.Header.Get("X-Enterprise-Id"); got != "ent-1" {
			stub.t.Errorf("X-Enterprise-Id: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(r.URL.Path, "/auth/realms/copilot/overseas/user/register"):
			// 国际版注册状态探测（refreshWorkbuddyQuota 顺带）。
			_, _ = w.Write([]byte(`{"code":200,"msg":"ok"}`))
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

// The cn endpoints overlap: the summary rollup reports the package without a
// name, the free-package detail reports the same package again with one. The
// two reports must collapse into a single resource (they used to be summed,
// doubling the account's capacity and showing a raw package code as a row).
func TestWorkbuddyCreditsDedupeSamePackage(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "get-user-resource-summary"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"Packages":[{"PackageCode":"TCACA_code_008_cfWoLwvjU4","CycleTotalCapacity":500,"CycleRemainCapacity":494.96,"CycleUsedCapacity":5.04,"CapacityUnit":"credits","TotalCount":1}],"TotalCredits":494.96,"SubscriptionPackageName":"体验版","IsPaidUser":false}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource-paid-packages"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[],"TotalCount":0}}`))
		case strings.HasSuffix(r.URL.Path, "get-user-resource-free-packages"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"Accounts":[{"PackageCode":"TCACA_code_008_cfWoLwvjU4","PackageName":"CodeBuddy个人体验版","CycleCapacitySizePrecise":500,"CycleCapacityRemainPrecise":494.96,"CycleCapacityUsedPrecise":5.04,"CycleCapacitySize":500,"CycleCapacityRemain":494,"CycleCapacityUsed":5,"CycleStartTime":"2026-10-01 00:00:00","CycleEndTime":"2026-10-31 23:59:59","DeductionEndTime":2047680986000,"ResourceId":"codebuddy-k6e011veumjgvdt"}],"TotalCount":1}}`))
		default:
			t.Errorf("unexpected billing path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
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
	if len(credits.Resources) != 1 {
		t.Fatalf("the same package must collapse into one resource: %+v", credits.Resources)
	}
	if credits.TotalCapacity != 500 || credits.TotalRemaining != 495 {
		t.Fatalf("capacity must not double-count the package: %+v", credits)
	}
	resource := credits.Resources[0]
	if resource.PackageName != "CodeBuddy个人体验版" {
		t.Fatalf("package name not merged from the detail endpoint: %+v", resource)
	}
	if resource.PackageCode != "TCACA_code_008_cfWoLwvjU4" {
		t.Fatalf("package code: %+v", resource)
	}
	if acc.Quota.Membership == nil || acc.Quota.Membership.Tier != "体验版" {
		t.Fatalf("subscription tier: %+v", acc.Quota.Membership)
	}
	// The aggregate window stays on the snapshot (auto-switch gates on it) but
	// is not part of the API view — the credits block already renders it.
	if len(acc.Quota.Limits) != 1 {
		t.Fatalf("synthetic window missing from the snapshot: %+v", acc.Quota.Limits)
	}
	view := quotaView(acc.Quota)
	limits, _ := view.MustGet("limits").(*jsoncompat.Array)
	if limits == nil || len(limits.Items) != 0 {
		t.Fatalf("aggregate window must not repeat in the view: %v", limits)
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

// 5.6 起桌面把 token 写成 {$wbEncrypted} 信封，每次刷新换 nonce，token 文本
// 比对注定失效：account.uid 兜底保证「当前使用」还认得出来，且换号后不误判。
func TestWorkbuddyActiveIDFallsBackToSlotUID(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	pooled := workbuddyFixtureAccount("w_useal", "u-seal", "seal")
	pooled.AccessToken = `{"$wbEncrypted":1,"envelope":"pool-nonce"}`
	pooled.RefreshToken = `{"$wbEncrypted":1,"envelope":"pool-refresh"}`
	if err := m.saveAccount("workbuddy", pooled); err != nil {
		t.Fatal(err)
	}

	writeWorkbuddySlotJSON(t, "cn", map[string]any{
		"domain":  "www.workbuddy.cn",
		"account": map[string]any{"uid": "u-seal"},
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": "desktop-nonce"},
			"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": "desktop-refresh"},
		},
	})
	if got := m.activeID("workbuddy"); got != "w_useal" {
		t.Fatalf("uid fallback: %q", got)
	}

	// 桌面换成了池里没有的账号：宁可空，也不能把别的账号标成当前。
	writeWorkbuddySlotJSON(t, "cn", map[string]any{
		"account": map[string]any{"uid": "u-other"},
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": "other-nonce"},
			"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": "other-refresh"},
		},
	})
	if got := m.activeID("workbuddy"); got != "" {
		t.Fatalf("unknown slot must not match: %q", got)
	}
}

// writeWorkbuddySlotJSON writes a desktop info file verbatim (envelope tokens,
// plaintext account block) for active-id tests.
func writeWorkbuddySlotJSON(t *testing.T, variant string, root map[string]any) {
	t.Helper()
	writeWorkbuddyAuthJSONAt(t, workbuddyAuthFilePath(variant), root)
}

// --- daily check-in ---------------------------------------------------------------

// 签到请求必须是官方客户端形态：billing 域单段 UA、每账号设备指纹头与
// X-User-Id 身份头（workbuddy-manager 对齐官方客户端的口径）。
func TestWorkbuddyCheckinClientShapedHeaders(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	restoreVersion := workbuddyInstalledVersion
	workbuddyInstalledVersion = func(string) string { return "9.9.9" }
	t.Cleanup(func() { workbuddyInstalledVersion = restoreVersion })

	var gotUA, gotMachineID, gotSessionID, gotUserID, gotPlatform string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":false}}`))
			return
		case "/v2/billing/meter/daily-checkin":
			gotUA = r.Header.Get("User-Agent")
			gotMachineID = r.Header.Get("X-Machine-ID")
			gotSessionID = r.Header.Get("X-Session-ID")
			gotUserID = r.Header.Get("X-User-Id")
			gotPlatform = r.Header.Get("X-Client-Platform")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"credit":7,"streak_days":3}}`))
			return
		default:
			t.Errorf("path: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	returned, message, err := m.Checkin(context.Background(), "workbuddy", "w_u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if message != "签到成功 +7 积分 · 连签 3 天" {
		t.Fatalf("message: %q", message)
	}
	if gotUA != "WorkBuddy/9.9.9" {
		t.Fatalf("billing UA must be the single-segment client form: %q", gotUA)
	}
	if gotPlatform != "" {
		t.Fatalf("billing must not carry the web platform header: %q", gotPlatform)
	}
	if len(gotMachineID) != 36 || len(gotSessionID) != 36 || gotMachineID == gotSessionID {
		t.Fatalf("fingerprint headers: machine=%q session=%q", gotMachineID, gotSessionID)
	}
	if gotUserID != "uid-1" {
		t.Fatalf("X-User-Id: %q", gotUserID)
	}
	if returned.CheckinAt == 0 || returned.CheckinNote != message {
		t.Fatalf("checkin bookkeeping: %+v", returned)
	}
}

// 活动状态探测的两个短路：今天已签（带当日到账积分）与活动未开——都不再打
// 真实签到接口。
func TestWorkbuddyCheckinProbeShortCircuit(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	claim := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":true,"today_credit":7}}`))
		case "/v2/billing/meter/daily-checkin":
			claim++
			_, _ = w.Write([]byte(`{"code":0,"msg":"should not be reached"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	_, message, err := m.Checkin(context.Background(), "workbuddy", "w_u1")
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if message != "今日已签到（今日 +7 积分）" {
		t.Fatalf("message: %q", message)
	}
	if claim != 0 {
		t.Fatalf("probe short-circuit must not hit the claim endpoint, hit %d", claim)
	}
}

func TestWorkbuddyCheckinActivityInactive(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	claim := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/billing/meter/checkin-activity-status" {
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":false}}`))
			return
		}
		claim++
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	_, message, err := m.Checkin(context.Background(), "workbuddy", "w_u1")
	if err != nil || message != "签到活动当前未开启" {
		t.Fatalf("inactive activity: message=%q err=%v", message, err)
	}
	if claim != 0 {
		t.Fatalf("inactive activity must not claim, hit %d", claim)
	}
	saved, _ := m.loadAccount("workbuddy", "w_u1")
	if saved.CheckinAt == 0 {
		t.Fatal("inactive activity still records the day as handled")
	}
}

// 签到接口的 401 要拿积分汇总复核：单边 401 是资格问题（不降级账号），
// 两边都 401 才是登录失效。
func TestWorkbuddyCheckinUnauthorizedCrossCheck(t *testing.T) {
	for _, tc := range []struct {
		name          string
		summaryStatus int
		summaryBody   string
		wantMarked    bool
		wantErrPart   string
	}{{
		name:          "summary ok → eligibility, not expired",
		summaryStatus: 200,
		summaryBody:   `{"code":0,"data":{}}`,
		wantMarked:    false,
		wantErrPart:   "官方 App",
	}, {
		name:          "both 401 → expired",
		summaryStatus: 401,
		summaryBody:   `{"code":401,"message":"unauthorized"}`,
		wantMarked:    true,
		wantErrPart:   "登录已失效",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := setupWorkbuddyFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v2/billing/meter/checkin-activity-status":
					_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":false}}`))
				case "/v2/billing/meter/daily-checkin":
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
				case "/billing/meter/get-user-resource-summary":
					w.WriteHeader(tc.summaryStatus)
					_, _ = w.Write([]byte(tc.summaryBody))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			stubWorkbuddyAPI(t, server.URL)

			acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
			if err := m.saveAccount("workbuddy", acc); err != nil {
				t.Fatal(err)
			}
			_, _, err := m.Checkin(context.Background(), "workbuddy", "w_u1")
			if err == nil || !strings.Contains(err.Error(), tc.wantErrPart) {
				t.Fatalf("err: %v", err)
			}
			saved, _ := m.loadAccount("workbuddy", "w_u1")
			if marked := saved.Status == StatusReauthRequired; marked != tc.wantMarked {
				t.Fatalf("reauth marked=%v want %v (status=%q)", marked, tc.wantMarked, saved.Status)
			}
		})
	}
}

// 10001 有两副面孔：「今日已签到」按成功记账；其他文案是瞬时限流，必须报错。
// 真实环境里「今日已签到」还可能走 HTTP 400 + 业务信封（传输层包成
// {code:400,message:正文}），必须再拆一层才能看到 10001。
type stubReply struct {
	status int
	body   string
}

func TestWorkbuddyCheckinCode10001Semantics(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	claims := []stubReply{
		{status: 200, body: `{"code":10001,"msg":"今日已签到，请勿重复签到"}`},
		{status: 400, body: `{"code":10001,"msg":"今天已签到，请明天再来","requestId":"req-1"}`},
		{status: 200, body: `{"code":10001,"msg":"签到失败，请稍后重试"}`},
	}
	var call int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/billing/meter/checkin-activity-status":
			// 探测都说未签，让三种领取回执都被走到。
			_, _ = w.Write([]byte(`{"code":0,"data":{"active":true,"today_checked_in":false}}`))
		case "/v2/billing/meter/daily-checkin":
			if call < len(claims) {
				reply := claims[call]
				w.WriteHeader(reply.status)
				_, _ = w.Write([]byte(reply.body))
			}
			call++
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	if _, message, err := m.Checkin(context.Background(), "workbuddy", "w_u1"); err != nil || message != "今日已签到" {
		t.Fatalf("already-checkin: message=%q err=%v", message, err)
	}
	saved, err := m.loadAccount("workbuddy", "w_u1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.CheckinAt == 0 || saved.CheckinNote != "今日已签到" {
		t.Fatalf("already-checkin must be recorded as done: %+v", saved)
	}
	// HTTP 400 信封形态的「今日已签到」同样按幂等成功处理。
	if _, message, err := m.Checkin(context.Background(), "workbuddy", "w_u1"); err != nil || message != "今日已签到" {
		t.Fatalf("http-400 envelope already-checkin: message=%q err=%v", message, err)
	}
	if _, _, err := m.Checkin(context.Background(), "workbuddy", "w_u1"); err == nil {
		t.Fatal("throttled 10001 must surface as an error")
	}
	// 限流失败不得覆盖此前「今日已签到」的记账。
	saved, _ = m.loadAccount("workbuddy", "w_u1")
	if saved.CheckinNote != "今日已签到" {
		t.Fatalf("throttled attempt must not clobber the earlier success: %+v", saved)
	}
}

// 刷新的瞬态失败（网关 5xx、无确定信号）不得把账号降级成「需重新登录」；
// invalid_grant 是确定信号，照旧降级。
func TestWorkbuddyRefreshTransientKeepsStatus(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		status     int
		wantAuth   bool
		wantStatus string
	}{{
		name:     "gateway blip keeps the account",
		body:     `{"code":500,"message":"upstream busy"}`,
		status:   200,
		wantAuth: false,
	}, {
		name:     "invalid_grant retires the account",
		body:     `{"code":401,"message":"invalid_grant"}`,
		status:   200,
		wantAuth: true,
	}, {
		name:     "code 401 retires the account",
		body:     `{"code":401,"message":"token 已过期"}`,
		status:   200,
		wantAuth: true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := setupWorkbuddyFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			stubWorkbuddyAPI(t, server.URL)

			acc := workbuddyFixtureAccount("w_uid1", "uid-1", "old")
			err := refreshWorkbuddyToken(context.Background(), acc, m.now())
			if err == nil {
				t.Fatal("refresh must fail")
			}
			if got := isAuthFailed(err); got != tc.wantAuth {
				t.Fatalf("authFailed=%v want %v (err=%v)", got, tc.wantAuth, err)
			}
			if tc.wantAuth && acc.Status != StatusReauthRequired {
				t.Fatalf("status: %q", acc.Status)
			}
			if !tc.wantAuth && acc.Status != StatusActive {
				t.Fatalf("transient failure must keep status active, got %q", acc.Status)
			}
		})
	}
}

// 国际版没有签到体系：不能把请求打过去（那本身就是风控信号）。
func TestWorkbuddyCheckinSkipsInternational(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("no request expected for the ai variant: %s", r.URL.Path)
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_ai1", "uid-ai", "tok")
	acc.Variant = "ai"
	acc.Domain = "www.workbuddy.ai"
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Checkin(context.Background(), "workbuddy", "w_ai1"); err == nil || !strings.Contains(err.Error(), "国际版") {
		t.Fatalf("ai checkin must be refused: %v", err)
	}
}

// 签到按本地自然日计：今天签过不再催，跨天重新欠签。
func TestWorkbuddyCheckinDueDayGate(t *testing.T) {
	acc := &Account{Variant: "cn", Status: StatusActive, AccessToken: "plain"}
	if !workbuddyCheckinDue(acc, time.Unix(1700000000, 0)) {
		t.Fatal("never checked in must be due")
	}
	acc.CheckinAt = 1700000000000 // fixture clock, same local day
	if workbuddyCheckinDue(acc, time.Unix(1700000000, 0)) {
		t.Fatal("same-day checkin must not be due")
	}
	acc.CheckinAt = 1700000000000 - 48*3600*1000
	if !workbuddyCheckinDue(acc, time.Unix(1700000000, 0)) {
		t.Fatal("previous-day checkin must be due again")
	}
}

// 全量轮询顺带补签：当天只打一次签到接口，跨天（或失败）才再打。
func TestWorkbuddyPollerAutoCheckin(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	checkins := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		default:
			// 额度接口：空成功即可（轮询忽略额度失败）。
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_u1", "uid-1", "tok")
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	poller := NewPoller(m)
	poller.AllInterval = time.Hour
	poller.tickAll(context.Background())
	poller.tickAll(context.Background())
	if checkins != 1 {
		t.Fatalf("checkin must fire once per calendar day, got %d", checkins)
	}
	saved, _ := m.loadAccount("workbuddy", "w_u1")
	if saved.CheckinAt == 0 {
		t.Fatal("auto checkin must be recorded")
	}
}

// --- international region registration + trial ------------------------------------

// 额度刷新时探测国际版注册状态：500/region required → pending。
func TestWorkbuddyAIRegistrationProbe(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	probes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/auth/realms/copilot/overseas/user/register") {
			probes++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"code":500,"msg":"region required"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_ai1", "uid-ai", "tok")
	acc.Variant = "ai"
	acc.Domain = "www.workbuddy.ai"
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RefreshQuota(context.Background(), "workbuddy", "w_ai1"); err != nil {
		t.Logf("quota refresh error tolerated: %v", err)
	}
	saved, _ := m.loadAccount("workbuddy", "w_ai1")
	if saved.AiProvision != aiProvisionPending {
		t.Fatalf("probe must mark pending: %q", saved.AiProvision)
	}
	if probes != 1 {
		t.Fatalf("probe calls: %d", probes)
	}
}

// 开通流程：地区三字段的取值各不相同（数字码/英文全名/IOS2），trial 14051 幂等。
func TestWorkbuddyProvisionRegionAndTrial(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	var gotRegionBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/billing/area/get-country-code":
			// data 是内嵌 JSON 字符串——上游脚本明确处理了这一层。
			inner := `{"data":{"list":[{"IOS2":"HK","EnName":"China Hong Kong","Code":"810000"},{"IOS2":"SG","EnName":"Singapore","Code":"702000"}]}}`
			_, _ = w.Write([]byte(`{"code":0,"data":"` + strings.ReplaceAll(inner, `"`, `\"`) + `"}`))
		case "/console/login/account":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &gotRegionBody)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case "/billing/ide/trial":
			_, _ = w.Write([]byte(`{"code":14051,"msg":"already claimed"}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		}
	}))
	defer server.Close()
	stubWorkbuddyAPI(t, server.URL)

	acc := workbuddyFixtureAccount("w_ai1", "uid-ai", "tok")
	acc.Variant = "ai"
	acc.Domain = "www.workbuddy.ai"
	acc.AiProvision = aiProvisionPending
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	returned, message, err := m.Provision(context.Background(), "workbuddy", "w_ai1", "hk")
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if !strings.Contains(message, "中国香港") || !strings.Contains(message, "此前已领取") {
		t.Fatalf("message: %q", message)
	}
	if returned.AiProvision != aiProvisionDone {
		t.Fatalf("provision state: %q", returned.AiProvision)
	}
	attrs, _ := gotRegionBody["attributes"].(map[string]any)
	if attrs == nil {
		t.Fatalf("region body missing attributes wrapper: %v", gotRegionBody)
	}
	if fmt.Sprint(attrs["countryCode"]) != "[810000]" ||
		fmt.Sprint(attrs["countryFullName"]) != "[China Hong Kong]" ||
		fmt.Sprint(attrs["countryName"]) != "[HK]" {
		t.Fatalf("region attributes take three different values: %v", attrs)
	}
	// 国内版账号不允许走开通。
	cn := workbuddyFixtureAccount("w_cn1", "uid-cn", "tok")
	if err := m.saveAccount("workbuddy", cn); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Provision(context.Background(), "workbuddy", "w_cn1", "HK"); err == nil {
		t.Fatal("cn provision must be refused")
	}
}

func writeWorkbuddyAuthJSONAt(t *testing.T, path string, root map[string]any) {
	t.Helper()
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 未接管 apiKeyHelper 时，CLI 跑在自己的登录槽（Tencent-Cloud.coding-copilot.info）
// 上——它和桌面 App 的槽是两个账号、两个文件，「当前使用」必须跟 CLI 走。
func TestWorkbuddyActiveIDPrefersCLILogin(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	cliAcc := workbuddyFixtureAccount("w_ucli", "u-cli", "cli")
	if err := m.saveAccount("workbuddy", cliAcc); err != nil {
		t.Fatal(err)
	}
	deskAcc := workbuddyFixtureAccount("w_udesk", "u-desk", "desk")
	if err := m.saveAccount("workbuddy", deskAcc); err != nil {
		t.Fatal(err)
	}
	writeWorkbuddySlotJSON(t, "cn", map[string]any{
		"account": map[string]any{"uid": "u-desk"},
		"auth": map[string]any{
			"accessToken":  "at-desk",
			"refreshToken": "rt-desk",
		},
	})

	// CLI 自己登录了另一个账号：当前使用应跟 CLI，而不是桌面槽。
	writeWorkbuddyAuthJSONAt(t, workbuddyCLILoginPath(), map[string]any{
		"account": map[string]any{"uid": "u-cli", "nickname": "CLI 本人"},
		"auth": map[string]any{
			"accessToken":  "at-cli-own",
			"refreshToken": "rt-cli-own",
			"domain":       "www.codebuddy.cn",
		},
	})
	if got := m.activeID("workbuddy"); got != "w_ucli" {
		t.Fatalf("cli login must win: %q", got)
	}

	// CLI 登录的是池外账号：如实返回空，不得误标桌面槽匹配的账号。
	writeWorkbuddyAuthJSONAt(t, workbuddyCLILoginPath(), map[string]any{
		"account": map[string]any{"uid": "u-outside"},
		"auth": map[string]any{
			"accessToken":  "at-outside",
			"refreshToken": "rt-outside",
			"domain":       "www.codebuddy.cn",
		},
	})
	if got := m.activeID("workbuddy"); got != "" {
		t.Fatalf("unpooled cli login must not fall back to the desktop slot: %q", got)
	}
}

// 已接管 apiKeyHelper 时，CLI 的账号来自 state.json，当前使用以它为准。
func TestWorkbuddyActiveIDFromCLIState(t *testing.T) {
	m, home := setupWorkbuddyFixture(t)
	stateAcc := workbuddyFixtureAccount("w_ustate", "u-state", "state")
	if err := m.saveAccount("workbuddy", stateAcc); err != nil {
		t.Fatal(err)
	}
	deskAcc := workbuddyFixtureAccount("w_udesk2", "u-desk2", "desk2")
	if err := m.saveAccount("workbuddy", deskAcc); err != nil {
		t.Fatal(err)
	}
	writeWorkbuddySlotJSON(t, "cn", map[string]any{
		"account": map[string]any{"uid": "u-desk2"},
		"auth": map[string]any{
			"accessToken":  "at-desk2",
			"refreshToken": "rt-desk2",
		},
	})
	settings := filepath.Join(home, ".codebuddy", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"apiKeyHelper":"`+workbuddyHelperPath()+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	rotate := workbuddyRotateDir()
	if err := os.MkdirAll(rotate, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workbuddyCLIStatePath(), []byte(`{"active":1,"activeAccountId":"w_ustate","updatedAt":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := m.activeID("workbuddy"); got != "w_ustate" {
		t.Fatalf("state.json must win: %q", got)
	}
	// activeAccountId 失效时按 active 下标兜底。
	if err := os.WriteFile(workbuddyCLIStatePath(), []byte(`{"active":1,"updatedAt":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second := m.list("workbuddy")[1].ID
	if got := m.activeID("workbuddy"); got != second {
		t.Fatalf("active index fallback: %q (list[1]=%q)", got, second)
	}
}

// 「捕获当前登录」承诺读 CLI 的登录态：未接管时必须先捕 CLI 自己的槽，
// 桌面 App 的槽只作兜底。
func TestWorkbuddyCapturePrefersCLILogin(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	writeWorkbuddySlotJSON(t, "cn", map[string]any{
		"account": map[string]any{"uid": "u-desk"},
		"auth": map[string]any{
			"accessToken":  "at-desk",
			"refreshToken": "rt-desk",
		},
	})
	writeWorkbuddyAuthJSONAt(t, workbuddyCLILoginPath(), map[string]any{
		"account": map[string]any{"uid": "u-cli", "nickname": "CLI 本人"},
		"auth": map[string]any{
			"accessToken":  "at-cli",
			"refreshToken": "rt-cli",
			"domain":       "www.codebuddy.cn",
		},
	})
	acc, created, err := m.CaptureCurrent(context.Background(), "workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	if !created || acc.ID != "w_u-cli" || acc.AccessToken != "at-cli" {
		t.Fatalf("capture must import the CLI login first: created=%v acc=%+v", created, acc)
	}
	if acc.Variant != "cn" {
		t.Fatalf("variant from domain: %+v", acc)
	}
}

// 信封账号原地扫码重登：密文凭据必须被明文替换，占位名也要换成真实昵称，
// 否则用户重登完还是查不到额度、列表里也一直叫「WorkBuddy cn xxxxxxxx」。
func TestWorkbuddyReauthReplacesEnvelopeAndPlaceholderLabel(t *testing.T) {
	m, _ := setupWorkbuddyFixture(t)
	sealed := &Account{
		ID: "w_u1", UserID: "u1", Label: "WorkBuddy cn u1", Variant: "cn",
		AccessToken:  `{"$wbEncrypted":1,"envelope":"old"}`,
		RefreshToken: `{"$wbEncrypted":1,"envelope":"old-refresh"}`,
		Status:       StatusActive,
	}
	if err := m.saveAccount("workbuddy", sealed); err != nil {
		t.Fatal(err)
	}
	candidate := &Account{
		ID: "w_u1", UserID: "u1", Nickname: "真名", Variant: "cn",
		AccessToken: "plain-at", RefreshToken: "plain-rt", TokenType: "Bearer",
		ExpiresAt: workbuddyNowMS + 3600_000, Status: StatusActive,
	}
	account, err := m.persistLogin("workbuddy", candidate, "w_u1")
	if err != nil {
		t.Fatal(err)
	}
	if workbuddyEnvelope(account.AccessToken) || account.AccessToken != "plain-at" {
		t.Fatalf("reauth must store plaintext: %+v", account)
	}
	if account.Label != "真名" {
		t.Fatalf("placeholder label must become the nickname: %q", account.Label)
	}
	if account.Nickname != "真名" {
		t.Fatalf("nickname: %q", account.Nickname)
	}
	// 已经手动改过名的账号不能被覆盖。
	renamed := &Account{ID: "w_u2", UserID: "u2", Label: "我的号", Variant: "cn",
		AccessToken: `{"$wbEncrypted":1,"envelope":"x"}`, Status: StatusActive}
	if err := m.saveAccount("workbuddy", renamed); err != nil {
		t.Fatal(err)
	}
	account, err = m.persistLogin("workbuddy", &Account{ID: "w_u2", UserID: "u2", Nickname: "真名2", Variant: "cn",
		AccessToken: "plain-at-2", RefreshToken: "plain-rt-2", Status: StatusActive}, "w_u2")
	if err != nil {
		t.Fatal(err)
	}
	if account.Label != "我的号" {
		t.Fatalf("custom label must survive reauth: %q", account.Label)
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

// Envelope-encrypted identity fields must never become display text: the
// capture derives a friendly label instead, and the view sanitizes accounts
// stored before the fix.
func TestWorkbuddyEnvelopeCaptureLabel(t *testing.T) {
	root := map[string]any{
		"domain": "www.workbuddy.cn",
		"account": map[string]any{
			"uid":      "3dd224ae-786f-4d68-9280-d95efd9dd6c5",
			"nickname": map[string]any{"$wbEncrypted": 1, "envelope": "eyJzdWl0ZSI6MSJ9"},
		},
		"auth": map[string]any{
			"accessToken":  map[string]any{"$wbEncrypted": 1, "envelope": "eyJzdWl0ZSI6MSJ9"},
			"refreshToken": map[string]any{"$wbEncrypted": 1, "envelope": "eyJzdWl0ZSI6MSJ9"},
		},
	}
	candidate := candidateFromWorkbuddyAuthRoot(root, "cn")
	if candidate == nil {
		t.Fatal("candidate missing")
	}
	if candidate.Nickname != "" || candidate.Email != "" {
		t.Fatalf("envelope identity must stay empty: %+v", candidate)
	}
	if candidate.Label != "WorkBuddy cn 3dd224ae" {
		t.Fatalf("derived label: %q", candidate.Label)
	}

	// Stored-before-fix accounts keep the envelope JSON in the label; the
	// redacted view must not leak it.
	m := newTestManager(t)
	acc := &Account{ID: candidate.ID, UserID: candidate.UserID, Variant: "cn",
		Label:       `{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6MSJ9"}`,
		Nickname:    `{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6MSJ9"}`,
		AccessToken: `{"$wbEncrypted":1,"envelope":"eyJzdWl0ZSI6MSJ9"}`, Status: StatusActive}
	if err := m.saveAccount("workbuddy", acc); err != nil {
		t.Fatal(err)
	}
	detail, err := m.Detail("workbuddy")
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := detail.MustGet("accounts").(*jsoncompat.Array)
	if rows == nil || len(rows.Items) == 0 {
		t.Fatalf("detail rows: %+v", detail)
	}
	first, _ := rows.Items[0].(*jsoncompat.Object)
	label, _ := first.MustGet("label").(string)
	if label != "WorkBuddy cn 3dd224ae" {
		t.Fatalf("view label fallback: %q", label)
	}
	if nickname, _ := first.MustGet("nickname").(string); nickname != "" {
		t.Fatalf("view nickname must be sanitized: %q", nickname)
	}
}
