package acpruntime

import (
	"testing"
	"time"
)

// 观察之后、进入等待之前已发生的变更必须立即唤醒：团队 watcher 曾在读快照后
// 才取变更通道，turn 恰好在两者之间落定时要等满一片（生产 25s）才发现。
func TestWaitAfterReturnsForChangeBeforeWait(t *testing.T) {
	session := newStubSession("s-wait", "/tmp")
	observed := session.Sequence()
	session.mu.Lock()
	session.signalLocked()
	session.mu.Unlock()

	start := time.Now()
	session.WaitAfter(observed, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("已越过观察点的变更应立即返回，实际等了 %v", elapsed)
	}
}

func TestWaitAfterWakesOnLaterChange(t *testing.T) {
	session := newStubSession("s-wait", "/tmp")
	observed := session.Sequence()
	go func() {
		time.Sleep(50 * time.Millisecond)
		session.mu.Lock()
		session.signalLocked()
		session.mu.Unlock()
	}()

	start := time.Now()
	session.WaitAfter(observed, 5*time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("等待期间的变更应唤醒，实际等了 %v", elapsed)
	}
}

func TestWaitAfterTimesOutWithoutChange(t *testing.T) {
	session := newStubSession("s-wait", "/tmp")
	start := time.Now()
	session.WaitAfter(session.Sequence(), 80*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 70*time.Millisecond {
		t.Fatalf("无变更时应等到超时，实际 %v 即返回", elapsed)
	}
}
