package ipc

import (
	"net"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestDesktopRequestRoundTripAndTimeoutCleanup(t *testing.T) {
	clientConn, agentConn := net.Pipe()
	defer clientConn.Close()
	defer agentConn.Close()
	desktop := NewDesktop("unused")
	desktop.NewID = func() string { return "request-1" }
	desktop.mu.Lock()
	desktop.conn = clientConn
	desktop.closed = false
	desktop.mu.Unlock()
	go desktop.readLoop(clientConn)

	go func() {
		value, err := DecodeFrame(agentConn)
		if err != nil {
			return
		}
		request := value.(*jsoncompat.Object)
		response := jsoncompat.NewObject()
		response.Set("type", "response")
		response.Set("requestId", request.MustGet("requestId"))
		response.Set("resultType", "success")
		response.Set("result", jsoncompat.NewObject())
		frame, _ := EncodeFrame(response)
		_, _ = agentConn.Write(frame)
		// Keep consuming the next request so the client can reach its timeout
		// path without blocking on the pipe write.
		_, _ = DecodeFrame(agentConn)
	}()

	result, err := desktop.Request("thread-owner-discovery", jsoncompat.NewObject(), "", "local", time.Second)
	if err != nil || result == nil {
		t.Fatalf("round trip failed: result=%v err=%v", result, err)
	}

	_, err = desktop.Request("thread-owner-discovery", jsoncompat.NewObject(), "", "local", time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout")
	}
	desktop.mu.Lock()
	pending := len(desktop.pending)
	desktop.mu.Unlock()
	if pending != 0 {
		t.Fatalf("timeout left %d pending requests", pending)
	}
}
