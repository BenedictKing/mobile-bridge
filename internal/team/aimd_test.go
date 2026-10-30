package team

// aimd 的单元测试（§13.1 team/scheduler 行的 AIMD/Retry-After 项，对应
// team-aimd 工作流）：错误分类表全表驱动（含与 jev.QuotaStop 的口径差异）、
// Retry-After 解析（含账本 500-rune 截断丢失场景）、AIMD 桶四规则（epoch 内
// 只减一次+冷却、lastGood 探测恢复与减半、空闲回 floor 清 lastGood、在途
// 计数准入）、桶键降级、快照导出与 policy 序列化往返。全部注入时钟推进
// epoch/冷却/空闲，不用固定 sleep 判定正确性。

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/acpview"
	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// aimdClock 是 AIMD 注册表的可控时钟（§13.1）。
type aimdClock struct {
	mu  sync.Mutex
	now time.Time
}

func newAIMDClock() *aimdClock {
	return &aimdClock{now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *aimdClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *aimdClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	c.mu.Unlock()
}

func TestClassifyTaskError(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		// rate_limited：仅确凿限流信号。
		{"HTTP 429 Too Many Requests", OutcomeRateLimited},
		{"too many requests", OutcomeRateLimited},
		{"Rate limit exceeded", OutcomeRateLimited},
		{"rate-limit reached", OutcomeRateLimited},
		{"请求过于频繁，请稍后再试", OutcomeRateLimited},
		{"触发频率限制", OutcomeRateLimited},
		{"已限流", OutcomeRateLimited},
		// quota：jev.QuotaStop 的计费子集。
		{"额度不足，请充值", OutcomeQuota},
		{"余额不足", OutcomeQuota},
		{"配额不足", OutcomeQuota},
		{"HTTP 402 Payment Required", OutcomeQuota},
		{"insufficient credit", OutcomeQuota},
		// auth。
		{"401 Unauthorized", OutcomeAuth},
		{"invalid_grant", OutcomeAuth},
		{"invalid_token", OutcomeAuth},
		{"403 forbidden", OutcomeAuth},
		{"认证失败", OutcomeAuth},
		{"登录过期", OutcomeAuth},
		// acpruntime.refusalFailure 的中文改写（401 → 停靠，不得判整轮失败）。
		{"WorkBuddy 登录已失效，请在 CLI 中执行 /login 后重试", OutcomeAuth},
		{"401 Authentication required. Please use /login command", OutcomeAuth},
		// not_configured。
		{"API key 未配置", OutcomeNotConfigured},
		{"没有配置模型", OutcomeNotConfigured},
		{"provider not configured", OutcomeNotConfigured},
		{"请先配置账号", OutcomeNotConfigured},
		// context_overflow。
		{"context length exceeded", OutcomeContextOverflow},
		{"上下文过长", OutcomeContextOverflow},
		{"prompt too long", OutcomeContextOverflow},
		{"maximum context length is 4096 tokens", OutcomeContextOverflow},
		{"This model's maximum context window is too long for the request", OutcomeContextOverflow},
		// transient。
		{"request timeout", OutcomeTransient},
		{"context deadline exceeded", OutcomeTransient},
		{"connection reset by peer", OutcomeTransient},
		{"unexpected EOF", OutcomeTransient},
		{"网络错误，请重试", OutcomeTransient},
		{"temporary failure in name resolution", OutcomeTransient},
		// 优先级：rate_limited 压过 quota（429 带计费文案仍是限流）。
		{"429 quota exceeded，余额不足", OutcomeRateLimited},
		// QuotaStop 的宽口径不计入 AIMD quota（误报代价从跳过续跑变成
		// 任务停靠，口径收紧）。
		{"usage limit reached", OutcomeUnknownText},
		{"billing", OutcomeUnknownText},
		// unknown：空文本与未分类。
		{"", OutcomeUnknownText},
		{"模型拒绝执行", OutcomeUnknownText},
		{"oops something went wrong", OutcomeUnknownText},
	}
	for _, testCase := range cases {
		if got := ClassifyTaskError(testCase.text); got != testCase.want {
			t.Errorf("ClassifyTaskError(%q) = %q，期望 %q", testCase.text, got, testCase.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		text string
		want time.Duration
	}{
		{"429 Too Many Requests; Retry-After: 120", 120 * time.Second},
		{"retry-after=45", 45 * time.Second},
		{"Retry-After: 60 seconds", 60 * time.Second},
		{"please retry_after 7", 7 * time.Second},
		// 无 Retry-After：缺省 30s。
		{"429 too many requests", 30 * time.Second},
		{"余额不足", 30 * time.Second},
		// 上限封顶 300s。
		{"Retry-After: 3600", 300 * time.Second},
	}
	for _, testCase := range cases {
		if got := ParseRetryAfter(testCase.text); got != testCase.want {
			t.Errorf("ParseRetryAfter(%q) = %v，期望 %v", testCase.text, got, testCase.want)
		}
	}
	// 账本 LastQueueError 经 500-rune 截断：数值在尾部丢失 → 回退缺省 30s；
	// 完整文本（截断前）应解析出数值。
	full := strings.Repeat("详", 495) + "Retry-After: 90" // 510 rune
	truncated := string([]rune(full)[:500])
	if strings.Contains(truncated, "90") {
		t.Fatal("截断用例构造错误：数值应被截掉")
	}
	if got := ParseRetryAfter(truncated); got != 30*time.Second {
		t.Fatalf("截断丢失 Retry-After 数值应回退 30s，实际 %v（文本尾 %q）", got,
			truncated[len(truncated)-30:])
	}
	if got := ParseRetryAfter(full); got != 90*time.Second {
		t.Fatalf("完整文本应解析出 90s，实际 %v", got)
	}
}

// bucketOf 取单桶快照（不存在时 Fatal）。
func bucketOf(t *testing.T, registry *aimdRegistry, key string) BucketState {
	t.Helper()
	for _, state := range registry.Snapshot() {
		if state.Bucket == key {
			return state
		}
	}
	t.Fatalf("桶 %s 不存在: %+v", key, registry.Snapshot())
	return BucketState{}
}

func TestAIMDDecreaseOncePerEpochWithCooldown(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("kimi:glm-5", 5, 1)
	if window, decreased := registry.OnFailure("kimi:glm-5", 30*time.Second); window != 4 || !decreased {
		t.Fatalf("首次限流应减窗 5→4: window=%d decreased=%v", window, decreased)
	}
	if registry.Admit("kimi:glm-5") {
		t.Fatal("冷却期内应拒绝准入")
	}
	// 同 epoch 重复限流：冷却顺延、不再减窗。
	if window, decreased := registry.OnFailure("kimi:glm-5", 30*time.Second); window != 4 || decreased {
		t.Fatalf("epoch 内重复限流不应再减窗: window=%d decreased=%v", window, decreased)
	}
	if registry.Admit("kimi:glm-5") {
		t.Fatal("顺延后的冷却期内仍应拒绝准入")
	}
	// 冷却结束：准入恢复，新 epoch 的限流再次减窗。
	clock.Advance(61 * time.Second) // 顺延后的冷却从 base+30s 起，60s 足以过期
	if !registry.Admit("kimi:glm-5") {
		t.Fatal("冷却结束后应恢复准入")
	}
	if window, decreased := registry.OnFailure("kimi:glm-5", 30*time.Second); window != 3 || !decreased {
		t.Fatalf("新 epoch 限流应减窗 4→3: window=%d decreased=%v", window, decreased)
	}
	// 未减容（窗口已到 1）时 decreased=false。
	registry2 := newAIMDRegistry(clock.Now)
	registry2.Touch("tiny:", 1, 1)
	if window, decreased := registry2.OnFailure("tiny:", 30*time.Second); window != 1 || decreased {
		t.Fatalf("窗口已到 1 不应再减: window=%d decreased=%v", window, decreased)
	}
}

func TestAIMDLastGoodProbeAndHalve(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("kimi:g", 5, 1)
	// 减容：w 5→4，lastGood 记住减前工作点 5。
	registry.OnFailure("kimi:g", 30*time.Second)
	if state := bucketOf(t, registry, "kimi:g"); state.Window != 4 || state.LastGood != 5 {
		t.Fatalf("减容后 w=4 lastGood=5，实际 %+v", state)
	}
	// 冷却过后 4 连成功：探回 lastGood=5（探测恢复）。
	clock.Advance(31 * time.Second)
	for i := 0; i < 4; i++ {
		registry.OnSuccess("kimi:g")
	}
	if state := bucketOf(t, registry, "kimi:g"); state.Window != 5 || state.LastGood != 5 {
		t.Fatalf("4 连成功应探回 lastGood=5，实际 %+v", state)
	}
	// 探回后（window==lastGood）再撞墙：正常减容，lastGood=减前 window=5。
	clock.Advance(31 * time.Second)
	registry.OnFailure("kimi:g", 30*time.Second)
	if state := bucketOf(t, registry, "kimi:g"); state.Window != 4 || state.LastGood != 5 {
		t.Fatalf("探回后撞墙应 w 5→4、lastGood=5，实际 %+v", state)
	}
	// 未探回 lastGood（w=4<5）又撞墙：lastGood 减半 5→2（防反复撞墙）。
	clock.Advance(31 * time.Second)
	registry.OnFailure("kimi:g", 30*time.Second)
	if state := bucketOf(t, registry, "kimi:g"); state.Window != 3 || state.LastGood != 2 {
		t.Fatalf("未探回又撞墙应 lastGood 减半 5→2、w 4→3，实际 %+v", state)
	}
	// 减半后 4 连成功从 w=3 探至 lastGood=2 之上逐档增长（w=3≥lg=2 → +1）。
	clock.Advance(31 * time.Second)
	for i := 0; i < 4; i++ {
		registry.OnSuccess("kimi:g")
	}
	if state := bucketOf(t, registry, "kimi:g"); state.Window != 4 || state.LastGood != 4 {
		t.Fatalf("增长应 +1 并刷新 lastGood，实际 %+v", state)
	}
}

func TestAIMDIdleResetToFloor(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("zcode:", 5, 2)
	registry.OnFailure("zcode:", 30*time.Second) // w 5→4，lastGood=5
	registry.OnDispatch("zcode:")
	if registry.Admit("zcode:") {
		t.Fatal("冷却期内应拒绝准入")
	}
	// 5 分钟无活动：窗口回 floor、清 lastGood 与冷却（§6.3 规则 c）。
	clock.Advance(6 * time.Minute)
	if !registry.Admit("zcode:") {
		t.Fatal("空闲重置后应恢复准入")
	}
	if state := bucketOf(t, registry, "zcode:"); state.Window != 2 || state.LastGood != 0 {
		t.Fatalf("空闲重置应回 floor=2 并清 lastGood，实际 %+v", state)
	}
	// 在途计数不因空闲重置清零（watcher 生命周期口径独立于窗口）。
	if state := bucketOf(t, registry, "zcode:"); state.InFlight != 1 {
		t.Fatalf("在途计数不应被空闲重置清除: %+v", state)
	}
}

func TestAIMDInFlightGatesAdmission(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("codex:", 2, 1)
	registry.OnDispatch("codex:")
	if !registry.Admit("codex:") {
		t.Fatal("在途 1 < 窗口 2 应放行")
	}
	registry.OnDispatch("codex:")
	if registry.Admit("codex:") {
		t.Fatal("在途 == 窗口时应拒绝")
	}
	registry.OnSettle("codex:")
	if !registry.Admit("codex:") {
		t.Fatal("收尾后应恢复放行")
	}
	// OnSettle 钳到 0 不为负（收养任务无对应 OnDispatch 的防御）。
	registry.OnSettle("codex:")
	registry.OnSettle("codex:")
	if state := bucketOf(t, registry, "codex:"); state.InFlight != 0 {
		t.Fatalf("在途计数不应为负: %+v", state)
	}
	// 未 Touch 的桶不设限；空键/nil registry 全放行。
	if !registry.Admit("unknown-bucket") {
		t.Fatal("未登记的桶不应设限")
	}
	var nilRegistry *aimdRegistry
	if !nilRegistry.Admit("codex:") || !nilRegistry.Admit("") {
		t.Fatal("nil registry 应全放行")
	}
	if window, decreased := nilRegistry.OnFailure("codex:", time.Second); window != 0 || decreased {
		t.Fatal("nil registry 的 OnFailure 应为 no-op")
	}
}

// TestAIMDAdmitReservesSlot：Admit 是预留制而非只读检查——通过即在桶内占
// 一个名额（reserved），后续 Admit 立即看到「在途+预留」达到窗口而拒绝；
// 派发未收尾（确定性失败/暂缓/取消窗口）经 Abandon 归还名额，派发成功经
// OnDispatch 把预留转记为 watcher 在途（计数总量不变）。只读口径下多个并发
// 派发池 worker 会在 Admit 与 OnDispatch 之间的整段派发 I/O 里全部通过，
// 实际在途超过窗口（限流后 window=1 时同桶仍可并发发出 2+ 条 prompt）。
func TestAIMDAdmitReservesSlot(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("kimi:", 1, 1) // 限流后 window=1 的最紧形态
	if !registry.Admit("kimi:") {
		t.Fatal("空桶首次准入应放行")
	}
	if registry.Admit("kimi:") {
		t.Fatal("首个准入已预留名额，window=1 下第二个 Admit 必须拒绝（check-then-act 超发）")
	}
	// 派发失败/暂缓：Abandon 归还名额。
	registry.Abandon("kimi:")
	if !registry.Admit("kimi:") {
		t.Fatal("Abandon 归还后应恢复准入")
	}
	// 派发成功：OnDispatch 把预留转记为在途（总量守恒，不重复占位）。
	registry.OnDispatch("kimi:")
	if state := bucketOf(t, registry, "kimi:"); state.InFlight != 1 {
		t.Fatalf("预留应转记为在途 1: %+v", state)
	}
	if registry.Admit("kimi:") {
		t.Fatal("在途 == 窗口时应拒绝")
	}
	// watcher 收尾：名额回到可用池。
	registry.OnSettle("kimi:")
	if !registry.Admit("kimi:") {
		t.Fatal("收尾后应恢复准入")
	}
	// Abandon 的防御：无预留时不动作、不吞掉在途计数。
	registry.OnDispatch("kimi:") // 预留转记为在途（inFlight=1, reserved=0）
	registry.Abandon("kimi:")
	registry.Abandon("kimi:")
	if state := bucketOf(t, registry, "kimi:"); state.InFlight != 1 {
		t.Fatalf("多余 Abandon 不应影响在途计数: %+v", state)
	}
}

// withModel 构造带 model 槽位的会话 drive（AIMD 桶键 kind:model 形态）。
func withModel(model string) func(view *acpview.View) {
	return func(view *acpview.View) {
		options := &jsoncompat.Array{}
		option := jsoncompat.NewObject()
		option.Set("id", "model")
		option.Set("currentValue", model)
		options.Items = append(options.Items, option)
		view.ApplyConfig(options, nil)
	}
}

func TestProviderBucketKeyDegradation(t *testing.T) {
	member := &TeamMember{ID: "lead", SessionID: "s-lead", AgentKind: "kimi"}
	if key := ProviderBucketKey(member, nil); key != "kimi:" {
		t.Fatalf("会话不可达应降级 kind 级: %q", key)
	}
	plain := newFakeSession("s-lead", "/tmp/lead")
	if key := ProviderBucketKey(member, plain); key != "kimi:" {
		t.Fatalf("无 model 的会话应降级 kind 级: %q", key)
	}
	withSlot := newFakeSession("s-lead", "/tmp/lead", withModel("glm-5.3"))
	if key := ProviderBucketKey(member, withSlot); key != "kimi:glm-5.3" {
		t.Fatalf("有 latestModel 应聚到 kind:model 桶: %q", key)
	}
	if key := ProviderBucketKey(nil, withSlot); key != "" {
		t.Fatalf("无成员应返回空键: %q", key)
	}
}

func TestAIMDSnapshotExport(t *testing.T) {
	clock := newAIMDClock()
	registry := newAIMDRegistry(clock.Now)
	registry.Touch("b:kimi", 5, 1)
	registry.Touch("a:codex", 3, 1)
	states := registry.Snapshot()
	if len(states) != 2 || states[0].Bucket != "a:codex" || states[1].Bucket != "b:kimi" {
		t.Fatalf("快照应按桶键排序: %+v", states)
	}
	if states[0].Window != 3 || states[0].Ceiling != 3 || states[1].Window != 5 {
		t.Fatalf("快照应含 window/ceiling: %+v", states)
	}
	registry.OnFailure("a:codex", 30*time.Second)
	if state := registry.Snapshot()[0]; state.Window != 2 || state.CooldownUntil == "" {
		t.Fatalf("减容与冷却应进入快照: %+v", state)
	}
	// Service 导出面：nil AIMD 返回 nil（httpd-api-2 据此给空表/404）。
	nilService := &Service{}
	if states := nilService.AIMDSnapshot(); states != nil {
		t.Fatalf("nil AIMD 应返回 nil，实际 %+v", states)
	}
	service := &Service{AIMD: registry}
	if len(service.AIMDSnapshot()) != 2 {
		t.Fatalf("Service.AIMDSnapshot 应透传注册表快照")
	}
}

func TestPolicyMaxTaskRetriesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	team := testTeam()
	team.Policy.MaxTaskRetries = 2
	if err := store.SaveTeam(team); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	reopened := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	if loaded := reopened.Team(team.ID); loaded == nil || loaded.Policy.MaxTaskRetries != 2 {
		t.Fatalf("maxTaskRetries 应在写入重载后保留: %+v", loaded.Policy)
	}
	// 手工序列化字段面：jsonObject 输出含 maxTaskRetries。
	encoded, err := team.MarshalJSON()
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), "\"maxTaskRetries\":2") {
		t.Fatalf("序列化输出应含 maxTaskRetries: %s", encoded)
	}
}
