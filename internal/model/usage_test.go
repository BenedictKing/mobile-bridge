package model

import (
	"encoding/json"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestNormalizeStateProjectsLatestTokenUsage(t *testing.T) {
	state := jsoncompat.NewObject()
	usage := jsoncompat.NewObject()
	last := jsoncompat.NewObject()
	last.Set("totalTokens", jsoncompat.Num(48066))
	total := jsoncompat.NewObject()
	total.Set("totalTokens", jsoncompat.Num(900000))
	usage.Set("last", last)
	usage.Set("total", total)
	usage.Set("modelContextWindow", jsoncompat.Num(960000))
	state.Set("latestTokenUsageInfo", usage)
	state.Set("tokenUsageFromHistory", true)
	view := NormalizeState(state, true)
	projected := view.MustGet("usage").(*jsoncompat.Object)
	if projected.MustGet("used").(json.Number).String() != "48066" || projected.MustGet("size").(json.Number).String() != "960000" {
		t.Fatalf("usage = %v", projected)
	}
	if projected.MustGet("stale") != true {
		t.Fatalf("restored usage was not marked stale: %v", projected)
	}
}

func TestNormalizeStateOmitsIncompleteTokenUsage(t *testing.T) {
	for _, raw := range []string{
		`{"latestTokenUsageInfo":{"total":{"totalTokens":900000},"modelContextWindow":960000}}`,
		`{"latestTokenUsageInfo":{"last":{"totalTokens":-1},"modelContextWindow":960000}}`,
		`{"latestTokenUsageInfo":{"last":{"totalTokens":48066},"modelContextWindow":null}}`,
	} {
		view := NormalizeState(normalizeFixture(t, raw), true)
		if _, exists := view.Get("usage"); exists {
			t.Fatalf("incomplete usage was exposed: %v", view)
		}
	}
}
