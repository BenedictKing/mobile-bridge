package accpool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Status values, mirroring zcode-switcher's account lifecycle.
const (
	StatusActive         = "active"
	StatusReauthRequired = "reauth_required"
	StatusDisabled       = "disabled"
	StatusCoolingDown    = "cooling_down"
)

// Account is one pooled credential snapshot. The JSON field names match
// zcode-switcher's kimiManager format so snapshot files can be imported
// verbatim.
type Account struct {
	ID           string `json:"id"`
	Label        string `json:"label,omitempty"`
	Note         string `json:"note,omitempty"`
	Email        string `json:"email,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	UserID       string `json:"userId,omitempty"`
	Avatar       string `json:"avatar,omitempty"`
	LevelName    string `json:"levelName,omitempty"`
	Region       string `json:"region,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	TokenType    string `json:"tokenType,omitempty"`
	Scope        string `json:"scope,omitempty"`
	ExpiresAt    int64  `json:"expiresAt,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
	DeviceID     string `json:"deviceId,omitempty"`
	// Credentials/Config carry zcode's raw slot file texts (machine-bound
	// enc:v1 fields included); Provider is the active provider id from the
	// fingerprint. kimi accounts leave all three empty.
	Credentials    string `json:"credentials,omitempty"`
	Config         string `json:"config,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Status         string `json:"status,omitempty"`
	StatusReason   string `json:"statusReason,omitempty"`
	Quota          *Quota `json:"quota,omitempty"`
	QuotaUpdatedAt int64  `json:"quotaUpdatedAt,omitempty"`
	QuotaError     string `json:"quotaError,omitempty"`
	CreatedAt      int64  `json:"createdAt,omitempty"`
	LastUsedAt     int64  `json:"lastUsedAt,omitempty"`
}

// Manager owns the per-agent account pools under <dataDir>/pool/<kind>/.
type Manager struct {
	DataDir string
	// ZcasDir points at a zcode-switcher data directory (the one containing
	// accounts-kimi/) used as the import source; empty disables that source.
	ZcasDir string
	now     func() time.Time
	// mu serializes credential-slot mutations: a quota refresh that writes the
	// slot back must not interleave with a switch replacing it.
	mu sync.Mutex
}

func New(dataDir, zcasDir string) *Manager {
	return &Manager{DataDir: dataDir, ZcasDir: zcasDir, now: time.Now}
}

// Kinds lists the agents with pool support; the HTTP layer validates route
// parameters against it.
func (m *Manager) Kinds() []string { return []string{"kimi", "zcode"} }

func (m *Manager) supports(kind string) bool {
	for _, k := range m.Kinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func (m *Manager) storeDir(kind string) string {
	return filepath.Join(m.DataDir, "pool", kind)
}

var safeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]{3,80}$`)

func safeID(id string) (string, error) {
	id = strings.TrimSpace(id)
	if !safeIDPattern.MatchString(id) {
		return "", fmt.Errorf("非法账号 id: %s", id)
	}
	return id, nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) loadAccount(kind, id string) (*Account, error) {
	if _, err := safeID(id); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(m.storeDir(kind), id+".json"))
	if err != nil {
		return nil, fmt.Errorf("找不到账号快照: %s", id)
	}
	var acc Account
	if err := json.Unmarshal(raw, &acc); err != nil {
		return nil, fmt.Errorf("账号快照损坏: %s", id)
	}
	return &acc, nil
}

func (m *Manager) saveAccount(kind string, acc *Account) error {
	if _, err := safeID(acc.ID); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(acc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(m.storeDir(kind), acc.ID+".json"), append(raw, '\n'), 0o600)
}

// list returns every pooled account, most recently used first.
func (m *Manager) list(kind string) []*Account {
	entries, err := os.ReadDir(m.storeDir(kind))
	if err != nil {
		return nil
	}
	out := []*Account{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.storeDir(kind), entry.Name()))
		if err != nil {
			continue
		}
		var acc Account
		if json.Unmarshal(raw, &acc) == nil && acc.ID != "" {
			out = append(out, &acc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsedAt > out[j].LastUsedAt })
	return out
}

func (m *Manager) findAccount(kind string, pred func(*Account) bool) *Account {
	for _, acc := range m.list(kind) {
		if pred(acc) {
			return acc
		}
	}
	return nil
}

// upsert merges a candidate into the pool with zcode-switcher's dedupe
// priority: userId > refreshToken > email. Tokens and identity always come
// from the candidate.
func (m *Manager) upsert(kind string, candidate *Account) (*Account, bool, error) {
	var existing *Account
	if candidate.UserID != "" {
		existing = m.findAccount(kind, func(a *Account) bool { return a.UserID != "" && a.UserID == candidate.UserID })
	}
	if existing == nil && candidate.RefreshToken != "" {
		existing = m.findAccount(kind, func(a *Account) bool { return a.RefreshToken != "" && a.RefreshToken == candidate.RefreshToken })
	}
	if existing == nil && candidate.Email != "" {
		existing = m.findAccount(kind, func(a *Account) bool { return a.Email != "" && a.Email == candidate.Email })
	}
	now := m.now().UnixMilli()
	if existing != nil {
		merged := *existing
		merged.AccessToken = candidate.AccessToken
		if candidate.RefreshToken != "" {
			merged.RefreshToken = candidate.RefreshToken
		}
		if candidate.TokenType != "" {
			merged.TokenType = candidate.TokenType
		}
		merged.Scope = candidate.Scope
		if candidate.ExpiresAt != 0 {
			merged.ExpiresAt = candidate.ExpiresAt
		}
		if candidate.ExpiresIn != 0 {
			merged.ExpiresIn = candidate.ExpiresIn
		}
		if candidate.DeviceID != "" {
			merged.DeviceID = candidate.DeviceID
		}
		if candidate.Email != "" {
			merged.Email = candidate.Email
		}
		if candidate.Nickname != "" {
			merged.Nickname = candidate.Nickname
		}
		if candidate.UserID != "" {
			merged.UserID = candidate.UserID
		}
		if candidate.Avatar != "" {
			merged.Avatar = candidate.Avatar
		}
		if candidate.LevelName != "" {
			merged.LevelName = candidate.LevelName
		}
		if candidate.Region != "" {
			merged.Region = candidate.Region
		}
		if candidate.Credentials != "" {
			merged.Credentials = candidate.Credentials
		}
		if candidate.Config != "" {
			merged.Config = candidate.Config
		}
		if candidate.Provider != "" {
			merged.Provider = candidate.Provider
		}
		if candidate.Label != "" {
			merged.Label = candidate.Label
		}
		merged.Status = StatusActive
		merged.StatusReason = ""
		merged.LastUsedAt = now
		if err := m.saveAccount(kind, &merged); err != nil {
			return nil, false, err
		}
		return &merged, false, nil
	}
	id := candidate.ID
	if id == "" {
		if candidate.UserID != "" {
			id = "u_" + candidate.UserID
		} else if candidate.RefreshToken != "" {
			id = "k_" + shortHash(candidate.RefreshToken)
		} else {
			return nil, false, errors.New("账号缺少 userId 与 refreshToken，无法建档")
		}
	}
	if _, err := safeID(id); err != nil {
		return nil, false, err
	}
	acc := *candidate
	acc.ID = id
	if acc.Label == "" {
		acc.Label = firstNonEmpty(candidate.Nickname, candidate.Email, id)
	}
	if acc.Status == "" {
		acc.Status = StatusActive
	}
	acc.CreatedAt = now
	acc.LastUsedAt = now
	if err := m.saveAccount(kind, &acc); err != nil {
		return nil, false, err
	}
	return &acc, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// kimiHome resolves the official CLI state directory (KIMI_CODE_HOME wins,
// same rule as zcode-switcher's kimiPaths.kimiHome).
func kimiHome() string {
	if value := strings.TrimSpace(os.Getenv("KIMI_CODE_HOME")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".kimi-code")
	}
	return ""
}

// kimiCredentials is the official slot file ~/.kimi-code/credentials/kimi-code.json.
type kimiCredentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Scope        string `json:"scope"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func readKimiCredentials() *kimiCredentials {
	home := kimiHome()
	if home == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(home, "credentials", "kimi-code.json"))
	if err != nil {
		return nil
	}
	var creds kimiCredentials
	if json.Unmarshal(raw, &creds) != nil || creds.AccessToken == "" {
		return nil
	}
	return &creds
}

// activeID identifies which pooled account currently occupies the official
// slot (kimi: slot token match; zcode: slot fingerprint match).
func (m *Manager) activeID(kind string) string {
	switch kind {
	case "kimi":
		creds := readKimiCredentials()
		if creds == nil {
			return ""
		}
		if creds.RefreshToken != "" {
			if match := m.findAccount(kind, func(a *Account) bool { return a.RefreshToken == creds.RefreshToken }); match != nil {
				return match.ID
			}
		}
		if match := m.findAccount(kind, func(a *Account) bool { return a.AccessToken != "" && a.AccessToken == creds.AccessToken }); match != nil {
			return match.ID
		}
		return ""
	case "zcode":
		return m.activeIDZcode()
	}
	return ""
}

// ImportSkip explains why one snapshot file was not imported.
type ImportSkip struct {
	ID     string
	Reason string
}

// Import copies snapshots from a zcode-switcher store (its accounts-<kind>
// directory) into the pool, skipping entries that already exist. source may
// be the zcode-switcher data dir or the accounts-<kind> directory itself;
// empty falls back to the manager's configured ZcasDir.
func (m *Manager) Import(kind, source string) (imported []string, skipped []ImportSkip, err error) {
	if !m.supports(kind) {
		return nil, nil, fmt.Errorf("暂不支持的 Agent 类型: %s", kind)
	}
	if source == "" {
		source = m.ZcasDir
	}
	if source == "" {
		return nil, nil, errors.New("未配置 zcode-switcher 数据目录（GATEWAY_ZCAS_DIR）")
	}
	dir := source
	if !strings.HasSuffix(filepath.Base(dir), "accounts-"+kind) {
		dir = filepath.Join(dir, "accounts-"+kind)
	}
	if kind == "zcode" {
		return m.importZcode(dir)
	}
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
		var acc Account
		if json.Unmarshal(raw, &acc) != nil || acc.ID == "" {
			skipped = append(skipped, ImportSkip{ID: entry.Name(), Reason: "账号条目不是有效快照"})
			continue
		}
		if acc.RefreshToken == "" {
			skipped = append(skipped, ImportSkip{ID: acc.ID, Reason: "缺少 refreshToken"})
			continue
		}
		if _, statErr := os.Stat(filepath.Join(m.storeDir(kind), acc.ID+".json")); statErr == nil {
			skipped = append(skipped, ImportSkip{ID: acc.ID, Reason: "已存在"})
			continue
		}
		if saveErr := m.saveAccount(kind, &acc); saveErr != nil {
			skipped = append(skipped, ImportSkip{ID: acc.ID, Reason: saveErr.Error()})
			continue
		}
		imported = append(imported, acc.ID)
	}
	return imported, skipped, nil
}

// CaptureCurrent snapshots the official slot's current login state into the
// pool (kimiManager.captureCurrent / zcode manager.capture).
func (m *Manager) CaptureCurrent(ctx context.Context, kind string) (*Account, bool, error) {
	if kind == "zcode" {
		return m.captureZcodeCurrent()
	}
	if kind != "kimi" {
		return nil, false, fmt.Errorf("暂不支持的 Agent 类型: %s", kind)
	}
	creds := readKimiCredentials()
	if creds == nil {
		return nil, false, errors.New("未找到 Kimi Code 登录凭据（请先用 kimi CLI 登录）")
	}
	if creds.RefreshToken == "" {
		return nil, false, errors.New("凭据缺少 refresh_token，无法作为可续期快照保存")
	}
	candidate := &Account{
		AccessToken:  creds.AccessToken,
		RefreshToken: creds.RefreshToken,
		TokenType:    creds.TokenType,
		Scope:        creds.Scope,
		ExpiresAt:    creds.ExpiresAt,
		ExpiresIn:    creds.ExpiresIn,
	}
	if needsKimiRefresh(candidate.ExpiresAt, candidate.ExpiresIn, m.now()) {
		if tokens, err := refreshKimiToken(ctx, candidate.RefreshToken, ""); err == nil {
			applyTokenSet(candidate, tokens)
		}
	}
	if payload, err := kimiGet(ctx, kimiAPIBase+"/me", candidate.AccessToken); err == nil {
		profile := parseKimiProfile(payload)
		candidate.UserID = profile.UserID
		candidate.Nickname = profile.Nickname
		candidate.Email = profile.Email
		candidate.Avatar = profile.Avatar
		candidate.LevelName = profile.LevelName
		candidate.Region = profile.Region
	}
	return m.upsert(kind, candidate)
}

func applyTokenSet(acc *Account, tokens *tokenSet) {
	acc.AccessToken = tokens.AccessToken
	acc.RefreshToken = tokens.RefreshToken
	if tokens.TokenType != "" {
		acc.TokenType = tokens.TokenType
	}
	acc.Scope = tokens.Scope
	acc.ExpiresIn = tokens.ExpiresIn
	acc.ExpiresAt = tokens.ExpiresAt
}

// RefreshQuota renews the account's quota (and the kimi token when due),
// persists everything, and — when the account occupies the official slot —
// writes refreshed kimi tokens back so the CLI does not keep a rotated-out
// refresh_token.
func (m *Manager) RefreshQuota(ctx context.Context, kind, id string) (*Account, error) {
	if !m.supports(kind) {
		return nil, errUnsupported(kind)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	acc, err := m.loadAccount(kind, id)
	if err != nil {
		return nil, err
	}
	if kind == "zcode" {
		return m.refreshZcodeQuota(ctx, acc)
	}
	return m.refreshKimiQuota(ctx, acc)
}

// refreshKimiQuota is the kimi branch of RefreshQuota (kimiManager.refreshQuota).
func (m *Manager) refreshKimiQuota(ctx context.Context, acc *Account) (*Account, error) {
	kind := "kimi"
	slotCreds := readKimiCredentials()
	slotIsThis := slotCreds != nil &&
		((slotCreds.RefreshToken != "" && slotCreds.RefreshToken == acc.RefreshToken) ||
			(acc.AccessToken != "" && slotCreds.AccessToken == acc.AccessToken))

	if acc.RefreshToken != "" && needsKimiRefresh(acc.ExpiresAt, acc.ExpiresIn, m.now()) {
		tokens, refreshErr := refreshKimiToken(ctx, acc.RefreshToken, acc.DeviceID)
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
	if payload, meErr := kimiGet(ctx, kimiAPIBase+"/me", acc.AccessToken); meErr == nil {
		profile := parseKimiProfile(payload)
		acc.UserID = firstNonEmpty(profile.UserID, acc.UserID)
		acc.Nickname = firstNonEmpty(profile.Nickname, acc.Nickname)
		acc.Email = firstNonEmpty(profile.Email, acc.Email)
		acc.Avatar = firstNonEmpty(profile.Avatar, acc.Avatar)
		acc.LevelName = firstNonEmpty(profile.LevelName, acc.LevelName)
		acc.Region = firstNonEmpty(profile.Region, acc.Region)
	} else {
		problems = append(problems, "/me: "+meErr.Error())
		if isAuthFailed(meErr) {
			acc.Status = StatusReauthRequired
			acc.StatusReason = meErr.Error()
		}
	}
	if acc.Status != StatusReauthRequired {
		if payload, usageErr := kimiGet(ctx, kimiAPIBase+"/usages", acc.AccessToken); usageErr == nil {
			quota := parseKimiUsages(payload)
			if quota.LevelName == "" {
				quota.LevelName = acc.LevelName
			}
			if quota.Region == "" {
				quota.Region = acc.Region
			}
			acc.Quota = quota
			acc.QuotaUpdatedAt = m.now().UnixMilli()
			acc.QuotaError = ""
		} else {
			problems = append(problems, "/usages: "+usageErr.Error())
			acc.QuotaError = usageErr.Error()
			if isAuthFailed(usageErr) {
				acc.Status = StatusReauthRequired
				acc.StatusReason = usageErr.Error()
			}
		}
	}
	if err := m.saveAccount(kind, acc); err != nil {
		return acc, err
	}
	if slotIsThis && slotCreds != nil &&
		(slotCreds.AccessToken != acc.AccessToken || slotCreds.RefreshToken != acc.RefreshToken) {
		m.writeKimiSlot(acc)
	}
	if len(problems) > 0 && acc.Quota == nil {
		return acc, errors.New(strings.Join(problems, "; "))
	}
	return acc, nil
}

// RefreshAllQuotas refreshes every pooled account, collecting per-account
// errors instead of failing the batch.
func (m *Manager) RefreshAllQuotas(ctx context.Context, kind string) map[string]error {
	out := map[string]error{}
	for _, acc := range m.list(kind) {
		if ctx.Err() != nil {
			break
		}
		_, err := m.RefreshQuota(ctx, kind, acc.ID)
		if err != nil {
			out[acc.ID] = err
		}
	}
	return out
}

// writeKimiSlot replaces the official credentials file atomically.
func (m *Manager) writeKimiSlot(acc *Account) {
	home := kimiHome()
	if home == "" {
		return
	}
	wire := kimiCredentials{
		AccessToken:  acc.AccessToken,
		RefreshToken: acc.RefreshToken,
		ExpiresAt:    acc.ExpiresAt,
		Scope:        acc.Scope,
		TokenType:    firstNonEmpty(acc.TokenType, "Bearer"),
		ExpiresIn:    acc.ExpiresIn,
	}
	raw, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return
	}
	_ = atomicWrite(filepath.Join(home, "credentials", "kimi-code.json"), append(raw, '\n'), 0o600)
}

// view strips credentials for API consumers (kimiManager.toView). The pool
// speaks jsoncompat trees end to end because httpd's output encoder rejects
// every other container type.
func (m *Manager) view(acc *Account, activeID string) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("id", acc.ID)
	row.Set("label", acc.Label)
	row.Set("email", acc.Email)
	row.Set("nickname", acc.Nickname)
	row.Set("userId", acc.UserID)
	row.Set("provider", acc.Provider)
	row.Set("levelName", acc.LevelName)
	row.Set("region", acc.Region)
	row.Set("status", acc.Status)
	row.Set("active", acc.ID == activeID)
	row.Set("createdAt", acc.CreatedAt)
	row.Set("lastUsedAt", acc.LastUsedAt)
	row.Set("hasRefreshToken", acc.RefreshToken != "")
	row.Set("note", acc.Note)
	row.Set("statusReason", acc.StatusReason)
	if acc.ExpiresAt != 0 {
		row.Set("tokenExpiresAt", acc.ExpiresAt)
	} else {
		row.Set("tokenExpiresAt", nil)
	}
	if acc.Quota != nil {
		row.Set("quota", quotaView(acc.Quota))
		row.Set("quotaUpdatedAt", acc.QuotaUpdatedAt)
	} else {
		row.Set("quota", nil)
		row.Set("quotaUpdatedAt", nil)
	}
	row.Set("quotaError", acc.QuotaError)
	return row
}

func windowView(w *WindowQuota) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	row.Set("name", w.Name)
	row.Set("windowUnit", w.WindowUnit)
	if w.WindowDuration != nil {
		row.Set("windowDuration", *w.WindowDuration)
	} else {
		row.Set("windowDuration", nil)
	}
	row.Set("used", w.Used)
	row.Set("limit", w.Limit)
	row.Set("resetAt", w.ResetAt)
	if w.Auxiliary {
		row.Set("auxiliary", true)
	}
	return row
}

func quotaView(q *Quota) *jsoncompat.Object {
	row := jsoncompat.NewObject()
	if q.Weekly != nil {
		row.Set("weekly", windowView(q.Weekly))
	} else {
		row.Set("weekly", nil)
	}
	limits := &jsoncompat.Array{}
	for _, w := range q.Limits {
		limits.Items = append(limits.Items, windowView(w))
	}
	row.Set("limits", limits)
	if q.Booster != nil {
		booster := jsoncompat.NewObject()
		if q.Booster.TotalCents != nil {
			booster.Set("totalCents", *q.Booster.TotalCents)
		} else {
			booster.Set("totalCents", nil)
		}
		if q.Booster.BalanceCents != nil {
			booster.Set("balanceCents", *q.Booster.BalanceCents)
		} else {
			booster.Set("balanceCents", nil)
		}
		booster.Set("currency", q.Booster.Currency)
		row.Set("booster", booster)
	} else {
		row.Set("booster", nil)
	}
	row.Set("levelName", q.LevelName)
	row.Set("region", q.Region)
	return row
}

// Detail is the GET /api/pool/{kind} payload.
func (m *Manager) Detail(kind string) (*jsoncompat.Object, error) {
	if !m.supports(kind) {
		return nil, fmt.Errorf("暂不支持的 Agent 类型: %s", kind)
	}
	active := m.activeID(kind)
	accounts := &jsoncompat.Array{}
	for _, acc := range m.list(kind) {
		accounts.Items = append(accounts.Items, m.view(acc, active))
	}
	detail := jsoncompat.NewObject()
	detail.Set("kind", kind)
	detail.Set("activeId", active)
	detail.Set("autoSwitch", m.AutoSwitchEnabled(kind))
	detail.Set("accounts", accounts)
	detail.Set("history", historyView(m.History(kind, 50)))
	return detail, nil
}

// Overview is the GET /api/pool payload: one summary row per supported agent.
func (m *Manager) Overview() *jsoncompat.Object {
	agents := &jsoncompat.Array{}
	for _, kind := range m.Kinds() {
		summary := jsoncompat.NewObject()
		summary.Set("kind", kind)
		summary.Set("activeId", m.activeID(kind))
		summary.Set("accountCount", int64(len(m.list(kind))))
		agents.Items = append(agents.Items, summary)
	}
	overview := jsoncompat.NewObject()
	overview.Set("agents", agents)
	return overview
}
