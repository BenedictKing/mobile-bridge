package httpd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/command"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestStopReceiptCannotCancelANewerTurn(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	path := filepath.Join(t.TempDir(), "receipts.json")
	ledger, err := command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Commands = ledger
	body := jsoncompat.NewObject()
	body.Set("commandId", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	body.Set("expectedTurnId", "old")
	calls := 0
	request := func(active string) *httptest.ResponseRecorder {
		writer := httptest.NewRecorder()
		h := &handler{server: f.server, writer: writer, request: f.build(http.MethodPost, "/stop", "{}", true)}
		h.runStop("thread", body, active, func(expected string) (any, error) {
			calls++
			if expected != "old" {
				t.Fatalf("stopped new turn %s", expected)
			}
			return jsoncompat.NewObject(), nil
		})
		return writer
	}
	if r := request("old"); r.Code != http.StatusOK {
		t.Fatal(r.Code, r.Body.String())
	}
	f.server.Commands, err = command.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if r := request("new"); r.Code != http.StatusOK || calls != 1 {
		t.Fatal(r.Code, r.Body.String(), calls)
	}
	body.Set("commandId", "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	if r := request("new"); r.Code != http.StatusOK || calls != 1 {
		t.Fatal(r.Code, r.Body.String(), calls)
	}
}

func TestSessionCommandWaitCanBeCancelledBeforeNativeIO(t *testing.T) {
	f := newMobileAPIFixture(t, "none")
	unlock, err := f.server.lockThreadCommand(context.Background(), "thread")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := httptest.NewRecorder()
	request := f.build(http.MethodPost, "/api/sessions/thread/stop", "{}", true).WithContext(ctx)
	f.server.ServeHTTP(writer, request)
	if writer.Code != http.StatusRequestTimeout {
		t.Fatal(writer.Code, writer.Body.String())
	}
}

func TestQueueClaimCannotReviveCancelledOrEditableInflightMessages(t *testing.T) {
	queue := mustLoadSubmissions(t, filepath.Join(t.TempDir(), "submissions.json"))
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := queue.enqueue("thread", id, "original", "queue", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := queue.cancel("thread", id); err != nil {
		t.Fatal(err)
	}
	if _, err := queue.claimDispatch("thread", id, false); err != errSubmissionGone {
		t.Fatal("cancelled intent claimed", err)
	}
	id = "bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if _, _, err := queue.enqueue("thread", id, "original", "queue", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	var claimed atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if _, err := queue.claimDispatch("thread", id, false); err == nil {
				claimed.Add(1)
			}
		}()
	}
	workers.Wait()
	if claimed.Load() != 1 {
		t.Fatal("multiple dispatch claims", claimed.Load())
	}
	if _, err := queue.edit("thread", id, "new"); err != errSubmissionGone {
		t.Fatal("inflight message edited", err)
	}
	if err := queue.cancel("thread", id); err != errSubmissionGone {
		t.Fatal("inflight message cancelled without stopping provider", err)
	}
	if entry := queue.get("thread", id); entry.Text != "original" || entry.Status != SubUnknown {
		t.Fatal(entry)
	}
}
