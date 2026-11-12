package swarmeval

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// SameRepository 只接受 Git 工作区根，按真实 common-dir 识别同一仓库。
// worktree 可有不同 HEAD；独立 clone 即使提交完全相同也不是同一个仓库。
// 本函数只执行 rev-parse，不创建分支/worktree 或修改 Git 配置。
func SameRepository(ctx context.Context, left, right string) error {
	leftCommon, err := repositoryIdentity(ctx, left)
	if err != nil {
		return fmt.Errorf("左侧工作区无效：%w", err)
	}
	rightCommon, err := repositoryIdentity(ctx, right)
	if err != nil {
		return fmt.Errorf("右侧工作区无效：%w", err)
	}
	if leftCommon != rightCommon {
		return errors.New("工作区不属于同一 Git 仓库")
	}
	return nil
}

func repositoryIdentity(ctx context.Context, dir string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("工作区路径不能为空")
	}
	real, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err = filepath.EvalSymlinks(real)
	if err != nil {
		return "", err
	}
	root, err := gitOutput(ctx, real, 1<<20, "rev-parse", "--path-format=absolute", "--show-toplevel")
	if err != nil {
		return "", err
	}
	gitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(root)))
	if err != nil {
		return "", err
	}
	if filepath.Clean(gitRoot) != filepath.Clean(real) {
		return "", errors.New("必须使用 Git 工作区根目录")
	}
	common, err := gitOutput(ctx, real, 1<<20, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		return "", errors.New("Git common-dir 不是绝对路径")
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", err
	}
	return filepath.Clean(commonDir), nil
}

// ChangedPathsBetween 比较两个冻结快照的实际内容，不使用 HEAD 差异。
// before 中原有且未改变的用户修改不会被错误归属为本次任务的写入。
func ChangedPathsBetween(ctx context.Context, before, after *Artifact) ([]string, error) {
	runner := &Runner{}
	return runner.changedPathsBetween(ctx, before, after)
}

// ChangedPathsBetween 同时验证制品属于本 Runner 的持久化账本。
func (r *Runner) ChangedPathsBetween(ctx context.Context, before, after *Artifact) ([]string, error) {
	if before == nil || after == nil {
		return nil, errors.New("比较候选不能为空")
	}
	loadedBefore, err := r.LoadArtifact(ctx, before.ID)
	if err != nil {
		return nil, err
	}
	loadedAfter, err := r.LoadArtifact(ctx, after.ID)
	if err != nil {
		return nil, err
	}
	if loadedBefore.ContentHash != before.ContentHash || loadedAfter.ContentHash != after.ContentHash {
		return nil, ErrConflict
	}
	return r.changedPathsBetween(ctx, loadedBefore, loadedAfter)
}

func (r *Runner) changedPathsBetween(ctx context.Context, before, after *Artifact) ([]string, error) {
	if before == nil || after == nil {
		return nil, errors.New("比较候选不能为空")
	}
	read := func(a *Artifact) ([]fileData, error) {
		if a.Version != 1 || a.Isolation != IsolationLocalSnapshot || !filepath.IsAbs(a.Directory) || a.ContentHash == "" {
			return nil, errors.New("候选内容身份无效")
		}
		files, err := r.readTree(ctx, a.Directory, a.Files)
		if err != nil {
			return nil, err
		}
		if treeHash(files) != a.ContentHash {
			return nil, ErrChanged
		}
		return files, nil
	}
	left, err := read(before)
	if err != nil {
		return nil, err
	}
	right, err := read(after)
	if err != nil {
		return nil, err
	}
	return changedPaths(left, right), nil
}
