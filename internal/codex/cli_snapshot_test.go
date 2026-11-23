package codex

import (
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func snapshotTurn(id, text string) *jsoncompat.Object {
	return object(map[string]any{
		"id": id, "status": "completed",
		"items": &jsoncompat.Array{Items: []any{object(map[string]any{
			"id": id + "-message", "type": "agentMessage", "text": text,
		})}},
	})
}

func TestCLIFullReadReplacesRemovedHistory(t *testing.T) {
	b, fixture := newCLIFixture(t)
	kept := snapshotTurn("kept", "current reply")
	kept.MustGet("items").(*jsoncompat.Array).Items = append(kept.MustGet("items").(*jsoncompat.Array).Items,
		object(map[string]any{"id": "removed-item", "type": "agentMessage", "text": "removed reply"}))
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{kept, snapshotTurn("removed", "removed turn")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "updated reply")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, connected, _ := b.Snapshot("cli-thread")
	turns := cliArray(state.MustGet("turns"))
	if connected || len(turns.Items) != 1 {
		t.Fatalf("full read must remove old turns without acquiring a writer: %s", jsoncompat.EncodeCompact(state))
	}
	items := cliArray(turns.Items[0].(*jsoncompat.Object).MustGet("items"))
	if len(items.Items) != 1 || items.Items[0].(*jsoncompat.Object).MustGet("text") != "updated reply" {
		t.Fatalf("full read retained a removed item: %s", jsoncompat.EncodeCompact(state))
	}
}

func TestCLIFullResumeReplacesRemovedHistory(t *testing.T) {
	b, fixture := newCLIFixture(t)
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("removed", "old reply")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	fixture.thread.Set("turns", &jsoncompat.Array{})
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, connected, _ := b.Snapshot("cli-thread")
	if !connected || len(cliArray(state.MustGet("turns")).Items) != 0 {
		t.Fatalf("empty full resume must clear removed history: %s", jsoncompat.EncodeCompact(state))
	}
}

func TestCLIMetadataReadPreservesHistory(t *testing.T) {
	b, fixture := newCLIFixture(t)
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "reply")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	fixture.thread.Delete("turns")
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, _, _ := b.Snapshot("cli-thread")
	if len(cliArray(state.MustGet("turns")).Items) != 1 {
		t.Fatal("a response without turns must not erase history")
	}
}

func TestCLIFullReadRetriesSnapshotThatRacedWithDelta(t *testing.T) {
	b, fixture := newCLIFixture(t)
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "A"), snapshotTurn("removed", "old")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	reads := 0
	fixture.mu.Lock()
	fixture.onCall = func(message *jsoncompat.Object) *jsoncompat.Object {
		if message.MustGet("method") != "thread/read" {
			return nil
		}
		reads++
		if reads != 1 {
			return nil
		}
		stale := jsoncompat.Clone(fixture.thread)
		fixture.mu.Lock()
		connection := fixture.connections[0]
		fixture.mu.Unlock()
		fixture.send(connection, object(map[string]any{
			"method": "item/agentMessage/delta", "params": object(map[string]any{
				"threadId": "cli-thread", "turnId": "kept", "itemId": "kept-message", "delta": "B",
			}),
		}))
		fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "AB")}})
		return object(map[string]any{"id": message.MustGet("id"), "result": object(map[string]any{"thread": stale})})
	}
	fixture.mu.Unlock()
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, _, _ := b.Snapshot("cli-thread")
	turns := cliArray(state.MustGet("turns"))
	if reads != 2 || len(turns.Items) != 1 {
		t.Fatalf("read must retry the stale snapshot and remove old turns: reads=%d state=%s", reads, jsoncompat.EncodeCompact(state))
	}
	items := cliArray(turns.Items[0].(*jsoncompat.Object).MustGet("items"))
	if items.Items[0].(*jsoncompat.Object).MustGet("text") != "AB" {
		t.Fatalf("late snapshot regressed newer content: %s", jsoncompat.EncodeCompact(state))
	}
}

func TestCLIFullResumeRefreshesSnapshotWithoutRepeatingResume(t *testing.T) {
	b, fixture := newCLIFixture(t)
	fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "A")}})
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.onCall = func(message *jsoncompat.Object) *jsoncompat.Object {
		if message.MustGet("method") != "thread/resume" {
			return nil
		}
		stale := jsoncompat.Clone(fixture.thread)
		fixture.mu.Lock()
		connection := fixture.connections[0]
		fixture.mu.Unlock()
		fixture.send(connection, object(map[string]any{
			"method": "item/agentMessage/delta", "params": object(map[string]any{
				"threadId": "cli-thread", "turnId": "kept", "itemId": "kept-message", "delta": "B",
			}),
		}))
		fixture.thread.Set("turns", &jsoncompat.Array{Items: []any{snapshotTurn("kept", "AB")}})
		return object(map[string]any{"id": message.MustGet("id"), "result": object(map[string]any{
			"thread": stale, "model": "gpt-test", "reasoningEffort": "high",
		})})
	}
	fixture.mu.Unlock()
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	state, _, connected, _ := b.Snapshot("cli-thread")
	turn := cliArray(state.MustGet("turns")).Items[0].(*jsoncompat.Object)
	item := cliArray(turn.MustGet("items")).Items[0].(*jsoncompat.Object)
	if !connected || item.MustGet("text") != "AB" || state.MustGet("latestModel") != "gpt-test" {
		t.Fatalf("resume refresh lost events, ownership or model settings: %s", jsoncompat.EncodeCompact(state))
	}
	if len(fixture.operations("thread/resume")) != 1 {
		t.Fatal("refresh must not resume twice")
	}
}
