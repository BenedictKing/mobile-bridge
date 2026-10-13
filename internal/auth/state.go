// Package auth: stateful side mirroring bridge/auth.py's Auth class —
// sessions, login failure accounting and cookie rendering. The pure helpers
// (password records, policy, expiry math) live in auth.go and are already
// parity-tested; this file adds the server state the HTTP layer needs.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"golang.org/x/crypto/pbkdf2"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	CookieName    = "codex_mobile_session"
	MaxFailures   = 5
	RememberHours = 7 * 24
)

// LoginRejected mirrors bridge.auth.LoginRejected: 403 + a login status body.
type LoginRejected struct {
	Message string
	Status  *jsoncompat.Object
}

func (e *LoginRejected) Error() string { return e.Message }

type Config struct {
	Mode         string // "password" | "none"
	Username     string
	Salt         string // hex
	Hash         string // hex
	Iterations   int
	SessionHours int
}

// Auth holds login state. now and newToken are injectable for tests.
type Auth struct {
	Config    Config
	Now       func() float64
	NewToken  func() string
	sessions  map[string]*jsoncompat.Object // sha256(token) -> session
	failures  map[string]int
	blockedIP map[string]bool
}

func pbkdf2Digest(password, salt []byte, iterations, keyLen int) []byte {
	return pbkdf2.Key(password, salt, iterations, keyLen, sha256.New)
}

func randomToken() string {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer)
}

func NewAuth(config Config) *Auth {
	if config.SessionHours == 0 {
		config.SessionHours = 12
	}
	if config.Mode == "" {
		config.Mode = "password"
	}
	return &Auth{Config: config, Now: func() float64 {
		return float64(time.Now().UnixNano()) / 1e9
	}, NewToken: randomToken, sessions: map[string]*jsoncompat.Object{},
		failures: map[string]int{}, blockedIP: map[string]bool{}}
}

func (a *Auth) sessionKey(token string) string {
	return SessionKey(token)
}

// LoginStatus mirrors login_status: attempts remaining / blocked shape.
func (a *Auth) LoginStatus(address string) *jsoncompat.Object {
	blocked := a.blockedIP[address]
	remaining := 0
	if !blocked {
		remaining = MaxFailures - a.failures[address]
		if remaining < 0 {
			remaining = 0
		}
	}
	status := jsoncompat.NewObject()
	status.Set("attemptsRemaining", jsoncompat.Num(remaining))
	status.Set("attemptLimit", jsoncompat.Num(MaxFailures))
	status.Set("blocked", blocked)
	return status
}

// Login mirrors Auth.login (password mode): constant-time compares of both
// the derived digest and the username, failure accounting with auto-block at
// MaxFailures, and failure clearing on success.
func (a *Auth) Login(username, password, address string, remember any) (string, *jsoncompat.Object, error) {
	if a.blockedIP[address] {
		return "", nil, &LoginRejected{Message: "此 IP 已被封禁，请在电脑网关 App 的“登录设备”中解除。",
			Status: a.LoginStatus(address)}
	}
	if a.Config.Mode != "none" {
		if !a.passwordMatches(password) || username != a.Config.Username {
			count := a.failures[address] + 1
			a.failures[address] = count
			if count >= MaxFailures {
				a.blockedIP[address] = true
				// Sessions from the offending address are dropped.
				for key, session := range a.sessions {
					if ip, _ := session.MustGet("ip").(string); ip == address {
						delete(a.sessions, key)
					}
				}
			}
			message := "账号或密码不正确"
			if a.blockedIP[address] {
				message = "此 IP 已被封禁，请在电脑网关 App 的“登录设备”中解除。"
			}
			return "", nil, &LoginRejected{Message: message, Status: a.LoginStatus(address)}
		}
	}
	delete(a.failures, address)
	return a.NewSession(address, "", remember)
}

func (a *Auth) passwordMatches(password string) bool {
	salt, err := hex.DecodeString(a.Config.Salt)
	if err != nil {
		return false
	}
	digest := pbkdf2Digest([]byte(password), salt, a.Config.Iterations, 32)
	return hex.EncodeToString(digest) == a.Config.Hash
}

// NewSession mirrors Auth.new_session: token + csrf + expiry bookkeeping.
func (a *Auth) NewSession(address, userAgent string, remember any) (string, *jsoncompat.Object, error) {
	if address != "" && a.blockedIP[address] {
		return "", nil, errors.New("此 IP 已被访问规则禁止")
	}
	now := a.Now()
	hours := a.Config.SessionHours
	if isTrue, ok := remember.(bool); ok && isTrue {
		hours = RememberHours
	}
	token := a.NewToken()
	session := jsoncompat.NewObject()
	session.Set("csrf", a.NewToken())
	session.Set("expires", json.Number(numberText(now+float64(hours)*3600)))
	session.Set("persistent", !isFalse(remember))
	session.Set("created", json.Number(numberText(now)))
	session.Set("lastSeen", json.Number(numberText(now)))
	session.Set("userAgent", truncateRunes(userAgent, 512))
	session.Set("ip", address)
	session.Set("peer", address)
	session.Set("source", "direct")
	a.sessions[a.sessionKey(token)] = session
	return token, session, nil
}

// Get mirrors Auth.get: validity check and lastSeen refresh (60s throttle).
func (a *Auth) Get(token string) *jsoncompat.Object {
	session, ok := a.sessions[a.sessionKey(token)]
	if !ok {
		return nil
	}
	expires, _ := session.MustGet("expires").(json.Number)
	expiry, _ := strconv.ParseFloat(expires.String(), 64)
	if !SessionValid(expiry, a.Now()) {
		delete(a.sessions, a.sessionKey(token))
		return nil
	}
	now := a.Now()
	lastSeen, _ := session.MustGet("lastSeen").(json.Number)
	seen, _ := strconv.ParseFloat(lastSeen.String(), 64)
	if now-seen >= 60 {
		session.Set("lastSeen", json.Number(numberText(now)))
	}
	return session
}

func (a *Auth) Logout(token string) {
	delete(a.sessions, a.sessionKey(token))
}

// CookieValue mirrors httpd's cookie(): the Set-Cookie header value for a
// session token, or a clearing cookie when token is empty.
func (a *Auth) CookieValue(token string) string {
	cookie := CookieName + "=" + token + "; Path=/; HttpOnly; SameSite=Strict"
	if token == "" {
		return cookie + "; Max-Age=0"
	}
	session := a.Get(token)
	if session == nil {
		return cookie + "; Max-Age=0"
	}
	if age := CookieAge(session, a.Now()); age != nil {
		return cookie + "; Max-Age=" + strconv.Itoa(*age)
	}
	return cookie
}

func numberText(value float64) string {
	if value == float64(int64(value)) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func isFalse(remember any) bool {
	value, ok := remember.(bool)
	return ok && !value
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
