package team

// inputqueue_test.go 覆盖持久输入队列账本（docs/team-demand-queue-plan.md
// §3/§8 阶段 1）：幂等受理与容量背压、修订不可变、操作幂等与版本冲突、
// 崩溃重启重放（接受后/修订后）、快照损坏按 journal 重建、分页顺序。

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newInputLedgerFixture(t *testing.T) (*InputLedger, string, string) {
	t.Helper()
	dir := t.TempDir()
	journal, err := OpenJournal(filepath.Join(dir, "team-inputs.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	t.Cleanup(func() { journal.Close() })
	ledger := LoadInputLedger(filepath.Join(dir, "team-inputs.json"), journal)
	return ledger, filepath.Join(dir, "team-inputs.json"), filepath.Join(dir, "team-inputs.jsonl")
}

// TestInputLedgerAcceptIdempotencyAndCapacity 同 ID 同内容重放返回原状态；
// 异内容冲突；容量满 429 语义（ErrQueueFull）；取消释放容量。
func TestInputLedgerAcceptIdempotencyAndCapacity(t *testing.T) {
	ledger, _, _ := newInputLedgerFixture(t)
	input, duplicate, err := ledger.Accept("team-1", "in-1", "给登录页补错误态", nil, "digest", 2)
	if err != nil || duplicate || input.ID != "in-1" || input.QueueStatus != InputQueued {
		t.Fatalf("首次受理失败: %+v %v %v", input, duplicate, err)
	}
	if input.Version != 1 || input.Revision != 1 || input.Sequence != 1 {
		t.Fatalf("初始版本号不符: %+v", input)
	}
	again, duplicate, err := ledger.Accept("team-1", "in-1", "给登录页补错误态", nil, "digest", 2)
	if err != nil || !duplicate || again.Version != input.Version {
		t.Fatalf("同内容重放应返回原状态: %+v %v %v", again, duplicate, err)
	}
	if _, _, err := ledger.Accept("team-1", "in-1", "换一个目标", nil, "digest", 2); !errors.Is(err, ErrInputContentMismatch) {
		t.Fatalf("异内容应冲突: %v", err)
	}
	// 容量 2：第二条占满，第三条 429 语义；另一团队不受影响。
	if _, _, err := ledger.Accept("team-1", "in-2", "第二个需求", nil, "digest", 2); err != nil {
		t.Fatalf("容量内受理失败: %v", err)
	}
	_, _, err = ledger.Accept("team-1", "in-3", "第三个需求", nil, "digest", 2)
	if !errors.Is(err, ErrQueueFull) || !strings.Contains(err.Error(), "上限 2") {
		t.Fatalf("容量满应 ErrQueueFull 且说明容量: %v", err)
	}
	if _, _, err := ledger.Accept("team-2", "in-4", "别的团队", nil, "digest", 2); err != nil {
		t.Fatalf("容量按 Team 独立: %v", err)
	}
	// 取消释放容量：in-2 取消后第三条可入。
	if _, _, err := ledger.Cancel("team-1", "in-2", "op-cancel-1", 1); err != nil {
		t.Fatalf("取消失败: %v", err)
	}
	if _, _, err := ledger.Accept("team-1", "in-3", "第三个需求", nil, "digest", 2); err != nil {
		t.Fatalf("取消后应释放容量: %v", err)
	}
}

// TestInputLedgerRevisePreservesHistory 修订保留不可变版本；版本冲突 409
// 语义；取消后拒绝原地编辑。
func TestInputLedgerRevisePreservesHistory(t *testing.T) {
	ledger, _, _ := newInputLedgerFixture(t)
	if _, _, err := ledger.Accept("team-1", "in-1", "初版正文", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	revised, duplicate, err := ledger.Revise("team-1", "in-1", "op-1", "修订正文", []string{"att-1"}, 1)
	if err != nil || duplicate {
		t.Fatalf("修订失败: %+v %v %v", revised, duplicate, err)
	}
	if revised.Revision != 2 || revised.Text != "修订正文" || revised.Version != 2 {
		t.Fatalf("修订后状态不符: %+v", revised)
	}
	if len(revised.Revisions) != 2 || revised.Revisions[0].ReplacedBy != 2 {
		t.Fatalf("旧版本应保留并标记被替换: %+v", revised.Revisions)
	}
	// 操作幂等：同 op 重放返回既有结果，不产生第三版。
	replay, duplicate, err := ledger.Revise("team-1", "in-1", "op-1", "修订正文", []string{"att-1"}, 1)
	if err != nil || !duplicate || replay.Revision != 2 || len(replay.Revisions) != 2 {
		t.Fatalf("操作重放应幂等: %+v %v %v", replay, duplicate, err)
	}
	// 版本冲突。
	if _, _, err := ledger.Revise("team-1", "in-1", "op-2", "再改", nil, 1); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("期望版本不符应冲突: %v", err)
	}
	// 取消后拒绝编辑。
	if _, _, err := ledger.Cancel("team-1", "in-1", "op-3", 2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Revise("team-1", "in-1", "op-4", "取消后改", nil, 3); !errors.Is(err, ErrInputNotQueued) {
		t.Fatalf("取消后应拒绝编辑: %v", err)
	}
	// 跨输入复用操作键：内容冲突。
	if _, _, err := ledger.Accept("team-1", "in-2", "另一条", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Prioritize("team-1", "in-2", "op-1", 1); !errors.Is(err, ErrInputContentMismatch) {
		t.Fatalf("操作键跨输入复用应冲突: %v", err)
	}
}

// TestInputLedgerRestartReplay 故障注入：接受后、修订后重启（重开账本），
// 输入与修订历史仍在；操作重放跨重启幂等。
func TestInputLedgerRestartReplay(t *testing.T) {
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "team-inputs.jsonl")
	snapshotPath := filepath.Join(dir, "team-inputs.json")

	open := func() *InputLedger {
		journal, err := OpenJournal(journalPath)
		if err != nil {
			t.Fatalf("journal 打开失败: %v", err)
		}
		t.Cleanup(func() { journal.Close() })
		return LoadInputLedger(snapshotPath, journal)
	}

	ledger := open()
	if _, _, err := ledger.Accept("team-1", "in-1", "重启前接受的输入", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	// 接受后重启：仍可查询、可继续（修订）。
	ledger = open()
	input := ledger.Get("team-1", "in-1")
	if input == nil || input.Text != "重启前接受的输入" || input.QueueStatus != InputQueued {
		t.Fatalf("接受后重启输入应仍在: %+v", input)
	}
	if _, _, err := ledger.Revise("team-1", "in-1", "op-1", "重启前修订的正文", nil, input.Version); err != nil {
		t.Fatal(err)
	}
	// 修订后重启：修订历史保留、序号不回退。
	ledger = open()
	input = ledger.Get("team-1", "in-1")
	if input == nil || input.Revision != 2 || input.Text != "重启前修订的正文" || len(input.Revisions) != 2 {
		t.Fatalf("修订后重启历史应保留: %+v", input)
	}
	if _, _, err := ledger.Accept("team-1", "in-2", "重启后的新输入", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	if got := ledger.Get("team-1", "in-2"); got == nil || got.Sequence != 2 {
		t.Fatalf("重启后接受序号应继续: %+v", got)
	}
	// 跨重启重放旧操作：幂等返回既有结果。
	replay, duplicate, err := ledger.Revise("team-1", "in-1", "op-1", "重启前修订的正文", nil, 1)
	if err != nil || !duplicate || replay.Revision != 2 {
		t.Fatalf("跨重启操作重放应幂等: %+v %v %v", replay, duplicate, err)
	}
}

// TestInputLedgerSnapshotCorruptRebuildsFromJournal 快照损坏（写入垃圾字
// 节）后重启：原件隔离留存，账本按 journal 事实重建。
func TestInputLedgerSnapshotCorruptRebuildsFromJournal(t *testing.T) {
	dir := t.TempDir()
	journalPath := filepath.Join(dir, "team-inputs.jsonl")
	snapshotPath := filepath.Join(dir, "team-inputs.json")
	journal, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	ledger := LoadInputLedger(snapshotPath, journal)
	if _, _, err := ledger.Accept("team-1", "in-1", "待重建的输入", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Prioritize("team-1", "in-1", "op-1", 1); err != nil {
		t.Fatal(err)
	}
	journal.Close()
	if err := os.WriteFile(snapshotPath, []byte("不是 JSON"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	_ = entries
	reopened, reopenErr := OpenJournal(journalPath)
	if reopenErr != nil {
		t.Fatal(reopenErr)
	}
	t.Cleanup(func() { reopened.Close() })
	rebuilt := LoadInputLedger(snapshotPath, journal2(reopened))
	input := rebuilt.Get("team-1", "in-1")
	if input == nil || input.Text != "待重建的输入" || !input.Priority || input.Version != 2 {
		t.Fatalf("快照损坏后应按 journal 重建: %+v", input)
	}
	// 损坏原件已隔离留存。
	quarantined := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "team-inputs.json.corrupt-") {
			quarantined = true
		}
	}
	// entries 在损坏写入前读取，重新列一次确认隔离文件存在。
	freshEntries, _ := os.ReadDir(dir)
	for _, entry := range freshEntries {
		if strings.HasPrefix(entry.Name(), "team-inputs.json.corrupt-") {
			quarantined = true
		}
	}
	if !quarantined {
		t.Fatal("损坏快照应隔离留存")
	}
}

// journal2 是断言辅助（显式类型确认），保持调用点可读。
func journal2(j *Journal) *Journal { return j }

// TestInputLedgerListPagination 列表按接受序分页，cursor 逐页推进。
func TestInputLedgerListPagination(t *testing.T) {
	ledger, _, _ := newInputLedgerFixture(t)
	ids := []string{"in-a", "in-b", "in-c"}
	for _, id := range ids {
		if _, _, err := ledger.Accept("team-1", id, "需求 "+id, nil, "digest", 8); err != nil {
			t.Fatal(err)
		}
	}
	rows, next := ledger.ListInputs("team-1", "", 2)
	if len(rows) != 2 || rows[0].ID != "in-a" || rows[1].ID != "in-b" || next != "in-b" {
		t.Fatalf("第一页不符: %+v next=%s", rows, next)
	}
	rows, next = ledger.ListInputs("team-1", next, 2)
	if len(rows) != 1 || rows[0].ID != "in-c" || next != "" {
		t.Fatalf("第二页不符: %+v next=%s", rows, next)
	}
	// 其他团队不可见。
	if rows, _ := ledger.ListInputs("team-2", "", 10); len(rows) != 0 {
		t.Fatalf("团队隔离不符: %+v", rows)
	}
}

// TestInputLedgerUnknownEventForwardCompat 未知事件类型（未来版本先行写入
// 的新事件）不破坏重放——只增不删的前向兼容。
func TestInputLedgerUnknownEventForwardCompat(t *testing.T) {
	ledger, _, journalPath := newInputLedgerFixture(t)
	if _, _, err := ledger.Accept("team-1", "in-1", "正文", nil, "digest", 8); err != nil {
		t.Fatal(err)
	}
	future := newEvent("", "input_future_event", "router", map[string]any{"inputId": "in-1", "teamId": "team-1"})
	if err := ledger.journal.Append(future); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	dir := filepath.Dir(journalPath)
	rebuilt := LoadInputLedger(filepath.Join(dir, "team-inputs.json"), reopened)
	input := rebuilt.Get("team-1", "in-1")
	if input == nil || input.QueueStatus != InputQueued {
		t.Fatalf("未知事件不得破坏重放: %+v", input)
	}
}
