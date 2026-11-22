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

func TestEffectiveFollowsDefaultsUnlessOverridden(t *testing.T) {
	manager := NewPolicyManager(t.TempDir())
	if requests, completion := Effective(manager.Snapshot(), "local", "other"); !requests || completion {
		t.Fatalf("defaults requests=%v completion=%v", requests, completion)
	}
	value := jsoncompat.NewObject()
	value.Set("requests", "off")
	value.Set("completion", "inherit")
	if _, err := manager.Chat("local", "thread", value); err != nil {
		t.Fatal(err)
	}
	defaults := jsoncompat.NewObject()
	defaults.Set("requests", true)
	defaults.Set("completion", true)
	if _, err := manager.Defaults(defaults); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if requests, completion := Effective(snapshot, "local", "thread"); requests || !completion {
		t.Fatalf("override requests=%v completion=%v", requests, completion)
	}
	if requests, completion := Effective(snapshot, "local", "other"); !requests || !completion {
		t.Fatalf("inherit requests=%v completion=%v", requests, completion)
	}
}
