package team

// runner.go 是编排执行主体（§6.1 调度循环、§6.5 结果即时入账、§9 暂停边界、
// §10.2 cancel/resume、§8.3 重启恢复的 I/O 侧）：Service 持有全部端口，每 Run
// 一个调度循环 goroutine（无 worker pool，§6.1）+ 每在途任务一个 watcher
// goroutine。终局分类唯一权威是 reducer.FoldRunStatus，runner 不发明终态
// 映射；任何未决 outcome_unknown 都把 run 停进 waiting_input（§9），resume
// 只做「确认不再重发」，绝不重建 submission。

import (
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// EventRunStopped 是 fold 到 stopped 的运行终局事件。model.go 的阶段一
// 事件子集未列 run_stopped（§10.3），但 resume 跳过部分任务后 run 通常落
// stopped，终局必须有事件可审计——此常量是事件表的增量补充。
const EventRunStopped = "run_stopped"

// defaultWaitSlice 是 watcher 与调度循环的等待片长：等待用 session.Wait
// （changed 通道，manager.go:1574-1582），片长 ≤25s。
const defaultWaitSlice = 25 * time.Second

// Service 是 Team 编排执行服务。构造后由装配方（httpd/main）直赋 OnEvent；
// store/journal/queue/resolver 在 NewService 注入后不再变更。
type Service struct {
	store    TeamStore
	journal  *Journal
	queue    QueueAdapter
	resolver SessionResolver
	locks    *LockRegistry

	// OnEvent 在每条 journal 事件成功落账后被调用（httpd 装配侧接
	// EventFeed.Add，把事实投影到事件流）。nil 表示无人订阅。
	OnEvent func(*Event)

	// cancelAgentTurn 取消会话中本任务自己的运行中 turn（取消/超时路径的
	// 会话副作用）。独立成字段是测试注入点：acpruntime.Session 的 changed
	// 通道未导出，进程外手工构造的 Session 调 Cancel 会 panic（signalLocked
	// 关闭 nil 通道）；生产实现恒为 cancelTurnIfOurs。
	cancelAgentTurn func(session *acpruntime.Session, sentText string) bool

	// waitSlice 是 watcher/循环的等待片长（默认 25s；测试在启动任何 Run
	// 前调小，避免固定 sleep 判正确性，§13.1）。
	waitSlice time.Duration

	mu   sync.Mutex
	runs map[string]*runState
}

// NewService 装配编排服务。四个端口都必须非 nil（生产由 main 装配；
// 测试注入内存实现）。
func NewService(store TeamStore, journal *Journal, queue QueueAdapter, resolver SessionResolver) *Service {
	service := &Service{
		store:     store,
		journal:   journal,
		queue:     queue,
		resolver:  resolver,
		locks:     NewLockRegistry(),
		waitSlice: defaultWaitSlice,
		runs:      map[string]*runState{},
	}
	service.cancelAgentTurn = service.cancelTurnIfOurs
	return service
}

// SetWaitSlice 收缩 watcher 与调度循环的等待片长。存在原因是让 internal/httpd
// 的集成测试把轮询片从默认 25s 缩到毫秒级（dispatcher_test.go 自设 waitSlice 的
// 导出面版本）——生产装配不要调用。仅在没有启动任何 Run（含 RecoverAll/Cancel/
// Resume 触达的 stateFor）之前调用才生效：已有循环与 watcher 按启动时的片长
// 运行，事后调用会被忽略并留痕；d≤0 恢复默认片长。
func (s *Service) SetWaitSlice(slice time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) > 0 {
		log.Printf("team SetWaitSlice 在 Run 已启动后被调用，已忽略: runs=%d", len(s.runs))
		return
	}
	s.waitSlice = slice
}

// runState 是单个 Run 的内存执行状态。mu 即该 Run 的运行写锁：串行化循环、
// watcher、cancel、resume 与恢复的全部「journal+快照」变更；纯读（账本轮询、
// 快照读取）不持锁。
type runState struct {
	runID string
	mu    sync.Mutex
	// wake（容量 1）在任务落定或状态变更后非阻塞唤醒调度循环。
	wake chan struct{}
	// done 在 run 到达终态时关闭：所有 watcher 与循环随即退出。
	done     chan struct{}
	finished bool // done 是否已关闭（mu 保护）
	// loopGeneration 是调度循环的代际号：每次启动（创建/resume/恢复）递增，
	// 只有最新一代循环存活，旧循环在下一轮检查点自行退出——规避「旧循环
	// 退出途中把 looping 标记留给新循环」的让位竞态。
	loopGeneration uint64
	// deadline 是 run 超时截止；每次循环启动重臂。
	deadline time.Time
	// watchers 记录在途任务的 watcher 退出信号（taskID → 关闭即退出）。
	watchers map[string]chan struct{}
	// taskCwds 记录派发时占用的写锁 cwd（终态释放用；进程重启后注册表
	// 本身为空，重启恢复由 adoptWatchersLocked 为被收养的在途 exclusive
	// 任务重新取锁——否则同一 cwd 的其他写任务会在恢复后被立即派发）。
	taskCwds map[string]string
	// dispatchedAt 是任务派发时刻（任务超时起算；恢复收养后从收养时刻起算）。
	dispatchedAt map[string]time.Time
}

// stateFor 返回（或创建）runID 的执行状态。
func (s *Service) stateFor(runID string) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.runs[runID]
	if state == nil {
		state = &runState{
			runID:        runID,
			wake:         make(chan struct{}, 1),
			done:         make(chan struct{}),
			watchers:     map[string]chan struct{}{},
			taskCwds:     map[string]string{},
			dispatchedAt: map[string]time.Time{},
		}
		s.runs[runID] = state
	}
	return state
}

// CreateRun 校验显式计划、先记账（run_created + 每任务 task_planned）再落
// 账本，然后启动调度循环（httpd 调用入口）。
func (s *Service) CreateRun(team *Team, goal, mode string, raw []PlanTaskInput) (*Run, error) {
	if team == nil {
		return nil, errors.New("team 不能为 nil")
	}
	plan, err := ParsePlan(team, goal, mode, raw)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	run := &Run{
		ID:        uuid.NewString(),
		TeamID:    team.ID,
		Goal:      plan.Goal,
		Status:    RunCreated,
		Round:     1,
		PlanHash:  plan.PlanHash,
		Tasks:     plan.Tasks,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// 先记账：payload 只含有界字段（instructionsHead 而非全量 prompt，
	// §10.3「事件流只推送有界 payload」）。teamId/goal 供 team-runs.json
	// 损坏后的 journal 重建（§8.1 快照可重建；goal 已过 10000 字校验）。
	created := newEvent(run.ID, EventRunCreated, "user", map[string]any{
		"teamId":    team.ID,
		"goal":      plan.Goal,
		"mode":      plan.Mode,
		"planHash":  plan.PlanHash,
		"taskCount": jsoncompat.Num(len(plan.Tasks)),
	})
	if err := s.journal.Append(created); err != nil {
		return nil, fmt.Errorf("run 创建记账失败: %w", err)
	}
	s.emit(created)
	for i := range plan.Tasks {
		task := &plan.Tasks[i]
		planned := newEvent(run.ID, EventTaskPlanned, "user", map[string]any{
			"taskId":           task.ID,
			"memberId":         task.MemberID,
			"instructionsHead": task.InstructionsHead,
		})
		if err := s.journal.Append(planned); err != nil {
			return nil, fmt.Errorf("run 创建记账失败: %w", err)
		}
		s.emit(planned)
	}
	// TOCTOU 补丁：创建窗口内团队可能已被并发删除（DeleteTeam 不触碰 run
	// 版本，SaveRun 无法靠版本冲突发现；账本层的 ErrActiveRuns 拦截只保护
	// 已落账的 Run）。落账前重验团队仍在，收窄「孤儿 Run」窗口。
	if s.store.Team(team.ID) == nil {
		return nil, fmt.Errorf("团队已被删除，请刷新列表")
	}
	if err := s.store.SaveRun(run); err != nil {
		return nil, err
	}
	s.startLoop(s.stateFor(run.ID))
	return run, nil
}

// ---- 调度循环 ----

// startLoop 启动（或复用）run 的调度循环。
func (s *Service) startLoop(state *runState) {
	state.mu.Lock()
	defer state.mu.Unlock()
	s.startLoopLocked(state)
}

// startLoopLocked 假定调用方持有 state.mu：递增代际号、重臂 run 超时，再脱离
// 锁启动新循环。旧一代循环在下一个检查点自行退出；两代短暂共存是安全的
// （全部变更经 state.mu 串行且按任务幂等）。
func (s *Service) startLoopLocked(state *runState) {
	if run := s.store.Run(state.runID); run != nil {
		if timeout := policyOf(s.store.Team(run.TeamID)).RunTimeoutSecs; timeout > 0 {
			state.deadline = time.Now().Add(time.Duration(timeout) * time.Second)
		} else {
			state.deadline = time.Time{}
		}
	}
	state.loopGeneration++
	generation := state.loopGeneration
	go s.runLoop(state, generation)
}

// loopCurrent 报告 generation 是否仍是存活的一代。
func (s *Service) loopCurrent(state *runState, generation uint64) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.loopGeneration == generation
}

// runLoop 是 §6.1 的核心循环：每轮做一次完整 pass（收养 watcher → 级联终止
// → 派发就绪 → 全终态 fold），然后等 wake/done/片到再跑。发现自己已是过期
// 代际（被 resume/恢复重启）即让位退出。
func (s *Service) runLoop(state *runState, generation uint64) {
	for {
		if !s.loopCurrent(state, generation) {
			return
		}
		if s.runExpired(state) {
			if s.expireRun(state) {
				return
			}
			// 超时记账失败（磁盘故障等）：等一片重试，自愈或驻留至重启恢复。
			select {
			case <-state.done:
				return
			case <-time.After(s.slice()):
			}
			continue
		}
		if !s.loopPass(state) {
			return
		}
		select {
		case <-state.wake:
		case <-state.done:
			return
		case <-time.After(s.slice()):
		}
	}
}

// loopPass 执行一轮调度决策（全程持有 state.mu）。返回 false 表示循环退出
// （run 终态/waiting_input 驻留/保存失败暂驻）。
func (s *Service) loopPass(state *runState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()

	run := s.store.Run(state.runID)
	if run == nil {
		// Run 已被删除（含随 Team 级联）：本 run 不可能再有在途任务，
		// 残留的写锁登记一并释放。
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}
	if terminalRun(run.Status) {
		s.finishLocked(state)
		return false
	}
	if run.Status == RunWaitingInput {
		return false // 驻留等待 resume；在途 watcher 继续收尾入账
	}
	team := s.store.Team(run.TeamID)
	policy := policyOf(team)
	if run.Status == RunCreated {
		// created → dispatching（§4.3；阶段一显式计划不经过 planning）。
		if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunCreated {
				return nil, false
			}
			fresh.Status = RunDispatching
			return nil, true
		}); err != nil {
			log.Printf("team run %s 状态推进失败，下轮重试: %v", run.ID, err)
			return true
		}
		if run = s.store.Run(state.runID); run == nil {
			return false
		}
	}

	// ① 收养无 watcher 的在途任务（重启恢复 / resume 后仍在飞的收尾）。
	s.adoptWatchersLocked(state, team, run, policy)

	// ② 级联终止被终态非完成依赖永久阻塞的分支（与 PlanResume 同语义）。
	s.cascadeBlockedLocked(state, run.ID)
	if run = s.store.Run(state.runID); run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}

	// ③ 派发就绪任务（§6.1：planned/ready 且依赖完成、成员空闲、写锁可得、
	// 并发上限内）。派发本身先记账后入队（dispatcher.submitTaskLocked）。
	inFlight := 0
	memberBusy := map[string]bool{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status == TaskDispatching || task.Status == TaskRunning {
			inFlight++
			memberBusy[task.MemberID] = true
		}
	}
	cwdByTask := map[string]string{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status != TaskPlanned && task.Status != TaskReady {
			continue
		}
		if task.WriteLock == TaskWriteLockNone {
			continue
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		// cwd 解析不到就不登记：ReadyTasks 对缺 cwd 的 exclusive 任务跳过
		// （无法证明写隔离，留待下一轮），绝不无锁派发。
		if cwd := s.resolver.Cwd(member.SessionID); cwd != "" {
			cwdByTask[task.ID] = cwd
		}
	}
	for _, taskID := range ReadyTasks(run, policy, inFlight, memberBusy, cwdByTask, s.locks) {
		dispatch, err := s.submitTaskLocked(state, team, run, taskID)
		if err != nil {
			// 确定性失败（内容超限、账本拒绝）：任务落 failed，分支级联终止。
			s.applyTaskTerminalLocked(state, run.ID, taskID, TaskFailed, "", err.Error(), nil, EventTaskFailed, "runner")
			continue
		}
		if dispatch == nil {
			continue // 本轮暂缓（写锁被占等）
		}
		s.spawnWatcherLocked(state, dispatch, policy, time.Now())
	}

	// ④ 全部任务终态 → fold（终局唯一权威，§3.1 原则 5）。
	if run = s.store.Run(state.runID); run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}
	if status, stopReason := FoldRunStatus(run.Tasks); status != "" {
		return s.finishRunLocked(state, run, status, stopReason)
	}
	return true
}

// adoptWatchersLocked 为「快照 dispatching/running 且有 submissionId 但没有
// 活 watcher」的任务重建 watcher（重启恢复、resume 后仍在飞的收尾）。
// sentText 从不可变 prompt + 依赖结果确定性重算（§6.2 幂等键派生的同款
// 稳定性），任务超时从收养时刻起算。同时在途 exclusive 任务在此重建
// cwd 写锁登记（§6.4 规则 2）：LockRegistry 纯内存、进程重启即空，不重取
// 锁的话同一 cwd 的其他 exclusive 任务会因 Holder 为空被立即派发，与被
// 收养任务并发写同一工作区。
func (s *Service) adoptWatchersLocked(state *runState, team *Team, run *Run, policy TeamPolicy) {
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status != TaskDispatching && task.Status != TaskRunning {
			continue
		}
		if task.SubmissionID == "" {
			continue
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		// 写锁重建：本进程派发路径已登记的（taskCwds 有记录且任务未终态）
		// 不重复取锁；重启后首次收养时补登记。cwd 解析不到时无法证明写
		// 隔离，留痕不登记（任务本就在途，无法撤回，但绝不因此放行同 cwd
		// 的后续派发——派发侧缺 cwd 一律跳过）。取锁失败说明另一在途任务
		// 已持同 cwd 锁（重启前就并发的既成事实），同样留痕跳过。
		if task.WriteLock != TaskWriteLockNone {
			if _, held := state.taskCwds[task.ID]; !held {
				if cwd := s.resolver.Cwd(member.SessionID); cwd != "" {
					if s.locks.Acquire(cwd, task.ID) {
						state.taskCwds[task.ID] = cwd
					} else {
						log.Printf("team run %s 收养任务 %s 时 %s 的写锁已被任务 %s 持有，跳过登记（重启前并发的既成事实）",
							run.ID, task.ID, cwd, s.locks.Holder(cwd))
					}
				} else {
					log.Printf("team run %s 收养 exclusive 任务 %s 时无法解析会话 %s 的 cwd，写锁未登记",
						run.ID, task.ID, member.SessionID)
				}
			}
		}
		if _, watched := state.watchers[task.ID]; watched {
			continue
		}
		context := BuildTaskContext(run, task, policy)
		s.spawnWatcherLocked(state, &taskDispatch{
			taskID:       task.ID,
			sessionID:    member.SessionID,
			submissionID: task.SubmissionID,
			sentText:     ComposeTaskMessage(context.Text, task.Prompt),
		}, policy, time.Now())
	}
}

// cascadeBlockedLocked 把「依赖已终态但非 completed」的 planned/ready 任务
// 级联终止（分支已死，run 永远等不到它们完成）。按拓扑序单遍收敛
// （a stopped 会继续传导给依赖 a 的 b），与 PlanResume 的恢复级联同语义。
func (s *Service) cascadeBlockedLocked(state *runState, runID string) {
	s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		statuses := map[string]string{}
		for i := range fresh.Tasks {
			statuses[fresh.Tasks[i].ID] = fresh.Tasks[i].Status
		}
		var events []*Event
		for _, ordered := range sortTasksByOrdinal(fresh.Tasks) {
			switch statuses[ordered.ID] {
			case TaskPlanned, TaskReady:
			default:
				continue
			}
			blocked := false
			for _, dep := range ordered.DependsOn {
				switch statuses[dep] {
				case TaskCompleted:
				case TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
					blocked = true
				}
			}
			if !blocked {
				continue
			}
			for i := range fresh.Tasks {
				task := &fresh.Tasks[i]
				if task.ID != ordered.ID {
					continue
				}
				task.Status = TaskStopped
				task.StopReason = "依赖分支已终止，任务不再执行"
				statuses[task.ID] = TaskStopped
				events = append(events, newEvent(fresh.ID, EventTaskStopped, "runner", map[string]any{
					"taskId":     task.ID,
					"stopReason": task.StopReason,
				}))
				break
			}
		}
		return events, len(events) > 0
	})
}

// spawnWatcherLocked 为一次派发启动 watcher goroutine（幂等：同任务已有
// watcher 时不重复启动）。exit 通道在 watcher 退出时关闭，供测试与收养
// 判断存活。
func (s *Service) spawnWatcherLocked(state *runState, dispatch *taskDispatch, policy TeamPolicy, dispatchedAt time.Time) {
	if _, watched := state.watchers[dispatch.taskID]; watched {
		return
	}
	exit := make(chan struct{})
	state.watchers[dispatch.taskID] = exit
	state.dispatchedAt[dispatch.taskID] = dispatchedAt
	go func() {
		defer close(exit)
		defer s.watchExited(state, dispatch.taskID)
		s.watchTask(state, dispatch, policy)
	}()
}

// watchExited 清理 watcher 登记并唤醒循环（在途任务失去 watcher 时重新
// 收养/推进）。
func (s *Service) watchExited(state *runState, taskID string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.watchers, taskID)
	delete(state.dispatchedAt, taskID)
	s.wakeLocked(state)
}

// ---- watcher：任务终态观察 ----

// watchTask 轮询观察一个在途任务直到终态。分类依据是实测 turn.status
// （turn 顶层 error 字段恒为 nil，view.go:54）：
//
//	账本 queued → 继续等；failed → 任务 failed；cancelled/ignored → 任务
//	stopped（消息已在队列中移除）；accepted/unknown → 每轮 Touch 保活（idle
//	sweeper 每 30s 关 5 分钟无人读的会话并取消运行中 turn，manager.go:
//	1501-1526）→ Snapshot 按 sentText 全文匹配本任务 turn → inProgress 继续等；
//	completed → ExtractResult 落 completed（§6.5 结果即时入账）；failed →
//	任务 failed（错误文本取 turn messages 里 kind==error 条目）；interrupted
//	或 stopReason==cancelled → 任务 stopped（cancelled），绝不记 completed。
func (s *Service) watchTask(state *runState, dispatch *taskDispatch, policy TeamPolicy) {
	slice := s.slice()
	taskTimeout := time.Duration(policy.TaskTimeoutSecs) * time.Second
	var session *acpruntime.Session
	for {
		settled, live := s.pollTask(state, dispatch, policy, session)
		if settled {
			return
		}
		session = live
		// 等待一片：会话可读时用 changed 通道（turn 落定即醒），否则定时。
		if session != nil && !session.IsClosed() {
			session.Wait(slice)
			select {
			case <-state.done:
				return
			default:
			}
		} else {
			select {
			case <-state.done:
				return
			case <-time.After(slice):
			}
		}
		if taskTimeout > 0 {
			state.mu.Lock()
			dispatchedAt := state.dispatchedAt[dispatch.taskID]
			state.mu.Unlock()
			if !dispatchedAt.IsZero() && time.Since(dispatchedAt) >= taskTimeout {
				s.expireTask(state, dispatch)
				return
			}
		}
	}
}

// pollTask 观察一轮并尽可能把任务落终态。不持 state.mu（Resolve/Snapshot 是
// 慢 I/O）；落账走 settle 包装（内部短暂持锁）。返回 (settled, 供等待用的
// 会话句柄)。
func (s *Service) pollTask(state *runState, dispatch *taskDispatch, policy TeamPolicy, session *acpruntime.Session) (bool, *acpruntime.Session) {
	// 任务已被外部（取消/resume/超时）落终态 → no-op 退出（与用户在原会话
	// 手动停止不存在竞争：先记账者的结论生效）。Run 被删除同样退出。
	if run := s.store.Run(state.runID); run == nil {
		return true, session
	} else if task := findTaskIn(run, dispatch.taskID); task != nil && terminalTask(task.Status) {
		return true, session
	}
	sub, ok := s.queue.Submission(dispatch.sessionID, dispatch.submissionID)
	if !ok {
		// 账本条目丢失（数据目录被清理等）：无法确认发送结果，绝不重发。
		s.settleUnknown(state, state.runID, dispatch.taskID, "账本条目丢失，无法确认发送结果，需人工对账")
		return true, session
	}
	switch sub.Status {
	case SubStatusQueued:
		return false, session // 交给既有 3s worker / settle 钩子派发
	case SubStatusFailed:
		s.failTask(state, state.runID, dispatch.taskID, firstNonEmpty(sub.Error, "队列派发失败"), "")
		return true, session
	case SubStatusCancelled, SubStatusIgnored:
		s.stopTask(state, state.runID, dispatch.taskID, "消息已在队列中移除")
		return true, session
	}
	// accepted / unknown：进入会话观察。
	if _, resolved, _ := s.resolver.Resolve(dispatch.sessionID); resolved != nil {
		session = resolved
		session.Touch()
		snapshot := session.Snapshot()
		if turn := matchTurn(snapshot, dispatch.sentText); turn != nil {
			if task := findTaskIn(s.store.Run(state.runID), dispatch.taskID); task != nil && task.Status == TaskDispatching {
				turnID, _ := turn.MustGet("id").(string)
				s.markRunning(state, state.runID, dispatch.taskID, dispatch.sessionID, turnID)
			}
			switch turnStatus(turn) {
			case "inProgress":
				return false, session
			case "completed":
				result := ExtractResult(dispatch.sessionID, turn, policy.MaxResultChars)
				s.completeTask(state, state.runID, dispatch.taskID, &result)
				return true, session
			case "failed":
				s.failTask(state, state.runID, dispatch.taskID, turnErrorText(turn), "")
				return true, session
			default:
				if cancelledTurn(turn) {
					s.stopTask(state, state.runID, dispatch.taskID, "cancelled")
					return true, session
				}
				return false, session // 未知形态：继续观察
			}
		}
		// 找不到本任务 turn：账本未确认且会话空闲 → 结果未知（§8.3 第 6 条）。
		if status, _ := snapshot.MustGet("status").(string); status != "active" && sub.Status == SubStatusUnknown {
			s.settleUnknown(state, state.runID, dispatch.taskID, "发送结果未知，需人工对账")
			return true, session
		}
		// 会话忙于其他 turn（用户手动发起）：继续等，超时兜底。
		return false, session
	}
	// 会话暂不可达（进程打不开等）：继续等，任务超时兜底。
	return false, session
}

// expireTask 处理任务超时：先标终态（failed / task_timeout），再取消副作用
// （仍在队列则撤条目；本任务 turn 仍在跑则取消——只取消自己的 turn，绝不
// 误杀用户回合）。
func (s *Service) expireTask(state *runState, dispatch *taskDispatch) {
	s.failTask(state, state.runID, dispatch.taskID, "任务运行超时，已停止", "task_timeout")
	s.cancelSideEffects(cancelTarget{
		sessionID:    dispatch.sessionID,
		submissionID: dispatch.submissionID,
		sentText:     dispatch.sentText,
	})
}

// expireRun 处理 run 超时：先记账（全部非终态任务 failed / run_timeout），再
// fold 落 run 终态（failed），最后取消副作用。返回 run 是否已到达终态（false
// 表示记账失败，调用方应择机重试）。
func (s *Service) expireRun(state *runState) bool {
	var targets []cancelTarget
	finished := false
	func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		run := s.store.Run(state.runID)
		if run == nil || terminalRun(run.Status) {
			// Run 消失或已被并发落终局：终局路径自身负责释放写锁，这里
			// 兜底清掉可能残留的登记（幂等）。
			s.releaseAllTaskLocksLocked(state)
			s.finishLocked(state)
			finished = true
			return
		}
		team := s.store.Team(run.TeamID)
		policy := policyOf(team)
		if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
			targets = targets[:0]
			var events []*Event
			for i := range fresh.Tasks {
				task := &fresh.Tasks[i]
				if terminalTask(task.Status) {
					continue
				}
				task.Status = TaskFailed
				task.StopReason = "run_timeout"
				task.Error = "运行超时，任务被终止"
				events = append(events, newEvent(fresh.ID, EventTaskFailed, "runner", map[string]any{
					"taskId":     task.ID,
					"error":      task.Error,
					"stopReason": "run_timeout",
				}))
				if task.SubmissionID == "" {
					continue
				}
				member := memberOf(team, task.MemberID)
				if member == nil {
					continue
				}
				context := BuildTaskContext(fresh, task, policy)
				targets = append(targets, cancelTarget{
					sessionID:    member.SessionID,
					submissionID: task.SubmissionID,
					sentText:     ComposeTaskMessage(context.Text, task.Prompt),
				})
			}
			return events, len(events) > 0
		}); err != nil {
			log.Printf("team run %s 超时记账失败，稍后重试: %v", run.ID, err)
			return
		}
		if fresh := s.store.Run(state.runID); fresh != nil {
			if status, stopReason := FoldRunStatus(fresh.Tasks); status != "" {
				// finishRunLocked 返回 false 表示已落终局。
				finished = !s.finishRunLocked(state, fresh, status, stopReason)
			}
		}
	}()
	for _, target := range targets {
		s.cancelSideEffects(target)
	}
	return finished
}

// ---- 终态落账（settle 原语）----

// applyTaskTerminalLocked 把任务落到指定终态（调用方持有 state.mu）：
// 任务或 run 已终态时 no-op（first-wins，§3.1 原则 5）；否则 journal 先行、
// 快照随后，最后释放写锁并唤醒循环。返回是否真的落账。
func (s *Service) applyTaskTerminalLocked(state *runState, runID, taskID, status, stopReason, errorText string, result *TaskResult, eventType, source string) bool {
	applied := false
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		task := findTaskIn(fresh, taskID)
		if task == nil || terminalTask(task.Status) {
			return nil, false
		}
		task.Status = status
		task.StopReason = stopReason
		task.Error = errorText
		if result != nil {
			task.Result = result.Result
			task.ResultDetail = result
		}
		payload := map[string]any{"taskId": taskID}
		switch eventType {
		case EventTaskCompleted:
			if result != nil {
				payload["sessionId"] = result.SessionID
				payload["turnId"] = result.TurnID
				payload["result"] = result.Result
				payload["resultChars"] = jsoncompat.Num(result.ResultChars)
				payload["truncated"] = result.Truncated
				if result.StopReason != "" {
					payload["stopReason"] = result.StopReason
				}
			}
		case EventTaskFailed:
			payload["error"] = errorText
			if stopReason != "" {
				payload["stopReason"] = stopReason
			}
		default: // task_stopped / task_outcome_unknown
			payload["stopReason"] = stopReason
		}
		applied = true
		return []*Event{newEvent(runID, eventType, source, payload)}, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 终态记账失败（watcher 继续观察重试）: %v", runID, taskID, err)
		return false
	}
	if applied {
		s.releaseTaskLockLocked(state, taskID)
		s.wakeLocked(state)
	}
	return applied
}

// completeTask 落任务 completed（§6.5 salvage：结果产生即入账，携带
// TaskResult 引用）。
func (s *Service) completeTask(state *runState, runID, taskID string, result *TaskResult) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	stopReason := ""
	if result != nil {
		stopReason = result.StopReason
	}
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskCompleted, stopReason, "", result, EventTaskCompleted, "watcher")
}

// failTask 落任务 failed（确定性错误，重放仍会失败，§4.3）。
func (s *Service) failTask(state *runState, runID, taskID, errorText, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskFailed, stopReason, errorText, nil, EventTaskFailed, "watcher")
}

// stopTask 落任务 stopped（用户/中断类外因，可对账继续）。
func (s *Service) stopTask(state *runState, runID, taskID, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskStopped, stopReason, "", nil, EventTaskStopped, "watcher")
}

// settleUnknown 落任务 outcome_unknown 并把 run 停进 waiting_input（§9 唯一
// 规则：未决 unknown 暂停新派发、在途 turn 收尾入账、驻留等待 resume；不
// fold、不重发）。run 已终态或已 waiting_input 时后者幂等跳过。
func (s *Service) settleUnknown(state *runState, runID, taskID, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !s.applyTaskTerminalLocked(state, runID, taskID, TaskOutcomeUnknown, stopReason, "", nil, EventTaskOutcomeUnknown, "watcher") {
		return false
	}
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if fresh.Status == RunWaitingInput || terminalRun(fresh.Status) {
			return nil, false
		}
		fresh.Status = RunWaitingInput
		fresh.StopReason = fmt.Sprintf("任务 %s 结果未知，等待用户处理", taskID)
		return []*Event{newEvent(runID, EventRunWaitingInput, "watcher", map[string]any{
			"reason": fresh.StopReason,
		})}, true
	})
	if err != nil {
		log.Printf("team run %s waiting_input 记账失败: %v", runID, err)
	}
	return true
}

// markRunning 把 dispatching 任务推到 running 并记 task_running 事件。
func (s *Service) markRunning(state *runState, runID, taskID, sessionID, turnID string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		task := findTaskIn(fresh, taskID)
		if task == nil || task.Status != TaskDispatching {
			return nil, false
		}
		task.Status = TaskRunning
		return []*Event{newEvent(runID, EventTaskRunning, "watcher", map[string]any{
			"taskId":    taskID,
			"sessionId": sessionID,
			"turnId":    turnID,
		})}, true
	}); err != nil {
		log.Printf("team run %s 任务 %s running 记账失败: %v", runID, taskID, err)
	}
}

// releaseTaskLockLocked 释放任务占用的项目写锁。
func (s *Service) releaseTaskLockLocked(state *runState, taskID string) {
	if cwd, held := state.taskCwds[taskID]; held {
		s.locks.Release(cwd, taskID)
		delete(state.taskCwds, taskID)
	}
}

// releaseAllTaskLocksLocked 释放 run 占用的全部项目写锁并清空登记（调用方
// 持有 state.mu）。供全部终局收尾路径共用（fold/取消/Run 消失）；记账失败
// 的中间路径绝不能调用——锁的存续必须与任务的在途状态一致。
func (s *Service) releaseAllTaskLocksLocked(state *runState) {
	for taskID, cwd := range state.taskCwds {
		s.locks.Release(cwd, taskID)
	}
	state.taskCwds = map[string]string{}
}

// finishRunLocked 把 run 落到 fold 终态并收尾（释放全部写锁、关闭 done）。
// 记账失败时返回 true 保持循环存活，下一轮重新 fold。
func (s *Service) finishRunLocked(state *runState, run *Run, status, stopReason string) bool {
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false // first-wins：已有终局（如取消先到）
		}
		fresh.Status = status
		fresh.StopReason = stopReason
		if eventType := runStatusEvent(status); eventType != "" {
			payload := map[string]any{}
			if stopReason != "" {
				payload["stopReason"] = stopReason
			}
			return []*Event{newEvent(fresh.ID, eventType, "runner", payload)}, true
		}
		return nil, true
	})
	s.releaseAllTaskLocksLocked(state)
	if err != nil {
		log.Printf("team run %s 终局记账失败，循环保持存活待重试: %v", run.ID, err)
		return true
	}
	s.finishLocked(state)
	return false
}

// ---- 记账基础设施 ----

// commitRun 执行「journal 先行、快照随后」的 run 变更（§3.1 原则 1/4；
// 调用方持有 state.mu）。mutate 应用到重载副本并返回 (需落账的事件, 是否
// 有变更)——事件可以为空但仍有变更（纯状态推进：waiting_input→dispatching、
// 对账回 ready 等，此时只保存不记账）；无变更则直接返回。SaveRun 版本冲突
// 时重载重试：进程内写者已被 state.mu 串行化，冲突只能来自外部写者；重试
// 路径重复落账的 journal 事件内容一致，重放按任务幂等、last-wins，可容忍。
func (s *Service) commitRun(runID string, mutate func(run *Run) ([]*Event, bool)) error {
	const attempts = 5
	var lastErr error
	for i := 0; i < attempts; i++ {
		run := s.store.Run(runID)
		if run == nil {
			return fmt.Errorf("%w: run=%s", ErrNotFound, runID)
		}
		events, changed := mutate(run)
		if !changed && len(events) == 0 {
			return nil
		}
		for _, event := range events {
			if err := s.journal.Append(event); err != nil {
				return fmt.Errorf("journal 写入失败: %w", err)
			}
			s.emit(event)
		}
		run.UpdatedAt = time.Now()
		lastErr = s.store.SaveRun(run)
		if lastErr == nil {
			return nil
		}
		if !errors.Is(lastErr, ErrVersionConflict) {
			return fmt.Errorf("team-runs.json 写入失败: %w", lastErr)
		}
	}
	return fmt.Errorf("run %s 保存连续版本冲突: %w", runID, lastErr)
}

func (s *Service) emit(event *Event) {
	if s.OnEvent != nil {
		s.OnEvent(event)
	}
}

// newEvent 构造 journal 事件（Sequence 由 Journal.Append 统一盖写）。
func newEvent(runID, eventType, source string, payload map[string]any) *Event {
	return &Event{
		ID:      uuid.NewString(),
		RunID:   runID,
		Type:    eventType,
		Source:  source,
		Payload: payload,
	}
}

func (s *Service) wakeLocked(state *runState) {
	select {
	case state.wake <- struct{}{}:
	default:
	}
}

func (s *Service) finishLocked(state *runState) {
	if !state.finished {
		state.finished = true
		close(state.done)
	}
}

func (s *Service) runExpired(state *runState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.deadline.IsZero() && time.Now().After(state.deadline)
}

func (s *Service) slice() time.Duration {
	if s.waitSlice > 0 {
		return s.waitSlice
	}
	return defaultWaitSlice
}

// ---- 用户操作：Cancel / Resume ----

// Cancel 取消整个运行（§10.2）。顺序保证 watcher no-op：先把全部非终态任务
// 与 run 本体在 store/journal 落 cancelled（先记账），再逐个执行副作用
// （撤账本条目 / 取消会话 turn）。重复取消（已终态）返回冲突错误
// （ErrRunFinished，HTTP 侧映射 409）。
func (s *Service) Cancel(runID string) (*Run, error) {
	state := s.stateFor(runID)
	var targets []cancelTarget
	err := func() error {
		state.mu.Lock()
		defer state.mu.Unlock()
		run := s.store.Run(runID)
		if run == nil {
			return fmt.Errorf("%w: run=%s", ErrNotFound, runID)
		}
		if terminalRun(run.Status) {
			return fmt.Errorf("%w: 当前状态 %s", ErrRunFinished, run.Status)
		}
		team := s.store.Team(run.TeamID)
		policy := policyOf(team)
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			targets = targets[:0]
			var events []*Event
			for i := range fresh.Tasks {
				task := &fresh.Tasks[i]
				if terminalTask(task.Status) {
					continue
				}
				task.Status = TaskCancelled
				task.StopReason = "用户取消运行"
				events = append(events, newEvent(fresh.ID, EventTaskStopped, "user", map[string]any{
					"taskId":     task.ID,
					"stopReason": task.StopReason,
				}))
				if task.SubmissionID == "" {
					continue
				}
				member := memberOf(team, task.MemberID)
				if member == nil {
					continue
				}
				context := BuildTaskContext(fresh, task, policy)
				targets = append(targets, cancelTarget{
					sessionID:    member.SessionID,
					submissionID: task.SubmissionID,
					sentText:     ComposeTaskMessage(context.Text, task.Prompt),
				})
			}
			fresh.Status = RunCancelled
			fresh.StopReason = "用户取消运行"
			events = append(events, newEvent(fresh.ID, EventRunCancelled, "user", map[string]any{
				"stopReason": fresh.StopReason,
			}))
			return events, true
		}); err != nil {
			return err
		}
		// 记账已落：全部非终态任务 cancelled、run 落 cancelled。Cancel 不经
		// finishRunLocked 的 fold 收尾，必须在此显式释放本项目持有的全部
		// 写锁——否则 LockRegistry 里 cwd→taskID 的登记会残留到进程重启，
		// 同一 cwd 的后续 exclusive 任务（任何 Run/Team）永远无法派发，且
		// 投影（writeLockHeldBy 按任务状态推导）显示无持有者，掩盖死锁。
		// 副作用（撤条目/取消 turn）与 expireRun 同序：锁释放先行。
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		s.wakeLocked(state)
		return nil
	}()
	if err != nil {
		return nil, err
	}
	for _, target := range targets {
		s.cancelSideEffects(target)
	}
	return s.store.Run(runID), nil
}

// Resume 恢复 waiting_input 的运行（§10.2：resume 只用于外因暂停，不得把
// 确定性 failed 或未决 unknown 直接改成 running）。流程：PlanResume 纯决策 →
// 未决 unknown 任务落 stopped（『结果未知，用户已确认不再重发』，绝不重发、
// 不重建 submission）→ 应用其余转移 → 可继续分支重启调度循环；无可派发且
// 全终态时 fold（存在 stopped → run stopped）。
func (s *Service) Resume(runID string) (*Run, error) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(runID)
	if run == nil {
		return nil, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	if run.Status != RunWaitingInput {
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, run.Status)
	}
	transitions, ok := PlanResume(run)
	if !ok {
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, run.Status)
	}
	folded := false
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		events, didFold := applyTransitions(fresh, transitions, "user")
		folded = didFold
		return events, len(events) > 0 || len(transitions) > 0
	}); err != nil {
		return nil, err
	}
	if folded {
		s.finishLocked(state)
	} else {
		// 仍有可派发分支或已在途收尾：回到执行态并重启循环。
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunWaitingInput {
				return nil, false
			}
			fresh.Status = RunDispatching
			return nil, true
		}); err != nil {
			return nil, err
		}
		s.startLoopLocked(state)
	}
	return s.store.Run(runID), nil
}

// ---- 重启恢复（§8.3 的 I/O 侧）----

// RecoverRun 对单个 run 跑启动对账：journal 重放 + Probe（submissions 账本
// 与会话存活）→ ReconcileRun 纯决策 → 落账 → 未终局的 run 重启调度循环
// （在途任务由循环收养 watcher）；waiting_input 驻留等待 resume。终态或
// 不存在的 run 直接跳过。
func (s *Service) RecoverRun(runID string) error {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(runID)
	if run == nil || terminalRun(run.Status) {
		return nil
	}
	events, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("run %s 对账失败: %w", runID, err)
	}
	transitions := ReconcileRun(run, events, &serviceProbe{service: s})
	if len(transitions) > 0 {
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			applied, _ := applyTransitions(fresh, transitions, "runner")
			return applied, len(applied) > 0 || len(transitions) > 0
		}); err != nil {
			return err
		}
	}
	// 崩溃窗口补偿：journal 已记账、快照未保存的 dispatching/running 任务
	// 补回 submissionId/attempt（幂等键确定性派生，§6.2）。
	s.backfillDispatchesLocked(runID)
	fresh := s.store.Run(runID)
	if fresh == nil || terminalRun(fresh.Status) {
		s.finishLocked(state)
		return nil
	}
	if fresh.Status == RunWaitingInput {
		return nil
	}
	s.startLoopLocked(state)
	return nil
}

// RecoverAll 在网关启动时对全部 run 逐个对账；单个 run 失败不阻断其余
// （返回首个错误）。
func (s *Service) RecoverAll() error {
	var firstErr error
	for _, run := range s.store.Runs() {
		if err := s.RecoverRun(run.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// backfillDispatchesLocked 为缺 submissionId 的 dispatching/running 任务按
// 确定性幂等键补全引用（ReconcileRun 的 Transition 不携带 submissionId，
// journal 记账先于快照保存的崩溃窗口会留下空引用）。
func (s *Service) backfillDispatchesLocked(runID string) {
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		changed := false
		for i := range fresh.Tasks {
			task := &fresh.Tasks[i]
			if task.SubmissionID != "" {
				continue
			}
			if task.Status != TaskDispatching && task.Status != TaskRunning {
				continue
			}
			attempt := task.Attempt
			if attempt < firstAttempt {
				attempt = firstAttempt
			}
			task.Attempt = attempt
			task.SubmissionID = TaskSubmissionID(fresh.TeamID, fresh.ID, task.ID, attempt)
			changed = true
		}
		// 纯快照修复：journal 里已有派发事实，不重复记账。
		return nil, changed
	}); err != nil {
		log.Printf("team run %s 派发引用补全失败: %v", runID, err)
	}
}

// ---- Transition 应用与状态判别 ----

// applyTransitions 把 PlanResume / ReconcileRun 的 Transition 列表应用到 run
// 副本并产出需落账的事件（EventType 非空的转移随迁记账；TransitionRunStatus
// 的事件按目标状态推导——PlanResume 的 fold 指令不携带事件类型）。返回
// (events, 是否 fold 到终态)。
func applyTransitions(run *Run, transitions []Transition, source string) ([]*Event, bool) {
	var events []*Event
	folded := false
	for _, transition := range transitions {
		switch transition.Kind {
		case TransitionTaskStatus:
			for i := range run.Tasks {
				task := &run.Tasks[i]
				if task.ID != transition.TaskID {
					continue
				}
				// 幂等重试保护：目标状态已精确应用则跳过。注意不能按
				// 「当前已终态」拦截——PlanResume 的未决 unknown→stopped 与
				// ReconcileRun 的快照校正正是要覆盖既有终态。
				if task.Status == transition.Status && task.StopReason == transition.StopReason {
					break
				}
				task.Status = transition.Status
				task.StopReason = transition.StopReason
				task.Error = transition.Error
				if transition.Result != nil {
					task.Result = transition.Result.Result
					task.ResultDetail = transition.Result
				}
				if transition.EventType != "" {
					payload := map[string]any{"taskId": task.ID}
					if transition.Result != nil && transition.EventType == EventTaskCompleted {
						payload["sessionId"] = transition.Result.SessionID
						payload["turnId"] = transition.Result.TurnID
						payload["result"] = transition.Result.Result
						payload["resultChars"] = jsoncompat.Num(transition.Result.ResultChars)
						payload["truncated"] = transition.Result.Truncated
					}
					if transition.StopReason != "" {
						payload["stopReason"] = transition.StopReason
					}
					if transition.Error != "" {
						payload["error"] = transition.Error
					}
					events = append(events, newEvent(run.ID, transition.EventType, source, payload))
				}
				break
			}
		case TransitionRunStatus:
			run.Status = transition.Status
			run.StopReason = transition.StopReason
			if terminalRun(transition.Status) {
				folded = true
			}
			if eventType := runStatusEvent(transition.Status); eventType != "" {
				payload := map[string]any{}
				if transition.StopReason != "" {
					payload["stopReason"] = transition.StopReason
				}
				events = append(events, newEvent(run.ID, eventType, source, payload))
			}
		case TransitionDispatch:
			for i := range run.Tasks {
				task := &run.Tasks[i]
				if task.ID != transition.TaskID {
					continue
				}
				if task.Status == TaskPlanned || task.Status == TaskReady {
					task.Status = transition.Status
				}
				break
			}
		}
	}
	return events, folded
}

// runStatusEvent 把 run 状态映射到 journal 事件类型；无对应事件类型的
// 状态（RunOutcomeUnknown 等防御分支）返回空串。
func runStatusEvent(status string) string {
	switch status {
	case RunCompleted:
		return EventRunCompleted
	case RunFailed:
		return EventRunFailed
	case RunCancelled:
		return EventRunCancelled
	case RunStopped:
		return EventRunStopped
	case RunWaitingInput:
		return EventRunWaitingInput
	}
	return ""
}

func terminalRun(status string) bool {
	switch status {
	case RunCompleted, RunFailed, RunCancelled, RunStopped, RunOutcomeUnknown:
		return true
	}
	return false
}

func terminalTask(status string) bool {
	switch status {
	case TaskCompleted, TaskFailed, TaskCancelled, TaskOutcomeUnknown, TaskStopped:
		return true
	}
	return false
}

func findTaskIn(run *Run, taskID string) *Task {
	if run == nil {
		return nil
	}
	for i := range run.Tasks {
		if run.Tasks[i].ID == taskID {
			return &run.Tasks[i]
		}
	}
	return nil
}

// policyOf 返回 run 所属团队的策略；团队已被删除时退回默认值（并发上限
// 等护栏仍然生效）。
func policyOf(team *Team) TeamPolicy {
	if team == nil {
		return DefaultPolicy()
	}
	return team.Policy
}
