package team

// aimd.go 是 §6.3 的 AIMD 控制器与错误策略表（阶段二工作项 6）：
//
//   - 错误分类表（ClassifyTaskError）：把 watcher 读到的失败文本（turn 错误
//     条目或账本 LastQueueError）分为 rate_limited / quota / auth /
//     not_configured / context_overflow / transient / unknown 七类，分层正则
//     按此优先级互斥命中。限流信号只能从错误文本读——429 从 agent 子进程
//     回传时只是消息文本，无 HTTP 状态码（dispatchOutcome 把 PromptWithBlocks
//     的一切非「任务正在运行」错误归 unknown，internal/httpd/queue.go）。
//   - AIMD 桶（aimdRegistry）：provider/model 级共享并发桶，跨 Team 共用
//     （多个 Team 打同一模型时共用一个桶）。桶键 = agentKind:latestModel，
//     会话快照取不到 latestModel（冷会话/会话不可达）时降级 kind 级，防
//     空键误聚桶。规则严格按计划 §6.3：
//       (a) 限流响应当 epoch（冷却期内）只减一次窗口，冷却延至
//           now+Retry-After，冷却内拒绝准入；
//       (b) 连续 4 次成功后窗口增长——先探回 lastGood（一次探测恢复），
//           再继续向 ceiling 逐档 +1；未探回 lastGood 又撞限流则 lastGood
//           减半（防反复撞墙）；window 刷新 lastGood=max(lastGood,window)；
//       (c) 5 分钟无活动回 floor 并清 lastGood（旧的限流状态不永久影响
//           新 Run）；
//       (d) quota/auth/not_configured 永不减桶、不冷却——只停靠受影响的
//           Run（§6.3「不冻结共享桶」；也规避与 accpool AutoSwitcher 的
//           桶级双重反应：quota 错误已由 poolhooks 切账号续跑）。
//
// 纯内存不落盘：空闲 5 分钟自然回 floor，网关重启重置可接受（桶只影响
// 准入节奏，不影响事实账本）。时钟经 now 注入供测试推进 epoch/冷却/空闲。

import (
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpruntime"
)

// ---- 错误策略表（§6.3）----

// 错误分类取值：写入 task_failed/task_stopped 事件 payload 的 outcome 键与
// Task.Outcome 快照字段。unknown 是 ClassifyTaskError 的兜底返回，不落
// Outcome 字段（未分类失败维持阶段一的确定性 failed 语义）。
const (
	OutcomeRateLimited     = "rate_limited"
	OutcomeQuota           = "quota"
	OutcomeAuth            = "auth"
	OutcomeNotConfigured   = "not_configured"
	OutcomeContextOverflow = "context_overflow"
	OutcomeTransient       = "transient"
	OutcomeUnknownText     = "unknown"
)

// 分类正则按优先级互斥：rate_limited > quota > auth > not_configured >
// context_overflow > transient。正则刻意不复用 jev.QuotaStop 整串——其口径
// 为「宁漏勿续」设计（误报代价=跳过一次续跑），AIMD 误报代价是压吞吐/误
// 停靠，各自独立维护（internal/jev/continue.go:10-15 的注释同理）。
var (
	// rate_limited：只认确凿的限流信号（429 / too many requests / rate
	// limit 及中文等价词）。rate[\s-]?limit 要求真实分隔符，排除
	// RateLimiter 一类代码标识。
	rateLimitedRE = regexp.MustCompile(`(?i)\b429\b|too\s+many\s+requests|rate[\s-]?limit|请求过于频繁|频率限制|限流`)
	// quota：jev.QuotaStop 的计费子集——额度/余额/配额/充值/402/
	// insufficient credit / payment required。
	quotaRE = regexp.MustCompile(`(?i)额度|余额|配额|充值|积分不足|欠费|insufficient\s+credit|payment\s+required|\b402\b`)
	// auth：401/403/invalid_grant/invalid_token/unauthorized 及中文等价词
	// （词表参照 accpool 的判定，internal/accpool/kimi.go:213-215）。
	// 「登录已失效」是 acpruntime.refusalFailure 对 401 的中文改写文案，必须
	// 命中——否则认证失败会被当成确定性失败，整轮 Run 直接 failed，而 §6.3
	// 的口径是停靠该任务 + Run 转 waiting_input 等用户处理。
	authRE = regexp.MustCompile(`(?i)\b401\b|\b403\b|invalid[\s_-]?grant|invalid[\s_-]?token|unauthorized|authentication\s+required|认证失败|鉴权失败|登录已失效|登录失效|登陆已失效|登陆失效|登录过期|登陆过期`)
	// notConfigured：Agent/账号未配置类文案（acphost/host.go:57 一类）。
	notConfiguredRE = regexp.MustCompile(`(?i)未配置|没有配置|not\s+configured|请先配置`)
	// contextOverflow：上下文超限（context length / prompt too long 等）。
	contextOverflowRE = regexp.MustCompile(`(?i)context\s+length|context\s+window|上下文过长|上下文超出|prompt\s+too\s+long|maximum\s+context|too\s+long`)
	// transient：网络瞬断与暂态错误——只有这类与限流进入受限重试。
	transientRE = regexp.MustCompile(`(?i)timeout|deadline|connection\s+reset|EOF|网络|temporar`)
)

// ClassifyTaskError 把失败文本映射到错误策略表（§6.3）。空文本与未命中
// 一律返回 unknown（确定性 failed，阶段一语义）。
func ClassifyTaskError(text string) string {
	switch {
	case rateLimitedRE.MatchString(text):
		return OutcomeRateLimited
	case quotaRE.MatchString(text):
		return OutcomeQuota
	case authRE.MatchString(text):
		return OutcomeAuth
	case notConfiguredRE.MatchString(text):
		return OutcomeNotConfigured
	case contextOverflowRE.MatchString(text):
		return OutcomeContextOverflow
	case transientRE.MatchString(text):
		return OutcomeTransient
	}
	return OutcomeUnknownText
}

// retryAfterRE 抓 "Retry-After: 120" 一类文本里的秒值：键名与数值之间
// 允许 ≤20 个非数字字符（冒号、空白、等号等）。全仓从未解析过 Retry-After
// HTTP 头（错误到网关时只剩文本）；账本 LastQueueError 经 500-rune 截断，
// 数值若在尾部可能丢失——ParseRetryAfter 对无匹配回退默认值。
var retryAfterRE = regexp.MustCompile(`(?i)retry[-_]?after[^0-9]{0,20}(\d+)`)

// ParseRetryAfter 从错误文本解析 Retry-After 秒值：无匹配（含被账本截断
// 丢失的场景）回退 30s；解析值超过 300s 封顶（防上游异常大值把桶冻死）。
func ParseRetryAfter(text string) time.Duration {
	if match := retryAfterRE.FindStringSubmatch(text); match != nil {
		if seconds, err := strconv.Atoi(match[1]); err == nil && seconds > 0 {
			delay := time.Duration(seconds) * time.Second
			if delay > aimdMaxRetryAfter {
				return aimdMaxRetryAfter
			}
			return delay
		}
	}
	return aimdDefaultRetryAfter
}

// ---- AIMD 桶（§6.3 规则 a–d）----

const (
	// aimdSuccessStreakTarget 是窗口增长所需的连续成功数（§6.3「连续 4 次
	// 成功后并发窗口增加 1」）。
	aimdSuccessStreakTarget = 4
	// aimdIdleReset 是空闲回 floor 的阈值（§6.3「5 分钟无活动」）。
	aimdIdleReset = 5 * time.Minute
	// Retry-After 的默认值与上限。
	aimdDefaultRetryAfter = 30 * time.Second
	aimdMaxRetryAfter     = 300 * time.Second
)

// aimdBucket 是一个 provider/model 桶的自适应并发窗口。
type aimdBucket struct {
	key            string
	window         int       // 当前并发窗口（inFlight+reserved 上限）
	ceiling        int       // 触碰过该桶的 Team 的 MaxProviderConcurrency 最大值
	floor          int       // 触碰过该桶的 Team 的 MinParallelTasks 最小值
	lastGood       int       // 最近一次减容前的工作点（探测恢复目标）
	inFlight       int       // 在途派发计数（watcher 生命周期口径）
	reserved       int       // 已通过准入、派发尚未收尾的预留计数（check-then-act 修补）
	cooldownUntil  time.Time // 冷却截止（冷却内拒绝准入）
	lastDecreaseAt time.Time // 最近一次窗口减小时刻（epoch 锚点）
	successStreak  int       // 连续成功计数
	lastActivity   time.Time // 最近活动（Touch/Admit/OnDispatch/OnSettle/成败记账）
}

// aimdRegistry 是 Service 级共享的 AIMD 桶集合：跨 Team 共桶（同 kind/model
// 的并发压力聚在一起），mutex 保护；纯内存，重启重置。
type aimdRegistry struct {
	mu      sync.Mutex
	now     func() time.Time
	buckets map[string]*aimdBucket
}

// newAIMDRegistry 创建空注册表（时钟注入点：测试用可控 clock 推进
// epoch/冷却/空闲，不用固定 sleep 判定正确性，§13.1）。
func newAIMDRegistry(now func() time.Time) *aimdRegistry {
	if now == nil {
		now = time.Now
	}
	return &aimdRegistry{now: now, buckets: map[string]*aimdBucket{}}
}

// stopOutcomeFreshSubmission 报告该 Outcome 分类的停靠任务重派时是否需要
// 派全新 submissionID（§6.3：quota/auth/not_configured 停靠→resume 重飞按
// task.Attempt+1 递增幂等键，与停靠前的旧 attempt 各自独立可审计）。
// rate_limited/transient 的重试在结算时已递增 attempt，不需要此标记。
func stopOutcomeFreshSubmission(outcome string) bool {
	switch outcome {
	case OutcomeQuota, OutcomeAuth, OutcomeNotConfigured:
		return true
	}
	return false
}

// ProviderBucketKey 计算 AIMD 桶键（§6.3 provider/model 级）：会话快照的
// latestModel 非空 → "kind:model"；会话不可达或 latestModel 为空（冷会话
// 取不到 model，依赖 session/update 回填）→ 降级 "kind:"（防空键误聚桶）。
// Resolve+Snapshot 是慢 I/O，只在锁外调用（executeDispatch 的准入步骤）。
func ProviderBucketKey(member *TeamMember, session *acpruntime.Session) string {
	if member == nil {
		return ""
	}
	if session != nil {
		if snapshot := session.Snapshot(); snapshot != nil {
			if model, _ := snapshot.MustGet("latestModel").(string); model != "" {
				return member.AgentKind + ":" + model
			}
		}
	}
	return member.AgentKind + ":"
}

// Touch 登记桶的团队策略边界：ceiling 取触碰过该桶的 Team 的
// MaxProviderConcurrency 最大值（阶段一零消费的现成配置面，现在启用），
// floor 取 MinParallelTasks 最小值。桶首次创建时窗口从 ceiling 起步
// （无失败信号不预降容，AIMD 负责向下适应）。
func (r *aimdRegistry) Touch(key string, ceiling, floor int) {
	if r == nil || key == "" {
		return
	}
	if ceiling < 1 {
		ceiling = 1
	}
	if floor < 1 {
		floor = 1
	}
	if floor > ceiling {
		floor = ceiling
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := r.buckets[key]
	if bucket == nil {
		r.buckets[key] = &aimdBucket{
			key: key, ceiling: ceiling, floor: floor,
			window: ceiling, lastActivity: r.now(),
		}
		return
	}
	if ceiling > bucket.ceiling {
		bucket.ceiling = ceiling
	}
	if floor < bucket.floor {
		bucket.floor = floor
	}
}

// Admit 报告桶当前是否可再接纳一个派发：非冷却且 inFlight+reserved<window
// （§6.3）。通过即预留一个名额（reserved++，预留制）——Admit 与 OnDispatch
// 之间隔着整段派发 I/O（记账写盘 + 入队，秒级），多个派发池 worker（同
// Team 上限 MaxParallelTasks 个、跨 Team 无上限）可同时通过只读检查后全部
// 派出，实际在途超过窗口；预留让上限在检查点即生效。预留必须经 OnDispatch
// （转记为 watcher 在途）或 Abandon（归还）收尾，否则窗口被永久占用。
// 附带惰性 IdleReset（规则 c）：距上次活动超过 5 分钟时窗口回 floor、清
// lastGood 与冷却——旧的限流状态不永久影响新 Run。registry 不主动唤醒：
// 准入被拒的任务保持 planned，由调度循环按等待片（≤25s，测试可缩）重新
// 决策；主动唤醒会形成「拒绝→唤醒→再投递→再拒绝」的自持热循环。
func (r *aimdRegistry) Admit(key string) bool {
	if r == nil || key == "" {
		return true // 未登记的桶不设限（nil registry 同理：AIMD 关闭）
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := r.buckets[key]
	if bucket == nil {
		return true
	}
	now := r.now()
	if now.Sub(bucket.lastActivity) > aimdIdleReset {
		bucket.window = bucket.floor
		bucket.lastGood = 0
		bucket.successStreak = 0
		bucket.cooldownUntil = time.Time{}
	}
	bucket.lastActivity = now
	if now.Before(bucket.cooldownUntil) {
		return false
	}
	if bucket.inFlight+bucket.reserved >= bucket.window {
		return false
	}
	bucket.reserved++
	return true
}

// OnFailure 记录一次限流响应（规则 a）：当前 epoch（距上次减小未过冷却）
// 只减一次窗口；每次限流都把冷却延至 now+retryAfter。返回 (窗口, 是否
// 本次减小)——仅窗口实际减小时记一次 provider_throttled 事件（§10.3）。
//
// 减容时的 lastGood 维护：window 已达/超过 lastGood（处于已证实的工作点
// 或更高）→ 记住减前 window 作为探测恢复目标；window 尚未探回 lastGood
// 又撞限流 → lastGood 减半（防反复撞墙）。
func (r *aimdRegistry) OnFailure(key string, retryAfter time.Duration) (window int, decreased bool) {
	if r == nil || key == "" {
		return 0, false
	}
	if retryAfter <= 0 {
		retryAfter = aimdDefaultRetryAfter
	}
	if retryAfter > aimdMaxRetryAfter {
		retryAfter = aimdMaxRetryAfter
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := r.buckets[key]
	if bucket == nil {
		return 0, false // 未 Touch 过的桶无从减容（Touch 先行）
	}
	now := r.now()
	bucket.lastActivity = now
	bucket.successStreak = 0
	if !now.Before(bucket.cooldownUntil) {
		// 新 epoch：本 epoch 首次限流，减一次窗口。
		if bucket.window >= bucket.lastGood {
			bucket.lastGood = bucket.window
		} else {
			bucket.lastGood = max(1, bucket.lastGood/2)
		}
		if bucket.window > 1 {
			bucket.window--
			decreased = true
		}
		bucket.lastDecreaseAt = now
	}
	// 冷却内重复限流：不再减窗口，但冷却顺延（持续 429 期间保持退避）。
	bucket.cooldownUntil = now.Add(retryAfter)
	return bucket.window, decreased
}

// OnSuccess 记录一次成功（规则 b）：连续 4 次成功后窗口增长——尚未探回
// lastGood 时直探 lastGood（探测恢复：4 次成功证明低窗口可用，一步回到
// 最近工作点，不必逐档爬），已回到 lastGood 之上则向 ceiling 逐档 +1；
// window 刷新 lastGood=max(lastGood,window)。
func (r *aimdRegistry) OnSuccess(key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	bucket := r.buckets[key]
	if bucket == nil {
		return
	}
	bucket.lastActivity = r.now()
	bucket.successStreak++
	if bucket.successStreak < aimdSuccessStreakTarget {
		return
	}
	bucket.successStreak = 0
	if bucket.window < bucket.ceiling {
		if bucket.window < bucket.lastGood {
			bucket.window = min(bucket.lastGood, bucket.ceiling)
		} else {
			bucket.window++
		}
	}
	if bucket.window > bucket.lastGood {
		bucket.lastGood = bucket.window
	}
}

// OnDispatch 把一个准入预留转记为在途派发（watcher 启动口径；inFlight 是
// 准入闸门的分母）。预留制下 Admit 已占位：这里只把 reserved 挪到 inFlight，
// 计数总量不变；无预留的调用（防御：预留已丢失或外部直调）按旧口径递增
// inFlight，与 OnSettle 保持对称。
func (r *aimdRegistry) OnDispatch(key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if bucket := r.buckets[key]; bucket != nil {
		if bucket.reserved > 0 {
			bucket.reserved--
		}
		bucket.inFlight++
		bucket.lastActivity = r.now()
	}
}

// Abandon 归还一个准入预留（派发未产生 watcher 的出口：确定性失败、本轮
// 暂缓、取消窗口、spawn 被旧 watcher 幂等跳过）。预留制下未归还的预留会
// 永久占住窗口名额——本方法与 OnDispatch 是预留的两个互斥收尾。
func (r *aimdRegistry) Abandon(key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if bucket := r.buckets[key]; bucket != nil && bucket.reserved > 0 {
		bucket.reserved--
		bucket.lastActivity = r.now()
	}
}

// OnSettle 记录一个在途派发收尾（watcher 退出口径；被收养任务的历史
// 派发可能没有对应的 OnDispatch，计数钳到 0 不为负）。
func (r *aimdRegistry) OnSettle(key string) {
	if r == nil || key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if bucket := r.buckets[key]; bucket != nil {
		if bucket.inFlight > 0 {
			bucket.inFlight--
		}
		bucket.lastActivity = r.now()
	}
}

// BucketState 是 AIMDSnapshot 的单桶导出（含 lastGood，§12 工作项 6 的
// 状态出口；HTTP 路由归 httpd-api-2）。
type BucketState struct {
	Bucket        string `json:"bucket"`
	Window        int    `json:"window"`
	Ceiling       int    `json:"ceiling"`
	Floor         int    `json:"floor"`
	LastGood      int    `json:"lastGood"`
	InFlight      int    `json:"inFlight"`
	SuccessStreak int    `json:"successStreak"`
	CooldownUntil string `json:"cooldownUntil,omitempty"` // RFC3339；零值省略
}

// Snapshot 返回全部桶的当前状态（按桶键排序，输出确定）。
func (r *aimdRegistry) Snapshot() []BucketState {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	states := make([]BucketState, 0, len(r.buckets))
	for _, bucket := range r.buckets {
		state := BucketState{
			Bucket:        bucket.key,
			Window:        bucket.window,
			Ceiling:       bucket.ceiling,
			Floor:         bucket.floor,
			LastGood:      bucket.lastGood,
			InFlight:      bucket.inFlight,
			SuccessStreak: bucket.successStreak,
		}
		if !bucket.cooldownUntil.IsZero() {
			state.CooldownUntil = bucket.cooldownUntil.Format(time.RFC3339)
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Bucket < states[j].Bucket })
	return states
}

// AIMDSnapshot 导出 AIMD 桶状态（nil registry 返回 nil——AIMD 关闭时
// httpd-api-2 的路由据此 404/空表）。
func (s *Service) AIMDSnapshot() []BucketState {
	if s == nil || s.AIMD == nil {
		return nil
	}
	return s.AIMD.Snapshot()
}
