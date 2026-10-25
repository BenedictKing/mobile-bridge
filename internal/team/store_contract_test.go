package team

// TeamStore 的内存/JSON 双实现 contract 测试（§12 工作项 8、§13.1 team/store
// 与 team/journal 行），外加 journal 契约、EventFeed 降级契约与模型常量的
// 单元测试。全部使用标准库 testing 与 channel 同步，不用固定 sleep 判断
// 正确性（§13.1 末行）。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

var testTime = time.Date(2026, 10, 24, 12, 30, 0, 123456789, time.UTC)

// ---- 纯内存实现（与 JSON Store 跑同一张契约表）----

// memStore 是 TeamStore 的纯内存实现：语义与 Store 一致——乐观版本、
// 克隆入库、插入序、删除 Team 级联删除其 Run，只是不落盘。
type memStore struct {
	mu         sync.Mutex
	teamsOrder []string
	teams      map[string]*Team
	runsOrder  []string
	runs       map[string]*Run
}

var _ TeamStore = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{teams: map[string]*Team{}, runs: map[string]*Run{}}
}

func (m *memStore) Team(id string) *Team {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.teams[id].Clone()
}

func (m *memStore) Teams() []*Team {
	m.mu.Lock()
	defer m.mu.Unlock()
	teams := make([]*Team, 0, len(m.teamsOrder))
	for _, id := range m.teamsOrder {
		if team := m.teams[id]; team != nil {
			teams = append(teams, team.Clone())
		}
	}
	return teams
}

func (m *memStore) SaveTeam(team *Team) error {
	if team == nil {
		return errors.New("team 不能为 nil")
	}
	if team.ID == "" {
		return errors.New("team 缺少 id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.teams[team.ID]
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
	if !exists {
		m.teamsOrder = append(m.teamsOrder, team.ID)
	}
	m.teams[team.ID] = stored
	team.Version = next
	return nil
}

func (m *memStore) DeleteTeam(id string, version uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.teams[id]
	if !exists {
		return fmt.Errorf("%w: team=%s", ErrNotFound, id)
	}
	if version != existing.Version {
		return fmt.Errorf("%w: team=%s 期望 version=%d 实际=%d", ErrVersionConflict, id, version, existing.Version)
	}
	for _, runID := range m.runsOrder {
		if run := m.runs[runID]; run != nil && run.TeamID == id && !terminalRun(run.Status) {
			return fmt.Errorf("%w: team=%s run=%s status=%s", ErrActiveRuns, id, runID, run.Status)
		}
	}
	delete(m.teams, id)
	m.teamsOrder = removeKey(m.teamsOrder, id)
	for _, runID := range m.runsOrder {
		if run := m.runs[runID]; run != nil && run.TeamID == id {
			delete(m.runs, runID)
		}
	}
	m.runsOrder = removeMissing(m.runsOrder, m.runs)
	return nil
}

func (m *memStore) Run(id string) *Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runs[id].Clone()
}

func (m *memStore) Runs() []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := make([]*Run, 0, len(m.runsOrder))
	for _, id := range m.runsOrder {
		if run := m.runs[id]; run != nil {
			runs = append(runs, run.Clone())
		}
	}
	return runs
}

func (m *memStore) RunsByTeam(teamID string) []*Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	runs := []*Run{}
	for _, id := range m.runsOrder {
		if run := m.runs[id]; run != nil && run.TeamID == teamID {
			runs = append(runs, run.Clone())
		}
	}
	return runs
}

func (m *memStore) SaveRun(run *Run) error {
	if run == nil {
		return errors.New("run 不能为 nil")
	}
	if run.ID == "" {
		return errors.New("run 缺少 id")
	}
	if run.TeamID == "" {
		return errors.New("run 缺少 teamId")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.runs[run.ID]
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
	if !exists {
		m.runsOrder = append(m.runsOrder, run.ID)
	}
	m.runs[run.ID] = stored
	run.Version = next
	return nil
}

func (m *memStore) DeleteRun(id string, version uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	existing, exists := m.runs[id]
	if !exists {
		return fmt.Errorf("%w: run=%s", ErrNotFound, id)
	}
	if version != existing.Version {
		return fmt.Errorf("%w: run=%s 期望 version=%d 实际=%d", ErrVersionConflict, id, version, existing.Version)
	}
	delete(m.runs, id)
	m.runsOrder = removeKey(m.runsOrder, id)
	return nil
}

// ---- 契约表驱动 ----

// storeHarness 把同一张契约表跑在不同实现上。reopen 为 nil 表示纯内存
// 实现，跳过持久化断言；teamsPath 为空表示无法预置/读取账本文件。
type storeHarness struct {
	label     string
	open      func(t *testing.T) TeamStore
	reopen    func(t *testing.T) TeamStore
	teamsPath string
	runsPath  string
}

func TestTeamStoreContract(t *testing.T) {
	factories := map[string]func(t *testing.T) *storeHarness{
		"mem": func(t *testing.T) *storeHarness {
			return &storeHarness{label: "mem", open: func(*testing.T) TeamStore { return newMemStore() }}
		},
		"json": func(t *testing.T) *storeHarness {
			dir := t.TempDir()
			teamsPath := filepath.Join(dir, "teams.json")
			runsPath := filepath.Join(dir, "team-runs.json")
			openStore := func(*testing.T) TeamStore { return LoadStore(teamsPath, runsPath) }
			return &storeHarness{
				label:     "json",
				open:      openStore,
				reopen:    openStore,
				teamsPath: teamsPath,
				runsPath:  runsPath,
			}
		},
	}
	cases := map[string]func(t *testing.T, h *storeHarness){
		"空账本启动":     testContractEmptyStart,
		"坏文件降级并留警告": testContractBadFileDegrades,
		"保存与读取往返":   testContractSaveRoundTrip,
		"版本冲突":      testContractVersionConflict,
		"删除与级联":     testContractDelete,
		"未知字段保留":    testContractUnknownFields,
		"输入校验":      testContractValidation,
	}
	for name, factory := range factories {
		factory := factory
		t.Run(name, func(t *testing.T) {
			for caseName, run := range cases {
				run := run
				t.Run(caseName, func(t *testing.T) {
					run(t, factory(t))
				})
			}
		})
	}
}

func testContractEmptyStart(t *testing.T, h *storeHarness) {
	store := h.open(t)
	if teams := store.Teams(); len(teams) != 0 {
		t.Fatalf("空账本应无 team，得到 %d 个", len(teams))
	}
	if runs := store.Runs(); len(runs) != 0 {
		t.Fatalf("空账本应无 run，得到 %d 个", len(runs))
	}
	if store.Team("missing") != nil {
		t.Fatal("空账本 Team() 应返回 nil")
	}
	if store.Run("missing") != nil {
		t.Fatal("空账本 Run() 应返回 nil")
	}
	if runs := store.RunsByTeam("missing"); len(runs) != 0 {
		t.Fatalf("空账本 RunsByTeam 应为空，得到 %d 个", len(runs))
	}
	if h.teamsPath == "" {
		return
	}
	// 空对象文件同样按空账本启动，且不产生警告。
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	for _, path := range []string{h.teamsPath, h.runsPath} {
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store = h.open(t)
	if len(store.Teams()) != 0 || len(store.Runs()) != 0 {
		t.Fatal("空对象文件应按空账本启动")
	}
	if strings.Contains(logs.String(), "teams.json") || strings.Contains(logs.String(), "team-runs.json") {
		t.Fatalf("空对象文件不应触发警告: %s", logs.String())
	}
}

func testContractBadFileDegrades(t *testing.T, h *storeHarness) {
	if h.teamsPath == "" {
		t.Skip("纯内存实现不读文件")
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	// 整文件损坏：警告 + 空账本。
	if err := os.WriteFile(h.teamsPath, []byte("不是 JSON{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.runsPath, []byte("[1, 2, 3]"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := h.open(t)
	if len(store.Teams()) != 0 || len(store.Runs()) != 0 {
		t.Fatal("坏文件应按空账本启动")
	}
	if !strings.Contains(logs.String(), "按空账本启动") {
		t.Fatalf("坏文件降级必须留下警告日志: %q", logs.String())
	}

	// 单条目损坏：只跳过坏条目，好条目保留；id 与键不一致同样跳过。
	logs.Reset()
	teamJSON := `{"id": "t1", "name": "幸存者", "members": [], "policy": {}, "createdAt": "2026-10-24T10:00:00Z", "updatedAt": "2026-10-24T10:00:00Z", "version": 1}`
	if err := os.WriteFile(h.teamsPath, []byte("{\n  \"t1\": "+teamJSON+",\n  \"bad\": 123,\n  \"mismatch\": {\"id\": \"other\"}\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store = h.open(t)
	teams := store.Teams()
	if len(teams) != 1 || teams[0].ID != "t1" || teams[0].Version != 1 {
		t.Fatalf("好条目应保留，坏条目应跳过: %+v", teams)
	}
	if !strings.Contains(logs.String(), "已跳过") {
		t.Fatalf("跳过条目必须留下警告日志: %q", logs.String())
	}
}

func testContractSaveRoundTrip(t *testing.T, h *storeHarness) {
	store := h.open(t)
	team := sampleTeam("t1")
	if err := store.SaveTeam(team); err != nil {
		t.Fatalf("SaveTeam: %v", err)
	}
	if team.Version != 1 {
		t.Fatalf("新条目入库后 version 应为 1，得到 %d", team.Version)
	}
	if got := store.Team("t1"); !reflect.DeepEqual(got, team) {
		t.Fatalf("Team 往返不一致:\n得到 %+v\n期望 %+v", got, team)
	}
	updated := store.Team("t1")
	updated.Name = "改名后的团队"
	updated.UpdatedAt = testTime.Add(time.Hour)
	if err := store.SaveTeam(updated); err != nil {
		t.Fatalf("SaveTeam 更新: %v", err)
	}
	if updated.Version != 2 {
		t.Fatalf("更新后 version 应为 2，得到 %d", updated.Version)
	}
	if got := store.Team("t1"); !reflect.DeepEqual(got, updated) {
		t.Fatalf("Team 更新往返不一致:\n得到 %+v\n期望 %+v", got, updated)
	}

	run := sampleRun("r1", "t1")
	if err := store.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if run.Version != 1 {
		t.Fatalf("Run 入库后 version 应为 1，得到 %d", run.Version)
	}
	if got := store.Run("r1"); !reflect.DeepEqual(got, run) {
		t.Fatalf("Run 往返不一致:\n得到 %+v\n期望 %+v", got, run)
	}
	// Task.ResultDetail 的引用字段必须原样存活（§6.5 salvage 依赖）。
	if got := store.Run("r1"); got.Tasks[0].ResultDetail == nil || got.Tasks[0].ResultDetail.TurnID != "turn-7" {
		t.Fatalf("ResultDetail 丢失: %+v", got.Tasks[0].ResultDetail)
	}
	updatedRun := store.Run("r1")
	updatedRun.Status = RunFailed
	updatedRun.StopReason = "max_rounds"
	updatedRun.Tasks[1].Status = TaskCancelled
	updatedRun.Tasks[1].Error = "被用户取消"
	if err := store.SaveRun(updatedRun); err != nil {
		t.Fatalf("SaveRun 更新: %v", err)
	}
	if got := store.Run("r1"); !reflect.DeepEqual(got, updatedRun) {
		t.Fatalf("Run 更新往返不一致:\n得到 %+v\n期望 %+v", got, updatedRun)
	}

	// 插入序与按 Team 过滤。
	if err := store.SaveTeam(sampleTeam("t2")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(sampleRun("r2", "t2")); err != nil {
		t.Fatal(err)
	}
	var teamIDs []string
	for _, entry := range store.Teams() {
		teamIDs = append(teamIDs, entry.ID)
	}
	if !reflect.DeepEqual(teamIDs, []string{"t1", "t2"}) {
		t.Fatalf("Teams 应按插入序返回，得到 %v", teamIDs)
	}
	if runs := store.RunsByTeam("t1"); len(runs) != 1 || runs[0].ID != "r1" {
		t.Fatalf("RunsByTeam 过滤错误: %+v", runs)
	}

	if h.reopen == nil {
		return
	}
	reopened := h.reopen(t)
	if got := reopened.Team("t1"); !reflect.DeepEqual(got, updated) {
		t.Fatalf("重开后 Team 不一致:\n得到 %+v\n期望 %+v", got, updated)
	}
	if got := reopened.Run("r1"); !reflect.DeepEqual(got, updatedRun) {
		t.Fatalf("重开后 Run 不一致:\n得到 %+v\n期望 %+v", got, updatedRun)
	}
	if h.teamsPath == "" {
		return
	}
	// 原子写不留临时文件残骸。
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(h.teamsPath), "*.tmp*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("原子写不应残留临时文件: %v", leftovers)
	}
}

func testContractVersionConflict(t *testing.T, h *storeHarness) {
	store := h.open(t)
	team := sampleTeam("t1")
	if err := store.SaveTeam(team); err != nil {
		t.Fatal(err)
	}
	stale := store.Team("t1")
	fresh := store.Team("t1")
	fresh.Name = "第二次写入"
	if err := store.SaveTeam(fresh); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTeam(stale); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("过期版本写入应报 ErrVersionConflict，得到 %v", err)
	}
	if err := store.SaveTeam(sampleTeam("t1")); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("覆盖既有条目（version=0）应报 ErrVersionConflict，得到 %v", err)
	}
	newWithVersion := sampleTeam("t9")
	newWithVersion.Version = 5
	if err := store.SaveTeam(newWithVersion); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("新条目携带非零版本应报 ErrVersionConflict，得到 %v", err)
	}
	if got := store.Team("t1"); got.Version != 2 || got.Name != "第二次写入" {
		t.Fatalf("冲突写入不得破坏账本: %+v", got)
	}

	run := sampleRun("r1", "t1")
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	staleRun := store.Run("r1")
	freshRun := store.Run("r1")
	freshRun.Goal = "修订后的目标"
	if err := store.SaveRun(freshRun); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(staleRun); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("Run 过期版本写入应报 ErrVersionConflict，得到 %v", err)
	}
	runWithVersion := sampleRun("r9", "t1")
	runWithVersion.Version = 3
	if err := store.SaveRun(runWithVersion); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("Run 新条目携带非零版本应报 ErrVersionConflict，得到 %v", err)
	}
}

func testContractDelete(t *testing.T, h *storeHarness) {
	store := h.open(t)
	if err := store.SaveTeam(sampleTeam("t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTeam(sampleTeam("t2")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(sampleRun("r1", "t1")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveRun(sampleRun("r2", "t2")); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteTeam("missing", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除不存在的 team 应报 ErrNotFound，得到 %v", err)
	}
	if err := store.DeleteTeam("t1", 99); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("删除版本不匹配应报 ErrVersionConflict，得到 %v", err)
	}
	if store.Team("t1") == nil {
		t.Fatal("失败删除不得移除条目")
	}
	// 非终态 Run 拦截删除（§10.1；账本层与版本校验同一临界区，消除 HTTP
	// 预检与删除之间的 TOCTOU 窗口）。
	if err := store.DeleteTeam("t1", 1); !errors.Is(err, ErrActiveRuns) {
		t.Fatalf("有非终态 Run 时删除应报 ErrActiveRuns，得到 %v", err)
	}
	if store.Team("t1") == nil || store.Run("r1") == nil {
		t.Fatal("被拦截的删除不得移除条目")
	}
	// Run 落终态后删除放行。
	terminal := store.Run("r1")
	terminal.Status = RunCompleted
	if err := store.SaveRun(terminal); err != nil {
		t.Fatalf("落终态失败: %v", err)
	}
	if err := store.DeleteTeam("t1", 1); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if store.Team("t1") != nil {
		t.Fatal("删除后 Team() 应返回 nil")
	}
	if store.Run("r1") != nil {
		t.Fatal("删除 Team 应级联删除其 Run")
	}
	if got := store.Run("r2"); got == nil || got.ID != "r2" {
		t.Fatalf("其他 Team 的 Run 不应受影响: %+v", got)
	}
	if runs := store.RunsByTeam("t1"); len(runs) != 0 {
		t.Fatalf("RunsByTeam 删除后应为空，得到 %d 个", len(runs))
	}

	// Run 单独删除：版本门槛 + 删除后可用 version 0 重建。
	if err := store.DeleteRun("r2", 99); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("DeleteRun 版本不匹配应报 ErrVersionConflict，得到 %v", err)
	}
	if err := store.DeleteRun("r2", 1); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if store.Run("r2") != nil {
		t.Fatal("删除后 Run() 应返回 nil")
	}
	recreated := sampleRun("r2", "t2")
	if err := store.SaveRun(recreated); err != nil {
		t.Fatalf("删除后重建应作为新条目（version=0）成功: %v", err)
	}
	if recreated.Version != 1 {
		t.Fatalf("重建条目 version 应为 1，得到 %d", recreated.Version)
	}

	if h.reopen == nil {
		return
	}
	reopened := h.reopen(t)
	if reopened.Team("t1") != nil || reopened.Run("r1") != nil {
		t.Fatal("删除应持久化")
	}
	if reopened.Run("r2") == nil {
		t.Fatal("重建的 Run 应持久化")
	}
}

func testContractUnknownFields(t *testing.T, h *storeHarness) {
	if h.teamsPath == "" {
		// 纯内存实现：直接以 JSON 装载未知字段，保存后仍可读回。
		var team Team
		payload := []byte(`{"id":"t1","name":"内存组","members":[],"policy":{},"createdAt":"2026-10-24T10:00:00Z","updatedAt":"2026-10-24T10:00:00Z","version":1,"futureField":{"x":1}}`)
		if err := json.Unmarshal(payload, &team); err != nil {
			t.Fatal(err)
		}
		team.Version = 0 // 内存实现没有文件预置路径，作为新条目入库
		store := h.open(t)
		if err := store.SaveTeam(&team); err != nil {
			t.Fatal(err)
		}
		got := store.Team("t1")
		if got.extra == nil {
			t.Fatal("内存实现应保留未知字段")
		}
		if _, ok := got.extra.Get("futureField"); !ok {
			t.Fatalf("未知字段丢失: %+v", got.extra)
		}
		return
	}
	teamFile := `{
  "t1": {
    "id": "t1",
    "name": "旧名",
    "members": [],
    "policy": {"maxParallelTasks": 3},
    "createdAt": "2026-10-24T10:00:00.123456789Z",
    "updatedAt": "2026-10-24T10:00:00.123456789Z",
    "version": 3,
    "futureField": {"nested": [1, 2]},
    "teamExtra": "保留我"
  }
}`
	runFile := `{
  "r1": {
    "id": "r1",
    "teamId": "t1",
    "goal": "目标",
    "status": "created",
    "round": 1,
    "planHash": "hash",
    "tasks": [
      {
        "id": "k1",
        "memberId": "lead",
        "prompt": "p",
        "inputHash": "ih",
        "attempt": 1,
        "status": "planned",
        "resultDetail": {"sessionId": "s1", "turnId": "t7", "result": "r", "resultChars": 1, "truncated": true, "rdExtra": 42},
        "taskExtra": {"a": 1}
      }
    ],
    "createdAt": "2026-10-24T10:00:00.123456789Z",
    "updatedAt": "2026-10-24T10:00:00.123456789Z",
    "version": 2,
    "runExtra": [1, 2]
  }
}`
	if err := os.WriteFile(h.teamsPath, []byte(teamFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.runsPath, []byte(runFile), 0o600); err != nil {
		t.Fatal(err)
	}
	store := h.open(t)
	team := store.Team("t1")
	if team == nil || team.Version != 3 {
		t.Fatalf("应加载带未知字段的 team: %+v", team)
	}
	if team.extra == nil {
		t.Fatal("Team 未知字段丢失")
	}
	run := store.Run("r1")
	if run == nil || run.Tasks[0].extra == nil || run.Tasks[0].ResultDetail == nil || run.Tasks[0].ResultDetail.extra == nil {
		t.Fatalf("Run/Task/TaskResult 未知字段丢失: %+v", run)
	}

	// 改写已知字段并保存：未知字段必须原样回写。
	team.Name = "新名"
	if err := store.SaveTeam(team); err != nil {
		t.Fatal(err)
	}
	run.Goal = "修订目标"
	if err := store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	rawTeams, err := os.ReadFile(h.teamsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`"futureField"`, `"nested"`, `"teamExtra"`, `"保留我"`, `"新名"`} {
		if !bytes.Contains(rawTeams, []byte(needle)) {
			t.Fatalf("teams.json 回写后应包含 %s:\n%s", needle, rawTeams)
		}
	}
	rawRuns, err := os.ReadFile(h.runsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{`"runExtra"`, `"taskExtra"`, `"rdExtra"`, `"修订目标"`} {
		if !bytes.Contains(rawRuns, []byte(needle)) {
			t.Fatalf("team-runs.json 回写后应包含 %s:\n%s", needle, rawRuns)
		}
	}
	// 重开后再读一轮：未知字段与已知字段同时存活。
	reopened := h.reopen(t)
	if got := reopened.Team("t1"); got == nil || got.extra == nil || got.Name != "新名" {
		t.Fatalf("重开后未知字段/已知字段不一致: %+v", got)
	}
	if got := reopened.Run("r1"); got == nil || got.Tasks[0].extra == nil || got.Tasks[0].ResultDetail.extra == nil {
		t.Fatalf("重开后 Run 未知字段不一致: %+v", got)
	}
}

func testContractValidation(t *testing.T, h *storeHarness) {
	store := h.open(t)
	if err := store.SaveTeam(nil); err == nil {
		t.Fatal("SaveTeam(nil) 应报错")
	}
	if err := store.SaveTeam(&Team{Name: "缺 id"}); err == nil {
		t.Fatal("缺 id 的 team 应报错")
	}
	if err := store.SaveRun(nil); err == nil {
		t.Fatal("SaveRun(nil) 应报错")
	}
	if err := store.SaveRun(&Run{ID: "r1"}); err == nil {
		t.Fatal("缺 teamId 的 run 应报错")
	}
}

// ---- team-runs.json 整文件损坏：原件留存 + journal 重建（§8.1）----

// TestStoreRunsCorruptionQuarantinesAndRebuilds：team-runs.json 整文件损坏时
// （区别于条目级损坏）原文件改名留存（不被下一次 SaveRun 的全量重写覆盖，
// 人工恢复仍有原始字节可依），并按 journal 事实重建可恢复的 Run——运行
// 事实（任务状态/submissionId/inputHash/结果引用/终态）恢复，团队已删除
// 或缺 teamId（旧数据）的 Run 跳过，不借重建复活。
func TestStoreRunsCorruptionQuarantinesAndRebuilds(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")

	// 先落一个团队（重建只保留团队仍存在的 Run）。
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(sampleTeam("t1")); err != nil {
		t.Fatal(err)
	}

	// journal 事实流：run_created（含 teamId/goal）→ planned → dispatching
	// → running → completed → run_completed；外加一条团队已删除的孤儿 Run。
	events := []*Event{
		{ID: "e1", RunID: "r1", Type: EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": "t1", "goal": "重建目标", "mode": "pipeline", "planHash": "hash-1", "taskCount": jsoncompat.Num(1)}, CreatedAt: testTime},
		{ID: "e2", RunID: "r1", Type: EventTaskPlanned, Source: "user",
			Payload: map[string]any{"taskId": "design", "memberId": "lead", "instructionsHead": "分析"}, CreatedAt: testTime.Add(time.Second)},
		{ID: "e3", RunID: "r1", Type: EventTaskDispatching, Source: "runner",
			Payload: map[string]any{"taskId": "design", "sessionId": "codex-1", "submissionId": "sub-1", "inputHash": "ih", "attempt": jsoncompat.Num(1)}, CreatedAt: testTime.Add(2 * time.Second)},
		{ID: "e4", RunID: "r1", Type: EventTaskRunning, Source: "watcher",
			Payload: map[string]any{"taskId": "design", "sessionId": "codex-1", "turnId": "turn-1"}, CreatedAt: testTime.Add(3 * time.Second)},
		{ID: "e5", RunID: "r1", Type: EventTaskCompleted, Source: "watcher",
			Payload: map[string]any{"taskId": "design", "sessionId": "codex-1", "turnId": "turn-1",
				"result": "分析完成。", "resultChars": jsoncompat.Num(5), "truncated": false, "stopReason": "end_turn"}, CreatedAt: testTime.Add(4 * time.Second)},
		{ID: "e6", RunID: "r1", Type: EventRunCompleted, Source: "runner",
			Payload: map[string]any{}, CreatedAt: testTime.Add(5 * time.Second)},
		{ID: "e7", RunID: "r-orphan", Type: EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": "t-gone", "goal": "孤儿", "mode": "single", "taskCount": jsoncompat.Num(1)}, CreatedAt: testTime.Add(6 * time.Second)},
	}

	// 整文件损坏 → 空账本启动 + 原件改名留存。
	if err := os.WriteFile(runsPath, []byte("不是 JSON{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if !store.RunsLedgerCorrupt() {
		t.Fatal("整文件损坏应置 RunsLedgerCorrupt")
	}
	if len(store.Runs()) != 0 {
		t.Fatal("损坏账本应按空账本启动")
	}
	if _, err := os.Stat(runsPath); !os.IsNotExist(err) {
		t.Fatalf("损坏原文件应已被改名移走: %v", err)
	}
	quarantined, err := filepath.Glob(runsPath + ".corrupt-*")
	if err != nil || len(quarantined) != 1 {
		t.Fatalf("应留存恰好一个损坏副本: %v %v", quarantined, err)
	}
	if raw, readErr := os.ReadFile(quarantined[0]); readErr != nil || string(raw) != "不是 JSON{{{" {
		t.Fatalf("留存副本应保留原始字节: %q %v", raw, readErr)
	}

	// journal 重建：r1 的运行事实恢复；团队已删除的 r-orphan 跳过。
	store.RebuildRunsFromJournal(events)
	run := store.Run("r1")
	if run == nil {
		t.Fatal("重建后 r1 应存在")
	}
	if run.TeamID != "t1" || run.Goal != "重建目标" || run.Status != RunCompleted || run.PlanHash != "hash-1" {
		t.Fatalf("重建的 Run 头字段异常: %+v", run)
	}
	if len(run.Tasks) != 1 {
		t.Fatalf("重建任务数异常: %+v", run.Tasks)
	}
	task := run.Tasks[0]
	if task.ID != "design" || task.Status != TaskCompleted ||
		task.SubmissionID != "sub-1" || task.InputHash != "ih" || task.MemberID != "lead" {
		t.Fatalf("重建任务事实异常: %+v", task)
	}
	if task.ResultDetail == nil || task.ResultDetail.TurnID != "turn-1" ||
		task.ResultDetail.Result != "分析完成。" || task.ResultDetail.ResultChars != 5 {
		t.Fatalf("重建结果引用异常: %+v", task.ResultDetail)
	}
	if runs := store.RunsByTeam("t1"); len(runs) != 1 || runs[0].ID != "r1" {
		t.Fatalf("RunsByTeam 过滤异常: %+v", runs)
	}
	if store.Run("r-orphan") != nil {
		t.Fatal("团队已删除的 Run 不得借重建复活")
	}

	// 重建结果落盘（重开仍在），留存副本不被清理/覆盖。
	reopened := LoadStore(teamsPath, runsPath)
	got := reopened.Run("r1")
	if got == nil || got.Status != RunCompleted || got.Tasks[0].ResultDetail == nil {
		t.Fatalf("重开后重建结果应持久化: %+v", got)
	}
	if again, globErr := filepath.Glob(runsPath + ".corrupt-*"); globErr != nil || len(again) != 1 {
		t.Fatalf("留存副本不得被清理: %v %v", again, globErr)
	}
}

// TestStoreRebuildIsNoopOnHealthyLedger：健康账本上重放 journal 必须是 no-op
// ——否则 DeleteTeam 已删除的 Run（journal 事实仍在）会借重建复活。
func TestStoreRebuildIsNoopOnHealthyLedger(t *testing.T) {
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	seed := LoadStore(teamsPath, runsPath)
	if err := seed.SaveTeam(sampleTeam("t1")); err != nil {
		t.Fatal(err)
	}
	store := LoadStore(teamsPath, runsPath)
	if store.RunsLedgerCorrupt() {
		t.Fatal("健康账本不应标记损坏")
	}
	store.RebuildRunsFromJournal([]*Event{
		{ID: "e1", RunID: "r1", Type: EventRunCreated, Source: "user",
			Payload: map[string]any{"teamId": "t1", "goal": "g", "mode": "single", "taskCount": jsoncompat.Num(1)}, CreatedAt: testTime},
	})
	if store.Run("r1") != nil {
		t.Fatal("健康账本上重放 journal 不得新增 Run")
	}
}

func sampleTeam(id string) *Team {
	return &Team{
		ID:   id,
		Name: "应用开发组",
		Members: []TeamMember{
			{ID: "lead", SessionID: "codex-1", AgentKind: "codex", Name: "主控", Role: RoleLead, Enabled: true},
			{ID: "reviewer", SessionID: "kimi-1", AgentKind: "kimi", Name: "审阅", Role: RoleReviewer, Enabled: true},
		},
		LeadID:    "lead",
		Policy:    DefaultPolicy(),
		CreatedAt: testTime,
		UpdatedAt: testTime,
	}
}

func sampleRun(id, teamID string) *Run {
	return &Run{
		ID:        id,
		TeamID:    teamID,
		Goal:      "实现用户头像上传接口，并补齐测试",
		Status:    RunCreated,
		Round:     1,
		PlanHash:  "deadbeefcafe",
		Tasks:     []Task{sampleTask(), {ID: "review", MemberID: "reviewer", Prompt: "检查实现并补测试", InputHash: "hash-review", Attempt: 1, DependsOn: []string{"design"}, WriteLock: "none", Status: TaskPlanned}},
		CreatedAt: testTime,
		UpdatedAt: testTime,
	}
}

func sampleTask() Task {
	return Task{
		ID:               "design",
		MemberID:         "lead",
		Prompt:           "分析需求并拆分任务",
		InputHash:        "hash-design",
		Attempt:          1,
		Status:           TaskCompleted,
		SubmissionID:     "sub-1",
		Result:           "完成接口设计。",
		ResultDetail:     &TaskResult{SessionID: "codex-1", TurnID: "turn-7", Result: "完成接口设计。", ResultChars: 7, Truncated: false, StopReason: "end_turn"},
		InstructionsHead: InstructionsHead("分析需求并拆分任务"),
	}
}

// ---- journal 契约（§13.1 team/journal）----

func journalLine(event *Event) []byte {
	return append(jsoncompat.EncodeCompact(eventToObject(event)), '\n')
}

func sampleEvent(id string, createdAt time.Time) *Event {
	return &Event{
		ID:        id,
		RunID:     "r1",
		Type:      EventRunCreated,
		Source:    "user",
		Payload:   map[string]any{"goal": "上传接口", "ordinal": 1},
		CreatedAt: createdAt,
	}
}

func TestJournalAppendReplayEquivalence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if journal.LastSequence() != 0 {
		t.Fatalf("空 journal LastSequence 应为 0，得到 %d", journal.LastSequence())
	}
	if replayed, err := journal.Replay(); err != nil || len(replayed) != 0 {
		t.Fatalf("空 journal Replay 应为空: %v %+v", err, replayed)
	}
	events := []*Event{
		sampleEvent("e1", testTime),
		{ID: "e2", RunID: "r1", Type: EventTaskPlanned, Source: "planner", Payload: map[string]any{"taskId": "design", "nested": map[string]any{"round": 1}}, IdempotencyKey: "team:t1:run:r1:task:design:attempt:1", CreatedAt: testTime.Add(time.Second)},
		{ID: "e3", RunID: "r2", Type: EventRunCompleted, Source: "reducer", Payload: nil, CreatedAt: testTime.Add(2 * time.Second)},
	}
	for index, event := range events {
		if err := journal.Append(event); err != nil {
			t.Fatalf("Append %s: %v", event.ID, err)
		}
		if event.Sequence != uint64(index+1) {
			t.Fatalf("Append 应盖写单调序号：%s 期望 %d 得到 %d", event.ID, index+1, event.Sequence)
		}
	}
	if journal.LastSequence() != 3 {
		t.Fatalf("LastSequence 应为 3，得到 %d", journal.LastSequence())
	}
	replayed, err := journal.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(replayed) != len(events) {
		t.Fatalf("Replay 数量不一致：%d vs %d", len(replayed), len(events))
	}
	for index := range events {
		want := jsoncompat.EncodeCompact(eventToObject(events[index]))
		got := jsoncompat.EncodeCompact(eventToObject(replayed[index]))
		if !bytes.Equal(want, got) {
			t.Fatalf("第 %d 条事件回放不一致:\n期望 %s\n得到 %s", index+1, want, got)
		}
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后序号延续、既有记录仍可重放。
	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("重开 journal: %v", err)
	}
	defer reopened.Close()
	if reopened.LastSequence() != 3 {
		t.Fatalf("重开后 LastSequence 应为 3，得到 %d", reopened.LastSequence())
	}
	fourth := sampleEvent("e4", testTime.Add(3*time.Second))
	if err := reopened.Append(fourth); err != nil {
		t.Fatal(err)
	}
	if fourth.Sequence != 4 {
		t.Fatalf("续写序号应为 4，得到 %d", fourth.Sequence)
	}
	if replayed, err = reopened.Replay(); err != nil || len(replayed) != 4 {
		t.Fatalf("续写后 Replay 应有 4 条: %v %d", err, len(replayed))
	}
}

func TestJournalTornTailTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []*Event{sampleEvent("e1", testTime), sampleEvent("e2", testTime)} {
		if err := journal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	journal.Close()

	// 模拟崩溃残留：半行 JSON 直接落盘（无换行结尾）。
	handle, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte(`{"id":"tor","runId":"r1"`)); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("尾部半行应被容忍: %v", err)
	}
	defer reopened.Close()
	if !strings.Contains(logs.String(), "截断") {
		t.Fatalf("截断残留必须留痕: %q", logs.String())
	}
	if reopened.LastSequence() != 2 {
		t.Fatalf("半行不得计入序号，LastSequence 应为 2，得到 %d", reopened.LastSequence())
	}
	replayed, err := reopened.Replay()
	if err != nil || len(replayed) != 2 {
		t.Fatalf("半行不得进入重放: %v %d", err, len(replayed))
	}
	third := sampleEvent("e3", testTime)
	if err := reopened.Append(third); err != nil {
		t.Fatal(err)
	}
	if third.Sequence != 3 {
		t.Fatalf("修复后续写序号应为 3，得到 %d", third.Sequence)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"tor"`)) {
		t.Fatalf("残留半行应被截断:\n%s", raw)
	}
	if got := bytes.Count(raw, []byte("\n")); got != 3 {
		t.Fatalf("修复后应有 3 个完整行尾，得到 %d:\n%s", got, raw)
	}
	if replayed, err = reopened.Replay(); err != nil || len(replayed) != 3 {
		t.Fatalf("修复后 Replay 应有 3 条: %v %d", err, len(replayed))
	}
}

func TestJournalCompleteTailWithoutNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "team-journal.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	first := sampleEvent("e1", testTime)
	second := sampleEvent("e2", testTime)
	if err := journal.Append(first); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(second); err != nil {
		t.Fatal(err)
	}
	journal.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// 剥掉最后一个换行：内容完整但未终止的尾行。
	if err := os.WriteFile(path, raw[:len(raw)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenJournal(path)
	if err != nil {
		t.Fatalf("完整尾行缺换行应被接受: %v", err)
	}
	defer reopened.Close()
	if reopened.LastSequence() != 2 {
		t.Fatalf("完整尾行应计入，LastSequence 应为 2，得到 %d", reopened.LastSequence())
	}
	replayed, err := reopened.Replay()
	if err != nil || len(replayed) != 2 {
		t.Fatalf("完整尾行应进入重放: %v %d", err, len(replayed))
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("打开时应补回缺失的换行:\n%q", raw)
	}
	third := sampleEvent("e3", testTime)
	if err := reopened.Append(third); err != nil {
		t.Fatal(err)
	}
	if third.Sequence != 3 {
		t.Fatalf("续写序号应为 3，得到 %d", third.Sequence)
	}
}

func TestJournalRejectsCorruption(t *testing.T) {
	good := func(id string, sequence uint64) []byte {
		event := sampleEvent(id, testTime)
		event.Sequence = sequence
		return journalLine(event)
	}
	cases := []struct {
		name    string
		lines   []byte
		wantErr string
	}{
		{"重复序号", append(append([]byte{}, good("e1", 1)...), good("e2", 1)...), "重复或回退"},
		{"回退序号", append(append([]byte{}, good("e1", 5)...), good("e2", 3)...), "重复或回退"},
		{"缺失序号", []byte(`{"id":"e1","runId":"r1","type":"run_created","source":"user"}` + "\n"), "sequence"},
		{"中途坏行", append(append(append([]byte{}, good("e1", 1)...), []byte("garbage\n")...), good("e2", 2)...), "解析失败"},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "team-journal.jsonl")
			if err := os.WriteFile(path, testCase.lines, 0o600); err != nil {
				t.Fatal(err)
			}
			journal, err := OpenJournal(path)
			if err == nil {
				journal.Close()
				t.Fatalf("应报错（含 %q），实际成功", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("错误信息应包含 %q，得到 %v", testCase.wantErr, err)
			}
		})
	}
}

// ---- EventFeed 投影（§10.3 changes/reset 契约）----

func feedEvent(sequence uint64, runID string) *Event {
	return &Event{
		ID:        fmt.Sprintf("e%d", sequence),
		RunID:     runID,
		Type:      EventTaskRunning,
		Source:    "scheduler",
		Payload:   map[string]any{"taskId": "design"},
		Sequence:  sequence,
		CreatedAt: testTime.Add(time.Duration(sequence) * time.Second),
	}
}

// rowSequences 提取 Changes 结果里各行的事件序号。
func rowSequences(t *testing.T, result *jsoncompat.Object) []uint64 {
	t.Helper()
	rawRows, ok := result.Get("rows")
	if !ok {
		t.Fatalf("Changes 结果缺少 rows: %s", jsoncompat.EncodePythonDefault(result))
	}
	rows, ok := rawRows.(*jsoncompat.Array)
	if !ok {
		t.Fatalf("rows 不是数组: %T", rawRows)
	}
	sequences := []uint64{}
	for _, item := range rows.Items {
		row, ok := item.(*jsoncompat.Object)
		if !ok {
			t.Fatalf("row 不是对象: %T", item)
		}
		number, ok := row.MustGet("sequence").(json.Number)
		if !ok {
			t.Fatalf("row 缺少 sequence: %s", jsoncompat.EncodePythonDefault(row))
		}
		value, err := number.Int64()
		if err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, uint64(value))
	}
	return sequences
}

func TestEventFeedChangesContract(t *testing.T) {
	feed := NewEventFeed("r1", nil)
	meta := jsoncompat.NewObject()
	meta.Set("status", RunCreated)
	feed.SetMeta(meta)
	for sequence := uint64(1); sequence <= 3; sequence++ {
		if !feed.Add(feedEvent(sequence, "r1")) {
			t.Fatalf("事件 %d 应被接受", sequence)
		}
	}
	epoch := feed.Epoch()

	page := feed.Changes(0, epoch)
	if got := rowSequences(t, page); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("首次全量应返回 1..3，得到 %v", got)
	}
	if _, reset := page.Get("reset"); reset {
		t.Fatal("epoch 匹配且未滑出缓冲时不应 reset")
	}
	if got := page.MustGet("sequence").(json.Number).String(); got != "3" {
		t.Fatalf("sequence 应为 3，得到 %s", got)
	}
	if got := page.MustGet("meta").(*jsoncompat.Object).MustGet("status"); got != RunCreated {
		t.Fatalf("meta 应回显，得到 %v", got)
	}

	if got := rowSequences(t, feed.Changes(2, epoch)); !reflect.DeepEqual(got, []uint64{3}) {
		t.Fatalf("after=2 应只返回 3，得到 %v", got)
	}
	if got := rowSequences(t, feed.Changes(3, epoch)); len(got) != 0 {
		t.Fatalf("after=最新时增量应为空，得到 %v", got)
	}

	// epoch 不匹配 → reset 整页。
	page = feed.Changes(3, "bogus-epoch")
	if value, _ := page.Get("reset"); value != true {
		t.Fatalf("epoch 不匹配应 reset，得到 %s", jsoncompat.EncodePythonDefault(page))
	}
	if got := rowSequences(t, page); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("reset 页应携带最近 ≤20 条，得到 %v", got)
	}

	// 未知游标 → reset。
	page = feed.Changes(99, epoch)
	if value, _ := page.Get("reset"); value != true {
		t.Fatal("未知游标应 reset")
	}
}

func TestEventFeedRingEviction(t *testing.T) {
	feed := NewEventFeed("r1", nil)
	for sequence := uint64(1); sequence <= 600; sequence++ {
		feed.Add(feedEvent(sequence, "r1"))
	}
	if got := len(feed.Events()); got != feedRingCapacity {
		t.Fatalf("环形缓冲应保留 %d 条，得到 %d", feedRingCapacity, got)
	}
	epoch := feed.Epoch()

	// after=0 但最早事件已被挤出 → reset，整页只带最近 20 条。
	page := feed.Changes(0, epoch)
	if value, _ := page.Get("reset"); value != true {
		t.Fatal("游标滑出缓冲应 reset")
	}
	want := make([]uint64, 0, feedResetRows)
	for sequence := uint64(600 - feedResetRows + 1); sequence <= 600; sequence++ {
		want = append(want, sequence)
	}
	if got := rowSequences(t, page); !reflect.DeepEqual(got, want) {
		t.Fatalf("reset 页应携带最近 %d 条，得到 %v", feedResetRows, got)
	}

	// 缓冲内的游标正常增量。
	if got := rowSequences(t, feed.Changes(150, epoch)); len(got) != 450 {
		t.Fatalf("after=150 应返回 450 条，得到 %d", len(got))
	}
	if got := rowSequences(t, feed.Changes(600, epoch)); len(got) != 0 {
		t.Fatalf("after=最新应为空，得到 %d", len(got))
	}

	// 被挤出的游标 → reset。
	if page := feed.Changes(100, epoch); func() bool { value, _ := page.Get("reset"); return value != true }() {
		t.Fatal("被挤出的游标应 reset")
	}

	// 其他 Run 的事件不得进入本 feed。
	if feed.Add(feedEvent(601, "r-other")) {
		t.Fatal("其他 Run 的事件应被拒绝")
	}
	if got := rowSequences(t, feed.Changes(600, epoch)); len(got) != 0 {
		t.Fatalf("其他 Run 的事件不得进入增量，得到 %v", got)
	}
	if feed.Add(feedEvent(0, "r1")) {
		t.Fatal("序号 0（未落账）的事件应被拒绝")
	}
}

func TestEventFeedSeedCursorNotify(t *testing.T) {
	seed := []*Event{
		feedEvent(1, "r1"),
		feedEvent(2, "r1"),
		feedEvent(7, "r2"), // 其他 Run：忽略
		feedEvent(3, "r1"),
	}
	feed := NewEventFeed("r1", seed)
	if feed.Sequence() != 3 {
		t.Fatalf("种子事件应过滤 Run，最新序号应为 3，得到 %d", feed.Sequence())
	}
	epoch := feed.Epoch()
	if got := rowSequences(t, feed.Changes(0, epoch)); !reflect.DeepEqual(got, []uint64{1, 2, 3}) {
		t.Fatalf("种子事件应可全量读取，得到 %v", got)
	}
	if cursor := feed.Cursor(); cursor != epoch+".3" {
		t.Fatalf("Cursor 应为 epoch.sequence，得到 %s", cursor)
	}

	// Notify：非阻塞信号，容量 1；先排干种子注入的信号。
	notify := feed.Notify()
	for {
		select {
		case <-notify:
			continue
		default:
		}
		break
	}
	if !feed.Add(feedEvent(4, "r1")) {
		t.Fatal("事件 4 应被接受")
	}
	select {
	case <-notify:
	default:
		t.Fatal("Add 之后应能立刻收到 Notify 信号（channel 同步，不依赖 sleep）")
	}
}

// ---- 模型常量与助手 ----

func TestDefaultPolicy(t *testing.T) {
	policy := DefaultPolicy()
	want := TeamPolicy{
		MaxParallelTasks:       3,
		MinParallelTasks:       1,
		MaxProviderConcurrency: 5,
		MaxTaskRetries:         1, // §6.3：限流/瞬态错误的受限重试（阶段二）
		MaxRounds:              3,
		MaxTasksPerRun:         12,
		MaxContextChars:        12000,
		MaxResultChars:         12000,
		TaskTimeoutSecs:        1800,
		RunTimeoutSecs:         7200,
		BudgetUnits:            0,
		WorkspaceMode:          WorkspaceShared,
		WritePolicy:            WritePolicyExclusive,
		AutoDispatch:           false,
		AutoApplyChanges:       false,
	}
	if policy != want {
		t.Fatalf("DefaultPolicy 与 §4.2 不一致:\n得到 %+v\n期望 %+v", policy, want)
	}
}

func TestInstructionsHead(t *testing.T) {
	if got := InstructionsHead("短提示"); got != "短提示" {
		t.Fatalf("短文本不应截断，得到 %q", got)
	}
	if got := InstructionsHead(strings.Repeat("a", 300)); len(got) != 240 {
		t.Fatalf("ASCII 应截 240 字符，得到 %d", len(got))
	}
	got := InstructionsHead(strings.Repeat("汉", 300))
	if count := utf8.RuneCountInString(got); count != 240 {
		t.Fatalf("多字节文本应按 rune 截 240，得到 %d", count)
	}
	if got := InstructionsHead(strings.Repeat("a", 240)); got != strings.Repeat("a", 240) {
		t.Fatal("恰好 240 字符不应截断")
	}
	if got := InstructionsHead(strings.Repeat("a", 241)); len(got) != 240 {
		t.Fatalf("241 字符应截为 240，得到 %d", len(got))
	}
}

func TestValidMemberID(t *testing.T) {
	for _, id := range []string{"lead", "a", "a-b_C9", strings.Repeat("m", 64)} {
		if !ValidMemberID(id) {
			t.Fatalf("%q 应是合法成员 id", id)
		}
	}
	for _, id := range []string{"", strings.Repeat("m", 65), "bad.id", "成员", "a b"} {
		if ValidMemberID(id) {
			t.Fatalf("%q 不应是合法成员 id", id)
		}
	}
}

func TestEventMarshalRoundTrip(t *testing.T) {
	event := &Event{
		ID:     "e1",
		RunID:  "r1",
		Type:   EventTaskInputHashMismatch,
		Source: "recovery",
		Payload: map[string]any{
			"taskId":   "design",
			"expected": "hash-a",
			"actual":   "hash-b",
			"nested":   map[string]any{"items": []any{1, "x", true}},
		},
		IdempotencyKey: "team:t1:run:r1:task:design:attempt:1",
		Sequence:       12,
		CreatedAt:      testTime,
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	want := jsoncompat.EncodeCompact(eventToObject(event))
	got := jsoncompat.EncodeCompact(eventToObject(&decoded))
	if !bytes.Equal(want, got) {
		t.Fatalf("Event 序列化往返不一致:\n期望 %s\n得到 %s", want, got)
	}
	if decoded.Payload["taskId"] != "design" || decoded.CreatedAt != event.CreatedAt {
		t.Fatalf("Event 字段往返不一致: %+v", decoded)
	}
}

// ---- 并发（供 -race 暴露数据竞争）----

func TestStoreConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	store := LoadStore(filepath.Join(dir, "teams.json"), filepath.Join(dir, "team-runs.json"))
	if err := store.SaveTeam(sampleTeam("shared")); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	// 各写各的 team：版本链互不干扰。
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			team := sampleTeam(fmt.Sprintf("t%d", worker))
			for round := 0; round < 10; round++ {
				team.Name = fmt.Sprintf("写手 %d 第 %d 轮", worker, round)
				if err := store.SaveTeam(team); err != nil {
					t.Errorf("独立写手不应冲突: %v", err)
					return
				}
			}
		}()
	}
	// 四个竞争者改同一个 team：冲突-重载-重试，各完成 5 次成功写入。
	const contenders = 4
	const winsEach = 5
	for worker := 0; worker < contenders; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			candidate := store.Team("shared")
			for successes := 0; successes < winsEach; {
				err := store.SaveTeam(candidate)
				if err == nil {
					successes++
					candidate.UpdatedAt = candidate.UpdatedAt.Add(time.Second)
					continue
				}
				if !errors.Is(err, ErrVersionConflict) {
					t.Errorf("竞争写入只应报版本冲突: %v", err)
					return
				}
				fresh := store.Team("shared")
				if fresh == nil {
					t.Error("共享 team 不应消失")
					return
				}
				candidate = fresh
			}
		}()
	}
	// 读者持续快照（独立 WaitGroup：写手全部结束后停止）。
	var readers sync.WaitGroup
	stopReaders := make(chan struct{})
	for reader := 0; reader < 2; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				for _, team := range store.Teams() {
					_ = team.Version
				}
				_ = store.Team("shared")
			}
		}()
	}
	wg.Wait()
	close(stopReaders)
	readers.Wait()
	if got := store.Team("t0"); got == nil || got.Version != 10 {
		t.Fatalf("独立写手的版本链应为 10，得到 %+v", got)
	}
	if got := store.Team("shared"); got == nil || got.Version != 1+contenders*winsEach {
		t.Fatalf("竞争链版本应为 %d，得到 %d", 1+contenders*winsEach, got.Version)
	}
}
