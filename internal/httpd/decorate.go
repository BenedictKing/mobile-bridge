package httpd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

// localProjects is the subset of the Codex app's global state
// (<codexHome>/.codex-global-state.json) that AppHosts.decorate matches
// against (bridge/remote.py): local-projects plus thread assignments.
type localProjects struct {
	names       map[string]string   // project id -> name
	roots       map[string][]string // project id -> normalized root paths
	assignments map[string]string   // session id -> assigned project id
}

func readLocalProjects(codexHome string) *localProjects {
	projects := &localProjects{names: map[string]string{}, roots: map[string][]string{},
		assignments: map[string]string{}}
	if codexHome == "" {
		return projects
	}
	raw, err := os.ReadFile(filepath.Join(codexHome, ".codex-global-state.json"))
	if err != nil {
		return projects
	}
	value, err := jsoncompat.Unmarshal(raw)
	if err != nil {
		return projects
	}
	root, ok := value.(*jsoncompat.Object)
	if !ok {
		return projects
	}
	if items, ok := root.MustGet("local-projects").(*jsoncompat.Object); ok {
		for _, id := range items.Keys() {
			entry, _ := items.MustGet(id).(*jsoncompat.Object)
			if entry == nil {
				continue
			}
			name, _ := entry.MustGet("name").(string)
			projects.names[id] = name
			var roots []string
			if list, ok := entry.MustGet("rootPaths").(*jsoncompat.Array); ok {
				for _, item := range list.Items {
					if path, ok := item.(string); ok {
						roots = append(roots, strings.TrimRight(path, "/"))
					}
				}
			}
			projects.roots[id] = roots
		}
	}
	if items, ok := root.MustGet("thread-project-assignments").(*jsoncompat.Object); ok {
		for _, sessionID := range items.Keys() {
			entry, _ := items.MustGet(sessionID).(*jsoncompat.Object)
			if entry == nil {
				continue
			}
			id, _ := entry.MustGet("projectId").(string)
			hostID, _ := entry.MustGet("hostId").(string)
			if id != "" && (hostID == "" || hostID == "local") {
				projects.assignments[sessionID] = id
			}
		}
	}
	return projects
}

// match mirrors decorate()'s selection: an explicit assignment wins (when the
// project still exists), else the longest root whose normalized form is the
// cwd or a prefix of it.
func (p *localProjects) match(sessionID, cwd string) (string, bool) {
	if id := p.assignments[sessionID]; id != "" {
		if _, known := p.names[id]; known {
			return id, true
		}
	}
	normalized := strings.TrimRight(cwd, "/")
	best, bestLen := "", -1
	for id, roots := range p.roots {
		for _, root := range roots {
			if root == "" {
				continue
			}
			if normalized == root || strings.HasPrefix(normalized, root+"/") {
				if len(root) > bestLen {
					best, bestLen = id, len(root)
				}
			}
		}
	}
	return best, best != ""
}

// decorateCodexRow mirrors AppHosts.decorate for the local codex host:
// hostLabel, projectKey/projectName and the unified recency.
func decorateCodexRow(projects *localProjects, row *jsoncompat.Object) {
	cwd, _ := row.MustGet("cwd").(string)
	id, _ := row.MustGet("id").(string)
	projectKey, projectName := cwd, projectBaseName(cwd)
	if projectID, ok := projects.match(id, cwd); ok {
		projectKey = projectID
		projectName = projects.names[projectID]
	}
	if projectKey == "" {
		projectKey = "unassigned"
	}
	if projectName == "" {
		projectName = "未归类"
	}
	row.Set("hostLabel", "此电脑")
	row.Set("projectKey", "local|"+projectKey)
	row.Set("projectName", projectName)
	row.Set("recency", recencyOf(row))
}

func projectBaseName(path string) string {
	if runtime.GOOS == "windows" {
		path = strings.ReplaceAll(path, "\\", "/")
	}
	return strings.TrimRight(path, "/")[strings.LastIndex(strings.TrimRight(path, "/"), "/")+1:]
}

// recencyOf folds the store's timestamps into the single recency field the
// list contract exposes: updated_at_ms, else updated_at in seconds, else the
// recency_at pair.
func recencyOf(row *jsoncompat.Object) json.Number {
	for _, field := range []string{"updated_at_ms", "updated_at", "recency_at_ms", "recency_at"} {
		if value, exists := row.Get(field); exists {
			if number, ok := value.(json.Number); ok {
				if parsed, err := number.Int64(); err == nil {
					if strings.HasSuffix(field, "_ms") {
						return jsoncompat.Num(int(parsed))
					}
					return jsoncompat.Num(int(parsed * 1000))
				}
			}
		}
	}
	return jsoncompat.Num(0)
}

// acpRecencyOf reads an ACP session/list row's updatedAt (epoch milliseconds
// or RFC 3339) the way the Python adapter's _recency_ms does, best effort.
func acpRecencyOf(row *jsoncompat.Object) json.Number {
	if value, exists := row.Get("updatedAt"); exists {
		if number, ok := value.(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				return jsoncompat.Num(int(parsed))
			}
		}
		if text, ok := value.(string); ok {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999Z0700"} {
				if parsed, err := time.Parse(layout, text); err == nil {
					return jsoncompat.Num(int(parsed.UnixMilli()))
				}
			}
		}
	}
	return jsoncompat.Num(0)
}

// sortSessionRows mirrors service.py's merge ordering: recency, then id, then
// host, all descending.
func sortSessionRows(items []any) {
	rowAt := func(i int) *jsoncompat.Object {
		row, _ := items[i].(*jsoncompat.Object)
		return row
	}
	numberOf := func(row *jsoncompat.Object) int64 {
		if row == nil {
			return 0
		}
		if number, ok := row.MustGet("recency").(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				return parsed
			}
		}
		return 0
	}
	textOf := func(row *jsoncompat.Object, key string) string {
		if row == nil {
			return ""
		}
		value, _ := row.MustGet(key).(string)
		return value
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := rowAt(i), rowAt(j)
		if numberOf(a) != numberOf(b) {
			return numberOf(a) > numberOf(b)
		}
		if textOf(a, "id") != textOf(b, "id") {
			return textOf(a, "id") > textOf(b, "id")
		}
		return textOf(a, "host") > textOf(b, "host")
	})
}
