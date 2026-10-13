package notifications

import (
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"testing"
)

func TestChatPolicyPersistsChoices(t *testing.T) {
	manager := NewPolicyManager(t.TempDir())
	value := jsoncompat.NewObject()
	value.Set("requests", "off")
	value.Set("completion", "on")
	result, err := manager.Chat("local", "thread", value)
	if err != nil || result.MustGet("notifyOnCompletion") != true || result.MustGet("notifyOnRequest") != false {
		t.Fatalf("result=%v err=%v", result, err)
	}
	loaded, err := manager.Chat("local", "thread", nil)
	if err != nil || loaded.MustGet("completion") != "on" {
		t.Fatalf("loaded=%v err=%v", loaded, err)
	}
}
