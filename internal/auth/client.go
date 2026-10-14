// Client-IP resolution mirroring Auth.client (bridge/auth.py): the direct
// peer is the client unless the request arrives on a secure HTTPS-tunnel host
// through a trusted proxy — then X-Forwarded-For is walked from the immediate
// proxy backwards, never trusting the user-controlled left end.
package auth

import (
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func (a *Auth) Client(peer, host, forwardedFor, cfConnectingIP string, secure bool) *jsoncompat.Object {
	canonicalPeer := peer
	if value, err := CanonicalIP(peer); err == nil {
		canonicalPeer = value
	}
	trusted := map[string]bool{"127.0.0.1": true, "::1": true}
	a.mu.Lock()
	if a.policy != nil {
		if proxies, ok := a.policy.MustGet("trustedProxies").(*jsoncompat.Array); ok {
			for _, item := range proxies.Items {
				if text, ok := item.(string); ok {
					if canonical, err := CanonicalIP(text); err == nil {
						trusted[canonical] = true
					}
				}
			}
		}
	}
	a.mu.Unlock()

	result := jsoncompat.NewObject()
	result.Set("peer", canonicalPeer)
	if secure && trusted[canonicalPeer] {
		raw := ""
		if strings.HasSuffix(host, ".trycloudflare.com") {
			raw = cfConnectingIP
		}
		if raw == "" {
			raw = forwardedFor
		}
		address := canonicalPeer
		chain := strings.Split(raw, ",")
		for i := len(chain) - 1; i >= 0; i-- {
			candidate, err := CanonicalIP(strings.TrimSpace(chain[i]))
			if err != nil {
				// Any malformed hop falls back to the proxy peer, as Python
				// does when canonical_ip raises.
				result.Set("ip", canonicalPeer)
				result.Set("source", "proxy")
				return result
			}
			if !trusted[address] {
				break
			}
			address = candidate
		}
		result.Set("ip", address)
		result.Set("source", "forwarded")
		return result
	}
	result.Set("ip", canonicalPeer)
	if secure {
		result.Set("source", "proxy")
	} else {
		result.Set("source", "direct")
	}
	return result
}
