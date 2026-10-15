package jev

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestAutoResponseMapsSafeACPRecommendation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"risk":{"score":1},"reversible":{"noul":0.95},"recommend":{"choice":"accept_once","confidence":0.96}}}`))
	}))
	defer server.Close()
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	if _, err := settings.Update(func() *jsoncompat.Object {
		value := jsoncompat.NewObject()
		value.Set("apiKey", "test-key-123456")
		value.Set("baseUrl", server.URL)
		value.Set("model", "jev-latest")
		value.Set("autoApprove", true)
		return value
	}()); err != nil {
		t.Fatal(err)
	}
	params := jsoncompat.NewObject()
	options := &jsoncompat.Array{}
	allow := jsoncompat.NewObject()
	allow.Set("id", "allow-once")
	allow.Set("kind", "allow_once")
	options.Items = append(options.Items, allow)
	params.Set("options", options)
	response := settings.AutoResponse("session/request_permission", params)
	if response == nil || response.MustGet("optionId") != "allow-once" {
		t.Fatalf("unexpected auto response: %v", response)
	}
}
