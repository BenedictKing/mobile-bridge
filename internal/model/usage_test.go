package model

import (
	"encoding/json"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestNormalizeStateProjectsTokenUsage(t *testing.T) {
	state := jsoncompat.NewObject()
	usage := jsoncompat.NewObject()
	total := jsoncompat.NewObject()
	total.Set("totalTokens", jsoncompat.Num(48066))
	usage.Set("total", total)
	usage.Set("modelContextWindow", jsoncompat.Num(960000))
	state.Set("tokenUsage", usage)
	view := NormalizeState(state, true)
	projected := view.MustGet("usage").(*jsoncompat.Object)
	if projected.MustGet("used").(json.Number).String() != "48066" || projected.MustGet("size").(json.Number).String() != "960000" {
		t.Fatalf("usage = %v", projected)
	}
}

func TestNormalizeStateOmitsIncompleteTokenUsage(t *testing.T) {
	state := jsoncompat.NewObject()
	usage := jsoncompat.NewObject()
	usage.Set("modelContextWindow", nil)
	state.Set("tokenUsage", usage)
	view := NormalizeState(state, true)
	if _, exists := view.Get("usage"); exists {
		t.Fatalf("incomplete usage was exposed: %v", view)
	}
}
