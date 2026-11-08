package model

import (
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestNormalizeACPPlanPermissionRetainsPresentationFields(t *testing.T) {
	params := jsoncompat.NewObject()
	params.Set("text", "计划摘要")
	params.Set("planContent", "# 计划详情")
	params.Set("allowedPrompts", &jsoncompat.Array{Items: []any{"run tests"}})
	params.Set("command", "")
	params.Set("options", &jsoncompat.Array{})
	request := jsoncompat.NewObject()
	request.Set("id", "req-plan")
	request.Set("method", "acp/requestPermission")
	request.Set("params", params)

	result := obj(NormalizeRequest(request).MustGet("params"))
	for _, key := range []string{"text", "planContent", "allowedPrompts", "command", "options"} {
		if _, ok := result.Get(key); !ok {
			t.Errorf("normalized params dropped %q", key)
		}
	}
	if result.MustGet("planContent") != "# 计划详情" {
		t.Fatalf("plan content changed: %v", result.MustGet("planContent"))
	}
}
