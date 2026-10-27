package jev

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// approvalSettings returns auto-approve-enabled settings pointed at serverURL.
func approvalSettings(t *testing.T, serverURL string) *Settings {
	t.Helper()
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	value := jsoncompat.NewObject()
	value.Set("apiKey", "test-key-123456")
	value.Set("baseUrl", serverURL)
	value.Set("autoApprove", true)
	if _, err := settings.Update(value); err != nil {
		t.Fatal(err)
	}
	return settings
}

// fullProbabilities renders an outcome distribution covering every default
// policy category except skip; overrides replace individual values with any
// JSON lexeme so malformed cases reuse the same builder.
func fullProbabilities(overrides map[string]string, skip string) string {
	parts := make([]string, 0, len(defaultPolicy))
	for _, category := range defaultPolicy {
		if category.Name == skip {
			continue
		}
		value := "0"
		if override, ok := overrides[category.Name]; ok {
			value = override
		}
		parts = append(parts, fmt.Sprintf("%q:%s", category.Name, value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// outcomeAnswers builds one systemone reply: the outcome choice with its
// probability distribution (omitted when probabilities is empty) plus the
// auxiliary gate answers.
func outcomeAnswers(choice, probabilities, recommend string, reversible, confidence float64) string {
	distribution := ""
	if probabilities != "" {
		distribution = fmt.Sprintf(`,"probabilities":%s`, probabilities)
	}
	return fmt.Sprintf(`{"answers":{"outcome":{"choice":%q%s},"reversible":{"noul":%s},"recommend":{"choice":%q,"confidence":%s}}}`,
		choice, distribution, numFloat(reversible).String(), recommend, numFloat(confidence).String())
}

func TestAutoResponseMapsSafeACPRecommendation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(outcomeAnswers("routine_work", fullProbabilities(map[string]string{"routine_work": "0.9"}, ""), "accept_once", 0.95, 0.96)))
	}))
	defer server.Close()
	settings := approvalSettings(t, server.URL)
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
