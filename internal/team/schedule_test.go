package team

// schedule_test.go 覆盖定时触发（§12 阶段四工作项 1、§13.1）：cron 最小
// 解析器表驱动、due 计算（错过不补跑）、非终态 Run 抑制（含 waiting_input
// 驻留）、journal-first 触发协议的崩溃窗口三态（journal 有事件/无事件/
// 半写——快照丢失但 journal 已落）、回扫不重触发、触发失败预算（确定性
// CreateRun 失败达预算放弃该触发点不楔死；预算耗尽当次恰半写的触发按
// journal 已生效收尾不误放弃）、Start/Stop 同步退出（done 通道关闭断言）、
// ScheduledFrom 的 JSON 往返/Clone/损坏重建回填。时钟全部可控
//（Scheduler.Now 注入固定时区的冻结时刻），不用固定 sleep 判定正确性
//（§13.1 末行）。

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// scheduleZone 是固定偏移时区（+8）：cron 语义按本地时区计算，测试注入
// 固定时区避免机器时区差异。
var scheduleZone = time.FixedZone("SCHED-TEST", 8*3600)

func scheduleTime(day int, hour, minute int) time.Time {
	return time.Date(2026, 10, day, hour, minute, 0, 0, scheduleZone)
}

// ---- cron 最小解析器 ----

func TestParseCronSpec(t *testing.T) {
	valid := []struct {
		name string
		expr string
	}{
		{"全通配", "* * * * *"},
		{"每日 9 点整", "0 9 * * *"},
		{"逗号列表", "0 9,21 * * *"},
		{"区间", "30 9-11 * * *"},
		{"区间与列表混用", "0,30 8,18-20 * * *"},
		{"前导零", "05 09 * * *"},
		{"周一到周五", "0 9 * * 1-5"},
		{"周日 0 与 7 等价", "0 9 * * 7"},
		{"日与月限定", "0 0 1 1 *"},
		{"全数字显式列出", "0 9 1,15 * *"},
	}
	for _, testCase := range valid {
		if _, err := parseCronSpec(testCase.expr); err != nil {
			t.Fatalf("%s: %q 应解析成功，实际 %v", testCase.name, testCase.expr, err)
		}
	}
	invalid := []struct {
		name    string
		expr    string
		wantErr string
	}{
		{"四个字段", "0 9 * *", "5 个字段"},
		{"六个字段（秒级不支持）", "0 0 9 * * *", "5 个字段"},
		{"@daily 别名不支持", "@daily", "5 个字段"},
		{"步进不支持", "*/5 * * * *", "不支持步进"},
		{"区间步进不支持", "1-30/2 * * * *", "不支持步进"},
		{"英文星期名不支持", "0 9 * * MON", "非数字"},
		{"英文月名不支持", "0 9 * JAN *", "非数字"},
		{"分钟越界", "60 * * * *", "越界"},
		{"小时越界", "* 24 * * *", "越界"},
		{"日越界", "* * 32 * *", "越界"},
		{"日为零越界", "* * 0 * *", "越界"},
		{"月越界", "* * * 13 *", "越界"},
		{"周越界（8）", "* * * * 8", "越界"},
		{"区间颠倒", "50-10 * * * *", "颠倒"},
		{"多个连字符", "1-2-3 * * * *", "多个连字符"},
		{"空列表项", "1,,2 * * * *", "空项"},
		{"负数", "-5 * * * *", "空数字"},
	}
	for _, testCase := range invalid {
		_, err := parseCronSpec(testCase.expr)
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: %q 应报含 %q 的错误，实际 %v", testCase.name, testCase.expr, testCase.wantErr, err)
		}
	}
}

func TestCronNextAfter(t *testing.T) {
	cases := []struct {
		name  string
		expr  string
		after time.Time
		want  time.Time
	}{
		{
			"每日 9 点：当日 9 点已过取次日",
			"0 9 * * *",
			scheduleTime(26, 10, 0),
			scheduleTime(27, 9, 0),
		},
		{
			"每日 9 点：恰好 9:00 取次日（严格晚于）",
			"0 9 * * *",
			scheduleTime(26, 9, 0),
			scheduleTime(27, 9, 0),
		},
		{
			"每日 9 点：8:59 取当日",
			"0 9 * * *",
			scheduleTime(26, 8, 59),
			scheduleTime(26, 9, 0),
		},
		{
			"工作日 9 点：周五午后取下周一",
			"0 9 * * 1-5",
			time.Date(2026, 10, 30, 12, 0, 0, 0, scheduleZone), // 周五
			time.Date(2026, 11, 2, 9, 0, 0, 0, scheduleZone),   // 周一
		},
		{
			"分钟列表取最早",
			"15,45 9 * * *",
			scheduleTime(26, 9, 16),
			scheduleTime(26, 9, 45),
		},
		{
			"月内日期限定",
			"0 0 15 * *",
			scheduleTime(16, 0, 0),
			time.Date(2026, 11, 15, 0, 0, 0, 0, scheduleZone),
		},
		{
			"日与周同时限定取 OR（每月 1 号或每周一）",
			"0 0 1 * 1",
			time.Date(2026, 10, 26, 1, 0, 0, 0, scheduleZone), // 周一 10-26（ dow 命中）
			time.Date(2026, 11, 1, 0, 0, 0, 0, scheduleZone),  // 下月 1 号（dom 命中，早于 11-02 周一）
		},
		{
			"周日 7 归一：周日 9 点",
			"0 9 * * 7",
			time.Date(2026, 10, 28, 10, 0, 0, 0, scheduleZone), // 周三
			time.Date(2026, 11, 1, 9, 0, 0, 0, scheduleZone),   // 周日
		},
	}
	for _, testCase := range cases {
		spec, err := parseCronSpec(testCase.expr)
		if err != nil {
			t.Fatalf("%s: 解析失败: %v", testCase.name, err)
		}
		got, ok := spec.nextAfter(testCase.after)
		if !ok {
			t.Fatalf("%s: 应存在下一个匹配时刻", testCase.name)
		}
		if !got.Equal(testCase.want) {
			t.Fatalf("%s: 下一时刻应为 %v，实际 %v", testCase.name, testCase.want, got)
		}
	}
	// 不可满足的 cron（2 月 30 日）在 4 年扫描上界内无解。
	spec, err := parseCronSpec("0 0 30 2 *")
	if err != nil {
		t.Fatalf("2 月 30 应可解析: %v", err)
	}
	if got, ok := spec.nextAfter(scheduleTime(26, 0, 0)); ok {
		t.Fatalf("2 月 30 日不应存在匹配时刻，实际 %v", got)
	}
}

func TestScheduleTriggerIDDeterminism(t *testing.T) {
	first := scheduleTriggerID("team-1", 1790000000)
	if first != scheduleTriggerID("team-1", 1790000000) {
		t.Fatal("同 team 同到期点的 triggerID 必须确定性一致（崩溃重试的幂等键）")
	}
	if first == scheduleTriggerID("team-1", 1790000001) {
		t.Fatal("不同到期点的 triggerID 必须互异")
	}
	if first == scheduleTriggerID("team-2", 1790000000) {
		t.Fatal("不同团队的 triggerID 必须互异")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("triggerID 应为合法 UUID: %v", err)
	}
}

func TestNextScheduleDue(t *testing.T) {
	now := scheduleTime(26, 12, 0)
	interval := &TeamSchedule{IntervalSecs: 300}
	if got, ok := nextScheduleDue(interval, now); !ok || !got.Equal(now.Add(300*time.Second)) {
		t.Fatalf("间隔模式的下一到期点应为 now+interval: %v %v", got, ok)
	}
	cron := &TeamSchedule{Cron: "0 9 * * *"}
	if got, ok := nextScheduleDue(cron, now); !ok || !got.Equal(scheduleTime(27, 9, 0)) {
		t.Fatalf("cron 模式的下一到期点应为下一次匹配: %v %v", got, ok)
	}
	if got, ok := nextScheduleDue(&TeamSchedule{Cron: "0 0 30 2 *"}, now); ok {
		t.Fatalf("不可满足 cron 应无下一到期点: %v", got)
	}
	if got, ok := nextScheduleDue(&TeamSchedule{}, now); ok {
		t.Fatalf("残缺配置（无 cron 无间隔）应无下一到期点: %v", got)
	}
}

func TestValidateSchedule(t *testing.T) {
	valid := []*TeamSchedule{
		{TeamID: "t", Goal: "每日站会摘要", IntervalSecs: MinScheduleIntervalSecs},
		{TeamID: "t", Goal: "每日 9 点跑", Cron: "0 9 * * *"},
		{TeamID: "t", Goal: "带计划", IntervalSecs: MinScheduleIntervalSecs,
			Mode: ModePipeline, CollectPolicy: RunCollectAll,
			VerifyCommand: []string{"make", "check"}, Plan: pipelinePlan()},
	}
	for i, schedule := range valid {
		if err := ValidateSchedule(schedule); err != nil {
			t.Fatalf("第 %d 份合法配置被拒: %v", i+1, err)
		}
	}
	invalid := []struct {
		name     string
		schedule *TeamSchedule
		wantErr  string
	}{
		{"缺 teamId", &TeamSchedule{Goal: "g", IntervalSecs: 300}, "teamId"},
		{"缺 goal", &TeamSchedule{TeamID: "t", IntervalSecs: 300}, "目标"},
		{"间隔低于生产下限", &TeamSchedule{TeamID: "t", Goal: "g", IntervalSecs: 299}, "至少 300 秒"},
		{"间隔与 cron 同时携带", &TeamSchedule{TeamID: "t", Goal: "g", IntervalSecs: 300, Cron: "0 9 * * *"}, "二选一"},
		{"cron 非法", &TeamSchedule{TeamID: "t", Goal: "g", Cron: "0 9 * * * *"}, "5 个字段"},
		{"mode 非法", &TeamSchedule{TeamID: "t", Goal: "g", IntervalSecs: 300, Mode: "turbo"}, "未知运行模式"},
		{"collect 非法", &TeamSchedule{TeamID: "t", Goal: "g", IntervalSecs: 300, CollectPolicy: "some"}, "未知收集策略"},
		{"闸门命令不在白名单", &TeamSchedule{TeamID: "t", Goal: "g", IntervalSecs: 300,
			VerifyCommand: []string{"sh", "-c", "make"}}, "不在允许清单内"},
	}
	for _, testCase := range invalid {
		err := ValidateSchedule(testCase.schedule)
		if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
			t.Fatalf("%s: 应报含 %q 的错误，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
}

// ---- ScheduleStore 账本 ----

func TestScheduleStoreLedger(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team-schedules.json")
	store := LoadScheduleStore(path)
	schedule := &TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "每日摘要",
		Mode: ModePipeline, CollectPolicy: RunCollectAll,
		VerifyCommand:    []string{"make", "check"},
		Plan:             pipelinePlan(),
		NextDue:          scheduleTime(27, 9, 0),
		PendingTriggerID: "trig-pending",
		TriggerFailures:  2,
	}
	if err := store.SaveSchedule(schedule); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if schedule.Version != 1 {
		t.Fatalf("新条目版本应为 1，实际 %d", schedule.Version)
	}
	// 正常更新：按账本最新版本改写并落盘。
	fresh := store.Schedule("team-1")
	fresh.Enabled = false
	fresh.LastRunID = "run-9"
	fresh.LastRunAt = scheduleTime(27, 9, 0)
	if err := store.SaveSchedule(fresh); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	// 乐观版本：拿更新前的克隆（版本已过期）回写必须被拒。
	stale := store.Schedule("team-1")
	stale.Version--
	stale.Goal = "过期写"
	if err := store.SaveSchedule(stale); err == nil || !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本重放应报版本冲突，实际 %v", err)
	}
	// 落盘重载：全字段（含计划深层依赖、时间、标记）往返。
	reloaded := LoadScheduleStore(path)
	got := reloaded.Schedule("team-1")
	if got == nil {
		t.Fatal("重载后条目丢失")
	}
	if got.Enabled {
		t.Fatal("enabled 往返失真")
	}
	if got.IntervalSecs != 300 || got.Goal != "每日摘要" || got.Mode != ModePipeline ||
		got.CollectPolicy != RunCollectAll || got.LastRunID != "run-9" ||
		!got.LastRunAt.Equal(scheduleTime(27, 9, 0)) || !got.NextDue.Equal(scheduleTime(27, 9, 0)) ||
		got.PendingTriggerID != "trig-pending" || got.TriggerFailures != 2 {
		t.Fatalf("配置往返失真: %+v", got)
	}
	if len(got.VerifyCommand) != 2 || got.VerifyCommand[1] != "check" {
		t.Fatalf("verifyCommand 往返失真: %v", got.VerifyCommand)
	}
	if len(got.Plan) != 3 || got.Plan[1].DependsOn[0] != "design" {
		t.Fatalf("plan 往返失真: %+v", got.Plan)
	}
	// 深拷贝：改副本不影响账本。
	got.Plan[1].DependsOn[0] = "tampered"
	if store.Schedule("team-1").Plan[1].DependsOn[0] != "design" {
		t.Fatal("Schedule 返回值应是深拷贝")
	}
	// 删除：乐观版本 + 条目消失。
	if err := reloaded.DeleteSchedule("team-1", got.Version-1); err == nil || !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本删除应报冲突，实际 %v", err)
	}
	if err := reloaded.DeleteSchedule("team-1", got.Version); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if reloaded.Schedule("team-1") != nil {
		t.Fatal("删除后条目应消失")
	}
	if again := LoadScheduleStore(path); len(again.Schedules()) != 0 {
		t.Fatal("删除应已落盘")
	}
}

func TestScheduleStoreUnknownFieldsAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "team-schedules.json")
	// 手写账本：携带未知字段与一条 teamId 与键不一致的条目。
	content := `{
  "team-1": {
    "teamId": "team-1", "enabled": true, "intervalSecs": 300, "goal": "g",
    "version": 1, "futureNote": "keep-me"
  },
  "mismatch": {
    "teamId": "other", "enabled": true, "intervalSecs": 300, "goal": "g",
    "version": 1
  }
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadScheduleStore(path)
	if len(store.Schedules()) != 1 {
		t.Fatalf("键不一致条目应跳过，实际 %d 条", len(store.Schedules()))
	}
	// 未知字段经保存往返保留（§8.1）。
	schedule := store.Schedule("team-1")
	schedule.Enabled = false
	if err := store.SaveSchedule(schedule); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "keep-me") {
		t.Fatalf("未知字段应在写盘后保留: %s", raw)
	}
	// 整文件损坏按空账本启动。
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LoadScheduleStore(path); len(got.Schedules()) != 0 {
		t.Fatalf("损坏文件应按空账本启动，实际 %d 条", len(got.Schedules()))
	}
}

// ---- Scheduler：到期触发与抑制 ----

// scheduleFixture 是 sweep 级测试基座：memStore + 真实 journal + 真实
// ScheduleStore（tmpdir）+ 注入 fake CreateRun 与冻结时钟。测试直接调
// sweep（确定性），不经 ticker。
type scheduleFixture struct {
	store     *memStore
	schedules *ScheduleStore
	journal   *Journal
	scheduler *Scheduler
	now       time.Time
	created   []string // 每次调用的 scheduledFrom
	teams     []*Team
}

func newScheduleFixture(t *testing.T) *scheduleFixture {
	t.Helper()
	journal, err := OpenJournal(filepath.Join(t.TempDir(), "team-journal.jsonl"))
	if err != nil {
		t.Fatalf("journal 打开失败: %v", err)
	}
	f := &scheduleFixture{
		store:     newMemStore(),
		schedules: LoadScheduleStore(filepath.Join(t.TempDir(), "team-schedules.json")),
		journal:   journal,
		now:       scheduleTime(26, 12, 0),
	}
	f.scheduler = &Scheduler{
		Schedules: f.schedules,
		Store:     f.store,
		Journal:   f.journal,
		Tick:      time.Hour, // 测试直接调 sweep，tick 不参与
		Now:       func() time.Time { return f.now },
		CreateRun: func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error) {
			f.created = append(f.created, scheduledFrom)
			return &Run{ID: "run-" + strconv.Itoa(len(f.created)), TeamID: team.ID, Goal: goal}, nil
		},
	}
	return f
}

func (f *scheduleFixture) addTeam(t *testing.T, id string) *Team {
	t.Helper()
	team := testTeam()
	team.ID = id
	if err := f.store.SaveTeam(team); err != nil {
		t.Fatalf("team 保存失败: %v", err)
	}
	f.teams = append(f.teams, team)
	return team
}

func (f *scheduleFixture) addRun(t *testing.T, id, teamID, status string) {
	t.Helper()
	run := &Run{ID: id, TeamID: teamID, Goal: "既有 Run", Status: status, Round: 1, PlanHash: "h"}
	if err := f.store.SaveRun(run); err != nil {
		t.Fatalf("run 保存失败: %v", err)
	}
}

func (f *scheduleFixture) sweep() {
	f.scheduler.sweep(context.Background())
}

func (f *scheduleFixture) schedule(t *testing.T, teamID string) *TeamSchedule {
	t.Helper()
	schedule := f.schedules.Schedule(teamID)
	if schedule == nil {
		t.Fatalf("schedule %s 不存在", teamID)
	}
	return schedule
}

func TestSweepFiresDueSchedule(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	due := f.now.Add(-time.Minute)
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "每日摘要",
		NextDue: due,
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("到期应恰好触发一次，实际 %d 次", len(f.created))
	}
	wantTrigger := scheduleTriggerID("team-1", due.Unix())
	if f.created[0] != wantTrigger {
		t.Fatalf("scheduledFrom 应为确定性 triggerID %s，实际 %s", wantTrigger, f.created[0])
	}
	after := f.schedule(t, "team-1")
	if after.PendingTriggerID != "" {
		t.Fatalf("成功后标记应清除，实际 %q", after.PendingTriggerID)
	}
	if after.LastRunID != "run-1" || !after.LastRunAt.Equal(f.now) {
		t.Fatalf("LastRun 簿记缺失: %+v", after)
	}
	if !after.NextDue.Equal(f.now.Add(300 * time.Second)) {
		t.Fatalf("NextDue 应推进到 now+interval，实际 %v", after.NextDue)
	}
	// 未到新到期点：再次 sweep 不触发，也不写盘（版本不变）。
	version := after.Version
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("未到期不应再次触发，实际 %d 次", len(f.created))
	}
	if f.schedule(t, "team-1").Version != version {
		t.Fatal("未到期的 sweep 不应写盘")
	}
}

func TestSweepMissedWindowSingleCatchUp(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	// 停机错过了三个到期点：NextDue 远在过去。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: f.now.Add(-3 * 300 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("错过的多次触发不补跑：至多一次，实际 %d 次", len(f.created))
	}
	after := f.schedule(t, "team-1")
	if !after.NextDue.Equal(f.now.Add(300 * time.Second)) {
		t.Fatalf("NextDue 应重锚到 now+interval（防风暴），实际 %v", after.NextDue)
	}
	// 连续 sweep（模拟更多轮 tick 落在同一时刻）不追发。
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("重锚后不应追发，实际 %d 次", len(f.created))
	}
}

func TestSweepBusyTeamSkipsOccurrence(t *testing.T) {
	for _, status := range []string{RunDispatching, RunCollecting, RunWaitingInput} {
		t.Run(status, func(t *testing.T) {
			f := newScheduleFixture(t)
			f.addTeam(t, "team-1")
			f.addRun(t, "run-live", "team-1", status)
			if err := f.schedules.SaveSchedule(&TeamSchedule{
				TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
				NextDue: f.now.Add(-time.Second),
			}); err != nil {
				t.Fatal(err)
			}
			f.sweep()
			if len(f.created) != 0 {
				t.Fatalf("团队有非终态 Run（%s）时不应触发，实际 %d 次", status, len(f.created))
			}
			after := f.schedule(t, "team-1")
			if after.PendingTriggerID != "" {
				t.Fatalf("跳过的到期点不应留标记，实际 %q", after.PendingTriggerID)
			}
			if !after.NextDue.Equal(f.now.Add(300 * time.Second)) {
				t.Fatalf("跳过的到期点应推进 NextDue（不排队），实际 %v", after.NextDue)
			}
		})
	}
}

func TestSweepNotYetDueAndZeroInit(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	// 未到期：不触发、不写盘。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: f.now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 0 {
		t.Fatalf("未到期不应触发，实际 %d 次", len(f.created))
	}
	// NextDue 零值：首个到期点归一为一个完整周期之后（不立即触发）。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-2", Enabled: true, IntervalSecs: 300, Goal: "g",
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 0 {
		t.Fatalf("零值 NextDue 不应立即触发，实际 %d 次", len(f.created))
	}
	if got := f.schedule(t, "team-2").NextDue; !got.Equal(f.now.Add(300 * time.Second)) {
		t.Fatalf("零值 NextDue 应归一为 now+interval，实际 %v", got)
	}
	// 推进时钟到新到期点后再触发（team-2 即刻补上，到期判定要查团队）。
	f.addTeam(t, "team-2")
	normalizedDue := f.now.Add(300 * time.Second)
	f.now = f.now.Add(301 * time.Second)
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("到达归一后的到期点应触发一次，实际 %d 次", len(f.created))
	}
	if f.created[0] != scheduleTriggerID("team-2", normalizedDue.Unix()) {
		t.Fatalf("触发应发生在归一后的到期点上，实际 %s", f.created[0])
	}
}

func TestSweepDisabledAndMisconfigured(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: false, IntervalSecs: 300, Goal: "g",
		NextDue: f.now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	// 残缺配置（无 cron 无间隔）：跳过、不停用、不触发。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-2", Enabled: true, Goal: "g",
		NextDue: f.now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 0 {
		t.Fatalf("禁用与残缺配置都不应触发，实际 %d 次", len(f.created))
	}
	if got := f.schedule(t, "team-1"); got.Enabled || !got.NextDue.Equal(f.now.Add(-time.Second)) {
		t.Fatalf("禁用配置不应被改动: %+v", got)
	}
	// 不可满足 cron：到期路径将其停用（否则每轮空转）。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-3", Enabled: true, Cron: "0 0 30 2 *", Goal: "g",
		NextDue: f.now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	f.addTeam(t, "team-3")
	f.sweep()
	if got := f.schedule(t, "team-3"); got.Enabled {
		t.Fatal("不可满足 cron 到期后应停用")
	}
}

func TestSweepTeamDeletedDropsSchedule(t *testing.T) {
	f := newScheduleFixture(t)
	// 不加 team：账本里的配置悬空。
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "gone", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: f.now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	f.sweep()
	if len(f.created) != 0 {
		t.Fatalf("团队不存在不应触发，实际 %d 次", len(f.created))
	}
	if f.schedules.Schedule("gone") != nil {
		t.Fatal("悬空配置应被惰性清理")
	}
}

// ---- journal-first 触发协议的崩溃窗口三态 ----

// seedPendingTrigger 构造「崩溃发生在协议步骤①之后」的账本状态：标记已落、
// NextDue 已推进到未来（步骤①与 NextDue 推进是同一次原子写，真实的中间态
// 一定是「标记在、NextDue 在未来」）。
func seedPendingTrigger(t *testing.T, f *scheduleFixture, teamID string) string {
	t.Helper()
	due := f.now.Add(-300 * time.Second)
	triggerID := scheduleTriggerID(teamID, due.Unix())
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: f.now.Add(300 * time.Second), PendingTriggerID: triggerID,
	}); err != nil {
		t.Fatal(err)
	}
	return triggerID
}

func TestTriggerProtocolCrashWindows(t *testing.T) {
	t.Run("journal已有事件_不重发_清标记记账", func(t *testing.T) {
		f := newScheduleFixture(t)
		f.addTeam(t, "team-1")
		triggerID := seedPendingTrigger(t, f, "team-1")
		// 崩溃窗口：CreateRun 已把 run_created 落进 journal、快照与簿记未收尾。
		event := newEvent("run-fired", EventRunCreated, "user", map[string]any{
			"teamId": "team-1", "goal": "g", "scheduledBy": triggerID,
		})
		if err := f.journal.Append(event); err != nil {
			t.Fatal(err)
		}
		f.sweep()
		if len(f.created) != 0 {
			t.Fatalf("journal 已有 scheduledBy 事件的触发不得重发，实际 %d 次", len(f.created))
		}
		after := f.schedule(t, "team-1")
		if after.PendingTriggerID != "" {
			t.Fatalf("回扫后标记应清除，实际 %q", after.PendingTriggerID)
		}
		if after.LastRunID != "run-fired" {
			t.Fatalf("LastRunID 应取 journal 事件的 runId，实际 %q", after.LastRunID)
		}
		// 幂等：再次 sweep 无事发生。
		f.sweep()
		if len(f.created) != 0 || f.schedule(t, "team-1").PendingTriggerID != "" {
			t.Fatal("回扫收尾应幂等")
		}
	})
	t.Run("journal无事件_provably未触发_重执行", func(t *testing.T) {
		f := newScheduleFixture(t)
		f.addTeam(t, "team-1")
		triggerID := seedPendingTrigger(t, f, "team-1")
		f.sweep()
		if len(f.created) != 1 || f.created[0] != triggerID {
			t.Fatalf("无事件应按原 triggerID 重执行一次，实际 %v", f.created)
		}
		after := f.schedule(t, "team-1")
		if after.PendingTriggerID != "" || after.LastRunID != "run-1" {
			t.Fatalf("重执行后应清标记记账: %+v", after)
		}
		f.sweep()
		if len(f.created) != 1 {
			t.Fatalf("重执行成功后不得再次触发，实际 %d 次", len(f.created))
		}
	})
	t.Run("半写_快照丢失但journal已落_不重发", func(t *testing.T) {
		f := newScheduleFixture(t)
		f.addTeam(t, "team-1")
		triggerID := seedPendingTrigger(t, f, "team-1")
		// 半写：journal 有 run_created（ Append 已成、SaveRun 未成——store 无该
		// run）。裁决只认 journal，绝不按 store 快照判断「未触发」。
		event := newEvent("run-lost", EventRunCreated, "user", map[string]any{
			"teamId": "team-1", "goal": "g", "scheduledBy": triggerID,
		})
		if err := f.journal.Append(event); err != nil {
			t.Fatal(err)
		}
		f.sweep()
		if len(f.created) != 0 {
			t.Fatalf("快照丢失不得判为未触发而重发，实际 %d 次", len(f.created))
		}
		if got := f.schedule(t, "team-1"); got.PendingTriggerID != "" || got.LastRunID != "run-lost" {
			t.Fatalf("半写状态应按 journal 已生效收尾: %+v", got)
		}
	})
	t.Run("禁用不撤销已提交的触发", func(t *testing.T) {
		f := newScheduleFixture(t)
		f.addTeam(t, "team-1")
		triggerID := seedPendingTrigger(t, f, "team-1")
		fresh := f.schedule(t, "team-1")
		fresh.Enabled = false
		if err := f.schedules.SaveSchedule(fresh); err != nil {
			t.Fatal(err)
		}
		f.sweep()
		if len(f.created) != 1 || f.created[0] != triggerID {
			t.Fatalf("已提交的触发应不因禁用而中止，实际 %v", f.created)
		}
	})
	t.Run("团队忙时挂起_空闲后续跑", func(t *testing.T) {
		f := newScheduleFixture(t)
		f.addTeam(t, "team-1")
		triggerID := seedPendingTrigger(t, f, "team-1")
		f.addRun(t, "run-live", "team-1", RunWaitingInput)
		f.sweep()
		if len(f.created) != 0 {
			t.Fatalf("团队忙时 pending 触发应挂起，实际 %d 次", len(f.created))
		}
		if got := f.schedule(t, "team-1").PendingTriggerID; got != triggerID {
			t.Fatalf("挂起的触发应保留标记，实际 %q", got)
		}
		// 用户处理完驻留 Run（终态）后，下一轮 sweep 续跑。
		fresh := f.store.Run("run-live")
		fresh.Status = RunStopped
		if err := f.store.SaveRun(fresh); err != nil {
			t.Fatal(err)
		}
		f.sweep()
		if len(f.created) != 1 || f.created[0] != triggerID {
			t.Fatalf("空闲后应续跑 pending 触发，实际 %v", f.created)
		}
	})
}

// ---- 触发失败预算：确定性 CreateRun 失败不得把标记楔死 ----

// TestTriggerFailureBudgetAbandonsWedgedTrigger 复现楔死场景：CreateRun 每次
// 确定性失败（如成员被移除后 ParsePlan 报「任务引用的成员不存在」）。无预算
// 时标记永不清除——每 tick 全量重放 journal 并重试，schedule 从此不再触发；
// 有预算时达到 budget 次后由回扫放弃该触发点，账本回到正常到期路径，配置
// 修复后的下一到期点自愈。
func TestTriggerFailureBudgetAbandonsWedgedTrigger(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	due := f.now.Add(-time.Minute)
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: due,
	}); err != nil {
		t.Fatal(err)
	}
	f.scheduler.CreateRun = func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error) {
		f.created = append(f.created, scheduledFrom)
		return nil, errors.New("任务 lead 引用了不存在的成员：gone")
	}
	wantTrigger := scheduleTriggerID("team-1", due.Unix())
	// 预算内：到期首发 + 回扫重试，每轮恰好一次，triggerID 恒定（标记在）。
	for i := 0; i < scheduleTriggerFailureBudget; i++ {
		f.sweep()
		if len(f.created) != i+1 {
			t.Fatalf("第 %d 轮 sweep 后应已尝试 %d 次，实际 %d 次", i+1, i+1, len(f.created))
		}
		if f.created[i] != wantTrigger {
			t.Fatalf("预算内重试应沿用同一 triggerID，第 %d 次 %s", i+1, f.created[i])
		}
	}
	if got := f.schedule(t, "team-1"); got.PendingTriggerID != wantTrigger || got.TriggerFailures != scheduleTriggerFailureBudget {
		t.Fatalf("预算耗尽前应保留标记并计数到位: %+v", got)
	}
	// 下一轮：回扫确认 journal 无该事件 → 放弃触发点（清标记归零），不再
	// 调用 CreateRun；NextDue 保持步骤①推进的值。
	f.sweep()
	if len(f.created) != scheduleTriggerFailureBudget {
		t.Fatalf("预算耗尽后不得继续重试，实际 %d 次", len(f.created))
	}
	after := f.schedule(t, "team-1")
	if after.PendingTriggerID != "" || after.TriggerFailures != 0 {
		t.Fatalf("放弃触发点应清标记并归零计数: %+v", after)
	}
	if !after.NextDue.Equal(f.now.Add(300*time.Second)) {
		t.Fatalf("放弃后 NextDue 应保持已推进值，实际 %v", after.NextDue)
	}
	// 故障修复（CreateRun 恢复成功）+ 时钟推进到新到期点：schedule 恢复
	// 触发——楔死不再永久。
	f.scheduler.CreateRun = func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error) {
		f.created = append(f.created, scheduledFrom)
		return &Run{ID: "run-fixed", TeamID: team.ID, Goal: goal}, nil
	}
	nextDue := f.now.Add(300 * time.Second)
	f.now = f.now.Add(301 * time.Second)
	f.sweep()
	if len(f.created) != scheduleTriggerFailureBudget+1 {
		t.Fatalf("修复后新到期点应恢复触发，实际共 %d 次", len(f.created))
	}
	if got := f.created[len(f.created)-1]; got != scheduleTriggerID("team-1", nextDue.Unix()) {
		t.Fatalf("新触发点应使用新 triggerID，实际 %s", got)
	}
	if got := f.schedule(t, "team-1"); got.PendingTriggerID != "" || got.LastRunID != "run-fixed" {
		t.Fatalf("恢复触发后应收尾清标记记账: %+v", got)
	}
}

// TestTriggerFailureBudgetSparesJournalConfirmedTrigger 验证放弃裁决后置于
// journal 事实之后：预算耗尽当次 CreateRun 恰好半写（journal 已落
// run_created 但 CreateRun 报错——accountCreatedRun 的 TOCTOU/SaveRun 失败
// 窗口）时，下一轮回扫必须按「已生效」收尾，绝不按预算放弃。
func TestTriggerFailureBudgetSparesJournalConfirmedTrigger(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	due := f.now.Add(-time.Minute)
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: due,
	}); err != nil {
		t.Fatal(err)
	}
	halfWrite := func(scheduledFrom string) {
		t.Helper()
		if err := f.journal.Append(newEvent("run-half", EventRunCreated, "user", map[string]any{
			"teamId": "team-1", "goal": "g", "scheduledBy": scheduledFrom,
		})); err != nil {
			t.Fatal(err)
		}
	}
	// 前 budget-1 次：干净失败（journal 无事件）；最后一次：半写 + 报错。
	f.scheduler.CreateRun = func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error) {
		f.created = append(f.created, scheduledFrom)
		if len(f.created) == scheduleTriggerFailureBudget {
			halfWrite(scheduledFrom)
		}
		return nil, errors.New("快照落账失败")
	}
	for i := 0; i < scheduleTriggerFailureBudget; i++ {
		f.sweep()
	}
	if len(f.created) != scheduleTriggerFailureBudget {
		t.Fatalf("预算内应恰好尝试 %d 次，实际 %d 次", scheduleTriggerFailureBudget, len(f.created))
	}
	// 下一轮：journal 已有 scheduledBy 事件 → 按「已生效」收尾（清标记、
	// 记账、归零计数），不重发也不放弃。
	f.sweep()
	if len(f.created) != scheduleTriggerFailureBudget {
		t.Fatalf("journal 已确认的触发不得重发或因预算放弃，实际 %d 次", len(f.created))
	}
	got := f.schedule(t, "team-1")
	if got.PendingTriggerID != "" || got.TriggerFailures != 0 {
		t.Fatalf("半写触发应按 journal 已生效收尾: %+v", got)
	}
	if got.LastRunID != "run-half" {
		t.Fatalf("LastRunID 应取 journal 事件的 runId，实际 %q", got.LastRunID)
	}
}

func TestSweepMarkerSaveConflictRetriesSameTrigger(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	due := f.now.Add(-time.Minute)
	if err := f.schedules.SaveSchedule(&TeamSchedule{
		TeamID: "team-1", Enabled: true, IntervalSecs: 300, Goal: "g",
		NextDue: due,
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟步骤①落账撞上外部写者：用陈旧版本克隆预先抢占账本（版本前移），
	// 迫使 sweep 的第一次保存版本冲突；下一轮 sweep 以盘上最新状态重算，
	// NextDue 未推进 ⇒ 同一 dueUnix ⇒ 同一 triggerID。
	fresh := f.schedule(t, "team-1")
	fresh.Goal = "外部改写"
	if err := f.schedules.SaveSchedule(fresh); err != nil {
		t.Fatal(err)
	}
	stale := f.schedule(t, "team-1")
	stale.Version-- // 回退一版模拟并发窗口中的陈旧读
	f.scheduler.processSchedule(stale, f.now)
	if len(f.created) != 0 {
		t.Fatalf("版本冲突的触发不应创建 Run，实际 %d 次", len(f.created))
	}
	// 盘上 NextDue 仍为原到期点（冲突写未生效）。
	if got := f.schedule(t, "team-1"); !got.NextDue.Equal(due) {
		t.Fatalf("冲突写不应推进 NextDue，实际 %v", got.NextDue)
	}
	f.sweep()
	if len(f.created) != 1 {
		t.Fatalf("下轮应以同 dueUnix 重算同 triggerID 完成触发，实际 %d 次", len(f.created))
	}
	if f.created[0] != scheduleTriggerID("team-1", due.Unix()) {
		t.Fatalf("重试的 triggerID 应与首算一致，实际 %s", f.created[0])
	}
}

// ---- Start/Stop 同步退出 ----

func TestSchedulerStartStopSynchronous(t *testing.T) {
	f := newScheduleFixture(t)
	f.addTeam(t, "team-1")
	f.scheduler.Tick = 5 * time.Millisecond
	f.scheduler.Start()
	f.scheduler.mu.Lock()
	done := f.scheduler.done
	f.scheduler.mu.Unlock()
	if done == nil {
		t.Fatal("Start 后应有 done 通道")
	}
	f.scheduler.Stop()
	select {
	case <-done:
		// Stop 返回即循环退出：done 已关闭。
	default:
		t.Fatal("Stop 返回后 done 必须已关闭（同步退出语义）")
	}
	// 再启停一轮可重入；禁用（Tick≤0）时 Start 为 no-op 且 Stop 安全。
	f.scheduler.Start()
	f.scheduler.Stop()
	disabled := &Scheduler{Schedules: f.schedules, Store: f.store, Journal: f.journal}
	disabled.Start()
	disabled.Stop()
	if disabled.cancel != nil {
		t.Fatal("禁用的 Scheduler 不应启动")
	}
	// 未启动即 Stop 也安全。
	(&Scheduler{}).Stop()
}

func TestNewSchedulerTickEnv(t *testing.T) {
	dir := t.TempDir()
	newWithEnv := func(value string) *Scheduler {
		t.Setenv("GATEWAY_TEAM_SCHEDULE_TICK_SECONDS", value)
		return NewScheduler(LoadScheduleStore(filepath.Join(dir, "s.json")), newMemStore(), nil, nil)
	}
	if got := newWithEnv(""); got.Tick != defaultScheduleTick {
		t.Fatalf("缺省应为 60s，实际 %v", got.Tick)
	}
	if got := newWithEnv("abc"); got.Tick != defaultScheduleTick {
		t.Fatalf("非法值应取缺省，实际 %v", got.Tick)
	}
	if got := newWithEnv("0"); got.Tick != defaultScheduleTick {
		t.Fatalf("0 应取缺省，实际 %v", got.Tick)
	}
	if got := newWithEnv("5"); got.Tick != 5*time.Second {
		t.Fatalf("正整数应为秒数，实际 %v", got.Tick)
	}
	if got := newWithEnv("-1"); got.Tick != 0 {
		t.Fatalf("负值应禁用（Start no-op），实际 %v", got.Tick)
	}
}

// ---- ScheduledFrom 的四处同步（autoPlan 先例的硬纪律）----

func TestRunScheduledFromJSONRoundTrip(t *testing.T) {
	run := &Run{
		ID: "run-sch", TeamID: "team-1", Goal: "g", Status: RunCompleted,
		Round: 1, PlanHash: "h", ScheduledFrom: "trig-1",
	}
	raw, err := run.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"scheduledFrom":"trig-1"`) {
		t.Fatalf("序列化应携带 scheduledFrom: %s", raw)
	}
	var decoded Run
	if err := decoded.UnmarshalJSON(raw); err != nil {
		t.Fatal(err)
	}
	if decoded.ScheduledFrom != "trig-1" {
		t.Fatalf("反序列化丢失 scheduledFrom: %+v", decoded)
	}
	// Clone 深拷贝保留（字符串随结构体拷贝，与 ParentRunID 同款）。
	if cloned := run.Clone(); cloned.ScheduledFrom != "trig-1" {
		t.Fatalf("Clone 丢失 scheduledFrom: %+v", cloned)
	}
	// 未知字段保留不受影响（knownFields 增键不破坏 extras 往返）。
	withExtra := []byte(`{"id":"r","teamId":"t","goal":"g","status":"created","round":1,"planHash":"h","scheduledFrom":"s","futureField":42}`)
	var future Run
	if err := future.UnmarshalJSON(withExtra); err != nil {
		t.Fatal(err)
	}
	again, err := future.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(again), `"futureField":42`) || !strings.Contains(string(again), `"scheduledFrom":"s"`) {
		t.Fatalf("未知字段与 scheduledFrom 应同时保留: %s", again)
	}
	// 已知字段不再是 extras：collectExtras 不应把 scheduledFrom 当未知键。
	if _, ok := future.extra.MustGet("scheduledFrom").(string); ok {
		t.Fatal("scheduledFrom 应登记进 runKnownFields")
	}
}

func TestRebuildRunsFromJournalBackfillsScheduledFrom(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(testTeam()); err != nil {
		t.Fatal(err)
	}
	// team-runs.json 整文件损坏（重建路径的唯一入口），journal 里已有带
	// scheduledBy 的 run_created。
	if err := os.WriteFile(runsPath, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal, err := OpenJournal(filepath.Join(dir, "team-journal.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(newEvent("run-sch", EventRunCreated, "user", map[string]any{
		"teamId": "team-1", "goal": "定时目标", "scheduledBy": "trig-rebuild",
	})); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if !store.RunsLedgerCorrupt() {
		t.Fatal("损坏的 runs 账本应置 RunsLedgerCorrupt")
	}
	events, err := journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	store.RebuildRunsFromJournal(events)
	rebuilt := store.Run("run-sch")
	if rebuilt == nil {
		t.Fatal("重建后 run 应存在")
	}
	if rebuilt.ScheduledFrom != "trig-rebuild" {
		t.Fatalf("重建应从 payload.scheduledBy 回填 ScheduledFrom，实际 %q", rebuilt.ScheduledFrom)
	}
	// 落盘后再重载仍在（jsonObject/knownFields 两处同步）。
	reloaded := LoadStore(teamsPath, runsPath)
	if got := reloaded.Run("run-sch"); got == nil || got.ScheduledFrom != "trig-rebuild" {
		t.Fatalf("重建结果落盘后 scheduledFrom 丢失: %+v", got)
	}
}

// ---- 真实 Service 链路（触发 → CreateRun → journal/快照打标）----

func TestSchedulerFiresThroughRealService(t *testing.T) {
	h := newDispatchHarness(t)
	schedules := LoadScheduleStore(filepath.Join(t.TempDir(), "team-schedules.json"))
	due := time.Now().Add(-time.Minute)
	if err := schedules.SaveSchedule(&TeamSchedule{
		TeamID: h.team.ID, Enabled: true, IntervalSecs: 300, Goal: "定时产出摘要",
		NextDue: due,
	}); err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{
		Schedules: schedules,
		Store:     h.store,
		Journal:   h.journal,
		CreateRun: func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error) {
			return h.service.CreateRun(team, "", goal, mode, collect, verifyCommand, plan, false, scheduledFrom)
		},
	}
	scheduler.sweep(context.Background())
	after := schedules.Schedule(h.team.ID)
	if after == nil || after.LastRunID == "" {
		t.Fatalf("触发应记账 LastRunID: %+v", after)
	}
	run := h.store.Run(after.LastRunID)
	if run == nil {
		t.Fatal("触发的 Run 应已入账")
	}
	wantTrigger := scheduleTriggerID(h.team.ID, due.Unix())
	if run.ScheduledFrom != wantTrigger {
		t.Fatalf("Run.ScheduledFrom 应为 triggerID，实际 %q", run.ScheduledFrom)
	}
	events, err := h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.RunID == run.ID && event.Type == EventRunCreated {
			if got := payloadString(event.Payload, "scheduledBy"); got == wantTrigger {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("run_created 事件应携带 payload.scheduledBy=triggerID")
	}
	if after.PendingTriggerID != "" {
		t.Fatalf("触发收尾后标记应清除，实际 %q", after.PendingTriggerID)
	}
	// 二次 sweep：该 triggerID 的事实已在 journal，不重发（防重触发回归）。
	scheduler.sweep(context.Background())
	events, err = h.journal.Replay()
	if err != nil {
		t.Fatal(err)
	}
	marked := 0
	for _, event := range events {
		if event.Type == EventRunCreated && payloadString(event.Payload, "scheduledBy") == wantTrigger {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("同 triggerID 的 run_created 应恰好一条（二次 sweep 不得重发），实际 %d", marked)
	}
}
