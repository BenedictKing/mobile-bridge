package acp

import (
	"os"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestRequestTimeoutDiscardsPendingCall(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	process := &Process{stdin: writer, pending: map[string]chan Delivery{}, done: make(chan struct{})}
	_, err = process.Request("session/prompt", jsoncompat.NewObject(), time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
	process.mu.Lock()
	deferred := len(process.pending)
	process.mu.Unlock()
	if deferred != 0 {
		t.Fatalf("timed out request left %d pending calls", deferred)
	}
}
