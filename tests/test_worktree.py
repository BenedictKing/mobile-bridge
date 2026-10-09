"""bridge.worktree：真实 git 仓库上的 worktree 创建行为。"""
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from bridge import worktree

ROOT = Path(__file__).resolve().parents[1]
(ROOT / '.tmp').mkdir(exist_ok=True)

# 与全局 git 配置隔离，避免依赖用户机器上的默认分支名或签名设置
GIT_ENV = {**os.environ,
           'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': os.devnull,
           'GIT_AUTHOR_NAME': 'Test', 'GIT_AUTHOR_EMAIL': 'test@example.com',
           'GIT_COMMITTER_NAME': 'Test', 'GIT_COMMITTER_EMAIL': 'test@example.com'}


def git(cwd, *args):
    result = subprocess.run(['git', '-C', str(cwd), *args], env=GIT_ENV,
                            capture_output=True, text=True)
    assert result.returncode == 0, (args, result.stderr)
    return result.stdout.strip()


class WorktreeTests(unittest.TestCase):
    def setUp(self):
        if shutil.which('git') is None:
            self.skipTest('需要 git')
        self.tmp = tempfile.TemporaryDirectory(dir=ROOT / '.tmp')
        self.addCleanup(self.tmp.cleanup)
        base = Path(self.tmp.name)
        self.repo = base / 'repo'
        self.origin = base / 'origin.git'
        # 本地 main 有一个未推送的提交，用于区分 fresh 与 head 基准
        git(base, 'init', '-b', 'main', self.repo)
        (self.repo / 'a.txt').write_text('a', encoding='utf-8')
        git(self.repo, 'add', '.')
        git(self.repo, 'commit', '-m', 'first')
        git(base, 'init', '--bare', '-b', 'main', self.origin)
        git(self.repo, 'remote', 'add', 'origin', str(self.origin))
        git(self.repo, 'push', 'origin', 'main')
        git(self.origin, 'symbolic-ref', 'HEAD', 'refs/heads/main')
        git(self.repo, 'fetch', 'origin')
        git(self.repo, 'remote', 'set-head', 'origin', '-a')
        (self.repo / 'b.txt').write_text('b', encoding='utf-8')
        git(self.repo, 'add', '.')
        git(self.repo, 'commit', '-m', 'unpushed')
        self.origin_head = git(self.repo, 'rev-parse', 'origin/HEAD')
        self.local_head = git(self.repo, 'rev-parse', 'HEAD')
        self.assertNotEqual(self.origin_head, self.local_head)

    def head_of(self, path):
        return git(path, 'rev-parse', 'HEAD')

    def test_fresh_uses_origin_head(self):
        path = worktree.create(self.repo, 'Fix Bug 123', 'fresh')
        self.assertTrue(path.startswith(str(self.repo / '.agents' / 'worktrees') + os.sep))
        self.assertTrue(Path(path).name.startswith('fix-bug-123-'))
        self.assertEqual(self.head_of(path), self.origin_head)
        self.assertTrue(git(path, 'branch', '--show-current').startswith('agents/'))
        # worktree 目录不被父仓库视为未跟踪
        self.assertNotIn('.agents', git(self.repo, 'status', '--porcelain'))

    def test_head_uses_local_head(self):
        path = worktree.create(self.repo, '本地基准', 'head')
        self.assertEqual(self.head_of(path), self.local_head)
        self.assertTrue(Path(path).name.startswith('task-'))  # 纯 CJK 标题折叠为空，回退 task

    def test_exclude_is_idempotent(self):
        worktree.create(self.repo, 'one', 'fresh')
        worktree.create(self.repo, 'two', 'head')
        exclude = (self.repo / '.git' / 'info' / 'exclude').read_text(encoding='utf-8')
        self.assertEqual(exclude.splitlines().count('/.agents/worktrees/'), 1)

    def test_remove_deletes_worktree(self):
        path = worktree.create(self.repo, 'gone', 'head')
        worktree.remove(self.repo, path)
        self.assertFalse(Path(path).exists())

    def test_non_git_directory_rejected(self):
        # 必须放在项目 .tmp 之外，否则会解析到外层仓库
        with tempfile.TemporaryDirectory() as outside:
            with self.assertRaisesRegex(ValueError, '不在 Git 仓库中'):
                worktree.create(outside, 'x', 'head')

    def test_fresh_without_default_branch_rejected(self):
        git(self.repo, 'remote', 'remove', 'origin')
        git(self.repo, 'branch', '-m', 'main', 'topic')
        with self.assertRaisesRegex(ValueError, '未找到默认分支'):
            worktree.create(self.repo, 'x', 'fresh')

    def test_invalid_base_rejected(self):
        with self.assertRaisesRegex(ValueError, 'worktree 基准无效'):
            worktree.create(self.repo, 'x', 'bogus')


if __name__ == '__main__':
    unittest.main()
