package team

// schedule.go 是定时触发的数据模型与执行体（docs/team-orchestration-plan.md
// §12 阶段四工作项 1）：Team 级定时配置持久化（team-schedules.json，JSON
// 原子账本），按 cron/间隔自动创建 Run。触发幂等走 journal-first 三步协议
// ——先落 PendingTriggerID 标记、再创建打标 Run（run_created
// payload.scheduledBy）、成功后清标记；崩溃/重启由 journal 回扫裁决（事件
// 已落=触发已生效，哪怕快照丢失；未落=provably 未触发，重执行）。错过的
// 到期点不补跑（停机/团队忙期间错过的多次触发至多补一次，防风暴）。
//
// 边界（§12 阶段四范围）：不做 launchd StartCalendarInterval 集成；cron 是
// 五字段「分 时 日 月 周」最小解析器（不支持 @别名/步进/秒级/英文星期月份
// 名）；不做按 Run 结果条件触发或链式调度；不改 RecoverRun/ReconcileRun
// 语义；不动 HTTP 层幂等件（HTTP 配置面属后续工作项，集成测试用 fixture
// 预置 team-schedules.json 绕开生产下限）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/google/uuid"
)

// MinScheduleIntervalSecs 是间隔模式的生产下限（300s）。env
// GATEWAY_TEAM_SCHEDULE_TICK_SECONDS 只能缩小扫描粒度、不能缩小该下限
// （防误配置把触发器变成高频创建器）；集成测试用 fixture 预置绕开。
const MinScheduleIntervalSecs = 300

// defaultScheduleTick 是定时触发器的默认扫描粒度。
const defaultScheduleTick = 60 * time.Second

// scheduleTriggerFailureBudget 是单个触发点（同一 PendingTriggerID）CreateRun
// 连续失败的预算。CreateRun 可能确定性失败——成员与计划的权威校验在触发时
// 才执行（ParsePlan，见 ValidateSchedule 注释），如配置后成员被移除会每次报
// 「任务引用的成员不存在」；无预算的重试会把标记楔死：每 tick 全量重放
// journal（journal 只增）且 schedule 从此不再触发。达到预算即放弃该触发点
// ——清标记归零计数，NextDue 保持步骤①已推进的值，回到正常到期路径，配置
// 修复后的下一到期点自愈。放弃只发生在回扫确认 journal 无该触发事件之后：
// 半写（journal 已落 run_created 但 CreateRun 报错）的触发按「已生效」收尾，
// 绝不误放弃。
const scheduleTriggerFailureBudget = 3

// TeamSchedule 是一个 Team 的定时触发配置（§12 阶段四工作项 1）。一队一份，
// 键=TeamID。Cron 与 IntervalSecs 二选一（ValidateSchedule 创建侧裁决；
// 调度侧 Cron 非空优先）。NextDue/PendingTriggerID/LastRun* 是簿记字段，
// 与配置同文件持久化——触发协议的崩溃窗口依赖「标记先于创建落账」的单次
// 原子写。extra 保留账本未知字段（§8.1）。
type TeamSchedule struct {
	TeamID       string `json:"teamId"`
	Enabled      bool   `json:"enabled"`
	IntervalSecs int    `json:"intervalSecs,omitempty"`
	// Cron 是五字段「分 时 日 月 周」表达式（本地时区；不支持 @别名/步进/
	// 秒级），与 IntervalSecs 二选一。
	Cron          string          `json:"cron,omitempty"`
	Goal          string          `json:"goal"`
	Mode          string          `json:"mode,omitempty"`
	CollectPolicy string          `json:"collectPolicy,omitempty"`
	VerifyCommand []string        `json:"verifyCommand,omitempty"`
	Plan          []PlanTaskInput `json:"plan,omitempty"`
	// LastRunAt/LastRunID 是最近一次成功触发的簿记（回扫收尾时 LastRunAt
	// 取对账时刻——journal 事件 createdAt 运行期为零值，无从取事件时间）。
	LastRunAt time.Time `json:"lastRunAt,omitempty"`
	LastRunID string    `json:"lastRunId,omitempty"`
	// NextDue 是下一个到期点；零值在首次扫描时归一为一个完整周期之后
	//（创建即触发会让间隔语义退化为「立即触发+间隔」）。
	NextDue time.Time `json:"nextDue,omitempty"`
	// PendingTriggerID 是 journal-first 协议的进行中触发标记：触发前原子
	// 落账（与推进后的 NextDue 同一次写），创建成功或回扫确认后清除。
	// 非空时该 schedule 的本个触发点已提交，重启后按 journal 裁决收尾。
	PendingTriggerID string `json:"pendingTriggerId,omitempty"`
	// TriggerFailures 是当前标记对应触发点的连续创建失败计数（预算
	// scheduleTriggerFailureBudget）：CreateRun 报错 +1；成功、回扫确认
	// （journal 有事件）或预算放弃时归零；新触发点落标记时重置。与标记
	// 同账本持久化，重启不重置。
	TriggerFailures int    `json:"triggerFailures,omitempty"`
	Version         uint64 `json:"version"`

	extra *jsoncompat.Object
}

// ValidateSchedule 是创建/更新侧的纯校验（未来 HTTP 配置面直接复用）：
//   - IntervalSecs 与 Cron 二选一：间隔模式须 ≥ MinScheduleIntervalSecs；
//     cron 模式须过最小解析器；
//   - goal 非空；mode/collectPolicy 取值合法；verifyCommand 过闸门白名单。
//
// 成员与计划的合法性不在配置时刻校验——成员表随时可变，权威校验在触发时
// 的 ParsePlan/CreateRun 全量执行。
func ValidateSchedule(schedule *TeamSchedule) error {
	if schedule == nil {
		return errors.New("schedule 不能为 nil")
	}
	if schedule.TeamID == "" {
		return errors.New("schedule 缺少 teamId")
	}
	if strings.TrimSpace(schedule.Goal) == "" {
		return errors.New("请填写定时运行目标")
	}
	switch {
	case schedule.Cron != "":
		if schedule.IntervalSecs > 0 {
			return errors.New("intervalSecs 与 cron 二选一")
		}
		if _, err := parseCronSpec(schedule.Cron); err != nil {
			return err
		}
	default:
		if schedule.IntervalSecs < MinScheduleIntervalSecs {
			return fmt.Errorf("定时间隔至少 %d 秒", MinScheduleIntervalSecs)
		}
	}
	switch schedule.Mode {
	case "", ModeSingle, ModePipeline, ModeBroadcast:
	default:
		return fmt.Errorf("未知运行模式：%s", schedule.Mode)
	}
	switch schedule.CollectPolicy {
	case "", RunCollectAll, RunCollectFirstSuccess:
	default:
		return fmt.Errorf("未知收集策略：%s（可用：all、first_success）", schedule.CollectPolicy)
	}
	if len(schedule.VerifyCommand) > 0 {
		if err := ValidateVerifyCommand(schedule.VerifyCommand); err != nil {
			return err
		}
	}
	return nil
}

// ---- TeamSchedule 的未知字段保留（§8.1，照 Team/Run 先例）----

var teamScheduleKnownFields = map[string]bool{
	"teamId": true, "enabled": true, "intervalSecs": true, "cron": true,
	"goal": true, "mode": true, "collectPolicy": true, "verifyCommand": true,
	"plan": true, "lastRunAt": true, "lastRunId": true, "nextDue": true,
	"pendingTriggerId": true, "triggerFailures": true, "version": true,
}

// UnmarshalJSON 解码 TeamSchedule 并保留未知字段。
func (s *TeamSchedule) UnmarshalJSON(data []byte) error {
	type scheduleAlias TeamSchedule
	var decoded scheduleAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	extra, err := collectExtras(data, teamScheduleKnownFields)
	if err != nil {
		return err
	}
	*s = TeamSchedule(decoded)
	s.extra = extra
	return nil
}

// MarshalJSON 按声明顺序输出已知字段，未知字段原样回写在末尾。
func (s *TeamSchedule) MarshalJSON() ([]byte, error) {
	return jsoncompat.EncodeCompact(s.jsonObject()), nil
}

func (s *TeamSchedule) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("teamId", s.TeamID)
	object.Set("enabled", s.Enabled)
	if s.IntervalSecs > 0 {
		object.Set("intervalSecs", jsoncompat.Num(s.IntervalSecs))
	}
	if s.Cron != "" {
		object.Set("cron", s.Cron)
	}
	object.Set("goal", s.Goal)
	if s.Mode != "" {
		object.Set("mode", s.Mode)
	}
	if s.CollectPolicy != "" {
		object.Set("collectPolicy", s.CollectPolicy)
	}
	if len(s.VerifyCommand) > 0 {
		command := &jsoncompat.Array{}
		for _, part := range s.VerifyCommand {
			command.Items = append(command.Items, part)
		}
		object.Set("verifyCommand", command)
	}
	if len(s.Plan) > 0 {
		plan := &jsoncompat.Array{}
		for i := range s.Plan {
			plan.Items = append(plan.Items, s.Plan[i].jsonObject())
		}
		object.Set("plan", plan)
	}
	if !s.LastRunAt.IsZero() {
		object.Set("lastRunAt", s.LastRunAt.Format(time.RFC3339Nano))
	}
	if s.LastRunID != "" {
		object.Set("lastRunId", s.LastRunID)
	}
	if !s.NextDue.IsZero() {
		object.Set("nextDue", s.NextDue.Format(time.RFC3339Nano))
	}
	if s.PendingTriggerID != "" {
		object.Set("pendingTriggerId", s.PendingTriggerID)
	}
	if s.TriggerFailures > 0 {
		object.Set("triggerFailures", jsoncompat.Num(s.TriggerFailures))
	}
	object.Set("version", json.Number(strconv.FormatUint(s.Version, 10)))
	mergeExtras(object, s.extra)
	return object
}

// jsonObject 按创建请求的 JSON 键序输出单条计划任务（与 PlanTaskInput 的
// json 标签一一对应）。
func (p *PlanTaskInput) jsonObject() *jsoncompat.Object {
	object := jsoncompat.NewObject()
	object.Set("id", p.ID)
	object.Set("memberId", p.MemberID)
	if len(p.MemberIDs) > 0 {
		members := &jsoncompat.Array{}
		for _, memberID := range p.MemberIDs {
			members.Items = append(members.Items, memberID)
		}
		object.Set("memberIds", members)
	}
	object.Set("prompt", p.Prompt)
	if len(p.DependsOn) > 0 {
		depends := &jsoncompat.Array{}
		for _, id := range p.DependsOn {
			depends.Items = append(depends.Items, id)
		}
		object.Set("dependsOn", depends)
	}
	if p.WriteLock != "" {
		object.Set("writeLock", p.WriteLock)
	}
	if p.WorkspaceMode != "" {
		object.Set("workspaceMode", p.WorkspaceMode)
	}
	if p.WorkspaceID != "" {
		object.Set("workspaceId", p.WorkspaceID)
	}
	return object
}

// Clone 深拷贝 TeamSchedule（含计划、命令与未知字段）。
func (s *TeamSchedule) Clone() *TeamSchedule {
	if s == nil {
		return nil
	}
	copied := *s
	copied.VerifyCommand = append([]string(nil), s.VerifyCommand...)
	copied.Plan = nil
	if len(s.Plan) > 0 {
		copied.Plan = make([]PlanTaskInput, len(s.Plan))
		for i := range s.Plan {
			copied.Plan[i] = s.Plan[i]
			copied.Plan[i].MemberIDs = append([]string(nil), s.Plan[i].MemberIDs...)
			copied.Plan[i].DependsOn = append([]string(nil), s.Plan[i].DependsOn...)
		}
	}
	copied.extra = cloneExtra(s.extra)
	return &copied
}

// ---- cron 最小解析器（五字段「分 时 日 月 周」）----

// cronSpec 是解析后的 cron 匹配表。domRestricted/dowRestricted 记录日/周
// 字段是否显式限定——两者同时限定时按 vixie 语义取 OR（「每月 1 号或每周
// 一」，不是且）。
type cronSpec struct {
	minutes, hours, doms, months, dows map[int]bool
	domRestricted, dowRestricted       bool
}

// parseCronSpec 解析五字段 cron 表达式。仅支持：每字段为 * 或逗号分隔的
// N / N-M 列表；周 0-7（7 归一为周日 0）。不支持 @别名、步进（/）、秒级
// 第六字段、英文月份/星期名（§12 阶段四：最小解析器）。
func parseCronSpec(expr string) (*cronSpec, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron 表达式需要 5 个字段（分 时 日 月 周），收到 %d 个", len(fields))
	}
	spec := &cronSpec{
		minutes: map[int]bool{}, hours: map[int]bool{},
		doms: map[int]bool{}, months: map[int]bool{}, dows: map[int]bool{},
	}
	bounds := []struct {
		name     string
		min, max int
		values   map[int]bool
	}{
		{"分", 0, 59, spec.minutes},
		{"时", 0, 23, spec.hours},
		{"日", 1, 31, spec.doms},
		{"月", 1, 12, spec.months},
		{"周", 0, 7, spec.dows},
	}
	for i, field := range fields {
		bound := bounds[i]
		star, err := parseCronField(field, bound.name, bound.min, bound.max, bound.values)
		if err != nil {
			return nil, err
		}
		switch i {
		case 2:
			spec.domRestricted = !star
		case 4:
			spec.dowRestricted = !star
			if bound.values[7] { // 7=周日，归一到 0 与 * 同键。
				delete(bound.values, 7)
				bound.values[0] = true
			}
		}
	}
	return spec, nil
}

// parseCronField 解析单个字段；返回是否为 *（日/周的 OR 语义需要区分
// 「* 展开」与「显式列出全部取值」）。
func parseCronField(field, name string, min, max int, values map[int]bool) (bool, error) {
	if field == "*" {
		for value := min; value <= max; value++ {
			values[value] = true
		}
		return true, nil
	}
	for _, item := range strings.Split(field, ",") {
		if item == "" {
			return false, fmt.Errorf("cron %s 字段存在空项：%q", name, field)
		}
		if strings.ContainsAny(item, "*/") {
			return false, fmt.Errorf("cron %s 字段不支持步进或通配片段：%q（最小解析器仅支持 *、N、N-M 与逗号列表）", name, item)
		}
		low, high, err := parseCronItem(item, name, min, max)
		if err != nil {
			return false, err
		}
		for value := low; value <= high; value++ {
			values[value] = true
		}
	}
	return false, nil
}

// parseCronItem 解析单项：N 或 N-M（端点含）。
func parseCronItem(item, name string, min, max int) (int, int, error) {
	lowText, highText := item, item
	if dash := strings.IndexByte(item, '-'); dash >= 0 {
		lowText, highText = item[:dash], item[dash+1:]
		if strings.Contains(highText, "-") {
			return 0, 0, fmt.Errorf("cron %s 字段区间含多个连字符：%q", name, item)
		}
	}
	low, err := parseCronNumber(lowText, name, min, max)
	if err != nil {
		return 0, 0, err
	}
	high, err := parseCronNumber(highText, name, min, max)
	if err != nil {
		return 0, 0, err
	}
	if low > high {
		return 0, 0, fmt.Errorf("cron %s 字段区间颠倒：%q", name, item)
	}
	return low, high, nil
}

func parseCronNumber(text, name string, min, max int) (int, error) {
	if text == "" {
		return 0, fmt.Errorf("cron %s 字段存在空数字", name)
	}
	for _, r := range text {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("cron %s 字段含非数字字符：%q", name, text)
		}
	}
	value, err := strconv.Atoi(text)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("cron %s 字段取值越界：%q（可用 %d-%d）", name, text, min, max)
	}
	return value, nil
}

// matchesDay 报告日期部分（月/日/周）是否匹配；日与周同时显式限定时按
// vixie OR 语义。
func (c *cronSpec) matchesDay(t time.Time) bool {
	if !c.months[int(t.Month())] {
		return false
	}
	domOK := c.doms[t.Day()]
	dowOK := c.dows[int(t.Weekday())]
	if c.domRestricted && c.dowRestricted {
		return domOK || dowOK
	}
	return domOK && dowOK
}

// nextAfter 返回严格晚于 after 的下一个匹配时刻（按 after 的时区，生产为
// 本地时区）。按天扫描（上界 4 年覆盖闰年周期），日内按小时/分钟升序取
// 最早；无可到达时刻（如 "0 0 30 2 *"）返回 (零值, false)。夏令时切换日
// 的不存在时刻由 time.Date 归一（最小解析器不额外处理）。
func (c *cronSpec) nextAfter(after time.Time) (time.Time, bool) {
	hours := sortedInts(c.hours)
	minutes := sortedInts(c.minutes)
	location := after.Location()
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, location)
	limit := day.AddDate(4, 0, 0)
	for day.Before(limit) {
		if c.matchesDay(day) {
			for _, hour := range hours {
				for _, minute := range minutes {
					at := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, location)
					if at.After(after) {
						return at, true
					}
				}
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}, false
}

func sortedInts(values map[int]bool) []int {
	sorted := make([]int, 0, len(values))
	for value := range values {
		sorted = append(sorted, value)
	}
	sort.Ints(sorted)
	return sorted
}

// ---- ScheduleStore：team-schedules.json 原子账本 ----

// ScheduleStore 是定时配置的持久化账本，模式与 teams.json 相同（§8.1：
// 内存 map + 插入序、唯一临时文件名 + rename 原子替换、乐观版本、未知字段
// 往返）。损坏按空账本启动（打警告留痕）——与 team-runs.json 不同，本账本
// 是可重建的配置而非运行事实，无 journal 重建路径，也不改名留存原件。
type ScheduleStore struct {
	mu      sync.Mutex
	path    string
	order   []string
	entries map[string]*TeamSchedule
}

// LoadScheduleStore 读取 team-schedules.json；文件不存在/损坏按空账本启动，
// 单条目损坏只跳过该条目，条目 teamId 与键不一致同样跳过。
func LoadScheduleStore(path string) *ScheduleStore {
	store := &ScheduleStore{path: path, entries: map[string]*TeamSchedule{}}
	object, ok := readLedgerObject(path, "team-schedules.json")
	if !ok {
		return store
	}
	for _, key := range object.Keys() {
		raw, _ := object.Get(key)
		var schedule TeamSchedule
		if err := json.Unmarshal(jsoncompat.EncodeCompact(raw), &schedule); err != nil {
			log.Printf("team-schedules.json 条目解析失败，已跳过: key=%s err=%v", key, err)
			continue
		}
		if schedule.TeamID == "" || schedule.TeamID != key {
			log.Printf("team-schedules.json 条目缺少 teamId 或与键不一致，已跳过: key=%s teamId=%s", key, schedule.TeamID)
			continue
		}
		store.entries[key] = &schedule
		store.order = append(store.order, key)
	}
	return store
}

// Schedule 返回 teamID 的配置深拷贝；不存在返回 nil。
func (s *ScheduleStore) Schedule(teamID string) *TeamSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[teamID].Clone()
}

// Schedules 按插入序返回全部配置副本。
func (s *ScheduleStore) Schedules() []*TeamSchedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	schedules := make([]*TeamSchedule, 0, len(s.order))
	for _, id := range s.order {
		if schedule := s.entries[id]; schedule != nil {
			schedules = append(schedules, schedule.Clone())
		}
	}
	return schedules
}

// SaveSchedule 以乐观版本写入配置（键=TeamID，一队一份）：新条目期望
// Version==0、更新期望与账本一致、成功后 +1，语义同 SaveTeam。不做配置
// 校验（ValidateSchedule 是创建侧的事；本账本还要承载簿记回写与集成测试
// 的 fixture 预置）。
func (s *ScheduleStore) SaveSchedule(schedule *TeamSchedule) error {
	if schedule == nil {
		return errors.New("schedule 不能为 nil")
	}
	if schedule.TeamID == "" {
		return errors.New("schedule 缺少 teamId")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.entries[schedule.TeamID]
	var storedVersion uint64
	if exists {
		storedVersion = existing.Version
	}
	next, err := checkVersion(exists, storedVersion, schedule.Version, "schedule="+schedule.TeamID)
	if err != nil {
		return err
	}
	stored := schedule.Clone()
	stored.Version = next
	dump := s.schedulesDumpWithLocked(schedule.TeamID, stored)
	if err := atomicWriteFile(s.path, append(jsoncompat.EncodePythonIndent2(dump), '\n')); err != nil {
		return fmt.Errorf("team-schedules.json 写入失败: %w", err)
	}
	if !exists {
		s.order = append(s.order, schedule.TeamID)
	}
	s.entries[schedule.TeamID] = stored
	schedule.Version = next
	return nil
}

// DeleteSchedule 以乐观版本删除单个配置。
func (s *ScheduleStore) DeleteSchedule(teamID string, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.entries[teamID]
	if !exists {
		return fmt.Errorf("%w: schedule=%s", ErrNotFound, teamID)
	}
	if version != existing.Version {
		return fmt.Errorf("%w: schedule=%s 期望 version=%d 实际=%d", ErrVersionConflict, teamID, version, existing.Version)
	}
	dump := jsoncompat.NewObject()
	for _, existingID := range s.order {
		if entry := s.entries[existingID]; entry != nil && existingID != teamID {
			dump.Set(existingID, entry.jsonObject())
		}
	}
	if err := atomicWriteFile(s.path, append(jsoncompat.EncodePythonIndent2(dump), '\n')); err != nil {
		return fmt.Errorf("team-schedules.json 写入失败: %w", err)
	}
	delete(s.entries, teamID)
	s.order = removeKey(s.order, teamID)
	return nil
}

// schedulesDumpWithLocked 生成以 substitute 替换（或追加）指定配置后的账本
// 快照；不改动内存状态，落盘失败时内存与磁盘保持一致。
func (s *ScheduleStore) schedulesDumpWithLocked(id string, substitute *TeamSchedule) *jsoncompat.Object {
	dump := jsoncompat.NewObject()
	inserted := false
	for _, existingID := range s.order {
		schedule := s.entries[existingID]
		if schedule == nil {
			continue
		}
		if existingID == id {
			dump.Set(existingID, substitute.jsonObject())
			inserted = true
			continue
		}
		dump.Set(existingID, schedule.jsonObject())
	}
	if !inserted {
		dump.Set(id, substitute.jsonObject())
	}
	return dump
}

// ---- Scheduler：定时触发执行体 ----

// ScheduleRunCreator 是触发时创建 Run 的端口：生产装配为 Service.CreateRun
// 的闭包（恒传新 runID 与 autoPlan=false，scheduledFrom=triggerID）；测试
// 注入 fake 断言调用。
type ScheduleRunCreator func(team *Team, goal, mode, collect string, verifyCommand []string, plan []PlanTaskInput, scheduledFrom string) (*Run, error)

// Scheduler 按 tick 扫描 enabled schedule，到期且团队无非终态 Run 时创建
// Run（§12 阶段四）。Start/Stop 仿 accpool Poller（ctx+cancel+done 同步
// 退出）；tick 只影响扫描粒度，到期判定以账本 NextDue 为准。
type Scheduler struct {
	Schedules *ScheduleStore
	Store     TeamStore
	Journal   *Journal
	CreateRun ScheduleRunCreator
	// Tick 是扫描粒度（默认 60s；≤0 禁用，Start 为 no-op）。
	Tick time.Duration
	// Now 是可控时钟（§13.1：状态机测试用可控 clock，不用固定 sleep）；
	// nil 表示 time.Now。
	Now func() time.Time

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewScheduler 装配定时触发器，tick 粒度读取 GATEWAY_TEAM_SCHEDULE_TICK_SECONDS
// （缺省/非法/0 → 60s；负值 → 禁用）——env 只缩粒度，间隔下限
// MinScheduleIntervalSecs 由 ValidateSchedule 把守，不受 env 影响。
func NewScheduler(schedules *ScheduleStore, store TeamStore, journal *Journal, createRun ScheduleRunCreator) *Scheduler {
	tick := defaultScheduleTick
	if raw := os.Getenv("GATEWAY_TEAM_SCHEDULE_TICK_SECONDS"); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds < 0 {
			tick = 0
		} else if err == nil && seconds > 0 {
			tick = time.Duration(seconds) * time.Second
		}
	}
	return &Scheduler{
		Schedules: schedules,
		Store:     store,
		Journal:   journal,
		CreateRun: createRun,
		Tick:      tick,
	}
}

// Start 启动触发循环（启动即先扫一轮——首轮 sweep 兼作 PendingTriggerID 的
// 回扫对账，即 §8.3 式的启动恢复）；已启动时 no-op；Tick≤0（禁用）no-op。
func (s *Scheduler) Start() {
	if s == nil || s.Tick <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.loop(ctx, s.done)
}

// Stop 取消循环并等待其退出（同步语义：返回即循环已终止），可再次 Start。
func (s *Scheduler) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *Scheduler) loop(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	s.sweep(ctx)
	ticker := time.NewTicker(s.Tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// sweep 扫一轮全部 schedule；ctx 取消时在条目间尽快返回（Stop 的同步退出
// 依赖此，绝不因长扫描阻塞关停）。
func (s *Scheduler) sweep(ctx context.Context) {
	now := s.now()
	for _, schedule := range s.Schedules.Schedules() {
		if ctx.Err() != nil {
			return
		}
		s.processSchedule(schedule, now)
	}
}

// processSchedule 处理单份配置的一个 tick。进行中的触发（PendingTriggerID
// 非空）优先回扫收尾——触发已提交就必须有个了结，与 Enabled 无关（禁用
// 只挡新触发点，不撤销已提交的触发）。所有簿记写失败都留待下轮：NextDue
// 未推进会以同一 dueUnix 重算出同一 triggerID，天然幂等。
func (s *Scheduler) processSchedule(schedule *TeamSchedule, now time.Time) {
	if schedule.PendingTriggerID != "" {
		s.reconcilePendingTrigger(schedule, now)
		return
	}
	if !schedule.Enabled {
		return
	}
	if schedule.Cron == "" && schedule.IntervalSecs < 1 {
		// 无效配置（创建侧被 ValidateSchedule 拦截；fixture 手写可能绕过）：
		// 无法推导任何到期点，跳过并留痕。
		log.Printf("team schedule %s 缺少 cron 与 intervalSecs，跳过", schedule.TeamID)
		return
	}
	if schedule.NextDue.IsZero() {
		next, ok := nextScheduleDue(schedule, now)
		if !ok {
			s.disableSchedule(schedule, "cron 无可到达的下一个到期点")
			return
		}
		schedule.NextDue = next
		if err := s.Schedules.SaveSchedule(schedule); err != nil {
			log.Printf("team schedule %s 首个到期点落账失败，下轮重试: %v", schedule.TeamID, err)
		}
		return
	}
	if now.Before(schedule.NextDue) {
		return
	}
	// 到期。团队已删 → 惰性清理悬空配置（DeleteTeam 不触碰本账本，由这里
	// 兜底回收）。
	team := s.Store.Team(schedule.TeamID)
	if team == nil {
		s.dropSchedule(schedule, "团队已删除")
		return
	}
	if s.teamBusy(schedule.TeamID) {
		// 错过窗口的显式语义：团队忙（含 waiting_input 驻留——它没有循环
		// 驻留、Resume 语义受限，一律算非终态）时该到期点跳过、不排队——
		// 每份配置至多一个在途触发，避免恢复后连发。
		next, ok := nextScheduleDue(schedule, now)
		if !ok {
			s.disableSchedule(schedule, "cron 无可到达的下一个到期点")
			return
		}
		schedule.NextDue = next
		if err := s.Schedules.SaveSchedule(schedule); err != nil {
			log.Printf("team schedule %s 跳过到期点落账失败，下轮重试: %v", schedule.TeamID, err)
		}
		return
	}
	// 协议步骤①：先原子落账 {PendingTriggerID, NextDue=下一个到期点}——
	// 单次写同时生效，不存在「标记已落而 NextDue 未推进」的中间态。落账
	// 失败则本轮放弃（磁盘 NextDue 未动，下轮以同一 dueUnix 重算出同一
	// triggerID）。
	triggerID := scheduleTriggerID(schedule.TeamID, schedule.NextDue.Unix())
	next, ok := nextScheduleDue(schedule, now)
	if !ok {
		s.disableSchedule(schedule, "cron 无可到达的下一个到期点")
		return
	}
	schedule.PendingTriggerID = triggerID
	schedule.TriggerFailures = 0 // 新触发点：失败计数重置。
	schedule.NextDue = next
	if err := s.Schedules.SaveSchedule(schedule); err != nil {
		log.Printf("team schedule %s 触发标记落账失败，下轮重试: %v", schedule.TeamID, err)
		return
	}
	// 协议步骤②③：创建打标 Run，成功后清标记并记账 LastRun。
	s.fireTrigger(schedule, team, triggerID, now)
}

// fireTrigger 执行协议步骤②③：以 scheduledFrom=triggerID 创建 Run，成功后
// 清 PendingTriggerID 并记 LastRunID/LastRunAt。失败保留标记并累加
// TriggerFailures——下轮 sweep 的回扫对账按 journal 事实裁决（事件已落=
// 触发已生效；未落=provably 未触发，预算未尽重执行、预算尽放弃该触发点）。
// mode 缺省按计划形状推断（与 HTTP 创建同款：多条任务即流水线，否则单任务）。
func (s *Scheduler) fireTrigger(schedule *TeamSchedule, team *Team, triggerID string, now time.Time) {
	if s.teamBusy(schedule.TeamID) {
		return // 团队此刻出现非终态 Run：触发挂起，下轮 pending 路径续跑。
	}
	mode := schedule.Mode
	if mode == "" {
		if len(schedule.Plan) > 1 {
			mode = ModePipeline
		} else {
			mode = ModeSingle
		}
	}
	run, err := s.CreateRun(team, schedule.Goal, mode, schedule.CollectPolicy, schedule.VerifyCommand, schedule.Plan, triggerID)
	if err != nil {
		// 失败计数（预算见 scheduleTriggerFailureBudget）：保留标记待下轮回
		// 扫裁决——journal 有事件＝已生效收尾；无事件且预算未尽＝重试；
		// 预算尽＝放弃该触发点。计数随标记落账，防确定性失败（如成员被移
		// 除致 ParsePlan 报错）把标记楔死成无限重试。
		schedule.TriggerFailures++
		log.Printf("team schedule %s 触发创建 Run 失败（第 %d 次，预算 %d），保留标记待下轮回扫: %v",
			schedule.TeamID, schedule.TriggerFailures, scheduleTriggerFailureBudget, err)
		if err := s.Schedules.SaveSchedule(schedule); err != nil {
			log.Printf("team schedule %s 失败计数落账失败，下轮重试: %v", schedule.TeamID, err)
		}
		return
	}
	schedule.PendingTriggerID = ""
	schedule.TriggerFailures = 0
	schedule.LastRunID = run.ID
	schedule.LastRunAt = now
	if err := s.Schedules.SaveSchedule(schedule); err != nil {
		log.Printf("team schedule %s 触发收尾落账失败，下轮按 journal 回扫收尾: %v", schedule.TeamID, err)
	}
}

// reconcilePendingTrigger 是 journal-first 协议的回扫对账（启动恢复与触发
// 收尾失败的兜底）：journal 存在 scheduledBy==PendingTriggerID 的
// run_created 事件 → 该触发已生效（哪怕 Run 快照丢失——快照按既有
// journal 重建路径恢复），清标记并记账；不存在 → provably 未触发，重执行
// 步骤②③。不存在「结果未知」的中间态：CreateRun 的 journal 追加要么整行
// 落盘、要么写失败即报错（Journal.Append 契约），回扫只认文件里的事实。
func (s *Scheduler) reconcilePendingTrigger(schedule *TeamSchedule, now time.Time) {
	events, err := s.Journal.Replay()
	if err != nil {
		log.Printf("team schedule %s 回扫 journal 失败，保留标记待下轮重试: %v", schedule.TeamID, err)
		return
	}
	for _, event := range events {
		if event.Type != EventRunCreated {
			continue
		}
		if payloadString(event.Payload, "scheduledBy") != schedule.PendingTriggerID {
			continue
		}
		schedule.PendingTriggerID = ""
		schedule.TriggerFailures = 0
		schedule.LastRunID = event.RunID
		schedule.LastRunAt = now
		if err := s.Schedules.SaveSchedule(schedule); err != nil {
			log.Printf("team schedule %s 回扫收尾落账失败，下轮重试: %v", schedule.TeamID, err)
		}
		return
	}
	team := s.Store.Team(schedule.TeamID)
	if team == nil {
		s.dropSchedule(schedule, "团队已删除")
		return
	}
	if s.teamBusy(schedule.TeamID) {
		return // 团队忙：触发挂起，下轮再试（pending 路径每轮都会走）。
	}
	if schedule.TriggerFailures >= scheduleTriggerFailureBudget {
		// 失败预算耗尽：走到这里意味着 journal 已确认该触发点 provably 未
		// 生效（有事件在上面已收尾，含 CreateRun 报错但 journal 已落的半写
		// 窗口），连续预算次创建失败属确定性错误（如成员被移除致 ParsePlan
		// 报错）。放弃该触发点：清标记归零计数，NextDue 保持步骤①已推进的
		// 值，schedule 回到正常到期路径——无此预算标记将楔死（每 tick 全量
		// 重放 journal 且从此不再触发），配置修复后的下一到期点自愈。
		log.Printf("team schedule %s 触发连续 %d 次创建 Run 失败，放弃该触发点（journal 确认未生效），待下一到期点；请检查配置或团队成员",
			schedule.TeamID, schedule.TriggerFailures)
		schedule.PendingTriggerID = ""
		schedule.TriggerFailures = 0
		if err := s.Schedules.SaveSchedule(schedule); err != nil {
			log.Printf("team schedule %s 放弃触发点落账失败，下轮重试: %v", schedule.TeamID, err)
		}
		return
	}
	s.fireTrigger(schedule, team, schedule.PendingTriggerID, now)
}

// teamBusy 报告团队是否存在非终态 Run（§12 阶段四验收：waiting_input 驻留
// 一律算非终态——定时触发必须绕开，Resume 只在该态上有意义）。
func (s *Scheduler) teamBusy(teamID string) bool {
	for _, run := range s.Store.RunsByTeam(teamID) {
		if !terminalRun(run.Status) {
			return true
		}
	}
	return false
}

func (s *Scheduler) dropSchedule(schedule *TeamSchedule, reason string) {
	if err := s.Schedules.DeleteSchedule(schedule.TeamID, schedule.Version); err != nil {
		log.Printf("team schedule %s 清理失败（%s），下轮重试: %v", schedule.TeamID, reason, err)
		return
	}
	log.Printf("team schedule %s 已清理：%s", schedule.TeamID, reason)
}

func (s *Scheduler) disableSchedule(schedule *TeamSchedule, reason string) {
	schedule.Enabled = false
	if err := s.Schedules.SaveSchedule(schedule); err != nil {
		log.Printf("team schedule %s 停用落账失败，下轮重试: %v", schedule.TeamID, err)
		return
	}
	log.Printf("team schedule %s 已停用：%s", schedule.TeamID, reason)
}

// nextScheduleDue 返回 now 之后的下一个到期点：间隔模式 now+intervalSecs
// （到期点被停机/忙碌错过时以此重锚——错过的多次触发不补跑）；cron 模式
// 按本地时区的下一次匹配。无可到达时刻（cron 不可满足或配置残缺）返回
// (零值, false)。
func nextScheduleDue(schedule *TeamSchedule, now time.Time) (time.Time, bool) {
	if schedule.Cron != "" {
		spec, err := parseCronSpec(schedule.Cron)
		if err != nil {
			return time.Time{}, false
		}
		return spec.nextAfter(now)
	}
	if schedule.IntervalSecs < 1 {
		return time.Time{}, false
	}
	return now.Add(time.Duration(schedule.IntervalSecs) * time.Second), true
}

// scheduleTriggerID 派生确定性触发标识：同一 team 的同一到期点（Unix 秒）
// 在崩溃重试之间保持不变（uuid5 + URL 命名空间，与 submissions 的 migrate
// 幂等键同款做法），是触发幂等与 Run 溯源（Run.ScheduledFrom）的公共键。
func scheduleTriggerID(teamID string, dueUnix int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL,
		[]byte(fmt.Sprintf("mobile-bridge:team-schedule:%s:%d", teamID, dueUnix))).String()
}
