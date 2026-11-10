// Mobile companion identity: the per-data-directory cookie namespace, the
// trusted-device session lifetime and the route-scoped cookie resolution that
// connection discovery (httpd/connection.go) builds on. Ports the mobile half
// of bridge/features/auth/auth.py: cookie_name, mobile_client, RouteToken and
// access-policy enforcement at the request path.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// mobileClientPattern matches the native shells' user agent
// (BridgeMobile/<version>-iOS|Android). It only selects a session lifetime
// after authentication; it is never a proof of identity.
var mobileClientPattern = regexp.MustCompile(`(?:^|\s)BridgeMobile/[\w.-]+-(?:iOS|Android)(?:\s|$)`)

// MobileClient mirrors Auth.mobile_client.
func MobileClient(userAgent string) bool {
	return mobileClientPattern.MatchString(userAgent)
}

// cookieNameSuffixLength matches Python's secrets.token_hex(12): 24 hex chars.
const cookieNameSuffixLength = 24

var cookieNamePattern = regexp.MustCompile(`^codex_mobile_session_[a-f0-9]{24}$`)

// ValidCookieName reports whether a persisted cookie name keeps the gateway's
// namespace invariant; the companion apps validate the same shape.
func ValidCookieName(name string) bool { return cookieNamePattern.MatchString(name) }

// cookieNames guards cookieName together with the session mutex; zero value
// means "not yet assigned".
func (a *Auth) initCookieNameLocked() {
	if a.cookieName == "" {
		a.cookieName = CookieName + "_" + randomHex(12)
	}
}

// CookieName returns this gateway instance's cookie name. The namespace
// belongs to the data directory (persisted in auth-sessions.json), so it
// survives restarts and lets one phone hold sessions for several gateways on
// the same origin form.
func (a *Auth) CookieName() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initCookieNameLocked()
	return a.cookieName
}

// DeviceID is the stable gateway identity companions see in mobileRoutes:
// sha256(cookie_name)[:32]. Derived from the persisted cookie namespace, so it
// survives restarts (discovery.py's gateway_id).
func (a *Auth) DeviceID() string {
	sum := sha256.Sum256([]byte(a.CookieName()))
	return hex.EncodeToString(sum[:])[:32]
}

// IsRouteCookie reports whether a cookie value is a route-scoped derived
// credential rather than a root login token.
func IsRouteCookie(value string) bool { return strings.HasPrefix(value, "cmbroute1_") }

// ResolveRouteCookie maps a cmbroute1_ cookie value back to its parent session
// key: accepted only while the parent session is live and its recorded binding
// for this exact target matches the value's digest (discovery.py's
// resolve_cookie). Returns "" when unresolved.
func (a *Auth) ResolveRouteCookie(value, target string) string {
	if !IsRouteCookie(value) {
		return ""
	}
	digest := SessionKey(value)
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, session := range a.sessions {
		if !a.sessionValidLocked(session) {
			continue
		}
		bindings, _ := session.MustGet("routeBindings").(*jsoncompat.Object)
		if bindings == nil {
			continue
		}
		if bound, _ := bindings.MustGet(target).(string); bound != "" && bound == digest {
			return key
		}
	}
	return ""
}

// SessionValidKey reports whether a stored session key (the owner recorded by
// push registration) still maps to a live session.
func (a *Auth) SessionValidKey(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	return ok && a.sessionValidLocked(session)
}

// SessionByKey snapshots the session stored under a key (nil when absent or
// expired). Callers use it with cookie/route resolution output.
func (a *Auth) SessionByKey(key string) *jsoncompat.Object {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if !ok || !a.sessionValidLocked(session) {
		return nil
	}
	snapshot, _ := jsoncompat.Clone(session).(*jsoncompat.Object)
	return snapshot
}

// GetSessionByKey is GetForClient keyed by the stored session key — the path
// route-scoped cookies authenticate through (their raw value is not the login
// token).
func (a *Auth) GetSessionByKey(key string, client *jsoncompat.Object, userAgent string) *jsoncompat.Object {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if !ok || !a.sessionValidLocked(session) {
		return nil
	}
	// The client/lastSeen refresh mirrors GetForClient over the same record.
	now := a.Now()
	lastSeen, _ := session.MustGet("lastSeen").(json.Number)
	seen, _ := strconv.ParseFloat(lastSeen.String(), 64)
	changed := applyMobileLifetime(session, userAgent, now) || now-seen >= 60
	if client != nil {
		ip, _ := client.MustGet("ip").(string)
		currentIP, _ := session.MustGet("ip").(string)
		if ip != currentIP {
			changed = true
		}
		for _, field := range []string{"ip", "peer", "source"} {
			if value, exists := client.Get(field); exists {
				session.Set(field, value)
			}
		}
	}
	if userAgent != "" {
		session.Set("userAgent", truncateRunes(userAgent, 512))
	}
	if changed {
		session.Set("lastSeen", json.Number(strconv.FormatFloat(now, 'f', -1, 64)))
		a.persistLocked()
	}
	snapshot, _ := jsoncompat.Clone(session).(*jsoncompat.Object)
	return snapshot
}

// MutateSessionByKey applies fn to the stored session under a.mu and persists
// when fn returns true. A missing or expired session aborts with false.
func (a *Auth) MutateSessionByKey(key string, fn func(session *jsoncompat.Object) bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	session, ok := a.sessions[key]
	if !ok || !a.sessionValidLocked(session) {
		return false
	}
	if !fn(session) {
		return true
	}
	a.persistLocked()
	return true
}

// EachSession iterates live sessions in sorted key order with snapshot copies
// until fn returns false. Prove walks the roster this way.
func (a *Auth) EachSession(fn func(key string, session *jsoncompat.Object) bool) {
	a.mu.Lock()
	keys := make([]string, 0, len(a.sessions))
	for key := range a.sessions {
		if session := a.sessions[key]; session != nil && a.sessionValidLocked(session) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	snapshots := make([]*jsoncompat.Object, 0, len(keys))
	for _, key := range keys {
		snapshot, _ := jsoncompat.Clone(a.sessions[key]).(*jsoncompat.Object)
		snapshots = append(snapshots, snapshot)
	}
	a.mu.Unlock()
	for index, key := range keys {
		if !fn(key, snapshots[index]) {
			return
		}
	}
}

// sessionValidLocked is SessionValid over the stored expires field.
func (a *Auth) sessionValidLocked(session *jsoncompat.Object) bool {
	if session == nil {
		return false
	}
	expires, _ := session.MustGet("expires").(json.Number)
	expiry, _ := strconv.ParseFloat(expires.String(), 64)
	return SessionValid(expiry, a.Now())
}

// Permitted mirrors Auth.permitted: auto-blocks always deny, an enabled
// allowlist must contain the canonical IP, a blocklist entry denies.
func (a *Auth) Permitted(address string) bool {
	a.mu.Lock()
	blocked := a.blockedIP[address]
	policy := a.policy
	a.mu.Unlock()
	if blocked {
		return false
	}
	if policy == nil {
		return true
	}
	if enabled, _ := policy.MustGet("allowlistEnabled").(bool); enabled {
		if !policyContains(policy, "allowlist", address) {
			return false
		}
	}
	return !policyContains(policy, "blocklist", address)
}

func policyContains(policy *jsoncompat.Object, key, address string) bool {
	list, _ := policy.MustGet(key).(*jsoncompat.Array)
	if list == nil {
		return false
	}
	for _, item := range list.Items {
		if text, _ := item.(string); text != "" {
			if canonical, err := CanonicalIP(text); err == nil {
				if canonical == address {
					return true
				}
				continue
			}
			if text == address {
				return true
			}
		}
	}
	return false
}

// applyMobileLifetime upgrades a session record for a mobile user agent at
// creation (newSessionLocked) and on later reads (GetForClient): trusted
// devices never expire. Reports whether the record changed.
func applyMobileLifetime(session *jsoncompat.Object, userAgent string, now float64) bool {
	if !MobileClient(userAgent) {
		return false
	}
	if trusted, _ := session.MustGet("trustedDevice").(bool); trusted {
		return false
	}
	session.Set("expires", jsoncompat.Num(0))
	session.Set("persistent", true)
	session.Set("trustedDevice", true)
	session.Set("remembered", false)
	return true
}
