package team

// worktree_test 是 isolated_worktree 引用与 diff 数据面的单元测试（§6.4
// 阶段二、§12 阶段二工作项 5，team-worktree 工作流必测清单）：绑定成功
// （baseRevision + workspace_bound 随派发同次记账）、成员会话 cwd 不一致拒、
// 注册表缺失/未装配拒、序列化往返（baseRevision 落盘重载仍在）、worktree
// 写锁与共享 cwd 锁互斥、重启恢复三态（路径删除/HEAD 前移→waiting_input，
// 脏工作区→不误报）、TaskDiff 基线门槛与截断透传、Cleanup 脏区失败落事件
// 且成功不动任务终态。fixture 是测试内 os/exec 的真实 git 仓库（git init +
// worktree add），产品代码只经 WorkspaceAdapter 端口访问。

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// ---- 真实 git fixture ----

// teamWorktreeGit 在 dir 下执行固定 argv 的 git 命令，失败即 Fatal。
func teamWorktreeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.quotePath=false", "-C", dir}, args...)...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v 失败: %v: %s", args, err, stderr.String())
	}
	return string(out)
}

// newTeamGitWorktreeFixture 建一个真实 git 仓库 + 一个 worktree，返回
// (worktreePath, headRevision)；base 提交含一行 a.txt。
func newTeamGitWorktreeFixture(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("测试需要 git: %v", err)
	}
	root := t.TempDir()
	main := filepath.Join(root, "main")
	worktree := filepath.Join(root, "wt")
	if err := os.MkdirAll(main, 0o700); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	teamWorktreeGit(t, main, "init", "-q")
	teamWorktreeGit(t, main, "config", "user.email", "team@test")
	teamWorktreeGit(t, main, "config", "user.name", "team test")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	teamWorktreeGit(t, main, "add", ".")
	teamWorktreeGit(t, main, "commit", "-qm", "base")
	teamWorktreeGit(t, main, "worktree", "add", "-q", worktree, "-b", "wt-task")
	t.Cleanup(func() {
		// 尽力而为：测试可能已删除 worktree，目录交给 TempDir 兜底。
		_ = exec.Command("git", "-C", main, "worktree", "remove", "--force", worktree).Run()
	})
	head := strings.TrimSpace(teamWorktreeGit(t, worktree, "rev-parse", "HEAD"))
	return worktree, head
}

// gitTestWorkspaceAdapter 把真实 git worktree 映射成 WorkspaceAdapter（注册
// 表是内存 map；HEAD/cleanup 落真实 git CLI）。Diff 只回原始整段——双层
// 截断是 httpd 适配器的职责，team 侧断言经 stubWorkspaceAdapter 验证透传。
type gitTestWorkspaceAdapter struct {
	mu    sync.Mutex
	paths map[string]string
}

func (a *gitTestWorkspaceAdapter) Resolve(workspaceID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	path, ok := a.paths[workspaceID]
	if !ok || path == "" {
		return "", errors.New("工作区未注册：" + workspaceID)
	}
	return path, nil
}

func (a *gitTestWorkspaceAdapter) HeadRevision(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "HEAD")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New(stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func (a *gitTestWorkspaceAdapter) Diff(path, baseRevision string) ([]WorkspaceDiff, bool, error) {
	cmd := exec.Command("git", "-C", path, "diff", baseRevision, "--unified=3")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, false, errors.New(stderr.String())
	}
	text := string(out)
	if strings.TrimSpace(text) == "" {
		return nil, false, nil
	}
	return []WorkspaceDiff{{Path: "(all)", DiffText: text}}, false, nil
}

func (a *gitTestWorkspaceAdapter) Cleanup(workspaceID string) error {
	path, err := a.Resolve(workspaceID)
	if err != nil {
		return err
	}
	cmd := exec.Command("git", "-C", path, "worktree", "remove", path)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return errors.New(stderr.String())
	}
	return nil
}

// stubWorkspaceAdapter 是 TaskDiff/Cleanup 的行为注入桩（截断标记与错误
// 都由测试手工设定，验证 Service 层只做门槛与透传）。
type stubWorkspaceAdapter struct {
	mu            sync.Mutex
	path          string
	diffFiles     []WorkspaceDiff
	diffTruncated bool
	diffErr       error
	cleanupErr    error
	cleanupCalls  int
}

func (a *stubWorkspaceAdapter) Resolve(workspaceID string) (string, error) {
	if a.path == "" {
		return "", errors.New("工作区未注册：" + workspaceID)
	}
	return a.path, nil
}

func (a *stubWorkspaceAdapter) HeadRevision(path string) (string, error) {
	return "stub-head", nil
}

func (a *stubWorkspaceAdapter) Diff(path, baseRevision string) ([]WorkspaceDiff, bool, error) {
	return a.diffFiles, a.diffTruncated, a.diffErr
}

func (a *stubWorkspaceAdapter) Cleanup(workspaceID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cleanupCalls++
	return a.cleanupErr
}

// newWorktreeHarness 在 dispatchHarness 基础上接入真实 git worktree。
func newWorktreeHarness(t *testing.T) (*dispatchHarness, *gitTestWorkspaceAdapter, string, string) {
	t.Helper()
	h := newDispatchHarness(t)
	worktree, head := newTeamGitWorktreeFixture(t)
	adapter := &gitTestWorkspaceAdapter{paths: map[string]string{"wt-1": worktree}}
	h.service.Workspaces = adapter
	return h, adapter, worktree, head
}

// isolatedPlan 返回单条 isolated 任务计划（exclusive 写锁）。
func isolatedPlan() []PlanTaskInput {
	return []PlanTaskInput{{
		ID: "iso", MemberID: "dev", Prompt: "在独立工作区实现实验特性",
		WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1",
		WriteLock: TaskWriteLockExclusive,
	}}
}

// findEvent 返回 run 内指定类型的首个 journal 事件（无则 nil）。
func findEvent(t *testing.T, journal *Journal, runID, eventType string) *Event {
	t.Helper()
	events, err := journal.Replay()
	if err != nil {
		t.Fatalf("journal 重放失败: %v", err)
	}
	for _, event := range events {
		if event.RunID == runID && event.Type == eventType {
			return event
		}
	}
	return nil
}

// ---- 绑定与派发 ----

// TestIsolatedDispatchBindsBaseRevision：成员会话开在 worktree 里时，isolated
// 任务首派在同一 commitRun 写入 BaseRevision 并追加 workspace_bound 事件；
// 写锁键是 worktree 路径；baseRevision 随 Run 序列化落盘（重启重载仍在）。
func TestIsolatedDispatchBindsBaseRevision(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	h.resolver.setCwd("s-dev", worktree)
	run := h.createRun(t, ModePipeline, isolatedPlan())

	_, _ = h.waitForSubmission(t, "s-dev", "isolated 任务派发")
	task := h.taskOf(run.ID, "iso")
	if task.Status != TaskDispatching && task.Status != TaskRunning {
		t.Fatalf("任务应在途，实际 %s", task.Status)
	}
	if task.BaseRevision != head {
		t.Fatalf("首派应记录基线 HEAD，实际 %q 期望 %q", task.BaseRevision, head)
	}
	// 写锁键 = worktree 路径（§6.4 规则 4）。
	waitFor(t, "worktree 写锁登记", func() bool {
		return h.service.locks.Holder(worktree) == "iso"
	})
	// workspace_bound 事件随派发同次记账（journal 先行）。
	bound := findEvent(t, h.journal, run.ID, EventWorkspaceBound)
	if bound == nil {
		t.Fatal("缺少 workspace_bound 事件")
	}
	if bound.Payload["taskId"] != "iso" || bound.Payload["workspaceId"] != "wt-1" ||
		bound.Payload["path"] != worktree || bound.Payload["baseRevision"] != head {
		t.Fatalf("workspace_bound 载荷不完整: %+v", bound.Payload)
	}
	if dispatch := findEvent(t, h.journal, run.ID, EventTaskDispatching); dispatch == nil {
		t.Fatal("缺少 task_dispatching 事件")
	}

	// 序列化往返：含 baseRevision 的 Run 经 JSON 账本落盘重载仍在（漏 Set
	// 分支会让重启恢复误判 workspace_lost——评审指出的静默失效模式）。
	dir := t.TempDir()
	teamsPath := filepath.Join(dir, "teams.json")
	runsPath := filepath.Join(dir, "team-runs.json")
	jsonStore := LoadStore(teamsPath, runsPath)
	teamCopy := h.team.Clone()
	teamCopy.Version = 0
	if err := jsonStore.SaveTeam(teamCopy); err != nil {
		t.Fatalf("团队落盘失败: %v", err)
	}
	runCopy := h.store.Run(run.ID)
	runCopy.Version = 0
	if err := jsonStore.SaveRun(runCopy); err != nil {
		t.Fatalf("Run 落盘失败: %v", err)
	}
	reloaded := LoadStore(teamsPath, runsPath).Run(run.ID)
	if reloaded == nil || findTaskIn(reloaded, "iso") == nil {
		t.Fatal("重载后 Run/任务丢失")
	}
	if got := findTaskIn(reloaded, "iso").BaseRevision; got != head {
		t.Fatalf("baseRevision 应随 Run 持久化，重载后 %q 期望 %q", got, head)
	}
	raw, err := os.ReadFile(runsPath)
	if err != nil {
		t.Fatalf("读取账本失败: %v", err)
	}
	if !strings.Contains(string(raw), "\"baseRevision\"") {
		t.Fatal("team-runs.json 应包含 baseRevision 键（手工序列化的 Set 分支）")
	}
}

// TestIsolatedDispatchRejectsCwdMismatch：成员会话不在 worktree 里时派发
// 确定性失败（绝不 spawn 新会话），无入队、无基线、无绑定事件。
func TestIsolatedDispatchRejectsCwdMismatch(t *testing.T) {
	h, _, _, _ := newWorktreeHarness(t)
	h.resolver.setCwd("s-dev", "/tmp/别的目录")
	run := h.createRun(t, ModePipeline, isolatedPlan())

	waitFor(t, "任务确定性失败", func() bool {
		return h.taskOf(run.ID, "iso").Status == TaskFailed
	})
	task := h.taskOf(run.ID, "iso")
	if !strings.Contains(task.Error, "成员会话工作区与任务工作区不一致") {
		t.Fatalf("失败原因应指向会话目录，实际 %q", task.Error)
	}
	if task.BaseRevision != "" {
		t.Fatalf("cwd 不一致不应记录基线，实际 %q", task.BaseRevision)
	}
	if entries := h.queue.entriesFor("s-dev"); len(entries) != 0 {
		t.Fatalf("拒绝路径绝不入队，实际 %d 条", len(entries))
	}
	if findEvent(t, h.journal, run.ID, EventWorkspaceBound) != nil {
		t.Fatal("拒绝路径不应有 workspace_bound 事件")
	}
}

// TestIsolatedDispatchRejectsUnknownWorkspace：注册表里没有该 workspaceId
// （或端口未装配）都是确定性失败，任务落 failed 而非静默搁置。
func TestIsolatedDispatchRejectsUnknownWorkspace(t *testing.T) {
	h, _, _, _ := newWorktreeHarness(t)
	h.resolver.setCwd("s-dev", "/tmp/任何目录")
	run := h.createRun(t, ModePipeline, []PlanTaskInput{{
		ID: "iso", MemberID: "dev", Prompt: "引用不存在的工作区",
		WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-missing",
	}})
	waitFor(t, "未注册工作区失败", func() bool {
		return h.taskOf(run.ID, "iso").Status == TaskFailed
	})
	if errText := h.taskOf(run.ID, "iso").Error; !strings.Contains(errText, "工作区不存在") {
		t.Fatalf("失败原因应为工作区不存在，实际 %q", errText)
	}
}

// TestIsolatedDispatchWithoutAdapterFailsDeterministically：网关未装配
// WorkspaceAdapter 时 isolated 任务确定性失败（可见的配置错误，而非悬置）。
func TestIsolatedDispatchWithoutAdapterFailsDeterministically(t *testing.T) {
	h := newDispatchHarness(t) // Workspaces 保持 nil
	h.resolver.setCwd("s-dev", "/tmp/lead")
	run := h.createRun(t, ModePipeline, isolatedPlan())
	waitFor(t, "未装配端口失败", func() bool {
		return h.taskOf(run.ID, "iso").Status == TaskFailed
	})
	if errText := h.taskOf(run.ID, "iso").Error; !strings.Contains(errText, ErrWorkspacesUnavailable.Error()) {
		t.Fatalf("失败原因应为未装配工作区注册表，实际 %q", errText)
	}
}

// ---- 写锁互斥（§6.4 规则 4：worktree 路径与共享 cwd 同一 LockRegistry）----

// TestIsolatedWorktreeWriteLockMutualExclusion：同一 worktree 的两个 isolated
// 写任务互斥（第二个暂缓）；共享 cwd 锁与 worktree 锁共用注册表——持有
// worktree 锁期间，同路径的共享 exclusive 任务同样取不到锁。
func TestIsolatedWorktreeWriteLockMutualExclusion(t *testing.T) {
	h, _, worktree, _ := newWorktreeHarness(t)
	run := &Run{
		ID: "run-iso-lock", TeamID: h.team.ID, Goal: "g", Mode: ModePipeline,
		Status: RunDispatching,
		Tasks: []Task{
			{ID: "iso1", MemberID: "dev", Prompt: "写一", Status: TaskPlanned, Ordinal: 0,
				WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1", WriteLock: TaskWriteLockExclusive},
			{ID: "iso2", MemberID: "rev", Prompt: "写二", Status: TaskPlanned, Ordinal: 1,
				WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1", WriteLock: TaskWriteLockExclusive},
		},
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatalf("播种 run 失败: %v", err)
	}
	h.resolver.setCwd("s-dev", worktree)
	h.resolver.setCwd("s-rev", worktree)

	state := h.service.stateFor(run.ID)
	state.mu.Lock()
	first := findTaskIn(h.store.Run(run.ID), "iso1")
	binding1, err := h.service.resolveWorkspaceBinding(first, memberOf(h.team, "dev"))
	if err != nil {
		state.mu.Unlock()
		t.Fatalf("第一个任务绑定失败: %v", err)
	}
	dispatch1, err := h.service.accountDispatchLocked(state, h.team, run, "iso1", binding1.path, binding1)
	if err != nil || dispatch1 == nil {
		state.mu.Unlock()
		t.Fatalf("第一个 isolated 任务应派发成功: dispatch=%v err=%v", dispatch1, err)
	}
	second := findTaskIn(h.store.Run(run.ID), "iso2")
	binding2, err := h.service.resolveWorkspaceBinding(second, memberOf(h.team, "rev"))
	if err != nil {
		state.mu.Unlock()
		t.Fatalf("第二个任务绑定失败: %v", err)
	}
	dispatch2, err := h.service.accountDispatchLocked(state, h.team, run, "iso2", binding2.path, binding2)
	state.mu.Unlock()
	if err != nil {
		t.Fatalf("同 worktree 第二写应暂缓而非失败: %v", err)
	}
	if dispatch2 != nil {
		t.Fatal("同 worktree 第二写必须暂缓（nil, nil），实际拿到派发产物")
	}
	if cwd := state.taskCwds["iso1"]; cwd != worktree {
		t.Fatalf("isolated 写锁登记应为 worktree 路径，实际 %q", cwd)
	}
	// 共享 cwd 键与 worktree 键同一注册表：同路径的外来写任务取不到锁。
	if h.service.locks.Acquire(worktree, "trespasser") {
		t.Fatal("worktree 锁被 iso1 持有时，同路径的任何写任务都不应获得锁")
	}
}

// ---- 重启恢复（§6.4：找不到 worktree 或基线不匹配 → waiting_input）----

// seedInflightIsolatedRun 播种一个「已派发、在途」的 isolated Run（journal
// 记账 + 快照 + 可选账本条目），模拟重启前的状态。
func (h *dispatchHarness) seedInflightIsolatedRun(t *testing.T, runID, baseRevision string, seedQueue func(submissionID string)) *Run {
	t.Helper()
	task := Task{
		ID: "iso", MemberID: "dev", Prompt: "隔离实现", Status: TaskDispatching,
		Ordinal: 0, Attempt: 1, WriteLock: TaskWriteLockExclusive,
		WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1",
		BaseRevision: baseRevision,
	}
	run := &Run{ID: runID, TeamID: h.team.ID, Goal: "g", Mode: ModePipeline, Status: RunDispatching, Tasks: []Task{task}}
	task.InputHash = InputHashFor(&task, PolicyDigest(h.team.Policy), "")
	submissionID := TaskSubmissionID(h.team.ID, runID, "iso", 1)
	task.SubmissionID = submissionID
	run.Tasks[0] = task
	for _, event := range []*Event{
		newEvent(runID, EventRunCreated, "user", map[string]any{"mode": ModePipeline, "taskCount": jsoncompat.Num(1)}),
		newEvent(runID, EventTaskPlanned, "user", map[string]any{"taskId": "iso", "memberId": "dev"}),
	} {
		if err := h.journal.Append(event); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	dispatch := newEvent(runID, EventTaskDispatching, "runner", map[string]any{
		"taskId": "iso", "sessionId": "s-dev", "submissionId": submissionID,
		"inputHash": task.InputHash, "attempt": jsoncompat.Num(1),
	})
	dispatch.IdempotencyKey = TaskIdempotencyKey(h.team.ID, runID, "iso", 1)
	if err := h.journal.Append(dispatch); err != nil {
		t.Fatalf("journal 预置失败: %v", err)
	}
	if baseRevision != "" {
		if err := h.journal.Append(newEvent(runID, EventWorkspaceBound, "runner", map[string]any{
			"taskId": "iso", "workspaceId": "wt-1", "baseRevision": baseRevision,
		})); err != nil {
			t.Fatalf("journal 预置失败: %v", err)
		}
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	if seedQueue != nil {
		seedQueue(submissionID)
	}
	return run
}

// TestRecoverWorkspaceLostWhenPathDeleted：重启后 worktree 目录已被删除：
// 任务停靠（stopped）+ workspace_lost(path_missing) + run waiting_input，
// 绝不自动创建新 worktree、不重发。
func TestRecoverWorkspaceLostWhenPathDeleted(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	run := h.seedInflightIsolatedRun(t, "run-ws-lost", head, func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-dev", submissionID, ComposeTaskMessage("", "隔离实现")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
	})
	// 重启前 worktree 目录已被外部删除（注册表仍在——路径失效）。
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatalf("删除 worktree 失败: %v", err)
	}
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "任务停靠", func() bool { return h.taskOf(run.ID, "iso").Status == TaskStopped })
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	lost := findEvent(t, h.journal, run.ID, EventWorkspaceLost)
	if lost == nil {
		t.Fatal("缺少 workspace_lost 事件")
	}
	if lost.Payload["reason"] != workspaceReasonPathMissing || lost.Payload["workspaceId"] != "wt-1" {
		t.Fatalf("workspace_lost 载荷不符: %+v", lost.Payload)
	}
	if got := h.queue.callCount(run.Tasks[0].SubmissionID); got != 1 {
		t.Fatalf("工作区丢失不得重发: %d 次", got)
	}
}

// TestRecoverWorkspaceLostWhenBaseChanged：重启后 worktree HEAD 已前移
// （基线变更）：任务停靠 + workspace_lost(base_changed) + waiting_input。
func TestRecoverWorkspaceLostWhenBaseChanged(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	run := h.seedInflightIsolatedRun(t, "run-ws-base", head, func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-dev", submissionID, ComposeTaskMessage("", "隔离实现")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
	})
	// 重启前有人在 worktree 里提交了新 commit（HEAD 前移）。
	if err := os.WriteFile(filepath.Join(worktree, "b.txt"), []byte("前进\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	teamWorktreeGit(t, worktree, "add", ".")
	teamWorktreeGit(t, worktree, "commit", "-qm", "moved on")
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "任务停靠", func() bool { return h.taskOf(run.ID, "iso").Status == TaskStopped })
	waitFor(t, "run 停 waiting_input", func() bool { return h.store.Run(run.ID).Status == RunWaitingInput })
	lost := findEvent(t, h.journal, run.ID, EventWorkspaceLost)
	if lost == nil || lost.Payload["reason"] != workspaceReasonBaseChanged {
		t.Fatalf("应记录 base_changed，实际 %+v", lost)
	}
}

// TestRecoverDirtyWorkspaceAdoptsWithoutFalsePositive：工作区脏（未提交改动
// 是任务的正常产出）不算基线变更——收养通过、watcher 重建、任务保持在途。
func TestRecoverDirtyWorkspaceAdoptsWithoutFalsePositive(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	run := h.seedInflightIsolatedRun(t, "run-ws-dirty", head, func(submissionID string) {
		if _, _, err := h.queue.Enqueue("s-dev", submissionID, ComposeTaskMessage("", "隔离实现")); err != nil {
			t.Errorf("预置账本条目失败: %v", err)
		}
	})
	// 任务在 worktree 里留下了未提交改动（脏区，HEAD 未动）。
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("base\n任务产出\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := h.service.RecoverRun(run.ID); err != nil {
		t.Fatalf("恢复失败: %v", err)
	}
	waitFor(t, "收养 watcher", func() bool {
		state := h.service.stateFor(run.ID)
		state.mu.Lock()
		defer state.mu.Unlock()
		_, watched := state.watchers["iso"]
		return watched
	})
	// 收养成功即证明工作区对账通过：无 workspace_lost、run 不停。
	if findEvent(t, h.journal, run.ID, EventWorkspaceLost) != nil {
		t.Fatal("脏工作区不应误报 workspace_lost")
	}
	if status := h.store.Run(run.ID).Status; status != RunDispatching {
		t.Fatalf("脏工作区不应停靠 run，实际 %s", status)
	}
	if status := h.taskOf(run.ID, "iso").Status; status != TaskDispatching {
		t.Fatalf("任务应保持在途，实际 %s", status)
	}
}

// ---- diff 数据面与清理 ----

// seedCompletedIsolatedRun 播种一个已完成的 isolated Run（含基线）。
func (h *dispatchHarness) seedCompletedIsolatedRun(t *testing.T, runID, baseRevision string) *Run {
	t.Helper()
	run := &Run{
		ID: runID, TeamID: h.team.ID, Goal: "g", Mode: ModePipeline,
		Status: RunCompleted, Round: 1, PlanHash: "hash",
		Tasks: []Task{{
			ID: "iso", MemberID: "dev", Prompt: "隔离实现", Status: TaskCompleted,
			Ordinal: 0, Attempt: 1, Result: "完成",
			WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1",
			BaseRevision: baseRevision, WriteLock: TaskWriteLockExclusive,
		}},
	}
	if err := h.store.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	return run
}

// TestTaskDiffRequiresBaseline：非 isolated 任务或未记录基线（未派发）的
// 任务没有可比较基线，直接报错。
func TestTaskDiffRequiresBaseline(t *testing.T) {
	h, _, _, _ := newWorktreeHarness(t)
	shared := &Run{
		ID: "run-shared", TeamID: h.team.ID, Goal: "g", Status: RunCompleted,
		Tasks: []Task{{ID: "plain", MemberID: "dev", Prompt: "共享任务", Status: TaskCompleted,
			WorkspaceMode: WorkspaceShared, WriteLock: TaskWriteLockExclusive}},
	}
	if err := h.store.SaveRun(shared); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.service.TaskDiff("run-shared", "plain"); err == nil ||
		!strings.Contains(err.Error(), "该任务没有工作区基线") {
		t.Fatalf("共享任务应无基线可 diff，实际 %v", err)
	}
	// isolated 但未派发（无 BaseRevision）同样拒绝。
	pending := &Run{
		ID: "run-pending", TeamID: h.team.ID, Goal: "g", Status: RunDispatching,
		Tasks: []Task{{ID: "iso", MemberID: "dev", Prompt: "隔离", Status: TaskPlanned,
			WorkspaceMode: WorkspaceIsolatedWorktree, WorkspaceID: "wt-1"}},
	}
	if err := h.store.SaveRun(pending); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.service.TaskDiff("run-pending", "iso"); err == nil ||
		!strings.Contains(err.Error(), "该任务没有工作区基线") {
		t.Fatalf("未派发的 isolated 任务应无基线，实际 %v", err)
	}
	if _, _, err := h.service.TaskDiff("run-pending", "不存在"); err == nil {
		t.Fatal("不存在的任务应报错")
	}
	if _, _, err := h.service.TaskDiff("run-missing", "iso"); err == nil {
		t.Fatal("不存在的 run 应报错")
	}
}

// TestTaskDiffPassesThroughBoundedEntries：Service.TaskDiff 透传适配器的
// 逐文件条目与顶层/单文件双层 truncated 标记。
func TestTaskDiffPassesThroughBoundedEntries(t *testing.T) {
	h := newDispatchHarness(t)
	stub := &stubWorkspaceAdapter{path: "/tmp/wt"}
	h.service.Workspaces = stub
	run := h.seedCompletedIsolatedRun(t, "run-diff", "base123")
	stub.diffFiles = []WorkspaceDiff{
		{Path: "a.txt", DiffText: "+one", Added: 1, Removed: 0, Truncated: false},
		{Path: "big.txt", DiffText: strings.Repeat("+", 20000), Added: 20000, Removed: 0, Truncated: true},
	}
	stub.diffTruncated = true
	files, truncated, err := h.service.TaskDiff(run.ID, "iso")
	if err != nil {
		t.Fatalf("diff 失败: %v", err)
	}
	if len(files) != 2 || files[0].Path != "a.txt" || files[1].Path != "big.txt" {
		t.Fatalf("应透传两个文件条目: %+v", files)
	}
	if !files[1].Truncated || files[0].Truncated {
		t.Fatalf("单文件截断标记应逐文件透传: %+v", files)
	}
	if !truncated {
		t.Fatal("顶层截断标记应透传")
	}
	if files[1].Added != 20000 {
		t.Fatalf("Added 计数应透传，实际 %d", files[1].Added)
	}
}

// TestCleanupWorkspaceFailureRecordsEvent：脏 worktree 清理失败——错误上抛
// + workspace_cleanup_failed 事件 + 任务终态不动（清理是工作区操作）。
func TestCleanupWorkspaceFailureRecordsEvent(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	run := h.seedCompletedIsolatedRun(t, "run-cleanup", head)
	// 脏区：git worktree remove（无 --force）拒绝。
	if err := os.WriteFile(filepath.Join(worktree, "dirty.txt"), []byte("用户改动\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := h.service.CleanupWorkspace(run.ID, "iso"); err == nil {
		t.Fatal("脏 worktree 清理应失败")
	}
	failed := findEvent(t, h.journal, run.ID, EventWorkspaceCleanupFailed)
	if failed == nil {
		t.Fatal("清理失败应落 workspace_cleanup_failed 事件")
	}
	if failed.Payload["taskId"] != "iso" || failed.Payload["workspaceId"] != "wt-1" ||
		failed.Payload["error"] == "" {
		t.Fatalf("workspace_cleanup_failed 载荷不完整: %+v", failed.Payload)
	}
	if _, err := os.Stat(filepath.Join(worktree, "dirty.txt")); err != nil {
		t.Fatalf("清理失败绝不能丢用户改动: %v", err)
	}
	if status := h.taskOf(run.ID, "iso").Status; status != TaskCompleted {
		t.Fatalf("清理失败不应动任务终态，实际 %s", status)
	}
	// 共享任务没有可清理的工作区。
	if err := h.service.CleanupWorkspace(run.ID, "plain"); err == nil {
		t.Fatal("任务不存在应报错")
	}
}

// TestCleanupWorkspaceSuccessKeepsTaskState：干净 worktree 清理成功，任务
// 终态与 Run 快照不动（workspaceId/baseRevision 保留供审计与 diff 复查）。
func TestCleanupWorkspaceSuccessKeepsTaskState(t *testing.T) {
	h, _, worktree, head := newWorktreeHarness(t)
	run := h.seedCompletedIsolatedRun(t, "run-clean", head)
	if err := h.service.CleanupWorkspace(run.ID, "iso"); err != nil {
		t.Fatalf("干净 worktree 清理应成功: %v", err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree 应已删除: %v", err)
	}
	task := h.taskOf(run.ID, "iso")
	if task.Status != TaskCompleted || task.WorkspaceID != "wt-1" || task.BaseRevision != head {
		t.Fatalf("清理成功不应动任务快照: %+v", task)
	}
	if findEvent(t, h.journal, run.ID, EventWorkspaceCleanupFailed) != nil {
		t.Fatal("成功清理不应落失败事件")
	}
}
