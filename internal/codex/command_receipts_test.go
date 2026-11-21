//go:build !windows

package codex

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/command"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func TestCLIApprovalReceiptDistinguishesReusedNativeRequestIDs(t *testing.T) {
	b, fixture := newCLIFixture(t)
	ledger, err := command.Open(filepath.Join(t.TempDir(), "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.Commands = ledger
	if _, err := b.WatchForWrite("cli-thread", time.Second); err != nil {
		t.Fatal(err)
	}
	response := object(map[string]any{"decision": "accept"})
	for _, turn := range []string{"first", "second"} {
		b.cli.onRequest(object(map[string]any{"id": jsoncompat.Num(99), "method": "item/commandExecution/requestApproval", "params": object(map[string]any{"threadId": "cli-thread", "turnId": turn, "command": "go test ./..."})}))
		if _, err := b.Respond("cli-thread", "99", response, "command-"+turn); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Respond("cli-thread", "99", response, "command-"+turn); err != nil {
			t.Fatal("ACK loss did not replay", err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		fixture.mu.Lock()
		count := 0
		for _, call := range fixture.calls {
			if call.MustGet("id") == jsoncompat.Num(99) && call.MustGet("result") != nil {
				count++
			}
		}
		fixture.mu.Unlock()
		if count == 2 {
			return
		}
		if count > 2 || time.Now().After(deadline) {
			t.Fatalf("native replies=%d, want one per turn", count)
		}
		time.Sleep(time.Millisecond)
	}
}
