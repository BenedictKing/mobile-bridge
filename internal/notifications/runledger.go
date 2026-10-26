package notifications

// runledger.go 是 Team Run 通知的记账与补发账本（编排计划 §12 阶段四工作项
// 3/5：完成/失败/取消/停止/waiting_input 四类通知 + stall 只通知不停止 §6.3，
// 带水位与送达痕迹的重启 reconciliation）。与 OutcomeLedger 并列而非复用：
// 后者的行结构硬编码 kind:"completion" 且键含 Python 时代的事件语义
// （outcomes.go outcomeKey），受 conformance goldens 压力，不适合混入 Run
// 生命周期词汇；本账本键=JSON["run",runId,kind]，行结构自带 attempts/
// nextAttempt（遗留 Python notification-delivery.json 的指数退避语义固化到
// 此，不依赖 worktree task-5572a3 存续）。
//
// 文件布局：{dataDir}/team-run-notifications.json，顶层是行键 map（照
// outcomes.go 先例）额外存 watermark（unix 秒，优雅关停时刻——启动补发扫描
// 的水位）。写盘必须临时文件+rename（照 team/store.go atomicWriteFile 的
// 语义；评审纠正：outcomes.go 的 saveLocked 是裸 os.WriteFile 非原子，
// 不可照抄）。

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// Run 通知类别（键的 kind 段；前四类是 Run 终态，waiting_input/stall 可随
// Run 生命周期反复出现）。
const (
	RunNotifyCompleted    = "completed"
	RunNotifyFailed       = "failed"
	RunNotifyCancelled    = "cancelled"
	RunNotifyStopped      = "stopped"
	RunNotifyWaitingInput = "waiting_input"
	RunNotifyStall        = "stall"
)

const (
	// runNotifyRetention 是送达痕迹与未决行的保留期（照 outcomeRetention
	// 先例 7 天）：痕迹过期即删（此后该 Run 的同类通知可再发）；未决行
	// 超期也删（事实已陈旧，不再无休止重试）。
	runNotifyRetention = 7 * 24 * time.Hour
	// runNotifyLimit 是行数上限（照 outcomeLimit 先例，按 at 最老裁剪）。
	runNotifyLimit = 2000
	// RunBackfillMargin 是启动补发扫描的水位裕量：只考虑
	// UpdatedAt≥watermark−margin 的 Run（覆盖停机期间完结的 Run 与关停
	// 瞬间在途的落账抖动）。
	RunBackfillMargin = 5 * time.Minute
	// runNotifyStallFloor 是 stall 类通知的重试间隔下限（§6.3 判定线本身
	// 是 20 分钟：比这更密的重发只会轰炸）。
	runNotifyStallFloor = 20 * time.Minute
)

// runNotifyRecurring 报告该类别是否随 Run 生命周期反复出现：waiting_input
// （一次 Run 可能多次停靠）与 stall（每段停滞各一次）送达后可重臂；终态
// 四类一经送达永久去重。
func runNotifyRecurring(kind string) bool {
	return kind == RunNotifyWaitingInput || kind == RunNotifyStall
}

// runNotificationRow 是一条 Run 通知的账本行。DeliveredAt>0 表示已送达
// （痕迹，去重用）；否则是待发行：NextAttempt≤now 即到期可试，Attempts
// 记失败次数并驱动指数退避。
type runNotificationRow struct {
	RunID       string
	TeamID      string
	Kind        string
	Title       string
	Body        string
	At          int64
	Attempts    int
	NextAttempt int64
	DeliveredAt int64
}

// RunNotificationLedger 是 Run 通知账本。Record 幂等（同键待发不重置退避；
// 终态类已送达不再发、反复类已送达按新事件重臂）；Reconcile 按 channel
// 重试直至送达。
type RunNotificationLedger struct {
	DataDir string

	mu        sync.Mutex
	rows      map[string]*runNotificationRow
	watermark int64
	wake      chan struct{}
}

// NewRunNotificationLedger 加载（或初始化）Run 通知账本。文件缺失按空账本
// 启动；解析失败（非原子写遗留的半文件等）按空账本启动并留痕——丢痕迹的
// 代价是同类通知可能重发一次，远好于拒绝启动。
func NewRunNotificationLedger(dataDir string) *RunNotificationLedger {
	ledger := &RunNotificationLedger{DataDir: dataDir, rows: map[string]*runNotificationRow{}}
	ledger.load()
	return ledger
}

func (l *RunNotificationLedger) path() string {
	return filepath.Join(l.DataDir, "team-run-notifications.json")
}

// Record 把一次 Run 通知事实写入账本（键=JSON["run",runId,kind]）：
//   - 无行：新建待发行（立即到期）；
//   - 待发中：刷新标题/正文但不重置退避（幂等重记，事件落账点可能重复回调）；
//   - 已送达：终态四类 no-op（重启补发扫描的去重依据）；waiting_input/stall
//     视为新一段事实重臂（一次 Run 可多次停靠/多段停滞）。
func (l *RunNotificationLedger) Record(runID, teamID, kind, title, body string, at time.Time) error {
	if runID == "" || kind == "" {
		return errors.New("Run 通知标识无效")
	}
	l.mu.Lock()
	key := runNotifyKey(runID, kind)
	switch row := l.rows[key]; {
	case row == nil:
		l.rows[key] = &runNotificationRow{
			RunID: runID, TeamID: teamID, Kind: kind,
			Title: title, Body: body, At: at.Unix(),
		}
	case row.DeliveredAt > 0 && !runNotifyRecurring(kind):
		l.mu.Unlock()
		return nil
	case row.DeliveredAt > 0:
		l.rows[key] = &runNotificationRow{
			RunID: runID, TeamID: teamID, Kind: kind,
			Title: title, Body: body, At: at.Unix(),
		}
	default:
		row.Title = title
		row.Body = body
		row.At = at.Unix()
	}
	l.trimLocked()
	err := l.saveLocked()
	wake := l.wake
	l.mu.Unlock()
	poke(wake)
	return err
}

// Delivered 报告该 Run 该类别的送达痕迹是否仍在（7 天内）——启动补发扫描
// 的跳过依据（崩溃无优雅关停、水位陈旧时的第二道防线）。
func (l *RunNotificationLedger) Delivered(runID, kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	row := l.rows[runNotifyKey(runID, kind)]
	return row != nil && row.DeliveredAt > 0
}

// SetWatermark 把水位盖到 at（网关优雅关停时刻）。重启后的补发扫描只考虑
// watermark−5min 之后更新的 Run；崩溃无优雅关停时水位陈旧，扫描窗口变宽，
// 由送达痕迹兜底去重。
func (l *RunNotificationLedger) SetWatermark(at time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.watermark = at.Unix()
	return l.saveLocked()
}

// Watermark 返回当前水位（从未优雅关停过为零值时间——注意 time.Unix(0,0)
// 不是零值时间，首启判定靠 IsZero）。
func (l *RunNotificationLedger) Watermark() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.watermark == 0 {
		return time.Time{}
	}
	return time.Unix(l.watermark, 0)
}

// RunBackfillCandidate 是补发扫描的输入项（装配方从 Run 快照投影：kind 由
// team.RunNotificationKindOfStatus 判定、title/body 由
// team.RunNotificationText 组装；kind 为空表示该状态不通知）。
type RunBackfillCandidate struct {
	RunID     string
	TeamID    string
	Kind      string
	UpdatedAt time.Time
	Title     string
	Body      string
}

// Backfill 是水位制启动补发扫描（评审缺失项④定案）：只考虑
// UpdatedAt≥watermark−margin 的候选（停机期间完结的 Run 覆盖在内），
// 送达痕迹仍在的跳过，命中的 Record 交给 worker 补发。水位为零（账本首启，
// 从未优雅关停）时不回扫——历史 Run 无水位约束会无界轰炸（评审问题⑤），
// 升级首启宁可不补发。返回补记条数（记日志用，含已待发的幂等重记）。
func (l *RunNotificationLedger) Backfill(now time.Time, margin time.Duration, candidates []RunBackfillCandidate) int {
	watermark := l.Watermark()
	if watermark.IsZero() {
		return 0
	}
	if margin <= 0 {
		margin = RunBackfillMargin
	}
	threshold := watermark.Add(-margin)
	recorded := 0
	for _, candidate := range candidates {
		if candidate.RunID == "" || candidate.Kind == "" {
			continue
		}
		if candidate.UpdatedAt.Before(threshold) {
			continue
		}
		if l.Delivered(candidate.RunID, candidate.Kind) {
			continue
		}
		if err := l.Record(candidate.RunID, candidate.TeamID, candidate.Kind,
			candidate.Title, candidate.Body, now); err != nil {
			log.Printf("team run %s 通知补记失败（%s）: %v", candidate.RunID, candidate.Kind, err)
			continue
		}
		recorded++
	}
	return recorded
}

// Reconcile 是一次补发尝试（worker 每 tick 调用；测试直接驱动）：读取当前
// 通知设置，对到期的待发行逐渠道 Publish（任一渠道成功即送达，行转痕迹，
// 保留 runNotifyRetention）；全部失败按指数退避顺延
// next=now+min(300,5*2^attempts) 秒，stall 类下限 20 分钟。无启用渠道时不
// 尝试也不计失败（用户关闭通知期间保持待发，开启后随下轮发出）。痕迹与
// 超期未决行按保留期清理。
func (l *RunNotificationLedger) Reconcile(now time.Time, publisher *Publisher) error {
	config := ReadSettings(l.DataDir)
	channels := Channels(config)
	clickBase, _ := config.MustGet("clickBase").(string)
	nowUnix := now.Unix()
	l.mu.Lock()
	changed := false
	for key, row := range l.rows {
		if row.DeliveredAt > 0 {
			if now.Sub(time.Unix(row.DeliveredAt, 0)) > runNotifyRetention {
				delete(l.rows, key)
				changed = true
			}
			continue
		}
		if row.At > 0 && now.Sub(time.Unix(row.At, 0)) > runNotifyRetention {
			delete(l.rows, key)
			changed = true
		}
	}
	due := []string{}
	for key, row := range l.rows {
		if row.DeliveredAt == 0 && row.NextAttempt <= nowUnix {
			due = append(due, key)
		}
	}
	l.mu.Unlock()
	sort.Strings(due)
	if publisher != nil && channels.Len() > 0 {
		for _, key := range due {
			l.mu.Lock()
			row := l.rows[key]
			l.mu.Unlock()
			if row == nil || row.DeliveredAt != 0 {
				continue // 并发清理/送达（如 Backfill 同轮重记）。
			}
			click := RunClickURL(clickBase, row.TeamID, row.RunID)
			delivered := false
			for _, channel := range channels.Keys() {
				if err := publisher.Publish(config, channel, row.Title, row.Body, click); err == nil {
					delivered = true
				}
			}
			l.mu.Lock()
			if current := l.rows[key]; current != nil && current.DeliveredAt == 0 {
				if delivered {
					current.DeliveredAt = nowUnix
					current.Attempts = 0
					current.NextAttempt = 0
				} else {
					current.Attempts++
					current.NextAttempt = nowUnix + int64(runNotifyBackoff(current.Kind, current.Attempts)/time.Second)
				}
				changed = true
			}
			l.mu.Unlock()
		}
	}
	if changed {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.saveLocked()
	}
	return nil
}

// RunClickURL 组装 Run 通知的 deep link（clickBase+'/#run='+teamId+'/'+runId）。
// clickBase 是通知设置里的跳转基址（notifications.json，此前只有遗留 Python
// 端消费、Go 端首读）；为空返回空串表示不带跳转（ntfy click/Bark url 字段
// 由 Publisher 空串时省略）。
func RunClickURL(clickBase, teamID, runID string) string {
	if clickBase == "" {
		return ""
	}
	return strings.TrimRight(clickBase, "/") + "/#run=" + teamID + "/" + runID
}

// runNotifyBackoff 固化遗留 Python delivery 的指数退避公式：
// next=now+min(300,5*2^attempts) 秒（attempts 为本轮失败后的累计失败次数，
// 首次失败 attempts=1 → 10s）；stall 类下限 20 分钟（§6.3 判定线同宽——
// 更密的重发只会轰炸）。
func runNotifyBackoff(kind string, attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	delay := 5 * time.Second
	for i := 0; i < attempts; i++ {
		delay *= 2
		if delay >= 5*time.Minute {
			delay = 5 * time.Minute
			break
		}
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	if kind == RunNotifyStall && delay < runNotifyStallFloor {
		delay = runNotifyStallFloor
	}
	return delay
}

// ---- worker（仿 OutcomeLedger.StartPublisher 的形态，补每键退避与即时唤醒）----

// RunNotificationWorker 周期执行 Reconcile；Record 通过 wake 通道即时唤醒
// （首次尝试不必等整周期）。Stop/Done 语义照 outcomes.Worker。
type RunNotificationWorker struct {
	ledger    *RunNotificationLedger
	publisher *Publisher
	interval  time.Duration
	stop      chan struct{}
	done      chan struct{}
	poke      chan struct{}
}

// StartWorker 启动补发 worker（interval≤0 按 1 分钟）。启动即 reconcile 一次
// （照 StartPublisher 先例：装配侧先 Backfill 再 StartWorker，补记的行随首
// 轮立即发出）。同一账本只应启动一个 worker（后启动者接管 wake）。
func (l *RunNotificationLedger) StartWorker(interval time.Duration, publisher *Publisher) *RunNotificationWorker {
	if interval <= 0 {
		interval = time.Minute
	}
	worker := &RunNotificationWorker{
		ledger:    l,
		publisher: publisher,
		interval:  interval,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		poke:      make(chan struct{}, 1),
	}
	l.mu.Lock()
	l.wake = worker.poke
	l.mu.Unlock()
	go worker.run()
	return worker
}

func (w *RunNotificationWorker) run() {
	defer close(w.done)
	w.reconcileOnce()
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-w.poke:
		case <-w.stop:
			return
		}
		w.reconcileOnce()
	}
}

func (w *RunNotificationWorker) reconcileOnce() {
	if err := w.ledger.Reconcile(time.Now(), w.publisher); err != nil {
		log.Printf("team run 通知补发失败（下轮重试）: %v", err)
	}
}

// Close 停止 worker 并等待在途 Reconcile 收尾（幂等）。
func (w *RunNotificationWorker) Close() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// ---- 持久化 ----

func runNotifyKey(runID, kind string) string {
	return string(jsoncompat.EncodePythonDefault(&jsoncompat.Array{Items: []any{"run", runID, kind}}))
}

// poke 非阻塞唤醒 worker（nil 通道安全跳过）。
func poke(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func (row *runNotificationRow) toObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("runId", row.RunID)
	object.Set("teamId", row.TeamID)
	object.Set("kind", row.Kind)
	object.Set("title", row.Title)
	object.Set("body", row.Body)
	object.Set("at", row.At)
	object.Set("attempts", row.Attempts)
	object.Set("nextAttempt", row.NextAttempt)
	if row.DeliveredAt > 0 {
		object.Set("deliveredAt", row.DeliveredAt)
	}
	return object
}

func runRowFromObject(value any) *runNotificationRow {
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		return nil
	}
	row := &runNotificationRow{}
	row.RunID, _ = object.MustGet("runId").(string)
	row.TeamID, _ = object.MustGet("teamId").(string)
	row.Kind, _ = object.MustGet("kind").(string)
	row.Title, _ = object.MustGet("title").(string)
	row.Body, _ = object.MustGet("body").(string)
	row.At = numberValue(object.MustGet("at"))
	row.Attempts = int(numberValue(object.MustGet("attempts")))
	row.NextAttempt = numberValue(object.MustGet("nextAttempt"))
	row.DeliveredAt = numberValue(object.MustGet("deliveredAt"))
	if row.RunID == "" || row.Kind == "" {
		return nil
	}
	return row
}

func (l *RunNotificationLedger) load() {
	raw, err := os.ReadFile(l.path())
	if err != nil {
		return
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		log.Printf("team run 通知账本读取失败，按空账本启动（半文件/损坏，原子写后不应出现）: %v", err)
		return
	}
	object, _ := value.(*jsoncompat.Object)
	if object == nil {
		return
	}
	for _, key := range object.Keys() {
		if key == "watermark" {
			l.watermark = numberValue(object.MustGet(key))
			continue
		}
		if row := runRowFromObject(object.MustGet(key)); row != nil {
			l.rows[key] = row
		}
	}
}

func (l *RunNotificationLedger) saveLocked() error {
	if err := os.MkdirAll(l.DataDir, 0o700); err != nil {
		return err
	}
	return atomicWriteRunLedger(l.path(), append(jsoncompat.EncodePythonIndent2(l.snapshotLocked()), '\n'))
}

func (l *RunNotificationLedger) snapshotLocked() *jsoncompat.Object {
	result := jsoncompat.NewObject()
	result.Set("watermark", l.watermark)
	keys := make([]string, 0, len(l.rows))
	for key := range l.rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result.Set(key, l.rows[key].toObject())
	}
	return result
}

func (l *RunNotificationLedger) trimLocked() {
	if len(l.rows) <= runNotifyLimit {
		return
	}
	keys := make([]string, 0, len(l.rows))
	for key := range l.rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return l.rows[keys[i]].At < l.rows[keys[j]].At
	})
	for _, key := range keys[:len(keys)-runNotifyLimit] {
		delete(l.rows, key)
	}
}

// atomicWriteRunLedger 照 team/store.go atomicWriteFile 的写盘语义（MkdirAll
// 0700、pid+纳秒唯一临时文件 0600、rename 原子替换）：杀进程最多留下孤儿
// 临时文件，主文件永远是完整的旧版或新版——「半写后可加载」由 rename 的
// 原子性保证，孤儿 tmp 与主文件同名前缀不同、load 不会读到。
func atomicWriteRunLedger(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
