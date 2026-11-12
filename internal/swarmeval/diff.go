package swarmeval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const MaxDiffBytes = 512 << 10

// Diff 返回冻结候选相对其已验证基线的有界 unified/binary patch。
// 路径仅含 a/<仓库路径> 和 b/<仓库路径>；截断内容只供阅读，不可直接应用。
func (r *Runner) Diff(ctx context.Context, id string) (text string, truncated bool, err error) {
	a, err := r.LoadArtifact(ctx, id)
	if err != nil {
		return "", false, err
	}
	base := map[string]bool{}
	current := map[string]bool{}
	for _, f := range a.BaseFiles {
		base[f.Path] = true
	}
	for _, f := range a.Files {
		current[f.Path] = true
	}
	dir := filepath.Dir(a.Directory)
	var patch strings.Builder
	for _, name := range a.ChangedPaths {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if !safePath(name) {
			return "", false, errors.New("候选差异路径无效")
		}
		left, right := filepath.Join("base", filepath.FromSlash(name)), filepath.Join("tree", filepath.FromSlash(name))
		if !base[name] {
			left = os.DevNull
		}
		if !current[name] {
			right = os.DevNull
		}
		remaining := MaxDiffBytes - patch.Len()
		if remaining <= 0 {
			truncated = true
			break
		}
		output := &boundedWriter{limit: remaining}
		stderr := &boundedWriter{limit: 2048}
		cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.quotePath=false", "-c", "diff.external=", "diff", "--no-index", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", "--", left, right)
		cmd.Dir = dir
		cmd.Env = append(systemEnvironment(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
		cmd.Stdout = output
		cmd.Stderr = stderr
		runErr := cmd.Run()
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if runErr != nil {
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || exit.ExitCode() != 1 {
				return "", false, fmt.Errorf("生成候选差异失败：%s：%w", stderr.String(), runErr)
			}
		}
		normalized := normalizePatchHeaders(output.String(), name, base[name], current[name])
		if len(normalized) > remaining {
			normalized = normalized[:remaining]
			for len(normalized) > 0 && !utf8.ValidString(normalized) {
				normalized = normalized[:len(normalized)-1]
			}
			truncated = true
		}
		patch.WriteString(normalized)
		if output.truncated {
			truncated = true
		}
		if truncated {
			break
		}
	}
	if _, err := r.LoadArtifact(ctx, id); err != nil {
		return "", false, err
	}
	return patch.String(), truncated, nil
}

func normalizePatchHeaders(text, name string, hasBefore, hasAfter bool) string {
	lines := strings.SplitAfter(text, "\n")
	headers := true
	for i, line := range lines {
		if strings.HasPrefix(line, "@@") || strings.HasPrefix(line, "GIT binary patch") {
			headers = false
		}
		if !headers {
			continue
		}
		switch {
		case strings.HasPrefix(line, "diff --git "):
			lines[i] = "diff --git " + quotePatchPath("a/"+name) + " " + quotePatchPath("b/"+name) + "\n"
		case strings.HasPrefix(line, "--- "):
			path := "/dev/null"
			if hasBefore {
				path = quotePatchPath("a/" + name)
			}
			lines[i] = "--- " + path + "\n"
		case strings.HasPrefix(line, "+++ "):
			path := "/dev/null"
			if hasAfter {
				path = quotePatchPath("b/" + name)
			}
			lines[i] = "+++ " + path + "\n"
		}
	}
	return strings.Join(lines, "")
}

func quotePatchPath(path string) string {
	quote := false
	for i := 0; i < len(path); i++ {
		if path[i] <= 32 || path[i] == 127 || path[i] == '"' || path[i] == '\\' {
			quote = true
			break
		}
	}
	if !quote {
		return path
	}
	var out strings.Builder
	out.WriteByte('"')
	for i := 0; i < len(path); i++ {
		b := path[i]
		switch {
		case b == '"' || b == '\\':
			out.WriteByte('\\')
			out.WriteByte(b)
		case b == '\t':
			out.WriteString(`\t`)
		case b < 32 || b == 127:
			fmt.Fprintf(&out, `\%03o`, b)
		default:
			out.WriteByte(b)
		}
	}
	out.WriteByte('"')
	return out.String()
}
