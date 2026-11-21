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

func TestCodexListTitlePrefersSavedName(t *testing.T) {
	for _, test := range []struct {
		name any
		want string
	}{
		{"检查已打开的 Codex app", "检查已打开的 Codex app"},
		{nil, "首条消息"},
		{"", "首条消息"},
		{" \t", "首条消息"},
	} {
		item := row(map[string]any{"name": test.name, "title": "首条消息"})
		decorateCodexRow(readLocalProjects(""), item)
		if item.MustGet("title") != test.want {
			t.Fatalf("name=%v title=%v want=%s", test.name, item.MustGet("title"), test.want)
		}
	}
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

// 非 codex 行的分组身份：只按桌面项目根目录合组，不采纳 codex 线程的
// 显式归属；目录不落在任何根下时按规范化 cwd 独立成组。
func TestGroupIdentityForNonCodexAgents(t *testing.T) {
	home := writeState(t, `{
	  "local-projects": {
	    "p1": {"id": "p1", "name": "alpha", "rootPaths": ["/tmp/x/a"]},
	    "p2": {"id": "p2", "name": "beta", "rootPaths": ["/tmp/x/a/nested", "/tmp/y"]}
	  },
	  "thread-project-assignments": {
	    "assigned": {"projectKind": "local", "projectId": "p1"}
	  }
	}`)
	projects := readLocalProjects(home)

	// 根目录下的会话（任意 Agent）并入对应桌面项目组。
	if key, name := groupIdentity(projects, "kimi-1", "/tmp/x/a/deep", false); key != "local|p1" || name != "alpha" {
		t.Fatalf("root-matched: key=%s name=%s", key, name)
	}
	// 嵌套根取最长匹配，与 codex 侧一致。
	if key, name := groupIdentity(projects, "kimi-1", "/tmp/x/a/nested/deep", false); key != "local|p2" || name != "beta" {
		t.Fatalf("nested: key=%s name=%s", key, name)
	}
	// 非 codex 行不读线程归属：同 id 走 codex 路径才会得到 p1。
	if key, _ := groupIdentity(projects, "assigned", "/somewhere/else", false); key != "local|/somewhere/else" {
		t.Fatalf("assignment must not leak: %s", key)
	}
	if key, _ := groupIdentity(projects, "assigned", "/somewhere/else", true); key != "local|p1" {
		t.Fatalf("codex assignment should win: %s", key)
	}
	// 不在任何根下：规范化 cwd（去掉尾斜杠）独立成组。
	if key, name := groupIdentity(projects, "kimi-1", "/tmp/z/other/", false); key != "local|/tmp/z/other" || name != "other" {
		t.Fatalf("fallback: key=%s name=%s", key, name)
	}
	if key, name := groupIdentity(projects, "kimi-1", "", false); key != "local|unassigned" || name != "未归类" {
		t.Fatalf("empty cwd: key=%s name=%s", key, name)
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
