package accpool

// MiniMax Code (mcode CLI) account management — the third pool kind, ported
// from zcode-switcher's src-tauri/src/minimax (mod.rs + api.rs).
//
// The official slot lives under ~/.minimax/ (MINIMAX_DATA_DIR overrides, same
// hook as the Rust side) and is the same six-file set mcode writes after
// login:
//
//	auth/prod/<region>/mcode-public/auth.json         OAuth record (source of truth)
//	auth/prod/<region>/mcode-public/auth-state.json   state machine (generation must match)
//	cli-auth/prod/<region>/local-runtime.auth.json    CLI runtime projection
//	cli-auth/prod/<region>/cli-auth.scope.json
//	cli-auth/shared-projection.json                   root projection owner (authFingerprint)
//	local-runtime.auth.json                           root projection (gateway auth context)
//	preferences/mcode-region.json                     region preference (cn / en)
//
// Unlike kimi, no process restart is needed: mcode reads auth.json per
// request (no in-memory cache) and watches auth-state.json, so a running
// instance picks up a switch as soon as the files land.
//
// Quota semantics differ from kimi too: MiniMax reports remaining PERCENT,
// which the pool inverts into its used/limit windows (used = 100 - pct,
// limit = 100). Snapshot timestamps: expiresAt is Unix SECONDS (like kimi's
// slot credentials), quotaUpdatedAt/createdAt/lastUsedAt are milliseconds.

import (
	"context"
	"crypto/md5"
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
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MiniMax OAuth/signature constants, aligned with api.rs (which mirrors the
// official mcode client). The yy/x-signature scheme is a wire constant, not a
// security boundary (same note as api.rs).
const (
	minimaxClientID        = "mcode-public"
	minimaxScopes          = "agent.default"
	minimaxAudience        = "agent-backend"
	minimaxNeedLoginCode   = 1000048
	minimaxDefaultExpireIn = 3600
)

// Per-region API origins (api.rs account_origin/matrix_origin/
// open_platform_origin). Package variables so tests can point them at a stub.
var (
	minimaxAccountOrigins = map[string]string{
		"cn": "https://account.minimax.cn",
		"en": "https://account.minimax.io",
	}
	minimaxMatrixOrigins = map[string]string{
		"cn": "https://agent.minimaxi.com",
		"en": "https://agent.minimax.io",
	}
	minimaxOpenOrigins = map[string]string{
		"cn": "https://www.minimaxi.com",
		"en": "https://platform.minimax.io",
	}
)

func minimaxOrigin(table map[string]string, region string) string {
	if origin := table[region]; origin != "" {
		return origin
	}
	return table["cn"]
}

var minimaxRegions = []string{"cn", "en"}

func validMinimaxRegion(region string) bool {
	for _, r := range minimaxRegions {
		if r == region {
			return true
		}
	}
	return false
}

// --- paths (data-dir.ts / namespace.ts / storage.ts) --------------------------

// minimaxHome resolves the mcode state directory (MINIMAX_DATA_DIR wins for
// tests, matching mod.rs minimax_home).
func minimaxHome() string {
	if value := strings.TrimSpace(os.Getenv("MINIMAX_DATA_DIR")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".minimax")
	}
	return ""
}

func minimaxAuthDir() string { return filepath.Join(minimaxHome(), "auth") }

func minimaxNSHome(region string) string {
	return filepath.Join(minimaxAuthDir(), "prod", region, minimaxClientID)
}

func minimaxAuthStatePath(region string) string {
	return filepath.Join(minimaxNSHome(region), "auth-state.json")
}

func minimaxCliAuthScopeDir(region string) string {
	return filepath.Join(minimaxHome(), "cli-auth", "prod", region)
}

func minimaxSharedProjectionPath() string {
	return filepath.Join(minimaxHome(), "cli-auth", "shared-projection.json")
}

func minimaxRootProjectionPath() string {
	return filepath.Join(minimaxHome(), "local-runtime.auth.json")
}

func minimaxRegionPrefPath() string {
	return filepath.Join(minimaxHome(), "preferences", "mcode-region.json")
}

// ensurePrivateChain mirrors mod.rs ensure_private_chain: the official
// healthCheck insists on 0700 for the auth directory chain; directories that
// already exist are left untouched.
func ensurePrivateChain(base string, rest ...string) error {
	if _, err := os.Stat(base); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(base, 0o700); err != nil {
			return err
		}
		_ = os.Chmod(base, 0o700)
	}
	current := base
	for _, segment := range rest {
		current = filepath.Join(current, segment)
		if _, err := os.Stat(current); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := os.Mkdir(current, 0o700); err != nil {
				return err
			}
			_ = os.Chmod(current, 0o700)
		}
	}
	return nil
}

// --- record key / slot reading -------------------------------------------------

// minimaxRecordKey is auth.json's record key (namespace.ts):
// service\0base64url(sha256(authHome\0clientId)).
func minimaxRecordKey(region string) string {
	sum := sha256.Sum256([]byte(minimaxAuthDir() + "\x00" + minimaxClientID))
	account := base64.RawURLEncoding.EncodeToString(sum[:])
	return "com.minimax.mcode.oauth.prod." + region + "\x00" + account
}

func readMinimaxJSON(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// minimaxSlotRecord extracts this region's OAuth record from an auth.json
// payload; nil when absent.
func minimaxSlotRecord(payload map[string]any, region string) map[string]any {
	records := objectValue(payload, "records")
	if records == nil {
		return nil
	}
	record, _ := records[minimaxRecordKey(region)].(map[string]any)
	return record
}

// scanMinimaxSlot scans both regions' slots and returns the first region with
// a usable credential (preferred region first) — mod.rs scan_slot.
func scanMinimaxSlot() (string, map[string]any, bool) {
	order := []string{minimaxPreferredRegion()}
	for _, region := range minimaxRegions {
		if order[0] != region {
			order = append(order, region)
		}
	}
	for _, region := range order {
		payload := readMinimaxJSON(filepath.Join(minimaxNSHome(region), "auth.json"))
		if payload == nil {
			continue
		}
		record := minimaxSlotRecord(payload, region)
		if record == nil {
			continue
		}
		if strings.TrimSpace(stringValue(record, "accessToken")) == "" {
			continue
		}
		return region, record, true
	}
	return "", nil, false
}

// minimaxSlotTokens is the slot record in snapshot terms: token pair plus the
// expiry in SECONDS (auth.json stores expiresAtMs; auth.json keeps no
// expires_in, refresh falls back to 3600 like the Rust side).
func minimaxSlotTokens(record map[string]any) (access, refresh string, expiresAtSec int64) {
	access = strings.TrimSpace(stringValue(record, "accessToken"))
	refresh = strings.TrimSpace(stringValue(record, "refreshToken"))
	if ms := int64Or(record["expiresAtMs"], 0); ms > 0 {
		expiresAtSec = ms / 1000
	}
	return access, refresh, expiresAtSec
}

// minimaxPreferredRegion reads preferences/mcode-region.json (regions.prod),
// defaulting to cn like the mcode TUI.
func minimaxPreferredRegion() string {
	payload := readMinimaxJSON(minimaxRegionPrefPath())
	region := ""
	if regions := objectValue(payload, "regions"); regions != nil {
		region, _ = regions["prod"].(string)
	}
	if validMinimaxRegion(region) {
		return region
	}
	return "cn"
}

// minimaxSlotIsAccount reports whether the official slot currently holds the
// given account (refresh/access token match). Must be compared BEFORE a token
// refresh, which rotates refresh_token.
func minimaxSlotIsAccount(acc *Account) bool {
	_, record, ok := scanMinimaxSlot()
	if !ok {
		return false
	}
	access, refresh, _ := minimaxSlotTokens(record)
	if access == "" {
		return false
	}
	return (refresh != "" && acc.RefreshToken != "" && acc.RefreshToken == refresh) ||
		(acc.AccessToken != "" && acc.AccessToken == access)
}

// --- projection JSON (storage.ts, FIXED key order) ------------------------------
//
// projection_auth feeds authContextFingerprint: the sha256 is taken over the
// compact JSON string, so the key order is part of the algorithm. zcode-
// switcher builds the object with a fixed insertion order (serde_json
// preserve_order); do NOT route this through a Go map, whose marshal sorts
// keys alphabetically.

func jsonString(value string) string {
	var buf strings.Builder
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return strings.TrimRight(buf.String(), "\n")
}

// minimaxProjectionAuthJSON renders the projection auth object with the
// storage.ts key order: accessToken, realUserID, userEmail, userName,
// subUserName. Empty values are dropped, matching normalizeLocalRuntimeAuthContext.
func minimaxProjectionAuthJSON(acc *Account) string {
	pairs := [][2]string{
		{"accessToken", acc.AccessToken},
		{"realUserID", acc.UserID},
		{"userEmail", acc.Email},
		{"userName", acc.Nickname},
		{"subUserName", acc.SubUserName},
	}
	var builder strings.Builder
	builder.WriteByte('{')
	wrote := false
	for _, pair := range pairs {
		value := strings.TrimSpace(pair[1])
		if value == "" {
			continue
		}
		if wrote {
			builder.WriteByte(',')
		}
		wrote = true
		builder.WriteString(jsonString(pair[0]))
		builder.WriteByte(':')
		builder.WriteString(jsonString(value))
	}
	builder.WriteByte('}')
	return builder.String()
}

// minimaxProjectionBody renders {"version":1,"updatedAtMs":now,"auth":{...}}
// with the same fixed key order the Rust json! macro produces.
func minimaxProjectionBody(acc *Account, nowMS int64) string {
	return `{"version":1,"updatedAtMs":` + strconv.FormatInt(nowMS, 10) +
		`,"auth":` + minimaxProjectionAuthJSON(acc) + `}`
}

// prettyJSON re-indents a compact JSON blob with two spaces, matching
// serde_json's to_string_pretty layout. Key order is preserved as given —
// encoding/json cannot do this (its map marshal sorts keys), so indent
// manually. Returns the input unchanged when it is not valid JSON.
func prettyJSON(compact string) string {
	var value any
	if json.Unmarshal([]byte(compact), &value) != nil {
		return compact
	}
	var out strings.Builder
	indent := 0
	inString := false
	escaped := false
	for _, r := range compact {
		switch {
		case escaped:
			out.WriteRune(r)
			escaped = false
		case inString:
			out.WriteRune(r)
			if r == '\\' {
				escaped = true
			} else if r == '"' {
				inString = false
			}
		case r == '"':
			inString = true
			out.WriteRune(r)
		case r == '{' || r == '[':
			out.WriteRune(r)
			indent++
			out.WriteByte('\n')
			out.WriteString(strings.Repeat("  ", indent))
		case r == '}' || r == ']':
			indent--
			out.WriteByte('\n')
			out.WriteString(strings.Repeat("  ", indent))
			out.WriteRune(r)
		case r == ',':
			out.WriteRune(r)
			out.WriteByte('\n')
			out.WriteString(strings.Repeat("  ", indent))
		case r == ':':
			out.WriteString(": ")
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func newMinimaxUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		// crypto/rand never fails in practice; degrade to a timestamp mix.
		n := time.Now().UnixNano()
		for i := 0; i < 16; i++ {
			bytes[i] = byte(n >> (i % 8 * 8))
		}
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// --- write_official (mod.rs) -----------------------------------------------------

// writeMinimaxSlot replaces the official six-file slot set. generation is
// max(existing record, auth-state) + 1 and lands in both auth.json and
// auth-state.json; newEpoch swaps the loginEpoch (a real account switch),
// while a pure renewal keeps the old one, matching the official
// "loginEpoch is stable across refreshes" behavior.
func (m *Manager) writeMinimaxSlot(acc *Account, newEpoch bool) error {
	home := minimaxHome()
	if home == "" {
		return errors.New("无法定位 MiniMax 数据目录")
	}
	region := acc.Region
	if !validMinimaxRegion(region) {
		region = "cn"
	}
	access := strings.TrimSpace(acc.AccessToken)
	refresh := strings.TrimSpace(acc.RefreshToken)
	if access == "" || refresh == "" {
		return errors.New("账号缺少 accessToken/refreshToken，无法写入官方槽位")
	}
	expiresMS := int64(0)
	if acc.ExpiresAt > 0 {
		expiresMS = acc.ExpiresAt * 1000
	}
	if err := ensurePrivateChain(minimaxAuthDir(), "prod", region, minimaxClientID); err != nil {
		return err
	}

	// generation: mcode trusts the login only when auth-state.generation ==
	// record.generation; a switch counts as a new login session.
	existing := readMinimaxJSON(filepath.Join(minimaxNSHome(region), "auth.json"))
	if existing == nil {
		existing = map[string]any{"schemaVersion": 1.0, "records": map[string]any{}}
	}
	if _, ok := existing["schemaVersion"]; !ok {
		existing["schemaVersion"] = 1.0
	}
	if records := objectValue(existing, "records"); records == nil {
		existing["records"] = map[string]any{}
	}
	generation := int64Or(objectValue(minimaxSlotRecord(existing, region), "generation"), 0)
	if state := readMinimaxJSON(minimaxAuthStatePath(region)); state != nil {
		if stateGen := int64Or(state["generation"], 0); stateGen > generation {
			generation = stateGen
		}
	}
	generation++
	loginEpoch := ""
	if !newEpoch {
		loginEpoch = stringValue(minimaxSlotRecord(existing, region), "loginEpoch")
	}
	if loginEpoch == "" {
		loginEpoch = newMinimaxUUID()
	}

	record := `{"schemaVersion":1,"accessToken":` + jsonString(access) +
		`,"refreshToken":` + jsonString(refresh) +
		`,"tokenType":"Bearer","clientId":` + jsonString(minimaxClientID) +
		`,"scopes":[` + jsonString(minimaxScopes) + `],"audience":` + jsonString(minimaxAudience) +
		`,"expiresAtMs":` + strconv.FormatInt(expiresMS, 10) +
		`,"generation":` + strconv.FormatInt(generation, 10) +
		`,"loginEpoch":` + jsonString(loginEpoch) + `}`

	// Merge the record into the existing payload preserving its key order:
	// schemaVersion first, then records (existing record keys keep their
	// position; this region's record moves last when newly added).
	payload := `{"schemaVersion":` + jsonNumber(existing["schemaVersion"], 1) + `,"records":{`
	records := objectValue(existing, "records")
	wrote := false
	for _, key := range orderedKeys(records) {
		if wrote {
			payload += ","
		}
		wrote = true
		payload += jsonString(key) + ":" + jsonRaw(records[key])
	}
	if wrote {
		payload += ","
	}
	payload += jsonString(minimaxRecordKey(region)) + ":" + record + `}}`
	if err := atomicWrite(filepath.Join(minimaxNSHome(region), "auth.json"), []byte(prettyJSON(payload)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 auth.json 失败: %w", err)
	}

	state := `{"schemaVersion":2,"status":"authenticated","storeKind":"file","clientId":` + jsonString(minimaxClientID) +
		`,"scopes":[` + jsonString(minimaxScopes) + `],"audience":` + jsonString(minimaxAudience) +
		`,"buildEnv":"prod","region":` + jsonString(region) +
		`,"expiresAtMs":` + strconv.FormatInt(expiresMS, 10) +
		`,"generation":` + strconv.FormatInt(generation, 10) + `}`
	if err := atomicWrite(minimaxAuthStatePath(region), []byte(prettyJSON(state)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 auth-state.json 失败: %w", err)
	}

	// cli-auth projections + root projection (fingerprint matches the official
	// authContextFingerprint algorithm).
	nowMS := m.now().UnixMilli()
	authJSON := minimaxProjectionAuthJSON(acc)
	fingerprint := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(authJSON)))
	projection := minimaxProjectionBody(acc, nowMS)

	if err := ensurePrivateChain(minimaxHome(), "cli-auth", "prod", region); err != nil {
		return err
	}
	scopeDir := minimaxCliAuthScopeDir(region)
	if err := atomicWrite(filepath.Join(scopeDir, "local-runtime.auth.json"), []byte(prettyJSON(projection)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 cli-auth 投影失败: %w", err)
	}
	// The previous account's verified identity cache is invalid for the new
	// token; mcode re-verifies it.
	_ = os.Remove(filepath.Join(scopeDir, "account-identity.json"))
	scope := `{"version":1,"updatedAtMs":` + strconv.FormatInt(nowMS, 10) +
		`,"region":` + jsonString(region) + `,"buildEnv":"prod"}`
	if err := atomicWrite(filepath.Join(scopeDir, "cli-auth.scope.json"), []byte(prettyJSON(scope)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 cli-auth.scope.json 失败: %w", err)
	}
	shared := `{"version":1,"region":` + jsonString(region) +
		`,"buildEnv":"prod","authFingerprint":` + jsonString(fingerprint) +
		`,"updatedAtMs":` + strconv.FormatInt(nowMS, 10) + `}`
	if err := atomicWrite(minimaxSharedProjectionPath(), []byte(prettyJSON(shared)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 shared-projection.json 失败: %w", err)
	}
	if err := ensurePrivateChain(minimaxHome()); err != nil {
		return err
	}
	if err := atomicWrite(minimaxRootProjectionPath(), []byte(prettyJSON(projection)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入根投影失败: %w", err)
	}

	return m.setMinimaxRegionPreference(region, nowMS)
}

// setMinimaxRegionPreference persists preferences/mcode-region.json keeping
// any other region entries (mod.rs set_region_preference).
func (m *Manager) setMinimaxRegionPreference(region string, nowMS int64) error {
	pref := readMinimaxJSON(minimaxRegionPrefPath())
	version := int64(1)
	regions := map[string]any{}
	if pref != nil {
		if v := int64Or(pref["version"], 0); v == 1 {
			if existing := objectValue(pref, "regions"); existing != nil {
				regions = existing
			}
		}
	}
	regions["prod"] = region
	compact := `{"version":` + strconv.FormatInt(version, 10) + `,"regions":` + jsonRaw(regions) +
		`,"updatedAtMs":` + strconv.FormatInt(nowMS, 10) + `}`
	if err := ensurePrivateChain(minimaxHome(), "preferences"); err != nil {
		return err
	}
	if err := atomicWrite(minimaxRegionPrefPath(), []byte(prettyJSON(compact)+"\n"), 0o600); err != nil {
		return fmt.Errorf("写入 region 偏好失败: %w", err)
	}
	return nil
}

// orderedKeys flattens a records map deterministically: Go map iteration is
// random, so sort (the official client parses auth.json, so key order there
// is not load-bearing; only the fingerprint input is).
func orderedKeys(records map[string]any) []string {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// jsonNumber renders a numeric-ish value as JSON (float64 from JSON decode).
func jsonNumber(value any, fallback int64) string {
	if n := toInt64(value); n != nil {
		return strconv.FormatInt(*n, 10)
	}
	return strconv.FormatInt(fallback, 10)
}

// jsonRaw re-serializes a decoded JSON value compactly.
func jsonRaw(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(raw)
}

// --- signature headers (api.rs sign_headers) --------------------------------

func md5Hex(text string) string {
	sum := md5.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

// uriEncodeComponent mirrors JS encodeURIComponent: keep A-Za-z0-9 and
// - _ . ! ~ * ' ( ), percent-encode everything else uppercase.
func uriEncodeComponent(text string) string {
	var builder strings.Builder
	for _, b := range []byte(text) {
		switch {
		case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9',
			b == '-', b == '_', b == '.', b == '!', b == '~', b == '*', b == '\'', b == '(', b == ')':
			builder.WriteByte(b)
		default:
			builder.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return builder.String()
}

// minimaxMatrixQuery builds the matrix family's common query string
// (buildUrl, same order as api.rs). userID empty renders as 0.
func minimaxMatrixQuery(path, userID, region string, nowMS int64) string {
	lang := "zh"
	if region == "en" {
		lang = "en"
	}
	uid := strings.TrimSpace(userID)
	if uid == "" {
		uid = "0"
	}
	_, offset := time.Now().Zone()
	return fmt.Sprintf("%s?device_platform=mcode&biz_id=3&app_id=3001&version_code=22201&unix=%d&timezone_offset=%d&sys_language=%s&lang=%s&device_id=0&os_name=%s&browser_name=mcode&user_id=%s&client=mcode",
		path, nowMS, offset, lang, lang, runtime.GOOS, uid)
}

// minimaxSignHeaders renders the official client signature headers; the GET
// body is always "{}" (api.rs sign_headers).
func minimaxSignHeaders(pathWithSearch, body string, requestMS int64) (yy, timestamp, signature string) {
	second := requestMS / 1000
	yy = md5Hex(uriEncodeComponent(pathWithSearch) + "_" + body + md5Hex(strconv.FormatInt(requestMS, 10)) + "ooui")
	signature = md5Hex(strconv.FormatInt(second, 10) + "I*7Cf%WZ#S&%1RlZJ&C2" + body)
	return yy, strconv.FormatInt(second, 10), signature
}

// checkMinimaxBodyStatus applies the statusInfo / base_resp dual-code check;
// NEED_LOGIN (1000048) means the credentials are dead (api.rs check_body_status).
func checkMinimaxBodyStatus(body map[string]any, operation string) error {
	var code *int64
	if info := objectValue(body, "statusInfo"); info != nil {
		code = toInt64(info["code"])
	}
	if code == nil {
		if base := objectValue(body, "base_resp"); base != nil {
			code = toInt64(base["status_code"])
		}
	}
	if code == nil || *code == 0 {
		return nil
	}
	if *code == minimaxNeedLoginCode {
		return &authFailedError{message: "MiniMax 登录已过期，请重新登录"}
	}
	return fmt.Errorf("%s 返回状态码 %d", operation, *code)
}

// --- HTTP helpers ----------------------------------------------------------------

func minimaxDo(ctx context.Context, request *http.Request) ([]byte, int, error) {
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("请求失败（网络）：%w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("读取响应失败：%w", err)
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, response.StatusCode, &authFailedError{
			message: fmt.Sprintf("HTTP %d：登录已过期", response.StatusCode),
		}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		preview := string(body)
		if len(preview) > 180 {
			preview = preview[:180]
		}
		return nil, response.StatusCode, fmt.Errorf("HTTP %d：%s", response.StatusCode, preview)
	}
	return body, response.StatusCode, nil
}

// minimaxMatrixJSON performs one signed matrix-family request (GET/POST).
func minimaxMatrixJSON(ctx context.Context, method, region, path, body, accessToken, userID, operation string, now time.Time) (map[string]any, error) {
	pathQuery := minimaxMatrixQuery(path, userID, region, now.UnixMilli())
	endpoint := minimaxOrigin(minimaxMatrixOrigins, region) + pathQuery
	// GET requests carry no body; "{}" is only the signing constant (api.rs).
	var reader io.Reader
	if method != http.MethodGet {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("content-type", "application/json")
	request.Header.Set("user-agent", "MiniMaxCode")
	request.Header.Set("authorization", "Bearer "+accessToken)
	yy, timestamp, signature := minimaxSignHeaders(pathQuery, body, now.UnixMilli())
	request.Header.Set("yy", yy)
	request.Header.Set("x-timestamp", timestamp)
	request.Header.Set("x-signature", signature)
	raw, _, err := minimaxDo(ctx, request)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return nil, fmt.Errorf("响应不是有效 JSON")
	}
	if err := checkMinimaxBodyStatus(decoded, operation); err != nil {
		return nil, err
	}
	return decoded, nil
}

// minimaxFetchUserInfo is GET {matrix}/v1/api/user/info (identity).
func minimaxFetchUserInfo(ctx context.Context, region, accessToken, userID string, now time.Time) (map[string]any, error) {
	return minimaxMatrixJSON(ctx, http.MethodGet, region, "/v1/api/user/info", "{}", accessToken, userID, "账号身份请求", now)
}

// minimaxFetchExtraInfo is POST {matrix}/matrix/api/v1/user/get_user_extra_info
// (personal workspace membership).
func minimaxFetchExtraInfo(ctx context.Context, region, accessToken, userID string, now time.Time) (map[string]any, error) {
	return minimaxMatrixJSON(ctx, http.MethodPost, region, "/matrix/api/v1/user/get_user_extra_info", "{}", accessToken, userID, "workspace 请求", now)
}

// minimaxFetchPlanRemains is GET {open}/v1/api/openplatform/coding_plan/remains
// with the x-group-id header (open-platform domain, no signature headers).
func minimaxFetchPlanRemains(ctx context.Context, region, accessToken, opGroupID string) (map[string]any, error) {
	endpoint := minimaxOrigin(minimaxOpenOrigins, region) + "/v1/api/openplatform/coding_plan/remains"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+accessToken)
	request.Header.Set("x-group-id", opGroupID)
	raw, _, err := minimaxDo(ctx, request)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if json.Unmarshal(raw, &decoded) != nil {
		return nil, fmt.Errorf("响应不是有效 JSON")
	}
	return decoded, nil
}

// --- identity / membership parsing (api.rs parse_profile / parse_membership) -----

type minimaxProfile struct {
	UserID      string
	Nickname    string
	Email       string
	SubUserName string
}

func parseMinimaxProfile(payload map[string]any) minimaxProfile {
	var info map[string]any
	for _, key := range []string{"userInfo", "user_info"} {
		if data := objectValue(payload, "data"); data != nil {
			if nested := objectValue(data, key); nested != nil {
				info = nested
				break
			}
		}
		if nested := objectValue(payload, key); nested != nil {
			info = nested
			break
		}
	}
	if info == nil {
		return minimaxProfile{}
	}
	return minimaxProfile{
		UserID:      stringValue(info, "realUserID", "real_user_id"),
		Nickname:    stringValue(info, "userName", "name"),
		Email:       stringValue(info, "userEmail", "email"),
		SubUserName: stringValue(info, "subUserName", "sub_user_name"),
	}
}

// QuotaMembership is MiniMax's Coding Plan membership block, preserved from
// zcode-switcher snapshots so the UI can flag accounts without a Coding Plan.
// HasTokenPlan is a pointer: nil means "unknown" (the API omitted it), which
// renders differently from an explicit false.
type QuotaMembership struct {
	HasTokenPlan  *bool  `json:"hasTokenPlan,omitempty"`
	OpGroupID     string `json:"opGroupId,omitempty"`
	Tier          string `json:"tier,omitempty"`
	CreditBalance string `json:"creditBalance,omitempty"`
}

// UnmarshalJSON tolerates zcode-switcher snapshots (floats, nulls).
func (q *QuotaMembership) UnmarshalJSON(data []byte) error {
	var raw struct {
		HasTokenPlan  any `json:"hasTokenPlan"`
		OpGroupID     any `json:"opGroupId"`
		Tier          any `json:"tier"`
		CreditBalance any `json:"creditBalance"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if b, ok := raw.HasTokenPlan.(bool); ok {
		q.HasTokenPlan = &b
	}
	q.OpGroupID = stringValue(map[string]any{"v": raw.OpGroupID}, "v")
	q.Tier = stringValue(map[string]any{"v": raw.Tier}, "v")
	q.CreditBalance = stringValue(map[string]any{"v": raw.CreditBalance}, "v")
	return nil
}

// minimaxMembership is one workspace's plan info plus its workspace id.
type minimaxMembership struct {
	HasTokenPlan  *bool
	OpGroupID     string
	Tier          string
	CreditBalance string
	WorkspaceID   string
}

// parseMinimaxMembership mirrors api.rs parse_membership applied to a
// workspace object.
func parseMinimaxMembership(source map[string]any) *minimaxMembership {
	out := &minimaxMembership{}
	if b, ok := source["has_token_plan"].(bool); ok {
		out.HasTokenPlan = &b
	}
	out.OpGroupID = stringValue(source, "op_group_id")
	out.Tier = stringValue(source, "token_plan_tier")
	if summary := objectValue(source, "op_credit_summary"); summary != nil {
		out.CreditBalance = stringValue(summary, "total_remaining_amount")
	}
	if out.CreditBalance == "" {
		out.CreditBalance = stringValue(source, "opcredit_balance")
	}
	return out
}

// personalMinimaxWorkspace finds the personal workspace (workspace_type == 0)
// and returns its membership + workspace id (api.rs personal_workspace_membership).
func personalMinimaxWorkspace(payload map[string]any) *minimaxMembership {
	workspaces, _ := payload["workspaces"].([]any)
	if workspaces == nil {
		if data := objectValue(payload, "data"); data != nil {
			workspaces, _ = data["workspaces"].([]any)
		}
	}
	for _, item := range workspaces {
		workspace, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if int64Or(workspace["workspace_type"], -1) != 0 {
			continue
		}
		workspaceID := ""
		switch value := workspace["workspace_id"].(type) {
		case string:
			workspaceID = strings.TrimSpace(value)
		case float64:
			workspaceID = strconv.FormatInt(int64(value), 10)
		case json.Number:
			workspaceID = value.String()
		}
		if workspaceID == "" {
			continue
		}
		membership := parseMinimaxMembership(workspace)
		membership.WorkspaceID = workspaceID
		return membership
	}
	return nil
}

// --- coding_plan/remains normalization -------------------------------------------

// minimaxWindow inverts MiniMax's remaining-percent semantics into the pool's
// used/limit window: used = 100 - pct, limit = 100. An unlimited window
// (status == 3) contributes nothing to exhaustion checks, so it is dropped.
func minimaxWindow(item map[string]any, prefix, resetKey, name, unit string, duration int64) *WindowQuota {
	if value, ok := toFloat(item[prefix+"_status"]); ok && value == 3 {
		return nil
	}
	percent, ok := toFloat(item[prefix+"_remaining_percent"])
	if !ok {
		total, hasTotal := toFloat(item[prefix+"_total_count"])
		usage, hasUsage := toFloat(item[prefix+"_usage_count"])
		if hasTotal && hasUsage && total > 0 {
			percent = usage / total * 100
			ok = true
		}
	}
	if !ok {
		return nil
	}
	remaining := int64(percent + 0.5)
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	window := &WindowQuota{
		Name:       name,
		WindowUnit: unit,
		Used:       100 - remaining,
		Limit:      100,
	}
	if duration > 0 {
		d := duration
		window.WindowDuration = &d
	}
	if resetMS, ok := toFloat(item[resetKey]); ok && resetMS > 0 {
		window.ResetAt = epochToRFC3339(int64(resetMS))
	}
	return window
}

// parseMinimaxRemains folds coding_plan/remains onto the pool Quota shape:
// the 5-hour window becomes a minute/300 limit row, the weekly window lands in
// Quota.Weekly (api.rs parse_remains, primary model only; the auxiliary video
// window is dropped).
func parseMinimaxRemains(payload map[string]any, membership *minimaxMembership) *Quota {
	quota := &Quota{Limits: []*WindowQuota{}, Membership: toQuotaMembership(membership)}
	if base := objectValue(payload, "base_resp"); base != nil {
		if code := int64Or(base["status_code"], 0); code != 0 {
			return quota
		}
	}
	models, _ := payload["model_remains"].([]any)
	var primary map[string]any
	for _, item := range models {
		if model, ok := item.(map[string]any); ok {
			primary = model
			break
		}
	}
	if primary == nil {
		return quota
	}
	if window := minimaxWindow(primary, "current_interval", "end_time", "5小时额度", "minute", 300); window != nil {
		quota.Limits = append(quota.Limits, window)
	}
	quota.Weekly = minimaxWindow(primary, "current_weekly", "weekly_end_time", "周额度", "week", 0)
	if membership != nil {
		quota.LevelName = membership.Tier
	}
	return quota
}

func toQuotaMembership(m *minimaxMembership) *QuotaMembership {
	if m == nil {
		return nil
	}
	return &QuotaMembership{
		HasTokenPlan:  m.HasTokenPlan,
		OpGroupID:     m.OpGroupID,
		Tier:          m.Tier,
		CreditBalance: m.CreditBalance,
	}
}

// --- OAuth (api.rs refresh_token / parse_token_grant / needs_refresh) -------------

// needsMinimaxRefresh reuses the kimi lead rule: renew max(expires_in/2,
// 300s) before expiry (api.rs needs_refresh is the same formula).
func needsMinimaxRefresh(expiresAt, expiresIn int64, now time.Time) bool {
	return needsKimiRefresh(expiresAt, expiresIn, now)
}

// refreshMinimaxToken exchanges a refresh_token for a new pair
// (api.rs refresh_token). A missing refresh_token in the response keeps the
// old one; expiresAt is Unix SECONDS.
func refreshMinimaxToken(ctx context.Context, region, refreshToken string, now time.Time) (*tokenSet, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, errors.New("缺少 refresh_token，请重新登录")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {minimaxClientID},
		"scope":         {minimaxScopes},
		"audience":      {minimaxAudience},
	}
	endpoint := minimaxOrigin(minimaxAccountOrigins, region) + "/oauth2/token"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
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
		message := fmt.Sprintf("刷新 MiniMax token 失败: %s", code)
		if code == "invalid_grant" || code == "invalid_token" ||
			response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, &authFailedError{message: message}
		}
		return nil, errors.New(message)
	}
	access, _ := data["access_token"].(string)
	refresh, _ := data["refresh_token"].(string)
	if refresh == "" {
		refresh = refreshToken
	}
	if access == "" {
		return nil, errors.New("MiniMax token 端点响应缺少必要字段")
	}
	expiresIn := int64Or(data["expires_in"], minimaxDefaultExpireIn)
	if expiresIn <= 0 {
		expiresIn = minimaxDefaultExpireIn
	}
	return &tokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		Scope:        minimaxScopes,
		ExpiresIn:    expiresIn,
		ExpiresAt:    now.Unix() + expiresIn,
	}, nil
}

// --- device flow 登录（api.rs start_device_login / poll_device_login） -------------

const (
	minimaxDeviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	// minimaxMaxLoginSeconds is the device flow ceiling (api.rs MAX_LOGIN_MS).
	minimaxMaxLoginSeconds = int64(15 * 60)
	// minimaxMaxTransportErrors: consecutive network failures abort the poll
	// (api.rs MAX_TRANSPORT_ERRORS).
	minimaxMaxTransportErrors = 3
)

// minimaxSlowDownStep is added to the poll interval on slow_down (api.rs
// poll_device_login); a package variable so tests can shrink it.
var minimaxSlowDownStep = 5 * time.Second

// minimaxDeviceLogin is the in-flight device flow state (api.rs DeviceLogin).
type minimaxDeviceLogin struct {
	DeviceCode              string // 轮询参数；account 变体时为 user_code 的值
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	Interval                time.Duration
	Deadline                time.Time // 真实时钟过期点（总超时 15 分钟兜底）
	PollWithUserCode        bool      // account 变体：无 device_code，轮询改用 user_code 参数
	CodeVerifier            string
}

func init() { registerLoginDriver("minimax", startMinimaxLogin) }

// newMinimaxCodeVerifier mints the PKCE verifier: 32 random bytes, base64url
// without padding (api.rs random_verifier).
func newMinimaxCodeVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成 PKCE verifier 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// minimaxPostForm posts one application/x-www-form-urlencoded request and
// returns the status plus raw body; err is only non-nil for transport/read
// failures, which the device flow counts separately from HTTP error payloads.
func minimaxPostForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
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

// startMinimaxDeviceLogin is api.rs start_device_login: POST
// {account}/oauth2/device/code with PKCE S256. The response has two shapes —
// standard RFC 8628 (device_code, expires_in, interval in seconds) and the
// account variant (no device_code, expired_in, interval in milliseconds,
// poll with user_code instead).
func startMinimaxDeviceLogin(ctx context.Context, region, verifier string) (*minimaxDeviceLogin, int64, error) {
	challengeSum := sha256.Sum256([]byte(verifier))
	form := url.Values{
		"client_id":             {minimaxClientID},
		"scope":                 {minimaxScopes},
		"audience":              {minimaxAudience},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challengeSum[:])},
		"code_challenge_method": {"S256"},
	}
	endpoint := minimaxOrigin(minimaxAccountOrigins, region) + "/oauth2/device/code"
	status, body, err := minimaxPostForm(ctx, endpoint, form)
	if err != nil {
		return nil, 0, err
	}
	if status < 200 || status >= 300 {
		return nil, 0, fmt.Errorf("MiniMax device flow 发起失败（HTTP %d）：%s", status, headString(string(body), 200))
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil {
		return nil, 0, errors.New("MiniMax device flow 响应不是有效 JSON")
	}
	str := func(key string) string { return strings.TrimSpace(stringValue(data, key)) }
	userCode := str("user_code")
	if userCode == "" {
		return nil, 0, errors.New("MiniMax device flow 响应缺少 user_code")
	}
	deviceCode := str("device_code")
	accountExpiredIn, hasAccountExpiry := toFloat(data["expired_in"])
	hasAccountExpiry = hasAccountExpiry && accountExpiredIn > 0
	usesAccountPolling := deviceCode == "" && hasAccountExpiry
	if deviceCode == "" && !usesAccountPolling {
		return nil, 0, errors.New("MiniMax device flow 响应缺少 device_code")
	}
	verificationURIComplete := str("verification_uri_complete")
	verificationURI := str("verification_uri")
	if verificationURI == "" {
		verificationURI = str("verification_url")
	}
	if verificationURI == "" {
		verificationURI = verificationURIComplete
	}
	if verificationURI == "" {
		return nil, 0, errors.New("MiniMax device flow 响应缺少 verification_uri")
	}

	expiresIn := int64Or(data["expires_in"], 0)
	if expiresIn <= 0 && hasAccountExpiry {
		// account 变体的 expired_in 可能是绝对毫秒时间戳，换算剩余秒。
		if accountExpiredIn < 1_000_000_000_000 {
			expiresIn = int64(accountExpiredIn)
		} else if remain := int64(accountExpiredIn) - time.Now().UnixMilli(); remain > 0 {
			expiresIn = (remain + 999) / 1000
		}
	}
	if expiresIn <= 0 {
		expiresIn = minimaxMaxLoginSeconds
	}
	if expiresIn > minimaxMaxLoginSeconds {
		expiresIn = minimaxMaxLoginSeconds
	}
	interval, _ := toFloat(data["interval"])
	intervalSeconds := int64(interval)
	if usesAccountPolling && interval > 0 {
		// account 变体的 interval 单位是毫秒。
		intervalSeconds = int64(interval/1000 + 0.999999)
	}
	if intervalSeconds < 1 {
		intervalSeconds = 1
	}

	login := &minimaxDeviceLogin{
		DeviceCode:              deviceCode,
		UserCode:                userCode,
		VerificationURI:         verificationURI,
		VerificationURIComplete: verificationURIComplete,
		Interval:                time.Duration(intervalSeconds) * time.Second,
		Deadline:                time.Now().Add(time.Duration(expiresIn) * time.Second),
		PollWithUserCode:        usesAccountPolling,
		CodeVerifier:            verifier,
	}
	if usesAccountPolling {
		login.DeviceCode = userCode
	}
	return login, expiresIn, nil
}

// parseMinimaxLoginGrant is api.rs parse_token_grant applied to the device
// flow result (no previous refresh token to fall back on). expiresAt is Unix
// SECONDS like every other minimax credential.
func parseMinimaxLoginGrant(data map[string]any, now time.Time) (*tokenSet, error) {
	access := strings.TrimSpace(stringValue(data, "access_token"))
	refresh := strings.TrimSpace(stringValue(data, "refresh_token"))
	if access == "" || refresh == "" {
		return nil, errors.New("MiniMax token 端点响应缺少必要字段")
	}
	expiresIn := int64Or(data["expires_in"], minimaxDefaultExpireIn)
	if expiresIn <= 0 {
		expiresIn = minimaxDefaultExpireIn
	}
	return &tokenSet{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		Scope:        minimaxScopes,
		ExpiresIn:    expiresIn,
		ExpiresAt:    now.Unix() + expiresIn,
	}, nil
}

// pollMinimaxDeviceLogin is api.rs poll_device_login: sleep interval → POST
// {account}/oauth2/token → interpret the dual pending/slow_down/denied/expired
// signalling (status field or standard error field) until the grant lands.
func pollMinimaxDeviceLogin(ctx context.Context, login *minimaxDeviceLogin, region string) (*tokenSet, error) {
	interval := login.Interval
	transportErrors := 0
	endpoint := minimaxOrigin(minimaxAccountOrigins, region) + "/oauth2/token"
	for {
		if !time.Now().Before(login.Deadline) {
			return nil, errors.New("登录超时（验证码已过期），请重试")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		form := url.Values{
			"grant_type":    {minimaxDeviceGrantType},
			"client_id":     {minimaxClientID},
			"code_verifier": {login.CodeVerifier},
		}
		if login.PollWithUserCode {
			form.Set("user_code", login.UserCode)
		} else {
			form.Set("device_code", login.DeviceCode)
		}
		status, body, err := minimaxPostForm(ctx, endpoint, form)
		if err != nil {
			transportErrors++
			if transportErrors >= minimaxMaxTransportErrors {
				return nil, fmt.Errorf("轮询登录状态失败（网络）：%w", err)
			}
			continue
		}
		transportErrors = 0

		var data map[string]any
		_ = json.Unmarshal(body, &data)
		errorField := stringValue(data, "error")
		pollingStatus := stringValue(data, "status")
		if status >= 200 && status < 300 && errorField == "" {
			switch pollingStatus {
			case "pending":
				continue
			case "slow_down":
				interval += minimaxSlowDownStep
				continue
			case "denied", "access_denied":
				return nil, errors.New("授权已被拒绝")
			case "expired", "expired_token":
				return nil, errors.New("验证码已过期，请重试")
			default:
				return parseMinimaxLoginGrant(data, time.Now())
			}
		}
		switch {
		case errorField == "authorization_pending" ||
			(errorField == "" && strings.Contains(string(body), "authorization_pending")):
			continue
		case errorField == "slow_down":
			interval += minimaxSlowDownStep
			continue
		case errorField == "access_denied":
			return nil, errors.New("授权已被拒绝")
		case errorField == "expired_token":
			return nil, errors.New("验证码已过期，请重试")
		case errorField != "":
			description := stringValue(data, "error_description")
			if description != "" {
				description = "（" + description + "）"
			}
			return nil, fmt.Errorf("MiniMax 登录失败: %s%s", errorField, description)
		default:
			return nil, fmt.Errorf("MiniMax token 端点返回异常（HTTP %d）：%s", status, headString(string(body), 200))
		}
	}
}

// headString truncates a payload preview like api.rs's chars().take(N).
func headString(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}

// startMinimaxLogin is the minimax loginStart driver: device flow + PKCE S256
// (Provider = region, cn 默认 / en)，授权完成后尽力补身份并返回 candidate
// 账号（不落盘，由 login.go 状态机统一持久化）。
func startMinimaxLogin(ctx context.Context, m *Manager, opts LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error) {
	region := opts.Provider
	if !validMinimaxRegion(region) {
		region = "cn"
	}
	verifier, err := newMinimaxCodeVerifier()
	if err != nil {
		return nil, nil, err
	}
	login, expiresIn, err := startMinimaxDeviceLogin(ctx, region, verifier)
	if err != nil {
		return nil, nil, err
	}
	page := login.VerificationURIComplete
	if page == "" {
		page = login.VerificationURI
	}
	challenge := &LoginChallenge{UserCode: login.UserCode, URL: page, ExpiresIn: expiresIn}
	wait := func(ctx context.Context) (*Account, error) {
		tokens, err := pollMinimaxDeviceLogin(ctx, login, region)
		if err != nil {
			return nil, err
		}
		candidate := &Account{
			AccessToken:  tokens.AccessToken,
			RefreshToken: tokens.RefreshToken,
			TokenType:    tokens.TokenType,
			Scope:        tokens.Scope,
			ExpiresIn:    tokens.ExpiresIn,
			ExpiresAt:    tokens.ExpiresAt,
			Region:       region,
		}
		// 补身份（尽力而为，api.rs complete_login）。
		if payload, err := minimaxFetchUserInfo(ctx, region, candidate.AccessToken, candidate.UserID, m.now()); err == nil {
			profile := parseMinimaxProfile(payload)
			candidate.UserID = profile.UserID
			candidate.Nickname = profile.Nickname
			candidate.Email = profile.Email
			candidate.SubUserName = profile.SubUserName
		}
		return candidate, nil
	}
	return challenge, wait, nil
}

// --- pool integration --------------------------------------------------------------

// activeIDMinimax matches the slot's current record against pooled accounts
// by refreshToken, then accessToken (mod.rs current/scan_slot).
func (m *Manager) activeIDMinimax() string {
	kind := "minimax"
	_, record, ok := scanMinimaxSlot()
	if !ok {
		return ""
	}
	access, refresh, _ := minimaxSlotTokens(record)
	if refresh != "" {
		if match := m.findAccount(kind, func(a *Account) bool { return a.RefreshToken != "" && a.RefreshToken == refresh }); match != nil {
			return match.ID
		}
	}
	if access != "" {
		if match := m.findAccount(kind, func(a *Account) bool { return a.AccessToken != "" && a.AccessToken == access }); match != nil {
			return match.ID
		}
	}
	return ""
}

// captureMinimaxCurrent snapshots the official slot's login state
// (mod.rs capture_current): refresh an expired token first (writing the slot
// back on success), then complete the identity via user/info, then upsert.
func (m *Manager) captureMinimaxCurrent(ctx context.Context) (*Account, bool, error) {
	kind := "minimax"
	region, record, ok := scanMinimaxSlot()
	if !ok {
		return nil, false, errors.New("未找到 MiniMax Code 登录凭据（请先用 mcode 登录）")
	}
	access, refresh, expiresAt := minimaxSlotTokens(record)
	if access == "" {
		return nil, false, errors.New("未找到 MiniMax Code 登录凭据（请先用 mcode 登录）")
	}
	if refresh == "" {
		return nil, false, errors.New("凭据缺少 refresh_token，无法作为可续期快照保存")
	}
	candidate := &Account{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresAt:    expiresAt,
		Region:       region,
	}
	if needsMinimaxRefresh(candidate.ExpiresAt, candidate.ExpiresIn, m.now()) {
		if tokens, err := refreshMinimaxToken(ctx, region, candidate.RefreshToken, m.now()); err == nil {
			applyTokenSet(candidate, tokens)
			_ = m.writeMinimaxSlot(candidate, false)
		}
	}
	if payload, err := minimaxFetchUserInfo(ctx, region, candidate.AccessToken, candidate.UserID, m.now()); err == nil {
		profile := parseMinimaxProfile(payload)
		candidate.UserID = profile.UserID
		candidate.Nickname = profile.Nickname
		candidate.Email = profile.Email
		candidate.SubUserName = profile.SubUserName
	}
	return m.upsert(kind, candidate)
}

// refreshMinimaxQuota is the minimax RefreshQuota branch (api.rs
// refresh_quota): slot comparison BEFORE the token refresh (the rotation
// invalidates the old refresh_token), then user/info → workspace membership →
// coding_plan/remains, then — when the account occupies the slot — write the
// possibly rotated tokens back so mcode does not keep a dead pair.
func (m *Manager) refreshMinimaxQuota(ctx context.Context, acc *Account) (*Account, error) {
	kind := "minimax"
	region := acc.Region
	if !validMinimaxRegion(region) {
		region = "cn"
	}
	slotIsThis := minimaxSlotIsAccount(acc)

	if acc.RefreshToken != "" && needsMinimaxRefresh(acc.ExpiresAt, acc.ExpiresIn, m.now()) {
		tokens, refreshErr := refreshMinimaxToken(ctx, region, acc.RefreshToken, m.now())
		if refreshErr != nil {
			if isAuthFailed(refreshErr) {
				acc.Status = StatusReauthRequired
				acc.StatusReason = refreshErr.Error()
				_ = m.saveAccount(kind, acc)
				return acc, errors.New("Token 已失效，请重新登录（reauth）")
			}
			return acc, refreshErr
		}
		applyTokenSet(acc, tokens)
		acc.Status = StatusActive
		acc.StatusReason = ""
	}

	var problems []string
	userID := acc.UserID
	if payload, err := minimaxFetchUserInfo(ctx, region, acc.AccessToken, userID, m.now()); err == nil {
		profile := parseMinimaxProfile(payload)
		acc.UserID = firstNonEmpty(profile.UserID, acc.UserID)
		acc.Nickname = firstNonEmpty(profile.Nickname, acc.Nickname)
		acc.Email = firstNonEmpty(profile.Email, acc.Email)
		acc.SubUserName = firstNonEmpty(profile.SubUserName, acc.SubUserName)
		userID = acc.UserID
	} else {
		problems = append(problems, "user/info: "+err.Error())
		if isAuthFailed(err) {
			acc.Status = StatusReauthRequired
			acc.StatusReason = err.Error()
		}
	}

	if acc.Status != StatusReauthRequired {
		var membership *minimaxMembership
		if payload, err := minimaxFetchExtraInfo(ctx, region, acc.AccessToken, userID, m.now()); err == nil {
			membership = personalMinimaxWorkspace(payload)
		} else {
			problems = append(problems, "workspace: "+err.Error())
			if isAuthFailed(err) {
				acc.Status = StatusReauthRequired
				acc.StatusReason = err.Error()
			}
		}
		quota := &Quota{Limits: []*WindowQuota{}, Membership: toQuotaMembership(membership)}
		if membership != nil {
			quota.LevelName = membership.Tier
			acc.Quota = quota
			if membership.OpGroupID != "" {
				if raw, err := minimaxFetchPlanRemains(ctx, region, acc.AccessToken, membership.OpGroupID); err == nil {
					quota = parseMinimaxRemains(raw, membership)
					if quota.Region == "" {
						quota.Region = region
					}
					acc.Quota = quota
				} else {
					problems = append(problems, "coding_plan/remains: "+err.Error())
					if isAuthFailed(err) {
						acc.Status = StatusReauthRequired
						acc.StatusReason = err.Error()
					}
				}
			}
			acc.QuotaUpdatedAt = m.now().UnixMilli()
			acc.QuotaError = ""
		} else {
			// No workspace info at all: keep the previous quota (if any) and
			// surface the collected problem as quotaError.
			acc.QuotaError = strings.Join(problems, "; ")
		}
		if len(problems) > 0 {
			acc.QuotaError = strings.Join(problems, "; ")
		}
	}

	if err := m.saveAccount(kind, acc); err != nil {
		return acc, err
	}
	if slotIsThis {
		_ = m.writeMinimaxSlot(acc, false)
	}
	if len(problems) > 0 && acc.Quota == nil {
		return acc, errors.New(strings.Join(problems, "; "))
	}
	return acc, nil
}
