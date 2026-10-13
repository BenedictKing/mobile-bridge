// Package pairing is the Go port of bridge/pairing.py: origin-bound, short
// lived sign-in grants for QR login. The clock, token source and session
// issuer are injectable; parity with Python is enforced against
// conformance/pairing goldens (including error strings).
package pairing

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const TTL = 300

const (
	errUnavailable = "地址不可用"
	errLoopback    = "回环地址不能用于手机扫码，请开启局域网或外网连接"
	errQR          = "二维码已失效或已使用，请在电脑上刷新二维码，或使用账号密码登录"
	errThrottled   = "尝试次数过多，请 5 分钟后再试"
	errUnknownOp   = "未知操作"
)

// PhoneOrigin mirrors phone_origin: validate an http(s) origin URL and
// reject loopback hosts.
func PhoneOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", errors.New(errUnavailable)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New(errUnavailable)
	}
	host := strings.ToLower(parsed.Hostname())
	loopback := false
	if address, parseErr := netip.ParseAddr(host); parseErr == nil {
		loopback = address.IsLoopback()
	} else {
		loopback = host == "localhost" || strings.HasSuffix(host, ".localhost")
	}
	if loopback {
		return "", errors.New(errLoopback)
	}
	return parsed.Scheme + "://" + parsed.Host, nil
}

type grant struct {
	origin  string
	digest  [32]byte
	expires float64
	used    bool
}

// Pairing mirrors the Pairing grant registry. SessionIssuer stands in for
// Auth.new_session during exchange.
type Pairing struct {
	origins  map[string]bool
	order    []string
	grants   map[string]*grant
	failures map[string][]float64
	Issue    func(address string) any
}

func New(origins []string) *Pairing {
	set := map[string]bool{}
	for _, origin := range origins {
		set[origin] = true
	}
	return &Pairing{origins: set, grants: map[string]*grant{},
		failures: map[string][]float64{}}
}

func (p *Pairing) dropGrant(identifier string) {
	if _, exists := p.grants[identifier]; !exists {
		return
	}
	delete(p.grants, identifier)
	for i, key := range p.order {
		if key == identifier {
			p.order = append(p.order[:i], p.order[i+1:]...)
			break
		}
	}
}

// Control mirrors Pairing.control with an injected clock and id sources.
func (p *Pairing) Control(value *jsoncompat.Object, now float64,
	newToken func() string, newIdentifier func() string) (*jsoncompat.Object, error) {
	// Expire grants past TTL + 300, preserving order.
	kept := p.order[:0]
	for _, key := range p.order {
		if p.grants[key].expires+300 > now {
			kept = append(kept, key)
		} else {
			delete(p.grants, key)
		}
	}
	p.order = kept

	action, _ := value.MustGet("action").(string)
	if action == "create" {
		originInput, _ := value.MustGet("url").(string)
		origin, err := PhoneOrigin(originInput)
		if err != nil {
			return nil, err
		}
		if !p.origins[origin] {
			return nil, errors.New(errUnavailable)
		}
		// One current grant per origin: refresh revokes the preceding QR.
		for _, key := range append([]string{}, p.order...) {
			if p.grants[key].origin == origin {
				p.dropGrant(key)
			}
		}
		token := newToken()
		identifier := newIdentifier()
		p.grants[identifier] = &grant{origin: origin,
			digest: sha256.Sum256([]byte(token)), expires: now + TTL}
		p.order = append(p.order, identifier)
		result := jsoncompat.NewObject()
		result.Set("id", identifier)
		result.Set("url", origin+"/#pair="+token)
		result.Set("expires", json.Number(numberLexeme(now+TTL)))
		result.Set("state", "active")
		return result, nil
	}
	identifier, _ := value.MustGet("id").(string)
	entry := p.grants[identifier]
	if action == "revoke" {
		p.dropGrant(identifier)
		result := jsoncompat.NewObject()
		result.Set("state", "revoked")
		return result, nil
	}
	if action == "status" {
		if ids, ok := value.MustGet("ids").(*jsoncompat.Array); ok {
			states := jsoncompat.NewObject()
			limit := len(ids.Items)
			if limit > 64 {
				limit = 64
			}
			for _, raw := range ids.Items[:limit] {
				key, isString := raw.(string)
				if !isString {
					continue
				}
				states.Set(key, grantState(p.grants[key], now))
			}
			result := jsoncompat.NewObject()
			result.Set("states", states)
			return result, nil
		}
		result := jsoncompat.NewObject()
		result.Set("state", grantState(entry, now))
		return result, nil
	}
	return nil, errors.New(errUnknownOp)
}

func grantState(entry *grant, now float64) string {
	if entry == nil || entry.expires <= now {
		return "expired"
	}
	if entry.used {
		return "used"
	}
	return "active"
}

// numberLexeme renders the expires timestamp the way Python's json emits it:
// integral values without a trailing .0.
func numberLexeme(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// Exchange mirrors Pairing.exchange; the issuer callback produces the
// (token, session) pair Auth.new_session would return.
func (p *Pairing) Exchange(token, origin, address string, now float64,
	issue func(address string) any) (any, error) {
	if len(token) != 43 {
		return nil, errors.New(errQR)
	}
	digest := sha256.Sum256([]byte(token))
	recent := []float64{}
	for _, stamp := range p.failures[address] {
		if now-stamp < 300 {
			recent = append(recent, stamp)
		}
	}
	p.failures[address] = recent
	if len(recent) >= 8 {
		return nil, errors.New(errThrottled)
	}
	p.failures[address] = append(recent, now)
	for _, key := range append([]string{}, p.order...) {
		entry := p.grants[key]
		if entry == nil {
			continue
		}
		if entry.digest == digest && entry.origin == origin && entry.expires > now && !entry.used {
			entry.used = true
			delete(p.failures, address)
			return issue(address), nil
		}
	}
	return nil, errors.New(errQR)
}
