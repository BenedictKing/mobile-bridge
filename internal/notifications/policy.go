package notifications

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type PolicyManager struct{ DataDir string }

func NewPolicyManager(dataDir string) *PolicyManager { return &PolicyManager{DataDir: dataDir} }
func (m *PolicyManager) path() string                { return filepath.Join(m.DataDir, "notification-policies.json") }
func (m *PolicyManager) read() *jsoncompat.Object {
	raw, err := os.ReadFile(m.path())
	if err != nil {
		result := jsoncompat.NewObject()
		result.Set("requests", true)
		result.Set("completion", false)
		result.Set("chats", jsoncompat.NewObject())
		return result
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		result := jsoncompat.NewObject()
		result.Set("requests", true)
		result.Set("completion", false)
		result.Set("chats", jsoncompat.NewObject())
		return result
	}
	result, _ := value.(*jsoncompat.Object)
	if result == nil {
		result = jsoncompat.NewObject()
	}
	if _, ok := result.Get("requests"); !ok {
		result.Set("requests", true)
	}
	if _, ok := result.Get("completion"); !ok {
		result.Set("completion", false)
	}
	if _, ok := result.Get("chats"); !ok {
		result.Set("chats", jsoncompat.NewObject())
	}
	return result
}
func (m *PolicyManager) save(value *jsoncompat.Object) error {
	if err := os.MkdirAll(m.DataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(m.path(), append(jsoncompat.EncodePythonIndent2(value), '\n'), 0o600)
}
func (m *PolicyManager) Defaults(value *jsoncompat.Object) (*jsoncompat.Object, error) {
	policies := m.read()
	if value != nil {
		for _, key := range []string{"requests", "completion"} {
			flag, ok := value.MustGet(key).(bool)
			if !ok {
				return nil, errors.New("通知开关格式不正确")
			}
			policies.Set(key, flag)
		}
		if err := m.save(policies); err != nil {
			return nil, err
		}
	}
	result := jsoncompat.NewObject()
	result.Set("requests", policies.MustGet("requests"))
	result.Set("completion", policies.MustGet("completion"))
	return result, nil
}
func (m *PolicyManager) Chat(host, id string, value *jsoncompat.Object) (*jsoncompat.Object, error) {
	if host == "" || id == "" {
		return nil, errors.New("通知监控操作格式不正确")
	}
	policies := m.read()
	chats, _ := policies.MustGet("chats").(*jsoncompat.Object)
	key := host + "|" + id
	choices := jsoncompat.NewObject()
	choices.Set("requests", "inherit")
	choices.Set("completion", "inherit")
	if chats != nil {
		if row, _ := chats.Get(key); row != nil {
			if obj, _ := row.(*jsoncompat.Object); obj != nil {
				choices = obj
			}
		}
	}
	if value != nil {
		for _, key := range []string{"requests", "completion"} {
			choice, ok := value.MustGet(key).(string)
			if !ok || choice != "inherit" && choice != "on" && choice != "off" {
				return nil, errors.New("聊天通知设置格式不正确")
			}
			choices.Set(key, choice)
		}
		chats.Set(host+"|"+id, choices)
		policies.Set("chats", chats)
		if err := m.save(policies); err != nil {
			return nil, err
		}
	}
	defaults := map[string]bool{}
	defaults["requests"], _ = policies.MustGet("requests").(bool)
	defaults["completion"], _ = policies.MustGet("completion").(bool)
	result := jsoncompat.NewObject()
	result.Set("available", len(Channels(ReadSettings(m.DataDir)).Keys()) > 0)
	result.Set("watching", choices.MustGet("requests") == "on" || choices.MustGet("completion") == "on" || (choices.MustGet("requests") == "inherit" && defaults["requests"]) || (choices.MustGet("completion") == "inherit" && defaults["completion"]))
	result.Set("notifyOnRequest", choices.MustGet("requests") == "on" || (choices.MustGet("requests") == "inherit" && defaults["requests"]))
	result.Set("notifyOnCompletion", choices.MustGet("completion") == "on" || (choices.MustGet("completion") == "inherit" && defaults["completion"]))
	result.Set("requests", choices.MustGet("requests"))
	result.Set("completion", choices.MustGet("completion"))
	return result, nil
}
