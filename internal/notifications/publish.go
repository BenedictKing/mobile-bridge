package notifications

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type Publisher struct{ Client *http.Client }

func NewPublisher(client *http.Client) *Publisher {
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Publisher{Client: client}
}

func (p *Publisher) Publish(config *jsoncompat.Object, channel, title, body, click string) error {
	if channel == "ntfy" {
		return p.publishNtfy(config, title, body, click)
	}
	if channel == "bark" {
		return p.publishBark(config, title, body, click)
	}
	return errors.New("未知通知通道")
}

func (p *Publisher) publishNtfy(config *jsoncompat.Object, title, body, click string) error {
	server, _ := config.MustGet("server").(string)
	topic, _ := config.MustGet("topic").(string)
	if topic == "" {
		return errors.New("请先配置 ntfy 服务和主题")
	}
	payload := map[string]any{"topic": topic, "title": title, "message": body, "tags": []string{"bell"}, "priority": 3}
	if click != "" {
		payload["click"] = click
	}
	return p.post(strings.TrimRight(server, "/")+"/", payload, config, false)
}

func (p *Publisher) publishBark(config *jsoncompat.Object, title, body, click string) error {
	server, _ := config.MustGet("barkServer").(string)
	key, _ := config.MustGet("barkKey").(string)
	if key == "" {
		return errors.New("请先配置 Bark 服务和 Device Key")
	}
	payload := map[string]any{"device_key": key, "title": title, "body": body, "group": "Mobile / Bridge"}
	if click != "" {
		payload["url"] = click
	}
	return p.post(strings.TrimRight(server, "/")+"/push", payload, config, true)
}

func (p *Publisher) post(endpoint string, payload any, config *jsoncompat.Object, bark bool) error {
	raw, _ := json.Marshal(payload)
	request, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if !bark {
		if token, _ := config.MustGet("token").(string); token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := p.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 65536))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s 未接受通知", map[bool]string{true: "Bark", false: "ntfy"}[bark])
	}
	if bark {
		var value struct {
			Code int `json:"code"`
		}
		if json.Unmarshal(data, &value) != nil || value.Code != 200 {
			return errors.New("Bark 未接受通知，请检查服务地址、Device Key 和网络")
		}
	}
	return nil
}
