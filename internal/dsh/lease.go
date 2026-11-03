package dsh

import (
	"path/filepath"
	"strings"
)

// WriteOwner probes the kernel lock held by a DSH writer. known=false means
// this platform cannot prove ownership; callers must retain their fallback.
func WriteOwner(root, sessionID string) (owned, known bool) {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
		return false, false
	}
	matches, err := filepath.Glob(filepath.Join(root, "sessions", "*", sessionID))
	if err != nil {
		return false, false
	}
	paths := make([]string, 0, len(matches))
	for _, dir := range matches {
		paths = append(paths, filepath.Join(dir, "session.lock"))
	}
	return probeWriteOwner(paths)
}
