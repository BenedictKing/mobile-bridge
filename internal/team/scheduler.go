package team

// scheduler.go 是依赖就绪调度的纯决策（§6.1 第一步、§6.3 并发护栏、§6.4
// 写锁规则 1/2/3）：过滤可派发任务、尊重成员互斥、项目写锁与并发上限。
// 不做任何 I/O——真实的派发、journal 记账与锁获取由 team-dispatch 执行；
// LockRegistry 是唯一的可变状态（纯内存登记，cwd 由调用方解析传入）。

import "sync"

// LockRegistry 是项目级写锁的内存登记表（§6.4 规则 1/2）：cwd → 持锁任务。
// Acquire/Release/Holder 并发安全；同一 cwd 同时最多一个持锁者。跨 Run 共享
// 由调用方（网关装配处）决定，本类型不感知 Run。
type LockRegistry struct {
	mu      sync.Mutex
	holders map[string]string
}

// NewLockRegistry 创建空写锁登记表。
func NewLockRegistry() *LockRegistry {
	return &LockRegistry{holders: map[string]string{}}
}

// Acquire 为 taskID 获取 cwd 写锁：cwd 已被其他任务持有时返回 false。
func (l *LockRegistry) Acquire(cwd, taskID string) bool {
	if cwd == "" {
		// 无 cwd 可锁视为不受写锁约束（调用方应为写任务解析 cwd）。
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if holder, held := l.holders[cwd]; held && holder != taskID {
		return false
	}
	l.holders[cwd] = taskID
	return true
}

// Release 释放 taskID 在 cwd 上的写锁；持有者不是 taskID 时不动作。
func (l *LockRegistry) Release(cwd, taskID string) {
	if cwd == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders[cwd] == taskID {
		delete(l.holders, cwd)
	}
}

// Holder 返回 cwd 的当前持锁任务（无人持锁返回空串）。
func (l *LockRegistry) Holder(cwd string) string {
	if cwd == "" {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.holders[cwd]
}

// ReadyTasks 是"哪些任务现在可以派发"的纯决策（§6.1）。按 Ordinal 顺序
// 贪心选中同时满足以下条件的任务：
//
//  1. status 为 planned/ready（planned 为计划初始态；ready 是对账后"已确认
//     未发送"的回退态，§8.3 第 5 条——两类都必须能重新进入派发）；
//  2. 依赖全部 completed：依赖 failed/cancelled/outcome_unknown/stopped 均
//     不释放下游（§4.3）；
//  3. 成员未被本 Run 其他在途任务占用（§5.2 同一成员单活动任务），批次内
//     同成员也只取一条；
//  4. WriteLock=exclusive 的任务要求其 cwd 当前无持锁者（§6.4 规则 1/2），
//     批次内同 cwd 的写任务互斥；WriteLock=none 可与写任务并行（规则 3）；
//  5. len(inFlight) + 已选中数 ≤ policy.MaxParallelTasks（§6.3 护栏；
//     无 worker pool，就绪即分派）。
//
// cwdByTask 由调用方解析传入（成员会话的 cwd）；exclusive 任务缺 cwd 条目
// 时跳过（无法证明写隔离，留待下一轮）。memberBusy 是当前在途任务占用的
// 成员集合，选中后批次内继续占用。locks 只读查询（Holder），实际 Acquire
// 由派发方在真正发送前执行。
func ReadyTasks(run *Run, policy TeamPolicy, inFlight int, memberBusy map[string]bool, cwdByTask map[string]string, locks *LockRegistry) []string {
	if run == nil {
		return nil
	}
	if locks == nil {
		locks = NewLockRegistry()
	}
	capacity := policy.MaxParallelTasks
	if capacity < 1 {
		// 并发上限非法（<1）时不派发任何任务：宁可停住等待修正策略，
		// 也不能绕过护栏。
		return nil
	}
	available := capacity - inFlight
	if available <= 0 {
		return nil
	}
	busy := map[string]bool{}
	for member, occupied := range memberBusy {
		busy[member] = occupied
	}
	statuses := map[string]string{}
	for i := range run.Tasks {
		statuses[run.Tasks[i].ID] = run.Tasks[i].Status
	}
	lockedCwds := map[string]bool{} // 本批次内已选中的写任务占用的 cwd
	ready := []string{}
	for _, task := range sortTasksByOrdinal(run.Tasks) {
		if len(ready) >= available {
			break
		}
		switch task.Status {
		case TaskPlanned, TaskReady:
		default:
			continue
		}
		released := true
		for _, dep := range task.DependsOn {
			if statuses[dep] != TaskCompleted {
				// 依赖未完成（含 failed/cancelled/outcome_unknown/stopped）
				// 一律不释放下游。
				released = false
				break
			}
		}
		if !released || busy[task.MemberID] {
			continue
		}
		if task.WriteLock != TaskWriteLockNone {
			// exclusive（含历史数据里的空值，默认写策略为独占）。
			cwd, resolved := cwdByTask[task.ID]
			if !resolved {
				continue
			}
			if cwd != "" && (locks.Holder(cwd) != "" || lockedCwds[cwd]) {
				continue
			}
			if cwd != "" {
				lockedCwds[cwd] = true
			}
		}
		ready = append(ready, task.ID)
		busy[task.MemberID] = true
	}
	return ready
}
