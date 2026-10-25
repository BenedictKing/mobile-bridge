package team

// runner.go 是编排执行主体（§6.1 调度循环、§6.3 团队级派发池、§6.5 结果
// 即时入账与收集策略、§9 暂停边界、§10.2 cancel/resume、§8.3 重启恢复的
// I/O 侧）：Service 持有全部端口，每 Run 一个调度循环 goroutine + 团队级
// 派发池（信号量限定同时在执行的派发 I/O 数）+ 每在途任务一个 watcher
// goroutine。并发护栏是选择期记账：调度决策把「快照在途 + 已投递派发池」
// 一并计入 inFlight，任务从投递池到落 dispatching 始终占并发额度——任何
// 时刻同 Run 在途任务数（含 broadcast 展开）不超过 policy.MaxParallelTasks。
// 终局分类唯一权威是 reducer.FoldRunStatus，runner 不发明终态映射；任何
// 未决 outcome_unknown 都把 run 停进 waiting_input（§9），resume 只做
// 「确认不再重发」，绝不重建 submission。

import (
	"errors"
	"fmt"
	"log"
	"strings"
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

// Service 是 Team 编排执行服务。构造后由装配方（httpd/main）直赋 OnEvent
// 与 Admission；store/journal/queue/resolver 在 NewService 注入后不再变更。
type Service struct {
	store    TeamStore
	journal  *Journal
	queue    QueueAdapter
	resolver SessionResolver
	locks    *LockRegistry

	// OnEvent 在每条 journal 事件成功落账后被调用（httpd 装配侧接
	// EventFeed.Add，把事实投影到事件流）。nil 表示无人订阅。
	OnEvent func(*Event)

	// Admission 是派发准入缝（§6.3 的注入点，测试用）：派发主体在锁外、
	// 记账前调用；返回 false 表示本轮暂缓（任务保持 planned，下一轮重新
	// 决策）。nil 表示全放行。生产准入走 AIMD（见下），此缝保留给测试注入。
	Admission DispatchAdmission

	// AIMD 是 provider/model 级共享自适应桶（§6.3，aimd.go）：跨 Team 共桶，
	// executeDispatch 在锁外 Touch+Admit，watcher 结算时记账成败与在途。
	// 纯内存不落盘（空闲自然回 floor，重启重置可接受）。nil 不该出现
	// （NewService 内建），防御性判空等价关闭 AIMD。
	AIMD *aimdRegistry

	// Transcripts 是会话转录提取端口（§7 成员间转发的来源侧，可选）：
	// sourceSessionId 转发时调用；nil（未装配）时该类转发报
	// ErrTranscriptUnavailable。httpd 装配方注入。
	Transcripts TranscriptSource

	// Workspaces 是用户预备 worktree 的注册表端口（§6.4 + §16 默认决策 6，
	// 可选）：isolated 任务的 Resolve/HeadRevision/Diff/Cleanup。nil（未
	// 装配）时 isolated 任务派发确定性失败（ErrWorkspacesUnavailable），
	// TaskDiff/CleanupWorkspace 同样报错。httpd 装配方注入。
	Workspaces WorkspaceAdapter

	// cancelAgentTurn 取消会话中本任务自己的运行中 turn（取消/超时路径的
	// 会话副作用）。独立成字段是测试注入点：acpruntime.Session 的 changed
	// 通道未导出，进程外手工构造的 Session 调 Cancel 会 panic（signalLocked
	// 关闭 nil 通道）；生产实现恒为 cancelTurnIfOurs。
	cancelAgentTurn func(session *acpruntime.Session, sentText string) bool

	// verifyTimeout 是验证闸门命令的执行超时（§6.4.1，默认 600s；测试直接
	// 调小，避免用真实十分钟等超时路径）。
	verifyTimeout time.Duration

	// waitSlice 是 watcher/循环的等待片长（默认 25s；测试在启动任何 Run
	// 前调小，避免固定 sleep 判正确性，§13.1）。
	waitSlice time.Duration

	mu   sync.Mutex
	runs map[string]*runState

	// pools 是团队级派发池（teamID → 信号量池，§6.3 层 1）。容量取 Team
	// 策略的 MaxParallelTasks，策略变更时整体重建；在途派发持有旧池信号
	// 量直至完成，重建瞬间可能短暂超出新容量。
	poolsMu sync.Mutex
	pools   map[string]*teamDispatchPool
}

// teamDispatchPool 是团队级派发池：信号量限定同时在执行的派发主体数
// （准入 + 记账 + 入队 I/O）。真正的并发上限由调度选择期的 inFlight 记账
// 保证（快照 dispatching/running + pendingDispatch 全量计入）——信号量在
// 入队完成即释放，不占任务运行期。
type teamDispatchPool struct {
	capacity int
	sem      chan struct{}
}

// poolFor 返回团队的派发池（容量=MaxParallelTasks；策略变更时重建）。
func (s *Service) poolFor(team *Team) *teamDispatchPool {
	capacity := policyOf(team).MaxParallelTasks
	if capacity < 1 {
		// 绝不构造 0 容量信号量（永久阻塞）；非法策略由 ReadyTasks 拦截，
		// 本池仅兜底 1 并发，不会成为放行通道。
		capacity = 1
	}
	key := ""
	if team != nil {
		key = team.ID
	}
	s.poolsMu.Lock()
	defer s.poolsMu.Unlock()
	pool := s.pools[key]
	if pool == nil || pool.capacity != capacity {
		pool = &teamDispatchPool{capacity: capacity, sem: make(chan struct{}, capacity)}
		s.pools[key] = pool
	}
	return pool
}

// NewService 装配编排服务。四个端口都必须非 nil（生产由 main 装配；
// 测试注入内存实现）。
func NewService(store TeamStore, journal *Journal, queue QueueAdapter, resolver SessionResolver) *Service {
	service := &Service{
		store:         store,
		journal:       journal,
		queue:         queue,
		resolver:      resolver,
		locks:         NewLockRegistry(),
		AIMD:          newAIMDRegistry(nil),
		verifyTimeout: defaultVerifyTimeout,
		waitSlice:     defaultWaitSlice,
		runs:          map[string]*runState{},
		pools:         map[string]*teamDispatchPool{},
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
	// pendingDispatch 记录已投递派发池、快照尚未落终局/dispatching 的任务
	// （§6.3 并发护栏的窗口期记账）：调度选择把它们计入 inFlight 与成员
	// 互斥，executeDispatch 收尾时清除。防止「任务在池内排队、快照仍是
	// planned」被下一轮重复选中。
	pendingDispatch map[string]bool
	// taskCwds 记录派发时占用的写锁 cwd（终态释放用；进程重启后注册表
	// 本身为空，重启恢复由 adoptWatchersLocked 为被收养的在途 exclusive
	// 任务重新取锁——否则同一 cwd 的其他写任务会在恢复后被立即派发）。
	taskCwds map[string]string
	// dispatchedAt 是任务派发时刻（任务超时起算；恢复收养后从收养时刻起算）。
	dispatchedAt map[string]time.Time
	// gateLaunched 标记验证闸门 goroutine 在飞（§6.4.1，mu 保护）：RunReducing
	// 分支据此避免重复启动闸门；settleVerifyGateLocked 收尾（含记账失败）
	// 复位，resume 复验经 fold 拦截重新启动。
	gateLaunched bool
}

// stateFor 返回（或创建）runID 的执行状态。
func (s *Service) stateFor(runID string) *runState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.runs[runID]
	if state == nil {
		state = &runState{
			runID:           runID,
			wake:            make(chan struct{}, 1),
			done:            make(chan struct{}),
			watchers:        map[string]chan struct{}{},
			pendingDispatch: map[string]bool{},
			taskCwds:        map[string]string{},
			dispatchedAt:    map[string]time.Time{},
		}
		s.runs[runID] = state
	}
	return state
}

// CreateRun 校验显式计划、先记账（run_created + 每任务 task_planned）再落
// 账本，然后启动调度循环（httpd 调用入口）。mode/collect 持久化进 Run
// 快照（重启后调度循环按快照决策，广播组语义不依赖事件流）。
//
// runID 是可选的创建幂等键（评审遗留③）：空串按现行为生成 uuid；同 id 的
// 重放比对由 HTTP 侧完成（handleRunCreate，仿 handleTeamsCreate），此处只
// 负责校验与落账。verifyCommand 是 Run 声明的验证闸门（§6.4.1，nil/空表示
// 不启用），创建期过 ValidateVerifyCommand 白名单校验，并随 run_created
// 事件落账（team-runs.json 损坏后的 journal 重建据此恢复闸门声明）。
func (s *Service) CreateRun(team *Team, runID, goal, mode, collect string, verifyCommand []string, raw []PlanTaskInput) (*Run, error) {
	if team == nil {
		return nil, errors.New("team 不能为 nil")
	}
	if len(verifyCommand) > 0 {
		if err := ValidateVerifyCommand(verifyCommand); err != nil {
			return nil, err
		}
	}
	plan, err := ParsePlan(team, goal, mode, collect, raw)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		runID = uuid.NewString()
	}
	now := time.Now()
	run := &Run{
		ID:            runID,
		TeamID:        team.ID,
		Goal:          plan.Goal,
		Mode:          plan.Mode,
		CollectPolicy: plan.CollectPolicy,
		Status:        RunCreated,
		Round:         1,
		PlanHash:      plan.PlanHash,
		Tasks:         plan.Tasks,
		VerifyCommand: append([]string(nil), verifyCommand...),
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	// 先记账：payload 只含有界字段（instructionsHead 而非全量 prompt，
	// §10.3「事件流只推送有界 payload」）。teamId/goal 供 team-runs.json
	// 损坏后的 journal 重建（§8.1 快照可重建；goal 已过 10000 字校验）；
	// verifyCommand 已过白名单校验（≤8 段、单段 ≤512 字节），重建据此
	// 恢复闸门声明。
	createdPayload := map[string]any{
		"teamId":        team.ID,
		"goal":          plan.Goal,
		"mode":          plan.Mode,
		"collectPolicy": plan.CollectPolicy,
		"planHash":      plan.PlanHash,
		"taskCount":     jsoncompat.Num(len(plan.Tasks)),
	}
	if len(verifyCommand) > 0 {
		createdPayload["verifyCommand"] = append([]string(nil), verifyCommand...)
	}
	created := newEvent(run.ID, EventRunCreated, "user", createdPayload)
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

// loopPass 执行一轮调度决策，分段拆锁（§6.3 worker pool：慢 I/O 绝不
// 占用 state.mu）：①②+收集策略（锁内记账，取消副作用目标锁外执行）→
// ⓪' 收养候选 isolated 任务的工作区对账（锁外）→ ③a 候选任务 cwd 解析
// （锁外）→ ③b ReadyTasks 纯决策并投递派发池（锁内）→ ④ 全终态 fold
// （锁内）。返回 false 表示循环退出（run 终态/waiting_input 驻留/保存失败
// 暂驻）。
func (s *Service) loopPass(state *runState) bool {
	var collectTargets []cancelTarget
	alive, proceed, adoptions := s.passAdvance(state, &collectTargets)
	if !alive {
		return false
	}
	if !proceed {
		return true // 状态推进记账失败等：本轮到此为止，下轮重试。
	}
	// first_success 取消副作用（撤账本条目/取消 turn）在锁外执行——
	// resolver.Resolve 是慢 I/O，绝不持有 state.mu。
	for _, target := range collectTargets {
		s.cancelSideEffects(target)
	}
	// ⓪' 收养候选 isolated 任务的工作区对账（§6.4 重启恢复，锁外 I/O：
	// 注册表读取 + git rev-parse）：找不到 worktree 或基线不匹配 → 任务
	// 停靠 + workspace_lost + run waiting_input，绝不自动创建新 worktree。
	s.adoptWorkspaceWatchers(state, adoptions)
	// ③a：候选 exclusive 任务的 cwd 解析（锁外慢 I/O；解析不到不登记，
	// ReadyTasks 对缺 cwd 的 exclusive 任务跳过，绝不无锁派发）。
	cwdByTask := s.resolveCandidateCwds(state.runID)
	// ③b：ReadyTasks 纯决策（含组释放）→ 投递团队派发池（池内锁外派发）。
	if !s.passDispatch(state, cwdByTask) {
		return false
	}
	// ④ 全部任务终态 → fold（终局唯一权威，§3.1 原则 5）。
	return s.passFold(state)
}

// passAdvance 执行循环的推进段（短暂持有 state.mu）：状态推进、① 收养、
// ② 级联终止与收集策略记账。返回 (循环是否继续, 本轮是否可继续派发,
// 收养候选中的 isolated 任务——其工作区对账含锁外 I/O，由 loopPass 在锁外
// 逐个处理（adoptWorkspaceWatchers）后才补 watcher)。
func (s *Service) passAdvance(state *runState, collectTargets *[]cancelTarget) (bool, bool, []isolatedAdoption) {
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil {
		// Run 已被删除（含随 Team 级联）：本 run 不可能再有在途任务，
		// 残留的写锁登记一并释放。
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false, false, nil
	}
	if terminalRun(run.Status) {
		// 终局路径（finishRunLocked/Cancel）自身已释放全部写锁；此处兜底清
		// 残留登记（幂等）：run 终态 ⇒ 全部任务终态 ⇒ 没有任何任务还有资格
		// 持锁，且循环即将退出、再无 release 时机——残留即永久泄漏（同 cwd
		// 的 exclusive 任务从此无法派发，writeLockHeldBy 投影还会显示无持有
		// 者掩盖死锁）。
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false, false, nil
	}
	if run.Status == RunWaitingInput {
		return false, false, nil // 驻留等待 resume；在途 watcher 继续收尾入账
	}
	// RunReducing：验证闸门态（§6.4.1，全任务终态后、终局前的闸门窗口）。
	// 短路 ①②③——不收养、不级联终止、不派发（闸门进行中的 run 不该再产生
	// 任何调度副作用）；闸门结果已落但判定未应用（记账失败的崩溃窗口）时
	// 就地补判，否则确保闸门已启动并在飞，等片/唤醒。
	if run.Status == RunReducing {
		if run.Verify != nil {
			s.applyVerifyVerdictLocked(state, run.ID)
			return true, false, nil
		}
		if !state.gateLaunched {
			s.launchVerifyGateLocked(state, run)
		}
		return true, false, nil
	}
	team := s.store.Team(run.TeamID)
	policy := policyOf(team)
	if run.Status == RunCreated {
		// created → dispatching（§4.3；显式计划不经过 planning）。
		if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunCreated {
				return nil, false
			}
			fresh.Status = RunDispatching
			return nil, true
		}); err != nil {
			log.Printf("team run %s 状态推进失败，下轮重试: %v", run.ID, err)
			return true, false, nil
		}
		if run = s.store.Run(state.runID); run == nil {
			s.releaseAllTaskLocksLocked(state)
			s.finishLocked(state)
			return false, false, nil
		}
	}

	// ① 收养无 watcher 的在途任务（重启恢复 / resume 后仍在飞的收尾）。
	// isolated 任务的收养要经锁外工作区对账（注册表 + rev-parse），先收集
	// 候选、暂不 spawn watcher。
	adoptions := s.adoptWatchersLocked(state, team, run, policy)

	// ② 级联终止被永久阻塞依赖卡死的分支（与 PlanResume 同语义，含组规则）。
	s.cascadeBlockedLocked(state, run.ID)
	if run = s.store.Run(state.runID); run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false, false, nil
	}

	// ②' 收集策略（§6.5 first_success）：已出现成功成员的组，其余非终态
	// 成员取消（记账先行；副作用目标交锁外执行）。
	*collectTargets = s.applyCollectLocked(state, run, team, policy)
	return true, true, adoptions
}

// resolveCandidateCwds 在锁外解析候选 exclusive 任务的 cwd（§6.4 规则 1/2
// 的派发前置：cwd 解析不到就不登记，绝不无锁派发）。isolated 任务的锁键
// 是 worktree 路径（§6.4 规则 4）：注册表解析不出时登记空串（不占锁，
// ReadyTasks 照常放行）——派发主体在锁外做确定性校验并落 failed，绝不把
// 任务静默搁置在 planned。
func (s *Service) resolveCandidateCwds(runID string) map[string]string {
	run := s.store.Run(runID)
	if run == nil {
		return nil
	}
	team := s.store.Team(run.TeamID)
	cwdByTask := map[string]string{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status != TaskPlanned && task.Status != TaskReady {
			continue
		}
		if task.WorkspaceMode == WorkspaceIsolatedWorktree {
			path := ""
			if s.Workspaces != nil {
				if resolved, err := s.Workspaces.Resolve(task.WorkspaceID); err == nil {
					path = resolved
				}
			}
			cwdByTask[task.ID] = path
			continue
		}
		if task.WriteLock == TaskWriteLockNone {
			continue
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		if cwd := s.resolver.Cwd(member.SessionID); cwd != "" {
			cwdByTask[task.ID] = cwd
		}
	}
	return cwdByTask
}

// passDispatch 执行派发段（短暂持有 state.mu）：ReadyTasks 纯决策（含组
// 释放、成员互斥、写锁与并发护栏）并投递团队派发池。返回 false 表示循环
// 退出。任务从投递池到 executeDispatch 落 dispatching 期间由
// pendingDispatch 记账占并发额度与成员互斥——多任务同时在途（含 broadcast
// 展开）不超过 policy.MaxParallelTasks 的硬保证。
func (s *Service) passDispatch(state *runState, cwdByTask map[string]string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}
	if terminalRun(run.Status) {
		s.finishLocked(state)
		return false
	}
	if run.Status != RunDispatching {
		return true // waiting_input 驻留/推进失败的中间态：本轮不派发。
	}
	team := s.store.Team(run.TeamID)
	policy := policyOf(team)
	inFlight := 0
	memberBusy := map[string]bool{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status == TaskDispatching || task.Status == TaskRunning {
			inFlight++
			memberBusy[task.MemberID] = true
		}
	}
	// 已投递派发池、快照尚未落 dispatching 的任务同样占并发额度与成员
	// 互斥（窗口期记账，防重复选中/超额派发）。
	for taskID := range state.pendingDispatch {
		inFlight++
		if task := findTaskIn(run, taskID); task != nil {
			memberBusy[task.MemberID] = true
		}
	}
	for _, taskID := range ReadyTasks(run, policy, inFlight, memberBusy, cwdByTask, s.locks) {
		state.pendingDispatch[taskID] = true
		s.dispatchViaPool(state, team, policy, run.ID, taskID, cwdByTask[taskID])
	}
	return true
}

// passFold 执行折叠段（短暂持有 state.mu）：全部任务终态 → fold。
// waiting_input 的 run 绝不在此 fold——停靠/结果未知语义是「驻留等待
// resume」（§9/§6.3），fold 会把它变成终态、Resume 从此被拒。窄窗口：
// 推进段读到 dispatching 后任务落终态并停 run，折叠段重读快照时全任务
// 已终态——没有此守卫会误折（stopTaskBlocked/settleUnknown 的两段式提交
// 与本段之间的竞态）。
func (s *Service) passFold(state *runState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}
	if run.Status == RunWaitingInput {
		return true // 驻留等待 resume：不 fold（下一轮推进段退出循环）。
	}
	if status, stopReason := FoldRunStatus(run.Tasks, run.Mode, run.CollectPolicy); status != "" {
		// 验证闸门拦截（§6.4.1）：fold 判 completed 且闸门未决 → 不直接终局，
		// 推进 RunReducing 并启动闸门（退出码交判定，绝不采信 Agent 口头
		// 声明）。fold 为失败/取消等非完成终局时不拦——run 已失败，闸门无从
		// 验证「成功」。幂等：ensureVerifyGateLocked 对闸门在飞/已落 no-op。
		if status == RunCompleted && verifyPending(run) {
			s.ensureVerifyGateLocked(state, run)
			return true
		}
		return s.finishRunLocked(state, run, status, stopReason)
	}
	return true
}

// dispatchViaPool 把一次派发投递到团队派发池（§6.3）：信号量满时排队等待
// ——阻塞期即取消窗口一（executeDispatch 获槽后先复查终态）。
func (s *Service) dispatchViaPool(state *runState, team *Team, policy TeamPolicy, runID, taskID, cwd string) {
	pool := s.poolFor(team)
	go func() {
		pool.sem <- struct{}{}
		defer func() { <-pool.sem }()
		s.executeDispatch(state, team, policy, runID, taskID, cwd)
	}()
}

// executeDispatch 是派发主体（在池工作体内、锁外执行）。时序硬性规定
// （§6.3 worker pool，顺序不可调换）：
//
//	(1) 获槽后复查任务状态——等待期间被取消/级联终止/并发处理即直接返回
//	    （取消窗口一）；
//	(2) 锁外慢 I/O：AIMD 桶准入（Touch 登记团队边界 → Admit；桶键解析需
//	    Resolve+Snapshot，绝不持 state.mu）与测试注入缝 Admission；拒绝
//	    =本轮暂缓，任务保持 planned，下轮重新决策；
//	(2.5) isolated 任务的锁外工作区绑定（§6.4）：Resolve + 会话 cwd 全等
//	    校验 + 首派基线读取；失败=确定性失败，成功则写锁键改用 worktree
//	    路径；
//	(3) 短暂持 state.mu：journal 记账 + 快照落 dispatching
//	    （accountDispatchLocked，含写锁获取、幂等键派生与 workspace_bound）；
//	(4) 锁外 queue.Enqueue（外部副作用；同 id 同内容幂等）；
//	(5) Enqueue 后复查终态（取消窗口二：记账到入账间隙被 Cancel 的任务
//	    立即撤账本条目，否则孤儿条目会被既有 3s worker 派发给 Agent）；
//	(6) 只有 Enqueue 成功且任务未终态才启动 watcher——严禁沿用「记账后即起
//	    watcher」：pollTask 第一步查账本，条目缺失即 settleUnknown，
//	    早启会把未入队任务误判为 outcome_unknown + run waiting_input。
//	    watcher 启动后 AIMD 记一次在途（OnDispatch），收尾在 watcher 退出
//	    时对称 OnSettle。
//
// 确定性失败（内容超限、账本拒绝、成员缺失）落任务 failed，分支级联终止。
func (s *Service) executeDispatch(state *runState, team *Team, policy TeamPolicy, runID, taskID, cwd string) {
	// spawnSkipped 标记「已记账入队但 watcher 未生成」（前一代 watcher 尚
	// 未退出，spawnWatcherLocked 幂等跳过）：收尾时额外唤醒循环，让
	// adoptWatchersLocked 在旧 watcher 退出后补收养，否则要等一个等待片。
	spawnSkipped := false
	// 收尾清理（不唤醒循环：准入拒绝/写锁暂缓的重试按等待片节奏进行，
	// 防止「拒绝→唤醒→再投递→再拒绝」的自持热循环；需要立即推进的事件
	// ——任务落终态、用户取消——各自经 applyTaskTerminalLocked/Cancel 唤醒）。
	defer func() {
		state.mu.Lock()
		delete(state.pendingDispatch, taskID)
		if spawnSkipped {
			s.wakeLocked(state)
		}
		state.mu.Unlock()
	}()
	// (1) 取消窗口一：获槽后先复查（等待期间状态可能已变）。
	run := s.store.Run(runID)
	if run == nil {
		return
	}
	task := findTaskIn(run, taskID)
	if task == nil || (task.Status != TaskPlanned && task.Status != TaskReady) {
		return
	}
	member := memberOf(team, task.MemberID)
	if member == nil {
		s.failDispatch(state, runID, taskID, fmt.Sprintf("任务 %s 引用的成员不存在：%s", taskID, task.MemberID))
		return
	}
	// (2) 锁外慢 I/O：AIMD 桶准入（§6.3）+ 测试注入缝。桶键按会话快照的
	// latestModel 聚桶，解析不到降级 kind 级（防误聚桶）；Touch 登记该
	// Team 的 MaxProviderConcurrency/MinParallelTasks 为桶的 ceiling/floor
	// （跨 Team 取 max/min）。准入被拒（冷却/满窗）=本轮暂缓：任务保持
	// planned，下轮重新决策，registry 不主动唤醒（aimd.go Admit 注释）。
	// 预留制（aimd.go）：Admit 通过即占一个名额，本 worker 的每条出口路径
	// 必须把它转记为 watcher 在途（OnDispatch）或归还（Abandon）——准入与
	// 派发收尾之间隔着记账写盘与入队，只读检查会让多个并发 worker 全部
	// 通过后在途数超过窗口（限流后 window=1 时尤甚）。
	bucketKey := ""
	aimdReserved := false
	if s.AIMD != nil || s.Admission != nil {
		_, session, _ := s.resolver.Resolve(member.SessionID)
		if s.AIMD != nil {
			bucketKey = ProviderBucketKey(member, session)
			s.AIMD.Touch(bucketKey, policy.MaxProviderConcurrency, policy.MinParallelTasks)
			if !s.AIMD.Admit(bucketKey) {
				return
			}
			if bucketKey != "" {
				aimdReserved = true // Admit 对空键/未登记桶不预留
			}
		}
		// 注入缝仅在有会话时生效（准入是限流缝，不是放行前提）。
		if s.Admission != nil && session != nil && !s.Admission.Admit(task, member, session) {
			return
		}
	}
	defer func() {
		// 预留收尾兜底：正常路径已在步骤⑥转记（aimdReserved=false），只有
		// 中途退出（绑定失败/记账失败/暂缓/入队失败/取消窗口/spawn 跳过）
		// 会走到这里。
		if aimdReserved {
			s.AIMD.Abandon(bucketKey)
		}
	}()
	// (2.5) isolated 工作区绑定（§6.4，锁外 I/O：注册表读取 + rev-parse）：
	// 注册表缺失/会话 cwd 不在 worktree/基线读取失败都是确定性失败
	// （failDispatch 落 task failed），绝不 spawn 新会话、绝不静默搁置；
	// 成功则写锁键改用 worktree 路径（与共享 cwd 同一 LockRegistry，同
	// worktree 并行写天然互斥）。
	binding := (*workspaceBinding)(nil)
	if task.WorkspaceMode == WorkspaceIsolatedWorktree {
		var bindErr error
		binding, bindErr = s.resolveWorkspaceBinding(task, member)
		if bindErr != nil {
			s.failDispatch(state, runID, taskID, bindErr.Error())
			return
		}
		cwd = binding.path
	}
	// (3) 短暂持锁：记账（先记账后发送，§3.1 原则 4）。accountDispatchLocked
	// 的调用约定是持有 state.mu（写 state.taskCwds 等 run 级内存状态）——
	// broadcast 的多个派发池 worker 会并发到达此处，裸调是 map 数据竞争
	//（阶段二集成测试 -race 实测：dispatcher.go 的 state.taskCwds 赋值）。
	state.mu.Lock()
	dispatch, err := s.accountDispatchLocked(state, team, run, taskID, cwd, binding)
	state.mu.Unlock()
	if err != nil {
		s.failDispatch(state, runID, taskID, err.Error())
		return
	}
	if dispatch == nil {
		return // 本轮暂缓（写锁被占等）
	}
	dispatch.bucketKey = bucketKey
	// (4) 锁外入队（mode=queue，绝不立即派发；duplicate 是幂等重入）。
	if _, _, err := s.queue.Enqueue(dispatch.sessionID, dispatch.submissionID, dispatch.sentText); err != nil {
		s.failDispatch(state, runID, taskID, fmt.Sprintf("任务 %s 入队失败: %v", taskID, err))
		return
	}
	// (5) 取消窗口二：入队期间任务被取消/run 被删除 → 立即撤条目。Cancel 的副作用
	// 即使带了本条目也撤不到（条目在 Enqueue 返回前不在账本里），孤儿条目
	// 若不在此收尾会被既有 3s worker 派发给 Agent。Run 被删除（含随 Team
	// 级联）同样必须撤：没有人再替这条已消失的 Run 收尾账本。
	fresh := s.store.Run(runID)
	current := (*Task)(nil)
	if fresh != nil {
		current = findTaskIn(fresh, taskID)
	}
	if fresh == nil || current == nil || terminalTask(current.Status) {
		s.cancelSideEffects(cancelTarget{
			sessionID:    dispatch.sessionID,
			submissionID: dispatch.submissionID,
			sentText:     dispatch.sentText,
		})
		return
	}
	// (6) Enqueue 成功且未终态 → 启动 watcher（短暂持锁登记）+ AIMD 在途
	//     记账（watcher 退出时对称 OnSettle；未生成 watcher 时在途不记，
	//     收养路径补 watcher 后由其退出记账——见 spawnSkipped）。OnDispatch
	//     把步骤②的准入预留转记为 watcher 在途（aimdReserved=false，杜绝
	//     defer 里重复归还）。
	state.mu.Lock()
	if s.spawnWatcherLocked(state, dispatch, policy, time.Now()) {
		s.AIMD.OnDispatch(dispatch.bucketKey)
		aimdReserved = false
	} else {
		spawnSkipped = true
	}
	state.mu.Unlock()
}

// failDispatch 把派发期确定性失败落任务 failed（短暂持锁；分支级联终止
// 由下一轮 cascade 步骤处理）。
func (s *Service) failDispatch(state *runState, runID, taskID, errorText string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	s.applyTaskTerminalLocked(state, runID, taskID, TaskFailed, "", errorText, "", nil, EventTaskFailed, "runner")
}

// applyCollectLocked 应用收集策略（§6.5 first_success）：PlanCollect 纯决策
// → commitRun 记账先行（first-wins 幂等）→ 返回取消副作用目标（调用方在
// 锁外执行；复用 Cancel 的 targets 机制）。重启恢复后每轮循环重算补齐。
func (s *Service) applyCollectLocked(state *runState, run *Run, team *Team, policy TeamPolicy) []cancelTarget {
	transitions := PlanCollect(run)
	if len(transitions) == 0 {
		return nil
	}
	var targets []cancelTarget
	if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		events, _ := applyTransitions(fresh, transitions, "runner")
		for _, transition := range transitions {
			task := findTaskIn(fresh, transition.TaskID)
			if task == nil || task.SubmissionID == "" {
				continue
			}
			member := memberOf(team, task.MemberID)
			if member == nil {
				continue
			}
			targets = append(targets, cancelTarget{
				sessionID:    member.SessionID,
				submissionID: task.SubmissionID,
				sentText:     s.sentTextFor(fresh, task, policy),
			})
		}
		return events, len(events) > 0
	}); err != nil {
		log.Printf("team run %s 收集策略记账失败，下轮重试: %v", run.ID, err)
		return nil
	}
	return targets
}

// adoptWatchersLocked 为「快照 dispatching/running 且有 submissionId 但没有
// 活 watcher」的任务重建 watcher（重启恢复、resume 后仍在飞的收尾）。
// sentText 从不可变 prompt + 依赖结果确定性重算（§6.2 幂等键派生的同款
// 稳定性），任务超时从收养时刻起算。同时在途 exclusive 任务在此重建
// cwd 写锁登记（§6.4 规则 2）：LockRegistry 纯内存、进程重启即空，不重取
// 锁的话同一 cwd 的其他 exclusive 任务会因 Holder 为空被立即派发，与被
// 收养任务并发写同一工作区。
//
// isolated 任务的收养不走这里直接 spawn：工作区对账（注册表 + rev-parse
// 是锁外 I/O）由 adoptWorkspaceWatchers 完成，通过者补 worktree 锁登记并
// 启动 watcher，失效者停靠（workspace_lost）。本函数把它们收集为候选
// 返回（含写锁登记也一并推迟——锁键是 worktree 路径，需先 Resolve）。
//
// 正在派发池内执行的任务（pendingDispatch）绝不收养：派发主体此刻可能
// 阻塞在入队途中（账本条目尚未落），早收养的 watcher 会按「账本条目丢失」
// 误落 outcome_unknown；这类任务的 watcher 由 executeDispatch 在 Enqueue
// 成功后自行启动。崩溃窗口（记账已落、入队未达）由重启恢复的 ReconcileRun
// 对账兜底（账本无条目→回 ready），与收养路径无关。
func (s *Service) adoptWatchersLocked(state *runState, team *Team, run *Run, policy TeamPolicy) []isolatedAdoption {
	var adoptions []isolatedAdoption
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status != TaskDispatching && task.Status != TaskRunning {
			continue
		}
		if task.SubmissionID == "" {
			continue
		}
		if state.pendingDispatch[task.ID] {
			continue // 派发池在途：watcher 由 executeDispatch 于入队成功后启动。
		}
		member := memberOf(team, task.MemberID)
		if member == nil {
			continue
		}
		if task.WorkspaceMode == WorkspaceIsolatedWorktree {
			if _, watched := state.watchers[task.ID]; watched {
				continue
			}
			adoptions = append(adoptions, isolatedAdoption{
				task:      *task.Clone(),
				sessionID: member.SessionID,
			})
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
		// sentText 用派发时落盘的存量文本（遗留④）：收养不得按当前策略重算
		// ——matchTurn 按全文匹配 turn，policy 改动会让重启收养错认 turn 或
		// 误落 outcome_unknown。空串（旧账本迁移窗口）才确定性重算。
		s.spawnWatcherLocked(state, &taskDispatch{
			taskID:       task.ID,
			sessionID:    member.SessionID,
			submissionID: task.SubmissionID,
			sentText:     s.sentTextFor(run, task, policy),
		}, policy, time.Now())
	}
	return adoptions
}

// isolatedAdoption 是一个待工作区对账的 isolated 收养候选（任务快照的深
// 拷贝 + 成员会话；对账在锁外进行，快照可能与 store 演进脱节，落账前一律
// 重读）。
type isolatedAdoption struct {
	task      Task
	sessionID string
}

// workspace_lost 的 reason 取值（§6.4：注册表键指向的路径丢失/不可用，或
// HEAD 不再等于记录的基线——工作区脏不算变更，脏区正是任务的正常产出）。
const (
	workspaceReasonPathMissing = "path_missing"
	workspaceReasonBaseChanged = "base_changed"
)

// adoptWorkspaceWatchers 对收养候选中的 isolated 任务做锁外工作区对账
// （§6.4 重启恢复）：Resolve 失败或 HeadRevision != BaseRevision → 任务
// 停靠（stopTaskWorkspaceLost：stopped + workspace_lost + run
// waiting_input，绝不自动创建新 worktree）；校验通过者补 worktree 写锁
// 登记并 spawn watcher（sentText 从不可变 prompt 确定性重算，与共享任务
// 收养同款）。每轮循环对无 watcher 的在途 isolated 任务重跑（watcher 存在
// 即不进候选），对账频率与收养事件一致，不构成在途任务的持续轮询。
func (s *Service) adoptWorkspaceWatchers(state *runState, candidates []isolatedAdoption) {
	for _, candidate := range candidates {
		path := ""
		reason := ""
		switch {
		case s.Workspaces == nil:
			reason = workspaceReasonPathMissing
		default:
			resolved, err := s.Workspaces.Resolve(candidate.task.WorkspaceID)
			switch {
			case err != nil:
				reason = workspaceReasonPathMissing
			default:
				head, headErr := s.Workspaces.HeadRevision(resolved)
				switch {
				case headErr != nil:
					reason = workspaceReasonPathMissing
				case head != candidate.task.BaseRevision:
					reason = workspaceReasonBaseChanged
				default:
					path = resolved
				}
			}
		}
		if reason != "" {
			s.stopTaskWorkspaceLost(state, state.runID, candidate.task.ID,
				candidate.task.WorkspaceID, reason)
			continue
		}
		// 校验通过：补 worktree 写锁登记（键=worktree 路径，与派发路径同
		// 一注册表）并启动 watcher（短暂持锁；任务可能已被并发落终态，
		// watcher 对终态任务 no-op 退出）。
		state.mu.Lock()
		if candidate.task.WriteLock != TaskWriteLockNone {
			if _, held := state.taskCwds[candidate.task.ID]; !held {
				if s.locks.Acquire(path, candidate.task.ID) {
					state.taskCwds[candidate.task.ID] = path
				} else {
					log.Printf("team run %s 收养 isolated 任务 %s 时 %s 的写锁已被任务 %s 持有，跳过登记（重启前并发的既成事实）",
						state.runID, candidate.task.ID, path, s.locks.Holder(path))
				}
			}
		}
		run := s.store.Run(state.runID)
		if run != nil {
			if task := findTaskIn(run, candidate.task.ID); task != nil && !terminalTask(task.Status) {
				policy := policyOf(s.store.Team(run.TeamID))
				s.spawnWatcherLocked(state, &taskDispatch{
					taskID:       task.ID,
					sessionID:    candidate.sessionID,
					submissionID: task.SubmissionID,
					sentText:     s.sentTextFor(run, task, policy),
				}, policy, time.Now())
			}
		}
		state.mu.Unlock()
	}
}

// stopTaskWorkspaceLost 落「工作区丢失或基线变更」停靠（§6.4 重启恢复）：
// workspace_lost 事件与任务 stopped 同一 commitRun 落账（journal 先行），
// 随后 run 停 waiting_input（两段式，settleUnknown 同款机制但落 stopped——
// 停靠任务在用户恢复 worktree/基线后可经 resume 继续，绝不自动创建新
// worktree、不自动重派）。
func (s *Service) stopTaskWorkspaceLost(state *runState, runID, taskID, workspaceID, reason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	const stopReason = "工作区丢失或基线变更，任务已停靠"
	applied := false
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		task := findTaskIn(fresh, taskID)
		if task == nil || terminalTask(task.Status) {
			return nil, false
		}
		task.Status = TaskStopped
		task.StopReason = stopReason
		lostPayload := map[string]any{
			"taskId": taskID,
			"reason": reason,
		}
		if workspaceID != "" {
			lostPayload["workspaceId"] = workspaceID
		}
		applied = true
		return []*Event{
			newEvent(runID, EventWorkspaceLost, "runner", lostPayload),
			newEvent(runID, EventTaskStopped, "runner", map[string]any{
				"taskId":     taskID,
				"stopReason": stopReason,
			}),
		}, true
	}); err != nil {
		log.Printf("team run %s 任务 %s 工作区停靠记账失败（下轮收养重试）: %v", runID, taskID, err)
		return false
	}
	if !applied {
		return false
	}
	s.releaseTaskLockLocked(state, taskID)
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if fresh.Status == RunWaitingInput || terminalRun(fresh.Status) {
			return nil, false
		}
		fresh.Status = RunWaitingInput
		fresh.StopReason = fmt.Sprintf("任务 %s 工作区丢失或基线变更，等待用户处理", taskID)
		return []*Event{newEvent(runID, EventRunWaitingInput, "runner", map[string]any{
			"reason": fresh.StopReason,
		})}, true
	}); err != nil {
		log.Printf("team run %s waiting_input 记账失败: %v", runID, err)
	}
	s.wakeLocked(state)
	return true
}

// cascadeBlockedLocked 把「依赖被永久阻塞」的 planned/ready 任务级联终止
// （分支已死，run 永远等不到它们完成）。依赖判定走 depGroupVerdict——与
// ReadyTasks/PlanResume 四处同规则（含 §6.5 组语义：first_success 下被
// superseded 的落败兄弟不得误杀下游合并任务）。按拓扑序单遍收敛
// （a stopped 会继续传导给依赖 a 的 b），与 PlanResume 的恢复级联同语义。
func (s *Service) cascadeBlockedLocked(state *runState, runID string) {
	s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		statuses := map[string]string{}
		for i := range fresh.Tasks {
			statuses[fresh.Tasks[i].ID] = fresh.Tasks[i].Status
		}
		groups := broadcastGroupsOf(fresh.Tasks)
		var events []*Event
		for _, ordered := range sortTasksByOrdinal(fresh.Tasks) {
			switch statuses[ordered.ID] {
			case TaskPlanned, TaskReady:
			default:
				continue
			}
			_, blocked := depGroupVerdict(statuses, groups, ordered.DependsOn, fresh.CollectPolicy)
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
// watcher 时不重复启动，返回 false）。exit 通道在 watcher 退出时关闭，供
// 测试与收养判断存活。
func (s *Service) spawnWatcherLocked(state *runState, dispatch *taskDispatch, policy TeamPolicy, dispatchedAt time.Time) bool {
	if _, watched := state.watchers[dispatch.taskID]; watched {
		return false
	}
	exit := make(chan struct{})
	state.watchers[dispatch.taskID] = exit
	state.dispatchedAt[dispatch.taskID] = dispatchedAt
	go func() {
		// LIFO：OnSettle（AIMD 在途收尾，先于唤醒让 Admit 看到回落后的
		// 计数）→ watchExited（清理登记+唤醒循环）→ close(exit)。
		defer close(exit)
		defer s.watchExited(state, dispatch.taskID)
		defer s.AIMD.OnSettle(dispatch.bucketKey)
		s.watchTask(state, dispatch, policy)
	}()
	return true
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
		s.settleTaskFailure(state, dispatch, policy, firstNonEmpty(sub.Error, "队列派发失败"))
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
				// 成功 settle → 桶记账（§6.3 规则 b：连续 4 次成功增长窗口）。
				s.AIMD.OnSuccess(dispatch.bucketKey)
				s.completeTask(state, state.runID, dispatch.taskID, &result)
				return true, session
			case "failed":
				s.settleTaskFailure(state, dispatch, policy, turnErrorText(turn))
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
		// 账本的 unknown 同时是派发的过渡态（dispatchQueuedThread 在
		// dispatchSubmission 前先落 unknown 防崩溃重发，queue.go）：观察点可能
		// 落在这个窗口里。先经 confirmUnknownIdle 短去抖复核，仍满足才落终态
		//——真实 unknown 恒满足；活会话的派发过渡窗远短于去抖间隔（阶段二
		// 集成测试 -race 实测曾把并行派发误判成 outcome_unknown 停靠）。
		if status, _ := snapshot.MustGet("status").(string); status != "active" &&
			sub.Status == SubStatusUnknown && s.confirmUnknownIdle(state, dispatch) {
			s.settleUnknown(state, state.runID, dispatch.taskID, "发送结果未知，需人工对账")
			return true, session
		}
		// 会话忙于其他 turn（用户手动发起）：继续等，超时兜底。
		return false, session
	}
	// 会话暂不可达（进程打不开等）：继续等，任务超时兜底。
	return false, session
}

// outcomeDebounce 是「结果未知」判定的去抖间隔：账本的 unknown 在派发期间
// 是过渡态（dispatchQueuedThread 派发前置 unknown，queue.go），活会话的
// 派发路径（prompt→BeginTurn→Submit）远短于该间隔；真实 unknown（崩溃/
// 派发结果不确认）恒满足复核。取值只影响误判防护强度与真实 unknown 的
// 落账延迟，不影响任何正确性判定（复核以实测状态为准）。
const outcomeDebounce = 250 * time.Millisecond

// confirmUnknownIdle 是结果未知判定的去抖复核：隔 outcomeDebounce 重读账本
// 与会话，「unknown + 会话空闲 + 无本任务 turn」仍成立才确证。调用方
// （pollTask）已观察到一次满足；run 终态（done 关闭）时放弃确证。
func (s *Service) confirmUnknownIdle(state *runState, dispatch *taskDispatch) bool {
	timer := time.NewTimer(outcomeDebounce)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-state.done:
		return false
	}
	sub, ok := s.queue.Submission(dispatch.sessionID, dispatch.submissionID)
	if !ok || sub.Status != SubStatusUnknown {
		return false // 派发已落定（accepted/failed/…）：非最终未知。
	}
	_, session, _ := s.resolver.Resolve(dispatch.sessionID)
	if session == nil {
		return false // 会话不可达：交回主循环按「暂不可达」继续等。
	}
	snapshot := session.Snapshot()
	if matchTurn(snapshot, dispatch.sentText) != nil {
		return false // turn 已出现：正常观察。
	}
	status, _ := snapshot.MustGet("status").(string)
	return status != "active"
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
				targets = append(targets, cancelTarget{
					sessionID:    member.SessionID,
					submissionID: task.SubmissionID,
					sentText:     s.sentTextFor(fresh, task, policy),
				})
			}
			return events, len(events) > 0
		}); err != nil {
			log.Printf("team run %s 超时记账失败，稍后重试: %v", run.ID, err)
			return
		}
		if fresh := s.store.Run(state.runID); fresh != nil {
			if status, stopReason := FoldRunStatus(fresh.Tasks, fresh.Mode, fresh.CollectPolicy); status != "" {
				switch {
				case status == RunCompleted && verifyPending(fresh):
					// 闸门未决：run 超时不得绕过闸门 fold（否则 Agent 的口头
					// 成功声明在超时路径上免检，§6.4.1）。推进/确保闸门并等
					// 闸门自行收尾（非零 → waiting_input；零 → 正常 fold）。
					s.ensureVerifyGateLocked(state, fresh)
				case fresh.Status == RunWaitingInput && strings.HasPrefix(fresh.StopReason, StopReasonVerifyFailed):
					// 闸门停靠的 run 过超时线也不 fold：verify_failed 的语义
					// 是「可修可重跑」，终局化会拒绝 Resume、堵死复验
					// （§13.3.7）。循环在超时分支等片驻留，resume 重臂期限。
				default:
					// finishRunLocked 返回 false 表示已落终局。
					finished = !s.finishRunLocked(state, fresh, status, stopReason)
				}
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
// 快照随后，最后释放写锁并唤醒循环。outcome 是 §6.3 错误策略表的分类值
// （非空时写 Task.Outcome 与事件 payload，recovery 重放据此推导重派语义）。
// 返回是否真的落账。
func (s *Service) applyTaskTerminalLocked(state *runState, runID, taskID, status, stopReason, errorText, outcome string, result *TaskResult, eventType, source string) bool {
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
		if outcome != "" {
			task.Outcome = outcome
		}
		if result != nil {
			task.Result = result.Result
			task.ResultDetail = result
		}
		payload := map[string]any{"taskId": taskID}
		if outcome != "" {
			payload["outcome"] = outcome
		}
		if task.Attempt > 0 && (eventType == EventTaskFailed || eventType == EventTaskStopped) {
			// 失败/停靠事实携带 attempt（recovery 判「更高 attempt 的派发
			// 取代既有终态」的依据，§6.3 重试与停靠重飞）。
			payload["attempt"] = jsoncompat.Num(task.Attempt)
		}
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
				if result.FileSummary != "" {
					payload["fileSummary"] = result.FileSummary
				}
				if result.CommandSummary != "" {
					payload["commandSummary"] = result.CommandSummary
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
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskCompleted, stopReason, "", "", result, EventTaskCompleted, "watcher")
}

// failTask 落任务 failed（确定性错误，重放仍会失败，§4.3）。outcome 非空
// 时为 §6.3 分类值（context_overflow 等）。
func (s *Service) failTask(state *runState, runID, taskID, errorText, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskFailed, stopReason, errorText, "", nil, EventTaskFailed, "watcher")
}

// stopTask 落任务 stopped（用户/中断类外因，可对账继续）。
func (s *Service) stopTask(state *runState, runID, taskID, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return s.applyTaskTerminalLocked(state, runID, taskID, TaskStopped, stopReason, "", "", nil, EventTaskStopped, "watcher")
}

// ---- §6.3 错误策略表：watcher 失败信号的分类结算 ----

// settleTaskFailure 是 watcher 失败信号（turn 错误文本 / 账本 LastQueueError）
// 的策略入口：ClassifyTaskError 分类后按 §6.3 分派——rate_limited/transient
// 受限重试（前者同时压 AIMD 桶并按 Retry-After 冷却）；quota/auth/
// not_configured 任务停靠 + run 暂停（不压共享桶）；context_overflow 单任务
// 确定性失败；unknown 维持阶段一的确定性 failed 语义。调用上下文为
// watcher（不持 state.mu；各分支内部短暂持锁）。
func (s *Service) settleTaskFailure(state *runState, dispatch *taskDispatch, policy TeamPolicy, errorText string) {
	switch outcome := ClassifyTaskError(errorText); outcome {
	case OutcomeRateLimited:
		// 限流：先压桶（每次限流都冷却顺延，epoch 内只减一次窗口），窗口
		// 实际减小时记 provider_throttled 事件（每 epoch 一次，§10.3）；
		// 再走受限重试——重试预算耗尽时也压桶（provider 的限流压力对同桶
		// 的其他任务/Team 同样成立）。
		retryAfter := ParseRetryAfter(errorText)
		window, decreased := s.AIMD.OnFailure(dispatch.bucketKey, retryAfter)
		if decreased {
			s.recordThrottled(state.runID, dispatch.bucketKey, window, retryAfter)
		}
		s.retryOrFailTask(state, dispatch, policy, outcome, errorText)
	case OutcomeTransient:
		// 网络瞬断/暂态：受限重试，不动 AIMD 桶（非限流信号）。
		s.retryOrFailTask(state, dispatch, policy, outcome, errorText)
	case OutcomeQuota, OutcomeAuth, OutcomeNotConfigured:
		// 计费/认证/未配置：任务停靠 + run 暂停等待用户（§6.3「归为
		// stop，只停止受影响 Run，不冻结共享桶」——也规避与 accpool
		// AutoSwitcher 的桶级双重反应，poolhooks 已对 quota 切账号）。
		s.stopTaskBlocked(state, state.runID, dispatch.taskID, outcome, errorText)
	case OutcomeContextOverflow:
		// 上下文超限：单任务确定性失败（重放同文本仍会超限），Outcome
		// 写分类值（model.go 已知字表现启用）。
		state.mu.Lock()
		s.applyTaskTerminalLocked(state, state.runID, dispatch.taskID, TaskFailed, "", errorText, outcome, nil, EventTaskFailed, "watcher")
		state.mu.Unlock()
	default:
		// unknown：确定性 failed（阶段一语义原样保留）。
		s.failTask(state, state.runID, dispatch.taskID, errorText, "")
	}
}

// retryOrFailTask 处理可重试失败（rate_limited/transient，§6.3 受限重试）：
// 预算内（task.Attempt < firstAttempt+MaxTaskRetries）单次 commitRun 落
// task_failed 事实（payload 携带 outcome 与失败 attempt）+ task_retry_scheduled
// 重臂事件 + attempt+1 回 ready（清 submissionId/inputHash/error——重派按新
// attempt 派生全新 submissionID，幂等键 attempt 递增）；预算耗尽按确定性
// failed 收场（Outcome 写分类值，不再回 ready，不落重臂事件）。重试失败的
// 事实已在 journal，不另落终态镜像；重臂事件是快照迁移（ready）的唯一
// journal 佐证，recovery 重放据此保住重试预算。
func (s *Service) retryOrFailTask(state *runState, dispatch *taskDispatch, policy TeamPolicy, outcome, errorText string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	applied := ""
	err := s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		task := findTaskIn(fresh, dispatch.taskID)
		if task == nil || terminalTask(task.Status) {
			return nil, false
		}
		failedAttempt := task.Attempt
		if failedAttempt < firstAttempt-1 {
			failedAttempt = firstAttempt - 1 // attempt 0（防御）：记为 0
		}
		budget := policy.MaxTaskRetries
		if budget < 0 {
			budget = 0
		}
		payload := map[string]any{
			"taskId":  dispatch.taskID,
			"error":   errorText,
			"outcome": outcome,
			"attempt": jsoncompat.Num(failedAttempt),
		}
		if task.Attempt >= firstAttempt+budget {
			// 预算耗尽：确定性失败收场（Outcome 保留分类值供审计）。
			task.Status = TaskFailed
			task.StopReason = ""
			task.Error = errorText
			task.Outcome = outcome
			applied = TaskFailed
			return []*Event{newEvent(fresh.ID, EventTaskFailed, "watcher", payload)}, true
		}
		// 预算内：failed 事实 + 重臂迁移（attempt+1 回 ready，清引用，重派派
		// 新键）。「回 ready」的快照迁移与失败事实同批落 task_retry_scheduled
		// 事件——没有它，重试冷却（AIMD 冷却上限 ~300s）或等待片内重启时
		// ReconcileRun 只能按 task_failed 终态重放，任务被折成确定性 failed、
		// 重试预算静默丢弃（§6.3 受限重试语义破坏且不可 resume）。
		next := task.Attempt + 1
		if next < firstAttempt {
			next = firstAttempt
		}
		task.Status = TaskReady
		task.Attempt = next
		task.SubmissionID = ""
		task.InputHash = ""
		task.Error = ""
		task.Outcome = ""
		task.StopReason = ""
		applied = TaskReady
		return []*Event{
			newEvent(fresh.ID, EventTaskFailed, "watcher", payload),
			newEvent(fresh.ID, EventTaskRetryScheduled, "watcher", map[string]any{
				"taskId":  dispatch.taskID,
				"attempt": jsoncompat.Num(next),
				"outcome": outcome,
			}),
		}, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 重试记账失败（watcher 继续观察重试）: %v", state.runID, dispatch.taskID, err)
		return
	}
	if applied != "" {
		s.releaseTaskLockLocked(state, dispatch.taskID)
		s.wakeLocked(state)
	}
}

// blockedStopReason 返回停靠类错误的中文原因（§6.3：任务停靠，用户处理
// 账号/配置后 resume 重飞）。
func blockedStopReason(outcome string) string {
	switch outcome {
	case OutcomeQuota:
		return "账号额度或余额不足，任务已停靠"
	case OutcomeAuth:
		return "账号认证失败或登录过期，任务已停靠"
	case OutcomeNotConfigured:
		return "Agent 或账号未配置，任务已停靠"
	}
	return "账号或配置问题，任务已停靠"
}

// stopTaskBlocked 把 quota/auth/not_configured 失败落任务停靠 + run 暂停
// （§6.3）：任务 stopped（非 failed——停靠可经 resume 重飞）+ Outcome 分类
// + 中文停靠原因；run 停 waiting_input（两段式，复用 settleUnknown 的
// 「任务终态 + run waiting_input」机制，但落 stopped 非 unknown）。
//
// 与 accpool AutoSwitcher 的任务级交互（显式定式）：poolhooks 在 quota 时
// 切账号并按 ResumeAfterSwitchPrompt 续跑成员会话——任务停靠后 watcher
// no-op，续跑产出保留在原会话 timeline；阶段二不自动认领续跑 turn（显式
// 不做：续跑文本不含本任务的幂等键，认领需引入新的对账口径）；resume 时
// 用户重派 attempt+1 带全新 submissionID，与续跑产出各自独立可审计。
func (s *Service) stopTaskBlocked(state *runState, runID, taskID, outcome, errorText string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	stopReason := blockedStopReason(outcome)
	if !s.applyTaskTerminalLocked(state, runID, taskID, TaskStopped, stopReason, errorText, outcome, nil, EventTaskStopped, "watcher") {
		return false
	}
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if fresh.Status == RunWaitingInput || terminalRun(fresh.Status) {
			return nil, false
		}
		fresh.Status = RunWaitingInput
		fresh.StopReason = fmt.Sprintf("任务 %s 停靠：%s（处理账号或配置后可继续）", taskID, stopReason)
		return []*Event{newEvent(runID, EventRunWaitingInput, "watcher", map[string]any{
			"reason":  fresh.StopReason,
			"outcome": outcome,
		})}, true
	})
	if err != nil {
		log.Printf("team run %s waiting_input 记账失败: %v", runID, err)
	}
	return true
}

// recordThrottled 在 AIMD 窗口实际减小时落 provider_throttled 事件
// （§6.3，每 epoch 一次；payload {bucket,window,retryAfterSecs}）。事实级
// 事件不改快照；HTTP/SSE 路由归 httpd-api-2。
func (s *Service) recordThrottled(runID, bucket string, window int, retryAfter time.Duration) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	event := newEvent(runID, EventProviderThrottled, "watcher", map[string]any{
		"bucket":         bucket,
		"window":         jsoncompat.Num(window),
		"retryAfterSecs": jsoncompat.Num(int(retryAfter / time.Second)),
	})
	if err := s.journal.Append(event); err != nil {
		log.Printf("team run %s provider_throttled 记账失败: %v", runID, err)
		return
	}
	s.emit(event)
}

// settleUnknown 落任务 outcome_unknown 并把 run 停进 waiting_input（§9 唯一
// 规则：未决 unknown 暂停新派发、在途 turn 收尾入账、驻留等待 resume；不
// fold、不重发）。run 已终态或已 waiting_input 时后者幂等跳过。
func (s *Service) settleUnknown(state *runState, runID, taskID, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	if !s.applyTaskTerminalLocked(state, runID, taskID, TaskOutcomeUnknown, stopReason, "", "", nil, EventTaskOutcomeUnknown, "watcher") {
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
				targets = append(targets, cancelTarget{
					sessionID:    member.SessionID,
					submissionID: task.SubmissionID,
					sentText:     s.sentTextFor(fresh, task, policy),
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
	// 闸门复验（§13.3.7：修复后 continue 即复验）：verify_failed 停靠不走
	// PlanResume（此时任务表全 completed，无可恢复分支），清空 Verify、回
	// RunDispatching 并重启循环——loopPass ④ 的 fold 拦截会重新启动闸门。
	if strings.HasPrefix(run.StopReason, StopReasonVerifyFailed) {
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunWaitingInput {
				return nil, false
			}
			fresh.Verify = nil
			fresh.Status = RunDispatching
			fresh.StopReason = ""
			return nil, true // 纯状态推进，复验事实由 run_verify 事件记账。
		}); err != nil {
			return nil, err
		}
		s.startLoopLocked(state)
		return s.store.Run(runID), nil
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
				if transition.Outcome != "" {
					// 非空才覆写：PlanResume 的停靠重臂（stopped→ready）不
					// 携带 Outcome，须保留原值供派发侧按 attempt+1 派新键。
					task.Outcome = transition.Outcome
				}
				if transition.Result != nil {
					task.Result = transition.Result.Result
					task.ResultDetail = transition.Result
				}
				if transition.EventType != "" {
					payload := map[string]any{"taskId": task.ID}
					if transition.Outcome != "" {
						payload["outcome"] = transition.Outcome
					}
					if transition.Result != nil && transition.EventType == EventTaskCompleted {
						payload["sessionId"] = transition.Result.SessionID
						payload["turnId"] = transition.Result.TurnID
						payload["result"] = transition.Result.Result
						payload["resultChars"] = jsoncompat.Num(transition.Result.ResultChars)
						payload["truncated"] = transition.Result.Truncated
						if transition.Result.FileSummary != "" {
							payload["fileSummary"] = transition.Result.FileSummary
						}
						if transition.Result.CommandSummary != "" {
							payload["commandSummary"] = transition.Result.CommandSummary
						}
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
