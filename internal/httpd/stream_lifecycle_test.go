package httpd

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type streamTestBridge struct {
	*scriptedSessionBridge
	reads atomic.Int64
	stop  atomic.Bool
}

func (b *streamTestBridge) Session(string) *ScriptedSession {
	return &ScriptedSession{Epoch: "scripted-epoch", Sequence: 7, Live: func() (string, int, bool) {
		b.reads.Add(1)
		if b.stop.Load() {
			return "scripted-epoch", 8, true
		}
		return "scripted-epoch", 7, true
	}}
}

type streamTestWriter struct {
	*httptest.ResponseRecorder
	first     chan struct{}
	once      sync.Once
	fail      bool
	flushFail bool
}

func (w *streamTestWriter) Write(value []byte) (int, error) {
	w.once.Do(func() { close(w.first) })
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(value)
}

func TestStreamStopsOnCancelAndErrors(t *testing.T) {
	for _, mode := range []string{"snapshot", "incremental"} {
		for _, failure := range []string{"cancel", "write_error", "flush_error", "bridge_close"} {
			t.Run(mode+"/"+failure, func(t *testing.T) {
				f := newMobileAPIFixture(t, "none")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				request := f.build(http.MethodGet, "/events", "", true).WithContext(ctx)
				writer := &streamTestWriter{ResponseRecorder: httptest.NewRecorder(),
					first: make(chan struct{}), fail: failure == "write_error", flushFail: failure == "flush_error"}
				bridge := &streamTestBridge{scriptedSessionBridge: newScriptedSessionBridge()}
				var closeOnce sync.Once
				stopBridge := func() { closeOnce.Do(func() { close(bridge.closed) }) }
				h := &handler{server: f.server, writer: &trackedWriter{ResponseWriter: writer}, request: request}
				query := map[string][]string{}
				if mode == "incremental" {
					query["incremental"] = []string{"1"}
				}
				done := make(chan struct{})
				go func() {
					defer close(done)
					h.stream(bridge, "cpu-diagnostic", query)
				}()
				defer func() {
					stopBridge()
					bridge.stop.Store(true)
					select {
					case <-done:
					case <-time.After(2 * time.Second):
						t.Error("diagnostic cleanup did not stop stream")
					}
				}()
				select {
				case <-writer.first:
				case <-time.After(2 * time.Second):
					t.Fatal("stream produced no frame")
				}
				if failure == "cancel" {
					cancel()
				}
				if failure == "bridge_close" {
					stopBridge()
				}
				before := bridge.reads.Load()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Errorf("stream remains active after %s; refreshes during next second=%d", failure, bridge.reads.Load()-before)
				}
			})
		}
	}
}

func TestStreamHTTPDisconnect(t *testing.T) {
	for attempt := range 10 {
		t.Run(strconv.Itoa(attempt), testStreamHTTPDisconnect)
	}
}

func testStreamHTTPDisconnect(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	bridge := &streamTestBridge{scriptedSessionBridge: newScriptedSessionBridge()}
	var closeOnce sync.Once
	stopBridge := func() { closeOnce.Do(func() { close(bridge.closed) }) }
	done, disconnected := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(done)
		go func() {
			<-request.Context().Done()
			close(disconnected)
		}()
		h := &handler{server: f.server, writer: &trackedWriter{ResponseWriter: writer}, request: request}
		h.stream(bridge, "cpu-diagnostic", map[string][]string{"incremental": {"1"}})
	}))
	defer func() {
		stopBridge()
		bridge.stop.Store(true)
		server.Close()
	}()
	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(&http.Cookie{Name: f.auth.CookieName(), Value: f.token})
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("status=%d", response.StatusCode)
	}
	response.Body.Close()
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe client disconnect")
	}
	before := bridge.reads.Load()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Errorf("HTTP client disconnected and context canceled, but stream survived past heartbeat; refreshes during next second=%d", bridge.reads.Load()-before)
	}
}

func (w *streamTestWriter) FlushError() error {
	if w.flushFail {
		return io.ErrClosedPipe
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestStreamIdleWaitUsesNotifications(t *testing.T) {
	reads := 0
	changed := make(chan struct{})
	session := &ScriptedSession{Epoch: "idle", Sequence: 7,
		Live:  func() (string, int, bool) { reads++; return "idle", 7, true },
		Watch: func() <-chan struct{} { return changed },
	}
	if session.WaitFor(func() bool { return session.Sequence != 7 }, 300*time.Millisecond) {
		t.Fatal("unchanged session woke as changed")
	}
	if reads != 1 {
		t.Fatalf("idle wait refreshed %d times, want only initial check", reads)
	}
}

func TestStreamSubscribesBeforeRefresh(t *testing.T) {
	changed, next := make(chan struct{}), make(chan struct{})
	calls := 0
	session := &ScriptedSession{Epoch: "wake", Sequence: 7}
	session.Watch = func() <-chan struct{} { return changed }
	session.Live = func() (string, int, bool) {
		calls++
		if calls == 1 {
			close(changed)
			changed = next
			return "wake", 7, true
		}
		return "wake", 8, true
	}
	if !session.WaitFor(func() bool { return session.Sequence == 8 }, 300*time.Millisecond) {
		t.Fatal("change during refresh was lost")
	}
}

func TestLongPollStopsDuringWait(t *testing.T) {
	for _, mode := range []string{"poll", "changes"} {
		t.Run(mode, func(t *testing.T) {
			f := newMobileAPIFixture(t, "none")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bridge := &streamTestBridge{scriptedSessionBridge: newScriptedSessionBridge()}
			defer close(bridge.closed)
			writer := httptest.NewRecorder()
			h := &handler{server: f.server, writer: writer, request: f.build(http.MethodGet, "/poll", "", true).WithContext(ctx)}
			done := make(chan struct{})
			go func() {
				defer close(done)
				query := map[string][]string{"after": {"7"}, "epoch": {"scripted-epoch"}}
				if mode == "poll" {
					h.sessionRoutePoll(bridge, "cpu-test", query)
				} else {
					h.sessionRouteChanges(bridge, "cpu-test", query)
				}
			}()
			deadline := time.Now().Add(time.Second)
			for bridge.reads.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			cancel()
			select {
			case <-done:
				if writer.Body.Len() != 0 {
					t.Fatal("canceled long poll still wrote a response")
				}
			case <-time.After(time.Second):
				t.Fatal("long poll ignored cancellation")
			}
		})
	}
}
