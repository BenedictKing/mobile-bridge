package notifications

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPublisherNtfyAndBark(t *testing.T) {
	seen := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		_ = json.NewDecoder(r.Body).Decode(&value)
		seen = append(seen, value)
		if r.URL.Path == "/push" {
			_, _ = w.Write([]byte(`{"code":200}`))
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	config := jsoncompat.NewObject()
	config.Set("server", server.URL)
	config.Set("topic", "topic")
	config.Set("token", "token")
	config.Set("barkServer", server.URL)
	config.Set("barkKey", "key")
	publisher := NewPublisher(nil)
	if err := publisher.Publish(config, "ntfy", "Title", "Body", "https://click"); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(config, "bark", "Title", "Body", "https://click"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0]["topic"] != "topic" || seen[1]["device_key"] != "key" {
		t.Fatalf("unexpected payloads: %#v", seen)
	}
}
