package acpruntime

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func nativeTurn(id, status, text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", id)
	turn.Set("status", status)
	message := jsoncompat.NewObject()
	message.Set("id", "u-"+id)
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", text)
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	return turn
}

func nativeSession() *Session {
	view := acpview.NewView("sess-1", nil, "/w", func() float64 { return 0 })
	return &Session{ID: "sess-1", View: view, changed: make(chan struct{})}
}

func TestShouldApplyNativeKeepsSettledTip(t *testing.T) {
	session := nativeSession()
	settled := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "同一个问题")}}
	if !session.View.ApplyNative(settled) {
		t.Fatal("seed apply failed")
	}
	if session.shouldApplyNative(&acpview.NativeSnapshot{Turns: []*jsoncompat.Object{
		nativeTurn("t1", "completed", "同一个问题"),
	}}) {
		t.Fatal("settled duplicate must not be re-applied")
	}
	// Store lag: the ACP turn settled, the store still reports it running.
	if session.shouldApplyNative(&acpview.NativeSnapshot{Turns: []*jsoncompat.Object{
		nativeTurn("t1", "inProgress", "同一个问题"),
	}}) {
		t.Fatal("lagging store must not overwrite a settled ACP turn")
	}
	// A long-lived mismatch means the ACP settle was wrong, not store lag.
	session.nativeLagSince = time.Now().Add(-2 * nativeStoreLagGrace)
	if !session.shouldApplyNative(&acpview.NativeSnapshot{Turns: []*jsoncompat.Object{
		nativeTurn("t1", "inProgress", "同一个问题"),
	}}) {
		t.Fatal("persistent running store copy must reclaim the view")
	}
	// A new prompt replaces the tip; the append-only store still carries the
	// previous turn.
	if !session.shouldApplyNative(&acpview.NativeSnapshot{Turns: []*jsoncompat.Object{
		nativeTurn("t1", "completed", "同一个问题"),
		nativeTurn("t2", "inProgress", "下一个问题"),
	}}) {
		t.Fatal("a new prompt must apply")
	}
}

func TestShouldApplyNativeNeverRollsBackToAnOlderStore(t *testing.T) {
	session := nativeSession()
	current := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "旧问题")}}
	session.View.ApplyNative(current)
	session.View.BeginTurn("电话刚发的问题")
	older := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "旧问题")}}
	if session.shouldApplyNative(older) {
		t.Fatal("a store snapshot without the view's prompt must not apply")
	}
	caught := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{
		nativeTurn("t1", "completed", "旧问题"),
		nativeTurn("t2", "inProgress", "电话刚发的问题"),
	}}
	if !session.shouldApplyNative(caught) {
		t.Fatal("the caught-up store must apply")
	}
}

func TestShouldApplyNativeFollowsRunningTip(t *testing.T) {
	session := nativeSession()
	running := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "同一个问题")}}
	if !session.View.ApplyNative(running) {
		t.Fatal("seed apply failed")
	}
	updated := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "同一个问题")}}
	if !session.shouldApplyNative(updated) {
		t.Fatal("running tip updates must apply")
	}
}

func TestSyncNativeAppliesAndSignals(t *testing.T) {
	session := nativeSession()
	snapshot := &acpview.NativeSnapshot{Title: "真标题", Turns: []*jsoncompat.Object{
		nativeTurn("t1", "inProgress", "桌面问题"),
	}}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return snapshot, nil }
	before := session.sequence
	session.syncNative()
	if session.sequence <= before {
		t.Fatal("sync did not bump the sequence")
	}
	if session.View.Status != "active" || session.View.Title != "真标题" {
		t.Fatalf("view = %q/%v", session.View.Status, session.View.Title)
	}
	// Prompt-running sessions keep the ACP view authoritative.
	session.promptRunning = true
	snapshot.Turns = []*jsoncompat.Object{nativeTurn("t2", "inProgress", "另一个问题")}
	changedBefore := session.sequence
	session.syncNative()
	if session.sequence != changedBefore {
		t.Fatal("sync applied while a bridge prompt was running")
	}
}

func TestSyncNativeFiresSettleHook(t *testing.T) {
	session := nativeSession()
	settled := make(chan struct{}, 1)
	session.nativeSettle = func(*Session) { settled <- struct{}{} }
	running := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "inProgress", "桌面问题")}}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return running, nil }
	session.syncNative()
	if session.View.Status != "active" {
		t.Fatalf("status = %q", session.View.Status)
	}
	done := &acpview.NativeSnapshot{Turns: []*jsoncompat.Object{nativeTurn("t1", "completed", "桌面问题")}}
	session.native = func(string) (*acpview.NativeSnapshot, error) { return done, nil }
	session.syncNative()
	select {
	case <-settled:
	case <-time.After(time.Second):
		t.Fatal("settle hook did not fire")
	}
}

func TestSyncNativeClosedSessionIsSilent(t *testing.T) {
	session := nativeSession()
	session.closed = true
	session.native = func(string) (*acpview.NativeSnapshot, error) {
		t.Fatal("closed session must not poll")
		return nil, nil
	}
	session.syncNative()
}

func TestManagerFillsNativeMetaIntoListAndOpen(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 if method == "initialize": result={"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}
 elif method == "session/list": result={"sessions":[{"sessionId":"session-x","cwd":"/work","updatedAt":"2020-01-01T00:00:00Z"}]}
 elif method == "session/load": result={"sessionId":m.get("params",{}).get("sessionId")}
 else: result={}
 print(json.dumps({"jsonrpc":"2.0","id":ident,"result":result}),flush=True)
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewCommand([]string{python, "-u", path})
	manager.NativeMeta = func(string) (string, int64) { return "实况差别未体现", 1792283000000 }
	rows, err := manager.List(3 * time.Second)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%v err=%v", rows, err)
	}
	if title, _ := rows[0].MustGet("title").(string); title != "实况差别未体现" {
		t.Fatalf("title = %q", title)
	}
	if updated, _ := rows[0].MustGet("updatedAt").(json.Number).Int64(); updated != 1792283000000 {
		t.Fatalf("updatedAt = %d", updated)
	}
	session, err := manager.Open("session-x", "/work", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if session.View.Title != "实况差别未体现" {
		t.Fatalf("view title = %v", session.View.Title)
	}
	manager.Close()
}
