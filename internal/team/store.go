package team

// Store 是 teams.json / team-runs.json 的持久化账本（§8.1、§8.4）：内存
// map + 插入序（仿 internal/httpd/queue.go 的 submissions ledger），写入走
// 原子替换——唯一临时文件名（pid+纳秒，防两进程共享 dataDir 互踩）+ rename，
// 落盘成功后才翻内存值（internal/conventions 的顺序）。Save/Delete 携带
// 期望版本，不匹配返回 ErrVersionConflict，HTTP 侧映射 409。

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ErrVersionConflict 表示乐观版本检查失败：账本已被其他写入更新（或已删除），
// 调用方应重新加载后重算（§8.4）。HTTP 层映射为 409。
var ErrVersionConflict = errors.New("版本冲突：账本已被其他写入更新，请重新加载后再试")

// ErrNotFound 表示账本中不存在该条目。
var ErrNotFound = errors.New("账本条目不存在")

// ErrActiveRuns 表示 Team 仍有非终态 Run（created/dispatching/collecting/
// reducing/waiting_input），不得删除（§10.1：用户必须先取消或等待结束）。
// 该校验在账本层与版本检查同一临界区执行——HTTP 层的预检与之存在 TOCTOU
// 窗口（检查后、删除前并发新建的 Run 会被静默连删），账本层是原子兜底。
var ErrActiveRuns = errors.New("团队仍有未结束的 Run，请先取消或等待结束")

// TeamStore 是 Team/Run 账本的共用契约（§12 工作项 8）：JSON 账本与纯内存
// 实现跑同一张 contract 测试；后续的调度器与 HTTP 层都只依赖此接口。
// 所有方法并发安全；读取返回深拷贝，写入同样克隆入库，调用方持有的对象
// 与账本状态互不影响。
type TeamStore interface {
	Team(id string) *Team
	Teams() []*Team
	SaveTeam(team *Team) error
	DeleteTeam(id string, version uint64) error

	Run(id string) *Run
	Runs() []*Run
	RunsByTeam(teamID string) []*Run
	SaveRun(run *Run) error
	DeleteRun(id string, version uint64) error
}

// Store 是 TeamStore 的 JSON 文件实现。
type Store struct {
	mu         sync.Mutex
	teamsPath  string
	runsPath   string
	teamsOrder []string
	teams      map[string]*Team
	runsOrder  []string
	runs       map[string]*Run
	// runsCorrupt 标记 team-runs.json 整文件损坏（解析失败/非对象；条目级
	// 损坏不算——那只是个别条目被跳过）。置位时 loadRuns 已把损坏文件改名
	// 留存（防后续写盘覆盖原始字节），调用方应打开 journal 后调
	// RebuildRunsFromJournal 按事实来源重建（§8.1）。
	runsCorrupt bool
}

var _ TeamStore = (*Store)(nil)

// LoadStore 读取 teams.json 与 team-runs.json；文件不存在按空账本启动，
// 损坏文件打警告后按空账本启动（现有各 Load 静默丢数据是已知坑，此处显式
// 留痕），单个条目损坏只跳过该条目。team-runs.json 整文件损坏时原文件改名
// 留存并置 RunsLedgerCorrupt——journal 是事实来源，调用方可据此重建。
func LoadStore(teamsPath, runsPath string) *Store {
	store := &Store{
		teamsPath: teamsPath,
		runsPath:  runsPath,
		teams:     map[string]*Team{},
		runs:      map[string]*Run{},
	}
	store.loadTeams()
	store.loadRuns()
	return store
}

// RunsLedgerCorrupt 报告 team-runs.json 是否整文件损坏（需要从 journal 重建）。
func (s *Store) RunsLedgerCorrupt() bool {
	return s.runsCorrupt
}

func (s *Store) loadTeams() {
	object, ok := readLedgerObject(s.teamsPath, "teams.json")
	if !ok {
		return
	}
	for _, key := range object.Keys() {
		raw, _ := object.Get(key)
		var team Team
		if err := json.Unmarshal(jsoncompat.EncodeCompact(raw), &team); err != nil {
			log.Printf("teams.json 条目解析失败，已跳过: key=%s err=%v", key, err)
			continue
		}
		if team.ID == "" || team.ID != key {
			log.Printf("teams.json 条目缺少 id 或与键不一致，已跳过: key=%s id=%s", key, team.ID)
			continue
		}
		s.teams[key] = &team
		s.teamsOrder = append(s.teamsOrder, key)
	}
}

func (s *Store) loadRuns() {
	object, ok := readLedgerObject(s.runsPath, "team-runs.json")
	if !ok {
		// 整文件损坏（区别于文件缺失）：原始字节改名留存，不被后续
		// SaveRun 的全量重写覆盖——人工恢复与审计仍有可能；重建走 journal。
		s.runsCorrupt = quarantineCorruptFile(s.runsPath)
		return
	}
	for _, key := range object.Keys() {
		raw, _ := object.Get(key)
		var run Run
		if err := json.Unmarshal(jsoncompat.EncodeCompact(raw), &run); err != nil {
			log.Printf("team-runs.json 条目解析失败，已跳过: key=%s err=%v", key, err)
			continue
		}
		if run.ID == "" || run.ID != key {
			log.Printf("team-runs.json 条目缺少 id 或与键不一致，已跳过: key=%s id=%s", key, run.ID)
			continue
		}
		s.runs[key] = &run
		s.runsOrder = append(s.runsOrder, key)
	}
}

// readLedgerObject 读取并解析账本文件为有序对象；文件缺失返回空、
// 损坏打警告后返回空。
func readLedgerObject(path, name string) (*jsoncompat.Object, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("%s 读取失败，按空账本启动: path=%s err=%v", name, path, err)
		}
		return nil, false
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		log.Printf("%s 解析失败，按空账本启动: path=%s err=%v", name, path, err)
		return nil, false
	}
	object, ok := value.(*jsoncompat.Object)
	if !ok {
		log.Printf("%s 不是 JSON 对象，按空账本启动: path=%s", name, path)
		return nil, false
	}
	return object, true
}

// quarantineCorruptFile 把损坏的账本文件改名为 <path>.corrupt-<pid>-<nanos>
// 留存（唯一名防并发互踩，与 atomicWriteFile 同惯例）。文件不存在返回
// false；改名失败（权限等）返回 true 并留痕——调用方按「账本损坏但无法
// 留存」处理，后续写盘会覆盖原文件（与既有行为一致，但显式留痕）。
func quarantineCorruptFile(path string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	quarantined := fmt.Sprintf("%s.corrupt-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, quarantined); err != nil {
		log.Printf("team-runs.json 损坏且无法改名留存（后续写盘将覆盖原始字节）: path=%s err=%v", path, err)
		return true
	}
	log.Printf("team-runs.json 损坏，原文件已留存: %s", quarantined)
	return true
}

// Team 返回 id 对应 Team 的深拷贝；不存在返回 nil。
func (s *Store) Team(id string) *Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.teams[id].Clone()
}

// Teams 按插入序返回全部 Team 副本。
func (s *Store) Teams() []*Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	teams := make([]*Team, 0, len(s.teamsOrder))
	for _, id := range s.teamsOrder {
		if team := s.teams[id]; team != nil {
			teams = append(teams, team.Clone())
		}
	}
	return teams
}

// SaveTeam 以乐观版本写入 Team：新条目期望 Version==0（入库后为 1），
// 已有条目期望 Version 与账本一致（入库后 +1）；成功后 team.Version 更新
// 为新版本。写盘成功前内存不变，写盘失败账本与内存都保持原状。
func (s *Store) SaveTeam(team *Team) error {
	if team == nil {
		return errors.New("team 不能为 nil")
	}
	if team.ID == "" {
		return errors.New("team 缺少 id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.teams[team.ID]
	var storedVersion uint64
	if exists {
		storedVersion = existing.Version
	}
	next, err := checkVersion(exists, storedVersion, team.Version, "team="+team.ID)
	if err != nil {
		return err
	}
	stored := team.Clone()
	stored.Version = next
	if err := s.writeFileLocked(s.teamsPath, s.teamsDumpWithLocked(team.ID, stored)); err != nil {
		return fmt.Errorf("teams.json 写入失败: %w", err)
	}
	if !exists {
		s.teamsOrder = append(s.teamsOrder, team.ID)
	}
	s.teams[team.ID] = stored
	team.Version = next
	return nil
}

// DeleteTeam 删除 Team 及其全部 Run（§10.1：Team 没有了，Run 历史也不应
// 成为永远不可达的孤儿）。仍有非终态 Run 时拒绝（ErrActiveRuns）——该校验
// 与版本检查、文件写入同临界区，消除 HTTP 层预检与删除之间的 TOCTOU 窗口。
//
// 两段写的顺序以 teams.json 为唯一提交点：先写剔除该 team 的 teams.json
// （此步失败时 runs 账本完全未动，磁盘/内存/用户认知三方一致——删除失败
// 即什么都没发生，绝不出现「报错的操作却已删掉 Run 历史」）；提交后再写
// 剔除其 Run 的 team-runs.json。孤儿清理失败只留痕不报错：team 已删是既成
// 事实，报错会让用户误以为删除失败；内存按提交结果同步删除（下一次任何
// Run 写盘的全量重写会自然带走磁盘清理）。
func (s *Store) DeleteTeam(id string, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.teams[id]
	if !exists {
		return fmt.Errorf("%w: team=%s", ErrNotFound, id)
	}
	if version != existing.Version {
		return fmt.Errorf("%w: team=%s 期望 version=%d 实际=%d", ErrVersionConflict, id, version, existing.Version)
	}
	for _, runID := range s.runsOrder {
		if run := s.runs[runID]; run != nil && run.TeamID == id && !terminalRun(run.Status) {
			return fmt.Errorf("%w: team=%s run=%s status=%s", ErrActiveRuns, id, runID, run.Status)
		}
	}
	dump := jsoncompat.NewObject()
	for _, existingID := range s.teamsOrder {
		if team := s.teams[existingID]; team != nil && existingID != id {
			dump.Set(existingID, team.jsonObject())
		}
	}
	if err := s.writeFileLocked(s.teamsPath, dump); err != nil {
		return fmt.Errorf("teams.json 写入失败: %w", err)
	}
	// 提交点已过：内存与磁盘同步删除 team。
	delete(s.teams, id)
	s.teamsOrder = removeKey(s.teamsOrder, id)
	// 孤儿 Run 清理（team 已删）：失败只留痕，不回滚也不报错（见函数注释）。
	if orphans := s.runsDumpWithoutTeamLocked(id); orphans != nil {
		if err := s.writeFileLocked(s.runsPath, orphans); err != nil {
			log.Printf("team %s 的孤儿 Run 清理写盘失败（下次 Run 写盘时全量重写自然带走）: %v", id, err)
		}
	}
	for _, runID := range s.runsOrder {
		if run := s.runs[runID]; run != nil && run.TeamID == id {
			delete(s.runs, runID)
		}
	}
	s.runsOrder = removeMissing(s.runsOrder, s.runs)
	return nil
}

// Run 返回 id 对应 Run 的深拷贝；不存在返回 nil。
func (s *Store) Run(id string) *Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id].Clone()
}

// Runs 按插入序返回全部 Run 副本。
func (s *Store) Runs() []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := make([]*Run, 0, len(s.runsOrder))
	for _, id := range s.runsOrder {
		if run := s.runs[id]; run != nil {
			runs = append(runs, run.Clone())
		}
	}
	return runs
}

// RunsByTeam 按插入序返回指定 Team 的 Run 副本。
func (s *Store) RunsByTeam(teamID string) []*Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	runs := []*Run{}
	for _, id := range s.runsOrder {
		if run := s.runs[id]; run != nil && run.TeamID == teamID {
			runs = append(runs, run.Clone())
		}
	}
	return runs
}

// SaveRun 以乐观版本写入 Run，语义同 SaveTeam。
func (s *Store) SaveRun(run *Run) error {
	if run == nil {
		return errors.New("run 不能为 nil")
	}
	if run.ID == "" {
		return errors.New("run 缺少 id")
	}
	if run.TeamID == "" {
		return errors.New("run 缺少 teamId")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveRunLocked(run)
}

// saveRunLocked 写入 Run（调用方持有 s.mu）；语义与 SaveRun 相同。
func (s *Store) saveRunLocked(run *Run) error {
	existing, exists := s.runs[run.ID]
	var storedVersion uint64
	if exists {
		storedVersion = existing.Version
	}
	next, err := checkVersion(exists, storedVersion, run.Version, "run="+run.ID)
	if err != nil {
		return err
	}
	stored := run.Clone()
	stored.Version = next
	if err := s.writeFileLocked(s.runsPath, s.runsDumpWithLocked(run.ID, stored)); err != nil {
		return fmt.Errorf("team-runs.json 写入失败: %w", err)
	}
	if !exists {
		s.runsOrder = append(s.runsOrder, run.ID)
	}
	s.runs[run.ID] = stored
	run.Version = next
	return nil
}

// DeleteRun 以乐观版本删除单个 Run。
func (s *Store) DeleteRun(id string, version uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, exists := s.runs[id]
	if !exists {
		return fmt.Errorf("%w: run=%s", ErrNotFound, id)
	}
	if version != existing.Version {
		return fmt.Errorf("%w: run=%s 期望 version=%d 实际=%d", ErrVersionConflict, id, version, existing.Version)
	}
	dump := jsoncompat.NewObject()
	for _, existingID := range s.runsOrder {
		if run := s.runs[existingID]; run != nil && existingID != id {
			dump.Set(existingID, run.jsonObject())
		}
	}
	if err := s.writeFileLocked(s.runsPath, dump); err != nil {
		return fmt.Errorf("team-runs.json 写入失败: %w", err)
	}
	delete(s.runs, id)
	s.runsOrder = removeKey(s.runsOrder, id)
	return nil
}

// checkVersion 校验期望版本并给出下一版本号：新条目期望 0（入库为 1），
// 更新期望与账本一致（入库 +1）；条目已删除时任何非零期望版本都算冲突。
func checkVersion(exists bool, storedVersion, presented uint64, what string) (uint64, error) {
	if exists {
		if presented != storedVersion {
			return 0, fmt.Errorf("%w: %s 期望 version=%d 实际=%d", ErrVersionConflict, what, presented, storedVersion)
		}
		return storedVersion + 1, nil
	}
	if presented != 0 {
		return 0, fmt.Errorf("%w: %s 已不存在", ErrVersionConflict, what)
	}
	return 1, nil
}

// writeFileLocked 原子写入账本（唯一临时文件名 + rename，0600，尾随换行）。
func (s *Store) writeFileLocked(path string, dump *jsoncompat.Object) error {
	return atomicWriteFile(path, append(jsoncompat.EncodePythonIndent2(dump), '\n'))
}

// teamsDumpWithLocked 生成以 substitute 替换（或追加）指定 Team 后的账本
// 快照；不改动内存状态，落盘失败时内存与磁盘保持一致。
func (s *Store) teamsDumpWithLocked(id string, substitute *Team) *jsoncompat.Object {
	dump := jsoncompat.NewObject()
	inserted := false
	for _, existingID := range s.teamsOrder {
		team := s.teams[existingID]
		if team == nil {
			continue
		}
		if existingID == id {
			dump.Set(existingID, substitute.jsonObject())
			inserted = true
			continue
		}
		dump.Set(existingID, team.jsonObject())
	}
	if !inserted {
		dump.Set(id, substitute.jsonObject())
	}
	return dump
}

func (s *Store) runsDumpWithLocked(id string, substitute *Run) *jsoncompat.Object {
	dump := jsoncompat.NewObject()
	inserted := false
	for _, existingID := range s.runsOrder {
		run := s.runs[existingID]
		if run == nil {
			continue
		}
		if existingID == id {
			dump.Set(existingID, substitute.jsonObject())
			inserted = true
			continue
		}
		dump.Set(existingID, run.jsonObject())
	}
	if !inserted {
		dump.Set(id, substitute.jsonObject())
	}
	return dump
}

// runsDumpWithoutTeamLocked 生成剔除指定 Team 全部 Run 后的 runs 账本快照；
// 该 Team 没有 Run 时返回 nil（无需落盘）。
func (s *Store) runsDumpWithoutTeamLocked(teamID string) *jsoncompat.Object {
	changed := false
	dump := jsoncompat.NewObject()
	for _, existingID := range s.runsOrder {
		run := s.runs[existingID]
		if run == nil {
			continue
		}
		if run.TeamID == teamID {
			changed = true
			continue
		}
		dump.Set(existingID, run.jsonObject())
	}
	if !changed {
		return nil
	}
	return dump
}

// ---- team-runs.json 整文件损坏后的 journal 重建（§8.1）----

// RebuildRunsFromJournal 在 team-runs.json 整文件损坏时按 journal 事实重建
// Run 账本（§8.1：team-journal.jsonl 是运行事实来源，team-runs.json 只是可
// 重建的查询快照）。仅 RunsLedgerCorrupt 时生效——健康账本重放会把
// DeleteTeam 已删除的 Run 从 journal 里复活。可恢复的是运行事实：任务
// 状态、submissionId、inputHash、结果引用与 run 终态；计划细节（完整
// prompt、依赖、写锁）不在事件载荷里（§10.3 有界 payload），重建后为空
// ——未派发任务重派发时会因空文本被账本输入校验确定性拒绝，不会静默
// 发送残缺内容。run_created 载荷的 teamId/goal 是重建能力后补的字段，
// 旧数据没有 teamId 的 Run 与团队已不存在的 Run 一律跳过并留痕。
func (s *Store) RebuildRunsFromJournal(events []*Event) {
	if !s.runsCorrupt {
		return
	}
	// ① 纯重放（无锁）：以 run_created 为锚，按事件序重建 Run 骨架。
	byID := map[string]*Run{}
	var order []string
	for _, event := range events {
		if event == nil {
			continue
		}
		run := byID[event.RunID]
		if run == nil {
			if event.Type != EventRunCreated {
				continue // 缺 run_created 锚点的孤儿事件：无从重建。
			}
			run = &Run{
				ID:            event.RunID,
				TeamID:        payloadString(event.Payload, "teamId"),
				Goal:          payloadString(event.Payload, "goal"),
				Mode:          payloadString(event.Payload, "mode"),
				CollectPolicy: payloadString(event.Payload, "collectPolicy"),
				Status:        RunCreated,
				Round:         1,
				PlanHash:      payloadString(event.Payload, "planHash"),
				VerifyCommand: payloadStrings(event.Payload, "verifyCommand"),
				CreatedAt:     event.CreatedAt,
				UpdatedAt:     event.CreatedAt,
			}
			byID[event.RunID] = run
			order = append(order, event.RunID)
			continue
		}
		applyJournalEventToRun(run, event)
	}
	// ② 落账（持锁）：条目均为新建（version 0），逐条写入并持久化。
	s.mu.Lock()
	defer s.mu.Unlock()
	rebuilt := 0
	for _, runID := range order {
		run := byID[runID]
		if run.TeamID == "" || s.teams[run.TeamID] == nil {
			log.Printf("team-runs.json 重建：跳过 Run %s（缺少 teamId 或团队已删除）", runID)
			continue
		}
		if err := s.saveRunLocked(run); err != nil {
			log.Printf("team-runs.json 重建：Run %s 保存失败: %v", runID, err)
			continue
		}
		rebuilt++
	}
	log.Printf("team-runs.json 损坏，已按 journal 重建 %d 条 Run（事件 %d 条）", rebuilt, len(events))
}

// applyJournalEventToRun 把一条 journal 事件应用到重建中的 Run（journal 的
// 单调 sequence 已保证事件因果序）。任务事件按 taskId 幂等 upsert——重建
// 只认事实，不做终态回退保护（journal 本身就是权威顺序）。
func applyJournalEventToRun(run *Run, event *Event) {
	run.UpdatedAt = event.CreatedAt
	if !isTaskScopedEvent(event.Type) && event.Type != EventTaskPlanned {
		switch event.Type {
		case EventRunWaitingInput:
			run.Status = RunWaitingInput
		case EventRunCompleted:
			run.Status = RunCompleted
		case EventRunFailed:
			run.Status = RunFailed
		case EventRunCancelled:
			run.Status = RunCancelled
		case EventRunStopped:
			run.Status = RunStopped
		case EventRunVerify:
			// 闸门结果随事件重建（§6.4.1）：RunReducing 态的 run 崩溃后凭
			// Verify 判定终局/等待 resume，缺了它会被误折成 completed。
			verify := &VerifyResult{
				ExitCode:   payloadInt(event.Payload, "exitCode"),
				OutputHead: payloadString(event.Payload, "outputHead"),
				FinishedAt: event.CreatedAt,
			}
			verify.Truncated, _ = event.Payload["truncated"].(bool)
			run.Verify = verify
		}
		if reason := payloadString(event.Payload, "stopReason"); reason != "" {
			run.StopReason = reason
		}
		return
	}
	taskID := payloadString(event.Payload, "taskId")
	if taskID == "" {
		return
	}
	task := findTaskIn(run, taskID)
	if task == nil {
		run.Tasks = append(run.Tasks, Task{ID: taskID, Ordinal: len(run.Tasks)})
		task = &run.Tasks[len(run.Tasks)-1]
	}
	switch event.Type {
	case EventTaskPlanned:
		task.Status = TaskPlanned
		task.MemberID = payloadString(event.Payload, "memberId")
		task.InstructionsHead = payloadString(event.Payload, "instructionsHead")
	case EventTaskDispatching:
		task.Status = TaskDispatching
		task.SubmissionID = payloadString(event.Payload, "submissionId")
		task.InputHash = payloadString(event.Payload, "inputHash")
		if attempt := payloadInt(event.Payload, "attempt"); attempt > 0 {
			task.Attempt = attempt
		}
	case EventTaskRunning:
		task.Status = TaskRunning
	case EventTaskCompleted:
		task.Status = TaskCompleted
		result := &TaskResult{
			SessionID:      payloadString(event.Payload, "sessionId"),
			TurnID:         payloadString(event.Payload, "turnId"),
			Result:         payloadString(event.Payload, "result"),
			ResultChars:    payloadInt(event.Payload, "resultChars"),
			StopReason:     payloadString(event.Payload, "stopReason"),
			FileSummary:    payloadString(event.Payload, "fileSummary"),
			CommandSummary: payloadString(event.Payload, "commandSummary"),
		}
		result.Truncated, _ = event.Payload["truncated"].(bool)
		task.Result = result.Result
		task.ResultDetail = result
	case EventTaskFailed:
		task.Status = TaskFailed
		task.Error = payloadString(event.Payload, "error")
		task.Outcome = payloadString(event.Payload, "outcome")
	case EventTaskOutcomeUnknown:
		task.Status = TaskOutcomeUnknown
	case EventTaskStopped, EventTaskInputHashMismatch:
		task.Status = TaskStopped
		task.Outcome = payloadString(event.Payload, "outcome")
	}
	if reason := payloadString(event.Payload, "stopReason"); reason != "" && event.Type != EventTaskDispatching {
		task.StopReason = reason
	}
}

// payloadInt 读取载荷里的整数字段（journal 解码保持 json.Number 字面量）。
func payloadInt(payload map[string]any, key string) int {
	number, ok := payload[key].(json.Number)
	if !ok {
		return 0
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0
	}
	return value
}

// payloadStrings 读取载荷里的字符串数组字段（run_created 的 verifyCommand
// 等）；缺失或形态不符返回 nil。
func payloadStrings(payload map[string]any, key string) []string {
	items, ok := payload[key].([]any)
	if !ok {
		return nil
	}
	values := make([]string, 0, len(items))
	for _, item := range items {
		if text, isString := item.(string); isString {
			values = append(values, text)
		}
	}
	return values
}

func removeKey(order []string, key string) []string {
	kept := order[:0]
	for _, existing := range order {
		if existing != key {
			kept = append(kept, existing)
		}
	}
	return kept
}

// removeMissing 收紧 order：只保留仍存在于 entries 的键。
func removeMissing[V any](order []string, entries map[string]V) []string {
	kept := order[:0]
	for _, existing := range order {
		if _, still := entries[existing]; still {
			kept = append(kept, existing)
		}
	}
	return kept
}

// atomicWriteFile 组合 accpool/pool.go 的唯一临时文件名（pid+纳秒，防并发
// 覆盖）与 conventions 的写盘语义：MkdirAll 0700、写 0600 临时文件、
// rename 原子替换。
func atomicWriteFile(path string, content []byte) error {
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
