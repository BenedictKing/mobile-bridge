package acp

import (
	"errors"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

type interruptedWriteCloser struct {
	writes int
}

func (w *interruptedWriteCloser) Write(data []byte) (int, error) {
	w.writes++
	return len(data) / 2, errors.New("broken pipe")
}

func (w *interruptedWriteCloser) Close() error { return nil }

func submitErrorKind(t *testing.T, process *Process) ErrorKind {
	t.Helper()
	_, _, err := process.Submit("session/prompt", jsoncompat.NewObject())
	if err == nil {
		t.Fatal("Submit must fail")
	}
	var acpErr *Error
	if !errors.As(err, &acpErr) {
		t.Fatalf("error type=%T, want *acp.Error", err)
	}
	return acpErr.Kind
}

func TestSubmitDistinguishesClosedBeforeWriteFromInterruptedWrite(t *testing.T) {
	closedWriter := &interruptedWriteCloser{}
	closed := &Process{
		stdin: closedWriter, pending: map[string]chan Delivery{}, closed: true,
	}
	if kind := submitErrorKind(t, closed); kind != KindUnavailable {
		t.Fatalf("closed-before-write kind=%s, want %s", kind, KindUnavailable)
	}
	if closedWriter.writes != 0 {
		t.Fatalf("closed process attempted %d writes", closedWriter.writes)
	}
	if len(closed.pending) != 0 {
		t.Fatal("closed Submit left a pending waiter")
	}

	writer := &interruptedWriteCloser{}
	interrupted := &Process{stdin: writer, pending: map[string]chan Delivery{}}
	if kind := submitErrorKind(t, interrupted); kind != KindWriteUnknown {
		t.Fatalf("interrupted-write kind=%s, want %s", kind, KindWriteUnknown)
	}
	if writer.writes != 1 {
		t.Fatalf("interrupted process writes=%d, want 1", writer.writes)
	}
	if len(interrupted.pending) != 0 {
		t.Fatal("interrupted Submit left a pending waiter")
	}
}
