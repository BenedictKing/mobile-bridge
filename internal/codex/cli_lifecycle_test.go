//go:build !windows

package codex

import (
	"strings"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/model"
)

func cliReply(call, result *jsoncompat.Object) *jsoncompat.Object {
	return object(map[string]any{"id": call.MustGet("id"), "result": result})
}

func (f *cliFixture) intercept(fn func(*jsoncompat.Object) *jsoncompat.Object) {
	f.mu.Lock()
	f.onCall = fn
	f.mu.Unlock()
}

func attachCLIFixture(t *testing.T) (*Bridge, *cliFixture) {
	t.Helper()
	b, f := newCLIFixture(t)
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	return b, f
}

func TestCLIHistoryNeverAcquiresOrExtendsWriter(t *testing.T) {
	b, f := newCLIFixture(t)
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call("cli-thread", "thread-follower-load-complete-history", jsoncompat.NewObject(), time.Second); err != nil {
		t.Fatal(err)
	}
	if len(f.operations("thread/resume")) != 0 {
		t.Fatal("history acquired an execution owner")
	}
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	used := time.Now().Add(-cliIdleDelay - time.Second)
	b.cli.lastUsed["cli-thread"] = used
	b.mu.Unlock()
	if _, err := b.Watch("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Call("cli-thread", "thread-follower-load-complete-history", jsoncompat.NewObject(), time.Second); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	unchanged := b.cli.lastUsed["cli-thread"].Equal(used)
	b.mu.Unlock()
	if !unchanged {
		t.Fatal("reading history extended writer ownership")
	}
	b.cli.releaseIdle(time.Now())
	if len(f.operations("thread/unsubscribe")) != 1 {
		t.Fatal("idle history reader prevented release")
	}
}

func TestCLIIdleReleaseKeepsHistoryAndResumesOnlyForNewWrite(t *testing.T) {
	b, f := attachCLIFixture(t)
	if _, err := b.StartTurn("cli-thread", "first-message", "第一条消息"); err != nil {
		t.Fatal(err)
	}
	b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
	state, _, connected, connectionError := b.Snapshot("cli-thread")
	if connected || connectionError != "" || !strings.Contains(string(jsoncompat.EncodeCompact(state)), "实时回复") {
		t.Fatalf("release lost history or remained connected: %s", jsoncompat.EncodeCompact(state))
	}
	if model.NormalizeState(state, connected).MustGet("writerReleased") != true {
		t.Fatal("released session did not expose its reconnectable state")
	}
	if len(f.operations("thread/unsubscribe")) != 1 || !b.IsCLI("cli-thread") {
		t.Fatal("release must unsubscribe without deleting the durable route")
	}
	for i := 0; i < 3; i++ {
		if _, err := b.Watch("cli-thread", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.operations("thread/resume")) != 1 || len(f.operations("turn/start")) != 1 {
		t.Fatal("passive reads reacquired ownership or replayed a message")
	}
	if _, err := b.StartTurn("cli-thread", "second-message", "新消息"); err != nil {
		t.Fatal(err)
	}
	if len(f.operations("thread/resume")) != 2 || len(f.operations("turn/start")) != 2 {
		t.Fatal("new write did not resume exactly once")
	}
	state, _, connected, _ = b.Snapshot("cli-thread")
	if !connected || model.NormalizeState(state, connected).MustGet("writerReleased") == true {
		t.Fatal("resumed session still appeared released")
	}
	params := f.operations("turn/start")[1].MustGet("params").(*jsoncompat.Object)
	if params.MustGet("threadId") != "cli-thread" || params.MustGet("clientUserMessageId") != "second-message" {
		t.Fatal("handoff changed the thread or submission identity")
	}
}

func TestCLIMaintenanceAutomaticallyReleasesIdleThread(t *testing.T) {
	b, f := attachCLIFixture(t)
	b.mu.Lock()
	b.cli.lastUsed["cli-thread"] = time.Now().Add(-cliIdleDelay - time.Second)
	b.mu.Unlock()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.operations("thread/unsubscribe")) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("maintenance loop did not release the idle thread")
}

func TestCLICloseInterruptsPendingWrite(t *testing.T) {
	b, f := attachCLIFixture(t)
	entered, proceed, finished, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
		if call.MustGet("method") == "turn/start" {
			close(entered)
			<-proceed
		}
		return nil
	})
	go func() { _, _ = b.StartTurn("cli-thread", "pending", "消息"); close(finished) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(proceed)
		t.Fatal("write did not reach the runtime")
	}
	go func() { b.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		close(proceed)
		t.Fatal("shutdown waited for an in-flight write")
	}
	close(proceed)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("closing transport did not unblock the write")
	}
}

func TestCLIReleaseProtectsActiveAndUnknownWork(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Bridge, *cliFixture)
	}{
		{"idle grace", func(b *Bridge, _ *cliFixture) {
			b.mu.Lock()
			b.cli.lastUsed["cli-thread"] = time.Now().Add(cliIdleDelay)
			b.mu.Unlock()
		}},
		{"active turn", func(b *Bridge, _ *cliFixture) {
			b.cli.onEvent(object(map[string]any{"method": "turn/started", "params": object(map[string]any{
				"threadId": "cli-thread", "turn": object(map[string]any{"id": "running", "status": "inProgress"}),
			})}))
		}},
		{"pending approval", func(b *Bridge, _ *cliFixture) {
			b.cli.onRequest(object(map[string]any{"id": jsoncompat.Num(99), "method": "item/commandExecution/requestApproval",
				"params": object(map[string]any{"threadId": "cli-thread", "command": "go test ./..."})}))
		}},
		{"native active status", func(_ *Bridge, f *cliFixture) {
			f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
				if call.MustGet("method") != "thread/read" {
					return nil
				}
				thread, _ := jsoncompat.Clone(f.thread).(*jsoncompat.Object)
				thread.Set("status", object(map[string]any{"type": "active"}))
				return cliReply(call, object(map[string]any{"thread": thread}))
			})
		}},
		{"background terminal", func(_ *Bridge, f *cliFixture) {
			f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
				if call.MustGet("method") == "thread/backgroundTerminals/list" {
					return cliReply(call, object(map[string]any{"data": &jsoncompat.Array{Items: []any{object(map[string]any{"processId": "build"})}}}))
				}
				return nil
			})
		}},
		{"unsupported background probe", func(_ *Bridge, f *cliFixture) {
			f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
				if call.MustGet("method") == "thread/backgroundTerminals/list" {
					return cliReply(call, jsoncompat.NewObject())
				}
				return nil
			})
		}},
		{"active descendant on second page", func(_ *Bridge, f *cliFixture) {
			f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
				if call.MustGet("method") != "thread/list" {
					return nil
				}
				params := call.MustGet("params").(*jsoncompat.Object)
				if params.MustGet("ancestorThreadId") != "cli-thread" {
					t.Error("descendant lookup omitted its ancestor filter")
				}
				kinds := cliArray(params.MustGet("sourceKinds"))
				if kinds == nil || !strings.Contains(string(jsoncompat.EncodeCompact(kinds)), "subAgentThreadSpawn") || len(kinds.Items) != 5 {
					t.Error("descendant lookup omitted native subagent source variants")
				}
				if cliText(params.MustGet("cursor")) == "" {
					if _, exists := params.Get("cursor"); exists {
						t.Error("first-page cursor must be omitted, not empty")
					}
					return cliReply(call, object(map[string]any{"data": &jsoncompat.Array{}, "nextCursor": "page2"}))
				}
				return cliReply(call, object(map[string]any{"data": &jsoncompat.Array{Items: []any{object(map[string]any{
					"id": "child", "status": object(map[string]any{"type": "active"}),
				})}}}))
			})
		}},
		{"idle descendant with background work", func(_ *Bridge, f *cliFixture) {
			f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
				if call.MustGet("method") == "thread/list" {
					return cliReply(call, object(map[string]any{"data": &jsoncompat.Array{Items: []any{object(map[string]any{
						"id": "child", "status": object(map[string]any{"type": "idle"}),
					})}}}))
				}
				if call.MustGet("method") == "thread/backgroundTerminals/list" && call.MustGet("params").(*jsoncompat.Object).MustGet("threadId") == "child" {
					return cliReply(call, object(map[string]any{"data": &jsoncompat.Array{Items: []any{"running"}}}))
				}
				return nil
			})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, f := attachCLIFixture(t)
			test.setup(b, f)
			b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
			if len(f.operations("thread/unsubscribe")) != 0 {
				t.Fatal("unsafe thread was unsubscribed")
			}
		})
	}
}

func TestCLIReleaseDoesNotRaceSendOrNativeActivity(t *testing.T) {
	t.Run("in-flight send", func(t *testing.T) {
		b, f := attachCLIFixture(t)
		entered, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
			if call.MustGet("method") == "turn/start" {
				close(entered)
				<-proceed
			}
			return nil
		})
		go func() { _, err := b.StartTurn("cli-thread", "new-message", "消息"); finished <- err }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("send did not reach runtime")
		}
		b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
		close(proceed)
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
		if len(f.operations("thread/unsubscribe")) != 0 {
			t.Fatal("release slipped into an in-flight write")
		}
	})
	t.Run("activity during native probe", func(t *testing.T) {
		b, f := attachCLIFixture(t)
		f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
			if call.MustGet("method") == "thread/backgroundTerminals/list" {
				b.cli.onEvent(object(map[string]any{"method": "turn/started", "params": object(map[string]any{
					"threadId": "cli-thread", "turn": object(map[string]any{"id": "new-turn", "status": "inProgress"}),
				})}))
			}
			return nil
		})
		b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
		if len(f.operations("thread/unsubscribe")) != 0 {
			t.Fatal("native activity invalidated the release preflight")
		}
	})
}

func TestCLIUnsubscribeFailureCanRetryWithoutReplaying(t *testing.T) {
	b, f := attachCLIFixture(t)
	f.intercept(func(call *jsoncompat.Object) *jsoncompat.Object {
		if call.MustGet("method") == "thread/unsubscribe" {
			return object(map[string]any{"id": call.MustGet("id"), "error": object(map[string]any{"code": -32000, "message": "temporary failure"})})
		}
		return nil
	})
	b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
	_, _, connected, connectionError := b.Snapshot("cli-thread")
	if connected || connectionError == "" {
		t.Fatal("uncertain unsubscribe remained write-ready")
	}
	f.intercept(nil)
	b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
	if len(f.operations("thread/unsubscribe")) != 2 || len(f.operations("turn/start")) != 0 {
		t.Fatal("failed release was not retried independently of message dispatch")
	}
}

func TestCLIClosedNotificationsAndResolvedRequests(t *testing.T) {
	for _, method := range []string{"thread/closed", "thread/status/changed"} {
		t.Run(method, func(t *testing.T) {
			b, f := attachCLIFixture(t)
			b.cli.onEvent(object(map[string]any{"method": method, "params": object(map[string]any{
				"threadId": "cli-thread", "status": object(map[string]any{"type": "notLoaded"}),
			})}))
			_, _, connected, connectionError := b.Snapshot("cli-thread")
			if connected || connectionError != "" {
				t.Fatal("closed thread remained write-ready")
			}
			if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
				t.Fatal(err)
			}
			if len(f.operations("thread/resume")) != 2 {
				t.Fatal("closed notification did not invalidate the stale owner")
			}
		})
	}
	b, f := attachCLIFixture(t)
	b.cli.onRequest(object(map[string]any{"id": jsoncompat.Num(99), "method": "item/commandExecution/requestApproval",
		"params": object(map[string]any{"threadId": "cli-thread", "command": "go test ./..."})}))
	b.cli.onEvent(object(map[string]any{"method": "serverRequest/resolved", "params": object(map[string]any{
		"threadId": "cli-thread", "requestId": jsoncompat.Num(99),
	})}))
	b.cli.releaseIdle(time.Now().Add(cliIdleDelay + time.Second))
	if len(f.operations("thread/unsubscribe")) != 1 {
		t.Fatal("resolved native approval left a phantom release blocker")
	}
}
