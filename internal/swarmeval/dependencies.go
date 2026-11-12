package swarmeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type PreparationStep struct {
	Command    []string  `json:"command"`
	ExitCode   int       `json:"exitCode"`
	ProcessID  int       `json:"processId,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`
}

func (r *Runner) prepareDependencies(ctx context.Context, evalDir, workDir string, env []string, output *boundedWriter) ([]PreparationStep, string, error) {
	commands, reason, err := dependencyCommands(workDir)
	if err != nil {
		return nil, reason, err
	}
	steps := []PreparationStep{}
	for i, command := range commands {
		if ctx.Err() != nil {
			return steps, "environment_setup_failed", ctx.Err()
		}
		_, _ = output.Write([]byte("\n[依赖准备] " + strings.Join(command, " ") + "\n"))
		step := PreparationStep{Command: append([]string(nil), command...), ExitCode: -1, StartedAt: time.Now().UTC()}
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdout = output
		cmd.Stderr = output
		cmd.WaitDelay = 2 * time.Second
		configureProcess(cmd)
		runErr := cmd.Start()
		if runErr == nil {
			step.ProcessID = cmd.Process.Pid
			if err := writeJSON(filepath.Join(evalDir, fmt.Sprintf("prepare-%d.json", i+1)), map[string]any{"command": command, "pid": step.ProcessID, "startedAt": step.StartedAt}); err != nil {
				cleanupProcess(cmd)
				_ = cmd.Wait()
				return steps, "environment_setup_failed", err
			}
			runErr = cmd.Wait()
		}
		cleanupProcess(cmd)
		if cmd.ProcessState != nil {
			step.ExitCode = cmd.ProcessState.ExitCode()
		}
		step.FinishedAt = time.Now().UTC()
		steps = append(steps, step)
		if runErr != nil {
			return steps, "environment_setup_failed", fmt.Errorf("%s：%w", command[0], runErr)
		}
	}
	return steps, "", nil
}

func dependencyCommands(dir string) ([][]string, string, error) {
	commands := [][]string{}
	read := func(name string) ([]byte, error) { return os.ReadFile(filepath.Join(dir, name)) }
	exists := func(name string) bool {
		info, err := os.Lstat(filepath.Join(dir, name))
		return err == nil && info.Mode().IsRegular()
	}
	if data, err := read("package.json"); err == nil {
		var pkg struct {
			Dependencies         map[string]json.RawMessage `json:"dependencies"`
			DevDependencies      map[string]json.RawMessage `json:"devDependencies"`
			OptionalDependencies map[string]json.RawMessage `json:"optionalDependencies"`
			PackageManager       string                     `json:"packageManager"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil {
			return nil, "environment_setup_failed", errors.New("package.json 格式无效")
		}
		locks := []string{}
		for _, name := range []string{"package-lock.json", "npm-shrinkwrap.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock", "bun.lockb"} {
			if exists(name) {
				locks = append(locks, name)
			}
		}
		if len(locks) > 1 {
			return nil, "lockfile_conflict", errors.New("存在多个包管理器锁文件，需明确评测依赖入口")
		}
		if len(locks) == 0 {
			if len(pkg.Dependencies)+len(pkg.DevDependencies)+len(pkg.OptionalDependencies) > 0 {
				return nil, "lockfile_missing", errors.New("package.json 包含依赖但缺少冻结锁文件")
			}
		} else {
			manager := ""
			var command []string
			switch locks[0] {
			case "package-lock.json", "npm-shrinkwrap.json":
				manager = "npm"
				command = []string{"npm", "ci", "--no-audit", "--no-fund"}
			case "pnpm-lock.yaml":
				manager = "pnpm"
				command = []string{"pnpm", "install", "--frozen-lockfile"}
			case "yarn.lock":
				manager = "yarn"
				command = []string{"yarn", "install", "--immutable"}
			default:
				return nil, "environment_setup_failed", errors.New("当前评测依赖准备尚不支持 bun 锁文件")
			}
			if pkg.PackageManager != "" && !strings.HasPrefix(pkg.PackageManager, manager+"@") {
				return nil, "lockfile_conflict", errors.New("packageManager 与锁文件不匹配")
			}
			commands = append(commands, command)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "environment_setup_failed", err
	}
	if data, err := read("go.mod"); err == nil {
		requires := false
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
			if line == "require(" || strings.HasPrefix(line, "require ") || strings.HasPrefix(line, "require\t") {
				requires = true
				break
			}
		}
		if requires {
			if !exists("go.sum") {
				return nil, "lockfile_missing", errors.New("go.mod 声明依赖但缺少冻结 go.sum")
			}
			commands = append(commands, []string{"go", "mod", "download"})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "environment_setup_failed", err
	}
	return commands, "", nil
}
