package httpd

// maintain_test.go 覆盖后台维护循环的启停（编排计划 §12 阶段四：queue
// worker 与 idle sweeper 此前是裸 ticker goroutine，补停止通道——stop chan +
// sync.Once 幂等关闭 + done 同步等待退出）。判定以 channel 同步为主；唯一
// 的时长窗口是「body 阻塞期间 close 不得返回」的防提前返回观察窗（判据是
// 返回与否，不是时长）。

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestMaintainLoopCloseWaitsForInFlightBody：close 不得在 body 执行中返回；
// 放行后 close 返回 ⟹ done 已关闭（循环 goroutine 确已退出）；幂等。
func TestMaintainLoopCloseWaitsForInFlightBody(t *testing.T) {
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	var runs int32
	loop := newMaintainLoop(time.Millisecond, func() {
		atomic.AddInt32(&runs, 1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate
	})
	// 首轮 body 起跑并阻塞在 gate（channel 同步，不用固定 sleep 等启动）。
	<-started
	stopped := make(chan struct{})
	go func() {
		loop.close()
		close(stopped)
	}()
	// body 仍阻塞：close 不应提前返回（有界观察窗，防「假同步」实现）。
	select {
	case <-stopped:
		t.Fatal("body 阻塞期间 close 不应返回")
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	<-stopped
	// 同步退出：close 返回即循环已结束（done 已关闭）。
	select {
	case <-loop.done:
	default:
		t.Fatal("close 返回时 done 应已关闭（同步等待退出）")
	}
	if count := atomic.LoadInt32(&runs); count == 0 {
		t.Fatal("body 至少应执行过一次")
	}
	loop.close() // 幂等：重复 close 无副作用。
}

// TestMaintainLoopCloseNilSafe：未启动的循环（nil 句柄）可安全停止。
func TestMaintainLoopCloseNilSafe(t *testing.T) {
	var loop *maintainLoop
	loop.close()
}

// TestServerQueueWorkerStopIsSynchronous：Start/Stop 生命周期（Stop 同步等
// 循环退出、幂等、可重启；Queue 未装配不启动——既有 nil 防御）。
func TestServerQueueWorkerStopIsSynchronous(t *testing.T) {
	server := &Server{Queue: LoadSubmissions(filepath.Join(t.TempDir(), "submissions.json"))}
	server.StartQueueWorker()
	if server.queueWorker == nil {
		t.Fatal("队列 worker 应已启动")
	}
	done := server.queueWorker.done
	server.StopQueueWorker()
	select {
	case <-done:
	default:
		t.Fatal("StopQueueWorker 返回时循环应已退出")
	}
	server.StopQueueWorker()  // 幂等。
	server.StartQueueWorker() // 可重启（Stop 已清出旧句柄）。
	if server.queueWorker == nil {
		t.Fatal("重启应产生新循环")
	}
	server.StopQueueWorker()
	// 重复 Start 不叠加循环（照 StartDiscovery 的幂等先例）。
	server.StartQueueWorker()
	first := server.queueWorker
	server.StartQueueWorker()
	if server.queueWorker != first {
		t.Fatal("重复 Start 不应替换在飞循环")
	}
	server.StopQueueWorker()

	empty := &Server{}
	empty.StartQueueWorker()
	if empty.queueWorker != nil {
		t.Fatal("Queue 未装配时不得启动 worker")
	}
	empty.StopQueueWorker() // nil 句柄安全。
}

// TestServerIdleSweeperStopIsSynchronous：sweeper 生命周期同款（ACPAgents
// 为空时清扫轮空转，启停语义不受影响）。
func TestServerIdleSweeperStopIsSynchronous(t *testing.T) {
	server := &Server{}
	server.StartIdleSweeper()
	if server.idleSweeper == nil {
		t.Fatal("空闲 sweeper 应已启动")
	}
	done := server.idleSweeper.done
	server.StopIdleSweeper()
	select {
	case <-done:
	default:
		t.Fatal("StopIdleSweeper 返回时循环应已退出")
	}
	server.StopIdleSweeper()  // 幂等。
	server.StartIdleSweeper() // 可重启。
	server.StopIdleSweeper()
}
