package acp

import (
	"os"
	"strings"
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

// ACP SDK 把 Agent 的普通 Error 包成 -32603「Internal error」，原因只在 data.details：
// 仅这个空泛文案拼回 details（有界），其他错误文本保持原样。
func TestDispatchErrorCarriesInternalErrorDetails(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error","data":{"details":"⚠ 会话 x 已被后端清理"}}}`, "Internal error: ⚠ 会话 x 已被后端清理"},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error","data":{}}}`, "Internal error"},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"普通失败","data":{"details":"细节"}}}`, "普通失败"},
	}
	for _, tc := range cases {
		value, err := jsoncompat.Unmarshal([]byte(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		message := value.(*jsoncompat.Object)
		idValue, _ := message.Get("id")
		channel := make(chan Delivery, 1)
		process := &Process{pending: map[string]chan Delivery{idKey(idValue): channel}, done: make(chan struct{})}
		process.dispatch(message)
		delivery := <-channel
		if delivery.Err == nil || delivery.Err.Error() != tc.want {
			t.Fatalf("error = %v, want %q", delivery.Err, tc.want)
		}
	}
	long := strings.Repeat("长", 400)
	value, _ := jsoncompat.Unmarshal([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32603,"message":"Internal error","data":{"details":"` + long + `"}}}`))
	message := value.(*jsoncompat.Object)
	idValue, _ := message.Get("id")
	channel := make(chan Delivery, 1)
	process := &Process{pending: map[string]chan Delivery{idKey(idValue): channel}, done: make(chan struct{})}
	process.dispatch(message)
	if got := len([]rune((<-channel).Err.Error())); got != len([]rune("Internal error: "))+300 {
		t.Fatalf("details must be bounded to 300 runes, got %d", got)
	}
}
