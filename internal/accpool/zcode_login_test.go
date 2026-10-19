package accpool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubZcodeLoginAPI points every login-flow endpoint at the stub server and
// shortens the billing-ready delays; tests must not run in parallel.
func stubZcodeLoginAPI(t *testing.T, server *httptest.Server) {
	t.Helper()
	restoreInit, restorePoll := zcodeOAuthInitURL, zcodeOAuthPollURLPrefix
	restoreBusiness := zcodeBusinessLoginURL
	restoreBilling, restoreDelays := zcodeBillingReadyURL, zcodeBillingReadyDelays
	zcodeOAuthInitURL = server.URL + "/api/v1/oauth/cli/init"
	zcodeOAuthPollURLPrefix = server.URL + "/api/v1/oauth/cli/poll/"
	zcodeBusinessLoginURL = server.URL + "/api/auth/z/login"
	zcodeBillingReadyURL = server.URL + "/billing-ready"
	zcodeBillingReadyDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond, 5 * time.Millisecond}
	t.Cleanup(func() {
		zcodeOAuthInitURL, zcodeOAuthPollURLPrefix = restoreInit, restorePoll
		zcodeBusinessLoginURL = restoreBusiness
		zcodeBillingReadyURL, zcodeBillingReadyDelays = restoreBilling, restoreDelays
	})
}

func writeStubJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode stub response: %v", err)
	}
}

var zcodePollTokenPattern = regexp.MustCompile(`^Bearer [0-9a-f]{64}$`)

// zcodeInitHandler serves a valid cli/init envelope, asserting the Bearer
// pollToken and the provider body along the way.
func zcodeInitHandler(t *testing.T, provider, flowID, authorizeURL string, expiresIn time.Duration, pollIntervalSec float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("init method = %s", r.Method)
		}
		if !zcodePollTokenPattern.MatchString(r.Header.Get("authorization")) {
			t.Errorf("init authorization = %q, want Bearer <32-byte hex>", r.Header.Get("authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("init body: %v", err)
		}
		if body["provider"] != provider {
			t.Errorf("init provider = %v", body["provider"])
		}
		writeStubJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"flow_id":           flowID,
				"authorize_url":     authorizeURL,
				"expires_at":        float64(time.Now().Add(expiresIn).UnixMilli()) / 1000,
				"poll_interval_sec": pollIntervalSec,
			},
		})
	}
}

func TestZcodeLoginZaiFullFlow(t *testing.T) {
	stubZcodeSecret(t)
	zcodeJWT := makeJWT(map[string]any{"user_id": "u123456789", "customer_id": "cust-1"})
	var polls, billingCalls, businessCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "zai", "flow-zai-1",
		"https://chat.z.ai/api/oauth/authorize?client_id=x&state=st-zai", 10*time.Minute, 1))
	mux.HandleFunc("/api/v1/oauth/cli/poll/flow-zai-1", func(w http.ResponseWriter, r *http.Request) {
		if !zcodePollTokenPattern.MatchString(r.Header.Get("authorization")) {
			t.Errorf("poll authorization = %q", r.Header.Get("authorization"))
		}
		if polls.Add(1) == 1 {
			writeStubJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"status": "pending"}})
			return
		}
		writeStubJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"status": "ready",
				"token":  zcodeJWT,
				"user": map[string]any{
					"user_id": "u123456789",
					"email":   "alice@example.com",
					"name":    "Alice",
					"avatar":  "https://cdn.example.com/a.png",
				},
				"zai": map[string]any{"access_token": "zai-oauth-access-token"},
			},
		})
	})
	mux.HandleFunc("/api/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		businessCalls.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("business body: %v", err)
		}
		if body["token"] != "zai-oauth-access-token" {
			t.Errorf("business token = %v", body["token"])
		}
		writeStubJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"access_token": "business-token-abcdefghijklmnopqrstuvwxyz-zzz"}})
	})
	mux.HandleFunc("/billing-ready", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+zcodeJWT {
			t.Errorf("billing authorization = %q", r.Header.Get("authorization"))
		}
		call := billingCalls.Add(1)
		plans := []any{}
		if call >= 2 {
			plans = []any{map[string]any{"plan_id": "pro", "status": "active"}}
		}
		writeStubJSON(t, w, map[string]any{"data": map[string]any{"plans": plans}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)

	m := loginTestManager(t)
	challenge, wait, err := startZcodeLogin(context.Background(), m, LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	if challenge.URL != "https://chat.z.ai/api/oauth/authorize?client_id=x&state=st-zai" {
		t.Fatalf("challenge URL = %q", challenge.URL)
	}
	if challenge.ExpiresIn < 590 || challenge.ExpiresIn > 600 {
		t.Fatalf("ExpiresIn = %d", challenge.ExpiresIn)
	}

	account, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls = %d, want 2", polls.Load())
	}
	if businessCalls.Load() != 1 {
		t.Fatalf("business exchanges = %d, want 1", businessCalls.Load())
	}
	if billingCalls.Load() != 2 {
		t.Fatalf("billing probes = %d, want 2 (empty then ready)", billingCalls.Load())
	}

	// Identity fields, same shape captureZcodeCurrent produces.
	wantID := "em-" + djb2Hex("alice@example.com")
	if len(wantID) > 13 {
		wantID = wantID[:13]
	}
	if account.ID != wantID || account.UserID != "u123456789" ||
		account.Email != "alice@example.com" || account.Nickname != "Alice" ||
		account.Provider != "builtin:zai" || account.Label != "alice@example.com" {
		t.Fatalf("candidate identity: %+v", account)
	}

	// Credentials/Config decrypt with the package secret and carry the
	// official field shape, compatible with the capture/fingerprint pipeline.
	secret := zcodeCredentialSecret()
	credentialsText, err := zcodeDecrypt(account.Credentials, secret)
	if err != nil {
		t.Fatalf("decrypt credentials: %v", err)
	}
	configText, err := zcodeDecrypt(account.Config, secret)
	if err != nil {
		t.Fatalf("decrypt config: %v", err)
	}
	credentials := parseJSONObject(credentialsText)
	config := parseJSONObject(configText)
	if credentials == nil || config == nil {
		t.Fatal("credentials/config must be JSON texts")
	}
	if got := zcodeDecryptSafe(stringValue(credentials, "zcodejwttoken")); got != zcodeJWT {
		t.Fatalf("zcodejwttoken = %q", got)
	}
	if got := zcodeDecryptSafe(stringValue(credentials, "oauth:active_provider")); got != "zai" {
		t.Fatalf("active_provider = %q", got)
	}
	// 写入凭据的必须是 business token，不是 OAuth access token。
	if got := zcodeDecryptSafe(stringValue(credentials, "oauth:zai:access_token")); got != "business-token-abcdefghijklmnopqrstuvwxyz-zzz" {
		t.Fatalf("zai access_token = %q", got)
	}
	userInfoRaw := zcodeDecryptSafe(stringValue(credentials, "oauth:zai:user_info"))
	var userInfo map[string]any
	if err := json.Unmarshal([]byte(userInfoRaw), &userInfo); err != nil || userInfo["email"] != "alice@example.com" {
		t.Fatalf("stored user_info: %q %v", userInfoRaw, err)
	}
	if _, exists := credentials["oauth:zai:refresh_token"]; exists {
		t.Fatal("zai login must not carry a refresh_token")
	}

	providers, _ := config["provider"].(map[string]any)
	for _, slot := range []string{"builtin:zai-start-plan", "builtin:zai-coding-plan", "builtin:zai"} {
		entry, _ := providers[slot].(map[string]any)
		options, _ := entry["options"].(map[string]any)
		if options["apiKey"] != zcodeJWT {
			t.Fatalf("slot %s apiKey = %v, want the zcode JWT", slot, options["apiKey"])
		}
		if entry["enabled"] != true {
			t.Fatalf("slot %s enabled = %v", slot, entry["enabled"])
		}
	}

	tokens := zcodeCandidateTokens(credentials, config)
	if len(tokens) == 0 || tokens[0] != zcodeJWT {
		t.Fatalf("candidate tokens: %v", tokens)
	}
	foundBusiness := false
	for _, token := range tokens {
		if token == "business-token-abcdefghijklmnopqrstuvwxyz-zzz" {
			foundBusiness = true
		}
	}
	if !foundBusiness {
		t.Fatalf("business token missing from candidate tokens: %v", tokens)
	}
	fp := zcodeFingerprintFrom(credentials, config)
	if fp == nil || fp.UserID != "u123456789" || fp.Email != "alice@example.com" || fp.Provider != "builtin:zai" {
		t.Fatalf("fingerprint: %+v", fp)
	}
}

func TestZcodeLoginBigmodelFlow(t *testing.T) {
	stubZcodeSecret(t)
	zcodeJWT := makeJWT(map[string]any{"user_id": "u-big-1", "sub": "u-big-1"})
	var polls, billingCalls, businessCalls atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "bigmodel", "flow-bm-1",
		"https://open.bigmodel.cn/oauth/authorize?client_id=y&state=st-bm", 10*time.Minute, 1))
	mux.HandleFunc("/api/v1/oauth/cli/poll/flow-bm-1", func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		writeStubJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"status": "ready",
				"token":  zcodeJWT,
				"user": map[string]any{
					"user_id": "u-big-1",
					"email":   "bob@bigmodel.cn",
					"name":    "Bob",
				},
				"bigmodel": map[string]any{
					"access_token":  "bm-access-token",
					"refresh_token": "bm-refresh-token",
				},
			},
		})
	})
	mux.HandleFunc("/api/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		businessCalls.Add(1)
		writeStubJSON(t, w, map[string]any{"code": 0})
	})
	mux.HandleFunc("/billing-ready", func(w http.ResponseWriter, r *http.Request) {
		billingCalls.Add(1)
		writeStubJSON(t, w, map[string]any{"data": map[string]any{"plans": []any{map[string]any{}}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)

	_, wait, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "bigmodel"})
	if err != nil {
		t.Fatal(err)
	}
	account, err := wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if businessCalls.Load() != 0 {
		t.Fatalf("bigmodel must not exchange a business token (%d calls)", businessCalls.Load())
	}
	if billingCalls.Load() != 0 {
		t.Fatalf("bigmodel must not wait for billing (%d calls)", billingCalls.Load())
	}
	if polls.Load() != 1 {
		t.Fatalf("polls = %d, want 1 (ready immediately)", polls.Load())
	}

	// camelCase 兜底归一化后取前 8 位作为 id；profile 无 email，不参与 id。
	if account.UserID != "u-big-1" || account.ID != "u-big-1" ||
		account.Nickname != "Bob" || account.Label != "Bob" || account.Provider != "builtin:bigmodel-start-plan" {
		t.Fatalf("candidate identity: %+v", account)
	}

	secret := zcodeCredentialSecret()
	credentials := parseJSONObject(mustZcodeDecrypt(t, account.Credentials, secret))
	config := parseJSONObject(mustZcodeDecrypt(t, account.Config, secret))
	if got := zcodeDecryptSafe(stringValue(credentials, "oauth:bigmodel:access_token")); got != "bm-access-token" {
		t.Fatalf("bigmodel access_token = %q", got)
	}
	if got := zcodeDecryptSafe(stringValue(credentials, "oauth:bigmodel:refresh_token")); got != "bm-refresh-token" {
		t.Fatalf("bigmodel refresh_token = %q", got)
	}
	if got := zcodeDecryptSafe(stringValue(credentials, "zcodejwttoken")); got != zcodeJWT {
		t.Fatalf("zcodejwttoken = %q", got)
	}
	// bigmodel 写盘的是官方 profile shape。
	userInfoRaw := zcodeDecryptSafe(stringValue(credentials, "oauth:bigmodel:user_info"))
	var userInfo map[string]any
	if err := json.Unmarshal([]byte(userInfoRaw), &userInfo); err != nil ||
		userInfo["id"] != "u-big-1" || userInfo["username"] != "Bob" || userInfo["displayName"] != "Bob" {
		t.Fatalf("stored profile: %q %v", userInfoRaw, err)
	}

	providers, _ := config["provider"].(map[string]any)
	slotKey := func(slot string) string {
		entry, _ := providers[slot].(map[string]any)
		options, _ := entry["options"].(map[string]any)
		key, _ := options["apiKey"].(string)
		return key
	}
	if got := slotKey("builtin:bigmodel-start-plan"); got != zcodeJWT {
		t.Fatalf("start-plan apiKey = %q, want zcode JWT", got)
	}
	if got := slotKey("builtin:bigmodel-coding-plan"); got != "bm-access-token" {
		t.Fatalf("coding-plan apiKey = %q, want bigmodel access token", got)
	}
	if entry, _ := providers["builtin:bigmodel"].(map[string]any); entry == nil {
		t.Fatal("builtin:bigmodel slot missing")
	} else if options, _ := entry["options"].(map[string]any); len(options) != 0 {
		t.Fatalf("builtin:bigmodel options = %v, want no apiKey", options)
	}
	if fp := zcodeFingerprintFrom(credentials, config); fp == nil || fp.UserID != "u-big-1" || fp.Provider != "builtin:bigmodel-start-plan" {
		t.Fatalf("fingerprint: %+v", fp)
	}
}

func mustZcodeDecrypt(t *testing.T, value, secret string) string {
	t.Helper()
	plain, err := zcodeDecrypt(value, secret)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	return plain
}

func TestZcodeLoginRejectsInvalidInit(t *testing.T) {
	stubZcodeSecret(t)
	cases := []struct {
		name        string
		authorize   string
		expiresIn   time.Duration
		pollSeconds float64
	}{
		{"authorize_url not https", "http://chat.z.ai/api/oauth/authorize?state=st", 10 * time.Minute, 1},
		{"authorize_url missing state", "https://chat.z.ai/api/oauth/authorize?client_id=x", 10 * time.Minute, 1},
		{"already expired", "https://chat.z.ai/api/oauth/authorize?state=st", -time.Minute, 1},
		{"poll interval below one second", "https://chat.z.ai/api/oauth/authorize?state=st", 10 * time.Minute, 0.5},
		{"poll interval beyond remaining", "https://chat.z.ai/api/oauth/authorize?state=st", 2 * time.Second, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "zai", "flow-x", tc.authorize, tc.expiresIn, tc.pollSeconds))
			server := httptest.NewServer(mux)
			defer server.Close()
			stubZcodeLoginAPI(t, server)
			if _, _, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "zai"}); err == nil {
				t.Fatal("invalid init response must be rejected")
			}
		})
	}
}

func TestZcodeLoginInitSurfacesServerMessage(t *testing.T) {
	stubZcodeSecret(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{"code": 1001, "msg": "provider 维护中"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)
	_, _, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "zai"})
	if err == nil || !strings.Contains(err.Error(), "provider 维护中") {
		t.Fatalf("err = %v", err)
	}
}

func TestZcodeLoginPollFailed(t *testing.T) {
	stubZcodeSecret(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "zai", "flow-fail",
		"https://chat.z.ai/api/oauth/authorize?state=st", 10*time.Minute, 1))
	mux.HandleFunc("/api/v1/oauth/cli/poll/flow-fail", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"status": "failed"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)

	_, wait, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil || !strings.Contains(err.Error(), "授权失败") {
		t.Fatalf("err = %v", err)
	}
}

func TestZcodeLoginBusinessRejected(t *testing.T) {
	stubZcodeSecret(t)
	zcodeJWT := makeJWT(map[string]any{"user_id": "u123456789"})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "zai", "flow-biz",
		"https://chat.z.ai/api/oauth/authorize?state=st", 10*time.Minute, 1))
	mux.HandleFunc("/api/v1/oauth/cli/poll/flow-biz", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{
			"code": 0,
			"data": map[string]any{
				"status": "ready",
				"token":  zcodeJWT,
				"user":   map[string]any{"user_id": "u123456789"},
				"zai":    map[string]any{"access_token": "zai-oauth-access-token"},
			},
		})
	})
	mux.HandleFunc("/api/auth/z/login", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{"code": 401, "msg": "授权被拒绝", "success": true})
	})
	mux.HandleFunc("/billing-ready", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{"data": map[string]any{"plans": []any{map[string]any{}}}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)

	_, wait, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wait(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "business token 被拒绝: 授权被拒绝") {
		t.Fatalf("err = %v", err)
	}
}

func TestZcodeLoginCtxCancel(t *testing.T) {
	stubZcodeSecret(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/oauth/cli/init", zcodeInitHandler(t, "zai", "flow-cancel",
		"https://chat.z.ai/api/oauth/authorize?state=st", 10*time.Minute, 1))
	mux.HandleFunc("/api/v1/oauth/cli/poll/flow-cancel", func(w http.ResponseWriter, r *http.Request) {
		writeStubJSON(t, w, map[string]any{"code": 0, "data": map[string]any{"status": "pending"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	stubZcodeLoginAPI(t, server)

	_, wait, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "zai"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := wait(ctx); err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestZcodeLoginUnsupportedProvider(t *testing.T) {
	if _, _, err := startZcodeLogin(context.Background(), loginTestManager(t), LoginOptions{Provider: "minimax"}); err == nil {
		t.Fatal("unsupported provider must fail")
	}
}
