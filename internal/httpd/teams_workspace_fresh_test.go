package httpd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

func TestFreshFingerprintBypassesCacheForNestedContent(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-fresh-content")
	nested := filepath.Join(worktree, "src")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(nested, "tracked.txt")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(tracked, "before\n")
	runGitTestCommand(t, worktree, "add", "src/tracked.txt")
	runGitTestCommand(t, worktree, "commit", "-qm", "nested fixture")
	untracked := filepath.Join(nested, "untracked.txt")
	write(untracked, "untracked-before\n")

	// 固定缓存时间和根目录 mtime，真实复现原地写文件不使展示缓存失效的窗口。
	now := time.Now()
	fingerprinter := &gitFingerprinter{now: func() time.Time { return now }}
	_, cached, err := fingerprinter.Fingerprint(worktree)
	if err != nil {
		t.Fatal(err)
	}
	rootInfo, err := os.Stat(worktree)
	if err != nil {
		t.Fatal(err)
	}
	write(tracked, "after!\n")
	if err := os.Chtimes(worktree, rootInfo.ModTime(), rootInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	_, stillCached, err := fingerprinter.Fingerprint(worktree)
	if err != nil || stillCached != cached || fingerprinter.computeCalls != 1 {
		t.Fatalf("展示缓存行为应保持不变：%s/%s calls=%d err=%v", cached, stillCached, fingerprinter.computeCalls, err)
	}
	_, fresh, err := fingerprinter.FingerprintFresh(worktree, nil)
	if err != nil || fresh == cached {
		t.Fatalf("即时核验必须检测嵌套跟踪文件原地变化：%s/%s %v", cached, fresh, err)
	}
	write(untracked, "untracked-after!\n")
	_, changedUntracked, err := fingerprinter.FingerprintFresh(worktree, nil)
	if err != nil || changedUntracked == fresh {
		t.Fatalf("即时核验必须检测未跟踪文件内容变化：%s/%s %v", fresh, changedUntracked, err)
	}

	headBefore, beforeCommit, err := fingerprinter.FingerprintFresh(worktree, nil)
	if err != nil {
		t.Fatal(err)
	}
	runGitTestCommand(t, worktree, "add", "src")
	runGitTestCommand(t, worktree, "commit", "-qm", "accepted content")
	headAfter, afterCommit, err := fingerprinter.FingerprintFresh(worktree, nil)
	if err != nil || headAfter == headBefore || afterCommit != beforeCommit {
		t.Fatalf("同一内容提交后只能改变出处：%s/%s -> %s/%s %v", headBefore, beforeCommit, headAfter, afterCommit, err)
	}
}

func TestFreshFingerprintScopesAndPreservesUserIndex(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-fresh-paths")
	adapter, ok := NewGitFingerprinter().(team.WorkspaceFreshFingerprinter)
	if !ok {
		t.Fatal("生产适配器未提供即时内容核验")
	}
	indexPath, err := gitWorkspaceOutput(worktree, "rev-parse", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	indexPath = filepath.Clean(string(bytes.TrimSpace([]byte(indexPath))))
	beforeIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	_, before, err := adapter.FingerprintFresh(worktree, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "other.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, unrelated, err := adapter.FingerprintFresh(worktree, []string{"a.txt"})
	if err != nil || before != unrelated {
		t.Fatalf("无关文件不应改变限定路径的身份：%s/%s %v", before, unrelated, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, err := adapter.FingerprintFresh(worktree, []string{"a.txt"})
	if err != nil || changed == before {
		t.Fatalf("被审路径变更必须立即可见：%s/%s %v", before, changed, err)
	}
	afterIndex, err := os.ReadFile(indexPath)
	if err != nil || !bytes.Equal(beforeIndex, afterIndex) {
		t.Fatalf("核验不得更改用户 index：%v", err)
	}
	if _, _, err := adapter.FingerprintFresh(worktree, []string{"a.txt", "../outside"}); err == nil {
		t.Fatal("非法路径不能被静默省略，缩小核验范围")
	}
	if _, _, err := adapter.FingerprintFresh("", nil); err == nil {
		t.Fatal("空工作区不能退回网关 cwd")
	}
}

func TestFreshFingerprintTreatsPathNamesLiterally(t *testing.T) {
	_, worktree := newGitWorktreeFixture(t, "wt-fresh-literal")
	fingerprinter := &gitFingerprinter{}
	for _, name := range []string{"[a].txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(worktree, name), []byte("before\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, before, err := fingerprinter.FingerprintFresh(worktree, []string{"[a].txt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "[a].txt"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, after, err := fingerprinter.FingerprintFresh(worktree, []string{"[a].txt"})
	if err != nil || before == after {
		t.Fatalf("带通配符语法的真实文件名必须被核验：%s/%s %v", before, after, err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "a.txt"), []byte("unrelated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, unrelated, err := fingerprinter.FingerprintFresh(worktree, []string{"[a].txt"})
	if err != nil || after != unrelated {
		t.Fatalf("文件名不得被解释成通配范围：%s/%s %v", after, unrelated, err)
	}
}
