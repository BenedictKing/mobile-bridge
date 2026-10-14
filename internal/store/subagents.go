// subagents.go: the codex adapter for the generic child-agent discovery —
// thread_spawn_edges in the desktop sqlite index is the roster, and each
// child's rollout is a readable history (Python's CodexAdapter.subagent_*).
package store

import (
	"encoding/json"
	"os"
	"sync"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
)

// SubagentStore projects thread_spawn_edges rows into the generic Child
// vocabulary. Roster reads are signature-cached against the sqlite file so
// per-view decoration (SSE polls) never hammers the database.
type SubagentStore struct {
	store *SessionStore

	mu    sync.Mutex
	cache map[string]cachedRoster
}

type cachedRoster struct {
	signature [2]int64
	children  []subagents.Child
}

// Subagents returns the child-agent adapter for this store.
func (s *SessionStore) Subagents() *SubagentStore {
	return &SubagentStore{store: s, cache: map[string]cachedRoster{}}
}

func (a *SubagentStore) signature() [2]int64 {
	path, err := a.store.databasePath()
	if err != nil {
		return [2]int64{}
	}
	if info, err := os.Stat(path); err == nil {
		return [2]int64{info.ModTime().UnixNano(), info.Size()}
	}
	return [2]int64{}
}

// Children lists one parent's spawned-child threads.
func (a *SubagentStore) Children(parentID string) []subagents.Child {
	signature := a.signature()
	a.mu.Lock()
	if hit, ok := a.cache[parentID]; ok && hit.signature == signature {
		a.mu.Unlock()
		return hit.children
	}
	a.mu.Unlock()
	rows, err := a.store.ChildThreads(parentID)
	children := []subagents.Child{}
	if err == nil {
		for _, row := range rows {
			id, _ := row.MustGet("id").(string)
			if id == "" {
				continue
			}
			name, _ := row.MustGet("name").(string)
			if name == "" {
				name, _ = row.MustGet("title").(string)
			}
			if name == "" {
				name = id
			}
			threadSource, _ := row.MustGet("thread_source").(string)
			spawnStatus, _ := row.MustGet("spawn_status").(string)
			children = append(children, subagents.Child{
				ID: id, Name: name, AgentRole: threadSource, SpawnStatus: spawnStatus,
				UpdatedAtMs: threadUpdatedMs(row),
			})
		}
		if len(children) > 32 {
			children = children[:32]
		}
	}
	a.mu.Lock()
	if len(a.cache) > 128 {
		for key := range a.cache {
			delete(a.cache, key)
		}
	}
	a.cache[parentID] = cachedRoster{signature: signature, children: children}
	a.mu.Unlock()
	return children
}

// History reads one child thread's rollout without the desktop whitelist,
// normalized into the message vocabulary the flatten renders (Python pipes
// it through normalize_state).
func (a *SubagentStore) History(childID string) *jsoncompat.Object {
	history, err := a.store.HistoryForAny(childID, nil)
	if err != nil {
		return jsoncompat.NewObject()
	}
	state := jsoncompat.NewObject()
	for _, key := range []string{"id", "title", "cwd", "turns", "turnsPagination"} {
		if value, exists := history.Get(key); exists {
			state.Set(key, value)
		}
	}
	return model.NormalizeState(state, false)
}

// threadUpdatedMs folds the thread row's timestamps into epoch milliseconds.
func threadUpdatedMs(row *jsoncompat.Object) int64 {
	if value, exists := row.Get("updated_at_ms"); exists {
		if number, ok := value.(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				return parsed
			}
		}
	}
	if value, exists := row.Get("updated_at"); exists {
		if number, ok := value.(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				return parsed * 1000
			}
		}
	}
	return 0
}
