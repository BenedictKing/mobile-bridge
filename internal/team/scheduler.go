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

// depGroupVerdict 是依赖释放判定的唯一权威（§6.5 组语义，阶段二）——
// ReadyTasks、cascadeBlockedLocked、PlanResume 四处同规则，杜绝「调度释放
// 了、级联却终止」的语义漂移。statuses 是任务表全量状态（taskID→status），
// groups 是任务表预构的组员映射（taskID→BroadcastGroup，空串=无组），
// taskDeps 是被判定任务的依赖列表，collect 是 Run 的收集策略。
//
// 返回 (released, blocked)：
//   - released：全部依赖满足，可派发/可继续；
//   - blocked：存在永久阻塞依赖（级联终止语义）；
//   - (false, false)：依赖仍在飞，等待释放。
//
// 规则：无组依赖退化为阶段一规则（completed→满足；终态非 completed→阻塞；
// 非终态→等待）。有组依赖按组聚合（组员从 groups 全表枚举）：
//   - collect=first_success：组内任一 completed 即满足（superseded 兄弟不
//     影响已满足的组）；
//   - collect=all：组内全部终态且 ≥1 completed 才满足（failed 成员不贡献
//     上下文但不阻塞下游合并，§12 阶段二验收）；
//   - 两种 collect 下组内全部终态且 0 completed→阻塞（级联终止）。
func depGroupVerdict(statuses map[string]string, groups map[string]string, taskDeps []string, collect string) (released, blocked bool) {
	released = true
	verdicts := map[string][2]bool{} // 组缓存：同组多个依赖只聚合一次
	for _, dep := range taskDeps {
		group := groups[dep]
		if group == "" {
			switch statuses[dep] {
			case TaskCompleted:
			case TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
				return false, true
			default:
				released = false // 依赖未终局：可能随后释放。
			}
			continue
		}
		if _, cached := verdicts[group]; !cached {
			satisfied, groupBlocked := groupVerdict(statuses, groups, group, collect)
			verdicts[group] = [2]bool{satisfied, groupBlocked}
		}
		satisfied, groupBlocked := verdicts[group][0], verdicts[group][1]
		if groupBlocked {
			return false, true
		}
		if !satisfied {
			released = false
		}
	}
	return released, false
}

// groupVerdict 聚合单个广播组的释放判定（depGroupVerdict 的组内实现）。
func groupVerdict(statuses map[string]string, groups map[string]string, group, collect string) (satisfied, blocked bool) {
	anyCompleted := false
	allTerminal := true
	for taskID, taskGroup := range groups {
		if taskGroup != group {
			continue
		}
		switch statuses[taskID] {
		case TaskCompleted:
			anyCompleted = true
		case TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
		default:
			allTerminal = false
		}
	}
	if anyCompleted && collect == RunCollectFirstSuccess {
		return true, false
	}
	if allTerminal {
		if anyCompleted {
			return true, false // collect=all：全终态且 ≥1 completed。
		}
		return false, true // 全终态且 0 completed：级联终止。
	}
	return false, false // 组内仍有非终态成员：等待。
}

// broadcastGroupsOf 从任务表预构组员映射（taskID→BroadcastGroup）。
func broadcastGroupsOf(tasks []Task) map[string]string {
	var groups map[string]string
	for i := range tasks {
		if tasks[i].BroadcastGroup == "" {
			continue
		}
		if groups == nil {
			groups = map[string]string{}
		}
		groups[tasks[i].ID] = tasks[i].BroadcastGroup
	}
	return groups
}

// ReadyTasks 是"哪些任务现在可以派发"的纯决策（§6.1）。按 Ordinal 顺序
// 贪心选中同时满足以下条件的任务：
//
//  1. status 为 planned/ready（planned 为计划初始态；ready 是对账后"已确认
//     未发送"的回退态，§8.3 第 5 条——两类都必须能重新进入派发）；
//  2. 依赖满足 depGroupVerdict（含 §6.5 组语义：first_success 组内任一
//     completed 即释放，all 组内全终态且 ≥1 completed 释放）；被永久阻塞
//     依赖（终态非完成，或全终态 0 完成的组）同样不选中——级联终止由
//     runner 的 cascade 步骤落账；
//  3. 成员未被本 Run 其他在途任务占用（§5.2 同一成员单活动任务），批次内
//     同成员也只取一条；
//  4. WriteLock=exclusive 的任务要求其 cwd 当前无持锁者（§6.4 规则 1/2），
//     批次内同 cwd 的写任务互斥；WriteLock=none 可与写任务并行（规则 3）；
//  5. len(inFlight) + 已选中数 ≤ policy.MaxParallelTasks（§6.3 护栏；
//     inFlight 应包含已投递派发池、快照尚未落 dispatching 的任务）。
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
	groups := broadcastGroupsOf(run.Tasks)
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
		released, blocked := depGroupVerdict(statuses, groups, task.DependsOn, run.CollectPolicy)
		if blocked || !released || busy[task.MemberID] {
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
