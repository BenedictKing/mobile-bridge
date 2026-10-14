// zcodecreds.go: Go port of bridge/agents/zcode_creds.py — self-heal the
// ZCode local state the headless wrapper reads from disk.
//
// Two defects the zcode-acp-server wrapper inherits from
// <zcode-home>/v2/config.json, both fixed here:
//
//  1. The coding-plan key slot. The ZCode desktop app in OAuth family mode
//     stores the OAuth access JWT in builtin:bigmodel-coding-plan's apiKey —
//     overwriting it at sign-in, token refresh and account switches. Client
//     signing cannot parse a JWT and every turn dies as a misleading
//     "[网络异常，正在重试 (n/5)…]" loop. The real id.secret plan key lives
//     AES-256-GCM-encrypted (enc:v1: prefix) in v2/credentials.json; the
//     healer decrypts the ACTIVE account's key and restores it.
//
//  2. The provider/model dropdown. The healer mirrors the desktop app's own
//     truth: the bundled provider table's builtinModelIds for the held plan,
//     and the coding-plan cache's availability verdict for a ghost start-plan.
//
// Unlike Python (which shells out to `node -e` for decryption) this port
// decrypts natively; the enc:v1 framing is identical.
package acpagents

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const (
	zcConfigProvider    = "builtin:bigmodel-coding-plan"
	zcStartPlanProvider = "builtin:bigmodel-start-plan"
	zcAccountRuleID     = "account:bigmodel-individual-coding-plan"
	zcAccountKeyPrefix  = "account-provider:coding-plan:account:bigmodel-individual-coding-plan:account:"
	zcUserInfoKey       = "oauth:bigmodel:user_info"
	zcCredentialEnv     = "ZCODE_CREDENTIAL_SECRET"
	zcCacheName         = "coding-plan-cache.json"
	zcPollInterval      = 2 * time.Second
)

// zcTableCandidates: the desktop app's bundled provider table, newest truth
// for what a plan offers.
var zcTableCandidates = []string{
	"/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json",
	"~/Applications/ZCode.app/Contents/Resources/config/provider/zcode-builtin.json",
	"/opt/ZCode/resources/config/provider/zcode-builtin.json",
	"/usr/share/zcode/resources/config/provider/zcode-builtin.json",
}

type zcPaths struct {
	configPath string
	credsPath  string
	cachePath  string
	tablePath  string
}

func (p *zcPaths) stamps() [4][2]int64 {
	var result [4][2]int64
	for i, path := range []string{p.configPath, p.credsPath, p.cachePath, p.tablePath} {
		if info, err := os.Stat(path); err == nil {
			result[i] = [2]int64{info.ModTime().UnixNano(), info.Size()}
		}
	}
	return result
}

// zcNodePlatform mirrors Python's sys.platform (and Node's process.platform):
// windows is "win32", everything else keeps its GOOS spelling.
func zcNodePlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// zcNodeUsername mirrors Node's userInfo().username: the passwd entry, never
// the environment.
func zcNodeUsername() string {
	if current, err := user.Current(); err == nil && current.Username != "" {
		return current.Username
	}
	return os.Getenv("USER")
}

// zcSecret replicates Node's defaultCredentialSecret() byte for byte:
// zcode-credential-fallback:<platform>:<homedir>:<username>.
func zcSecret(env func(string) string, home string) string {
	if explicit := strings.TrimSpace(env(zcCredentialEnv)); explicit != "" {
		return explicit
	}
	nodeHome := env("HOME")
	if nodeHome == "" {
		nodeHome = env("USERPROFILE")
	}
	if nodeHome == "" {
		nodeHome = home
	}
	return fmt.Sprintf("zcode-credential-fallback:%s:%s:%s", zcNodePlatform(), nodeHome, zcNodeUsername())
}

func zcDecodeBase64URL(value string) ([]byte, bool) {
	for _, encoding := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, true
		}
	}
	return nil, false
}

// zcDecrypt decrypts one enc:v1 value: nonce.tag.ciphertext (base64url)
// under aes-256-gcm with sha256(secret) as the key. Non-enc values pass
// through; failures return ("", false) like Python's None.
func zcDecrypt(value, secret string) (string, bool) {
	if !strings.HasPrefix(value, "enc:v1:") {
		return value, true
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "enc:v1:"), ".", 3)
	if len(parts) != 3 {
		return "", false
	}
	nonce, okN := zcDecodeBase64URL(parts[0])
	tag, okT := zcDecodeBase64URL(parts[1])
	ciphertext, okC := zcDecodeBase64URL(parts[2])
	if !okN || !okT || !okC {
		return "", false
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", false
	}
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plaintext, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", false
	}
	return string(plaintext), true
}

// zcEncrypt is the inverse, used by tests to build fixtures in the same
// framing the ZCode app writes.
func zcEncrypt(plaintext, secret string) string {
	key := sha256.Sum256([]byte(secret))
	block, _ := aes.NewCipher(key[:])
	gcm, _ := cipher.NewGCM(block)
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	sealed := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	tag, ciphertext := sealed[len(sealed)-gcm.Overhead():], sealed[:len(sealed)-gcm.Overhead()]
	return "enc:v1:" + base64.RawURLEncoding.EncodeToString(nonce) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(ciphertext)
}

func zcReadJSON(path string) *jsoncompat.Object {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return nil
	}
	object, _ := value.(*jsoncompat.Object)
	return object
}

// zcPlanKeys returns every stored bigmodel coding-plan api-key entry keyed
// by account id.
func zcPlanKeys(creds *jsoncompat.Object) map[string]string {
	out := map[string]string{}
	if creds == nil {
		return out
	}
	for _, name := range creds.Keys() {
		value, _ := creds.MustGet(name).(string)
		if value == "" {
			continue
		}
		if strings.HasPrefix(name, zcAccountKeyPrefix) && strings.HasSuffix(name, ":api-key") {
			account := name[len(zcAccountKeyPrefix) : len(name)-len(":api-key")]
			out[account] = value
		}
	}
	return out
}

// zcLooksLikeJWT: a JWT overwrite is the defect being healed.
func zcLooksLikeJWT(value string) bool {
	return strings.HasPrefix(value, "eyJ") && strings.Count(value, ".") == 2
}

// zcIDSecret: a valid plan credential is exactly apiKeyId.secret.
func zcIDSecret(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t\n") {
		return false
	}
	parts := strings.Split(value, ".")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

// zcResolvePlanKey returns (decrypted id.secret, account id) for the active
// account, or ("", "") when nothing servable exists.
func zcResolvePlanKey(creds *jsoncompat.Object, secret string) (string, string) {
	keys := zcPlanKeys(creds)
	if len(keys) == 0 {
		return "", ""
	}
	account := ""
	if userInfo, ok := creds.MustGet(zcUserInfoKey).(string); ok {
		if decrypted, valid := zcDecrypt(userInfo, secret); valid {
			if info := zcReadJSONBytes([]byte(decrypted)); info != nil {
				account, _ = info.MustGet("id").(string)
			}
		}
	}
	if _, known := keys[account]; !known {
		// Stale or absent user_info: unambiguous only with a single account.
		if len(keys) == 1 {
			for only := range keys {
				account = only
			}
		} else {
			account = ""
		}
	}
	if account == "" {
		return "", ""
	}
	planKey, ok := zcDecrypt(keys[account], secret)
	if ok && zcIDSecret(planKey) {
		return planKey, account
	}
	return "", ""
}

func zcReadJSONBytes(raw []byte) *jsoncompat.Object {
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return nil
	}
	object, _ := value.(*jsoncompat.Object)
	return object
}

func zcFindBuiltinTable() string {
	for _, candidate := range zcTableCandidates {
		path := candidate
		if strings.HasPrefix(candidate, "~/") {
			home := os.Getenv("HOME")
			if home == "" {
				continue
			}
			path = filepath.Join(home, candidate[2:])
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

// zcTablePlanModels: builtinModelIds the bundled table declares for the held
// plan, preserving the declared order.
func zcTablePlanModels(tablePath string) []string {
	if tablePath == "" {
		return nil
	}
	data := zcReadJSON(tablePath)
	if data == nil {
		return nil
	}
	config, _ := data.MustGet("config").(*jsoncompat.Object)
	if config == nil {
		return nil
	}
	rulesRoot, _ := config.MustGet("providerConfigRules").(*jsoncompat.Object)
	if rulesRoot == nil {
		return nil
	}
	rules, _ := rulesRoot.MustGet("providerRules").(*jsoncompat.Array)
	if rules == nil {
		return nil
	}
	for _, item := range rules.Items {
		rule, ok := item.(*jsoncompat.Object)
		if !ok {
			continue
		}
		if id, _ := rule.MustGet("providerId").(string); id != zcAccountRuleID {
			continue
		}
		ruleConfig, _ := rule.MustGet("config").(*jsoncompat.Object)
		if ruleConfig == nil {
			return nil
		}
		ids, _ := ruleConfig.MustGet("builtinModelIds").(*jsoncompat.Array)
		if ids == nil || len(ids.Items) == 0 {
			return nil
		}
		out := []string{}
		for _, value := range ids.Items {
			if modelID, ok := value.(string); ok && modelID != "" {
				out = append(out, modelID)
			}
		}
		return out
	}
	return nil
}

func zcCacheStatus(cachePath, providerID string) string {
	data := zcReadJSON(cachePath)
	if data == nil {
		return ""
	}
	statusRoot, _ := data.MustGet("entryStatus").(*jsoncompat.Object)
	if statusRoot == nil {
		return ""
	}
	items, _ := statusRoot.MustGet("items").(*jsoncompat.Object)
	if items == nil {
		return ""
	}
	entry, _ := items.MustGet(providerID).(*jsoncompat.Object)
	if entry == nil {
		return ""
	}
	status, _ := entry.MustGet("status").(string)
	return status
}

// zcSyncModels mirrors the plan's builtinModelIds onto the coding-plan entry.
// Existing per-model specs are preserved; models the plan no longer offers
// are dropped.
func zcSyncModels(config *jsoncompat.Object, tablePath string, changes *[]string) {
	if tablePath == "" {
		return
	}
	providers, _ := config.MustGet("provider").(*jsoncompat.Object)
	if providers == nil {
		return
	}
	provider, _ := providers.MustGet(zcConfigProvider).(*jsoncompat.Object)
	if provider == nil {
		return
	}
	planIDs := zcTablePlanModels(tablePath)
	if len(planIDs) == 0 {
		return
	}
	models, _ := provider.MustGet("models").(*jsoncompat.Object)
	if models == nil {
		models = jsoncompat.NewObject()
	}
	if strings.Join(models.Keys(), "\x00") == strings.Join(planIDs, "\x00") {
		return
	}
	updated := jsoncompat.NewObject()
	for _, modelID := range planIDs {
		if spec, ok := models.MustGet(modelID).(*jsoncompat.Object); ok {
			updated.Set(modelID, spec)
		} else {
			updated.Set(modelID, jsoncompat.NewObject())
		}
	}
	provider.Set("models", updated)
	*changes = append(*changes, "models -> "+strings.Join(planIDs, ","))
}

// zcDisableGhostStartPlan hides a start-plan the account does not hold: only
// an explicit 'unavailable' verdict from the desktop's own cache plus a
// non-plan key triggers the disable.
func zcDisableGhostStartPlan(config *jsoncompat.Object, cachePath string, changes *[]string) {
	providers, _ := config.MustGet("provider").(*jsoncompat.Object)
	if providers == nil {
		return
	}
	provider, _ := providers.MustGet(zcStartPlanProvider).(*jsoncompat.Object)
	if provider == nil {
		return
	}
	if enabled, _ := provider.MustGet("enabled").(bool); !enabled {
		return
	}
	options, _ := provider.MustGet("options").(*jsoncompat.Object)
	key := ""
	if options != nil {
		key, _ = options.MustGet("apiKey").(string)
	}
	if zcIDSecret(key) {
		return
	}
	if zcCacheStatus(cachePath, zcStartPlanProvider) != "unavailable" {
		return
	}
	provider.Set("enabled", false)
	*changes = append(*changes, "start-plan disabled (not entitled)")
}

// EnsureZcodeState runs one healing pass and returns a status word: ok,
// patched, custom or unavailable. It never returns an error — a failed heal
// must not kill the gateway.
func EnsureZcodeState(env func(string) string, home string, paths *zcPaths) string {
	if paths == nil {
		paths = &zcPaths{
			configPath: filepath.Join(zcodeHome(env, home), "v2", "config.json"),
			credsPath:  filepath.Join(zcodeHome(env, home), "v2", "credentials.json"),
			cachePath:  filepath.Join(zcodeHome(env, home), "v2", zcCacheName),
			tablePath:  zcFindBuiltinTable(),
		}
	}
	config := zcReadJSON(paths.configPath)
	if config == nil {
		return "unavailable"
	}
	providers, _ := config.MustGet("provider").(*jsoncompat.Object)
	if providers == nil {
		return "unavailable"
	}
	provider, _ := providers.MustGet(zcConfigProvider).(*jsoncompat.Object)
	if provider == nil {
		return "unavailable"
	}

	secret := zcSecret(env, home)
	keyStatus := "unavailable"
	slot := ""
	if options, _ := provider.MustGet("options").(*jsoncompat.Object); options != nil {
		slot, _ = options.MustGet("apiKey").(string)
	}
	creds := zcReadJSON(paths.credsPath)
	planKey, account := zcResolvePlanKey(creds, secret)
	switch {
	case planKey != "" && slot == planKey:
		keyStatus = "ok"
	case planKey != "":
		// Rewrite when the app overwrote the slot (JWT), left it empty, or a
		// previous heal went stale after an account switch. A foreign
		// hand-written key is respected.
		stored := map[string]bool{}
		for _, encrypted := range zcPlanKeys(creds) {
			if decrypted, ok := zcDecrypt(encrypted, secret); ok {
				stored[decrypted] = true
			}
		}
		if slot == "" || zcLooksLikeJWT(slot) || stored[slot] {
			options, _ := provider.MustGet("options").(*jsoncompat.Object)
			if options == nil {
				options = jsoncompat.NewObject()
				provider.Set("options", options)
			}
			options.Set("apiKey", planKey)
			keyStatus = "patched"
		} else {
			keyStatus = "custom"
		}
	}

	changes := []string{}
	if keyStatus == "patched" {
		changes = append(changes, fmt.Sprintf("plan key (account %s)", account))
	}
	zcSyncModels(config, paths.tablePath, &changes)
	zcDisableGhostStartPlan(config, paths.cachePath, &changes)
	if len(changes) > 0 {
		if zcWriteJSONAtomic(paths.configPath, config) {
			return "patched"
		}
		return "unavailable"
	}
	return keyStatus
}

// zcWriteJSONAtomic mirrors Python's _atomic_write_json: indent=2,
// ensure_ascii=False, tmp file then rename.
func zcWriteJSONAtomic(path string, object *jsoncompat.Object) bool {
	raw := jsoncompat.EncodePythonIndent2(object)
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		raw = append(raw, '\n')
	}
	tmp := path + ".bridge-tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

var zcWatchOnce sync.Once

// EnsureAndWatch runs one synchronous heal plus a background stamp watcher;
// safe to call repeatedly (the watcher is a singleton). The watcher re-heals
// when any input changes: an account switch in the desktop app, a plan
// refresh or an app update all converge back to a consistent state.
func EnsureAndWatch(env func(string) string, home string, logf func(string, ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	status := EnsureZcodeState(env, home, nil)
	if status == "patched" || status == "unavailable" {
		logf("zcode state heal: initial pass %s", status)
	}
	zcWatchOnce.Do(func() {
		go func() {
			paths := &zcPaths{
				configPath: filepath.Join(zcodeHome(env, home), "v2", "config.json"),
				credsPath:  filepath.Join(zcodeHome(env, home), "v2", "credentials.json"),
				cachePath:  filepath.Join(zcodeHome(env, home), "v2", zcCacheName),
				tablePath:  zcFindBuiltinTable(),
			}
			signature := paths.stamps()
			for {
				time.Sleep(zcPollInterval)
				current := paths.stamps()
				if current == signature {
					continue
				}
				signature = current
				if status := EnsureZcodeState(env, home, nil); status == "patched" {
					logf("zcode state heal: re-healed after input change")
				}
			}
		}()
	})
}
