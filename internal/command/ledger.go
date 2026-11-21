// Package command records gateway-owned control intents before provider I/O.
package command

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

var (
	ErrUnknown  = errors.New("操作结果未知，请核实原会话；此命令不会再次执行")
	ErrConflict = errors.New("同一命令或请求不能用于不同回应")
	ErrStore    = errors.New("命令回执无法持久化，尚未调用 Agent")
)

type Intent struct {
	ID, Scope, Target string
	Payload           any
}

type record struct {
	ID        string          `json:"id"`
	Scope     string          `json:"scope"`
	Target    string          `json:"target,omitempty"`
	Hash      string          `json:"hash"`
	Status    string          `json:"status"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	UpdatedAt int64           `json:"updatedAt"`
}

type Ledger struct {
	mu      sync.Mutex
	path    string
	entries map[string]*record
	targets map[string]*record
}

func (l *Ledger) HasTarget(target string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.targets[target] != nil
}

// Open fails closed on unreadable receipts. Claimed records are never replayed.
func Open(path string) (*Ledger, error) {
	l := &Ledger{path: path, entries: map[string]*record{}, targets: map[string]*record{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &l.entries); err != nil {
		return nil, err
	}
	if l.entries == nil {
		return nil, errors.New("命令回执必须是对象")
	}
	for id, r := range l.entries {
		if r == nil || id != r.ID || r.Scope == "" || r.Hash == "" {
			return nil, errors.New("命令回执条目无效")
		}
		switch r.Status {
		case "claimed", "accepted", "rejected", "outcome_unknown":
		default:
			return nil, errors.New("命令回执状态无效")
		}
		if r.Target != "" && r.Status != "rejected" {
			if old := l.targets[r.Target]; old == nil || old.UpdatedAt < r.UpdatedAt {
				l.targets[r.Target] = r
			}
		}
	}
	return l, nil
}

// Hash canonicalizes JSON objects so field order does not change identity.
func Hash(payload any) string {
	raw, _ := json.Marshal(nativeJSON(payload))
	var value any
	if json.Unmarshal(raw, &value) == nil {
		raw, _ = json.Marshal(value)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (l *Ledger) Replay(id, scope string, payload any) (json.RawMessage, error, bool) {
	if l == nil || id == "" {
		return nil, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.entries[id]
	if r == nil {
		return nil, nil, false
	}
	if r.Scope != scope || r.Hash != Hash(payload) {
		return nil, ErrConflict, true
	}
	result, err := replay(r)
	return result, err, true
}

func replay(r *record) (json.RawMessage, error) {
	switch r.Status {
	case "accepted":
		return append(json.RawMessage(nil), r.Result...), nil
	case "rejected":
		return nil, Rejected(errors.New(r.Error))
	default:
		return nil, ErrUnknown
	}
}

// Rejected marks a failure known to occur before any provider write.
type rejection struct{ error }

func (r rejection) Unwrap() error { return r.error }
func Rejected(err error) error    { return rejection{err} }
func IsRejected(err error) bool   { var rejected rejection; return errors.As(err, &rejected) }

// Run persists a claim before calling fn, and never repeats uncertain I/O.
// Target lets human and automatic responses claim the same native request.
func (l *Ledger) Run(intent Intent, fn func() (any, error)) (json.RawMessage, error) {
	if l == nil {
		value, err := fn()
		raw, _ := json.Marshal(nativeJSON(value))
		return raw, err
	}
	if intent.ID == "" || intent.Scope == "" {
		return nil, errors.New("命令身份不能为空")
	}
	hash := Hash(intent.Payload)
	l.mu.Lock()
	prior := l.entries[intent.ID]
	if prior == nil && intent.Target != "" {
		prior = l.targets[intent.Target]
	}
	if prior != nil {
		if prior.Scope != intent.Scope || prior.Hash != hash {
			l.mu.Unlock()
			return nil, ErrConflict
		}
		if l.entries[intent.ID] == nil {
			alias := *prior
			alias.ID = intent.ID
			l.entries[intent.ID] = &alias
			if err := l.saveLocked(); err != nil {
				delete(l.entries, intent.ID)
				l.mu.Unlock()
				return nil, fmt.Errorf("%w: %v", ErrStore, err)
			}
		}
		result, err := replay(prior)
		l.mu.Unlock()
		return result, err
	}
	r := &record{ID: intent.ID, Scope: intent.Scope, Target: intent.Target, Hash: hash, Status: "claimed", UpdatedAt: time.Now().UnixNano()}
	l.entries[r.ID] = r
	if err := l.saveLocked(); err != nil {
		delete(l.entries, r.ID)
		l.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if r.Target != "" {
		l.targets[r.Target] = r
	}
	l.mu.Unlock()
	value, callErr := fn()
	raw, encodeErr := json.Marshal(nativeJSON(value))
	if callErr == nil {
		callErr = encodeErr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r.Status, r.Result = "accepted", raw
	if callErr != nil {
		r.Status, r.Error, r.Result = "outcome_unknown", callErr.Error(), nil
		var rejected rejection
		if errors.As(callErr, &rejected) {
			r.Status = "rejected"
			delete(l.targets, r.Target)
		}
	}
	r.UpdatedAt = time.Now().UnixNano()
	for id, alias := range l.entries {
		if alias != r && alias.Status != "rejected" && r.Target != "" && alias.Target == r.Target && alias.Hash == r.Hash && alias.Scope == r.Scope {
			*alias = *r
			alias.ID = id
		}
	}
	if err := l.saveLocked(); err != nil {
		r.Status, r.Result = "outcome_unknown", nil
		for _, alias := range l.entries {
			if alias.Status != "rejected" && alias.Target == r.Target && alias.Scope == r.Scope && alias.Hash == r.Hash {
				alias.Status, alias.Result = "outcome_unknown", nil
			}
		}
		return nil, fmt.Errorf("%w；结果回执落盘失败: %v", ErrUnknown, err)
	}
	var rejected rejection
	if callErr != nil && !errors.As(callErr, &rejected) {
		return nil, fmt.Errorf("%w: %w", ErrUnknown, callErr)
	}
	return raw, callErr
}

func (l *Ledger) saveLocked() error {
	if l.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(l.entries, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(l.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(l.path), ".command-receipts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), l.path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	} // Directory fsync is unavailable.
	directory, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
