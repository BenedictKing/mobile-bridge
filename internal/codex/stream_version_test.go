package codex

import (
	"strings"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

// v13 captured from the desktop uses canonical history and the same
// revision/baseRevision JSON patches as v11. Keep the old dialect covered.
func TestCanonicalStreamVersions(t *testing.T) {
	for _, version := range []int{11, 13} {
		t.Run(jsoncompat.PyStr(jsoncompat.Num(version)), func(t *testing.T) {
			bridge := New(t.TempDir(), "unused")
			defer bridge.Close()
			state, err := jsoncompat.Unmarshal([]byte(`{"id":"thread-1","turns":[],"turnHistory":{"kind":"canonical","history":{"entitiesByKey":{"tail:0:turn-1":{"turnId":"turn-1","status":"inProgress","items":[{"id":"answer-1","type":"agentMessage","text":"first"}]}},"islands":[{"entries":[{"value":"tail:0:turn-1"}]}]}}}`))
			if err != nil {
				t.Fatal(err)
			}
			snapshot := object(map[string]any{"type": "snapshot", "revision": jsoncompat.Num(1), "conversationState": state})
			message := event("thread-1", snapshot, "owner")
			message.Set("version", jsoncompat.Num(version))
			bridge.handleEvent(message)
			_, changed, ready := bridge.Revision("thread-1")
			if !ready {
				t.Fatal("supported snapshot was discarded")
			}
			patch := object(map[string]any{"type": "patches", "baseRevision": jsoncompat.Num(1), "revision": jsoncompat.Num(2),
				"patches": &jsoncompat.Array{Items: []any{object(map[string]any{
					"op": "replace", "path": &jsoncompat.Array{Items: []any{"turnHistory", "history", "entitiesByKey", "tail:0:turn-1", "items", jsoncompat.Num(0), "text"}}, "value": "updated",
				})}}})
			message = event("thread-1", patch, "owner")
			message.Set("version", jsoncompat.Num(version))
			bridge.handleEvent(message)
			select {
			case <-changed:
			default:
				t.Fatal("patch did not wake SSE subscriber")
			}
			current, sequence, connected, problem := bridge.Snapshot("thread-1")
			view := model.NormalizeState(current, connected)
			if !connected || problem != "" || sequence != 2 || len(model.OrderedTurns(current).Items) != 1 ||
				!strings.Contains(string(jsoncompat.EncodeCompact(view)), "updated") {
				t.Fatalf("canonical patch was not normalized: connected=%v sequence=%d error=%s", connected, sequence, problem)
			}
		})
	}
}

func TestUnsupportedStreamVersionIsVisibleAndDeduplicated(t *testing.T) {
	bridge := New(t.TempDir(), "unused")
	defer bridge.Close()
	snapshot := object(map[string]any{"type": "snapshot", "revision": jsoncompat.Num(1), "conversationState": object(map[string]any{"id": "thread-1"})})
	message := event("thread-1", snapshot, "owner")
	message.Set("version", jsoncompat.Num(99))
	bridge.handleEvent(message)
	_, first, connected, problem := bridge.Snapshot("thread-1")
	if connected || first == 0 || !strings.Contains(problem, "99") {
		t.Fatalf("unsupported version was silent: connected=%v sequence=%d error=%q", connected, first, problem)
	}
	bridge.handleEvent(message)
	if sequence, _ := bridge.LiveSequence("thread-1"); sequence != first {
		t.Fatal("repeated unsupported event caused a notification storm")
	}
	message.Set("version", jsoncompat.Num(13))
	bridge.handleEvent(message)
	_, _, connected, problem = bridge.Snapshot("thread-1")
	if !connected || problem != "" {
		t.Fatal("supported snapshot did not restore the connection")
	}
}
