package swarmeval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	StatusPassed       = "passed"
	StatusFailed       = "failed"
	StatusInconclusive = "inconclusive"
	StatusInterrupted  = "interrupted"
)

type Spec struct {
	PrepareDependencies bool              `json:"prepareDependencies,omitempty"`
	Command             []string          `json:"command"`
	Timeout             time.Duration     `json:"timeout"`
	Environment         map[string]string `json:"environment,omitempty"`
}

type Result struct {
	OperationID     string            `json:"operationId"`
	ArtifactID      string            `json:"artifactId"`
	ArtifactHash    string            `json:"artifactHash"`
	SpecHash        string            `json:"specHash"`
	EnvironmentHash string            `json:"environmentHash"`
	Status          string            `json:"status"`
	ExitCode        int               `json:"exitCode"`
	OutputHead      string            `json:"outputHead"`
	Truncated       bool              `json:"truncated"`
	LogHash         string            `json:"logHash"`
	LogPath         string            `json:"logPath"`
	StartedAt       time.Time         `json:"startedAt"`
	FinishedAt      time.Time         `json:"finishedAt"`
	Reason          string            `json:"reason,omitempty"`
	Isolation       string            `json:"isolation"`
	Command         []string          `json:"command"`
	ProcessID       int               `json:"processId,omitempty"`
	ExecutablePath  string            `json:"executablePath,omitempty"`
	ExecutableHash  string            `json:"executableHash,omitempty"`
	Preparation     []PreparationStep `json:"preparation,omitempty"`
}

type evalIntent struct{ OperationID, ArtifactID, ArtifactHash, SpecHash string }

// ValidateSpec 检查固定参数和有限的非敏感环境。执行内容仍须由调用方授权；
// make/npm 等可运行仓库程序，local_snapshot 不是 OS 沙箱。
func ValidateSpec(spec Spec) error {
	if len(spec.Command) == 0 || len(spec.Command) > 32 {
		return errors.New("评测命令需要 1–32 个固定参数")
	}
	var size int
	for _, arg := range spec.Command {
		size += len(arg)
		if arg == "" || len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("评测命令参数无效")
		}
	}
	if size > 16384 {
		return errors.New("评测命令超过上限")
	}
	switch strings.ToLower(filepath.Base(spec.Command[0])) {
	case "sh", "bash", "zsh", "fish", "csh", "tcsh", "cmd", "cmd.exe", "powershell", "powershell.exe", "pwsh", "pwsh.exe":
		return errors.New("评测命令不允许直接启动 shell")
	}
	if spec.Timeout < 0 || spec.Timeout > 30*time.Minute {
		return errors.New("评测超时应在 0–30 分钟内")
	}
	allowed := map[string]bool{"CI": true, "NO_COLOR": true, "LANG": true, "LC_ALL": true, "TZ": true, "PORT": true, "NODE_ENV": true, "CGO_ENABLED": true, "GOTOOLCHAIN": true, "GOPROXY": true, "GOSUMDB": true}
	if len(spec.Environment) > len(allowed) {
		return errors.New("评测环境变量超过上限")
	}
	for key, value := range spec.Environment {
		if !allowed[key] || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("评测不允许环境变量 %s", key)
		}
		if key == "PORT" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("评测端口无效")
			}
		}
		if key == "GOPROXY" && (strings.Contains(value, "@") || strings.Contains(value, "?")) {
			return errors.New("代理地址不能包含凭据或查询参数")
		}
	}
	return nil
}

// Evaluate 每次在独立复制的候选中执行；相同操作的请求重放读取结果。
// 已开始却没有结果的操作视为待核查，绝不自动再执行可能有副作用的命令。
func (r *Runner) Evaluate(ctx context.Context, operationID string, artifact *Artifact, spec Spec) (*Result, error) {
	if err := ValidateSpec(spec); err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, errors.New("缺少评测候选")
	}
	if !ProcessTreeSupported() {
		return nil, errors.New("此平台尚不支持评测进程树控制")
	}
	a, err := r.LoadArtifact(ctx, artifact.ID)
	if err != nil {
		return nil, err
	}
	if a.ContentHash != artifact.ContentHash {
		return nil, ErrConflict
	}
	if err := r.sourceMatches(ctx, a); err != nil {
		return nil, err
	}
	dir, err := r.location("evals", operationID)
	if err != nil {
		return nil, err
	}
	specHash := hashJSON(spec)
	if result, loadErr := r.LoadResult(operationID); loadErr == nil {
		if result.ArtifactID != a.ID || result.ArtifactHash != a.ContentHash || result.SpecHash != specHash {
			return nil, ErrConflict
		}
		return result, nil
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return nil, loadErr
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w：已有未结算评测，必须核查旧进程", ErrConflict)
		}
		return nil, err
	}
	intent := evalIntent{operationID, a.ID, a.ContentHash, specHash}
	if err := writeJSON(filepath.Join(dir, "intent.json"), intent); err != nil {
		return nil, err
	}
	files, err := r.readTree(ctx, a.Directory, a.Files)
	if err != nil {
		return nil, err
	}
	workDir := filepath.Join(dir, "tree")
	if err := writeTree(workDir, files); err != nil {
		return nil, err
	}
	env, err := evaluationEnvironment(dir, spec.Environment)
	if err != nil {
		return nil, err
	}
	if spec.PrepareDependencies && spec.Environment["GOTOOLCHAIN"] == "" {
		env = append(env, "GOTOOLCHAIN=local")
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := &Result{OperationID: operationID, ArtifactID: a.ID, ArtifactHash: a.ContentHash, SpecHash: specHash, EnvironmentHash: hashJSON(env),
		Status: StatusInconclusive, ExitCode: -1, StartedAt: time.Now().UTC(), LogPath: filepath.Join(dir, "output.log"), Isolation: IsolationLocalSnapshot, Command: append([]string(nil), spec.Command...)}
	output := &boundedWriter{limit: r.Limits.normalized().MaxOutputBytes}
	if err := writeJSON(filepath.Join(dir, "started.json"), map[string]any{"operationId": operationID, "startedAt": result.StartedAt}); err != nil {
		return nil, err
	}
	prepared := true
	if spec.PrepareDependencies {
		steps, reason, prepareErr := r.prepareDependencies(commandCtx, dir, workDir, env, output)
		result.Preparation = steps
		if prepareErr != nil {
			prepared = false
			result.Reason = reason
			_, _ = output.Write([]byte("\n依赖准备未完成：" + prepareErr.Error() + "\n"))
		}
		if inputErr := r.checkInputs(workDir, a.Files); inputErr != nil {
			prepared = false
			result.Reason = "input_changed"
		}
	}
	var cmd *exec.Cmd
	var runErr error
	if prepared {
		cmd = exec.CommandContext(commandCtx, spec.Command[0], spec.Command[1:]...)
		result.ExecutablePath = cmd.Path
		if hash, hashErr := executableHash(cmd.Path); hashErr == nil {
			result.ExecutableHash = hash
		}
		cmd.Dir = workDir
		cmd.Env = env
		cmd.Stdout = output
		cmd.Stderr = output
		configureProcess(cmd)
		cmd.WaitDelay = 2 * time.Second
		runErr = cmd.Start()
		if runErr == nil {
			result.ProcessID = cmd.Process.Pid
			if persistErr := writeJSON(filepath.Join(dir, "process.json"), map[string]any{"operationId": operationID, "pid": result.ProcessID, "startedAt": result.StartedAt}); persistErr != nil {
				cleanupProcess(cmd)
				_ = cmd.Wait()
				return nil, persistErr
			}
			runErr = cmd.Wait()
		}
		cleanupProcess(cmd)
		if cmd.ProcessState != nil {
			result.ExitCode = cmd.ProcessState.ExitCode()
		}
	}
	switch {
	case ctx.Err() != nil:
		result.Status = StatusInterrupted
		result.Reason = "cancelled"
		result.ExitCode = -1
	case commandCtx.Err() == context.DeadlineExceeded:
		result.Reason = "timeout"
		result.ExitCode = -1
	case !prepared:
		// 环境失败不作为业务断言失败，具体原因在准备阶段记录。
	case errors.Is(runErr, exec.ErrWaitDelay):
		result.Reason = "descendant_cleanup"
	case runErr != nil && cmd.ProcessState == nil:
		result.Reason = "start_failed"
	case runErr != nil && result.ExitCode < 0:
		result.Reason = "process_interrupted"
	case runErr != nil:
		result.Status = StatusFailed
		result.Reason = "nonzero_exit"
	default:
		result.Status = StatusPassed
	}
	if runErr != nil && (cmd == nil || cmd.ProcessState == nil) {
		_, _ = output.Write([]byte(runErr.Error()))
	}
	// 评测副本允许生成输出，但不能修改原冻结制品或其源后再签发成功。
	if _, verifyErr := r.LoadArtifact(context.WithoutCancel(ctx), a.ID); verifyErr != nil {
		result.Status = StatusInconclusive
		result.Reason = "artifact_changed"
	}
	if inputErr := r.checkInputs(workDir, a.Files); inputErr != nil {
		result.Status = StatusInconclusive
		result.Reason = "input_changed"
	}
	if sourceErr := r.sourceMatches(context.WithoutCancel(ctx), a); sourceErr != nil {
		result.Status = StatusInconclusive
		result.Reason = "source_changed"
	}
	result.FinishedAt = time.Now().UTC()
	result.OutputHead = output.String()
	result.Truncated = output.truncated
	if err := os.WriteFile(result.LogPath, output.Bytes(), 0600); err != nil {
		return nil, err
	}
	logHash := sha256.Sum256(output.Bytes())
	result.LogHash = hex.EncodeToString(logHash[:])
	if err := writeJSON(filepath.Join(dir, "result.json"), result); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Runner) LoadResult(operationID string) (*Result, error) {
	dir, err := r.location("evals", operationID)
	if err != nil {
		return nil, err
	}
	root, err := openPlainRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	data, err := readBounded(root, "result.json", 2<<20)
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 {
		return nil, errors.New("评测结果超过上限")
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if result.OperationID != operationID || result.LogPath != filepath.Join(dir, "output.log") || result.Isolation != IsolationLocalSnapshot {
		return nil, errors.New("评测结果身份无效")
	}
	log, err := readBounded(root, "output.log", int64(r.Limits.normalized().MaxOutputBytes))
	if err != nil {
		return nil, err
	}
	if len(log) > r.Limits.normalized().MaxOutputBytes {
		return nil, errors.New("评测日志超过上限")
	}
	hash := sha256.Sum256(log)
	if result.LogHash != hex.EncodeToString(hash[:]) {
		return nil, ErrChanged
	}
	return &result, nil
}

func (r *Runner) checkInputs(dir string, expected []File) error {
	root, err := openPlainRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range expected {
		info, err := root.Lstat(filepath.FromSlash(f.Path))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return ErrChanged
		}
		current, err := readRootFile(root, f.Path, info.Mode(), r.Limits.normalized())
		if err != nil {
			return err
		}
		if current.File != f {
			return ErrChanged
		}
	}
	return nil
}

func readBounded(root *os.Root, name string, maxBytes int64) ([]byte, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("制品数据超过上限")
	}
	return data, nil
}

func (r *Runner) sourceMatches(ctx context.Context, a *Artifact) error {
	if a.SourceDir == "" {
		return nil
	}
	_, hash, err := r.Fingerprint(ctx, a.SourceDir)
	if err != nil {
		return err
	}
	if hash != a.ContentHash {
		return ErrChanged
	}
	return nil
}

func evaluationEnvironment(dir string, extra map[string]string) ([]string, error) {
	env := systemEnvironment()
	values := map[string]string{
		"HOME": filepath.Join(dir, "home"), "USERPROFILE": filepath.Join(dir, "home"), "TMPDIR": filepath.Join(dir, "tmp"), "TMP": filepath.Join(dir, "tmp"), "TEMP": filepath.Join(dir, "tmp"),
		"XDG_CACHE_HOME": filepath.Join(dir, "cache"), "GOCACHE": filepath.Join(dir, "cache", "go-build"), "GOPATH": filepath.Join(dir, "cache", "go"),
		"GOMODCACHE":       filepath.Join(dir, "cache", "go", "pkg", "mod"),
		"npm_config_cache": filepath.Join(dir, "cache", "npm"), "npm_config_userconfig": os.DevNull, "YARN_CACHE_FOLDER": filepath.Join(dir, "cache", "yarn"), "PNPM_STORE_DIR": filepath.Join(dir, "cache", "pnpm"),
		"CI": "1", "NO_COLOR": "1", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0",
	}
	for _, name := range []string{"home", "tmp", "cache"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0700); err != nil {
			return nil, err
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// 先删除默认环境中的同名变量，避免 exec 环境在不同平台出现不同取值。
	filtered := env[:0]
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, ok := values[key]; !ok {
			filtered = append(filtered, entry)
		}
	}
	env = filtered
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env, nil
}

type boundedWriter struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	room := max(w.limit-len(w.data), 0)
	if len(p) > room {
		p = p[:room]
		w.truncated = true
	}
	w.data = append(w.data, p...)
	return n, nil
}
func (w *boundedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	data := w.data
	for len(data) > 0 && !utf8.Valid(data) {
		data = data[:len(data)-1]
	}
	return string(data)
}
func (w *boundedWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data...)
}

func hashJSON(value any) string {
	data, _ := json.Marshal(value)
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func executableHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<20 {
		return "", errors.New("可执行文件类型或大小不受支持")
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 256<<20+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(data)
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}

var _ io.Writer = (*boundedWriter)(nil)
