// Package notifications is the Go port of bridge/notifications.py's pure
// layer: push URL validation, settings persistence with token preservation,
// and channel destination derivation. Parity: conformance/notifications
// goldens.
package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

var defaults = [][2]any{
	{"enabled", false}, {"server", "https://ntfy.sh"}, {"topic", ""}, {"token", ""},
	{"barkEnabled", false}, {"barkServer", "https://api.day.app"}, {"barkKey", ""},
	{"clickBase", ""}, {"includeTitle", false}, {"addressEnabled", true},
	{"addressName", ""}, {"securityEnabled", true},
}

// FieldError mirrors bridge.validation.FieldError.
type FieldError struct {
	Message string
	Field   string
}

func (e *FieldError) Error() string { return e.Message }

// ErrorKind mirrors the Python exception class names.
func ErrorKind(err error) string {
	var field *FieldError
	if errors.As(err, &field) {
		return "FieldError"
	}
	return "ValueError"
}

func fieldOf(err error) string {
	var field *FieldError
	if errors.As(err, &field) {
		return field.Field
	}
	return ""
}

// Describe renders the oracle's error envelope.
func Describe(err error) *jsoncompat.Object {
	envelope := jsoncompat.NewObject()
	envelope.Set("$error", ErrorKind(err)+": "+err.Error())
	if field := fieldOf(err); field != "" {
		envelope.Set("field", field)
	}
	return envelope
}

// ValidURL mirrors valid_url: http(s) only, no credentials/query/fragment,
// HTTP restricted to loopback hosts, trailing slashes trimmed.
func ValidURL(value string, allowPath bool) (string, error) {
	// Python's urlsplit tolerates surrounding whitespace; the returned value
	// keeps the caller's original form (only trailing slashes are trimmed).
	parsed, err := url.Parse(strings.TrimSpace(value))
	invalid := errors.New("请输入完整的 HTTP/HTTPS 地址，不含账号、查询参数或片段")
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" || (!allowPath && parsed.Path != "" && parsed.Path != "/") {
		return "", invalid
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" &&
		parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		return "", errors.New("推送服务请使用 HTTPS；HTTP 仅用于本机测试")
	}
	return strings.TrimRight(value, "/"), nil
}

func settingsPath(dataDir string) string {
	return filepath.Join(dataDir, "notifications.json")
}

// ReadSettings mirrors settings(): defaults overlaid with the stored file.
func ReadSettings(dataDir string) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, pair := range defaults {
		result.Set(pair[0].(string), pair[1])
	}
	if raw, err := os.ReadFile(settingsPath(dataDir)); err == nil {
		if value, err := jsoncompat.Unmarshal(raw); err == nil {
			if stored := objectOf(value); stored != nil {
				for _, key := range stored.Keys() {
					entry, _ := stored.Get(key)
					result.Set(key, entry)
				}
			}
		}
	}
	return result
}

// SaveSettings mirrors save_settings: merge into DEFAULTS keys, validate in
// order, preserve blank tokens only for the same server.
func SaveSettings(dataDir string, value *jsoncompat.Object) (*jsoncompat.Object, error) {
	prior := ReadSettings(dataDir)
	result := jsoncompat.NewObject()
	for _, pair := range defaults {
		key := pair[0].(string)
		merged := pair[1]
		if previous, ok := prior.Get(key); ok {
			merged = previous
		}
		if incoming, ok := value.Get(key); ok {
			merged = incoming
		}
		result.Set(key, merged)
	}
	server, _ := result.MustGet("server").(string)
	if trimmed, err := ValidURL(server, true); err != nil {
		return nil, &FieldError{Message: err.Error(), Field: "ntfy-server"}
	} else {
		result.Set("server", trimmed)
	}
	barkServer, _ := result.MustGet("barkServer").(string)
	if trimmed, err := ValidURL(barkServer, true); err != nil {
		return nil, &FieldError{Message: err.Error(), Field: "bark-server"}
	} else {
		result.Set("barkServer", trimmed)
	}
	for _, key := range [...]string{"enabled", "barkEnabled", "includeTitle",
		"addressEnabled", "securityEnabled"} {
		if _, ok := result.MustGet(key).(bool); !ok {
			return nil, errors.New("通知开关格式不正确")
		}
	}
	addressName, _ := result.MustGet("addressName").(string)
	if len([]rune(addressName)) > 80 || !isPrintableLine(addressName) {
		return nil, &FieldError{Message: "网关名称应为不超过 80 字的单行文本",
			Field: "address-name"}
	}
	topic, _ := result.MustGet("topic").(string)
	topicPattern := regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	if topic != "" && !topicPattern.MatchString(topic) {
		return nil, &FieldError{Message: "ntfy 主题只允许 1–64 位字母、数字、下划线和短横线",
			Field: "ntfy-topic"}
	}
	enabled, _ := result.MustGet("enabled").(bool)
	if enabled && topic == "" {
		return nil, &FieldError{Message: "开启通知前请填写 ntfy 主题", Field: "ntfy-topic"}
	}
	clickBase, _ := result.MustGet("clickBase").(string)
	if clickBase != "" {
		if _, err := ValidURL(clickBase, true); err != nil {
			return nil, &FieldError{Message: "通知跳转地址格式不正确", Field: "click-base"}
		}
		result.Set("clickBase", strings.TrimRight(clickBase, "/"))
	}
	token, _ := result.MustGet("token").(string)
	if len(token) > 2000 || strings.ContainsAny(token, "\n\r") {
		return nil, &FieldError{Message: "ntfy Token 格式不正确", Field: "ntfy-token"}
	}
	if blank, _ := value.MustGet("token").(string); blank == "" {
		if _, exists := value.Get("token"); !exists || true {
			clear, _ := value.MustGet("clearToken").(bool)
			priorServer, _ := prior.MustGet("server").(string)
			priorToken, _ := prior.MustGet("token").(string)
			if clear || server != priorServer {
				result.Set("token", "")
			} else {
				result.Set("token", priorToken)
			}
		}
	}
	barkKey, _ := result.MustGet("barkKey").(string)
	if len(barkKey) > 2000 || !isBarkKeySafe(barkKey) {
		return nil, &FieldError{
			Message: "Bark Device Key 格式不正确，请仅填写密钥，不要粘贴完整推送地址",
			Field:   "bark-key"}
	}
	if blank, _ := value.MustGet("barkKey").(string); blank == "" {
		clear, _ := value.MustGet("clearBarkKey").(bool)
		priorBarkServer, _ := prior.MustGet("barkServer").(string)
		priorKey, _ := prior.MustGet("barkKey").(string)
		if clear || barkServer != priorBarkServer {
			result.Set("barkKey", "")
		} else {
			result.Set("barkKey", priorKey)
		}
	}
	barkEnabled, _ := result.MustGet("barkEnabled").(bool)
	barkKeyNow, _ := result.MustGet("barkKey").(string)
	if barkEnabled && barkKeyNow == "" {
		return nil, &FieldError{Message: "开启 Bark 前请填写 Device Key；更换服务地址后需重新填写",
			Field: "bark-key"}
	}
	// write_json: indent=2, ensure_ascii=False, trailing newline.
	payload := append(jsoncompat.EncodePythonIndent2(result), '\n')
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(settingsPath(dataDir), payload, 0o600); err != nil {
		return nil, err
	}
	return result, nil
}

func objectOf(value any) *jsoncompat.Object {
	object, _ := value.(*jsoncompat.Object)
	return object
}

// isPrintableLine mirrors Python's str.isprintable() over the whole string.
func isPrintableLine(text string) bool {
	for _, r := range text {
		if r == ' ' {
			continue
		}
		if !unicode.IsPrint(r) || r == '\u00a0' || isSpaceSeparator(r) {
			return false
		}
	}
	return true
}

func isSpaceSeparator(r rune) bool {
	return unicode.Is(unicode.Zs, r) && r != ' '
}

func isBarkKeySafe(text string) bool {
	for _, r := range text {
		if r == ' ' || strings.ContainsRune("/?#", r) {
			return false
		}
		if r != ' ' && !unicode.IsPrint(r) {
			return false
		}
		if unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// Destination mirrors destination: channel + sha256 of the default-separator
// JSON of [server, credential].
func Destination(config *jsoncompat.Object, channel string) (string, error) {
	var values []any
	switch channel {
	case "ntfy":
		server, _ := config.MustGet("server").(string)
		topic, _ := config.MustGet("topic").(string)
		values = []any{strings.TrimRight(server, "/"), topic}
	case "bark":
		server, _ := config.MustGet("barkServer").(string)
		key, _ := config.MustGet("barkKey").(string)
		values = []any{strings.TrimRight(server, "/"), key}
	default:
		return "", errors.New("未知通知通道")
	}
	sum := sha256.Sum256(jsoncompat.EncodePythonDefault(&jsoncompat.Array{Items: values}))
	return channel + ":" + hex.EncodeToString(sum[:]), nil
}

// Channels mirrors channels(): the destination per enabled channel.
func Channels(config *jsoncompat.Object) *jsoncompat.Object {
	result := jsoncompat.NewObject()
	for _, pair := range [...][2]string{{"ntfy", "enabled"}, {"bark", "barkEnabled"}} {
		if enabled, _ := config.MustGet(pair[1]).(bool); enabled {
			if destination, err := Destination(config, pair[0]); err == nil {
				result.Set(pair[0], destination)
			}
		}
	}
	return result
}
