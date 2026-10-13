package notifications

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const outcomeRetention = 7 * 24 * time.Hour
const outcomeLimit = 2000

type OutcomeLedger struct {
	DataDir  string
	mu       sync.Mutex
	outcomes map[string]*jsoncompat.Object
}

func NewOutcomeLedger(dataDir string) *OutcomeLedger {
	ledger := &OutcomeLedger{DataDir: dataDir, outcomes: map[string]*jsoncompat.Object{}}
	ledger.load()
	return ledger
}
func (l *OutcomeLedger) path() string { return l.DataDir + "/notification-outcomes.json" }

func (l *OutcomeLedger) Record(host, threadID, event, title string, at time.Time) error {
	if host == "" || threadID == "" || event == "" {
		return errors.New("通知 outcome 标识无效")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.outcomes == nil {
		l.outcomes = map[string]*jsoncompat.Object{}
	}
	key := outcomeKey(host, threadID, event)
	row := jsoncompat.NewObject()
	row.Set("kind", "completion")
	row.Set("host", host)
	row.Set("id", threadID)
	row.Set("event", event)
	row.Set("title", title)
	row.Set("at", at.Unix())
	l.outcomes[key] = row
	l.trimLocked()
	return l.saveLocked()
}

func (l *OutcomeLedger) Reconcile(now time.Time, delivered func(host, threadID, event string) bool, enabled func(host, threadID string) bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := false
	for key, row := range l.outcomes {
		stamp := numberValue(row.MustGet("at"))
		host, _ := row.MustGet("host").(string)
		threadID, _ := row.MustGet("id").(string)
		event, _ := row.MustGet("event").(string)
		if stamp <= 0 || now.Sub(time.Unix(stamp, 0)) > outcomeRetention || (enabled != nil && !enabled(host, threadID)) || (delivered != nil && delivered(host, threadID, event)) {
			delete(l.outcomes, key)
			changed = true
		}
	}
	if changed {
		return l.saveLocked()
	}
	return nil
}

func (l *OutcomeLedger) Snapshot() *jsoncompat.Object {
	l.mu.Lock()
	defer l.mu.Unlock()
	result := jsoncompat.NewObject()
	for key, row := range l.outcomes {
		result.Set(key, jsoncompat.Clone(row))
	}
	return result
}

func (l *OutcomeLedger) load() {
	raw, err := os.ReadFile(l.path())
	if err != nil {
		return
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return
	}
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		return
	}
	for _, key := range object.Keys() {
		if row, ok := object.MustGet(key).(*jsoncompat.Object); ok {
			l.outcomes[key] = row
		}
	}
}
func (l *OutcomeLedger) saveLocked() error {
	if err := os.MkdirAll(l.DataDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(l.path(), append(jsoncompat.EncodePythonIndent2(l.snapshotLocked()), '\n'), 0o600)
}
func (l *OutcomeLedger) snapshotLocked() *jsoncompat.Object {
	result := jsoncompat.NewObject()
	keys := make([]string, 0, len(l.outcomes))
	for key := range l.outcomes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result.Set(key, l.outcomes[key])
	}
	return result
}
func (l *OutcomeLedger) trimLocked() {
	if len(l.outcomes) <= outcomeLimit {
		return
	}
	keys := make([]string, 0, len(l.outcomes))
	for key := range l.outcomes {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return numberValue(l.outcomes[keys[i]].MustGet("at")) < numberValue(l.outcomes[keys[j]].MustGet("at"))
	})
	for _, key := range keys[:len(keys)-outcomeLimit] {
		delete(l.outcomes, key)
	}
}
func outcomeKey(host, threadID, event string) string {
	key := jsoncompat.NewObject()
	key.Set("kind", "completion")
	key.Set("host", host)
	key.Set("id", threadID)
	key.Set("event", event)
	return string(jsoncompat.EncodePythonDefault(key))
}
func numberValue(value any) int64 {
	switch number := value.(type) {
	case json.Number:
		parsed, _ := number.Int64()
		return parsed
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return 0
	}
}
