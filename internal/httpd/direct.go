package httpd

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	directGrantTTL      = time.Minute
	directFailureWindow = 5 * time.Minute
	directFailureLimit  = 8
	directGrantCapacity = 64
)

type directGrant struct {
	publicOrigin string
	directOrigin string
	expires      time.Time
}

type directGrantStore struct {
	mu       sync.Mutex
	entries  map[[32]byte]directGrant
	order    [][32]byte
	failures map[string][]time.Time
	now      func() time.Time
}

func newDirectGrantStore() *directGrantStore {
	return &directGrantStore{entries: map[[32]byte]directGrant{}, failures: map[string][]time.Time{}, now: time.Now}
}

func (s *directGrantStore) cleanLocked(now time.Time) {
	kept := s.order[:0]
	for _, key := range s.order {
		if entry, ok := s.entries[key]; ok && entry.expires.After(now) {
			kept = append(kept, key)
		} else {
			delete(s.entries, key)
		}
	}
	s.order = kept
	for address, stamps := range s.failures {
		recent := stamps[:0]
		for _, stamp := range stamps {
			if now.Sub(stamp) < directFailureWindow {
				recent = append(recent, stamp)
			}
		}
		if len(recent) == 0 {
			delete(s.failures, address)
		} else {
			s.failures[address] = recent
		}
	}
}

func (s *directGrantStore) issue(publicOrigin, directOrigin string) (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buffer)
	digest := sha256.Sum256([]byte(token))
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanLocked(now)
	if len(s.order) >= directGrantCapacity {
		delete(s.entries, s.order[0])
		s.order = s.order[1:]
	}
	s.entries[digest] = directGrant{publicOrigin: publicOrigin, directOrigin: directOrigin, expires: now.Add(directGrantTTL)}
	s.order = append(s.order, digest)
	return token, nil
}

func (s *directGrantStore) exchange(token, publicOrigin, directOrigin, address string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanLocked(now)
	if len(s.failures[address]) >= directFailureLimit {
		return false
	}
	digest := sha256.Sum256([]byte(token))
	entry, ok := s.entries[digest]
	if ok && entry.publicOrigin == publicOrigin && entry.directOrigin == directOrigin && entry.expires.After(now) {
		delete(s.entries, digest)
		for i, key := range s.order {
			if key == digest {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
		delete(s.failures, address)
		return true
	}
	s.failures[address] = append(s.failures[address], now)
	return false
}

func directHTTPSOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", errors.New("直连地址必须是 HTTPS origin")
	}
	return "https://" + parsed.Host, nil
}

// AddDirect registers the only cross-origin pair allowed to exchange a direct
// session. The public origin remains the page origin; the direct origin is a
// separate HTTPS listener on the same gateway.
func (s *Server) AddDirect(publicValue, directValue string) error {
	publicOrigin, err := directHTTPSOrigin(publicValue)
	if err != nil {
		return err
	}
	directOrigin, err := directHTTPSOrigin(directValue)
	if err != nil {
		return err
	}
	if publicOrigin == directOrigin {
		return errors.New("直连地址必须与公网地址不同")
	}
	publicURL, _ := url.Parse(publicOrigin)
	directURL, _ := url.Parse(directOrigin)
	s.DirectByPublic[publicURL.Host] = directOrigin
	s.PublicByDirect[directURL.Host] = publicOrigin
	s.Hosts[publicURL.Host] = true
	s.Hosts[directURL.Host] = true
	s.Origins[publicOrigin] = true
	s.SecureHosts[publicURL.Host] = true
	s.SecureHosts[directURL.Host] = true
	return nil
}

func (h *handler) directPublicOrigin() string {
	return h.server.PublicByDirect[h.request.Host]
}

func (h *handler) directCandidate() string {
	return h.server.DirectByPublic[h.request.Host]
}

func (h *handler) directOrigin() string {
	if h.directPublicOrigin() == "" {
		return ""
	}
	return "https://" + h.request.Host
}

func (h *handler) contentSecurityPolicy() string {
	directOrigin := h.directCandidate()
	if directOrigin == "" {
		return contentSecurityPolicy
	}
	policy := strings.Replace(contentSecurityPolicy, "connect-src 'self'", "connect-src 'self' "+directOrigin, 1)
	return strings.Replace(policy, "img-src 'self' data:", "img-src 'self' data: "+directOrigin, 1)
}

func (h *handler) decorateDirectResponse() {
	directOrigin := h.directOrigin()
	if directOrigin == "" {
		return
	}
	header := h.writer.Header()
	header.Add("Vary", "Origin")
	if h.request.Header.Get("Origin") == h.directPublicOrigin() {
		header.Set("Access-Control-Allow-Origin", h.directPublicOrigin())
		header.Set("Access-Control-Allow-Credentials", "true")
	}
}

func (h *handler) handleDirectPreflight() bool {
	if h.request.Method != http.MethodOptions || h.directOrigin() == "" {
		return false
	}
	if h.request.Header.Get("Origin") != h.directPublicOrigin() {
		h.fail(http.StatusForbidden, errorBody("不允许跨站请求"))
		return true
	}
	method := h.request.Header.Get("Access-Control-Request-Method")
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete:
	default:
		h.fail(http.StatusForbidden, errorBody("不允许的请求方法"))
		return true
	}
	for _, name := range strings.Split(h.request.Header.Get("Access-Control-Request-Headers"), ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || name == "content-type" || name == "x-csrf-token" || name == "last-event-id" {
			continue
		}
		h.fail(http.StatusForbidden, errorBody("不允许的请求头"))
		return true
	}
	header := h.writer.Header()
	header.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE")
	header.Set("Access-Control-Allow-Headers", "Content-Type, X-CSRF-Token, Last-Event-ID")
	header.Set("Access-Control-Max-Age", "600")
	if h.request.Header.Get("Access-Control-Request-Private-Network") == "true" {
		header.Set("Access-Control-Allow-Private-Network", "true")
	}
	h.output(http.StatusNoContent, []byte{}, contentTypeJSON, "")
	return true
}

func (h *handler) handleDirectGrant() {
	directOrigin := h.directCandidate()
	if directOrigin == "" || h.directPublicOrigin() != "" {
		h.fail(http.StatusNotFound, errorBody("未配置局域网直连"))
		return
	}
	publicOrigin := "https://" + h.request.Host
	token, err := h.server.directGrants.issue(publicOrigin, directOrigin)
	if err != nil {
		h.fail(http.StatusInternalServerError, errorBody("无法创建直连凭据"))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("direct", directOrigin)
	result.Set("token", token)
	h.output(http.StatusOK, result, contentTypeJSON, "")
}

func (h *handler) handleDirectExchange() {
	directOrigin := h.directOrigin()
	publicOrigin := h.directPublicOrigin()
	if directOrigin == "" || publicOrigin == "" || h.request.Header.Get("Origin") != publicOrigin {
		h.fail(http.StatusForbidden, errorBody("不允许的请求来源"))
		return
	}
	value, err := h.readJSON()
	if err != nil {
		h.fail(http.StatusBadRequest, errorBody(err.Error()))
		return
	}
	token, _ := value.(*jsoncompat.Object).MustGet("token").(string)
	if len(token) != 43 || !h.server.directGrants.exchange(token, publicOrigin, "https://"+h.request.Host, clientIP(h.request)) {
		h.fail(http.StatusForbidden, errorBody("直连凭据已失效，请重试"))
		return
	}
	sessionToken, session, err := h.server.Auth.NewSessionWithClient(h.client(), h.request.UserAgent(), true)
	if err != nil {
		h.fail(http.StatusForbidden, errorBody(err.Error()))
		return
	}
	result := jsoncompat.NewObject()
	result.Set("csrf", session.MustGet("csrf"))
	result.Set("direct", directOrigin)
	h.output(http.StatusOK, result, contentTypeJSON, h.cookieValue(sessionToken))
}
