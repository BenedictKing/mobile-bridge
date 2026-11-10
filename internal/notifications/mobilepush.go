// MobilePush is the opt-in native push registry (bridge/features/
// notifications/push.py port): device tokens belong to one login, each device
// drains the inbox feed on its own cursor with per-device exponential
// backoff, invalid tokens are removed, and private provider configuration
// never comes from phone requests (it lives in mobile-push.json).
package notifications

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// InvalidPushToken marks a device token the provider no longer knows
// (APNs 410/BadDeviceToken/Unregistered, FCM UNREGISTERED).
type InvalidPushToken struct{ Reason string }

func (e *InvalidPushToken) Error() string { return "推送设备标识已失效：" + e.Reason }

// PushSender delivers one event to one device through a configured provider.
type PushSender interface {
	Send(config map[string]any, device *PushDevice, event *MobileEventView) error
}

// PushDevice is one registered push target (mobile-push-devices.json value).
type PushDevice struct {
	Owner      string  `json:"owner"` // session key the registration belongs to
	Token      string  `json:"token"`
	Kind       string  `json:"kind"` // apns | fcm | activity
	SourceIcon bool    `json:"sourceIcon"`
	Origin     string  `json:"origin"`
	Thread     string  `json:"thread,omitempty"`
	Host       string  `json:"host,omitempty"`
	Updated    float64 `json:"updated"`
	Cursor     int64   `json:"cursor"`
	StreamID   string  `json:"streamId"`
	Next       float64 `json:"next"`
	Attempts   int     `json:"attempts,omitempty"`
	Error      string  `json:"error,omitempty"`
	Phase      string  `json:"phase,omitempty"`
	StateSent  float64 `json:"stateSent,omitempty"`
	LastAccept float64 `json:"lastAccepted,omitempty"`
}

// MobilePush is safe for concurrent use. SessionValid keys registered owners
// (session keys) back to live sessions; nil disables delivery removals.
type MobilePush struct {
	dir          string
	feed         *MobileEvents
	sender       PushSender
	sessionValid func(string) bool
	now          func() float64

	mu        sync.Mutex
	devices   map[string]*PushDevice
	activity  map[[2]string]*activityState
	pushHooks []func()
}

type activityState struct {
	Phase     string
	CreatedAt float64
}

const pushDeviceLimit = 100

var (
	apnsTokenPattern = regexp.MustCompile(`^[0-9a-fA-F]{32,512}$`)
	// Go's regexp caps repeat counts at 1000; the 4096 upper bound is a
	// length check in Register (fcmTokenLength).
	fcmTokenPattern   = regexp.MustCompile(`^[A-Za-z0-9_:.-]{20,}$`)
	uuidTextPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	providerIDPattern = regexp.MustCompile(`^[a-z0-9-]+$`)
)

// NewMobilePush loads (or creates) <dataDir>/mobile-push-devices.json.
func NewMobilePush(dataDir string, feed *MobileEvents) *MobilePush {
	push := &MobilePush{dir: dataDir, feed: feed, now: func() float64 {
		return float64(time.Now().UnixNano()) / 1e9
	}, devices: map[string]*PushDevice{}, activity: map[[2]string]*activityState{}}
	push.load()
	return push
}

// Sender overrides the default provider transport (tests inject fakes).
func (p *MobilePush) Sender(sender PushSender) { p.sender = sender }

// SessionValidator wires the owner validity check (auth session keys).
func (p *MobilePush) SessionValidator(valid func(string) bool) { p.sessionValid = valid }

func (p *MobilePush) path() string {
	return p.dir + string(os.PathSeparator) + "mobile-push-devices.json"
}

func (p *MobilePush) configPath() string {
	return p.dir + string(os.PathSeparator) + "mobile-push.json"
}

// Config reads mobile-push.json (server-side provider credentials).
func (p *MobilePush) Config() map[string]any {
	raw, err := os.ReadFile(p.configPath())
	if err != nil {
		return map[string]any{}
	}
	var config map[string]any
	if err := json.Unmarshal(raw, &config); err != nil {
		return map[string]any{}
	}
	return config
}

// Status mirrors MobilePush.status: which providers are configured.
func (p *MobilePush) Status() map[string]any {
	config := p.Config()
	apns, _ := config["apns"].(map[string]any)
	fcm, _ := config["fcm"].(map[string]any)
	return map[string]any{"apns": apns != nil, "fcm": fcm != nil,
		"configured": apns != nil || fcm != nil}
}

func (p *MobilePush) providerConfigured(provider string) bool {
	enabled, _ := p.Status()[provider].(bool)
	return enabled
}

func (p *MobilePush) load() {
	raw, err := os.ReadFile(p.path())
	if err != nil {
		return
	}
	var devices map[string]*PushDevice
	if err := json.Unmarshal(raw, &devices); err != nil || devices == nil {
		return
	}
	p.devices = devices
}

func (p *MobilePush) saveLocked() error {
	raw, err := json.MarshalIndent(p.devices, "", "  ")
	if err != nil {
		return fmt.Errorf("推送设备编码失败: %w", err)
	}
	tmp := p.path() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("推送设备写入失败: %w", err)
	}
	if err := os.Rename(tmp, p.path()); err != nil {
		return fmt.Errorf("推送设备替换失败: %w", err)
	}
	return nil
}

// Register mirrors MobilePush.register. owner is the registering session's
// key; origin must be a trusted origin the caller already validated.
func (p *MobilePush) Register(body map[string]any, owner, origin string) (map[string]any, error) {
	if owner == "" {
		return nil, errors.New("登录已失效，请重新连接电脑")
	}
	identifierText, _ := body["deviceId"].(string)
	if !uuidTextPattern.MatchString(identifierText) {
		return nil, errors.New("推送设备标识格式不正确")
	}
	kind, _ := body["kind"].(string)
	if kind != "apns" && kind != "fcm" && kind != "activity" {
		return nil, errors.New("不支持的推送类型")
	}
	key := pushDeviceKey(owner, identifierText, kind)
	p.mu.Lock()
	defer p.mu.Unlock()
	if enabled, ok := body["enabled"].(bool); ok && !enabled {
		if _, existed := p.devices[key]; existed {
			delete(p.devices, key)
			if err := p.saveLocked(); err != nil {
				return nil, err
			}
		}
		return map[string]any{"registered": false}, nil
	}
	token, _ := body["token"].(string)
	if kind == "fcm" {
		if !fcmTokenPattern.MatchString(token) || len(token) > 4096 {
			return nil, errors.New("推送设备标识格式不正确")
		}
	} else if !apnsTokenPattern.MatchString(token) {
		return nil, errors.New("推送设备标识格式不正确")
	}
	provider := "apns"
	if kind == "fcm" {
		provider = "fcm"
	}
	if !p.providerConfigured(provider) {
		return nil, errors.New("电脑尚未配置系统推送服务")
	}
	thread, host := "", ""
	if kind == "activity" {
		threadText, _ := body["thread"].(string)
		if !uuidTextPattern.MatchString(threadText) {
			return nil, errors.New("实时活动会话标识无效")
		}
		thread = threadText
		host, _ = body["host"].(string)
		if host == "" {
			host = "local"
		}
		if len(host) > 256 {
			return nil, errors.New("主机标识无效")
		}
	}
	if _, exists := p.devices[key]; !exists && len(p.devices) >= pushDeviceLimit {
		return nil, errors.New("推送设备数量已达上限")
	}
	// A token belongs to one login. Re-registration replaces an older
	// session's device with the same token.
	for old, device := range p.devices {
		if old != key && device.Token == token {
			delete(p.devices, old)
		}
	}
	previous := p.devices[key]
	if previous == nil {
		previous = &PushDevice{}
	}
	feed := p.feed.Read(0, nil)
	if previous.StreamID != feed.StreamID || (kind == "activity" && previous.Token != token) {
		previous = &PushDevice{Cursor: previous.Cursor}
	}
	device := &PushDevice{Owner: owner, Token: token, Kind: kind,
		SourceIcon: body["sourceIcon"] == true, Origin: origin, Thread: thread, Host: host,
		Updated: p.now(), Cursor: previous.Cursor, StreamID: feed.StreamID, Next: 0}
	p.devices[key] = device
	if err := p.saveLocked(); err != nil {
		return nil, err
	}
	return map[string]any{"registered": true}, nil
}

func pushDeviceKey(owner, identifier, kind string) string {
	sum := sha256.Sum256([]byte(owner + identifier + kind))
	return hex.EncodeToString(sum[:])
}

// UpdateActivity records the latest Live Activity phase for a thread so the
// drain loop can coalesce updates (30s phase change window).
func (p *MobilePush) UpdateActivity(host, threadID, phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := [2]string{host, threadID}
	for _, device := range p.devices {
		if device.Kind == "activity" && device.Host == host && device.Thread == threadID {
			p.activity[key] = &activityState{Phase: phase, CreatedAt: p.now()}
			return
		}
	}
}

// PushBackoff mirrors the shared retry curve: min(300, 5*2^attempts) seconds.
func PushBackoff(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	if attempts > 6 {
		attempts = 6
	}
	return time.Duration(min(300, 5<<attempts)) * time.Second
}

// Drain walks every device once: owners whose session died are removed, each
// device reads the feed from its own cursor, and a slow or failing device
// never holds up the others (its own next/attempts bookkeeping only).
func (p *MobilePush) Drain(enabled func() bool, allowed func(*MobileEvent) bool) {
	if enabled != nil && !enabled() {
		return
	}
	config := p.Config()
	sender := p.sender
	if sender == nil {
		sender = &ProviderPushSender{}
	}
	p.mu.Lock()
	snapshot := make([]struct {
		key    string
		device *PushDevice
	}, 0, len(p.devices))
	for key, device := range p.devices {
		copy := *device
		snapshot = append(snapshot, struct {
			key    string
			device *PushDevice
		}{key: key, device: &copy})
	}
	p.mu.Unlock()
	for _, entry := range snapshot {
		key, device := entry.key, entry.device
		remove := p.sessionValid != nil && !p.sessionValid(device.Owner)
		changed := remove
		if !remove && p.now() >= device.Next {
			feed := p.feed.Read(device.Cursor, allowed)
			if device.StreamID != feed.StreamID {
				device.Cursor = feed.Cursor
				device.StreamID = feed.StreamID
				changed = true
			} else {
				for _, original := range feed.Events {
					view := &MobileEventView{MobileEvent: original.MobileEvent, Provider: original.Provider,
						ClientID: original.ClientID, Unread: original.Unread, StreamID: device.StreamID}
					matches := device.Kind != "activity" ||
						(view.ThreadID == device.Thread && view.Host == device.Host)
					if !matches || (allowed != nil && !allowed(view.MobileEvent)) || p.now()-view.CreatedAt > 3600 {
						device.Cursor = view.Sequence
						changed = true
						continue
					}
					if p.sessionValid != nil && !p.sessionValid(device.Owner) {
						remove, changed = true, true
						break
					}
					if err := sender.Send(config, device, view); err != nil {
						var invalid *InvalidPushToken
						if errors.As(err, &invalid) {
							remove, changed = true, true
							break
						}
						device.Attempts++
						device.Next = p.now() + PushBackoff(device.Attempts).Seconds()
						device.Error = "推送失败，等待重试"
						changed = true
						break
					}
					device.Cursor = view.Sequence
					device.Attempts = 0
					device.Next = 0
					device.Error = ""
					device.LastAccept = p.now()
					changed = true
					if device.Kind == "activity" && view.Kind == "completion" {
						remove = true
						break
					}
				}
			}
		}
		if !remove && device.Kind == "activity" && p.now() >= device.Next {
			p.mu.Lock()
			state := p.activity[[2]string{device.Host, device.Thread}]
			p.mu.Unlock()
			if state != nil && p.now()-state.CreatedAt < 30 &&
				(state.Phase != device.Phase || p.now()-device.StateSent > 120) {
				event := &MobileEventView{MobileEvent: &MobileEvent{ID: pushDeviceKey(device.Owner, device.Thread, device.Kind),
					Kind: "state", Host: device.Host, ThreadID: device.Thread, CreatedAt: state.CreatedAt,
					Phase: state.Phase},
					Provider: "codex"}
				if (p.sessionValid == nil || p.sessionValid(device.Owner)) && (allowed == nil || allowed(event.MobileEvent)) {
					if err := sender.Send(config, device, event); err != nil {
						var invalid *InvalidPushToken
						if errors.As(err, &invalid) {
							remove = true
						} else {
							device.Next = p.now() + 60
							device.Error = "实时活动更新失败，等待重试"
						}
					} else {
						device.Phase = state.Phase
						device.StateSent = p.now()
						device.Next = 0
						device.Error = ""
					}
					changed = true
				}
			}
		}
		if !changed {
			continue
		}
		p.commitDevice(key, device, remove)
	}
}

// commitDevice writes one drained device back under its registry key. The
// updated stamp guards against a concurrent re-registration replacing the
// record mid-drain (upstream's optimistic check).
func (p *MobilePush) commitDevice(key string, device *PushDevice, remove bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.devices[key]
	if !ok || current.Updated != device.Updated {
		return
	}
	if remove {
		delete(p.devices, key)
	} else {
		p.devices[key] = device
	}
	if err := p.saveLocked(); err != nil {
		log.Printf("推送设备落盘失败: %v", err)
	}
}

// StartWorker drains on an interval until Stop closes. Wake pushes an extra
// tick so a fresh event reaches devices without waiting a full interval.
func (p *MobilePush) StartWorker(interval time.Duration, enabled func() bool, allowed func(*MobileEvent) bool) (stop chan struct{}, done chan struct{}) {
	if interval <= 0 {
		interval = time.Minute
	}
	stop, done = make(chan struct{}), make(chan struct{})
	p.mu.Lock()
	waker := make(chan struct{}, 1)
	p.pushHooks = append(p.pushHooks, func() {
		select {
		case waker <- struct{}{}:
		default:
		}
	})
	p.mu.Unlock()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			p.Drain(enabled, allowed)
			select {
			case <-ticker.C:
			case <-waker:
			case <-stop:
				return
			}
		}
	}()
	return stop, done
}

// Wake fires the workers' immediate-drain hook (called on Append).
func (p *MobilePush) Wake() {
	p.mu.Lock()
	hooks := append([]func(){}, p.pushHooks...)
	p.mu.Unlock()
	for _, hook := range hooks {
		hook()
	}
}

// DevicesJSON exposes the registry for diagnostics/tests.
func (p *MobilePush) DevicesJSON() map[string]*PushDevice {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*PushDevice, len(p.devices))
	for key, device := range p.devices {
		copy := *device
		out[key] = &copy
	}
	return out
}

// MobileInboxEnabled reads the companion inbox switch from notifications.json.
// It overlays the settings file instead of extending the parity-tested
// defaults list: absent means enabled (upstream DEFAULTS ships mobileEnabled
// true), so fresh gateways deliver to the inbox without configuration.
func MobileInboxEnabled(dataDir string) bool {
	raw, err := os.ReadFile(settingsPath(dataDir))
	if err != nil {
		return true
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return true
	}
	stored := objectOf(value)
	if stored == nil {
		return true
	}
	if enabled, ok := stored.MustGet("mobileEnabled").(bool); ok {
		return enabled
	}
	return true
}

// SetMobileInboxEnabled overlays the switch into notifications.json, keeping
// every other stored field byte-stable.
func SetMobileInboxEnabled(dataDir string, enabled bool) error {
	stored := jsoncompat.NewObject()
	if raw, err := os.ReadFile(settingsPath(dataDir)); err == nil {
		if value, err := jsoncompat.Unmarshal(raw); err == nil {
			if existing := objectOf(value); existing != nil {
				stored = existing
			}
		}
	}
	stored.Set("mobileEnabled", enabled)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(settingsPath(dataDir), append(jsoncompat.EncodePythonIndent2(stored), '\n'), 0o600)
}

// sortedDeviceKeys keeps persistence deterministic.
func sortedDeviceKeys(devices map[string]*PushDevice) []string {
	keys := make([]string, 0, len(devices))
	for key := range devices {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
