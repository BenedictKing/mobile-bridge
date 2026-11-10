package httpd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/notifications"
)

// mobileAPIFixture wires a server with the mobile inbox (feed + push registry)
// and one desktop session, mirroring main.go's assembly.
type mobileAPIFixture struct {
	t      *testing.T
	server *Server
	auth   *auth.Auth
	dir    string
	feed   *notifications.MobileEvents
	token  string
	csrf   string
}

func newMobileAPIFixture(t *testing.T, mode string) *mobileAPIFixture {
	t.Helper()
	dir := t.TempDir()
	authenticator := auth.NewAuth(auth.Config{Mode: mode})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.NotificationDir = dir
	feed := notifications.NewMobileEvents(dir)
	server.Mobile = feed
	server.MobilePush = notifications.NewMobilePush(dir, feed)
	return &mobileAPIFixture{t: t, server: server, auth: authenticator, dir: dir, feed: feed, token: token, csrf: csrf}
}

// build prepares a request against the default loopback origin; authed adds
// the legacy cookie plus (for bodies) the CSRF token.
func (f *mobileAPIFixture) build(method, target, body string, authed bool) *http.Request {
	f.t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", f.csrf)
	}
	if authed {
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: f.token})
	}
	return request
}

func (f *mobileAPIFixture) serve(request *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	response := httptest.NewRecorder()
	f.server.ServeHTTP(response, request)
	return response
}

func (f *mobileAPIFixture) request(method, target, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.serve(f.build(method, target, body, true))
}

func decodeMobileJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON (%d): %s", recorder.Code, recorder.Body.String())
	}
	return body
}

func mobileNumber(t *testing.T, container map[string]any, key string) float64 {
	t.Helper()
	value, ok := container[key].(float64)
	if !ok {
		t.Fatalf("%s is %T (%v), want number", key, container[key], container[key])
	}
	return value
}

func testSHA256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func testHMACHex(key, value string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestMobileEventsRequireLogin(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	response := f.serve(f.build(http.MethodGet, "/api/mobile/events", "", false))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeMobileJSON(t, response)
	if body["code"] != "unauthenticated" {
		t.Fatalf("body=%v", body)
	}
}

func TestMobileEventsParameterValidation(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	cases := []struct {
		name   string
		query  string
		status int
	}{
		{"after 非数字", "?after=abc", http.StatusBadRequest},
		{"after 超 16 位", "?after=12345678901234567", http.StatusBadRequest},
		{"wait 超 60", "?wait=61", http.StatusBadRequest},
		{"wait 三位数", "?wait=100", http.StatusBadRequest},
		{"revision 非 32hex", "?revision=xyz", http.StatusBadRequest},
		{"streamId 控制字符", "?streamId=%03bad", http.StatusBadRequest},
		{"合法参数", "?after=0&wait=0", http.StatusOK},
	}
	for _, item := range cases {
		response := f.request(http.MethodGet, "/api/mobile/events"+item.query, "")
		if response.Code != item.status {
			t.Fatalf("%s status=%d want %d body=%s", item.name, response.Code, item.status, response.Body.String())
		}
	}
}

func TestMobileEventsSnapshotShape(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	response := f.request(http.MethodGet, "/api/mobile/events?after=0&wait=0", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeMobileJSON(t, response)
	for _, key := range []string{"streamId", "cursor", "unread", "seen", "summary", "events",
		"enabled", "clients", "longPoll", "revision", "timing"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("missing key %s: %v", key, body)
		}
	}
	if body["streamId"] == "" || body["cursor"] != float64(0) {
		t.Fatalf("streamId=%v cursor=%v", body["streamId"], body["cursor"])
	}
	if body["enabled"] != true || body["longPoll"] != float64(60) {
		t.Fatalf("enabled=%v longPoll=%v", body["enabled"], body["longPoll"])
	}
	if revision, ok := body["revision"].(string); !ok || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(revision) {
		t.Fatalf("revision=%v", body["revision"])
	}
	unread, _ := body["unread"].(map[string]any)
	if unread["codex"] != float64(0) {
		t.Fatalf("unread=%v", unread)
	}
	if events, ok := body["events"].([]any); !ok || len(events) != 0 {
		t.Fatalf("events=%v", body["events"])
	}
	if summary, ok := body["summary"].([]any); !ok || len(summary) != 0 {
		t.Fatalf("summary=%v", body["summary"])
	}
	clients, _ := body["clients"].([]any)
	if len(clients) != 1 {
		t.Fatalf("clients=%v", clients)
	}
	client, _ := clients[0].(map[string]any)
	if client["id"] != "codex" || client["enabled"] != true || client["unread"] != float64(0) || client["connected"] != nil {
		t.Fatalf("client=%v", client)
	}
	timing, _ := body["timing"].(map[string]any)
	if mobileNumber(t, timing, "waitMs") != 0 {
		t.Fatalf("timing=%v", timing)
	}
}

func TestMobileEventsAppendView(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	if err := f.feed.Append([]string{"k1"}, "thread-x", "local", "需要确认", "任务等待确认", "request", ""); err != nil {
		t.Fatal(err)
	}
	response := f.request(http.MethodGet, "/api/mobile/events?after=0&wait=0", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeMobileJSON(t, response)
	if body["cursor"] != float64(1) {
		t.Fatalf("cursor=%v", body["cursor"])
	}
	unread, _ := body["unread"].(map[string]any)
	if unread["codex"] != float64(1) {
		t.Fatalf("unread=%v", unread)
	}
	events, _ := body["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("events=%v", events)
	}
	event, _ := events[0].(map[string]any)
	if event["provider"] != "codex" || event["clientId"] != "codex" || event["unread"] != true {
		t.Fatalf("event=%v", event)
	}
	if event["kind"] != "request" || event["title"] != "需要确认" || event["threadId"] != "thread-x" {
		t.Fatalf("event=%v", event)
	}
	if age := mobileNumber(t, event, "ageMs"); age < 0 {
		t.Fatalf("ageMs=%v", age)
	}
	if event["streamId"] != body["streamId"] {
		t.Fatalf("event streamId=%v", event["streamId"])
	}

	// A phone that already read everything gets an empty incremental list.
	empty := f.request(http.MethodGet, "/api/mobile/events?after=1&wait=0", "")
	tail := decodeMobileJSON(t, empty)
	if tailEvents, _ := tail["events"].([]any); len(tailEvents) != 0 {
		t.Fatalf("incremental events=%v", tailEvents)
	}
}

// longPollRequest starts a caught-up long-poll (after == cursor, matching
// revision) in the background and returns its eventual response.
func (f *mobileAPIFixture) longPollRequest() <-chan *httptest.ResponseRecorder {
	f.t.Helper()
	request := f.build(http.MethodGet, "/api/mobile/events?after=0&wait=5&revision="+
		f.feedRevision()+"&streamId="+f.streamID(), "", true)
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		f.server.ServeHTTP(recorder, request)
		response <- recorder
	}()
	return response
}

func (f *mobileAPIFixture) feedRevision() string {
	f.t.Helper()
	body := decodeMobileJSON(f.t, f.request(http.MethodGet, "/api/mobile/events?after=0&wait=0", ""))
	return body["revision"].(string)
}

func (f *mobileAPIFixture) streamID() string {
	f.t.Helper()
	checkpoint := f.feed.Checkpoint()
	return checkpoint["streamId"].(string)
}

func TestMobileEventsLongPollWake(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	responses := f.longPollRequest()
	time.Sleep(150 * time.Millisecond)
	if err := f.feed.Append([]string{"wake"}, "t", "h", "标题", "正文", "request", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		body := decodeMobileJSON(t, response)
		if body["cursor"] != float64(1) {
			t.Fatalf("cursor=%v", body["cursor"])
		}
		if events, _ := body["events"].([]any); len(events) != 1 {
			t.Fatalf("events=%v", events)
		}
		timing, _ := body["timing"].(map[string]any)
		if wait := mobileNumber(t, timing, "waitMs"); wait < 0 {
			t.Fatalf("waitMs=%v", wait)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("long-poll did not wake on append")
	}
}

func TestMobileEventsLongPollRevokedDuringWait(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	responses := f.longPollRequest()
	time.Sleep(150 * time.Millisecond)
	f.auth.Logout(f.token)
	if err := f.feed.Append([]string{"wake"}, "t", "h", "标题", "正文", "request", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-responses:
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		if body := decodeMobileJSON(t, response); body["code"] != "unauthenticated" {
			t.Fatalf("body=%v", body)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("long-poll did not return after revocation")
	}
}

func TestMobileEventsReadReceipt(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	if err := f.feed.Append([]string{"c1"}, "thread-c", "local", "标题", "正文", "request", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.feed.Append([]string{"z1"}, "thread-z", "local", "标题", "正文", "request", "zcode"); err != nil {
		t.Fatal(err)
	}
	stream := f.streamID()

	// A read receipt is a write: the CSRF token is mandatory.
	bare := f.build(http.MethodPost, "/api/mobile/events/read",
		`{"provider":"zcode","streamId":"`+stream+`","through":2}`, true)
	bare.Header.Del("X-CSRF-Token")
	if response := f.serve(bare); response.Code != http.StatusForbidden {
		t.Fatalf("no-csrf status=%d body=%s", response.Code, response.Body.String())
	}

	read := f.request(http.MethodPost, "/api/mobile/events/read",
		`{"provider":"zcode","streamId":"`+stream+`","through":2}`)
	if read.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", read.Code, read.Body.String())
	}
	body := decodeMobileJSON(t, read)
	unread, _ := body["unread"].(map[string]any)
	if unread["zcode"] != float64(0) || unread["codex"] != float64(1) {
		t.Fatalf("unread after zcode read=%v", unread)
	}
	seen, _ := body["seen"].(map[string]any)
	if seen["zcode"] != float64(2) {
		t.Fatalf("seen=%v", seen)
	}
	// The reply is the freshest full snapshot, not just the ack.
	events, _ := body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events=%v", events)
	}
	first, _ := events[0].(map[string]any)
	if first["provider"] != "codex" || first["unread"] != true {
		t.Fatalf("codex event=%v", first)
	}
	second, _ := events[1].(map[string]any)
	if second["provider"] != "zcode" || second["unread"] != false {
		t.Fatalf("zcode event=%v", second)
	}

	// A stale receipt cannot rewind the read cursor.
	rewind := f.request(http.MethodPost, "/api/mobile/events/read",
		`{"provider":"zcode","streamId":"`+stream+`","through":1}`)
	if rewind.Code != http.StatusOK {
		t.Fatalf("rewind status=%d body=%s", rewind.Code, rewind.Body.String())
	}
	if seen, _ = decodeMobileJSON(t, rewind)["seen"].(map[string]any); seen["zcode"] != float64(2) {
		t.Fatalf("seen after rewind=%v", seen)
	}

	// A mismatched stream is refused.
	stale := f.request(http.MethodPost, "/api/mobile/events/read",
		`{"provider":"zcode","streamId":"other","through":2}`)
	if stale.Code != http.StatusBadRequest ||
		!strings.Contains(stale.Body.String(), "通知状态已变化") {
		t.Fatalf("stale status=%d body=%s", stale.Code, stale.Body.String())
	}
}

func TestMobilePushStatusAndRegistration(t *testing.T) {
	f := newMobileAPIFixture(t, "password")
	if response := f.request(http.MethodGet, "/api/mobile/push", ""); response.Code != http.StatusOK {
		t.Fatalf("status GET=%d body=%s", response.Code, response.Body.String())
	} else if body := decodeMobileJSON(t, response); body["configured"] != false ||
		body["apns"] != false || body["fcm"] != false {
		t.Fatalf("status=%v", body)
	}
	if response := f.serve(f.build(http.MethodGet, "/api/mobile/push", "", false)); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET=%d", response.Code)
	}

	// Registration needs a configured provider first.
	register := `{"deviceId":"11111111-2222-4333-8444-555566667777","kind":"apns","token":"` +
		strings.Repeat("ab", 32) + `"}`
	if response := f.request(http.MethodPost, "/api/mobile/push", register); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "电脑尚未配置系统推送服务") {
		t.Fatalf("unconfigured status=%d body=%s", response.Code, response.Body.String())
	}

	config := `{"apns": {"teamId": "TEAM", "keyId": "KEY", "keyFile": "/unused.pem"}}`
	if err := os.WriteFile(filepath.Join(f.dir, "mobile-push.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if response := f.request(http.MethodPost, "/api/mobile/push", register); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"registered": true`) {
		t.Fatalf("register status=%d body=%s", response.Code, response.Body.String())
	}
	statusAfter := decodeMobileJSON(t, f.request(http.MethodGet, "/api/mobile/push", ""))
	if statusAfter["apns"] != true {
		t.Fatalf("status after config=%v", statusAfter)
	}

	// An untrusted origin never reaches registration.
	crossOrigin := f.build(http.MethodPost, "/api/mobile/push", register, true)
	crossOrigin.Header.Set("Origin", "http://evil.example")
	if response := f.serve(crossOrigin); response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d body=%s", response.Code, response.Body.String())
	}

	// Passwordless gateways refuse registration outright.
	passwordless := newMobileAPIFixture(t, "none")
	if response := passwordless.request(http.MethodPost, "/api/mobile/push", register); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "请启用密码保护后注册系统推送") {
		t.Fatalf("passwordless status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConnectionProve(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	token, _, err := f.auth.NewSession("127.0.0.1", "BridgeMobile/0.1-iOS", false)
	if err != nil {
		t.Fatal(err)
	}
	deviceID := f.server.Auth.DeviceID()
	sessionKey := testSHA256Hex(token)
	identifier := testSHA256Hex(sessionKey)
	challenge := strings.Repeat("ab", 32)
	target := "http://127.0.0.1:8790"
	query := "?id=" + identifier + "&challenge=" + challenge + "&device=" + deviceID

	// The anonymous proof is the HMAC over device\ntarget\nchallenge keyed by
	// the live credential (the session key here).
	response := f.serve(f.build(http.MethodGet, "/api/connection/prove"+query, "", false))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeMobileJSON(t, response)
	if body["deviceId"] != deviceID {
		t.Fatalf("deviceId=%v", body["deviceId"])
	}
	if proof, _ := body["proof"].(string); proof != testHMACHex(sessionKey, deviceID+"\n"+target+"\n"+challenge) {
		t.Fatalf("proof=%v", body["proof"])
	}

	// Malformed identity material is a client error.
	if response := f.serve(f.build(http.MethodGet, "/api/connection/prove?id=nothex&challenge="+challenge+"&device="+deviceID, "", false)); response.Code != http.StatusBadRequest {
		t.Fatalf("bad id status=%d body=%s", response.Code, response.Body.String())
	}
	// Another gateway's device id is refused, as is a missing device (relay
	// path this gateway does not serve).
	wrongDevice := "?id=" + identifier + "&challenge=" + challenge + "&device=" + strings.Repeat("cd", 32)
	if response := f.serve(f.build(http.MethodGet, "/api/connection/prove"+wrongDevice, "", false)); response.Code != http.StatusForbidden {
		t.Fatalf("wrong device status=%d body=%s", response.Code, response.Body.String())
	}
	if response := f.serve(f.build(http.MethodGet, "/api/connection/prove?id="+identifier+"&challenge="+challenge, "", false)); response.Code != http.StatusForbidden {
		t.Fatalf("no device status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConnectionRoutesManifestAndRenew(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	hostA, originA := "192.168.1.50:8790", "http://192.168.1.50:8790"
	hostB, originB := "192.168.1.60:8790", "http://192.168.1.60:8790"
	f.server.Hosts[hostA], f.server.Origins[originA] = true, true
	f.server.Hosts[hostB], f.server.Origins[originB] = true, true

	token, session, err := f.auth.NewSession("127.0.0.1", "BridgeMobile/0.1-iOS", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	deviceID := f.server.Auth.DeviceID()
	cookieName := f.server.Auth.CookieName()

	// Desktop user agents never see the manifest.
	desktop := f.request(http.MethodGet, "/api/connection/routes", "")
	if desktop.Code != http.StatusForbidden || !strings.Contains(desktop.Body.String(), "需要手机网关绑定") {
		t.Fatalf("desktop status=%d body=%s", desktop.Code, desktop.Body.String())
	}

	// A paired phone gets the LAN manifest (loopback entries unpublished).
	phone := f.build(http.MethodGet, "/api/connection/routes", "", false)
	phone.Header.Set("User-Agent", "BridgeMobile/0.1-iOS")
	phone.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	response := f.serve(phone)
	if response.Code != http.StatusOK {
		t.Fatalf("phone status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeMobileJSON(t, response)
	if id, _ := body["deviceId"].(string); !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(id) {
		t.Fatalf("deviceId=%v", body["deviceId"])
	}
	race, _ := body["race"].(map[string]any)
	if race["parallel"] != float64(4) || race["select"] != "firstVerified" {
		t.Fatalf("race=%v", race)
	}
	endpoints, _ := body["endpoints"].([]any)
	if len(endpoints) != 2 || endpoints[0] != originA || endpoints[1] != originB {
		t.Fatalf("endpoints=%v", endpoints)
	}
	if unbound, _ := body["unbound"].([]any); len(unbound) != 0 {
		t.Fatalf("unbound=%v", unbound)
	}
	if body["renewal"] != "paired" {
		t.Fatalf("renewal=%v", body["renewal"])
	}

	// Renew rides a route-scoped cookie on one bound entry and grants a new
	// binding signed with the renewal verifier (ConnectionDiscovery.swift's
	// digest/proof formulas).
	credentialA := "cmbroute1_" + testHMACHex(token, "gateway-route-v1\n"+originA)
	digestB := testSHA256Hex("cmbroute1_" + testHMACHex(token, "gateway-route-v1\n"+originB))
	verifier := testSHA256Hex(testHMACHex(token, "gateway-route-renew-v1"))
	proofB := testHMACHex(verifier, "route-grant-v1\n"+deviceID+"\n"+originB+"\n"+digestB)
	renewJSON := `{"deviceId":"` + deviceID + `","bindings":{"` + originB + `":{"digest":"` +
		digestB + `","proof":"` + proofB + `"}}}`

	renew := httptest.NewRequest(http.MethodPost, originA+"/api/connection/routes", strings.NewReader(renewJSON))
	renew.Host = hostA
	renew.Header.Set("Origin", originA)
	renew.Header.Set("Content-Type", "application/json")
	renew.Header.Set("User-Agent", "BridgeMobile/0.1-iOS")
	renew.Header.Set("X-CSRF-Token", csrf)
	renew.AddCookie(&http.Cookie{Name: cookieName, Value: credentialA})
	if response := f.serve(renew); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"bound": true`) {
		t.Fatalf("renew status=%d body=%s", response.Code, response.Body.String())
	}

	// A wrongly signed grant is refused.
	forged := strings.Replace(renewJSON, proofB, strings.Repeat("ef", 32), 1)
	badRenew := httptest.NewRequest(http.MethodPost, originA+"/api/connection/routes", strings.NewReader(forged))
	badRenew.Host = hostA
	badRenew.Header.Set("Origin", originA)
	badRenew.Header.Set("Content-Type", "application/json")
	badRenew.Header.Set("User-Agent", "BridgeMobile/0.1-iOS")
	badRenew.Header.Set("X-CSRF-Token", csrf)
	badRenew.AddCookie(&http.Cookie{Name: cookieName, Value: credentialA})
	if response := f.serve(badRenew); response.Code != http.StatusForbidden ||
		!strings.Contains(response.Body.String(), "信道绑定验证失败") {
		t.Fatalf("forged renew status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestMobileChannelShellAndSessionExtras(t *testing.T) {
	f := newMobileAPIFixture(t, "none")

	// The bare shell page loads anonymously.
	shell := f.serve(f.build(http.MethodGet, "/mobile-channel", "", false))
	if shell.Code != http.StatusOK || !strings.Contains(shell.Body.String(), "<!doctype html>") {
		t.Fatalf("shell status=%d body=%s", shell.Code, shell.Body.String())
	}

	// The session list carries the feed checkpoint for reconnect alignment.
	if err := f.feed.Append([]string{"k1"}, "t", "h", "标题", "正文", "request", ""); err != nil {
		t.Fatal(err)
	}
	list := f.request(http.MethodGet, "/api/sessions", "")
	if list.Code != http.StatusOK {
		t.Fatalf("sessions status=%d body=%s", list.Code, list.Body.String())
	}
	checkpoint, _ := decodeMobileJSON(t, list)["notificationRead"].(map[string]any)
	if checkpoint == nil || checkpoint["streamId"] != f.streamID() || checkpoint["cursor"] != float64(1) {
		t.Fatalf("notificationRead=%v", checkpoint)
	}

	// /api/auth names the namespaced cookie and reports trusted devices.
	authn := f.serve(f.build(http.MethodGet, "/api/auth", "", false))
	authnBody := decodeMobileJSON(t, authn)
	if name, _ := authnBody["cookieName"].(string); !regexp.MustCompile(`^codex_mobile_session_[a-f0-9]{24}$`).MatchString(name) {
		t.Fatalf("cookieName=%v", authnBody["cookieName"])
	}
	if authnBody["trustedDevice"] != false || authnBody["authenticated"] != false {
		t.Fatalf("anonymous auth=%v", authnBody)
	}
	token, _, err := f.auth.NewSession("127.0.0.1", "BridgeMobile/0.1-iOS", false)
	if err != nil {
		t.Fatal(err)
	}
	mobile := f.build(http.MethodGet, "/api/auth", "", false)
	mobile.Header.Set("User-Agent", "BridgeMobile/0.1-iOS")
	mobile.AddCookie(&http.Cookie{Name: f.server.Auth.CookieName(), Value: token})
	mobileBody := decodeMobileJSON(t, f.serve(mobile))
	if mobileBody["authenticated"] != true || mobileBody["trustedDevice"] != true {
		t.Fatalf("mobile auth=%v", mobileBody)
	}
	if routes, _ := mobileBody["mobileRoutes"].(map[string]any); routes == nil {
		t.Fatalf("mobileRoutes missing: %v", mobileBody)
	}
}
