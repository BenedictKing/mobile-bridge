// Package jev owns the gateway's persisted Jev settings surface.
// Assessment execution stays optional; the HTTP contract must still match
// the Python gateway when no API key is configured.
package jev

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const defaultBaseURL = "https://api.typesafe.ai"

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Settings is safe for concurrent HTTP reads and updates.
type Settings struct {
	path   string
	mu     sync.Mutex
	values *jsoncompat.Object
}

func New(path string) *Settings {
	s := &Settings{path: filepath.Clean(path)}
	s.values = s.load()
	return s
}

func (s *Settings) load() *jsoncompat.Object {
	value := jsoncompat.NewObject()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return value
	}
	decoded, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return value
	}
	object, ok := decoded.(*jsoncompat.Object)
	if !ok {
		return value
	}
	for _, key := range []string{"apiKey", "baseUrl", "model", "proxy", "autoApprove", "autoContinue"} {
		if item, exists := object.Get(key); exists {
			switch key {
			case "apiKey", "baseUrl", "model", "proxy":
				if _, ok := item.(string); ok {
					value.Set(key, item)
				}
			default:
				if _, ok := item.(bool); ok {
					value.Set(key, item)
				}
			}
		}
	}
	return value
}

func (s *Settings) effectiveLocked() *jsoncompat.Object {
	effective := jsoncompat.NewObject()
	for _, field := range []struct{ key, env string }{
		{"apiKey", "TYPESAFE_API_KEY"}, {"baseUrl", "TYPESAFE_BASE_URL"},
		{"model", "JEV_MODEL"}, {"proxy", "JEV_PROXY"},
	} {
		value, exists := s.values.Get(field.key)
		if !exists {
			value = os.Getenv(field.env)
		}
		if text, ok := value.(string); ok {
			effective.Set(field.key, text)
		} else {
			effective.Set(field.key, "")
		}
	}
	if effective.MustGet("baseUrl") == "" {
		effective.Set("baseUrl", defaultBaseURL)
	}
	if effective.MustGet("model") == "" {
		effective.Set("model", "jev-latest")
	}
	if effective.MustGet("proxy") == nil {
		effective.Set("proxy", "")
	}
	for _, field := range []string{"autoApprove", "autoContinue"} {
		value, _ := s.values.Get(field)
		flag, _ := value.(bool)
		effective.Set(field, flag)
	}
	return effective
}

// Public returns the client-safe settings view. API keys are never returned
// in full.
func (s *Settings) Public() *jsoncompat.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publicLocked()
}

// Update validates and persists a settings form submission.
func (s *Settings) Update(input *jsoncompat.Object) (*jsoncompat.Object, error) {
	if input == nil {
		return nil, errors.New("Jev 设置格式不正确")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	saved := jsoncompat.Clone(s.values).(*jsoncompat.Object)
	parsed := jsoncompat.NewObject()
	if clear, _ := input.MustGet("clearApiKey").(bool); clear {
		parsed.Set("apiKey", "")
	} else if raw, exists := input.Get("apiKey"); exists {
		text, ok := raw.(string)
		if !ok {
			return nil, errors.New("API Key 格式不正确")
		}
		text = strings.TrimSpace(text)
		if text != "" {
			if len([]rune(text)) < 8 || len([]rune(text)) > 4096 || strings.IndexFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }) >= 0 {
				return nil, errors.New("API Key 格式不正确")
			}
			parsed.Set("apiKey", text)
		} else if value, exists := saved.Get("apiKey"); exists {
			parsed.Set("apiKey", value)
		}
	} else if value, exists := saved.Get("apiKey"); exists {
		parsed.Set("apiKey", value)
	}

	base, err := stringField(input, "baseUrl")
	if err != nil {
		return nil, err
	}
	if base != "" {
		parsedURL, parseErr := url.Parse(base)
		if parseErr != nil || parsedURL.Hostname() == "" || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") {
			return nil, errors.New("端点必须是 http(s) 地址")
		}
		if parsedURL.Scheme == "http" && parsedURL.Hostname() != "localhost" && parsedURL.Hostname() != "127.0.0.1" && parsedURL.Hostname() != "::1" {
			return nil, errors.New("端点请使用 HTTPS；HTTP 仅用于本机测试")
		}
	}
	parsed.Set("baseUrl", base)
	model, err := stringField(input, "model")
	if err != nil {
		return nil, err
	}
	if model != "" && !modelPattern.MatchString(model) {
		return nil, errors.New("模型名格式不正确")
	}
	parsed.Set("model", model)
	if raw, exists := input.Get("proxy"); exists {
		proxy, ok := raw.(string)
		if !ok {
			return nil, errors.New("Jev 设置格式不正确")
		}
		proxy = strings.TrimSpace(proxy)
		if proxy != "" {
			parsedURL, parseErr := url.Parse(proxy)
			if parseErr != nil || parsedURL.Scheme != "http" || parsedURL.Hostname() == "" || parsedURL.Port() == "" || parsedURL.User != nil || parsedURL.Path != "" || parsedURL.RawQuery != "" || parsedURL.Fragment != "" {
				return nil, errors.New("代理必须是 http://主机:端口 形式")
			}
		}
		parsed.Set("proxy", proxy)
	} else if value, exists := saved.Get("proxy"); exists {
		parsed.Set("proxy", value)
	}
	for _, field := range []string{"autoApprove", "autoContinue"} {
		if raw, exists := input.Get(field); exists {
			flag, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s 设置格式不正确", map[string]string{"autoApprove": "自动审核", "autoContinue": "自动继续"}[field])
			}
			parsed.Set(field, flag)
		} else if value, exists := saved.Get(field); exists {
			parsed.Set(field, value)
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.path, append(jsoncompat.EncodePythonIndent2(parsed), '\n'), 0o600); err != nil {
		return nil, err
	}
	s.values = parsed
	return s.publicLocked(), nil
}

func (s *Settings) publicLocked() *jsoncompat.Object {
	effective := s.effectiveLocked()
	key, _ := effective.MustGet("apiKey").(string)
	masked := ""
	if len([]rune(key)) > 8 {
		runes := []rune(key)
		masked = string(runes[:3]) + "…" + string(runes[len(runes)-4:])
	} else if key != "" {
		masked = "已配置"
	}
	result := jsoncompat.NewObject()
	result.Set("configured", key != "")
	result.Set("apiKeyMasked", masked)
	result.Set("baseUrl", effective.MustGet("baseUrl"))
	result.Set("model", effective.MustGet("model"))
	result.Set("proxy", effective.MustGet("proxy"))
	result.Set("autoApprove", effective.MustGet("autoApprove"))
	result.Set("autoContinue", effective.MustGet("autoContinue"))
	return result
}

func stringField(input *jsoncompat.Object, key string) (string, error) {
	raw := input.MustGet(key)
	if raw == nil {
		return "", nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", errors.New("Jev 设置格式不正确")
	}
	return strings.TrimSpace(value), nil
}
