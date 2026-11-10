package notifications

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// mobilePushFixture builds a feed plus registry sharing one injectable clock;
// configured decides whether mobile-push.json names an apns provider.
func mobilePushFixture(t *testing.T, configured bool) (*MobilePush, *MobileEvents, *float64) {
	t.Helper()
	dir := t.TempDir()
	if configured {
		config := `{"apns": {"teamId": "TEAM", "keyId": "KEY", "keyFile": "/unused.pem"}}`
		if err := os.WriteFile(filepath.Join(dir, "mobile-push.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clock := 1700000000.0
	feed := NewMobileEvents(dir)
	feed.now = func() float64 { return clock }
	push := NewMobilePush(dir, feed)
	push.now = func() float64 { return clock }
	return push, feed, &clock
}

const (
	testDeviceID  = "11111111-2222-4333-8444-555566667777"
	testDeviceID2 = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	testAPNSToken = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6a7b8c9d0e1f2a3b4c5d6a7b8c9d0"
)

func mobileRegisterBody(deviceID, kind, token string) map[string]any {
	return map[string]any{"deviceId": deviceID, "kind": kind, "token": token}
}

func TestMobilePushRegisterValidation(t *testing.T) {
	push, _, _ := mobilePushFixture(t, true)
	badOwner, _, _ := mobilePushFixture(t, true) // separate registry for the owner case

	cases := []struct {
		name  string
		push  *MobilePush
		body  map[string]any
		owner string
		want  string
	}{
		{"空登录", badOwner, mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "", "登录已失效，请重新连接电脑"},
		{"设备标识非 UUID", push, mobileRegisterBody("not-a-uuid", "apns", testAPNSToken), "owner", "推送设备标识格式不正确"},
		{"kind 不支持", push, mobileRegisterBody(testDeviceID, "web", testAPNSToken), "owner", "不支持的推送类型"},
		{"apns token 非 hex", push, mobileRegisterBody(testDeviceID, "apns", "ZZZZ"+strings.Repeat("a", 60)), "owner", "推送设备标识格式不正确"},
		{"apns token 过短", push, mobileRegisterBody(testDeviceID, "apns", "abcd"), "owner", "推送设备标识格式不正确"},
		{"fcm token 过短", push, mobileRegisterBody(testDeviceID, "fcm", "short"), "owner", "推送设备标识格式不正确"},
		{"fcm token 非法字符", push, mobileRegisterBody(testDeviceID, "fcm", "bad token! with spaces and more"), "owner", "推送设备标识格式不正确"},
		{"fcm token 超长", push, mobileRegisterBody(testDeviceID, "fcm", strings.Repeat("t", 4097)), "owner", "推送设备标识格式不正确"},
	}
	for _, item := range cases {
		_, err := item.push.Register(item.body, item.owner, "http://127.0.0.1:8790")
		if err == nil || err.Error() != item.want {
			t.Fatalf("%s err=%v want %q", item.name, err, item.want)
		}
	}

	// An unconfigured provider refuses registration before any write.
	pushNone, _, _ := mobilePushFixture(t, false)
	if _, err := pushNone.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "o"); err == nil ||
		err.Error() != "电脑尚未配置系统推送服务" {
		t.Fatalf("unconfigured err=%v", err)
	}
	if status := pushNone.Status(); status["configured"] != false || status["apns"] != false || status["fcm"] != false {
		t.Fatalf("empty status=%v", status)
	}

	// activity kind validates its thread/host pair.
	if _, err := push.Register(map[string]any{"deviceId": testDeviceID, "kind": "activity",
		"token": testAPNSToken, "thread": "oops"}, "owner", "o"); err == nil || err.Error() != "实时活动会话标识无效" {
		t.Fatalf("activity thread err=%v", err)
	}
	if _, err := push.Register(map[string]any{"deviceId": testDeviceID, "kind": "activity",
		"token": testAPNSToken, "thread": testDeviceID2, "host": strings.Repeat("h", 257)}, "owner", "o"); err == nil ||
		err.Error() != "主机标识无效" {
		t.Fatalf("activity host err=%v", err)
	}
	if len(push.DevicesJSON()) != 0 {
		t.Fatalf("rejected registrations wrote devices: %v", push.DevicesJSON())
	}
}

func TestMobilePushRegisterUnregisterAndStatus(t *testing.T) {
	push, _, _ := mobilePushFixture(t, true)
	if status := push.Status(); status["apns"] != true || status["fcm"] != false || status["configured"] != true {
		t.Fatalf("status=%v", status)
	}

	result, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "http://192.168.1.5:8790")
	if err != nil || result["registered"] != true {
		t.Fatalf("register result=%v err=%v", result, err)
	}
	devices := push.DevicesJSON()
	if len(devices) != 1 {
		t.Fatalf("devices=%d", len(devices))
	}
	for _, device := range devices {
		if device.Owner != "owner" || device.Token != testAPNSToken || device.Kind != "apns" ||
			device.Origin != "http://192.168.1.5:8790" || device.StreamID == "" {
			t.Fatalf("device=%+v", device)
		}
	}

	// The registry file reloads with its devices.
	reloaded := NewMobilePush(filepath.Dir(push.path()), push.feed)
	if len(reloaded.DevicesJSON()) != 1 {
		t.Fatalf("reloaded devices=%d", len(reloaded.DevicesJSON()))
	}

	// enabled:false unregisters.
	off := mobileRegisterBody(testDeviceID, "apns", testAPNSToken)
	off["enabled"] = false
	result, err = push.Register(off, "owner", "http://192.168.1.5:8790")
	if err != nil || result["registered"] != false {
		t.Fatalf("unregister result=%v err=%v", result, err)
	}
	if len(push.DevicesJSON()) != 0 {
		t.Fatalf("devices after unregister=%d", len(push.DevicesJSON()))
	}
}

func TestMobilePushTokenBelongsToOneLogin(t *testing.T) {
	push, _, _ := mobilePushFixture(t, true)
	if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner-1", "o"); err != nil {
		t.Fatal(err)
	}
	if _, err := push.Register(mobileRegisterBody(testDeviceID2, "apns", testAPNSToken), "owner-2", "o"); err != nil {
		t.Fatal(err)
	}
	// The token belongs to one login: the older session's entry is replaced.
	devices := push.DevicesJSON()
	if len(devices) != 1 {
		t.Fatalf("devices=%d want 1", len(devices))
	}
	for _, device := range devices {
		if device.Owner != "owner-2" {
			t.Fatalf("remaining owner=%q", device.Owner)
		}
	}
}

func TestMobilePushDeviceLimit(t *testing.T) {
	push, _, _ := mobilePushFixture(t, true)
	for index := 0; index < pushDeviceLimit; index++ {
		deviceID := fmt.Sprintf("%08x-2222-4333-8444-5555%08d", index, index)
		token := fmt.Sprintf("%063x", index)
		if _, err := push.Register(mobileRegisterBody(deviceID, "apns", token), "owner", "o"); err != nil {
			t.Fatalf("register %d: %v", index, err)
		}
	}
	if _, err := push.Register(mobileRegisterBody(testDeviceID2, "apns", "b"+testAPNSToken[1:]), "owner", "o"); err == nil ||
		err.Error() != "推送设备数量已达上限" {
		t.Fatalf("limit err=%v", err)
	}
}

// mobileFakeSender records every delivery and can fail per event.
type mobileFakeSender struct {
	mu   sync.Mutex
	sent []string // token@sequence
	fail func(device *PushDevice, event *MobileEventView) error
}

func (s *mobileFakeSender) Send(config map[string]any, device *PushDevice, event *MobileEventView) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, device.Token[:8]+"@"+strconv.FormatInt(event.Sequence, 10))
	if s.fail != nil {
		return s.fail(device, event)
	}
	return nil
}

func (s *mobileFakeSender) deliveries() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func TestMobilePushDrainAdvancesPerDeviceCursor(t *testing.T) {
	push, feed, _ := mobilePushFixture(t, true)
	if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner-a", "o"); err != nil {
		t.Fatal(err)
	}
	if _, err := push.Register(mobileRegisterBody(testDeviceID2, "apns", "b"+testAPNSToken[1:]), "owner-b", "o"); err != nil {
		t.Fatal(err)
	}
	_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")
	_ = feed.Append([]string{"e2"}, "t", "h", "标题", "正文", "request", "")

	// Device A behaves as if it had already drained the first event.
	keyA := pushDeviceKey("owner-a", testDeviceID, "apns")
	push.mu.Lock()
	push.devices[keyA].Cursor = 1
	push.mu.Unlock()

	sender := &mobileFakeSender{}
	push.Sender(sender)
	push.Drain(nil, nil)

	delivered := sender.deliveries()
	if len(delivered) != 3 { // A: seq2; B: seq1+seq2
		t.Fatalf("delivered=%v", delivered)
	}
	devices := push.DevicesJSON()
	if device := devices[keyA]; device == nil || device.Cursor != 2 || device.Attempts != 0 || device.Error != "" {
		t.Fatalf("device A=%+v", device)
	}
	for key, device := range devices {
		if device.Cursor != 2 {
			t.Fatalf("device %s cursor=%d", key, device.Cursor)
		}
	}
}

func TestMobilePushBackoffCurve(t *testing.T) {
	cases := []struct {
		attempts int
		seconds  int
	}{{0, 5}, {1, 10}, {2, 20}, {3, 40}, {5, 160}, {6, 300}, {7, 300}, {-1, 5}}
	for _, item := range cases {
		if got := PushBackoff(item.attempts); got.Seconds() != float64(item.seconds) {
			t.Fatalf("PushBackoff(%d)=%v want %ds", item.attempts, got, item.seconds)
		}
	}
}

func TestMobilePushDrainFailureBackoff(t *testing.T) {
	push, feed, clock := mobilePushFixture(t, true)
	if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "o"); err != nil {
		t.Fatal(err)
	}
	_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")

	sender := &mobileFakeSender{fail: func(*PushDevice, *MobileEventView) error {
		return errors.New("网络故障")
	}}
	push.Sender(sender)
	push.Drain(nil, nil)

	key := pushDeviceKey("owner", testDeviceID, "apns")
	device := push.DevicesJSON()[key]
	if device.Attempts != 1 || device.Next != *clock+10 || device.Cursor != 0 ||
		device.Error != "推送失败，等待重试" {
		t.Fatalf("after first failure device=%+v clock=%v", device, *clock)
	}
	if deliveries := sender.deliveries(); len(deliveries) != 1 {
		t.Fatalf("deliveries=%v", deliveries)
	}

	// Inside the backoff window the device is skipped entirely.
	*clock += 5
	push.Drain(nil, nil)
	if device := push.DevicesJSON()[key]; device.Attempts != 1 {
		t.Fatalf("backoff window attempts=%d", device.Attempts)
	}
	if deliveries := sender.deliveries(); len(deliveries) != 1 {
		t.Fatalf("deliveries inside window=%v", deliveries)
	}

	// Past the window the retry fails again with the doubled curve.
	*clock += 6
	push.Drain(nil, nil)
	device = push.DevicesJSON()[key]
	if device.Attempts != 2 || device.Next != *clock+20 || device.Cursor != 0 {
		t.Fatalf("after second failure device=%+v clock=%v", device, *clock)
	}

	// A recovered sender clears the failure bookkeeping.
	sender.fail = nil
	*clock += 21
	push.Drain(nil, nil)
	if device := push.DevicesJSON()[key]; device.Attempts != 0 || device.Next != 0 ||
		device.Error != "" || device.Cursor != 1 {
		t.Fatalf("after recovery device=%+v", device)
	}
}

func TestMobilePushDrainRemovesDevices(t *testing.T) {
	t.Run("失效 token", func(t *testing.T) {
		push, feed, _ := mobilePushFixture(t, true)
		if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "o"); err != nil {
			t.Fatal(err)
		}
		_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")
		push.Sender(&mobileFakeSender{fail: func(*PushDevice, *MobileEventView) error {
			return &InvalidPushToken{Reason: "Unregistered"}
		}})
		push.Drain(nil, nil)
		if devices := push.DevicesJSON(); len(devices) != 0 {
			t.Fatalf("devices after invalid token=%d", len(devices))
		}
	})

	t.Run("会话失效", func(t *testing.T) {
		push, feed, _ := mobilePushFixture(t, true)
		if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "dead-owner", "o"); err != nil {
			t.Fatal(err)
		}
		_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")
		push.SessionValidator(func(key string) bool { return key == "live-owner" })
		sender := &mobileFakeSender{}
		push.Sender(sender)
		push.Drain(nil, nil)
		if devices := push.DevicesJSON(); len(devices) != 0 {
			t.Fatalf("devices after dead owner=%d", len(devices))
		}
		if deliveries := sender.deliveries(); len(deliveries) != 0 {
			t.Fatalf("dead owner still delivered=%v", deliveries)
		}
	})
}

func TestMobilePushDrainSkipsStaleEvents(t *testing.T) {
	push, feed, clock := mobilePushFixture(t, true)
	if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "o"); err != nil {
		t.Fatal(err)
	}
	_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")
	*clock += 7201 // the event is over an hour old

	sender := &mobileFakeSender{}
	push.Sender(sender)
	push.Drain(nil, nil)
	if deliveries := sender.deliveries(); len(deliveries) != 0 {
		t.Fatalf("stale event delivered=%v", deliveries)
	}
	key := pushDeviceKey("owner", testDeviceID, "apns")
	if device := push.DevicesJSON()[key]; device.Cursor != 1 {
		t.Fatalf("cursor after skip=%d", device.Cursor)
	}
}

func TestMobilePushDrainRealignsStream(t *testing.T) {
	push, feed, _ := mobilePushFixture(t, true)
	if _, err := push.Register(mobileRegisterBody(testDeviceID, "apns", testAPNSToken), "owner", "o"); err != nil {
		t.Fatal(err)
	}
	_ = feed.Append([]string{"e1"}, "t", "h", "标题", "正文", "request", "")
	_ = feed.Append([]string{"e2"}, "t", "h", "标题", "正文", "request", "")
	current := feed.Checkpoint()["streamId"]

	// A device persisted against an older stream realigns instead of replaying.
	key := pushDeviceKey("owner", testDeviceID, "apns")
	push.mu.Lock()
	push.devices[key].StreamID = "older-stream"
	push.mu.Unlock()

	sender := &mobileFakeSender{}
	push.Sender(sender)
	push.Drain(nil, nil)
	if deliveries := sender.deliveries(); len(deliveries) != 0 {
		t.Fatalf("reset stream replayed=%v", deliveries)
	}
	device := push.DevicesJSON()[key]
	if device.StreamID != current || device.Cursor != 2 {
		t.Fatalf("after realign stream=%q cursor=%d", device.StreamID, device.Cursor)
	}
}
