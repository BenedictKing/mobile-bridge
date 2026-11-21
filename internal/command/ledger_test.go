package command

import (
	"context"
	"errors"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func openTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(filepath.Join(t.TempDir(), "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestClaimPrecedesIOAndReplaysAfterRestart(t *testing.T) {
	l := openTestLedger(t)
	intent := Intent{ID: "one", Scope: "stop:thread", Payload: map[string]any{"turn": "old"}}
	calls := 0
	result, err := l.Run(intent, func() (any, error) {
		calls++
		restarted, err := Open(l.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err, found := restarted.Replay(intent.ID, intent.Scope, intent.Payload); !found || !errors.Is(err, ErrUnknown) {
			t.Fatalf("uncommitted claim replayed: %v %v", found, err)
		}
		return map[string]any{"stopped": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(l.path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := restarted.Run(intent, func() (any, error) { calls++; return nil, nil })
	if err != nil || Hash(result) != Hash(again) || calls != 1 {
		t.Fatalf("result=%s calls=%d err=%v", again, calls, err)
	}
	intent.Payload = map[string]any{"turn": "new"}
	if _, err := restarted.Run(intent, func() (any, error) { calls++; return nil, nil }); !errors.Is(err, ErrConflict) || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestHumanAndAutomaticClaimsShareOneNativeResponse(t *testing.T) {
	l := openTestLedger(t)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	var calls atomic.Int32
	intent := Intent{ID: "auto", Scope: "reply:thread:request", Target: "native-turn:request", Payload: map[string]any{"decision": "accept"}}
	go func() {
		_, err := l.Run(intent, func() (any, error) { calls.Add(1); close(entered); <-release; return map[string]any{}, nil })
		done <- err
	}()
	<-entered
	manual := intent
	manual.ID = "human"
	if _, err := l.Run(manual, func() (any, error) { calls.Add(1); return nil, nil }); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := l.Run(manual, func() (any, error) { calls.Add(1); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	manual.ID = "contradictory"
	manual.Payload = map[string]any{"decision": "decline"}
	if _, err := l.Run(manual, func() (any, error) { calls.Add(1); return nil, nil }); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal(calls.Load())
	}
}

func TestStoreFailureNeverCallsProvider(t *testing.T) {
	l := openTestLedger(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	l.path = filepath.Join(blocker, "receipts.json")
	calls := 0
	if _, err := l.Run(Intent{ID: "one", Scope: "reply", Payload: 1}, func() (any, error) { calls++; return nil, nil }); !errors.Is(err, ErrStore) || calls != 0 {
		t.Fatal(err, calls)
	}
}

func TestUnknownWriteAndSettlementFailureCannotReplay(t *testing.T) {
	for _, failure := range []string{"write", "settlement"} {
		t.Run(failure, func(t *testing.T) {
			l := openTestLedger(t)
			intent := Intent{ID: "one", Scope: "reply", Target: "request", Payload: true}
			calls := 0
			_, err := l.Run(intent, func() (any, error) {
				calls++
				if failure == "write" {
					return nil, errors.New("partial write")
				}
				l.path = filepath.Join(l.path, "child")
				return map[string]any{}, nil
			})
			if !errors.Is(err, ErrUnknown) {
				t.Fatal(err)
			}
			if _, err := l.Run(intent, func() (any, error) { calls++; return nil, nil }); !errors.Is(err, ErrUnknown) || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestRejectedTargetCanAcceptANewIntent(t *testing.T) {
	l := openTestLedger(t)
	intent := Intent{ID: "one", Scope: "reply", Target: "request", Payload: true}
	if _, err := l.Run(intent, func() (any, error) { return nil, Rejected(errors.New("not written")) }); err == nil {
		t.Fatal("expected rejection")
	}
	intent.ID = "new"
	if _, err := l.Run(intent, func() (any, error) { return map[string]any{}, nil }); err != nil {
		t.Fatal(err)
	}
}

func TestNestedCompatibilityObjectsRetainPayloadIdentityAndResults(t *testing.T) {
	l := openTestLedger(t)
	response := jsoncompat.NewObject()
	response.Set("decision", "accept")
	result := jsoncompat.NewObject()
	result.Set("id", "native-turn")
	result.Set("items", &jsoncompat.Array{Items: []any{response}})
	raw, err := l.Run(Intent{ID: "one", Scope: "reply", Payload: response}, func() (any, error) { return result, nil })
	if err != nil || Hash(raw) != Hash(map[string]any{"id": "native-turn", "items": []any{map[string]any{"decision": "accept"}}}) {
		t.Fatal(string(raw), err)
	}
	response.Set("decision", "decline")
	if _, err, found := l.Replay("one", "reply", response); !found || !errors.Is(err, ErrConflict) {
		t.Fatal(found, err)
	}
}

func TestSerialIndependentKeysAndCancelledWaiters(t *testing.T) {
	var serial Serial
	unlock, err := serial.Lock(context.Background(), "one")
	if err != nil {
		t.Fatal(err)
	}
	other, err := serial.Lock(context.Background(), "two")
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := serial.Lock(ctx, "one"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	unlock()
	unlock()
	if len(serial.slots) != 0 {
		t.Fatal("idle keys leaked")
	}
}
