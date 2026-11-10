// Provider transports for MobilePush (push.py's send_provider port). The
// HTTP clients are strict: fixed HTTPS endpoints, no redirects, bounded
// bodies. Provider credentials come only from the server-side
// mobile-push.json; phone requests never carry them.
package notifications

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// ProviderPushSender delivers through APNs (token-based JWT) or FCM v1
// (service-account OAuth). An unconfigured provider is an error the registry
// already prevented at registration time.
type ProviderPushSender struct {
	Client *http.Client

	mu       sync.Mutex
	apnsAuth map[string]apnsToken
	fcmAuth  map[string]fcmToken
}

type apnsToken struct {
	issued float64
	value  string
}

type fcmToken struct {
	issued float64
	value  string
}

func (s *ProviderPushSender) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	// http.Transport negotiates HTTP/2 over TLS by default (APNs requires it).
	return &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
}

// Send mirrors send_provider's dispatch and status mapping.
func (s *ProviderPushSender) Send(config map[string]any, device *PushDevice, event *MobileEventView) error {
	if device.Kind == "fcm" {
		return s.sendFCM(config, device, event)
	}
	return s.sendAPNs(config, device, event)
}

func pushHTTPError(status int, body []byte) error {
	if status == http.StatusGone {
		return &InvalidPushToken{Reason: "Gone"}
	}
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		var decoded struct {
			Reason string `json:"reason"`
			Error  struct {
				Details []struct {
					ErrorCode string `json:"errorCode"`
				} `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &decoded) == nil {
			if decoded.Reason == "BadDeviceToken" || decoded.Reason == "Unregistered" {
				return &InvalidPushToken{Reason: decoded.Reason}
			}
			for _, detail := range decoded.Error.Details {
				if detail.ErrorCode == "UNREGISTERED" {
					return &InvalidPushToken{Reason: "UNREGISTERED"}
				}
			}
		}
	}
	return errors.New("系统推送服务拒绝请求")
}

// ---- APNs ----

func (s *ProviderPushSender) sendAPNs(config map[string]any, device *PushDevice, event *MobileEventView) error {
	cfg, _ := config["apns"].(map[string]any)
	if cfg == nil {
		return errors.New("电脑尚未配置系统推送服务")
	}
	token, err := s.apnsJWT(cfg)
	if err != nil {
		return err
	}
	sandbox, _ := cfg["sandbox"].(bool)
	host := "api.push.apple.com"
	if sandbox {
		host = "api.sandbox.push.apple.com"
	}
	body, err := json.Marshal(PushPayload(device, event))
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, "https://"+host+"/3/device/"+device.Token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	bundleID, _ := cfg["bundleId"].(string)
	live := device.Kind == "activity"
	topic := bundleID
	if live {
		topic += ".push-type.liveactivity"
	}
	request.Header.Set("Authorization", "bearer "+token)
	request.Header.Set("apns-topic", topic)
	if live {
		request.Header.Set("apns-push-type", "liveactivity")
	} else {
		request.Header.Set("apns-push-type", "alert")
	}
	request.Header.Set("apns-priority", "10")
	request.Header.Set("apns-expiration", fmt.Sprint(time.Now().Add(time.Hour).Unix()))
	collapse := event.ID
	if len(collapse) > 64 {
		collapse = collapse[:64]
	}
	request.Header.Set("apns-collapse-id", collapse)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pushHTTPError(response.StatusCode, raw)
	}
	return nil
}

// apnsJWT mints the provider token (ES256): cached ~50 minutes like Python's
// 3000s window.
func (s *ProviderPushSender) apnsJWT(cfg map[string]any) (string, error) {
	teamID, _ := cfg["teamId"].(string)
	keyID, _ := cfg["keyId"].(string)
	keyFile, _ := cfg["keyFile"].(string)
	if teamID == "" || keyID == "" || keyFile == "" {
		return "", errors.New("APNs 配置不完整")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apnsAuth == nil {
		s.apnsAuth = map[string]apnsToken{}
	}
	cacheKey := teamID + "\x00" + keyID + "\x00" + keyFile
	if cached, ok := s.apnsAuth[cacheKey]; ok && time.Since(time.Unix(int64(cached.issued), 0)) < 50*time.Minute {
		return cached.value, nil
	}
	keyPEM, err := readKeyFile(keyFile)
	if err != nil {
		return "", err
	}
	private, err := parseECPrivateKey(keyPEM)
	if err != nil {
		return "", errors.New("APNs 密钥格式无效")
	}
	issued := time.Now().Unix()
	header := base64URL([]byte(`{"alg":"ES256","kid":"` + keyID + `"}`))
	claims := base64URL([]byte(fmt.Sprintf(`{"iss":%q,"iat":%d}`, teamID, issued)))
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	r, ss, err := ecdsa.Sign(nil, private, digest[:])
	if err != nil {
		return "", err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	ss.FillBytes(signature[32:])
	token := signing + "." + base64URL(signature)
	s.apnsAuth = map[string]apnsToken{cacheKey: {issued: float64(issued), value: token}}
	return token, nil
}

func base64URL(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func readKeyFile(path string) ([]byte, error) {
	raw, err := readFileBounded(path)
	if err != nil {
		return nil, errors.New("APNs/FCM 密钥文件不可读")
	}
	return raw, nil
}

func parseECPrivateKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("密钥不是 PEM 格式")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := anyKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("密钥不是 EC 私钥")
	}
	return key, nil
}

// ---- FCM ----

func (s *ProviderPushSender) sendFCM(config map[string]any, device *PushDevice, event *MobileEventView) error {
	cfg, _ := config["fcm"].(map[string]any)
	if cfg == nil {
		return errors.New("电脑尚未配置系统推送服务")
	}
	projectID, _ := cfg["projectId"].(string)
	if !providerIDPattern.MatchString(projectID) {
		return errors.New("Firebase project ID 无效")
	}
	token, err := s.fcmAccessToken(cfg)
	if err != nil {
		return err
	}
	body, err := json.Marshal(PushPayload(device, event))
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost,
		"https://fcm.googleapis.com/v1/projects/"+projectID+"/messages:send", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client().Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return pushHTTPError(response.StatusCode, raw)
	}
	return nil
}

// fcmAccessToken exchanges the service account's RS256 assertion for an OAuth
// access token (google.oauth2.service_account flow).
func (s *ProviderPushSender) fcmAccessToken(cfg map[string]any) (string, error) {
	credentialsFile, _ := cfg["credentialsFile"].(string)
	if credentialsFile == "" {
		return "", errors.New("FCM 配置不完整")
	}
	s.mu.Lock()
	cached, ok := s.fcmAuth[credentialsFile]
	s.mu.Unlock()
	if ok && cached.value != "" && time.Since(time.Unix(int64(cached.issued), 0)) < 50*time.Minute {
		return cached.value, nil
	}
	raw, err := readFileBounded(credentialsFile)
	if err != nil {
		return "", errors.New("FCM 凭据文件不可读")
	}
	var account struct {
		ClientEmail  string `json:"client_email"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
	}
	if err := json.Unmarshal(raw, &account); err != nil || account.ClientEmail == "" || account.PrivateKey == "" {
		return "", errors.New("FCM 服务账号凭据无效")
	}
	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return "", errors.New("FCM 私钥格式无效")
	}
	var private *rsa.PrivateKey
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		private = key
	} else {
		anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return "", errors.New("FCM 私钥格式无效")
		}
		rsaKey, ok := anyKey.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("FCM 私钥不是 RSA 私钥")
		}
		private = rsaKey
	}
	now := time.Now().Unix()
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}` + optionalKeyID(account.PrivateKeyID)))
	claims := base64URL([]byte(fmt.Sprintf(
		`{"iss":%q,"scope":"https://www.googleapis.com/auth/firebase.messaging","aud":"https://oauth2.googleapis.com/token","iat":%d,"exp":%d}`,
		account.ClientEmail, now, now+3600)))
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(nil, private, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	assertion := signing + "." + base64URL(signature)
	form := "grant_type=urn%3Aietf%3Aparams%3Aoauth%3Agrant-type%3Ajwt-bearer&assertion=" + assertion
	request, err := http.NewRequest(http.MethodPost, "https://oauth2.googleapis.com/token",
		bytes.NewBufferString(form))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.client().Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	rawBody, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode != http.StatusOK {
		return "", errors.New("系统推送服务拒绝请求")
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(rawBody, &tokenResponse); err != nil || tokenResponse.AccessToken == "" {
		return "", errors.New("系统推送服务响应无效")
	}
	s.mu.Lock()
	if s.fcmAuth == nil {
		s.fcmAuth = map[string]fcmToken{}
	}
	s.fcmAuth[credentialsFile] = fcmToken{issued: float64(now), value: tokenResponse.AccessToken}
	s.mu.Unlock()
	return tokenResponse.AccessToken, nil
}

func readFileBounded(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

func optionalKeyID(id string) string {
	if id == "" {
		return ""
	}
	return `,"kid":"` + id + `"`
}

// ---- payload (push.py payload port) ----

var pushClientNames = map[string]string{"claude": "Claude", "deepseek": "DeepSeek Harness", "codex": "Codex"}

// PushPayload builds the provider body. Push carries generic state only —
// full inbox content stays behind gateway authentication.
func PushPayload(device *PushDevice, event *MobileEventView) map[string]any {
	provider := event.Provider
	if provider == "" {
		provider = "codex"
	}
	route := map[string]any{"origin": device.Origin, "thread": event.ThreadID, "host": event.Host,
		"provider": provider, "sequence": fmt.Sprint(event.Sequence), "streamId": event.StreamID}
	title := "有任务等待你确认"
	if event.Kind == "attention" {
		title = "运行遇到问题，需要你处理"
	} else if event.Kind == "completion" {
		title = "本轮已结束"
	}
	clientName := pushClientNames[provider]
	if clientName == "" {
		clientName = provider
	}
	heading := clientName + " · Codex Bridge"
	if device.Kind == "fcm" {
		if device.SourceIcon {
			data := map[string]any{}
			for key, value := range route {
				data[key] = value
			}
			data["title"] = heading
			data["body"] = title
			return map[string]any{"message": map[string]any{"token": device.Token, "data": data,
				"android": map[string]any{"priority": "high", "ttl": "3600s"}}}
		}
		return map[string]any{"message": map[string]any{"token": device.Token,
			"notification": map[string]any{"title": heading, "body": title}, "data": route,
			"android": map[string]any{"priority": "high", "ttl": "3600s",
				"notification": map[string]any{"channel_id": "tasks",
					"tag": "bridge|" + device.Origin + "|" + fmt.Sprint(event.Sequence)}}}}
	}
	if device.Kind == "activity" {
		now := time.Now().Unix()
		phase := "ended"
		if event.Kind != "completion" {
			phase = "waiting"
		}
		if event.Kind == "state" && event.Phase != "" {
			phase = event.Phase
		}
		aps := map[string]any{"timestamp": now, "event": eventKindToAPNs(event.Kind),
			"content-state": map[string]any{"phase": phase, "updatedAt": now - 978307200}}
		if event.Kind == "completion" {
			aps["dismissal-date"] = now + 300
		} else {
			aps["stale-date"] = now + 300
		}
		return map[string]any{"aps": aps}
	}
	aps := map[string]any{"alert": map[string]any{"title": heading, "body": title},
		"sound": "default", "thread-id": event.ThreadID, "mutable-content": 1}
	for key, value := range route {
		aps[key] = value
	}
	return map[string]any{"aps": aps}
}

// Phase carries the Live Activity phase on state events (MobileEventView
// helper: the drain loop sets Phase on the underlying event).
func eventKindToAPNs(kind string) string {
	if kind == "completion" {
		return "end"
	}
	return "update"
}
