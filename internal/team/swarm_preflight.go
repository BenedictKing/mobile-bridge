package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BenedictKing/mobile-bridge/internal/swarmeval"
	"github.com/google/uuid"
)

// 在已有写租约内、入队前保存输入快照。实际任务改动与该输入比较，保留用户已有修改。
func (s *Service) preflightSwarmInput(state *runState, run *Run, task *Task, cwd string) error {
	if s.SwarmRunner == nil {
		return fmt.Errorf("候选执行器未装配")
	}
	if _, _, err := s.fingerprintFreshForPaths(cwd, nil); err != nil {
		return err
	}
	team := s.store.Team(run.TeamID)
	if team == nil {
		return ErrNotFound
	}
	if team.ProjectCwd != "" {
		if err := swarmeval.SameRepository(context.Background(), team.ProjectCwd, cwd); err != nil {
			return err
		}
	}
	if b := swarmBinding(run, task); b != nil {
		for _, id := range b.EvalSpecIDs {
			spec := swarmEvalSpec(run, id)
			if spec == nil {
				continue
			}
			if spec.WorkspaceID != "" && spec.WorkspaceID != task.WorkspaceID {
				return fmt.Errorf("检查规格绑定其他工作区，不能用当前候选替代")
			}
			switch spec.Command[0] {
			case "npm", "pnpm", "yarn":
				if _, err := os.Stat(filepath.Join(cwd, "package.json")); err != nil {
					return fmt.Errorf("前端入口或依赖环境未就绪：%w", err)
				}
			}
		}
	}
	if run.Swarm.TaskInputs[task.ID] != "" {
		return nil
	}
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("swarm-input:"+run.ID+":"+task.ID+fmt.Sprint(task.Attempt))).String()
	ctx, cancel := s.swarmContext(run.ID)
	defer cancel()
	a, err := s.SwarmRunner.Capture(ctx, id, cwd, task.BaseRevision)
	if err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	fresh := s.store.Run(run.ID)
	if fresh == nil || fresh.Status == RunCancelled {
		return ErrRunFinished
	}
	if fresh.Swarm.TaskInputs == nil {
		fresh.Swarm.TaskInputs = map[string]string{}
	}
	fresh.Swarm.TaskInputs[task.ID] = a.ID
	return s.saveSwarmLocked(fresh, "runner")
}

func (s *Service) swarmActualChanges(ctx context.Context, run *Run, task *Task, after *swarmeval.Artifact) ([]string, error) {
	id := run.Swarm.TaskInputs[task.ID]
	if id == "" {
		return after.ChangedPaths, nil
	}
	before, err := s.SwarmRunner.LoadArtifact(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.SwarmRunner.ChangedPathsBetween(ctx, before, after)
}

func (s *Service) swarmProtectedChecks(ctx context.Context, run *Run, task *Task, after *swarmeval.Artifact) error {
	id := run.Swarm.TaskInputs[task.ID]
	if id == "" {
		return nil
	}
	before, err := s.SwarmRunner.LoadArtifact(ctx, id)
	if err != nil {
		return err
	}
	changed, err := s.SwarmRunner.ChangedPathsBetween(ctx, before, after)
	if err != nil {
		return err
	}
	b := swarmBinding(run, task)
	if b == nil {
		return fmt.Errorf("缺少任务契约")
	}
	protected := append([]string(nil), b.ProtectedPaths...)
	// 执行入口同样属于冻结检查规格；新增测试允许，改验收入口必须形成新契约。
	for _, id := range b.EvalSpecIDs {
		spec := swarmEvalSpec(run, id)
		if spec == nil {
			continue
		}
		switch spec.Command[0] {
		case "make":
			protected = append(protected, "Makefile", "makefile", "GNUmakefile")
		case "npm", "pnpm", "yarn":
			protected = append(protected, "package.json")
		case "go":
			protected = append(protected, "go.mod", "go.sum")
		}
	}
	for _, name := range changed {
		if pathAllowed(filepath.ToSlash(name), protected) {
			return fmt.Errorf("冻结检查入口或受保护内容被修改：%s", name)
		}
	}
	return nil
}
