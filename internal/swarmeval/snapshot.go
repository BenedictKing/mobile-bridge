// Package swarmeval 提供本地候选快照与受控评测。local_snapshot 隔离工作副本，
// 不提供操作系统沙箱；调用方不能把结果当作网络或文件权限已经隔离的证明。
package swarmeval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const IsolationLocalSnapshot = "local_snapshot"

var (
	ErrConflict      = errors.New("评测操作标识或候选内容冲突")
	ErrChanged       = errors.New("候选源内容已变化")
	operationPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
)

type Limits struct {
	MaxFiles       int
	MaxFileBytes   int64
	MaxTotalBytes  int64
	MaxOutputBytes int
}

func (l Limits) normalized() Limits {
	if l.MaxFiles <= 0 {
		l.MaxFiles = 20000
	}
	if l.MaxFileBytes <= 0 {
		l.MaxFileBytes = 32 << 20
	}
	if l.MaxTotalBytes <= 0 {
		l.MaxTotalBytes = 256 << 20
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = 64 << 10
	}
	return l
}

type Runner struct {
	Root   string
	Limits Limits
}

type File struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Size int64  `json:"size"`
	Hash string `json:"hash"`
}

type Artifact struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	Directory    string    `json:"directory"`
	SourceDir    string    `json:"sourceDir,omitempty"`
	HeadRevision string    `json:"headRevision"`
	BaseRevision string    `json:"baseRevision"`
	BaseHash     string    `json:"baseHash"`
	ContentHash  string    `json:"contentHash"`
	ChangedPaths []string  `json:"changedPaths"`
	Files        []File    `json:"files"`
	BaseFiles    []File    `json:"baseFiles"`
	Inputs       []string  `json:"inputs,omitempty"`
	Isolation    string    `json:"isolation"`
	CreatedAt    time.Time `json:"createdAt"`
}

type fileData struct {
	File
	data []byte
}

// Capture 冻结 Git 工作区的当前内容，包括未跟踪且未忽略的普通文件。
// 仅执行只读 Git 命令，不写源 index/objects，不创建分支或 worktree。
func (r *Runner) Capture(ctx context.Context, operationID, sourceDir, baseRevision string) (*Artifact, error) {
	dest, err := r.location("artifacts", operationID)
	if err != nil {
		return nil, err
	}
	source, head, current, err := r.readSource(ctx, sourceDir)
	if err != nil {
		return nil, err
	}
	if baseRevision == "" {
		baseRevision = head
	}
	base, err := r.readBase(ctx, source, baseRevision)
	if err != nil {
		return nil, err
	}
	resolved, err := gitOutput(ctx, source, 200, "rev-parse", "--verify", baseRevision+"^{commit}")
	if err != nil {
		return nil, err
	}
	a := &Artifact{Version: 1, ID: operationID, Directory: filepath.Join(dest, "tree"), SourceDir: source,
		HeadRevision: head, BaseRevision: strings.TrimSpace(string(resolved)), BaseHash: treeHash(base),
		ContentHash: treeHash(current), Files: manifest(current), BaseFiles: manifest(base),
		ChangedPaths: changedPaths(base, current), Isolation: IsolationLocalSnapshot, CreatedAt: time.Now().UTC()}
	if old, loadErr := r.LoadArtifact(ctx, operationID); loadErr == nil {
		if old.SourceDir == a.SourceDir && old.ContentHash == a.ContentHash && old.BaseRevision == a.BaseRevision {
			return old, nil
		}
		return nil, ErrConflict
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return nil, loadErr
	}
	if err := r.writeArtifact(dest, a, current, base); err != nil {
		return nil, err
	}
	_, fresh, err := r.Fingerprint(ctx, source)
	if err != nil || fresh != a.ContentHash {
		if err != nil {
			return nil, err
		}
		return nil, ErrChanged
	}
	return a, nil
}

// Fingerprint 不使用缓存。相同文件内容在提交前后保持相同 contentHash。
func (r *Runner) Fingerprint(ctx context.Context, sourceDir string) (head, contentHash string, err error) {
	_, head, files, err := r.readSource(ctx, sourceDir)
	if err != nil {
		return "", "", err
	}
	return head, treeHash(files), nil
}

func (r *Runner) location(kind, id string) (string, error) {
	if !operationPattern.MatchString(id) {
		return "", errors.New("操作标识无效")
	}
	root, err := filepath.Abs(r.Root)
	if err != nil || strings.TrimSpace(r.Root) == "" {
		return "", errors.New("评测存储目录不能为空")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(real, kind), 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(filepath.Join(real, kind))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("评测存储路径不是普通目录")
	}
	return filepath.Join(real, kind, id), nil
}

func (r *Runner) readSource(ctx context.Context, sourceDir string) (string, string, []fileData, error) {
	source, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", "", nil, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return "", "", nil, err
	}
	root, err := gitOutput(ctx, source, 1<<20, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", nil, err
	}
	gitRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(root)))
	if err != nil || gitRoot != source {
		return "", "", nil, errors.New("候选目录必须是 Git 工作区根目录")
	}
	head, err := gitOutput(ctx, source, 200, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", "", nil, err
	}
	names, err := gitOutput(ctx, source, int64(r.Limits.normalized().MaxFiles)*4097, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return "", "", nil, err
	}
	paths := map[string]bool{}
	for _, name := range bytes.Split(names, []byte{0}) {
		if len(name) > 0 {
			paths[string(name)] = true
		}
	}
	if len(paths) > r.Limits.normalized().MaxFiles {
		return "", "", nil, errors.New("候选文件数超过上限")
	}
	opened, err := os.OpenRoot(source)
	if err != nil {
		return "", "", nil, err
	}
	defer opened.Close()
	files := make([]fileData, 0, len(paths))
	var total int64
	managed, _ := filepath.Abs(r.Root)
	if resolved, resolveErr := filepath.EvalSymlinks(managed); resolveErr == nil {
		managed = resolved
	}
	for name := range paths {
		if ctx.Err() != nil {
			return "", "", nil, ctx.Err()
		}
		if !safePath(name) {
			return "", "", nil, fmt.Errorf("无效候选路径：%q", name)
		}
		candidate := filepath.Join(source, filepath.FromSlash(name))
		if managed != "" && (candidate == managed || strings.HasPrefix(candidate, managed+string(filepath.Separator))) {
			continue
		}
		info, err := opened.Lstat(filepath.FromSlash(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", "", nil, err
		}
		if !info.Mode().IsRegular() {
			return "", "", nil, fmt.Errorf("候选仅支持普通文件：%s", name)
		}
		f, err := readRootFile(opened, name, info.Mode(), r.Limits.normalized())
		if err != nil {
			return "", "", nil, err
		}
		total += f.Size
		if total > r.Limits.normalized().MaxTotalBytes {
			return "", "", nil, errors.New("候选总大小超过上限")
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return source, strings.TrimSpace(string(head)), files, nil
}

func safePath(name string) bool {
	return name != "" && len(name) <= 4096 && !strings.ContainsAny(name, "\\\x00\r\n") && !filepath.IsAbs(name) && filepath.ToSlash(filepath.Clean(name)) == name && name != ".." && !strings.HasPrefix(name, "../") && name != ".git" && !strings.HasPrefix(name, ".git/")
}

func readRootFile(root *os.Root, name string, mode fs.FileMode, limits Limits) (fileData, error) {
	f, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return fileData{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileData{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > limits.MaxFileBytes {
		return fileData{}, fmt.Errorf("文件类型或大小不受支持：%s", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, limits.MaxFileBytes+1))
	if err != nil {
		return fileData{}, err
	}
	if int64(len(data)) > limits.MaxFileBytes {
		return fileData{}, fmt.Errorf("文件大小超过上限：%s", name)
	}
	normalMode := uint32(0644)
	if mode&0111 != 0 {
		normalMode = 0755
	}
	h := sha256.Sum256(data)
	return fileData{File: File{Path: name, Mode: normalMode, Size: int64(len(data)), Hash: hex.EncodeToString(h[:])}, data: data}, nil
}

func treeHash(files []fileData) string {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.Path, f.Mode, f.Hash)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func manifest(files []fileData) []File {
	out := make([]File, len(files))
	for i := range files {
		out[i] = files[i].File
	}
	return out
}

func changedPaths(base, current []fileData) []string {
	old := map[string]File{}
	for _, f := range base {
		old[f.Path] = f.File
	}
	changes := map[string]bool{}
	for _, f := range current {
		if prior, ok := old[f.Path]; !ok || prior.Hash != f.Hash || prior.Mode != f.Mode {
			changes[f.Path] = true
		}
		delete(old, f.Path)
	}
	for name := range old {
		changes[name] = true
	}
	paths := make([]string, 0, len(changes))
	for name := range changes {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

func gitOutput(ctx context.Context, dir string, limit int64, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", dir}, args...)...)
	cmd.Env = append(systemEnvironment(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	out := &boundedWriter{limit: int(limit)}
	stderr := &boundedWriter{limit: 2048}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("只读 Git 操作失败：%s：%w", stderr.String(), err)
	}
	if out.truncated {
		return nil, errors.New("Git 输出超过上限")
	}
	return append([]byte(nil), out.data...), nil
}

func (r *Runner) readBase(ctx context.Context, source, revision string) ([]fileData, error) {
	if len(revision) > 100 || strings.HasPrefix(revision, "-") || strings.ContainsAny(revision, "\x00\r\n") {
		return nil, errors.New("基线版本无效")
	}
	rows, err := gitOutput(ctx, source, int64(r.Limits.normalized().MaxFiles)*4300, "ls-tree", "-rz", revision)
	if err != nil {
		return nil, err
	}
	files := []fileData{}
	var total int64
	for _, row := range bytes.Split(rows, []byte{0}) {
		if len(row) == 0 {
			continue
		}
		parts := bytes.SplitN(row, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, errors.New("Git 基线格式无效")
		}
		meta := strings.Fields(string(parts[0]))
		name := string(parts[1])
		if len(meta) != 3 || meta[1] != "blob" || (meta[0] != "100644" && meta[0] != "100755") || !safePath(name) {
			return nil, fmt.Errorf("基线存在不支持的文件：%s", name)
		}
		if len(files) >= r.Limits.normalized().MaxFiles {
			return nil, errors.New("基线文件数超过上限")
		}
		data, err := gitOutput(ctx, source, r.Limits.normalized().MaxFileBytes, "cat-file", "blob", meta[2])
		if err != nil {
			return nil, err
		}
		total += int64(len(data))
		if total > r.Limits.normalized().MaxTotalBytes {
			return nil, errors.New("基线总大小超过上限")
		}
		mode := uint32(0644)
		if meta[0] == "100755" {
			mode = 0755
		}
		hash := sha256.Sum256(data)
		files = append(files, fileData{File: File{Path: name, Mode: mode, Size: int64(len(data)), Hash: hex.EncodeToString(hash[:])}, data: data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (r *Runner) writeArtifact(dest string, a *Artifact, current, base []fileData) error {
	staging, err := os.MkdirTemp(filepath.Dir(dest), "."+a.ID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging) // 仅清理本操作创建的 staging，不涉及用户工作区。
	if err := writeTree(filepath.Join(staging, "tree"), current); err != nil {
		return err
	}
	if err := writeTree(filepath.Join(staging, "base"), base); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(staging, "artifact.json"), a); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return ErrConflict
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staging, dest); err != nil {
		return fmt.Errorf("%w：%v", ErrConflict, err)
	}
	return nil
}

func writeTree(dir string, files []fileData) error {
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	root, err := openPlainRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, f := range files {
		if !safePath(f.Path) {
			return errors.New("制品路径无效")
		}
		if err := root.MkdirAll(filepath.Dir(filepath.FromSlash(f.Path)), 0700); err != nil {
			return err
		}
		if err := root.WriteFile(filepath.FromSlash(f.Path), f.data, fs.FileMode(f.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) LoadArtifact(ctx context.Context, id string) (*Artifact, error) {
	dir, err := r.location("artifacts", id)
	if err != nil {
		return nil, err
	}
	root, err := openPlainRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open("artifact.json")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 8<<20+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("制品元数据超过上限")
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	if a.Version != 1 || a.ID != id || a.Directory != filepath.Join(dir, "tree") || a.Isolation != IsolationLocalSnapshot {
		return nil, errors.New("制品元数据身份无效")
	}
	files, err := r.readTree(ctx, a.Directory, a.Files)
	if err != nil {
		return nil, err
	}
	base, err := r.readTree(ctx, filepath.Join(dir, "base"), a.BaseFiles)
	if err != nil {
		return nil, err
	}
	if treeHash(files) != a.ContentHash || treeHash(base) != a.BaseHash || !equalStrings(changedPaths(base, files), a.ChangedPaths) {
		return nil, ErrChanged
	}
	return &a, nil
}

func (r *Runner) readTree(ctx context.Context, dir string, expected []File) ([]fileData, error) {
	if len(expected) > r.Limits.normalized().MaxFiles {
		return nil, errors.New("制品文件数超过上限")
	}
	root, err := openPlainRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	files := make([]fileData, 0, len(expected))
	var total int64
	seen := map[string]bool{}
	for _, entry := range expected {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !safePath(entry.Path) || seen[entry.Path] {
			return nil, errors.New("制品路径重复或无效")
		}
		seen[entry.Path] = true
		info, err := root.Lstat(filepath.FromSlash(entry.Path))
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, ErrChanged
		}
		f, err := readRootFile(root, entry.Path, info.Mode(), r.Limits.normalized())
		if err != nil {
			return nil, err
		}
		if f.File != entry {
			return nil, ErrChanged
		}
		total += f.Size
		if total > r.Limits.normalized().MaxTotalBytes {
			return nil, errors.New("制品总大小超过上限")
		}
		files = append(files, f)
	}
	// 新增文件也使冻结制品失效，不能只检查旧清单。
	err = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !seen[name] {
			return ErrChanged
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func systemEnvironment() []string {
	paths := []string{}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(p) {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		paths = []string{"/usr/bin", "/bin"}
	}
	env := []string{"PATH=" + strings.Join(paths, string(os.PathListSeparator)), "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	for _, key := range []string{"SYSTEMROOT", "WINDIR", "COMSPEC", "PATHEXT"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func openPlainRoot(dir string) (*os.Root, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("制品目录不能是符号链接")
	}
	return os.OpenRoot(dir)
}
