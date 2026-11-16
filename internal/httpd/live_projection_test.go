package httpd

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpgoal"
	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/subagents"
)

func projectionCacheFixture(t testing.TB) (*Server, *acpruntime.Manager, *acpruntime.Session) {
	t.Helper()
	manager := acpruntime.NewCommand(nil)
	manager.Kind = "kimi"
	manager.NativeInterval = time.Hour
	manager.Created = acpruntime.LoadCreated(filepath.Join(t.TempDir(), "created.json"))
	turns := slotTestView(0, strings.Repeat("long conversation ", 4000)).MustGet("turns").(*jsoncompat.Array)
	snapshot := &acpview.NativeSnapshot{Title: "CPU regression", Turns: []*jsoncompat.Object{turns.Items[0].(*jsoncompat.Object)}}
	manager.NativeTranscript = func(string) (*acpview.NativeSnapshot, error) { return snapshot, nil }
	session, err := manager.OpenHistory("cpu-cache", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "cpu-test")
	server.ACPAgents = map[string]*acpruntime.Manager{"kimi": manager}
	return server, manager, session
}

func TestLiveProjectionCodexSharesAndUpdates(t *testing.T) {
	harness := newConcurrencyHarness(t)
	t.Cleanup(harness.bridge.Close)
	harness.bridge.Publish(harness.threadID, codexState(harness.threadID, "first", "output", false))
	_, before, ok := harness.server.refreshLiveProjection(harness.threadID)
	if !ok {
		t.Fatal("missing follower snapshot")
	}
	slot := harness.server.lookupProjection(harness.threadID)
	view := slot.live.view
	for range 10 {
		harness.server.refreshLiveProjection(harness.threadID)
	}
	if slot.live.view != view {
		t.Fatal("unchanged Codex follower copied its history")
	}
	harness.bridge.Publish(harness.threadID, codexState(harness.threadID, "second", "output", false))
	_, after, _ := harness.server.refreshLiveProjection(harness.threadID)
	if after <= before || slot.live.view == view || !strings.Contains(string(jsoncompat.EncodeCompact(slot.live.view)), "second") {
		t.Fatal("Codex publication did not refresh the shared view")
	}
}

func TestLiveProjectionSharesUnchangedView(t *testing.T) {
	server, _, session := projectionCacheFixture(t)
	_, initial, ok := server.refreshLiveProjection(session.ID)
	if !ok {
		t.Fatal("missing initial projection")
	}
	slot := server.lookupProjection(session.ID)
	view := slot.live.view
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for range 25 {
				_, current, ok := server.refreshLiveProjection(session.ID)
				if !ok || current != initial {
					t.Errorf("idle projection changed: %d -> %d", initial, current)
				}
			}
		}()
	}
	readers.Wait()
	if slot.live.view != view {
		t.Fatal("idle subscribers rebuilt the long conversation")
	}
	// A periodic auxiliary check also leaves the full snapshot untouched.
	slot.live.checkedAt = time.Time{}
	server.refreshLiveProjection(session.ID)
	if slot.live.view != view {
		t.Fatal("unchanged auxiliary metadata rebuilt the conversation")
	}
	// Session replacement with the same source sequence must not reuse old data.
	session.Close()
	replacement, err := server.ACPAgents["kimi"].OpenHistory(session.ID, session.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	_, updated, _ := server.refreshLiveProjection(session.ID)
	if replacement == session || slot.live.view == view || updated <= initial {
		t.Fatal("replacement session reused the old projection")
	}
}

func TestLiveProjectionDoesNotRetainUnknownThreads(t *testing.T) {
	server := New(auth.NewAuth(auth.Config{Mode: "none"}), fsys{}, 8790, "unknown")
	if _, _, ok := server.refreshLiveProjection("missing-thread"); ok {
		t.Fatal("missing thread reported a projection")
	}
	if server.lookupProjection("missing-thread") != nil {
		t.Fatal("missing thread retained an empty projection")
	}
}

func TestLiveProjectionGoalChangesWithoutSessionSequence(t *testing.T) {
	server, _, session := projectionCacheFixture(t)
	var err error
	server.ACPGoals, err = acpgoal.Open(filepath.Join(t.TempDir(), "goals.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, before, _ := server.refreshLiveProjection(session.ID)
	sourceSequence := session.Sequence()
	if _, err := server.ACPGoals.Create("g", "create", "kimi", session.ID, "finish goal", 2); err != nil {
		t.Fatal(err)
	}
	slot := server.lookupProjection(session.ID)
	slot.live.checkedAt = time.Time{} // simulate the supplementary check deadline
	_, after, _ := server.refreshLiveProjection(session.ID)
	if after <= before || session.Sequence() != sourceSequence {
		t.Fatalf("goal needs an independent revision: before=%d after=%d source=%d", before, after, session.Sequence())
	}
	goal, _ := slot.live.view.MustGet("goal").(*jsoncompat.Object)
	if goal == nil || goal.MustGet("objective") != "finish goal" {
		t.Fatal("goal update did not reach the view")
	}
	// The snapshot SSE/poll payload must use the same cursor as incremental SSE.
	if server.View(session.ID).(*jsoncompat.Object).MustGet("sequence") != jsoncompat.Num(after) {
		t.Fatal("snapshot and incremental cursors diverged")
	}
}

type projectionChildStore struct{ revision int64 }

func (s *projectionChildStore) Children(string) []subagents.Child {
	return []subagents.Child{{ID: "child", Name: "child", SpawnStatus: "running", UpdatedAtMs: s.revision}}
}
func (s *projectionChildStore) History(string) *jsoncompat.Object { return jsoncompat.NewObject() }

func TestLiveProjectionSubagentAndConnectionChanges(t *testing.T) {
	server, manager, session := projectionCacheFixture(t)
	children := &projectionChildStore{revision: 1}
	manager.Subagents = subagents.NewFlattener(children)
	_, before, _ := server.refreshLiveProjection(session.ID)
	slot := server.lookupProjection(session.ID)
	children.revision = 2
	slot.live.checkedAt = time.Time{}
	_, after, _ := server.refreshLiveProjection(session.ID)
	if after <= before || slot.live.view.MustGet("subagentRevision") != jsoncompat.Num(2) {
		t.Fatal("independent subagent update was missed")
	}
	session.Close()
	_, closed, _ := server.refreshLiveProjection(session.ID)
	if closed <= after || slot.live.view.MustGet("connected") != false {
		t.Fatal("closed connection did not invalidate the view")
	}
}

func BenchmarkIdleSharedProjection(b *testing.B) {
	server, _, session := projectionCacheFixture(b)
	server.refreshLiveProjection(session.ID)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		server.refreshLiveProjection(session.ID)
	}
}

func BenchmarkUncachedACPView(b *testing.B) {
	server, manager, session := projectionCacheFixture(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		server.acpViewState("kimi", manager, session)
	}
}
