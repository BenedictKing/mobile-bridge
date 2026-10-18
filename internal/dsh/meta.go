// Package dsh reads the DeepSeek Harness app's own on-disk session state.
//
// dsh-acp lists every DSH session, but rows for sessions the desktop app
// currently owns carry no title (and a creation-time updatedAt), so the phone
// list showed the conversation in use as 未命名聊天, buried under its
// creation time. The app keeps the real title in its projection cache and the
// authoritative last-activity time on the session transcript, both readable
// without touching the app.
package dsh

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// MetaReader returns a resolver for (title, last activity in ms) keyed by
// session id. Missing or malformed files yield empty results; the caller
// keeps the agent's own row fields.
func MetaReader(root string) func(sessionID string) (string, int64) {
	cacheDir := filepath.Join(root, "storages", "session_projcache", "sessions")
	sessionsDir := filepath.Join(root, "sessions")
	return func(sessionID string) (string, int64) {
		if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
			return "", 0
		}
		title := readTitle(filepath.Join(cacheDir, sessionID+".json"))
		return title, transcriptActivity(sessionsDir, sessionID)
	}
}

// readTitle pulls record.rows.title.val (the app's sidebar title) with the
// first-user-message input as a fallback.
func readTitle(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return ""
	}
	record, _ := value.(*jsoncompat.Object)
	if record == nil {
		return ""
	}
	if inner, ok := record.MustGet("record").(*jsoncompat.Object); ok && inner != nil {
		record = inner
	}
	if rows, ok := record.MustGet("rows").(*jsoncompat.Object); ok && rows != nil {
		if title, ok := rows.MustGet("title").(*jsoncompat.Object); ok && title != nil {
			if text, _ := title.MustGet("val").(string); strings.TrimSpace(text) != "" {
				return strings.TrimSpace(text)
			}
		}
		if input, ok := rows.MustGet("titleInput").(*jsoncompat.Object); ok && input != nil {
			if val, ok := input.MustGet("val").(*jsoncompat.Object); ok && val != nil {
				if first, ok := val.MustGet("first").(*jsoncompat.Object); ok && first != nil {
					if text, _ := first.MustGet("text").(string); strings.TrimSpace(text) != "" {
						return strings.TrimSpace(text)
					}
				}
			}
		}
	}
	return ""
}

// transcriptActivity is the session transcript's mtime: the app appends every
// event there, so it is the real "recent interaction" signal dsh-acp's
// creation-time updatedAt lacks. A session directory may hold more than one
// transcript generation; the newest wins.
func transcriptActivity(sessionsDir, sessionID string) int64 {
	matches, err := filepath.Glob(filepath.Join(sessionsDir, "*", sessionID, "session.*"))
	if err != nil {
		return 0
	}
	var newest int64
	for _, match := range matches {
		if strings.HasSuffix(match, ".lock") {
			continue
		}
		info, err := os.Stat(match)
		if err != nil || info.IsDir() {
			continue
		}
		if updated := info.ModTime().UnixMilli(); updated > newest {
			newest = updated
		}
	}
	return newest
}
