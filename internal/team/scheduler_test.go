package team

// scheduler 的单元测试（§13.1 team/scheduler 行阶段一子集）：依赖释放、依赖
// failed/cancelled/stopped 不释放、写锁互斥与 none 并行、并发上限、同成员互斥。

import (
	"reflect"
	"sync"
	"testing"
)

func taskWith(id, member, status string, deps ...string) Task {
	return Task{ID: id, MemberID: member, Status: status, DependsOn: deps, WriteLock: TaskWriteLockNone}
}

func runWithTasks(tasks ...Task) *Run {
	for i := range tasks {
		tasks[i].Ordinal = i
	}
	return &Run{ID: "run-1", TeamID: "team-1", Status: RunDispatching, Tasks: tasks}
}

func TestReadyTasksReleasesOnCompletedDependencies(t *testing.T) {
	run := runWithTasks(
		taskWith("design", "lead", TaskCompleted),
		taskWith("implement", "dev", TaskPlanned, "design"),
		taskWith("review", "rev", TaskPlanned, "implement"),
	)
	ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"implement"}) {
		t.Fatalf("design 完成后只应释放 implement，实际 %v", ready)
	}
	// 无依赖的任务立即就绪。
	fresh := runWithTasks(taskWith("a", "dev", TaskPlanned), taskWith("b", "rev", TaskReady))
	ready = ReadyTasks(fresh, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"a", "b"}) {
		t.Fatalf("planned 与 ready 都应可派发，实际 %v", ready)
	}
}

func TestReadyTasksBlockedByNonCompletedDependencies(t *testing.T) {
	for _, status := range []string{TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped, TaskRunning} {
		run := runWithTasks(
			taskWith("design", "lead", status),
			taskWith("implement", "dev", TaskPlanned, "design"),
		)
		ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
		if len(ready) != 0 {
			t.Fatalf("依赖 %s 不应释放下游，实际 %v", status, ready)
		}
	}
}

func TestReadyTasksWriteLockExclusiveMutualExclusion(t *testing.T) {
	run := runWithTasks(
		taskWith("write-a", "dev", TaskPlanned),
		taskWith("write-b", "rev", TaskPlanned),
	)
	run.Tasks[0].WriteLock = TaskWriteLockExclusive
	run.Tasks[1].WriteLock = TaskWriteLockExclusive
	cwd := map[string]string{"write-a": "/repo", "write-b": "/repo"}
	locks := NewLockRegistry()
	ready := ReadyTasks(run, DefaultPolicy(), 0, nil, cwd, locks)
	if !reflect.DeepEqual(ready, []string{"write-a"}) {
		t.Fatalf("同 cwd 的两个写任务只应选第一个，实际 %v", ready)
	}
	// 注册表已有外部持锁者时全部阻塞。
	locked := NewLockRegistry()
	if !locked.Acquire("/repo", "other-run-task") {
		t.Fatal("空注册表获取写锁应成功")
	}
	ready = ReadyTasks(run, DefaultPolicy(), 0, nil, cwd, locked)
	if len(ready) != 0 {
		t.Fatalf("cwd 已被持锁时应阻塞写任务，实际 %v", ready)
	}
	// 只读任务可与写任务并行（§6.4 规则 3）。
	run2 := runWithTasks(
		taskWith("write-a", "dev", TaskPlanned),
		taskWith("read-b", "rev", TaskPlanned),
	)
	run2.Tasks[0].WriteLock = TaskWriteLockExclusive
	run2.Tasks[1].WriteLock = TaskWriteLockNone
	ready = ReadyTasks(run2, DefaultPolicy(), 0, nil, cwd, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"write-a", "read-b"}) {
		t.Fatalf("写任务与只读任务应并行选中，实际 %v", ready)
	}
	// 空 writeLock 视为 exclusive（历史数据默认写策略）。
	run3 := runWithTasks(taskWith("legacy", "dev", TaskPlanned))
	run3.Tasks[0].WriteLock = ""
	ready = ReadyTasks(run3, DefaultPolicy(), 0, nil, map[string]string{"legacy": ""}, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"legacy"}) {
		t.Fatalf("空 cwd 的写任务不受锁约束，实际 %v", ready)
	}
	if ready2 := ReadyTasks(run3, DefaultPolicy(), 0, nil, nil, NewLockRegistry()); len(ready2) != 0 {
		t.Fatalf("exclusive 任务缺 cwd 条目应跳过，实际 %v", ready2)
	}
}

func TestReadyTasksRespectsParallelCap(t *testing.T) {
	policy := DefaultPolicy() // MaxParallelTasks=3。
	run := runWithTasks(
		taskWith("a", "dev", TaskPlanned),
		taskWith("b", "rev", TaskPlanned),
		taskWith("c", "lead", TaskPlanned),
	)
	if ready := ReadyTasks(run, policy, 3, nil, nil, NewLockRegistry()); len(ready) != 0 {
		t.Fatalf("在途已满不应再选任务，实际 %v", ready)
	}
	ready := ReadyTasks(run, policy, 2, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"a"}) {
		t.Fatalf("只剩 1 个并发额度应只选第一条，实际 %v", ready)
	}
	ready = ReadyTasks(run, policy, 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"a", "b", "c"}) {
		t.Fatalf("额度充足应全选，实际 %v", ready)
	}
	// 上限非法（<1）时不派发，宁可停住等待修正策略。
	broken := policy
	broken.MaxParallelTasks = 0
	if ready := ReadyTasks(run, broken, 0, nil, nil, NewLockRegistry()); len(ready) != 0 {
		t.Fatalf("MaxParallelTasks<1 不应派发任何任务，实际 %v", ready)
	}
}

func TestReadyTasksMemberMutualExclusion(t *testing.T) {
	run := runWithTasks(
		taskWith("a", "dev", TaskPlanned),
		taskWith("b", "dev", TaskPlanned),
		taskWith("c", "rev", TaskPlanned),
	)
	ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"a", "c"}) {
		t.Fatalf("同成员两条就绪任务只应选第一条，实际 %v", ready)
	}
	// 外部传入的成员占用同样生效（§5.2 同一成员单活动任务）。
	ready = ReadyTasks(run, DefaultPolicy(), 0, map[string]bool{"dev": true}, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"c"}) {
		t.Fatalf("dev 在途时只应选 rev 的任务，实际 %v", ready)
	}
	// 在途成员占用并计入并发上限。
	ready = ReadyTasks(run, DefaultPolicy(), 1, map[string]bool{"rev": true}, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"a"}) {
		t.Fatalf("rev 占用 + 1 在途时只应选 a，实际 %v", ready)
	}
}

func TestReadyTasksIgnoresNonCandidateStatuses(t *testing.T) {
	run := runWithTasks(
		taskWith("dispatching", "dev", TaskDispatching),
		taskWith("running", "rev", TaskRunning),
		taskWith("completed", "lead", TaskCompleted),
		taskWith("failed", "dev", TaskFailed),
	)
	if ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry()); len(ready) != 0 {
		t.Fatalf("无 planned/ready 任务时不应选中任何任务，实际 %v", ready)
	}
	if ready := ReadyTasks(nil, DefaultPolicy(), 0, nil, nil, NewLockRegistry()); ready != nil {
		t.Fatalf("nil run 应返回 nil，实际 %v", ready)
	}
}

func TestLockRegistryAcquireRelease(t *testing.T) {
	locks := NewLockRegistry()
	if holder := locks.Holder("/repo"); holder != "" {
		t.Fatalf("空注册表不应有持锁者，实际 %q", holder)
	}
	if !locks.Acquire("/repo", "task-a") {
		t.Fatal("首次获取写锁应成功")
	}
	if locks.Acquire("/repo", "task-b") {
		t.Fatal("他人持锁时获取应失败")
	}
	if !locks.Acquire("/repo", "task-a") {
		t.Fatal("同一任务重复获取应幂等成功")
	}
	if holder := locks.Holder("/repo"); holder != "task-a" {
		t.Fatalf("持锁者应为 task-a，实际 %q", holder)
	}
	locks.Release("/repo", "task-b") // 非持有者释放无效。
	if holder := locks.Holder("/repo"); holder != "task-a" {
		t.Fatalf("非持有者释放不应改变持锁者，实际 %q", holder)
	}
	locks.Release("/repo", "task-a")
	if holder := locks.Holder("/repo"); holder != "" {
		t.Fatalf("释放后应无人持锁，实际 %q", holder)
	}
}

func TestLockRegistryConcurrentAcquire(t *testing.T) {
	locks := NewLockRegistry()
	const contenders = 16
	var mu sync.Mutex
	winners := map[string]int{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if locks.Acquire("/repo", "same-task") {
				mu.Lock()
				winners["same-task"]++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()
	// 同一任务竞争获取是幂等的：人人都应成功。
	if winners["same-task"] != contenders {
		t.Fatalf("同一任务并发获取应全部成功，实际 %d/%d", winners["same-task"], contenders)
	}
}
