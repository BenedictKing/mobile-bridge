package jev

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestPublicUsesEnvironmentDefaultsAndMasksKey(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "env-secret-key")
	t.Setenv("TYPESAFE_BASE_URL", "https://env.example.test")
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	public := settings.Public()
	if public.MustGet("configured") != true || public.MustGet("apiKeyMasked") != "env…-key" {
		t.Fatalf("unexpected public settings: %s", jsoncompat.EncodePythonDefault(public))
	}
	if public.MustGet("baseUrl") != "https://env.example.test" {
		t.Fatalf("env base URL not used: %v", public.MustGet("baseUrl"))
	}
}

func TestUpdateClearsSavedKeyWithoutFallingBackToEnvironment(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "environment-key")
	path := filepath.Join(t.TempDir(), "nested", "jev.json")
	settings := New(path)
	input := jsoncompat.NewObject()
	input.Set("clearApiKey", true)
	input.Set("baseUrl", "")
	input.Set("model", "")
	public, err := settings.Update(input)
	if err != nil {
		t.Fatal(err)
	}
	if public.MustGet("configured") != false {
		t.Fatalf("cleared key unexpectedly active: %s", jsoncompat.EncodePythonDefault(public))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	reloaded := New(path).Public()
	if reloaded.MustGet("configured") != false {
		t.Fatalf("cleared key returned after reload: %s", jsoncompat.EncodePythonDefault(reloaded))
	}
}

func TestUpdateRejectsUnsafeProxy(t *testing.T) {
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	input := jsoncompat.NewObject()
	input.Set("baseUrl", "")
	input.Set("model", "")
	input.Set("proxy", "https://proxy.example.test:443")
	if _, err := settings.Update(input); err == nil {
		t.Fatal("expected HTTPS proxy to be rejected")
	}
}

func policyInput() *jsoncompat.Array {
	danger := jsoncompat.NewObject()
	danger.Set("name", "danger")
	danger.Set("polarity", "negative")
	danger.Set("description", "dangerous work")
	safe := jsoncompat.NewObject()
	safe.Set("name", "safe")
	safe.Set("polarity", "positive")
	safe.Set("description", "ordinary work")
	return &jsoncompat.Array{Items: []any{danger, safe}}
}

func TestUpdatePersistsPolicyAndThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jev.json")
	settings := New(path)
	input := jsoncompat.NewObject()
	input.Set("baseUrl", "")
	input.Set("model", "")
	input.Set("policy", policyInput())
	input.Set("threshold", numFloat(0.45))
	public, err := settings.Update(input)
	if err != nil {
		t.Fatal(err)
	}
	if number(public.MustGet("threshold"), 0) != 0.45 {
		t.Fatalf("threshold = %v", public.MustGet("threshold"))
	}
	policy, ok := public.MustGet("policy").(*jsoncompat.Array)
	if !ok || len(policy.Items) != 2 {
		t.Fatalf("policy = %v", public.MustGet("policy"))
	}
	first, _ := policy.Items[0].(*jsoncompat.Object)
	if first.MustGet("name") != "danger" || first.MustGet("polarity") != "negative" {
		t.Fatalf("policy category = %v", first)
	}
	reloaded := New(path).Public()
	if number(reloaded.MustGet("threshold"), 0) != 0.45 {
		t.Fatalf("reloaded threshold = %v", reloaded.MustGet("threshold"))
	}
	if policy, ok := reloaded.MustGet("policy").(*jsoncompat.Array); !ok || len(policy.Items) != 2 {
		t.Fatalf("reloaded policy = %v", reloaded.MustGet("policy"))
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(jsoncompat.EncodeCompact(decoded)) == 0 {
		t.Fatal("stored settings failed to encode")
	}
}

func TestUpdateRejectsBadPolicy(t *testing.T) {
	category := func(name, polarity, description string) *jsoncompat.Object {
		entry := jsoncompat.NewObject()
		entry.Set("name", name)
		entry.Set("polarity", polarity)
		entry.Set("description", description)
		return entry
	}
	withPolicy := func(policy any) *jsoncompat.Object {
		input := jsoncompat.NewObject()
		input.Set("baseUrl", "")
		input.Set("model", "")
		input.Set("policy", policy)
		return input
	}
	cases := []struct {
		name  string
		input *jsoncompat.Object
		want  string
	}{
		{"not an array", withPolicy("nope"), "策略格式不正确"},
		{"bad name", withPolicy(&jsoncompat.Array{Items: []any{
			category("Danger", "negative", "d"), category("safe", "positive", "p")}}),
			"策略类别 Danger 名称不合法"},
		{"duplicate name", withPolicy(&jsoncompat.Array{Items: []any{
			category("danger", "negative", "d"), category("danger", "positive", "p")}}),
			"策略类别名称不能重复"},
		{"missing polarity", withPolicy(&jsoncompat.Array{Items: []any{
			category("danger", "", "d"), category("safe", "positive", "p")}}),
			"策略类别 danger 缺少 polarity（negative/positive）"},
		{"empty description", withPolicy(&jsoncompat.Array{Items: []any{
			category("danger", "negative", ""), category("safe", "positive", "p")}}),
			"策略类别 danger 描述不能为空"},
		{"no positive", withPolicy(&jsoncompat.Array{Items: []any{
			category("danger", "negative", "d"), category("worse", "negative", "d")}}),
			"策略至少需要一个 positive 类别"},
	}
	for _, item := range cases {
		item := item
		t.Run(item.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "jev.json")
			_, err := New(path).Update(item.input)
			if err == nil || err.Error() != item.want {
				t.Fatalf("err=%v want=%q", err, item.want)
			}
			if _, statErr := os.Stat(path); statErr == nil {
				t.Fatal("rejected policy must not be written to disk")
			}
		})
	}
	for _, threshold := range []string{"0", "1", "1.5", "-0.5", "abc"} {
		input := jsoncompat.NewObject()
		input.Set("baseUrl", "")
		input.Set("model", "")
		input.Set("threshold", json.Number(threshold))
		path := filepath.Join(t.TempDir(), "jev.json")
		_, err := New(path).Update(input)
		if err == nil || err.Error() != "阈值必须在 0 和 1 之间" {
			t.Fatalf("threshold %s: err=%v", threshold, err)
		}
		if _, statErr := os.Stat(path); statErr == nil {
			t.Fatalf("threshold %s must not be written to disk", threshold)
		}
	}
}

func TestPublicEncodesFloatsWithoutPanic(t *testing.T) {
	settings := New(filepath.Join(t.TempDir(), "jev.json"))
	encoded := string(jsoncompat.EncodeCompact(settings.Public()))
	if !strings.Contains(encoded, `"threshold"`) || !strings.Contains(encoded, `"policy"`) {
		t.Fatalf("default public settings missing policy/threshold: %s", encoded)
	}
	input := jsoncompat.NewObject()
	input.Set("baseUrl", "")
	input.Set("model", "")
	if _, err := settings.Update(input); err != nil {
		t.Fatal(err)
	}
	// An update that never carried policy/threshold still serves defaults in
	// their jsoncompat forms, so encoding the public view cannot panic.
	encoded = string(jsoncompat.EncodeCompact(settings.Public()))
	if !strings.Contains(encoded, `"threshold":0.3`) {
		t.Fatalf("default threshold missing after unrelated save: %s", encoded)
	}
}
