// Package auth is the Go port of bridge/auth.py's pure helpers: session-hour
// validation, canonical IP formatting (including IPv4-mapped collapsing and
// scoped addresses), access-policy normalization, PBKDF2 password records and
// session expiry math. Byte parity with Python is enforced against
// conformance/auth goldens; error strings are part of the contract.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"

	"golang.org/x/crypto/pbkdf2"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	pbkdf2Iterations = 600000
	// RememberHours / cookie caps mirror bridge/auth.py.
	CookieCapDays = 400
)

// SessionHours mirrors session_hours: JSON ints only (no floats, no bools),
// range 0–87600.
func SessionHours(value any) (int, error) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, errors.New("登录有效期必须为 0–87600 的整数小时")
	}
	lexeme := number.String()
	parsed, err := strconv.Atoi(lexeme)
	if err != nil {
		// Atoi accepts "-3"; it rejects "3.0", "3e2" and anything else Python's
		// json would have decoded as float instead of int.
		return 0, errors.New("登录有效期必须为 0–87600 的整数小时")
	}
	if parsed < 0 || parsed > 87600 {
		return 0, errors.New("登录有效期必须为 0–87600 的整数小时")
	}
	return parsed, nil
}

// CanonicalIP mirrors canonical_ip: parse, collapse IPv4-mapped IPv6, and
// render. Scoped IPv6 addresses (fe80::1%eth0) pass through like Python's
// ipaddress module.
func CanonicalIP(value string) (string, error) {
	address, err := netip.ParseAddr(value)
	if err != nil {
		return "", fmt.Errorf("'%s' does not appear to be an IPv4 or IPv6 address", value)
	}
	if address.Is4In6() {
		address = address.Unmap()
	}
	return address.String(), nil
}

var policyListKeys = [...]string{"allowlist", "blocklist", "trustedProxies"}

// AccessPolicy mirrors access_policy: shape validation, per-entry IP
// canonicalization with order-preserving dedupe, and the allowlist rule.
// The result always carries all four keys in Python's order.
func AccessPolicy(value *jsoncompat.Object) (*jsoncompat.Object, error) {
	if value == nil {
		return nil, errors.New("IP 访问规则格式不正确")
	}
	enabled := false
	if raw, ok := value.Get("allowlistEnabled"); ok {
		enabled, ok = raw.(bool)
		if !ok {
			return nil, errors.New("IP 访问规则格式不正确")
		}
	}
	result := jsoncompat.NewObject()
	result.Set("allowlistEnabled", enabled)
	for _, key := range policyListKeys {
		entries, ok := value.Get(key)
		if !ok {
			entries = &jsoncompat.Array{}
		}
		list, isList := entries.(*jsoncompat.Array)
		if !isList || len(list.Items) > 256 {
			return nil, errors.New("IP 列表最多允许 256 个地址")
		}
		seen := map[string]bool{}
		canonical := &jsoncompat.Array{}
		invalid := false
		for _, item := range list.Items {
			text, isString := item.(string)
			if !isString || strings.Contains(text, "%") {
				invalid = true
				break
			}
			ip, err := CanonicalIP(strings.TrimSpace(text))
			if err != nil {
				invalid = true
				break
			}
			if !seen[ip] {
				seen[ip] = true
				canonical.Items = append(canonical.Items, ip)
			}
		}
		if invalid {
			return nil, errors.New("请填写有效的 IPv4 或 IPv6 地址，每行一个")
		}
		result.Set(key, canonical)
	}
	if enabled && len(result.MustGet("allowlist").(*jsoncompat.Array).Items) == 0 {
		return nil, errors.New("启用白名单前，请至少添加一个允许访问的 IP")
	}
	return result, nil
}

// PasswordRecord mirrors password_record with an injected salt (Python draws
// 16 random bytes; tests pass the oracle's salt through).
func PasswordRecord(password string, salt []byte) *jsoncompat.Object {
	digest := pbkdf2.Key([]byte(password), salt, pbkdf2Iterations, 32, sha256.New)
	record := jsoncompat.NewObject()
	record.Set("algorithm", "pbkdf2-sha256")
	record.Set("iterations", jsoncompat.Num(pbkdf2Iterations))
	record.Set("salt", hex.EncodeToString(salt))
	record.Set("hash", hex.EncodeToString(digest))
	return record
}

// SessionKey mirrors Auth.key: the on-disk session id is the SHA-256 of the
// bearer token.
func SessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SessionValid mirrors Auth.valid with an injected clock.
func SessionValid(expires float64, now float64) bool {
	return expires == 0 || expires > now
}

// CookieAge mirrors Auth.cookie_age's lifetime math with an injected clock.
// A nil result means "session cookie" (Python None). An expired session
// yields 0 because Auth.get() drops it before the math runs.
func CookieAge(session *jsoncompat.Object, now float64) *int {
	expires, _ := session.MustGet("expires").(json.Number)
	expiry, err := strconv.ParseFloat(expires.String(), 64)
	if err != nil {
		expiry = 0
	}
	if !SessionValid(expiry, now) {
		zero := 0
		return &zero
	}
	persistent := true
	if value, ok := session.Get("persistent"); ok {
		persistent = jsoncompat.Truthy(value)
	}
	if !persistent {
		return nil
	}
	if expiry == 0 {
		age := CookieCapDays * 86400
		return &age
	}
	age := int(math.Ceil(expiry - now))
	if age < 1 {
		age = 1
	}
	if age > CookieCapDays*86400 {
		age = CookieCapDays * 86400
	}
	return &age
}
