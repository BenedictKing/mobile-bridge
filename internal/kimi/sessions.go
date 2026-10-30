// Package kimi reads the Kimi Code session index without depending on ACP
// cursor support. ACP rows remain authoritative; this store only supplements
// sessions missing from the agent's paginated response.
package kimi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
)

const (
	maxSessionStateBytes = 1 << 20
	maxTitleWireBytes    = 1 << 20
)

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
		title := textutil.CleanTitle(state.Title)
		if strings.TrimSpace(title) == "" {
			title = readTitleFromWire(filepath.Join(workspaceDir, sessionID, "agents", "main", "wire.jsonl"))
		}
		if title != "" {
			row.Set("title", title)
		}
		row.Set("cwd", cwd)
		row.Set("updatedAt", jsoncompat.Num(int(updated)))
		return row, updated
	}
	return nil, 0
}

var internalTitleOrigins = map[string]bool{
	"injection": true, "system_trigger": true, "compaction_summary": true,
	"hook_result": true, "cron_job": true, "cron_missed": true, "retry": true,
}

func internalTitleOrigin(value any) bool {
	origin, _ := value.(*jsoncompat.Object)
	if origin == nil {
		return false
	}
	kind, _ := origin.MustGet("kind").(string)
	return internalTitleOrigins[kind]
}

func titleContent(value any) string {
	switch typed := value.(type) {
	case string:
		return textutil.TitleFromText(typed)
	case *jsoncompat.Object:
		text, _ := typed.MustGet("text").(string)
		return textutil.TitleFromText(text)
	case *jsoncompat.Array:
		parts := []string{}
		for _, item := range typed.Items {
			if text := titleContent(item); text != "" {
				parts = append(parts, text)
			}
		}
		return textutil.TitleFromText(strings.Join(parts, " "))
	default:
		return ""
	}
}

func readTitleFromWire(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReader(io.LimitReader(file, maxTitleWireBytes))
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			value, err := jsoncompat.Unmarshal(line)
			event, _ := value.(*jsoncompat.Object)
			if err == nil && event != nil {
				kind, _ := event.MustGet("type").(string)
				var content any
				switch kind {
				case "turn.prompt":
					if !internalTitleOrigin(event.MustGet("origin")) {
						content = event.MustGet("input")
					}
				case "context.append_message":
					message, _ := event.MustGet("message").(*jsoncompat.Object)
					if message != nil && message.MustGet("role") == "user" && !internalTitleOrigin(message.MustGet("origin")) {
						content = message.MustGet("content")
					}
				}
				if title := titleContent(content); title != "" {
					return title
				}
			}
		}
		if readErr != nil {
			return ""
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// metaIndexTTL bounds how long one session-id → wire map serves lookups
// before the sessions tree is re-enumerated. A list refresh asks for every
// row in one burst; the TTL keeps that at a single readdir pass.
const metaIndexTTL = 30 * time.Second

// MetaReader returns a resolver for (title, last interaction in ms) keyed by
// session id. Kimi Code rewrites state.json's updatedAt on every
// session/load — the gateway's startup preload issues one per recent chat and
// so does opening a chat on the phone — so rows built from that file floated
// to the top of the list after each restart. The wire transcript is not
// touched by a load and its last timestamped event is the real last
// interaction. Titles stay empty: state.json already carries them.
func MetaReader(home string) func(sessionID string) (string, int64) {
	index := &wireIndex{root: filepath.Join(home, "sessions")}
	return func(sessionID string) (string, int64) {
		if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
			return "", 0
		}
		return "", lastWireTimeMs(index.resolve(sessionID))
	}
}

// wireIndex maps session ids to their agents/main/wire.jsonl transcript.
// Workspace directories group sessions by project; a session belongs to one
// workspace, but a relocated project can leave copies behind — newest mtime
// wins, mirroring the claude transcript index.
type wireIndex struct {
	root  string
	mu    sync.Mutex
	at    time.Time
	files map[string]string
}

func (i *wireIndex) resolve(sessionID string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.files == nil || time.Since(i.at) > metaIndexTTL {
		i.rebuild()
	}
	return i.files[sessionID]
}

func (i *wireIndex) rebuild() {
	i.files = map[string]string{}
	i.at = time.Now()
	newest := map[string]time.Time{}
	workspaces, err := os.ReadDir(i.root)
	if err != nil {
		return
	}
	for _, workspace := range workspaces {
		if !workspace.IsDir() || strings.HasPrefix(workspace.Name(), ".") {
			continue
		}
		sessions, err := os.ReadDir(filepath.Join(i.root, workspace.Name()))
		if err != nil {
			continue
		}
		for _, session := range sessions {
			if !session.IsDir() {
				continue
			}
			wire := filepath.Join(i.root, workspace.Name(), session.Name(), "agents", "main", "wire.jsonl")
			info, err := os.Lstat(wire)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if known, ok := newest[session.Name()]; ok && !info.ModTime().After(known) {
				continue
			}
			newest[session.Name()] = info.ModTime()
			i.files[session.Name()] = wire
		}
	}
}

const (
	wireTailFirstScan = 64 << 10
	wireTailMaxScan   = 4 << 20
)

// lastWireTimeMs walks a wire transcript's tail backwards for the newest
// event carrying a time. session/load appends nothing here, so the value
// only moves on real conversation traffic. 0 when the transcript is missing
// or holds no time at all, leaving the row's own fields in place.
func lastWireTimeMs(path string) int64 {
	if path == "" {
		return 0
	}
	file, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	size := info.Size()
	for window := int64(wireTailFirstScan); ; window *= 4 {
		if window > size {
			window = size
		}
		buffer := make([]byte, window)
		if _, err := file.ReadAt(buffer, size-window); err != nil && err != io.EOF {
			return 0
		}
		if ms := scanLastWireTime(buffer, window < size); ms > 0 {
			return ms
		}
		if window >= size || window >= wireTailMaxScan {
			return 0
		}
	}
}

// scanLastWireTime parses complete lines of a tail buffer from the end. The
// leading element may be an event cut at the window edge; skip it unless the
// buffer starts the file.
func scanLastWireTime(buffer []byte, partialFirst bool) int64 {
	lines := bytes.Split(buffer, []byte{'\n'})
	start := 0
	if partialFirst {
		start = 1
	}
	end := len(lines)
	if end > start && len(lines[end-1]) == 0 {
		end--
	}
	for index := end - 1; index >= start; index-- {
		line := lines[index]
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		var record struct {
			Time any `json:"time"`
		}
		if decoder.Decode(&record) != nil {
			continue
		}
		if ms := parseSessionTime(record.Time); ms > 0 {
			return ms
		}
	}
	return 0
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
