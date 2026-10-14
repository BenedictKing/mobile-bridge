// Session persistence in bridge/auth.py's auth-sessions.json format, so a
// gateway restart (or a Python rollback) keeps phones logged in and keeps
// failure counts and auto blocks durable.
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// EnablePersistence binds the auth state to <dataDir>/auth-sessions.json and
// loads whatever survives: policy, failure counts, auto blocks, and sessions
// when the identity (the auth config fingerprint) still matches.
func (a *Auth) EnablePersistence(dataDir string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.path = filepath.Join(dataDir, "auth-sessions.json")
	a.identity = a.identityText()
	a.loadLocked()
}

// identityText reproduces Python's fingerprint byte for byte: the auth config
// keys plus sessionHours, json.dumps(sort_keys=True) with the default ", "/
// ": " separators, hashed with sha256.
func (a *Auth) identityText() string {
	identity := jsoncompat.NewObject()
	// Insert in sorted order: json.dumps(sort_keys=True) output order.
	identity.Set("hash", a.Config.Hash)
	identity.Set("iterations", jsoncompat.Num(a.Config.Iterations))
	identity.Set("mode", a.Config.Mode)
	identity.Set("salt", a.Config.Salt)
	identity.Set("sessionHours", jsoncompat.Num(a.Config.SessionHours))
	identity.Set("username", a.Config.Username)
	digest := sha256.Sum256([]byte(jsoncompat.EncodePythonDefault(identity)))
	return hex.EncodeToString(digest[:])
}

func (a *Auth) defaultPolicyLocked() *jsoncompat.Object {
	if a.policy != nil {
		return a.policy
	}
	policy := jsoncompat.NewObject()
	policy.Set("allowlistEnabled", false)
	a.policy = policy
	return policy
}

func (a *Auth) loadLocked() {
	raw, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return
	}
	root, ok := value.(*jsoncompat.Object)
	if !ok {
		return
	}
	if policy, ok := root.MustGet("policy").(*jsoncompat.Object); ok {
		a.policy = policy
	}
	if failures, ok := root.MustGet("failures").(*jsoncompat.Object); ok {
		for _, address := range failures.Keys() {
			if number, ok := failures.MustGet(address).(json.Number); ok {
				if count, err := strconv.Atoi(number.String()); err == nil {
					a.failures[address] = count
				}
			}
		}
	}
	if blocks, ok := root.MustGet("autoBlocks").(*jsoncompat.Object); ok {
		for _, address := range blocks.Keys() {
			if entry, ok := blocks.MustGet(address).(*jsoncompat.Object); ok {
				a.autoBlocks[address] = entry
				a.blockedIP[address] = true
			}
		}
	}
	if saved, ok := root.MustGet("sessions").(*jsoncompat.Object); ok {
		if identity, _ := root.MustGet("identity").(string); identity == a.identity {
			now := a.Now()
			for _, key := range saved.Keys() {
				session, ok := saved.MustGet(key).(*jsoncompat.Object)
				if !ok {
					continue
				}
				if expires, ok := session.MustGet("expires").(json.Number); ok {
					if expiry, err := strconv.ParseFloat(expires.String(), 64); err == nil && SessionValid(expiry, now) {
						a.sessions[key] = session
					}
				}
			}
			return
		}
		// Auth config changed (e.g. password rotated): drop the stale
		// sessions and rewrite the file, exactly like Python does.
		a.persistLocked()
	}
}

func (a *Auth) persistLocked() {
	if a.path == "" {
		return
	}
	root := jsoncompat.NewObject()
	root.Set("identity", a.identity)
	root.Set("policy", a.defaultPolicyLocked())
	sessions := jsoncompat.NewObject()
	keys := make([]string, 0, len(a.sessions))
	for key := range a.sessions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		sessions.Set(key, a.sessions[key])
	}
	failures := jsoncompat.NewObject()
	for _, address := range sortedIntKeys(a.failures) {
		failures.Set(address, jsoncompat.Num(a.failures[address]))
	}
	blocks := jsoncompat.NewObject()
	for _, address := range sortedObjectKeys(a.autoBlocks) {
		blocks.Set(address, a.autoBlocks[address])
	}
	root.Set("sessions", sessions)
	root.Set("failures", failures)
	root.Set("autoBlocks", blocks)
	directory := filepath.Dir(a.path)
	temporary, err := os.CreateTemp(directory, filepath.Base(a.path)+".*.tmp")
	if err != nil {
		return
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err := temporary.Write([]byte(jsoncompat.EncodePythonDefaultASCII(root))); err != nil {
		temporary.Close()
		return
	}
	if err := temporary.Close(); err != nil {
		return
	}
	_ = os.Chmod(name, 0o600)
	_ = os.Rename(name, a.path)
}

func sortedIntKeys(source map[string]int) []string {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedObjectKeys(source map[string]*jsoncompat.Object) []string {
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
