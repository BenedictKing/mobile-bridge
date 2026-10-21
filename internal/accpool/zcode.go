package accpool

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ZCode billing endpoints, aligned with zcode-switcher's src/quota.js. They
// are package variables so tests can point them at a stub server.
var (
	zcodeBillingCurrentURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/current"
	zcodeBillingBalanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
	// BigModel monitor endpoints are the fallback for apiKeys that belong to
	// the open.bigmodel.cn / api.z.ai systems (zcode.z.ai billing 401s them).
	zcodeMonitorURLs = []string{
		"https://open.bigmodel.cn/api/monitor/usage/quota/limit",
		"https://api.z.ai/api/monitor/usage/quota/limit",
	}
	// coding-plan reset cards (ZCode's 重置 5 小时额度), a package variable so
	// tests can point it at a stub server.
	zcodeResetBaseURL = "https://zcode.z.ai/api/v1/coding-plan/reset"
)

// The ZCode client sends app_version + platform with billing/current; the
// server routes plan versions on them and may answer empty plans otherwise.
const (
	zcodeClientAppVersion = "4.1.10"
	zcodeClientPlatform   = "win32-x64"
)

// zcodeHome resolves the ZCode state directory (ZCODE_HOME wins for tests,
// the real CLI and desktop app always use ~/.zcode).
func zcodeHome() string {
	if value := strings.TrimSpace(os.Getenv("ZCODE_HOME")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".zcode")
	}
	return ""
}

func zcodeCredentialsPath() string {
	if home := zcodeHome(); home != "" {
		return filepath.Join(home, "v2", "credentials.json")
	}
	return ""
}

func zcodeConfigPath() string {
	if home := zcodeHome(); home != "" {
		return filepath.Join(home, "v2", "config.json")
	}
	return ""
}

// zcodeSlot is the pair of files that make up one ZCode login state; both are
// carried as raw text so machine-bound enc:v1 fields round-trip byte-exact.
type zcodeSlot struct {
	Credentials string
	Config      string
}

func readZcodeSlot() *zcodeSlot {
	credPath, cfgPath := zcodeCredentialsPath(), zcodeConfigPath()
	if credPath == "" || cfgPath == "" {
		return nil
	}
	credentials, err := os.ReadFile(credPath)
	if err != nil || len(strings.TrimSpace(string(credentials))) == 0 {
		return nil
	}
	config, err := os.ReadFile(cfgPath)
	if err != nil || len(strings.TrimSpace(string(config))) == 0 {
		return nil
	}
	return &zcodeSlot{Credentials: string(credentials), Config: string(config)}
}

// writeZcodeSlot atomically replaces both slot files.
func writeZcodeSlot(slot *zcodeSlot) error {
	if slot == nil {
		return errors.New("zcode 快照为空")
	}
	if err := atomicWrite(zcodeCredentialsPath(), []byte(slot.Credentials), 0o600); err != nil {
		return fmt.Errorf("写入 credentials.json 失败: %w", err)
	}
	if err := atomicWrite(zcodeConfigPath(), []byte(slot.Config), 0o600); err != nil {
		return fmt.Errorf("写入 config.json 失败: %w", err)
	}
	return nil
}

// --- enc:v1 (zcodeCrypto.js) ------------------------------------------------

const zcodeEncPrefix = "enc:v1:"

func isZcodeEncrypted(value string) bool { return strings.HasPrefix(value, zcodeEncPrefix) }

// zcodeCredentialSecret mirrors zcodeCrypto.defaultCredentialSecret: the
// env override wins, otherwise the machine-bound fallback
// `zcode-credential-fallback:<platform>:<homedir>:<username>`.
func zcodeCredentialSecret() string {
	if value := os.Getenv("ZCODE_CREDENTIAL_SECRET"); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	username := "unknown"
	if current, err := user.Current(); err == nil && current.Username != "" {
		username = current.Username
	}
	return "zcode-credential-fallback:" + runtime.GOOS + ":" + home + ":" + username
}

func zcodeDeriveKey(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// zcodeDecrypt decodes `enc:v1:<nonce>.<tag>.<cipher>` (aes-256-gcm,
// base64url unpadded, key = sha256(secret)). Plaintext passes through.
func zcodeDecrypt(value, secret string) (string, error) {
	if !isZcodeEncrypted(value) {
		return value, nil
	}
	parts := strings.Split(strings.TrimPrefix(value, zcodeEncPrefix), ".")
	if len(parts) != 3 {
		return "", errors.New("enc:v1 格式不正确")
	}
	decode := base64.RawURLEncoding.DecodeString
	nonce, err := decode(parts[0])
	if err != nil {
		return "", fmt.Errorf("enc:v1 nonce 解码失败: %w", err)
	}
	tag, err := decode(parts[1])
	if err != nil {
		return "", fmt.Errorf("enc:v1 authTag 解码失败: %w", err)
	}
	ciphertext, err := decode(parts[2])
	if err != nil {
		return "", fmt.Errorf("enc:v1 密文解码失败: %w", err)
	}
	block, err := aes.NewCipher(zcodeDeriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(nonce))
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return "", fmt.Errorf("enc:v1 解密失败（密钥不匹配？）: %w", err)
	}
	return string(plain), nil
}

// zcodeEncrypt is the symmetric writer, used by tests and future tooling.
func zcodeEncrypt(plain, secret string) (string, error) {
	block, err := aes.NewCipher(zcodeDeriveKey(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(plain), nil)
	tag := sealed[len(sealed)-gcm.Overhead():]
	ciphertext := sealed[:len(sealed)-gcm.Overhead()]
	encode := base64.RawURLEncoding.EncodeToString
	return zcodeEncPrefix + encode(nonce) + "." + encode(tag) + "." + encode(ciphertext), nil
}

// zcodeDecryptSafe never fails out the caller: undecryptable values (foreign
// machine, missing secret) simply yield no token.
func zcodeDecryptSafe(value string) string {
	if value == "" {
		return ""
	}
	plain, err := zcodeDecrypt(value, zcodeCredentialSecret())
	if err != nil {
		return ""
	}
	return plain
}

// --- fingerprint (fingerprint.js) -------------------------------------------

// decodeJWTPayload reads the payload of a JWT without verifying the
// signature, exactly like zcode-switcher's decodeJwt.
func decodeJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	// base64url, tolerating missing padding.
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(decoded, &out) != nil {
		return nil
	}
	return out
}

// djb2Hex mirrors zcode-switcher's simpleHash (djb2 xor variant, unsigned
// 32-bit, hex) used for the email dedupe key.
func djb2Hex(s string) string {
	var h uint32 = 5381
	for i := 0; i < len(s); i++ {
		h = ((h * 33) ^ uint32(s[i])) & 0xffffffff
	}
	return fmt.Sprintf("%x", h)
}

// zcodeFingerprint is the account identity extracted from one login state.
type zcodeFingerprint struct {
	ID         string // emailShortId when an email is known, else shortId
	ShortID    string
	UserID     string
	Email      string
	Name       string
	Avatar     string
	Provider   string
	CustomerID string
	UserKey    string
}

// zcodeFingerprintFrom parses the raw credentials.json/config.json contents.
// Primary source: the enabled provider's plaintext apiKey JWT in config.json
// (payload user_id/sub, must be a string); identity fields come from the
// decrypted oauth user_info. Returns nil when no usable fingerprint exists.
func zcodeFingerprintFrom(credentials, config map[string]any) *zcodeFingerprint {
	profile := zcodeCredentialProfile(credentials)
	providers, _ := config["provider"].(map[string]any)
	type candidate struct {
		id     string
		apiKey string
	}
	var candidates []candidate
	for id, value := range providers {
		provider, _ := value.(map[string]any)
		options, _ := provider["options"].(map[string]any)
		apiKey, _ := options["apiKey"].(string)
		if apiKey == "" || strings.HasPrefix(apiKey, "enc:") || len(apiKey) < 30 {
			continue
		}
		candidates = append(candidates, candidate{id: id, apiKey: apiKey})
	}
	// enabled providers first, then stable id order.
	sort.Slice(candidates, func(i, j int) bool {
		enabledI, _ := providers[candidates[i].id].(map[string]any)["enabled"].(bool)
		enabledJ, _ := providers[candidates[j].id].(map[string]any)["enabled"].(bool)
		if enabledI != enabledJ {
			return enabledI
		}
		return candidates[i].id < candidates[j].id
	})
	for _, c := range candidates {
		payload := decodeJWTPayload(c.apiKey)
		if payload == nil {
			continue
		}
		raw, _ := payload["user_id"].(string)
		if raw == "" {
			raw, _ = payload["sub"].(string)
		}
		if raw == "" {
			continue
		}
		return buildZcodeFingerprint(raw, c.id, payload, profile)
	}
	return nil
}

type zcodeProfile struct {
	ActiveProvider string
	Email          string
	Name           string
	Avatar         string
	UserID         string
}

func zcodeCredentialProfile(credentials map[string]any) *zcodeProfile {
	profile := &zcodeProfile{ActiveProvider: "zai"}
	if credentials == nil {
		return profile
	}
	if raw := zcodeDecryptSafe(stringValue(credentials, "oauth:active_provider")); raw != "" {
		profile.ActiveProvider = raw
	}
	userInfoRaw := zcodeDecryptSafe(stringValue(credentials, "oauth:"+profile.ActiveProvider+":user_info"))
	var userInfo map[string]any
	if userInfoRaw != "" && json.Unmarshal([]byte(userInfoRaw), &userInfo) == nil {
		profile.Email = stringValue(userInfo, "email")
		profile.Name = stringValue(userInfo, "name", "username", "displayName")
		profile.Avatar = stringValue(userInfo, "avatar", "avatarUrl")
		profile.UserID = stringValue(userInfo, "user_id", "id")
	}
	return profile
}

func buildZcodeFingerprint(userID, provider string, payload map[string]any, profile *zcodeProfile) *zcodeFingerprint {
	shortID := userID
	if len(shortID) > 8 {
		shortID = shortID[:8]
	}
	fp := &zcodeFingerprint{
		ShortID:  shortID,
		UserID:   userID,
		Provider: provider,
	}
	if profile != nil {
		fp.Email = profile.Email
		fp.Name = profile.Name
		fp.Avatar = profile.Avatar
	}
	fp.CustomerID, _ = payload["customer_id"].(string)
	fp.UserKey, _ = payload["user_key"].(string)
	fp.ID = fp.ShortID
	if fp.Email != "" {
		id := "em-" + djb2Hex(strings.ToLower(fp.Email))
		if len(id) > 13 {
			id = id[:13]
		}
		fp.ID = id
	}
	return fp
}

// --- candidate tokens (quota.js readCandidateTokensFromObjects) --------------

func looksLikeToken(value string) bool { return len(strings.TrimSpace(value)) > 20 }

// zcodeCandidateTokens collects quota-query tokens in zcode-switcher's order:
// zcodejwttoken first (the one zcode.z.ai billing accepts), then the oauth
// access tokens, then any plaintext provider apiKeys from config.json.
func zcodeCandidateTokens(credentials, config map[string]any) []string {
	activeProvider := "zai"
	if raw := zcodeDecryptSafe(stringValue(credentials, "oauth:active_provider")); raw != "" {
		activeProvider = raw
	}
	var tokens []string
	add := func(value string) {
		plain := zcodeDecryptSafe(value)
		if looksLikeToken(plain) {
			for _, existing := range tokens {
				if existing == plain {
					return
				}
			}
			tokens = append(tokens, plain)
		}
	}
	add(stringValue(credentials, "zcodejwttoken"))
	add(stringValue(credentials, "oauth:zai:access_token"))
	add(stringValue(credentials, "oauth:bigmodel:access_token"))
	add(stringValue(credentials, "oauth:"+activeProvider+":access_token"))
	providers, _ := config["provider"].(map[string]any)
	keys := make([]string, 0, len(providers))
	for id := range providers {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	for _, id := range keys {
		provider, _ := providers[id].(map[string]any)
		options, _ := provider["options"].(map[string]any)
		if apiKey, _ := options["apiKey"].(string); looksLikeToken(apiKey) {
			add(apiKey)
		}
	}
	return tokens
}

func parseJSONObject(text string) map[string]any {
	var out map[string]any
	if text == "" || json.Unmarshal([]byte(text), &out) != nil {
		return nil
	}
	return out
}

// --- quota fetching (quota.js) ------------------------------------------------

func zcodeBillingURL(base string) string {
	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}
	query := parsed.Query()
	query.Set("app_version", zcodeClientAppVersion)
	query.Set("platform", zcodeClientPlatform)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// zcodeGet performs one authorized GET against a JSON endpoint with the
// shared 15s client. bearer selects the "Bearer " prefix (billing) versus a
// bare token (BigModel monitor, which 401s a duplicated scheme).
func zcodeGet(ctx context.Context, endpoint, token string, bearer bool) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	if bearer {
		request.Header.Set("accept", "application/json, text/plain, */*")
		request.Header.Set("authorization", "Bearer "+token)
	} else {
		request.Header.Set("accept", "application/json")
		request.Header.Set("authorization", token)
	}
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
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, &authFailedError{message: "Token 已过期或无效（" + message + "）"}
	}
	return nil, response.StatusCode, errors.New(message)
}

// fetchZcodeBilling fetches one billing endpoint with quota.js's progressive
// 429 backoff (0.5s / 1.5s / 4s).
func fetchZcodeBilling(ctx context.Context, endpoint, token string) (map[string]any, error) {
	backoffs := []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second}
	var lastErr error
	for attempt := 0; ; attempt++ {
		body, status, err := zcodeGet(ctx, endpoint, token, true)
		if err == nil {
			var decoded map[string]any
			if jsonErr := json.Unmarshal(body, &decoded); jsonErr != nil {
				return nil, fmt.Errorf("响应不是有效 JSON: %w", jsonErr)
			}
			return decoded, nil
		}
		lastErr = err
		if status != http.StatusTooManyRequests || attempt >= len(backoffs) {
			return nil, lastErr
		}
		if sleepCtx(ctx, backoffs[attempt]) != nil {
			return nil, lastErr
		}
	}
}

// queryZcodeQuota tries each candidate token against the billing endpoints;
// when every candidate answered 401/403 it waits 1.5s and retries the first
// token once (first-activation lag), then falls back to the BigModel monitor
// endpoints. All-401 at the end means the account needs a fresh login.
func queryZcodeQuota(ctx context.Context, tokens []string) (*Quota, error) {
	if len(tokens) == 0 {
		return nil, errors.New("该账号快照里未找到可用于查询额度的 token")
	}
	var lastErr error
	authFailures := 0
	for _, token := range tokens {
		quota, err := fetchZcodeBillingPair(ctx, token)
		if err == nil {
			return quota, nil
		}
		lastErr = err
		if isAuthFailed(err) {
			authFailures++
		}
	}
	if authFailures == len(tokens) {
		if sleepCtx(ctx, 1500*time.Millisecond) == nil {
			if quota, err := fetchZcodeBillingPair(ctx, tokens[0]); err == nil {
				return quota, nil
			} else {
				lastErr = err
			}
		}
	}
	if quota, err := queryZcodeMonitor(ctx, tokens); err == nil && quota != nil {
		return quota, nil
	}
	if authFailures == len(tokens) {
		return nil, &authFailedError{message: "该账号 Token 已过期，请删除后重新登录"}
	}
	return nil, lastErr
}

func fetchZcodeBillingPair(ctx context.Context, token string) (*Quota, error) {
	current, err := fetchZcodeBilling(ctx, zcodeBillingURL(zcodeBillingCurrentURL), token)
	if err != nil {
		return nil, err
	}
	balance, err := fetchZcodeBilling(ctx, zcodeBillingBalanceURL, token)
	if err != nil {
		return nil, err
	}
	return normalizeZcodeQuota(current, balance), nil
}

// fetchZcodeMonitor queries one BigModel monitor endpoint; the payload must
// carry code == 200 and a data object to count as usable.
func fetchZcodeMonitor(ctx context.Context, endpoint, token string) (map[string]any, error) {
	body, _, err := zcodeGet(ctx, endpoint, token, false)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if json.Unmarshal(body, &decoded) != nil {
		return nil, errors.New("monitor 响应不是有效 JSON")
	}
	if code := int64Or(decoded["code"], 0); code != 200 {
		return nil, fmt.Errorf("monitor 响应 code=%d", code)
	}
	if success, ok := decoded["success"].(bool); ok && !success {
		return nil, errors.New("monitor 响应 success=false")
	}
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		return nil, errors.New("monitor 响应缺少 data")
	}
	return decoded, nil
}

func queryZcodeMonitor(ctx context.Context, tokens []string) (*Quota, error) {
	var lastErr error
	for _, token := range tokens {
		for _, endpoint := range zcodeMonitorURLs {
			body, err := fetchZcodeMonitor(ctx, endpoint, token)
			if err != nil {
				lastErr = err
				continue
			}
			return normalizeZcodeMonitor(body), nil
		}
	}
	return nil, lastErr
}

// --- quota normalization (quota.js normalizeQuota / normalizeMonitorQuota) ----

// unwrapData peels up to four data/result envelopes (unwrap in quota.js).
func unwrapData(data map[string]any) map[string]any {
	current := data
	for i := 0; i < 4; i++ {
		if current == nil {
			return nil
		}
		if nested, ok := current["data"].(map[string]any); ok {
			current = nested
			continue
		}
		if nested, ok := current["result"].(map[string]any); ok {
			current = nested
			continue
		}
		break
	}
	if current == nil {
		return map[string]any{}
	}
	return current
}

// toFloat accepts numbers, json.Number and comma-grouped numeric strings
// (quota.js toNumber).
func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f, true
		}
	case string:
		text := strings.TrimSpace(strings.ReplaceAll(v, ",", ""))
		if text == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(text, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// flattenNumbers walks the object tree collecting numeric leaves keyed by
// dotted path (flattenNumbers in quota.js).
func flattenNumbers(obj map[string]any, prefix string, out map[string]float64) {
	for key, value := range obj {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if n, ok := toFloat(value); ok {
			out[path] = n
			continue
		}
		if nested, ok := value.(map[string]any); ok {
			flattenNumbers(nested, path, out)
		}
		if list, ok := value.([]any); ok {
			for i, item := range list {
				if nested, ok := item.(map[string]any); ok {
					flattenNumbers(nested, fmt.Sprintf("%s.%d", path, i), out)
				}
			}
		}
	}
}

func lastPathSegment(path string) string {
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func sumByLeafName(pool map[string]float64, names ...string) (float64, bool) {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	total := 0.0
	count := 0
	for path, value := range pool {
		if wanted[lastPathSegment(path)] {
			total += value
			count++
		}
	}
	return total, count > 0
}

func firstByLeafName(pool map[string]float64, names ...string) (float64, bool) {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	keys := make([]string, 0, len(pool))
	for path := range pool {
		keys = append(keys, path)
	}
	sort.Strings(keys)
	for _, path := range keys {
		if wanted[lastPathSegment(path)] {
			return pool[path], true
		}
	}
	return 0, false
}

// normalizeResetAt renders the various reset-time shapes (RFC3339 text,
// epoch seconds/millis) as RFC3339 so the auto-switch gate can parse them.
func normalizeResetAt(value any) string {
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return ""
		}
		if n, ok := toFloat(text); ok {
			return epochToRFC3339(int64(n))
		}
		return text
	case float64:
		return epochToRFC3339(int64(v))
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return epochToRFC3339(n)
		}
	}
	return ""
}

func epochToRFC3339(value int64) string {
	if value <= 0 {
		return ""
	}
	if value > 1e12 {
		return time.UnixMilli(value).UTC().Format(time.RFC3339)
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

func quotaWindow(name string, used, limit float64, resetAt any) *WindowQuota {
	return &WindowQuota{
		Name:    name,
		Used:    int64(used + 0.5),
		Limit:   int64(limit + 0.5),
		ResetAt: normalizeResetAt(resetAt),
	}
}

// extractPlanTier replicates the ZCode client plan labeling: active plans
// whose id/name contains max/pro/lite/start (Max first to avoid fuzzy hits).
func extractPlanTier(currentData map[string]any) string {
	current := unwrapData(currentData)
	plans, _ := current["plans"].([]any)
	matchActive := func(keywords ...string) bool {
		for _, item := range plans {
			plan, _ := item.(map[string]any)
			if plan == nil || !strings.EqualFold(stringValue(plan, "status"), "active") {
				continue
			}
			haystack := strings.ToLower(stringValue(plan, "plan_id") + " " + stringValue(plan, "name"))
			for _, keyword := range keywords {
				if strings.Contains(haystack, keyword) {
					return true
				}
			}
		}
		return false
	}
	switch {
	case matchActive("max"):
		return "Max"
	case matchActive("pro"):
		return "Pro"
	case matchActive("lite"):
		return "Lite"
	case matchActive("start-plan", "start plan"):
		return "Start Plan"
	}
	return ""
}

// normalizeZcodeQuota folds billing/current + billing/balance into the pool's
// Quota shape: every balance item becomes one window, with a synthetic total
// window when only aggregate numbers exist.
func normalizeZcodeQuota(currentData, balanceData map[string]any) *Quota {
	quota := &Quota{Limits: []*WindowQuota{}}
	current := unwrapData(currentData)
	balance := unwrapData(balanceData)

	pool := map[string]float64{}
	flattenNumbers(map[string]any{"current": current, "balance": balance}, "", pool)
	total, okTotal := sumByLeafName(pool, "total_units")
	if !okTotal {
		total, okTotal = firstByLeafName(pool, "total", "totalQuota", "totalCredits", "quotaTotal", "amountTotal", "creditTotal")
	}
	used, okUsed := sumByLeafName(pool, "used_units")
	if !okUsed {
		used, okUsed = firstByLeafName(pool, "used", "usedQuota", "usedCredits", "quotaUsed", "amountUsed", "consumed", "totalUsed")
	}
	remaining, okRemaining := sumByLeafName(pool, "remaining_units")
	if !okRemaining {
		remaining, okRemaining = firstByLeafName(pool, "remaining", "remain", "balance", "available", "availableQuota", "left", "quotaRemaining")
	}
	if !okTotal && okUsed && okRemaining {
		total, okTotal = used+remaining, true
	}
	if !okUsed && okTotal && okRemaining {
		used, okUsed = total-remaining, true
		if used < 0 {
			used = 0
		}
	}

	balances, _ := balance["balances"].([]any)
	for _, item := range balances {
		row, _ := item.(map[string]any)
		if row == nil {
			continue
		}
		itemTotal, _ := toFloat(row["total_units"])
		itemUsed, _ := toFloat(row["used_units"])
		name := stringValue(row, "show_name", "name", "entitlement_id", "plan_id")
		if name == "" {
			name = "未知模型"
		}
		reset := row["period_end"]
		if reset == nil {
			reset = row["expires_at"]
		}
		quota.Limits = append(quota.Limits, quotaWindow(name, itemUsed, itemTotal, reset))
	}
	if len(quota.Limits) == 0 && okTotal && total > 0 && okUsed {
		quota.Limits = append(quota.Limits, quotaWindow("总额度", used, total, nil))
	}
	quota.LevelName = extractPlanTier(currentData)
	return quota
}

var monitorTypeLabels = map[string]string{
	// TIME_LIMIT is the built-in tools call quota (search-prime/web-reader/
	// zread), reset monthly, marketed as MCP 用量.
	"TIME_LIMIT":   "MCP 用量",
	"TOKENS_LIMIT": "Token 用量",
}

// normalizeZcodeMonitor maps the BigModel monitor payload onto the same Quota
// shape (normalizeMonitorQuota in quota.js).
func normalizeZcodeMonitor(body map[string]any) *Quota {
	quota := &Quota{Limits: []*WindowQuota{}}
	data, _ := body["data"].(map[string]any)
	limits, _ := data["limits"].([]any)
	order := map[string]int{"TOKENS_LIMIT": 0, "TIME_LIMIT": 1}
	rows := make([]map[string]any, 0, len(limits))
	for _, item := range limits {
		if row, ok := item.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return order[stringValue(rows[i], "type")] < order[stringValue(rows[j], "type")]
	})
	for _, row := range rows {
		kind := stringValue(row, "type")
		name := monitorTypeLabels[kind]
		if name == "" {
			name = kind
		}
		if name == "" {
			name = "限额"
		}
		total, hasTotal := toFloat(row["usage"])
		used, hasUsed := toFloat(row["currentValue"])
		if (!hasTotal || total <= 0) || !hasUsed {
			// TOKENS_LIMIT rows carry no absolute numbers at all — only
			// percentage (已用%) and nextResetTime; without this fallback the
			// window parses as 0/0 and the UI reports 暂未提供 while
			// zcode-switcher (which reads percentage) shows live numbers.
			if pct, ok := toFloat(row["percentage"]); ok && pct >= 0 {
				total, used, hasTotal, hasUsed = 1000, pct*10, true, true
			}
		}
		if !hasTotal {
			total = 0
		}
		if !hasUsed {
			used = 0
		}
		window := quotaWindow(name, used, total, row["nextResetTime"])
		if kind == "TIME_LIMIT" {
			// MCP 用量 covers built-in tool calls, not model tokens; it never
			// gates account availability.
			window.Auxiliary = true
		}
		quota.Limits = append(quota.Limits, window)
	}
	switch strings.ToLower(stringValue(data, "level")) {
	case "max":
		quota.LevelName = "Max"
	case "pro":
		quota.LevelName = "Pro"
	case "lite":
		quota.LevelName = "Lite"
	}
	return quota
}

// --- coding-plan reset cards (ZCode 的「重置 5 小时额度」) --------------------
// 端点契约来自官方 ZCode 桌面端 bigmodelUsageQuotaProvider：
//   GET  {base}/status  → 已持有的重置卡与最近核销历史
//   POST {base}/use     → {idempotency_key, reset_type} 核销一张
// 鉴权双 token：Authorization: Bearer <zcodejwttoken>；X-Bigmodel-Authorization
// 直传当前 provider family 的 oauth access_token（不套 Bearer）。

// zcodeResetAuth carries the two tokens the reset endpoints require.
type zcodeResetAuth struct {
	zcodeJWT  string
	planToken string
}

// zcodeResetAuthorization decrypts the reset token pair from a snapshot.
// Missing either token means the account cannot use reset cards (older
// snapshots, apiKey-only logins).
func zcodeResetAuthorization(acc *Account) *zcodeResetAuth {
	credentials := parseJSONObject(acc.Credentials)
	if credentials == nil {
		return nil
	}
	jwt := zcodeDecryptSafe(stringValue(credentials, "zcodejwttoken"))
	if !looksLikeToken(jwt) {
		return nil
	}
	// The plan token must follow the active provider family — the server
	// rejects cross-family tokens (same rule as the official client).
	activeProvider := zcodeDecryptSafe(stringValue(credentials, "oauth:active_provider"))
	if activeProvider == "" {
		activeProvider = "zai"
	}
	if activeProvider != "zai" {
		activeProvider = "bigmodel"
	}
	planToken := zcodeDecryptSafe(stringValue(credentials, "oauth:"+activeProvider+":access_token"))
	if !looksLikeToken(planToken) {
		return nil
	}
	return &zcodeResetAuth{zcodeJWT: jwt, planToken: planToken}
}

// zcodeResetRequest performs one authorized call against the reset endpoints;
// the business envelope is {code, msg, data} with code == 0 meaning success.
func zcodeResetRequest(ctx context.Context, method, endpoint string, auth *zcodeResetAuth, body []byte) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+auth.zcodeJWT)
	request.Header.Set("X-Bigmodel-Authorization", auth.planToken)
	request.Header.Set("Bigmodel-Target-Type", "PERSONAL")
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		preview := string(raw)
		if len(preview) > 180 {
			preview = preview[:180]
		}
		message := fmt.Sprintf("HTTP %d：%s", response.StatusCode, preview)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, &authFailedError{message: message}
		}
		return nil, errors.New(message)
	}
	var envelope struct {
		Code any            `json:"code"`
		Msg  string         `json:"msg"`
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return nil, errors.New("重置卡接口响应不是有效 JSON")
	}
	if code := int64Or(envelope.Code, 0); code != 0 {
		if envelope.Msg == "" {
			envelope.Msg = fmt.Sprintf("code=%d", code)
		}
		return nil, errors.New(envelope.Msg)
	}
	return envelope.Data, nil
}

// ZcodeResetStatus reports the reset cards an account currently holds.
type ZcodeResetStatus struct {
	FiveHourCards int
	WeekCards     int
}

// zcodeResetStatus reads the held cards (GET /status).
func zcodeResetStatus(ctx context.Context, acc *Account) (*ZcodeResetStatus, error) {
	auth := zcodeResetAuthorization(acc)
	if auth == nil {
		return nil, errors.New("账号快照缺少重置卡所需的 token")
	}
	data, err := zcodeResetRequest(ctx, http.MethodGet, zcodeResetBaseURL+"/status", auth, nil)
	if err != nil {
		return nil, err
	}
	status := &ZcodeResetStatus{}
	if items, ok := data["available_five_hour_resets"].([]any); ok {
		status.FiveHourCards = len(items)
	}
	if items, ok := data["available_week_resets"].([]any); ok {
		status.WeekCards = len(items)
	}
	return status, nil
}

// useZcodeResetCard spends one card of resetType ("FIVE_HOUR" | "WEEK") and
// reports whether the server consumed it.
func useZcodeResetCard(ctx context.Context, acc *Account, resetType string) (bool, error) {
	auth := zcodeResetAuthorization(acc)
	if auth == nil {
		return false, errors.New("账号快照缺少重置卡所需的 token")
	}
	body, err := json.Marshal(map[string]string{
		"idempotency_key": uuid.NewString(),
		"reset_type":      resetType,
	})
	if err != nil {
		return false, err
	}
	data, err := zcodeResetRequest(ctx, http.MethodPost, zcodeResetBaseURL+"/use", auth, body)
	if err != nil {
		return false, err
	}
	used, _ := data["used"].(bool)
	return used, nil
}

// --- pool integration ---------------------------------------------------------

// refreshZcodeQuota is the zcode RefreshQuota branch: no token refresh flow
// exists (provider apiKeys are JWTs), so it decrypts candidate tokens from
// the snapshot and polls billing, marking the account reauth_required when
// every credential is rejected.
func (m *Manager) refreshZcodeQuota(ctx context.Context, acc *Account) (*Account, error) {
	credentials := parseJSONObject(acc.Credentials)
	config := parseJSONObject(acc.Config)
	tokens := zcodeCandidateTokens(credentials, config)
	quota, err := queryZcodeQuota(ctx, tokens)
	if err != nil {
		acc.QuotaError = err.Error()
		if isAuthFailed(err) {
			acc.Status = StatusReauthRequired
			acc.StatusReason = err.Error()
		}
		_ = m.saveAccount("zcode", acc)
		return acc, err
	}
	if quota.LevelName == "" {
		quota.LevelName = acc.LevelName
	}
	acc.Quota = quota
	acc.QuotaUpdatedAt = m.now().UnixMilli()
	acc.QuotaError = ""
	if quota.LevelName != "" {
		acc.LevelName = quota.LevelName
	}
	if acc.Status == StatusReauthRequired {
		acc.Status = StatusActive
		acc.StatusReason = ""
	}
	return acc, m.saveAccount("zcode", acc)
}

// activeIDZcode matches the slot fingerprint against pooled accounts by
// userId, then email, then the derived snapshot id.
func (m *Manager) activeIDZcode() string {
	slot := readZcodeSlot()
	if slot == nil {
		return ""
	}
	fp := zcodeFingerprintFrom(parseJSONObject(slot.Credentials), parseJSONObject(slot.Config))
	if fp == nil {
		return ""
	}
	if fp.UserID != "" {
		if match := m.findAccount("zcode", func(a *Account) bool { return a.UserID != "" && a.UserID == fp.UserID }); match != nil {
			return match.ID
		}
	}
	if fp.Email != "" {
		if match := m.findAccount("zcode", func(a *Account) bool { return a.Email != "" && a.Email == fp.Email }); match != nil {
			return match.ID
		}
	}
	if _, err := m.loadAccount("zcode", fp.ID); err == nil {
		return fp.ID
	}
	return ""
}

// captureZcodeCurrent snapshots the current ZCode login state into the pool
// (manager.capture / syncCurrent in zcode-switcher).
func (m *Manager) captureZcodeCurrent() (*Account, bool, error) {
	slot := readZcodeSlot()
	if slot == nil {
		return nil, false, errors.New("未找到 ZCode 登录态（请先在 ZCode 里登录任意账号）")
	}
	fp := zcodeFingerprintFrom(parseJSONObject(slot.Credentials), parseJSONObject(slot.Config))
	if fp == nil {
		return nil, false, errors.New("无法从登录态确定账号标识（user_id 为空），已放弃保存")
	}
	label := fp.Email
	if label == "" {
		label = fp.Name
	}
	if label == "" {
		label = "账号-" + fp.ShortID
	}
	candidate := &Account{
		ID:          fp.ID,
		Label:       label,
		Email:       fp.Email,
		Nickname:    fp.Name,
		UserID:      fp.UserID,
		Avatar:      fp.Avatar,
		Provider:    fp.Provider,
		Credentials: slot.Credentials,
		Config:      slot.Config,
	}
	return m.upsert("zcode", candidate)
}

// zcodeMeta is the zcode-switcher accounts-zcode/<id>.meta.json sidecar.
type zcodeMeta struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Note       string `json:"note"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Avatar     string `json:"avatar"`
	UserID     string `json:"userId"`
	Provider   string `json:"provider"`
	CustomerID string `json:"customerId"`
	UserKey    string `json:"userKey"`
	CapturedAt int64  `json:"capturedAt"`
}

// importZcode folds zcode-switcher's <id>.meta.json + <id>.snap.json pairs
// into the pool's single-file account format; snapshots stay byte-exact so
// machine-bound enc:v1 fields survive the round trip.
func (m *Manager) importZcode(dir string) (imported []string, skipped []ImportSkip, err error) {
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return nil, nil, fmt.Errorf("读取导入目录失败: %w", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		raw, readFileErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readFileErr != nil {
			continue
		}
		var meta zcodeMeta
		if json.Unmarshal(raw, &meta) != nil || meta.ID == "" {
			skipped = append(skipped, ImportSkip{ID: entry.Name(), Reason: "meta 不是有效快照描述"})
			continue
		}
		if _, idErr := safeID(meta.ID); idErr != nil {
			skipped = append(skipped, ImportSkip{ID: meta.ID, Reason: idErr.Error()})
			continue
		}
		snapRaw, snapErr := os.ReadFile(filepath.Join(dir, meta.ID+".snap.json"))
		if snapErr != nil {
			skipped = append(skipped, ImportSkip{ID: meta.ID, Reason: "缺少对应 .snap.json"})
			continue
		}
		var snap zcodeSlot
		if json.Unmarshal(snapRaw, &snap) != nil ||
			parseJSONObject(snap.Credentials) == nil || parseJSONObject(snap.Config) == nil {
			skipped = append(skipped, ImportSkip{ID: meta.ID, Reason: "snap 里的 credentials/config 不是有效 JSON 文本"})
			continue
		}
		if _, statErr := os.Stat(filepath.Join(m.storeDir("zcode"), meta.ID+".json")); statErr == nil {
			skipped = append(skipped, ImportSkip{ID: meta.ID, Reason: "已存在"})
			continue
		}
		acc := &Account{
			ID:          meta.ID,
			Label:       firstNonEmpty(meta.Label, meta.Email, meta.Name, meta.ID),
			Note:        meta.Note,
			Email:       meta.Email,
			Nickname:    meta.Name,
			UserID:      meta.UserID,
			Avatar:      meta.Avatar,
			Provider:    meta.Provider,
			Credentials: snap.Credentials,
			Config:      snap.Config,
			Status:      StatusActive,
			CreatedAt:   meta.CapturedAt,
		}
		if saveErr := m.saveAccount("zcode", acc); saveErr != nil {
			skipped = append(skipped, ImportSkip{ID: meta.ID, Reason: saveErr.Error()})
			continue
		}
		imported = append(imported, meta.ID)
	}
	return imported, skipped, nil
}

// --- ZCode desktop process control (switcher.js) ------------------------------
//
// The desktop app rewrites credentials.json/config.json while running, so a
// switch must stop it first. All three hooks are package variables so tests
// can replace them.

// zcodeDesktopRunning reports whether the ZCode desktop app is up. On darwin
// pgrep cannot see the main process (zcode-switcher comment), so match the
// ps ucomm column exactly; Helpers ("ZCode Helper…") and the CLI never match.
var zcodeDesktopRunning = func() bool {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ps", "-axo", "ucomm=").Output()
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.TrimSpace(line) == "ZCode" {
				return true
			}
		}
	case "windows":
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq ZCode.exe", "/NH", "/FO", "CSV").Output()
		if err == nil && strings.Contains(strings.ToLower(string(out)), `"zcode.exe"`) {
			return true
		}
	}
	return false
}

// zcodeDesktopStop terminates the desktop app and waits until it is gone.
var zcodeDesktopStop = func(wait time.Duration) bool {
	if !zcodeDesktopRunning() {
		return true
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("ps", "-axo", "pid=,ucomm=").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 2 && fields[1] == "ZCode" {
					if pid, convErr := strconv.Atoi(fields[0]); convErr == nil {
						if process, findErr := os.FindProcess(pid); findErr == nil {
							_ = process.Signal(syscall.SIGTERM)
						}
					}
				}
			}
		}
	case "windows":
		_ = exec.Command("taskkill", "/F", "/IM", "ZCode.exe").Run()
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !zcodeDesktopRunning() {
			return true
		}
		time.Sleep(400 * time.Millisecond)
	}
	return !zcodeDesktopRunning()
}

// zcodeDesktopRelaunch reopens the desktop app after a switch (best effort;
// headless hosts simply have nothing to launch).
var zcodeDesktopRelaunch = func() {
	if runtime.GOOS != "darwin" {
		return
	}
	home, _ := os.UserHomeDir()
	for _, app := range []string{"/Applications/ZCode.app", filepath.Join(home, "Applications", "ZCode.app")} {
		if _, err := os.Stat(app); err == nil {
			cmd := exec.Command("open", app)
			_ = cmd.Start()
			return
		}
	}
}

// --- in-gateway OAuth login (zcode-switcher zcode/oauth.rs) -------------------
//
// Server-proxied OAuth: the gateway POSTs cli/init with a local random
// pollToken, hands the authorize_url to the user (web UI opens the browser),
// polls cli/poll until ready, and for zai exchanges the OAuth access_token for
// the business token that actually goes into the credentials. The wait
// closure returns an unpersisted candidate; login.go's state machine owns
// persistence and the slot write happens later, on switch.

var (
	zcodeOAuthInitURL       = "https://zcode.z.ai/api/v1/oauth/cli/init"
	zcodeOAuthPollURLPrefix = "https://zcode.z.ai/api/v1/oauth/cli/poll/"
	zcodeBusinessLoginURL   = "https://api.z.ai/api/auth/z/login"
	// The login flow's billing-ready probe reports the desktop-arm platform
	// tag (unlike the quota path's win32-x64).
	zcodeBillingReadyURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/current?app_version=4.1.10&platform=darwin-arm64"
	// Progressive billing-ready delays (~4 minutes total), best effort.
	zcodeBillingReadyDelays = []time.Duration{
		time.Second, 3 * time.Second, 6 * time.Second, 15 * time.Second,
		30 * time.Second, time.Minute, 90 * time.Second, 2 * time.Minute,
	}
)

// zcodeLoginFlowTimeoutCap mirrors the official 300s per-flow ceiling
// (ese=300s): the poll loop stops at min(300s, the flow's remaining lifetime).
const zcodeLoginFlowTimeoutCap = 300 * time.Second

// zcodeBusinessLoginTimeout mirrors ZaiBusinessTokenResolver's 10s timeout.
const zcodeBusinessLoginTimeout = 10 * time.Second

func init() { registerLoginDriver("zcode", startZcodeLogin) }

// zcodeOAuthFlow is one started login flow (start_oauth_flow's OAuthFlow).
type zcodeOAuthFlow struct {
	provider     string // "zai" | "bigmodel"
	flowID       string
	pollToken    string // local random 32-byte hex; init/poll Bearer credential
	pollInterval time.Duration
	expiresAt    time.Time // flow lifetime claimed by the server
	deadline     time.Time // min(300s cap, remaining lifetime) from start
}

// zcodeReadyTokens is the ready response's token set (oauth.rs ReadyTokens):
// zcodeJWT is the API JWT; accessToken is the provider OAuth access token —
// for zai it is exchanged for the business token before hitting the disk.
type zcodeReadyTokens struct {
	zcodeJWT     string
	accessToken  string
	refreshToken string // bigmodel only
	user         map[string]any
}

// startZcodeLogin implements loginStart: POST cli/init, validate the response
// like the official client (https + non-empty state + sane expiry/interval),
// and hand back the challenge plus a wait closure that polls to completion.
func startZcodeLogin(ctx context.Context, m *Manager, opts LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error) {
	provider := strings.TrimSpace(opts.Provider)
	if provider == "" {
		provider = "zai"
	}
	if provider != "zai" && provider != "bigmodel" {
		return nil, nil, fmt.Errorf("不支持的 provider: %s（仅支持 zai / bigmodel）", provider)
	}
	pollTokenBytes := make([]byte, 32)
	if _, err := rand.Read(pollTokenBytes); err != nil {
		return nil, nil, fmt.Errorf("生成 pollToken 失败: %w", err)
	}
	pollToken := hex.EncodeToString(pollTokenBytes)
	body, err := zcodePostJSON(ctx, zcodeOAuthInitURL, pollToken, map[string]any{"provider": provider})
	if err != nil {
		return nil, nil, fmt.Errorf("OAuth flow 初始化请求失败: %w", err)
	}
	flow, challenge, err := parseZcodeInitResponse(body, provider, pollToken, time.Now())
	if err != nil {
		return nil, nil, err
	}
	return challenge, func(ctx context.Context) (*Account, error) {
		return finishZcodeLogin(ctx, flow)
	}, nil
}

// parseZcodeInitResponse mirrors oauth.rs parse_init_response: code===0,
// flow_id/authorize_url non-empty, authorize_url must be https and carry a
// non-empty state, expires_at in the future, poll_interval ∈ [1s, remaining).
func parseZcodeInitResponse(body []byte, provider, pollToken string, now time.Time) (*zcodeOAuthFlow, *LoginChallenge, error) {
	decoded := parseJSONObject(string(body))
	invalid := func() error {
		if decoded == nil {
			return errors.New("OAuth flow 初始化响应无效")
		}
		if msg := strings.TrimSpace(stringValue(decoded, "msg")); msg != "" {
			return errors.New(msg)
		}
		return errors.New("OAuth flow 初始化响应无效")
	}
	if decoded == nil || int64Or(decoded["code"], -1) != 0 {
		return nil, nil, invalid()
	}
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		return nil, nil, invalid()
	}
	flowID := strings.TrimSpace(stringValue(data, "flow_id"))
	authorizeRaw := strings.TrimSpace(stringValue(data, "authorize_url"))
	expiresRaw, okExpires := data["expires_at"].(float64)
	pollRaw, okPoll := data["poll_interval_sec"].(float64)
	if flowID == "" || authorizeRaw == "" || !okExpires || !okPoll {
		return nil, nil, invalid()
	}
	parsed, err := url.Parse(authorizeRaw)
	if err != nil {
		return nil, nil, invalid()
	}
	state := ""
	if parsed.Scheme == "https" {
		if query, queryErr := url.ParseQuery(parsed.RawQuery); queryErr == nil {
			state = strings.TrimSpace(query.Get("state"))
		}
	}
	expiresAt := time.UnixMilli(int64(expiresRaw * 1000))
	remaining := expiresAt.Sub(now)
	pollInterval := time.Duration(pollRaw * float64(time.Second))
	if parsed.Scheme != "https" || state == "" || remaining <= 0 ||
		pollInterval < time.Second || pollInterval >= remaining {
		return nil, nil, invalid()
	}
	timeout := zcodeLoginFlowTimeoutCap
	if remaining < timeout {
		timeout = remaining
	}
	flow := &zcodeOAuthFlow{
		provider:     provider,
		flowID:       flowID,
		pollToken:    pollToken,
		pollInterval: pollInterval,
		expiresAt:    expiresAt,
		deadline:     now.Add(timeout),
	}
	challenge := &LoginChallenge{URL: parsed.String(), ExpiresIn: int64(remaining / time.Second)}
	return flow, challenge, nil
}

// zcodePostJSON POSTs one JSON body, returning the raw response body like
// oauth.rs read_json (any HTTP status — the caller's parser validates the
// envelope). A non-empty bearer sets the Authorization: Bearer header.
func zcodePostJSON(ctx context.Context, endpoint, bearer string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("accept", "application/json")
	if bearer != "" {
		request.Header.Set("authorization", "Bearer "+bearer)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	return payload, nil
}

// poll runs the cli/poll loop (oauth.rs poll_oauth_flow): pending keeps
// waiting, failed errors out, ready returns the token set; network errors and
// 5xx retry next tick, other 4xx are fatal. ctx cancellation wins over the
// interval sleep.
func (f *zcodeOAuthFlow) poll(ctx context.Context) (map[string]any, error) {
	pollURL := zcodeOAuthPollURLPrefix + url.QueryEscape(f.flowID)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		now := time.Now()
		if !now.Before(f.expiresAt) {
			return nil, errors.New("OAuth flow 已过期")
		}
		if !now.Before(f.deadline) {
			return nil, errors.New("登录超时，请重试")
		}
		body, status, err := zcodeGet(ctx, pollURL, f.pollToken, true)
		if err != nil {
			// 4xx（408/429 除外）是致命的；其余（网络抖动、5xx）下一周期重试。
			if status >= 400 && status < 500 &&
				status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
				return nil, fmt.Errorf("OAuth flow 查询失败 HTTP %d", status)
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if sleepErr := sleepCtx(ctx, f.pollInterval); sleepErr != nil {
				return nil, ctx.Err()
			}
			continue
		}
		decoded := parseJSONObject(string(body))
		data, _ := decoded["data"].(map[string]any)
		if decoded == nil || int64Or(decoded["code"], -1) != 0 || data == nil {
			if decoded != nil {
				if msg := strings.TrimSpace(stringValue(decoded, "msg")); msg != "" {
					return nil, errors.New(msg)
				}
			}
			return nil, errors.New("OAuth flow 查询响应无效")
		}
		switch status := strings.TrimSpace(stringValue(data, "status")); status {
		case "pending":
			if sleepErr := sleepCtx(ctx, f.pollInterval); sleepErr != nil {
				return nil, ctx.Err()
			}
		case "failed":
			return nil, errors.New("OAuth flow 授权失败")
		case "ready":
			return data, nil
		default:
			return nil, errors.New("OAuth flow 查询响应无效")
		}
	}
}

// parseZcodeReady mirrors parse_ready_token_fields: token / provider
// access_token / user.user_id all required; bigmodel gets snake/camel
// fallbacks plus the refresh_token.
func parseZcodeReady(data map[string]any, provider string) (*zcodeReadyTokens, error) {
	user, _ := data["user"].(map[string]any)
	token := strings.TrimSpace(stringValue(data, "token"))
	userID := strings.TrimSpace(stringValue(user, "user_id"))
	tokens := &zcodeReadyTokens{zcodeJWT: token, user: user}
	if provider == "bigmodel" {
		bigmodel, _ := data["bigmodel"].(map[string]any)
		tokens.accessToken = strings.TrimSpace(stringValue(bigmodel, "access_token", "accessToken"))
		tokens.refreshToken = strings.TrimSpace(stringValue(bigmodel, "refresh_token", "refreshToken"))
	} else {
		zai, _ := data["zai"].(map[string]any)
		tokens.accessToken = strings.TrimSpace(stringValue(zai, "access_token"))
	}
	if token == "" || tokens.accessToken == "" || userID == "" {
		return nil, errors.New("OAuth flow 查询响应无效")
	}
	return tokens, nil
}

// zcodeBusinessCodeOK mirrors is_successful_business_code: code ∈
// {null, 0, 200, "0", "200"} and success !== false.
func zcodeBusinessCodeOK(decoded map[string]any) bool {
	codeOK := true
	if code, present := decoded["code"]; present && code != nil {
		codeOK = false
		if n, ok := toFloat(code); ok && (n == 0 || n == 200) {
			codeOK = true
		}
		if s, ok := code.(string); ok && (s == "0" || s == "200") {
			codeOK = true
		}
	}
	if success, ok := decoded["success"].(bool); ok && !success {
		return false
	}
	return codeOK
}

// exchangeZcodeBusinessToken mirrors exchange_business_token: zai's OAuth
// access_token only becomes usable after POST api.z.ai/api/auth/z/login, whose
// access_token (the business token) is what the credentials carry.
func exchangeZcodeBusinessToken(ctx context.Context, zaiAccessToken string) (string, error) {
	exchangeCtx, cancel := context.WithTimeout(ctx, zcodeBusinessLoginTimeout)
	defer cancel()
	body, err := zcodePostJSON(exchangeCtx, zcodeBusinessLoginURL, "", map[string]any{"token": zaiAccessToken})
	if err != nil {
		return "", fmt.Errorf("zai_oauth_required: business token 请求失败: %w", err)
	}
	decoded := parseJSONObject(string(body))
	if decoded == nil {
		return "", errors.New("zai_oauth_required: business token 响应非 JSON")
	}
	if !zcodeBusinessCodeOK(decoded) {
		reason := strings.TrimSpace(stringValue(decoded, "msg"))
		if reason == "" {
			reason = fmt.Sprintf("code=%v", decoded["code"])
		}
		return "", fmt.Errorf("zai_oauth_required: business token 被拒绝: %s", reason)
	}
	data, _ := decoded["data"].(map[string]any)
	token := strings.TrimSpace(stringValue(data, "access_token", "accessToken"))
	if token == "" {
		return "", errors.New("zai_oauth_required: business token 响应缺少 access_token")
	}
	return token, nil
}

// waitZcodeBillingReady mirrors check_billing_ready: after the business
// exchange the server initializes the billing plan asynchronously, so the
// login waits through progressive delays until /data/plans is non-empty.
// Never fatal: exhausting the delays returns (false, nil); only ctx
// cancellation surfaces an error.
func waitZcodeBillingReady(ctx context.Context, jwt string) (bool, error) {
	if jwt == "" {
		return false, nil
	}
	for _, delay := range zcodeBillingReadyDelays {
		if err := sleepCtx(ctx, delay); err != nil {
			return false, ctx.Err()
		}
		ready, err := zcodeBillingHasPlans(ctx, jwt)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			continue
		}
		if ready {
			return true, nil
		}
	}
	return false, nil
}

// zcodeBillingHasPlans probes billing/current once: 2xx with a non-empty
// /data/plans array counts as ready.
func zcodeBillingHasPlans(ctx context.Context, jwt string) (bool, error) {
	body, _, err := zcodeGet(ctx, zcodeBillingReadyURL, jwt, true)
	if err != nil {
		return false, err
	}
	decoded := parseJSONObject(string(body))
	data, _ := decoded["data"].(map[string]any)
	if data == nil {
		return false, nil
	}
	plans, _ := data["plans"].([]any)
	return len(plans) > 0, nil
}

// finishZcodeLogin drives one flow to its candidate account: poll → ready →
// (zai only) business exchange → (zai only) billing-ready wait → build the
// encrypted credentials/config snapshot. Nothing is persisted here and the
// ~/.zcode slot is untouched — login.go's state machine owns persistence.
func finishZcodeLogin(ctx context.Context, flow *zcodeOAuthFlow) (*Account, error) {
	data, err := flow.poll(ctx)
	if err != nil {
		return nil, err
	}
	tokens, err := parseZcodeReady(data, flow.provider)
	if err != nil {
		return nil, err
	}
	accessForCredentials := tokens.accessToken
	if flow.provider == "zai" {
		business, err := exchangeZcodeBusinessToken(ctx, tokens.accessToken)
		if err != nil {
			return nil, err
		}
		accessForCredentials = business
	}
	if flow.provider == "zai" {
		// Best effort: a timeout here never blocks the login (the plan keeps
		// initializing in the background and the quota refresh will see it).
		if _, err := waitZcodeBillingReady(ctx, tokens.zcodeJWT); err != nil {
			return nil, err
		}
	}
	return buildZcodeLoginCandidate(flow.provider, tokens, accessForCredentials)
}

// zcodeLoginSlotIDs mirrors provider_slot_ids: the three official provider
// slots the OAuth login enables in config.json.
func zcodeLoginSlotIDs(provider string) []string {
	if provider == "bigmodel" {
		return []string{"builtin:bigmodel-start-plan", "builtin:bigmodel-coding-plan", "builtin:bigmodel"}
	}
	return []string{"builtin:zai-start-plan", "builtin:zai-coding-plan", "builtin:zai"}
}

// zcodeLoginSlotAPIKey mirrors slot_api_key: every zai slot carries the zcode
// JWT; bigmodel start-plan carries the JWT, coding-plan the bigmodel access
// token (JWT fallback), and the plain bigmodel slot no apiKey at all.
func zcodeLoginSlotAPIKey(provider, slot string, tokens *zcodeReadyTokens) string {
	if provider == "bigmodel" {
		if slot == "builtin:bigmodel" {
			return ""
		}
		if slot == "builtin:bigmodel-coding-plan" && tokens.accessToken != "" {
			return tokens.accessToken
		}
	}
	return tokens.zcodeJWT
}

// normalizeZcodeUserInfo mirrors normalize_user_info: fold the provider's user
// object onto the canonical email/name/avatar/user_id keys.
func normalizeZcodeUserInfo(user map[string]any) map[string]any {
	return map[string]any{
		"email":   stringValue(user, "email", "mail"),
		"name":    stringValue(user, "name", "username", "nickName", "displayName"),
		"avatar":  stringValue(user, "avatar", "avatarUrl", "picture"),
		"user_id": stringValue(user, "user_id", "userId", "id", "customerNumber", "sub"),
	}
}

// zcodeOfficialProfile mirrors to_official_profile: the bigmodel branch stores
// the {id, username, displayName, avatarUrl?} profile the official client
// writes, while zai stores the server's raw user object.
func zcodeOfficialProfile(userInfo map[string]any) map[string]any {
	display := firstNonEmpty(
		stringValue(userInfo, "name"),
		stringValue(userInfo, "email"),
		stringValue(userInfo, "user_id"),
	)
	profile := map[string]any{
		"id":          userInfo["user_id"],
		"username":    display,
		"displayName": display,
	}
	if avatar := stringValue(userInfo, "avatar"); avatar != "" {
		profile["avatarUrl"] = avatar
	}
	return profile
}

// buildZcodeLoginCandidate assembles the credentials.json/config.json texts
// exactly like write_oauth_credentials (enc:v1 values via the shared machine
// secret), then fingerprints them through the same zcodeFingerprintFrom the
// capture path uses, so the candidate is shape-compatible with captured
// accounts.
func buildZcodeLoginCandidate(provider string, tokens *zcodeReadyTokens, accessForCredentials string) (*Account, error) {
	secret := zcodeCredentialSecret()
	encrypt := func(value string) (string, error) {
		sealed, err := zcodeEncrypt(value, secret)
		if err != nil {
			return "", fmt.Errorf("加密登录态失败: %w", err)
		}
		return sealed, nil
	}
	userInfo := normalizeZcodeUserInfo(tokens.user)
	storedUser := tokens.user
	if provider == "bigmodel" {
		storedUser = zcodeOfficialProfile(userInfo)
	}
	storedUserRaw, err := json.Marshal(storedUser)
	if err != nil {
		return nil, fmt.Errorf("序列化用户信息失败: %w", err)
	}
	credentials := map[string]any{}
	put := func(key, value string) error {
		sealed, err := encrypt(value)
		if err != nil {
			return err
		}
		credentials[key] = sealed
		return nil
	}
	if err := put("oauth:active_provider", provider); err != nil {
		return nil, err
	}
	if accessForCredentials != "" {
		if err := put("oauth:"+provider+":access_token", accessForCredentials); err != nil {
			return nil, err
		}
	}
	if tokens.refreshToken != "" {
		if err := put("oauth:"+provider+":refresh_token", tokens.refreshToken); err != nil {
			return nil, err
		}
	}
	if tokens.zcodeJWT != "" {
		if err := put("zcodejwttoken", tokens.zcodeJWT); err != nil {
			return nil, err
		}
	}
	if err := put("oauth:"+provider+":user_info", string(storedUserRaw)); err != nil {
		return nil, err
	}

	providers := map[string]any{}
	for _, slot := range zcodeLoginSlotIDs(provider) {
		entry := map[string]any{"enabled": true, "options": map[string]any{}}
		if apiKey := zcodeLoginSlotAPIKey(provider, slot, tokens); apiKey != "" {
			entry["options"].(map[string]any)["apiKey"] = apiKey
		}
		providers[slot] = entry
	}
	config := map[string]any{"provider": providers}

	credentialsText, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return nil, err
	}
	configText, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	// Identity via the very same fingerprint pipeline captureZcodeCurrent uses.
	fp := zcodeFingerprintFrom(credentials, config)
	if fp == nil {
		return nil, errors.New("无法从登录态确定账号标识（user_id 为空），已放弃保存")
	}
	label := firstNonEmpty(fp.Email, fp.Name, "账号-"+fp.ShortID)
	return &Account{
		ID:          fp.ID,
		Label:       label,
		Email:       fp.Email,
		Nickname:    fp.Name,
		UserID:      fp.UserID,
		Avatar:      fp.Avatar,
		Provider:    fp.Provider,
		Credentials: string(credentialsText),
		Config:      string(configText),
	}, nil
}
