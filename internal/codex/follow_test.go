package codex

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/ipc"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestEnsureWatchingCoalescesAndReconnects(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket fixture")
	}
	// macOS limits Unix socket paths to 104 bytes; testing.TempDir's full test
	// name can exceed that before the filename is added.
	dir, err := os.MkdirTemp("", "mb-ipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "ipc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	bridge := New(t.TempDir(), path)
	defer bridge.Close()
	connections := make(chan net.Conn, 2)
	var accepts atomic.Int64
	var serving sync.WaitGroup
	serving.Add(1)
	go func() {
		defer serving.Done()
		for range 2 {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			for {
				value, err := ipc.DecodeFrame(conn)
				if err != nil {
					conn.Close()
					return
				}
				message := value.(*jsoncompat.Object)
				if message.MustGet("method") == "thread-stream-following-changed" {
					snapshot := event("follow-test", object(map[string]any{
						"type": "snapshot", "revision": jsoncompat.Num(int(accepts.Load())),
						"conversationState": object(map[string]any{"id": "follow-test", "turns": &jsoncompat.Array{}}),
					}), "owner")
					snapshot.Set("type", "broadcast")
					frame, _ := ipc.EncodeFrame(snapshot)
					if _, err := conn.Write(frame); err != nil {
						conn.Close()
						return
					}
					connections <- conn
					break
				}
				response := object(map[string]any{"type": "response", "requestId": message.MustGet("requestId"),
					"resultType": "success", "handledByClientId": "owner", "result": object(map[string]any{"clientId": "test-client"})})
				frame, _ := ipc.EncodeFrame(response)
				if _, err := conn.Write(frame); err != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	// Ensure calls return before network discovery finishes and share one attempt.
	start := func() net.Conn {
		t.Helper()
		for range 100 {
			bridge.EnsureWatching("follow-test")
		}
		select {
		case conn := <-connections:
			bridge.followWG.Wait()
			return conn
		case <-time.After(3 * time.Second):
			t.Fatal("follower did not connect")
			return nil
		}
	}
	first := start()
	sequence, changed, ready := bridge.Revision("follow-test")
	if !ready {
		t.Fatal("missing connected snapshot")
	}
	first.Close()
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not wake subscriber")
	}
	bridge.mu.Lock()
	bridge.followAt["follow-test"] = time.Time{} // elapse the reconnect backoff
	bridge.mu.Unlock()
	second := start()
	defer second.Close()
	updated, _, ready := bridge.Revision("follow-test")
	if !ready || updated <= sequence || accepts.Load() != 2 {
		t.Fatalf("reconnect: ready=%v sequence=%d accepts=%d", ready, updated, accepts.Load())
	}
	serving.Wait()
	bridge.Close()
	bridge.EnsureWatching("follow-test")
	if accepts.Load() != 2 {
		t.Fatal("closed bridge started a new follower")
	}
}
