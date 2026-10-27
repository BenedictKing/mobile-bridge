package jev

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	// cachePerSession bounds entries per session; on overflow the session's
	// entries are cleared wholesale (bounded memory beats partial history).
	cachePerSession = 256
	// cacheMaxSessions bounds how many sessions keep a cache at all; the
	// earliest-inserted session is evicted first.
	cacheMaxSessions = 64
)

// cacheEntry stores one approved verdict's numbers — never the native
// response: option ids differ on every card (kimi mints q0_opt_0 style ids
// per request), so hits remap through approvalResponse on the current card
// and a stored response would replay stale ids.
type cacheEntry struct {
	categories  map[string]float64
	negativeSum float64
	threshold   float64
	reversible  float64
	confidence  float64
	recommend   string
	topCategory string
}

// sessionCache is one session's entry table, keyed by method + subject.
type sessionCache struct {
	entries map[string]cacheEntry
}

// approvalCache memoizes approved verdicts per (agent, session, method,
// normalized subject), versioned by the effective-config fingerprint: any
// settings change invalidates everything in one comparison.
type approvalCache struct {
	mu           sync.Mutex
	fingerprint  string
	sessions     map[string]*sessionCache
	sessionOrder []string
}

var (
	uuidRE  = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	isoRE   = regexp.MustCompile(`\d{4}-\d{2}-\d{2}([T ]\d{2}:\d{2}(:\d{2}(\.\d+)?)?(Z|[+-]\d{2}:?\d{2})?)?`)
	epochRE = regexp.MustCompile(`\b\d{10,13}\b`)
)

// Cache-key budgets: title and command each keep their own share so an
// over-long title cannot crowd the command's distinguishing bytes out of
// the key.
const (
	subjectTitleBudget   = 240
	subjectCommandBudget = 271
)

// normalizeSubject builds the cache key material from BOTH the title and the
// command. ACP toolCall titles are tool-level constants ("Bash", "编辑"), so
// a title-only key would let one approval cover every later call of the same
// tool in the session; the command is what actually distinguishes cards.
// Volatile fragments — UUIDs, ISO timestamps, standalone epoch seconds —
// collapse to placeholders so the same recurring operation hits one key.
// Question cards return "" and are never cached: their shared title would
// merge genuinely different questions.
func normalizeSubject(params *jsoncompat.Object) string {
	if params == nil {
		return ""
	}
	title, command, question := subjectOf(params)
	if question {
		return ""
	}
	if title == permissionRequestTitle {
		title = ""
	}
	title = truncateRunes(stripVolatile(title), subjectTitleBudget)
	command = truncateRunes(stripVolatile(command), subjectCommandBudget)
	if title == "" && command == "" {
		return ""
	}
	return title + "\x00" + command
}

// stripVolatile scrubs volatile fragments from one key segment and squeezes
// whitespace.
func stripVolatile(text string) string {
	text = uuidRE.ReplaceAllString(text, "<id>")
	text = isoRE.ReplaceAllString(text, "<ts>")
	text = collapseEpoch(text)
	return strings.Join(strings.Fields(text), " ")
}

// collapseEpoch replaces standalone 10-13 digit runs (epoch seconds or
// millis) with a placeholder, but keeps digits that sit inside path or
// filename tokens: adjacent to / . - _ they identify distinct objects
// (build/1730000000 vs build/9999999999), not timestamps.
func collapseEpoch(text string) string {
	matches := epochRE.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var rebuilt strings.Builder
	last := 0
	for _, loc := range matches {
		start, end := loc[0], loc[1]
		if start > last {
			rebuilt.WriteString(text[last:start])
		}
		if pathAdjacent(text, start, end) {
			rebuilt.WriteString(text[start:end])
		} else {
			rebuilt.WriteString("<ts>")
		}
		last = end
	}
	if last < len(text) {
		rebuilt.WriteString(text[last:])
	}
	return rebuilt.String()
}

// pathAdjacent reports whether the slice borders a filename-joining
// separator on either side.
func pathAdjacent(text string, start, end int) bool {
	border := func(r rune) bool {
		return r == '/' || r == '.' || r == '-' || r == '_'
	}
	if start > 0 {
		if prev, _ := utf8.DecodeLastRuneInString(text[:start]); border(prev) {
			return true
		}
	}
	if end < len(text) {
		if next, _ := utf8.DecodeRuneInString(text[end:]); border(next) {
			return true
		}
	}
	return false
}

// configFingerprint versions the cache against the effective configuration.
// The API key only ever feeds the hash; it never leaves this function. This
// is the content layer of cache versioning; every successful settings save
// additionally calls invalidate() so even a save that lands on identical
// content (an autoApprove round trip, say) drops cached verdicts.
func configFingerprint(config *jsoncompat.Object) string {
	input := jsoncompat.NewObject()
	input.Set("policy", config.MustGet("policy"))
	input.Set("threshold", config.MustGet("threshold"))
	input.Set("autoApprove", config.MustGet("autoApprove"))
	input.Set("model", config.MustGet("model"))
	input.Set("baseUrl", config.MustGet("baseUrl"))
	input.Set("apiKey", config.MustGet("apiKey"))
	sum := sha256.Sum256(jsoncompat.EncodeCompact(input))
	return hex.EncodeToString(sum[:])[:16]
}

// invalidate drops every cached verdict; the next lookup re-primes the
// fingerprint. Called after each successful settings save.
func (c *approvalCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fingerprint = ""
	c.sessions = map[string]*sessionCache{}
	c.sessionOrder = nil
}

func cacheKey(method, subject string) string { return method + "\x00" + subject }

func sessionKey(agent, sessionID string) string { return agent + "\x00" + sessionID }

// lookup returns the cached verdict for the key, rebuilding the whole cache
// first when the configuration fingerprint moved. It never creates sessions:
// misses must not grow the table.
func (c *approvalCache) lookup(fingerprint, agent, sessionID, method, subject string) (cacheEntry, bool) {
	if subject == "" {
		return cacheEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fingerprint != fingerprint {
		c.fingerprint = fingerprint
		c.sessions = map[string]*sessionCache{}
		c.sessionOrder = nil
		return cacheEntry{}, false
	}
	session := c.sessions[sessionKey(agent, sessionID)]
	if session == nil {
		return cacheEntry{}, false
	}
	entry, ok := session.entries[cacheKey(method, subject)]
	return entry, ok
}

// store records an approved verdict — but only when the cache still carries
// the fingerprint the evaluation ran under: a settings save that landed
// mid-assessment invalidates the verdict for caching purposes (fix for the
// stale-fingerprint race).
func (c *approvalCache) store(fingerprint, agent, sessionID, method, subject string, d Decision) {
	if subject == "" || !d.Approved {
		return
	}
	if d.Recommend != "accept_once" && d.Recommend != "accept_session" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fingerprint != fingerprint {
		return
	}
	key := sessionKey(agent, sessionID)
	session := c.sessions[key]
	if session == nil {
		session = &sessionCache{entries: map[string]cacheEntry{}}
		if c.sessions == nil {
			c.sessions = map[string]*sessionCache{}
		}
		c.sessions[key] = session
		c.sessionOrder = append(c.sessionOrder, key)
		for len(c.sessionOrder) > cacheMaxSessions {
			oldest := c.sessionOrder[0]
			c.sessionOrder = c.sessionOrder[1:]
			delete(c.sessions, oldest)
		}
	}
	if len(session.entries) >= cachePerSession {
		// Session overflow clears wholesale: keeps the bound simple and the
		// memory flat instead of tracking per-entry age.
		session.entries = map[string]cacheEntry{}
	}
	categories := make(map[string]float64, len(d.Categories))
	for name, probability := range d.Categories {
		categories[name] = probability
	}
	session.entries[cacheKey(method, subject)] = cacheEntry{
		categories:  categories,
		negativeSum: d.NegativeSum,
		threshold:   d.Threshold,
		reversible:  d.Reversible,
		confidence:  d.Confidence,
		recommend:   d.Recommend,
		topCategory: d.TopCategory,
	}
}
