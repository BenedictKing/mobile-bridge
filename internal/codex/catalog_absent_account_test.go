package codex

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestFastModeWithoutAccountIsUnavailable(t *testing.T) {
	result, err := fastModeOf(func(method string, _ any) (*jsoncompat.Object, error) {
		switch method {
		case "config/read":
			return object(map[string]any{"config": object(map[string]any{"model_provider": "openai"})}), nil
		case "account/read":
			return object(map[string]any{"account": nil, "requiresOpenaiAuth": true}), nil
		default:
			t.Fatalf("accountless runtime requested %s", method)
			return nil, nil
		}
	}, t.TempDir())
	if err != nil || result["allowed"] != false {
		t.Fatalf("missing account did not disable Fast: result=%v error=%v", result, err)
	}
}
