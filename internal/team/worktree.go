package team

// worktree.go 是 isolated_worktree 的用户侧数据面（§6.4 规则 6 + §16 默认
// 决策 6）：只引用用户预备的 worktree，绝不自动创建/合并/清理；改动以
// 双层截断的有界 diff 呈现，由用户决定是否应用；清理是用户显式触发的
// 工作区操作，与任务运行状态解耦。派发绑定（baseRevision 记账）在
// dispatcher.go，重启对账（workspace_lost）在 runner.go。

import (
	"errors"
	"fmt"
	"log"
)

// TaskDiff 返回任务工作区相对基线的有界 diff（§6.4 规则 6：并行 worktree
// 的结果不自动合并到主工作区，第一版只生成 diff 供审查）。非 isolated
// 任务或尚未记录基线（未派发过）的任务没有可比较的基线，直接报错。
// 双层截断由适配器保证：单文件超限置该文件 Truncated，整包超文件数/
// 字节上限置顶层 truncated。
func (s *Service) TaskDiff(runID, taskID string) ([]WorkspaceDiff, bool, error) {
	run := s.store.Run(runID)
	if run == nil {
		return nil, false, fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	task := findTaskIn(run, taskID)
	if task == nil {
		return nil, false, fmt.Errorf("%w: run=%s task=%s", ErrNotFound, runID, taskID)
	}
	if task.WorkspaceMode != WorkspaceIsolatedWorktree || task.BaseRevision == "" {
		return nil, false, errors.New("该任务没有工作区基线")
	}
	if s.Workspaces == nil {
		return nil, false, ErrWorkspacesUnavailable
	}
	path, err := s.Workspaces.Resolve(task.WorkspaceID)
	if err != nil {
		return nil, false, fmt.Errorf("工作区不存在：%s（%v）", task.WorkspaceID, err)
	}
	return s.Workspaces.Diff(path, task.BaseRevision)
}

// CleanupWorkspace 删除任务引用的 worktree（用户显式触发，绝非自动——
// §6.4 红线：自动编排器不执行 merge/rebase/删除 worktree）。git worktree
// remove 不带 --force：脏区（未提交改动）时失败上抛并落
// workspace_cleanup_failed 事件，绝不丢用户改动。成功不触碰任务终态
// （清理是工作区操作，与运行状态解耦；任务快照的 workspaceId/
// baseRevision 保留供审计与 diff 复查）。
func (s *Service) CleanupWorkspace(runID, taskID string) error {
	run := s.store.Run(runID)
	if run == nil {
		return fmt.Errorf("%w: run=%s", ErrNotFound, runID)
	}
	task := findTaskIn(run, taskID)
	if task == nil {
		return fmt.Errorf("%w: run=%s task=%s", ErrNotFound, runID, taskID)
	}
	if task.WorkspaceMode != WorkspaceIsolatedWorktree || task.WorkspaceID == "" {
		return errors.New("该任务没有引用独立工作区")
	}
	if s.Workspaces == nil {
		return ErrWorkspacesUnavailable
	}
	if err := s.Workspaces.Cleanup(task.WorkspaceID); err != nil {
		s.recordWorkspaceCleanupFailed(runID, taskID, task.WorkspaceID, err)
		return err
	}
	return nil
}

// recordWorkspaceCleanupFailed 落 workspace_cleanup_failed 事件（journal
// 先行；事件本身不改任务终态——清理失败是工作区侧事实，任务状态由运行
// 流程独立管理）。错误文本按 500 rune 截断（§10.3 有界 payload）。
func (s *Service) recordWorkspaceCleanupFailed(runID, taskID, workspaceID string, cause error) {
	state := s.stateFor(runID)
	state.mu.Lock()
	defer state.mu.Unlock()
	event := newEvent(runID, EventWorkspaceCleanupFailed, "user", map[string]any{
		"taskId":      taskID,
		"workspaceId": workspaceID,
		"error":       truncateRunes(cause.Error(), 500),
	})
	if err := s.journal.Append(event); err != nil {
		log.Printf("team run %s workspace_cleanup_failed 记账失败: %v", runID, err)
		return
	}
	s.emit(event)
}
