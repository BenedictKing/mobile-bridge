package httpd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BenedictKing/mobile-bridge/internal/jsoncompat"
)

func writeState(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".codex-global-state.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func row(fields map[string]any) *jsoncompat.Object {
	object := jsoncompat.NewObject()
	for key, value := range fields {
		if number, ok := value.(int64); ok {
			object.Set(key, jsoncompat.Num(int(number)))
			continue
		}
		object.Set(key, value)
	}
	return object
}

func TestDecorateCodexRowMatchesAssignmentRootsAndFallbacks(t *testing.T) {
	home := writeState(t, `{
	  "local-projects": {
	    "p1": {"id": "p1", "name": "alpha", "rootPaths": ["/tmp/x/a"]},
	    "p2": {"id": "p2", "name": "beta", "rootPaths": ["/tmp/x/a/nested", "/tmp/y"]}
	  },
	  "thread-project-assignments": {
	    "assigned": {"projectKind": "local", "projectId": "p1"},
	    "ghost": {"projectKind": "local", "projectId": "gone"},
	    "remote-owned": {"projectId": "p1", "hostId": "server1"}
	  }
	}`)
	projects := readLocalProjects(home)

	assigned := row(map[string]any{"id": "assigned", "cwd": "/somewhere/else", "updated_at": int64(100)})
	decorateCodexRow(projects, assigned)
	if assigned.MustGet("projectName") != "alpha" || assigned.MustGet("projectKey") != "local|p1" {
		t.Fatalf("assigned row: %s", jsoncompat.EncodePythonDefault(assigned))
	}
	if assigned.MustGet("recency").(json.Number).String() != "100000" {
		t.Fatalf("recency should fold seconds to ms: %v", assigned.MustGet("recency"))
	}

	// Longest root wins over the shorter /tmp/x/a prefix.
	nested := row(map[string]any{"id": "nested", "cwd": "/tmp/x/a/nested/deep", "updated_at_ms": int64(5)})
	decorateCodexRow(projects, nested)
	if nested.MustGet("projectName") != "beta" || nested.MustGet("hostLabel") != "此电脑" {
		t.Fatalf("nested row: %s", jsoncompat.EncodePythonDefault(nested))
	}
	if nested.MustGet("recency").(json.Number).String() != "5" {
		t.Fatalf("updated_at_ms preferred: %v", nested.MustGet("recency"))
	}

	// Ghost assignment falls back to root matching; remote hostId is ignored.
	ghost := row(map[string]any{"id": "ghost", "cwd": "/tmp/y/z"})
	decorateCodexRow(projects, ghost)
	if ghost.MustGet("projectName") != "beta" {
		t.Fatalf("ghost row: %s", jsoncompat.EncodePythonDefault(ghost))
	}

	plain := row(map[string]any{"id": "plain", "cwd": "/Users/x/works/mobile-bridge"})
	decorateCodexRow(projects, plain)
	if plain.MustGet("projectName") != "mobile-bridge" || plain.MustGet("projectKey") != "local|/Users/x/works/mobile-bridge" {
		t.Fatalf("plain row: %s", jsoncompat.EncodePythonDefault(plain))
	}

	empty := row(map[string]any{"id": "empty", "cwd": ""})
	decorateCodexRow(projects, empty)
	if empty.MustGet("projectName") != "未归类" || empty.MustGet("projectKey") != "local|unassigned" {
		t.Fatalf("empty row: %s", jsoncompat.EncodePythonDefault(empty))
	}
}

func TestProjectBaseNameEdges(t *testing.T) {
	for path, want := range map[string]string{"/a/b/": "b", "b": "b", "": "", "/x": "x"} {
		if got := projectBaseName(path); got != want {
			t.Fatalf("projectBaseName(%q)=%q want %q", path, got, want)
		}
	}
}

func TestSortSessionRowsByRecencyThenId(t *testing.T) {
	items := []any{
		row(map[string]any{"id": "b", "recency": int64(5), "host": "local"}),
		row(map[string]any{"id": "c", "recency": int64(9), "host": "local"}),
		row(map[string]any{"id": "a", "recency": int64(9), "host": "local"}),
	}
	sortSessionRows(items)
	first := items[0].(*jsoncompat.Object)
	second := items[1].(*jsoncompat.Object)
	// Python sort is reverse over (recency, id, host): higher id wins ties.
	if first.MustGet("id") != "c" || second.MustGet("id") != "a" {
		t.Fatalf("order wrong: %s", jsoncompat.EncodePythonDefault(first))
	}
}
