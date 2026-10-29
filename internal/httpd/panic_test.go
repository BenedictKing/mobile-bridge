package httpd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The wrapper must keep serving SSE: writeRaw type-asserts http.Flusher.
var _ http.Flusher = (*trackedWriter)(nil)

func TestTrackedWriterFlushesThrough(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracked := &trackedWriter{ResponseWriter: recorder}
	tracked.Flush()
	if !recorder.Flushed {
		t.Fatal("Flush must reach the underlying writer")
	}
}

// A panic in a view must not drop the connection: the client would only see
// "Failed to fetch" (no status, no message), which reads like a network fault.
func TestRecoverRequestAnswersJSONWhenNothingWritten(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracked := &trackedWriter{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/pool/workbuddy", nil)
	func() {
		defer recoverRequest(tracked, request)
		panic("jsoncompat: unsupported value float64")
	}()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "网关内部错误") {
		t.Fatalf("body=%s", recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("content-type=%q", contentType)
	}
}

// Already-streamed responses (SSE) stay untouched: appending a JSON error to a
// half-written event stream would corrupt it.
func TestRecoverRequestLeavesCommittedResponseAlone(t *testing.T) {
	recorder := httptest.NewRecorder()
	tracked := &trackedWriter{ResponseWriter: recorder}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8790/api/events", nil)
	func() {
		defer recoverRequest(tracked, request)
		tracked.WriteHeader(http.StatusOK)
		_, _ = tracked.Write([]byte("event: state\ndata: {}\n\n"))
		panic("boom")
	}()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "网关内部错误") {
		t.Fatalf("committed stream must not be rewritten: %s", recorder.Body.String())
	}
}
