package httpd

// teams_workspace.go 是 team.WorkspaceAdapter 的 git 实现（§6.4 阶段二 +
// §16 默认决策 6：第一版不自动创建 worktree，只引用用户经注册表预备好的
// worktree）。全部 git 调用为固定 argv 的 exec.Command，绝不过 shell。
// 注册表是 {dataDir}/team-workspaces.json（{id:{path,createdAt,createdBy}}），
// 读取走整文件读（原子替换写下对读者始终是完整文件）；写入用唯一临时
// 文件 + rename 的原子替换（team/store.go atomicWriteFile 同惯例）。注册/
// 列表的 HTTP 入口归阶段二 API 侧（httpd-api-2），本文件只提供读写原语与
// 适配器，main 装配方经 NewTeamWorkspaceAdapter 直赋 Service.Workspaces。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BenedictKing/mobile-bridge/internal/team"
)

// teamWorkspaceEntry 是注册表里的单个 worktree 记录（path 必填；createdAt/
// createdBy 供审计展示，由注册入口填写）。
type teamWorkspaceEntry struct {
	Path      string `json:"path"`
	CreatedAt string `json:"createdAt,omitempty"`
	CreatedBy string `json:"createdBy,omitempty"`
}

// loadTeamWorkspaceRegistry 读取注册表；文件不存在视为空表（首次注册前），
// 损坏（非法 JSON）按错误上抛——静默当空表会让全部 isolated 任务误报
// 「工作区不存在」。
func loadTeamWorkspaceRegistry(path string) (map[string]teamWorkspaceEntry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]teamWorkspaceEntry{}, nil
		}
		return nil, fmt.Errorf("工作区注册表读取失败: %w", err)
	}
	entries := map[string]teamWorkspaceEntry{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return entries, nil
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("工作区注册表损坏: %w", err)
	}
	return entries, nil
}

// saveTeamWorkspaceRegistry 原子替换写入注册表（唯一临时文件名 + rename，
// 0600，team/store.go atomicWriteFile 同惯例）。调用方负责注册表级的读改
// 写串行（HTTP 处理器自身串行）。
func saveTeamWorkspaceRegistry(path string, entries map[string]teamWorkspaceEntry) error {
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// teamWorkspaceAdapter 把 WorkspaceAdapter 落到注册表 + git CLI。
type teamWorkspaceAdapter struct {
	registryPath string
}

// NewTeamWorkspaceAdapter 返回 worktree 适配器；registryPath 通常是
// {dataDir}/team-workspaces.json（main 装配，构造后直赋 Service.Workspaces，
// 与 NewTeamAdapters 同惯例）。
func NewTeamWorkspaceAdapter(registryPath string) team.WorkspaceAdapter {
	return teamWorkspaceAdapter{registryPath: registryPath}
}

// Resolve 从注册表解析 workspaceID → 路径。
func (a teamWorkspaceAdapter) Resolve(workspaceID string) (string, error) {
	entries, err := loadTeamWorkspaceRegistry(a.registryPath)
	if err != nil {
		return "", err
	}
	entry, ok := entries[workspaceID]
	if !ok || strings.TrimSpace(entry.Path) == "" {
		return "", fmt.Errorf("工作区未注册：%s", workspaceID)
	}
	return entry.Path, nil
}

// HeadRevision 读取 worktree 当前 HEAD（rev-parse 固定 argv）。
func (a teamWorkspaceAdapter) HeadRevision(path string) (string, error) {
	out, err := gitWorkspaceOutput(path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	revision := strings.TrimSpace(out)
	if revision == "" {
		return "", fmt.Errorf("工作区 %s 没有提交历史（空仓库无基线）", path)
	}
	return revision, nil
}

// Diff 返回 worktree 相对 baseRevision 的逐文件有界 diff。
func (a teamWorkspaceAdapter) Diff(path, baseRevision string) ([]team.WorkspaceDiff, bool, error) {
	out, err := gitWorkspaceOutput(path, "diff", baseRevision, "--unified=3")
	if err != nil {
		return nil, false, err
	}
	return boundWorkspaceDiffs(parseUnifiedDiff(out))
}

// Cleanup 删除 workspaceID 注册的 worktree：`git worktree remove` 不带
// --force，脏区（未提交改动）时 git 拒绝并失败上抛——绝不丢用户改动。
func (a teamWorkspaceAdapter) Cleanup(workspaceID string) error {
	entries, err := loadTeamWorkspaceRegistry(a.registryPath)
	if err != nil {
		return err
	}
	entry, ok := entries[workspaceID]
	if !ok || strings.TrimSpace(entry.Path) == "" {
		return fmt.Errorf("工作区未注册：%s", workspaceID)
	}
	_, err = gitWorkspaceOutput(entry.Path, "worktree", "remove", entry.Path)
	return err
}

// gitWorkspaceOutput 在 dir 下执行固定 argv 的 git 子命令（绝不过 shell），
// 返回 stdout；失败时错误文本携带有界的 stderr 摘要（git 的中文/英文
// fatal 信息对定位最有价值）。全局附带 core.quotePath=false：diff 的路径
// 输出保持原始 UTF-8，不做八进制转义（否则中文文件名会变成 "\346..."）。
func gitWorkspaceOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-c", "core.quotePath=false", "-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		reason := strings.TrimSpace(stderr.String())
		if reason == "" {
			reason = err.Error()
		}
		if len(reason) > 300 {
			reason = reason[:300]
		}
		return "", fmt.Errorf("git %s 失败：%s", strings.Join(args, " "), reason)
	}
	return string(out), nil
}

// diff 数据面的双层截断常量：单文件 20000 字节、整包 ≤50 文件且总量
// ≤512000 字节；超限置对应 truncated 标记（§6.4 规则 6 的有界 payload）。
const (
	maxWorkspaceDiffFileBytes  = 20000
	maxWorkspaceDiffFiles      = 50
	maxWorkspaceDiffTotalBytes = 512000
)

// parseUnifiedDiff 把 `git diff --unified=3` 输出按 "diff --git " 行分文件
// 重组：路径优先取 "+++ b/<path>"（新文件侧；删除文件取 "--- a/<path>"），
// 二进制文件退化解析 "diff --git a/x b/x" 头；Added/Removed 统计 hunk 内
// +/- 行（+++/--- 头除外），DiffText 保留该文件的完整输出段（含 hunk 头
// 与上下文行，供前端 unified diff 渲染直接消费）。
func parseUnifiedDiff(out string) []team.WorkspaceDiff {
	if strings.TrimSpace(out) == "" {
		return nil
	}
	lines := strings.Split(out, "\n")
	files := []team.WorkspaceDiff{}
	current := (*team.WorkspaceDiff)(nil)
	flush := func() {
		if current != nil {
			files = append(files, *current)
			current = nil
		}
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			current = &team.WorkspaceDiff{Path: diffGitHeaderPath(line)}
			current.DiffText = line + "\n"
			continue
		}
		if current == nil {
			continue // 前导噪声（不应出现，防御）
		}
		switch {
		case strings.HasPrefix(line, "+++"):
			if path := diffHeaderSidePath(line[3:]); path != "" && current.Path == "" {
				current.Path = path
			}
		case strings.HasPrefix(line, "---"):
			if path := diffHeaderSidePath(line[3:]); path != "" && current.Path == "" {
				current.Path = path
			}
		case strings.HasPrefix(line, "+"):
			current.Added++
		case strings.HasPrefix(line, "-"):
			current.Removed++
		}
		current.DiffText += line + "\n"
	}
	flush()
	return files
}

// diffHeaderSidePath 解析 "+++ b/<path>" / "--- a/<path>" 头的内容侧路径：
// 去掉前缀与可选的 \t 时间戳、外层引号；a/、b/ 之外的取值（/dev/null 等）
// 返回空串。
func diffHeaderSidePath(rest string) string {
	text := strings.TrimSpace(rest)
	if index := strings.IndexByte(text, '\t'); index >= 0 {
		text = strings.TrimSpace(text[:index])
	}
	if len(text) >= 2 && strings.HasPrefix(text, "\"") && strings.HasSuffix(text, "\"") {
		text = text[1 : len(text)-1]
	}
	for _, prefix := range []string{"b/", "a/"} {
		if strings.HasPrefix(text, prefix) {
			return text[len(prefix):]
		}
	}
	return ""
}

// diffGitHeaderPath 从 "diff --git a/<path> b/<path>" 头退化解析路径
// （二进制 diff 没有 +++/--- 行）；带引号或含空格的路径不做完整还原，
// 留空交由调用方按未识别路径处理。
func diffGitHeaderPath(line string) string {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "diff --git"))
	if strings.ContainsAny(rest, "\"") {
		return ""
	}
	index := strings.Index(rest, " b/")
	if index < 0 {
		return ""
	}
	path := rest[index+3:]
	if path == "" || strings.Contains(path, " ") {
		return "" // 无法可靠切分：宁缺毋错
	}
	return path
}

// boundWorkspaceDiffs 应用双层截断：逐文件按 min(单文件上限, 剩余整包预算)
// 截断（该文件置 Truncated），文件数或累计字节超限时停止携带后续文件并置
// 顶层 truncated。仅单文件超自身上限时只置该文件标记，不动顶层（两层的
// 触发口径独立：文件级=20000 字节，顶层=50 文件/512000 字节）。Added/Removed
// 保持全量计数（截断只影响展示正文）。
func boundWorkspaceDiffs(files []team.WorkspaceDiff) ([]team.WorkspaceDiff, bool, error) {
	bounded := make([]team.WorkspaceDiff, 0, len(files))
	truncated := false
	total := 0
	for _, file := range files {
		if len(bounded) >= maxWorkspaceDiffFiles || total >= maxWorkspaceDiffTotalBytes {
			truncated = true
			break
		}
		limit := maxWorkspaceDiffFileBytes
		if remaining := maxWorkspaceDiffTotalBytes - total; remaining < limit {
			limit = remaining
		}
		text := file.DiffText
		fileTruncated := false
		if len(text) > limit {
			text = cutUTF8(text, limit)
			fileTruncated = true
			if limit < maxWorkspaceDiffFileBytes {
				// 截断由整包剩余预算决定：总量口径触发顶层标记。
				truncated = true
			}
		}
		total += len(text)
		bounded = append(bounded, team.WorkspaceDiff{
			Path:      file.Path,
			DiffText:  text,
			Added:     file.Added,
			Removed:   file.Removed,
			Truncated: fileTruncated,
		})
	}
	return bounded, truncated, nil
}

// cutUTF8 在不超过 max 字节的前提下按 rune 边界截断（多字节字符不劈半）。
func cutUTF8(text string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(text) <= max {
		return text
	}
	cut := max
	for cut > 0 && cut > max-utf8.UTFMax && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut]
}
