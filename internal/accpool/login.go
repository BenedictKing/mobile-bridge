package accpool

// In-gateway login flows (the zcode-switcher "add account" flows ported to
// the gateway, so the pool no longer depends on the standalone switcher for
// re-logins). Each provider registers a loginStart in its own kind file via
// registerLoginDriver; the shared state machine lives here.
//
// A login session is memory-only (like zcode-switcher's pending logins): a
// gateway restart drops in-flight logins and the web UI asks to start over.
// A successful login upserts the account (or replaces the tokens of the
// reauth target in place) and triggers one quota refresh.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// newLoginID mints a session id that stays unique across frozen test clocks
// and rapid successive starts.
func newLoginID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("lg_%d", time.Now().UnixNano())
	}
	return "lg_" + hex.EncodeToString(buf)
}

// LoginOptions carries the provider-specific choice (minimax: region cn/en;
// workbuddy: variant cn/ai; zcode: zai/bigmodel) and an optional reauth
// target — when set, the fresh credentials replace that account's tokens in
// place instead of creating a new pool entry.
type LoginOptions struct {
	Provider string
	ReauthID string
}

// LoginChallenge is the user-facing half of a started login: the code/link
// the user must confirm in a browser or the provider's app.
type LoginChallenge struct {
	UserCode  string `json:"userCode,omitempty"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int64  `json:"expiresIn,omitempty"` // seconds
}

// loginStart launches one provider login flow. It returns the user-facing
// challenge and a wait closure that blocks until the flow completes
// (returning the account candidate, not yet persisted), fails (error), or is
// cancelled (ctx). Implementations live in the provider's own file and
// register via registerLoginDriver.
type loginStart func(ctx context.Context, m *Manager, opts LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error)

var loginDrivers = map[string]loginStart{}

// registerLoginDriver is called from each provider's file (init or package
// var). Kind files register their driver unconditionally; kinds without a
// registered driver answer 该 agent 暂不支持网关内登录.
func registerLoginDriver(kind string, fn loginStart) {
	loginDrivers[kind] = fn
}

// RegisterLoginDriverForTest overrides the driver for kind and returns a
// restore function. It lets tests in other packages (httpd) stub a provider
// login instead of hitting the real network.
func RegisterLoginDriverForTest(kind string, fn func(ctx context.Context, m *Manager, opts LoginOptions) (*LoginChallenge, func(ctx context.Context) (*Account, error), error)) func() {
	prev, had := loginDrivers[kind]
	loginDrivers[kind] = loginStart(fn)
	return func() {
		if had {
			loginDrivers[kind] = prev
		} else {
			delete(loginDrivers, kind)
		}
	}
}

// Login session statuses.
const (
	LoginPending   = "pending"
	LoginDone      = "done"
	LoginFailed    = "failed"
	LoginCancelled = "cancelled"
)

// LoginSession tracks one in-flight (or recently finished) login.
type LoginSession struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Provider  string `json:"provider,omitempty"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	AccountID string `json:"accountId,omitempty"`
	StartedAt int64  `json:"startedAt"` // ms
}

type loginEntry struct {
	session *LoginSession
	cancel  context.CancelFunc
	done    chan struct{} // closed when the wait goroutine exits (entry cleanup)
}

// StartLogin validates the kind/provider, cancels any in-flight login of the
// same kind (mirrors zcode-switcher's single pending login per provider),
// starts the provider flow and spawns the wait goroutine.
func (m *Manager) StartLogin(ctx context.Context, kind string, opts LoginOptions) (*LoginSession, *LoginChallenge, error) {
	if !m.supports(kind) {
		return nil, nil, fmt.Errorf("不支持的 agent: %s", kind)
	}
	driver := loginDrivers[kind]
	if driver == nil {
		return nil, nil, fmt.Errorf("%s 暂不支持网关内登录", kind)
	}
	if opts.ReauthID != "" {
		if _, err := m.loadAccount(kind, opts.ReauthID); err != nil {
			return nil, nil, err
		}
	}
	challenge, wait, err := driver(ctx, m, opts)
	if err != nil {
		return nil, nil, err
	}
	session := &LoginSession{
		ID:        newLoginID(),
		Kind:      kind,
		Provider:  opts.Provider,
		Status:    LoginPending,
		StartedAt: m.now().UnixMilli(),
	}
	waitCtx, cancel := context.WithCancel(context.Background())
	entry := &loginEntry{session: session, cancel: cancel, done: make(chan struct{})}

	m.loginsMu.Lock()
	if m.logins == nil {
		m.logins = map[string]*loginEntry{}
	}
	if old := m.logins[kind]; old != nil && old.session.Status == LoginPending {
		old.cancel()
		m.finishLoginLocked(old, LoginCancelled, "被新的登录请求取代", "")
	}
	m.logins[kind] = entry
	m.loginsMu.Unlock()

	go m.runLogin(waitCtx, kind, entry, wait, opts.ReauthID)
	return session, challenge, nil
}

// LoginStatus returns a snapshot of the kind's current/last login session.
func (m *Manager) LoginStatus(kind string) (*LoginSession, error) {
	m.loginsMu.Lock()
	defer m.loginsMu.Unlock()
	entry := m.logins[kind]
	if entry == nil {
		return nil, errors.New("没有进行中的登录")
	}
	session := *entry.session
	return &session, nil
}

// CancelLogin cancels the kind's in-flight login (no-op when none pending).
func (m *Manager) CancelLogin(kind string) (*LoginSession, error) {
	m.loginsMu.Lock()
	defer m.loginsMu.Unlock()
	entry := m.logins[kind]
	if entry == nil {
		return nil, errors.New("没有进行中的登录")
	}
	if entry.session.Status == LoginPending {
		entry.cancel()
		m.finishLoginLocked(entry, LoginCancelled, "已取消", "")
	}
	session := *entry.session
	return &session, nil
}

func (m *Manager) finishLoginLocked(entry *loginEntry, status, message, accountID string) {
	entry.session.Status = status
	entry.session.Error = message
	entry.session.AccountID = accountID
}

func (m *Manager) runLogin(ctx context.Context, kind string, entry *loginEntry, wait func(ctx context.Context) (*Account, error), reauthID string) {
	defer close(entry.done)
	candidate, err := wait(ctx)
	if err != nil {
		m.loginsMu.Lock()
		if entry.session.Status == LoginPending {
			status := LoginFailed
			if errors.Is(err, context.Canceled) {
				status = LoginCancelled
			}
			m.finishLoginLocked(entry, status, err.Error(), "")
		}
		m.loginsMu.Unlock()
		return
	}
	// Persist under loginsMu: a superseded/cancelled login that completed
	// anyway (user confirmed after starting over) must not write its
	// credentials into the pool.
	m.loginsMu.Lock()
	defer m.loginsMu.Unlock()
	if entry.session.Status != LoginPending {
		return
	}
	account, err := m.persistLogin(kind, candidate, reauthID)
	if err != nil {
		m.finishLoginLocked(entry, LoginFailed, err.Error(), "")
		return
	}
	m.finishLoginLocked(entry, LoginDone, "", account.ID)
	// Best-effort: validate the fresh credentials in the background so login
	// completion never blocks on the provider's quota API.
	go func() {
		refreshCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = m.RefreshQuota(refreshCtx, kind, account.ID)
	}()
}

// persistLogin lands a completed login in the pool: a reauth target gets its
// tokens replaced in place; otherwise the candidate is upserted with the
// kind's dedupe rules.
func (m *Manager) persistLogin(kind string, candidate *Account, reauthID string) (*Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var account *Account
	if reauthID != "" {
		existing, err := m.loadAccount(kind, reauthID)
		if err != nil {
			return nil, err
		}
		merged := *existing
		merged.AccessToken = candidate.AccessToken
		if candidate.RefreshToken != "" {
			merged.RefreshToken = candidate.RefreshToken
		}
		if candidate.TokenType != "" {
			merged.TokenType = candidate.TokenType
		}
		if candidate.Scope != "" {
			merged.Scope = candidate.Scope
		}
		if candidate.ExpiresAt != 0 {
			merged.ExpiresAt = candidate.ExpiresAt
		}
		if candidate.ExpiresIn != 0 {
			merged.ExpiresIn = candidate.ExpiresIn
		}
		if candidate.RefreshExpiresAt != 0 {
			merged.RefreshExpiresAt = candidate.RefreshExpiresAt
		}
		if candidate.Credentials != "" {
			merged.Credentials = candidate.Credentials
		}
		if candidate.Config != "" {
			merged.Config = candidate.Config
		}
		if candidate.AuthRaw != "" {
			merged.AuthRaw = candidate.AuthRaw
		}
		if candidate.ProfileRaw != "" {
			merged.ProfileRaw = candidate.ProfileRaw
		}
		if candidate.UserID != "" {
			merged.UserID = candidate.UserID
		}
		if candidate.Email != "" {
			merged.Email = candidate.Email
		}
		if candidate.Nickname != "" {
			merged.Nickname = candidate.Nickname
		}
		if candidate.Domain != "" {
			merged.Domain = candidate.Domain
		}
		merged.Status = StatusActive
		merged.StatusReason = ""
		merged.QuotaError = ""
		if err := m.saveAccount(kind, &merged); err != nil {
			return nil, err
		}
		account = &merged
	} else {
		var upserted *Account
		var err error
		if kind == "workbuddy" {
			upserted, _, err = m.upsertWorkbuddy(candidate, m.now())
		} else {
			upserted, _, err = m.upsert(kind, candidate)
		}
		if err != nil {
			return nil, err
		}
		account = upserted
	}
	return account, nil
}
