package accpool

// WorkBuddy / CodeBuddy account management — the fourth pool kind, ported
// from zcode-switcher's src-tauri/src/workbuddy (mod/api/cli/process/rotate).
//
// Two variants share one CLI: cn (www.codebuddy.cn, desktop app
// WorkBuddy.app) and ai (www.workbuddy.ai, desktop app "WorkBuddy AI.app").
//
// The desktop slot is the desktop app's four-segment JSON
// (~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/
// workbuddy-desktop[|-ai].info = {account, auth, accounts, allAccounts}), so
// a switch stops and relaunches the app. The CLI consumers (the gateway's ACP
// sessions) use the apiKeyHelper mode: ~/.codebuddy/settings.json points at
// ~/.codebuddy-rotate/helper.cjs, which prints the active account's plain
// access token from the pool per ~/.codebuddy-rotate/state.json. Switching
// the CLI = writing state.json + the region env (cn: internal; ai: public +
// CODEBUDDY_BASE_URL) + rewriting the local_storage runtime caches.
//
// Token timestamps are Unix MILLISECONDS (unlike kimi/minimax seconds). A
// token may be a WorkBuddy 5.6 encrypted envelope ({$wbEncrypted, envelope})
// which cannot call the APIs — such accounts go straight to reauth_required
// on refresh and are refused as switch targets for the CLI.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// WorkBuddy per-variant endpoints (api.rs Variant::api_endpoint); package
// variables so tests can point them at a stub server.
var workbuddyEndpoints = map[string]string{
	"cn": "https://www.codebuddy.cn",
	"ai": "https://www.workbuddy.ai",
}

const (
	workbuddyUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
	// workbuddyFreshWindowMs: lazy-refresh the access token when fewer than
	// 24h remain (api.rs FRESH_WINDOW_MS).
	workbuddyFreshWindowMs = int64(24 * time.Hour / time.Millisecond)
	// workbuddyFarFutureDays: expiry further out than this is a placeholder
	// year-2049 style value → treated as long-valid (no reset time).
	workbuddyFarFutureDays = 730
	// workbuddyExpiringSoonDays: packages expiring within 7 days are
	// highlighted (api.rs EXPIRING_SOON_DAYS).
	workbuddyExpiringSoonDays = 7
	// workbuddyLiveSessionStaleMs: CLI session heartbeat freshness window
	// (cli.rs LIVE_SESSION_STALE_MS, client beats every 30s).
	workbuddyLiveSessionStaleMs = int64(120_000)

	workbuddyCNEnvValue = "internal"
	workbuddyAIEnvValue = "public"
	workbuddyCNEndpoint = "https://copilot.tencent.com"
	workbuddyAIEndpoint = "https://www.workbuddy.ai"
	workbuddyAIBaseURL  = "https://www.codebuddy.ai/v2"
)

// billing 请求体里的固定套餐码表（api.rs；解析不依赖这份清单，多带码无副作用）。
var (
	workbuddyPaidPackageCodes = []string{
		"TCACA_code_002_AkiJS3ZHF5", "TCACA_code_023_4xbGhMrE6q",
		"TCACA_code_026_BaESVICNoi", "TCACA_code_027_0FCGVA6vSa",
		"TCACA_code_009_0XmEQc2xOf", "TCACA_code_038_OhvqZtiPKr",
		"TCACA_code_003_FAnt7lcmRT", "TCACA_code_036_lupO5WgNdG",
	}
	workbuddyFreePackageCodes = []string{
		"TCACA_code_008_cfWoLwvjU4", "TCACA_code_007_nzdH5h4Nl0",
		"TCACA_code_028_NtpWi0jzXs", "TCACA_code_029_6wCGEWquYy",
		"TCACA_code_030_BjSt89qTvr", "TCACA_code_001_PqouKr6QWV",
		"TCACA_code_006_DbXS0lrypC", "TCACA_code_035_ArVxJcGDsm",
		"TCACA_code_037_WxOD3MpI2o", "TCACA_code_039_KRcQj7wUat",
		"TCACA_code_040_mi9rCYg46x",
	}
)

// local_storage runtime caches that must be rewritten on a region switch
// (cli.rs): env/endpoint caches are rewritten, the product cache is dropped.
const (
	workbuddyEnvCacheFile      = "entry_3bab4ce61838088127d444e4cc042d6d.info"
	workbuddyEndpointCacheFile = "entry_933d5543e80177622c17a73869c0fad7.info"
	workbuddyProductCacheFile  = "entry_604f48c944053e01d9546675443286c1.info"
)

// --- variant ----------------------------------------------------------------

func validWorkbuddyVariant(variant string) bool { return variant == "cn" || variant == "ai" }

// workbuddyVariantFromAccount mirrors Variant::from_account: explicit variant
// field wins, .workbuddy.ai domain suffix falls back to ai, else cn.
func workbuddyVariantFromAccount(acc *Account) string {
	switch strings.ToLower(strings.TrimSpace(acc.Variant)) {
	case "ai", "intl", "global":
		return "ai"
	case "cn":
		return "cn"
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(acc.Domain)), ".workbuddy.ai") {
		return "ai"
	}
	return "cn"
}

// workbuddyBillingPaths is Variant::billing_paths: ai tries the /v2-stripped
// shape first and only an HTTP 404 falls back to the /v2 form.
func workbuddyBillingPaths(variant, path string) []string {
	if variant != "ai" {
		return []string{path}
	}
	primary := strings.TrimPrefix(path, "/v2")
	fallback := "/v2" + primary
	if fallback == primary {
		return []string{primary}
	}
	return []string{primary, fallback}
}

// --- paths --------------------------------------------------------------------

// workbuddyHome resolves the override home (WORKBUDDY_HOME, same hook as
// mod.rs HOME_ENV_VAR): the auth file, ~/.codebuddy and ~/.codebuddy-rotate
// all live under it.
func workbuddyHome() string {
	if value := strings.TrimSpace(os.Getenv("WORKBUDDY_HOME")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return ""
}

func workbuddyAuthFilePath(variant string) string {
	sub := ".local/share/CodeBuddyExtension/Data/Public/auth"
	switch runtime.GOOS {
	case "darwin":
		sub = "Library/Application Support/CodeBuddyExtension/Data/Public/auth"
	case "windows":
		sub = "AppData/Local/CodeBuddyExtension/Data/Public/auth"
	}
	name := "workbuddy-desktop.info"
	if variant == "ai" {
		name = "workbuddy-desktop-ai.info"
	}
	return filepath.Join(workbuddyHome(), sub, name)
}

func workbuddyCodebuddyHome() string { return filepath.Join(workbuddyHome(), ".codebuddy") }

func workbuddyRotateDir() string { return filepath.Join(workbuddyHome(), ".codebuddy-rotate") }

func workbuddySettingsPath() string { return filepath.Join(workbuddyCodebuddyHome(), "settings.json") }

func workbuddyCLIStatePath() string { return filepath.Join(workbuddyRotateDir(), "state.json") }

func workbuddyHelperPath() string { return filepath.Join(workbuddyRotateDir(), "helper.cjs") }

func workbuddyCLISessionsDir() string {
	return filepath.Join(workbuddyCodebuddyHome(), "sessions")
}

func (m *Manager) workbuddyCurrentMarkerPath() string {
	return filepath.Join(m.storeDir("workbuddy"), ".workbuddy-current")
}

// --- secrets: plain strings vs WorkBuddy 5.6 encrypted envelopes -------------

// workbuddyEnvelope reports whether a token field holds the encrypted
// {$wbEncrypted, envelope} object instead of a plain string.
// IsEnvelopeAccount reports whether the account's credentials are WorkBuddy
// envelope-state ciphertext (machine-bound, cannot call APIs from the pool).
func IsEnvelopeAccount(acc *Account) bool {
	return acc != nil && workbuddyEnvelope(acc.AccessToken)
}

func workbuddyEnvelope(value string) bool {
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		return false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(value), &obj) != nil {
		return false
	}
	_, ok := obj["$wbEncrypted"]
	return ok
}

// workbuddySecretText reads a secret field from a decoded JSON object,
// rendering an envelope object back to its compact JSON text so it fits the
// pool's string fields while surviving round trips.
func workbuddySecretText(obj map[string]any, key string) string {
	switch value := obj[key].(type) {
	case string:
		return strings.TrimSpace(value)
	case map[string]any:
		if _, ok := value["$wbEncrypted"]; ok {
			raw, err := json.Marshal(value)
			if err == nil {
				return string(raw)
			}
		}
	}
	return ""
}

// workbuddyPlainToken returns the token only when it is a plain string.
func workbuddyPlainToken(value string) string {
	if value == "" || workbuddyEnvelope(value) {
		return ""
	}
	return value
}

// workbuddyHasUnexpiredPlainToken mirrors mod.rs: a missing expiry counts as
// valid (conservative, never discards credentials).
func workbuddyHasUnexpiredPlainToken(acc *Account, now time.Time) bool {
	if workbuddyPlainToken(acc.AccessToken) == "" {
		return false
	}
	if acc.ExpiresAt == 0 {
		return true
	}
	return acc.ExpiresAt > now.UnixMilli()
}

// workbuddyNormTsMS normalizes a second/millisecond timestamp to
// milliseconds (mod.rs norm_ts).
func workbuddyNormTsMS(value any) int64 {
	n, ok := toFloat(value)
	if !ok {
		return 0
	}
	ms := int64(n)
	if ms != 0 && abs64(ms) < 10_000_000_000 {
		ms *= 1000
	}
	return ms
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// --- slot reading ---------------------------------------------------------------

func readWorkbuddyAuthFile(variant string) map[string]any {
	raw, err := os.ReadFile(workbuddyAuthFilePath(variant))
	if err != nil {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	return root
}

// --- four-segment slot writing (mod.rs write_account_to_auth_file) --------------

// buildWorkbuddyAccountObj mirrors build_account_obj: profileRaw as the base
// with uid/nickname forced and the official defaults filled in.
func buildWorkbuddyAccountObj(acc *Account) map[string]any {
	obj := map[string]any{}
	if raw := parseJSONObject(acc.ProfileRaw); raw != nil {
		for key, value := range raw {
			obj[key] = value
		}
	}
	obj["uid"] = acc.UserID
	obj["nickname"] = acc.Nickname
	setDefault := func(key string, value any) {
		if _, present := obj[key]; !present {
			obj[key] = value
		}
	}
	setDefault("type", "personal")
	setDefault("accountType", "")
	setDefault("idp", "")
	setDefault("oneidAccountId", "")
	setDefault("areaInfoComplete", false)
	setDefault("isCurrentOneIdEnterprise", false)
	setDefault("isCurrentOneIdPersonal", false)
	setDefault("isFirstLogin", false)
	setDefault("isCreator", false)
	setDefault("isAdmin", false)
	setDefault("uin", "")
	setDefault("phoneNumber", "")
	setDefault("lastLogin", true)
	setDefault("pluginEnabled", true)
	setDefault("deployStatus", map[string]any{"statusCode": 0, "statusMsg": "", "detailMsg": ""})
	setDefault("sso", map[string]any{"domain": "", "domainModifiedTimes": 0})
	return obj
}

// workbuddySecretWire converts a pool string field back to its wire shape:
// envelope JSON text becomes an object again (writing "" would destroy the
// login state — mod.rs build_auth_obj note).
func workbuddySecretWire(value string) any {
	if workbuddyEnvelope(value) {
		var obj map[string]any
		if json.Unmarshal([]byte(value), &obj) == nil {
			return obj
		}
	}
	return value
}

// buildWorkbuddyAuthObj mirrors build_auth_obj: authRaw as the base with the
// token/expiry fields recomputed (expiresAt is MILLISECONDS here).
func buildWorkbuddyAuthObj(acc *Account, now time.Time) map[string]any {
	obj := map[string]any{}
	if raw := parseJSONObject(acc.AuthRaw); raw != nil {
		inner := raw
		if nested := objectValue(raw, "auth"); nested != nil {
			inner = nested
		}
		for key, value := range inner {
			obj[key] = value
		}
	}
	obj["accessToken"] = workbuddySecretWire(acc.AccessToken)
	obj["refreshToken"] = workbuddySecretWire(acc.RefreshToken)
	obj["tokenType"] = firstNonEmpty(acc.TokenType, "Bearer")
	obj["domain"] = acc.Domain
	obj["lastRefreshTime"] = now.UnixMilli()
	if _, present := obj["scope"]; !present {
		obj["scope"] = "openid profile offline_access email"
	}
	nowMS := now.UnixMilli()
	if acc.ExpiresAt > 0 {
		expiresIn := (acc.ExpiresAt - nowMS) / 1000
		if expiresIn < 0 {
			expiresIn = 0
		}
		obj["expiresAt"] = acc.ExpiresAt
		obj["expiresIn"] = expiresIn
		refreshExp := acc.RefreshExpiresAt
		if refreshExp == 0 {
			refreshExp = acc.ExpiresAt
		}
		if _, present := obj["refreshExpiresAt"]; !present {
			obj["refreshExpiresAt"] = refreshExp
		}
		refreshIn := (refreshExp - nowMS) / 1000
		if refreshIn < 0 {
			refreshIn = 0
		}
		obj["refreshExpiresIn"] = refreshIn
	} else {
		if _, present := obj["expiresIn"]; !present {
			obj["expiresIn"] = 0
		}
		if _, present := obj["refreshExpiresIn"]; !present {
			obj["refreshExpiresIn"] = 0
		}
	}
	if _, present := obj["notBeforePolicy"]; !present {
		obj["notBeforePolicy"] = 0
	}
	if _, present := obj["sessionState"]; !present {
		obj["sessionState"] = ""
	}
	return obj
}

// writeWorkbuddyAuthFile replaces the desktop slot atomically, verifies the
// write by value, and clears the .logged-out marker (WorkBuddy treats the
// marker as authoritative even when credentials exist).
func writeWorkbuddyAuthFile(acc *Account, variant string, now time.Time) error {
	path := workbuddyAuthFilePath(variant)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	existing := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &existing)
	}
	allAccounts := []any{}
	if value, ok := existing["allAccounts"].([]any); ok {
		allAccounts = append(allAccounts, value...)
	} else if value, ok := existing["accounts"].([]any); ok {
		allAccounts = append(allAccounts, value...)
	}
	accountObj := buildWorkbuddyAccountObj(acc)
	authObj := buildWorkbuddyAuthObj(acc, now)
	targetUID := acc.UserID
	if targetUID != "" {
		filtered := allAccounts[:0]
		for _, item := range allAccounts {
			entry, _ := item.(map[string]any)
			if stringValue(entry, "uid") == targetUID {
				continue
			}
			filtered = append(filtered, item)
		}
		allAccounts = filtered
	}
	allAccounts = append(allAccounts, accountObj)
	session := map[string]any{
		"account":     accountObj,
		"auth":        authObj,
		"accounts":    allAccounts,
		"allAccounts": allAccounts,
	}
	raw, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(path, raw, 0o600); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "denied") {
			return errors.New("无权限写入认证文件：请打开 系统设置→隐私与安全性→App 管理，允许本 App 控制 WorkBuddy 的数据（或为其开启『完全磁盘访问』后重试）")
		}
		return err
	}
	// Write-then-verify by value (the token may be an envelope object).
	written := readWorkbuddyAuthFile(variant)
	if written == nil {
		return errors.New("认证文件写后校验失败：无法重新读取")
	}
	got, _ := json.Marshal(objectValue(written, "auth")["accessToken"])
	want, _ := json.Marshal(authObj["accessToken"])
	if string(got) != string(want) {
		return errors.New("认证文件写后校验失败，未写入目标账号")
	}
	marker := path + ".logged-out"
	if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("清理 WorkBuddy 退出标记失败（%s）：%w", marker, err)
	}
	return nil
}

// backupWorkbuddyAuthFile copies the current slot into the pool's backups
// directory before a switch (mod.rs backup_auth_file).
func (m *Manager) backupWorkbuddyAuthFile(variant string, now time.Time) string {
	source := workbuddyAuthFilePath(variant)
	if _, err := os.Stat(source); err != nil {
		return ""
	}
	dir := filepath.Join(m.storeDir("workbuddy"), "backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	stem := "workbuddy-desktop"
	if variant == "ai" {
		stem = "workbuddy-desktop-ai"
	}
	dest := filepath.Join(dir, stem+"."+now.UTC().Format("2006-01-02T15-04-05")+".info")
	raw, err := os.ReadFile(source)
	if err != nil {
		return ""
	}
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		return ""
	}
	return dest
}

// --- desktop process control (process.rs) ----------------------------------------
//
// All three hooks are package variables so tests can replace them (the zcode
// desktop hooks follow the same pattern). Only the graceful osascript quit is
// ever attempted first; kill -9 is reserved for bundle leftovers afterwards.

var workbuddyAppNames = map[string]string{"cn": "WorkBuddy.app", "ai": "WorkBuddy AI.app"}

func workbuddyBundleID(variant string) string {
	if variant == "ai" {
		return "com.workbuddy.workbuddy-ai"
	}
	return "com.tencent.workbuddy.mac"
}

var workbuddyAppRunning = func(variant string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("ps", "-axo", "args=").Output()
	if err != nil {
		return false
	}
	pattern := "/Applications/" + workbuddyAppNames[variant] + "/Contents/MacOS"
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, pattern) {
			return true
		}
	}
	return false
}

// workbuddyPids matches bundle processes for cleanup (main binary + helpers).
var workbuddyPids = func(variant string) []int {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	name := workbuddyAppNames[variant]
	patterns := []string{"/Applications/" + name, "/Applications/" + strings.ToLower(name)}
	seen := map[int]bool{}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || seen[pid] {
			continue
		}
		args := strings.Join(fields[1:], " ")
		if strings.Contains(args, "zcode-switcher") {
			continue
		}
		for _, pattern := range patterns {
			if strings.Contains(args, pattern) {
				seen[pid] = true
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids
}

var workbuddyKillPids = func(pids []int) {
	if len(pids) == 0 {
		return
	}
	args := []string{"-9"}
	for _, pid := range pids {
		args = append(args, strconv.Itoa(pid))
	}
	_ = exec.Command("kill", args...).Run()
}

var workbuddyWaitGone = func(variant string, timeout time.Duration, bundle bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		alive := workbuddyAppRunning(variant)
		if bundle {
			alive = len(workbuddyPids(variant)) > 0
		}
		if !alive || time.Now().After(deadline) {
			return !alive
		}
		time.Sleep(500 * time.Millisecond)
	}
}

var workbuddyCloseApp = func(variant string, timeout time.Duration) error {
	if runtime.GOOS != "darwin" {
		return errors.New("当前平台暂不支持自动关闭 WorkBuddy，请手动退出后重试")
	}
	started := time.Now()
	remaining := func() time.Duration {
		left := timeout - time.Since(started)
		if left < 100*time.Millisecond {
			left = 100 * time.Millisecond
		}
		return left
	}
	// 1) graceful quit via AppleEvents (osascript errors are fine when the app
	// is not running).
	quit := fmt.Sprintf(`quit app id "%s"`, workbuddyBundleID(variant))
	_ = exec.Command("osascript", "-e", quit).Run()
	// 2) graceful window for the main process to disappear.
	graceful := 8 * time.Second
	if graceful > remaining() {
		graceful = remaining()
	}
	workbuddyWaitGone(variant, graceful, false)
	// 3) kill leftover bundle processes (daemon children holding the
	// single-instance lock).
	if pids := workbuddyPids(variant); len(pids) > 0 {
		workbuddyKillPids(pids)
	}
	// 4) wait until the bundle set is empty.
	if workbuddyWaitGone(variant, remaining(), true) {
		return nil
	}
	left := workbuddyPids(variant)
	var parts []string
	for _, pid := range left {
		parts = append(parts, strconv.Itoa(pid))
	}
	return fmt.Errorf("WorkBuddy 进程无法完全关闭（残留进程: %s）。请手动执行: kill -9 %s",
		strings.Join(parts, ", "), strings.Join(parts, " "))
}

var workbuddyLaunchApp = func(variant string) error {
	if runtime.GOOS != "darwin" {
		return errors.New("当前平台暂不支持自动启动 WorkBuddy，请手动打开")
	}
	name := workbuddyAppNames[variant]
	app := ""
	for _, base := range []string{"/Applications", filepath.Join(workbuddyHome(), "Applications")} {
		candidate := filepath.Join(base, name)
		if info, err := os.Stat(filepath.Join(candidate, "Contents", "Info.plist")); err == nil && !info.IsDir() {
			app = candidate
			break
		}
	}
	if app == "" {
		app = filepath.Join("/Applications", name)
	}
	if pids := workbuddyPids(variant); len(pids) > 0 {
		workbuddyKillPids(pids)
		_ = workbuddyWaitGone(variant, 5*time.Second, true)
	}
	if err := exec.Command("open", "-n", "-a", app).Run(); err != nil {
		return fmt.Errorf("启动 WorkBuddy 失败: %v（路径: %s）", err, app)
	}
	// Sustain check: appearing then vanishing means the single-instance lock
	// bounced the second instance.
	deadline := time.Now().Add(30 * time.Second)
	var seenAt time.Time
	for time.Now().Before(deadline) {
		if workbuddyAppRunning(variant) {
			if seenAt.IsZero() {
				seenAt = time.Now()
			} else if time.Since(seenAt) >= 5*time.Second {
				return nil
			}
		} else if !seenAt.IsZero() {
			return errors.New("WorkBuddy 启动后立即退出，疑似残留单例锁。请手动打开一次 WorkBuddy 后再试。")
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("WorkBuddy 启动超时，未能确认运行（路径: %s）。请手动打开排查。", app)
}

// --- API plumbing (api.rs) -------------------------------------------------------

func workbuddyEndpoint(variant string) string {
	if endpoint := workbuddyEndpoints[variant]; endpoint != "" {
		return endpoint
	}
	return workbuddyEndpoints["cn"]
}

func workbuddyAuthHeaders(acc *Account, accessToken string) map[string]string {
	headers := map[string]string{
		"Authorization": "Bearer " + accessToken,
		"Accept":        "application/json",
	}
	if acc.UserID != "" {
		headers["X-User-Id"] = acc.UserID
	}
	if acc.EnterpriseID != "" {
		headers["X-Enterprise-Id"] = acc.EnterpriseID
		headers["X-Tenant-Id"] = acc.EnterpriseID
	}
	if acc.Domain != "" {
		headers["X-Domain"] = acc.Domain
	}
	return headers
}

// workbuddyResourceHeaders mimics the web user-center Axios interceptor
// shape for billing endpoints.
func workbuddyResourceHeaders(acc *Account, variant, accessToken string) map[string]string {
	headers := workbuddyAuthHeaders(acc, accessToken)
	origin := workbuddyEndpoint(variant)
	headers["X-Client-Platform"] = "web"
	headers["Accept"] = "application/json, text/plain, */*"
	headers["Origin"] = origin
	headers["Referer"] = origin + "/profile/plans-usage"
	return headers
}

// workbuddyPostJSON mirrors api.rs http_json: transport errors and HTTP
// statuses both come back as envelopes ({code: -1|status, message}), success
// as the parsed body.
func workbuddyPostJSON(ctx context.Context, url, method string, headers map[string]string, body any) map[string]any {
	request, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(jsonRaw(body)))
	if err != nil {
		return map[string]any{"code": -1.0, "message": err.Error()}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", workbuddyUserAgent)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return map[string]any{"code": -1.0, "message": "请求失败（网络）：" + err.Error()}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return map[string]any{"code": -1.0, "message": "读取响应失败：" + err.Error()}
	}
	var payload map[string]any
	if json.Unmarshal(raw, &payload) != nil {
		payload = nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return map[string]any{"code": float64(response.StatusCode), "message": workbuddyErrorMessage(string(raw))}
	}
	if payload == nil {
		return map[string]any{"code": -1.0, "message": "响应不是有效 JSON"}
	}
	return payload
}

// workbuddyErrorMessage normalizes non-JSON error pages (gateway 401/5xx
// serves HTML; extract <title>) like normalize_error_body.
func workbuddyErrorMessage(text string) string {
	if strings.HasPrefix(strings.TrimSpace(text), "<") {
		lower := strings.ToLower(text)
		start := strings.Index(lower, "<title>")
		end := strings.Index(lower, "</title>")
		if start >= 0 && end > start {
			title := strings.TrimSpace(text[start+len("<title>") : end])
			if title != "" {
				return "服务端返回 HTML 错误页：" + title
			}
		}
		return "服务端返回 HTML 错误页（无标题）"
	}
	runes := []rune(text)
	if len(runes) > 500 {
		runes = runes[:500]
	}
	return string(runes)
}

func workbuddyResponseCode(response map[string]any) (int64, bool) {
	if value := firstNumberValue(response["code"]); value != nil {
		return *value, true
	}
	if data := objectValue(response, "data"); data != nil {
		if value := firstNumberValue(data["code"]); value != nil {
			return *value, true
		}
	}
	return 0, false
}

func firstNumberValue(value any) *int64 {
	switch v := value.(type) {
	case float64:
		n := int64(v)
		return &n
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			n := int64(f)
			return &n
		}
	}
	return nil
}

func workbuddySuccess(response map[string]any) bool {
	if response == nil {
		return false
	}
	if code, ok := workbuddyResponseCode(response); ok {
		return code == 0 || code == 200
	}
	if _, hasData := response["data"]; !hasData {
		return false
	}
	if okValue, present := response["ok"].(bool); present && !okValue {
		return false
	}
	if successValue, present := response["success"].(bool); present && !successValue {
		return false
	}
	return true
}

// workbuddyUnauthorized mirrors is_unauthorized: the WAF fingerprint block
// (10085) is NOT a token expiry.
func workbuddyUnauthorized(response map[string]any) bool {
	code, ok := workbuddyResponseCode(response)
	if ok && code == 10085 {
		return false
	}
	if ok && (code == 401 || code == 403) {
		return true
	}
	var message string
	for _, candidate := range []any{response["message"], response["msg"]} {
		if text, isString := candidate.(string); isString {
			message = text
			break
		}
	}
	if message == "" {
		if data := objectValue(response, "data"); data != nil {
			if text, isString := data["message"].(string); isString {
				message = text
			} else if text, isString := data["msg"].(string); isString {
				message = text
			}
		}
	}
	lower := strings.ToLower(message)
	for _, keyword := range []string{"unauthorized", "401", "登录", "失效", "过期", "token"} {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

// --- token refresh (api.rs refresh_account_token) ---------------------------------

// needsWorkbuddyRefresh is the lazy 24h window (api.rs ensure_fresh_token):
// missing or past expiry, or fewer than 24h left.
func needsWorkbuddyRefresh(expiresAtMS int64, now time.Time) bool {
	if expiresAtMS == 0 {
		return true
	}
	remain := expiresAtMS - now.UnixMilli()
	return remain <= 0 || remain < workbuddyFreshWindowMs
}

// refreshWorkbuddyToken exchanges the refresh token (api.rs
// refresh_account_token): POST /v2/plugin/auth/token/refresh with an empty
// JSON body and the X-Refresh-Token / X-Auth-Refresh-Source plugin headers.
// On failure the account is marked reauth_required (the refresh token is dead
// weight either way).
func refreshWorkbuddyToken(ctx context.Context, acc *Account, now time.Time) error {
	if acc.RefreshToken == "" || workbuddyEnvelope(acc.RefreshToken) {
		acc.Status = StatusReauthRequired
		acc.StatusReason = "缺少可用的 refresh token，请重新扫码登录"
		return &authFailedError{message: acc.StatusReason}
	}
	variant := workbuddyVariantFromAccount(acc)
	url := workbuddyEndpoint(variant) + "/v2/plugin/auth/token/refresh"
	headers := workbuddyAuthHeaders(acc, workbuddyPlainToken(acc.AccessToken))
	headers["X-Refresh-Token"] = workbuddyPlainToken(acc.RefreshToken)
	headers["X-Auth-Refresh-Source"] = "plugin"
	response := workbuddyPostJSON(ctx, url, http.MethodPost, headers, map[string]any{})
	if !workbuddySuccess(response) {
		code, _ := workbuddyResponseCode(response)
		message, _ := response["message"].(string)
		if message == "" {
			message, _ = response["msg"].(string)
		}
		if message == "" {
			message = "未知错误"
		}
		reason := fmt.Sprintf("刷新失败（code=%d）：%s", code, message)
		acc.Status = StatusReauthRequired
		acc.StatusReason = reason
		if workbuddyUnauthorized(response) {
			return &authFailedError{message: reason}
		}
		return errors.New(reason)
	}
	data := objectValue(response, "data")
	if data == nil {
		acc.Status = StatusReauthRequired
		acc.StatusReason = "刷新响应缺少 data"
		return errors.New("刷新响应缺少 data")
	}
	access := workbuddyPlainToken(workbuddySecretText(data, "accessToken"))
	if access == "" {
		acc.Status = StatusReauthRequired
		acc.StatusReason = "刷新响应缺少 accessToken"
		return errors.New("刷新响应缺少 accessToken")
	}
	acc.AccessToken = access
	if refresh := workbuddySecretText(data, "refreshToken"); refresh != "" {
		acc.RefreshToken = refresh
	}
	if value := workbuddyNormTsMS(data["expiresAt"]); value > 0 {
		acc.ExpiresAt = value
	} else if seconds := int64Or(data["expiresIn"], 0); seconds > 0 {
		acc.ExpiresAt = now.UnixMilli() + seconds*1000
	}
	if value := workbuddyNormTsMS(data["refreshExpiresAt"]); value > 0 {
		acc.RefreshExpiresAt = value
	} else if seconds := int64Or(data["refreshExpiresIn"], 0); seconds > 0 {
		acc.RefreshExpiresAt = now.UnixMilli() + seconds*1000
	}
	acc.TokenType = firstNonEmpty(acc.TokenType, "Bearer")
	acc.Status = StatusActive
	acc.StatusReason = ""
	return nil
}

// --- credits / quota (api.rs get_credit_expiry) -------------------------------------

// CreditsResource is one normalized package/account row.
type CreditsResource struct {
	PackageCode  string `json:"packageCode,omitempty"`
	PackageName  string `json:"packageName,omitempty"`
	Total        int64  `json:"total"`
	Remaining    int64  `json:"remaining"`
	Used         int64  `json:"used"`
	ExpireAt     int64  `json:"expireAt,omitempty"` // ms; 0 = long-valid
	Expired      bool   `json:"expired,omitempty"`
	ExpiringSoon bool   `json:"expiringSoon,omitempty"`
}

// CreditsQuota is the aggregated credit summary kept on Quota alongside the
// synthetic single window (field names match zcode-switcher snapshots).
type CreditsQuota struct {
	TotalCapacity         float64            `json:"totalCapacity"`
	TotalRemaining        float64            `json:"totalRemaining"`
	ExpiringSoonRemaining float64            `json:"expiringSoonRemaining"`
	SoonestExpireAt       int64              `json:"soonestExpireAt,omitempty"`
	ExpiringSoon          bool               `json:"expiringSoon,omitempty"`
	Expired               bool               `json:"expired,omitempty"`
	Resources             []*CreditsResource `json:"resources"`
}

var workbuddyCapacityTotalKeys = []string{
	"CycleCapacitySizePrecise", "CycleCapacitySize", "CycleTotalCapacity",
	"CapacitySizePrecise", "CapacitySize", "SlicePeriodCapacitySizePrecise", "SlicePeriodCapacitySize",
}
var workbuddyCapacityRemainKeys = []string{
	"CycleCapacityRemainPrecise", "CycleCapacityRemain", "CycleRemainCapacity",
	"CapacityRemainPrecise", "CapacityRemain", "SlicePeriodCapacityRemainPrecise", "SlicePeriodCapacityRemain",
}
var workbuddyCapacityUsedKeys = []string{
	"CycleCapacityUsedPrecise", "CycleCapacityUsed", "CycleUsedCapacity",
	"CapacityUsedPrecise", "CapacityUsed", "SlicePeriodCapacityUsedPrecise", "SlicePeriodCapacityUsed",
}

func workbuddyFirstNumber(obj map[string]any, keys []string) (float64, bool) {
	for _, key := range keys {
		if value, ok := toFloat(obj[key]); ok {
			return value, true
		}
	}
	return 0, false
}

// workbuddyResolveExpireAt mirrors api.rs: DeductionEndTime/ExpiredTime win
// unless they look like a >365d placeholder versus CycleEndTime; anything
// more than 730 days out is long-valid (no expiry).
func workbuddyResolveExpireAt(raw map[string]any, nowMS int64) int64 {
	var deductionEnd, cycleEnd int64
	for _, key := range []string{"DeductionEndTime", "deductionEndTime", "ExpiredTime", "expiredTime"} {
		if value := workbuddyNormTsMS(raw[key]); value > 0 {
			deductionEnd = value
			break
		}
	}
	for _, key := range []string{"CycleEndTime", "cycleEndTime"} {
		if value := workbuddyNormTsMS(raw[key]); value > 0 {
			cycleEnd = value
			break
		}
	}
	var expireAt int64
	switch {
	case deductionEnd > 0 && cycleEnd > 0 && deductionEnd-cycleEnd > 365*24*3600*1000:
		expireAt = cycleEnd
	case deductionEnd > 0:
		expireAt = deductionEnd
	default:
		expireAt = cycleEnd
	}
	if expireAt == 0 {
		return 0
	}
	if expireAt-nowMS > workbuddyFarFutureDays*24*3600*1000 {
		return 0
	}
	return expireAt
}

func workbuddyResourceSummary(raw map[string]any, nowMS int64) *CreditsResource {
	var slice map[string]any
	if details, ok := raw["SlicePeriodUsageDetails"].([]any); ok && len(details) > 0 {
		slice, _ = details[0].(map[string]any)
	}
	total, hasTotal := workbuddyFirstNumber(raw, workbuddyCapacityTotalKeys)
	remaining, hasRemaining := workbuddyFirstNumber(raw, workbuddyCapacityRemainKeys)
	used, hasUsed := workbuddyFirstNumber(raw, workbuddyCapacityUsedKeys)
	if !hasTotal && slice != nil {
		total, hasTotal = workbuddyFirstNumber(slice, workbuddyCapacityTotalKeys)
	}
	if !hasRemaining && slice != nil {
		remaining, hasRemaining = workbuddyFirstNumber(slice, workbuddyCapacityRemainKeys)
	}
	if !hasUsed && slice != nil {
		used, hasUsed = workbuddyFirstNumber(slice, workbuddyCapacityUsedKeys)
	}
	if !hasTotal {
		switch {
		case hasRemaining && hasUsed:
			total = remaining + used
			hasTotal = true
		case hasRemaining:
			total = remaining
			hasTotal = true
		case hasUsed:
			total = used
			hasTotal = true
		default:
			total = 0
		}
	}
	if total < 0 {
		total = 0
	}
	if !hasRemaining {
		remaining = total - used
		if remaining < 0 {
			remaining = 0
		}
	}
	if remaining < 0 {
		remaining = 0
	}
	if !hasUsed {
		used = total - remaining
		if used < 0 {
			used = 0
		}
	}
	if used < 0 {
		used = 0
	}
	expireAt := workbuddyResolveExpireAt(raw, nowMS)
	expired := expireAt > 0 && expireAt <= nowMS
	expiringSoon := expireAt > nowMS && expireAt-nowMS <= workbuddyExpiringSoonDays*24*3600*1000
	resource := &CreditsResource{
		PackageCode:  stringValue(raw, "PackageCode", "packageCode"),
		PackageName:  stringValue(raw, "PackageName", "packageName"),
		Total:        int64(total + 0.5),
		Remaining:    int64(remaining + 0.5),
		Used:         int64(used + 0.5),
		ExpireAt:     expireAt,
		Expired:      expired,
		ExpiringSoon: expiringSoon,
	}
	return resource
}

// workbuddyCollectArrays pulls Accounts/Packages rows out of the many
// observed envelope shapes (api.rs collect_array).
func workbuddyCollectArrays(response map[string]any) []map[string]any {
	var out []map[string]any
	try := func(keys ...string) bool {
		current := response
		for _, key := range keys {
			next, ok := current[key].(map[string]any)
			if !ok {
				return false
			}
			current = next
		}
		for _, name := range []string{"Accounts", "accounts", "Packages", "packages"} {
			if items, ok := current[name].([]any); ok {
				for _, item := range items {
					if row, ok := item.(map[string]any); ok {
						out = append(out, row)
					}
				}
				return true
			}
		}
		return false
	}
	for _, path := range [][]string{
		{"data"}, {"data", "data"}, {"data", "Response", "Data"}, {"data", "data", "Response", "Data"},
	} {
		try(path...)
	}
	return out
}

func workbuddyPaidPackagesBody() map[string]any {
	return map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"PackageCodes": workbuddyPaidPackageCodes, "NeedRenewInfo": true,
	}
}

func workbuddyFreePackagesBody() map[string]any {
	return map[string]any{
		"PageNumber": 1, "PageSize": 200, "Status": []int{0, 3},
		"PackageCodes": workbuddyFreePackageCodes,
	}
}

func workbuddyLegacyResourceBody() map[string]any {
	return map[string]any{
		"PageNumber": 1, "PageSize": 100, "ProductCode": "p_tcaca", "Status": []int{0, 3},
	}
}

// workbuddyPostBilling posts one billing request with the ai 404 path
// fallback (only HTTP 404 switches candidates, api.rs post_billing).
func workbuddyPostBilling(ctx context.Context, acc *Account, variant, accessToken, path string, body map[string]any) map[string]any {
	var last map[string]any
	paths := workbuddyBillingPaths(variant, path)
	for index, candidate := range paths {
		response := workbuddyPostJSON(ctx, workbuddyEndpoint(variant)+candidate, http.MethodPost,
			workbuddyResourceHeaders(acc, variant, accessToken), body)
		if index+1 == len(paths) || !workbuddyRouteMissing(response) {
			return response
		}
		last = response
	}
	return last
}

func workbuddyRouteMissing(response map[string]any) bool {
	code, ok := workbuddyResponseCode(response)
	return ok && code == 404
}

// workbuddyCredits is the aggregated query result.
type workbuddyCredits struct {
	OK                    bool
	Error                 string
	TotalCapacity         float64
	TotalRemaining        float64
	ExpiringSoonRemaining float64
	SoonestExpireAt       int64
	ExpiringSoon          bool
	Expired               bool
	Resources             []*CreditsResource
}

// queryWorkbuddyCredits mirrors api.rs get_credit_expiry: envelope tokens
// short-circuit; cn queries summary/paid/free in parallel-shape (sequenced
// here — same requests, KISS), ai goes straight to the legacy single path;
// an empty result falls back to get-user-resource; one 401-triggered refresh
// and retry is allowed. The returned account carries any rotated tokens.
func (m *Manager) queryWorkbuddyCredits(ctx context.Context, acc *Account, now time.Time) (*workbuddyCredits, *Account) {
	fail := func(message string) *workbuddyCredits {
		return &workbuddyCredits{Error: message, Resources: []*CreditsResource{}}
	}
	if workbuddyEnvelope(acc.AccessToken) {
		return fail("该账号凭据为 WorkBuddy 加密信封态，无法调用积分接口；如需该功能请删除后用「添加账号」扫码重新获取明文凭据"), acc
	}
	working := *acc
	// Lazy refresh first (24h window).
	if needsWorkbuddyRefresh(working.ExpiresAt, now) && working.RefreshToken != "" {
		if err := refreshWorkbuddyToken(ctx, &working, now); err != nil {
			if isAuthFailed(err) {
				return fail(err.Error()), &working
			}
			return fail(err.Error()), &working
		}
	}
	variant := workbuddyVariantFromAccount(&working)
	nowMS := now.UnixMilli()
	accessToken := workbuddyPlainToken(working.AccessToken)
	var resources []*CreditsResource

	collect := func(response map[string]any) {
		if !workbuddySuccess(response) {
			return
		}
		for _, raw := range workbuddyCollectArrays(response) {
			resources = append(resources, workbuddyResourceSummary(raw, nowMS))
		}
	}

	if variant == "cn" {
		summary := workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-summary", map[string]any{})
		paid := workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-paid-packages", workbuddyPaidPackagesBody())
		free := workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-free-packages", workbuddyFreePackagesBody())
		unauthorized := workbuddyUnauthorized(summary) || workbuddyUnauthorized(paid) || workbuddyUnauthorized(free)
		if unauthorized && working.RefreshToken != "" {
			refreshed := working
			if err := refreshWorkbuddyToken(ctx, &refreshed, now); err != nil {
				// Refresh is the only way back from a 401; when it fails the
				// stored token is dead weight — surface the real reason (and
				// the reauth_required status it set) instead of the generic
				// "no resource data" after retrying with a dead token.
				return fail(err.Error()), &refreshed
			}
			working = refreshed
			accessToken = workbuddyPlainToken(working.AccessToken)
			summary = workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-summary", map[string]any{})
			paid = workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-paid-packages", workbuddyPaidPackagesBody())
			free = workbuddyPostBilling(ctx, &working, variant, accessToken, "/billing/meter/get-user-resource-free-packages", workbuddyFreePackagesBody())
		}
		collect(summary)
		collect(paid)
		collect(free)
	}

	// ai single path, or the cn three-path empty fallback.
	if len(resources) == 0 {
		response := workbuddyPostBilling(ctx, &working, variant, accessToken, "/v2/billing/meter/get-user-resource", workbuddyLegacyResourceBody())
		if workbuddyUnauthorized(response) && working.RefreshToken != "" {
			refreshed := working
			if err := refreshWorkbuddyToken(ctx, &refreshed, now); err != nil {
				return fail(err.Error()), &refreshed
			}
			working = refreshed
			accessToken = workbuddyPlainToken(working.AccessToken)
			response = workbuddyPostBilling(ctx, &working, variant, accessToken, "/v2/billing/meter/get-user-resource", workbuddyLegacyResourceBody())
		}
		collect(response)
	}
	if len(resources) == 0 {
		return fail("积分查询失败：接口未返回任何资源数据"), &working
	}
	summary := &workbuddyCredits{OK: true, Resources: resources}
	for _, resource := range resources {
		summary.TotalCapacity += float64(resource.Total)
		summary.TotalRemaining += float64(resource.Remaining)
		if resource.ExpiringSoon {
			summary.ExpiringSoon = true
			summary.ExpiringSoonRemaining += float64(resource.Remaining)
		}
		if resource.Expired && resource.Remaining > 0 {
			summary.Expired = true
		}
		if resource.Remaining > 0 && resource.ExpireAt > 0 &&
			(summary.SoonestExpireAt == 0 || resource.ExpireAt < summary.SoonestExpireAt) {
			summary.SoonestExpireAt = resource.ExpireAt
		}
	}
	return summary, &working
}

// normalizeWorkbuddyCredits folds the credit summary into the pool Quota
// shape: one synthetic "积分" window (used = capacity - remaining) plus the
// detail block for the UI.
func normalizeWorkbuddyCredits(summary *workbuddyCredits) *Quota {
	used := summary.TotalCapacity - summary.TotalRemaining
	if used < 0 {
		used = 0
	}
	window := &WindowQuota{
		Name:  "积分",
		Used:  int64(used + 0.5),
		Limit: int64(summary.TotalCapacity + 0.5),
	}
	if summary.SoonestExpireAt > 0 {
		window.ResetAt = epochToRFC3339(summary.SoonestExpireAt)
	}
	return &Quota{
		Limits: []*WindowQuota{window},
		Credits: &CreditsQuota{
			TotalCapacity:         summary.TotalCapacity,
			TotalRemaining:        summary.TotalRemaining,
			ExpiringSoonRemaining: summary.ExpiringSoonRemaining,
			SoonestExpireAt:       summary.SoonestExpireAt,
			ExpiringSoon:          summary.ExpiringSoon,
			Expired:               summary.Expired,
			Resources:             summary.Resources,
		},
	}
}

// --- CLI apiKeyHelper mode (cli.rs) --------------------------------------------------

// workbuddyHelperSource is the helper.cjs template with the absolute pool and
// rotate directories burned in (the CLI spawns it without our environment).
const workbuddyHelperSource = `#!/usr/bin/env node
// CodeBuddy CLI apiKeyHelper (WorkBuddy pool). Prints "Bearer <token>" of the
// active account to stdout; the CLI uses it as the Authorization header.
// The active account comes from state.json's activeAccountId; snapshots live
// in the pool directory next to this file's directory.
"use strict";
const fs = require("fs");
const path = require("path");
const storeDir = __STORE_DIR__;
const rotateDir = process.env.CODEBUDDY_ROTATE_DIR || __ROTATE_DIR__;
function fail(msg) { process.stderr.write("workbuddy-pool: " + msg + "\n"); process.exit(1); }
function readJson(file, fallback) {
  try { return JSON.parse(fs.readFileSync(file, "utf8").replace(/^\uFEFF/, "")); }
  catch { return fallback; }
}
const state = readJson(path.join(rotateDir, "state.json"), {});
const activeId = typeof state.activeAccountId === "string" ? state.activeAccountId : "";
if (!activeId) fail("state.json missing activeAccountId; switch account in the gateway first");
const acc = readJson(path.join(storeDir, activeId + ".json"), null);
if (!acc || typeof acc !== "object") fail("account snapshot not found: " + activeId);
const token = typeof acc.accessToken === "string" ? acc.accessToken.trim() : "";
if (!token) fail("selected account has no plain access token (re-login via scan code)");
process.stdout.write("Bearer " + token + "\n");
`

func (m *Manager) renderWorkbuddyHelper() string {
	source := strings.ReplaceAll(workbuddyHelperSource, "__STORE_DIR__", jsonString(m.storeDir("workbuddy")))
	source = strings.ReplaceAll(source, "__ROTATE_DIR__", jsonString(workbuddyRotateDir()))
	return source
}

// workbuddyConfiguredHelper returns settings.json's apiKeyHelper command.
func workbuddyConfiguredHelper() string {
	settings := readMinimaxJSON(workbuddySettingsPath())
	if settings == nil {
		return ""
	}
	return strings.TrimSpace(stringValue(settings, "apiKeyHelper"))
}

// workbuddyRunHelper executes helper.cjs with node and returns its stdout;
// the zsh login-shell probe covers GUI-spawned processes without a PATH.
var workbuddyRunHelper = func() (string, error) {
	helper := workbuddyHelperPath()
	candidates := []string{"node", "/opt/homebrew/bin/node", "/usr/local/bin/node"}
	if out, err := exec.Command("/bin/zsh", "-lic", "command -v node").Output(); err == nil {
		lines := strings.Split(string(out), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(lines[i])
			if strings.HasPrefix(candidate, "/") && !containsString(candidates, candidate) {
				candidates = append(candidates, candidate)
			}
		}
	}
	var lastErr error
	for _, node := range candidates {
		command := exec.Command(node, helper)
		command.Stdin = nil
		out, err := command.Output()
		if err == nil {
			return string(out), nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("未找到可用的 Node.js（如通过 nvm 安装，请确认登录 shell 能执行 node）：%v", lastErr)
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// setupWorkbuddyHelper installs helper.cjs + the settings.json apiKeyHelper
// pointer + the region env (cli.rs setup_helper). Refuses to take over a
// helper owned by another tool. Returns whether this call performed the
// takeover.
func (m *Manager) setupWorkbuddyHelper(now time.Time) (bool, error) {
	configured := workbuddyConfiguredHelper()
	if configured != "" {
		same := configured == workbuddyHelperPath()
		if !same {
			if canonical, err := filepath.EvalSymlinks(configured); err == nil {
				if target, err := filepath.EvalSymlinks(workbuddyHelperPath()); err == nil {
					same = canonical == target
				}
			}
		}
		if !same {
			return false, fmt.Errorf("已有其他 CodeBuddy CLI apiKeyHelper（%s）；请先确认后再替换", configured)
		}
		return false, nil
	}
	if err := os.MkdirAll(workbuddyRotateDir(), 0o700); err != nil {
		return false, errors.New("无法创建 helper 目录，请检查用户目录权限")
	}
	helper := workbuddyHelperPath()
	if err := os.WriteFile(helper, []byte(m.renderWorkbuddyHelper()), 0o700); err != nil {
		return false, errors.New("无法写入 helper.cjs，请检查文件权限")
	}
	// Point settings.json at the helper (restore on failure).
	previousSettings, _ := os.ReadFile(workbuddySettingsPath())
	settingsValue := map[string]any{}
	if len(previousSettings) > 0 {
		if err := json.Unmarshal(previousSettings, &settingsValue); err != nil {
			_ = os.Remove(helper)
			return false, errors.New("CodeBuddy settings.json 不是有效 JSON")
		}
	}
	settingsValue["apiKeyHelper"] = helper
	if err := m.persistWorkbuddySettings(settingsValue); err != nil {
		_ = os.Remove(helper)
		return false, err
	}
	return true, nil
}

func (m *Manager) persistWorkbuddySettings(value map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(workbuddySettingsPath()), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(workbuddySettingsPath(), raw, 0o600); err != nil {
		return errors.New("无法写入 CodeBuddy settings.json，请检查文件权限")
	}
	return nil
}

// applyWorkbuddyRegionEnv writes the variant env into settings.json (cn:
// CODEBUDDY_INTERNET_ENVIRONMENT=internal, no BASE_URL; ai: =public plus
// CODEBUDDY_BASE_URL) and rewrites the local_storage runtime caches; a cache
// failure rolls the settings file back (cli.rs persist_cli_region_env).
func (m *Manager) applyWorkbuddyRegionEnv(variant string) error {
	previous, _ := os.ReadFile(workbuddySettingsPath())
	settingsValue := map[string]any{}
	if len(previous) > 0 {
		if err := json.Unmarshal(previous, &settingsValue); err != nil {
			return errors.New("CodeBuddy settings.json 不是有效 JSON")
		}
	}
	env, ok := settingsValue["env"].(map[string]any)
	if !ok {
		env = map[string]any{}
	}
	if variant == "ai" {
		env["CODEBUDDY_INTERNET_ENVIRONMENT"] = workbuddyAIEnvValue
		env["CODEBUDDY_BASE_URL"] = workbuddyAIBaseURL
	} else {
		env["CODEBUDDY_INTERNET_ENVIRONMENT"] = workbuddyCNEnvValue
		delete(env, "CODEBUDDY_BASE_URL")
	}
	settingsValue["env"] = env
	if err := m.persistWorkbuddySettings(settingsValue); err != nil {
		return err
	}
	if err := m.syncWorkbuddyRuntimeCache(variant); err != nil {
		if len(previous) > 0 {
			_ = atomicWrite(workbuddySettingsPath(), previous, 0o600)
		}
		return err
	}
	return nil
}

// syncWorkbuddyRuntimeCache rewrites the env/endpoint local_storage caches
// and drops the product cache (cli.rs sync_cli_runtime_cache).
func (m *Manager) syncWorkbuddyRuntimeCache(variant string) error {
	dir := filepath.Join(workbuddyCodebuddyHome(), "local_storage")
	_ = os.Remove(filepath.Join(dir, workbuddyProductCacheFile))
	envValue, endpointValue := workbuddyCNEnvValue, workbuddyCNEndpoint
	if variant == "ai" {
		envValue, endpointValue = workbuddyAIEnvValue, workbuddyAIEndpoint
	}
	writeCache := func(name, value string) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		return atomicWrite(filepath.Join(dir, name), raw, 0o600)
	}
	if err := writeCache(workbuddyEnvCacheFile, envValue); err != nil {
		return err
	}
	return writeCache(workbuddyEndpointCacheFile, endpointValue)
}

// writeWorkbuddyCLIState points the CLI at the target account
// (cli.rs switch_active_account's state write, without the process kill —
// the gateway re-attaches its own ACP sessions instead).
func (m *Manager) writeWorkbuddyCLIState(target *Account, now time.Time) error {
	if err := os.MkdirAll(workbuddyRotateDir(), 0o700); err != nil {
		return errors.New("无法创建 CLI 账号状态目录，请检查用户目录权限")
	}
	if workbuddyPlainToken(target.AccessToken) == "" {
		return errors.New("所选账号没有明文 access token（加密信封态无法供 CLI 使用），请扫码重新添加")
	}
	index := 0
	for i, account := range m.list("workbuddy") {
		if account.ID == target.ID {
			index = i
			break
		}
	}
	state := map[string]any{
		"active":          index,
		"activeAccountId": target.ID,
		"updatedAt":       now.UnixMilli(),
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(workbuddyCLIStatePath(), raw, 0o600); err != nil {
		return fmt.Errorf("无法写入所选 CLI 账号状态: %w", err)
	}
	return nil
}

// --- live session gate (cli.rs has_live_session) ---------------------------------------

func workbuddyHeartbeatLive(value map[string]any, nowMS int64) bool {
	var beat int64
	for _, key := range []string{"lastHeartbeat", "heartbeat", "startedAt"} {
		if n := int64Or(value[key], 0); n != 0 {
			beat = n
			break
		}
	}
	if beat == 0 {
		return false
	}
	if beat != 0 && abs64(beat) < 10_000_000_000 {
		beat *= 1000
	}
	return nowMS-beat <= workbuddyLiveSessionStaleMs
}

// workbuddyHasLiveSession reports whether any ~/.codebuddy/sessions/*.json
// heartbeat is fresh (120s window).
func workbuddyHasLiveSession(now time.Time) bool {
	entries, err := os.ReadDir(workbuddyCLISessionsDir())
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if value := readMinimaxJSON(filepath.Join(workbuddyCLISessionsDir(), entry.Name())); value != nil {
			if workbuddyHeartbeatLive(value, now.UnixMilli()) {
				return true
			}
		}
	}
	return false
}

// --- gateway scan login (api.rs start_scan_login / poll_scan_login /
// wait_scan_complete / fetch_profile) ------------------------------------------------

func init() { registerLoginDriver("workbuddy", startWorkbuddyLogin) }

// workbuddyScanPollInterval is the scan-status poll cadence (api.rs
// wait_scan_complete sleeps 2s between polls); a package variable so tests
// can shrink it.
var workbuddyScanPollInterval = 2 * time.Second

// workbuddyScanMaxWait bounds one login attempt at 10 minutes (the QR itself
// lives 600s; the slack covers a slow final poll); a package variable so
// tests can shrink it.
var workbuddyScanMaxWait = 10 * time.Minute

// workbuddyScanLogin mirrors api.rs ScanLogin: the state handle of one scan
// login attempt.
type workbuddyScanLogin struct {
	variant   string
	state     string
	authURL   string
	expiresAt time.Time // QR lifetime, on the Manager clock
}

// startWorkbuddyLogin is the workbuddy loginStart (registered in init): POST
// /v2/plugin/auth/state mints the scan state; the challenge carries the scan
// URL; the wait closure polls until the user confirms and returns the
// candidate only — the shared state machine in login.go persists it.
func startWorkbuddyLogin(ctx context.Context, m *Manager, opts LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error) {
	variant := strings.ToLower(strings.TrimSpace(opts.Provider))
	if variant == "" {
		variant = "cn"
	}
	if !validWorkbuddyVariant(variant) {
		return nil, nil, fmt.Errorf("不支持的 WorkBuddy 档位: %s", opts.Provider)
	}
	platform := "workbuddy"
	if variant == "ai" {
		platform = "workbuddy-ai"
	}
	endpoint := workbuddyEndpoint(variant)
	response := workbuddyPostJSON(ctx, endpoint+"/v2/plugin/auth/state?platform="+platform, http.MethodPost, nil, map[string]any{})
	data := objectValue(response, "data")
	if data == nil {
		code, _ := workbuddyResponseCode(response)
		message, _ := response["message"].(string)
		if message == "" {
			message, _ = response["msg"].(string)
		}
		if message == "" {
			message = "未知错误"
		}
		return nil, nil, fmt.Errorf("发起扫码登录失败（code=%d）：%s", code, message)
	}
	state := strings.TrimSpace(stringValue(data, "state"))
	if state == "" {
		return nil, nil, errors.New("发起扫码登录失败：auth/state 响应缺少 state")
	}
	authURL := firstNonEmpty(stringValue(data, "authUrl", "auth_url", "url"),
		endpoint+"/login?state="+state)
	login := &workbuddyScanLogin{
		variant:   variant,
		state:     state,
		authURL:   authURL,
		expiresAt: m.now().Add(600 * time.Second),
	}
	return &LoginChallenge{URL: authURL, ExpiresIn: 600},
		func(ctx context.Context) (*Account, error) { return m.workbuddyWaitScan(ctx, login) },
		nil
}

// workbuddyWaitScan mirrors wait_scan_complete: poll every 2s until the token
// arrives, the QR/attempt budget runs out, or ctx is done.
func (m *Manager) workbuddyWaitScan(ctx context.Context, login *workbuddyScanLogin) (*Account, error) {
	deadline := m.now().Add(workbuddyScanMaxWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !m.now().Before(login.expiresAt) {
			return nil, errors.New("登录超时（二维码已过期），请重试")
		}
		if !m.now().Before(deadline) {
			return nil, errors.New("扫码登录超时，请重试")
		}
		data, err := m.workbuddyPollScan(ctx, login)
		if err != nil {
			return nil, err
		}
		if data != nil {
			return m.workbuddyCompleteScan(ctx, login, data)
		}
		timer := time.NewTimer(workbuddyScanPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// workbuddyPollScan mirrors poll_scan_login one round: (nil, nil) = not
// scanned yet (any code other than 0/200 — including transport failures —
// just means keep waiting); (data, nil) = scanned; (nil, err) = terminal
// error (domain/tier mismatch).
func (m *Manager) workbuddyPollScan(ctx context.Context, login *workbuddyScanLogin) (map[string]any, error) {
	endpoint := workbuddyEndpoint(login.variant)
	response := workbuddyPostJSON(ctx, fmt.Sprintf("%s/v2/plugin/auth/token?state=%s", endpoint, login.state), http.MethodGet, nil, nil)
	code, _ := workbuddyResponseCode(response)
	if code != 0 && code != 200 {
		return nil, nil
	}
	data := objectValue(response, "data")
	if data == nil || workbuddyPlainToken(workbuddySecretText(data, "accessToken")) == "" {
		return nil, nil
	}
	// 档位一致性校验：拿 cn 的 state 换 ai 的 token（或反之）会让网关一直
	// 拒绝，直接报错不入库（api.rs poll_scan_login）。
	domain := strings.TrimSpace(stringValue(data, "domain"))
	if !workbuddyVariantMatchesDomain(login.variant, domain) {
		tier := "国内版"
		if login.variant == "ai" {
			tier = "国际版"
		}
		return nil, fmt.Errorf("登录响应的 domain（%s）与%s档位不符，已拒绝入库", domain, tier)
	}
	return data, nil
}

// workbuddyVariantMatchesDomain mirrors Variant::matches_domain.
func workbuddyVariantMatchesDomain(variant, domain string) bool {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return true
	}
	isAI := strings.HasSuffix(strings.ToLower(domain), ".workbuddy.ai")
	if variant == "ai" {
		return isAI
	}
	return !isAI
}

// workbuddyCompleteScan turns the token payload into the account candidate
// (plain tokens — a scan login never produces the encrypted envelope) and
// best-effort enriches it with the login/account profile (api.rs
// fetch_profile + complete_login).
func (m *Manager) workbuddyCompleteScan(ctx context.Context, login *workbuddyScanLogin, data map[string]any) (*Account, error) {
	nowMS := m.now().UnixMilli()
	expiresAt := workbuddyNormTsMS(data["expiresAt"])
	if expiresAt == 0 {
		if seconds := int64Or(data["expiresIn"], 0); seconds > 0 {
			expiresAt = nowMS + seconds*1000
		}
	}
	refreshExpiresAt := workbuddyNormTsMS(data["refreshExpiresAt"])
	if refreshExpiresAt == 0 {
		if seconds := int64Or(data["refreshExpiresIn"], 0); seconds > 0 {
			refreshExpiresAt = nowMS + seconds*1000
		}
	}
	candidate := &Account{
		Variant:          login.variant,
		AccessToken:      workbuddyPlainToken(workbuddySecretText(data, "accessToken")),
		RefreshToken:     workbuddySecretText(data, "refreshToken"),
		TokenType:        firstNonEmpty(stringValue(data, "tokenType"), "Bearer"),
		Domain:           strings.TrimSpace(stringValue(data, "domain")),
		ExpiresAt:        expiresAt,
		RefreshExpiresAt: refreshExpiresAt,
		AuthRaw:          jsonRaw(data),
		Status:           StatusActive,
	}
	if profile := m.workbuddyFetchScanProfile(ctx, login, candidate); profile != nil {
		candidate.UserID = firstNonEmpty(stringValue(profile, "uid"), stringValue(profile, "id"))
		candidate.Nickname = stringValue(profile, "nickname")
		candidate.Email = stringValue(profile, "email")
		candidate.EnterpriseID = stringValue(profile, "enterpriseId")
		candidate.EnterpriseName = stringValue(profile, "enterpriseName")
		candidate.ProfileRaw = jsonRaw(profile)
		if candidate.UserID != "" {
			candidate.ID = "w_" + candidate.UserID
		}
	}
	return candidate, nil
}

// workbuddyFetchScanProfile mirrors fetch_profile: GET /v2/plugin/login/
// account with the fresh Bearer + X-Domain. Best-effort — a missing profile
// must not fail a login that already holds tokens.
func (m *Manager) workbuddyFetchScanProfile(ctx context.Context, login *workbuddyScanLogin, candidate *Account) map[string]any {
	if candidate.AccessToken == "" {
		return nil
	}
	endpoint := workbuddyEndpoint(login.variant)
	response := workbuddyPostJSON(ctx, fmt.Sprintf("%s/v2/plugin/login/account?state=%s", endpoint, login.state),
		http.MethodGet, workbuddyAuthHeaders(candidate, candidate.AccessToken), nil)
	if !workbuddySuccess(response) {
		return nil
	}
	return objectValue(response, "data")
}

// --- pool integration --------------------------------------------------------------------

// candidateFromWorkbuddyAuthRoot extracts the account candidate from the
// desktop slot's four-segment JSON (mod.rs candidate_from_auth_root).
func candidateFromWorkbuddyAuthRoot(root map[string]any, variant string) *Account {
	accountObj := objectValue(root, "account")
	if accountObj == nil {
		return nil
	}
	authObj := objectValue(root, "auth")
	access := workbuddySecretText(authObj, "accessToken")
	if access == "" {
		return nil
	}
	uid := firstNonEmpty(stringValue(accountObj, "uid"), stringValue(accountObj, "id"))
	candidate := &Account{
		UserID:   uid,
		// Envelope-encrypted identity fields stay empty ({$wbEncrypted} values
		// are machine-bound ciphertext, not display text); the label is
		// derived from variant + uid instead.
		Nickname:         firstNonEmpty(workbuddyPlaintextText(accountObj, "nickname"), workbuddyPlaintextText(accountObj, "label")),
		Email:            stringValue(accountObj, "email"),
		Variant:          variant,
		EnterpriseID:     stringValue(accountObj, "enterpriseId"),
		EnterpriseName:   stringValue(accountObj, "enterpriseName"),
		AccessToken:      access,
		RefreshToken:     workbuddySecretText(authObj, "refreshToken"),
		TokenType:        firstNonEmpty(stringValue(authObj, "tokenType"), "Bearer"),
		Domain:           firstNonEmpty(stringValue(authObj, "domain"), stringValue(root, "domain")),
		ExpiresAt:        workbuddyNormTsMS(authObj["expiresAt"]),
		RefreshExpiresAt: workbuddyNormTsMS(authObj["refreshExpiresAt"]),
		AuthRaw:          jsonRaw(root),
		ProfileRaw:       jsonRaw(accountObj),
	}
	if candidate.Nickname == "" && candidate.Email == "" && uid != "" {
		short := uid
		if len(short) > 8 {
			short = short[:8]
		}
		candidate.Label = "WorkBuddy " + variant + " " + short
	}
	if uid != "" {
		candidate.ID = "w_" + uid
	}
	return candidate
}

// workbuddyPlaintextText returns a field only when it is a plain string —
// envelope maps ({$wbEncrypted}) mean the desktop app encrypted the value and
// the ciphertext must never leak into display fields.
func workbuddyPlaintextText(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return strings.TrimSpace(value)
}

// findWorkbuddyAccount mirrors the upsert dedupe priority uid > email >
// refreshToken (mod.rs upsert).
func (m *Manager) findWorkbuddyAccount(candidate *Account) *Account {
	kind := "workbuddy"
	if candidate.UserID != "" {
		if match := m.findAccount(kind, func(a *Account) bool { return a.UserID != "" && a.UserID == candidate.UserID }); match != nil {
			return match
		}
	}
	if candidate.Email != "" && strings.Contains(candidate.Email, "@") {
		if match := m.findAccount(kind, func(a *Account) bool { return a.Email != "" && a.Email == candidate.Email }); match != nil {
			return match
		}
	}
	if candidate.RefreshToken != "" {
		if match := m.findAccount(kind, func(a *Account) bool { return a.RefreshToken != "" && a.RefreshToken == candidate.RefreshToken }); match != nil {
			return match
		}
	}
	return nil
}

// upsertWorkbuddy merges a candidate with the envelope protection: an
// envelope capture must not overwrite a record that still holds an
// unexpired plain token (mod.rs upsert).
func (m *Manager) upsertWorkbuddy(candidate *Account, now time.Time) (*Account, bool, error) {
	existing := m.findWorkbuddyAccount(candidate)
	if existing != nil && workbuddyEnvelope(candidate.AccessToken) && workbuddyHasUnexpiredPlainToken(existing, now) {
		return existing, false, nil
	}
	return m.upsert("workbuddy", candidate)
}

// activeIDWorkbuddy matches the desktop slot's token against pooled
// accounts per variant (mod.rs current).
func (m *Manager) activeIDWorkbuddy() string {
	kind := "workbuddy"
	for _, variant := range []string{"cn", "ai"} {
		root := readWorkbuddyAuthFile(variant)
		if root == nil {
			continue
		}
		auth := objectValue(root, "auth")
		access := workbuddySecretText(auth, "accessToken")
		if access == "" {
			continue
		}
		refresh := workbuddySecretText(auth, "refreshToken")
		match := m.findAccount(kind, func(a *Account) bool {
			if workbuddyVariantFromAccount(a) != variant {
				return false
			}
			return a.AccessToken == access || (refresh != "" && a.RefreshToken == refresh)
		})
		if match != nil {
			return match.ID
		}
	}
	return ""
}

// captureWorkbuddyCurrent snapshots whichever variant's desktop slot is
// logged in (mod.rs capture_current/sync_current scan both variants).
func (m *Manager) captureWorkbuddyCurrent(_ context.Context) (*Account, bool, error) {
	for _, variant := range []string{"cn", "ai"} {
		root := readWorkbuddyAuthFile(variant)
		if root == nil {
			continue
		}
		candidate := candidateFromWorkbuddyAuthRoot(root, variant)
		if candidate == nil {
			continue
		}
		return m.upsertWorkbuddy(candidate, m.now())
	}
	return nil, false, errors.New("未读取到本机 WorkBuddy 登录信息（请先打开 WorkBuddy 登录，或扫码登录）")
}

// importWorkbuddy folds zcode-switcher's accounts-workbuddy single-file
// snapshots into the pool. Token fields may be plain strings OR envelope
// objects — the latter are preserved as compact JSON text.
func (m *Manager) importWorkbuddy(dir string) (imported []string, skipped []ImportSkip, err error) {
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		return nil, nil, fmt.Errorf("读取导入目录失败: %w", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, readFileErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readFileErr != nil {
			continue
		}
		var snapshot map[string]any
		if json.Unmarshal(raw, &snapshot) != nil {
			skipped = append(skipped, ImportSkip{ID: entry.Name(), Reason: "账号条目不是有效快照"})
			continue
		}
		id := strings.TrimSpace(stringValue(snapshot, "id"))
		if _, idErr := safeID(id); idErr != nil {
			skipped = append(skipped, ImportSkip{ID: id, Reason: idErr.Error()})
			continue
		}
		// refreshToken may be a plain string or an envelope object.
		refresh := workbuddySecretText(snapshot, "refreshToken")
		if refresh == "" {
			skipped = append(skipped, ImportSkip{ID: id, Reason: "缺少 refreshToken"})
			continue
		}
		if _, statErr := os.Stat(filepath.Join(m.storeDir("workbuddy"), id+".json")); statErr == nil {
			skipped = append(skipped, ImportSkip{ID: id, Reason: "已存在"})
			continue
		}
		access := workbuddySecretText(snapshot, "accessToken")
		if access == "" {
			skipped = append(skipped, ImportSkip{ID: id, Reason: "缺少 accessToken"})
			continue
		}
		acc := &Account{
			ID:               id,
			Label:            firstNonEmpty(stringValue(snapshot, "label"), stringValue(snapshot, "nickname"), stringValue(snapshot, "email"), id),
			Note:             stringValue(snapshot, "note"),
			UserID:           stringValue(snapshot, "uid"),
			Nickname:         stringValue(snapshot, "nickname"),
			Email:            stringValue(snapshot, "email"),
			Variant:          stringValue(snapshot, "variant"),
			EnterpriseID:     stringValue(snapshot, "enterpriseId"),
			EnterpriseName:   stringValue(snapshot, "enterpriseName"),
			AccessToken:      access,
			RefreshToken:     refresh,
			TokenType:        firstNonEmpty(stringValue(snapshot, "tokenType"), "Bearer"),
			Domain:           stringValue(snapshot, "domain"),
			ExpiresAt:        int64Or(snapshot["expiresAt"], 0),
			RefreshExpiresAt: int64Or(snapshot["refreshExpiresAt"], 0),
			AuthRaw:          jsonRawValue(snapshot["authRaw"]),
			ProfileRaw:       jsonRawValue(snapshot["profileRaw"]),
			Status:           StatusActive,
			CreatedAt:        int64Or(snapshot["createdAt"], 0),
		}
		if saveErr := m.saveAccount("workbuddy", acc); saveErr != nil {
			skipped = append(skipped, ImportSkip{ID: id, Reason: saveErr.Error()})
			continue
		}
		imported = append(imported, id)
	}
	return imported, skipped, nil
}

// jsonRawValue renders a decoded JSON value back to compact text ("" when absent).
func jsonRawValue(value any) string {
	if value == nil {
		return ""
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

// refreshWorkbuddyQuota is the workbuddy RefreshQuota branch (api.rs
// refresh_quota): lazy token refresh, credit query, synthetic quota window,
// rotated tokens persisted.
func (m *Manager) refreshWorkbuddyQuota(ctx context.Context, acc *Account) (*Account, error) {
	kind := "workbuddy"
	credits, working := m.queryWorkbuddyCredits(ctx, acc, m.now())
	merged := *working
	if credits.OK {
		merged.Quota = normalizeWorkbuddyCredits(credits)
		merged.QuotaUpdatedAt = m.now().UnixMilli()
		merged.QuotaError = ""
	} else {
		merged.QuotaError = credits.Error
	}
	if err := m.saveAccount(kind, &merged); err != nil {
		return &merged, err
	}
	if !credits.OK {
		return &merged, errors.New(credits.Error)
	}
	return &merged, nil
}
