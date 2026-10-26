package notifications

// runledger_test.go 是 Run 通知账本的单元测试（§12 阶段四工作项 3/5）：
// Record 幂等与反复类重臂、原子写（杀进程模拟半写后可加载）、每键指数退避
// 曲线（stall 类 20 分钟下限）、送达转痕迹与保留期、水位制启动补发扫描
// （窗口内补发/痕迹在的跳过/崩溃陈旧水位不轰炸/首启无水位不回扫）、
// httptest fake 服务端验证 ntfy/bark 载荷含 click deep link（照 publish_test
// 先例）。等待不用固定 sleep 判定正确性（§13.1）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func runRowOf(t *testing.T, ledger *RunNotificationLedger, runID, kind string) *runNotificationRow {
	t.Helper()
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	row := ledger.rows[runNotifyKey(runID, kind)]
	if row == nil {
		return nil
	}
	copied := *row
	return &copied
}

func runRowCount(ledger *RunNotificationLedger) int {
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return len(ledger.rows)
}

// writeRunNotifySettings 落一份通知设置（ReadSettings 按缺省合并，测试只写
// 关心的键）。
func writeRunNotifySettings(t *testing.T, dir string, fields map[string]any) {
	t.Helper()
	object := jsoncompat.NewObject()
	for key, value := range fields {
		object.Set(key, value)
	}
	if err := os.WriteFile(filepath.Join(dir, "notifications.json"),
		append(jsoncompat.EncodePythonIndent2(object), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunClickURLComposition(t *testing.T) {
	if got := RunClickURL("https://gw.example.com/", "team-1", "run-9"); got != "https://gw.example.com/#run=team-1/run-9" {
		t.Fatalf("click 组装不符: %s", got)
	}
	if got := RunClickURL("", "team-1", "run-9"); got != "" {
		t.Fatalf("空 clickBase 不应带跳转: %s", got)
	}
}

// TestRunLedgerRecordDedupAndRearm：Record 幂等（同键待发不重置退避）；终态
// 类送达后永不再发；waiting_input/stall 送达后按新事件重臂。
func TestRunLedgerRecordDedupAndRearm(t *testing.T) {
	ledger := NewRunNotificationLedger(t.TempDir())
	at := time.Unix(1000, 0)
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "t1", "b1", at); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "t2", "b2", at); err != nil {
		t.Fatal(err)
	}
	if runRowCount(ledger) != 1 {
		t.Fatalf("同键重记不应新增行: %d", runRowCount(ledger))
	}
	row := runRowOf(t, ledger, "run-1", RunNotifyCompleted)
	if row.Title != "t2" || row.DeliveredAt != 0 || row.Attempts != 0 {
		t.Fatalf("待发重记应刷新文案且不重置状态: %+v", row)
	}
	// 手工落送达痕迹（送达路径由 Reconcile 测试覆盖）。
	ledger.mu.Lock()
	ledger.rows[runNotifyKey("run-1", RunNotifyCompleted)].DeliveredAt = at.Unix()
	ledger.rows[runNotifyKey("run-1", RunNotifyWaitingInput)] = &runNotificationRow{
		RunID: "run-1", TeamID: "team-1", Kind: RunNotifyWaitingInput, At: at.Unix(), DeliveredAt: at.Unix()}
	ledger.mu.Unlock()
	// 终态类：已送达 → no-op。
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "t3", "b3", at); err != nil {
		t.Fatal(err)
	}
	if row := runRowOf(t, ledger, "run-1", RunNotifyCompleted); row.DeliveredAt == 0 || row.Title != "t2" {
		t.Fatalf("终态类已送达应永久去重: %+v", row)
	}
	// 反复类（waiting_input/stall）：已送达 → 重臂为待发。
	if err := ledger.Record("run-1", "team-1", RunNotifyWaitingInput, "w1", "wb", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if row := runRowOf(t, ledger, "run-1", RunNotifyWaitingInput); row.DeliveredAt != 0 || row.Title != "w1" || row.NextAttempt != 0 {
		t.Fatalf("waiting_input 送达后应按新停靠重臂: %+v", row)
	}
	if err := ledger.Record("run-1", "team-1", RunNotifyStall, "s1", "sb", at.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if row := runRowOf(t, ledger, "run-1", RunNotifyStall); row.DeliveredAt != 0 || row.Title != "s1" {
		t.Fatalf("stall 送达后应按下段停滞重臂: %+v", row)
	}
	if err := ledger.Record("", "team-1", RunNotifyCompleted, "t", "b", at); err == nil {
		t.Fatal("空 runID 应报错")
	}
}

// TestRunLedgerAtomicWriteAndTornLoad：写盘走临时文件+rename（杀进程最多留
// 孤儿 tmp，主文件恒完整）；半写主文件（旧非原子写遗留/磁盘损坏形态）按空
// 账本加载，绝不拒绝启动。
func TestRunLedgerAtomicWriteAndTornLoad(t *testing.T) {
	dir := t.TempDir()
	ledger := NewRunNotificationLedger(dir)
	at := time.Unix(2000, 0)
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "标题", "正文", at); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SetWatermark(at); err != nil {
		t.Fatal(err)
	}
	// 正常往返：重新加载后行与水位都在。
	loaded := NewRunNotificationLedger(dir)
	if row := runRowOf(t, loaded, "run-1", RunNotifyCompleted); row == nil || row.Title != "标题" || row.At != at.Unix() {
		t.Fatalf("行应持久化往返: %+v", row)
	}
	if !loaded.Watermark().Equal(at) {
		t.Fatalf("水位应持久化往返: %v", loaded.Watermark())
	}
	// 杀进程模拟①：rename 前的孤儿临时文件（垃圾内容）不影响加载。
	path := filepath.Join(dir, "team-run-notifications.json")
	tmp := path + ".tmp-99999-1"
	if err := os.WriteFile(tmp, []byte("{garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	again := NewRunNotificationLedger(dir)
	if runRowOf(t, again, "run-1", RunNotifyCompleted) == nil {
		t.Fatal("孤儿 tmp 不应影响主文件加载")
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	// 杀进程模拟②：主文件被截成半截 JSON（非原子写者的崩溃残留形态）——
	// 按空账本加载，不 panic、不报错。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	torn := NewRunNotificationLedger(dir)
	if runRowCount(torn) != 0 || !torn.Watermark().IsZero() {
		t.Fatalf("半文件应按空账本启动: rows=%d watermark=%v", runRowCount(torn), torn.Watermark())
	}
}

// TestRunLedgerBackoffCurve：每键指数退避 next=now+min(300,5*2^attempts) 秒；
// 未到期不重试；stall 类下限 20 分钟。
func TestRunLedgerBackoffCurve(t *testing.T) {
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	dir := t.TempDir()
	writeRunNotifySettings(t, dir, map[string]any{
		"enabled": true, "server": server.URL, "topic": "runs",
	})
	ledger := NewRunNotificationLedger(dir)
	publisher := NewPublisher(nil)
	t0 := time.Unix(3000, 0)
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "标题", "正文", t0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record("run-2", "team-1", RunNotifyStall, "停滞", "停滞正文", t0); err != nil {
		t.Fatal(err)
	}
	// 退避曲线：attempts=n → next=t0+min(300,5*2^n) 秒。
	wantCurve := []int64{10, 20, 40, 80, 160, 300, 300}
	offset := int64(0)
	for i, want := range wantCurve {
		attemptAt := t0.Add(time.Duration(offset) * time.Second)
		if err := ledger.Reconcile(attemptAt, publisher); err != nil {
			t.Fatal(err)
		}
		row := runRowOf(t, ledger, "run-1", RunNotifyCompleted)
		if row.Attempts != i+1 {
			t.Fatalf("第 %d 次重试后 attempts 应为 %d，实际 %d", i+1, i+1, row.Attempts)
		}
		if row.NextAttempt != attemptAt.Unix()+want {
			t.Fatalf("第 %d 次失败退避应为 +%ds，实际 +%ds", i+1, want, row.NextAttempt-attemptAt.Unix())
		}
		// 未到期不重试。
		if err := ledger.Reconcile(attemptAt.Add(time.Duration(want-1)*time.Second), publisher); err != nil {
			t.Fatal(err)
		}
		if row = runRowOf(t, ledger, "run-1", RunNotifyCompleted); row.Attempts != i+1 {
			t.Fatalf("退避未到期不应重试: attempts=%d", row.Attempts)
		}
		offset += want
	}
	// stall 类：首次失败即 20 分钟下限。
	if err := ledger.Reconcile(t0, publisher); err != nil {
		t.Fatal(err)
	}
	stall := runRowOf(t, ledger, "run-2", RunNotifyStall)
	if stall.Attempts != 1 || stall.NextAttempt != t0.Unix()+int64(runNotifyStallFloor/time.Second) {
		t.Fatalf("stall 退避应命中 20 分钟下限: %+v", stall)
	}
	mu.Lock()
	total := requests
	mu.Unlock()
	if total != len(wantCurve)+1 {
		t.Fatalf("发送次数应与到期重试一致（%d），实际 %d", len(wantCurve)+1, total)
	}
}

// TestRunLedgerReconcileDeliversClickPayload：fake ntfy/bark 服务端（照
// publish_test 先例）验证载荷含 click deep link；送达后行转痕迹、不再重发、
// 保留 7 天后清理。
func TestRunLedgerReconcileDeliversClickPayload(t *testing.T) {
	var mu sync.Mutex
	seen := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		_ = json.NewDecoder(r.Body).Decode(&value)
		mu.Lock()
		seen = append(seen, value)
		mu.Unlock()
		if r.URL.Path == "/push" {
			_, _ = w.Write([]byte(`{"code":200}`))
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	writeRunNotifySettings(t, dir, map[string]any{
		"enabled": true, "server": server.URL, "topic": "runs", "token": "tk",
		"barkEnabled": true, "barkServer": server.URL, "barkKey": "bk",
		"clickBase": "https://gw.example.com",
	})
	ledger := NewRunNotificationLedger(dir)
	publisher := NewPublisher(nil)
	t0 := time.Unix(4000, 0)
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "运行完成：目标", "正文", t0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reconcile(t0, publisher); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	payloads := append([]map[string]any(nil), seen...)
	mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("ntfy 与 Bark 应各收到一条，实际 %d: %#v", len(payloads), payloads)
	}
	ntfy, bark := payloads[0], payloads[1]
	if ntfy["click"] != "https://gw.example.com/#run=team-1/run-1" || ntfy["title"] != "运行完成：目标" {
		t.Fatalf("ntfy 载荷应含 click 与标题: %#v", ntfy)
	}
	if ntfy["message"] != "正文" {
		t.Fatalf("ntfy 正文不符: %#v", ntfy)
	}
	if bark["url"] != "https://gw.example.com/#run=team-1/run-1" || bark["title"] != "运行完成：目标" {
		t.Fatalf("Bark 载荷应含 url 跳转与标题: %#v", bark)
	}
	// 送达后转痕迹；同刻再 reconcile 不重发。
	if !ledger.Delivered("run-1", RunNotifyCompleted) {
		t.Fatal("送达痕迹应在")
	}
	if err := ledger.Reconcile(t0.Add(time.Minute), publisher); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	after := len(seen)
	mu.Unlock()
	if after != 2 {
		t.Fatalf("已送达不应重发，实际 %d 条", after)
	}
	// 痕迹保留 7 天后清理（此后同类通知可再发）。
	if err := ledger.Reconcile(t0.Add(8*24*time.Hour), publisher); err != nil {
		t.Fatal(err)
	}
	if runRowOf(t, ledger, "run-1", RunNotifyCompleted) != nil {
		t.Fatal("超期送达痕迹应清理")
	}
}

// TestRunLedgerReconcileNoChannelsKeepsPending：通知全关时不尝试、不计
// 失败（开启后随下轮发出），行保持待发。
func TestRunLedgerReconcileNoChannelsKeepsPending(t *testing.T) {
	ledger := NewRunNotificationLedger(t.TempDir())
	t0 := time.Unix(5000, 0)
	if err := ledger.Record("run-1", "team-1", RunNotifyFailed, "标题", "正文", t0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reconcile(t0, NewPublisher(nil)); err != nil {
		t.Fatal(err)
	}
	row := runRowOf(t, ledger, "run-1", RunNotifyFailed)
	if row.Attempts != 0 || row.DeliveredAt != 0 {
		t.Fatalf("无启用渠道不应计失败: %+v", row)
	}
}

// TestRunLedgerWatermarkBackfill：水位制启动补发扫描——停机窗口内完结的 Run
// 补记、送达痕迹仍在的跳过、窗口外的跳过、首启无水位不回扫、崩溃陈旧水位
// 只靠痕迹去重（不无界轰炸）。
func TestRunLedgerWatermarkBackfill(t *testing.T) {
	dir := t.TempDir()
	ledger := NewRunNotificationLedger(dir)
	shutdown := time.Unix(6000, 0)
	if err := ledger.SetWatermark(shutdown); err != nil {
		t.Fatal(err)
	}
	now := shutdown.Add(2 * time.Hour)
	candidates := []RunBackfillCandidate{
		// 停机期间完结：应补记。
		{RunID: "run-during", TeamID: "t1", Kind: RunNotifyCompleted,
			UpdatedAt: shutdown.Add(time.Hour), Title: "停机完结", Body: "b"},
		// 关停前后 5min 裕量内的：应补记。
		{RunID: "run-margin", TeamID: "t1", Kind: RunNotifyFailed,
			UpdatedAt: shutdown.Add(-4 * time.Minute), Title: "裕量内", Body: "b"},
		// 窗口外（水位−5min 之前完结的历史 Run）：跳过。
		{RunID: "run-old", TeamID: "t1", Kind: RunNotifyCancelled,
			UpdatedAt: shutdown.Add(-6 * time.Minute), Title: "窗口外", Body: "b"},
		// 非通知状态：跳过。
		{RunID: "run-active", TeamID: "t1", Kind: "",
			UpdatedAt: shutdown.Add(time.Hour), Title: "", Body: ""},
	}
	if recorded := ledger.Backfill(now, RunBackfillMargin, candidates); recorded != 2 {
		t.Fatalf("窗口内应补记 2 条，实际 %d", recorded)
	}
	if runRowOf(t, ledger, "run-during", RunNotifyCompleted) == nil ||
		runRowOf(t, ledger, "run-margin", RunNotifyFailed) == nil {
		t.Fatal("停机窗口内的通知应入账待发")
	}
	if runRowOf(t, ledger, "run-old", RunNotifyCancelled) != nil {
		t.Fatal("窗口外的历史 Run 不应回扫")
	}
	// 送达痕迹仍在 → 跳过（崩溃陈旧水位的兜底：不重复轰炸）。把窗口内
	// 两条都置为已送达再扫：零补记、痕迹不被重记抹掉。
	ledger.mu.Lock()
	ledger.rows[runNotifyKey("run-during", RunNotifyCompleted)].DeliveredAt = now.Unix()
	ledger.rows[runNotifyKey("run-margin", RunNotifyFailed)].DeliveredAt = now.Unix()
	ledger.mu.Unlock()
	if recorded := ledger.Backfill(now, RunBackfillMargin, candidates); recorded != 0 {
		t.Fatalf("痕迹仍在的不应重记，实际 %d", recorded)
	}
	if row := runRowOf(t, ledger, "run-during", RunNotifyCompleted); row == nil || row.DeliveredAt == 0 {
		t.Fatal("痕迹不应被重记抹掉")
	}
	// 首启无水位：不回扫（防历史 Run 无界轰炸）。
	fresh := NewRunNotificationLedger(t.TempDir())
	if recorded := fresh.Backfill(now, RunBackfillMargin, candidates); recorded != 0 {
		t.Fatalf("无水位不应回扫，实际 %d", recorded)
	}
}

// TestRunLedgerWorkerStopsCleanly：worker 启停同步收尾（照 outcomes 先例）。
func TestRunLedgerWorkerStopsCleanly(t *testing.T) {
	worker := NewRunNotificationLedger(t.TempDir()).StartWorker(time.Millisecond, nil)
	worker.Close()
	worker.Close() // 幂等
}

// TestRunLedgerWorkerWakesOnRecord：Record 即时唤醒 worker（首次尝试不必等
// 整周期）——fake 服务端在远大于 interval 的时限内收到请求即算通过。
func TestRunLedgerWorkerWakesOnRecord(t *testing.T) {
	var mu sync.Mutex
	seen := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dir := t.TempDir()
	writeRunNotifySettings(t, dir, map[string]any{
		"enabled": true, "server": server.URL, "topic": "runs", "clickBase": "https://gw.example.com",
	})
	ledger := NewRunNotificationLedger(dir)
	// interval 取远大于测试时限：只有即时唤醒能在时限内送达。
	worker := ledger.StartWorker(time.Hour, NewPublisher(nil))
	defer worker.Close()
	if err := ledger.Record("run-1", "team-1", RunNotifyWaitingInput, "需要输入", "正文", time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := seen
		mu.Unlock()
		if count > 0 && ledger.Delivered("run-1", RunNotifyWaitingInput) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	count := seen
	mu.Unlock()
	t.Fatalf("Record 应即时唤醒 worker（interval=1h 仍在 5s 内送达），请求数 %d", count)
}

// TestRunLedgerBodyCarriesTaskResults：完成通知的正文按行原样透传（前 3 个
// 任务结果的组装在 team.RunNotificationText，这里验证账本→载荷不丢正文）。
func TestRunLedgerBodyCarriesTaskResults(t *testing.T) {
	var mu sync.Mutex
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var value map[string]any
		_ = json.NewDecoder(r.Body).Decode(&value)
		mu.Lock()
		body, _ = value["message"].(string)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	dir := t.TempDir()
	writeRunNotifySettings(t, dir, map[string]any{
		"enabled": true, "server": server.URL, "topic": "runs",
	})
	ledger := NewRunNotificationLedger(dir)
	long := strings.Repeat("结", 250)
	if err := ledger.Record("run-1", "team-1", RunNotifyCompleted, "标题",
		"任务 2 个：完成 2\n- a：完成甲\n- b："+long, time.Unix(7000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Reconcile(time.Unix(7000, 0), NewPublisher(nil)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(body, "- a：完成甲") || !strings.Contains(body, long) {
		t.Fatalf("正文应原样透传（含结果摘要行）: %q", body)
	}
}
