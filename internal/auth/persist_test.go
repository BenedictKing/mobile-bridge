package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func testConfig() Config {
	return Config{Mode: "password", Username: "admin", Salt: "aabb", Hash: "ccdd",
		Iterations: 600000, SessionHours: 12}
}

func fixedClock() (func() float64, *float64) {
	now := float64(1791940000)
	return func() float64 { return now }, &now
}

// The identity text must be byte-identical to Python's
// sha256(json.dumps({...}, sort_keys=True)) so both implementations accept
// each other's session stores. Vector computed with CPython 3.
func TestIdentityMatchesPythonVector(t *testing.T) {
	auth := NewAuth(testConfig())
	if got := auth.identityText(); got != "ffad6985aa4fb3eae2fd67ffe01ce6efdf6140fbb6cd685795057bebd0311661" {
		t.Fatalf("identity mismatch: %s (from %q)", got, auth.identityText())
	}
}

func TestSessionsSurviveRestartAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	clock, _ := fixedClock()
	first := NewAuth(testConfig())
	first.Now = clock
	first.EnablePersistence(dir)
	token, _, err := first.NewSession("127.0.0.1", "ua", nil)
	if err != nil {
		t.Fatal(err)
	}

	second := NewAuth(testConfig())
	second.Now = clock
	second.EnablePersistence(dir)
	if second.Get(token) == nil {
		t.Fatal("session did not survive the simulated restart")
	}
	second.Logout(token)

	third := NewAuth(testConfig())
	third.Now = clock
	third.EnablePersistence(dir)
	if third.Get(token) != nil {
		t.Fatal("logout was not durable")
	}
}

func TestIdentityMismatchDropsSessions(t *testing.T) {
	dir := t.TempDir()
	clock, _ := fixedClock()
	first := NewAuth(testConfig())
	first.Now = clock
	first.EnablePersistence(dir)
	token, _, _ := first.NewSession("127.0.0.1", "", nil)

	rotated := testConfig()
	rotated.Hash = "different"
	second := NewAuth(rotated)
	second.Now = clock
	second.EnablePersistence(dir)
	if second.Get(token) != nil {
		t.Fatal("session survived a password rotation")
	}
	// The mismatch rewrite keeps the new identity: a third instance loads it.
	third := NewAuth(rotated)
	third.Now = clock
	third.EnablePersistence(dir)
	if _, err := os.Stat(filepath.Join(dir, "auth-sessions.json")); err != nil {
		t.Fatalf("store not rewritten after rotation: %v", err)
	}
}

func TestLoadsPythonWrittenStore(t *testing.T) {
	dir := t.TempDir()
	identity := NewAuth(testConfig()).identityText()
	store := `{"identity": "` + identity + `", "policy": {"allowlistEnabled": false, "blocklist": ["9.9.9.9"]}, ` +
		`"sessions": {"` + SessionKey("token123") + `": {"csrf": "c1", "expires": 9999999999.5, "persistent": true, ` +
		`"created": 100.5, "lastSeen": 100.5, "userAgent": "iPhone", "ip": "127.0.0.1", "peer": "127.0.0.1", "source": "direct"}}, ` +
		`"failures": {"1.2.3.4": 2}, "autoBlocks": {"5.6.7.8": {"id": "ab", "ip": "5.6.7.8", "blockedAt": 1.5, "attempts": 5}}}`
	if err := os.WriteFile(filepath.Join(dir, "auth-sessions.json"), []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}

	clock, now := fixedClock()
	*now = float64(1791940000)
	loaded := NewAuth(testConfig())
	loaded.Now = clock
	loaded.EnablePersistence(dir)
	if loaded.Get("token123") == nil {
		t.Fatal("python-written session not loaded")
	}
	status := loaded.LoginStatus("1.2.3.4")
	if remaining, _ := status.MustGet("attemptsRemaining").(json.Number); remaining.String() != "3" {
		t.Fatalf("failures not restored: %s", jsoncompat.EncodePythonDefault(status))
	}
	if !loaded.blockedIP["5.6.7.8"] {
		t.Fatal("auto block not restored")
	}
}
