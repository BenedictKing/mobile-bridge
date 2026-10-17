package httpd

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/auth"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func slotTestView(sequence int, text string) *jsoncompat.Object {
	turn := jsoncompat.NewObject()
	turn.Set("id", "turn-1")
	turn.Set("status", "completed")
	message := jsoncompat.NewObject()
	message.Set("id", "message-1")
	message.Set("kind", "userMessage")
	message.Set("role", "user")
	message.Set("text", text)
	turn.Set("messages", &jsoncompat.Array{Items: []any{message}})
	view := jsoncompat.NewObject()
	view.Set("sequence", jsoncompat.Num(sequence))
	view.Set("turns", &jsoncompat.Array{Items: []any{turn}})
	return view
}

// TestProjectionSlotsSerializePerThread pins the lock topology: one thread's
// fold must not wait on another thread's fold, while the same thread stays
// mutually exclusive.
func TestProjectionSlotsSerializePerThread(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	if _, _, err := authenticator.NewSession("127.0.0.1", "", false); err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")

	slotA := server.projectionSlotFor("thread-a")
	slotA.mu.Lock()

	doneB := make(chan struct{})
	doneA := make(chan struct{})
	go func() {
		server.foldProjection("thread-b", slotTestView(1, "B"))
		close(doneB)
	}()
	go func() {
		server.foldProjection("thread-a", slotTestView(1, "A"))
		close(doneA)
	}()

	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatal("thread-b fold blocked behind thread-a's slot lock")
	}
	select {
	case <-doneA:
		t.Fatal("thread-a fold entered while the slot was held")
	case <-time.After(150 * time.Millisecond):
	}
	slotA.mu.Unlock()
	select {
	case <-doneA:
	case <-time.After(2 * time.Second):
		t.Fatal("thread-a fold never ran after unlock")
	}

	// Concurrent folds on one thread serialize, and the last one wins.
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			server.foldProjection("thread-c", slotTestView(i+1, "C"))
		}(i)
	}
	group.Wait()
	slot := server.lookupProjection("thread-c")
	if slot == nil {
		t.Fatal("thread-c slot missing")
	}
	if got := slot.timeline.State().Rows; got != 1 {
		t.Fatalf("rows=%d", got)
	}
	if _, _, ok := server.projectionState("thread-unknown"); ok {
		t.Fatal("unknown thread must not materialize a projection")
	}
}

// TestProjectionSlotsSurviveParallelFolds hammers two threads from parallel
// goroutines; run under -race this fails on any shared-state slip.
func TestProjectionSlotsSurviveParallelFolds(t *testing.T) {
	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	if _, _, err := authenticator.NewSession("127.0.0.1", "", false); err != nil {
		t.Fatal(err)
	}
	server := New(authenticator, fsys{}, 8790, "instance")
	var group sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			thread := "thread-" + string(rune('a'+worker%2))
			for i := 0; i < 40; i++ {
				epoch, sequence, ok := server.foldProjection(thread, slotTestView(i+1, "内容"))
				if !ok || epoch == "" || sequence < 0 {
					t.Errorf("fold reported %q/%d/%v", epoch, sequence, ok)
					return
				}
				if slot := server.lookupProjection(thread); slot != nil {
					slot.mu.Lock()
					slot.timeline.Page(20, "")
					slot.mu.Unlock()
				}
			}
		}(worker)
	}
	group.Wait()
}

// streamingDeltaAgent is a stateless ACP agent that streams three message
// chunks (250ms apart) while answering session/prompt, then ends the turn:
// the first chunk creates a row (changes frame), the next two are pure
// appends (delta frames), and end_turn flips the turn status (changes again).
func streamingDeltaAgent(t *testing.T) []string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip(err)
	}
	script := `import json,sys,time
for line in sys.stdin:
 m=json.loads(line); method=m.get("method"); ident=m.get("id")
 def send(payload):
  print(json.dumps(payload),flush=True)
 if method == "initialize": send({"jsonrpc":"2.0","id":ident,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake"},"authMethods":[]}})
 elif method == "session/new": send({"jsonrpc":"2.0","id":ident,"result":{"sessionId":"uuid-delta"}})
 elif method == "session/prompt":
  for chunk in ["你好", "，世界", "，再来一段"]:
   send({"jsonrpc":"2.0","method":"session/update","params":{"sessionId":m["params"]["sessionId"],"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":chunk}}}})
   time.sleep(0.25)
  send({"jsonrpc":"2.0","id":ident,"result":{"stopReason":"end_turn"}})
 else:
  send({"jsonrpc":"2.0","id":ident,"result":{}})
`
	path := filepath.Join(t.TempDir(), "agent.py")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return []string{python, "-u", path}
}

// TestStreamIncrementalSendsDeltaFrames walks the SSE surface end to end:
// ready, then a changes frame for the new row, delta frames for the pure
// appends, and a final changes frame when the turn status moves — each with
// an epoch.sequence cursor.
func TestStreamIncrementalSendsDeltaFrames(t *testing.T) {
	manager := acpruntime.NewCommand(streamingDeltaAgent(t))
	manager.Label = "ZCode"
	defer manager.Close()

	authenticator := auth.NewAuth(auth.Config{Mode: "none"})
	token, session, err := authenticator.NewSession("127.0.0.1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	csrf, _ := session.MustGet("csrf").(string)
	server := New(authenticator, fsys{}, 8790, "instance")
	server.ACPAgents = map[string]*acpruntime.Manager{"zcode": manager}

	api := httptest.NewServer(server)
	// Closed in the background after the test: the SSE handler only ends at
	// its next auth check (the loop's WaitFor runs up to 12s first), and
	// httptest.Server.Close waits for outstanding handlers.
	hostPort := api.URL[len("http://"):]
	server.Hosts[hostPort] = true
	server.Origins["http://"+hostPort] = true
	server.LocalHosts[hostPort] = true
	client := &http.Client{Timeout: 15 * time.Second}

	authorized := func(method, target, body string) (*http.Response, error) {
		request, err := http.NewRequest(method, api.URL+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = api.URL[len("http://"):]
		request.Header.Set("Origin", "http://"+request.Host)
		if body != "" {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-CSRF-Token", csrf)
		}
		request.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
		return client.Do(request)
	}

	// Create the chat so the agent owns a session id, then start the SSE
	// subscription before prompting so no frame is missed.
	create, err := authorized(http.MethodPost, "/api/sessions",
		`{"agent":"zcode","cwd":".","title":"增量","id":"9aaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}`)
	if err != nil {
		t.Fatal(err)
	}
	if create.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(create.Body)
		t.Fatalf("create status=%d body=%s", create.StatusCode, body)
	}
	_ = create.Body.Close()

	events, err := authorized(http.MethodGet, "/api/sessions/uuid-delta/events?incremental=1", "")
	if err != nil {
		t.Fatal(err)
	}
	if events.StatusCode != http.StatusOK {
		t.Fatalf("events status=%d", events.StatusCode)
	}
	var bodyMu sync.Mutex
	frames := &strings.Builder{}
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		reader := bufio.NewReader(events.Body)
		for {
			line, err := reader.ReadString('\n')
			bodyMu.Lock()
			frames.WriteString(line)
			bodyMu.Unlock()
			if err != nil {
				if err != io.EOF {
					return
				}
				return
			}
		}
	}()

	// Prompt: blocks until the fake agent finishes streaming (~0.8s).
	prompt, err := authorized(http.MethodPost, "/api/sessions/uuid-delta/send", `{"text":"讲下去"}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = prompt.Body.Close()

	deadline := time.Now().Add(15 * time.Second)
	for {
		bodyMu.Lock()
		count := strings.Count(frames.String(), "event: changes")
		bodyMu.Unlock()
		if count >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	bodyMu.Lock()
	collected := frames.String()
	bodyMu.Unlock()
	epoch := ""
	sawReady, sawChanges := false, 0
	sawDelta := 0
	lastSequence := -1
	for _, frame := range strings.Split(collected, "\n\n") {
		var id string
		var event string
		var data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event == "" {
			continue
		}
		switch event {
		case "ready":
			sawReady = true
		case "changes", "reset":
			sawChanges++
		case "delta":
			sawDelta++
			payload, err := jsoncompat.Unmarshal([]byte(data))
			if err != nil {
				t.Fatalf("delta payload: %v", err)
			}
			object := payload.(*jsoncompat.Object)
			rows := object.MustGet("rows").(*jsoncompat.Array)
			if len(rows.Items) == 0 {
				t.Fatal("delta frame without rows")
			}
			row := rows.Items[0].(*jsoncompat.Object)
			for _, field := range []string{"key", "version", "prevVersion", "append"} {
				if _, exists := row.Get(field); !exists {
					t.Fatalf("delta row missing %s: %s", field, data)
				}
			}
			if appendText, _ := row.MustGet("append").(string); appendText == "" {
				t.Fatalf("empty append: %s", data)
			}
			if _, hasMeta := object.Get("meta"); !hasMeta {
				t.Fatal("delta frame without meta")
			}
		default:
			continue
		}
		if event != "ready" {
			if id == "" {
				t.Fatalf("%s frame without cursor id: %q", event, frame)
			}
			head, tail, found := strings.Cut(id, ".")
			if !found || head == "" || tail == "" {
				t.Fatalf("malformed cursor %q", id)
			}
			if epoch == "" {
				epoch = head
			}
			if head != epoch {
				t.Fatalf("epoch changed mid-stream: %q", id)
			}
			sequence := 0
			for _, r := range tail {
				sequence = sequence*10 + int(r-'0')
			}
			if sequence <= lastSequence {
				t.Fatalf("cursor went backwards: %d after %d", sequence, lastSequence)
			}
			lastSequence = sequence
		}
	}
	// End the stream: the closed body unblocks the reader, the logout ends
	// the handler at its next auth check, and the server teardown (which
	// waits for that handler) runs in the background.
	_ = events.Body.Close()
	authenticator.Logout(token)
	api.CloseClientConnections()
	go api.Close()
	select {
	case <-readerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stream reader never finished")
	}

	t.Logf("frames:\n%s", collected)
	if !sawReady {
		t.Fatalf("missing ready frame: %q", collected)
	}
	if sawChanges == 0 {
		t.Fatalf("missing changes frames: %q", collected)
	}
	if sawDelta == 0 {
		t.Fatalf("missing delta frames: %q", collected)
	}
}
