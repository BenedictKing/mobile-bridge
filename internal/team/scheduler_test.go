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

// ---- 依赖组释放判定（§6.5 组语义，depGroupVerdict 四处同规则的唯一权威）----

func depFixture() (map[string]string, map[string]string) {
	// 组 g：g-a/g-b；组 h：h-a/h-b；无组任务 plain。
	statuses := map[string]string{}
	groups := map[string]string{
		"g-a": "g", "g-b": "g",
		"h-a": "h", "h-b": "h",
		"plain": "",
	}
	for id := range groups {
		statuses[id] = TaskPlanned
	}
	return statuses, groups
}

func TestDepGroupVerdictTable(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(statuses map[string]string)
		deps        []string
		collect     string
		wantRelease bool
		wantBlocked bool
	}{
		// 无组依赖：阶段一规则退化。
		{"无组 completed", func(s map[string]string) { s["plain"] = TaskCompleted }, []string{"plain"}, RunCollectAll, true, false},
		{"无组 failed", func(s map[string]string) { s["plain"] = TaskFailed }, []string{"plain"}, RunCollectAll, false, true},
		{"无组 running 等待", func(s map[string]string) { s["plain"] = TaskRunning }, []string{"plain"}, RunCollectAll, false, false},
		{"无组 cancelled", func(s map[string]string) { s["plain"] = TaskCancelled }, []string{"plain"}, RunCollectFirstSuccess, false, true},
		// first_success：任一 completed 即满足（superseded 兄弟不影响）。
		{"first_success 任一 completed", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskCancelled
		}, []string{"g-a"}, RunCollectFirstSuccess, true, false},
		{"first_success 组全终态 0 completed", func(s map[string]string) {
			s["g-a"] = TaskFailed
			s["g-b"] = TaskStopped
		}, []string{"g-a"}, RunCollectFirstSuccess, false, true},
		{"first_success 组未终态等待", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskRunning
		}, []string{"g-a"}, RunCollectFirstSuccess, true, false},
		// all：全终态且 ≥1 completed 才满足。
		{"all 全终态且 1 completed", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskFailed
		}, []string{"g-a"}, RunCollectAll, true, false},
		{"all 1 completed 但组未全终态", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskRunning
		}, []string{"g-a"}, RunCollectAll, false, false},
		{"all 组全终态 0 completed", func(s map[string]string) {
			s["g-a"] = TaskFailed
			s["g-b"] = TaskCancelled
		}, []string{"g-a"}, RunCollectAll, false, true},
		// 混合依赖：组满足 + 无组依赖在飞 → 等待；组满足 + 无组 failed → 阻塞优先。
		{"混合 组满足+无组在飞", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskCompleted
			s["plain"] = TaskDispatching
		}, []string{"g-a", "plain"}, RunCollectAll, false, false},
		{"混合 组满足+无组 failed", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskCompleted
			s["plain"] = TaskFailed
		}, []string{"g-a", "plain"}, RunCollectAll, false, true},
		// 同组多依赖引用只聚合一次；跨组独立判定。
		{"双组依赖 一组等待", func(s map[string]string) {
			s["g-a"] = TaskCompleted
			s["g-b"] = TaskCompleted
			s["h-a"] = TaskRunning
		}, []string{"g-a", "g-b", "h-a"}, RunCollectAll, false, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			statuses, groups := depFixture()
			testCase.mutate(statuses)
			released, blocked := depGroupVerdict(statuses, groups, testCase.deps, testCase.collect)
			if released != testCase.wantRelease || blocked != testCase.wantBlocked {
				t.Fatalf("期望 (released=%v, blocked=%v)，实际 (%v, %v)",
					testCase.wantRelease, testCase.wantBlocked, released, blocked)
			}
		})
	}
	// 空依赖：恒释放。
	released, blocked := depGroupVerdict(map[string]string{}, map[string]string{}, nil, RunCollectAll)
	if !released || blocked {
		t.Fatalf("空依赖应释放，实际 (%v, %v)", released, blocked)
	}
}

func TestReadyTasksReleasesOnBroadcastGroup(t *testing.T) {
	// first_success：组内首个 completed 即释放下游（superseded 兄弟不得误杀）。
	run := runWithTasks(
		taskWith("b-lead", "lead", TaskCompleted),
		taskWith("b-dev", "dev", TaskCancelled),
		taskWith("merge", "rev", TaskPlanned, "b-lead", "b-dev"),
	)
	run.CollectPolicy = RunCollectFirstSuccess
	run.Tasks[0].BroadcastGroup = "b"
	run.Tasks[1].BroadcastGroup = "b"
	run.Tasks[1].StopReason = StopReasonSupersededByFirstSuccess
	ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"merge"}) {
		t.Fatalf("first_success 组内已有成功者应释放 merge，实际 %v", ready)
	}

	// all：组未全终态（dev 在飞）→ 等待。
	run2 := runWithTasks(
		taskWith("b-lead", "lead", TaskCompleted),
		taskWith("b-dev", "dev", TaskRunning),
		taskWith("merge", "rev", TaskPlanned, "b-lead", "b-dev"),
	)
	run2.CollectPolicy = RunCollectAll
	run2.Tasks[0].BroadcastGroup = "b"
	run2.Tasks[1].BroadcastGroup = "b"
	ready = ReadyTasks(run2, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if len(ready) != 0 {
		t.Fatalf("all 组未全终态不应释放下游，实际 %v", ready)
	}

	// all：组全终态且 ≥1 completed（失败成员不阻塞合并，§12 阶段二验收）。
	run3 := runWithTasks(
		taskWith("b-lead", "lead", TaskCompleted),
		taskWith("b-dev", "dev", TaskFailed),
		taskWith("merge", "rev", TaskPlanned, "b-lead", "b-dev"),
	)
	run3.CollectPolicy = RunCollectAll
	run3.Tasks[0].BroadcastGroup = "b"
	run3.Tasks[1].BroadcastGroup = "b"
	ready = ReadyTasks(run3, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"merge"}) {
		t.Fatalf("all 组全终态且含成功者应释放 merge，实际 %v", ready)
	}

	// 组全终态 0 completed：不选中（级联终止由 runner 落账）。
	run4 := runWithTasks(
		taskWith("b-lead", "lead", TaskFailed),
		taskWith("b-dev", "dev", TaskFailed),
		taskWith("merge", "rev", TaskPlanned, "b-lead", "b-dev"),
	)
	run4.CollectPolicy = RunCollectAll
	run4.Tasks[0].BroadcastGroup = "b"
	run4.Tasks[1].BroadcastGroup = "b"
	ready = ReadyTasks(run4, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if len(ready) != 0 {
		t.Fatalf("全失败组不应释放下游，实际 %v", ready)
	}
}

// TestReadyTasksActionDerivedTasks（阶段三 actions-exec 补口径）：动作派生
// 任务（delegate 的 {source}-d{round}-{index} / summarize 的 {source}-s{round}-
// {index}）以普通 planned 任务参与调度——依赖释放、成员互斥与并发上限与显式
// 计划任务同口径；summarize 的 DependsOn=已完成任务集，未完成的依赖照常等待。
func TestReadyTasksActionDerivedTasks(t *testing.T) {
	run := runWithTasks(
		taskWith("lead", "lead", TaskCompleted),
		taskWith("lead-d1-0", "dev", TaskPlanned),               // delegate 派生：无依赖，立即可派。
		taskWith("lead-s2-0", "lead", TaskPlanned, "lead-d1-0"), // summarize 派生：等 delegate 完成。
	)
	ready := ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"lead-d1-0"}) {
		t.Fatalf("summarize 派生应等待 delegate 派生完成，实际 %v", ready)
	}
	// delegate 派生完成后 summarize 释放。
	run.Tasks[1].Status = TaskCompleted
	ready = ReadyTasks(run, DefaultPolicy(), 0, nil, nil, NewLockRegistry())
	if !reflect.DeepEqual(ready, []string{"lead-s2-0"}) {
		t.Fatalf("delegate 完成后应释放 summarize 派生，实际 %v", ready)
	}
	// 成员单活动任务口径：Lead 已有在途任务时 summarize 派生不选中。
	busy := map[string]bool{"lead": true}
	if ready = ReadyTasks(run, DefaultPolicy(), 0, busy, nil, NewLockRegistry()); len(ready) != 0 {
		t.Fatalf("Lead 在途时不得选中 summarize 派生，实际 %v", ready)
	}
	// 并发上限口径：额度耗尽时派生任务同样排队等待。
	if ready = ReadyTasks(run, DefaultPolicy(), DefaultPolicy().MaxParallelTasks, nil, nil, NewLockRegistry()); len(ready) != 0 {
		t.Fatalf("并发额度耗尽时不得选中派生任务，实际 %v", ready)
	}
}
