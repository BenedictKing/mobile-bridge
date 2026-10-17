package httpd

// Fork-origin ledger (Python service.Bridge's message_actions-derived
// _fork_origin): child thread -> {id, host, title} of the chat it branched or
// migrated from. The phone reads it from timeline meta as forkedFrom and
// links back to the source chat.

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type forkOrigin struct {
	ID    string `json:"id"`
	Host  string `json:"host"`
	Title string `json:"title"`
}

type forkLedger struct {
	mu      sync.Mutex
	path    string
	entries map[string]forkOrigin
	order   []string
}

// LoadForks reads <dataDir>/forks.json.
func LoadForks(path string) *forkLedger {
	ledger := &forkLedger{path: path, entries: map[string]forkOrigin{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ledger
	}
	var decoded map[string]forkOrigin
	if json.Unmarshal(raw, &decoded) == nil {
		for child, origin := range decoded {
			if child == "" || origin.ID == "" {
				continue
			}
			ledger.entries[child] = origin
			ledger.order = append(ledger.order, child)
		}
	}
	return ledger
}

// Record keeps the first origin of a child (a retried create/migrate never
// overwrites it).
func (f *forkLedger) Record(childID string, origin forkOrigin) {
	if childID == "" || origin.ID == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.entries[childID]; exists {
		return
	}
	f.entries[childID] = origin
	f.order = append(f.order, childID)
	f.saveLocked()
}

func (f *forkLedger) Origin(childID string) (forkOrigin, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	origin, ok := f.entries[childID]
	return origin, ok
}

func (f *forkLedger) saveLocked() {
	if f.path == "" {
		return
	}
	dump := map[string]forkOrigin{}
	for _, child := range f.order {
		if origin := f.entries[child]; origin.ID != "" {
			dump[child] = origin
		}
	}
	raw, err := json.MarshalIndent(dump, "", "  ")
	if err != nil {
		return
	}
	tmp := f.path + ".tmp"
	if os.WriteFile(tmp, append(raw, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, f.path)
	}
}

// forkedFromMeta builds the timeline meta payload (nil when this thread has
// no recorded origin — Python always carries the key).
func (s *Server) forkedFromMeta(threadID string) any {
	if s.Forks == nil {
		return nil
	}
	origin, ok := s.Forks.Origin(threadID)
	if !ok {
		return nil
	}
	meta := jsoncompat.NewObject()
	meta.Set("id", origin.ID)
	meta.Set("host", origin.Host)
	meta.Set("title", origin.Title)
	return meta
}
