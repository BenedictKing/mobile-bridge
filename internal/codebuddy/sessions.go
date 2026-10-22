// Package codebuddy reads the CodeBuddy CLI's own on-disk session store.
//
// `codebuddy --acp` implements session/load but not session/list (it answers
// -32601), so the gateway's conversation list would never show a single
// WorkBuddy chat. The CLI persists every session as one JSONL transcript at
// ~/.codebuddy/projects/<cwd-slug>/<sessionId>.jsonl — the same layout as
// Claude Code — which is readable without touching the CLI.
package codebuddy

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// listReadCap bounds how much of one transcript's head is scanned for the
// session-meta / ai-title / first-user-message lines (all appear within the
// first turn); the recency timestamp comes from the file mtime instead.
const listReadCap = 1 << 20

// ProjectsDir is the CLI's session transcript root under the user's home.
func ProjectsDir(home string) string {
	return filepath.Join(home, ".codebuddy", "projects")
}

// transcriptLine is the subset of the JSONL row shapes a list row needs.
type transcriptLine struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	AiTitle   string `json:"aiTitle"`
	Role      string `json:"role"`
	Cwd       string `json:"cwd"`
	Content   []struct {
		Text string `json:"text"`
	} `json:"content"`
}

// ListSessions enumerates the CLI's persisted sessions as ACP session/list
// rows (sessionId/title/cwd/updatedAt epoch ms). The title is the CLI's own
// ai-title with the first user message as fallback; updatedAt is the
// transcript mtime — the CLI appends every event, so it is the real last
// activity. Transcripts without a cwd (empty or meta-only in-progress files)
// are skipped: session/load needs a cwd, so they could never be opened. A
// missing store yields nil.
func ListSessions(home string) []*jsoncompat.Object {
	projectsDir := ProjectsDir(home)
	projects, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}
	rows := []*jsoncompat.Object{}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			row := readSessionRow(filepath.Join(dir, entry.Name()), strings.TrimSuffix(entry.Name(), ".jsonl"))
			if row != nil {
				rows = append(rows, row)
			}
		}
	}
	return rows
}

// readSessionRow scans one transcript's head for the list-row facts; nil when
// not a single line parses (empty in-progress transcripts).
func readSessionRow(path, fallbackID string) *jsoncompat.Object {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	var sessionID, title, cwd string
	parsed := false
	reader := bufio.NewReader(io.LimitReader(file, listReadCap))
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var entry transcriptLine
			if json.Unmarshal(line, &entry) == nil && entry.Type != "" {
				parsed = true
				if entry.SessionID != "" {
					sessionID = entry.SessionID
				}
				if entry.Type == "ai-title" && strings.TrimSpace(entry.AiTitle) != "" {
					title = strings.TrimSpace(entry.AiTitle)
				}
				if title == "" && entry.Type == "message" && entry.Role == "user" {
					for _, block := range entry.Content {
						if text := strings.TrimSpace(block.Text); text != "" {
							title = text
							break
						}
					}
				}
				if cwd == "" {
					cwd = strings.TrimSpace(entry.Cwd)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	if !parsed || cwd == "" {
		return nil
	}
	if sessionID == "" {
		sessionID = fallbackID
	}
	row := jsoncompat.NewObject()
	row.Set("sessionId", sessionID)
	if title != "" {
		row.Set("title", title)
	}
	row.Set("cwd", cwd)
	row.Set("updatedAt", jsoncompat.Num(int(info.ModTime().UnixMilli())))
	return row
}
