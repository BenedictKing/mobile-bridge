// Connection discovery for paired companions (bridge/features/network/
// discovery.py port): anonymous ownership proofs over candidate entries,
// per-route derived credentials and the routes manifest that lets a phone
// migrate between LAN/tunnel entries. The relay discovery half is out of
// scope here; prove without a device parameter refuses like an unconfigured
// relay.
package httpd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const routeCookiePrefix = "cmbroute1_"

var hex64Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// routeMAC is discovery.py's mac(): HMAC-SHA256-hex over UTF-8 strings.
func routeMAC(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

// routeDigest is auth.key(): the sha256-hex stored form of a credential.
func routeDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// target returns the request's origin form "(https|http)://<Host>" the way
// httpd.py builds it for prove and cookie scoping.
func (h *handler) target() string {
	scheme := "http"
	if h.server.SecureHosts[h.request.Host] {
		scheme = "https"
	}
	return scheme + "://" + h.request.Host
}

// trustedOrigin reports whether an origin form is a published entry of this
// gateway (static hosts, dynamic LAN listeners or allowed tunnels).
func (h *handler) trustedOrigin(origin string) bool {
	if h.server.Origins[origin] {
		return true
	}
	return h.server.AllowOrigin != nil && h.server.AllowOrigin(origin)
}

// publishedRoutes enumerates candidate entries the phone may race: static
// origins plus native LAN candidates, filtered like discovery.mobile_routes
// (no path/credentials, no loopback/localhost, HTTPS or private-network
// HTTP).
func (h *handler) publishedRoutes() []string {
	candidates := make([]string, 0, len(h.server.Origins)+4)
	for origin := range h.server.Origins {
		candidates = append(candidates, origin)
	}
	if h.server.NativeCandidates != nil {
		candidates = append(candidates, h.server.NativeCandidates()...)
	}
	seen := map[string]bool{}
	routes := []string{}
	for _, origin := range candidates {
		if seen[origin] || !h.trustedOrigin(origin) || !publishableOrigin(origin) {
			continue
		}
		seen[origin] = true
		routes = append(routes, origin)
	}
	sort.Strings(routes)
	return routes
}

func publishableOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	host := parsed.Hostname()
	if host == "" || strings.EqualFold(host, "localhost") {
		return false
	}
	address := net.ParseIP(host)
	if address != nil && (address.IsLoopback() || address.IsUnspecified()) {
		return false
	}
	if parsed.Scheme == "https" {
		return true
	}
	return parsed.Scheme == "http" && address != nil && address.IsPrivate()
}

// routeCredential derives the cookie value a paired phone uses on one entry
// (the same formula GatewayChannels.token applies client-side).
func routeCredential(rootToken, target string) string {
	return routeCookiePrefix + routeMAC(rootToken, "gateway-route-v1\n"+target)
}

// mobileRoutes builds the routes manifest for a trusted companion session.
// rootToken is the session's raw cookie value; when the caller authenticated
// through a route-scoped cookie the stored bindings are left untouched (the
// paired manifest refresh only rides the root login).
func (h *handler) mobileRoutes(sessionKey, rootToken string, viaRouteCookie bool) *jsoncompat.Object {
	session := h.server.Auth.SessionByKey(sessionKey)
	if session == nil {
		return nil
	}
	if trusted, _ := session.MustGet("trustedDevice").(bool); !trusted {
		return nil
	}
	routes := h.publishedRoutes()
	bindings := jsoncompat.NewObject()
	if stored, ok := session.MustGet("routeBindings").(*jsoncompat.Object); ok {
		for _, key := range stored.Keys() {
			value, _ := stored.Get(key)
			bindings.Set(key, value)
		}
	}
	if !viaRouteCookie {
		// Separate verifier for proof of possession; it is not a login
		// credential. Refresh the expected bindings for the current roster.
		verifier := routeDigest(routeMAC(rootToken, "gateway-route-renew-v1"))
		current := jsoncompat.NewObject()
		for _, origin := range routes {
			current.Set(origin, routeDigest(routeCredential(rootToken, origin)))
		}
		same := len(current.Keys()) == len(bindings.Keys())
		if same {
			for _, key := range current.Keys() {
				value, _ := current.Get(key)
				bound, _ := bindings.Get(key)
				if value != bound {
					same = false
					break
				}
			}
		}
		storedRenewal, _ := session.MustGet("routeRenewal").(string)
		if !same || storedRenewal != verifier {
			h.server.Auth.MutateSessionByKey(sessionKey, func(record *jsoncompat.Object) bool {
				record.Set("routeBindings", current)
				record.Set("routeRenewal", verifier)
				record.Delete("routeLegacyRenewal")
				return true
			})
			bindings = current
		}
	}
	unbound := &jsoncompat.Array{}
	for _, origin := range routes {
		if value, _ := bindings.Get(origin); value == nil {
			unbound.Items = append(unbound.Items, origin)
		}
	}
	renewal := "anchor"
	if value, exists := session.Get("routeRenewal"); exists && value != nil {
		renewal = "paired"
	}
	// The manifest refresh may have updated the record; re-read for renewal.
	if refreshed := h.server.Auth.SessionByKey(sessionKey); refreshed != nil {
		if value, exists := refreshed.Get("routeRenewal"); exists && value != nil {
			renewal = "paired"
		}
	}
	race := jsoncompat.NewObject()
	race.Set("select", "firstVerified")
	race.Set("parallel", jsoncompat.Num(4))
	endpoints := &jsoncompat.Array{}
	for _, origin := range routes {
		endpoints.Items = append(endpoints.Items, origin)
	}
	result := jsoncompat.NewObject()
	result.Set("deviceId", h.server.Auth.DeviceID())
	result.Set("name", h.server.ComputerName)
	result.Set("endpoints", endpoints)
	result.Set("race", race)
	result.Set("renewal", renewal)
	result.Set("unbound", unbound)
	return result
}

// renewRoutes authorizes new entry bindings: a route-scoped login plus a
// signed statement from the paired phone (discovery.py renew).
func (h *handler) renewRoutes(sessionKey, cookieValue string, body *jsoncompat.Object) (int, error) {
	descriptor := h.mobileRoutes(sessionKey, cookieValue, true)
	session := h.server.Auth.SessionByKey(sessionKey)
	if descriptor == nil || session == nil || !strings.HasPrefix(cookieValue, routeCookiePrefix) {
		return http.StatusForbidden, errString("无效的信道绑定")
	}
	deviceID, _ := body.MustGet("deviceId").(string)
	claims, _ := body.MustGet("bindings").(*jsoncompat.Object)
	if deviceID != h.server.Auth.DeviceID() || claims == nil || len(claims.Keys()) < 1 || len(claims.Keys()) > 32 {
		return http.StatusBadRequest, errString("无效的信道绑定")
	}
	allowed := map[string]bool{}
	if entries, ok := descriptor.MustGet("endpoints").(*jsoncompat.Array); ok {
		for _, item := range entries.Items {
			if origin, _ := item.(string); origin != "" {
				allowed[origin] = true
			}
		}
	}
	anchor, _ := body.MustGet("anchor").(string)
	current := jsoncompat.NewObject()
	if stored, ok := session.MustGet("routeBindings").(*jsoncompat.Object); ok {
		for _, key := range stored.Keys() {
			value, _ := stored.Get(key)
			current.Set(key, value)
		}
	}
	verifier, hasVerifier := session.MustGet("routeRenewal").(string)
	if !hasVerifier || verifier == "" {
		verifier, hasVerifier = session.MustGet("routeLegacyRenewal").(string)
	}
	if !hasVerifier || verifier == "" {
		// Older clients sent the root login on the anchor address; the derived
		// secret for that address is unused, so its recorded digest serves.
		if anchor != "" {
			if bound, _ := current.MustGet(anchor).(string); bound != "" && bound != routeDigest(cookieValue) {
				verifier, hasVerifier = bound, true
			}
		}
	}
	for _, key := range claims.Keys() {
		if !allowed[key] {
			return http.StatusForbidden, errString("请重新验证原网关绑定")
		}
	}
	if !hasVerifier || verifier == "" {
		return http.StatusForbidden, errString("请重新验证原网关绑定")
	}
	proposed := jsoncompat.NewObject()
	for _, key := range current.Keys() {
		value, _ := current.Get(key)
		if allowed[key] {
			proposed.Set(key, value)
		}
	}
	for _, entry := range claims.Keys() {
		claim, _ := claims.MustGet(entry).(*jsoncompat.Object)
		if claim == nil {
			return http.StatusBadRequest, errString("信道凭据格式无效")
		}
		digest, _ := claim.MustGet("digest").(string)
		proof, _ := claim.MustGet("proof").(string)
		if !hex64Pattern.MatchString(digest) || !hex64Pattern.MatchString(proof) {
			return http.StatusBadRequest, errString("信道凭据格式无效")
		}
		if bound, exists := proposed.Get(entry); exists {
			if previous, _ := bound.(string); previous != "" && previous != digest {
				return http.StatusForbidden, errString("信道绑定身份已变化")
			}
		}
		message := strings.Join([]string{"route-grant-v1", h.server.Auth.DeviceID(), entry, digest}, "\n")
		if !hmac.Equal([]byte(routeMAC(verifier, message)), []byte(proof)) {
			return http.StatusForbidden, errString("信道绑定验证失败")
		}
		proposed.Set(entry, digest)
	}
	if !objectEqual(proposed, current) {
		h.server.Auth.MutateSessionByKey(sessionKey, func(record *jsoncompat.Object) bool {
			if _, hasRenewal := record.Get("routeRenewal"); !hasRenewal {
				record.Set("routeLegacyRenewal", verifier)
			}
			record.Set("routeBindings", proposed)
			return true
		})
	}
	result := jsoncompat.NewObject()
	result.Set("bound", true)
	return 0, nil
}

// prove answers an anonymous ownership challenge: only the holder of a live
// credential (root session key or this target's route binding) can produce
// the HMAC. A device parameter must name this gateway; without one only the
// relay path exists, which this gateway does not serve.
func (h *handler) prove(identifier, challenge, target, device string) (int, *jsoncompat.Object, error) {
	if !hex64Pattern.MatchString(identifier) || !hex64Pattern.MatchString(challenge) {
		return http.StatusBadRequest, nil, errString("Invalid identity challenge")
	}
	if device == "" {
		return http.StatusForbidden, nil, errString("Connection discovery unavailable")
	}
	if device != h.server.Auth.DeviceID() || !h.trustedOrigin(target) {
		return http.StatusForbidden, nil, errString("Connection discovery unavailable")
	}
	var proof *jsoncompat.Object
	h.server.Auth.EachSession(func(key string, session *jsoncompat.Object) bool {
		if trusted, _ := session.MustGet("trustedDevice").(bool); !trusted {
			return true
		}
		candidates := []string{key}
		if bindings, ok := session.MustGet("routeBindings").(*jsoncompat.Object); ok {
			if bound, _ := bindings.MustGet(target).(string); bound != "" {
				candidates = append(candidates, bound)
			}
		}
		for _, candidate := range candidates {
			if candidate == "" || routeDigest(candidate) != identifier {
				continue
			}
			result := jsoncompat.NewObject()
			result.Set("deviceId", device)
			result.Set("proof", routeMAC(candidate, device+"\n"+target+"\n"+challenge))
			proof = result
			return false
		}
		return true
	})
	if proof == nil {
		return http.StatusForbidden, nil, errString("Binding expired or revoked")
	}
	return 0, proof, nil
}

func objectEqual(left, right *jsoncompat.Object) bool {
	return string(jsoncompat.EncodeCompact(left)) == string(jsoncompat.EncodeCompact(right))
}

type fixedError struct{ message string }

func (e *fixedError) Error() string { return e.message }

func errString(message string) error { return &fixedError{message: message} }

// routeSessionKey resolves the effective session for a request's cookie: the
// namespaced cookie first (route values map to their parent session), the
// legacy pre-namespace cookie as a read-only fallback until renewal reissues.
func (h *handler) routeSessionKey() (token, sessionKey string, viaRouteCookie bool) {
	namespaced := h.server.Auth.CookieName()
	if cookie, err := h.request.Cookie(namespaced); err == nil && cookie.Value != "" {
		if strings.HasPrefix(cookie.Value, routeCookiePrefix) {
			return cookie.Value, h.server.Auth.ResolveRouteCookie(cookie.Value, h.target()), true
		}
		return cookie.Value, auth.SessionKey(cookie.Value), false
	}
	if cookie, err := h.request.Cookie(auth.CookieName); err == nil && cookie.Value != "" {
		return cookie.Value, auth.SessionKey(cookie.Value), false
	}
	return "", "", false
}
