package notifications

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// appendMobile is Append with plain delivery keys (the sha256 identity the
// feed derives is an internal detail tests need not recompute).
func appendMobile(t *testing.T, feed *MobileEvents, keys []string, threadID, host, kind, provider string) {
	t.Helper()
	if err := feed.Append(keys, threadID, host, "标题", "正文", kind, provider); err != nil {
		t.Fatalf("Append(%v) failed: %v", keys, err)
	}
}

func TestMobileAppendIdempotentAndCursorMonotonic(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())

	// The same delivery keys (in any order) are one event: retries dedup.
	appendMobile(t, feed, []string{"a", "b"}, "thread-1", "local", "request", "")
	if snapshot := feed.Read(0, nil); snapshot.Cursor != 1 || len(snapshot.Events) != 1 {
		t.Fatalf("first append cursor=%d events=%d", snapshot.Cursor, len(snapshot.Events))
	}
	appendMobile(t, feed, []string{"b", "a"}, "thread-1", "local", "request", "")
	if snapshot := feed.Read(0, nil); snapshot.Cursor != 1 || len(snapshot.Events) != 1 {
		t.Fatalf("dedup cursor=%d events=%d", snapshot.Cursor, len(snapshot.Events))
	}

	// Distinct keys advance the cursor by one each time.
	appendMobile(t, feed, []string{"c"}, "thread-1", "local", "request", "")
	appendMobile(t, feed, []string{"d"}, "thread-1", "local", "completion", "")
	snapshot := feed.Read(0, nil)
	if snapshot.Cursor != 3 || len(snapshot.Events) != 3 {
		t.Fatalf("cursor=%d events=%d", snapshot.Cursor, len(snapshot.Events))
	}
	for index, event := range snapshot.Events {
		if event.Sequence != int64(index+1) {
			t.Fatalf("event %d sequence=%d", index, event.Sequence)
		}
	}

	// Empty delivery keys are refused before any state changes.
	if err := feed.Append(nil, "t", "h", "标题", "正文", "request", ""); err == nil || err.Error() != "通知投递键缺失" {
		t.Fatalf("empty keys err=%v", err)
	}
	if snapshot := feed.Read(0, nil); snapshot.Cursor != 3 {
		t.Fatalf("cursor moved on refused append: %d", snapshot.Cursor)
	}
}

func TestMobileRingBufferCapKeepsPendingDecoupled(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())
	for index := 0; index < mobileEventLimit+5; index++ {
		appendMobile(t, feed, []string{"k" + strconv.Itoa(index)}, "t", "h", "request", "")
	}
	snapshot := feed.Read(0, nil)
	if len(snapshot.Events) != mobileEventLimit {
		t.Fatalf("events=%d want %d", len(snapshot.Events), mobileEventLimit)
	}
	if snapshot.Events[0].Sequence != 6 || snapshot.Events[len(snapshot.Events)-1].Sequence != 205 {
		t.Fatalf("window=[%d,%d]", snapshot.Events[0].Sequence, snapshot.Events[len(snapshot.Events)-1].Sequence)
	}
	if snapshot.Cursor != 205 {
		t.Fatalf("cursor=%d", snapshot.Cursor)
	}
	// The unread layer counts every append, not just the retained window.
	if snapshot.Unread["codex"] != 205 {
		t.Fatalf("unread=%d want 205", snapshot.Unread["codex"])
	}
	// Acknowledging past the window still clears the pending layer.
	if err := feed.Acknowledge("codex", snapshot.StreamID, snapshot.Cursor); err != nil {
		t.Fatalf("ack failed: %v", err)
	}
	if after := feed.Read(0, nil); after.Unread["codex"] != 0 || len(after.Events) != mobileEventLimit {
		t.Fatalf("after ack unread=%d events=%d", after.Unread["codex"], len(after.Events))
	}
}

func TestMobileProviderOwnershipAndReadAfter(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())
	appendMobile(t, feed, []string{"k1"}, "thread-z", "local", "request", "zcode")
	appendMobile(t, feed, []string{"k2"}, "thread-z", "local", "completion", "")
	appendMobile(t, feed, []string{"k3"}, "thread-c", "local", "request", "")

	snapshot := feed.Read(0, nil)
	// Ownership is per thread|host: a later append without a provider keeps
	// the recorded one; unknown threads fall back to codex.
	want := []struct{ provider, threadID string }{{"zcode", "thread-z"}, {"zcode", "thread-z"}, {"codex", "thread-c"}}
	for index, expected := range want {
		view := snapshot.Events[index]
		if view.Provider != expected.provider || view.ClientID != expected.provider || view.ThreadID != expected.threadID {
			t.Fatalf("event %d provider=%q client=%q thread=%q", index, view.Provider, view.ClientID, view.ThreadID)
		}
	}
	if snapshot.Unread["zcode"] != 2 || snapshot.Unread["codex"] != 1 {
		t.Fatalf("unread=%v", snapshot.Unread)
	}

	// `after` returns only the incremental tail.
	incremental := feed.Read(1, nil)
	if len(incremental.Events) != 2 || incremental.Events[0].Sequence != 2 {
		t.Fatalf("incremental events=%d first=%d", len(incremental.Events), incremental.Events[0].Sequence)
	}
	// An `after` older than the bounded window still yields the retained
	// window (not an error, not empty) so a reconnect can rebuild state.
	overflow := feed.Read(-3, nil)
	if len(overflow.Events) != 3 {
		t.Fatalf("negative after events=%d", len(overflow.Events))
	}

	// The per-provider summary mirrors the newest event per provider.
	if len(snapshot.Summary) != 2 ||
		snapshot.Summary[0]["provider"] != "codex" || snapshot.Summary[0]["sequence"] != int64(3) ||
		snapshot.Summary[1]["provider"] != "zcode" || snapshot.Summary[1]["sequence"] != int64(2) {
		t.Fatalf("summary=%v", snapshot.Summary)
	}
}

func TestMobileAcknowledgeProviderIsolationAndMaxSemantics(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())
	appendMobile(t, feed, []string{"c1"}, "thread-c", "local", "request", "")
	appendMobile(t, feed, []string{"z1"}, "thread-z", "local", "request", "zcode")
	appendMobile(t, feed, []string{"c2"}, "thread-c", "local", "request", "")
	stream := feed.Read(0, nil).StreamID

	// Reading codex leaves zcode's bucket untouched.
	if err := feed.Acknowledge("codex", stream, 3); err != nil {
		t.Fatalf("ack codex: %v", err)
	}
	snapshot := feed.Read(0, nil)
	if snapshot.Seen["codex"] != 3 || snapshot.Unread["codex"] != 0 {
		t.Fatalf("codex seen=%v unread=%v", snapshot.Seen["codex"], snapshot.Unread["codex"])
	}
	if snapshot.Unread["zcode"] != 1 || snapshot.Seen["zcode"] != 0 {
		t.Fatalf("zcode seen=%v unread=%v", snapshot.Seen["zcode"], snapshot.Unread["zcode"])
	}
	if snapshot.Events[1].Unread != true {
		t.Fatalf("zcode event should stay unread")
	}

	// A stale phone cannot rewind another device's read cursor (max semantics):
	// the lower through is a covered no-op, seen stays at 3.
	if err := feed.Acknowledge("codex", stream, 1); err != nil {
		t.Fatalf("stale ack errored: %v", err)
	}
	if snapshot := feed.Read(0, nil); snapshot.Seen["codex"] != 3 {
		t.Fatalf("codex seen rewound to %v", snapshot.Seen["codex"])
	}

	// all fans out to every provider that has activity.
	if err := feed.Acknowledge("all", stream, 3); err != nil {
		t.Fatalf("ack all: %v", err)
	}
	if snapshot := feed.Read(0, nil); snapshot.Unread["zcode"] != 0 || snapshot.Unread["codex"] != 0 {
		t.Fatalf("after all unread=%v", snapshot.Unread)
	}
}

func TestMobileAcknowledgeRejectsInvalidArguments(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())
	appendMobile(t, feed, []string{"k1"}, "thread-c", "local", "request", "")
	stream := feed.Read(0, nil).StreamID

	cases := []struct {
		name     string
		provider string
		stream   string
		through  int64
		want     string
	}{
		{"未知 provider", "gemini", stream, 1, "通知已读参数无效"},
		{"负游标", "codex", stream, -1, "通知已读参数无效"},
		{"streamId 不符", "codex", "not-the-stream", 1, "通知状态已变化，请刷新后重试"},
		{"游标越界", "codex", stream, 2, "通知状态已变化，请刷新后重试"},
	}
	for _, item := range cases {
		if err := feed.Acknowledge(item.provider, item.stream, item.through); err == nil || err.Error() != item.want {
			t.Fatalf("%s err=%v want %q", item.name, err, item.want)
		}
	}
}

func TestMobilePersistenceReloadKeepsStreamAndRebuildsPending(t *testing.T) {
	dir := t.TempDir()
	feed := NewMobileEvents(dir)
	appendMobile(t, feed, []string{"k1"}, "thread-c", "local", "request", "")
	appendMobile(t, feed, []string{"k2"}, "thread-z", "local", "request", "zcode")
	appendMobile(t, feed, []string{"k3"}, "thread-z", "local", "request", "")
	stream := feed.Read(0, nil).StreamID
	if err := feed.Acknowledge("zcode", stream, 2); err != nil {
		t.Fatalf("ack: %v", err)
	}

	// A reload keeps the stream identity (devices stay aligned) and the feed.
	reloaded := NewMobileEvents(dir)
	if checkpoint := reloaded.Checkpoint(); checkpoint["streamId"] != stream || checkpoint["cursor"] != int64(3) {
		t.Fatalf("reloaded checkpoint=%v", checkpoint)
	}
	if snapshot := reloaded.Read(0, nil); len(snapshot.Events) != 3 || snapshot.Unread["zcode"] != 1 {
		t.Fatalf("reloaded events=%d unread=%v", len(snapshot.Events), snapshot.Unread)
	}

	// An older file without the pending layer rebuilds it from seen cursors.
	raw, err := os.ReadFile(filepath.Join(dir, "mobile-events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "pending")
	trimmed, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mobile-events.json"), trimmed, 0o600); err != nil {
		t.Fatal(err)
	}
	upgraded := NewMobileEvents(dir)
	snapshot := upgraded.Read(0, nil)
	if snapshot.Unread["zcode"] != 1 || snapshot.Unread["codex"] != 1 {
		t.Fatalf("rebuilt unread=%v", snapshot.Unread)
	}
	// A corrupt file falls back to a fresh feed rather than failing.
	if err := os.WriteFile(filepath.Join(dir, "mobile-events.json"), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := NewMobileEvents(dir)
	if checkpoint := fresh.Checkpoint(); checkpoint["cursor"] != int64(0) {
		t.Fatalf("corrupt reload cursor=%v", checkpoint["cursor"])
	}
}

func TestMobileVersionWaitAndClose(t *testing.T) {
	feed := NewMobileEvents(t.TempDir())
	version := feed.Version()

	// Wait times out quietly when nothing changes.
	started := time.Now()
	feed.Wait(version, 30*time.Millisecond)
	if elapsed := time.Since(started); elapsed < 20*time.Millisecond {
		t.Fatalf("wait returned early: %v", elapsed)
	}
	if feed.Version() != version {
		t.Fatalf("version moved without changes")
	}

	// An append bumps the generation and wakes the waiter before the timeout.
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = feed.Append([]string{"wake"}, "t", "h", "标题", "正文", "request", "")
	}()
	started = time.Now()
	feed.Wait(version, 2*time.Second)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("wait blocked %v", elapsed)
	}
	if feed.Version() == version {
		t.Fatalf("version unchanged after append")
	}

	// Close wakes waiters immediately; later waits return at once.
	current := feed.Version()
	feed.Close()
	started = time.Now()
	feed.Wait(current, time.Hour)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("close did not wake waiter: %v", elapsed)
	}
	if !feed.Closed() {
		t.Fatalf("closed flag missing")
	}
}
