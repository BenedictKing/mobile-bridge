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
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
	"github.com/BenedictKing/mobile-bridge/internal/textutil"
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
					// The first user rows can be local-command caveats/echoes or a Team
					// envelope; blocks that clean to "" are skipped so the scan reaches
					// the first real prompt.
					for _, block := range entry.Content {
						if text := textutil.TitleFromText(block.Text); text != "" {
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

// transcriptIndexTTL bounds how long one session-id → transcript path map
// serves lookups before the projects tree is re-enumerated. A list refresh
// asks for every row in one burst; the TTL keeps that at a single readdir pass.
const transcriptIndexTTL = 30 * time.Second

// MetaReader returns a resolver for (title, last interaction in ms) keyed by
// session id. CodeBuddy CLI writes session-meta on every session/load — the
// gateway's startup preload issues one per recent chat — so rows built from
// the file mtime floated to the top of the list after each restart. The real
// last interaction is the newest honest event (message/tool) across every
// transcript copy; session/meta timestamps never count, and neither do the
// parasitic meta-only copies a load answered under a foreign cwd leaves in
// that project's slug. Titles stay empty: ListSessions already carries them.
func MetaReader(home string) func(sessionID string) (string, int64) {
	index := &transcriptIndex{root: ProjectsDir(home)}
	return func(sessionID string) (string, int64) {
		if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
			return "", 0
		}
		return "", lastInteractionMs(index.resolve(sessionID))
	}
}

// transcriptIndex maps session ids to every .jsonl transcript copy found.
// Project directories group sessions by cwd slug; besides relocated projects,
// a session/load answered under another cwd appends session-meta records into
// that project's slug, so one session can own several files — the honest time
// is the max across them all, not the newest-mtime one (the parasitic copy is
// exactly the one the preload keeps touching).
type transcriptIndex struct {
	root  string
	mu    sync.Mutex
	at    time.Time
	files map[string][]string
}

func (i *transcriptIndex) resolve(sessionID string) []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.files == nil || time.Since(i.at) > transcriptIndexTTL {
		i.rebuild()
	}
	return i.files[sessionID]
}

func (i *transcriptIndex) rebuild() {
	i.files = map[string][]string{}
	i.at = time.Now()
	projects, err := os.ReadDir(i.root)
	if err != nil {
		return
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(i.root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			sessionID := strings.TrimSuffix(entry.Name(), ".jsonl")
			transcriptPath := filepath.Join(dir, entry.Name())
			info, err := os.Lstat(transcriptPath)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			i.files[sessionID] = append(i.files[sessionID], transcriptPath)
		}
	}
}

const (
	transcriptTailFirstScan = 64 << 10
	transcriptTailMaxScan   = 4 << 20
)

// lastInteractionMs folds every transcript copy of one session into the
// newest honest event time. 0 when no copy holds one (a fresh never-prompted
// session, or only parasitic meta-only copies), leaving the row's own fields
// in place.
func lastInteractionMs(paths []string) int64 {
	var latest int64
	for _, path := range paths {
		if ms := lastTranscriptTimeMs(path); ms > latest {
			latest = ms
		}
	}
	return latest
}

// lastTranscriptTimeMs walks a transcript's tail backwards for the newest
// event carrying a timestamp. session/load appends session-meta records but
// they don't represent actual user interaction, so we scan for the last
// message or other timestamped event. Returns 0 when the transcript is
// missing or holds no timestamp at all, leaving the row's own fields in place.
func lastTranscriptTimeMs(path string) int64 {
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
	for window := int64(transcriptTailFirstScan); ; window *= 4 {
		if window > size {
			window = size
		}
		buffer := make([]byte, window)
		if _, err := file.ReadAt(buffer, size-window); err != nil && err != io.EOF {
			return 0
		}
		if ms := scanLastTranscriptTime(buffer, window < size); ms > 0 {
			return ms
		}
		if window >= size || window >= transcriptTailMaxScan {
			return 0
		}
	}
}

// scanLastTranscriptTime parses complete lines of a tail buffer from the end.
// The leading element may be an event cut at the window edge; skip it unless
// the buffer starts the file. We prioritize message events over session-meta
// to avoid using load-induced timestamps.
func scanLastTranscriptTime(buffer []byte, partialFirst bool) int64 {
	lines := bytes.Split(buffer, []byte{'\n'})
	start := 0
	if partialFirst {
		start = 1
	}
	end := len(lines)
	if end > start && len(lines[end-1]) == 0 {
		end--
	}
	// First pass: look for message events (real user interaction)
	for index := end - 1; index >= start; index-- {
		line := lines[index]
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var record struct {
			Type      string `json:"type"`
			Timestamp any    `json:"timestamp"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		// Prefer message events (user interaction) over session-meta (loads)
		if record.Type == "message" || record.Type == "tool-result" || record.Type == "tool-use" {
			if ms := parseTranscriptTime(record.Timestamp); ms > 0 {
				return ms
			}
		}
	}
	// Second pass: older transcripts may leave message rows untimestamped;
	// fall back to the newest timestamped non-meta event (reasoning/summary/
	// turn-metrics). session-meta never qualifies: the CLI appends those on
	// every session/load, so their timestamps are load time, not interaction.
	for index := end - 1; index >= start; index-- {
		line := lines[index]
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var record struct {
			Type      string `json:"type"`
			Timestamp any    `json:"timestamp"`
		}
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if record.Type == "session-meta" {
			continue
		}
		if ms := parseTranscriptTime(record.Timestamp); ms > 0 {
			return ms
		}
	}
	return 0
}

func parseTranscriptTime(value any) int64 {
	switch typed := value.(type) {
	case json.Number:
		if ms, err := typed.Int64(); err == nil {
			return ms
		}
	case float64:
		return int64(typed)
	case int64:
		return typed
	case int:
		return int64(typed)
	}
	return 0
}
