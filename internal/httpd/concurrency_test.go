// Concurrency stress: the production fatal ("concurrent map read and map
// write") came from one goroutine folding the shared per-thread projection
// under historyMu while another read Timeline internals without it (SSE
// incremental readers, message-action validation). This file drives that
// topology end-to-end over the real HTTP surface — a codex follower snapshot
// producer racing HTTP timeline/changes/detail readers, message-action probes
// and an incremental SSE connection — so `go test -race` gates the locks.
package httpd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/codex"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// concurrencyHarness wires one Server against a codex Bridge that never
// dials a desktop socket ("unused-endpoint") and is fed via Publish.
type concurrencyHarness struct {
	t        *testing.T
	server   *Server
	bridge   *codex.Bridge
	auth     *auth.Auth
	token    string
	csrf     string
	threadID string
}

func newConcurrencyHarness(t *testing.T) *concurrencyHarness {
	t.Helper()
	bridge := codex.New(t.TempDir(), "unused-endpoint")
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.Codex = bridge
	return &concurrencyHarness{t: t, server: server, bridge: bridge, auth: authenticator,
		token: token, csrf: csrf, threadID: "6c0nstress-1111-4222-8333-444455556666"}
}

func (a *concurrencyHarness) request(method, target, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	request := httptest.NewRequest(method, "http://127.0.0.1:8790"+target, strings.NewReader(body))
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-CSRF-Token", a.csrf)
	}
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: a.token})
	response := httptest.NewRecorder()
	a.server.ServeHTTP(response, request)
	return response
}

// codexState builds one follower conversation snapshot with a stable first
// turn and an evolving second turn; replaceHistory rewrites the first turn's
// content, which invalidates every cursor (projection epoch bump).
func codexState(threadID, answer, toolOutput string, replaceHistory bool) *jsoncompat.Object {
	userItem := func(id, text string) *jsoncompat.Object {
		item := jsoncompat.NewObject()
		item.Set("id", id)
		item.Set("type", "userMessage")
		content := jsoncompat.NewObject()
		content.Set("type", "input_text")
		content.Set("text", text)
		item.Set("content", &jsoncompat.Array{Items: []any{content}})
		return item
	}
	agentItem := func(id, text string) *jsoncompat.Object {
		item := jsoncompat.NewObject()
		item.Set("id", id)
		item.Set("type", "agentMessage")
		item.Set("text", text)
		return item
	}
	toolItem := func(id string) *jsoncompat.Object {
		item := jsoncompat.NewObject()
		item.Set("id", id)
		item.Set("type", "function_call")
		item.Set("call_id", id)
		item.Set("name", "shell")
		item.Set("arguments", "ls")
		return item
	}
	firstTurn := jsoncompat.NewObject()
	firstTurn.Set("turnId", "turn-1")
	firstTurn.Set("status", "completed")
	firstItems := &jsoncompat.Array{Items: []any{userItem("u1", "第一问"), agentItem("a1", "初次回答")}}
	if replaceHistory {
		firstItems.Items[1] = agentItem("a1", "改写后的历史")
	}
	firstTurn.Set("items", firstItems)
	secondTurn := jsoncompat.NewObject()
	secondTurn.Set("turnId", "turn-2")
	secondTurn.Set("status", "completed")
	secondTurn.Set("items", &jsoncompat.Array{Items: []any{
		userItem("u2", "追问"), agentItem("a2", answer), toolItem("t2")}})
	state := jsoncompat.NewObject()
	state.Set("id", threadID)
	state.Set("title", "并发会话")
	state.Set("turns", &jsoncompat.Array{Items: []any{firstTurn, secondTurn}})
	return state
}

type pageRow struct {
	key     string
	version string
	text    string
	order   int
	role    string
}

type pageView struct {
	epoch    string
	sequence int
	reset    bool
	before   string
	rows     []pageRow
}

// parsePage decodes one timeline/changes payload. It is safe to call from
// reader goroutines: decode problems report via t.Errorf and yield an empty
// view the callers tolerate.
func parsePage(t *testing.T, body []byte) pageView {
	value, err := jsoncompat.Unmarshal(body)
	if err != nil {
		t.Errorf("page body unparsable: %v (%s)", err, body)
		return pageView{}
	}
	page, _ := value.(*jsoncompat.Object)
	if page == nil {
		t.Errorf("page body is not an object: %s", body)
		return pageView{}
	}
	view := pageView{}
	if epoch, ok := page.MustGet("epoch").(string); ok {
		view.epoch = epoch
	}
	if number, ok := page.MustGet("sequence").(json.Number); ok {
		parsed, _ := number.Int64()
		view.sequence = int(parsed)
	}
	_, view.reset = page.Get("reset")
	if before, ok := page.MustGet("before").(string); ok {
		view.before = before
	}
	rows, _ := page.MustGet("rows").(*jsoncompat.Array)
	if rows == nil {
		return view
	}
	for _, item := range rows.Items {
		row, _ := item.(*jsoncompat.Object)
		if row == nil {
			continue
		}
		entry := pageRow{}
		entry.key, _ = row.MustGet("key").(string)
		entry.version, _ = row.MustGet("version").(string)
		entry.text, _ = row.MustGet("text").(string)
		entry.role, _ = row.MustGet("role").(string)
		if number, ok := row.MustGet("order").(json.Number); ok {
			parsed, _ := number.Int64()
			entry.order = int(parsed)
		}
		view.rows = append(view.rows, entry)
	}
	return view
}

// readLoop is one concurrent HTTP reader: it walks timeline pages, probes
// detail consistency, polls changes with its live cursor and periodically
// exercises the message-action validation path that historically raced the
// projection lock. Failures are recorded (not fatal) so the race detector
// output stays the primary signal.
func (a *concurrencyHarness) readLoop(id int, stop <-chan struct{}, failures *sync.Map) {
	fail := func(format string, args ...any) {
		failures.LoadOrStore(fmt.Sprintf("reader%d: %s", id, fmt.Sprintf(format, args...)), true)
	}
	seenSequence, seenEpoch := 0, ""
	for iteration := 0; ; iteration++ {
		select {
		case <-stop:
			return
		default:
		}
		pageResponse := a.request(http.MethodGet,
			"/api/sessions/"+a.threadID+"/timeline?limit=10", "")
		if pageResponse.Code != http.StatusOK {
			fail("timeline status=%d body=%s", pageResponse.Code, pageResponse.Body.String())
			continue
		}
		page := parsePage(a.t, pageResponse.Body.Bytes())
		seenSequence, seenEpoch = page.sequence, page.epoch
		orders := map[int]bool{}
		keys := map[string]bool{}
		for _, row := range page.rows {
			if orders[row.order] || keys[row.key] {
				fail("duplicate row order=%d key=%s", row.order, row.key)
			}
			orders[row.order], keys[row.key] = true, true
		}
		for index := 1; index < len(page.rows); index++ {
			if page.rows[index-1].order >= page.rows[index].order {
				fail("row order not ascending: %v", page.rows)
			}
		}
		if len(page.rows) > 0 {
			// Version equality implies identical content (versions are
			// content hashes), so detail must return the page text verbatim.
			row := page.rows[len(page.rows)/2]
			cursor := page.epoch + "." + row.key
			detailResponse := a.request(http.MethodGet,
				"/api/sessions/"+a.threadID+"/detail?key="+cursor+"&version="+row.version+"&offset=0", "")
			if detailResponse.Code != http.StatusOK {
				fail("detail status=%d body=%s", detailResponse.Code, detailResponse.Body.String())
			} else if detail, err := jsoncompat.Unmarshal(detailResponse.Body.Bytes()); err != nil {
				fail("detail unparsable: %v", err)
			} else if object, _ := detail.(*jsoncompat.Object); object != nil {
				if version, _ := object.MustGet("version").(string); version == row.version {
					if text, _ := object.MustGet("text").(string); text != row.text {
						fail("detail text %q != page text %q", text, row.text)
					}
				}
			}
		}
		changesResponse := a.request(http.MethodGet,
			"/api/sessions/"+a.threadID+"/changes?after="+strconv.Itoa(seenSequence)+"&epoch="+seenEpoch, "")
		if changesResponse.Code != http.StatusOK {
			fail("changes status=%d body=%s", changesResponse.Code, changesResponse.Body.String())
		} else {
			// An epoch move must always answer with a reset frame; the same
			// epoch may legitimately reset too (version window eviction).
			changes := parsePage(a.t, changesResponse.Body.Bytes())
			if seenEpoch != "" && changes.epoch != seenEpoch && !changes.reset {
				fail("epoch moved without reset: requested=%q answered=%q", seenEpoch, changes.epoch)
			}
		}
		if iteration%7 == 3 {
			for _, row := range page.rows {
				if row.role != "user" {
					continue
				}
				// The latest user row is the editable one; the edit itself
				// fails at the IPC call (no desktop App) but the projection-
				// side validation must stay race-free under concurrent folds.
				cursor := page.epoch + "." + row.key
				body := fmt.Sprintf(`{"action":"edit","id":"%d-%d","key":%q,"version":%q,"text":"编辑"}`,
					id, iteration, cursor, row.version)
				editResponse := a.request(http.MethodPost,
					"/api/sessions/"+a.threadID+"/message-action", body)
				if editResponse.Code >= 500 {
					fail("message-action status=%d body=%s", editResponse.Code, editResponse.Body.String())
				}
				break
			}
		}
		if iteration%5 == 2 {
			viewResponse := a.request(http.MethodGet, "/api/sessions/"+a.threadID, "")
			if viewResponse.Code != http.StatusOK {
				fail("view status=%d body=%s", viewResponse.Code, viewResponse.Body.String())
			}
		}
	}
}

func TestConcurrentTimelineHTTPReadersAndCodexSnapshots(t *testing.T) {
	a := newConcurrencyHarness(t)
	// Seed one snapshot so every reader starts against a real projection.
	a.bridge.Publish(a.threadID, codexState(a.threadID, "初始回答", "初始输出", false))

	stop := make(chan struct{})
	failures := sync.Map{}
	var readers sync.WaitGroup
	for id := 1; id <= 4; id++ {
		readers.Add(1)
		go func(id int) {
			defer readers.Done()
			a.readLoop(id, stop, &failures)
		}(id)
	}

	// One incremental SSE connection racing the same projection, exactly the
	// transport the phone keeps open (ready → changes/reset frames).
	sse := a.startIncrementalSSE(t)
	defer sse.close()

	const steps = 120
	for step := 1; step <= steps; step++ {
		a.bridge.Publish(a.threadID, codexState(a.threadID,
			fmt.Sprintf("回答 %d", step), fmt.Sprintf("输出 %d", step), step%37 == 0))
	}
	// Keep the SSE connection fed until it has demonstrably delivered live
	// frames beyond the initial ready (the phone's steady-state behavior).
	sse.waitForFrames(t, 3, func(counter int) {
		a.bridge.Publish(a.threadID, codexState(a.threadID,
			"SSE 补发 "+strconv.Itoa(counter), "SSE 输出", false))
	})

	close(stop)
	readers.Wait()

	// The last published state must be the one the timeline serves.
	a.bridge.Publish(a.threadID, codexState(a.threadID, "终态回答", "终态输出", false))
	final := parsePage(t, a.request(http.MethodGet,
		"/api/sessions/"+a.threadID+"/timeline?limit=20", "").Body.Bytes())
	found := false
	for _, row := range final.rows {
		if strings.Contains(row.text, "终态回答") {
			found = true
		}
	}
	if len(final.rows) != 5 || !found {
		t.Fatalf("final timeline lost the last answer: %v", final.rows)
	}
	sse.assertFrameOrder(t)
	failures.Range(func(key, _ any) bool {
		t.Errorf("concurrent reader failure: %v", key)
		return true
	})
}

// sseConnection runs one incremental /events connection against a recording
// writer and collects the frames it emits.
type sseConnection struct {
	frames func() string
	done   func() bool
	close  func()
}

func (a *concurrencyHarness) startIncrementalSSE(t *testing.T) sseConnection {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet,
		"http://127.0.0.1:8790/api/sessions/"+a.threadID+"/events?incremental=1", nil)
	request.Host = "127.0.0.1:8790"
	request.Header.Set("Origin", "http://127.0.0.1:8790")
	request.Header.Set("Accept", "text/event-stream")
	request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: a.token})
	writer := &recordingWriter{header: http.Header{}}
	stream := &handler{server: a.server, writer: writer, request: request}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		stream.handleMethod(false)
	}()
	return sseConnection{
		frames: writer.snapshot,
		done: func() bool {
			select {
			case <-finished:
				return true
			default:
				return false
			}
		},
		close: func() {
			// Logout ends the stream; a final publish wakes the pending
			// WaitFor so the loop observes the revoked session promptly
			// instead of idling to its 12-second timeout.
			a.auth.Logout(a.token)
			a.bridge.Publish(a.threadID, codexState(a.threadID, "SSE 收尾", "SSE 输出", false))
			select {
			case <-finished:
			case <-time.After(15 * time.Second):
				t.Errorf("SSE handler did not terminate after logout")
			}
		},
	}
}

// waitForFrames polls until the SSE buffer contains at least n "event:" lines
// (the ready frame plus live changes frames), publishing fresh snapshots so
// the stream has something to deliver.
func (s sseConnection) waitForFrames(t *testing.T, n int, produce func(counter int)) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	counter := 0
	for time.Now().Before(deadline) {
		if strings.Count(s.frames(), "event: ") >= n {
			return
		}
		counter++
		produce(counter)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("SSE produced fewer than %d frames: %s", n, s.frames())
}

// assertFrameOrder checks the incremental stream contract: a ready frame
// first, then only changes/reset/heartbeat frames, each decodable, with ids
// of the form epoch.sequence.
func (s sseConnection) assertFrameOrder(t *testing.T) {
	t.Helper()
	raw := s.frames()
	if !strings.Contains(raw, "event: ready") {
		t.Fatalf("missing ready frame: %s", raw)
	}
	for _, frame := range strings.Split(raw, "\n\n") {
		if strings.TrimSpace(frame) == "" || strings.HasPrefix(frame, ": heartbeat") {
			continue
		}
		event := ""
		data := ""
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case strings.HasPrefix(line, "id: "):
				cursor := strings.TrimPrefix(line, "id: ")
				if !strings.Contains(cursor, ".") {
					t.Errorf("malformed SSE cursor %q in frame %q", cursor, frame)
				}
			}
		}
		switch event {
		case "ready", "logout":
		case "changes", "reset":
			value, err := jsoncompat.Unmarshal([]byte(data))
			if err != nil {
				t.Errorf("frame data unparsable: %v (%s)", err, data)
				continue
			}
			object, _ := value.(*jsoncompat.Object)
			if object == nil || object.MustGet("epoch") == nil || object.MustGet("sequence") == nil {
				t.Errorf("changes frame missing epoch/sequence: %s", data)
			}
		case "":
			t.Errorf("frame without event line: %q", frame)
		default:
			t.Errorf("unexpected SSE event %q in frame %q", event, frame)
		}
	}
}

// recordingWriter is a concurrency-safe ResponseRecorder stand-in for the
// long-lived SSE handler goroutine.
type recordingWriter struct {
	mu     sync.Mutex
	header http.Header
	buf    strings.Builder
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *recordingWriter) WriteHeader(code int) {}

func (w *recordingWriter) Flush() {}

func (w *recordingWriter) snapshot() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
