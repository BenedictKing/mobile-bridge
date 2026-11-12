package team

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// NormalizeTouchedPaths 返回可安全用于 git pathspec 的仓库相对路径。
func NormalizeTouchedPaths(paths []string) []string {
	seen := map[string]bool{}
	var normalized []string
	for _, value := range paths {
		value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
		if value == "" || strings.Contains(value, ":") || strings.HasPrefix(value, "/") {
			continue
		}
		clean := path.Clean(value)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || seen[clean] {
			continue
		}
		seen[clean] = true
		normalized = append(normalized, clean)
	}
	sort.Strings(normalized)
	return normalized
}

func reviewTouchedPaths(run *Run, task *Task) []string {
	if run == nil || task == nil {
		return nil
	}
	var paths []string
	for _, taskID := range task.ReviewOf {
		reviewed := findTaskIn(run, taskID)
		if reviewed == nil || reviewed.ResultDetail == nil {
			continue
		}
		paths = append(paths, reviewed.ResultDetail.TouchedPaths...)
	}
	return NormalizeTouchedPaths(paths)
}

func touchedPathsOverlap(left, right []string) bool {
	for _, a := range left {
		for _, b := range right {
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return true
			}
		}
	}
	return false
}

func reviewStaleAfterWrite(review, write *Task) bool {
	if review == nil || write == nil || len(review.ReviewPaths) == 0 || write.ResultDetail == nil || len(write.ResultDetail.TouchedPaths) == 0 {
		return true
	}
	return touchedPathsOverlap(review.ReviewPaths, write.ResultDetail.TouchedPaths)
}

func (s *Service) fingerprintForPaths(cwd string, paths []string) (string, string, error) {
	if len(paths) > 0 {
		if fingerprinter, ok := s.Fingerprints.(WorkspacePathFingerprinter); ok {
			return fingerprinter.FingerprintPaths(cwd, paths)
		}
	}
	return s.Fingerprints.Fingerprint(cwd)
}

// fingerprintFreshForPaths 专供严格门禁，旧展示/审查路径继续使用原有 helper。
func (s *Service) fingerprintFreshForPaths(cwd string, paths []string) (string, string, error) {
	if s == nil {
		return "", "", ErrFreshWorkspaceFingerprintUnavailable
	}
	fingerprinter, ok := s.Fingerprints.(WorkspaceFreshFingerprinter)
	if !ok {
		return "", "", ErrFreshWorkspaceFingerprintUnavailable
	}
	head, content, err := fingerprinter.FingerprintFresh(cwd, paths)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(head) == "" || strings.TrimSpace(content) == "" {
		return "", "", fmt.Errorf("工作区即时内容核验未返回完整身份")
	}
	return head, content, nil
}
