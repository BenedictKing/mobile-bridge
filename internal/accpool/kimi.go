// Package accpool manages credential pools for ACP agents (kimi first): it
// imports account snapshots from zcode-switcher's store, polls the official
// quota APIs, and exposes a redacted view to the gateway's HTTP layer.
package accpool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Kimi Code endpoints and OAuth constants, aligned with zcode-switcher's
// src/kimiPaths.js / src/kimiOauth.js (which mirror the official CLI).
// They are package variables so tests can point them at a stub server.
var (
	kimiAPIBase  = "https://api.kimi.com/coding/v1"
	kimiAuthHost = "https://auth.kimi.com"
	kimiClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"
	kimiTokenURL = kimiAuthHost + "/api/oauth/token"
	// kimiDeviceAuthURL is the RFC 8628 device authorization endpoint
	// (a package variable so tests can point it at a stub server).
	kimiDeviceAuthURL = kimiAuthHost + "/api/oauth/device_authorization"
)

const (
	// refreshLeadSeconds mirrors oauth.needsRefresh's floor: the access token
	// is renewed max(expires_in/2, 300s) before its expiry.
	refreshLeadSeconds = 300
	// kimiDeviceGrantType is RFC 8628's device_code grant type urn.
	kimiDeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// kimiLoginMaxSeconds caps one device-flow login at 15 minutes, matching
	// zcode-switcher's MAX_LOGIN_MS.
	kimiLoginMaxSeconds = int64(15 * 60)
	// kimiLoginMaxTransportErrors caps consecutive network failures while
	// polling the token endpoint (zcode-switcher's MAX_TRANSPORT_ERRORS).
	kimiLoginMaxTransportErrors = 3
	// kimiClientVersion reports the gateway build in the X-Msh-Version header.
	kimiClientVersion = "1.0.0"
)

func init() { registerLoginDriver("kimi", startKimiLogin) }

// kimiDeviceIDFile is where the gateway's per-installation kimi device id
// lives (zcode-switcher's storeDir("kimi")/device-id).
const kimiDeviceIDFile = "device-id"

// ensureKimiDeviceID returns the persisted gateway device id, generating and
// storing a UUID v4 (0600) on first use.
func ensureKimiDeviceID(m *Manager) (string, error) {
	path := filepath.Join(m.storeDir("kimi"), kimiDeviceIDFile)
	if raw, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id, nil
		}
	}
	id := uuid.NewString()
	if err := atomicWrite(path, []byte(id+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("保存 Kimi 设备 ID 失败：%w", err)
	}
	return id, nil
}

// setKimiDeviceHeaders stamps the X-Msh-* device identity headers
// (zcode-switcher's device_headers), shared by the refresh and login flows.
func setKimiDeviceHeaders(request *http.Request, deviceID string) {
	request.Header.Set("X-Msh-Platform", "mobile-bridge")
	request.Header.Set("X-Msh-Version", kimiClientVersion)
	request.Header.Set("X-Msh-Device-Name", kimiHostname())
	request.Header.Set("X-Msh-Device-Model", runtime.GOOS+"/"+runtime.GOARCH)
	request.Header.Set("X-Msh-Os-Version", kimiOSRelease())
	if deviceID != "" {
		request.Header.Set("X-Msh-Device-Id", deviceID)
	}
}

var (
	kimiHostnameOnce = sync.OnceValue(func() string {
		if name, err := os.Hostname(); err == nil && strings.TrimSpace(name) != "" {
			return strings.TrimSpace(name)
		}
		return "unknown"
	})
	kimiOSReleaseOnce = sync.OnceValue(func() string {
		// 与 zcode-switcher 的 os_release() 一致：uname -r。
		if output, err := exec.Command("uname", "-r").Output(); err == nil {
			if release := strings.TrimSpace(string(output)); release != "" {
				return release
			}
		}
		return "unknown"
	})
)

func kimiHostname() string { return kimiHostnameOnce() }

func kimiOSRelease() string { return kimiOSReleaseOnce() }

// kimiPostForm posts a form to a kimi auth endpoint with the X-Msh-* device
// headers, returning the status and raw body (transport errors are wrapped
// with the 请求失败（网络） marker shared with the rest of the pool).
func kimiPostForm(ctx context.Context, endpoint, deviceID string, form url.Values) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	setKimiDeviceHeaders(request, deviceID)
	response, err := httpClient.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return response.StatusCode, nil, fmt.Errorf("读取响应失败：%w", err)
	}
	return response.StatusCode, body, nil
}

// authFailed marks credentials that the server rejected outright (401/403 or
// invalid_grant/invalid_token): refreshing again will not help, the account
// needs a fresh login.
type authFailedError struct{ message string }

func (e *authFailedError) Error() string { return e.message }

func isAuthFailed(err error) bool {
	var target *authFailedError
	return errors.As(err, &target)
}

// httpClient is shared by every pool request; the 15s budget matches
// zcode-switcher's AbortSignal.timeout(15000).
var httpClient = &http.Client{Timeout: 15 * time.Second}

// kimiGet fetches a JSON document with Bearer auth, retrying 429s with the
// same progressive backoff as zcode-switcher's quota.js (0.5s / 1.5s / 4s)
// and a single 1.5s-delayed retry when every attempt answered 401 (the "first
// activation lag" workaround).
func kimiGet(ctx context.Context, endpoint, accessToken string) (map[string]any, error) {
	backoffs := []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second}
	var lastErr error
	retried401 := false
	for attempt := 0; ; attempt++ {
		body, status, err := kimiGetOnce(ctx, endpoint, accessToken)
		if err == nil {
			var decoded map[string]any
			if jsonErr := json.Unmarshal(body, &decoded); jsonErr != nil {
				return nil, fmt.Errorf("响应不是有效 JSON: %w", jsonErr)
			}
			return decoded, nil
		}
		lastErr = err
		if status == http.StatusUnauthorized || status == http.StatusForbidden || isAuthFailed(err) {
			if status == http.StatusUnauthorized && !retried401 {
				retried401 = true
				if sleepCtx(ctx, 1500*time.Millisecond) != nil {
					return nil, lastErr
				}
				continue
			}
			return nil, lastErr
		}
		if status != http.StatusTooManyRequests || attempt >= len(backoffs) {
			return nil, lastErr
		}
		if sleepCtx(ctx, backoffs[attempt]) != nil {
			return nil, lastErr
		}
	}
}

func kimiGetOnce(ctx context.Context, endpoint, accessToken string) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Accept", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("读取响应失败：%w", err)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return body, response.StatusCode, nil
	}
	preview := string(body)
	if len(preview) > 180 {
		preview = preview[:180]
	}
	message := fmt.Sprintf("HTTP %d：%s", response.StatusCode, preview)
	lower := strings.ToLower(string(body))
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden ||
		strings.Contains(lower, "invalid_grant") || strings.Contains(lower, "invalid_token") {
		return nil, response.StatusCode, &authFailedError{message: message}
	}
	return nil, response.StatusCode, errors.New(message)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// WindowQuota is one rate-limit window (kimi's 5-hour window is the
// windowUnit=minute, windowDuration=300 row).
type WindowQuota struct {
	Name           string `json:"name,omitempty"`
	WindowUnit     string `json:"windowUnit,omitempty"`
	WindowDuration *int64 `json:"windowDuration,omitempty"`
	Used           int64  `json:"used"`
	Limit          int64  `json:"limit"`
	ResetAt        string `json:"resetAt,omitempty"`
	// Auxiliary marks side quotas that never gate AI work (zcode's MCP 用量
	// covers built-in tool calls, not model tokens): exhaustion checks and
	// switch-target ranking skip auxiliary windows; the UI still shows them.
	Auxiliary bool `json:"auxiliary,omitempty"`
}

// UnmarshalJSON tolerates zcode-switcher snapshots, which serialize whole
// numbers as floats (65.0) that encoding/json refuses into int64 fields.
func (w *WindowQuota) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name           string `json:"name"`
		WindowUnit     string `json:"windowUnit"`
		WindowDuration any    `json:"windowDuration"`
		Used           any    `json:"used"`
		Limit          any    `json:"limit"`
		ResetAt        string `json:"resetAt"`
		Auxiliary      bool   `json:"auxiliary"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	w.Name = raw.Name
	w.WindowUnit = raw.WindowUnit
	w.WindowDuration = toInt64(raw.WindowDuration)
	if v := toInt64(raw.Used); v != nil {
		w.Used = *v
	}
	if v := toInt64(raw.Limit); v != nil {
		w.Limit = *v
	}
	w.ResetAt = raw.ResetAt
	w.Auxiliary = raw.Auxiliary
	return nil
}

// BoosterQuota is kimi's booster wallet, converted from the official
// fixed-point amounts (÷1e6) into cents.
type BoosterQuota struct {
	TotalCents   *int64 `json:"totalCents,omitempty"`
	BalanceCents *int64 `json:"balanceCents,omitempty"`
	Currency     string `json:"currency,omitempty"`
}

// UnmarshalJSON tolerates zcode-switcher snapshots; see WindowQuota.
func (b *BoosterQuota) UnmarshalJSON(data []byte) error {
	var raw struct {
		TotalCents   any    `json:"totalCents"`
		BalanceCents any    `json:"balanceCents"`
		Currency     string `json:"currency"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.TotalCents = toInt64(raw.TotalCents)
	b.BalanceCents = toInt64(raw.BalanceCents)
	b.Currency = raw.Currency
	return nil
}

// Quota is the parsed /usages payload; field names match zcode-switcher's
// parseUsages output so imported snapshots round-trip unchanged. MiniMax
// accounts carry the Coding Plan membership block under Membership (the
// zcode-switcher minimax snapshot shape {windows, membership} imports with
// windows folded into Limits/Weekly by refresh and membership kept verbatim).
type Quota struct {
	Weekly     *WindowQuota     `json:"weekly,omitempty"`
	Limits     []*WindowQuota   `json:"limits"`
	Booster    *BoosterQuota    `json:"booster,omitempty"`
	Membership *QuotaMembership `json:"membership,omitempty"`
	// Credits is the WorkBuddy credit summary (points-based quota with
	// per-package expiry); the gating window is the synthetic single
	// "积分" row in Limits.
	Credits      *CreditsQuota     `json:"credits,omitempty"`
	ResetCredits *ZcodeResetStatus `json:"resetCredits,omitempty"`
	LevelName    string            `json:"levelName,omitempty"`
	Region       string            `json:"region,omitempty"`
}

var windowUnitMap = map[string]string{
	"TIME_UNIT_MINUTE": "minute", "minute": "minute",
	"TIME_UNIT_HOUR": "hour", "hour": "hour",
	"TIME_UNIT_DAY": "day", "day": "day",
	"TIME_UNIT_WEEK": "week", "week": "week",
}

func toInt64(value any) *int64 {
	switch v := value.(type) {
	case float64:
		n := int64(v)
		return &n
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return &n
		}
		if f, err := v.Float64(); err == nil {
			n := int64(f)
			return &n
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return &n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			n := int64(f)
			return &n
		}
	}
	return nil
}

func int64Or(value any, fallback int64) int64 {
	if n := toInt64(value); n != nil {
		return *n
	}
	return fallback
}

func objectValue(root map[string]any, key string) map[string]any {
	if nested, ok := root[key].(map[string]any); ok {
		return nested
	}
	return nil
}

func stringValue(root map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := root[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// parseKimiUsages mirrors zcode-switcher's kimiApi.parseUsages: tolerant of
// missing fields, defaults used to 0 (the official CLI shows 0% then).
func parseKimiUsages(payload map[string]any) *Quota {
	quota := &Quota{Limits: []*WindowQuota{}}
	if payload == nil {
		return quota
	}
	if usage := objectValue(payload, "usage"); usage != nil {
		weekly := &WindowQuota{Used: int64Or(usage["used"], 0)}
		if limit := toInt64(usage["limit"]); limit != nil {
			weekly.Limit = *limit
		}
		weekly.ResetAt = stringValue(usage, "resetTime", "reset_time")
		quota.Weekly = weekly
	}
	if limits, ok := payload["limits"].([]any); ok {
		for _, item := range limits {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			detail := objectValue(row, "detail")
			if detail == nil {
				detail = row
			}
			window := &WindowQuota{
				Name:    stringValue(row, "name"),
				Used:    int64Or(detail["used"], 0),
				Limit:   int64Or(detail["limit"], 0),
				ResetAt: stringValue(detail, "resetTime", "reset_time"),
			}
			if window.Name == "" {
				window.Name = stringValue(detail, "name")
			}
			if win := objectValue(row, "window"); win != nil {
				if unit := stringValue(win, "timeUnit", "unit"); unit != "" {
					if mapped, ok := windowUnitMap[unit]; ok {
						window.WindowUnit = mapped
					} else {
						window.WindowUnit = unit
					}
				}
				window.WindowDuration = toInt64(win["duration"])
			}
			quota.Limits = append(quota.Limits, window)
		}
	}
	wallet := objectValue(payload, "boosterWallet")
	if wallet == nil {
		wallet = objectValue(payload, "booster_wallet")
	}
	if wallet != nil {
		if balance := objectValue(wallet, "balance"); balance != nil && balance["type"] == "BOOSTER" {
			booster := &BoosterQuota{}
			if total := toInt64(balance["amount"]); total != nil {
				cents := (*total + 500000) / 1000000
				booster.TotalCents = &cents
			}
			left := toInt64(balance["amountLeft"])
			if left == nil {
				left = toInt64(balance["amount_left"])
			}
			if left != nil {
				cents := (*left + 500000) / 1000000
				booster.BalanceCents = &cents
			}
			if charge := objectValue(wallet, "monthlyChargeLimit"); charge != nil {
				booster.Currency = stringValue(charge, "currency")
			}
			quota.Booster = booster
		}
	}
	return quota
}

// kimiProfile is the parsed /me identity.
type kimiProfile struct {
	UserID    string
	Nickname  string
	Email     string
	Avatar    string
	LevelName string
	Region    string
}

func parseKimiProfile(payload map[string]any) kimiProfile {
	if payload == nil {
		return kimiProfile{}
	}
	return kimiProfile{
		UserID:    stringValue(payload, "user_id"),
		Nickname:  stringValue(payload, "nickname"),
		Email:     stringValue(payload, "email"),
		Avatar:    stringValue(payload, "avatar"),
		LevelName: stringValue(payload, "user_level_name"),
		Region:    stringValue(payload, "region"),
	}
}

// tokenSet is a refreshed OAuth token pair.
type tokenSet struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	ExpiresIn    int64
	ExpiresAt    int64
}

// needsKimiRefresh mirrors oauth.needsRefresh: renew max(expires_in/2, 300s)
// before expiry.
func needsKimiRefresh(expiresAt, expiresIn int64, now time.Time) bool {
	if expiresAt == 0 {
		return false
	}
	lead := int64(refreshLeadSeconds)
	if expiresIn > 0 && expiresIn/2 > lead {
		lead = expiresIn / 2
	}
	return now.Unix()+lead >= expiresAt
}

// refreshKimiToken exchanges a refresh_token for a new pair
// (kimiOauth.refreshToken).
func refreshKimiToken(ctx context.Context, refreshToken, deviceID string) (*tokenSet, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("缺少 refresh_token，请重新登录")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {kimiClientID},
		"refresh_token": {refreshToken},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, kimiTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	setKimiDeviceHeaders(request, deviceID)
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	var data map[string]any
	_ = json.Unmarshal(body, &data)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := fmt.Sprintf("HTTP %d", response.StatusCode)
		if value, ok := data["error"].(string); ok && value != "" {
			code = value
		}
		desc := ""
		if value, ok := data["error_description"].(string); ok && value != "" {
			desc = "（" + value + "）"
		}
		message := fmt.Sprintf("刷新 Kimi token 失败: %s%s", code, desc)
		if code == "invalid_grant" || code == "invalid_token" ||
			response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, &authFailedError{message: message}
		}
		return nil, errors.New(message)
	}
	access, _ := data["access_token"].(string)
	refresh, _ := data["refresh_token"].(string)
	if access == "" || refresh == "" {
		return nil, errors.New("刷新 Kimi token 响应缺少必要字段")
	}
	expiresIn := int64Or(data["expires_in"], 3600)
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return &tokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    stringValue(data, "token_type"),
		Scope:        stringValue(data, "scope"),
		ExpiresIn:    expiresIn,
		ExpiresAt:    time.Now().Unix() + expiresIn,
	}, nil
}

// ===== device flow 登录（RFC 8628，zcode-switcher kimiOauth.js/api.rs 移植）=====

// startKimiLogin launches the kimi device authorization flow and returns the
// user-facing challenge plus a wait closure that polls until the user
// confirms (returning the account candidate for the login state machine to
// persist — nothing is written here). Mirrors zcode-switcher's
// start_device_login + poll_device_login.
func startKimiLogin(ctx context.Context, m *Manager, _ LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error) {
	deviceID, err := ensureKimiDeviceID(m)
	if err != nil {
		return nil, nil, err
	}
	status, body, err := kimiPostForm(ctx, kimiDeviceAuthURL, deviceID, url.Values{"client_id": {kimiClientID}})
	if err != nil {
		return nil, nil, err
	}
	if status < 200 || status >= 300 {
		return nil, nil, fmt.Errorf("Kimi device flow 发起失败（HTTP %d）：%s", status, truncateString(string(body), 200))
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, nil, fmt.Errorf("Kimi device flow 响应不是有效 JSON: %w", err)
	}
	deviceCode := stringValue(data, "device_code")
	userCode := stringValue(data, "user_code")
	if deviceCode == "" || userCode == "" {
		return nil, nil, errors.New("Kimi device flow 响应缺少 device_code/user_code")
	}
	verifyURL := strings.TrimSpace(stringValue(data, "verification_uri_complete"))
	if verifyURL == "" {
		verifyURL = strings.TrimSpace(stringValue(data, "verification_uri"))
	}
	if verifyURL == "" {
		return nil, nil, errors.New("Kimi device flow 响应缺少 verification_uri")
	}
	expiresIn := int64Or(data["expires_in"], kimiLoginMaxSeconds)
	if expiresIn < 1 {
		expiresIn = 1
	}
	if expiresIn > kimiLoginMaxSeconds {
		expiresIn = kimiLoginMaxSeconds
	}
	interval := int64Or(data["interval"], 5)
	if interval < 1 {
		interval = 1
	}
	challenge := &LoginChallenge{UserCode: userCode, URL: verifyURL, ExpiresIn: expiresIn}
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	wait := func(ctx context.Context) (*Account, error) {
		tokens, err := pollKimiDeviceLogin(ctx, deviceCode, deviceID, interval, deadline)
		if err != nil {
			return nil, err
		}
		account := &Account{
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			TokenType:    tokens.TokenType,
			Scope:        tokens.Scope,
			ExpiresIn:    tokens.ExpiresIn,
			ExpiresAt:    tokens.ExpiresAt, // Unix 秒，与 refreshKimiToken 一致
			DeviceID:     deviceID,
		}
		// 补身份（尽力而为，失败不阻断登录；与 pool.go 导入路径同一 /me 解析）。
		if payload, err := kimiGet(ctx, kimiAPIBase+"/me", tokens.AccessToken); err == nil {
			profile := parseKimiProfile(payload)
			account.UserID = profile.UserID
			account.Nickname = profile.Nickname
			account.Email = profile.Email
			account.Avatar = profile.Avatar
			account.LevelName = profile.LevelName
			account.Region = profile.Region
		}
		return account, nil
	}
	return challenge, wait, nil
}

// pollKimiDeviceLogin polls the token endpoint until the user confirms the
// device code (success), the flow fails, or the deadline/ctx gives up.
func pollKimiDeviceLogin(ctx context.Context, deviceCode, deviceID string, interval int64, deadline time.Time) (*tokenSet, error) {
	transportErrors := 0
	for {
		if !time.Now().Before(deadline) {
			return nil, errors.New("登录超时（验证码已过期），请重试")
		}
		if err := sleepCtx(ctx, time.Duration(interval)*time.Second); err != nil {
			return nil, err
		}
		form := url.Values{
			"grant_type":  {kimiDeviceGrantType},
			"device_code": {deviceCode},
			"client_id":   {kimiClientID},
		}
		status, body, err := kimiPostForm(ctx, kimiTokenURL, deviceID, form)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			transportErrors++
			if transportErrors >= kimiLoginMaxTransportErrors {
				return nil, fmt.Errorf("轮询登录状态失败（网络）：%w", err)
			}
			continue
		}
		transportErrors = 0
		var data map[string]any
		_ = json.Unmarshal(body, &data)
		access := stringValue(data, "access_token")
		refresh := stringValue(data, "refresh_token")
		if status >= 200 && status < 300 && access != "" && refresh != "" {
			expiresIn := int64Or(data["expires_in"], 3600)
			if expiresIn <= 0 {
				expiresIn = 3600
			}
			return &tokenSet{
				AccessToken:  access,
				RefreshToken: refresh,
				TokenType:    stringValue(data, "token_type"),
				Scope:        stringValue(data, "scope"),
				ExpiresIn:    expiresIn,
				ExpiresAt:    time.Now().Unix() + expiresIn,
			}, nil
		}
		text := string(body)
		code := stringValue(data, "error")
		switch {
		case code == "authorization_pending" || (code == "" && strings.Contains(text, "authorization_pending")):
			continue
		case code == "slow_down":
			interval += 5
			continue
		case code == "access_denied":
			return nil, errors.New("授权已被拒绝")
		case code == "expired_token":
			return nil, errors.New("验证码已过期，请重试")
		case code != "":
			desc := stringValue(data, "error_description")
			if desc != "" {
				desc = "（" + desc + "）"
			}
			return nil, fmt.Errorf("Kimi 登录失败: %s%s", code, desc)
		default:
			return nil, fmt.Errorf("Kimi token 端点返回异常（HTTP %d）：%s", status, truncateString(text, 200))
		}
	}
}

// truncateString caps s at max bytes, for surfacing response previews in
// user-facing errors (zcode-switcher's chars().take(200)).
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func envOrHome(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
