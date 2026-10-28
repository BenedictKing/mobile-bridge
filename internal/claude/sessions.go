// Package claude reads Claude Code's own on-disk session store.
//
// claude-agent-acp's session/list reports each transcript's file mtime as
// updatedAt. Resuming a session — session/load, which the gateway's startup
// preload issues for every recent chat — makes Claude Code append cost-state
// records that carry no timestamp and no conversation content, so each
// restart restamped the preloaded chats as just-used and floated them to the
// top of the list. The real last-interaction time is the transcript's last
// timestamped record, recovered here read-only.
package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ProjectsRoot is Claude Code's transcript root: CLAUDE_CONFIG_DIR/projects
// when the CLI runs under a custom config dir, else ~/.claude/projects.
func ProjectsRoot(env func(string) string, home string) string {
	if explicit := env("CLAUDE_CONFIG_DIR"); explicit != "" {
		return filepath.Join(explicit, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// indexTTL bounds how long one session-id → transcript map serves lookups
// before the project directories are re-enumerated. A list refresh asks for
// every row in one burst; the TTL keeps that at a single readdir pass.
const indexTTL = 30 * time.Second

// MetaReader returns a resolver for (title, last activity in ms) keyed by
// session id. Titles stay empty — claude-agent-acp provides them — and a
// missing or unreadable transcript yields 0 so the caller keeps the row's
// own fields.
func MetaReader(root string) func(sessionID string) (string, int64) {
	index := &transcriptIndex{root: root}
	return func(sessionID string) (string, int64) {
		if sessionID == "" || strings.ContainsAny(sessionID, `/\`) {
			return "", 0
		}
		return "", lastTimestampedMs(index.resolve(sessionID))
	}
}

// transcriptIndex maps session ids to their newest transcript across every
// project directory. A session copied or continued elsewhere leaves several
// files; the newest mtime wins, matching claude-agent-acp's own lookup.
type transcriptIndex struct {
	root  string
	mu    sync.Mutex
	at    time.Time
	files map[string]string
}

func (i *transcriptIndex) resolve(sessionID string) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.files == nil || time.Since(i.at) > indexTTL {
		i.rebuild()
	}
	return i.files[sessionID]
}

func (i *transcriptIndex) rebuild() {
	i.files = map[string]string{}
	i.at = time.Now()
	newest := map[string]time.Time{}
	projects, err := os.ReadDir(i.root)
	if err != nil {
		return
	}
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(i.root, project.Name())
		transcripts, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, transcript := range transcripts {
			name := transcript.Name()
			if transcript.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(name, ".jsonl")
			if id == "" {
				continue
			}
			info, err := transcript.Info()
			if err != nil {
				continue
			}
			if known, ok := newest[id]; ok && !info.ModTime().After(known) {
				continue
			}
			newest[id] = info.ModTime()
			i.files[id] = filepath.Join(dir, name)
		}
	}
}

const (
	tailFirstScan = 64 << 10
	tailMaxScan   = 4 << 20
)

// lastTimestampedMs walks a transcript's tail backwards for the newest
// record carrying a timestamp. The CLI stamps every conversation event
// (messages, tool traffic, system records) but not its cost-state or
// summary metadata, so resume-time appends never move the result. 0 when
// the transcript is missing or holds no timestamp at all.
func lastTimestampedMs(path string) int64 {
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
	for window := int64(tailFirstScan); window <= tailMaxScan; window *= 4 {
		if window > size {
			window = size
		}
		buffer := make([]byte, window)
		if _, err := file.ReadAt(buffer, size-window); err != nil && err != io.EOF {
			return 0
		}
		if ms := scanLastTimestamp(buffer, window < size); ms > 0 {
			return ms
		}
		if window >= size || window >= tailMaxScan {
			return 0
		}
	}
	return 0
}

// scanLastTimestamp parses complete lines of a tail buffer from the end.
// The leading element may be a record cut at the window edge; skip it
// unless the buffer starts the file.
func scanLastTimestamp(buffer []byte, partialFirst bool) int64 {
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
		var record struct {
			Timestamp string `json:"timestamp"`
		}
		if json.Unmarshal(line, &record) != nil || record.Timestamp == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, record.Timestamp); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}
