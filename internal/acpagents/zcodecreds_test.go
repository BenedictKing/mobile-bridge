package acpagents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestZcDecryptRoundTrip(t *testing.T) {
	secret := "zcode-credential-fallback:darwin:/Users/demo:demo"
	encrypted := zcEncrypt("keyid.secret", secret)
	if !strings.HasPrefix(encrypted, "enc:v1:") {
		t.Fatalf("framing: %s", encrypted)
	}
	decrypted, ok := zcDecrypt(encrypted, secret)
	if !ok || decrypted != "keyid.secret" {
		t.Fatalf("decrypted=%q ok=%v", decrypted, ok)
	}
	if _, ok := zcDecrypt(encrypted, "wrong-secret"); ok {
		t.Fatal("wrong secret accepted")
	}
	if passthrough, ok := zcDecrypt("plain", secret); !ok || passthrough != "plain" {
		t.Fatalf("passthrough=%q ok=%v", passthrough, ok)
	}
}

// writeCredsFixture builds a v2 directory with a JWT-poisoned config slot,
// an encrypted plan key and an encrypted user_info identifying the account.
func writeCredsFixture(t *testing.T, home, secret, account, planKey string) *zcPaths {
	t.Helper()
	v2 := filepath.Join(home, ".zcode", "v2")
	if err := os.MkdirAll(v2, 0o700); err != nil {
		t.Fatal(err)
	}
	creds := jsoncompat.NewObject()
	creds.Set("account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:"+account+":api-key",
		zcEncrypt(planKey, secret))
	userInfo := jsoncompat.NewObject()
	userInfo.Set("id", account)
	creds.Set("oauth:bigmodel:user_info", zcEncrypt(string(jsoncompat.EncodeCompact(userInfo)), secret))
	if err := os.WriteFile(filepath.Join(v2, "credentials.json"), jsoncompat.EncodePythonIndent2(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	config := jsoncompat.NewObject()
	providers := jsoncompat.NewObject()
	plan := jsoncompat.NewObject()
	options := jsoncompat.NewObject()
	options.Set("apiKey", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig")
	plan.Set("enabled", true)
	plan.Set("options", options)
	models := jsoncompat.NewObject()
	old := jsoncompat.NewObject()
	old.Set("priority", 1)
	models.Set("glm-old", old)
	plan.Set("models", models)
	providers.Set("builtin:bigmodel-coding-plan", plan)
	start := jsoncompat.NewObject()
	startOptions := jsoncompat.NewObject()
	startOptions.Set("apiKey", "eyJanother.jwt.token")
	start.Set("enabled", true)
	start.Set("options", startOptions)
	providers.Set("builtin:bigmodel-start-plan", start)
	config.Set("provider", providers)
	if err := os.WriteFile(filepath.Join(v2, "config.json"), jsoncompat.EncodePythonIndent2(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := jsoncompat.NewObject()
	entryStatus := jsoncompat.NewObject()
	items := jsoncompat.NewObject()
	verdict := jsoncompat.NewObject()
	verdict.Set("status", "unavailable")
	items.Set("builtin:bigmodel-start-plan", verdict)
	entryStatus.Set("items", items)
	cache.Set("entryStatus", entryStatus)
	if err := os.WriteFile(filepath.Join(v2, "coding-plan-cache.json"), jsoncompat.EncodePythonIndent2(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	table := jsoncompat.NewObject()
	tableConfig := jsoncompat.NewObject()
	rulesRoot := jsoncompat.NewObject()
	rules := &jsoncompat.Array{}
	rule := jsoncompat.NewObject()
	rule.Set("providerId", "account:bigmodel-individual-coding-plan")
	ruleConfig := jsoncompat.NewObject()
	ruleConfig.Set("builtinModelIds", &jsoncompat.Array{Items: []any{"glm-5.3", "glm-5.3-flash"}})
	rule.Set("config", ruleConfig)
	rules.Items = append(rules.Items, rule)
	rulesRoot.Set("providerRules", rules)
	tableConfig.Set("providerConfigRules", rulesRoot)
	table.Set("config", tableConfig)
	tablePath := filepath.Join(v2, "provider-table.json")
	if err := os.WriteFile(tablePath, jsoncompat.EncodePythonIndent2(table), 0o600); err != nil {
		t.Fatal(err)
	}
	return &zcPaths{
		configPath: filepath.Join(v2, "config.json"),
		credsPath:  filepath.Join(v2, "credentials.json"),
		cachePath:  filepath.Join(v2, "coding-plan-cache.json"),
		tablePath:  tablePath,
	}
}

func envWith(key, value string) func(string) string {
	return func(name string) string {
		if name == key {
			return value
		}
		return ""
	}
}

func TestEnsureZcodeStateHealsAllThreeDefects(t *testing.T) {
	home := t.TempDir()
	secret := "test-secret"
	paths := writeCredsFixture(t, home, secret, "uid-1", "realkey.realsecret")
	status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths)
	if status != "patched" {
		t.Fatalf("status=%s", status)
	}
	healed := zcReadJSON(paths.configPath)
	if healed == nil {
		t.Fatal("config unreadable after heal")
	}
	providers, _ := healed.MustGet("provider").(*jsoncompat.Object)
	plan, _ := providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ := plan.MustGet("options").(*jsoncompat.Object)
	if key, _ := options.MustGet("apiKey").(string); key != "realkey.realsecret" {
		t.Fatalf("apiKey=%q", key)
	}
	models, _ := plan.MustGet("models").(*jsoncompat.Object)
	if strings.Join(models.Keys(), ",") != "glm-5.3,glm-5.3-flash" {
		t.Fatalf("models=%v", models.Keys())
	}
	if spec, ok := models.MustGet("glm-old").(*jsoncompat.Object); ok {
		t.Fatalf("stale model retained: %v", spec)
	}
	start, _ := providers.MustGet("builtin:bigmodel-start-plan").(*jsoncompat.Object)
	if enabled, _ := start.MustGet("enabled").(bool); enabled {
		t.Fatal("ghost start-plan still enabled")
	}
	// A second pass with nothing left to do reports ok.
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "ok" {
		t.Fatalf("second pass status=%s", status)
	}
}

func TestEnsureZcodeStateRespectsForeignKey(t *testing.T) {
	home := t.TempDir()
	secret := "test-secret"
	paths := writeCredsFixture(t, home, secret, "uid-1", "realkey.realsecret")
	// First pass converges the non-key parts (model lineup, ghost plan).
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "patched" {
		t.Fatalf("first pass status=%s", status)
	}
	// A hand-written foreign key is respected even when it differs from the
	// stored plan key; with nothing else left to change the pass reports
	// custom and never overwrites the slot.
	config := zcReadJSON(paths.configPath)
	providers, _ := config.MustGet("provider").(*jsoncompat.Object)
	plan, _ := providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ := plan.MustGet("options").(*jsoncompat.Object)
	options.Set("apiKey", "handwritten.foreign")
	if err := os.WriteFile(paths.configPath, jsoncompat.EncodePythonIndent2(config), 0o600); err != nil {
		t.Fatal(err)
	}
	status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths)
	if status != "custom" {
		t.Fatalf("status=%s", status)
	}
	healed := zcReadJSON(paths.configPath)
	providers, _ = healed.MustGet("provider").(*jsoncompat.Object)
	plan, _ = providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ = plan.MustGet("options").(*jsoncompat.Object)
	if key, _ := options.MustGet("apiKey").(string); key != "handwritten.foreign" {
		t.Fatalf("foreign key overwritten: %q", key)
	}
}

func TestEnsureZcodeStateUnavailableWithoutConfig(t *testing.T) {
	home := t.TempDir()
	if status := EnsureZcodeState(emptyEnv, home, &zcPaths{
		configPath: filepath.Join(home, "missing.json"),
		credsPath:  filepath.Join(home, "creds.json"),
	}); status != "unavailable" {
		t.Fatalf("status=%s", status)
	}
}

// mintedToken builds a project-access-token-shaped JWT (token_use + exp
// claims) with a bogus signature — the healer never verifies it, the remote
// endpoint issued it.
func mintedToken(exp time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims, _ := json.Marshal(map[string]any{
		"token_use": "project_access", "client_type": "zcode",
		"exp": exp.Unix(),
	})
	payload := base64.RawURLEncoding.EncodeToString(claims)
	return header + "." + payload + ".sig"
}

// writeCloudKeyFixture: active account keeps its plan key in the cloud — a
// project-key-location entry plus the OAuth token, no local api-key entries.
func writeCloudKeyFixture(t *testing.T, home, secret, account string) *zcPaths {
	t.Helper()
	paths := writeCredsFixture(t, home, secret, account, "")
	v2 := filepath.Dir(paths.configPath)
	creds := zcReadJSON(paths.credsPath)
	creds.Delete("account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:" + account + ":api-key")
	location := jsoncompat.NewObject()
	location.Set("organizationId", "org-1")
	location.Set("projectId", "proj-1")
	location.Set("apiKeyId", "key-1")
	locationName := "project-key-location:v1:" + url.QueryEscape(
		`["https://bigmodel.cn","bigmodel","`+account+`",null,1,"non-team"]`)
	creds.Set(locationName, zcEncrypt(string(jsoncompat.EncodeCompact(location)), secret))
	creds.Set("oauth:bigmodel:access_token", zcEncrypt("oauth-access-jwt", secret))
	if err := os.WriteFile(paths.credsPath, jsoncompat.EncodePythonIndent2(creds), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = v2
	return paths
}

func stubMint(token string, calls *int) func() {
	previous := zcMint
	zcMint = func(location zcKeyLocation, oauthToken string) (string, error) {
		if calls != nil {
			*calls++
		}
		if oauthToken != "oauth-access-jwt" {
			return "", errors.New("unexpected oauth token")
		}
		if location.OrganizationID != "org-1" || location.ProjectID != "proj-1" || location.APIKeyID != "key-1" {
			return "", errors.New("unexpected location " + location.OrganizationID)
		}
		return token, nil
	}
	return func() { zcMint = previous }
}

func resetMintSchedule() { zcScheduleNextMint(time.Time{}) }

func TestEnsureZcodeStateMintsProjectToken(t *testing.T) {
	home := t.TempDir()
	secret := "test-secret"
	paths := writeCloudKeyFixture(t, home, secret, "uid-1")
	token := mintedToken(zcNow().Add(time.Hour))
	restore := stubMint(token, nil)
	defer restore()
	resetMintSchedule()
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "minted" {
		t.Fatalf("status=%s", status)
	}
	healed := zcReadJSON(paths.configPath)
	providers, _ := healed.MustGet("provider").(*jsoncompat.Object)
	plan, _ := providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ := plan.MustGet("options").(*jsoncompat.Object)
	if key, _ := options.MustGet("apiKey").(string); key != token {
		t.Fatalf("apiKey not the minted token: %q", key[:min(20, len(key))])
	}
	// A valid token is left alone: second pass neither mints again nor
	// rewrites the file.
	calls := 0
	restore2 := stubMint(token, &calls)
	defer restore2()
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "ok" {
		t.Fatalf("second pass status=%s", status)
	}
	if calls != 0 {
		t.Fatalf("healthy token re-minted %d times", calls)
	}
}

func TestEnsureZcodeStateRemintsNearExpiry(t *testing.T) {
	home := t.TempDir()
	secret := "test-secret"
	paths := writeCloudKeyFixture(t, home, secret, "uid-1")
	expiring := mintedToken(zcNow().Add(3 * time.Minute))
	restore := stubMint(expiring, nil)
	resetMintSchedule()
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "minted" {
		t.Fatalf("status=%s", status)
	}
	restore()
	fresh := mintedToken(zcNow().Add(time.Hour))
	calls := 0
	restore2 := stubMint(fresh, &calls)
	defer restore2()
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "minted" {
		t.Fatalf("re-mint status=%s", status)
	}
	if calls != 1 {
		t.Fatalf("mint calls=%d", calls)
	}
	healed := zcReadJSON(paths.configPath)
	providers, _ := healed.MustGet("provider").(*jsoncompat.Object)
	plan, _ := providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ := plan.MustGet("options").(*jsoncompat.Object)
	if key, _ := options.MustGet("apiKey").(string); key != fresh {
		t.Fatal("expiring token not replaced")
	}
}

func TestEnsureZcodeStateMintFailureKeepsUnavailable(t *testing.T) {
	home := t.TempDir()
	secret := "test-secret"
	paths := writeCloudKeyFixture(t, home, secret, "uid-1")
	previous := zcMint
	zcMint = func(zcKeyLocation, string) (string, error) { return "", errors.New("boom") }
	defer func() { zcMint = previous }()
	resetMintSchedule()
	if status := EnsureZcodeState(envWith("ZCODE_CREDENTIAL_SECRET", secret), home, paths); status != "unavailable" {
		t.Fatalf("status=%s", status)
	}
	healed := zcReadJSON(paths.configPath)
	providers, _ := healed.MustGet("provider").(*jsoncompat.Object)
	plan, _ := providers.MustGet("builtin:bigmodel-coding-plan").(*jsoncompat.Object)
	options, _ := plan.MustGet("options").(*jsoncompat.Object)
	if key, _ := options.MustGet("apiKey").(string); !zcLooksLikeJWT(key) {
		t.Fatalf("poisoned slot rewritten on mint failure: %q", key)
	}
}

func TestZcProjectTokenExpRejectsOAuthJWT(t *testing.T) {
	if _, ok := zcProjectTokenExp("eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.sig"); ok {
		t.Fatal("OAuth-shaped JWT accepted as project token")
	}
	exp := zcNow().Add(time.Hour)
	parsed, ok := zcProjectTokenExp(mintedToken(exp))
	if !ok || parsed.Unix() != exp.Unix() {
		t.Fatalf("parsed=%v ok=%v", parsed, ok)
	}
	if _, ok := zcProjectTokenExp("realkey.realsecret"); ok {
		t.Fatal("id.secret accepted as project token")
	}
}
