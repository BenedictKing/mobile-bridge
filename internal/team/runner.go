package team

// runner.go 是编排执行主体（§6.1 调度循环、§6.3 团队级派发池、§6.5 结果
// 即时入账与收集策略、§9 暂停边界、§10.2 cancel/resume、§8.3 重启恢复的
// I/O 侧）：Service 持有全部端口，每 Run 一个调度循环 goroutine + 团队级
// 派发池（信号量限定同时在执行的派发 I/O 数）+ 每在途任务一个 watcher
// goroutine。并发护栏是选择期记账：调度决策把「快照在途 + 已投递派发池」
// 一并计入 inFlight，任务从投递池到落 dispatching 始终占并发额度——任何
// 时刻同 Team 各 Run 在途任务合计（含 broadcast 展开）不超过
// policy.MaxParallelTasks（§6.3 层 1 的 Team 口径，评审遗留②）。
// 终局分类唯一权威是 reducer.FoldRunStatus，runner 不发明终态映射；任何
// 未决 outcome_unknown 都把 run 停进 waiting_input（§9），resume 只做
// 「确认不再重发」，绝不重建 submission。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
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

	// OnRunStatus 在 run 状态事件（run_completed/run_failed/run_cancelled/
	// run_stopped/run_waiting_input，§12 阶段四工作项 3）与 stall 事实
	// （run_stalled，§6.3）成功落账后回调（挂点收口见 notify.go 的
	// notifyRunStatusEvents）。回调在该 Run 的 state.mu 下同步执行：装配方
	// 只做「读 store + 写通知账本」，绝不能再入 Service 的运行方法（死锁）。
	// nil 表示无人订阅。
	OnRunStatus func(runID, kind string)

	// stallAfter 是 §6.3 的 stall 判定线（默认 20 分钟，notify.go）。≤0
	// 关闭 stall 检查；测试经此注入缝调小。
	stallAfter time.Duration

	// nowFn 是可控 clock 注入缝（stall 计时与 lastSuccessAt 锚点取时用；
	// 测试注入假时钟推进 20 分钟而不真睡，§13.1）。NewService 填 time.Now。
	nowFn func() time.Time

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

	// respondAgent 把问题答案投递到成员会话（§5.4 decision 的 Respond 步）。
	// 独立成字段是测试注入点，与 cancelAgentTurn 同因：手工构造的 Session
	// 成功回应会走 signalLocked 关闭 nil changed 通道而 panic；生产实现恒为
	// session.Respond。
	respondAgent func(session *acpruntime.Session, requestID string, response *jsoncompat.Object) error

	// verifyTimeout 是验证闸门命令的执行超时（§6.4.1，默认 600s；测试直接
	// 调小，避免用真实十分钟等超时路径）。
	verifyTimeout time.Duration

	// waitSlice 是 watcher/循环的等待片长（默认 25s；测试在启动任何 Run
	// 前调小，避免固定 sleep 判正确性，§13.1）。
	waitSlice time.Duration

	mu   sync.Mutex
	runs map[string]*runState

	// teamPending 是 Team 级派发窗口在途计数（§6.3 层 1 的 Team 口径，评审
	// 遗留②）：teamID → 已投递派发池、快照尚未落 dispatching/终态的任务数
	// （各 Run 的 pendingDispatch 汇总）。与 state.pendingDispatch 在同两点
	// 维护（passDispatch 登记、executeDispatch 收尾清除，1:1 对应），s.mu
	// 保护。passDispatch 据此 + 全 Team 各 Run 快照的 dispatching/running 合成
	// Team 级 inFlight——同 Team 多 Run 并发时在途合计不超过
	// policy.MaxParallelTasks。纯内存，重启即空（与 pendingDispatch 同存亡）。
	teamPending map[string]int

	// pools 是团队级派发池（teamID → 信号量池，§6.3 层 1）。容量取 Team
	// 策略的 MaxParallelTasks，策略变更时整体重建；在途派发持有旧池信号
	// 量直至完成，重建瞬间可能短暂超出新容量。
	poolsMu sync.Mutex
	pools   map[string]*teamDispatchPool

	// reconciler 是 waiting_input Run 的周期刷新循环（§12 阶段四工作项 2，
	// 仿 accpool Poller 的 ctx+done 形态）：每 tick 对全部 waiting_input Run
	// 依次 RefreshRun。Start/Stop 同步退出，字段由 reconcilerMu 保护。
	reconcilerMu     sync.Mutex
	reconcilerCancel context.CancelFunc
	reconcilerDone   chan struct{}
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
		stallAfter:    defaultStallAfter,
		nowFn:         time.Now,
		runs:          map[string]*runState{},
		teamPending:   map[string]int{},
		pools:         map[string]*teamDispatchPool{},
	}
	service.cancelAgentTurn = service.cancelTurnIfOurs
	service.respondAgent = func(session *acpruntime.Session, requestID string, response *jsoncompat.Object) error {
		return session.Respond(requestID, response)
	}
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

// SetStallAfter 调整 §6.3 的 stall 判定线（默认 defaultStallAfter=20 分钟）。
// 存在原因是让 internal/httpd 的集成测试把判定线缩到毫秒级（notify.go
// maybeStallLocked 的取时走真实时钟——推进由 deadline 轮询兜住，只有阈值
// 需要注入），notify_test.go 经包内 stallAfter 字段注入、本方法是它的导出
// 面版本——生产装配不要调用。照 SetWaitSlice 契约：仅在没有启动任何 Run
// 之前调用才生效；d≤0 关闭 stall 检查。
func (s *Service) SetStallAfter(after time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.runs) > 0 {
		log.Printf("team SetStallAfter 在 Run 已启动后被调用，已忽略: runs=%d", len(s.runs))
		return
	}
	s.stallAfter = after
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
	// deadline 是 run 超时截止；每次循环启动重臂。paused 驻留期间冻结
	//（循环已退出、无人检查，软停不烧时钟）；Resume 从 paused 经
	// startLoopLocked 重臂，从 resume 时刻重新起算。
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
	// lastSuccessAt 是最近一次任务 completed 的时刻（§6.3 stall 计时锚点，
	// mu 保护）：循环启动（create/resume/恢复）时初始化或重建（notify.go），
	// 任务 completed 落账时刷新（noteTaskSuccessLocked）。lastCompletedTaskID
	// 随锚点记录，供 run_stalled payload 追溯最近成功。
	lastSuccessAt       time.Time
	lastCompletedTaskID string
	// stalled 标记本段停滞已报过 stall（§6.3 只通知不停止：每段至多一条
	// run_stalled；任务 completed 后重置，新一段可再报）。
	stalled bool
	// dispatchCount 是本 Run 已落账的 task_dispatching 事实数（§9 预算
	// 计量，mu 保护）：创建起 0，executeDispatch 记账成功 +1，重启恢复按
	// journal 重放重建（countRunDispatches，与 Resume 重算同一函数）。
	// 预算闸门（passDispatch）与追加上限推导（Resume 的 budget 分支）都
	// 以它为「已派发数」，绝不另设口径。
	dispatchCount int
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
//
// autoPlan 声明「plan 为空、Lead 自动规划」（§5.1/§10.2）：只有 AutoPlan
// Run 的 Lead 任务回复才解析 team-actions（显式 Plan 优先级高于 Agent 自动
// 计划）——autoPlan 与显式计划互斥，同时携带直接拒绝。autoPlan 随
// run_created payload 落账供 journal 重建（旧账本缺字段按 false 保守解析）。
//
// scheduledFrom 是定时触发的可追溯标记（§12 阶段四工作项 1，schedule.go 的
// journal-first 触发协议传入）：非空时写入 Run.ScheduledFrom 与 run_created
// payload.scheduledBy——回扫对账只认 journal 里的该事件，绝不依赖 HTTP 层
// 的 creations 账本。空串=用户手工创建（HTTP 调用方恒传空串）。
func (s *Service) CreateRun(team *Team, runID, goal, mode, collect string, verifyCommand []string, raw []PlanTaskInput, autoPlan bool, scheduledFrom string) (*Run, error) {
	return s.createRun(team, runID, goal, mode, collect, verifyCommand, raw, autoPlan, scheduledFrom, nil)
}

// CreateConversationRun 把一条主控团队消息创建为 autoPlan Run；附件只随首个
// Lead 任务持久化和派发，后续 delegate 任务不重复携带。
func (s *Service) CreateConversationRun(team *Team, runID, goal string, attachments []string) (*Run, error) {
	return s.createRun(team, runID, goal, ModeSingle, "", nil, nil, true, "", attachments)
}

func (s *Service) createRun(team *Team, runID, goal, mode, collect string, verifyCommand []string, raw []PlanTaskInput, autoPlan bool, scheduledFrom string, attachments []string) (*Run, error) {
	if team == nil {
		return nil, errors.New("team 不能为 nil")
	}
	if autoPlan && len(raw) > 0 {
		return nil, errors.New("自动规划的 Run 不能携带显式计划（显式 Plan 优先，Lead 自动动作只在空计划时解析）")
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
	if len(attachments) > 0 {
		if !autoPlan || len(plan.Tasks) != 1 {
			return nil, errors.New("团队聊天附件只能随首个 Lead 任务创建")
		}
		plan.Tasks[0].Attachments = append([]string(nil), attachments...)
		plan.Tasks[0].InputHash = InputHashFor(&plan.Tasks[0], PolicyDigest(team.Policy), "")
		plan.PlanHash = PlanHash(plan.Mode, plan.CollectPolicy, plan.Goal, plan.Tasks)
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
		AutoPlan:      autoPlan,
		Status:        RunCreated,
		Round:         1,
		PlanHash:      plan.PlanHash,
		Tasks:         plan.Tasks,
		VerifyCommand: append([]string(nil), verifyCommand...),
		ScheduledFrom: scheduledFrom,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	// 先记账：payload 只含有界字段（instructionsHead 而非全量 prompt，
	// §10.3「事件流只推送有界 payload」）。teamId/goal 供 team-runs.json
	// 损坏后的 journal 重建（§8.1 快照可重建；goal 已过 10000 字校验）；
	// verifyCommand 已过白名单校验（≤8 段、单段 ≤512 字节），重建据此
	// 恢复闸门声明；autoPlan 供重建恢复解析闸门。
	createdPayload := map[string]any{
		"teamId":        team.ID,
		"goal":          plan.Goal,
		"mode":          plan.Mode,
		"collectPolicy": plan.CollectPolicy,
		"planHash":      plan.PlanHash,
		"taskCount":     jsoncompat.Num(len(plan.Tasks)),
	}
	if autoPlan {
		createdPayload["autoPlan"] = true
	}
	if len(verifyCommand) > 0 {
		createdPayload["verifyCommand"] = append([]string(nil), verifyCommand...)
	}
	// scheduledBy 是定时触发的事实凭据（§12 阶段四）：崩溃窗口的回扫对账
	// （schedule.go）按「journal 有无 scheduledBy==triggerID 的 run_created」
	// 裁决该触发是否已生效，键名与 Run.ScheduledFrom 字段名刻意区分——
	// payload 侧描述「由谁触发」，快照侧描述「来源于哪」。
	if scheduledFrom != "" {
		createdPayload["scheduledBy"] = scheduledFrom
	}
	created := newEvent(run.ID, EventRunCreated, "user", createdPayload)
	events := []*Event{created}
	for i := range plan.Tasks {
		task := &plan.Tasks[i]
		payload := map[string]any{
			"taskId":           task.ID,
			"memberId":         task.MemberID,
			"instructionsHead": task.InstructionsHead,
		}
		if len(task.Attachments) > 0 {
			payload["attachments"] = append([]string(nil), task.Attachments...)
		}
		events = append(events, newEvent(run.ID, EventTaskPlanned, "user", payload))
	}
	if err := s.accountCreatedRun(run, events); err != nil {
		return nil, err
	}
	return run, nil
}

// accountCreatedRun 落账一个 Run 的创建批次（CreateRun 与 CreateAmendRun
// 共用的尾部，§3.1 原则 4：journal 先行、快照随后）：逐条追加 events（首条
// 必须是 run_created，其后按任务序排列）→ 团队仍在的 TOCTOU 复查 → 一次
// SaveRun 落全量快照 → 启动调度循环。amend 的缓存命中任务由调用方在快照
// 构造期直接置 completed（创建批次内单次落账，绝不二次改写）。
func (s *Service) accountCreatedRun(run *Run, events []*Event) error {
	for _, event := range events {
		if err := s.journal.Append(event); err != nil {
			return fmt.Errorf("run 创建记账失败: %w", err)
		}
		s.emit(event)
	}
	// TOCTOU 补丁：创建窗口内团队可能已被并发删除（DeleteTeam 不触碰 run
	// 版本，SaveRun 无法靠版本冲突发现；账本层的 ErrActiveRuns 拦截只保护
	// 已落账的 Run）。落账前重验团队仍在，收窄「孤儿 Run」窗口。
	if s.store.Team(run.TeamID) == nil {
		return errors.New("团队已被删除，请刷新列表")
	}
	if err := s.store.SaveRun(run); err != nil {
		return err
	}
	s.startLoop(s.stateFor(run.ID))
	return nil
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
	// stall 计时锚点（§6.3）：create 首启从现在起算；resume 显式重置（驻留
	// 时长是用户治理时间，不计入「无成功」）、恢复路径按 journal 重建
	// （notify.go rebuildLastSuccessAt），这里的零值兜底只兜创建。
	if state.lastSuccessAt.IsZero() {
		state.lastSuccessAt = s.now()
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
			finished, dwellPass := s.expireRun(state)
			if finished {
				return
			}
			if !dwellPass {
				// 超时记账失败（磁盘故障等）：等一片重试，自愈或驻留至重启恢复。
				select {
				case <-state.done:
					return
				case <-time.After(s.slice()):
				}
				continue
			}
			// 超时豁免驻留（runnable proposed 动作，§5.2）：不原地等片重试——
			// 动作执行段（passActions）只在完整 pass 里运行，超时分支短路 pass
			// 会把待执行动作悬死。落回下方 pass（收养/动作/派发/fold 照常），
			// 动作事实执行/拒绝落定后，派生任务由下一轮超时路径按剩余事实收口。
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
	// ⓵ Lead 自动动作执行段（§5.2，阶段三）：proposed 动作的判定/执行/拒绝。
	// 位于派发段之前——同 pass 内派生任务即可参与派发（依赖释放/写锁/AIMD/
	// 幂等键/watcher 全套复用 executeDispatch，零新派发代码）。
	s.passActions(state)
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
		// 驻留等待 resume；在途 watcher 继续收尾入账。stall 不计时——
		// 停靠时刻已有独立通知，驻留时长是用户治理时间（盲区声明见
		// notify.go maybeStallLocked）。
		return false, false, nil
	}
	if run.Status == RunPaused {
		// 用户软停驻留（§12 阶段四工作项 4）：循环退出、不再推进/级联/派发；
		// 在途 watcher 继续收尾入账（停靠/结果未知会把 run 升级 waiting_input，
		// 语义严格强于 paused）。run 超时 deadline 与 stall 计时在此冻结——
		// 循环已无人检查两者，软停不该烧时钟（有意设计，照 verify_failed 停靠
		// 豁免 expireRun 的注释先例；Pause/Resume 的重臂语义与测试钉住）。
		return false, false, nil
	}
	// RunReducing：验证闸门态（§6.4.1，全任务终态后、终局前的闸门窗口）。
	// 短路 ①②③——不收养、不级联终止、不派发（闸门进行中的 run 不该再产生
	// 任何调度副作用）；闸门结果已落但判定未应用（记账失败的崩溃窗口）时
	// 就地补判，否则确保闸门已启动并在飞，等片/唤醒。闸门挂起也参与 stall
	// 计时（有活循环、无成功推进的形态之一）。
	if run.Status == RunReducing {
		s.maybeStallLocked(state, run)
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
	// stall 检查（§6.3「20 分钟无成功只通知不停止」）：能走到这里的是
	// 有活调度循环的 run（dispatching 等执行态）；waiting_input 已在上方
	// 早退、终态已收口——盲区声明见 notify.go maybeStallLocked。
	s.maybeStallLocked(state, run)
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

// ---- Lead 自动动作执行（§5.2，阶段三 actions-exec）----

// passActions 是循环的动作执行段（loopPass 在收养/级联之后、派发之前调用）：
// 扫描快照里 Status=proposed 的 PendingActions，逐个判定并执行/拒绝。loop
// 驱动而非 watcher 内执行：重启后快照存活、循环每轮重放补执行
// （PlanCollect 重入语义同款）；watcher 路径不持 state.mu，在那里执行会绕过
// run 终态复查（first-wins 无从谈起）。执行闸门（§5.1 规则 4）：delegate 与
// request_context 仅 policy.AutoDispatch=true 时执行，false 时停在 proposed
// （§11 动作预览的免执行形态）；summarize 恒可执行（§5.2）。解析门（autoPlan）
// 在 settleLeadTaskLocked——两道门分开（§10.2）。
//
// 分段拆锁（仿 resolveCandidateCwds）：resolver.Resolve（冷探测可达 15s）、
// 团队级在途统计与 journal Replay 都在锁外预取；判定与落账在 state.mu 内的
// 单批 commitRun（拒绝原因的唯一持久落点是快照 Action.Status/Reason）。
func (s *Service) passActions(state *runState) {
	run := s.store.Run(state.runID)
	if run == nil || terminalRun(run.Status) || run.Status != RunDispatching {
		return
	}
	team := s.store.Team(run.TeamID)
	if team == nil {
		return // 团队已删（级联删除窗口）：动作无从执行，留给终局路径收尾。
	}
	policy := team.Policy
	var proposed []Action
	for i := range run.PendingActions {
		if run.PendingActions[i].Status == ActionStatusProposed {
			proposed = append(proposed, run.PendingActions[i])
		}
	}
	if len(proposed) == 0 {
		return
	}
	runnable := make([]Action, 0, len(proposed))
	for i := range proposed {
		action := proposed[i]
		if (action.Type == ActionTypeDelegate || action.Type == ActionTypeRequestContext) && !policy.AutoDispatch {
			continue // 执行闸门关闭：停在 proposed（动作预览），不判定不落账。
		}
		runnable = append(runnable, action)
	}
	if len(runnable) == 0 {
		return
	}
	pre := &actionPrefetch{sessionLive: map[string]bool{}, teamInFlight: -1}
	pre.facts = s.actionFacts(state.runID)
	// ① 锁外预取（慢 I/O）：目标成员会话可达性（delegate/request_context 用
	// member，summarize 用 Lead）。journal 已有终局事实的（崩溃窗口）无需预取。
	for i := range runnable {
		action := &runnable[i]
		if pre.facts != nil {
			if _, settled := pre.facts.settled[action.ActionKey]; settled {
				continue
			}
		}
		memberID := action.Member
		if action.Type == ActionTypeSummarize {
			memberID = team.LeadID
		}
		if memberID == "" {
			continue
		}
		if _, resolved := pre.sessionLive[memberID]; resolved {
			continue
		}
		member := memberOf(team, memberID)
		if member == nil {
			continue // 不存在的成员由 ValidateExecutable 给出可解释拒绝。
		}
		manager, session, _ := s.resolver.Resolve(member.SessionID)
		switch {
		case session != nil:
			pre.sessionLive[memberID] = true
		case manager == nil:
			// 会话不属于任何 ACP Agent（已删除/从未接入）：绑定真失效，
			// 落 sessionLive=false（§5.2 拒绝项，一次性可解释拒绝）。
			pre.sessionLive[memberID] = false
		default:
			// 归属明确（manager.Knows 命中）但当前打不开——openACP 的瞬态
			// 或环境性失败（agent 冷启超时/被桌面端持有/CwdFor 暂空）：不算
			// 绑定失效，不落键（键缺省＝跳过该项判定），动作保持 proposed，
			// 由后续循环与派发/转发路径兜底重试——与 executeDispatch 对同类
			// Resolve 失败仅降级 AIMD 桶键、任务照常入队的语义对齐。
		}
	}
	// ② 团队级在途（锁外：本函数此刻不持有任何 state.mu；teamInFlight 会短暂
	// 持有其他 Run 的 state.mu，持锁调用会锁序倒置）。
	pre.teamInFlight = s.teamInFlight(team.ID)
	// ③ 逐动作执行/收敛/拒绝。
	for i := range runnable {
		s.executeAction(state, team, policy, &runnable[i], pre)
	}
}

// executeAction 执行单个 proposed 动作：journal 已有终局事实的先做崩溃窗口
// 收敛（只补快照、不重复记账）；delegate/summarize 走单批 commitRun；request_
// context 锁内判定、锁外转发。
func (s *Service) executeAction(state *runState, team *Team, policy TeamPolicy, action *Action, pre *actionPrefetch) {
	if pre != nil && pre.facts != nil {
		if verdict, settled := pre.facts.settled[action.ActionKey]; settled {
			s.convergeAction(state, team, action, verdict, pre.facts.reason[action.ActionKey])
			return
		}
	}
	switch action.Type {
	case ActionTypeDelegate, ActionTypeSummarize:
		s.executePlannedAction(state, team, policy, action, pre)
	case ActionTypeRequestContext:
		s.executeForwardAction(state, team, policy, action, pre)
	}
}

// executePlannedAction 落地 delegate/summarize（§5.2）：判定→拒绝或执行，
// 全部在单批 commitRun 内原子落账。普通 delegate 新建派生任务；分诊 delegate
// 通过 task_retry_scheduled 重臂原任务并恢复其级联停止的下游。两类结果都交
// 同 pass 的 ReadyTasks/executeDispatch 全套派出。
func (s *Service) executePlannedAction(state *runState, team *Team, policy TeamPolicy, action *Action, pre *actionPrefetch) {
	state.mu.Lock()
	defer state.mu.Unlock()
	err := s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		index := findProposedActionIndex(fresh, action.ActionKey)
		if index < 0 {
			return nil, false // 已被并发处理（first-wins）。
		}
		if ok, reason := ValidateExecutable(fresh, team, policy, action, pre, memberBusyOf(fresh, state.pendingDispatch)); !ok {
			fresh.PendingActions[index].Status = ActionStatusRejected
			fresh.PendingActions[index].Reason = reason
			event := newEvent(fresh.ID, EventActionRejected, "runner", actionRejectedPayload(action, reason))
			event.IdempotencyKey = action.ActionKey
			return []*Event{event}, true
		}
		var events []*Event
		if target, retriage := failureRetriageTarget(fresh, action); retriage {
			events = applyRetriage(fresh, target, action)
		} else {
			task := buildActionTask(fresh, team, action)
			fresh.Tasks = append(fresh.Tasks, task)
			event := newEvent(fresh.ID, EventTaskPlanned, "runner", map[string]any{
				"taskId":           task.ID,
				"memberId":         task.MemberID,
				"instructionsHead": task.InstructionsHead,
				"round":            jsoncompat.Num(action.Round),
				"actionKey":        action.ActionKey,
			})
			event.IdempotencyKey = actionExecutionKey(fresh.TeamID, fresh.ID, action.ActionKey)
			events = []*Event{event}
		}
		if action.Round > fresh.Round {
			fresh.Round = action.Round
		}
		fresh.PendingActions[index].Status = ActionStatusExecuted
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 动作 %s 执行记账失败（下轮重试）: %v", state.runID, action.ActionKey, err)
	}
}

// executeForwardAction 落地 request_context（§5.2「请求另一成员提供已有
// 结果」）：① 锁内判定（拒绝同批落 action_rejected + 快照；通过则无快照
// 变更）；② 锁外调 ForwardToMember——其内部自持 state.mu，RequestID 由
// ActionKey 确定性派生，幂等/记账/入队/信封整套复用，task_forwarded 事件
// payload 增载 actionKey（journal 重建据此置 executed）；③ 成功后快照置
// executed（纯收敛——执行凭据是 task_forwarded+actionKey 事实，不重复记账）。
//
// ② 的剩余错误按可重试处理：来源状态类错误已在 ① 判定拒绝（拒绝是一次性
// 且可解释的），走到这里的失败只会是 journal/入队 I/O 或 Run 并发终局——
// 前者保持 proposed 下轮重试（hasRunnableProposedActions 拦住 fold，直至
// 成功或 Run 超时），后者随 Run 收场。
func (s *Service) executeForwardAction(state *runState, team *Team, policy TeamPolicy, action *Action, pre *actionPrefetch) {
	proceed := false
	func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		err := s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
			if terminalRun(fresh.Status) {
				return nil, false
			}
			index := findProposedActionIndex(fresh, action.ActionKey)
			if index < 0 {
				return nil, false
			}
			if ok, reason := ValidateExecutable(fresh, team, policy, action, pre, nil); !ok {
				fresh.PendingActions[index].Status = ActionStatusRejected
				fresh.PendingActions[index].Reason = reason
				event := newEvent(fresh.ID, EventActionRejected, "runner", actionRejectedPayload(action, reason))
				event.IdempotencyKey = action.ActionKey
				return []*Event{event}, true
			}
			proceed = true
			return nil, false // 判定通过：无快照变更，转发在锁外执行。
		})
		if err != nil {
			log.Printf("team run %s 动作 %s 判定记账失败（下轮重试）: %v", state.runID, action.ActionKey, err)
		}
	}()
	if !proceed {
		return
	}
	run := s.store.Run(state.runID)
	if run == nil {
		return
	}
	_, _, err := s.ForwardToMember(team, run, ForwardRequest{
		RequestID:      ActionRequestID(action.ActionKey),
		SourceTaskID:   action.FromTaskID,
		TargetMemberID: action.Member,
		Note:           action.Task,
		ActionKey:      action.ActionKey,
	})
	if err != nil {
		if errors.Is(err, ErrForwardRunFinished) {
			return // Run 已终局：动作随 Run 收场（快照保持 proposed，纯预览）。
		}
		log.Printf("team run %s 动作 %s 转发失败（保持 proposed，下轮重试）: %v", state.runID, action.ActionKey, err)
		return
	}
	// 成功：快照置 executed（崩溃窗口由下一轮 actionFacts 按 journal 事实补齐）。
	state.mu.Lock()
	err = s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
		index := findProposedActionIndex(fresh, action.ActionKey)
		if index < 0 {
			return nil, false
		}
		fresh.PendingActions[index].Status = ActionStatusExecuted
		return nil, true
	})
	state.mu.Unlock()
	if err != nil {
		log.Printf("team run %s 动作 %s executed 收敛失败（下轮按 journal 事实补齐）: %v", state.runID, action.ActionKey, err)
	}
}

// convergeAction 补齐崩溃窗口（journal 已有终局事实、快照仍 proposed）的快照
// 收敛：executed → 置 executed 并补建缺失的派生任务（快照-only，事实已在
// journal 的 task_planned/task_forwarded）；rejected → 置 rejected+Reason
// （原因取自事件载荷）。不重复记账（RebuildActionEvents 的重建语义与这里
// 的收敛语义同源）。
func (s *Service) convergeAction(state *runState, team *Team, action *Action, verdict, reason string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	err := s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		index := findProposedActionIndex(fresh, action.ActionKey)
		if index < 0 {
			return nil, false
		}
		if verdict == actionVerdictRejected {
			fresh.PendingActions[index].Status = ActionStatusRejected
			fresh.PendingActions[index].Reason = reason
			return nil, true
		}
		fresh.PendingActions[index].Status = ActionStatusExecuted
		if target, retriage := failureRetriageTarget(fresh, action); retriage {
			if target != nil && target.Status == TaskFailed {
				_ = applyRetriage(fresh, target, action)
			}
		} else if action.Type == ActionTypeDelegate || action.Type == ActionTypeSummarize {
			if derived := ActionDerivedTaskID(action); derived != "" && findTaskIn(fresh, derived) == nil {
				fresh.Tasks = append(fresh.Tasks, buildActionTask(fresh, team, action))
			}
		}
		return nil, true
	})
	if err != nil {
		log.Printf("team run %s 动作 %s 崩溃窗口收敛失败（下轮重试）: %v", state.runID, action.ActionKey, err)
	}
}

// findProposedActionIndex 返回快照中仍处 proposed 的指定动作下标（无则 -1）。
func findProposedActionIndex(run *Run, actionKey string) int {
	for i := range run.PendingActions {
		if run.PendingActions[i].ActionKey == actionKey && run.PendingActions[i].Status == ActionStatusProposed {
			return i
		}
	}
	return -1
}

// memberBusyOf 返回本 Run 当前在途任务占用的成员集（快照 dispatching/running
// + 派发池在途 pendingDispatch——与 passDispatch 的 inFlight 记账同口径，
// §5.2 成员单活动任务）。
func memberBusyOf(run *Run, pendingDispatch map[string]bool) map[string]bool {
	busy := map[string]bool{}
	for i := range run.Tasks {
		switch run.Tasks[i].Status {
		case TaskDispatching, TaskRunning:
			busy[run.Tasks[i].MemberID] = true
		}
	}
	for taskID := range pendingDispatch {
		if task := findTaskIn(run, taskID); task != nil {
			busy[task.MemberID] = true
		}
	}
	return busy
}

// actionJournalFacts 是一次 journal Replay 提取的动作事实：
//   - proposed：action_proposed 幂等键计数（>1 即同 Run 重复提案，§5.2）；
//   - settled：动作终局（携带 actionKey 的 task_planned/task_forwarded/
//     task_retry_scheduled＝executed；action_rejected＝rejected）——崩溃窗口（事实已落、快照未存）
//     据此收敛，重复执行由它幂等短路；
//   - reason：rejected 事实的拒绝原因（快照收敛用）。
type actionJournalFacts struct {
	proposed map[string]int
	settled  map[string]string
	reason   map[string]string
}

// actionFacts 重放 journal 提取本 Run 的动作事实（锁外调用，本地磁盘读；
// forwardRecorded 同款读法）。读取失败返回 nil——后续判定退化为「重复与
// 终局由幂等重试兜底」（journal 重放 last-wins，内容一致不双写）。
func (s *Service) actionFacts(runID string) *actionJournalFacts {
	events, err := s.journal.Replay()
	if err != nil {
		return nil
	}
	facts := &actionJournalFacts{
		proposed: map[string]int{},
		settled:  map[string]string{},
		reason:   map[string]string{},
	}
	for _, event := range events {
		if event == nil || event.RunID != runID {
			continue
		}
		switch event.Type {
		case EventActionProposed:
			if event.IdempotencyKey != "" {
				facts.proposed[event.IdempotencyKey]++
			}
		case EventActionRejected:
			if event.IdempotencyKey != "" {
				facts.settled[event.IdempotencyKey] = actionVerdictRejected
				facts.reason[event.IdempotencyKey] = payloadString(event.Payload, "reason")
			}
		case EventTaskPlanned, EventTaskForwarded, EventTaskRetryScheduled:
			if key := payloadString(event.Payload, "actionKey"); key != "" {
				facts.settled[key] = actionVerdictExecuted
			}
		}
	}
	return facts
}

// teamInFlight 统计团队级在途任务数（§6.3 层 1 的 Team 级口径，§5.2「超过
// 并发数」拒绝与 passDispatch 派发护栏共用的同一口径）：全部 Run 快照的
// dispatching/running 任务 + teamPending（各 Run 派发池在途窗口）。锁外调用
// 即可——只读快照与 s.mu 计数，绝不触碰任何 Run 的 state.mu。
func (s *Service) teamInFlight(teamID string) int {
	count := 0
	for _, run := range s.store.RunsByTeam(teamID) {
		if terminalRun(run.Status) {
			continue
		}
		for i := range run.Tasks {
			if run.Tasks[i].Status == TaskDispatching || run.Tasks[i].Status == TaskRunning {
				count++
			}
		}
	}
	s.mu.Lock()
	count += s.teamPending[teamID]
	s.mu.Unlock()
	return count
}

// teamPendingDelta 调整团队的派发窗口计数（s.mu 保护）；归零/负值即删除键，
// 防长期运行下按 teamID 无限增长。计数与 pendingDispatch 严格 1:1（登记一次
// 必有一次收尾清除），负值只在防御意义下出现。
func (s *Service) teamPendingDelta(teamID string, delta int) {
	s.mu.Lock()
	s.teamPending[teamID] += delta
	if s.teamPending[teamID] <= 0 {
		delete(s.teamPending, teamID)
	}
	s.mu.Unlock()
}

// passDispatch 执行派发段（短暂持有 state.mu）：ReadyTasks 纯决策（含组
// 释放、成员互斥、写锁与并发护栏）并投递团队派发池。返回 false 表示循环
// 退出。任务从投递池到 executeDispatch 落 dispatching 期间由
// pendingDispatch 记账占并发额度与成员互斥——并发护栏是 Team 级口径
// （§6.3 层 1，评审遗留②）：全 Team 各 Run 的在途任务合计（含 broadcast
// 展开与派发窗口）不超过 policy.MaxParallelTasks。
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
	// 预算闸门（§9，阶段四 cost-stats）：计量=任务派发次数（journal
	// task_dispatching 计数，可精确重放；token 无法归因到 Run/Task，不做
	// token 预算——见 stats.go 文件头）。BudgetUnits>0 且已派发数≥
	// max(BudgetCeiling,BudgetUnits) 时不再选新任务，run 落 waiting_input
	//（stopReason=budget_exhausted；预算停靠是 run 级，未派发任务保持
	// planned/ready 不落终态，在途任务自然收尾——照 stopTaskBlocked 的
	// 单批落账纪律）。BudgetUnits=0 只统计（§4.2 默认）。Lead 动作派生的
	// 任务不经本闸门预检（passActions 在派发段之前同 pass 运行），其派发
	// 照常计入计数、下一轮 pass 在此停靠——预算计量以 journal 事实为准，
	// 动作路径的少量超额是可接受的有界过冲，绝不静默放行后续派发。
	if s.parkBudgetLocked(state, run, policy) {
		return true
	}
	memberBusy := map[string]bool{}
	for i := range run.Tasks {
		task := &run.Tasks[i]
		if task.Status == TaskDispatching || task.Status == TaskRunning {
			memberBusy[task.MemberID] = true
		}
	}
	// 已投递派发池、快照尚未落 dispatching 的任务同样占成员互斥（窗口期
	// 记账，防重复选中）。
	for taskID := range state.pendingDispatch {
		if task := findTaskIn(run, taskID); task != nil {
			memberBusy[task.MemberID] = true
		}
	}
	// Team 级并发护栏（评审遗留②）：inFlight 从「本 Run 快照 + 本 Run 派发
	// 窗口」改为「全 Team 各 Run 快照 dispatching/running + teamPending（各
	// Run 派发窗口，s.mu 保护，含本 Run）」。锁纪律（state.mu→s.mu 单向）：
	// 绝不锁兄弟 Run 的 state.mu（与其它 Run 的取锁方向倒置会死锁），快照经
	// store.RunsByTeam 读取；「读计数 → ReadyTasks 决策 → 登记」整段收在
	// 同一 s.mu 临界区内——读与登记之间松锁会让并发决策的 Run 各自读到旧
	// 计数、合计超发。等待中的兄弟 Run 派发窗口任务已计入 teamPending，本
	// 临界区与它们的登记/清除互斥，读数不会漏（快照先落、计数后减，计数只
	// 可能短暂偏大，绝不偏小）。
	s.mu.Lock()
	inFlight := 0
	for _, sibling := range s.store.RunsByTeam(run.TeamID) {
		if terminalRun(sibling.Status) {
			continue
		}
		for i := range sibling.Tasks {
			if sibling.Tasks[i].Status == TaskDispatching || sibling.Tasks[i].Status == TaskRunning {
				inFlight++
			}
		}
	}
	inFlight += s.teamPending[run.TeamID]
	selected := ReadyTasks(run, policy, inFlight, memberBusy, cwdByTask, s.locks)
	// 预算余量封顶（§9）：闸门判定用的是已落账计数，本批选择还要扣掉
	// 派发窗口在途（pendingDispatch，尚未落账的记账窗口）——否则同批
	// ReadyTasks 一次放行 N 个会越过阈值。余量外的任务保持 planned，
	// 追加预算（Resume）后下一轮自然入选。
	if policy.BudgetUnits > 0 {
		remaining := budgetThreshold(policy.BudgetUnits, run.BudgetCeiling) -
			state.dispatchCount - len(state.pendingDispatch)
		if remaining < len(selected) {
			selected = selected[:max(remaining, 0)]
		}
	}
	for _, taskID := range selected {
		state.pendingDispatch[taskID] = true
		s.teamPending[run.TeamID]++
		s.dispatchViaPool(state, team, policy, run.TeamID, run.ID, taskID, cwdByTask[taskID])
	}
	s.mu.Unlock()
	return true
}

// passFold 执行折叠段（短暂持有 state.mu）：全部任务终态 → fold。
// waiting_input 的 run 绝不在此 fold——停靠/结果未知语义是「驻留等待
// resume」（§9/§6.3），fold 会把它变成终态、Resume 从此被拒。窄窗口防线：
// 推进段读到 dispatching 后任务落终态并停 run，折叠段重读快照时全任务
// 已终态——停靠路径（stopTaskBlocked/settleUnknown/stopTaskWorkspaceLost）
// 的「任务终态 + run waiting_input」已改单批原子落账（评审遗留④），本守卫
// 是对任何「全终态快照 + 非 waiting_input」中间态的兜底，没有它这类快照
// 会被误折。
func (s *Service) passFold(state *runState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(state.runID)
	if run == nil {
		s.releaseAllTaskLocksLocked(state)
		s.finishLocked(state)
		return false
	}
	if run.Status == RunWaitingInput || run.Status == RunPaused {
		// 驻留等待 resume：不 fold（下一轮推进段退出循环）。paused 同守：
		// 推进段读到 dispatching 后 Pause 落账的窄窗口里，全终态快照若在此
		// 折叠会把用户的软停悄悄变成终局（resume 从此被拒），「暂停期间普通
		// completed 不改 run 状态」也就无从谈起——终局统一延到 resume 后的
		// 下一轮。
		return true
	}
	// Lead 自动动作待执行（闸门开的 proposed）时不得 fold（§5.2，阶段三）：
	// 动作执行段在本段之前的同一 pass 运行，但 watcher 结算与本段快照读取
	// 之间存在竞态——守卫确保动作事实（执行或拒绝）先于终局。闸门关闭的
	// 动作是 §11 预览形态（永不执行），不拦 fold。
	if hasRunnableProposedActions(run, policyOf(s.store.Team(run.TeamID))) {
		return true
	}
	if s.ensureAutoPlanReductionLocked(state, run) {
		return true
	}
	if status, stopReason := FoldRunStatus(run.Tasks, run.Mode, run.CollectPolicy); status != "" {
		if status == RunFailed && s.ensureFailureTriageLocked(state, run) {
			return true
		}
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
// ——阻塞期即取消窗口一（executeDispatch 获槽后先复查终态）。teamID 是计数
// 键（run.TeamID 的显式传递——team 快照可能为 nil，登记/清除必须用同一键）。
func (s *Service) dispatchViaPool(state *runState, team *Team, policy TeamPolicy, teamID, runID, taskID, cwd string) {
	pool := s.poolFor(team)
	go func() {
		pool.sem <- struct{}{}
		defer func() { <-pool.sem }()
		s.executeDispatch(state, team, policy, teamID, runID, taskID, cwd)
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
func (s *Service) executeDispatch(state *runState, team *Team, policy TeamPolicy, teamID, runID, taskID, cwd string) {
	// spawnSkipped 标记「已记账入队但 watcher 未生成」（前一代 watcher 尚
	// 未退出，spawnWatcherLocked 幂等跳过）：收尾时额外唤醒循环，让
	// adoptWatchersLocked 在旧 watcher 退出后补收养，否则要等一个等待片。
	spawnSkipped := false
	// accounted 标记窗口计数已转记：accountDispatchLocked 成功（快照落
	// dispatching，接管计数）即减 teamPending，此后入队/收尾途中不再占窗口
	// 额度——取消窗口一的信号量等待期由池容量兜底并发上限；未到达记账的
	// 出口（复查终态/准入拒绝/绑定失败等）由 defer 补减，1:1 不漏。
	accounted := false
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
		if !accounted {
			s.teamPendingDelta(teamID, -1)
		}
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
	_, session, resolveErr := s.resolver.Resolve(member.SessionID)
	if sessionNotFound(resolveErr) {
		if err := s.recoverMissingMemberSession(team, member, cwd); err != nil {
			s.failDispatch(state, runID, taskID, err.Error())
			return
		}
		_, session, _ = s.resolver.Resolve(member.SessionID)
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
	// 记账成功即转记窗口计数：快照已落 dispatching（接管 inFlight 计数），
	// 在同一 state.mu 段内减 teamPending——快照先落、计数后减，Team 级
	// 读数不漏计；此后本主体的入队/取消收尾不再占窗口额度，取消窗口一的
	// 等待期由池信号量（容量=MaxParallelTasks）兜底并发上限。
	state.mu.Lock()
	dispatch, err := s.accountDispatchLocked(state, team, run, taskID, cwd, binding)
	if err == nil && dispatch != nil {
		s.teamPendingDelta(teamID, -1)
		accounted = true
		// 预算计量（§9）：task_dispatching 事实已落 journal，已派发数 +1
		//（与 countRunDispatches 的重放口径一致——每次成功记账恰一次）。
		state.dispatchCount++
	}
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
	if _, _, err := s.queue.Enqueue(dispatch.sessionID, dispatch.submissionID, dispatch.sentText, task.Attachments...); err != nil {
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
		// 版本冲突重跑 mutate 前先重置（expireRun/Cancel 同款纪律）：闭包外
		// 声明、闭包内 append，不重置会把两轮 mutate 的取消目标重复累积
		// （评审遗留①）。
		targets = targets[:0]
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
			threadID:     task.ExecutionThreadID,
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
// workspace_lost 事件、任务 stopped 与 run waiting_input 在同一 commitRun 内
// 原子落账（journal 先行，事件序 workspace_lost → task_stopped →
// run_waiting_input；评审遗留④同款纪律——两段式提交的后段失败会让先落的
// 任务终态把全终态快照折成 RunStopped，堵死 Resume），写锁释放移到单批之后。
// 停靠任务在用户恢复 worktree/基线后可经 resume 继续，绝不自动创建新
// worktree、不自动重派。
func (s *Service) stopTaskWorkspaceLost(state *runState, runID, taskID, workspaceID, reason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	const stopReason = "工作区丢失或基线变更，任务已停靠"
	applied := false
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
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
		events := []*Event{
			newEvent(runID, EventWorkspaceLost, "runner", lostPayload),
			newEvent(runID, EventTaskStopped, "runner", map[string]any{
				"taskId":     taskID,
				"stopReason": stopReason,
			}),
		}
		if fresh.Status != RunWaitingInput {
			fresh.Status = RunWaitingInput
			fresh.StopReason = fmt.Sprintf("任务 %s 工作区丢失或基线变更，等待用户处理", taskID)
			events = append(events, newEvent(runID, EventRunWaitingInput, "runner", map[string]any{
				"reason": fresh.StopReason,
			}))
		}
		applied = true
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 工作区停靠记账失败（下轮收养重试）: %v", runID, taskID, err)
		return false
	}
	if !applied {
		return false
	}
	s.releaseTaskLockLocked(state, taskID)
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
		if taskTimeout > 0 && s.taskTimeoutExpired(state, dispatch, taskTimeout) {
			s.expireTask(state, dispatch)
			return
		}
	}
}

// taskTimeoutExpired 检查任务是否超时（§5.4 超时豁免的判定入口）：超时线已过
// 且该任务仍有 open 问题时，把 dispatchedAt 重臂为当前时刻并返回 false——
// 用户未回答不烧任务时钟（开放问题期间任务超时停算）；无开放问题照常超时。
// 重臂写入与读取都在 state.mu 内（dispatchedAt 是 mu 保护的 map）。
func (s *Service) taskTimeoutExpired(state *runState, dispatch *taskDispatch, taskTimeout time.Duration) bool {
	state.mu.Lock()
	dispatchedAt := state.dispatchedAt[dispatch.taskID]
	if dispatchedAt.IsZero() || time.Since(dispatchedAt) < taskTimeout {
		state.mu.Unlock()
		return false
	}
	if hasOpenQuestion(s.store.Run(state.runID), dispatch.taskID) {
		state.dispatchedAt[dispatch.taskID] = time.Now()
		state.mu.Unlock()
		return false
	}
	state.mu.Unlock()
	return true
}

// pollTask 观察一轮并尽可能把任务落终态。不持 state.mu（Resolve/Snapshot 是
// 慢 I/O）；落账走 settle 包装（内部短暂持锁）。返回 (settled, 供等待用的
// 会话句柄)。
func (s *Service) pollTask(state *runState, dispatch *taskDispatch, policy TeamPolicy, session *acpruntime.Session) (bool, *acpruntime.Session) {
	// 任务已被外部（取消/resume/超时）落终态 → no-op 退出（与用户在原会话
	// 手动停止不存在竞争：先记账者的结论生效）。Run 被删除同样退出。
	// 血缘防护（阶段一执行线程契约）：watcher 携带的 submissionID/线程与任务
	// 当前值不一致＝旧 attempt 或旧线程的迟到观察者——重派已发生，迟到事件
	// 绝不能完成新任务，退出不结算。空值（升级前的存量快照）不触发防护。
	if run := s.store.Run(state.runID); run == nil {
		return true, session
	} else if task := findTaskIn(run, dispatch.taskID); task != nil {
		if terminalTask(task.Status) {
			return true, session
		}
		if task.SubmissionID != "" && task.SubmissionID != dispatch.submissionID {
			return true, session
		}
		if task.ExecutionThreadID != "" && dispatch.threadID != "" && task.ExecutionThreadID != dispatch.threadID {
			return true, session
		}
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
				// 成员主动提问观察（§5.4，阶段三）：本任务 turn 进行中时兼读
				// 快照顶层 requests，AskUserQuestion 提问卡与输入卡登记为
				// pendingQuestions（任务保持 running，普通权限卡不登记——
				// questions.go 的边界声明）。
				s.observeQuestions(state, dispatch, snapshot)
				return false, session
			case "completed":
				result := ExtractResult(dispatch.sessionID, turn, policy.MaxResultChars)
				// 成功 settle → 桶记账（§6.3 规则 b：连续 4 次成功增长窗口）。
				s.AIMD.OnSuccess(dispatch.bucketKey)
				// AutoPlan Run 的 Lead 任务：completed 事实与 team-actions 动作
				// 事实单批原子落账（§5.1，settleLeadTaskLocked）；整批失败返回
				// 未 settled，watcher 下一片整套重试。其余任务走 completeTask。
				if s.settlesLeadActions(state, dispatch.taskID) {
					if !s.settleLeadTaskLocked(state, dispatch, policy, turn, &result) {
						return false, session
					}
					return true, session
				}
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

// countRunnableProposedActions 统计会被执行流处理的 proposed 动作数（§5.2
// 豁免日志用；判定与 hasRunnableProposedActions 同口径：summarize 恒可执行，
// delegate/request_context 仅 autoDispatch 开启时）。
func countRunnableProposedActions(run *Run, policy TeamPolicy) int {
	if run == nil {
		return 0
	}
	count := 0
	for i := range run.PendingActions {
		action := &run.PendingActions[i]
		if action.Status != ActionStatusProposed {
			continue
		}
		switch action.Type {
		case ActionTypeSummarize:
			count++
		case ActionTypeDelegate, ActionTypeRequestContext:
			if policy.AutoDispatch {
				count++
			}
		}
	}
	return count
}

// expireRun 处理 run 超时：先记账（全部非终态任务 failed / run_timeout），再
// fold 落 run 终态（failed），最后取消副作用。返回 (run 是否已到达终态, 豁免
// 驻留是否需要推进一轮 pass)：finished=false 且记账失败时调用方应择机重试；
// dwellPass=true 表示 runnable proposed 动作的「动作事实先于终局」豁免（§5.2，
// passFold 同一守卫）命中——超时分支跑不到动作执行段，调用方必须走一轮完整
// pass 让 passActions 收口，否则待执行动作悬死。
func (s *Service) expireRun(state *runState) (finished, dwellPass bool) {
	var targets []cancelTarget
	finished = false
	func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		run := s.store.Run(state.runID)
		if run != nil && run.Status == RunPaused {
			// 暂停冻结 run 超时（§12 阶段四工作项 4，与 stall 同调）：Pause
			// 落账到循环退出之间存在窄窗口——循环最后一轮醒来先查 runExpired
			// 再进推进段，超时线恰在窗口内越过时不得触发（软停不烧时钟）。
			// 返回 true 让循环退出（与推进段的 paused 驻留分支殊途同归）；
			// resume 经 startLoopLocked 重臂 deadline，从 resume 时刻重新起算。
			finished = true
			return
		}
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
				// 超时豁免（§5.4）：有 open 问题的任务跳过批量 failed——
				// 保持非终态，fold 因存在非终态任务不触发、run 维持非终态
				// 驻留（stall 语义，§6.3「20 分钟无成功只 stall 不停止」
				// 同调；verify_failed 超时豁免同款先例）。答案落定后下一轮
				// 超时路径按剩余时钟收口。
				if hasOpenQuestion(fresh, task.ID) {
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
				case hasRunnableProposedActions(fresh, policy):
					// Lead 自动动作待执行（§5.2「动作事实先于终局」，与
					// passFold 的 fold 前守卫对齐，置于闸门分支之前——闸门
					// 只对「动作清空后的 completed 折叠」有意义）：超时路径
					// 不得绕过守卫强制终局，否则待执行动作被静默丢弃——
					// 「待执行」变「从未发生」。照 verify_failed 超时豁免
					// 同款不 fold，但驻留不等片：返回 dwellPass 让循环走
					// 完整 pass，passActions 执行/拒绝动作后，派生任务由
					// 下一轮超时路径按 run_timeout 收口、passFold 按剩余
					// 事实终局（超时语义对任务成立，只对动作事实让步；
					// 动作链受 MaxTasksPerRun/MaxRounds 天然有界，长驻留
					// 由 §6.3 stall 通知兜底）。
					log.Printf("team run %s 超时豁免：%d 个待执行动作先于终局，驻留推进一轮 pass",
						fresh.ID, countRunnableProposedActions(fresh, policy))
					dwellPass = true
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
	return finished, dwellPass
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
		event, done := mutateTaskTerminal(fresh, taskID, status, stopReason, errorText, outcome, result, eventType, source)
		if !done {
			return nil, false
		}
		// 任务终态同批撤回其全部 open 问题（§5.4）：事件序任务事实先于
		// 问题撤回（low④ 教训——两段式提交的后段失败会让先落事实堵死
		// 收口路径）。
		events := append([]*Event{event},
			withdrawTaskQuestions(fresh, taskID, questionWithdrawTaskTerminal, source)...)
		applied = true
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 终态记账失败（watcher 继续观察重试）: %v", runID, taskID, err)
		return false
	}
	if applied {
		if eventType == EventTaskCompleted {
			// stall 计时锚点刷新（§6.3：成功即恢复，stalled 标记重置）。
			s.noteTaskSuccessLocked(state, taskID)
		}
		s.releaseTaskLockLocked(state, taskID)
		s.wakeLocked(state)
	}
	return applied
}

// mutateTaskTerminal 是任务终态的 mutate 段（applyTaskTerminalLocked 的
// 闭包抽成可组合函数）：把 fresh 快照里的任务落到指定终态并构造对应 journal
// 事件。返回 (事件, 是否应用)；任务不存在或已终态时 (nil, false)——first-wins
// 由调用方的 commitRun 闭包组合（settleLeadTaskLocked 在同一批里追加动作
// 事实，保证任务 completed 与 action_proposed/invalid_action_block 原子落账）。
func mutateTaskTerminal(run *Run, taskID, status, stopReason, errorText, outcome string, result *TaskResult, eventType, source string) (*Event, bool) {
	task := findTaskIn(run, taskID)
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
			if result.ReviewVerdict != "" {
				payload["reviewVerdict"] = result.ReviewVerdict
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
	return newEvent(run.ID, eventType, source, payload), true
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

// settlesLeadActions 报告该任务的 completed 回复是否要做 team-actions 解析
// （§5.1 解析闸门，pollTask 在锁外调用）：仅 AutoPlan Run（创建时显式声明
// plan 为空、Lead 自动规划，§10.2「plan 为空时才允许 Lead 产生 team-actions」
// ——显式 Plan 优先）且任务成员是团队 Lead 时成立。执行闸门（autoDispatch）
// 归执行流，解析与修复不受其影响。
func (s *Service) settlesLeadActions(state *runState, taskID string) bool {
	run := s.store.Run(state.runID)
	if run == nil || !run.AutoPlan {
		return false
	}
	task := findTaskIn(run, taskID)
	if task == nil {
		return false
	}
	team := s.store.Team(run.TeamID)
	return team != nil && team.LeadID != "" && task.MemberID == team.LeadID
}

// settleLeadTaskLocked 结算 AutoPlan Run 中 Lead 任务的 completed 回复（§5.1，
// 调用方约定与 Cancel/Resume 相同的锁形：本函数自行短暂持有 state.mu）。把
// 「任务 completed 事实 + 动作事实」在单个 commitRun 内原子落账（§3.1 原则
// 1：消除 completeTask 已落而动作丢失的窗口）：
//
//   - 解析成功：每动作一条 action_proposed（IdempotencyKey=ActionKey）+
//     PendingActions append（Status=proposed）；
//   - 解析/校验失败：invalid_action_block（每次失败一条，幂等键按根任务与
//     尝试号派生）+ 修复任务 actfix-{root}-{n}（成员=Lead、prompt=RepairPrompt、
//     WriteLock=none，planned）随同批 task_planned 事件落快照——先扩快照再
//     落事件、事件序任务事实先于动作事实。修复任务经既有 executeDispatch
//     派发+watcher 观察（超时/收养/终态机械免费复用），其回复在同一
//     completed 分支重解析；第 maxActionRepairs+1 次失败落 final=true、不再
//     创建修复回合，结果按普通文本保留（§5.1 规则 3）。修复 ID 超长/非法或
//     任务数达 MaxTasksPerRun 时同样确定性收场为 final，绝不静默。
//
// 幂等：批次内全部事件带幂等键，落账前 Replay 预扫描已记账键（journal.Append
// 不去重；forwardRecorded 同款）——整批失败（journal/快照写失败）时返回
// false，pollTask 以未 settled 返回、watcher 下一片整套重试（turn 仍按
// SentText 匹配；first-wins 守卫 + 键预扫描保证重试不双写）。
func (s *Service) settleLeadTaskLocked(state *runState, dispatch *taskDispatch, policy TeamPolicy, turn *jsoncompat.Object, result *TaskResult) bool {
	// ① 解析（纯内存）：完整 assistant 全文——ExtractResult 的结果已按
	// MaxResultChars 截断，尾部标记会丢，必须用全文（actions.go）。
	var actions []Action
	var violations []string
	if run := s.store.Run(state.runID); run != nil {
		actions, violations = ParseLeadActions(AssistantFullText(turn), s.store.Team(run.TeamID), run, policy)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	// 幂等键预扫描（journal 全量重放，本地磁盘读；forwardRecorded 在 state.mu
	// 内重放同款）：整批失败重试（journal 已落、快照未存）不重复落账——
	// journal.Append 不去重，防重靠这里。
	recorded := s.recordedActionKeys(state.runID)
	settled := false
	err := s.commitRun(state.runID, func(fresh *Run) ([]*Event, bool) {
		settled = false
		if terminalRun(fresh.Status) {
			return nil, false
		}
		source := findTaskIn(fresh, dispatch.taskID)
		stopReason := ""
		if result != nil {
			stopReason = result.StopReason
		}
		completedEvent, done := mutateTaskTerminal(fresh, dispatch.taskID, TaskCompleted,
			stopReason, "", "", result, EventTaskCompleted, "watcher")
		if !done {
			return nil, false
		}
		// Lead 任务结算的全部事实（含 completed 本身）带幂等键：整批失败重试
		//（journal 已落、快照未存的崩溃窗口）只补快照、不重复落账。
		events := []*Event{}
		completedKey := leadSettleKey(fresh.TeamID, fresh.ID, dispatch.taskID)
		if !recorded[completedKey] {
			completedEvent.IdempotencyKey = completedKey
			events = append(events, completedEvent)
		}
		// 任务终态同批撤回其全部 open 问题（§5.4）：本路径不经
		// applyTaskTerminalLocked（动作事实与之原子同批），撤回义务在此补齐，
		// 事件序任务事实先于问题撤回。
		events = append(events,
			withdrawTaskQuestions(fresh, dispatch.taskID, questionWithdrawTaskTerminal, "watcher")...)
		team := s.store.Team(fresh.TeamID)
		leadDriven := fresh.AutoPlan && source != nil && !isAutoReductionTask(source) && team != nil &&
			team.LeadID != "" && source.MemberID == team.LeadID
		switch {
		case leadDriven && len(violations) > 0:
			root := actionRepairRoot(dispatch.taskID)
			// 尝试号＝既有修复回合数+1，向后跳过已占用 ID（撞号守卫：
			// actionRepairRoot 已把修复任务的派生子任务归回本根链，这里再
			// 防御历史快照里的占位，绝不追加重复任务 ID）。
			attempt := nextActionRepairAttempt(fresh, root)
			repairID := actionRepairID(root, attempt)
			final := attempt > maxActionRepairs
			list := boundedViolations(violations)
			if !final && !ValidMemberID(repairID) {
				final = true
				list = append(list, fmt.Sprintf("修复任务 ID %s 超长或含非法字符（字母/数字/下划线/连字符，1-64 字符），无法创建修复回合", repairID))
			}
			if !final && len(fresh.Tasks)+1 > policy.MaxTasksPerRun {
				final = true
				list = append(list, fmt.Sprintf("任务数已达上限 %d，无法创建修复回合", policy.MaxTasksPerRun))
			}
			invalidPayload := map[string]any{
				"taskId":     dispatch.taskID,
				"attempt":    jsoncompat.Num(attempt),
				"final":      final,
				"round":      jsoncompat.Num(fresh.Round),
				"violations": list,
			}
			if key := invalidActionKey(fresh.TeamID, fresh.ID, root, attempt); !recorded[key] {
				event := newEvent(fresh.ID, EventInvalidActionBlock, "watcher", invalidPayload)
				event.IdempotencyKey = key
				events = append(events, event)
			}
			if !final {
				prompt := RepairPrompt(violations, attempt)
				ordinal := 0
				for i := range fresh.Tasks {
					if fresh.Tasks[i].Ordinal > ordinal {
						ordinal = fresh.Tasks[i].Ordinal
					}
				}
				repair := Task{
					ID:               repairID,
					MemberID:         team.LeadID,
					Prompt:           prompt,
					Attempt:          0,
					Ordinal:          ordinal + 1,
					WriteLock:        TaskWriteLockNone,
					WorkspaceMode:    WorkspaceShared,
					Status:           TaskPlanned,
					InstructionsHead: InstructionsHead(prompt),
				}
				// 创建期确定性字段（与 planner 同口径）：修复任务无依赖，
				// 创建期与派发期的 InputHash 相同（contextText 恒空串）。
				repair.InputHash = InputHashFor(&repair, PolicyDigest(team.Policy), "")
				fresh.Tasks = append(fresh.Tasks, repair)
				if key := actionRepairPlannedKey(fresh.TeamID, fresh.ID, repairID); !recorded[key] {
					event := newEvent(fresh.ID, EventTaskPlanned, "runner", map[string]any{
						"taskId":           repairID,
						"memberId":         team.LeadID,
						"instructionsHead": repair.InstructionsHead,
						"round":            jsoncompat.Num(fresh.Round),
					})
					event.IdempotencyKey = key
					events = append(events, event)
				}
			}
		case leadDriven:
			for i := range actions {
				action := actions[i]
				fresh.PendingActions = append(fresh.PendingActions, action)
				if action.Round > fresh.Round {
					fresh.Round = action.Round
				}
				if recorded[action.ActionKey] {
					continue
				}
				payload := map[string]any{
					"taskId":       dispatch.taskID,
					"index":        jsoncompat.Num(i),
					"actionKey":    action.ActionKey,
					"type":         action.Type,
					"round":        jsoncompat.Num(action.Round),
					"sourceTaskId": action.SourceTaskID,
				}
				// 有界 payload（§10.3）：任务正文只带 240 rune 头；空可选键不落。
				if action.Member != "" {
					payload["member"] = action.Member
				}
				if action.Task != "" {
					payload["taskHead"] = InstructionsHead(action.Task)
				}
				if action.FromTaskID != "" {
					payload["fromTaskId"] = action.FromTaskID
				}
				if action.WriteLock != "" {
					payload["writeLock"] = action.WriteLock
				}
				event := newEvent(fresh.ID, EventActionProposed, "watcher", payload)
				event.IdempotencyKey = action.ActionKey
				events = append(events, event)
			}
		}
		settled = true
		return events, true
	})
	if err != nil {
		log.Printf("team run %s Lead 任务 %s 结算记账失败（watcher 继续观察整套重试）: %v", state.runID, dispatch.taskID, err)
		return false
	}
	if settled {
		// Lead 任务的 completed 不经 applyTaskTerminalLocked（动作事实原子
		// 同批），stall 锚点在此刷新（§6.3）。
		s.noteTaskSuccessLocked(state, dispatch.taskID)
		s.releaseTaskLockLocked(state, dispatch.taskID)
		s.wakeLocked(state)
	}
	// settled=false：任务已被并发置终态（取消/级联等，先落者已收尾）。
	return true
}

// recordedActionKeys 收集 runID 已落账事件的全部幂等键（journal 全量重放，
// forwardRecorded 的同款读法）。读取失败返回 nil——后续按键判重全部未命中，
// 行为退化为「重复落账由内容一致的幂等重试兜底」（journal 重放 last-wins）。
func (s *Service) recordedActionKeys(runID string) map[string]bool {
	events, err := s.journal.Replay()
	if err != nil {
		return nil
	}
	keys := map[string]bool{}
	for _, event := range events {
		if event != nil && event.RunID == runID && event.IdempotencyKey != "" {
			keys[event.IdempotencyKey] = true
		}
	}
	return keys
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
// + 中文停靠原因；run 停 waiting_input。「任务终态 + run waiting_input」在
// 单个 commitRun 内原子落账（评审遗留④，Cancel 同款纪律）：事件序任务事实
// （含问题撤回）先于 run 状态——两段式提交的后段失败会让先落的任务终态把
// 全终态快照折成 RunStopped，堵死 Resume。
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
	applied := false
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		event, done := mutateTaskTerminal(fresh, taskID, TaskStopped, stopReason, errorText, outcome, nil, EventTaskStopped, "watcher")
		if !done {
			return nil, false
		}
		events := append([]*Event{event},
			withdrawTaskQuestions(fresh, taskID, questionWithdrawTaskTerminal, "watcher")...)
		if fresh.Status != RunWaitingInput {
			fresh.Status = RunWaitingInput
			fresh.StopReason = fmt.Sprintf("任务 %s 停靠：%s（处理账号或配置后可继续）", taskID, stopReason)
			events = append(events, newEvent(runID, EventRunWaitingInput, "watcher", map[string]any{
				"reason":  fresh.StopReason,
				"outcome": outcome,
			}))
		}
		applied = true
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 停靠记账失败（watcher 继续观察重试）: %v", runID, taskID, err)
		return false
	}
	if applied {
		s.releaseTaskLockLocked(state, taskID)
		s.wakeLocked(state)
	}
	return applied
}

// parkBudgetLocked 是 §9 的预算停靠（调用方持有 state.mu；passDispatch 的
// 派发段每轮调用）：BudgetUnits>0 且已派发数≥max(BudgetCeiling,BudgetUnits)
// 时把 run 落 waiting_input——单批 commitRun 原子落账（照 stopTaskBlocked
// 纪律）：stopReason 前缀 budget_exhausted，事件 run_waiting_input payload
// {units,used,stopReason}（stopReason 供 store.go 的 journal 重建还原停靠
// 语义、Resume 按前缀路由）。预算停靠是 run 级：未派发任务保持 planned/
// ready 不落终态（resume 追加预算后天然可派发），在途任务自然收尾——
// watcher 的 completed/failed 照常入账，全终态后不 fold（passFold 对
// waiting_input 驻留），等用户 Resume（追加预算继续）或 Cancel。
// 返回 true=预算已到线：本轮绝不选新任务（落账失败也拦——宁可下轮重试
// 停靠，绝不放行超额派发）。
func (s *Service) parkBudgetLocked(state *runState, run *Run, policy TeamPolicy) bool {
	used := state.dispatchCount
	units := policy.BudgetUnits
	if !budgetExhausted(units, run.BudgetCeiling, used) {
		return false
	}
	threshold := budgetThreshold(units, run.BudgetCeiling)
	applied := false
	err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		// 版本冲突重载后复查：并发停靠/取消可能已改变状态（first-wins）。
		if terminalRun(fresh.Status) || fresh.Status != RunDispatching {
			return nil, false
		}
		fresh.Status = RunWaitingInput
		fresh.StopReason = budgetStopReason(threshold, used)
		applied = true
		return []*Event{newEvent(fresh.ID, EventRunWaitingInput, "runner", map[string]any{
			"units":      jsoncompat.Num(units),
			"used":       jsoncompat.Num(used),
			"stopReason": fresh.StopReason,
		})}, true
	})
	if err != nil {
		log.Printf("team run %s 预算停靠记账失败（下轮重试，本轮拦截派发）: %v", run.ID, err)
		return true
	}
	if applied {
		// 停靠即通知（notify.go 经 commitRun 的 notifyRunStatusEvents 挂点
		// 已回调 OnRunStatus）；此处只唤醒循环——推进段读到 waiting_input
		// 即驻留退出，在途 watcher 继续收尾。
		s.wakeLocked(state)
	}
	return true
}

// resumeBudgetLocked 落预算追加事实（§9「继续本轮」；调用方持有 state.mu；
// Resume 的 budget 分支调用，落账后回到 PlanResume 既有路径）：
// run_budget_resumed payload {ceiling,units,used}，ceiling=已派发数
// （journal 重放计数，绝不依赖内存增量——重启后首次 Resume 同样成立）
// +当前 BudgetUnits，同一 commitRun 内同步写 Run.BudgetCeiling 并清
// StopReason。store.go 的 journal 重建重放本事件回填 ceiling（单调取高），
// 快照损坏也不丢追加事实。units≤0（用户把预算调成只统计）同样落事件——
// 记录「继续」的显式决定，闸门自此关闭（budgetExhausted 对 units≤0 恒
// false），Run 跑完为止。
func (s *Service) resumeBudgetLocked(state *runState, run *Run) error {
	events, err := s.journal.Replay()
	if err != nil {
		return fmt.Errorf("run %s 预算续跑对账失败: %w", run.ID, err)
	}
	used := countRunDispatches(events, run.ID)
	units := policyOf(s.store.Team(run.TeamID)).BudgetUnits
	if units < 0 {
		units = 0
	}
	ceiling := used + units
	applied := false
	if err := s.commitRun(run.ID, func(fresh *Run) ([]*Event, bool) {
		if fresh.Status != RunWaitingInput {
			return nil, false // 并发 Cancel/停靠已改写：first-wins。
		}
		fresh.BudgetCeiling = ceiling
		fresh.StopReason = ""
		applied = true
		return []*Event{newEvent(run.ID, EventRunBudgetResumed, "user", map[string]any{
			"ceiling": jsoncompat.Num(ceiling),
			"units":   jsoncompat.Num(units),
			"used":    jsoncompat.Num(used),
		})}, true
	}); err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, s.store.Run(run.ID).Status)
	}
	return nil
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
// fold、不重发）。run 已终态或已 waiting_input 时后者幂等跳过。「任务终态 +
// run waiting_input」在单个 commitRun 内原子落账（评审遗留④：两段式提交的
// 后段失败会让先落的全终态快照被 passFold 折成 RunStopped，堵死 Resume）。
func (s *Service) settleUnknown(state *runState, runID, taskID, stopReason string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	applied := false
	err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		if terminalRun(fresh.Status) {
			return nil, false
		}
		event, done := mutateTaskTerminal(fresh, taskID, TaskOutcomeUnknown, stopReason, "", "", nil, EventTaskOutcomeUnknown, "watcher")
		if !done {
			return nil, false
		}
		events := append([]*Event{event},
			withdrawTaskQuestions(fresh, taskID, questionWithdrawTaskTerminal, "watcher")...)
		if fresh.Status != RunWaitingInput {
			fresh.Status = RunWaitingInput
			fresh.StopReason = fmt.Sprintf("任务 %s 结果未知，等待用户处理", taskID)
			events = append(events, newEvent(runID, EventRunWaitingInput, "watcher", map[string]any{
				"reason": fresh.StopReason,
			}))
		}
		applied = true
		return events, true
	})
	if err != nil {
		log.Printf("team run %s 任务 %s 未决记账失败（watcher 继续观察重试）: %v", runID, taskID, err)
		return false
	}
	if applied {
		s.releaseTaskLockLocked(state, taskID)
		s.wakeLocked(state)
	}
	return applied
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
			// 终局同批撤回全部 open 问题（§5.4 兜底：常规路径问题已随任务
			// 终态撤回，这里收口竞态窗口——fold 判定与终局落账之间新登记
			// 的问题不允许留悬）。
			return append([]*Event{newEvent(fresh.ID, eventType, "runner", payload)},
				withdrawRunQuestions(fresh, questionWithdrawRunFinished, "runner")...), true
		}
		withdrawRunQuestions(fresh, questionWithdrawRunFinished, "runner")
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
			// run 状态事件成功落账（journal+快照）后的通知挂点（notify.go）：
			// 放在 SaveRun 之后，回调侧从 store 读到的才是终局快照。
			s.notifyRunStatusEvents(events)
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

// newEvent 构造 journal 事件（Sequence 由 Journal.Append 统一盖写，
// CreatedAt 在此统一盖章——§12 阶段四工作项 5：耗时统计（stats.go）与
// stall 锚点重建（notify.go rebuildLastSuccessAt）都依赖事件时间戳；
// Append 只盖 Sequence 不动时间戳。旧 journal 的事件 createdAt 恒为零值，
// 无法回填——统计侧以 coverage=none 明示（stats.go）。
func newEvent(runID, eventType, source string, payload map[string]any) *Event {
	return &Event{
		ID:        uuid.NewString(),
		RunID:     runID,
		Type:      eventType,
		Source:    source,
		Payload:   payload,
		CreatedAt: time.Now(),
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

// ---- 用户操作：Cancel / Pause / Resume ----

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
			// Run 终止同批撤回全部 open 问题（§5.4，low④ 教训：单批双事实，
			// 事件序任务事实→run 事实→问题撤回）；撤回后 decision → 409。
			events = append(events,
				withdrawRunQuestions(fresh, questionWithdrawRunFinished, "user")...)
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

// Pause 把运行软停到 paused（§12 阶段四工作项 4：区别于 cancel 的可恢复
// 软停——cancel 是终局，先落账再撤全部副作用、重复 409；pause 非终态）。
// 语义：
//
//   - 只对新派发生效：runLoop 推进段对 paused 驻留退出（照 waiting_input
//     分支同款），在途 watcher 继续跑到终态自然入账——普通 completed/failed
//     只落任务事实，run 停在 paused；停靠（quota/auth/not_configured）与
//     结果未知会把 run 升级 waiting_input（有意设计：waiting_input 语义严格
//     强于 paused，升级后 Resume 走 PlanResume 既有路径）；
//   - paused 不撤回 open 问题（非终局，问题随任务终态/Run 终局撤回）；
//   - run 超时 deadline 与 stall 计时冻结（软停不烧时钟），Resume 经
//     startLoopLocked 重臂；
//   - 暂停期间 Cancel 可用（终局优先）。
//
// checkpoint＝Run 快照 + journal 游标，不加新存储：run_paused payload.sequence
// 记录暂停落账前 journal 已有事实的最大序号（游标锚点）；恢复续跑不重发由
// 既有机制保证（ReconcileRun 账本有条目不回 ready + submissionID 确定性幂等）。
//
// 状态门：终态拒绝（ErrRunFinished→409）；已 paused 拒绝重复暂停、
// waiting_input 拒绝（它本就是停靠态，语义严格强于 paused，直接 Resume 或
// Cancel 即可）——两者都包 ErrRunNotWaiting 哨兵（HTTP 侧既有映射 409）。
func (s *Service) Pause(runID string) (*Run, error) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(runID)
	if run == nil {
		return nil, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	if terminalRun(run.Status) {
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunFinished, run.Status)
	}
	if run.Status == RunPaused {
		return nil, fmt.Errorf("%w: 当前状态 %s（已暂停，请先继续）", ErrRunNotWaiting, run.Status)
	}
	if run.Status == RunWaitingInput {
		return nil, fmt.Errorf("%w: 当前状态 %s（已停靠等待输入，无需暂停，可直接继续或取消）", ErrRunNotWaiting, run.Status)
	}
	// 游标锚点：暂停落账前 journal 已有事实的最大序号（run_paused 事件自身
	// 是游标后的首条新事实；LastSequence 与 commitRun 的 Append 都经 journal
	// 自身的锁，state.mu 已把本 Run 的并发写者挡在门外）。
	cursor := s.journal.LastSequence()
	applied := false
	if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
		// 版本冲突重载后复查：并发停靠/取消可能已改变状态（first-wins）。
		if terminalRun(fresh.Status) || fresh.Status == RunPaused || fresh.Status == RunWaitingInput {
			return nil, false
		}
		fresh.Status = RunPaused
		fresh.StopReason = "用户暂停运行"
		applied = true
		return []*Event{newEvent(runID, EventRunPaused, "user", map[string]any{
			"reason":   fresh.StopReason,
			"sequence": json.Number(strconv.FormatUint(cursor, 10)),
		})}, true
	}); err != nil {
		return nil, err
	}
	if !applied {
		// 外部写者抢先把 run 挪进停靠/终局（进程内写者已被 state.mu 串行化）：
		// 按当前状态报冲突，绝不谎报暂停成功。
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, s.store.Run(runID).Status)
	}
	// 唤醒循环：推进段读到 paused 即驻留退出。绝不 finishLocked（done 保持
	// 打开，在途 watcher 继续收尾）也绝不释放写锁（锁的存续与在途任务一致）。
	s.wakeLocked(state)
	return s.store.Run(runID), nil
}

// Resume 恢复 waiting_input / paused 的运行（§10.2：resume 只用于外因暂停，
// 不得把确定性 failed 或未决 unknown 直接改成 running）。waiting_input 流程：
// PlanResume 纯决策 → 未决 unknown 任务落 stopped（『结果未知，用户已确认
// 不再重发』，绝不重发、不重建 submission）→ 应用其余转移 → 可继续分支重启
// 调度循环；无可派发且全终态时 fold（存在 stopped → run stopped）。
// paused 流程（§12 阶段四工作项 4）见下方 paused 分支：不走 PlanResume——
// 暂停期间在途任务已自然收尾、任务快照即真相，没有 waiting_input 的用户
// 裁决迁移（unknown 转 stopped、停靠重飞）要补做。
func (s *Service) Resume(runID string) (*Run, error) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	run := s.store.Run(runID)
	if run == nil {
		return nil, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	if run.Status != RunWaitingInput && run.Status != RunPaused {
		return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, run.Status)
	}
	if run.Status == RunPaused {
		// paused 续跑（§12 工作项 4）：commitRun 推回执行态 + startLoopLocked
		// 重启循环。有意行为（resume 前写明，测试钉住）：startLoopLocked 每次启
		// 动都重臂 run 超时 deadline——暂停驻留时长是用户治理时间，从 resume
		// 时刻重新起算（stall 计时同调，resetStallClockLocked）。
		//
		// 目标态选择：暂停落在验证闸门窗口（reducing）时，在飞/已落未判的
		// 闸门结果随暂停自然收尾（Verify 已落账、判定因状态不在 reducing 而
		// 未应用）——推回 RunReducing 让推进段就地补判（闸门未过 → 升级
		// waiting_input 走 verify_failed 复验；过了 → 正常 fold）；否则推回
		// RunDispatching，剩余任务照常派发、全终态由折叠段收口（闸门未决的
		// 会被 fold 拦截重新过闸）。绝不重发已确认任务：在途任务按快照事实
		// 收养，planned/ready 才是新派发。
		target := RunDispatching
		if run.Verify != nil {
			target = RunReducing
		}
		applied := false
		if err := s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
			if fresh.Status != RunPaused {
				return nil, false // 并发 Cancel/停靠已改写：first-wins。
			}
			fresh.Status = target
			fresh.StopReason = ""
			applied = true
			return nil, true // 纯状态推进：续跑事实由其后的派发/终局事件记账。
		}); err != nil {
			return nil, err
		}
		if !applied {
			return nil, fmt.Errorf("%w: 当前状态 %s", ErrRunNotWaiting, s.store.Run(runID).Status)
		}
		s.resetStallClockLocked(state)
		s.startLoopLocked(state)
		return s.store.Run(runID), nil
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
		s.resetStallClockLocked(state)
		s.startLoopLocked(state)
		return s.store.Run(runID), nil
	}
	// 预算停靠续跑（§9「继续本轮」，与上方 paused/verify_failed 分支并列）：
	// 先 journal-first 落 run_budget_resumed（ceiling=已派发数+当前
	// BudgetUnits，同步写 Run.BudgetCeiling 快照），再走 PlanResume 既有
	// 路径——planned/ready 任务天然可派发，未决 unknown 落 stopped，quota
	// 停靠重臂。上限推导钉死为 journal 事实（事件重放，重启后
	// applyJournalEventToRun 回填 ceiling）+快照双载体，不丢失、不漂移；
	// 决策审计由 Resume 动作本身的 journal 事件承担（§9 的 decisionID 载体
	// 是成员问题 decision 端点，预算继续走 Resume 即显式用户动作+事件审计，
	// 与 amend/repair 同口径，不新增 decisionID 通道）。
	if strings.HasPrefix(run.StopReason, StopReasonBudgetExhausted) {
		if err := s.resumeBudgetLocked(state, run); err != nil {
			return nil, err
		}
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
		s.resetStallClockLocked(state)
		s.startLoopLocked(state)
	}
	return s.store.Run(runID), nil
}

// ---- 重启恢复（§8.3 的 I/O 侧）----

// RecoverRun 对单个 run 跑启动对账：journal 重放 + Probe（submissions 账本
// 与会话存活）→ ReconcileRun 纯决策 → 落账 → 未终局的 run 重启调度循环
// （在途任务由循环收养 watcher）；waiting_input/paused 驻留等待 resume，
// 但在途任务仍经 adoptInFlight 重新有人观察（§12 阶段四收养解耦；paused
// 照 waiting_input 先例不启动循环——startLoopLocked 会重臂 run 超时时钟，
// 暂停驻留不该烧时钟）。终态或不存在的 run 直接跳过。
func (s *Service) RecoverRun(runID string) error {
	state := s.stateFor(runID)
	parked := false
	if err := func() error {
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
		// 预算计量重建（§9）：已派发数按 journal 重放（与运行期增量同一
		// 口径，countRunDispatches 去重幂等）——重启绝不丢预算事实。
		state.dispatchCount = countRunDispatches(events, runID)
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
		if fresh.Status == RunWaitingInput || fresh.Status == RunPaused {
			parked = true
			return nil // 驻留等待 resume：不启动调度循环（deadline 不重臂）。
		}
		// stall 锚点重建（§6.3）：journal 最近 task_completed 的 createdAt
		// （无有效时间戳回退当前时刻，见 notify.go 的施工顺序依赖说明）。
		s.rebuildLastSuccessAt(state, events)
		s.startLoopLocked(state)
		return nil
	}(); err != nil {
		return err
	}
	if parked {
		// 收养解耦（§12 阶段四工作项 2）：waiting_input/paused Run 的在途任务
		// 重启后重新有人观察（watcher 自带任务超时兜底），但绝不启动调度循环
		// ——startLoopLocked 会重臂 run 超时时钟，驻留语义只归 resume。
		s.adoptInFlight(state)
	}
	return nil
}

// adoptInFlight 为 run 快照在途（dispatching/running）的任务补 watcher
// （§12 阶段四工作项 2 的「收养解耦」：watcher 本就独立于调度循环存活——
// 起 watcher 不等于起循环，runner.go:474 的驻留注释）。持 state.mu 复用
// adoptWatchersLocked 主体；isolated 任务的收养要走锁外工作区对账（注册表
// + rev-parse），照 loopPass 先例先收集候选、锁外交 adoptWorkspaceWatchers
// 处理。不启动调度循环、不触碰 run 状态与 deadline。终态/reducing 闸门
// 窗口/已删除的 run 直接跳过（照 passAdvance 的短路语义）。
func (s *Service) adoptInFlight(state *runState) {
	var adoptions []isolatedAdoption
	func() {
		state.mu.Lock()
		defer state.mu.Unlock()
		run := s.store.Run(state.runID)
		if run == nil || terminalRun(run.Status) || run.Status == RunReducing {
			return
		}
		team := s.store.Team(run.TeamID)
		adoptions = s.adoptWatchersLocked(state, team, run, policyOf(team))
	}()
	if len(adoptions) > 0 {
		s.adoptWorkspaceWatchers(state, adoptions)
	}
}

// RefreshRun 刷新一个 Run 的运行中状态（§12 阶段四工作项 2，周期 reconciler
// 与 RecoverRun 之外的独立对账入口）：① 收养在途任务的 watcher（adoptInFlight
// 同款主体）；② outcome_unknown 任务只读重探（ReconcileUnknown 纯决策 +
// 专用 mutate 落账，探到确定事实才单向纠正）。
//
// 三条铁律（对齐阶段四验收「结果未知任务不会自动重发」）：
//
//  1. 不改 run.Status——waiting_input 驻留等 resume；全任务因纠正转终态也
//     绝不 fold（passFold 对 waiting_input 早退正是 verify_failed/quota 停靠
//     的保护，refresh 绕过它等于杀死停靠语义），终局只由用户 Resume 触发；
//  2. 不把任何任务转回 ready——账本无条目只证明「现在查不到」，不构成
//     「从未发送」的证明（Probe 契约，recovery.go）；
//  3. 不 startLoop——startLoopLocked 每次启动都重臂 run 超时时钟。
//
// 终态或不存在的 run 直接跳过（幂等）；重探记账失败返回错误（周期循环下轮
// 重试），收养仍照常执行。
func (s *Service) RefreshRun(runID string) error {
	state := s.stateFor(runID)
	var adoptions []isolatedAdoption
	reconcileErr := func() error {
		state.mu.Lock()
		defer state.mu.Unlock()
		run := s.store.Run(runID)
		if run == nil || terminalRun(run.Status) {
			return nil
		}
		var err error
		// ② unknown 只读重探：纯决策按入口快照计算，专用 mutate 在 fresh
		// 副本上复核 outcome_unknown（快照被并发演进时本轮放弃该迁移）。
		if transitions := ReconcileUnknown(run, s.store.Team(run.TeamID), &serviceProbe{service: s}); len(transitions) > 0 {
			err = s.commitRun(runID, func(fresh *Run) ([]*Event, bool) {
				events := applyReconciledUnknown(fresh, transitions, "runner")
				return events, len(events) > 0
			})
			if run = s.store.Run(runID); run == nil {
				return err
			}
		}
		// ① 收养在途任务（reducing 闸门窗口不收养，照 passAdvance 短路）。
		if run.Status != RunReducing {
			team := s.store.Team(run.TeamID)
			adoptions = s.adoptWatchersLocked(state, team, run, policyOf(team))
		}
		return err
	}()
	if len(adoptions) > 0 {
		s.adoptWorkspaceWatchers(state, adoptions)
	}
	return reconcileErr
}

// DefaultReconcileInterval 是 waiting_input Run 刷新循环的默认周期（§12
// 阶段四工作项 2；env GATEWAY_TEAM_RECONCILE_SECONDS 可调，解析见
// cmd/gateway/main.go 的 teamReconcileIntervalFromEnv——默认值的唯一来源）。
const DefaultReconcileInterval = 60 * time.Second

// StartReconciler 启动运行中状态刷新循环（§12 阶段四工作项 2）：每 tick
// 对全部 waiting_input Run 依次 RefreshRun——各 Run 的 state.mu 天然串行
// 防重入（循环与用户 Resume/watcher 收尾互斥）。仿 accpool Poller 的
// ctx+done 形态：interval≤0 不启动，重复调用无副作用。
func (s *Service) StartReconciler(interval time.Duration) {
	if interval <= 0 {
		return
	}
	s.reconcilerMu.Lock()
	defer s.reconcilerMu.Unlock()
	if s.reconcilerCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.reconcilerCancel = cancel
	s.reconcilerDone = make(chan struct{})
	go s.reconcileLoop(ctx, interval, s.reconcilerDone)
}

// StopReconciler 停止刷新循环并同步等待在途 RefreshRun 收尾（幂等；未启动
// 时无副作用）。
func (s *Service) StopReconciler() {
	s.reconcilerMu.Lock()
	cancel, done := s.reconcilerCancel, s.reconcilerDone
	s.reconcilerCancel, s.reconcilerDone = nil, nil
	s.reconcilerMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// reconcileLoop 每 interval 对全部 waiting_input Run 依次刷新；单个 Run
// 失败只记日志不阻断其余。ctx 取消即返回（进行中的 RefreshRun 完成后退出）。
func (s *Service) reconcileLoop(ctx context.Context, interval time.Duration, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, run := range s.store.Runs() {
			if run.Status != RunWaitingInput {
				continue // 快照初筛：非 waiting_input 的 Run 循环常驻已覆盖。
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			if err := s.RefreshRun(run.ID); err != nil {
				log.Printf("team run %s 运行状态刷新失败（下轮重试）: %v", run.ID, err)
			}
		}
	}
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
				if transition.Retry != nil {
					if !taskRetryAssignmentApplied(task, *transition.Retry) {
						applyTaskRetryAssignment(task, *transition.Retry)
					}
					break
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
						if transition.Result.ReviewVerdict != "" {
							payload["reviewVerdict"] = transition.Result.ReviewVerdict
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

// RunTerminal 是 terminalRun 的导出包装：HTTP 投影按 Run 终态派生执行线程
// 的关闭态（线程随 Run 关闭，不落盘）；内部路径继续用 terminalRun。
func RunTerminal(status string) bool { return terminalRun(status) }

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

func findThreadIn(run *Run, threadID string) *ExecutionThread {
	if run == nil || threadID == "" {
		return nil
	}
	for i := range run.Threads {
		if run.Threads[i].ID == threadID {
			return &run.Threads[i]
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
