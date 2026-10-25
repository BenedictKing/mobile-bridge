package httpd

// teams_workspace_test 验证 WorkspaceAdapter 的 git 实现（§6.4 阶段二）：
// 注册表读取（缺失/损坏/未注册）、HEAD 基线读取、unified diff 的逐文件
// 重组与双层截断（单文件 20000 字节 / 整包 50 文件、512000 字节）、
// worktree 清理的脏区拒绝与干净删除。fixture 是测试内 os/exec 的真实
// git 仓库（git init + worktree add），产品代码只经适配器访问。

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// newGitWorktreeFixture 建一个真实 git 仓库 + 一个 worktree，返回
// (mainRepo, worktree)；base 提交含一行 a.txt。
func newGitWorktreeFixture(t *testing.T, name string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("测试需要 git: %v", err)
	}
	root := t.TempDir()
	main := filepath.Join(root, "main")
	worktree := filepath.Join(root, name)
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v 失败: %v: %s", args, err, stderr.String())
		}
		return string(out)
	}
	if err := os.MkdirAll(main, 0o700); err != nil {
		t.Fatalf("mkdir 失败: %v", err)
	}
	run(main, "init", "-q")
	run(main, "config", "user.email", "team@test")
	run(main, "config", "user.name", "team test")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	run(main, "add", ".")
	run(main, "commit", "-qm", "base")
	run(main, "worktree", "add", "-q", worktree, "-b", name)
	t.Cleanup(func() {
		// 尽力而为：测试内可能已删除 worktree，失败交给 TempDir 兜底清理。
		cmd := exec.Command("git", "-C", main, "worktree", "remove", "--force", worktree)
		_ = cmd.Run()
	})
	return main, worktree
}

// stageIntentToMakeNewFileVisible 让新建（未跟踪）文件进入 git diff 的
// 视野（git add -N 意图登记；git diff <base> 只看跟踪文件——未提交新文件
// 的可见性由创建者负责）。
func stageIntentToMakeNewFileVisible(t *testing.T, dir, name string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "add", "-N", "--", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add -N %s 失败: %v: %s", name, err, out)
	}
}

func writeRegistry(t *testing.T, path string, entries map[string]teamWorkspaceEntry) {
	t.Helper()
	if err := saveTeamWorkspaceRegistry(path, entries); err != nil {
		t.Fatalf("注册表写入失败: %v", err)
	}
}

func TestWorkspaceAdapterResolve(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-resolve")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{
		"wt-1": {Path: worktree, CreatedBy: "user"},
	})
	adapter := NewTeamWorkspaceAdapter(registry)
	path, err := adapter.Resolve("wt-1")
	if err != nil || path != worktree {
		t.Fatalf("注册的工作区应解析成功: path=%q err=%v", path, err)
	}
	if _, err := adapter.Resolve("missing"); err == nil || !strings.Contains(err.Error(), "工作区未注册") {
		t.Fatalf("未注册的工作区应报错，实际 %v", err)
	}
	// 注册表缺失（首次注册前）：任何 id 都报未注册，而非崩溃。
	empty := NewTeamWorkspaceAdapter(filepath.Join(t.TempDir(), "none.json"))
	if _, err := empty.Resolve("wt-1"); err == nil || !strings.Contains(err.Error(), "工作区未注册") {
		t.Fatalf("空注册表应报未注册，实际 %v", err)
	}
	// 注册表损坏：上抛而非静默当空表。
	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("不是 JSON"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if _, err := NewTeamWorkspaceAdapter(corrupt).Resolve("wt-1"); err == nil ||
		!strings.Contains(err.Error(), "工作区注册表损坏") {
		t.Fatalf("损坏的注册表应报错，实际 %v", err)
	}
}

func TestWorkspaceAdapterHeadRevision(t *testing.T) {
	main, worktree := newGitWorktreeFixture(t, "wt-head")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	head, err := adapter.HeadRevision(worktree)
	if err != nil || len(head) != 40 {
		t.Fatalf("HEAD 应为 40 位 sha，实际 %q err=%v", head, err)
	}
	cmd := exec.Command("git", "-C", main, "rev-parse", "HEAD")
	want, _ := cmd.Output()
	if strings.TrimSpace(string(want)) != head {
		t.Fatalf("worktree HEAD 应等于主仓 HEAD: %q vs %q", head, strings.TrimSpace(string(want)))
	}
	if _, err := adapter.HeadRevision(filepath.Join(t.TempDir(), "nowhere")); err == nil {
		t.Fatal("不存在的目录应报错")
	}
}

func TestWorkspaceAdapterDiffParsesFiles(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-diff")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	base, err := adapter.HeadRevision(worktree)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	// 工作区改动：修改跟踪文件 + 新建文件（add -N 登记意图，未提交——
	// 脏区正是任务的正常产出）。
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("base\nchanged\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "新增.txt"), []byte("新内容\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	stageIntentToMakeNewFileVisible(t, worktree, "新增.txt")
	files, truncated, err := adapter.Diff(worktree, base)
	if err != nil {
		t.Fatalf("diff 失败: %v", err)
	}
	if truncated {
		t.Fatal("两个小文件不应触发顶层截断")
	}
	if len(files) != 2 {
		t.Fatalf("应解析出 2 个文件，实际 %d: %+v", len(files), files)
	}
	byPath := map[string]team.WorkspaceDiff{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	changed, ok := byPath["a.txt"]
	if !ok {
		t.Fatalf("应包含 a.txt，实际 %+v", files)
	}
	if changed.Added != 1 || changed.Removed != 0 || changed.Truncated {
		t.Fatalf("a.txt 计数错误: %+v", changed)
	}
	if !strings.Contains(changed.DiffText, "+changed") || !strings.Contains(changed.DiffText, "@@ ") {
		t.Fatalf("a.txt 正文应含 hunk 头与 +changed:\n%s", changed.DiffText)
	}
	added, ok := byPath["新增.txt"]
	if !ok {
		t.Fatalf("应包含 新增.txt，实际 %+v", files)
	}
	if added.Added != 1 || added.Removed != 0 || added.Truncated {
		t.Fatalf("新增文件计数错误: %+v", added)
	}
	// 干净工作区：清掉意图登记、还原内容后相对基线无差异。
	reset := exec.Command("git", "-C", worktree, "reset", "-q")
	if out, err := reset.CombinedOutput(); err != nil {
		t.Fatalf("git reset 失败: %v: %s", err, out)
	}
	if err := os.Remove(filepath.Join(worktree, "新增.txt")); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	files, truncated, err = adapter.Diff(worktree, base)
	if err != nil || truncated || len(files) != 0 {
		t.Fatalf("干净工作区应无差异，实际 files=%d truncated=%v err=%v", len(files), truncated, err)
	}
}

func TestWorkspaceAdapterDiffPerFileTruncation(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-big")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	base, err := adapter.HeadRevision(worktree)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	big := make([]byte, 0, 60000)
	chunk := []byte(strings.Repeat("字", 256) + "\n") // 每行 769 字节，多字节内容不劈半
	for len(big) < 60000 {
		big = append(big, chunk...)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), big, 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	files, truncated, err := adapter.Diff(worktree, base)
	if err != nil {
		t.Fatalf("diff 失败: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("应只有 a.txt，实际 %d", len(files))
	}
	if !files[0].Truncated {
		t.Fatal("单文件超 20000 字节应置 Truncated")
	}
	if len(files[0].DiffText) > maxWorkspaceDiffFileBytes {
		t.Fatalf("截断后不应超过单文件上限，实际 %d", len(files[0].DiffText))
	}
	if !utf8.ValidString(files[0].DiffText) {
		t.Fatal("截断不应把多字节字符劈半")
	}
	// 单文件截断不触发顶层标记（总量远低于 512000）。
	if truncated {
		t.Fatal("单文件截断不应置顶层 truncated")
	}
}

func TestWorkspaceAdapterDiffTotalTruncation(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-total")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	base, err := adapter.HeadRevision(worktree)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	// 26 个文件 × >20000 字节 diff → 截断后仍 >512000 总量：顶层截断。
	body := strings.Repeat("x", 22000) + "\n"
	for i := 0; i < 26; i++ {
		name := filepath.Join(worktree, strings.Repeat("f", 2)+string(rune('a'+i))+".txt")
		if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	stageIntentToMakeNewFileVisible(t, worktree, ".")
	files, truncated, err := adapter.Diff(worktree, base)
	if err != nil {
		t.Fatalf("diff 失败: %v", err)
	}
	if !truncated {
		t.Fatal("总量超 512000 字节应置顶层 truncated")
	}
	total := 0
	for _, file := range files {
		total += len(file.DiffText)
	}
	if total > maxWorkspaceDiffTotalBytes {
		t.Fatalf("截断后总量应 ≤512000，实际 %d", total)
	}
	// 25 个整段（各 20000）+ 第 26 个按剩余预算截断 = 512000：全部 26 个
	// 都在结果里，但最后一个必须是预算截断的残段。
	if len(files) != 26 {
		t.Fatalf("应携带 26 个文件（末位为预算截断残段），实际 %d", len(files))
	}
	if !files[25].Truncated {
		t.Fatal("末位文件应由整包预算截断")
	}
	if files[0].Truncated != true || files[24].Truncated != true {
		t.Fatal("超 20000 字节的文件都应置自身 Truncated")
	}
}

func TestWorkspaceAdapterDiffFileCountTruncation(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-count")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	base, err := adapter.HeadRevision(worktree)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	// 51 个小文件（每个 1 行差异）：文件数超 50 → 顶层截断。
	for i := 0; i < 51; i++ {
		name := filepath.Join(worktree, "c"+string(rune('a'+i/26))+string(rune('a'+i%26))+".txt")
		if err := os.WriteFile(name, []byte("内容\n"), 0o600); err != nil {
			t.Fatalf("写入失败: %v", err)
		}
	}
	stageIntentToMakeNewFileVisible(t, worktree, ".")
	files, truncated, err := adapter.Diff(worktree, base)
	if err != nil {
		t.Fatalf("diff 失败: %v", err)
	}
	if !truncated {
		t.Fatal("文件数超 50 应置顶层 truncated")
	}
	if len(files) > maxWorkspaceDiffFiles {
		t.Fatalf("携带文件数应 ≤50，实际 %d", len(files))
	}
}

func TestWorkspaceAdapterCleanup(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-clean")
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	writeRegistry(t, registry, map[string]teamWorkspaceEntry{"wt-1": {Path: worktree}})
	adapter := NewTeamWorkspaceAdapter(registry)
	// 脏区：git worktree remove（无 --force）拒绝，错误上抛。
	if err := os.WriteFile(filepath.Join(worktree, "dirty.txt"), []byte("未提交改动\n"), 0o600); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if err := adapter.Cleanup("wt-1"); err == nil {
		t.Fatal("脏 worktree 清理应失败")
	} else if !strings.Contains(err.Error(), "git worktree remove") {
		t.Fatalf("错误应携带 git 拒绝原因: %v", err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "dirty.txt")); err != nil {
		t.Fatalf("清理失败绝不能丢用户改动: %v", err)
	}
	// 干净 worktree：成功删除。
	if err := os.Remove(filepath.Join(worktree, "dirty.txt")); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if err := adapter.Cleanup("wt-1"); err != nil {
		t.Fatalf("干净 worktree 清理应成功: %v", err)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree 目录应已删除: %v", err)
	}
	// 未注册 id：报错而非动 git。
	if err := adapter.Cleanup("missing"); err == nil || !strings.Contains(err.Error(), "工作区未注册") {
		t.Fatalf("未注册的工作区清理应报错，实际 %v", err)
	}
}

func TestWorkspaceRegistryRoundtrip(t *testing.T) {
	registry := filepath.Join(t.TempDir(), "team-workspaces.json")
	entries := map[string]teamWorkspaceEntry{
		"wt-1": {Path: "/tmp/wt-1", CreatedAt: "2026-10-25T00:00:00Z", CreatedBy: "user"},
	}
	if err := saveTeamWorkspaceRegistry(registry, entries); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	loaded, err := loadTeamWorkspaceRegistry(registry)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got := loaded["wt-1"]; got.Path != "/tmp/wt-1" || got.CreatedBy != "user" {
		t.Fatalf("注册表往返应保真: %+v", got)
	}
	// 写入目录不存在时自动创建（首写前 .local 可能尚无该文件）。
	nested := filepath.Join(t.TempDir(), "sub", "dir", "team-workspaces.json")
	if err := saveTeamWorkspaceRegistry(nested, entries); err != nil {
		t.Fatalf("嵌套路径写入失败: %v", err)
	}
	if _, err := loadTeamWorkspaceRegistry(nested); err != nil {
		t.Fatalf("嵌套路径读取失败: %v", err)
	}
}
