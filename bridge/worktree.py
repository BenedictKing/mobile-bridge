"""新建聊天时可选创建 Git worktree，让会话在隔离工作区中运行。"""
import re
import secrets
import subprocess
from pathlib import Path

BASES = ('fresh', 'head')
# fresh 基准依次尝试远程默认分支与本地主干分支
FRESH_REFS = ('origin/HEAD', 'origin/main', 'origin/master', 'main', 'master')
EXCLUDE_LINE = '/.agents/worktrees/'


def _git(cwd, *args):
    return subprocess.run(['git', '-C', str(cwd), *args], capture_output=True, text=True)


def create(cwd, title, base):
    """在仓库根的 .agents/worktrees/ 下创建 worktree，返回其绝对路径。"""
    if base not in BASES:
        raise ValueError('worktree 基准无效')
    probe = _git(cwd, 'rev-parse', '--show-toplevel')
    if probe.returncode != 0:
        raise ValueError('所选项目不在 Git 仓库中，无法使用 worktree')
    root = probe.stdout.strip()
    if base == 'head':
        if _git(root, 'rev-parse', '--verify', '--quiet', 'HEAD').returncode != 0:
            raise ValueError('仓库尚无提交，无法基于当前 HEAD 创建 worktree')
        ref = 'HEAD'
    else:
        ref = next((candidate for candidate in FRESH_REFS
                    if _git(root, 'rev-parse', '--verify', '--quiet', candidate).returncode == 0), None)
        if ref is None:
            raise ValueError('未找到默认分支，可改用"基于当前 HEAD"')
    slug = re.sub(r'[^a-z0-9]+', '-', title.lower()).strip('-')[:24].strip('-') or 'task'
    name = slug + '-' + secrets.token_hex(3)
    path = str(Path(root) / '.agents' / 'worktrees' / name)
    result = _git(root, 'worktree', 'add', '-b', 'agents/' + name, path, ref)
    if result.returncode != 0:
        tail = (result.stderr or '').strip().splitlines()
        raise ValueError('创建 worktree 失败：' + (tail[-1] if tail else 'git 退出码 ' + str(result.returncode)))
    _exclude(root)
    return path


def remove(cwd, path):
    """尽力清理 worktree（例如会话创建失败时），失败不抛出。"""
    try:
        _git(cwd, 'worktree', 'remove', '--force', str(path))
    except OSError:
        pass


def _exclude(root):
    """把 /.agents/worktrees/ 幂等追加进共享 git 目录的 info/exclude。"""
    common = _git(root, 'rev-parse', '--git-common-dir')
    if common.returncode != 0:
        return
    common_dir = Path(common.stdout.strip())
    if not common_dir.is_absolute():
        common_dir = Path(root) / common_dir
    exclude = common_dir / 'info' / 'exclude'
    try:
        exclude.parent.mkdir(parents=True, exist_ok=True)
        content = exclude.read_text(encoding='utf-8') if exclude.exists() else ''
        if EXCLUDE_LINE not in content.splitlines():
            with exclude.open('a', encoding='utf-8') as handle:
                if content and not content.endswith('\n'):
                    handle.write('\n')
                handle.write(EXCLUDE_LINE + '\n')
    except OSError:
        pass  # exclude 失败不影响 worktree 本身
