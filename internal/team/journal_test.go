package team

// journal_test.go 是事件时间戳契约的单元测试（§12 阶段四工作项 5，§13.1
// team/journal）：newEvent 统一盖章 createdAt 后，Append→Replay（含重开）
// 必须无损往返（RFC3339Nano 精度）；Append 只盖 Sequence、绝不改写既有
// createdAt；旧 journal 的零值 createdAt 照常读回（历史 Run 统计 coverage
// =none 的宽容基础，绝不拒绝旧账本）。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestNewEventStampsCreatedAt 钉住 newEvent 的盖章事实：全部事件工厂共用
// 该构造点（grep 确认包内唯一 `&Event{` 生产构造），一处盖章全覆盖——
// 耗时统计（stats.go）与 stall 锚点重建（notify.go）的数据源契约。
func TestNewEventStampsCreatedAt(t *testing.T) {
	before := time.Now()
	event := newEvent("run-1", EventTaskDispatching, "runner", map[string]any{"taskId": "a"})
	if event.CreatedAt.IsZero() {
		t.Fatal("newEvent 应盖章 createdAt（统计/stall 依赖），得到零值")
	}
	if event.CreatedAt.After(time.Now().Add(time.Second)) || event.CreatedAt.Before(before.Add(-time.Second)) {
		t.Fatalf("createdAt 应取当前时刻，得到 %v（区间 [%v, now]）", event.CreatedAt, before)
	}
}

// TestJournalAppendRoundTripsCreatedAt：盖章事件经 Append 落盘、重开
// journal 后 Replay 回读，createdAt 必须与原值同一时刻（RFC3339Nano 序列化
// 无损；毫秒精度断言防时钟精度差异误报）。
func TestJournalAppendRoundTripsCreatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	stamped := newEvent("run-ts", EventRunCreated, "user", map[string]any{"goal": "统计目标"})
	if stamped.CreatedAt.IsZero() {
		t.Fatal("前置失败：newEvent 未盖章")
	}
	if err := journal.Append(stamped); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("重开 journal: %v", err)
	}
	defer reopened.Close()
	events, err := reopened.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应回读 1 条事件，得到 %d", len(events))
	}
	got := events[0].CreatedAt
	if got.IsZero() {
		t.Fatal("Replay 回读的 createdAt 不得为零值（统计会退化 coverage=none）")
	}
	diff := got.Sub(stamped.CreatedAt)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Millisecond {
		t.Fatalf("createdAt 往返应无损：写入 %v 回读 %v", stamped.CreatedAt, got)
	}
}

// TestJournalAppendPreservesPresetCreatedAt：Append 的契约是只盖 Sequence
// （journal.go 注释），调用方预置的 createdAt（如测试、外部对账写入）必须
// 原样落盘，不被 Append 改写。
func TestJournalAppendPreservesPresetCreatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer journal.Close()
	preset := time.Date(2026, 10, 1, 8, 30, 0, 123456789, time.UTC)
	event := newEvent("run-p", EventTaskPlanned, "user", map[string]any{"taskId": "a"})
	event.CreatedAt = preset
	if err := journal.Append(event); err != nil {
		t.Fatalf("Append: %v", err)
	}
	events, err := journal.Replay()
	if err != nil || len(events) != 1 {
		t.Fatalf("Replay: %v %d", err, len(events))
	}
	if !events[0].CreatedAt.Equal(preset) {
		t.Fatalf("Append 不得改写调用方预置的 createdAt：%v vs %v", events[0].CreatedAt, preset)
	}
}

// TestJournalLegacyZeroCreatedAtTolerated：旧 journal（时间戳盖章前落盘）
// 的 createdAt 是零值序列化（0001-01-01T00:00:00Z），重放必须照常读回零值
// ——绝不拒绝旧账本；统计侧据此落 coverage=none（ComputeRunStats 的历史
// Run 明示「无数据」，见 stats_test.go）。
func TestJournalLegacyZeroCreatedAtTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	legacy := map[string]any{
		"id": "legacy-1", "runId": "run-old", "type": EventRunCreated,
		"source": "user", "payload": map[string]any{"goal": "历史目标"},
		"sequence": 1, "createdAt": "0001-01-01T00:00:00Z",
	}
	line, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("旧账本（零值 createdAt）必须可打开: %v", err)
	}
	defer journal.Close()
	events, err := journal.Replay()
	if err != nil || len(events) != 1 {
		t.Fatalf("旧账本 Replay: %v %d", err, len(events))
	}
	if !events[0].CreatedAt.IsZero() {
		t.Fatalf("旧事件的零值 createdAt 应原样读回，得到 %v", events[0].CreatedAt)
	}
	// 续写新事件：新旧时间戳共存于同一 journal（升级窗口），互不干扰。
	fresh := newEvent("run-old", EventTaskPlanned, "user", map[string]any{"taskId": "a"})
	if err := journal.Append(fresh); err != nil {
		t.Fatalf("续写: %v", err)
	}
	if fresh.Sequence != 2 {
		t.Fatalf("续写序号应为 2，得到 %d", fresh.Sequence)
	}
	// 落盘行内新旧两条的 createdAt 分别为零值与时间戳字面量。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"createdAt":"0001-01-01T00:00:00Z"`)) {
		t.Fatalf("旧事件行应保留零值 createdAt 字面量: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"type":"`+EventTaskPlanned+`"`)) {
		t.Fatalf("新事件行应已落盘: %s", raw)
	}
}
