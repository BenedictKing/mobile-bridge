// Package kimi reads the Kimi Code session index without depending on ACP
// cursor support. ACP rows remain authoritative; this store only supplements
// sessions missing from the agent's paginated response.
package kimi

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

const maxSessionStateBytes = 1 << 20

type sessionState struct {
	Title     string           `json:"title"`
	Cwd       string           `json:"cwd"`
	WorkDir   string           `json:"workDir"`
	UpdatedAt any              `json:"updatedAt"`
	Archived  bool             `json:"archived"`
	Custom    jsoncompatObject `json:"custom"`
}

type jsoncompatObject struct {
	Cwd string `json:"cwd"`
}

// ListSessions reads <home>/sessions and returns ACP session/list-shaped rows
// sorted newest first. Unreadable, archived, or unloadable records are skipped.
func ListSessions(home string) []*jsoncompat.Object {
	workspaces, err := os.ReadDir(filepath.Join(home, "sessions"))
	if err != nil {
		return nil
	}
	rows := []*jsoncompat.Object{}
	for _, workspace := range workspaces {
		if !workspace.IsDir() || strings.HasPrefix(workspace.Name(), ".") {
			continue
		}
		workspaceDir := filepath.Join(home, "sessions", workspace.Name())
		sessions, err := os.ReadDir(workspaceDir)
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			row, _ := readSessionRow(workspaceDir, session.Name())
			if row != nil {
				rows = append(rows, row)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, _ := rows[i].MustGet("updatedAt").(json.Number)
		right, _ := rows[j].MustGet("updatedAt").(json.Number)
		leftTime, _ := left.Float64()
		rightTime, _ := right.Float64()
		return leftTime > rightTime
	})
	return rows
}

func readSessionRow(workspaceDir, sessionID string) (*jsoncompat.Object, int64) {
	for _, relative := range []string{"state.json", filepath.Join("session-meta", "state.json")} {
		path := filepath.Join(workspaceDir, sessionID, relative)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxSessionStateBytes {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var state sessionState
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&state); err != nil {
			continue
		}
		cwd := firstNonEmpty(state.Cwd, state.WorkDir, state.Custom.Cwd)
		if state.Archived || cwd == "" {
			return nil, 0
		}
		updated := parseSessionTime(state.UpdatedAt)
		if updated == 0 {
			updated = info.ModTime().UnixMilli()
		}
		row := jsoncompat.NewObject()
		row.Set("sessionId", sessionID)
		if state.Title != "" {
			row.Set("title", state.Title)
		}
		row.Set("cwd", cwd)
		row.Set("updatedAt", jsoncompat.Num(int(updated)))
		return row, updated
	}
	return nil, 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseSessionTime(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return int64(parsed)
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, typed)
		if err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}
