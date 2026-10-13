package jev

import (
	"os"
	"path/filepath"
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
